package httpx

import (
	"bufio"
	"context"
	"database/sql"
	"encoding/json"
	"errors"
	"io"
	"log"
	"net"
	"net/http"
	"net/url"
	"strings"
	"time"

	"enclava-go/internal/auth"
	"enclava-go/internal/chatbot"
	"enclava-go/internal/llm"
)

func (a *App) handleChatbots(w http.ResponseWriter, r *http.Request, principal auth.Principal) {
	if a.chatbotSvc == nil {
		WriteOpenAIError(w, http.StatusNotImplemented, "chatbots are not configured", "server_error", "not_configured")
		return
	}
	switch r.Method {
	case http.MethodGet:
		bots, err := a.chatbotSvc.List(r.Context(), principal)
		if err != nil {
			log.Printf("failed to list chatbots: %v", err)
			WriteOpenAIError(w, http.StatusInternalServerError, "internal server error", "server_error", "chatbots_list_failed")
			return
		}
		WriteJSON(w, http.StatusOK, map[string]any{"chatbots": bots})
	case http.MethodPost:
		if !auth.HasScope(principal, "chatbot.manage") {
			WriteForbidden(w, "missing required scope: chatbot.manage")
			return
		}
		var input chatbot.CreateChatbotInput
		if err := DecodeJSON(r, &input); err != nil {
			WriteOpenAIError(w, http.StatusBadRequest, "invalid JSON payload", "invalid_request_error", "invalid_json")
			return
		}
		bot, err := a.chatbotSvc.Create(r.Context(), principal, input)
		if err != nil {
			WriteOpenAIError(w, http.StatusBadRequest, err.Error(), "invalid_request_error", "chatbot_invalid")
			return
		}
		WriteJSON(w, http.StatusCreated, bot)
	default:
		WriteMethodNotAllowed(w)
	}
}

func (a *App) handleChatbotByID(w http.ResponseWriter, r *http.Request, principal auth.Principal) {
	if a.chatbotSvc == nil {
		WriteOpenAIError(w, http.StatusNotImplemented, "chatbots are not configured", "server_error", "not_configured")
		return
	}
	id := strings.Trim(strings.TrimPrefix(r.URL.Path, "/api/v1/chatbots/"), "/")
	if id == "" {
		WriteOpenAIError(w, http.StatusBadRequest, "chatbot id is required", "invalid_request_error", "missing_chatbot")
		return
	}
	switch r.Method {
	case http.MethodGet:
		bot, err := a.chatbotSvc.Get(r.Context(), principal, id)
		if err != nil {
			writeChatbotNotFoundOrError(w, err, "chatbot_fetch_failed")
			return
		}
		WriteJSON(w, http.StatusOK, bot)
	case http.MethodPut:
		if !auth.HasScope(principal, "chatbot.manage") {
			WriteForbidden(w, "missing required scope: chatbot.manage")
			return
		}
		var input chatbot.UpdateChatbotInput
		if err := DecodeJSON(r, &input); err != nil {
			WriteOpenAIError(w, http.StatusBadRequest, "invalid JSON payload", "invalid_request_error", "invalid_json")
			return
		}
		bot, err := a.chatbotSvc.Update(r.Context(), principal, id, input)
		if err != nil {
			if errors.Is(err, sql.ErrNoRows) {
				WriteOpenAIError(w, http.StatusNotFound, "chatbot not found", "invalid_request_error", "not_found")
				return
			}
			WriteOpenAIError(w, http.StatusBadRequest, err.Error(), "invalid_request_error", "chatbot_invalid")
			return
		}
		WriteJSON(w, http.StatusOK, bot)
	case http.MethodDelete:
		if !auth.HasScope(principal, "chatbot.manage") {
			WriteForbidden(w, "missing required scope: chatbot.manage")
			return
		}
		if err := a.chatbotSvc.Delete(r.Context(), principal, id); err != nil {
			writeChatbotNotFoundOrError(w, err, "chatbot_delete_failed")
			return
		}
		WriteJSON(w, http.StatusOK, map[string]any{"deleted": true, "id": id})
	default:
		WriteMethodNotAllowed(w)
	}
}

func (a *App) handleExternalChatbot(w http.ResponseWriter, r *http.Request, principal auth.Principal) {
	if a.chatbotSvc == nil {
		WriteOpenAIError(w, http.StatusNotImplemented, "chatbots are not configured", "server_error", "not_configured")
		return
	}
	rest := strings.Trim(strings.TrimPrefix(r.URL.Path, "/api/v1/chatbot/external/"), "/")
	parts := strings.Split(rest, "/")
	if len(parts) < 2 || strings.TrimSpace(parts[0]) == "" {
		WriteOpenAIError(w, http.StatusBadRequest, "chatbot id is required", "invalid_request_error", "missing_chatbot")
		return
	}
	ref := parts[0]
	action := strings.Join(parts[1:], "/")
	switch action {
	case "v1/models":
		if r.Method != http.MethodGet {
			WriteMethodNotAllowed(w)
			return
		}
		models, err := a.chatbotSvc.ExternalModels(r.Context(), principal, ref)
		if err != nil {
			a.writeChatbotCompletionError(w, r.Context(), err)
			return
		}
		WriteJSON(w, http.StatusOK, models)
	case "chat/completions":
		if r.Method != http.MethodPost {
			WriteMethodNotAllowed(w)
			return
		}
		a.handleExternalChatbotCompletion(w, r, principal, ref)
	default:
		WriteOpenAIError(w, http.StatusNotFound, "chatbot route not found", "invalid_request_error", "not_found")
	}
}

func (a *App) handleExternalChatbotCompletion(w http.ResponseWriter, r *http.Request, principal auth.Principal, ref string) {
	if a.prober != nil && !a.prober.Ready() {
		WriteOpenAIError(w, http.StatusServiceUnavailable, "provider not ready", "server_error", "provider_not_ready")
		return
	}
	if err := a.EnforceBudget(r.Context(), principal); err != nil {
		a.WriteBudgetError(w, err)
		return
	}
	requestBytes, err := io.ReadAll(io.LimitReader(r.Body, 16*1024*1024))
	if err != nil {
		WriteOpenAIError(w, http.StatusBadRequest, "invalid request body", "invalid_request_error", "invalid_body")
		return
	}
	if chatbotRequestWantsStream(requestBytes) {
		a.handleExternalChatbotCompletionStream(w, r, principal, ref, requestBytes)
		return
	}
	out, err := a.chatbotSvc.CreateChatCompletion(r.Context(), principal, ref, requestBytes)
	if err != nil {
		a.writeChatbotCompletionError(w, r.Context(), err)
		return
	}
	w.Header().Set("Content-Type", "application/json")
	w.WriteHeader(http.StatusOK)
	_, _ = w.Write(out.Response)
}

func (a *App) handleExternalChatbotCompletionStream(w http.ResponseWriter, r *http.Request, principal auth.Principal, ref string, requestBytes []byte) {
	prepared, err := a.chatbotSvc.PrepareChatCompletion(r.Context(), principal, ref, requestBytes, true)
	if err != nil {
		a.writeChatbotCompletionError(w, r.Context(), err)
		return
	}
	stream, err := a.provider.CreateChatCompletionStream(r.Context(), prepared.UpstreamRequest)
	if err != nil {
		recordCtx, cancel := context.WithTimeout(context.WithoutCancel(r.Context()), 5*time.Second)
		defer cancel()
		a.chatbotSvc.RecordFailure(recordCtx, principal, prepared, statusForProviderError(err))
		a.WriteProviderError(w, err)
		return
	}
	defer func() {
		_ = stream.Close()
	}()

	w.Header().Set("Content-Type", "text/event-stream")
	w.Header().Set("Cache-Control", "no-cache")
	w.Header().Set("Connection", "keep-alive")
	w.Header().Set("X-Accel-Buffering", "no")
	w.Header().Set("X-Enclava-Chatbot-ID", prepared.Chatbot.ID)
	w.Header().Set("X-Enclava-Conversation-ID", prepared.Conversation.ID)
	w.WriteHeader(http.StatusOK)

	flusher, _ := w.(http.Flusher)
	scanner := bufio.NewScanner(stream)
	scanner.Buffer(make([]byte, 0, maxSSELineBytes), maxSSELineBytes)
	usage := usageParsed{Model: prepared.Model}
	streamStatusCode := http.StatusOK
	eventData := make([]byte, 0, 256)
	var assistant strings.Builder

	for {
		if !scanner.Scan() {
			if err := scanner.Err(); err != nil {
				streamStatusCode = http.StatusBadGateway
			}
			break
		}

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
			a.applyChatbotStreamEvent(eventData, &usage, &assistant)
			eventData = eventData[:0]
		}
	}
	if len(eventData) > 0 {
		a.applyChatbotStreamEvent(eventData, &usage, &assistant)
	}
	if usage.Model == "" {
		usage.Model = prepared.Model
	}

	recordCtx, cancel := context.WithTimeout(context.WithoutCancel(r.Context()), 5*time.Second)
	defer cancel()
	a.chatbotSvc.RecordCompletion(recordCtx, principal, prepared, assistant.String(), usage.PromptTokens, usage.CompletionTokens, usage.TotalTokens, streamStatusCode)
}

func (a *App) applyChatbotStreamEvent(eventData []byte, usage *usageParsed, assistant *strings.Builder) {
	if delta := parseChatbotStreamingAssistantDelta(eventData); delta != "" {
		assistant.WriteString(delta)
	}
	parsed := parseUsageFromResponse(eventData)
	if parsed == (usageParsed{}) {
		return
	}
	if parsed.Model == "" {
		parsed.Model = usage.Model
	}
	*usage = parsed
}

func chatbotRequestWantsStream(body []byte) bool {
	var payload struct {
		Stream bool `json:"stream"`
	}
	if err := json.Unmarshal(body, &payload); err != nil {
		return false
	}
	return payload.Stream
}

func parseChatbotStreamingAssistantDelta(body []byte) string {
	var env map[string]any
	if json.Unmarshal(body, &env) != nil {
		return ""
	}
	choices, ok := env["choices"].([]any)
	if !ok || len(choices) == 0 {
		return ""
	}
	first, ok := choices[0].(map[string]any)
	if !ok {
		return ""
	}
	if delta, ok := first["delta"].(map[string]any); ok {
		return chatbotContentToString(delta["content"])
	}
	if message, ok := first["message"].(map[string]any); ok {
		return chatbotContentToString(message["content"])
	}
	return ""
}

func chatbotContentToString(v any) string {
	switch content := v.(type) {
	case string:
		return content
	case nil:
		return ""
	default:
		b, err := json.Marshal(content)
		if err != nil {
			return ""
		}
		return string(b)
	}
}


func (a *App) writeChatbotCompletionError(w http.ResponseWriter, ctx context.Context, err error) {
	if errors.Is(err, chatbot.ErrCollectionNotFound) {
		WriteOpenAIError(w, http.StatusNotFound, "knowledge collection was not found", "invalid_request_error", "not_found")
		return
	}
	if errors.Is(err, sql.ErrNoRows) || errors.Is(err, chatbot.ErrDisabled) {
		WriteOpenAIError(w, http.StatusNotFound, "chatbot not found", "invalid_request_error", "not_found")
		return
	}
	if errors.Is(err, chatbot.ErrModelNotAllowed) {
		WriteForbidden(w, err.Error())
		return
	}
	if errors.Is(err, chatbot.ErrChatbotNotAllowed) {
		WriteForbidden(w, err.Error())
		return
	}
	if errors.Is(err, chatbot.ErrInvalidInput) {
		WriteOpenAIError(w, http.StatusBadRequest, err.Error(), "invalid_request_error", "chatbot_completion_failed")
		return
	}
	var upstream *llm.UpstreamError
	if errors.As(err, &upstream) {
		a.WriteProviderError(w, upstream)
		return
	}
	status, message, code := classifyChatbotCompletionError(ctx, err)
	log.Printf("chatbot completion failed: %v", err)
	WriteOpenAIError(w, status, message, "server_error", code)
}

func classifyChatbotCompletionError(ctx context.Context, err error) (int, string, string) {
	if isChatbotDeadline(ctx, err) {
		return http.StatusGatewayTimeout, "upstream request timed out", "gateway_timeout"
	}
	if isChatbotProviderDialError(err) {
		return http.StatusBadGateway, "upstream provider unavailable", "upstream_unavailable"
	}
	return http.StatusInternalServerError, "internal server error", "chatbot_completion_failed"
}

func isChatbotDeadline(ctx context.Context, err error) bool {
	if err == nil {
		return false
	}
	if errors.Is(err, context.DeadlineExceeded) {
		return true
	}
	// context.Canceled is a deadline when the caller context already timed out,
	// or when the error text itself reports a deadline. A plain client cancel is not.
	if !errors.Is(err, context.Canceled) {
		return false
	}
	if ctx != nil && errors.Is(ctx.Err(), context.DeadlineExceeded) {
		return true
	}
	msg := strings.ToLower(err.Error())
	return strings.Contains(msg, "deadline exceeded") || strings.Contains(msg, "context deadline")
}

func isChatbotProviderDialError(err error) bool {
	if err == nil {
		return false
	}
	var netErr net.Error
	if errors.As(err, &netErr) {
		return true
	}
	var urlErr *url.Error
	if errors.As(err, &urlErr) {
		return true
	}
	msg := strings.ToLower(err.Error())
	for _, needle := range []string{
		"dial tcp",
		"dial udp",
		"no such host",
		"connection refused",
		"connection reset",
		"network is unreachable",
		"i/o timeout",
		"tls handshake timeout",
	} {
		if strings.Contains(msg, needle) {
			return true
		}
	}
	return false
}

func writeChatbotNotFoundOrError(w http.ResponseWriter, err error, code string) {
	if errors.Is(err, sql.ErrNoRows) {
		WriteOpenAIError(w, http.StatusNotFound, "chatbot not found", "invalid_request_error", "not_found")
		return
	}
	log.Printf("chatbot error: %v", err)
	WriteOpenAIError(w, http.StatusInternalServerError, "internal server error", "server_error", code)
}
