package httpx

import (
	"container/list"
	"context"
	"errors"
	"io"
	"log"
	"net/http"
	"regexp"
	"strings"
	"sync"
	"time"
)

type requestIDContextKey struct{}

var sensitiveLogPatterns = []*regexp.Regexp{
	regexp.MustCompile(`(?i)(bearer\s+)[A-Za-z0-9._~+/=-]+`),
	regexp.MustCompile(`(?i)(authorization["'=:\s]+)[^,\s"]+`),
	regexp.MustCompile(`(?i)(api[_-]?key["'=:\s]+)[^,\s"]+`),
	regexp.MustCompile(`(?i)(password["'=:\s]+)[^,\s"]+`),
	regexp.MustCompile(`\b(enc_)[A-Za-z0-9_-]{8,}\b`),
}

func requestIDMiddleware(next http.Handler) http.Handler {
	return http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		requestID := sanitizeRequestID(r.Header.Get("X-Request-ID"))
		if requestID == "" {
			requestID = NewID("req")
		}
		w.Header().Set("X-Request-ID", requestID)
		ctx := context.WithValue(r.Context(), requestIDContextKey{}, requestID)
		next.ServeHTTP(w, r.WithContext(ctx))
	})
}

func requestIDFromContext(ctx context.Context) string {
	id, _ := ctx.Value(requestIDContextKey{}).(string)
	return id
}

func sanitizeRequestID(value string) string {
	value = strings.TrimSpace(value)
	if value == "" || len(value) > 128 {
		return ""
	}
	for _, r := range value {
		if (r >= 'a' && r <= 'z') || (r >= 'A' && r <= 'Z') || (r >= '0' && r <= '9') || r == '-' || r == '_' || r == '.' {
			continue
		}
		return ""
	}
	return value
}

func (a *App) accessLogMiddleware(next http.Handler) http.Handler {
	return http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		start := time.Now()
		rec := &statusRecorder{ResponseWriter: w, status: http.StatusOK}
		next.ServeHTTP(rec, r)
		log.Printf(
			"request id=%s method=%s path=%s status=%d bytes=%d duration=%s remote=%s",
			requestIDFromContext(r.Context()),
			r.Method,
			redactForLog(r.URL.EscapedPath()),
			rec.status,
			rec.bytes,
			time.Since(start).Round(time.Millisecond),
			clientIP(r, a.trustProxy),
		)
	})
}

type statusRecorder struct {
	http.ResponseWriter
	status int
	bytes  int
}

func (r *statusRecorder) WriteHeader(status int) {
	r.status = status
	r.ResponseWriter.WriteHeader(status)
}

func (r *statusRecorder) Write(b []byte) (int, error) {
	n, err := r.ResponseWriter.Write(b)
	r.bytes += n
	return n, err
}

func (r *statusRecorder) Flush() {
	if flusher, ok := r.ResponseWriter.(http.Flusher); ok {
		flusher.Flush()
	}
}

func secureHeadersMiddleware(next http.Handler) http.Handler {
	return http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		h := w.Header()
		h.Set("X-Content-Type-Options", "nosniff")
		h.Set("X-Frame-Options", "DENY")
		h.Set("Referrer-Policy", "strict-origin-when-cross-origin")
		h.Set("Permissions-Policy", "camera=(), microphone=(), geolocation=()")
		h.Set("Content-Security-Policy", "default-src 'self'; script-src 'self' 'unsafe-inline'; style-src 'self' 'unsafe-inline' https://fonts.googleapis.com; font-src 'self' data: https://fonts.gstatic.com; img-src 'self' data:; connect-src 'self'; frame-ancestors 'none'; base-uri 'self'; form-action 'self'")
		next.ServeHTTP(w, r)
	})
}

func (a *App) corsMiddleware(next http.Handler) http.Handler {
	allowed := map[string]struct{}{}
	allowAll := false
	for _, origin := range a.corsOrigins {
		origin = strings.TrimSpace(origin)
		if origin == "" {
			continue
		}
		if origin == "*" {
			allowAll = true
			continue
		}
		allowed[origin] = struct{}{}
	}

	return http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		origin := strings.TrimSpace(r.Header.Get("Origin"))
		if origin != "" && (allowAll || originAllowed(allowed, origin)) {
			if allowAll {
				w.Header().Set("Access-Control-Allow-Origin", "*")
			} else {
				w.Header().Set("Access-Control-Allow-Origin", origin)
				w.Header().Add("Vary", "Origin")
				w.Header().Set("Access-Control-Allow-Credentials", "true")
			}
			w.Header().Set("Access-Control-Allow-Methods", "GET, POST, PUT, PATCH, DELETE, OPTIONS")
			allowHeaders, varyRequestHeaders := corsAllowHeaderValue(r.Header.Get("Access-Control-Request-Headers"))
			w.Header().Set("Access-Control-Allow-Headers", allowHeaders)
			if varyRequestHeaders {
				w.Header().Add("Vary", "Access-Control-Request-Headers")
			}
			w.Header().Set("Access-Control-Max-Age", "600")
		}
		if r.Method == http.MethodOptions {
			w.WriteHeader(http.StatusNoContent)
			return
		}
		next.ServeHTTP(w, r)
	})
}

func originAllowed(allowed map[string]struct{}, origin string) bool {
	_, ok := allowed[origin]
	return ok
}

var corsAllowedRequestHeaders = []string{
	"Authorization",
	"Content-Type",
	"X-API-Key",
	"X-Request-ID",
	"Idempotency-Key",
	"X-Client-Request-Id",
}

func corsAllowHeaderValue(requested string) (string, bool) {
	requested = strings.TrimSpace(requested)
	if requested == "" {
		return strings.Join(corsAllowedRequestHeaders, ", "), false
	}
	parts := strings.Split(requested, ",")
	reflected := make([]string, 0, len(parts))
	seen := make(map[string]struct{}, len(parts))
	for _, part := range parts {
		name := strings.TrimSpace(part)
		if !corsHeaderNameOK(name) {
			continue
		}
		canonical, ok := canonicalCORSHeader(name)
		if !ok {
			continue
		}
		lower := strings.ToLower(canonical)
		if _, dup := seen[lower]; dup {
			continue
		}
		seen[lower] = struct{}{}
		reflected = append(reflected, canonical)
	}
	return strings.Join(reflected, ", "), true
}

func canonicalCORSHeader(name string) (string, bool) {
	lower := strings.ToLower(name)
	for _, allowed := range corsAllowedRequestHeaders {
		if strings.ToLower(allowed) == lower {
			return allowed, true
		}
	}
	if strings.HasPrefix(lower, "openai-") {
		return name, true
	}
	return "", false
}

func corsHeaderNameOK(name string) bool {
	if name == "" || len(name) > 128 {
		return false
	}
	for _, r := range name {
		if r <= ' ' || r >= 127 {
			return false
		}
		switch r {
		case '(', ')', '<', '>', '@', ',', ';', ':', '\\', '"', '/', '[', ']', '?', '=', '{', '}':
			return false
		}
	}
	return true
}

func (a *App) maxRequestBodyBytes() int64 {
	maxUploadBytes := a.maxUploadMB * 1024 * 1024
	if maxUploadBytes <= 0 {
		maxUploadBytes = 16 * 1024 * 1024
	}
	chatLimit := int64(16 * 1024 * 1024)
	if maxUploadBytes < chatLimit {
		return chatLimit
	}
	return maxUploadBytes
}

func redactForLog(value string) string {
	out := value
	for _, pattern := range sensitiveLogPatterns {
		out = pattern.ReplaceAllString(out, "${1}<redacted>")
	}
	return out
}

// GateConfig bounds the proxy request surface (CON-01): how many /v1/
// requests may run at once, how many may queue, and how many request-body
// bytes may be held in flight across the queue. Zero means "unbounded";
// the gate is absent unless every configured dimension is wired through
// NewApp's inference gate parameter.
type GateConfig struct {
	// MaxInflight caps concurrently executing /v1 requests, counted until
	// the handler returns — a streamed response holds its slot until the
	// relay finishes, so early header return never frees it.
	MaxInflight int
	// MaxQueued bounds callers waiting for an inflight slot; overflow is
	// rejected explicitly with 429, never queued forever.
	MaxQueued int
	// MaxBodyBytes bounds bytes held by in-flight request bodies. Bodies
	// with a known Content-Length are charged before dispatch; unknown or
	// chunked lengths are charged as bytes actually arrive.
	MaxBodyBytes int64
}

// errOverloaded is the gate's explicit rejection; the middleware maps it to
// a bounded 429 instead of queueing forever.
var errOverloaded = errors.New("inference gate: overloaded")

// inferenceGate is the process-local request/byte admission for the proxy
// surface. It is a pure capacity guard: per-tenant fairness lives in the
// executor's admission pools, shared budget admission in PostgreSQL.
type inferenceGate struct {
	cfg GateConfig

	mu       sync.Mutex
	inflight int
	queued   int
	waiters  *list.List // FIFO of *gateWaiter
	bodyUsed int64
}

type gateWaiter struct {
	known   bool
	bodyLen int64
	granted bool
	wake    chan struct{}
}

func newInferenceGate(cfg GateConfig) *inferenceGate {
	if cfg.MaxInflight <= 0 {
		cfg.MaxInflight = 64
	}
	return &inferenceGate{cfg: cfg, waiters: list.New()}
}

// enter claims one inflight slot plus len bytes of body capacity, waiting
// FIFO until both fit. The returned release runs once when the request
// completes; for unknown-length bodies the gate tracks arrival through the
// wrapped reader instead of this charge.
func (g *inferenceGate) enter(ctx context.Context, bodyLen int64, known bool) (release func(), err error) {
	if known && g.cfg.MaxBodyBytes > 0 && bodyLen > g.cfg.MaxBodyBytes {
		return nil, errOverloaded
	}
	lease := func() func() {
		var once sync.Once
		return func() {
			once.Do(func() {
				g.mu.Lock()
				g.inflight--
				if known {
					g.bodyUsed -= bodyLen
				}
				g.grantLocked()
				g.mu.Unlock()
			})
		}
	}
	g.mu.Lock()
	if g.fitsLocked(bodyLen, known) && g.waiters.Len() == 0 {
		g.inflight++
		if known {
			g.bodyUsed += bodyLen
		}
		g.mu.Unlock()
		return lease(), nil
	}
	if g.cfg.MaxQueued <= 0 || g.queued >= g.cfg.MaxQueued {
		g.mu.Unlock()
		return nil, errOverloaded
	}
	g.queued++
	w := &gateWaiter{known: known, bodyLen: bodyLen, wake: make(chan struct{}, 1)}
	el := g.waiters.PushBack(w)
	g.mu.Unlock()

	select {
	case <-w.wake:
		// The grant already charged our slot and bytes.
		return lease(), nil
	case <-ctx.Done():
		g.mu.Lock()
		if w.granted {
			g.inflight--
			if known {
				g.bodyUsed -= bodyLen
			}
		} else {
			g.waiters.Remove(el)
			g.queued--
		}
		g.grantLocked()
		g.mu.Unlock()
		return nil, ctx.Err()
	}
}

// fitsLocked reports whether an inflight slot plus these body bytes fit.
// Unknown-length bodies don't pre-charge; their arrival is metered by
// gatedBody against the same pool.
func (g *inferenceGate) fitsLocked(bodyLen int64, known bool) bool {
	if g.inflight >= g.cfg.MaxInflight {
		return false
	}
	if known && g.cfg.MaxBodyBytes > 0 && g.bodyUsed+bodyLen > g.cfg.MaxBodyBytes {
		return false
	}
	return true
}

// grantLocked hands freed capacity to head-of-line waiters in FIFO order:
// a request whose bytes don't fit yet blocks the line rather than being
// leapfrogged — its own per-request oversize is rejected at enter.
func (g *inferenceGate) grantLocked() {
	for g.waiters.Len() > 0 {
		el := g.waiters.Front()
		w := el.Value.(*gateWaiter)
		if !g.fitsLocked(w.bodyLen, w.known) {
			return
		}
		g.waiters.Remove(el)
		g.queued--
		g.inflight++
		if w.known {
			g.bodyUsed += w.bodyLen
		}
		w.granted = true
		w.wake <- struct{}{}
	}
}

// chargeBody accounts for bytes consumed from an unknown-length body,
// waiting briefly for capacity so a chunked upload cannot flood the bound.
func (g *inferenceGate) chargeBody(ctx context.Context, n int64) error {
	for {
		g.mu.Lock()
		if g.cfg.MaxBodyBytes <= 0 || g.bodyUsed+n <= g.cfg.MaxBodyBytes {
			if g.cfg.MaxBodyBytes > 0 {
				g.bodyUsed += n
			}
			g.mu.Unlock()
			return nil
		}
		g.mu.Unlock()
		select {
		case <-ctx.Done():
			return ctx.Err()
		case <-time.After(5 * time.Millisecond):
		}
	}
}

func (g *inferenceGate) unchargeBody(n int64) {
	g.mu.Lock()
	if g.cfg.MaxBodyBytes > 0 {
		g.bodyUsed -= n
	}
	g.grantLocked()
	g.mu.Unlock()
}

// gatedBody charges arriving bytes of an unknown-length request body
// against the byte pool, releasing the charge on Close.
type gatedBody struct {
	io.ReadCloser
	gate    *inferenceGate
	ctx     context.Context
	charged int64
	once    sync.Once
}

func (b *gatedBody) Read(p []byte) (int, error) {
	n, err := b.ReadCloser.Read(p)
	if n <= 0 {
		return n, err
	}
	if cerr := b.gate.chargeBody(b.ctx, int64(n)); cerr != nil {
		return 0, cerr
	}
	b.charged += int64(n)
	return n, err
}

func (b *gatedBody) Close() error {
	err := b.ReadCloser.Close()
	b.once.Do(func() { b.gate.unchargeBody(b.charged) })
	return err
}

// inferenceAdmissionMiddleware bounds in-flight /v1 requests, queued
// callers and held body bytes (CON-01). Only the proxy surface is gated —
// dashboard/auth paths are unrelated and stay unbounded here.
func (a *App) inferenceAdmissionMiddleware(next http.Handler) http.Handler {
	return http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		if !strings.HasPrefix(r.URL.Path, "/v1/") {
			next.ServeHTTP(w, r)
			return
		}
		known := r.ContentLength >= 0
		release, err := a.gate.enter(r.Context(), r.ContentLength, known)
		if err != nil {
			if errors.Is(err, errOverloaded) {
				writeOverloaded(w)
				return
			}
			// Client went away while queued: nothing to write.
			return
		}
		defer release()
		if !known && r.Body != nil && a.gate.cfg.MaxBodyBytes > 0 {
			r.Body = &gatedBody{ReadCloser: r.Body, gate: a.gate, ctx: r.Context()}
		}
		next.ServeHTTP(w, r)
	})
}

// writeOverloaded is the gate's explicit rejection — 429 like rate limiting,
// distinct from an upstream budget verdict.
func writeOverloaded(w http.ResponseWriter) {
	WriteOpenAIError(w, http.StatusTooManyRequests, "service is at capacity; retry shortly", "rate_limit_error", "overloaded")
}
