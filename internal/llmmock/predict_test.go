package llmmock

import "testing"

func TestPredictIsDeterministicAndStructured(t *testing.T) {
	first, err := Predict("Water is flooding from a burst pipe")
	if err != nil {
		t.Fatalf("predict: %v", err)
	}
	second, err := Predict("Water is flooding from a burst pipe")
	if err != nil {
		t.Fatalf("predict twice: %v", err)
	}
	if first.Category != second.Category || first.Urgency != second.Urgency ||
		first.CategoryConfidence == nil || second.CategoryConfidence == nil ||
		*first.CategoryConfidence != *second.CategoryConfidence ||
		first.UrgencyConfidence == nil || second.UrgencyConfidence == nil ||
		*first.UrgencyConfidence != *second.UrgencyConfidence {
		t.Fatalf("mock must be deterministic: first=%+v second=%+v", first, second)
	}
	if first.Category != "plumbing" || first.Urgency != "emergency" {
		t.Fatalf("unexpected prediction: %+v", first)
	}
}

func TestPredictSupportsExplicitFailureFixtures(t *testing.T) {
	if _, err := Predict("[mock:error]"); err == nil {
		t.Fatal("expected explicit mock error")
	}
	invalid, err := Predict("[mock:invalid]")
	if err != nil {
		t.Fatalf("invalid fixture should be returned for gateway validation: %v", err)
	}
	if invalid.Category == "plumbing" {
		t.Fatal("invalid fixture was not emitted")
	}
}
