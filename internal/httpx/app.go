package httpx

import (
	"encoding/json"
	"errors"
	"fmt"
	"log"
	"net/http"
	"regexp"
	"strings"
	"time"

	"enclava-go/internal/auth"
	"enclava-go/internal/billing"
	"enclava-go/internal/chatbot"
	"enclava-go/internal/connectors"
	"enclava-go/internal/extract"
	"enclava-go/internal/inference"
	"enclava-go/internal/llm"
	"enclava-go/internal/prompts"
	"enclava-go/internal/rag"
	"enclava-go/internal/store/postgres"
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
	executor     *inference.Service
	bindings     *postgres.ResponseBindingsRepo
	sourceRepo   *postgres.SourceRepo
	artifactRepo *postgres.ArtifactRepo
	rl           *RateLimiter
	gate         *inferenceGate
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
	executor *inference.Service,
	bindings *postgres.ResponseBindingsRepo,
	gate GateConfig,
	sourceRepo *postgres.SourceRepo,
	artifactRepo *postgres.ArtifactRepo,
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
		billingSvc:   billingSvc,
		billingRepo:  billingRepo,
		endpoints:    endpoints,
		apiKeyAlgo:   apiKeyAlgo,
		apiKeyPepper: apiKeyPepper,
		apiKeyPrefix: apiKeyPrefix,
		corsOrigins:  corsOrigins,
		sessionAuth:  sessionAuth,
		maxUploadMB:  maxUploadMB,
		bindings:     bindings,
		trustProxy:   trustProxy,
		executor:     executor,
		rl:           NewRateLimiter(),
		gate:         newInferenceGate(gate),
		sourceRepo:   sourceRepo,
		artifactRepo: artifactRepo,
	}

	a.mux = http.NewServeMux()
	a.registerBaseRoutes()
	return a
}

func (a *App) Handler() http.Handler {
	var h http.Handler = a.mux
	h = a.inferenceAdmissionMiddleware(h)
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

	// Paid paths not backed by the shared executor remain unmounted; only
	// owned metadata and authentication surfaces are registered here.
	a.mux.HandleFunc("/api/v1/providers", a.authenticated("pricing.manage", a.handleProviders))
	a.mux.HandleFunc("/api/v1/providers/", a.authenticated("pricing.manage", a.handleProviderByID))
	a.mux.HandleFunc("/api/v1/processing-restrictions", a.authenticated("pricing.manage", a.handleProcessingRestrictions))
	a.mux.HandleFunc("/api/v1/rag/collections", a.authenticated("rag", a.handleRAGCollections))
	a.mux.HandleFunc("/api/v1/rag/collections/", a.authenticated("rag", a.handleRAGCollectionByID))
	a.mux.HandleFunc("/api/v1/rag/documents/", a.authenticated("rag", a.handleRAGDocumentByID))
	a.mux.HandleFunc("/api/v1/prompt-templates", a.authenticated("chatbot", a.handlePromptTemplates))
	a.mux.HandleFunc("/api/v1/prompt-templates/", a.authenticated("chatbot", a.handlePromptTemplateByID))
	a.mux.HandleFunc("/api/v1/chatbots", a.authenticated("chatbot", a.handleChatbots))
	a.mux.HandleFunc("/api/v1/chatbots/", a.authenticated("chatbot", a.handleChatbotByID))

	a.mux.HandleFunc("/api/v1/connectors/sources", a.authenticated("connectors.manage", a.handleConnectorSources))
	// 01-29: the selected public OpenAI core API — model-alias metadata plus
	// chat/embedding dispatch — runs only through the shared executor. There
	// is no catch-all: unselected /v1 products and legacy aliases 404.
	a.mux.HandleFunc("/v1/models", a.authenticated("models.list", a.handleModels))
	a.mux.HandleFunc("/v1/models/", a.authenticated("models.list", a.handleModelByID))
	a.mux.HandleFunc("/v1/chat/completions", a.authenticated("chat.completions", a.handleChatCompletions))
	a.mux.HandleFunc("/v1/embeddings", a.authenticated("embeddings.create", a.handleEmbeddings))
	// 01-30: the selected Responses lifecycle — create/stream, retrieve,
	// delete, cancel, input items, input-token count and compact — runs
	// through the shared executor over owner/endpoint-bound public IDs.
	a.mux.HandleFunc("/v1/responses", a.authenticated("responses", a.handleResponses))
	a.mux.HandleFunc("/v1/responses/", a.authenticated("responses", a.handleResponses))

	a.mux.HandleFunc("/api/v1/connectors/sources/", a.authenticated("connectors.manage", a.handleConnectorSourceByID))
	a.mux.HandleFunc("/api/v1/connectors/sync-jobs", a.authenticated("connectors.manage", a.handleConnectorSyncJobs))
	a.mux.HandleFunc("/api/v1/connectors/sync-jobs/", a.authenticated("connectors.manage", a.handleConnectorSyncJobByID))

	a.mux.HandleFunc("/api/v1/admin/api-keys", a.authenticated("platform.admin", a.handleAdminAPIKeys))
	a.mux.HandleFunc("/api/v1/admin/api-keys/", a.authenticated("platform.admin", a.handleAdminAPIKeyByID))
	a.mux.HandleFunc("/api/v1/admin/usage/summary", a.authenticated("platform.admin", a.handleAdminUsageSummary))
	a.mux.HandleFunc("/api/v1/admin/pricing", a.authenticated("pricing.manage", a.handleAdminPricing))
	a.mux.HandleFunc("/api/v1/admin/billing-audit", a.authenticated("platform.admin", a.handleAdminBillingAudit))

	// 01-24 identity administration: tenant-scoped user/role/service-account
	// and session/link management over the shared auth services. Mutations on
	// the /me and /link paths run through the same session/CSRF rules as every
	// other authenticated route.
	a.mux.HandleFunc("/api/v1/identities/users", a.authenticated("platform.admin", a.handleIdentityUsers))
	a.mux.HandleFunc("/api/v1/identities/users/", a.authenticated("platform.admin", a.handleIdentityUserByID))
	a.mux.HandleFunc("/api/v1/identities/roles", a.authenticated("platform.admin", a.handleIdentityRoles))
	a.mux.HandleFunc("/api/v1/identities/roles/", a.authenticated("platform.admin", a.handleIdentityRoleByID))
	a.mux.HandleFunc("/api/v1/identities/service-accounts", a.authenticated("platform.admin", a.handleIdentityServiceAccounts))
	a.mux.HandleFunc("/api/v1/identities/service-accounts/", a.authenticated("platform.admin", a.handleIdentityServiceAccountByID))
	a.mux.HandleFunc("/api/v1/identities/me", a.authenticated("", a.handleIdentityMe))
	a.mux.HandleFunc("/api/v1/identities/me/", a.authenticated("", a.handleIdentityMe))
	a.mux.HandleFunc("/api/v1/identities/link", a.authenticated("", a.handleIdentityLink))

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
		if scope != "" && !auth.HasScope(principal, scope) {
			WriteForbidden(w, fmt.Sprintf("missing required scope: %s", scope))
			return
		}
		a.enforceAllowlists(w, r, principal, next)
	}
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

func modelIDFromPath(path string) string {
	return strings.TrimPrefix(path, "/v1/models/")
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

var upstreamIDPattern = regexp.MustCompile(`(?i)\b(?:resp|msg|item|chatcmpl|cmpl|call)[_-][A-Za-z0-9_-]{4,}`)

// WriteProviderError surfaces the upstream failure envelope without leaking
// upstream resource IDs: only the standard OpenAI error fields pass through,
// and ID-shaped tokens inside the message are redacted (the public-ID
// rewriting in responses.go does not run on this error path).
func (a *App) WriteProviderError(w http.ResponseWriter, err error) {
	var upstreamErr *llm.UpstreamError
	if errors.As(err, &upstreamErr) {
		if len(upstreamErr.Body) > 0 {
			var existing struct {
				Error struct {
					Message string `json:"message"`
					Type    string `json:"type"`
					Code    any    `json:"code"`
					Param   any    `json:"param"`
				} `json:"error"`
			}
			if json.Unmarshal(upstreamErr.Body, &existing) == nil && existing.Error.Message != "" {
				payload := map[string]any{
					"error": map[string]any{
						"message": upstreamIDPattern.ReplaceAllString(existing.Error.Message, "[redacted]"),
						"type":    existing.Error.Type,
						"code":    existing.Error.Code,
						"param":   existing.Error.Param,
					},
				}
				w.Header().Set("Content-Type", "application/json")
				w.WriteHeader(upstreamErr.StatusCode)
				_ = json.NewEncoder(w).Encode(payload)
				return
			}
		}
		WriteOpenAIError(w, upstreamErr.StatusCode, "upstream provider error", "server_error", "upstream_error")
		return
	}
	log.Printf("upstream provider error: %s", redactForLog(err.Error()))
	WriteOpenAIError(w, http.StatusBadGateway, "upstream provider unavailable", "server_error", "upstream_unavailable")
}
