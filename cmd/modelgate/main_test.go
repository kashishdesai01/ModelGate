package main

import (
	"net/http"
	"testing"
)

func TestGatewayServerHasBoundedRequestTimeouts(t *testing.T) {
	server := newGatewayServer(":0", http.NotFoundHandler())
	if server.ReadHeaderTimeout <= 0 || server.ReadTimeout <= 0 || server.WriteTimeout <= 0 || server.IdleTimeout <= 0 {
		t.Fatalf("server timeouts are incomplete: %+v", server)
	}
}

func TestSeedConfigFromEnvironment(t *testing.T) {
	t.Setenv("DATABASE_URL", "postgres://example")
	t.Setenv("SEED_API_KEY", "test-key")
	t.Setenv("SEED_MONTHLY_BUDGET_MICROUSD", "250000")
	t.Setenv("SEED_RATE_CAPACITY", "750")
	t.Setenv("SEED_RATE_REFILL_PER_SEC", "125.5")

	config, err := seedConfigFromEnv()
	if err != nil {
		t.Fatalf("seedConfigFromEnv: %v", err)
	}
	if config.DatabaseURL != "postgres://example" || config.APIKey != "test-key" {
		t.Fatalf("unexpected required values: %+v", config)
	}
	if config.MonthlyBudgetMicrousd != 250000 || config.RateCapacity != 750 || config.RateRefillPerSecond != 125.5 {
		t.Fatalf("unexpected numeric values: %+v", config)
	}
}

func TestSeedConfigRejectsInvalidRatePolicy(t *testing.T) {
	t.Setenv("DATABASE_URL", "postgres://example")
	t.Setenv("SEED_API_KEY", "test-key")
	t.Setenv("SEED_RATE_CAPACITY", "0")

	if _, err := seedConfigFromEnv(); err == nil {
		t.Fatal("expected zero capacity to be rejected")
	}
}
