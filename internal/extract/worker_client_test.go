package extract

import (
	"context"
	"errors"
	"net"
	"net/http"
	"path/filepath"
	"strings"
	"testing"
	"time"
)

// serveWorkerSocket binds an http.Server on a fresh unix socket and returns
// its path; cleanup closes both.
func serveWorkerSocket(t *testing.T, h http.Handler) string {
	t.Helper()
	path := filepath.Join(t.TempDir(), "worker.sock")
	listener, err := net.Listen("unix", path)
	if err != nil {
		t.Fatal(err)
	}
	server := &http.Server{Handler: h, ReadHeaderTimeout: 10 * time.Second}
	go func() { _ = server.Serve(listener) }()
	t.Cleanup(func() { _ = server.Close(); _ = listener.Close() })
	return path
}

// hijackCloseHandler grabs the raw connection and drops it: withHeader
// writes a truncated response first (mid-body EOF), withoutHeader closes
// before any response bytes (reset while reading headers).
func hijackCloseHandler(t *testing.T, withHeader bool) http.Handler {
	return http.HandlerFunc(func(w http.ResponseWriter, _ *http.Request) {
		hj, ok := w.(http.Hijacker)
		if !ok {
			t.Fatal("cannot hijack connection")
		}
		conn, _, err := hj.Hijack()
		if err != nil {
			t.Fatal(err)
		}
		if withHeader {
			_, _ = conn.Write([]byte("HTTP/1.1 200 OK\r\nContent-Type: application/json\r\nContent-Length: 500\r\n\r\n{\"blocks\":["))
		}
		_ = conn.Close()
	})
}

func workerParseErr(t *testing.T) error {
	t.Helper()
	_, _, err := parseViaWorker(t.Context(), []byte("%PDF-1.4 stub"), "pdf")
	return err
}

// TestWorkerUnavailableSentinels pins plan §3.7: dial-time failures, gateway
// statuses and worker-side timeouts wrap ErrWorkerUnavailable (never the
// mid-parse sentinel), so ingest defers instead of failing.
func TestWorkerUnavailableSentinels(t *testing.T) {
	missing := filepath.Join(t.TempDir(), "absent.sock")
	cases := map[string]func(t *testing.T){
		"absent socket": func(t *testing.T) {
			SetNativeWorker(missing)
			err := workerParseErr(t)
			if !errors.Is(err, ErrWorkerUnavailable) || errors.Is(err, ErrWorkerDroppedMidParse) {
				t.Fatalf("absent socket: err = %v, want ErrWorkerUnavailable only", err)
			}
		},
		"connection refused": func(t *testing.T) {
			// A bound-then-closed socket path: dial fails with refusal.
			l, err := net.Listen("unix", filepath.Join(t.TempDir(), "closed.sock"))
			if err != nil {
				t.Fatal(err)
			}
			path := l.Addr().String()
			_ = l.Close()
			SetNativeWorker(path)
			err = workerParseErr(t)
			if !errors.Is(err, ErrWorkerUnavailable) || errors.Is(err, ErrWorkerDroppedMidParse) {
				t.Fatalf("refused socket: err = %v, want ErrWorkerUnavailable only", err)
			}
		},
		"503 from worker": func(t *testing.T) {
			SetNativeWorker(serveWorkerSocket(t, http.HandlerFunc(func(w http.ResponseWriter, _ *http.Request) {
				w.WriteHeader(http.StatusServiceUnavailable)
			})))
			err := workerParseErr(t)
			if !errors.Is(err, ErrWorkerUnavailable) || errors.Is(err, ErrWorkerDroppedMidParse) {
				t.Fatalf("503: err = %v, want ErrWorkerUnavailable only", err)
			}
		},
		"worker-side timeout": func(t *testing.T) {
			SetNativeWorker(serveWorkerSocket(t, http.HandlerFunc(func(w http.ResponseWriter, _ *http.Request) {
				w.WriteHeader(http.StatusRequestTimeout)
				_, _ = w.Write([]byte(`{"code":"timeout","message":"context deadline exceeded"}`))
			})))
			err := workerParseErr(t)
			// A worker-side timeout counts toward the mid-parse drop cap.
			if !errors.Is(err, ErrWorkerDroppedMidParse) {
				t.Fatalf("timeout code: err = %v, want ErrWorkerDroppedMidParse", err)
			}
		},
	}
	for name, run := range cases {
		t.Run(name, func(t *testing.T) {
			SetNativeWorker("")
			defer SetNativeWorker("")
			run(t)
		})
	}
}

// TestWorkerDroppedMidParseSentinel pins the mid-parse classification:
// connection lost after the request was sent wraps
// ErrWorkerDroppedMidParse (which itself unwraps to
// ErrWorkerUnavailable).
func TestWorkerDroppedMidParseSentinel(t *testing.T) {
	for name, handler := range map[string]http.Handler{
		"truncated body":    hijackCloseHandler(t, true),
		"closed no headers": hijackCloseHandler(t, false),
	} {
		t.Run(name, func(t *testing.T) {
			SetNativeWorker(serveWorkerSocket(t, handler))
			defer SetNativeWorker("")
			err := workerParseErr(t)
			if !errors.Is(err, ErrWorkerDroppedMidParse) || !errors.Is(err, ErrWorkerUnavailable) {
				t.Fatalf("err = %v, want ErrWorkerDroppedMidParse wrapping ErrWorkerUnavailable", err)
			}
		})
	}
}

// TestWorkerClientKeepsDocumentSentinels pins that the classification did
// not swallow real document defects: an encrypted/malformed verdict from the
// worker still maps to the document sentinels, and a plain 500 keeps its
// old shape.
func TestWorkerClientKeepsDocumentSentinels(t *testing.T) {
	t.Run("malformed code", func(t *testing.T) {
		SetNativeWorker(serveWorkerSocket(t, http.HandlerFunc(func(w http.ResponseWriter, _ *http.Request) {
			w.WriteHeader(http.StatusUnprocessableEntity)
			_, _ = w.Write([]byte(`{"code":"malformed","message":"malformed document: bad xref"}`))
		})))
		defer SetNativeWorker("")
		err := workerParseErr(t)
		if !errors.Is(err, ErrMalformedDocument) {
			t.Fatalf("err = %v, want ErrMalformedDocument", err)
		}
		if errors.Is(err, ErrWorkerUnavailable) {
			t.Fatal("document defects must not classify as worker unavailability")
		}
	})
	t.Run("plain 500 unchanged", func(t *testing.T) {
		SetNativeWorker(serveWorkerSocket(t, http.HandlerFunc(func(w http.ResponseWriter, _ *http.Request) {
			w.WriteHeader(http.StatusInternalServerError)
		})))
		defer SetNativeWorker("")
		err := workerParseErr(t)
		if !strings.Contains(err.Error(), "extract worker returned 500 Internal Server Error") {
			t.Fatalf("err = %v, want unchanged plain status error", err)
		}
	})
	t.Run("context cancellation preserved", func(t *testing.T) {
		SetNativeWorker(missingSocketPath(t))
		defer SetNativeWorker("")
		ctx, cancel := context.WithCancel(context.Background())
		cancel()
		_, _, err := parseViaWorker(ctx, []byte("%PDF-1.4 stub"), "pdf")
		if !errors.Is(err, context.Canceled) {
			t.Fatalf("err = %v, want context error passthrough", err)
		}
	})
}

func missingSocketPath(t *testing.T) string {
	t.Helper()
	return filepath.Join(t.TempDir(), "gone.sock")
}

// TestWorkerAvailabilityPinsProbe pins the readiness field source: "" with
// no socket, "unavailable" against a dead socket, "ok" against a live one.
func TestWorkerAvailabilityPinsProbe(t *testing.T) {
	SetNativeWorker("")
	if got := WorkerAvailability(); got != "" {
		t.Fatalf("no socket: availability = %q, want empty", got)
	}
	SetNativeWorker(filepath.Join(t.TempDir(), "dead.sock"))
	if got := WorkerAvailability(); got != "unavailable" {
		t.Fatalf("dead socket: availability = %q, want unavailable", got)
	}
	SetNativeWorker(serveTestWorker(t, 1))
	if got := WorkerAvailability(); got != "ok" {
		t.Fatalf("live socket: availability = %q, want ok", got)
	}
	SetNativeWorker("")
}
