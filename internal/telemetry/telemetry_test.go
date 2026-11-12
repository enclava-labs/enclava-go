package telemetry

import (
	"bytes"
	"net/http"
	"strings"
	"testing"
)

func TestWriteTextContainsCollectors(t *testing.T) {
	m := NewMetrics()
	m.Observe(http.MethodGet, "GET /ping", http.StatusOK, 0)
	var buf bytes.Buffer
	if err := m.WriteText(&buf); err != nil {
		t.Fatalf("write text: %v", err)
	}
	out := buf.String()
	for _, want := range []string{
		"enclava_http_requests_total",
		"enclava_http_request_duration_seconds",
		"route=\"GET /ping\"",
	} {
		if !strings.Contains(out, want) {
			t.Errorf("metrics output missing %q", want)
		}
	}
	// httpx /metrics already writes go_goroutines by hand; a second family
	// with the same name makes Prometheus reject the scrape.
	if strings.Contains(out, "go_goroutines") {
		t.Error("registry must not export stock Go collectors")
	}
}

func TestWrapTransportNoopWhenTracingDisabled(t *testing.T) {
	base := http.DefaultTransport
	if got := WrapTransport(base); got != base {
		t.Fatalf("WrapTransport wrapped transport with tracing disabled")
	}
}
