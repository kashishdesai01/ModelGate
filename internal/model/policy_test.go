package model

import "testing"

func TestPolicyUsesPrimaryWhenConfident(t *testing.T) {
	policy := Policy{Version: "hybrid-v1", CategoryThreshold: 0.60, UrgencyThreshold: 0.65, PreserveEmergency: true}
	primary := Prediction{Category: "plumbing", Urgency: "soon", CategoryConfidence: ptr(0.91), UrgencyConfidence: ptr(0.88)}

	decision := policy.Decide(primary)

	if decision.UseFallback {
		t.Fatalf("expected primary prediction, got fallback reason %q", decision.Reason)
	}
}

func TestPolicyFallsBackOnEitherLowConfidence(t *testing.T) {
	tests := []struct {
		name       string
		prediction Prediction
		reason     string
	}{
		{
			name:       "category",
			prediction: Prediction{Category: "plumbing", Urgency: "soon", CategoryConfidence: ptr(0.59), UrgencyConfidence: ptr(0.90)},
			reason:     "low_category_confidence",
		},
		{
			name:       "urgency",
			prediction: Prediction{Category: "plumbing", Urgency: "soon", CategoryConfidence: ptr(0.90), UrgencyConfidence: ptr(0.64)},
			reason:     "low_urgency_confidence",
		},
	}

	policy := Policy{Version: "hybrid-v1", CategoryThreshold: 0.60, UrgencyThreshold: 0.65}
	for _, tc := range tests {
		t.Run(tc.name, func(t *testing.T) {
			decision := policy.Decide(tc.prediction)
			if !decision.UseFallback || decision.Reason != tc.reason {
				t.Fatalf("expected fallback reason %q, got %+v", tc.reason, decision)
			}
		})
	}
}

func TestPolicyPreservesPrimaryEmergency(t *testing.T) {
	policy := Policy{Version: "hybrid-v1", CategoryThreshold: 0.60, UrgencyThreshold: 0.65, PreserveEmergency: true}
	primary := Prediction{Category: "electrical", Urgency: "emergency", CategoryConfidence: ptr(0.40), UrgencyConfidence: ptr(0.99)}

	decision := policy.Decide(primary)

	if decision.UseFallback {
		t.Fatal("emergency prediction must not be downgraded through fallback")
	}
	if decision.Reason != "primary_emergency_preserved" {
		t.Fatalf("unexpected reason %q", decision.Reason)
	}
}

func TestValidatePredictionRejectsUnknownLabelsAndConfidence(t *testing.T) {
	valid := Prediction{Category: "plumbing", Urgency: "soon", CategoryConfidence: ptr(0.8), UrgencyConfidence: ptr(0.7)}
	if err := valid.Validate(); err != nil {
		t.Fatalf("valid prediction rejected: %v", err)
	}

	invalid := []Prediction{
		{Category: "urgent", Urgency: "soon"},
		{Category: "plumbing", Urgency: "now"},
		{Category: "plumbing", Urgency: "soon", CategoryConfidence: ptr(1.1)},
	}
	for _, prediction := range invalid {
		if err := prediction.Validate(); err == nil {
			t.Fatalf("expected invalid prediction to fail: %+v", prediction)
		}
	}
}

func ptr(value float64) *float64 { return &value }
