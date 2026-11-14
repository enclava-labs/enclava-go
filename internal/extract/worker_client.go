package extract

import (
	"bytes"
	"context"
	"encoding/json"
	"errors"
	"fmt"
	"io"
	"net"
	"net/http"
	"net/url"
	"strings"
	"sync"
	"sync/atomic"
	"time"
	"unicode/utf8"

	"enclava-go/internal/artifacts"
	"enclava-go/internal/source"
)

const (
	maxWorkerResponse = 256 << 20
	// workerRetryInterval bounds re-probing after a failed/unreachable worker.
	workerRetryInterval = 5 * time.Second
)

var nativeWorkerSocket atomic.Value // string

// SetNativeWorker configures the extract worker socket path; empty restores
// local native processing. Call before any capability probe or parse.
func SetNativeWorker(socketPath string) {
	nativeWorkerSocket.Store(socketPath)
	workerCaps.Lock()
	workerCaps.cached = nil
	workerCaps.lastProbe = time.Time{}
	workerCaps.Unlock()
}

func nativeWorkerPath() string {
	if v, ok := nativeWorkerSocket.Load().(string); ok {
		return v
	}
	return ""
}

var workerCaps struct {
	sync.Mutex
	cached    *NativeCapabilities
	lastProbe time.Time
}

func probeNativeWorker() NativeCapabilities {
	workerCaps.Lock()
	defer workerCaps.Unlock()
	if workerCaps.cached != nil {
		caps := *workerCaps.cached
		caps.Languages = append([]string(nil), caps.Languages...)
		return caps
	}
	if time.Since(workerCaps.lastProbe) < workerRetryInterval {
		return NativeCapabilities{Reason: "extract worker unreachable (re-probe pending)"}
	}
	workerCaps.lastProbe = time.Now()
	probeCtx, cancel := context.WithTimeout(context.Background(), 10*time.Second)
	caps, err := fetchWorkerCapabilities(probeCtx)
	cancel()
	if err != nil {
		return NativeCapabilities{Reason: "extract worker unreachable: " + err.Error()}
	}
	workerCaps.cached = &caps
	caps.Languages = append([]string(nil), caps.Languages...)
	return caps
}

// invalidateWorkerCaps clears a cached probe when a parse fails as
// unreachable: the worker may have restarted, so the next probe re-checks
// immediately.
func invalidateWorkerCaps() {
	workerCaps.Lock()
	workerCaps.cached = nil
	workerCaps.lastProbe = time.Time{}
	workerCaps.Unlock()
}

func workerHTTPClient() *http.Client {
	return &http.Client{
		Transport: &http.Transport{
			DialContext: func(ctx context.Context, _, _ string) (net.Conn, error) {
				var d net.Dialer
				return d.DialContext(ctx, "unix", nativeWorkerPath())
			},
		},
	}
}

func fetchWorkerCapabilities(ctx context.Context) (NativeCapabilities, error) {
	req, err := http.NewRequestWithContext(ctx, http.MethodGet, "http://extract-worker/v1/capabilities", nil)
	if err != nil {
		return NativeCapabilities{}, err
	}
	resp, err := workerHTTPClient().Do(req)
	if err != nil {
		return NativeCapabilities{}, err
	}
	defer func() { _ = resp.Body.Close() }()
	if resp.StatusCode != http.StatusOK {
		return NativeCapabilities{}, fmt.Errorf("worker returned %s", resp.Status)
	}
	var caps NativeCapabilities
	if err := json.NewDecoder(io.LimitReader(resp.Body, 1<<20)).Decode(&caps); err != nil {
		return NativeCapabilities{}, err
	}
	return caps, nil
}

func parseNativeViaWorker(ctx context.Context, ref artifacts.Ref, opts ParseOptions, data []byte, format string) (source.Document, error) {
	if err := ctx.Err(); err != nil {
		return source.Document{}, err
	}
	if len(data) > maxNativeInput {
		return source.Document{}, fmt.Errorf("%w: file is %d MB, the limit is %d MB", ErrDocumentTooLarge, len(data)>>20, maxNativeInput>>20)
	}
	if format == "image" {
		if len(data) < 8 || string(data[:8]) != "\x89PNG\r\n\x1a\n" {
			return source.Document{}, fmt.Errorf("%w: %s is unsupported; only PNG image OCR is supported", ErrUnsupportedFormat, http.DetectContentType(data))
		}
	}
	blocks, warnings, err := parseViaWorker(ctx, data, format)
	if err != nil {
		return source.Document{}, err
	}
	document := makeDocument(opts.ObjectRef, ref, blocks)
	document.ParseWarnings = warnings
	return document, nil
}

func parseViaWorker(ctx context.Context, data []byte, format string) ([]parseBlock, []string, error) {
	req, err := http.NewRequestWithContext(ctx, http.MethodPost, "http://extract-worker/v1/parse?format="+url.QueryEscape(format), bytes.NewReader(data)) //nolint:gosec // G704: fixed host; the transport dials only the configured unix socket
	if err != nil {
		return nil, nil, err
	}
	req.Header.Set("Content-Type", "application/octet-stream")
	resp, err := workerHTTPClient().Do(req) //nolint:gosec // G704: fixed host; the transport dials only the configured unix socket
	if err != nil {
		invalidateWorkerCaps()
		if ctx.Err() != nil {
			return nil, nil, ctx.Err()
		}
		return nil, nil, fmt.Errorf("extract worker unreachable: %w", err)
	}
	defer func() { _ = resp.Body.Close() }()
	if resp.StatusCode != http.StatusOK {
		body, _ := io.ReadAll(io.LimitReader(resp.Body, 1<<20))
		var eb workerErrorBody
		if json.Unmarshal(body, &eb) == nil && eb.Code != "" {
			return nil, nil, workerSentinelError(eb.Code, eb.Message)
		}
		return nil, nil, fmt.Errorf("extract worker returned %s", resp.Status)
	}
	var out workerParseResponse
	dec := json.NewDecoder(io.LimitReader(resp.Body, maxWorkerResponse+1))
	if err := dec.Decode(&out); err != nil {
		return nil, nil, fmt.Errorf("extract worker response: %w", err)
	}
	blocks := make([]parseBlock, 0, len(out.Blocks))
	for _, wb := range out.Blocks {
		b, ok := wb.block()
		if !ok {
			return nil, nil, errors.New("extract worker returned invalid block")
		}
		blocks = append(blocks, b)
	}
	for _, w := range out.Warnings {
		if !utf8.ValidString(w) {
			return nil, nil, errors.New("extract worker returned invalid warning")
		}
	}
	return blocks, out.Warnings, nil
}

func workerSentinelError(code, message string) error {
	var sentinel error
	switch code {
	case "too_large":
		sentinel = ErrDocumentTooLarge
	case "encrypted":
		sentinel = ErrEncryptedDocument
	case "malformed":
		sentinel = ErrMalformedDocument
	case "unsupported_format":
		sentinel = ErrUnsupportedFormat
	default:
		return errors.New(message)
	}
	if message == "" || message == sentinel.Error() {
		return sentinel
	}
	// Keep the detailed worker message while preserving errors.Is on the sentinel.
	return fmt.Errorf("%w: %s", sentinel, strings.TrimPrefix(message, sentinel.Error()+": "))
}
