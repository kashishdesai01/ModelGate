package api

import (
	"context"
	"crypto/rand"
	"encoding/hex"
	"encoding/json"
	"errors"
	"io"
	"log/slog"
	"mime"
	"net/http"
	"strconv"
	"strings"
	"time"

	"github.com/kashishdesai01/modelgate/internal/control"
	"github.com/kashishdesai01/modelgate/internal/gateway"
	"github.com/kashishdesai01/modelgate/internal/model"
	"github.com/kashishdesai01/modelgate/internal/store"
)

const maxRequestBytes = 20 << 10

type Store interface {
	Ping(context.Context) error
	Authenticate(context.Context, string) (store.Identity, error)
	Reserve(context.Context, store.ReserveParams) (store.Reservation, error)
	Reconcile(context.Context, store.ReconcileParams) error
	Usage(context.Context, string) (store.Usage, error)
	RecordCacheHit(context.Context, store.CacheHitParams) error
}

type Controls interface {
	Allow(context.Context, string, int, float64) (control.RateDecision, error)
	CacheGet(context.Context, string, string, string) ([]byte, bool, error)
	CacheSet(context.Context, string, string, string, []byte, time.Duration) error
	Ping(context.Context) error
}

type Predictor interface {
	Predict(context.Context, gateway.PredictionInput) (gateway.PredictionResult, error)
	CacheNamespace() string
}

type Handler struct {
	store     Store
	controls  Controls
	predictor Predictor
	metrics   *Metrics
	mux       *http.ServeMux
}

type predictionRequest struct {
	Strategy string `json:"strategy"`
	Text     string `json:"text"`
}

type usageResponse struct {
	CostMicroUSD int64 `json:"cost_microusd"`
}

type predictionResponse struct {
	RequestID  string                  `json:"request_id"`
	Prediction model.Prediction        `json:"prediction"`
	Backend    gateway.BackendIdentity `json:"backend"`
	Routing    gateway.RoutingInfo     `json:"routing"`
	Usage      usageResponse           `json:"usage"`
	CacheHit   bool                    `json:"cache_hit"`
}

type errorEnvelope struct {
	Error errorBody `json:"error"`
}

type errorBody struct {
	Code      string `json:"code"`
	Message   string `json:"message"`
	RequestID string `json:"request_id"`
	Retryable bool   `json:"retryable"`
}

func NewHandler(store Store, controls Controls, predictor Predictor, metrics *Metrics) http.Handler {
	handler := &Handler{store: store, controls: controls, predictor: predictor, metrics: metrics, mux: http.NewServeMux()}
	handler.mux.HandleFunc("POST /v1/predictions", handler.predictions)
	handler.mux.HandleFunc("GET /v1/usage", handler.usage)
	handler.mux.HandleFunc("GET /livez", func(writer http.ResponseWriter, _ *http.Request) {
		writeJSON(writer, http.StatusOK, map[string]string{"status": "ok"})
	})
	handler.mux.HandleFunc("GET /readyz", handler.ready)
	handler.mux.Handle("GET /metrics", metrics)
	return handler
}

func (h *Handler) ServeHTTP(writer http.ResponseWriter, request *http.Request) {
	h.mux.ServeHTTP(writer, request)
}

func (h *Handler) predictions(writer http.ResponseWriter, request *http.Request) {
	started := time.Now()
	status := http.StatusInternalServerError
	cacheHit := false
	defer func() { h.metrics.Observe(status, cacheHit, time.Since(started)) }()
	requestID := requestID(request)
	writer.Header().Set("X-Request-ID", requestID)

	identity, authStatus, ok := h.authenticate(writer, request, requestID)
	if !ok {
		status = authStatus
		return
	}
	if !isJSONMediaType(request.Header.Get("Content-Type")) {
		status = http.StatusUnsupportedMediaType
		writeError(writer, status, "unsupported_media_type", "Content-Type must be application/json", requestID, false)
		return
	}
	request.Body = http.MaxBytesReader(writer, request.Body, maxRequestBytes)
	decoder := json.NewDecoder(request.Body)
	decoder.DisallowUnknownFields()
	var payload predictionRequest
	if err := decoder.Decode(&payload); err != nil {
		status = http.StatusBadRequest
		writeError(writer, status, "invalid_request", "request must be valid JSON with only strategy and text", requestID, false)
		return
	}
	if err := ensureEOF(decoder); err != nil {
		status = http.StatusBadRequest
		writeError(writer, status, "invalid_request", "request must contain one JSON object", requestID, false)
		return
	}
	payload.Text = strings.TrimSpace(payload.Text)
	if payload.Text == "" || len(payload.Text) > 16_384 || !validStrategy(payload.Strategy) {
		status = http.StatusBadRequest
		writeError(writer, status, "invalid_request", "strategy or text is invalid", requestID, false)
		return
	}
	decision, err := h.controls.Allow(request.Context(), identity.UserID, identity.RateCapacity, identity.RateRefillPerSecond)
	if err != nil {
		status = http.StatusServiceUnavailable
		writeError(writer, status, "dependency_unavailable", "rate limiter unavailable", requestID, true)
		return
	}
	writer.Header().Set("RateLimit-Remaining", intString(decision.Remaining))
	if !decision.Allowed {
		status = http.StatusTooManyRequests
		writer.Header().Set("Retry-After", intString(max(int64(decision.RetryAfter.Round(time.Second)/time.Second), 1)))
		writeError(writer, status, "rate_limited", "request rate exceeded", requestID, true)
		return
	}

	cacheVariant := payload.Strategy + "|" + h.predictor.CacheNamespace()
	if cached, found, cacheErr := h.controls.CacheGet(request.Context(), identity.UserID, cacheVariant, payload.Text); cacheErr == nil && found {
		var response predictionResponse
		if json.Unmarshal(cached, &response) == nil {
			response.RequestID = requestID
			response.CacheHit = true
			response.Usage.CostMicroUSD = 0
			auditCtx, cancel := cleanupContext(request.Context())
			defer cancel()
			if err := h.store.RecordCacheHit(auditCtx, store.CacheHitParams{
				RequestID: requestID, UserID: identity.UserID, Strategy: payload.Strategy,
				ModelKind: response.Backend.Kind, ModelID: response.Backend.ModelID,
				ModelVersion: response.Backend.ModelVersion, PolicyVersion: response.Routing.PolicyVersion,
				Latency: time.Since(started),
			}); err != nil {
				status = http.StatusServiceUnavailable
				writeError(writer, status, "audit_unavailable", "request could not be durably recorded", requestID, true)
				return
			}
			status, cacheHit = http.StatusOK, true
			writeJSON(writer, status, response)
			return
		}
	}

	reservation, err := h.store.Reserve(request.Context(), store.ReserveParams{
		RequestID: requestID, UserID: identity.UserID, Strategy: payload.Strategy,
		EstimatedCostMicroUSD: estimatedCost(payload.Strategy),
	})
	if errors.Is(err, store.ErrBudgetExceeded) {
		status = http.StatusPaymentRequired
		writeError(writer, status, "budget_exceeded", "monthly inference budget exhausted", requestID, false)
		return
	}
	if err != nil {
		status = http.StatusServiceUnavailable
		writeError(writer, status, "audit_unavailable", "request could not be durably reserved", requestID, true)
		return
	}

	result, predictErr := h.predictor.Predict(request.Context(), gateway.PredictionInput{RequestID: requestID, Strategy: payload.Strategy, Text: payload.Text})
	if predictErr != nil {
		reconcileCtx, cancel := cleanupContext(request.Context())
		defer cancel()
		if reconcileErr := h.store.Reconcile(reconcileCtx, store.ReconcileParams{
			RequestID: reservation.RequestID, ActualCost: result.CostMicroUSD, ErrorCode: "backend_error",
			ModelKind: result.Backend.Kind, ModelID: result.Backend.ModelID, ModelVersion: result.Backend.ModelVersion,
			PolicyVersion: result.Routing.PolicyVersion, FallbackUsed: result.Routing.FallbackUsed, Latency: time.Since(started),
		}); reconcileErr != nil {
			slog.Error("failed to reconcile failed request", "request_id", requestID, "error", reconcileErr)
		}
		status = http.StatusBadGateway
		if errors.Is(predictErr, context.DeadlineExceeded) {
			status = http.StatusGatewayTimeout
		}
		writeError(writer, status, "backend_error", "model backend failed", requestID, true)
		return
	}
	reconcileCtx, cancel := cleanupContext(request.Context())
	defer cancel()
	if err := h.store.Reconcile(reconcileCtx, store.ReconcileParams{
		RequestID: reservation.RequestID, ActualCost: result.CostMicroUSD, Succeeded: true,
		ModelKind: result.Backend.Kind, ModelID: result.Backend.ModelID, ModelVersion: result.Backend.ModelVersion,
		PolicyVersion: result.Routing.PolicyVersion, FallbackUsed: result.Routing.FallbackUsed, Latency: time.Since(started),
	}); err != nil {
		status = http.StatusServiceUnavailable
		writeError(writer, status, "audit_unavailable", "model responded but accounting failed; retry may be unsafe", requestID, false)
		return
	}
	response := predictionResponse{
		RequestID: requestID, Prediction: result.Prediction, Backend: result.Backend,
		Routing: result.Routing, Usage: usageResponse{CostMicroUSD: result.CostMicroUSD}, CacheHit: false,
	}
	encoded, _ := json.Marshal(response)
	if err := h.controls.CacheSet(request.Context(), identity.UserID, cacheVariant, payload.Text, encoded, time.Hour); err != nil {
		slog.Warn("cache set failed", "request_id", requestID, "error", err)
	}
	status = http.StatusOK
	writeJSON(writer, status, response)
}

func (h *Handler) usage(writer http.ResponseWriter, request *http.Request) {
	requestID := requestID(request)
	identity, _, ok := h.authenticate(writer, request, requestID)
	if !ok {
		return
	}
	usage, err := h.store.Usage(request.Context(), identity.UserID)
	if err != nil {
		writeError(writer, http.StatusServiceUnavailable, "dependency_unavailable", "usage unavailable", requestID, true)
		return
	}
	writeJSON(writer, http.StatusOK, usage)
}

func (h *Handler) ready(writer http.ResponseWriter, request *http.Request) {
	ctx, cancel := context.WithTimeout(request.Context(), time.Second)
	defer cancel()
	if err := h.store.Ping(ctx); err != nil {
		writeJSON(writer, http.StatusServiceUnavailable, map[string]string{"status": "not_ready"})
		return
	}
	if err := h.controls.Ping(ctx); err != nil {
		writeJSON(writer, http.StatusServiceUnavailable, map[string]string{"status": "not_ready"})
		return
	}
	writeJSON(writer, http.StatusOK, map[string]string{"status": "ready"})
}

func (h *Handler) authenticate(writer http.ResponseWriter, request *http.Request, requestID string) (store.Identity, int, bool) {
	header := request.Header.Get("Authorization")
	if !strings.HasPrefix(header, "Bearer ") || len(header) <= len("Bearer ") {
		writeError(writer, http.StatusUnauthorized, "unauthorized", "valid bearer API key required", requestID, false)
		return store.Identity{}, http.StatusUnauthorized, false
	}
	identity, err := h.store.Authenticate(request.Context(), strings.TrimPrefix(header, "Bearer "))
	if errors.Is(err, store.ErrUnauthorized) {
		writeError(writer, http.StatusUnauthorized, "unauthorized", "valid bearer API key required", requestID, false)
		return store.Identity{}, http.StatusUnauthorized, false
	}
	if err != nil {
		writeError(writer, http.StatusServiceUnavailable, "dependency_unavailable", "authentication store unavailable", requestID, true)
		return store.Identity{}, http.StatusServiceUnavailable, false
	}
	return identity, http.StatusOK, true
}

func cleanupContext(parent context.Context) (context.Context, context.CancelFunc) {
	return context.WithTimeout(context.WithoutCancel(parent), 2*time.Second)
}

func requestID(request *http.Request) string {
	if candidate := request.Header.Get("X-Request-ID"); candidate != "" && len(candidate) <= 128 && !strings.ContainsAny(candidate, "\r\n") {
		return candidate
	}
	buffer := make([]byte, 16)
	if _, err := rand.Read(buffer); err != nil {
		return intString(time.Now().UnixNano())
	}
	return hex.EncodeToString(buffer)
}

func ensureEOF(decoder *json.Decoder) error {
	var extra any
	if err := decoder.Decode(&extra); err != io.EOF {
		if err == nil {
			return errors.New("trailing JSON")
		}
		return err
	}
	return nil
}

func isJSONMediaType(value string) bool {
	mediaType, _, err := mime.ParseMediaType(value)
	return err == nil && strings.EqualFold(mediaType, "application/json")
}

func validStrategy(value string) bool {
	return value == "sklearn" || value == "llm" || value == "hybrid"
}

func estimatedCost(strategy string) int64 {
	if strategy == "sklearn" {
		return 100
	}
	if strategy == "llm" {
		return 1000
	}
	return 1100
}

func intString(value int64) string {
	return strconv.FormatInt(value, 10)
}

func writeError(writer http.ResponseWriter, status int, code, message, requestID string, retryable bool) {
	writeJSON(writer, status, errorEnvelope{Error: errorBody{Code: code, Message: message, RequestID: requestID, Retryable: retryable}})
}

func writeJSON(writer http.ResponseWriter, status int, value any) {
	writer.Header().Set("Content-Type", "application/json")
	writer.WriteHeader(status)
	_ = json.NewEncoder(writer).Encode(value)
}
