package backend

import (
	"context"
	"encoding/json"
	"net/http"
	"net/http/httptest"
	"testing"
	"time"

	"github.com/kashishdesai01/modelgate/internal/gateway"
)

func TestClassifierAdapterPropagatesRequestAndVersion(t *testing.T) {
	server := httptest.NewServer(http.HandlerFunc(func(writer http.ResponseWriter, request *http.Request) {
		if request.URL.Path != "/v1/models/baseline-v1/predict" {
			t.Fatalf("unexpected path %s", request.URL.Path)
		}
		var body map[string]string
		if err := json.NewDecoder(request.Body).Decode(&body); err != nil {
			t.Fatalf("decode request: %v", err)
		}
		if body["request_id"] != "req-1" || body["text"] != "sink leak" {
			t.Fatalf("unexpected request body: %+v", body)
		}
		writer.Header().Set("Content-Type", "application/json")
		_, _ = writer.Write([]byte(`{"request_id":"req-1","prediction":{"category":"plumbing","urgency":"soon","category_confidence":0.9,"urgency_confidence":0.8},"backend":{"kind":"sklearn","model_id":"home-service-classifier","model_version":"baseline-v1"}}`))
	}))
	defer server.Close()

	adapter := NewClassifier(server.URL, "baseline-v1", time.Second)
	result, err := adapter.Predict(context.Background(), gateway.PredictionInput{RequestID: "req-1", Text: "sink leak"})
	if err != nil {
		t.Fatalf("predict: %v", err)
	}
	if result.Prediction.Category != "plumbing" || result.Identity.ModelVersion != "baseline-v1" {
		t.Fatalf("unexpected result: %+v", result)
	}
}

func TestAdapterRejectsOversizedAndMalformedResponses(t *testing.T) {
	tests := map[string]http.HandlerFunc{
		"malformed": func(writer http.ResponseWriter, _ *http.Request) { _, _ = writer.Write([]byte(`not-json`)) },
		"oversized": func(writer http.ResponseWriter, _ *http.Request) {
			_, _ = writer.Write(make([]byte, maxResponseBytes+1))
		},
		"status": func(writer http.ResponseWriter, _ *http.Request) {
			http.Error(writer, "private upstream details", http.StatusInternalServerError)
		},
	}
	for name, handler := range tests {
		t.Run(name, func(t *testing.T) {
			server := httptest.NewServer(handler)
			defer server.Close()
			adapter := NewClassifier(server.URL, "baseline-v1", time.Second)
			if _, err := adapter.Predict(context.Background(), gateway.PredictionInput{RequestID: "r", Text: "test"}); err == nil {
				t.Fatal("expected adapter error")
			}
		})
	}
}
