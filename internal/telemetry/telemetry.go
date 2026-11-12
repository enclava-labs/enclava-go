// Package telemetry wires optional Prometheus metrics and OTLP tracing.
// Both are off by default: metrics only when METRICS_ADDR is set, tracing
// only when OTEL_EXPORTER_OTLP_ENDPOINT is set.
package telemetry

import (
	"context"
	"io"
	"net/http"
	"os"
	"strconv"
	"strings"
	"sync/atomic"
	"time"

	"github.com/prometheus/client_golang/prometheus"
	"github.com/prometheus/common/expfmt"
	"go.opentelemetry.io/contrib/instrumentation/net/http/otelhttp"
	"go.opentelemetry.io/otel"
	"go.opentelemetry.io/otel/exporters/otlp/otlptrace/otlptracehttp"
	"go.opentelemetry.io/otel/sdk/resource"
	sdktrace "go.opentelemetry.io/otel/sdk/trace"
	semconv "go.opentelemetry.io/otel/semconv/v1.34.0"
)

// tracingEnabled gates client transport wrapping so the no-tracing path adds
// zero overhead.
var tracingEnabled atomic.Bool

// Metrics holds the private registry and the HTTP request collectors.
type Metrics struct {
	registry *prometheus.Registry
	requests *prometheus.CounterVec
	duration *prometheus.HistogramVec
}

// NewMetrics builds the HTTP collectors on a private registry. Go runtime and
// process gauges stay hand-written in httpx /metrics; registering the stock
// collectors here would emit duplicate families and break the scrape.
func NewMetrics() *Metrics {
	m := &Metrics{registry: prometheus.NewRegistry()}
	m.requests = prometheus.NewCounterVec(prometheus.CounterOpts{
		Name: "enclava_http_requests_total",
	}, []string{"method", "route", "status"})
	m.duration = prometheus.NewHistogramVec(prometheus.HistogramOpts{
		Name: "enclava_http_request_duration_seconds",
	}, []string{"method", "route"})
	m.registry.MustRegister(m.requests, m.duration)
	return m
}

// WriteText appends the registry's metrics in Prometheus text format. It
// writes no status/header, so it can follow the hand-written /metrics gauges
// in the same response.
func (m *Metrics) WriteText(w io.Writer) error {
	families, err := m.registry.Gather()
	if err != nil {
		return err
	}
	enc := expfmt.NewEncoder(w, expfmt.NewFormat(expfmt.TypeTextPlain))
	for _, family := range families {
		if err := enc.Encode(family); err != nil {
			return err
		}
	}
	return nil
}

// Observe records one completed HTTP request.
func (m *Metrics) Observe(method, route string, status int, d time.Duration) {
	m.requests.WithLabelValues(method, route, strconv.Itoa(status)).Inc()
	m.duration.WithLabelValues(method, route).Observe(d.Seconds())
}

// InitTracing installs the OTLP trace SDK when OTEL_EXPORTER_OTLP_ENDPOINT is
// set; otherwise it returns a no-op shutdown. The returned func flushes
// pending spans and must be called on shutdown.
func InitTracing(ctx context.Context) (func(context.Context) error, error) {
	endpoint := strings.TrimSpace(os.Getenv("OTEL_EXPORTER_OTLP_ENDPOINT"))
	if endpoint == "" {
		return func(context.Context) error { return nil }, nil
	}
	serviceName := strings.TrimSpace(os.Getenv("OTEL_SERVICE_NAME"))
	if serviceName == "" {
		serviceName = "enclava"
	}
	exp, err := otlptracehttp.New(ctx, otlptracehttp.WithEndpointURL(endpoint))
	if err != nil {
		return nil, err
	}
	res, err := resource.New(ctx, resource.WithAttributes(semconv.ServiceName(serviceName)))
	if err != nil {
		return nil, err
	}
	tp := sdktrace.NewTracerProvider(sdktrace.WithBatcher(exp), sdktrace.WithResource(res))
	otel.SetTracerProvider(tp)
	tracingEnabled.Store(true)
	return tp.Shutdown, nil
}

// WrapTransport wraps an outbound HTTP transport with otelhttp when tracing
// is enabled; otherwise the transport is returned unchanged.
func WrapTransport(rt http.RoundTripper) http.RoundTripper {
	if !tracingEnabled.Load() {
		return rt
	}
	return otelhttp.NewTransport(rt)
}

// WrapHandler adds a server span around the main handler when tracing is
// enabled.
func WrapHandler(h http.Handler) http.Handler {
	if !tracingEnabled.Load() {
		return h
	}
	return otelhttp.NewHandler(h, "enclava")
}
