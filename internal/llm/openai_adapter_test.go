package llm

import (
	"context"
	"net"
	"net/http"
	"net/http/httptest"
	"strings"
	"sync/atomic"
	"testing"
	"time"
)

func TestAdapterRejectsUnapprovedPrivateEndpoint(t *testing.T) {
	adapter := NewOpenAICompatibleAdapter("http://127.0.0.1:9999/v1", "secret-key", time.Second)
	if _, err := adapter.ListModels(context.Background()); err == nil {
		t.Fatal("expected unapproved private endpoint to be refused")
	}
	if _, err := adapter.CreateChatCompletion(context.Background(), []byte(`{"model":"m"}`)); err == nil {
		t.Fatal("expected mutation to unapproved private endpoint to be refused")
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
