package httpx

import (
	"bufio"
	"context"
	"encoding/json"
	"errors"
	"io"
	"log"
	"net/http"
	"sort"
	"strings"
	"unicode/utf8"

	"enclava-go/internal/auth"
	"enclava-go/internal/billing"
	"enclava-go/internal/inference"
	"enclava-go/internal/llm"
)

// The selected public OpenAI core surface: tenant endpoint model-alias
// metadata for models, and the shared executor for chat/vision/embeddings.
// Nothing here talks to a provider directly and there is no fallback proxy —
// unselected /v1 paths are not registered and return 404.

// modelAlias extracts the public alias half of a "model:<alias>[=<upstream>]"
// capability binding; the upstream id never reaches a client response.
func modelAlias(capability string) (string, bool) {
	if !strings.HasPrefix(capability, inference.ModelCapabilityPrefix) {
		return "", false
	}
	binding := strings.TrimPrefix(capability, inference.ModelCapabilityPrefix)
	alias, _, _ := strings.Cut(binding, "=")
	alias = strings.TrimSpace(alias)
	return alias, alias != ""
}

// permittedModels returns the distinct model aliases the principal may see:
// enabled tenant endpoints, intersected with the actor's endpoint and model
// allowlists. Other tenants' endpoints are never consulted.
func (a *App) permittedModels(ctx context.Context, principal auth.Principal) ([]llm.Model, error) {
	if a.endpoints == nil {
		return nil, errors.New("endpoint store not configured")
	}
	endpoints, err := a.endpoints.List(ctx, principal.TenantID)
	if err != nil {
		return nil, err
	}
	aliases := map[string]struct{}{}
	for _, e := range endpoints {
		if !e.Enabled || !auth.EndpointAllowed(principal, e.ID) {
			continue
		}
		for _, cap := range e.Capabilities {
			alias, ok := modelAlias(cap)
			if !ok || !auth.ModelAllowed(principal, alias) {
				continue
			}
			aliases[alias] = struct{}{}
		}
	}
	ids := make([]string, 0, len(aliases))
	for alias := range aliases {
		ids = append(ids, alias)
	}
	sort.Strings(ids)
	models := make([]llm.Model, 0, len(ids))
	for _, id := range ids {
		models = append(models, llm.Model{ID: id, Object: "model", OwnedBy: "enclava"})
	}
	return models, nil
}

func (a *App) handleModels(w http.ResponseWriter, r *http.Request, principal auth.Principal) {
	if r.Method != http.MethodGet {
		WriteMethodNotAllowed(w)
		return
	}
	models, err := a.permittedModels(r.Context(), principal)
	if err != nil {
		log.Printf("list permitted models: %v", err)
		WriteOpenAIError(w, http.StatusInternalServerError, "internal server error", "server_error", "internal_error")
		return
	}
	WriteJSON(w, http.StatusOK, llm.ModelsResponse{Object: "list", Data: models})
}

func (a *App) handleModelByID(w http.ResponseWriter, r *http.Request, principal auth.Principal) {
	if r.Method != http.MethodGet {
		WriteMethodNotAllowed(w)
		return
	}
	modelID := modelIDFromPath(r.URL.Path)
	if strings.TrimSpace(modelID) == "" {
		WriteOpenAIError(w, http.StatusBadRequest, "model id is required", "invalid_request_error", "missing_model")
		return
	}
	models, err := a.permittedModels(r.Context(), principal)
	if err != nil {
		log.Printf("get permitted model: %v", err)
		WriteOpenAIError(w, http.StatusInternalServerError, "internal server error", "server_error", "internal_error")
		return
	}
	for _, m := range models {
		if m.ID == modelID {
			WriteJSON(w, http.StatusOK, m)
			return
		}
	}
	// Unknown and disallowed aliases are indistinguishable: 404 either way.
	WriteOpenAIError(w, http.StatusNotFound, "model not found", "invalid_request_error", "model_not_found")
}

// inferenceRequestMeta is the narrow decode of a client payload: only the
// fields the gateway itself needs. The raw body is handed to the executor
// untouched, so exact numeric data never round-trips through floats here.
type inferenceRequestMeta struct {
	Model    string          `json:"model"`
	Stream   bool            `json:"stream"`
	Messages json.RawMessage `json:"messages"`
	Input    json.RawMessage `json:"input"`
}

func (a *App) handleChatCompletions(w http.ResponseWriter, r *http.Request, principal auth.Principal) {
	if r.Method != http.MethodPost {
		WriteMethodNotAllowed(w)
		return
	}
	requestBytes, ok := a.readInferenceBody(w, r, 16*1024*1024)
	if !ok {
		return
	}
	var meta inferenceRequestMeta
	if err := json.Unmarshal(requestBytes, &meta); err != nil {
		WriteOpenAIError(w, http.StatusBadRequest, "invalid JSON payload", "invalid_request_error", "invalid_json")
		return
	}
	if strings.TrimSpace(meta.Model) == "" {
		WriteOpenAIError(w, http.StatusBadRequest, "model is required", "invalid_request_error", "missing_model")
		return
	}
	if len(meta.Messages) == 0 || string(meta.Messages) == "null" {
		WriteOpenAIError(w, http.StatusBadRequest, "messages is required", "invalid_request_error", "missing_messages")
		return
	}
	if !auth.ModelAllowed(principal, meta.Model) {
		WriteForbidden(w, "model is not allowed for this API key")
		return
	}
	res, err := a.executor.Execute(r.Context(), principal, inference.Request{
		Capability: inference.CapabilityChat,
		Model:      meta.Model,
		Payload:    requestBytes,
		Stream:     meta.Stream,
		Estimate:   chatEstimate(requestBytes),
	})
	if err != nil {
		a.writeInferenceError(w, err)
		return
	}
	defer func() { _ = res.Body.Close() }()
	if meta.Stream {
		a.proxyInferenceStream(w, r, res.Body)
		return
	}
	if res.SettlementError != nil {
		log.Printf("chat completion settlement failed: %v", res.SettlementError)
	}
	a.writeInferenceBody(w, res.Body)
}

func (a *App) handleEmbeddings(w http.ResponseWriter, r *http.Request, principal auth.Principal) {
	if r.Method != http.MethodPost {
		WriteMethodNotAllowed(w)
		return
	}
	requestBytes, ok := a.readInferenceBody(w, r, 8*1024*1024)
	if !ok {
		return
	}
	var meta inferenceRequestMeta
	if err := json.Unmarshal(requestBytes, &meta); err != nil {
		WriteOpenAIError(w, http.StatusBadRequest, "invalid JSON payload", "invalid_request_error", "invalid_json")
		return
	}
	if strings.TrimSpace(meta.Model) == "" {
		WriteOpenAIError(w, http.StatusBadRequest, "model is required", "invalid_request_error", "missing_model")
		return
	}
	if len(meta.Input) == 0 || string(meta.Input) == "null" {
		WriteOpenAIError(w, http.StatusBadRequest, "input is required", "invalid_request_error", "missing_input")
		return
	}
	if meta.Stream {
		WriteOpenAIError(w, http.StatusBadRequest, "embeddings do not support streaming", "invalid_request_error", "unsupported_option")
		return
	}
	if !auth.ModelAllowed(principal, meta.Model) {
		WriteForbidden(w, "model is not allowed for this API key")
		return
	}
	res, err := a.executor.Execute(r.Context(), principal, inference.Request{
		Capability: inference.CapabilityEmbeddings,
		Model:      meta.Model,
		Payload:    requestBytes,
		Estimate:   billing.Usage{InputUnits: tokenEstimate(requestBytes)},
	})
	if err != nil {
		a.writeInferenceError(w, err)
		return
	}
	defer func() { _ = res.Body.Close() }()
	if res.SettlementError != nil {
		log.Printf("embeddings settlement failed: %v", res.SettlementError)
	}
	a.writeInferenceBody(w, res.Body)
}

// readInferenceBody enforces the route's body bound and reports the standard
// OpenAI error on failure.
func (a *App) readInferenceBody(w http.ResponseWriter, r *http.Request, limit int64) ([]byte, bool) {
	if a.executor == nil {
		WriteOpenAIError(w, http.StatusServiceUnavailable, "inference is not configured", "server_error", "inference_unavailable")
		return nil, false
	}
	requestBytes, err := io.ReadAll(io.LimitReader(r.Body, limit))
	if err != nil {
		WriteOpenAIError(w, http.StatusBadRequest, "invalid request body", "invalid_request_error", "invalid_body")
		return nil, false
	}
	return requestBytes, true
}

func (a *App) writeInferenceBody(w http.ResponseWriter, body io.Reader) {
	w.Header().Set("Content-Type", "application/json")
	w.WriteHeader(http.StatusOK)
	_, _ = io.Copy(w, body)
}

// proxyInferenceStream relays the executor's bounded SSE stream to the
// client, parsing individual events under the same line/event bounds the
// platform already uses so tool-call deltas, usage and terminal semantics
// pass through verbatim. A client disconnect aborts the read; Body.Close
// then settles or retains the reservation in the executor.
func (a *App) proxyInferenceStream(w http.ResponseWriter, r *http.Request, body io.Reader) {
	ctx := r.Context()
	w.Header().Set("Content-Type", "text/event-stream")
	w.Header().Set("Cache-Control", "no-cache")
	w.Header().Set("Connection", "keep-alive")
	w.Header().Set("X-Accel-Buffering", "no")
	w.WriteHeader(http.StatusOK)

	flusher, _ := w.(http.Flusher)
	scanner := bufio.NewScanner(body)
	scanner.Buffer(make([]byte, 0, maxSSELineBytes), maxSSELineBytes)
	eventBytes := 0
	for {
		if ctx.Err() != nil {
			return
		}
		if !scanner.Scan() {
			return
		}
		line := scanner.Text()
		if _, err := io.WriteString(w, line); err != nil {
			return
		}
		if _, err := w.Write([]byte{'\n'}); err != nil {
			return
		}
		if flusher != nil {
			flusher.Flush()
		}
		trimmed := strings.TrimSpace(line)
		if strings.HasPrefix(trimmed, "data:") {
			eventBytes += len(trimmed) - len("data:") + 1
			if eventBytes > maxSSEEventBytes {
				// Oversized upstream event: terminate the relay rather than
				// buffer unbounded data; the executor retains liability.
				return
			}
		} else if trimmed == "" {
			eventBytes = 0
		}
	}
}

// writeInferenceError maps executor denial families to public outcomes.
// Internal detail (SQL, credentials, endpoints) never reaches the client.
func (a *App) writeInferenceError(w http.ResponseWriter, err error) {
	var upstreamErr *llm.UpstreamError
	if errors.As(err, &upstreamErr) {
		a.WriteProviderError(w, upstreamErr)
		return
	}
	switch {
	case errors.Is(err, inference.ErrDenied), errors.Is(err, inference.ErrNoRoute):
		WriteForbidden(w, "request denied")
	case errors.Is(err, inference.ErrOverloaded):
		writeOverloaded(w)
	case errors.Is(err, inference.ErrUnbounded), errors.Is(err, inference.ErrUnsupportedCapability):
		WriteOpenAIError(w, http.StatusBadRequest, "invalid inference request", "invalid_request_error", "unsupported_request")
	case errors.Is(err, billing.ErrBudgetExceeded):
		// The sentinel comes straight from the shared admission boundary —
		// it is a bounded 429, not the endpoint-policy error shape.
		WriteOpenAIError(w, http.StatusTooManyRequests, "budget exceeded", "rate_limit_error", "budget_exceeded")
	case errors.Is(err, context.DeadlineExceeded):
		WriteOpenAIError(w, http.StatusGatewayTimeout, "upstream request timed out", "server_error", "gateway_timeout")
	default:
		log.Printf("inference request failed: %v", err)
		WriteOpenAIError(w, http.StatusBadGateway, "upstream provider unavailable", "server_error", "upstream_unavailable")
	}
}

// chatEstimate declares the per-kind unit bound for one chat attempt: input
// sized from the marshaled payload (~4 bytes per token) and output at the
// operation's configured maximum, matching the assistant estimate shape.
func chatEstimate(payload []byte) billing.Usage {
	return billing.Usage{
		InputUnits:  tokenEstimate(payload),
		OutputUnits: 1 << 17,
	}
}

// tokenEstimate approximates input tokens from payload size; the executor's
// bound check keeps it honest against the operation ceiling.
func tokenEstimate(payload []byte) int64 {
	return int64((utf8.RuneCount(payload) + 3) / 4)
}
