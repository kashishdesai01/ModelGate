package gateway

import (
	"context"
	"errors"
	"fmt"
	"time"

	"github.com/kashishdesai01/modelgate/internal/model"
)

var (
	ErrInvalidStrategy = errors.New("invalid strategy")
	ErrOverloaded      = errors.New("backend overloaded")
)

type PredictionInput struct {
	RequestID string `json:"request_id"`
	Strategy  string `json:"strategy"`
	Text      string `json:"text"`
}

type BackendIdentity struct {
	Kind         string `json:"kind"`
	ModelID      string `json:"model_id"`
	ModelVersion string `json:"model_version"`
}

type BackendResult struct {
	Prediction   model.Prediction
	Identity     BackendIdentity
	CostMicroUSD int64
}

type Backend interface {
	Predict(context.Context, PredictionInput) (BackendResult, error)
}

type BackendFunc func(context.Context, PredictionInput) (BackendResult, error)

func (f BackendFunc) Predict(ctx context.Context, input PredictionInput) (BackendResult, error) {
	return f(ctx, input)
}

type RoutingInfo struct {
	PolicyVersion  string `json:"policy_version"`
	FallbackUsed   bool   `json:"fallback_used"`
	FallbackReason string `json:"fallback_reason,omitempty"`
	FallbackError  string `json:"fallback_error,omitempty"`
}

type PredictionResult struct {
	RequestID    string           `json:"request_id"`
	Prediction   model.Prediction `json:"prediction"`
	Backend      BackendIdentity  `json:"backend"`
	Routing      RoutingInfo      `json:"routing"`
	CostMicroUSD int64            `json:"cost_microusd"`
}

type Service struct {
	primary       Backend
	fallback      Backend
	policy        model.Policy
	timeout       time.Duration
	primarySlots  chan struct{}
	fallbackSlots chan struct{}
}

func NewService(primary, fallback Backend, policy model.Policy, timeout time.Duration) *Service {
	return &Service{
		primary: primary, fallback: fallback, policy: policy, timeout: timeout,
		primarySlots: make(chan struct{}, 32), fallbackSlots: make(chan struct{}, 8),
	}
}

type identityProvider interface {
	Identity() BackendIdentity
}

func (s *Service) CacheNamespace() string {
	return fmt.Sprintf("policy=%s:category=%.4f:urgency=%.4f:preserve_emergency=%t:primary=%s:fallback=%s",
		s.policy.Version, s.policy.CategoryThreshold, s.policy.UrgencyThreshold, s.policy.PreserveEmergency,
		backendNamespace(s.primary), backendNamespace(s.fallback))
}

func backendNamespace(backend Backend) string {
	if identified, ok := backend.(identityProvider); ok {
		identity := identified.Identity()
		return identity.Kind + "/" + identity.ModelID + "/" + identity.ModelVersion
	}
	return fmt.Sprintf("%T", backend)
}

func (s *Service) Predict(ctx context.Context, input PredictionInput) (PredictionResult, error) {
	if input.Strategy != "sklearn" && input.Strategy != "llm" && input.Strategy != "hybrid" {
		return PredictionResult{}, ErrInvalidStrategy
	}
	ctx, cancel := context.WithTimeout(ctx, s.timeout)
	defer cancel()

	if input.Strategy == "llm" {
		result, err := callBounded(ctx, s.fallbackSlots, s.fallback, input)
		if err != nil {
			return PredictionResult{}, err
		}
		if err := result.Prediction.Validate(); err != nil {
			return PredictionResult{
				RequestID: input.RequestID, Backend: result.Identity, CostMicroUSD: result.CostMicroUSD,
				Routing: RoutingInfo{PolicyVersion: s.policy.Version},
			}, fmt.Errorf("invalid fallback response: %w", err)
		}
		return PredictionResult{RequestID: input.RequestID, Prediction: result.Prediction, Backend: result.Identity, CostMicroUSD: result.CostMicroUSD, Routing: RoutingInfo{PolicyVersion: s.policy.Version}}, nil
	}

	primary, err := callBounded(ctx, s.primarySlots, s.primary, input)
	if err != nil {
		return PredictionResult{}, err
	}
	if err := primary.Prediction.Validate(); err != nil {
		return PredictionResult{
			RequestID: input.RequestID, Backend: primary.Identity, CostMicroUSD: primary.CostMicroUSD,
			Routing: RoutingInfo{PolicyVersion: s.policy.Version},
		}, fmt.Errorf("invalid primary response: %w", err)
	}
	result := PredictionResult{
		RequestID: input.RequestID, Prediction: primary.Prediction, Backend: primary.Identity,
		CostMicroUSD: primary.CostMicroUSD, Routing: RoutingInfo{PolicyVersion: s.policy.Version},
	}
	if input.Strategy == "sklearn" {
		return result, nil
	}

	decision := s.policy.Decide(primary.Prediction)
	result.Routing.FallbackReason = decision.Reason
	if !decision.UseFallback {
		return result, nil
	}
	fallback, err := callBounded(ctx, s.fallbackSlots, s.fallback, input)
	if err != nil {
		result.Routing.FallbackError = classifyFallbackError(err)
		return result, nil
	}
	result.CostMicroUSD += fallback.CostMicroUSD
	if err := fallback.Prediction.Validate(); err != nil {
		result.Routing.FallbackError = "invalid_response"
		return result, nil
	}
	result.Prediction = fallback.Prediction
	result.Backend = fallback.Identity
	result.Routing.FallbackUsed = true
	return result, nil
}

func callBounded(ctx context.Context, slots chan struct{}, backend Backend, input PredictionInput) (BackendResult, error) {
	select {
	case slots <- struct{}{}:
		defer func() { <-slots }()
	case <-ctx.Done():
		return BackendResult{}, ctx.Err()
	case <-time.After(100 * time.Millisecond):
		return BackendResult{}, ErrOverloaded
	}
	return backend.Predict(ctx, input)
}

func classifyFallbackError(err error) string {
	if errors.Is(err, context.DeadlineExceeded) || errors.Is(err, context.Canceled) {
		return "timeout"
	}
	return "backend_error"
}
