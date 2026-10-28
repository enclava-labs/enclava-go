package httpx

import (
	"context"
	"embed"
	"fmt"
	"html/template"
	"io/fs"
	"log"
	"net/http"
	"strings"
	"time"

	"enclava-go/internal/auth"
)

//go:embed dashboard/templates/*.tmpl
var dashboardTemplateFS embed.FS

//go:embed dashboard/static/*
var dashboardStaticFS embed.FS

var dashboardTemplates = template.Must(template.New("").Funcs(template.FuncMap{
	"formatCurrencyMicros": formatCurrencyMicros,
}).ParseFS(dashboardTemplateFS, "dashboard/templates/*.tmpl"))

// DashboardTemplates exposes the embedded dashboard template set for the
// session auth handlers constructed in main.
func DashboardTemplates() *template.Template {
	return dashboardTemplates
}

var dashboardStaticFiles fs.FS = func() fs.FS {
	files, err := fs.Sub(dashboardStaticFS, "dashboard/static")
	if err != nil {
		panic(err)
	}
	return files
}()

// dashboardDefaultScopes is the hidden form field's least-privilege default:
// use/read scopes only. platform.admin and every *.manage grant are absent —
// the create handler still enforces "scopes ⊆ caller's scopes" on whatever
// arrives, so a crafted form cannot mint beyond the operator's own authority.
var dashboardDefaultScopes = []string{
	"models.list",
	"chat.completions",
	"embeddings.create",
	"responses",
	"openai.proxy",
	"extract",
	"rag",
	"chatbot",
}

type dashboardKeysData struct {
	Keys       []auth.APIKeyRecord
	Message    string
	CreatedKey string
}

type dashboardLoginData struct {
	Email            string
	Error            string
	ReturnTo         string
	Providers        []auth.OIDCProvider
	LinkToken        string
	LinkProviderName string
	LinkEmail        string
}

type dashboardPageCommon struct {
	ActivePage        string
	PageTitle         string
	AuthMode          string
	UserEmail         string
	DefaultScopes     string
	KnowledgeReadable bool
	ConnectorsManage  bool
	PlatformAdmin     bool
	scopes            map[string]struct{}
}

// HasScope mirrors auth.HasScope for templates so nav items stay in step
// with the route gates — a link is hidden when the session principal could
// only ever receive a 403 from the page behind it.
func (d dashboardPageCommon) HasScope(scope string) bool {
	if _, ok := d.scopes["*"]; ok {
		return true
	}
	_, ok := d.scopes[scope]
	return ok
}

// dashboardIdentityData feeds page_identity: the signed-in account plus the
// OIDC providers an employee can use for sign-in/linking.
type dashboardIdentityData struct {
	dashboardPageCommon
	Providers []auth.OIDCProvider
}

// registerLegacyDashboardRoutes registers the pre-phase-7 dashboard partials
// and pages only — static/auth routes live in dashboard_shell.go now. The
// /dashboard and /dashboard/ Home registrations plus the old stats,
// chatbots, prompt-templates and rag surfaces died with the phase-10 Home
// cutover (dashboard_home.go owns /dashboard and the subtree 404 catch-all
// now); the remaining legacy pages stay until their page phases replace
// them (deletions ride WITH the replacement).
func (a *App) registerLegacyDashboardRoutes() {
	// Dashboard data endpoints carry the same scope as the equivalent
	// /api/v1 route — session sign-in alone never widens what a member may
	// read or change. Mutating POST branches keep an additional *.manage
	// check inside the handlers where the API splits read and write scopes.
	a.mux.HandleFunc("/dashboard/partials/keys", a.sessionAuth.RequireSessionScope(auth.ScopePlatformAdmin, a.handleDashboardAPIKeys))
	a.mux.HandleFunc("/dashboard/partials/providers", a.sessionAuth.RequireSessionScope("pricing.manage", a.handleDashboardProviders))
	a.mux.HandleFunc("/dashboard/partials/pricing", a.sessionAuth.RequireSessionScope("pricing.manage", a.handleDashboardPricing))
	a.mux.HandleFunc("/dashboard/partials/mcp", a.sessionAuth.RequireSessionScope(auth.ScopePlatformAdmin, a.handleDashboardMCP))
	a.mux.HandleFunc("/dashboard/partials/connectors", a.sessionAuth.RequireSessionScope("connectors.manage", a.handleDashboardConnectors))
	a.mux.HandleFunc("/dashboard/keys", a.sessionAuth.RequireSessionScope(auth.ScopePlatformAdmin, a.handleDashboardPageKeys))
	a.mux.HandleFunc("/dashboard/providers", a.sessionAuth.RequireSessionScope("pricing.manage", a.handleDashboardPageProviders))
	a.mux.HandleFunc("/dashboard/pricing", a.sessionAuth.RequireSessionScope("pricing.manage", a.handleDashboardPagePricing))
	a.mux.HandleFunc("/dashboard/connectors", a.sessionAuth.RequireSessionScope("connectors.manage", a.handleDashboardPageConnectors))
	a.mux.HandleFunc("/dashboard/mcp", a.sessionAuth.RequireSessionScope(auth.ScopePlatformAdmin, a.handleDashboardPageMCP))
	a.mux.HandleFunc("/dashboard/identity", a.sessionAuth.RequireSession(a.handleDashboardPageIdentity))
}

func (a *App) dashboardPageData(r *http.Request, activePage, pageTitle string) dashboardPageCommon {
	data := dashboardPageCommon{
		ActivePage:    activePage,
		PageTitle:     pageTitle,
		AuthMode:      "session",
		DefaultScopes: strings.Join(dashboardDefaultScopes, ", "),
	}
	if principal, ok := auth.PrincipalFromContext(r.Context()); ok {
		data.scopes = principal.Scopes
		data.KnowledgeReadable = auth.HasScope(principal, "rag")
		data.ConnectorsManage = auth.HasScope(principal, "connectors.manage")
		data.PlatformAdmin = auth.HasScope(principal, "platform.admin")
	}
	// The principal is session-verified; the email renders in the account
	// menu so the operator sees which employee account holds the session.
	if principal, ok := auth.PrincipalFromContext(r.Context()); ok && principal.UserID != "" && a.identity != nil {
		if user, err := a.identity.GetUser(r.Context(), principal.TenantID, principal.UserID); err == nil {
			data.UserEmail = user.Email
		}
	}
	return data
}

// handleDashboardPageIdentity serves the account/identity page. The page's
// sections load through the /api/v1/identities session+CSRF APIs; only the
// provider list (for link/sign-in buttons) is rendered server-side.
func (a *App) handleDashboardPageIdentity(w http.ResponseWriter, r *http.Request) {
	if r.Method != http.MethodGet {
		WriteMethodNotAllowed(w)
		return
	}
	data := dashboardIdentityData{
		dashboardPageCommon: a.dashboardPageData(r, "identity", "Identity"),
	}
	if a.sessionAuth != nil && a.sessionAuth.oidc != nil {
		if principal, ok := auth.PrincipalFromContext(r.Context()); ok {
			if providers, err := a.sessionAuth.oidc.EnabledProviders(r.Context(), principal.TenantID); err == nil {
				data.Providers = providers
			} else {
				log.Printf("identity page providers: %v", err)
			}
		}
	}
	w.Header().Set("Content-Type", "text/html; charset=utf-8")
	if err := dashboardTemplates.ExecuteTemplate(w, "page-identity", data); err != nil {
		log.Printf("failed to render identity page: %v", err)
		WriteOpenAIError(w, http.StatusInternalServerError, "internal server error", "server_error", "dashboard_identity_render_failed")
	}
}

func (a *App) handleDashboardPageKeys(w http.ResponseWriter, r *http.Request) {
	if r.Method != http.MethodGet {
		WriteMethodNotAllowed(w)
		return
	}
	w.Header().Set("Content-Type", "text/html; charset=utf-8")
	if err := dashboardTemplates.ExecuteTemplate(w, "page-keys", a.dashboardPageData(r, "keys", "API Keys")); err != nil {
		log.Printf("failed to render dashboard keys page: %v", err)
		WriteOpenAIError(w, http.StatusInternalServerError, "internal server error", "server_error", "dashboard_keys_page_render_failed")
	}
}

// dashboardTenantID resolves the tenant for dashboard key/usage reads from
// the request's session-verified principal — RequireSession has already
// placed it on the context, so an absent principal is a truthful 401 rather
// than a fabricated tenant.
func (a *App) dashboardTenantID(w http.ResponseWriter, r *http.Request) (string, bool) {
	if p, ok := auth.PrincipalFromContext(r.Context()); ok && p.TenantID != "" {
		return p.TenantID, true
	}
	WriteUnauthorized(w, "authenticated principal required")
	return "", false
}

func (a *App) handleDashboardAPIKeys(w http.ResponseWriter, r *http.Request) {
	setNoStoreHeaders(w)
	if a.identity == nil {
		WriteOpenAIError(w, http.StatusNotImplemented, "API key store not configured", "server_error", "not_configured")
		return
	}
	tenantID, ok := a.dashboardTenantID(w, r)
	if !ok {
		return
	}
	switch r.Method {
	case http.MethodGet:
		a.renderDashboardKeys(w, r.Context(), tenantID, "")
	case http.MethodPost:
		if err := r.ParseForm(); err != nil {
			WriteOpenAIError(w, http.StatusBadRequest, "invalid form data", "invalid_request_error", "invalid_form")
			return
		}
		action := strings.ToLower(strings.TrimSpace(r.FormValue("action")))
		switch action {
		case "", "create":
			a.createDashboardAPIKey(w, r, tenantID)
		case "toggle":
			a.toggleDashboardAPIKey(w, r, tenantID)
		case "delete":
			a.deleteDashboardAPIKey(w, r, tenantID)
		case "regenerate":
			a.regenerateDashboardAPIKey(w, r, tenantID)
		default:
			WriteOpenAIError(w, http.StatusBadRequest, "invalid action", "invalid_request_error", "invalid_action")
		}
	default:
		WriteMethodNotAllowed(w)
	}
}

func (a *App) createDashboardAPIKey(w http.ResponseWriter, r *http.Request, tenantID string) {
	name := strings.TrimSpace(r.FormValue("name"))
	if name == "" {
		a.renderDashboardKeys(w, r.Context(), tenantID, "Name is required")
		return
	}
	scopes := parseDashboardScopes(r.FormValue("scopes"))
	principal, _ := auth.PrincipalFromContext(r.Context())
	req := adminAPIKeyCreateRequest{
		Name:     name,
		Scopes:   scopes,
		IsActive: ptrBool(true),
	}
	if err := sanitizeAdminAPIKeyRequest(&req, true); err != nil {
		a.renderDashboardKeys(w, r.Context(), tenantID, err.Error())
		return
	}

	key, err := a.apiKeyRecordFromAdminRequest(r.Context(), principal, req)
	if err != nil {
		a.renderDashboardKeys(w, r.Context(), tenantID, err.Error())
		return
	}
	// Same rule as the admin API route: a key's scopes must be a subset of
	// the operator's own — the dashboard form cannot mint beyond them.
	if !principalCoversScopes(principal, key.Scopes) {
		a.renderDashboardKeys(w, r.Context(), tenantID, "Key scopes must be a subset of your scopes")
		return
	}
	// The form carries no grant fields, so the built record requests
	// mode:all on every resource — only a caller holding all of them may
	// mint it (D-25).
	if !principalCoversKeyGrants(principal, key) {
		a.renderDashboardKeys(w, r.Context(), tenantID, "Key resource grants exceed your authority")
		return
	}
	plaintext, prefix, hash, usedAlgo, err := a.generateKey(a.apiKeyPrefix, a.apiKeyAlgo, a.apiKeyPepper)
	if err != nil {
		log.Printf("failed to generate dashboard API key: %v", err)
		a.renderDashboardKeys(w, r.Context(), tenantID, "Failed to generate API key")
		return
	}
	key.KeyPrefix = prefix
	key.KeyHash = hash
	key.KeyHashAlgo = usedAlgo
	if err := a.identity.CreateAPIKey(r.Context(), tenantID, key); err != nil {
		log.Printf("failed to create dashboard API key: %v", err)
		a.renderDashboardKeys(w, r.Context(), tenantID, "Failed to persist API key")
		return
	}

	a.renderDashboardKeys(w, r.Context(), tenantID, "API key created", plaintext)
}

func (a *App) toggleDashboardAPIKey(w http.ResponseWriter, r *http.Request, tenantID string) {
	id := strings.TrimSpace(r.FormValue("key_id"))
	if id == "" {
		a.renderDashboardKeys(w, r.Context(), tenantID, "key id is required")
		return
	}
	key, err := a.identity.GetAPIKey(r.Context(), tenantID, id)
	if err != nil {
		a.renderDashboardKeys(w, r.Context(), tenantID, "API key not found")
		return
	}
	// Bound only the ACTIVATE edge: re-arming re-opens the key's stored
	// grants, so they must sit inside the operator's own authority.
	// Deactivation stays free — disabling never grants anything.
	if !key.IsActive {
		principal, _ := auth.PrincipalFromContext(r.Context())
		if !principalCoversKeyGrants(principal, key) {
			a.renderDashboardKeys(w, r.Context(), tenantID, "Key resource grants exceed your authority")
			return
		}
	}
	key.IsActive = !key.IsActive
	if err := a.identity.UpdateAPIKey(r.Context(), tenantID, key); err != nil {
		log.Printf("failed to toggle dashboard API key: %v", err)
		a.renderDashboardKeys(w, r.Context(), tenantID, "Failed to update key status")
		return
	}
	a.renderDashboardKeys(w, r.Context(), tenantID, "API key updated")
}

func (a *App) deleteDashboardAPIKey(w http.ResponseWriter, r *http.Request, tenantID string) {
	id := strings.TrimSpace(r.FormValue("key_id"))
	if id == "" {
		a.renderDashboardKeys(w, r.Context(), tenantID, "key id is required")
		return
	}
	if err := a.identity.RevokeAPIKey(r.Context(), tenantID, id, time.Now().UTC()); err != nil {
		log.Printf("failed to delete dashboard API key: %v", err)
		a.renderDashboardKeys(w, r.Context(), tenantID, "Failed to delete API key")
		return
	}
	a.renderDashboardKeys(w, r.Context(), tenantID, "API key deleted")
}

func (a *App) regenerateDashboardAPIKey(w http.ResponseWriter, r *http.Request, tenantID string) {
	id := strings.TrimSpace(r.FormValue("key_id"))
	if id == "" {
		a.renderDashboardKeys(w, r.Context(), tenantID, "key id is required")
		return
	}
	key, err := a.identity.GetAPIKey(r.Context(), tenantID, id)
	if err != nil {
		a.renderDashboardKeys(w, r.Context(), tenantID, "API key not found")
		return
	}
	// A regenerated secret re-arms the key immediately: refuse before any
	// secret is generated or stored when the stored grants exceed the
	// operator's authority — KeyPrefix/KeyHash/IsActive stay untouched.
	principal, _ := auth.PrincipalFromContext(r.Context())
	if !principalCoversKeyGrants(principal, key) {
		a.renderDashboardKeys(w, r.Context(), tenantID, "Key resource grants exceed your authority")
		return
	}

	plaintext, prefix, hash, usedAlgo, err := a.generateKey(a.apiKeyPrefix, a.apiKeyAlgo, a.apiKeyPepper)
	if err != nil {
		log.Printf("failed to regenerate dashboard API key secret: %v", err)
		a.renderDashboardKeys(w, r.Context(), tenantID, "Failed to regenerate API key")
		return
	}
	if err := a.identity.SetAPIKeySecret(r.Context(), tenantID, id, prefix, hash, usedAlgo); err != nil {
		log.Printf("failed to store regenerated dashboard API key secret: %v", err)
		a.renderDashboardKeys(w, r.Context(), tenantID, "Failed to persist regenerated secret")
		return
	}
	if !key.IsActive {
		key.IsActive = true
		if err := a.identity.UpdateAPIKey(r.Context(), tenantID, key); err != nil {
			log.Printf("failed to activate regenerated dashboard API key: %v", err)
			a.renderDashboardKeys(w, r.Context(), tenantID, "Failed to activate API key")
			return
		}
	}
	a.renderDashboardKeys(w, r.Context(), tenantID, "API key regenerated", plaintext)
}

func (a *App) renderDashboardKeys(w http.ResponseWriter, ctx context.Context, tenantID string, message string, createdKey ...string) {
	keys, err := a.identity.ListAPIKeys(ctx, tenantID)
	if err != nil {
		log.Printf("failed to load dashboard API keys: %v", err)
		WriteOpenAIError(w, http.StatusInternalServerError, "internal server error", "server_error", "dashboard_keys_failed")
		return
	}
	for i := range keys {
		keys[i] = redactAdminAPIKey(keys[i])
	}
	// Revoked keys are deleted from the operator view.
	live := keys[:0]
	for _, k := range keys {
		if k.RevokedAt == nil {
			live = append(live, k)
		}
	}
	keys = live
	secret := ""
	if len(createdKey) > 0 {
		secret = createdKey[0]
	}
	if secret != "" && message == "" {
		message = "API key generated. Store this value now."
	}
	if err := dashboardTemplates.ExecuteTemplate(w, "partial-keys", dashboardKeysData{
		Keys:       keys,
		Message:    message,
		CreatedKey: secret,
	}); err != nil {
		log.Printf("failed to render dashboard keys: %v", err)
		WriteOpenAIError(w, http.StatusInternalServerError, "internal server error", "server_error", "dashboard_keys_render_failed")
	}
}

func parseDashboardScopes(raw string) []string {
	seen := make(map[string]struct{}, len(dashboardDefaultScopes))
	out := make([]string, 0, len(dashboardDefaultScopes))
	for _, scope := range strings.Split(raw, ",") {
		scope = strings.TrimSpace(scope)
		if scope == "" {
			continue
		}
		if _, exists := seen[scope]; exists {
			continue
		}
		seen[scope] = struct{}{}
		out = append(out, scope)
	}
	if len(out) == 0 {
		out = append(out, dashboardDefaultScopes...)
	}
	return out
}

func ptrBool(v bool) *bool {
	return &v
}

func formatCurrencyMicros(currency string, micros int64) string {
	major, fraction := micros/1_000_000, micros%1_000_000
	if fraction < 0 {
		fraction = -fraction
	}
	return fmt.Sprintf("%s %d.%06d", currency, major, fraction)
}
