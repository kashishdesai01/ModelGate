package eval

import (
	"testing"

	"github.com/kashishdesai01/modelgate/internal/model"
)

func confidence(value float64) *float64 { return &value }

func TestCompareComputesPairedMetricsAndSliceRegression(t *testing.T) {
	cases := []Case{
		{ID: "1", Expected: model.Prediction{Category: "plumbing", Urgency: "emergency"}, Slices: map[string]string{"noise": "typo_emergency"}},
		{ID: "2", Expected: model.Prediction{Category: "electrical", Urgency: "emergency"}, Slices: map[string]string{"noise": "typo_emergency"}},
		{ID: "3", Expected: model.Prediction{Category: "cleaning", Urgency: "routine"}, Slices: map[string]string{"noise": "clean"}},
		{ID: "4", Expected: model.Prediction{Category: "hvac", Urgency: "soon"}, Slices: map[string]string{"noise": "clean"}},
		{ID: "5", Expected: model.Prediction{Category: "roofing", Urgency: "routine"}, Slices: map[string]string{"noise": "clean"}},
		{ID: "6", Expected: model.Prediction{Category: "painting", Urgency: "soon"}, Slices: map[string]string{"noise": "clean"}},
	}
	baseline := map[string]model.Prediction{
		"1": {Category: "plumbing", Urgency: "emergency", CategoryConfidence: confidence(.9), UrgencyConfidence: confidence(.9)},
		"2": {Category: "electrical", Urgency: "emergency", CategoryConfidence: confidence(.9), UrgencyConfidence: confidence(.9)},
		"3": {Category: "cleaning", Urgency: "soon", CategoryConfidence: confidence(.8), UrgencyConfidence: confidence(.6)},
		"4": {Category: "hvac", Urgency: "routine", CategoryConfidence: confidence(.8), UrgencyConfidence: confidence(.6)},
		"5": {Category: "roofing", Urgency: "soon", CategoryConfidence: confidence(.8), UrgencyConfidence: confidence(.6)},
		"6": {Category: "painting", Urgency: "routine", CategoryConfidence: confidence(.8), UrgencyConfidence: confidence(.6)},
	}
	candidate := map[string]model.Prediction{
		"1": {Category: "plumbing", Urgency: "soon", CategoryConfidence: confidence(.9), UrgencyConfidence: confidence(.8)},
		"2": {Category: "electrical", Urgency: "soon", CategoryConfidence: confidence(.9), UrgencyConfidence: confidence(.8)},
		"3": {Category: "cleaning", Urgency: "routine", CategoryConfidence: confidence(.9), UrgencyConfidence: confidence(.9)},
		"4": {Category: "hvac", Urgency: "soon", CategoryConfidence: confidence(.9), UrgencyConfidence: confidence(.9)},
		"5": {Category: "roofing", Urgency: "routine", CategoryConfidence: confidence(.9), UrgencyConfidence: confidence(.9)},
		"6": {Category: "painting", Urgency: "soon", CategoryConfidence: confidence(.9), UrgencyConfidence: confidence(.9)},
	}

	report, err := Compare(cases, baseline, candidate, GateConfig{
		MaxMacroF1Regression:   0.10,
		MaxEmergencyRecallDrop: 0.25,
		CriticalSlice:          SliceGate{Key: "noise", Value: "typo_emergency", Metric: "urgency_accuracy", MaxRegression: 0.25, MinSupport: 2},
	})
	if err != nil {
		t.Fatalf("compare failed: %v", err)
	}
	if report.Passed {
		t.Fatal("candidate must fail because emergency and critical slice recall regressed")
	}
	if report.Accounting.Total != 6 || report.Accounting.Completed != 6 || report.Accounting.Errors != 0 {
		t.Fatalf("unexpected accounting: %+v", report.Accounting)
	}
	if report.Candidate.UrgencyAccuracy <= report.Baseline.UrgencyAccuracy {
		t.Fatal("fixture should demonstrate aggregate urgency accuracy improving")
	}
	if report.Candidate.EmergencyRecall != 0 || report.Baseline.EmergencyRecall != 1 {
		t.Fatalf("unexpected emergency recall baseline=%v candidate=%v", report.Baseline.EmergencyRecall, report.Candidate.EmergencyRecall)
	}
	if len(report.GateReasons) == 0 {
		t.Fatal("expected actionable gate reasons")
	}
	var typoSlice *SliceReport
	for index := range report.Slices {
		if report.Slices[index].Key == "noise" && report.Slices[index].Value == "typo_emergency" {
			typoSlice = &report.Slices[index]
			break
		}
	}
	if typoSlice == nil || typoSlice.Support != 2 || typoSlice.Baseline.EmergencyRecall != 1 || typoSlice.Candidate.EmergencyRecall != 0 || typoSlice.Delta.EmergencyRecall != -1 {
		t.Fatalf("missing or invalid paired slice metrics: %+v", typoSlice)
	}
}

func TestCompareFailsClosedOnMissingOrInvalidPrediction(t *testing.T) {
	cases := []Case{{ID: "1", Expected: model.Prediction{Category: "plumbing", Urgency: "soon"}}}

	for name, predictions := range map[string]map[string]model.Prediction{
		"missing": {},
		"invalid": {"1": {Category: "made_up", Urgency: "soon"}},
	} {
		t.Run(name, func(t *testing.T) {
			_, err := Compare(cases, map[string]model.Prediction{"1": {Category: "plumbing", Urgency: "soon"}}, predictions, GateConfig{})
			if err == nil {
				t.Fatal("expected comparison to fail closed")
			}
		})
	}
}

func TestMacroF1IncludesAbsentPredictionClasses(t *testing.T) {
	expected := []string{"emergency", "soon", "routine"}
	actual := []string{"soon", "soon", "soon"}
	labels := []string{"routine", "soon", "emergency"}

	value := macroF1(expected, actual, labels)
	if value <= 0 || value >= 1 {
		t.Fatalf("unexpected macro F1 %f", value)
	}
}
