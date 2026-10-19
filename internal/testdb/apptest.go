// Real-process application test helper for parent-owned tests.
//
// Convention: every app/process test supplies an isolated testdb schema
// (NewWithDSN's DSN; use 01-08 seed helpers for identities), a per-test
// ENCLAVA_DATA_DIR, and calls StartApp once per process — two StartApp calls
// yield two real app processes on distinct APP_PORT-selected ports sharing
// the caller's test schema. The child NEVER inherits HOME or the user's
// .env: the environment is exactly DATABASE_URL, ENCLAVA_DATA_DIR, APP_PORT
// and whatever opts.Env passes. Helpers t.Fatal on missing prerequisites —
// never skip.
package testdb

import (
	"context"
	"fmt"
	"net"
	"net/http"
	"net/http/cookiejar"
	"net/url"
	"os"
	"os/exec"
	"path/filepath"
	"strings"
	"syscall"
	"testing"
	"time"
)

// StartOpts configures one real enclava process launch.
type StartOpts struct {
	// DatabaseURL is the caller-supplied test-schema DSN (required).
	DatabaseURL string
	// Bootstrap runs `enclava bootstrap` once on the same DSN/data dir
	// before serving; BootstrapArgs adds flags (e.g. --tenant).
	Bootstrap     bool
	BootstrapArgs []string
	// Env adds whitelisted-by-caller env vars on top of the fixed trio
	// (e.g. SESSION_TENANT_SLUG, INFERENCE_PROVIDER).
	Env map[string]string
}

// AppHandle is a running real enclava process.
type AppHandle struct {
	BaseURL string
	Client  *http.Client

	cmd  *exec.Cmd
	port int
}

// StartApp builds ./cmd/enclava into t.TempDir, optionally runs bootstrap,
// then launches the serve process on a free port and waits for /livez.
func StartApp(t testing.TB, opts StartOpts) *AppHandle {
	t.Helper()
	if strings.TrimSpace(opts.DatabaseURL) == "" {
		t.Fatalf("StartApp requires opts.DatabaseURL (a testdb schema DSN)")
	}

	bin := filepath.Join(t.TempDir(), "enclava")
	build := exec.Command("go", "build", "-o", bin, "./cmd/enclava")
	if out, err := build.CombinedOutput(); err != nil {
		t.Fatalf("build enclava binary: %v\n%s", err, out)
	}

	dataDir := t.TempDir()
	env := []string{
		"PATH=" + os.Getenv("PATH"), // go toolchains and dynamic loaders still need it
		"DATABASE_URL=" + opts.DatabaseURL,
		"ENCLAVA_DATA_DIR=" + dataDir,
	}
	for k, v := range opts.Env {
		env = append(env, k+"="+v)
	}

	if opts.Bootstrap {
		cmd := exec.Command(bin, append([]string{"bootstrap"}, opts.BootstrapArgs...)...)
		cmd.Env = env
		if out, err := cmd.CombinedOutput(); err != nil {
			t.Fatalf("bootstrap failed: %v\n%s", err, out)
		}
	}

	port := freePort(t)

	cmd := exec.Command(bin)
	cmd.Env = append(env, fmt.Sprintf("APP_PORT=%d", port))
	if err := cmd.Start(); err != nil {
		t.Fatalf("start app process: %v", err)
	}

	h := &AppHandle{
		BaseURL: fmt.Sprintf("http://127.0.0.1:%d", port),
		cmd:     cmd,
		port:    port,
	}
	jar, err := cookiejar.New(nil)
	if err != nil {
		t.Fatalf("cookie jar: %v", err)
	}
	h.Client = &http.Client{
		Jar: jar,
		// No redirect-follow: tests assert dashboard redirects themselves.
		CheckRedirect: func(*http.Request, []*http.Request) error {
			return http.ErrUseLastResponse
		},
	}

	t.Cleanup(h.Stop)
	ctx, cancel := context.WithTimeout(context.Background(), 20*time.Second)
	defer cancel()
	h.WaitHealthy(ctx, t)
	return h
}

// WaitHealthy polls /livez until the process serves 200.
func (h *AppHandle) WaitHealthy(ctx context.Context, t testing.TB) {
	t.Helper()
	for {
		req, _ := http.NewRequestWithContext(ctx, http.MethodGet, h.BaseURL+"/livez", nil)
		resp, err := h.Client.Do(req)
		if err == nil {
			_ = resp.Body.Close()
			if resp.StatusCode == http.StatusOK {
				return
			}
		}
		if ctx.Err() != nil {
			t.Fatalf("app on %s never became healthy: %v", h.BaseURL, ctx.Err())
		}
		time.Sleep(50 * time.Millisecond)
	}
}

// CSRFToken returns the jar's session CSRF cookie for mutations.
func (h *AppHandle) CSRFToken() string {
	for _, c := range h.Client.Jar.Cookies(mustURL(h.BaseURL)) {
		if c.Name == "enclava_csrf" {
			return c.Value
		}
	}
	return ""
}

// Stop SIGTERMs the process, then SIGKILLs after a grace period.
func (h *AppHandle) Stop() {
	if h.cmd == nil || h.cmd.Process == nil {
		return
	}
	_ = h.cmd.Process.Signal(syscall.SIGTERM)
	done := make(chan error, 1)
	go func() { done <- h.cmd.Wait() }()
	select {
	case <-done:
	case <-time.After(5 * time.Second):
		_ = h.cmd.Process.Kill()
		<-done
	}
}

func freePort(t testing.TB) int {
	l, err := net.Listen("tcp", "127.0.0.1:0")
	if err != nil {
		t.Fatalf("reserve free port: %v", err)
	}
	defer func() { _ = l.Close() }()
	return l.Addr().(*net.TCPAddr).Port
}

func mustURL(raw string) *url.URL {
	u, err := url.Parse(raw)
	if err != nil {
		panic(err)
	}
	return u
}
