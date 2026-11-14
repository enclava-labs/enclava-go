package extract

import (
	"context"
	"encoding/json"
	"errors"
	"io"
	"log"
	"net"
	"net/http"
	"os"
	"time"
	"unicode/utf8"

	"enclava-go/internal/source"
)

// workerParseFunc is the local parse the handler invokes; tests substitute a
// stub so the handler never needs Poppler/Tesseract.
var workerParseFunc = func(ctx context.Context, data []byte, format string) ([]parseBlock, []string, error) {
	return parseNativeBlocks(ctx, data, format)
}

// workerParseResponse is the JSON body POST /v1/parse returns: blocks carry
// the unexported parseBlock shape plus warnings.
type workerParseResponse struct {
	Blocks   []workerBlock `json:"blocks"`
	Warnings []string      `json:"warnings"`
}

type workerBlock struct {
	Kind     string        `json:"kind"`
	Text     string        `json:"text"`
	Language string        `json:"language"`
	Level    int           `json:"level"`
	Start    int           `json:"start"`
	End      int           `json:"end"`
	Locator  workerLocator `json:"locator"`
}

type workerLocator struct {
	Page         *int   `json:"page,omitempty"`
	Box          string `json:"box,omitempty"`
	Table        string `json:"table,omitempty"`
	Clause       string `json:"clause,omitempty"`
	Section      string `json:"section,omitempty"`
	HeadingLevel int    `json:"heading_level,omitempty"`
	Language     string `json:"language,omitempty"`
}

func (b parseBlock) wire() workerBlock {
	return workerBlock{Kind: b.kind, Text: b.text, Language: b.language, Level: b.level, Start: b.start, End: b.end,
		Locator: workerLocator{Page: b.locator.Page, Box: b.locator.Box, Table: b.locator.Table, Clause: b.locator.Clause, Section: b.locator.Section, HeadingLevel: b.locator.HeadingLevel, Language: b.locator.Language}}
}

func (b workerBlock) block() (parseBlock, bool) {
	if !utf8.ValidString(b.Text) || b.Start < 0 || b.End < b.Start {
		return parseBlock{}, false
	}
	return parseBlock{kind: b.Kind, text: b.Text, language: b.Language, level: b.Level, start: b.Start, end: b.End,
		locator: sourceLocator(b.Locator)}, true
}

func sourceLocator(l workerLocator) source.Locator {
	return source.Locator{Page: l.Page, Box: l.Box, Table: l.Table, Clause: l.Clause, Section: l.Section, HeadingLevel: l.HeadingLevel, Language: l.Language}
}

// workerErrorBody lets the client rebuild the sentinel via errors.Is while
// keeping the detailed message.
type workerErrorBody struct {
	Code    string `json:"code"`
	Message string `json:"message"`
}

func workerErrorCode(err error) (int, string) {
	switch {
	case errors.Is(err, ErrDocumentTooLarge):
		return http.StatusRequestEntityTooLarge, "too_large"
	case errors.Is(err, ErrEncryptedDocument):
		return http.StatusUnprocessableEntity, "encrypted"
	case errors.Is(err, ErrMalformedDocument):
		return http.StatusUnprocessableEntity, "malformed"
	case errors.Is(err, ErrUnsupportedFormat):
		return http.StatusUnsupportedMediaType, "unsupported_format"
	case errors.Is(err, context.Canceled), errors.Is(err, context.DeadlineExceeded):
		return http.StatusRequestTimeout, "timeout"
	default:
		return http.StatusInternalServerError, "internal"
	}
}

// workerHandler serves capabilities and parse requests on the unix socket.
type workerHandler struct {
	sem chan struct{}
}

func newWorkerHandler(concurrency int) *workerHandler {
	if concurrency < 1 {
		concurrency = 2
	}
	return &workerHandler{sem: make(chan struct{}, concurrency)}
}

func (h *workerHandler) ServeHTTP(w http.ResponseWriter, r *http.Request) {
	switch {
	case r.Method == http.MethodGet && r.URL.Path == "/v1/capabilities":
		// Always report this process's own tools: the worker never proxies
		// to another worker, even if a socket is configured in-process.
		_ = json.NewEncoder(w).Encode(probeNativeLocally())
	case r.Method == http.MethodPost && r.URL.Path == "/v1/parse":
		h.serveParse(w, r)
	default:
		http.NotFound(w, r)
	}
}

func (h *workerHandler) serveParse(w http.ResponseWriter, r *http.Request) {
	format := r.URL.Query().Get("format")
	if format != "pdf" && format != "image" {
		h.writeError(w, http.StatusBadRequest, "bad_request", "format must be pdf or image")
		return
	}
	select {
	case h.sem <- struct{}{}:
		defer func() { <-h.sem }()
	case <-r.Context().Done():
		return
	}
	data, err := io.ReadAll(http.MaxBytesReader(w, r.Body, maxNativeInput+1))
	if err != nil {
		var maxErr *http.MaxBytesError
		if errors.As(err, &maxErr) {
			h.writeError(w, http.StatusRequestEntityTooLarge, "too_large", ErrDocumentTooLarge.Error())
			return
		}
		h.writeError(w, http.StatusBadRequest, "bad_request", err.Error())
		return
	}
	if len(data) > maxNativeInput {
		h.writeError(w, http.StatusRequestEntityTooLarge, "too_large", ErrDocumentTooLarge.Error())
		return
	}
	blocks, warnings, err := workerParseFunc(r.Context(), data, format)
	if err != nil {
		if errors.Is(err, context.Canceled) || errors.Is(err, context.DeadlineExceeded) {
			return // client gone; nothing to report
		}
		status, code := workerErrorCode(err)
		h.writeError(w, status, code, err.Error())
		return
	}
	resp := workerParseResponse{Warnings: warnings}
	for _, b := range blocks {
		resp.Blocks = append(resp.Blocks, b.wire())
	}
	w.Header().Set("Content-Type", "application/json")
	_ = json.NewEncoder(w).Encode(resp)
}

func (h *workerHandler) writeError(w http.ResponseWriter, status int, code, message string) {
	w.Header().Set("Content-Type", "application/json")
	w.WriteHeader(status)
	_ = json.NewEncoder(w).Encode(workerErrorBody{Code: code, Message: message})
}

// ServeExtractWorker runs the unix-socket HTTP server until ctx ends. Called
// only by the extract-worker subcommand after enabling container mode.
func ServeExtractWorker(ctx context.Context, socketPath string, concurrency int) error {
	enableContainerSandbox()
	if err := os.Remove(socketPath); err != nil && !errors.Is(err, os.ErrNotExist) {
		return err
	}
	listener, err := net.Listen("unix", socketPath)
	if err != nil {
		return err
	}
	defer func() { _ = listener.Close() }()
	if err := os.Chmod(socketPath, 0o660); err != nil { //nolint:gosec // G302: the app container shares this socket as group member; 0660 is required
		return err
	}
	server := &http.Server{Handler: newWorkerHandler(concurrency), ReadHeaderTimeout: 10 * time.Second}
	go func() { //nolint:gosec // G118: server-lifetime shutdown, not request scoped
		<-ctx.Done()
		shutdownCtx, cancel := context.WithTimeout(context.Background(), 5*time.Second)
		defer cancel()
		_ = server.Shutdown(shutdownCtx)
	}()
	caps := ProbeNative()
	log.Printf("extract-worker listening on %s: PDF=%t OCR=%t languages=%v reason=%s", socketPath, caps.PDF, caps.OCR, caps.Languages, caps.Reason)
	err = server.Serve(listener)
	if errors.Is(err, http.ErrServerClosed) {
		return nil
	}
	return err
}
