package eval

import (
	"bufio"
	"bytes"
	"context"
	"crypto/sha256"
	"encoding/hex"
	"encoding/json"
	"fmt"
	"io"
	"net/http"
	"net/url"
	"os"
	"path/filepath"
	"strings"
	"sync"
	"time"

	"github.com/kashishdesai01/modelgate/internal/model"
)

type RunConfig struct {
	DatasetPath      string
	BaseURL          string
	BaselineVersion  string
	CandidateVersion string
	Concurrency      int
	Gate             GateConfig
}

type RunMetadata struct {
	DatasetPath             string `json:"dataset_path"`
	DatasetSHA256           string `json:"dataset_sha256"`
	BaselineVersion         string `json:"baseline_version"`
	BaselineArtifactSHA256  string `json:"baseline_artifact_sha256"`
	CandidateVersion        string `json:"candidate_version"`
	CandidateArtifactSHA256 string `json:"candidate_artifact_sha256"`
}

type reportEnvelope struct {
	Metadata RunMetadata `json:"metadata"`
	Report   Report      `json:"report"`
}

func LoadCases(path string) ([]Case, error) {
	file, err := os.Open(path)
	if err != nil {
		return nil, fmt.Errorf("open dataset: %w", err)
	}
	defer file.Close()
	scanner := bufio.NewScanner(file)
	scanner.Buffer(make([]byte, 64*1024), 1<<20)
	cases := make([]Case, 0)
	seen := make(map[string]struct{})
	line := 0
	for scanner.Scan() {
		line++
		if strings.TrimSpace(scanner.Text()) == "" {
			continue
		}
		var testCase Case
		decoder := json.NewDecoder(strings.NewReader(scanner.Text()))
		decoder.DisallowUnknownFields()
		if err := decoder.Decode(&testCase); err != nil {
			return nil, fmt.Errorf("decode dataset line %d: %w", line, err)
		}
		if testCase.ID == "" || testCase.Text == "" {
			return nil, fmt.Errorf("dataset line %d has empty case_id or text", line)
		}
		if _, duplicate := seen[testCase.ID]; duplicate {
			return nil, fmt.Errorf("duplicate case ID %q", testCase.ID)
		}
		if err := testCase.Expected.Validate(); err != nil {
			return nil, fmt.Errorf("invalid expected prediction for %s: %w", testCase.ID, err)
		}
		seen[testCase.ID] = struct{}{}
		cases = append(cases, testCase)
	}
	if err := scanner.Err(); err != nil {
		return nil, fmt.Errorf("scan dataset: %w", err)
	}
	if len(cases) == 0 {
		return nil, fmt.Errorf("dataset is empty")
	}
	return cases, nil
}

func Run(ctx context.Context, config RunConfig) (Report, error) {
	cases, err := LoadCases(config.DatasetPath)
	if err != nil {
		return Report{}, err
	}
	if config.Concurrency <= 0 || config.Concurrency > 64 {
		return Report{}, fmt.Errorf("concurrency must be between 1 and 64")
	}
	baseURL, err := url.Parse(config.BaseURL)
	if err != nil || baseURL.Scheme == "" || baseURL.Host == "" {
		return Report{}, fmt.Errorf("base URL must be absolute")
	}
	client := &http.Client{Timeout: 5 * time.Second, CheckRedirect: func(_ *http.Request, _ []*http.Request) error { return http.ErrUseLastResponse }}
	baseline := make(map[string]model.Prediction, len(cases))
	candidate := make(map[string]model.Prediction, len(cases))
	var mu sync.Mutex
	jobs := make(chan Case)
	errCh := make(chan error, 1)
	worker := func() {
		for testCase := range jobs {
			base, runErr := fetchPrediction(ctx, client, baseURL, config.BaselineVersion, testCase)
			if runErr != nil {
				select {
				case errCh <- runErr:
				default:
				}
				continue
			}
			cand, runErr := fetchPrediction(ctx, client, baseURL, config.CandidateVersion, testCase)
			if runErr != nil {
				select {
				case errCh <- runErr:
				default:
				}
				continue
			}
			mu.Lock()
			baseline[testCase.ID] = base
			candidate[testCase.ID] = cand
			mu.Unlock()
		}
	}
	var wg sync.WaitGroup
	wg.Add(config.Concurrency)
	for index := 0; index < config.Concurrency; index++ {
		go func() { defer wg.Done(); worker() }()
	}
	for _, testCase := range cases {
		select {
		case jobs <- testCase:
		case <-ctx.Done():
			close(jobs)
			wg.Wait()
			return Report{}, ctx.Err()
		}
	}
	close(jobs)
	wg.Wait()
	select {
	case runErr := <-errCh:
		return Report{}, runErr
	default:
	}
	return Compare(cases, baseline, candidate, config.Gate)
}

type predictionAPIResponse struct {
	RequestID  string           `json:"request_id"`
	Prediction model.Prediction `json:"prediction"`
	Backend    struct {
		Kind         string `json:"kind"`
		ModelID      string `json:"model_id"`
		ModelVersion string `json:"model_version"`
	} `json:"backend"`
}

func fetchPrediction(ctx context.Context, client *http.Client, baseURL *url.URL, version string, testCase Case) (model.Prediction, error) {
	payload, _ := json.Marshal(map[string]string{"request_id": testCase.ID, "text": testCase.Text})
	endpoint := *baseURL
	endpoint.Path = strings.TrimRight(endpoint.Path, "/") + "/v1/models/" + url.PathEscape(version) + "/predict"
	request, err := http.NewRequestWithContext(ctx, http.MethodPost, endpoint.String(), bytes.NewReader(payload))
	if err != nil {
		return model.Prediction{}, err
	}
	request.Header.Set("Content-Type", "application/json")
	response, err := client.Do(request)
	if err != nil {
		return model.Prediction{}, fmt.Errorf("case %s version %s: %w", testCase.ID, version, err)
	}
	defer response.Body.Close()
	body, err := io.ReadAll(io.LimitReader(response.Body, (1<<20)+1))
	if err != nil {
		return model.Prediction{}, err
	}
	if len(body) > 1<<20 || response.StatusCode != http.StatusOK {
		return model.Prediction{}, fmt.Errorf("case %s version %s returned status %d or oversized body", testCase.ID, version, response.StatusCode)
	}
	decoder := json.NewDecoder(bytes.NewReader(body))
	decoder.DisallowUnknownFields()
	var decoded predictionAPIResponse
	if err := decoder.Decode(&decoded); err != nil {
		return model.Prediction{}, fmt.Errorf("decode case %s: %w", testCase.ID, err)
	}
	if decoded.RequestID != testCase.ID || decoded.Backend.ModelVersion != version {
		return model.Prediction{}, fmt.Errorf("case %s response identity mismatch", testCase.ID)
	}
	if err := decoded.Prediction.Validate(); err != nil {
		return model.Prediction{}, fmt.Errorf("case %s invalid prediction: %w", testCase.ID, err)
	}
	return decoded.Prediction, nil
}

func LoadArtifactDigest(modelDir, version string) (string, error) {
	if version == "" || filepath.Base(version) != version {
		return "", fmt.Errorf("invalid model version %q", version)
	}
	manifestPath := filepath.Join(modelDir, version, "manifest.json")
	manifestBytes, err := os.ReadFile(manifestPath)
	if err != nil {
		return "", fmt.Errorf("read model manifest: %w", err)
	}
	var manifest struct {
		ModelID               string            `json:"model_id"`
		ModelVersion          string            `json:"model_version"`
		ArtifactSHA256        string            `json:"artifact_sha256"`
		TrainingDatasetSHA256 string            `json:"training_dataset_sha256"`
		TrainingSources       map[string]string `json:"training_sources"`
		LabelSchemaVersion    string            `json:"label_schema_version"`
		RandomSeed            int               `json:"random_seed"`
		PythonVersion         string            `json:"python_version"`
		ScikitLearnVersion    string            `json:"scikit_learn_version"`
		KnownBadFixture       bool              `json:"known_bad_fixture"`
	}
	decoder := json.NewDecoder(bytes.NewReader(manifestBytes))
	decoder.DisallowUnknownFields()
	if err := decoder.Decode(&manifest); err != nil {
		return "", fmt.Errorf("decode model manifest: %w", err)
	}
	if manifest.ModelVersion != version || len(manifest.ArtifactSHA256) != 64 {
		return "", fmt.Errorf("model manifest identity or digest is invalid")
	}
	actual, err := fileSHA256(filepath.Join(modelDir, version, "model.joblib"))
	if err != nil {
		return "", fmt.Errorf("hash model artifact: %w", err)
	}
	if actual != manifest.ArtifactSHA256 {
		return "", fmt.Errorf("model artifact digest mismatch for %s", version)
	}
	return actual, nil
}

func WriteReports(directory string, report Report, metadata RunMetadata) error {
	if err := os.MkdirAll(directory, 0o755); err != nil {
		return fmt.Errorf("create report directory: %w", err)
	}
	if metadata.DatasetSHA256 == "" && metadata.DatasetPath != "" {
		digest, err := fileSHA256(metadata.DatasetPath)
		if err != nil {
			return err
		}
		metadata.DatasetSHA256 = digest
	}
	envelope := reportEnvelope{Metadata: metadata, Report: report}
	encoded, err := json.MarshalIndent(envelope, "", "  ")
	if err != nil {
		return err
	}
	if err := os.WriteFile(filepath.Join(directory, "report.json"), append(encoded, '\n'), 0o644); err != nil {
		return err
	}
	markdown := renderMarkdown(envelope)
	if err := os.WriteFile(filepath.Join(directory, "report.md"), []byte(markdown), 0o644); err != nil {
		return err
	}
	caseFile, err := os.Create(filepath.Join(directory, "case_results.jsonl"))
	if err != nil {
		return err
	}
	encoder := json.NewEncoder(caseFile)
	for _, result := range report.Cases {
		if err := encoder.Encode(result); err != nil {
			_ = caseFile.Close()
			return err
		}
	}
	return caseFile.Close()
}

func renderMarkdown(envelope reportEnvelope) string {
	status := "PASS"
	if !envelope.Report.Passed {
		status = "FAIL"
	}
	var builder strings.Builder
	fmt.Fprintf(&builder, "# ModelGate evaluation report\n\n**Gate:** %s\n\n**Dataset SHA-256:** `%s`\n\n**Baseline:** `%s` (`%s`)\n\n**Candidate:** `%s` (`%s`)\n\n", status, envelope.Metadata.DatasetSHA256, envelope.Metadata.BaselineVersion, envelope.Metadata.BaselineArtifactSHA256, envelope.Metadata.CandidateVersion, envelope.Metadata.CandidateArtifactSHA256)
	builder.WriteString("| Metric | Baseline | Candidate | Delta |\n|---|---:|---:|---:|\n")
	writeMetric := func(name string, baseline, candidate float64) {
		fmt.Fprintf(&builder, "| %s | %.4f | %.4f | %+.4f |\n", name, baseline, candidate, candidate-baseline)
	}
	writeMetric("Category macro-F1", envelope.Report.Baseline.CategoryMacroF1, envelope.Report.Candidate.CategoryMacroF1)
	writeMetric("Urgency macro-F1", envelope.Report.Baseline.UrgencyMacroF1, envelope.Report.Candidate.UrgencyMacroF1)
	writeMetric("Emergency recall", envelope.Report.Baseline.EmergencyRecall, envelope.Report.Candidate.EmergencyRecall)
	builder.WriteString("\n## Slice metrics\n\n| Slice | Support | Category accuracy baseline | Category accuracy candidate | Delta | Urgency accuracy baseline | Urgency accuracy candidate | Delta | Emergency recall baseline | Emergency recall candidate | Delta |\n|---|---:|---:|---:|---:|---:|---:|---:|---:|---:|---:|\n")
	for _, slice := range envelope.Report.Slices {
		name := strings.ReplaceAll(strings.ReplaceAll(slice.Key+"="+slice.Value, "|", "\\|"), "\n", " ")
		fmt.Fprintf(&builder, "| %s | %d | %.4f | %.4f | %+.4f | %.4f | %.4f | %+.4f | %.4f | %.4f | %+.4f |\n",
			name, slice.Support,
			slice.Baseline.CategoryAccuracy, slice.Candidate.CategoryAccuracy, slice.Delta.CategoryAccuracy,
			slice.Baseline.UrgencyAccuracy, slice.Candidate.UrgencyAccuracy, slice.Delta.UrgencyAccuracy,
			slice.Baseline.EmergencyRecall, slice.Candidate.EmergencyRecall, slice.Delta.EmergencyRecall)
	}
	if len(envelope.Report.GateReasons) > 0 {
		builder.WriteString("\n## Gate failures\n")
		for _, reason := range envelope.Report.GateReasons {
			fmt.Fprintf(&builder, "- %s\n", reason)
		}
	}
	fmt.Fprintf(&builder, "\nCases: %d completed / %d total; errors: %d.\n", envelope.Report.Accounting.Completed, envelope.Report.Accounting.Total, envelope.Report.Accounting.Errors)
	return builder.String()
}

func fileSHA256(path string) (string, error) {
	file, err := os.Open(path)
	if err != nil {
		return "", err
	}
	defer file.Close()
	digest := sha256.New()
	if _, err := io.Copy(digest, file); err != nil {
		return "", err
	}
	return hex.EncodeToString(digest.Sum(nil)), nil
}
