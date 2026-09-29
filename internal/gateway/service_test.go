package gateway

import (
	"context"
	"errors"
	"testing"
	"time"

	"github.com/kashishdesai01/modelgate/internal/model"
)

type fakeBackend struct {
	prediction model.Prediction
	identity   BackendIdentity
	err        error
	calls      int
}

func (f *fakeBackend) Predict(context.Context, PredictionInput) (BackendResult, error) {
	f.calls++
	return BackendResult{Prediction: f.prediction, Identity: f.identity, CostMicroUSD: 100}, f.err
}

func testPrediction(category, urgency string, categoryConfidence, urgencyConfidence float64) model.Prediction {
	return model.Prediction{
		Category: category, Urgency: urgency,
		CategoryConfidence: &categoryConfidence, UrgencyConfidence: &urgencyConfidence,
	}
}

func TestPredictHybridUsesFallbackOnlyWhenConfidenceIsLow(t *testing.T) {
	primary := &fakeBackend{prediction: testPrediction("plumbing", "soon", .40, .90), identity: BackendIdentity{Kind: "sklearn", ModelVersion: "baseline-v1"}}
	fallback := &fakeBackend{prediction: testPrediction("plumbing", "emergency", .95, .95), identity: BackendIdentity{Kind: "llm_mock", ModelVersion: "mock-v1"}}
	service := NewService(primary, fallback, model.Policy{Version: "hybrid-v1", CategoryThreshold: .60, UrgencyThreshold: .65, PreserveEmergency: true}, time.Second)

	result, err := service.Predict(context.Background(), PredictionInput{RequestID: "req-1", Strategy: "hybrid", Text: "burst pipe"})

	if err != nil {
		t.Fatalf("predict failed: %v", err)
	}
	if !result.Routing.FallbackUsed || result.Routing.FallbackReason != "low_category_confidence" {
		t.Fatalf("unexpected routing: %+v", result.Routing)
	}
	if result.Prediction.Urgency != "emergency" || primary.calls != 1 || fallback.calls != 1 {
		t.Fatalf("unexpected result=%+v primary_calls=%d fallback_calls=%d", result, primary.calls, fallback.calls)
	}
}

func TestPredictHybridRetainsPrimaryWhenFallbackFailsValidation(t *testing.T) {
	primary := &fakeBackend{prediction: testPrediction("plumbing", "soon", .40, .90), identity: BackendIdentity{Kind: "sklearn", ModelVersion: "baseline-v1"}}
	fallback := &fakeBackend{prediction: testPrediction("invented", "soon", .95, .95), identity: BackendIdentity{Kind: "llm_mock", ModelVersion: "mock-v1"}}
	service := NewService(primary, fallback, model.Policy{Version: "hybrid-v1", CategoryThreshold: .60, UrgencyThreshold: .65}, time.Second)

	result, err := service.Predict(context.Background(), PredictionInput{RequestID: "req-1", Strategy: "hybrid", Text: "unclear problem"})

	if err != nil {
		t.Fatalf("predict failed: %v", err)
	}
	if result.Prediction.Category != "plumbing" || result.Routing.FallbackError == "" {
		t.Fatalf("primary was not retained with visible fallback error: %+v", result)
	}
}

func TestPredictDirectLLMReturnsConsumedCostWhenResponseIsInvalid(t *testing.T) {
	fallback := &fakeBackend{prediction: testPrediction("invented", "soon", .95, .95), identity: BackendIdentity{Kind: "llm_mock", ModelID: "structured-llm", ModelVersion: "mock-v1"}}
	service := NewService(&fakeBackend{}, fallback, model.Policy{Version: "hybrid-v1"}, time.Second)

	result, err := service.Predict(context.Background(), PredictionInput{RequestID: "req-invalid", Strategy: "llm", Text: "invalid response"})
	if err == nil {
		t.Fatal("expected invalid response error")
	}
	if result.CostMicroUSD != 100 || result.Backend != fallback.identity || result.Routing.PolicyVersion != "hybrid-v1" {
		t.Fatalf("consumed backend metadata was lost: %+v", result)
	}
}

func TestPredictHonorsContextDeadline(t *testing.T) {
	blocking := BackendFunc(func(ctx context.Context, _ PredictionInput) (BackendResult, error) {
		<-ctx.Done()
		return BackendResult{}, ctx.Err()
	})
	service := NewService(blocking, &fakeBackend{}, model.Policy{}, 10*time.Millisecond)

	_, err := service.Predict(context.Background(), PredictionInput{RequestID: "req-1", Strategy: "sklearn", Text: "test"})
	if !errors.Is(err, context.DeadlineExceeded) {
		t.Fatalf("expected deadline, got %v", err)
	}
}

func TestPredictRejectsUnknownStrategyWithoutCallingBackend(t *testing.T) {
	primary := &fakeBackend{}
	service := NewService(primary, &fakeBackend{}, model.Policy{}, time.Second)

	_, err := service.Predict(context.Background(), PredictionInput{RequestID: "req-1", Strategy: "unknown", Text: "test"})
	if !errors.Is(err, ErrInvalidStrategy) {
		t.Fatalf("expected invalid strategy, got %v", err)
	}
	if primary.calls != 0 {
		t.Fatal("backend called after strategy rejection")
	}
}
