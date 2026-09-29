package control

import (
	"context"
	"os"
	"testing"
	"time"
)

func TestTokenBucketIsAtomicAndFailsClosed(t *testing.T) {
	address := os.Getenv("MODEL_GATE_TEST_REDIS_ADDR")
	if address == "" {
		t.Skip("MODEL_GATE_TEST_REDIS_ADDR is not set")
	}
	ctx := context.Background()
	controls := NewRedisControls(address, "integration-secret")
	defer controls.Close()
	if err := controls.FlushForTest(ctx); err != nil {
		t.Fatalf("flush: %v", err)
	}

	for index := 0; index < 3; index++ {
		decision, err := controls.Allow(ctx, "user-1", 3, .01)
		if err != nil || !decision.Allowed {
			t.Fatalf("request %d unexpectedly denied: decision=%+v err=%v", index, decision, err)
		}
	}
	decision, err := controls.Allow(ctx, "user-1", 3, .01)
	if err != nil {
		t.Fatalf("fourth decision: %v", err)
	}
	if decision.Allowed || decision.RetryAfter <= 0 {
		t.Fatalf("fourth request must be denied with retry: %+v", decision)
	}
}

func TestCacheKeysAreIsolatedByUserStrategyAndText(t *testing.T) {
	address := os.Getenv("MODEL_GATE_TEST_REDIS_ADDR")
	if address == "" {
		t.Skip("MODEL_GATE_TEST_REDIS_ADDR is not set")
	}
	ctx := context.Background()
	controls := NewRedisControls(address, "integration-secret")
	defer controls.Close()
	if err := controls.FlushForTest(ctx); err != nil {
		t.Fatalf("flush: %v", err)
	}

	if err := controls.CacheSet(ctx, "user-a", "hybrid", "sink leak", []byte(`{"ok":true}`), time.Minute); err != nil {
		t.Fatalf("cache set: %v", err)
	}
	if _, found, err := controls.CacheGet(ctx, "user-a", "hybrid", "sink leak"); err != nil || !found {
		t.Fatalf("cache miss for exact key: found=%v err=%v", found, err)
	}
	for _, changed := range []struct{ user, strategy, text string }{
		{"user-b", "hybrid", "sink leak"},
		{"user-a", "sklearn", "sink leak"},
		{"user-a", "hybrid", "roof leak"},
	} {
		if _, found, err := controls.CacheGet(ctx, changed.user, changed.strategy, changed.text); err != nil || found {
			t.Fatalf("cache crossed boundary %+v: found=%v err=%v", changed, found, err)
		}
	}
}
