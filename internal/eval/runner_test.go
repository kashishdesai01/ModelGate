package eval

import (
	"context"
	"crypto/sha256"
	"encoding/json"
	"fmt"
	"net/http"
	"net/http/httptest"
	"os"
	"path/filepath"
	"strings"
	"testing"
)

func TestLoadArtifactDigestVerifiesManifestAndArtifact(t *testing.T) {
	modelDir := t.TempDir()
	versionDir := filepath.Join(modelDir, "candidate-v2")
	if err := os.MkdirAll(versionDir, 0o755); err != nil {
		t.Fatal(err)
	}
	artifact := []byte("model-bytes")
	if err := os.WriteFile(filepath.Join(versionDir, "model.joblib"), artifact, 0o600); err != nil {
		t.Fatal(err)
	}
	digest := fmt.Sprintf("%x", sha256.Sum256(artifact))
	manifest := fmt.Sprintf(`{"model_version":"candidate-v2","artifact_sha256":"%s"}`, digest)
	if err := os.WriteFile(filepath.Join(versionDir, "manifest.json"), []byte(manifest), 0o600); err != nil {
		t.Fatal(err)
	}
	loaded, err := LoadArtifactDigest(modelDir, "candidate-v2")
	if err != nil || loaded != digest {
		t.Fatalf("loaded=%q err=%v", loaded, err)
	}
	if err := os.WriteFile(filepath.Join(versionDir, "model.joblib"), []byte("tampered"), 0o600); err != nil {
		t.Fatal(err)
	}
	if _, err := LoadArtifactDigest(modelDir, "candidate-v2"); err == nil {
		t.Fatal("tampered artifact must fail closed")
	}
}

func TestLoadCasesRejectsDuplicateIDs(t *testing.T) {
	path := filepath.Join(t.TempDir(), "golden.jsonl")
	line := `{"case_id":"same","text":"sink leak","expected":{"category":"plumbing","urgency":"soon"},"slices":{"noise":"clean"}}`
	if err := os.WriteFile(path, []byte(line+"\n"+line+"\n"), 0o600); err != nil {
		t.Fatal(err)
	}
	if _, err := LoadCases(path); err == nil {
		t.Fatal("duplicate cases must fail closed")
	}
}

func TestRunnerCallsBothVersionsAndWritesReports(t *testing.T) {
	server := httptest.NewServer(http.HandlerFunc(func(writer http.ResponseWriter, request *http.Request) {
		var payload struct {
			RequestID string `json:"request_id"`
			Text      string `json:"text"`
		}
		if err := json.NewDecoder(request.Body).Decode(&payload); err != nil {
			t.Fatal(err)
		}
		version := "baseline-v1"
		urgency := "emergency"
		if strings.Contains(request.URL.Path, "candidate-v2") {
			version = "candidate-v2"
		}
		response := map[string]any{
			"request_id": payload.RequestID,
			"prediction": map[string]any{"category": "plumbing", "urgency": urgency, "category_confidence": .9, "urgency_confidence": .9},
			"backend":    map[string]any{"kind": "sklearn", "model_id": "home-service-classifier", "model_version": version},
		}
		writer.Header().Set("Content-Type", "application/json")
		_ = json.NewEncoder(writer).Encode(response)
	}))
	defer server.Close()

	dataset := filepath.Join(t.TempDir(), "golden.jsonl")
	content := `{"case_id":"case-1","text":"floding pipe","expected":{"category":"plumbing","urgency":"emergency"},"slices":{"noise":"typo_emergency"}}` + "\n"
	if err := os.WriteFile(dataset, []byte(content), 0o600); err != nil {
		t.Fatal(err)
	}
	report, err := Run(context.Background(), RunConfig{
		DatasetPath: dataset, BaseURL: server.URL, BaselineVersion: "baseline-v1", CandidateVersion: "candidate-v2", Concurrency: 2,
		Gate: GateConfig{MaxMacroF1Regression: .01, MaxEmergencyRecallDrop: .02, CriticalSlice: SliceGate{Key: "noise", Value: "typo_emergency", Metric: "emergency_recall", MaxRegression: .03, MinSupport: 1}},
	})
	if err != nil {
		t.Fatalf("run: %v", err)
	}
	if !report.Passed {
		t.Fatalf("equal predictions should pass: %+v", report.GateReasons)
	}
	output := t.TempDir()
	if err := WriteReports(output, report, RunMetadata{
		DatasetPath: dataset, BaselineVersion: "baseline-v1", CandidateVersion: "candidate-v2",
		BaselineArtifactSHA256: "baseline-sha", CandidateArtifactSHA256: "candidate-sha",
	}); err != nil {
		t.Fatalf("write reports: %v", err)
	}
	for _, name := range []string{"report.json", "report.md", "case_results.jsonl"} {
		if _, err := os.Stat(filepath.Join(output, name)); err != nil {
			t.Fatalf("missing %s: %v", name, err)
		}
	}
	markdown, err := os.ReadFile(filepath.Join(output, "report.md"))
	if err != nil {
		t.Fatalf("read report: %v", err)
	}
	if strings.Contains(string(markdown), "  \n") {
		t.Fatalf("report contains trailing whitespace: %q", markdown)
	}
	if !strings.Contains(string(markdown), "baseline-sha") || !strings.Contains(string(markdown), "candidate-sha") {
		t.Fatalf("report omitted artifact digests: %s", markdown)
	}
	if !strings.Contains(string(markdown), "## Slice metrics") || !strings.Contains(string(markdown), "noise=typo_emergency") {
		t.Fatalf("report omitted slice metrics: %s", markdown)
	}
	if !strings.Contains(string(markdown), "Category accuracy baseline") {
		t.Fatalf("slice table should use per-slice accuracy rather than global-label macro-F1: %s", markdown)
	}
}
