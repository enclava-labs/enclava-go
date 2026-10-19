package llm

import (
	"bytes"
	"context"
	"errors"
	"fmt"
	"io"
	"net"
	"net/http"
	"net/url"
	"strings"
	"sync"
	"time"
)

type UpstreamError struct {
	StatusCode int
	Body       []byte
	Message    string
}

func (e *UpstreamError) Error() string {
	if e == nil {
		return ""
	}
	if e.Message != "" {
		return e.Message
	}
	return "upstream provider error"
}

type Provider interface {
	ListModels(ctx context.Context) (ModelsResponse, error)
	GetModel(ctx context.Context, modelID string) (Model, error)
	CreateChatCompletion(ctx context.Context, body []byte) ([]byte, error)
	CreateChatCompletionStream(ctx context.Context, body []byte) (io.ReadCloser, error)
	CreateEmbeddings(ctx context.Context, body []byte) ([]byte, error)
}

type ResponsesProvider interface {
	DoResponsesRequest(ctx context.Context, method, path string, body []byte) ([]byte, error)
	DoResponsesStream(ctx context.Context, method, path string, body []byte) (io.ReadCloser, error)
}

type HealthProvider interface {
	Health(ctx context.Context) error
}

type ModelCacheInvalidator interface {
	InvalidateModelCache()
}

type AttestationStatus struct {
	Enabled            bool      `json:"enabled"`
	Verified           bool      `json:"verified"`
	Provider           string    `json:"provider"`
	Enclave            string    `json:"enclave,omitempty"`
	Repo               string    `json:"repo,omitempty"`
	Digest             string    `json:"digest,omitempty"`
	CodeFingerprint    string    `json:"code_fingerprint,omitempty"`
	EnclaveFingerprint string    `json:"enclave_fingerprint,omitempty"`
	VerifiedAt         time.Time `json:"verified_at,omitempty"`
	LastError          string    `json:"last_error,omitempty"`
}

type AttestationProvider interface {
	VerifyAttestation(ctx context.Context) (AttestationStatus, error)
	AttestationStatus() AttestationStatus
}

// --- Endpoint transport policy ---
//
// All client traffic to an approved endpoint shares pooled transports keyed by
// origin + private-allowance. The dial guard resolves the real dial address
// once and pins it, so DNS rebinding cannot swap the approved origin for an
// unapproved address after validation.

const (
	maxJSONBodyBytes      int64 = 8 * 1024 * 1024
	maxErrorBodyBytes     int64 = 2 * 1024 * 1024
	defaultResponseHeader       = 30 * time.Second
	defaultStreamHeader         = 5 * time.Minute
	defaultStreamIdle           = 3 * time.Minute
	defaultStreamWall           = 30 * time.Minute
)

var errResponseTooLarge = errors.New("upstream response body exceeds maximum size")

type transportKey struct {
	origin               string
	allowPrivate         bool
	responseHeaderSecond int64
}

var sharedTransports sync.Map // transportKey -> *http.Transport

// endpointTransport returns a shared pooled transport for the given origin.
// allowPrivate permits dialing private/loopback addresses (explicit endpoint
// approval); link-local/metadata, unspecified and multicast targets are never
// permitted.
func endpointTransport(origin string, allowPrivate bool, responseHeaderTimeout time.Duration) http.RoundTripper {
	key := transportKey{origin: origin, allowPrivate: allowPrivate, responseHeaderSecond: int64(responseHeaderTimeout)}
	if t, ok := sharedTransports.Load(key); ok {
		return t.(*http.Transport)
	}
	base := http.DefaultTransport.(*http.Transport).Clone()
	base.DialContext = func(ctx context.Context, network, addr string) (net.Conn, error) {
		return guardedDialContext(ctx, network, addr, allowPrivate)
	}
	base.ResponseHeaderTimeout = responseHeaderTimeout
	actual, _ := sharedTransports.LoadOrStore(key, base)
	return actual.(*http.Transport)
}

// guardedDialContext resolves addr once, enforces the endpoint's approved
// address policy, and dials the resolved IP so the connection cannot be
// rebound by a later DNS change.
func guardedDialContext(ctx context.Context, network, addr string, allowPrivate bool) (net.Conn, error) {
	host, port, err := net.SplitHostPort(addr)
	if err != nil {
		return nil, fmt.Errorf("invalid dial address %q: %w", addr, err)
	}
	ips, err := net.DefaultResolver.LookupIPAddr(ctx, host)
	if err != nil {
		return nil, fmt.Errorf("resolve endpoint host %q: %w", host, err)
	}
	var allowed []net.IP
	for _, ipa := range ips {
		if addrAllowed(ipa.IP, allowPrivate) {
			allowed = append(allowed, ipa.IP)
		}
	}
	if len(allowed) == 0 {
		return nil, fmt.Errorf("endpoint host %q resolved to no approved addresses", host)
	}
	dialer := &net.Dialer{Timeout: 30 * time.Second, KeepAlive: 30 * time.Second}
	var lastErr error
	for _, ip := range allowed {
		conn, err := dialer.DialContext(ctx, network, net.JoinHostPort(ip.String(), port))
		if err == nil {
			return conn, nil
		}
		lastErr = err
	}
	return nil, fmt.Errorf("dial endpoint %q: %w", host, lastErr)
}

// addrAllowed reports whether a resolved dial address may receive endpoint
// credentials. Link-local/metadata, unspecified and multicast targets are
// always refused; private/loopback require explicit approval.
func addrAllowed(ip net.IP, allowPrivate bool) bool {
	if ip == nil || ip.IsUnspecified() || ip.IsMulticast() || ip.IsLinkLocalUnicast() || ip.IsLinkLocalMulticast() {
		return false
	}
	if !allowPrivate && (ip.IsPrivate() || ip.IsLoopback()) {
		return false
	}
	return true
}

// validateEndpointBaseURL checks the configured origin before any request is
// built. It does not resolve names — the dial guard approves the actual dial
// address — but it rejects malformed origins, cleartext or literal-IP private
// endpoints that lack explicit approval.
func validateEndpointBaseURL(raw string, allowPrivate bool) (*url.URL, error) {
	u, err := url.Parse(strings.TrimSpace(raw))
	if err != nil || u.Scheme == "" || u.Host == "" {
		return nil, fmt.Errorf("invalid endpoint base URL")
	}
	if u.User != nil || u.RawQuery != "" || u.Fragment != "" {
		return nil, fmt.Errorf("endpoint base URL must not contain credentials, query, or fragment")
	}
	scheme := strings.ToLower(u.Scheme)
	if scheme != "https" && scheme != "http" {
		return nil, fmt.Errorf("endpoint base URL scheme %q is not http(s)", u.Scheme)
	}
	host := u.Hostname()
	if ip := net.ParseIP(host); ip != nil && !addrAllowed(ip, allowPrivate) {
		return nil, fmt.Errorf("endpoint address %s is not an approved target", host)
	}
	if !allowPrivate && (scheme == "http" || strings.EqualFold(host, "localhost") || strings.HasSuffix(strings.ToLower(host), ".local") || strings.HasSuffix(strings.ToLower(host), ".internal")) {
		return nil, fmt.Errorf("endpoint %q is a local/cleartext target and requires explicit approval", host)
	}
	return u, nil
}

// originOf returns the scheme://hostport identity used for redirect and
// transport-pool isolation.
func originOf(u *url.URL) string {
	return strings.ToLower(u.Scheme) + "://" + strings.ToLower(u.Host)
}

// sameOriginRedirectPolicy allows at most 5 redirects and refuses any redirect
// that changes origin, so endpoint credentials can never be forwarded to a
// credential-changing redirect target.
func sameOriginRedirectPolicy(req *http.Request, via []*http.Request) error {
	if len(via) >= 5 {
		return fmt.Errorf("stopped after 5 redirects")
	}
	if len(via) > 0 && !strings.EqualFold(originOf(req.URL), originOf(via[0].URL)) {
		return fmt.Errorf("refusing cross-origin redirect to %s", req.URL.Redacted())
	}
	return nil
}

// noRetryBody wraps the payload so net/http cannot set GetBody, which disables
// client-level replay of a possibly accepted mutation or partial stream.
func noRetryBody(body []byte) io.Reader {
	if body == nil {
		return nil
	}
	return io.NopCloser(bytes.NewReader(body))
}

// readBoundedBody reads the full body, failing explicitly on overflow or
// truncation rather than returning clipped success JSON.
func readBoundedBody(body io.ReadCloser, limit int64) ([]byte, error) {
	defer func() { _ = body.Close() }()
	b, err := io.ReadAll(io.LimitReader(body, limit+1))
	if err != nil {
		return nil, fmt.Errorf("read upstream response: %w", err)
	}
	if int64(len(b)) > limit {
		return nil, fmt.Errorf("%w (limit %d bytes)", errResponseTooLarge, limit)
	}
	return b, nil
}

// boundedStreamBody wraps a streaming response body with cancellable idle and
// wall-clock limits. Timers stop on Close; expiry closes the real body.
type boundedStreamBody struct {
	body      io.ReadCloser
	idle      time.Duration
	idleTimer *time.Timer
	wallTimer *time.Timer
	closed    chan struct{}
	closeOnce sync.Once
}

func newBoundedStreamBody(body io.ReadCloser, idle, wall time.Duration) *boundedStreamBody {
	b := &boundedStreamBody{body: body, idle: idle, closed: make(chan struct{})}
	if idle > 0 {
		b.idleTimer = time.AfterFunc(idle, b.expire)
	}
	if wall > 0 {
		b.wallTimer = time.AfterFunc(wall, b.expire)
	}
	return b
}

func (b *boundedStreamBody) expire() {
	b.closeOnce.Do(func() {
		close(b.closed)
		_ = b.body.Close()
	})
}

func (b *boundedStreamBody) Read(p []byte) (int, error) {
	select {
	case <-b.closed:
		return 0, fmt.Errorf("upstream stream terminated: %w", context.DeadlineExceeded)
	default:
	}
	n, err := b.body.Read(p)
	if n > 0 && b.idleTimer != nil {
		b.idleTimer.Reset(b.idle)
	}
	return n, err
}

func (b *boundedStreamBody) Close() error {
	err := b.body.Close()
	b.closeOnce.Do(func() { close(b.closed) })
	if b.idleTimer != nil {
		b.idleTimer.Stop()
	}
	if b.wallTimer != nil {
		b.wallTimer.Stop()
	}
	return err
}
