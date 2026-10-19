package llm

import (
	"context"
	"io"
	"net"
	"net/http"
	"net/http/httptest"
	"strings"
	"sync/atomic"
	"testing"
	"time"
)

func TestDoPassthroughHTTPForwardsOpenAIHeadersAndClientRequestID(t *testing.T) {
	var got http.Header
	var gotAuth string
	server := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		got = r.Header.Clone()
		gotAuth = r.Header.Get("Authorization")
		w.Header().Set("Content-Type", "application/json")
		w.WriteHeader(http.StatusAccepted)
		_, _ = w.Write([]byte(`{"ok":true}`))
	}))
	defer server.Close()

	headers := http.Header{}
	headers.Set("Authorization", "Bearer caller-key")
	headers.Set("OpenAI-Beta", "assistants=v2")
	headers.Set("OpenAI-Project", "proj_123")
	headers.Set("X-Client-Request-Id", "client-123")

	resp, err := doPassthroughHTTP(context.Background(), server.Client(), server.URL, "provider-key", http.MethodPost, "/assistants", []byte(`{}`), headers)
	if err != nil {
		t.Fatalf("passthrough request failed: %v", err)
	}
	defer func() { _ = resp.Body.Close() }()
	_, _ = io.ReadAll(resp.Body)

	if resp.StatusCode != http.StatusAccepted {
		t.Fatalf("expected status 202, got %d", resp.StatusCode)
	}
	if gotAuth != "Bearer provider-key" {
		t.Fatalf("expected provider authorization, got %q", gotAuth)
	}
	if got.Get("OpenAI-Beta") != "assistants=v2" {
		t.Fatalf("expected OpenAI-Beta header to be forwarded")
	}
	if got.Get("OpenAI-Project") != "proj_123" {
		t.Fatalf("expected OpenAI-Project header to be forwarded")
	}
	if got.Get("X-Client-Request-Id") != "client-123" {
		t.Fatalf("expected X-Client-Request-Id header to be forwarded")
	}
}

func TestDoPassthroughHTTPRejectsDotDotAndKeepsNormalPathUnderBase(t *testing.T) {
	var gotURI string
	calls := 0
	client := &http.Client{Transport: roundTripFunc(func(r *http.Request) (*http.Response, error) {
		calls++
		gotURI = r.URL.RequestURI()
		return &http.Response{
			StatusCode: http.StatusOK,
			Header:     make(http.Header),
			Body:       io.NopCloser(strings.NewReader(`{"ok":true}`)),
			Request:    r,
		}, nil
	})}
	base := "https://api.example.com/v1"
	rejected := []string{
		"/chat/completions/../../../latest/meta-data",
		"/chat/completions/%2e%2e/%2E%2E/latest/meta-data",
		`/chat/completions/\latest`,
		"https://evil.example/latest/meta-data",
	}
	for _, path := range rejected {
		calls = 0
		_, err := doPassthroughHTTP(context.Background(), client, base, "provider-key", http.MethodPost, path, []byte(`{}`), nil)
		if err == nil {
			t.Fatalf("expected %q to be rejected", path)
		}
		if calls != 0 {
			t.Fatalf("rejected path %q was sent to %s", path, gotURI)
		}
	}

	resp, err := doPassthroughHTTP(context.Background(), client, base, "provider-key", http.MethodPost, "/chat/completions", []byte(`{}`), nil)
	if err != nil {
		t.Fatalf("normal passthrough failed: %v", err)
	}
	defer func() { _ = resp.Body.Close() }()
	if _, err := io.ReadAll(resp.Body); err != nil {
		t.Fatalf("read response: %v", err)
	}
	if calls != 1 || gotURI != "/v1/chat/completions" {
		t.Fatalf("expected URL under base, calls=%d uri=%q", calls, gotURI)
	}
}

func TestPassthroughStreamClientSelection(t *testing.T) {
	cases := []struct {
		name    string
		path    string
		headers map[string]string
		body    string
		want    string
	}{
		{
			name:    "json stream true",
			path:    "/chat/completions",
			headers: map[string]string{"Accept": "application/json", "Content-Type": "application/json"},
			body:    `{"stream": true, "model": "m"}`,
			want:    "stream",
		},
		{
			name:    "json stream false",
			path:    "/chat/completions",
			headers: map[string]string{"Accept": "application/json", "Content-Type": "application/json"},
			body:    `{"stream": false}`,
			want:    "timed",
		},
		{
			name:    "invalid json",
			path:    "/chat/completions",
			headers: map[string]string{"Accept": "application/json", "Content-Type": "application/json"},
			body:    `{"stream":`,
			want:    "timed",
		},
		{
			name:    "empty content type stream true",
			path:    "/chat/completions",
			headers: map[string]string{"Accept": "application/json"},
			body:    `{"stream": true}`,
			want:    "stream",
		},
		{
			name:    "accept event stream",
			path:    "/chat/completions",
			headers: map[string]string{"Accept": "text/event-stream", "Content-Type": "application/json"},
			body:    `{"stream": false}`,
			want:    "stream",
		},
		{
			name:    "query stream",
			path:    "/chat/completions?stream=true",
			headers: map[string]string{"Accept": "application/json"},
			body:    ``,
			want:    "stream",
		},
	}
	for _, tc := range cases {
		t.Run(tc.name, func(t *testing.T) {
			chosen := ""
			var gotPath string
			trip := func(name string) http.RoundTripper {
				return roundTripFunc(func(r *http.Request) (*http.Response, error) {
					chosen = name
					gotPath = r.URL.Path
					return &http.Response{
						StatusCode: http.StatusOK,
						Header:     make(http.Header),
						Body:       io.NopCloser(strings.NewReader(`{"ok":true}`)),
						Request:    r,
					}, nil
				})
			}
			adapter := &OpenAICompatibleAdapter{
				baseURL:      "https://api.example.com/v1",
				apiKey:       "provider-key",
				client:       &http.Client{Transport: trip("timed")},
				streamClient: &http.Client{Transport: trip("stream")},
			}
			headers := http.Header{}
			for key, value := range tc.headers {
				headers.Set(key, value)
			}
			resp, err := adapter.DoPassthroughRequest(context.Background(), http.MethodPost, tc.path, []byte(tc.body), headers)
			if err != nil {
				t.Fatalf("passthrough request failed: %v", err)
			}
			defer func() { _ = resp.Body.Close() }()
			if chosen != tc.want {
				t.Fatalf("client %s, want %s", chosen, tc.want)
			}
			if gotPath != "/v1/chat/completions" {
				t.Fatalf("expected URL under base, got %s", gotPath)
			}
		})
	}
}

type roundTripFunc func(*http.Request) (*http.Response, error)

func (f roundTripFunc) RoundTrip(r *http.Request) (*http.Response, error) {
	return f(r)
}

func TestAdapterRejectsUnapprovedPrivateEndpoint(t *testing.T) {
	adapter := NewOpenAICompatibleAdapter("http://127.0.0.1:9999/v1", "secret-key", time.Second)
	if _, err := adapter.ListModels(context.Background()); err == nil {
		t.Fatal("expected unapproved private endpoint to be refused")
	}
	if _, err := adapter.CreateChatCompletion(context.Background(), []byte(`{"model":"m"}`)); err == nil {
		t.Fatal("expected mutation to unapproved private endpoint to be refused")
	}
	if _, err := adapter.DoPassthroughRequest(context.Background(), http.MethodPost, "/chat/completions", []byte(`{}`), nil); err == nil {
		t.Fatal("expected passthrough to unapproved private endpoint to be refused")
	}
}

func TestAdapterRejectsMetadataTarget(t *testing.T) {
	adapter := NewOpenAICompatibleAdapter("http://169.254.169.254/latest", "secret-key", time.Second).
		WithAllowPrivateEndpoint()
	if _, err := adapter.ListModels(context.Background()); err == nil {
		t.Fatal("expected link-local metadata target to be refused even with approval")
	}
}

func TestAdapterRedirectDoesNotLeakCredentials(t *testing.T) {
	var secondAuth string
	second := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		secondAuth = r.Header.Get("Authorization")
		w.WriteHeader(http.StatusOK)
		_, _ = w.Write([]byte(`{}`))
	}))
	defer second.Close()

	first := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		http.Redirect(w, r, second.URL+"/x", http.StatusFound)
	}))
	defer first.Close()

	adapter := NewOpenAICompatibleAdapter(first.URL, "secret-key", 5*time.Second).WithAllowPrivateEndpoint()
	_, err := adapter.ListModels(context.Background())
	if err == nil {
		t.Fatal("expected cross-origin redirect to be refused")
	}
	if secondAuth != "" {
		t.Fatalf("endpoint credentials reached redirect target: %q", secondAuth)
	}
}

func TestAdapterOversizedBodyIsExplicitFailure(t *testing.T) {
	server := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		w.Header().Set("Content-Type", "application/json")
		_, _ = w.Write(make([]byte, 4096))
	}))
	defer server.Close()

	adapter := NewOpenAICompatibleAdapter(server.URL, "k", 5*time.Second).WithAllowPrivateEndpoint()
	adapter.maxBody = 64
	_, err := adapter.ListModels(context.Background())
	if err == nil || !strings.Contains(err.Error(), "exceeds maximum size") {
		t.Fatalf("expected explicit overflow failure, got %v", err)
	}
}

func TestAdapterTruncatedBodyIsFailure(t *testing.T) {
	server := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		w.Header().Set("Content-Length", "100")
		w.Header().Set("Content-Type", "application/json")
		_, _ = w.Write([]byte(`{"short"`))
	}))
	defer server.Close()

	adapter := NewOpenAICompatibleAdapter(server.URL, "k", 5*time.Second).WithAllowPrivateEndpoint()
	if _, err := adapter.ListModels(context.Background()); err == nil {
		t.Fatal("expected truncated body to fail, not return clipped JSON")
	}
}

func TestAdapterMutationIsNotReplayed(t *testing.T) {
	listener, err := net.Listen("tcp", "127.0.0.1:0")
	if err != nil {
		t.Fatalf("listen: %v", err)
	}
	var calls atomic.Int32
	server := &http.Server{Handler: http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		if calls.Add(1) == 1 {
			// Accept the mutation, then drop the connection before responding:
			// the request may have been accepted, so the client must not replay it.
			if hj, ok := w.(http.Hijacker); ok {
				conn, _, _ := hj.Hijack()
				_ = conn.Close()
				return
			}
		}
		w.WriteHeader(http.StatusOK)
		_, _ = w.Write([]byte(`{"ok":true}`))
	})}
	go func() { _ = server.Serve(listener) }()
	defer func() { _ = server.Close() }()

	adapter := NewOpenAICompatibleAdapter("http://"+listener.Addr().String(), "k", 5*time.Second).
		WithAllowPrivateEndpoint()
	_, _ = adapter.CreateChatCompletion(context.Background(), []byte(`{"model":"m"}`))
	if got := calls.Load(); got != 1 {
		t.Fatalf("possibly accepted mutation was replayed: %d requests", got)
	}
}

func TestAdapterStreamCancelClosesUpstreamBody(t *testing.T) {
	bodyClosed := make(chan struct{})
	server := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		w.Header().Set("Content-Type", "text/event-stream")
		w.WriteHeader(http.StatusOK)
		if f, ok := w.(http.Flusher); ok {
			f.Flush()
		}
		select {
		case <-r.Context().Done():
			close(bodyClosed)
		case <-time.After(5 * time.Second):
		}
	}))
	defer server.Close()

	ctx, cancel := context.WithCancel(context.Background())
	adapter := NewOpenAICompatibleAdapter(server.URL, "k", 5*time.Second).WithAllowPrivateEndpoint()
	stream, err := adapter.CreateChatCompletionStream(ctx, []byte(`{"stream":true}`))
	if err != nil {
		t.Fatalf("stream: %v", err)
	}
	cancel()
	_ = stream.Close()
	select {
	case <-bodyClosed:
	case <-time.After(3 * time.Second):
		t.Fatal("cancellation did not close the real upstream body")
	}
}

func TestAdapterStreamIdleLimit(t *testing.T) {
	server := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		w.Header().Set("Content-Type", "text/event-stream")
		w.WriteHeader(http.StatusOK)
		if f, ok := w.(http.Flusher); ok {
			f.Flush()
			_, _ = w.Write([]byte("data: hello\n\n"))
			f.Flush()
		}
		// Then go silent forever.
		select {
		case <-r.Context().Done():
		case <-time.After(5 * time.Second):
		}
	}))
	defer server.Close()

	adapter := NewOpenAICompatibleAdapter(server.URL, "k", 5*time.Second).
		WithAllowPrivateEndpoint().
		WithStreamLimits(150*time.Millisecond, 0)
	stream, err := adapter.CreateChatCompletionStream(context.Background(), []byte(`{"stream":true}`))
	if err != nil {
		t.Fatalf("stream: %v", err)
	}
	defer func() { _ = stream.Close() }()
	buf := make([]byte, 64)
	_, err = stream.Read(buf)
	if err != nil {
		t.Fatalf("first read: %v", err)
	}
	_, err = stream.Read(buf)
	if err == nil {
		t.Fatal("expected idle timeout to terminate the silent stream")
	}
}
