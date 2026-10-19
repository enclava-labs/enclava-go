package httpx

import (
	"bytes"
	"context"
	"encoding/json"
	"errors"
	"io"
	"log"
	"mime"
	"mime/multipart"
	"net/http"
	"net/url"
	"strings"
	"time"

	"enclava-go/internal/auth"
	"enclava-go/internal/llm"
	"enclava-go/internal/store/sqlite"
)

const openAIProxyScope = "openai.proxy"

func (a *App) handleOpenAIPassthrough(w http.ResponseWriter, r *http.Request, principal auth.Principal) {
	a.handleOpenAIPassthroughWithPrincipal(w, r, principal)
}

func (a *App) maybeHandleOpenAIPassthrough(w http.ResponseWriter, r *http.Request, principal auth.Principal) bool {
	if !auth.HasScope(principal, openAIProxyScope) {
		return false
	}
	a.handleOpenAIPassthroughWithPrincipal(w, r, principal)
	return true
}

func (a *App) handleOpenAIPassthroughWithPrincipal(w http.ResponseWriter, r *http.Request, principal auth.Principal) {
	if !auth.HasScope(principal, openAIProxyScope) {
		WriteForbidden(w, "missing required scope: "+openAIProxyScope)
		return
	}
	if a.prober != nil && !a.prober.Ready() {
		WriteOpenAIError(w, http.StatusServiceUnavailable, "provider not ready", "server_error", "provider_not_ready")
		return
	}
	proxy, ok := a.provider.(llm.PassthroughProvider)
	if !ok {
		WriteOpenAIError(w, http.StatusNotImplemented, "configured provider does not support OpenAI passthrough", "server_error", "openai_passthrough_unsupported")
		return
	}
	// Dispatch authorization is scopes + the endpoint-ID allowlist enforced
	// at authentication; only transport path-safety remains here — endpoint
	// grants never compare URL paths.
	endpointPath, ok := openAIV1EndpointPath(r.URL.Path)
	if !ok || !openAIProxyPathSafe(endpointPath) {
		WriteOpenAIError(w, http.StatusNotFound, "OpenAI endpoint is not supported by this gateway", "invalid_request_error", "not_found")
		return
	}
	if !openAIProxyMethodAllowed(r.Method) {
		WriteMethodNotAllowed(w)
		return
	}
	if openAIProxyMayUseBudget(r.Method) {
		if err := a.EnforceBudget(r.Context(), principal); err != nil {
			a.WriteBudgetError(w, err)
			return
		}
	}

	requestBytes, err := readProxyRequestBody(r, a.maxRequestBodyBytes())
	if err != nil {
		WriteOpenAIError(w, http.StatusBadRequest, "invalid request body", "invalid_request_error", "invalid_body")
		return
	}
	model := modelFromOpenAIProxyRequest(requestBytes, r.Header.Get("Content-Type"))
	if model != "" && !auth.ModelAllowed(principal, model) {
		WriteForbidden(w, "model is not allowed for this API key")
		return
	}

	upstreamPath := endpointPath
	if r.URL.RawQuery != "" {
		upstreamPath += "?" + r.URL.RawQuery
	}
	resp, err := proxy.DoPassthroughRequest(r.Context(), r.Method, upstreamPath, requestBytes, r.Header)
	if err != nil {
		a.recordParsedUsage(r.Context(), principal, "/api/v1"+endpointPath, model, usageParsed{Model: model}, statusForProviderError(err))
		a.WriteProviderError(w, err)
		return
	}
	defer func() { _ = resp.Body.Close() }()

	copyPassthroughResponseHeaders(w.Header(), resp.Header)
	statusCode := resp.StatusCode
	if statusCode == 0 {
		statusCode = http.StatusOK
	}
	w.WriteHeader(statusCode)

	capture := &tailBuffer{limit: maxSSEEventBytes}
	writer := io.MultiWriter(w, capture)
	if responseIsEventStream(resp.Header) {
		if flusher, ok := w.(http.Flusher); ok {
			writer = io.MultiWriter(flushWriter{w: w, flusher: flusher}, capture)
		}
	}
	if _, err := io.Copy(writer, resp.Body); err != nil {
		log.Printf("openai passthrough response copy failed: %s", redactForLog(err.Error()))
		statusCode = http.StatusBadGateway
	}

	usage := parseUsageFromProxyPayload(resp.Header.Get("Content-Type"), capture.Bytes())
	a.recordOpenAIProxyUsage(context.WithoutCancel(r.Context()), principal, "/api/v1"+endpointPath, model, usage, statusCode)
}

func readProxyRequestBody(r *http.Request, limit int64) ([]byte, error) {
	if r.Body == nil {
		return nil, nil
	}
	defer func() { _ = r.Body.Close() }()
	if limit <= 0 {
		limit = 16 * 1024 * 1024
	}
	body, err := io.ReadAll(io.LimitReader(r.Body, limit+1))
	if err != nil {
		return nil, err
	}
	if int64(len(body)) > limit {
		return nil, errors.New("request body too large")
	}
	return body, nil
}

func openAIProxyPathSafe(endpoint string) bool {
	if endpoint == "" || endpoint[0] != '/' {
		return false
	}
	if strings.Contains(endpoint, `\`) || strings.Contains(endpoint, "://") || strings.Contains(endpoint, "..") {
		return false
	}
	if strings.Contains(strings.ToLower(endpoint), "%2e%2e") {
		return false
	}
	decoded, err := url.PathUnescape(endpoint)
	if err != nil {
		return false
	}
	if decoded != endpoint {
		if decoded == "" || decoded[0] != '/' || strings.Contains(decoded, `\`) || strings.Contains(decoded, "://") || strings.Contains(decoded, "..") {
			return false
		}
	}
	return true
}

func openAIProxyMethodAllowed(method string) bool {
	switch method {
	case http.MethodGet, http.MethodPost, http.MethodPut, http.MethodPatch, http.MethodDelete:
		return true
	default:
		return false
	}
}

func openAIProxyMayUseBudget(method string) bool {
	switch method {
	case http.MethodPost, http.MethodPut, http.MethodPatch:
		return true
	default:
		return false
	}
}

func modelFromOpenAIProxyRequest(body []byte, contentType string) string {
	if len(bytes.TrimSpace(body)) == 0 {
		return ""
	}
	mediaType, params, _ := mime.ParseMediaType(contentType)
	switch mediaType {
	case "application/json", "":
		var payload map[string]any
		if json.Unmarshal(body, &payload) == nil {
			if model, ok := payload["model"].(string); ok {
				return strings.TrimSpace(model)
			}
		}
	case "application/x-www-form-urlencoded":
		values, err := url.ParseQuery(string(body))
		if err == nil {
			return strings.TrimSpace(values.Get("model"))
		}
	case "multipart/form-data":
		boundary := params["boundary"]
		if boundary == "" {
			return ""
		}
		reader := multipart.NewReader(bytes.NewReader(body), boundary)
		for {
			part, err := reader.NextPart()
			if err != nil {
				return ""
			}
			if part.FormName() != "model" {
				_ = part.Close()
				continue
			}
			value, _ := io.ReadAll(io.LimitReader(part, 1024))
			_ = part.Close()
			return strings.TrimSpace(string(value))
		}
	}
	return ""
}

func copyPassthroughResponseHeaders(dst, src http.Header) {
	for name, values := range src {
		if !forwardPassthroughResponseHeader(name) {
			continue
		}
		for _, value := range values {
			dst.Add(name, value)
		}
	}
}

func forwardPassthroughResponseHeader(name string) bool {
	switch http.CanonicalHeaderKey(name) {
	case "Connection", "Content-Length", "Keep-Alive", "Proxy-Authenticate", "Proxy-Authorization", "Te", "Trailer", "Transfer-Encoding", "Upgrade":
		return false
	default:
		return true
	}
}

func responseIsEventStream(header http.Header) bool {
	return strings.Contains(strings.ToLower(header.Get("Content-Type")), "text/event-stream")
}

func parseUsageFromProxyPayload(contentType string, body []byte) usageParsed {
	if len(bytes.TrimSpace(body)) == 0 {
		return usageParsed{}
	}
	lowerType := strings.ToLower(contentType)
	if strings.Contains(lowerType, "text/event-stream") {
		if parsed, ok := parseLastUsageObject(body); ok {
			return parsed
		}
		return parseUsageFromSSEBuffer(body)
	}
	if strings.Contains(lowerType, "application/json") || json.Valid(body) {
		if parsed, ok := parseLastUsageObject(body); ok {
			return parsed
		}
		if json.Valid(body) {
			return parseUsageFromResponse(body)
		}
	}
	return usageParsed{}
}

func parseLastUsageObject(body []byte) (usageParsed, bool) {
	searchEnd := len(body)
	needle := []byte(`"usage"`)
	for searchEnd > 0 {
		rel := bytes.LastIndex(body[:searchEnd], needle)
		if rel < 0 {
			return usageParsed{}, false
		}
		rest := bytes.TrimLeft(body[rel+len(needle):], " \t\r\n")
		if len(rest) == 0 || rest[0] != ':' {
			searchEnd = rel
			continue
		}
		rest = bytes.TrimLeft(rest[1:], " \t\r\n")
		if len(rest) == 0 || rest[0] != '{' {
			searchEnd = rel
			continue
		}
		raw, ok := completeJSONObject(rest)
		if !ok {
			searchEnd = rel
			continue
		}
		return usageFromObject(raw), true
	}
	return usageParsed{}, false
}

func completeJSONObject(body []byte) ([]byte, bool) {
	dec := json.NewDecoder(bytes.NewReader(body))
	var raw json.RawMessage
	if err := dec.Decode(&raw); err != nil {
		return nil, false
	}
	raw = bytes.TrimSpace(raw)
	if len(raw) == 0 || raw[0] != '{' {
		return nil, false
	}
	return raw, true
}

func usageFromObject(raw []byte) usageParsed {
	var usageRaw map[string]any
	if json.Unmarshal(raw, &usageRaw) != nil {
		return usageParsed{}
	}
	out := usageParsed{}
	out.PromptTokens = int64(toFloat(usageRaw["prompt_tokens"]))
	if out.PromptTokens == 0 {
		out.PromptTokens = int64(toFloat(usageRaw["input_tokens"]))
	}
	out.CompletionTokens = int64(toFloat(usageRaw["completion_tokens"]))
	if out.CompletionTokens == 0 {
		out.CompletionTokens = int64(toFloat(usageRaw["output_tokens"]))
	}
	out.TotalTokens = int64(toFloat(usageRaw["total_tokens"]))
	if out.TotalTokens == 0 {
		out.TotalTokens = out.PromptTokens + out.CompletionTokens
	}
	return out
}

func parseUsageFromSSEBuffer(body []byte) usageParsed {
	usage := usageParsed{}
	events := bytes.Split(body, []byte("\n\n"))
	for _, event := range events {
		for _, line := range bytes.Split(event, []byte("\n")) {
			line = bytes.TrimSpace(line)
			if !bytes.HasPrefix(line, []byte("data:")) {
				continue
			}
			data := bytes.TrimSpace(bytes.TrimPrefix(line, []byte("data:")))
			if len(data) == 0 || bytes.Equal(data, []byte("[DONE]")) {
				continue
			}
			if parsed := parseUsageFromResponse(data); parsed != (usageParsed{}) {
				usage = parsed
			}
		}
	}
	return usage
}

func (a *App) recordOpenAIProxyUsage(ctx context.Context, principal auth.Principal, endpoint, model string, usage usageParsed, statusCode int) {
	if usage.Model == "" {
		usage.Model = model
	}
	a.recordParsedUsage(ctx, principal, endpoint, model, usage, statusCode)
}

// tailBuffer keeps the last limit bytes so trailing usage can be parsed without buffering the body.
type tailBuffer struct {
	buf   []byte
	limit int
}

func (b *tailBuffer) Write(p []byte) (int, error) {
	if b.limit <= 0 || len(p) == 0 {
		return len(p), nil
	}
	if len(p) >= b.limit {
		b.buf = append(b.buf[:0], p[len(p)-b.limit:]...)
		return len(p), nil
	}
	total := len(b.buf) + len(p)
	if total <= b.limit {
		b.buf = append(b.buf, p...)
		return len(p), nil
	}
	drop := total - b.limit
	b.buf = append(b.buf[drop:], p...)
	return len(p), nil
}

func (b *tailBuffer) Bytes() []byte {
	return b.buf
}

type flushWriter struct {
	w       io.Writer
	flusher http.Flusher
}

func (w flushWriter) Write(p []byte) (int, error) {
	n, err := w.w.Write(p)
	w.flusher.Flush()
	return n, err
}

// Passthrough-only helpers kept here pending 01-34's removal of this file:
// the Responses handler no longer shares them.

func openAIV1EndpointPath(path string) (string, bool) {
	switch {
	case strings.HasPrefix(path, "/api/v1/"):
		return strings.TrimPrefix(path, "/api/v1"), true
	case path == "/api/v1":
		return "", true
	case strings.HasPrefix(path, "/v1/"):
		return strings.TrimPrefix(path, "/v1"), true
	case path == "/v1":
		return "", true
	default:
		return "", false
	}
}

func statusForProviderError(err error) int {
	var upstreamErr *llm.UpstreamError
	if err != nil && strings.Contains(err.Error(), "context deadline exceeded") {
		return http.StatusGatewayTimeout
	}
	if err != nil && strings.Contains(err.Error(), "context canceled") {
		return 499
	}
	if err != nil && errors.As(err, &upstreamErr) && upstreamErr.StatusCode > 0 {
		return upstreamErr.StatusCode
	}
	return http.StatusBadGateway
}

func (a *App) recordParsedUsage(ctx context.Context, principal auth.Principal, endpoint, model string, usage usageParsed, statusCode int) {
	if a.usageRepo == nil {
		return
	}
	if usage.Model == "" {
		usage.Model = model
	}
	if usage.TotalTokens == 0 {
		usage.TotalTokens = usage.PromptTokens + usage.CompletionTokens
	}
	recordCtx, cancel := context.WithTimeout(ctx, 5*time.Second)
	defer cancel()
	a.RecordUsage(recordCtx, sqlite.UsageRecord{
		ID:               NewID("usage"),
		APIKeyID:         principal.APIKeyID,
		Endpoint:         endpoint,
		Model:            usage.Model,
		PromptTokens:     usage.PromptTokens,
		CompletionTokens: usage.CompletionTokens,
		TotalTokens:      usage.TotalTokens,
		CostCents:        0,
		StatusCode:       statusCode,
	})
}
