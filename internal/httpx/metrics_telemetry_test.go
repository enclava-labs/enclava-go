package httpx

import (
	"context"
	"net/http"
	"net/http/httptest"
	"strings"
	"testing"

	"enclava-go/internal/telemetry"
)

type ctxKey struct{}

// TestTelemetryMetricsRoutePattern proves the metrics middleware reads the
// mux-set r.Pattern even when an outer middleware clones the request via
// r.WithContext, and that unmatched routes fall back to "unmatched".
func TestTelemetryMetricsRoutePattern(t *testing.T) {
	m := telemetry.NewMetrics()
	a := &App{}
	a.ConfigureMetrics("", true, nil, nil)
	a.ConfigureTelemetryMetrics(m)
	a.mux = http.NewServeMux()
	a.mux.HandleFunc("GET /ping", func(w http.ResponseWriter, _ *http.Request) {
		if _, ok := w.(http.Flusher); !ok {
			t.Error("handler lost http.Flusher")
		}
		w.WriteHeader(http.StatusTeapot)
	})
	a.mux.HandleFunc("/metrics", a.handleMetrics)

	var h http.Handler = a.mux
	h = a.metricsMiddleware(h)
	// requestIDMiddleware-style outer wrapper cloning the request.
	withCtx := func(next http.Handler) http.Handler {
		return http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
			next.ServeHTTP(w, r.WithContext(context.WithValue(r.Context(), ctxKey{}, 1)))
		})
	}
	checkCtx := func(next http.Handler) http.Handler {
		return http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
			if _, ok := r.Context().Value(ctxKey{}).(int); !ok {
				t.Error("outer context value lost")
			}
			next.ServeHTTP(w, r)
		})
	}

	full := withCtx(checkCtx(h))
	resp := httptest.NewRecorder()
	full.ServeHTTP(resp, httptest.NewRequest(http.MethodGet, "/ping", nil))
	if resp.Code != http.StatusTeapot {
		t.Fatalf("ping status = %d", resp.Code)
	}

	resp2 := httptest.NewRecorder()
	full.ServeHTTP(resp2, httptest.NewRequest(http.MethodGet, "/nope", nil))
	if resp2.Code != http.StatusNotFound {
		t.Fatalf("unmatched status = %d", resp2.Code)
	}

	resp3 := httptest.NewRecorder()
	full.ServeHTTP(resp3, httptest.NewRequest(http.MethodGet, "/metrics", nil))
	body := resp3.Body.String()
	if !strings.Contains(body, "enclava_jobs") {
		t.Error("hand-written /metrics gauges missing")
	}
	for _, want := range []string{
		"enclava_http_requests_total{method=\"GET\",route=\"GET /ping\",status=\"418\"} 1",
		"enclava_http_requests_total{method=\"GET\",route=\"unmatched\",status=\"404\"} 1",
	} {
		if !strings.Contains(body, want) {
			t.Errorf("metrics output missing %q", want)
		}
	}
}
