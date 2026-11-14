package extract

import (
	"context"
	"encoding/json"
	"errors"
	"fmt"
	"io"
	"net"
	"net/http"
	"os"
	"os/exec"
	"path/filepath"
	"strings"
	"testing"
	"time"

	"enclava-go/internal/source"
)

// serveTestWorker binds the handler on a real unix socket under t.TempDir()
// and returns the path.
func serveTestWorker(t *testing.T, concurrency int) string {
	t.Helper()
	path := filepath.Join(t.TempDir(), "worker.sock")
	listener, err := net.Listen("unix", path)
	if err != nil {
		t.Fatal(err)
	}
	server := &http.Server{Handler: newWorkerHandler(concurrency), ReadHeaderTimeout: 10 * time.Second}
	go func() { _ = server.Serve(listener) }()
	t.Cleanup(func() { _ = server.Close(); _ = listener.Close() })
	return path
}

func workerRoundTrip(t *testing.T, socket, method, path string, body []byte) (int, []byte) {
	t.Helper()
	req, err := http.NewRequest(method, "http://extract-worker"+path, strings.NewReader(string(body)))
	if err != nil {
		t.Fatal(err)
	}
	resp, err := workerHTTPClientFor(socket).Do(req)
	if err != nil {
		t.Fatal(err)
	}
	defer func() { _ = resp.Body.Close() }()
	raw, err := io.ReadAll(resp.Body)
	if err != nil {
		t.Fatal(err)
	}
	return resp.StatusCode, raw
}

func workerHTTPClientFor(socket string) *http.Client {
	return &http.Client{Transport: &http.Transport{
		DialContext: func(ctx context.Context, _, _ string) (net.Conn, error) {
			var d net.Dialer
			return d.DialContext(ctx, "unix", socket)
		},
	}}
}

func TestWorkerRoundTripBlocksWarningsAndErrors(t *testing.T) {
	socket := serveTestWorker(t, 2)

	// capabilities endpoint
	status, raw := workerRoundTrip(t, socket, http.MethodGet, "/v1/capabilities", nil)
	if status != http.StatusOK {
		t.Fatalf("capabilities status=%d body=%s", status, raw)
	}
	var caps NativeCapabilities
	if err := json.Unmarshal(raw, &caps); err != nil {
		t.Fatalf("capabilities decode: %v", err)
	}

	// stubbed parse: set workerParseFunc directly for deterministic blocks
	page := 3
	origParse := workerParseFunc
	workerParseFunc = func(ctx context.Context, data []byte, format string) ([]parseBlock, []string, error) {
		if format != "pdf" {
			return nil, nil, ErrUnsupportedFormat
		}
		return []parseBlock{{kind: "paragraph", text: "hello world", level: 1, start: 0, end: 11, locator: source.Locator{Page: &page, Box: "1,2,3,4"}}},
			[]string{"page 1 contains embedded text; scanned regions not OCR'd"}, nil
	}
	defer func() { workerParseFunc = origParse }()

	status, raw = workerRoundTrip(t, socket, http.MethodPost, "/v1/parse?format=pdf", []byte("%PDF-1.4"))
	if status != http.StatusOK {
		t.Fatalf("parse status=%d body=%s", status, raw)
	}
	var resp workerParseResponse
	if err := json.Unmarshal(raw, &resp); err != nil {
		t.Fatalf("parse decode: %v", err)
	}
	if len(resp.Blocks) != 1 || resp.Blocks[0].Text != "hello world" || resp.Blocks[0].Locator.Page == nil || *resp.Blocks[0].Locator.Page != 3 {
		t.Fatalf("blocks lost in round trip: %+v", resp.Blocks)
	}
	if len(resp.Warnings) != 1 || resp.Warnings[0] != "page 1 contains embedded text; scanned regions not OCR'd" {
		t.Fatalf("warnings lost: %v", resp.Warnings)
	}

	// error mapping: sentinel + detailed message preserved through client
	detailed := fmt.Errorf("%w: PDF has 5001 pages, the limit is 5000", ErrDocumentTooLarge)
	workerParseFunc = func(ctx context.Context, data []byte, format string) ([]parseBlock, []string, error) {
		return nil, nil, detailed
	}
	status, raw = workerRoundTrip(t, socket, http.MethodPost, "/v1/parse?format=pdf", []byte("%PDF-1.4"))
	if status != http.StatusRequestEntityTooLarge {
		t.Fatalf("too_large status=%d body=%s", status, raw)
	}
	var eb workerErrorBody
	if err := json.Unmarshal(raw, &eb); err != nil || eb.Code != "too_large" {
		t.Fatalf("error body=%s err=%v", raw, err)
	}
	clientErr := workerSentinelError(eb.Code, eb.Message)
	if !errors.Is(clientErr, ErrDocumentTooLarge) || !strings.Contains(clientErr.Error(), "5001 pages") {
		t.Fatalf("client error lost sentinel/detail: %v", clientErr)
	}

	workerParseFunc = func(ctx context.Context, data []byte, format string) ([]parseBlock, []string, error) {
		return nil, nil, ErrEncryptedDocument
	}
	status, raw = workerRoundTrip(t, socket, http.MethodPost, "/v1/parse?format=pdf", []byte("%PDF-1.4"))
	if status != http.StatusUnprocessableEntity {
		t.Fatalf("encrypted status=%d body=%s", status, raw)
	}
	if err := json.Unmarshal(raw, &eb); err != nil || eb.Code != "encrypted" {
		t.Fatalf("encrypted body=%s err=%v", raw, err)
	}
	if !errors.Is(workerSentinelError(eb.Code, eb.Message), ErrEncryptedDocument) {
		t.Fatal("encrypted sentinel lost")
	}

	workerParseFunc = func(ctx context.Context, data []byte, format string) ([]parseBlock, []string, error) {
		return nil, nil, ErrUnsupportedFormat
	}
	status, raw = workerRoundTrip(t, socket, http.MethodPost, "/v1/parse?format=pdf", []byte("x"))
	if status != http.StatusUnsupportedMediaType {
		t.Fatalf("unsupported status=%d body=%s", status, raw)
	}
	if err := json.Unmarshal(raw, &eb); err != nil || eb.Code != "unsupported_format" {
		t.Fatalf("unsupported body=%s err=%v", raw, err)
	}
}

func TestWorkerOversizeBody(t *testing.T) {
	socket := serveTestWorker(t, 1)
	big := make([]byte, maxNativeInput+1)
	status, raw := workerRoundTrip(t, socket, http.MethodPost, "/v1/parse?format=pdf", big)
	if status != http.StatusRequestEntityTooLarge {
		t.Fatalf("oversize status=%d body=%s", status, raw[:min(len(raw), 200)])
	}
}

func TestWorkerCancelledContextReturnsPromptly(t *testing.T) {
	SetNativeWorker(serveTestWorker(t, 1))
	defer SetNativeWorker("")
	entered := make(chan struct{})
	origParse := workerParseFunc
	workerParseFunc = func(ctx context.Context, data []byte, format string) ([]parseBlock, []string, error) {
		close(entered)
		<-ctx.Done()
		return nil, nil, ctx.Err()
	}
	defer func() { workerParseFunc = origParse }()

	ctx, cancel := context.WithCancel(context.Background())
	done := make(chan error, 1)
	go func() {
		_, _, err := parseViaWorker(ctx, []byte("x"), "pdf")
		done <- err
	}()
	<-entered
	cancel()
	select {
	case err := <-done:
		if !errors.Is(err, context.Canceled) {
			t.Fatalf("cancelled parse err=%v", err)
		}
	case <-time.After(5 * time.Second):
		t.Fatal("cancelled parse did not return promptly")
	}
}

func TestWorkerProbeRetryAfterFailure(t *testing.T) {
	// no socket listening yet: first probe fails and caches nothing
	missing := filepath.Join(t.TempDir(), "missing.sock")
	SetNativeWorker(missing)
	defer SetNativeWorker("")
	caps := ProbeNative()
	if !strings.Contains(caps.Reason, "unreachable") {
		t.Fatalf("expected unreachable reason, got %+v", caps)
	}
	// worker comes up
	socket := serveTestWorker(t, 1)
	SetNativeWorker(socket)
	// point the dialer at the real socket: SetNativeWorker already did, but
	// probeNativeWorker uses nativeWorkerPath() which is now `socket`
	caps = ProbeNative()
	if caps.Reason != "" && strings.Contains(caps.Reason, "unreachable") {
		t.Fatalf("probe did not recover: %+v", caps)
	}
}

func TestRunNativeContainerMode(t *testing.T) {
	prlimit, err := exec.LookPath("prlimit")
	if err != nil {
		t.Skip("prlimit missing")
	}
	enableContainerSandbox()
	defer func() { nativeSandbox.Store(nativeSandboxBwrap) }()
	dir := t.TempDir()
	out, err := runNative(context.Background(), "", prlimit, "/bin/echo", []string{"hello", "/work/ignored"}, dir, 1<<20)
	if err != nil {
		t.Fatalf("container runNative: %v", err)
	}
	// The /work arg is rewritten to the scratch dir before reaching the tool.
	want := "hello " + filepath.Join(dir, "ignored")
	if strings.TrimSpace(string(out)) != want {
		t.Fatalf("unexpected output %q want %q", out, want)
	}
}

func TestWorkerClientParseRoundTrip(t *testing.T) {
	socket := serveTestWorker(t, 2)
	SetNativeWorker(socket)
	defer SetNativeWorker("")
	origParse := workerParseFunc
	page := 1
	workerParseFunc = func(ctx context.Context, data []byte, format string) ([]parseBlock, []string, error) {
		return []parseBlock{{kind: "paragraph", text: "round trip", start: 0, end: 10, locator: source.Locator{Page: &page, Box: "0,0,1,1"}}}, []string{"w"}, nil
	}
	defer func() { workerParseFunc = origParse }()

	doc, err := parseNative(context.Background(), artifactsRef(), ParseOptions{ObjectRef: source.ObjectRef{TenantID: "t", ObjectID: "o", ObservedSeq: 1}}, []byte("pdf-bytes"), "pdf")
	if err != nil {
		t.Fatalf("parseNative via worker: %v", err)
	}
	if len(doc.Blocks) != 1 || doc.Blocks[0].Text != "round trip" || len(doc.ParseWarnings) != 1 {
		t.Fatalf("document mismatch: %+v", doc)
	}
}

// The fixture round trip runs through the real worker handler and client in
// container mode, and the block text must match the local bwrap path.
func TestWorkerContainerModeRealPDFFixture(t *testing.T) {
	if os.Getenv("ENCLAVA_NATIVE_INTEGRATION") != "1" {
		t.Skip("native integration requires ENCLAVA_NATIVE_INTEGRATION=1")
	}
	for _, bin := range []string{"bwrap", "prlimit", "pdftotext", "pdftoppm", "pdfinfo", "tesseract"} {
		if _, err := exec.LookPath(bin); err != nil {
			t.Skipf("%s missing", bin)
		}
	}
	pdf, err := os.ReadFile("testdata/legal-document.pdf")
	if err != nil {
		t.Fatal(err)
	}
	// Baseline: local bwrap parse.
	localBlocks, localWarnings, err := parseNativeBlocks(context.Background(), pdf, "pdf")
	if err != nil {
		t.Fatalf("local parse: %v", err)
	}
	// Worker+client in container mode.
	enableContainerSandbox()
	socket := serveTestWorker(t, 2)
	SetNativeWorker(socket)
	defer func() {
		SetNativeWorker("")
		nativeSandbox.Store(nativeSandboxBwrap)
	}()
	doc, err := parseNative(context.Background(), artifactsRef(), ParseOptions{ObjectRef: source.ObjectRef{TenantID: "tenant", ObjectID: "pdf", ObservedSeq: 1}}, pdf, "pdf")
	if err != nil {
		t.Fatalf("worker parse: %v", err)
	}
	var remoteText, localText strings.Builder
	for _, b := range doc.Blocks {
		remoteText.WriteString(b.Text)
		remoteText.WriteByte(' ')
	}
	for _, b := range localBlocks {
		localText.WriteString(b.text)
		localText.WriteByte(' ')
	}
	if remoteText.String() != localText.String() {
		t.Fatalf("worker text diverged from local:\nremote: %.300s\nlocal:  %.300s", remoteText.String(), localText.String())
	}
	if len(doc.ParseWarnings) != len(localWarnings) {
		t.Fatalf("warnings diverged: %v vs %v", doc.ParseWarnings, localWarnings)
	}
}
