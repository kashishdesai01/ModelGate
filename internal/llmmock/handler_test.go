package llmmock

import (
	"bytes"
	"encoding/json"
	"net/http"
	"net/http/httptest"
	"testing"
)

func TestHTTPHandlerReturnsStrictPredictionContract(t *testing.T) {
	handler := NewHandler()
	recorder := httptest.NewRecorder()
	request := httptest.NewRequest(http.MethodPost, "/v1/predict", bytes.NewBufferString(`{"request_id":"req-1","text":"burst pipe is floding right now"}`))
	request.Header.Set("Content-Type", "application/json")
	handler.ServeHTTP(recorder, request)
	if recorder.Code != http.StatusOK {
		t.Fatalf("status=%d body=%s", recorder.Code, recorder.Body.String())
	}
	var response struct {
		RequestID string `json:"request_id"`
		Usage     struct {
			PromptTokens int `json:"prompt_tokens"`
		} `json:"usage"`
	}
	if err := json.Unmarshal(recorder.Body.Bytes(), &response); err != nil {
		t.Fatal(err)
	}
	if response.RequestID != "req-1" || response.Usage.PromptTokens <= 0 {
		t.Fatalf("unexpected response: %+v", response)
	}
}

func TestHTTPHandlerRejectsInvalidJSONMediaType(t *testing.T) {
	recorder := httptest.NewRecorder()
	request := httptest.NewRequest(http.MethodPost, "/v1/predict", bytes.NewBufferString(`{"request_id":"req-1","text":"sink"}`))
	request.Header.Set("Content-Type", "application/json-invalid")
	NewHandler().ServeHTTP(recorder, request)
	if recorder.Code != http.StatusUnsupportedMediaType {
		t.Fatalf("status=%d body=%s", recorder.Code, recorder.Body.String())
	}
}

func TestHTTPHandlerRejectsUnknownFields(t *testing.T) {
	recorder := httptest.NewRecorder()
	request := httptest.NewRequest(http.MethodPost, "/v1/predict", bytes.NewBufferString(`{"request_id":"req-1","text":"sink","extra":true}`))
	request.Header.Set("Content-Type", "application/json")
	NewHandler().ServeHTTP(recorder, request)
	if recorder.Code != http.StatusBadRequest {
		t.Fatalf("status=%d body=%s", recorder.Code, recorder.Body.String())
	}
}
