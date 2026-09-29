package main

import (
	"context"
	"errors"
	"flag"
	"fmt"
	"log/slog"
	"net/http"
	"os"
	"os/signal"
	"strconv"
	"syscall"
	"time"

	"github.com/kashishdesai01/modelgate/internal/api"
	"github.com/kashishdesai01/modelgate/internal/backend"
	"github.com/kashishdesai01/modelgate/internal/control"
	evalpkg "github.com/kashishdesai01/modelgate/internal/eval"
	"github.com/kashishdesai01/modelgate/internal/gateway"
	"github.com/kashishdesai01/modelgate/internal/model"
	"github.com/kashishdesai01/modelgate/internal/store"
)

func main() {
	slog.SetDefault(slog.New(slog.NewJSONHandler(os.Stdout, nil)))
	if err := run(os.Args[1:]); err != nil {
		slog.Error("modelgate command failed", "error", err)
		os.Exit(1)
	}
}

func run(args []string) error {
	if len(args) == 0 {
		return fmt.Errorf("usage: modelgate <serve|seed|eval>")
	}
	switch args[0] {
	case "serve":
		return serve()
	case "seed":
		return seed()
	case "eval":
		return evaluate(args[1:])
	default:
		return fmt.Errorf("unknown command %q", args[0])
	}
}

func serve() error {
	ctx := context.Background()
	databaseURL, err := requireEnv("DATABASE_URL")
	if err != nil {
		return err
	}
	cacheSecret, err := requireEnv("CACHE_HMAC_SECRET")
	if err != nil {
		return err
	}
	database, err := store.Open(ctx, databaseURL)
	if err != nil {
		return err
	}
	defer database.Close()
	if err := database.Migrate(ctx); err != nil {
		return err
	}
	controls := control.NewRedisControls(envOr("REDIS_ADDR", "redis:6379"), cacheSecret)
	defer controls.Close()
	if err := controls.Ping(ctx); err != nil {
		return err
	}
	primary := backend.NewClassifier(envOr("CLASSIFIER_URL", "http://classifier:8000"), envOr("CLASSIFIER_VERSION", "baseline-v1"), 3*time.Second)
	fallback := backend.NewStructuredLLM(envOr("LLM_MOCK_URL", "http://llm-mock:8081"), "mock-v1", 5*time.Second)
	service := gateway.NewService(primary, fallback, model.Policy{
		Version: "hybrid-v1", CategoryThreshold: .60, UrgencyThreshold: .65, PreserveEmergency: true,
	}, 6*time.Second)
	handler := api.NewHandler(database, controls, service, api.NewMetrics())
	server := newGatewayServer(envOr("HTTP_ADDR", ":8080"), handler)
	shutdownSignals := make(chan os.Signal, 1)
	signal.Notify(shutdownSignals, syscall.SIGINT, syscall.SIGTERM)
	serverErrors := make(chan error, 1)
	go func() {
		slog.Info("gateway listening", "address", server.Addr)
		serverErrors <- server.ListenAndServe()
	}()
	select {
	case serverErr := <-serverErrors:
		if !errors.Is(serverErr, http.ErrServerClosed) {
			return serverErr
		}
	case signalReceived := <-shutdownSignals:
		slog.Info("shutdown requested", "signal", signalReceived.String())
		shutdownCtx, cancel := context.WithTimeout(context.Background(), 15*time.Second)
		defer cancel()
		if err := server.Shutdown(shutdownCtx); err != nil {
			return fmt.Errorf("graceful shutdown: %w", err)
		}
	}
	return nil
}

func newGatewayServer(address string, handler http.Handler) *http.Server {
	return &http.Server{
		Addr: address, Handler: handler,
		ReadHeaderTimeout: 2 * time.Second, ReadTimeout: 10 * time.Second,
		WriteTimeout: 15 * time.Second, IdleTimeout: 60 * time.Second,
	}
}

func seed() error {
	ctx, cancel := context.WithTimeout(context.Background(), 10*time.Second)
	defer cancel()
	config, err := seedConfigFromEnv()
	if err != nil {
		return err
	}
	database, err := store.Open(ctx, config.DatabaseURL)
	if err != nil {
		return err
	}
	defer database.Close()
	if err := database.Migrate(ctx); err != nil {
		return err
	}
	return database.SeedUser(ctx, store.SeedUserParams{
		UserID: envOr("SEED_USER_ID", "00000000-0000-0000-0000-000000000001"),
		Name:   envOr("SEED_USER_NAME", "demo-user"), APIKey: config.APIKey,
		MonthlyBudgetMicroUSD: config.MonthlyBudgetMicrousd,
		RateCapacity:          config.RateCapacity,
		RateRefillPerSecond:   config.RateRefillPerSecond,
	})
}

type seedConfig struct {
	DatabaseURL           string
	APIKey                string
	MonthlyBudgetMicrousd int64
	RateCapacity          int
	RateRefillPerSecond   float64
}

func seedConfigFromEnv() (seedConfig, error) {
	databaseURL, err := requireEnv("DATABASE_URL")
	if err != nil {
		return seedConfig{}, err
	}
	apiKey, err := requireEnv("SEED_API_KEY")
	if err != nil {
		return seedConfig{}, err
	}
	budget, err := strconv.ParseInt(envOr("SEED_MONTHLY_BUDGET_MICROUSD", "100000"), 10, 64)
	if err != nil || budget < 0 {
		return seedConfig{}, fmt.Errorf("SEED_MONTHLY_BUDGET_MICROUSD must be a nonnegative integer")
	}
	capacity, err := strconv.Atoi(envOr("SEED_RATE_CAPACITY", "20"))
	if err != nil || capacity <= 0 {
		return seedConfig{}, fmt.Errorf("SEED_RATE_CAPACITY must be a positive integer")
	}
	refill, err := strconv.ParseFloat(envOr("SEED_RATE_REFILL_PER_SEC", "5"), 64)
	if err != nil || refill <= 0 {
		return seedConfig{}, fmt.Errorf("SEED_RATE_REFILL_PER_SEC must be positive")
	}
	return seedConfig{
		DatabaseURL: databaseURL, APIKey: apiKey, MonthlyBudgetMicrousd: budget,
		RateCapacity: capacity, RateRefillPerSecond: refill,
	}, nil
}

func evaluate(args []string) error {
	flags := flag.NewFlagSet("eval", flag.ContinueOnError)
	dataset := flags.String("dataset", "datasets/golden.jsonl", "golden-set JSONL")
	baseURL := flags.String("classifier-url", "http://localhost:8000", "classifier base URL")
	baseline := flags.String("baseline", "baseline-v1", "baseline model version")
	candidate := flags.String("candidate", "candidate-v2", "candidate model version")
	modelDir := flags.String("model-dir", "services/classifier/models", "directory containing versioned model artifacts")
	output := flags.String("output", "artifacts/eval", "report output directory")
	concurrency := flags.Int("concurrency", 8, "bounded worker count")
	if err := flags.Parse(args); err != nil {
		return err
	}
	baselineDigest, err := evalpkg.LoadArtifactDigest(*modelDir, *baseline)
	if err != nil {
		return fmt.Errorf("verify baseline artifact: %w", err)
	}
	candidateDigest, err := evalpkg.LoadArtifactDigest(*modelDir, *candidate)
	if err != nil {
		return fmt.Errorf("verify candidate artifact: %w", err)
	}
	ctx, cancel := signal.NotifyContext(context.Background(), syscall.SIGINT, syscall.SIGTERM)
	defer cancel()
	report, err := evalpkg.Run(ctx, evalpkg.RunConfig{
		DatasetPath: *dataset, BaseURL: *baseURL, BaselineVersion: *baseline, CandidateVersion: *candidate, Concurrency: *concurrency,
		Gate: evalpkg.GateConfig{
			MaxMacroF1Regression: .02, MaxEmergencyRecallDrop: .03,
			CriticalSlice: evalpkg.SliceGate{Key: "noise", Value: "typo_emergency", Metric: "emergency_recall", MaxRegression: .03, MinSupport: 30},
		},
	})
	if err != nil {
		return err
	}
	metadata := evalpkg.RunMetadata{
		DatasetPath: *dataset, BaselineVersion: *baseline, CandidateVersion: *candidate,
		BaselineArtifactSHA256: baselineDigest, CandidateArtifactSHA256: candidateDigest,
	}
	if err := evalpkg.WriteReports(*output, report, metadata); err != nil {
		return err
	}
	fmt.Printf("evaluation passed=%t cases=%d report=%s/report.md\n", report.Passed, report.Accounting.Total, *output)
	if !report.Passed {
		return fmt.Errorf("evaluation regression gate failed: %v", report.GateReasons)
	}
	return nil
}

func envOr(name, fallback string) string {
	if value := os.Getenv(name); value != "" {
		return value
	}
	return fallback
}

func requireEnv(name string) (string, error) {
	value := os.Getenv(name)
	if value == "" {
		return "", fmt.Errorf("required environment variable is not set: %s", name)
	}
	return value, nil
}
