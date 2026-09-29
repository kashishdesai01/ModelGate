package eval

import (
	"fmt"
	"sort"

	"github.com/kashishdesai01/modelgate/internal/model"
)

var categoryLabels = []string{"plumbing", "electrical", "hvac", "cleaning", "landscaping", "roofing", "painting", "appliance_repair", "handyman", "other"}
var urgencyLabels = []string{"routine", "soon", "emergency"}

type Case struct {
	SchemaVersion       string            `json:"schema_version,omitempty"`
	ID                  string            `json:"case_id"`
	HouseholdScenarioID string            `json:"household_scenario_id,omitempty"`
	Text                string            `json:"text"`
	Expected            model.Prediction  `json:"expected"`
	Slices              map[string]string `json:"slices"`
	Provenance          map[string]any    `json:"provenance,omitempty"`
}

type SliceGate struct {
	Key           string  `json:"key"`
	Value         string  `json:"value"`
	Metric        string  `json:"metric"`
	MaxRegression float64 `json:"max_regression"`
	MinSupport    int     `json:"min_support"`
}

type GateConfig struct {
	MaxMacroF1Regression   float64   `json:"max_macro_f1_regression"`
	MaxEmergencyRecallDrop float64   `json:"max_emergency_recall_drop"`
	CriticalSlice          SliceGate `json:"critical_slice"`
}

type Metrics struct {
	CategoryAccuracy float64 `json:"category_accuracy"`
	UrgencyAccuracy  float64 `json:"urgency_accuracy"`
	CategoryMacroF1  float64 `json:"category_macro_f1"`
	UrgencyMacroF1   float64 `json:"urgency_macro_f1"`
	EmergencyRecall  float64 `json:"emergency_recall"`
}

type Accounting struct {
	Total     int `json:"total"`
	Completed int `json:"completed"`
	Errors    int `json:"errors"`
}

type CaseResult struct {
	CaseID    string            `json:"case_id"`
	Expected  model.Prediction  `json:"expected"`
	Baseline  model.Prediction  `json:"baseline"`
	Candidate model.Prediction  `json:"candidate"`
	Slices    map[string]string `json:"slices"`
}

type SliceReport struct {
	Key       string  `json:"key"`
	Value     string  `json:"value"`
	Support   int     `json:"support"`
	Baseline  Metrics `json:"baseline"`
	Candidate Metrics `json:"candidate"`
	Delta     Metrics `json:"delta"`
}

type Report struct {
	Passed      bool          `json:"passed"`
	Baseline    Metrics       `json:"baseline"`
	Candidate   Metrics       `json:"candidate"`
	Accounting  Accounting    `json:"accounting"`
	GateReasons []string      `json:"gate_reasons"`
	Slices      []SliceReport `json:"slices"`
	Cases       []CaseResult  `json:"cases"`
}

func Compare(cases []Case, baseline, candidate map[string]model.Prediction, gate GateConfig) (Report, error) {
	if len(cases) == 0 {
		return Report{}, fmt.Errorf("golden set is empty")
	}
	seen := make(map[string]struct{}, len(cases))
	results := make([]CaseResult, 0, len(cases))
	for _, testCase := range cases {
		if testCase.ID == "" {
			return Report{}, fmt.Errorf("case has empty ID")
		}
		if _, duplicate := seen[testCase.ID]; duplicate {
			return Report{}, fmt.Errorf("duplicate case ID %q", testCase.ID)
		}
		seen[testCase.ID] = struct{}{}
		if err := testCase.Expected.Validate(); err != nil {
			return Report{}, fmt.Errorf("case %s has invalid expected result: %w", testCase.ID, err)
		}
		base, ok := baseline[testCase.ID]
		if !ok {
			return Report{}, fmt.Errorf("baseline missing case %s", testCase.ID)
		}
		cand, ok := candidate[testCase.ID]
		if !ok {
			return Report{}, fmt.Errorf("candidate missing case %s", testCase.ID)
		}
		if err := base.Validate(); err != nil {
			return Report{}, fmt.Errorf("baseline case %s invalid: %w", testCase.ID, err)
		}
		if err := cand.Validate(); err != nil {
			return Report{}, fmt.Errorf("candidate case %s invalid: %w", testCase.ID, err)
		}
		results = append(results, CaseResult{CaseID: testCase.ID, Expected: testCase.Expected, Baseline: base, Candidate: cand, Slices: testCase.Slices})
	}

	report := Report{
		Passed:     true,
		Baseline:   computeMetrics(results, false),
		Candidate:  computeMetrics(results, true),
		Accounting: Accounting{Total: len(cases), Completed: len(results)},
		Slices:     computeSliceReports(results),
		Cases:      results,
	}
	if report.Baseline.CategoryMacroF1-report.Candidate.CategoryMacroF1 > gate.MaxMacroF1Regression {
		report.GateReasons = append(report.GateReasons, fmt.Sprintf("category macro-F1 regressed by %.4f", report.Baseline.CategoryMacroF1-report.Candidate.CategoryMacroF1))
	}
	if report.Baseline.UrgencyMacroF1-report.Candidate.UrgencyMacroF1 > gate.MaxMacroF1Regression {
		report.GateReasons = append(report.GateReasons, fmt.Sprintf("urgency macro-F1 regressed by %.4f", report.Baseline.UrgencyMacroF1-report.Candidate.UrgencyMacroF1))
	}
	if report.Baseline.EmergencyRecall-report.Candidate.EmergencyRecall > gate.MaxEmergencyRecallDrop {
		report.GateReasons = append(report.GateReasons, fmt.Sprintf("emergency recall regressed by %.4f", report.Baseline.EmergencyRecall-report.Candidate.EmergencyRecall))
	}
	if reason := evaluateSliceGate(results, gate.CriticalSlice); reason != "" {
		report.GateReasons = append(report.GateReasons, reason)
	}
	sort.Strings(report.GateReasons)
	report.Passed = len(report.GateReasons) == 0
	return report, nil
}

func computeSliceReports(results []CaseResult) []SliceReport {
	grouped := make(map[string][]CaseResult)
	identities := make(map[string][2]string)
	for _, result := range results {
		for key, value := range result.Slices {
			identity := key + "\x00" + value
			grouped[identity] = append(grouped[identity], result)
			identities[identity] = [2]string{key, value}
		}
	}
	keys := make([]string, 0, len(grouped))
	for identity := range grouped {
		keys = append(keys, identity)
	}
	sort.Strings(keys)
	reports := make([]SliceReport, 0, len(keys))
	for _, identity := range keys {
		rows := grouped[identity]
		baseline := computeMetrics(rows, false)
		candidate := computeMetrics(rows, true)
		parts := identities[identity]
		reports = append(reports, SliceReport{
			Key: parts[0], Value: parts[1], Support: len(rows), Baseline: baseline, Candidate: candidate,
			Delta: metricDelta(baseline, candidate),
		})
	}
	return reports
}

func metricDelta(baseline, candidate Metrics) Metrics {
	return Metrics{
		CategoryAccuracy: candidate.CategoryAccuracy - baseline.CategoryAccuracy,
		UrgencyAccuracy:  candidate.UrgencyAccuracy - baseline.UrgencyAccuracy,
		CategoryMacroF1:  candidate.CategoryMacroF1 - baseline.CategoryMacroF1,
		UrgencyMacroF1:   candidate.UrgencyMacroF1 - baseline.UrgencyMacroF1,
		EmergencyRecall:  candidate.EmergencyRecall - baseline.EmergencyRecall,
	}
}

func computeMetrics(results []CaseResult, useCandidate bool) Metrics {
	categoriesExpected := make([]string, 0, len(results))
	categoriesActual := make([]string, 0, len(results))
	urgenciesExpected := make([]string, 0, len(results))
	urgenciesActual := make([]string, 0, len(results))
	categoryCorrect, urgencyCorrect := 0, 0
	for _, result := range results {
		actual := result.Baseline
		if useCandidate {
			actual = result.Candidate
		}
		categoriesExpected = append(categoriesExpected, result.Expected.Category)
		categoriesActual = append(categoriesActual, actual.Category)
		urgenciesExpected = append(urgenciesExpected, result.Expected.Urgency)
		urgenciesActual = append(urgenciesActual, actual.Urgency)
		if result.Expected.Category == actual.Category {
			categoryCorrect++
		}
		if result.Expected.Urgency == actual.Urgency {
			urgencyCorrect++
		}
	}
	return Metrics{
		CategoryAccuracy: ratio(categoryCorrect, len(results)),
		UrgencyAccuracy:  ratio(urgencyCorrect, len(results)),
		CategoryMacroF1:  macroF1(categoriesExpected, categoriesActual, categoryLabels),
		UrgencyMacroF1:   macroF1(urgenciesExpected, urgenciesActual, urgencyLabels),
		EmergencyRecall:  recallForLabel(urgenciesExpected, urgenciesActual, "emergency"),
	}
}

func evaluateSliceGate(results []CaseResult, gate SliceGate) string {
	if gate.Key == "" || gate.Metric == "" {
		return ""
	}
	selected := make([]CaseResult, 0)
	for _, result := range results {
		if result.Slices[gate.Key] == gate.Value {
			selected = append(selected, result)
		}
	}
	if len(selected) < gate.MinSupport {
		return fmt.Sprintf("critical slice %s=%s has support %d below required %d", gate.Key, gate.Value, len(selected), gate.MinSupport)
	}
	base := computeMetrics(selected, false)
	cand := computeMetrics(selected, true)
	var regression float64
	switch gate.Metric {
	case "urgency_accuracy":
		regression = base.UrgencyAccuracy - cand.UrgencyAccuracy
	case "emergency_recall":
		regression = base.EmergencyRecall - cand.EmergencyRecall
	case "category_accuracy":
		regression = base.CategoryAccuracy - cand.CategoryAccuracy
	default:
		return fmt.Sprintf("unknown critical slice metric %q", gate.Metric)
	}
	if regression > gate.MaxRegression {
		return fmt.Sprintf("critical slice %s=%s %s regressed by %.4f (support=%d)", gate.Key, gate.Value, gate.Metric, regression, len(selected))
	}
	return ""
}

func macroF1(expected, actual, labels []string) float64 {
	if len(expected) == 0 || len(expected) != len(actual) || len(labels) == 0 {
		return 0
	}
	total := 0.0
	for _, label := range labels {
		tp, fp, fn := 0, 0, 0
		for i := range expected {
			switch {
			case expected[i] == label && actual[i] == label:
				tp++
			case expected[i] != label && actual[i] == label:
				fp++
			case expected[i] == label && actual[i] != label:
				fn++
			}
		}
		denominator := 2*tp + fp + fn
		if denominator > 0 {
			total += float64(2*tp) / float64(denominator)
		}
	}
	return total / float64(len(labels))
}

func recallForLabel(expected, actual []string, label string) float64 {
	tp, fn := 0, 0
	for i := range expected {
		if expected[i] != label {
			continue
		}
		if actual[i] == label {
			tp++
		} else {
			fn++
		}
	}
	return ratio(tp, tp+fn)
}

func ratio(numerator, denominator int) float64 {
	if denominator == 0 {
		return 0
	}
	return float64(numerator) / float64(denominator)
}
