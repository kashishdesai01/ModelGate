package llmmock

import (
	"encoding/json"
	"io"
	"mime"
	"net/http"
	"strings"
	"time"
)

type mockRequest struct {
	RequestID string `json:"request_id"`
	Text      string `json:"text"`
}

type mockUsage struct {
	PromptTokens     int `json:"prompt_tokens"`
	CompletionTokens int `json:"completion_tokens"`
}

type mockResponse struct {
	RequestID  string `json:"request_id"`
	Prediction any    `json:"prediction"`
	Backend    struct {
		Kind         string `json:"kind"`
		ModelID      string `json:"model_id"`
		ModelVersion string `json:"model_version"`
	} `json:"backend"`
	Usage mockUsage `json:"usage"`
}

func NewHandler() http.Handler {
	mux := http.NewServeMux()
	mux.HandleFunc("GET /healthz", func(writer http.ResponseWriter, _ *http.Request) {
		writeMockJSON(writer, http.StatusOK, map[string]string{"status": "ok"})
	})
	mux.HandleFunc("POST /v1/predict", handlePredict)
	return mux
}

func handlePredict(writer http.ResponseWriter, request *http.Request) {
	if !isJSONMediaType(request.Header.Get("Content-Type")) {
		http.Error(writer, "Content-Type must be application/json", http.StatusUnsupportedMediaType)
		return
	}
	request.Body = http.MaxBytesReader(writer, request.Body, 20<<10)
	decoder := json.NewDecoder(request.Body)
	decoder.DisallowUnknownFields()
	var payload mockRequest
	if err := decoder.Decode(&payload); err != nil || payload.RequestID == "" || strings.TrimSpace(payload.Text) == "" || len(payload.Text) > 16_384 {
		http.Error(writer, "invalid request", http.StatusBadRequest)
		return
	}
	if decoder.Decode(&struct{}{}) != io.EOF {
		http.Error(writer, "invalid request", http.StatusBadRequest)
		return
	}
	if strings.Contains(payload.Text, "[mock:timeout]") {
		select {
		case <-time.After(30 * time.Second):
		case <-request.Context().Done():
			return
		}
	}
	prediction, err := Predict(payload.Text)
	if err != nil {
		http.Error(writer, "mock backend failure", http.StatusServiceUnavailable)
		return
	}
	response := mockResponse{RequestID: payload.RequestID, Prediction: prediction, Usage: mockUsage{PromptTokens: max(len(strings.Fields(payload.Text)), 1), CompletionTokens: 8}}
	response.Backend.Kind = "llm_mock"
	response.Backend.ModelID = "structured-llm"
	response.Backend.ModelVersion = "mock-v1"
	writeMockJSON(writer, http.StatusOK, response)
}

func isJSONMediaType(value string) bool {
	mediaType, _, err := mime.ParseMediaType(value)
	return err == nil && strings.EqualFold(mediaType, "application/json")
}

func writeMockJSON(writer http.ResponseWriter, status int, value any) {
	writer.Header().Set("Content-Type", "application/json")
	writer.WriteHeader(status)
	_ = json.NewEncoder(writer).Encode(value)
}
