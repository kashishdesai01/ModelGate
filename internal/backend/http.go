package backend

import (
	"bytes"
	"context"
	"encoding/json"
	"fmt"
	"io"
	"net/http"
	"net/url"
	"strings"
	"time"

	"github.com/kashishdesai01/modelgate/internal/gateway"
	"github.com/kashishdesai01/modelgate/internal/model"
)

const maxResponseBytes = 1 << 20

type HTTPBackend struct {
	baseURL  *url.URL
	path     string
	identity gateway.BackendIdentity
	cost     int64
	client   *http.Client
}

func (b *HTTPBackend) Identity() gateway.BackendIdentity { return b.identity }

func NewClassifier(rawURL, version string, timeout time.Duration) *HTTPBackend {
	return newHTTPBackend(rawURL, "/v1/models/"+url.PathEscape(version)+"/predict", gateway.BackendIdentity{
		Kind: "sklearn", ModelID: "home-service-classifier", ModelVersion: version,
	}, 100, timeout)
}

func NewStructuredLLM(rawURL, version string, timeout time.Duration) *HTTPBackend {
	return newHTTPBackend(rawURL, "/v1/predict", gateway.BackendIdentity{
		Kind: "llm_mock", ModelID: "structured-llm", ModelVersion: version,
	}, 1000, timeout)
}

func newHTTPBackend(rawURL, path string, identity gateway.BackendIdentity, cost int64, timeout time.Duration) *HTTPBackend {
	parsed, err := url.Parse(rawURL)
	if err != nil || parsed.Scheme == "" || parsed.Host == "" {
		panic("backend URL must be an absolute HTTP(S) URL")
	}
	return &HTTPBackend{
		baseURL: parsed, path: path, identity: identity, cost: cost,
		client: &http.Client{
			Timeout:       timeout,
			CheckRedirect: func(_ *http.Request, _ []*http.Request) error { return http.ErrUseLastResponse },
			Transport: &http.Transport{
				Proxy: http.ProxyFromEnvironment, MaxIdleConns: 64, MaxIdleConnsPerHost: 32,
				IdleConnTimeout: 30 * time.Second, ResponseHeaderTimeout: timeout,
			},
		},
	}
}

type backendRequest struct {
	RequestID string `json:"request_id"`
	Text      string `json:"text"`
}

type backendResponse struct {
	RequestID  string                  `json:"request_id"`
	Prediction model.Prediction        `json:"prediction"`
	Backend    gateway.BackendIdentity `json:"backend"`
	Usage      struct {
		PromptTokens     int `json:"prompt_tokens"`
		CompletionTokens int `json:"completion_tokens"`
	} `json:"usage,omitempty"`
}

func (b *HTTPBackend) Predict(ctx context.Context, input gateway.PredictionInput) (gateway.BackendResult, error) {
	payload, err := json.Marshal(backendRequest{RequestID: input.RequestID, Text: input.Text})
	if err != nil {
		return gateway.BackendResult{}, fmt.Errorf("marshal backend request: %w", err)
	}
	endpoint := *b.baseURL
	endpoint.Path = strings.TrimRight(endpoint.Path, "/") + b.path
	request, err := http.NewRequestWithContext(ctx, http.MethodPost, endpoint.String(), bytes.NewReader(payload))
	if err != nil {
		return gateway.BackendResult{}, fmt.Errorf("create backend request: %w", err)
	}
	request.Header.Set("Content-Type", "application/json")
	request.Header.Set("X-Request-ID", input.RequestID)
	response, err := b.client.Do(request)
	if err != nil {
		return gateway.BackendResult{}, fmt.Errorf("backend request: %w", err)
	}
	defer response.Body.Close()
	limited := io.LimitReader(response.Body, maxResponseBytes+1)
	body, err := io.ReadAll(limited)
	if err != nil {
		return gateway.BackendResult{}, fmt.Errorf("read backend response: %w", err)
	}
	if len(body) > maxResponseBytes {
		return gateway.BackendResult{}, fmt.Errorf("backend response exceeds %d bytes", maxResponseBytes)
	}
	if response.StatusCode < 200 || response.StatusCode >= 300 {
		return gateway.BackendResult{}, fmt.Errorf("backend returned status %d", response.StatusCode)
	}
	decoder := json.NewDecoder(bytes.NewReader(body))
	decoder.DisallowUnknownFields()
	var decoded backendResponse
	if err := decoder.Decode(&decoded); err != nil {
		return gateway.BackendResult{}, fmt.Errorf("decode backend response: %w", err)
	}
	if decoder.Decode(&struct{}{}) != io.EOF {
		return gateway.BackendResult{}, fmt.Errorf("backend response contains trailing JSON")
	}
	if decoded.RequestID != input.RequestID {
		return gateway.BackendResult{}, fmt.Errorf("backend returned mismatched request ID")
	}
	if decoded.Backend.ModelVersion != "" && decoded.Backend.ModelVersion != b.identity.ModelVersion {
		return gateway.BackendResult{}, fmt.Errorf("backend served unexpected model version")
	}
	return gateway.BackendResult{Prediction: decoded.Prediction, Identity: b.identity, CostMicroUSD: b.cost}, nil
}
