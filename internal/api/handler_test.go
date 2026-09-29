package api

import (
	"bytes"
	"context"
	"encoding/json"
	"errors"
	"net/http"
	"net/http/httptest"
	"strings"
	"testing"
	"time"

	"github.com/kashishdesai01/modelgate/internal/control"
	"github.com/kashishdesai01/modelgate/internal/gateway"
	"github.com/kashishdesai01/modelgate/internal/model"
	"github.com/kashishdesai01/modelgate/internal/store"
)

type fakeStore struct {
	identity        store.Identity
	authErr         error
	pingErr         error
	reserveErr      error
	reserveCalls    int
	reconcileCalls  int
	reconcileCtxErr error
	reconcileParams store.ReconcileParams
	cacheLogCalls   int
	cacheHitParams  store.CacheHitParams
}

func (f *fakeStore) Authenticate(context.Context, string) (store.Identity, error) {
	return f.identity, f.authErr
}
func (f *fakeStore) Reserve(context.Context, store.ReserveParams) (store.Reservation, error) {
	f.reserveCalls++
	return store.Reservation{RequestID: "request", ReservedCost: 1100}, f.reserveErr
}
func (f *fakeStore) Reconcile(ctx context.Context, params store.ReconcileParams) error {
	f.reconcileCalls++
	f.reconcileCtxErr = ctx.Err()
	f.reconcileParams = params
	return nil
}
func (f *fakeStore) Usage(context.Context, string) (store.Usage, error) {
	return store.Usage{LimitMicroUSD: 10000}, nil
}
func (f *fakeStore) RecordCacheHit(_ context.Context, params store.CacheHitParams) error {
	f.cacheLogCalls++
	f.cacheHitParams = params
	return nil
}
func (f *fakeStore) Ping(context.Context) error { return f.pingErr }

type fakeControls struct {
	decision      control.RateDecision
	allowErr      error
	cached        []byte
	found         bool
	cacheVariants []string
}

func (f *fakeControls) Allow(context.Context, string, int, float64) (control.RateDecision, error) {
	return f.decision, f.allowErr
}
func (f *fakeControls) CacheGet(_ context.Context, _ string, variant string, _ string) ([]byte, bool, error) {
	f.cacheVariants = append(f.cacheVariants, variant)
	return f.cached, f.found, nil
}
func (f *fakeControls) CacheSet(_ context.Context, _ string, variant string, _ string, _ []byte, _ time.Duration) error {
	f.cacheVariants = append(f.cacheVariants, variant)
	return nil
}
func (f *fakeControls) Ping(context.Context) error { return nil }

type fakePredictor struct {
	calls     int
	namespace string
}

func (f *fakePredictor) CacheNamespace() string {
	if f.namespace == "" {
		return "baseline-v1|hybrid-v1"
	}
	return f.namespace
}

func (f *fakePredictor) Predict(_ context.Context, input gateway.PredictionInput) (gateway.PredictionResult, error) {
	f.calls++
	cc, uc := .9, .8
	return gateway.PredictionResult{
		RequestID:  input.RequestID,
		Prediction: model.Prediction{Category: "plumbing", Urgency: "soon", CategoryConfidence: &cc, UrgencyConfidence: &uc},
		Backend:    gateway.BackendIdentity{Kind: "sklearn", ModelID: "home-service-classifier", ModelVersion: "baseline-v1"},
		Routing:    gateway.RoutingInfo{PolicyVersion: "hybrid-v1"}, CostMicroUSD: 100,
	}, nil
}

type cancelingPredictor struct {
	cancel context.CancelFunc
}

func (p *cancelingPredictor) CacheNamespace() string { return "cancel-test" }

func (p *cancelingPredictor) Predict(context.Context, gateway.PredictionInput) (gateway.PredictionResult, error) {
	p.cancel()
	return gateway.PredictionResult{}, context.Canceled
}

func newTestHandler(storeFake *fakeStore, controlsFake *fakeControls, predictor Predictor) http.Handler {
	if storeFake.identity.UserID == "" && storeFake.authErr == nil {
		storeFake.identity = store.Identity{UserID: "user-1", RateCapacity: 20, RateRefillPerSecond: 5}
	}
	if !controlsFake.decision.Allowed && controlsFake.allowErr == nil {
		controlsFake.decision = control.RateDecision{Allowed: true, Remaining: 19}
	}
	return NewHandler(storeFake, controlsFake, predictor, NewMetrics())
}

func request(t *testing.T, handler http.Handler, body string) *httptest.ResponseRecorder {
	t.Helper()
	recorder := httptest.NewRecorder()
	req := httptest.NewRequest(http.MethodPost, "/v1/predictions", bytes.NewBufferString(body))
	req.Header.Set("Authorization", "Bearer mg_test")
	req.Header.Set("Content-Type", "application/json")
	handler.ServeHTTP(recorder, req)
	return recorder
}

type costedErrorPredictor struct{}

func (*costedErrorPredictor) CacheNamespace() string { return "costed-error" }
func (*costedErrorPredictor) Predict(_ context.Context, input gateway.PredictionInput) (gateway.PredictionResult, error) {
	return gateway.PredictionResult{
		RequestID: input.RequestID, CostMicroUSD: 1000,
		Backend: gateway.BackendIdentity{Kind: "llm_mock", ModelID: "structured-llm", ModelVersion: "mock-v1"},
		Routing: gateway.RoutingInfo{PolicyVersion: "hybrid-v1"},
	}, errors.New("invalid fallback response")
}

func TestPredictionErrorReconcilesConsumedBackendCost(t *testing.T) {
	storeFake := &fakeStore{}
	recorder := request(t, newTestHandler(storeFake, &fakeControls{}, &costedErrorPredictor{}), `{"strategy":"llm","text":"invalid response"}`)
	if recorder.Code != http.StatusBadGateway {
		t.Fatalf("status=%d body=%s", recorder.Code, recorder.Body.String())
	}
	if storeFake.reconcileParams.ActualCost != 1000 || storeFake.reconcileParams.ModelID != "structured-llm" || storeFake.reconcileParams.ModelVersion != "mock-v1" || storeFake.reconcileParams.PolicyVersion != "hybrid-v1" {
		t.Fatalf("consumed backend cost/provenance was not reconciled: %+v", storeFake.reconcileParams)
	}
}

func TestCancellationStillReconcilesWithLiveCleanupContext(t *testing.T) {
	storeFake := &fakeStore{}
	ctx, cancel := context.WithCancel(context.Background())
	handler := newTestHandler(storeFake, &fakeControls{}, &cancelingPredictor{cancel: cancel})
	recorder := httptest.NewRecorder()
	req := httptest.NewRequest(http.MethodPost, "/v1/predictions", bytes.NewBufferString(`{"strategy":"llm","text":"sink leak"}`)).WithContext(ctx)
	req.Header.Set("Authorization", "Bearer mg_test")
	req.Header.Set("Content-Type", "application/json")
	handler.ServeHTTP(recorder, req)
	if storeFake.reconcileCalls != 1 || storeFake.reconcileCtxErr != nil {
		t.Fatalf("reconcile calls=%d context error=%v", storeFake.reconcileCalls, storeFake.reconcileCtxErr)
	}
}

func TestPredictionRejectsInvalidJSONMediaType(t *testing.T) {
	predictor := &fakePredictor{}
	handler := newTestHandler(&fakeStore{}, &fakeControls{}, predictor)
	recorder := httptest.NewRecorder()
	req := httptest.NewRequest(http.MethodPost, "/v1/predictions", bytes.NewBufferString(`{"strategy":"hybrid","text":"sink leak"}`))
	req.Header.Set("Authorization", "Bearer mg_test")
	req.Header.Set("Content-Type", "application/json-invalid")
	handler.ServeHTTP(recorder, req)
	if recorder.Code != http.StatusUnsupportedMediaType || predictor.calls != 0 {
		t.Fatalf("status=%d calls=%d body=%s", recorder.Code, predictor.calls, recorder.Body.String())
	}
}

func TestPredictionRejectsUnknownFieldsBeforeBackend(t *testing.T) {
	predictor := &fakePredictor{}
	recorder := request(t, newTestHandler(&fakeStore{}, &fakeControls{}, predictor), `{"strategy":"hybrid","text":"sink leak","extra":true}`)
	if recorder.Code != http.StatusBadRequest || predictor.calls != 0 {
		t.Fatalf("status=%d calls=%d body=%s", recorder.Code, predictor.calls, recorder.Body.String())
	}
}

func TestPredictionRequiresAuthentication(t *testing.T) {
	predictor := &fakePredictor{}
	handler := newTestHandler(&fakeStore{authErr: store.ErrUnauthorized}, &fakeControls{}, predictor)
	recorder := httptest.NewRecorder()
	req := httptest.NewRequest(http.MethodPost, "/v1/predictions", bytes.NewBufferString(`{"strategy":"sklearn","text":"sink leak"}`))
	handler.ServeHTTP(recorder, req)
	if recorder.Code != http.StatusUnauthorized || predictor.calls != 0 {
		t.Fatalf("status=%d calls=%d", recorder.Code, predictor.calls)
	}
}

func TestAuthenticationDependencyFailureIsRetryable(t *testing.T) {
	handler := newTestHandler(&fakeStore{authErr: errors.New("database down")}, &fakeControls{}, &fakePredictor{})
	recorder := request(t, handler, `{"strategy":"sklearn","text":"sink leak"}`)
	if recorder.Code != http.StatusServiceUnavailable || !strings.Contains(recorder.Body.String(), "dependency_unavailable") {
		t.Fatalf("status=%d body=%s", recorder.Code, recorder.Body.String())
	}
}

func TestReadinessChecksDatabase(t *testing.T) {
	handler := newTestHandler(&fakeStore{pingErr: errors.New("database down")}, &fakeControls{}, &fakePredictor{})
	recorder := httptest.NewRecorder()
	handler.ServeHTTP(recorder, httptest.NewRequest(http.MethodGet, "/readyz", nil))
	if recorder.Code != http.StatusServiceUnavailable {
		t.Fatalf("status=%d body=%s", recorder.Code, recorder.Body.String())
	}
}

func TestPredictionReservesReconcilesAndReturnsStructuredResult(t *testing.T) {
	storeFake := &fakeStore{}
	predictor := &fakePredictor{}
	recorder := request(t, newTestHandler(storeFake, &fakeControls{}, predictor), `{"strategy":"hybrid","text":"sink leak"}`)
	if recorder.Code != http.StatusOK {
		t.Fatalf("status=%d body=%s", recorder.Code, recorder.Body.String())
	}
	if storeFake.reserveCalls != 1 || storeFake.reconcileCalls != 1 || predictor.calls != 1 {
		t.Fatalf("reserve=%d reconcile=%d predict=%d", storeFake.reserveCalls, storeFake.reconcileCalls, predictor.calls)
	}
	if storeFake.reconcileParams.ModelID != "home-service-classifier" || storeFake.reconcileParams.PolicyVersion != "hybrid-v1" {
		t.Fatalf("reconciliation omitted provenance: %+v", storeFake.reconcileParams)
	}
	var body map[string]any
	if err := json.Unmarshal(recorder.Body.Bytes(), &body); err != nil {
		t.Fatalf("decode: %v", err)
	}
	if body["cache_hit"] != false {
		t.Fatalf("unexpected body: %+v", body)
	}
}

func TestBudgetAndRedisFailuresDoNotCallBackend(t *testing.T) {
	tests := map[string]struct {
		store    *fakeStore
		controls *fakeControls
		status   int
	}{
		"budget": {&fakeStore{reserveErr: store.ErrBudgetExceeded}, &fakeControls{}, http.StatusPaymentRequired},
		"redis":  {&fakeStore{}, &fakeControls{allowErr: errors.New("redis down")}, http.StatusServiceUnavailable},
	}
	for name, testCase := range tests {
		t.Run(name, func(t *testing.T) {
			predictor := &fakePredictor{}
			recorder := request(t, newTestHandler(testCase.store, testCase.controls, predictor), `{"strategy":"sklearn","text":"sink leak"}`)
			if recorder.Code != testCase.status || predictor.calls != 0 {
				t.Fatalf("status=%d calls=%d body=%s", recorder.Code, predictor.calls, recorder.Body.String())
			}
		})
	}
}

func TestCacheHitStillConsumesRateTokenAndSkipsBackend(t *testing.T) {
	cached := []byte(`{"request_id":"cached","prediction":{"category":"plumbing","urgency":"soon","category_confidence":0.9,"urgency_confidence":0.8},"backend":{"kind":"sklearn","model_id":"home-service-classifier","model_version":"baseline-v1"},"routing":{"policy_version":"hybrid-v1","fallback_used":false},"usage":{"cost_microusd":0},"cache_hit":true}`)
	storeFake := &fakeStore{}
	predictor := &fakePredictor{}
	controlsFake := &fakeControls{decision: control.RateDecision{Allowed: true}, cached: cached, found: true}
	recorder := request(t, newTestHandler(storeFake, controlsFake, predictor), `{"strategy":"hybrid","text":"sink leak"}`)
	if recorder.Code != http.StatusOK || predictor.calls != 0 || storeFake.cacheLogCalls != 1 {
		t.Fatalf("status=%d predict=%d cacheLog=%d", recorder.Code, predictor.calls, storeFake.cacheLogCalls)
	}
	if storeFake.cacheHitParams.ModelID != "home-service-classifier" || storeFake.cacheHitParams.ModelVersion != "baseline-v1" || storeFake.cacheHitParams.PolicyVersion != "hybrid-v1" {
		t.Fatalf("cache audit omitted provenance: %+v", storeFake.cacheHitParams)
	}
}

func TestCacheVariantChangesWithPredictorNamespace(t *testing.T) {
	firstControls := &fakeControls{}
	secondControls := &fakeControls{}
	request(t, newTestHandler(&fakeStore{}, firstControls, &fakePredictor{namespace: "baseline-v1|hybrid-v1"}), `{"strategy":"hybrid","text":"sink leak"}`)
	request(t, newTestHandler(&fakeStore{}, secondControls, &fakePredictor{namespace: "candidate-v2|hybrid-v2"}), `{"strategy":"hybrid","text":"sink leak"}`)
	if len(firstControls.cacheVariants) == 0 || len(secondControls.cacheVariants) == 0 || firstControls.cacheVariants[0] == secondControls.cacheVariants[0] {
		t.Fatalf("cache variants were not versioned: first=%v second=%v", firstControls.cacheVariants, secondControls.cacheVariants)
	}
}
