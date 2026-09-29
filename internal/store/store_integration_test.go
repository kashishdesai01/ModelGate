package store

import (
	"context"
	"errors"
	"fmt"
	"os"
	"sync"
	"sync/atomic"
	"testing"
	"time"
)

func TestConcurrentReservationsCannotOverspend(t *testing.T) {
	dsn := os.Getenv("MODEL_GATE_TEST_DATABASE_URL")
	if dsn == "" {
		t.Skip("MODEL_GATE_TEST_DATABASE_URL is not set")
	}
	ctx := context.Background()
	store, err := Open(ctx, dsn)
	if err != nil {
		t.Fatalf("open store: %v", err)
	}
	defer store.Close()
	if err := store.Migrate(ctx); err != nil {
		t.Fatalf("migrate: %v", err)
	}

	userID := "00000000-0000-0000-0000-000000000123"
	resetTestUser(t, store, userID)
	apiKey := "mg_test_concurrency_key"
	if err := store.SeedUser(ctx, SeedUserParams{UserID: userID, Name: "concurrency", APIKey: apiKey, MonthlyBudgetMicroUSD: 500}); err != nil {
		t.Fatalf("seed: %v", err)
	}

	var accepted atomic.Int64
	var rejected atomic.Int64
	var unexpected atomic.Int64
	var wg sync.WaitGroup
	start := make(chan struct{})
	for index := 0; index < 20; index++ {
		wg.Add(1)
		go func(index int) {
			defer wg.Done()
			<-start
			_, reserveErr := store.Reserve(ctx, ReserveParams{
				RequestID: "budget-test-" + time.Now().Add(time.Duration(index)).Format("150405.000000000"),
				UserID:    userID, Strategy: "sklearn", EstimatedCostMicroUSD: 100,
			})
			switch {
			case reserveErr == nil:
				accepted.Add(1)
			case errors.Is(reserveErr, ErrBudgetExceeded):
				rejected.Add(1)
			default:
				unexpected.Add(1)
			}
		}(index)
	}
	close(start)
	wg.Wait()

	if unexpected.Load() != 0 {
		t.Fatalf("unexpected reservation errors: %d", unexpected.Load())
	}
	if accepted.Load() != 5 || rejected.Load() != 15 {
		t.Fatalf("accepted=%d rejected=%d; want 5 and 15", accepted.Load(), rejected.Load())
	}
	usage, err := store.Usage(ctx, userID)
	if err != nil {
		t.Fatalf("usage: %v", err)
	}
	if usage.ReservedMicroUSD != 500 || usage.SpentMicroUSD != 0 {
		t.Fatalf("unexpected usage: %+v", usage)
	}
}

func TestReseedingPreservesSpendAndAuditHistory(t *testing.T) {
	dsn := os.Getenv("MODEL_GATE_TEST_DATABASE_URL")
	if dsn == "" {
		t.Skip("MODEL_GATE_TEST_DATABASE_URL is not set")
	}
	ctx := context.Background()
	database, err := Open(ctx, dsn)
	if err != nil {
		t.Fatal(err)
	}
	defer database.Close()
	if err := database.Migrate(ctx); err != nil {
		t.Fatal(err)
	}
	userID := "00000000-0000-0000-0000-000000000125"
	resetTestUser(t, database, userID)
	params := SeedUserParams{UserID: userID, Name: "reseed", APIKey: "mg_test_reseed", MonthlyBudgetMicroUSD: 500}
	if err := database.SeedUser(ctx, params); err != nil {
		t.Fatal(err)
	}
	requestID := fmt.Sprintf("reseed-%d", time.Now().UnixNano())
	if _, err := database.Reserve(ctx, ReserveParams{RequestID: requestID, UserID: userID, Strategy: "sklearn", EstimatedCostMicroUSD: 100}); err != nil {
		t.Fatal(err)
	}
	if err := database.Reconcile(ctx, ReconcileParams{RequestID: requestID, ActualCost: 100, Succeeded: true}); err != nil {
		t.Fatal(err)
	}
	params.MonthlyBudgetMicroUSD = 700
	if err := database.SeedUser(ctx, params); err != nil {
		t.Fatal(err)
	}
	usage, err := database.Usage(ctx, userID)
	if err != nil {
		t.Fatal(err)
	}
	if usage.LimitMicroUSD != 700 || usage.SpentMicroUSD != 100 || usage.ReservedMicroUSD != 0 {
		t.Fatalf("reseed changed accounting: %+v", usage)
	}
	var callCount int
	if err := database.pool.QueryRow(ctx, `SELECT count(*) FROM inference_calls WHERE request_id=$1`, requestID).Scan(&callCount); err != nil {
		t.Fatal(err)
	}
	if callCount != 1 {
		t.Fatalf("reseed deleted audit history: count=%d", callCount)
	}
}

func TestReservationReconcilesAgainstItsOriginalMonth(t *testing.T) {
	dsn := os.Getenv("MODEL_GATE_TEST_DATABASE_URL")
	if dsn == "" {
		t.Skip("MODEL_GATE_TEST_DATABASE_URL is not set")
	}
	ctx := context.Background()
	database, err := Open(ctx, dsn)
	if err != nil {
		t.Fatal(err)
	}
	defer database.Close()
	if err := database.Migrate(ctx); err != nil {
		t.Fatal(err)
	}
	january := time.Date(2026, time.January, 31, 23, 59, 0, 0, time.UTC)
	database.now = func() time.Time { return january }
	userID := "00000000-0000-0000-0000-000000000126"
	resetTestUser(t, database, userID)
	if err := database.SeedUser(ctx, SeedUserParams{UserID: userID, Name: "rollover", APIKey: "mg_test_rollover", MonthlyBudgetMicroUSD: 500}); err != nil {
		t.Fatal(err)
	}
	requestID := fmt.Sprintf("rollover-%d", time.Now().UnixNano())
	if _, err := database.Reserve(ctx, ReserveParams{RequestID: requestID, UserID: userID, Strategy: "sklearn", EstimatedCostMicroUSD: 100}); err != nil {
		t.Fatal(err)
	}
	database.now = func() time.Time { return january.Add(2 * time.Minute) }
	if err := database.Reconcile(ctx, ReconcileParams{RequestID: requestID, ActualCost: 100, Succeeded: true}); err != nil {
		t.Fatal(err)
	}
	februaryUsage, err := database.Usage(ctx, userID)
	if err != nil {
		t.Fatal(err)
	}
	if februaryUsage.LimitMicroUSD != 500 || februaryUsage.SpentMicroUSD != 0 || februaryUsage.ReservedMicroUSD != 0 {
		t.Fatalf("unexpected February usage: %+v", februaryUsage)
	}
	var januarySpent, januaryReserved int64
	if err := database.pool.QueryRow(ctx, `SELECT spent_microusd,reserved_microusd FROM budget_periods WHERE user_id=$1 AND period_start=$2`, userID, time.Date(2026, time.January, 1, 0, 0, 0, 0, time.UTC)).Scan(&januarySpent, &januaryReserved); err != nil {
		t.Fatal(err)
	}
	if januarySpent != 100 || januaryReserved != 0 {
		t.Fatalf("original period was not reconciled: spent=%d reserved=%d", januarySpent, januaryReserved)
	}
}

func TestCacheHitPersistsModelAndPolicyProvenance(t *testing.T) {
	dsn := os.Getenv("MODEL_GATE_TEST_DATABASE_URL")
	if dsn == "" {
		t.Skip("MODEL_GATE_TEST_DATABASE_URL is not set")
	}
	ctx := context.Background()
	database, err := Open(ctx, dsn)
	if err != nil {
		t.Fatal(err)
	}
	defer database.Close()
	if err := database.Migrate(ctx); err != nil {
		t.Fatal(err)
	}
	userID := "00000000-0000-0000-0000-000000000127"
	resetTestUser(t, database, userID)
	if err := database.SeedUser(ctx, SeedUserParams{UserID: userID, Name: "cache", APIKey: "mg_test_cache", MonthlyBudgetMicroUSD: 500}); err != nil {
		t.Fatal(err)
	}
	requestID := fmt.Sprintf("cache-%d", time.Now().UnixNano())
	params := CacheHitParams{
		RequestID: requestID, UserID: userID, Strategy: "hybrid", ModelKind: "sklearn",
		ModelID: "home-service-classifier", ModelVersion: "candidate-v2", PolicyVersion: "hybrid-v1",
	}
	if err := database.RecordCacheHit(ctx, params); err != nil {
		t.Fatal(err)
	}
	var modelID, modelVersion, policyVersion string
	if err := database.pool.QueryRow(ctx, `SELECT model_id,model_version,policy_version FROM inference_calls WHERE request_id=$1`, requestID).Scan(&modelID, &modelVersion, &policyVersion); err != nil {
		t.Fatal(err)
	}
	if modelID != params.ModelID || modelVersion != params.ModelVersion || policyVersion != params.PolicyVersion {
		t.Fatalf("cache provenance mismatch: model=%s version=%s policy=%s", modelID, modelVersion, policyVersion)
	}
}

func TestAPIKeysAreHashedAndRevokedKeysFailAuthentication(t *testing.T) {
	dsn := os.Getenv("MODEL_GATE_TEST_DATABASE_URL")
	if dsn == "" {
		t.Skip("MODEL_GATE_TEST_DATABASE_URL is not set")
	}
	ctx := context.Background()
	store, err := Open(ctx, dsn)
	if err != nil {
		t.Fatalf("open store: %v", err)
	}
	defer store.Close()
	if err := store.Migrate(ctx); err != nil {
		t.Fatalf("migrate: %v", err)
	}
	params := SeedUserParams{UserID: "00000000-0000-0000-0000-000000000124", Name: "auth", APIKey: "mg_test_auth_key", MonthlyBudgetMicroUSD: 1000}
	resetTestUser(t, store, params.UserID)
	if err := store.SeedUser(ctx, params); err != nil {
		t.Fatalf("seed: %v", err)
	}

	identity, err := store.Authenticate(ctx, params.APIKey)
	if err != nil || identity.UserID != params.UserID {
		t.Fatalf("authenticate: identity=%+v error=%v", identity, err)
	}
	if err := store.RevokeAPIKey(ctx, params.APIKey); err != nil {
		t.Fatalf("revoke: %v", err)
	}
	if _, err := store.Authenticate(ctx, params.APIKey); !errors.Is(err, ErrUnauthorized) {
		t.Fatalf("expected unauthorized after revoke, got %v", err)
	}
}

func resetTestUser(t *testing.T, database *Store, userID string) {
	t.Helper()
	ctx := context.Background()
	transaction, err := database.pool.Begin(ctx)
	if err != nil {
		t.Fatalf("begin test reset: %v", err)
	}
	defer func() { _ = transaction.Rollback(ctx) }()
	for _, statement := range []string{
		`DELETE FROM inference_calls WHERE user_id=$1`,
		`DELETE FROM budget_periods WHERE user_id=$1`,
		`DELETE FROM api_keys WHERE user_id=$1`,
		`DELETE FROM users WHERE id=$1`,
	} {
		if _, err := transaction.Exec(ctx, statement, userID); err != nil {
			t.Fatalf("reset test user: %v", err)
		}
	}
	if err := transaction.Commit(ctx); err != nil {
		t.Fatalf("commit test reset: %v", err)
	}
}
