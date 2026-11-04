package httpx

import (
	"context"
	"encoding/json"
	"errors"
	"fmt"
	"log"
	"net/http"
	"os"
	"regexp"
	"strconv"
	"strings"
	"sync"
	"time"

	"enclava-go/internal/auth"
	"enclava-go/internal/billing"
	"enclava-go/internal/chatbot"
	"enclava-go/internal/connectors"
	"enclava-go/internal/credentials"
	"enclava-go/internal/extract"
	"enclava-go/internal/inference"
	"enclava-go/internal/jobs"
	"enclava-go/internal/llm"
	"enclava-go/internal/mcp"
	"enclava-go/internal/prompts"
	"enclava-go/internal/rag"
	"enclava-go/internal/store/postgres"
)

type App struct {
	authSvc             *auth.Service
	provider            llm.Provider
	providerID          string
	prober              *llm.Prober
	identity            *postgres.IdentityRepo
	extractSvc          *extract.Service
	ragSvc              *rag.Service
	promptSvc           *prompts.Service
	chatbotSvc          *chatbot.Service
	connectorSvc        *connectors.Service
	credentials         *credentials.Service
	billingSvc          *billing.Service
	billingRepo         *postgres.BillingRepo
	endpoints           *postgres.EndpointsRepo
	apiKeyAlgo          string
	apiKeyPepper        string
	apiKeyPrefix        string
	corsOrigins         []string
	sessionAuth         *SessionAuth
	maxUploadMB         int64
	trustProxy          bool
	executor            *inference.Service
	bindings            *postgres.ResponseBindingsRepo
	sourceRepo          *postgres.SourceRepo
	artifactRepo        *postgres.ArtifactRepo
	mcpRepo             *postgres.MCPToolsRepo
	mcpTransport        *mcp.StreamableHTTP
	jobsSvc             *jobs.Service
	vectorStore         *dependencyProbe
	securityAudit       SecurityAuditRecorder
	metricsToken        string
	metricsAuthDisabled bool
	metricsJobs         jobCounter
	metricsAdmission    *inference.Admission
	rl                  *RateLimiter
	gate                *inferenceGate
	mux                 *http.ServeMux
}

// ConfigureActivity wires the shared durable-job service after construction
// (the ConfigureMCP pattern — NewApp's signature stays untouched). main.go
// calls it next to app.ConfigureMCP. Producers degrade to the non-job items
// while it is unset.
func (a *App) ConfigureActivity(jobSvc *jobs.Service) { a.jobsSvc = jobSvc }

// dependencyProbe is a bounded readiness check for one optional dependency:
// the result is cached for refreshAfter so a dependency outage never turns
// readiness probes into per-request upstream hangs (single-flight).
type dependencyProbe struct {
	name         string
	timeout      time.Duration
	refreshAfter time.Duration
	probe        func(ctx context.Context) error
	mu           sync.Mutex
	last         time.Time
	lastErr      error
}

const (
	depProbeTimeout      = 2 * time.Second
	depProbeRefreshAfter = 10 * time.Second
)

func (p *dependencyProbe) state(ctx context.Context) map[string]any {
	p.mu.Lock()
	defer p.mu.Unlock()
	if time.Since(p.last) >= p.refreshAfter {
		probeCtx, cancel := context.WithTimeout(ctx, p.timeout)
		p.lastErr = p.probe(probeCtx)
		cancel()
		p.last = time.Now()
	}
	st := map[string]any{"ready": p.lastErr == nil, "checked_at": p.last.UTC().Format(time.RFC3339)}
	if p.lastErr != nil {
		st["error"] = "vector store unavailable"
	}
	return st
}

// ConfigureVectorStoreProbe wires the knowledge-readiness dependency after
// construction (the ConfigureMCP pattern — NewApp's signature stays
// untouched). /readyz reports it under dependencies.vector_store and
// /readyz/knowledge answers it; unset means the dependency is not reported.
func (a *App) ConfigureVectorStoreProbe(probe func(ctx context.Context) error) {
	if probe == nil {
		return
	}
	a.vectorStore = &dependencyProbe{
		name: "vector_store", timeout: depProbeTimeout,
		refreshAfter: depProbeRefreshAfter, probe: probe,
	}
}

// ProbeHTTP checks that the dependency returns a successful HTTP response.
func ProbeHTTP(url string) func(ctx context.Context) error {
	client := &http.Client{Timeout: depProbeTimeout}
	return func(ctx context.Context) error {
		req, err := http.NewRequestWithContext(ctx, http.MethodGet, url, nil)
		if err != nil {
			return err
		}
		res, err := client.Do(req)
		if err != nil {
			return err
		}
		_ = res.Body.Close()
		if res.StatusCode < 200 || res.StatusCode >= 300 {
			return fmt.Errorf("dependency returned HTTP %d", res.StatusCode)
		}
		return nil
	}
}

// handleAPINotFound answers any /api/* and /v1/* path that matched no
// mounted route with the shared JSON error envelope — API clients never see
// the plain-text 404 (dashboard and other routes keep their HTML 404).
func (a *App) handleAPINotFound(w http.ResponseWriter, r *http.Request) {
	WriteOpenAIError(w, http.StatusNotFound, "route not found", "invalid_request_error", "not_found")
}

const (
	preAuthRateLimitPerMinute = 120
	preAuthRateLimitPerHour   = 1000
	preAuthRateLimitPerDay    = 20000

	maxSSELineBytes  = 256 * 1024
	maxSSEEventBytes = 4 * 1024 * 1024
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
	a.mux.HandleFunc("/readyz/knowledge", a.handleReadyzKnowledge)
	a.mux.HandleFunc("/metrics", a.handleMetrics)

	// Paid paths not backed by the shared executor remain unmounted; only
	// owned metadata and authentication surfaces are registered here.
	a.mux.HandleFunc("/api/v1/providers", a.authenticated("pricing.manage", a.handleProviders))
	// Registered before the /providers/ subtree so discovery wins the exact match.
	a.mux.HandleFunc("/api/v1/providers/discover", a.authenticated("pricing.manage", a.handleProviderDiscover))
	a.mux.HandleFunc("/api/v1/providers/", a.authenticated("pricing.manage", a.handleProviderByID))
	a.mux.HandleFunc("/api/v1/models/settings", a.authenticated("pricing.manage", a.handleModelSettings))
	a.mux.HandleFunc("/api/v1/models/settings/", a.authenticated("pricing.manage", a.handleModelSettingByID))
	a.mux.HandleFunc("/api/v1/processing-restrictions", a.authenticated("pricing.manage", a.handleProcessingRestrictions))
	a.mux.HandleFunc("/api/v1/rag/collections", a.authenticated("rag", a.handleRAGCollections))
	// 08-08: bounded multi-collection search page — fused results plus
	// post-authorization source-kind facets over exactly the readable set.
	a.mux.HandleFunc("/api/v1/rag/search", a.authenticated("rag", a.handleRAGSearchReadable))
	a.mux.HandleFunc("/api/v1/knowledge/imports", a.authenticated("rag", a.handleRAGImports))
	a.mux.HandleFunc("/api/v1/rag/collections/", a.authenticated("rag", a.handleRAGCollectionByID))
	a.mux.HandleFunc("/api/v1/rag/documents/", a.authenticated("rag", a.handleRAGDocumentByID))
	a.mux.HandleFunc("/api/v1/rag/evidence/", a.authenticated("rag", a.handleRAGEvidenceByID))
	a.mux.HandleFunc("/api/v1/prompt-templates", a.authenticated("chatbot", a.handlePromptTemplates))
	a.mux.HandleFunc("/api/v1/prompt-templates/", a.authenticated("chatbot", a.handlePromptTemplateByID))
	a.mux.HandleFunc("/api/v1/chatbots", a.authenticated("chatbot", a.handleChatbots))
	a.mux.HandleFunc("/api/v1/chatbots/", a.authenticated("chatbot", a.handleChatbotByID))
	a.mux.HandleFunc("/api/v1/agents", a.authenticated("chatbot", a.handleChatbots))
	a.mux.HandleFunc("/api/v1/chatbot/external/", a.authenticated("chatbot", a.handleExternalChatbot))
	a.mux.HandleFunc("/api/v1/agents/", a.authenticated("chatbot", a.handleChatbotByID))
	a.mux.HandleFunc("/api/v1/agent-runs", a.authenticated("chatbot", a.handleAgentRuns))
	a.mux.HandleFunc("/api/v1/agent-runs/", a.authenticated("chatbot", a.handleAgentRunByID))
	a.mux.HandleFunc("/api/v1/conversations", a.authenticated("chatbot", a.handleConversations))
	a.mux.HandleFunc("/api/v1/conversations/", a.authenticated("chatbot", a.handleConversationByID))

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
	a.mux.HandleFunc("/api/v1/connectors/credentials", a.authenticated("connectors.manage", a.handleConnectorCredentials))
	a.mux.HandleFunc("/api/v1/connectors/credentials/", a.authenticated("connectors.manage", a.handleConnectorCredentialByID))
	a.mux.HandleFunc("/api/v1/connectors/oauth-apps/", a.authenticated("connectors.manage", a.handleConnectorOAuthApp))

	a.mux.HandleFunc("/api/v1/admin/api-keys", a.authenticated("platform.admin", a.handleAdminAPIKeys))
	a.mux.HandleFunc("/api/v1/admin/api-keys/", a.authenticated("platform.admin", a.handleAdminAPIKeyByID))
	a.mux.HandleFunc("/api/v1/admin/usage/summary", a.authenticated("platform.admin", a.handleAdminUsageSummary))
	a.mux.HandleFunc("/api/v1/admin/pricing", a.authenticated("pricing.manage", a.handleAdminPricing))
	a.mux.HandleFunc("/api/v1/admin/billing-audit", a.authenticated("platform.admin", a.handleAdminBillingAudit))
	a.mux.HandleFunc("/api/v1/admin/security-audit", a.authenticated("platform.admin", a.handleSecurityAudit))
	// 08-07: the narrow budget write API — create, all-subject list and
	// limit-only PATCH. The legacy is_unlimited/budget_* fields on the
	// api-key route stay no-ops; these routes are the real budget surface.
	a.mux.HandleFunc("/api/v1/admin/budgets", a.authenticated("platform.admin", a.handleAdminBudgets))
	a.mux.HandleFunc("/api/v1/admin/budgets/", a.authenticated("platform.admin", a.handleAdminBudgetByID))
	// Unknown-reservation reconciliation: list outstanding canonical
	// reservations and settle unknown holds with reconciled usage.
	a.mux.HandleFunc("/api/v1/admin/reservations", a.authenticated("platform.admin", a.handleAdminReservations))
	a.mux.HandleFunc("/api/v1/admin/reservations/", a.authenticated("platform.admin", a.handleAdminReservationByID))

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

	// JSON 404 catch-alls for the API surfaces — registered last so every
	// concrete route above (and the module-registered /api/v1/* routes) wins.
	a.mux.HandleFunc("/api", a.handleAPINotFound)
	a.mux.HandleFunc("/api/", a.handleAPINotFound)
	a.mux.HandleFunc("/v1", a.handleAPINotFound)
	a.mux.HandleFunc("/v1/", a.handleAPINotFound)

	a.registerDashboardRoutes()
}

func (a *App) handleRoot(w http.ResponseWriter, r *http.Request) {
	if r.URL.Path != "/" {
		if strings.HasPrefix(r.URL.Path, "/dashboard/") {
			w.Header().Set("Content-Type", "text/html; charset=utf-8")
			w.WriteHeader(http.StatusNotFound)
			_, _ = w.Write([]byte("<!doctype html><title>Not found</title><h1>Not found</h1>"))
		} else {
			http.NotFound(w, r)
		}
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
				{limit: envRateLimit("ENCLAVA_PREAUTH_RATE_LIMIT_PER_MINUTE", preAuthRateLimitPerMinute), window: time.Minute},
				{limit: envRateLimit("ENCLAVA_PREAUTH_RATE_LIMIT_PER_HOUR", preAuthRateLimitPerHour), window: time.Hour},
				{limit: envRateLimit("ENCLAVA_PREAUTH_RATE_LIMIT_PER_DAY", preAuthRateLimitPerDay), window: 24 * time.Hour},
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
			// token (header or form field — never the cookie alone) and be a
			// same-origin request.
			if !sameOriginMutation(r, a.trustProxy) ||
				a.authSvc.ValidateCSRF(r.Context(), principal.TenantID, principal.SessionID, csrfFromRequest(r)) != nil {
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
	// Admission first: a request denied by one window consumes no budget in
	// the others. Each window gets its own bucket so one request counts once
	// per window and windows keep independent reset timestamps.
	for _, check := range checks {
		if check.limit <= 0 {
			continue
		}
		if dec := a.rl.CheckKey(rateLimitBucketKey(key, check.window), check.limit, check.window, now); !dec.Allowed {
			writeRateLimited(w, dec)
			return false
		}
	}
	for _, check := range checks {
		if check.limit <= 0 {
			continue
		}
		a.rl.AllowKey(rateLimitBucketKey(key, check.window), check.limit, check.window, now)
	}
	return true
}

// rateLimitBucketKey namespaces the caller's limiter key by window size so a
// single identity holds one counter per window (IDOP-I01).
func rateLimitBucketKey(key string, window time.Duration) string {
	return key + ":" + itoa(int64(window/time.Second))
}

// envRateLimit reads an operator-tunable pre-auth budget from the
// environment, falling back to the compiled default. Zero disables the
// check (enforceRateLimits skips limits <= 0).
func envRateLimit(name string, def int64) int64 {
	if v := strings.TrimSpace(os.Getenv(name)); v != "" {
		if n, err := strconv.ParseInt(v, 10, 64); err == nil && n >= 0 {
			return n
		}
	}
	return def
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
	deps := map[string]any{
		"inference":    map[string]any{"ready": true, "configured": false},
		"vector_store": map[string]any{"ready": false, "configured": false},
	}
	if a.vectorStore != nil {
		deps[a.vectorStore.name] = a.vectorStore.state(r.Context())
	}
	if a.prober == nil {
		WriteJSON(w, http.StatusOK, map[string]any{"ready": true, "dependencies": deps})
		return
	}
	snap := a.prober.Snapshot()
	deps["inference"] = snap
	payload := struct {
		llm.ReadinessSnapshot
		Dependencies map[string]any `json:"dependencies"`
	}{ReadinessSnapshot: snap, Dependencies: deps}
	if !snap.Ready {
		WriteJSON(w, http.StatusServiceUnavailable, payload)
		return
	}
	WriteJSON(w, http.StatusOK, payload)
}

// handleReadyzKnowledge answers only the knowledge dependency: 200 while the
// configured vector store is reachable, 503 when it is not (or was never
// configured). /readyz stays inference-only — an inference-only deployment
// must not flap on a knowledge outage.
func (a *App) handleReadyzKnowledge(w http.ResponseWriter, r *http.Request) {
	if r.Method != http.MethodGet {
		WriteMethodNotAllowed(w)
		return
	}
	if a.vectorStore == nil {
		WriteJSON(w, http.StatusServiceUnavailable, map[string]any{
			"ready": false, "dependencies": map[string]any{
				"vector_store": map[string]any{"ready": false, "error": "vector store probe not configured"},
			},
		})
		return
	}
	state := a.vectorStore.state(r.Context())
	ready, _ := state["ready"].(bool)
	payload := map[string]any{"ready": ready, "dependencies": map[string]any{"vector_store": state}}
	if !ready {
		WriteJSON(w, http.StatusServiceUnavailable, payload)
		return
	}
	WriteJSON(w, http.StatusOK, payload)
}

func modelIDFromPath(path string) string {
	return strings.TrimPrefix(path, "/v1/models/")
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
