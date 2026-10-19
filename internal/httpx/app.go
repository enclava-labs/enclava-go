package httpx

import (
	"bufio"
	"context"
	"encoding/json"
	"errors"
	"fmt"
	"io"
	"log"
	"net/http"
	"strings"
	"time"

	"enclava-go/internal/auth"
	"enclava-go/internal/billing"
	"enclava-go/internal/chatbot"
	"enclava-go/internal/connectors"
	"enclava-go/internal/extract"
	"enclava-go/internal/llm"
	"enclava-go/internal/prompts"
	"enclava-go/internal/rag"
	"enclava-go/internal/store/postgres"
	"enclava-go/internal/store/sqlite"
)

type App struct {
	authSvc      *auth.Service
	provider     llm.Provider
	providerID   string
	prober       *llm.Prober
	identity     *postgres.IdentityRepo
	extractSvc   *extract.Service
	ragSvc       *rag.Service
	promptSvc    *prompts.Service
	chatbotSvc   *chatbot.Service
	connectorSvc *connectors.Service
	usageRepo    *sqlite.UsageRepo
	billingSvc   *billing.Service
	billingRepo  *postgres.BillingRepo
	endpoints    *postgres.EndpointsRepo
	apiKeyAlgo   string
	apiKeyPepper string
	apiKeyPrefix string
	corsOrigins  []string
	sessionAuth  *SessionAuth
	maxUploadMB  int64
	trustProxy   bool
	rl           *RateLimiter
	mux          *http.ServeMux
}

const (
	preAuthRateLimitPerMinute = 120
	preAuthRateLimitPerHour   = 1000
	preAuthRateLimitPerDay    = 20000

	maxSSELineBytes  = 256 * 1024
	maxSSEEventBytes = 128 * 1024
)

type rateLimitCheck struct {
	limit  int64
	window time.Duration
}

func NewApp(
	authSvc *auth.Service,
	provider llm.Provider,
	providerID string,
	prober *llm.Prober,
	identity *postgres.IdentityRepo,
	extractSvc *extract.Service,
	ragSvc *rag.Service,
	promptSvc *prompts.Service,
	chatbotSvc *chatbot.Service,
	connectorSvc *connectors.Service,
	usageRepo *sqlite.UsageRepo,
	billingSvc *billing.Service,
	billingRepo *postgres.BillingRepo,
	endpoints *postgres.EndpointsRepo,
	apiKeyAlgo string,
	apiKeyPepper string,
	apiKeyPrefix string,
	corsOrigins []string,
	sessionAuth *SessionAuth,
	maxUploadMB int64,
	trustProxy bool,
) *App {
	a := &App{
		authSvc:    authSvc,
		provider:   provider,
		providerID: providerID,
		prober:     prober,
		identity:   identity,

		extractSvc:   extractSvc,
		ragSvc:       ragSvc,
		promptSvc:    promptSvc,
		chatbotSvc:   chatbotSvc,
		connectorSvc: connectorSvc,
		usageRepo:    usageRepo,
		billingSvc:   billingSvc,
		billingRepo:  billingRepo,
		endpoints:    endpoints,
		apiKeyAlgo:   apiKeyAlgo,
		apiKeyPepper: apiKeyPepper,
		apiKeyPrefix: apiKeyPrefix,
		corsOrigins:  corsOrigins,
		sessionAuth:  sessionAuth,
		maxUploadMB:  maxUploadMB,
		trustProxy:   trustProxy,
		rl:           NewRateLimiter(),
	}

	a.mux = http.NewServeMux()
	a.registerBaseRoutes()
	return a
}

func (a *App) Handler() http.Handler {
	var h http.Handler = a.mux
	h = http.MaxBytesHandler(h, a.maxRequestBodyBytes())
	h = recoverMiddleware(h)
	h = a.corsMiddleware(h)
	h = secureHeadersMiddleware(h)
	h = a.accessLogMiddleware(h)
	h = requestIDMiddleware(h)
	return h
}

func (a *App) Mux() *http.ServeMux {
	return a.mux
}

// Authenticated exposes the platform auth wrapper for modules.
func (a *App) Authenticated(scope string, next func(http.ResponseWriter, *http.Request, auth.Principal)) http.HandlerFunc {
	return a.authenticated(scope, next)
}

func (a *App) registerBaseRoutes() {
	a.mux.HandleFunc("/", a.handleRoot)
	a.mux.HandleFunc("/health", a.handleHealth)
	a.mux.HandleFunc("/livez", a.handleLivez)
	a.mux.HandleFunc("/readyz", a.handleReadyz)

	a.mux.HandleFunc("/api/v1/models", a.authenticated("models.list", a.handleModels))
	a.mux.HandleFunc("/api/v1/models/cache/invalidate", a.authenticated("models.list", a.handleModelsCacheInvalidate))
	a.mux.HandleFunc("/api/v1/models/", a.authenticated("models.list", a.handleModelByID))
	a.mux.HandleFunc("/api/v1/chat/completions", a.authenticated("chat.completions", a.handleChatCompletions))
	a.mux.HandleFunc("/api/v1/embeddings", a.authenticated("embeddings.create", a.handleEmbeddings))
	a.mux.HandleFunc("/api/v1/responses", a.authenticated("responses", a.handleResponses))
	a.mux.HandleFunc("/api/v1/responses/", a.authenticated("responses", a.handleResponses))
	a.mux.HandleFunc("/v1/models", a.authenticated("models.list", a.handleModels))
	a.mux.HandleFunc("/v1/models/", a.authenticated("models.list", a.handleModelByID))
	a.mux.HandleFunc("/v1/chat/completions", a.authenticated("chat.completions", a.handleChatCompletions))
	a.mux.HandleFunc("/v1/embeddings", a.authenticated("embeddings.create", a.handleEmbeddings))
	a.mux.HandleFunc("/v1/responses", a.authenticated("responses", a.handleResponses))
	a.mux.HandleFunc("/v1/responses/", a.authenticated("responses", a.handleResponses))
	a.mux.HandleFunc("/api/v1/", a.authenticated("openai.proxy", a.handleOpenAIPassthrough))
	a.mux.HandleFunc("/v1/", a.authenticated("openai.proxy", a.handleOpenAIPassthrough))
	a.mux.HandleFunc("/api/v1/attestation/status", a.authenticated("models.list", a.handleAttestationStatus))
	a.mux.HandleFunc("/api/v1/attestation/verify", a.authenticated("models.list", a.handleAttestationVerify))
	a.mux.HandleFunc("/api/v1/providers", a.authenticated("pricing.manage", a.handleProviders))
	a.mux.HandleFunc("/api/v1/providers/", a.authenticated("pricing.manage", a.handleProviderByID))
	a.mux.HandleFunc("/api/v1/rag/collections", a.authenticated("rag", a.handleRAGCollections))
	a.mux.HandleFunc("/api/v1/rag/collections/", a.authenticated("rag", a.handleRAGCollectionByID))
	a.mux.HandleFunc("/api/v1/rag/documents/", a.authenticated("rag", a.handleRAGDocumentByID))
	a.mux.HandleFunc("/api/v1/rag/search", a.authenticated("rag", a.handleRAGSearch))
	a.mux.HandleFunc("/api/v1/prompt-templates", a.authenticated("chatbot", a.handlePromptTemplates))
	a.mux.HandleFunc("/api/v1/prompt-templates/", a.authenticated("chatbot", a.handlePromptTemplateByID))
	a.mux.HandleFunc("/api/v1/chatbots", a.authenticated("chatbot", a.handleChatbots))
	a.mux.HandleFunc("/api/v1/chatbots/", a.authenticated("chatbot", a.handleChatbotByID))
	a.mux.HandleFunc("/api/v1/chatbot/external/", a.authenticated("chatbot", a.handleExternalChatbot))
	a.mux.HandleFunc("/api/v1/connectors/sources", a.authenticated("connectors.manage", a.handleConnectorSources))
	a.mux.HandleFunc("/api/v1/connectors/sources/", a.authenticated("connectors.manage", a.handleConnectorSourceByID))
	a.mux.HandleFunc("/api/v1/connectors/sync-jobs", a.authenticated("connectors.manage", a.handleConnectorSyncJobs))
	a.mux.HandleFunc("/api/v1/connectors/sync-jobs/", a.authenticated("connectors.manage", a.handleConnectorSyncJobByID))

	a.mux.HandleFunc("/api/v1/admin/api-keys", a.authenticated("platform.admin", a.handleAdminAPIKeys))
	a.mux.HandleFunc("/api/v1/admin/api-keys/", a.authenticated("platform.admin", a.handleAdminAPIKeyByID))
	a.mux.HandleFunc("/api/v1/admin/usage/summary", a.authenticated("platform.admin", a.handleAdminUsageSummary))
	a.mux.HandleFunc("/api/v1/admin/pricing", a.authenticated("pricing.manage", a.handleAdminPricing))
	a.mux.HandleFunc("/api/v1/admin/billing-audit", a.authenticated("platform.admin", a.handleAdminBillingAudit))

	a.registerDashboardRoutes()
}

func (a *App) handleRoot(w http.ResponseWriter, r *http.Request) {
	if r.URL.Path != "/" {
		http.NotFound(w, r)
		return
	}
	if r.Method != http.MethodGet && r.Method != http.MethodHead {
		WriteMethodNotAllowed(w)
		return
	}
	http.Redirect(w, r, "/dashboard", http.StatusSeeOther)
}

func recoverMiddleware(next http.Handler) http.Handler {
	return http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		defer func() {
			if recovered := recover(); recovered != nil {
				log.Printf("panic: %s", redactForLog(fmt.Sprint(recovered)))
				WriteOpenAIError(w, http.StatusInternalServerError, "internal server error", "server_error", "internal_error")
			}
		}()
		next.ServeHTTP(w, r)
	})
}

func (a *App) authenticated(scope string, next func(http.ResponseWriter, *http.Request, auth.Principal)) http.HandlerFunc {
	return func(w http.ResponseWriter, r *http.Request) {
		apiKey := auth.ParseAPIKey(r)
		now := time.Now().UTC()
		if a.rl != nil {
			if !a.enforceRateLimits(w, a.unauthenticatedRateLimitKey(r, apiKey), now, []rateLimitCheck{
				{limit: preAuthRateLimitPerMinute, window: time.Minute},
				{limit: preAuthRateLimitPerHour, window: time.Hour},
				{limit: preAuthRateLimitPerDay, window: 24 * time.Hour},
			}) {
				return
			}
		}
		// Credential resolution order: Bearer/X-API-Key → API key auth; the
		// dashboard session cookie → session auth (with CSRF on mutations).
		var principal auth.Principal
		var err error
		cookieAuth := false
		if strings.TrimSpace(apiKey) != "" {
			principal, err = a.authSvc.Authenticate(r.Context(), apiKey)
		} else if cookie, cerr := r.Cookie(sessionCookieName); cerr == nil && strings.TrimSpace(cookie.Value) != "" {
			cookieAuth = true
			principal, err = a.authSvc.AuthenticateSession(r.Context(), cookie.Value)
		} else {
			principal, err = a.authSvc.Authenticate(r.Context(), "")
		}
		if err != nil {
			WriteUnauthorized(w, err.Error())
			return
		}
		if cookieAuth && r.Method != http.MethodGet && r.Method != http.MethodHead && r.Method != http.MethodOptions {
			// Cookie-authenticated mutations must carry the session's CSRF
			// token (header preferred, script-readable cookie as fallback).
			if err := a.authSvc.ValidateCSRF(r.Context(), principal.TenantID, principal.SessionID, csrfFromRequest(r)); err != nil {
				WriteForbidden(w, "missing or invalid CSRF token")
				return
			}
		}
		if a.rl != nil {
			checks := []rateLimitCheck{}
			if principal.RateLimitPerMinute != nil {
				checks = append(checks, rateLimitCheck{limit: int64(*principal.RateLimitPerMinute), window: time.Minute})
			}
			if principal.RateLimitPerHour != nil {
				checks = append(checks, rateLimitCheck{limit: int64(*principal.RateLimitPerHour), window: time.Hour})
			}
			if principal.RateLimitPerDay != nil {
				checks = append(checks, rateLimitCheck{limit: int64(*principal.RateLimitPerDay), window: 24 * time.Hour})
			}
			if !a.enforceRateLimits(w, "credential:"+principal.CredentialID, now, checks) {
				return
			}
		}
		if !auth.HasScope(principal, scope) {
			WriteForbidden(w, fmt.Sprintf("missing required scope: %s", scope))
			return
		}
		a.enforceAllowlists(w, r, principal, next)
	}
}

// enforceAllowlists applies the stored endpoint and IP grants at dispatch.
// Grants carry the decoded {"mode":"all"}→All or {"mode":"list"}→item map
// shape: All==true is unrestricted; an empty/absent/malformed list denies
// everything (fail closed — it never "skips the check"). Endpoint grants
// hold inference endpoint IDs (route families like "chat/completions"),
// never URL path patterns. Only API-key credentials carry endpoint/IP
// grants — session-authenticated dashboard requests are scope-gated only.
func (a *App) enforceAllowlists(w http.ResponseWriter, r *http.Request, principal auth.Principal, next func(http.ResponseWriter, *http.Request, auth.Principal)) {
	if principal.CredentialKind == auth.CredentialKindAPIKey {
		if !principal.AllowedEndpointsAll {
			if len(principal.AllowedEndpoints) == 0 {
				WriteForbidden(w, "endpoint is not allowed for this API key")
				return
			}
			if id := inferenceEndpointID(r.URL.Path); id != "" && !auth.EndpointAllowed(principal, id) {
				WriteForbidden(w, "endpoint is not allowed for this API key")
				return
			}
		}
		if !principal.AllowedIPsAll {
			ip := clientIP(r, a.trustProxy)
			if ip == "" {
				WriteForbidden(w, "source IP could not be determined")
				return
			}
			if !sourceIPAllowed(principal.AllowedIPs, ip) {
				WriteForbidden(w, "source IP is not allowed for this API key")
				return
			}
		}
	}
	next(w, r.WithContext(auth.WithPrincipal(r.Context(), principal)), principal)
}

// inferenceEndpointID maps an inference route to its endpoint ID — the
// route family: "/api/v1/responses/resp_1/cancel" and the "/v1" alias both
// resolve to "responses"; "/api/v1/chat/completions" → "chat/completions".
// Non-inference paths return "" and are governed by scopes only.
func inferenceEndpointID(path string) string {
	var rest string
	switch {
	case strings.HasPrefix(path, "/api/v1/"):
		rest = strings.TrimPrefix(path, "/api/v1/")
	case strings.HasPrefix(path, "/v1/"):
		rest = strings.TrimPrefix(path, "/v1/")
	default:
		return ""
	}
	rest = strings.Trim(rest, "/")
	if rest == "" {
		return ""
	}
	if strings.HasPrefix(rest, "chat/completions") {
		return "chat/completions"
	}
	if i := strings.Index(rest, "/"); i > 0 {
		return rest[:i]
	}
	return rest
}

func (a *App) unauthenticatedRateLimitKey(r *http.Request, apiKey string) string {
	if key := strings.TrimSpace(apiKey); key != "" {
		if len(key) > 12 {
			key = key[:12]
		}
		return "api-key:" + key
	}
	ip := strings.TrimSpace(clientIP(r, a.trustProxy))
	if ip == "" {
		return "ip:unknown"
	}
	return "ip:" + ip
}

func (a *App) enforceRateLimits(w http.ResponseWriter, key string, now time.Time, checks []rateLimitCheck) bool {
	if a.rl == nil || len(checks) == 0 {
		return true
	}
	for _, check := range checks {
		if check.limit <= 0 {
			continue
		}
		dec := a.rl.AllowKey(key, check.limit, check.window, now)
		if !dec.Allowed {
			writeRateLimited(w, dec)
			return false
		}
	}
	return true
}

func (a *App) handleHealth(w http.ResponseWriter, r *http.Request) {
	if r.Method != http.MethodGet {
		WriteMethodNotAllowed(w)
		return
	}
	payload := map[string]any{
		"status": "ok",
		"time":   time.Now().UTC().Format(time.RFC3339),
	}
	if a.prober != nil {
		payload["provider"] = a.prober.Snapshot()
	}
	if attested, ok := a.provider.(llm.AttestationProvider); ok {
		status := attested.AttestationStatus()
		// /health is unauthenticated; redact sensitive attestation details.
		payload["attestation"] = map[string]any{
			"enabled":     status.Enabled,
			"provider":    status.Provider,
			"verified":    status.Verified,
			"verified_at": status.VerifiedAt,
		}
	}
	WriteJSON(w, http.StatusOK, payload)
}

func (a *App) handleLivez(w http.ResponseWriter, r *http.Request) {
	if r.Method != http.MethodGet {
		WriteMethodNotAllowed(w)
		return
	}
	WriteJSON(w, http.StatusOK, map[string]any{
		"status": "ok",
		"time":   time.Now().UTC().Format(time.RFC3339),
	})
}

func (a *App) handleReadyz(w http.ResponseWriter, r *http.Request) {
	if r.Method != http.MethodGet {
		WriteMethodNotAllowed(w)
		return
	}
	if a.prober == nil {
		WriteJSON(w, http.StatusOK, map[string]any{"ready": true})
		return
	}
	snap := a.prober.Snapshot()
	if !snap.Ready {
		WriteJSON(w, http.StatusServiceUnavailable, snap)
		return
	}
	WriteJSON(w, http.StatusOK, snap)
}

func (a *App) handleModels(w http.ResponseWriter, r *http.Request, principal auth.Principal) {
	if r.Method != http.MethodGet {
		if a.maybeHandleOpenAIPassthrough(w, r, principal) {
			return
		}
		WriteMethodNotAllowed(w)
		return
	}
	if a.prober != nil && !a.prober.Ready() {
		WriteOpenAIError(w, http.StatusServiceUnavailable, "provider not ready", "server_error", "provider_not_ready")
		return
	}
	modelsResp, err := a.provider.ListModels(r.Context())
	if err != nil {
		a.WriteProviderError(w, err)
		return
	}
	if len(principal.AllowedModels) > 0 {
		filtered := make([]llm.Model, 0, len(modelsResp.Data))
		for _, m := range modelsResp.Data {
			if auth.ModelAllowed(principal, m.ID) {
				filtered = append(filtered, m)
			}
		}
		modelsResp.Data = filtered
	}
	WriteJSON(w, http.StatusOK, modelsResp)
}

func (a *App) handleModelsCacheInvalidate(w http.ResponseWriter, r *http.Request, _ auth.Principal) {
	if r.Method != http.MethodPost {
		WriteMethodNotAllowed(w)
		return
	}
	invalidator, ok := a.provider.(llm.ModelCacheInvalidator)
	if !ok {
		WriteJSON(w, http.StatusOK, map[string]any{"invalidated": false, "reason": "provider has no model cache"})
		return
	}
	invalidator.InvalidateModelCache()
	WriteJSON(w, http.StatusOK, map[string]any{"invalidated": true})
}

func (a *App) handleModelByID(w http.ResponseWriter, r *http.Request, principal auth.Principal) {
	if a.prober != nil && !a.prober.Ready() {
		WriteOpenAIError(w, http.StatusServiceUnavailable, "provider not ready", "server_error", "provider_not_ready")
		return
	}
	modelID := modelIDFromPath(r.URL.Path)
	if strings.TrimSpace(modelID) == "" {
		WriteOpenAIError(w, http.StatusBadRequest, "model id is required", "invalid_request_error", "missing_model")
		return
	}
	if !auth.ModelAllowed(principal, modelID) {
		WriteForbidden(w, "model is not allowed for this API key")
		return
	}
	if r.Method == http.MethodDelete {
		a.handleOpenAIPassthroughWithPrincipal(w, r, principal)
		return
	}
	if r.Method != http.MethodGet {
		if a.maybeHandleOpenAIPassthrough(w, r, principal) {
			return
		}
		WriteMethodNotAllowed(w)
		return
	}
	model, err := a.provider.GetModel(r.Context(), modelID)
	if err != nil {
		a.WriteProviderError(w, err)
		return
	}
	WriteJSON(w, http.StatusOK, model)
}

func modelIDFromPath(path string) string {
	for _, prefix := range []string{"/api/v1/models/", "/v1/models/"} {
		if strings.HasPrefix(path, prefix) {
			return strings.TrimPrefix(path, prefix)
		}
	}
	return ""
}

func (a *App) handleChatCompletions(w http.ResponseWriter, r *http.Request, principal auth.Principal) {
	if r.Method != http.MethodPost {
		a.handleOpenAIPassthroughWithPrincipal(w, r, principal)
		return
	}
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

	var chatReq llm.ChatRequest
	if err := json.Unmarshal(requestBytes, &chatReq); err != nil {
		WriteOpenAIError(w, http.StatusBadRequest, "invalid JSON payload", "invalid_request_error", "invalid_json")
		return
	}
	if strings.TrimSpace(chatReq.Model) == "" {
		WriteOpenAIError(w, http.StatusBadRequest, "model is required", "invalid_request_error", "missing_model")
		return
	}
	if !auth.ModelAllowed(principal, chatReq.Model) {
		WriteForbidden(w, "model is not allowed for this API key")
		return
	}

	if chatReq.Stream {
		stream, err := a.provider.CreateChatCompletionStream(r.Context(), requestBytes)
		if err != nil {
			a.recordParsedUsage(r.Context(), principal, "/api/v1/chat/completions", chatReq.Model, usageParsed{Model: chatReq.Model}, statusForProviderError(err))
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
		w.WriteHeader(http.StatusOK)

		flusher, _ := w.(http.Flusher)
		scanner := bufio.NewScanner(stream)
		scanner.Buffer(make([]byte, 0, maxSSELineBytes), maxSSELineBytes)
		usage := usageParsed{Model: chatReq.Model}
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
			usage.Model = chatReq.Model
		}
		if a.usageRepo != nil {
			recordCtx, recordCancel := context.WithTimeout(context.WithoutCancel(r.Context()), 5*time.Second)
			defer recordCancel()
			a.RecordUsage(recordCtx, sqlite.UsageRecord{
				ID:               NewID("usage"),
				APIKeyID:         principal.APIKeyID,
				Endpoint:         "/api/v1/chat/completions",
				Model:            usage.Model,
				PromptTokens:     usage.PromptTokens,
				CompletionTokens: usage.CompletionTokens,
				TotalTokens:      usage.TotalTokens,
				StatusCode:       streamStatusCode,
			})
		}
		return
	}

	resp, err := a.provider.CreateChatCompletion(r.Context(), requestBytes)
	if err != nil {
		a.recordParsedUsage(r.Context(), principal, "/api/v1/chat/completions", chatReq.Model, usageParsed{Model: chatReq.Model}, statusForProviderError(err))
		a.WriteProviderError(w, err)
		return
	}
	if a.usageRepo != nil {
		u := parseUsageFromResponse(resp)
		recordCtx, recordCancel := context.WithTimeout(context.WithoutCancel(r.Context()), 5*time.Second)
		defer recordCancel()
		a.RecordUsage(recordCtx, sqlite.UsageRecord{
			ID:               NewID("usage"),
			APIKeyID:         principal.APIKeyID,
			Endpoint:         "/api/v1/chat/completions",
			Model:            u.Model,
			PromptTokens:     u.PromptTokens,
			CompletionTokens: u.CompletionTokens,
			TotalTokens:      u.TotalTokens,
			StatusCode:       http.StatusOK,
		})
	}
	w.Header().Set("Content-Type", "application/json")
	w.WriteHeader(http.StatusOK)
	_, _ = w.Write(resp)
}

func (a *App) handleEmbeddings(w http.ResponseWriter, r *http.Request, principal auth.Principal) {
	if r.Method != http.MethodPost {
		if a.maybeHandleOpenAIPassthrough(w, r, principal) {
			return
		}
		WriteMethodNotAllowed(w)
		return
	}
	if a.prober != nil && !a.prober.Ready() {
		WriteOpenAIError(w, http.StatusServiceUnavailable, "provider not ready", "server_error", "provider_not_ready")
		return
	}
	if err := a.EnforceBudget(r.Context(), principal); err != nil {
		a.WriteBudgetError(w, err)
		return
	}
	requestBytes, err := io.ReadAll(io.LimitReader(r.Body, 8*1024*1024))
	if err != nil {
		WriteOpenAIError(w, http.StatusBadRequest, "invalid request body", "invalid_request_error", "invalid_body")
		return
	}

	var payload struct {
		Model string `json:"model"`
	}
	if err := json.Unmarshal(requestBytes, &payload); err != nil {
		WriteOpenAIError(w, http.StatusBadRequest, "invalid JSON payload", "invalid_request_error", "invalid_json")
		return
	}
	if strings.TrimSpace(payload.Model) == "" {
		WriteOpenAIError(w, http.StatusBadRequest, "model is required", "invalid_request_error", "missing_model")
		return
	}
	if !auth.ModelAllowed(principal, payload.Model) {
		WriteForbidden(w, "model is not allowed for this API key")
		return
	}

	resp, err := a.provider.CreateEmbeddings(r.Context(), requestBytes)
	if err != nil {
		a.recordParsedUsage(r.Context(), principal, "/api/v1/embeddings", payload.Model, usageParsed{Model: payload.Model}, statusForProviderError(err))
		a.WriteProviderError(w, err)
		return
	}
	if a.usageRepo != nil {
		u := parseUsageFromResponse(resp)
		if u.TotalTokens == 0 {
			u.TotalTokens = u.PromptTokens + u.CompletionTokens
		}
		recordCtx, recordCancel := context.WithTimeout(context.WithoutCancel(r.Context()), 5*time.Second)
		defer recordCancel()
		a.RecordUsage(recordCtx, sqlite.UsageRecord{
			ID:               NewID("usage"),
			APIKeyID:         principal.APIKeyID,
			Endpoint:         "/api/v1/embeddings",
			Model:            payload.Model,
			PromptTokens:     u.PromptTokens,
			CompletionTokens: u.CompletionTokens,
			TotalTokens:      u.TotalTokens,
			CostCents:        0,
			StatusCode:       http.StatusOK,
		})
	}
	w.Header().Set("Content-Type", "application/json")
	w.WriteHeader(http.StatusOK)
	_, _ = w.Write(resp)
}

func (a *App) handleAttestationStatus(w http.ResponseWriter, r *http.Request, _ auth.Principal) {
	if r.Method != http.MethodGet {
		WriteMethodNotAllowed(w)
		return
	}
	attested, ok := a.provider.(llm.AttestationProvider)
	if !ok {
		WriteJSON(w, http.StatusOK, map[string]any{
			"enabled":  false,
			"provider": "non-attested",
		})
		return
	}
	WriteJSON(w, http.StatusOK, attested.AttestationStatus())
}

func (a *App) handleAttestationVerify(w http.ResponseWriter, r *http.Request, _ auth.Principal) {
	if r.Method != http.MethodPost {
		WriteMethodNotAllowed(w)
		return
	}
	attested, ok := a.provider.(llm.AttestationProvider)
	if !ok {
		WriteOpenAIError(w, http.StatusBadRequest, "configured provider does not support attestation verification", "invalid_request_error", "attestation_unsupported")
		return
	}
	status, err := attested.VerifyAttestation(r.Context())
	if err != nil {
		log.Printf("attestation verification failed: %v", err)
		WriteOpenAIError(w, http.StatusBadGateway, "attestation verification failed", "server_error", "attestation_verify_failed")
		return
	}
	WriteJSON(w, http.StatusOK, status)
}

func (a *App) WriteProviderError(w http.ResponseWriter, err error) {
	var upstreamErr *llm.UpstreamError
	if errors.As(err, &upstreamErr) {
		if len(upstreamErr.Body) > 0 {
			var existing map[string]any
			if json.Unmarshal(upstreamErr.Body, &existing) == nil {
				if _, ok := existing["error"]; ok {
					w.Header().Set("Content-Type", "application/json")
					w.WriteHeader(upstreamErr.StatusCode)
					_, _ = w.Write(upstreamErr.Body)
					return
				}
			}
		}
		WriteOpenAIError(w, upstreamErr.StatusCode, "upstream provider error", "server_error", "upstream_error")
		return
	}
	log.Printf("upstream provider error: %s", redactForLog(err.Error()))
	WriteOpenAIError(w, http.StatusBadGateway, "upstream provider unavailable", "server_error", "upstream_unavailable")
}
