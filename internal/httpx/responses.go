package httpx

import (
	"bufio"
	"bytes"
	"context"
	"encoding/json"
	"errors"
	"io"
	"net/http"
	"strings"
	"time"

	"enclava-go/internal/auth"
	"enclava-go/internal/llm"
	"enclava-go/internal/store/sqlite"
)

type responseAPIRequest struct {
	Model  string `json:"model"`
	Stream bool   `json:"stream"`
}

func (a *App) handleResponses(w http.ResponseWriter, r *http.Request, principal auth.Principal) {
	if a.prober != nil && !a.prober.Ready() {
		WriteOpenAIError(w, http.StatusServiceUnavailable, "provider not ready", "server_error", "provider_not_ready")
		return
	}
	responses, ok := a.provider.(llm.ResponsesProvider)
	if !ok {
		WriteOpenAIError(w, http.StatusNotImplemented, "configured provider does not support responses", "server_error", "responses_unsupported")
		return
	}

	upstreamPath, endpointPath, err := responseUpstreamPath(r)
	if err != nil {
		WriteOpenAIError(w, http.StatusNotFound, err.Error(), "invalid_request_error", "not_found")
		return
	}
	usageEndpoint := "/api/v1" + endpointPath
	if !responseMethodAllowed(r.Method, endpointPath) {
		if openAIProxyPathSafe(endpointPath) && a.maybeHandleOpenAIPassthrough(w, r, principal) {
			return
		}
		WriteMethodNotAllowed(w)
		return
	}

	recordUsage := responsesCreateRequest(r.Method, endpointPath)
	var requestBytes []byte
	reqMeta := responseAPIRequest{}
	if recordUsage {
		if err := a.EnforceBudget(r.Context(), principal); err != nil {
			a.WriteBudgetError(w, err)
			return
		}
	}
	if r.Method == http.MethodPost {
		body, err := io.ReadAll(io.LimitReader(r.Body, 16*1024*1024))
		if err != nil {
			WriteOpenAIError(w, http.StatusBadRequest, "invalid request body", "invalid_request_error", "invalid_body")
			return
		}
		requestBytes = body
		if len(bytes.TrimSpace(body)) > 0 {
			_ = json.Unmarshal(body, &reqMeta)
		}
	}
	if reqMeta.Model != "" && !auth.ModelAllowed(principal, reqMeta.Model) {
		WriteForbidden(w, "model is not allowed for this API key")
		return
	}

	if responseShouldStream(r, reqMeta) {
		a.proxyResponsesStream(w, r, principal, responses, upstreamPath, usageEndpoint, requestBytes, reqMeta.Model, recordUsage)
		return
	}

	resp, err := responses.DoResponsesRequest(r.Context(), r.Method, upstreamPath, requestBytes)
	if err != nil {
		if recordUsage {
			a.recordParsedUsage(r.Context(), principal, usageEndpoint, reqMeta.Model, usageParsed{}, statusForProviderError(err))
		}
		a.WriteProviderError(w, err)
		return
	}
	usage := parseUsageFromResponse(resp)
	if recordUsage && shouldRecordParsedUsage(r.Method, usage, http.StatusOK) {
		a.recordParsedUsage(r.Context(), principal, usageEndpoint, reqMeta.Model, usage, http.StatusOK)
	}
	w.Header().Set("Content-Type", "application/json")
	w.WriteHeader(http.StatusOK)
	_, _ = w.Write(resp)
}

func (a *App) proxyResponsesStream(w http.ResponseWriter, r *http.Request, principal auth.Principal, responses llm.ResponsesProvider, upstreamPath, endpointPath string, requestBytes []byte, model string, recordUsage bool) {
	stream, err := responses.DoResponsesStream(r.Context(), r.Method, upstreamPath, requestBytes)
	if err != nil {
		if recordUsage {
			a.recordParsedUsage(r.Context(), principal, endpointPath, model, usageParsed{Model: model}, statusForProviderError(err))
		}
		a.WriteProviderError(w, err)
		return
	}
	defer func() { _ = stream.Close() }()

	w.Header().Set("Content-Type", "text/event-stream")
	w.Header().Set("Cache-Control", "no-cache")
	w.Header().Set("Connection", "keep-alive")
	w.Header().Set("X-Accel-Buffering", "no")
	w.WriteHeader(http.StatusOK)

	flusher, _ := w.(http.Flusher)
	scanner := bufio.NewScanner(stream)
	scanner.Buffer(make([]byte, 0, maxSSELineBytes), maxSSELineBytes)
	usage := usageParsed{Model: model}
	streamStatusCode := http.StatusOK
	eventData := make([]byte, 0, 256)

	for scanner.Scan() {
		line := scanner.Text()
		if _, writeErr := io.WriteString(w, line); writeErr != nil {
			streamStatusCode = http.StatusBadGateway
			break
		}
		if _, writeErr := w.Write([]byte{'\n'}); writeErr != nil {
			streamStatusCode = http.StatusBadGateway
			break
		}
		if flusher != nil {
			flusher.Flush()
		}

		trimmed := strings.TrimSpace(line)
		if strings.HasPrefix(trimmed, "data:") {
			data := strings.TrimSpace(strings.TrimPrefix(trimmed, "data:"))
			if data == "" || data == "[DONE]" {
				continue
			}
			if len(eventData)+len(data)+1 > maxSSEEventBytes {
				eventData = eventData[:0]
				continue
			}
			if len(eventData) > 0 {
				eventData = append(eventData, '\n')
			}
			eventData = append(eventData, []byte(data)...)
		} else if trimmed == "" && len(eventData) > 0 {
			if parsed := parseUsageFromResponse(eventData); parsed != (usageParsed{}) {
				usage = parsed
			}
			eventData = eventData[:0]
		}
	}
	if err := scanner.Err(); err != nil {
		streamStatusCode = http.StatusBadGateway
	}
	if len(eventData) > 0 {
		if parsed := parseUsageFromResponse(eventData); parsed != (usageParsed{}) {
			usage = parsed
		}
	}
	if usage.Model == "" {
		usage.Model = model
	}
	if recordUsage {
		a.recordParsedUsage(context.WithoutCancel(r.Context()), principal, endpointPath, model, usage, streamStatusCode)
	}
}

func responseUpstreamPath(r *http.Request) (string, string, error) {
	endpoint, ok := openAIV1EndpointPath(r.URL.Path)
	if !ok || endpoint == "" {
		return "", "", errResponsePath("invalid responses path")
	}
	if endpoint == "/responses/" {
		endpoint = "/responses"
	}
	upstream := endpoint
	if r.URL.RawQuery != "" {
		upstream += "?" + r.URL.RawQuery
	}
	return upstream, endpoint, nil
}

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

func responsesCreateRequest(method, endpointPath string) bool {
	return method == http.MethodPost && endpointPath == "/responses"
}

func responseMethodAllowed(method, endpoint string) bool {
	if endpoint == "/responses" {
		return method == http.MethodPost
	}
	if endpoint == "/responses/input_tokens" || endpoint == "/responses/compact" {
		return method == http.MethodPost
	}
	if !strings.HasPrefix(endpoint, "/responses/") {
		return false
	}
	parts := strings.Split(strings.TrimPrefix(endpoint, "/responses/"), "/")
	if len(parts) == 0 || strings.TrimSpace(parts[0]) == "" {
		return false
	}
	if len(parts) > 2 {
		return false
	}
	if len(parts) == 1 {
		return method == http.MethodGet || method == http.MethodDelete
	}
	switch {
	case strings.HasSuffix(endpoint, "/cancel"):
		return method == http.MethodPost
	case strings.HasSuffix(endpoint, "/input_items"):
		return method == http.MethodGet
	default:
		return false
	}
}

func responseShouldStream(r *http.Request, meta responseAPIRequest) bool {
	if meta.Stream {
		return true
	}
	return strings.EqualFold(r.URL.Query().Get("stream"), "true")
}

func shouldRecordParsedUsage(method string, usage usageParsed, statusCode int) bool {
	if statusCode >= 400 {
		return true
	}
	if usage.PromptTokens > 0 || usage.CompletionTokens > 0 || usage.TotalTokens > 0 {
		return true
	}
	return method == http.MethodPost
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

type responsePathError string

func (e responsePathError) Error() string { return string(e) }

func errResponsePath(msg string) error { return responsePathError(msg) }
