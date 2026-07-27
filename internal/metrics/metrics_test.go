package metrics

import (
	"net/http"
	"net/http/httptest"
	"testing"

	"github.com/prometheus/client_golang/prometheus"
	dto "github.com/prometheus/client_model/go"
)

func TestNormalizePath(t *testing.T) {
	tests := []struct {
		input string
		want  string
	}{
		{"/health", "/health"},
		{"/agents/my_agent", "/agents/{id}"},
		{"/threads/550e8400-e29b-41d4-a716-446655440000/runs/660e8400-e29b-41d4-a716-446655440001", "/threads/{id}/runs/{id}"},
		{"/runs/550e8400-e29b-41d4-a716-446655440000", "/runs/{id}"},
		{"/runs/550e8400-e29b-41d4-a716-446655440000/stream", "/runs/{id}/stream"},
		{"/store/items", "/store/items"},
		{"/threads/550e8400-e29b-41d4-a716-446655440000/state", "/threads/{id}/state"},
		{"/internal/connectors/slack", "/internal/connectors/{id}"},
		{"/assistants/my_bot/schemas", "/assistants/{id}/schemas"},
	}
	for _, tt := range tests {
		got := normalizePath(tt.input)
		if got != tt.want {
			t.Errorf("normalizePath(%q) = %q, want %q", tt.input, got, tt.want)
		}
	}
}

func TestResponseWriterCapturesStatus(t *testing.T) {
	rec := httptest.NewRecorder()
	rw := &responseWriter{ResponseWriter: rec, statusCode: 200}

	rw.WriteHeader(http.StatusNotFound)
	if rw.statusCode != http.StatusNotFound {
		t.Errorf("statusCode = %d, want %d", rw.statusCode, http.StatusNotFound)
	}

	// Second WriteHeader should not change the captured code
	rw.WriteHeader(http.StatusOK)
	if rw.statusCode != http.StatusNotFound {
		t.Errorf("statusCode changed to %d on second WriteHeader, want %d", rw.statusCode, http.StatusNotFound)
	}
}

func TestResponseWriterDefaultStatus(t *testing.T) {
	rec := httptest.NewRecorder()
	rw := &responseWriter{ResponseWriter: rec, statusCode: 200}

	// Write without explicit WriteHeader should keep default 200
	rw.Write([]byte("ok"))
	if rw.statusCode != http.StatusOK {
		t.Errorf("statusCode = %d after Write without WriteHeader, want %d", rw.statusCode, http.StatusOK)
	}
}

func TestHTTPMiddleware(t *testing.T) {
	// Use a fresh registry to avoid polluting the default one across tests
	reg := prometheus.NewRegistry()

	reqTotal := prometheus.NewCounterVec(
		prometheus.CounterOpts{Name: "test_http_requests_total", Help: "test"},
		[]string{"method", "path", "status"},
	)
	reqDuration := prometheus.NewHistogramVec(
		prometheus.HistogramOpts{Name: "test_http_request_duration_seconds", Help: "test", Buckets: prometheus.DefBuckets},
		[]string{"method", "path"},
	)
	reg.MustRegister(reqTotal, reqDuration)

	// Temporarily swap package-level vars (test-only, single goroutine)
	origTotal, origDur := HTTPRequestsTotal, HTTPRequestDuration
	HTTPRequestsTotal, HTTPRequestDuration = reqTotal, reqDuration
	defer func() { HTTPRequestsTotal, HTTPRequestDuration = origTotal, origDur }()

	handler := HTTPMiddleware(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		w.WriteHeader(http.StatusCreated)
	}))

	req := httptest.NewRequest("POST", "/threads/550e8400-e29b-41d4-a716-446655440000/runs", nil)
	rec := httptest.NewRecorder()
	handler.ServeHTTP(rec, req)

	// Verify counter incremented
	m := &dto.Metric{}
	c, err := reqTotal.GetMetricWithLabelValues("POST", "/threads/{id}/runs", "201")
	if err != nil {
		t.Fatalf("GetMetricWithLabelValues: %v", err)
	}
	c.(prometheus.Metric).Write(m)
	if got := m.GetCounter().GetValue(); got != 1 {
		t.Errorf("request counter = %v, want 1", got)
	}

	// Verify histogram observed (count should be 1)
	m2 := &dto.Metric{}
	h, err := reqDuration.GetMetricWithLabelValues("POST", "/threads/{id}/runs")
	if err != nil {
		t.Fatalf("GetMetricWithLabelValues: %v", err)
	}
	h.(prometheus.Metric).Write(m2)
	if got := m2.GetHistogram().GetSampleCount(); got != 1 {
		t.Errorf("duration histogram count = %v, want 1", got)
	}
}

// TestHTTPMiddleware_PreservesFlusher guards against a critical regression:
// responseWriter embeds http.ResponseWriter as an interface field, which
// does NOT promote Flush() (that method isn't part of the http.ResponseWriter
// interface, even though the concrete value underneath implements it).
// Every SSE handler in internal/api does a raw `w.(http.Flusher)` type
// assertion. Without an explicit Flush() method here, that assertion fails
// for every request wrapped by this middleware -- which is every request,
// since HTTPMiddleware wraps the whole handler in cmd/serve.go -- so every
// streaming endpoint would silently return a 200 with correct SSE headers
// and an empty body forever, with no error anywhere. This exact scenario
// is verified live in the project's manual testing; this test pins it down
// permanently at the unit level.
func TestHTTPMiddleware_PreservesFlusher(t *testing.T) {
	var gotFlusher bool
	handler := HTTPMiddleware(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		_, gotFlusher = w.(http.Flusher)
		w.WriteHeader(http.StatusOK)
	}))

	rec := httptest.NewRecorder() // httptest.ResponseRecorder implements http.Flusher
	handler.ServeHTTP(rec, httptest.NewRequest("GET", "/runs/abc/stream", nil))

	if !gotFlusher {
		t.Fatal("downstream handler could not type-assert http.Flusher through the metrics middleware -- every SSE endpoint would silently break")
	}
}

func TestHTTPMiddlewareSkipsMetricsPath(t *testing.T) {
	reg := prometheus.NewRegistry()
	reqTotal := prometheus.NewCounterVec(
		prometheus.CounterOpts{Name: "test_skip_requests_total", Help: "test"},
		[]string{"method", "path", "status"},
	)
	reg.MustRegister(reqTotal)

	origTotal := HTTPRequestsTotal
	HTTPRequestsTotal = reqTotal
	defer func() { HTTPRequestsTotal = origTotal }()

	called := false
	handler := HTTPMiddleware(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		called = true
	}))

	req := httptest.NewRequest("GET", "/metrics", nil)
	rec := httptest.NewRecorder()
	handler.ServeHTTP(rec, req)

	if !called {
		t.Error("/metrics request was not forwarded to next handler")
	}

	// Counter should not have been incremented
	gathered, _ := reg.Gather()
	for _, mf := range gathered {
		if mf.GetName() == "test_skip_requests_total" {
			t.Errorf("metrics path should not be counted, but found metric family")
		}
	}
}
