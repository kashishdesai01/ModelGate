package store

import (
	"context"
	"crypto/sha256"
	"errors"
	"fmt"
	"time"

	"github.com/jackc/pgx/v5"
	"github.com/jackc/pgx/v5/pgxpool"
)

var (
	ErrBudgetExceeded = errors.New("budget exceeded")
	ErrUnauthorized   = errors.New("unauthorized")
	ErrDuplicate      = errors.New("duplicate request")
)

const schema = `
CREATE TABLE IF NOT EXISTS users (
    id TEXT PRIMARY KEY,
    display_name TEXT NOT NULL,
    monthly_budget_microusd BIGINT NOT NULL CHECK (monthly_budget_microusd >= 0),
    rate_capacity INTEGER NOT NULL DEFAULT 20 CHECK (rate_capacity > 0),
    rate_refill_per_second DOUBLE PRECISION NOT NULL DEFAULT 5 CHECK (rate_refill_per_second > 0),
    created_at TIMESTAMPTZ NOT NULL DEFAULT now()
);
CREATE TABLE IF NOT EXISTS api_keys (
    key_hash BYTEA PRIMARY KEY,
    user_id TEXT NOT NULL REFERENCES users(id) ON DELETE CASCADE,
    revoked_at TIMESTAMPTZ,
    created_at TIMESTAMPTZ NOT NULL DEFAULT now()
);
CREATE TABLE IF NOT EXISTS budget_periods (
    user_id TEXT NOT NULL REFERENCES users(id) ON DELETE CASCADE,
    period_start DATE NOT NULL,
    period_end DATE NOT NULL,
    limit_microusd BIGINT NOT NULL CHECK (limit_microusd >= 0),
    spent_microusd BIGINT NOT NULL DEFAULT 0 CHECK (spent_microusd >= 0),
    reserved_microusd BIGINT NOT NULL DEFAULT 0 CHECK (reserved_microusd >= 0),
    updated_at TIMESTAMPTZ NOT NULL DEFAULT now(),
    PRIMARY KEY (user_id, period_start)
);
CREATE TABLE IF NOT EXISTS inference_calls (
    request_id TEXT PRIMARY KEY,
    user_id TEXT NOT NULL REFERENCES users(id),
    strategy TEXT NOT NULL,
    state TEXT NOT NULL CHECK (state IN ('pending','succeeded','failed','cache_hit')),
    reserved_cost_microusd BIGINT NOT NULL DEFAULT 0 CHECK (reserved_cost_microusd >= 0),
    actual_cost_microusd BIGINT NOT NULL DEFAULT 0 CHECK (actual_cost_microusd >= 0),
    budget_period_start DATE NOT NULL,
    model_kind TEXT,
    model_id TEXT,
    model_version TEXT,
    policy_version TEXT,
    fallback_used BOOLEAN NOT NULL DEFAULT false,
    error_code TEXT,
    latency_ms INTEGER,
    created_at TIMESTAMPTZ NOT NULL DEFAULT now(),
    completed_at TIMESTAMPTZ
);
ALTER TABLE inference_calls ADD COLUMN IF NOT EXISTS budget_period_start DATE;
ALTER TABLE inference_calls ADD COLUMN IF NOT EXISTS model_id TEXT;
ALTER TABLE inference_calls ADD COLUMN IF NOT EXISTS policy_version TEXT;
UPDATE inference_calls SET budget_period_start=date_trunc('month',created_at)::date WHERE budget_period_start IS NULL;
ALTER TABLE inference_calls ALTER COLUMN budget_period_start SET NOT NULL;
CREATE INDEX IF NOT EXISTS inference_calls_user_created_idx ON inference_calls(user_id, created_at DESC);
`

type Store struct {
	pool *pgxpool.Pool
	now  func() time.Time
}

func Open(ctx context.Context, dsn string) (*Store, error) {
	config, err := pgxpool.ParseConfig(dsn)
	if err != nil {
		return nil, fmt.Errorf("parse database URL: %w", err)
	}
	config.MaxConns = 10
	pool, err := pgxpool.NewWithConfig(ctx, config)
	if err != nil {
		return nil, fmt.Errorf("open database: %w", err)
	}
	if err := pool.Ping(ctx); err != nil {
		pool.Close()
		return nil, fmt.Errorf("ping database: %w", err)
	}
	return &Store{pool: pool, now: time.Now}, nil
}

func (s *Store) Close() { s.pool.Close() }

func (s *Store) Ping(ctx context.Context) error {
	if err := s.pool.Ping(ctx); err != nil {
		return fmt.Errorf("database unavailable: %w", err)
	}
	return nil
}

func (s *Store) Migrate(ctx context.Context) error {
	if _, err := s.pool.Exec(ctx, schema); err != nil {
		return fmt.Errorf("apply schema: %w", err)
	}
	return nil
}

type SeedUserParams struct {
	UserID                string
	Name                  string
	APIKey                string
	MonthlyBudgetMicroUSD int64
	RateCapacity          int
	RateRefillPerSecond   float64
}

func (s *Store) SeedUser(ctx context.Context, params SeedUserParams) error {
	if params.RateCapacity == 0 {
		params.RateCapacity = 20
	}
	if params.RateRefillPerSecond == 0 {
		params.RateRefillPerSecond = 5
	}
	tx, err := s.pool.BeginTx(ctx, pgx.TxOptions{})
	if err != nil {
		return err
	}
	defer func() { _ = tx.Rollback(ctx) }()
	_, err = tx.Exec(ctx, `
        INSERT INTO users(id, display_name, monthly_budget_microusd, rate_capacity, rate_refill_per_second)
        VALUES ($1,$2,$3,$4,$5)
        ON CONFLICT (id) DO UPDATE SET display_name=EXCLUDED.display_name,
          monthly_budget_microusd=EXCLUDED.monthly_budget_microusd,
          rate_capacity=EXCLUDED.rate_capacity, rate_refill_per_second=EXCLUDED.rate_refill_per_second`,
		params.UserID, params.Name, params.MonthlyBudgetMicroUSD, params.RateCapacity, params.RateRefillPerSecond)
	if err != nil {
		return fmt.Errorf("seed user: %w", err)
	}
	keyHash := hashKey(params.APIKey)
	_, err = tx.Exec(ctx, `INSERT INTO api_keys(key_hash,user_id,revoked_at) VALUES ($1,$2,NULL)
        ON CONFLICT (key_hash) DO UPDATE SET user_id=EXCLUDED.user_id, revoked_at=NULL`, keyHash[:], params.UserID)
	if err != nil {
		return fmt.Errorf("seed API key: %w", err)
	}
	periodStart, periodEnd := periodAt(s.now())
	_, err = tx.Exec(ctx, `INSERT INTO budget_periods(user_id,period_start,period_end,limit_microusd,spent_microusd,reserved_microusd)
        VALUES ($1,$2,$3,$4,0,0)
        ON CONFLICT (user_id,period_start) DO UPDATE SET limit_microusd=EXCLUDED.limit_microusd,
          period_end=EXCLUDED.period_end,updated_at=now()`, params.UserID, periodStart, periodEnd, params.MonthlyBudgetMicroUSD)
	if err != nil {
		return fmt.Errorf("seed budget: %w", err)
	}
	return tx.Commit(ctx)
}

type Identity struct {
	UserID              string
	RateCapacity        int
	RateRefillPerSecond float64
}

func (s *Store) Authenticate(ctx context.Context, apiKey string) (Identity, error) {
	keyHash := hashKey(apiKey)
	var identity Identity
	err := s.pool.QueryRow(ctx, `SELECT u.id,u.rate_capacity,u.rate_refill_per_second
        FROM api_keys k JOIN users u ON u.id=k.user_id
        WHERE k.key_hash=$1 AND k.revoked_at IS NULL`, keyHash[:]).Scan(
		&identity.UserID, &identity.RateCapacity, &identity.RateRefillPerSecond)
	if errors.Is(err, pgx.ErrNoRows) {
		return Identity{}, ErrUnauthorized
	}
	if err != nil {
		return Identity{}, fmt.Errorf("authenticate: %w", err)
	}
	return identity, nil
}

func (s *Store) RevokeAPIKey(ctx context.Context, apiKey string) error {
	keyHash := hashKey(apiKey)
	result, err := s.pool.Exec(ctx, `UPDATE api_keys SET revoked_at=now() WHERE key_hash=$1`, keyHash[:])
	if err != nil {
		return fmt.Errorf("revoke API key: %w", err)
	}
	if result.RowsAffected() == 0 {
		return ErrUnauthorized
	}
	return nil
}

type ReserveParams struct {
	RequestID             string
	UserID                string
	Strategy              string
	EstimatedCostMicroUSD int64
}

type Reservation struct {
	RequestID    string
	ReservedCost int64
	PeriodStart  time.Time
}

func (s *Store) Reserve(ctx context.Context, params ReserveParams) (Reservation, error) {
	if params.EstimatedCostMicroUSD < 0 {
		return Reservation{}, fmt.Errorf("estimated cost must be nonnegative")
	}
	tx, err := s.pool.BeginTx(ctx, pgx.TxOptions{})
	if err != nil {
		return Reservation{}, err
	}
	defer func() { _ = tx.Rollback(ctx) }()
	periodStart, periodEnd := periodAt(s.now())
	if err := ensureBudgetPeriod(ctx, tx, params.UserID, periodStart, periodEnd); err != nil {
		return Reservation{}, err
	}
	var limit, spent, reserved int64
	err = tx.QueryRow(ctx, `SELECT limit_microusd,spent_microusd,reserved_microusd
        FROM budget_periods WHERE user_id=$1 AND period_start=$2 FOR UPDATE`, params.UserID, periodStart).Scan(&limit, &spent, &reserved)
	if err != nil {
		return Reservation{}, fmt.Errorf("lock budget: %w", err)
	}
	if params.EstimatedCostMicroUSD > limit-spent-reserved {
		return Reservation{}, ErrBudgetExceeded
	}
	_, err = tx.Exec(ctx, `INSERT INTO inference_calls(request_id,user_id,strategy,state,reserved_cost_microusd,budget_period_start)
        VALUES ($1,$2,$3,'pending',$4,$5)`, params.RequestID, params.UserID, params.Strategy, params.EstimatedCostMicroUSD, periodStart)
	if err != nil {
		if isUniqueViolation(err) {
			return Reservation{}, ErrDuplicate
		}
		return Reservation{}, fmt.Errorf("insert call: %w", err)
	}
	_, err = tx.Exec(ctx, `UPDATE budget_periods SET reserved_microusd=reserved_microusd+$1,updated_at=now()
        WHERE user_id=$2 AND period_start=$3`, params.EstimatedCostMicroUSD, params.UserID, periodStart)
	if err != nil {
		return Reservation{}, fmt.Errorf("reserve budget: %w", err)
	}
	if err := tx.Commit(ctx); err != nil {
		return Reservation{}, err
	}
	return Reservation{RequestID: params.RequestID, ReservedCost: params.EstimatedCostMicroUSD, PeriodStart: periodStart}, nil
}

type ReconcileParams struct {
	RequestID     string
	ActualCost    int64
	Succeeded     bool
	ModelKind     string
	ModelID       string
	ModelVersion  string
	PolicyVersion string
	FallbackUsed  bool
	ErrorCode     string
	Latency       time.Duration
}

type CacheHitParams struct {
	RequestID     string
	UserID        string
	Strategy      string
	ModelKind     string
	ModelID       string
	ModelVersion  string
	PolicyVersion string
	Latency       time.Duration
}

func (s *Store) Reconcile(ctx context.Context, params ReconcileParams) error {
	if params.ActualCost < 0 {
		return fmt.Errorf("actual cost must be nonnegative")
	}
	tx, err := s.pool.BeginTx(ctx, pgx.TxOptions{})
	if err != nil {
		return err
	}
	defer func() { _ = tx.Rollback(ctx) }()
	var userID, state string
	var reserved int64
	var periodStart time.Time
	err = tx.QueryRow(ctx, `SELECT user_id,state,reserved_cost_microusd,budget_period_start
        FROM inference_calls WHERE request_id=$1 FOR UPDATE`, params.RequestID).Scan(&userID, &state, &reserved, &periodStart)
	if err != nil {
		return fmt.Errorf("load call for reconcile: %w", err)
	}
	if state != "pending" {
		return nil
	}
	result, err := tx.Exec(ctx, `UPDATE budget_periods SET reserved_microusd=reserved_microusd-$1,
        spent_microusd=spent_microusd+$2,updated_at=now() WHERE user_id=$3 AND period_start=$4`, reserved, params.ActualCost, userID, periodStart)
	if err != nil {
		return fmt.Errorf("reconcile budget: %w", err)
	}
	if result.RowsAffected() != 1 {
		return fmt.Errorf("reconcile budget: original budget period missing")
	}
	finalState := "failed"
	if params.Succeeded {
		finalState = "succeeded"
	}
	_, err = tx.Exec(ctx, `UPDATE inference_calls SET state=$1,actual_cost_microusd=$2,model_kind=$3,
        model_id=$4,model_version=$5,policy_version=$6,fallback_used=$7,error_code=NULLIF($8,''),latency_ms=$9,completed_at=now()
        WHERE request_id=$10`, finalState, params.ActualCost, params.ModelKind, params.ModelID, params.ModelVersion, params.PolicyVersion, params.FallbackUsed, params.ErrorCode, params.Latency.Milliseconds(), params.RequestID)
	if err != nil {
		return fmt.Errorf("finalize call: %w", err)
	}
	return tx.Commit(ctx)
}

type Usage struct {
	LimitMicroUSD    int64 `json:"limit_microusd"`
	SpentMicroUSD    int64 `json:"spent_microusd"`
	ReservedMicroUSD int64 `json:"reserved_microusd"`
}

func (s *Store) Usage(ctx context.Context, userID string) (Usage, error) {
	periodStart, periodEnd := periodAt(s.now())
	tx, err := s.pool.BeginTx(ctx, pgx.TxOptions{})
	if err != nil {
		return Usage{}, err
	}
	defer func() { _ = tx.Rollback(ctx) }()
	if err := ensureBudgetPeriod(ctx, tx, userID, periodStart, periodEnd); err != nil {
		return Usage{}, err
	}
	var usage Usage
	err = tx.QueryRow(ctx, `SELECT limit_microusd,spent_microusd,reserved_microusd
        FROM budget_periods WHERE user_id=$1 AND period_start=$2`, userID, periodStart).Scan(&usage.LimitMicroUSD, &usage.SpentMicroUSD, &usage.ReservedMicroUSD)
	if err != nil {
		return Usage{}, fmt.Errorf("load usage: %w", err)
	}
	if err := tx.Commit(ctx); err != nil {
		return Usage{}, err
	}
	return usage, nil
}

func (s *Store) RecordCacheHit(ctx context.Context, params CacheHitParams) error {
	periodStart, _ := periodAt(s.now())
	_, err := s.pool.Exec(ctx, `INSERT INTO inference_calls(
        request_id,user_id,strategy,state,budget_period_start,model_kind,model_id,model_version,policy_version,latency_ms,completed_at
    ) VALUES ($1,$2,$3,'cache_hit',$4,$5,$6,$7,$8,$9,now())`,
		params.RequestID, params.UserID, params.Strategy, periodStart, params.ModelKind, params.ModelID,
		params.ModelVersion, params.PolicyVersion, params.Latency.Milliseconds())
	if err != nil {
		if isUniqueViolation(err) {
			return ErrDuplicate
		}
		return fmt.Errorf("record cache hit: %w", err)
	}
	return nil
}

func hashKey(apiKey string) [32]byte { return sha256.Sum256([]byte(apiKey)) }

func periodAt(now time.Time) (time.Time, time.Time) {
	now = now.UTC()
	start := time.Date(now.Year(), now.Month(), 1, 0, 0, 0, 0, time.UTC)
	return start, start.AddDate(0, 1, 0)
}

func ensureBudgetPeriod(ctx context.Context, tx pgx.Tx, userID string, periodStart, periodEnd time.Time) error {
	result, err := tx.Exec(ctx, `INSERT INTO budget_periods(user_id,period_start,period_end,limit_microusd)
        SELECT id,$2,$3,monthly_budget_microusd FROM users WHERE id=$1
        ON CONFLICT (user_id,period_start) DO NOTHING`, userID, periodStart, periodEnd)
	if err != nil {
		return fmt.Errorf("ensure budget period: %w", err)
	}
	if result.RowsAffected() == 0 {
		var exists bool
		if err := tx.QueryRow(ctx, `SELECT EXISTS(SELECT 1 FROM users WHERE id=$1)`, userID).Scan(&exists); err != nil {
			return fmt.Errorf("verify budget user: %w", err)
		}
		if !exists {
			return fmt.Errorf("budget user missing")
		}
	}
	return nil
}

func isUniqueViolation(err error) bool {
	var pgErr interface{ SQLState() string }
	return errors.As(err, &pgErr) && pgErr.SQLState() == "23505"
}
