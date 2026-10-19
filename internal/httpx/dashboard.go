package httpx

import (
	"context"
	"embed"
	"fmt"
	"html/template"
	"io/fs"
	"log"
	"net/http"
	"sort"
	"strconv"
	"strings"
	"time"

	"enclava-go/internal/auth"
	"enclava-go/internal/billing"
	"enclava-go/internal/extract"
	"enclava-go/internal/store/postgres"
)

//go:embed dashboard/templates/*.tmpl
var dashboardTemplateFS embed.FS

//go:embed dashboard/static/*
var dashboardStaticFS embed.FS

var dashboardTemplates = template.Must(template.New("").ParseFS(dashboardTemplateFS, "dashboard/templates/*.tmpl"))

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

var dashboardDefaultScopes = []string{
	"platform.admin",
	"api_keys.manage",
	"analytics.read",
	"budgets.manage",
	"models.list",
	"chat.completions",
	"embeddings.create",
	"responses",
	"openai.proxy",
	"extract",
	"extract.manage",
	"rag",
	"rag.manage",
	"chatbot",
	"chatbot.manage",
	"connectors.manage",
	"pricing.manage",
}

type dashboardUsageRow struct {
	APIKeyID     string
	APIKeyName   string
	TotalTokens  int64
	TotalCost    int64
	CostDisplay  string
	RequestCount int64
}

type dashboardAnalyticsData struct {
	TotalRequests    int64
	TotalTokens      int64
	TotalCostDisplay string
	Days             int
	ByEndpoint       []postgres.UsageGroupSum
	ByModel          []postgres.UsageGroupSum
	Rows             []dashboardUsageRow
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

type dashboardStatsData struct {
	ActiveAPIKeys     int
	RecentExtractJobs int64
	TotalExtractJobs  int64
	BudgetPercent     int
	BudgetUsed        string
	BudgetTotal       string
}

type dashboardPageCommon struct {
	ActivePage    string
	PageTitle     string
	AuthMode      string
	UserEmail     string
	DefaultScopes string
}

// dashboardIdentityData feeds page_identity: the signed-in account plus the
// OIDC providers an employee can use for sign-in/linking.
type dashboardIdentityData struct {
	dashboardPageCommon
	Providers []auth.OIDCProvider
}

type dashboardBudgetRow struct {
	APIKeyID         string
	APIKeyName       string
	BudgetLimitCents *int64
	BudgetPeriod     string
	UsedCents        int64
	UsedPercent      int
	UsedDisplay      string
	LimitDisplay     string
	IsActive         bool
	IsOverBudget     bool
}

type dashboardBudgetsData struct {
	Rows       []dashboardBudgetRow
	TotalUsed  string
	TotalLimit string
	OverCount  int
	ActiveKeys int
}

type dashboardExtractData struct {
	RecentJobs int64
	TotalJobs  int64
	Templates  []extract.Template
}

const dashboardBudgetTotalCents int64 = 10000

func (a *App) registerDashboardRoutes() {
	a.mux.Handle("/dashboard/static/", http.StripPrefix("/dashboard/static/", http.FileServer(http.FS(dashboardStaticFiles))))
	a.mux.HandleFunc("/dashboard/login", a.sessionAuth.HandleLogin)
	a.mux.HandleFunc("/dashboard/logout", a.sessionAuth.HandleLogout)
	a.mux.HandleFunc("/auth/oidc/link", a.sessionAuth.HandleLinkIdentity)
	a.mux.HandleFunc("/auth/oidc/", func(w http.ResponseWriter, r *http.Request) {
		switch {
		case strings.HasSuffix(r.URL.Path, "/start"):
			a.sessionAuth.HandleOIDCStart(w, r)
		case strings.HasSuffix(r.URL.Path, "/callback"):
			a.sessionAuth.HandleOIDCCallback(w, r)
		default:
			http.NotFound(w, r)
		}
	})
	a.mux.HandleFunc("/dashboard/partials/stats", a.sessionAuth.RequireSession(a.handleDashboardStats))
	a.mux.HandleFunc("/dashboard/partials/keys", a.sessionAuth.RequireSession(a.handleDashboardAPIKeys))
	a.mux.HandleFunc("/dashboard/partials/usage", a.sessionAuth.RequireSession(a.handleDashboardUsage))
	a.mux.HandleFunc("/dashboard/partials/budgets", a.sessionAuth.RequireSession(a.handleDashboardBudgets))
	a.mux.HandleFunc("/dashboard/partials/extract", a.sessionAuth.RequireSession(a.handleDashboardExtract))
	a.mux.HandleFunc("/dashboard/partials/extract/templates", a.sessionAuth.RequireSession(a.handleDashboardExtractTemplates))
	a.mux.HandleFunc("/dashboard/partials/extract/templates/", a.sessionAuth.RequireSession(a.handleDashboardExtractTemplateByID))
	a.mux.HandleFunc("/dashboard/partials/extract/reset-defaults", a.sessionAuth.RequireSession(a.handleDashboardExtractResetDefaults))
	a.mux.HandleFunc("/dashboard/partials/extract/settings", a.sessionAuth.RequireSession(a.handleDashboardExtractSettings))
	a.mux.HandleFunc("/dashboard/partials/extract/jobs", a.sessionAuth.RequireSession(a.handleDashboardExtractJobs))
	a.mux.HandleFunc("/dashboard/partials/providers", a.sessionAuth.RequireSession(a.handleDashboardProviders))
	a.mux.HandleFunc("/dashboard/partials/pricing", a.sessionAuth.RequireSession(a.handleDashboardPricing))
	a.mux.HandleFunc("/dashboard/partials/billing-audit", a.sessionAuth.RequireSession(a.handleDashboardBillingAudit))
	a.mux.HandleFunc("/dashboard/partials/rag", a.sessionAuth.RequireSession(a.handleDashboardRAG))
	a.mux.HandleFunc("/dashboard/partials/chatbots", a.sessionAuth.RequireSession(a.handleDashboardChatbots))
	a.mux.HandleFunc("/dashboard/partials/prompt-templates", a.sessionAuth.RequireSession(a.handleDashboardPromptTemplates))
	a.mux.HandleFunc("/dashboard/partials/connectors", a.sessionAuth.RequireSession(a.handleDashboardConnectors))
	a.mux.HandleFunc("/dashboard/rag/documents/", a.sessionAuth.RequireSession(a.handleDashboardRAGDocumentDownload))
	a.mux.HandleFunc("/dashboard/extract", a.sessionAuth.RequireSession(a.handleDashboardPageExtract))
	a.mux.HandleFunc("/dashboard/keys", a.sessionAuth.RequireSession(a.handleDashboardPageKeys))
	a.mux.HandleFunc("/dashboard/budgets", a.sessionAuth.RequireSession(a.handleDashboardPageBudgets))
	a.mux.HandleFunc("/dashboard/analytics", a.sessionAuth.RequireSession(a.handleDashboardPageAnalytics))
	a.mux.HandleFunc("/dashboard/providers", a.sessionAuth.RequireSession(a.handleDashboardPageProviders))
	a.mux.HandleFunc("/dashboard/pricing", a.sessionAuth.RequireSession(a.handleDashboardPagePricing))
	a.mux.HandleFunc("/dashboard/billing-audit", a.sessionAuth.RequireSession(a.handleDashboardPageBillingAudit))
	a.mux.HandleFunc("/dashboard/rag", a.sessionAuth.RequireSession(a.handleDashboardPageRAG))
	a.mux.HandleFunc("/dashboard/chatbots", a.sessionAuth.RequireSession(a.handleDashboardPageChatbots))
	a.mux.HandleFunc("/dashboard/prompt-templates", a.sessionAuth.RequireSession(a.handleDashboardPagePromptTemplates))
	a.mux.HandleFunc("/dashboard/connectors", a.sessionAuth.RequireSession(a.handleDashboardPageConnectors))
	a.mux.HandleFunc("/dashboard/identity", a.sessionAuth.RequireSession(a.handleDashboardPageIdentity))
	a.mux.HandleFunc("/dashboard", a.sessionAuth.RequireSession(a.handleDashboardPage))
	a.mux.HandleFunc("/dashboard/", a.sessionAuth.RequireSession(a.handleDashboardPage))
}

func (a *App) dashboardPageData(r *http.Request, activePage, pageTitle string) dashboardPageCommon {
	data := dashboardPageCommon{
		ActivePage:    activePage,
		PageTitle:     pageTitle,
		AuthMode:      "session",
		DefaultScopes: strings.Join(dashboardDefaultScopes, ", "),
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

func (a *App) handleDashboardPage(w http.ResponseWriter, r *http.Request) {
	if r.Method != http.MethodGet {
		WriteMethodNotAllowed(w)
		return
	}
	w.Header().Set("Content-Type", "text/html; charset=utf-8")
	if err := dashboardTemplates.ExecuteTemplate(w, "page-dashboard", a.dashboardPageData(r, "dashboard", "Dashboard")); err != nil {
		log.Printf("failed to render dashboard page: %v", err)
		WriteOpenAIError(w, http.StatusInternalServerError, "internal server error", "server_error", "dashboard_render_failed")
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

func (a *App) handleDashboardPageAnalytics(w http.ResponseWriter, r *http.Request) {
	if r.Method != http.MethodGet {
		WriteMethodNotAllowed(w)
		return
	}
	w.Header().Set("Content-Type", "text/html; charset=utf-8")
	if err := dashboardTemplates.ExecuteTemplate(w, "page-analytics", a.dashboardPageData(r, "analytics", "Analytics")); err != nil {
		log.Printf("failed to render dashboard analytics page: %v", err)
		WriteOpenAIError(w, http.StatusInternalServerError, "internal server error", "server_error", "dashboard_analytics_page_render_failed")
	}
}

func (a *App) handleDashboardPageBudgets(w http.ResponseWriter, r *http.Request) {
	if r.Method != http.MethodGet {
		WriteMethodNotAllowed(w)
		return
	}
	w.Header().Set("Content-Type", "text/html; charset=utf-8")
	if err := dashboardTemplates.ExecuteTemplate(w, "page-budgets", a.dashboardPageData(r, "budgets", "Budgets")); err != nil {
		log.Printf("failed to render dashboard budgets page: %v", err)
		WriteOpenAIError(w, http.StatusInternalServerError, "internal server error", "server_error", "dashboard_budgets_page_render_failed")
	}
}

func (a *App) handleDashboardPageExtract(w http.ResponseWriter, r *http.Request) {
	if r.Method != http.MethodGet {
		WriteMethodNotAllowed(w)
		return
	}
	w.Header().Set("Content-Type", "text/html; charset=utf-8")
	if err := dashboardTemplates.ExecuteTemplate(w, "page-extract", a.dashboardPageData(r, "extract", "Extract")); err != nil {
		log.Printf("failed to render dashboard extract page: %v", err)
		WriteOpenAIError(w, http.StatusInternalServerError, "internal server error", "server_error", "dashboard_extract_page_render_failed")
	}
}

func (a *App) handleDashboardStats(w http.ResponseWriter, r *http.Request) {
	setNoStoreHeaders(w)
	if r.Method != http.MethodGet {
		WriteMethodNotAllowed(w)
		return
	}
	tenantID, ok := a.dashboardTenantID(w, r)
	if !ok {
		return
	}
	if a.identity == nil {
		WriteOpenAIError(w, http.StatusNotImplemented, "API key store not configured", "server_error", "not_configured")
		return
	}

	keys, err := a.identity.ListAPIKeys(r.Context(), tenantID)
	if err != nil {
		log.Printf("failed to load dashboard stats keys: %v", err)
		WriteOpenAIError(w, http.StatusInternalServerError, "internal server error", "server_error", "dashboard_stats_keys_failed")
		return
	}

	activeKeys := 0
	for _, key := range keys {
		if key.IsActive {
			activeKeys++
		}
	}

	var recentExtractJobs int64
	var totalExtractJobs int64
	if a.extractSvc != nil {
		p, ok := auth.PrincipalFromContext(r.Context())
		if !ok || p.TenantID == "" {
			WriteUnauthorized(w, "authenticated principal required")
			return
		}
		since := time.Now().UTC().Add(-24 * time.Hour)
		recentExtractJobs, totalExtractJobs, err = a.extractSvc.JobStats(r.Context(), p, since)
		if err != nil {
			log.Printf("failed to load dashboard extract job stats: %v", err)
			WriteOpenAIError(w, http.StatusInternalServerError, "internal server error", "server_error", "dashboard_stats_extract_failed")
			return
		}
	}

	var usedBudgetCents int64
	if a.billingRepo != nil {
		totals, err := a.billingRepo.UsageTotals(r.Context(), tenantID, time.Now().UTC().AddDate(0, 0, -30))
		if err != nil {
			log.Printf("failed to load dashboard stats usage: %v", err)
			WriteOpenAIError(w, http.StatusInternalServerError, "internal server error", "server_error", "dashboard_stats_usage_failed")
			return
		}
		for _, item := range totals {
			usedBudgetCents += microsToCents(item.AmountMicros)
		}
	}

	w.Header().Set("Content-Type", "text/html; charset=utf-8")
	if err := dashboardTemplates.ExecuteTemplate(w, "partial-stats", dashboardStatsData{
		ActiveAPIKeys:     activeKeys,
		RecentExtractJobs: recentExtractJobs,
		TotalExtractJobs:  totalExtractJobs,
		BudgetPercent:     percentInt(usedBudgetCents, dashboardBudgetTotalCents),
		BudgetUsed:        formatUSD(usedBudgetCents),
		BudgetTotal:       formatUSD(dashboardBudgetTotalCents),
	}); err != nil {
		log.Printf("failed to render dashboard stats: %v", err)
		WriteOpenAIError(w, http.StatusInternalServerError, "internal server error", "server_error", "dashboard_stats_render_failed")
	}
}

func (a *App) handleDashboardUsage(w http.ResponseWriter, r *http.Request) {
	setNoStoreHeaders(w)
	if r.Method != http.MethodGet {
		WriteMethodNotAllowed(w)
		return
	}
	if a.identity == nil || a.billingRepo == nil {
		WriteOpenAIError(w, http.StatusNotImplemented, "usage tracking not configured", "server_error", "not_configured")
		return
	}
	tenantID, ok := a.dashboardTenantID(w, r)
	if !ok {
		return
	}

	days := 30
	if d := r.URL.Query().Get("days"); d != "" {
		if parsed, err := strconv.Atoi(d); err == nil {
			switch parsed {
			case 7, 30, 90:
				days = parsed
			}
		}
	}
	since := time.Now().UTC().AddDate(0, 0, -days)

	totals, err := a.billingRepo.UsageTotals(r.Context(), tenantID, since)
	if err != nil {
		log.Printf("failed to load dashboard usage totals: %v", err)
		WriteOpenAIError(w, http.StatusInternalServerError, "internal server error", "server_error", "dashboard_usage_totals_failed")
		return
	}
	var totalRequests, totalTokens, totalCostCents int64
	for _, t := range totals {
		totalRequests += t.RequestCount
		totalTokens += t.TotalUnits
		totalCostCents += microsToCents(t.AmountMicros)
	}

	byEndpoint, err := a.billingRepo.UsageByEndpoint(r.Context(), tenantID, since)
	if err != nil {
		log.Printf("failed to load dashboard usage by endpoint: %v", err)
		WriteOpenAIError(w, http.StatusInternalServerError, "internal server error", "server_error", "dashboard_usage_endpoint_failed")
		return
	}

	byModel, err := a.billingRepo.UsageByModel(r.Context(), tenantID, since)
	if err != nil {
		log.Printf("failed to load dashboard usage by model: %v", err)
		WriteOpenAIError(w, http.StatusInternalServerError, "internal server error", "server_error", "dashboard_usage_model_failed")
		return
	}

	usage, err := a.billingRepo.UsageBySubject(r.Context(), tenantID, since)
	if err != nil {
		log.Printf("failed to load dashboard usage summary: %v", err)
		WriteOpenAIError(w, http.StatusInternalServerError, "internal server error", "server_error", "dashboard_usage_failed")
		return
	}
	keys, err := a.identity.ListAPIKeys(r.Context(), tenantID)
	if err != nil {
		log.Printf("failed to load dashboard keys for usage: %v", err)
		WriteOpenAIError(w, http.StatusInternalServerError, "internal server error", "server_error", "dashboard_usage_keys_failed")
		return
	}

	// usage_records.subject_id carries the stored key UUID for api_key
	// subjects — names map directly.
	nameByID := make(map[string]string, len(keys))
	for _, key := range keys {
		name := strings.TrimSpace(key.Name)
		if name == "" {
			name = "API Key " + key.ID
		}
		nameByID[key.ID] = name
	}

	rows := make([]dashboardUsageRow, 0, len(usage))
	for _, item := range usage {
		if item.SubjectType != billing.BudgetSubjectAPIKey {
			continue
		}
		costCents := microsToCents(item.AmountMicros)
		rows = append(rows, dashboardUsageRow{
			APIKeyID:     item.SubjectID,
			APIKeyName:   nameByID[item.SubjectID],
			TotalTokens:  item.TotalUnits,
			TotalCost:    costCents,
			RequestCount: item.RequestCount,
		})
	}

	sort.Slice(rows, func(i, j int) bool {
		return rows[i].TotalTokens > rows[j].TotalTokens
	})
	if len(rows) == 0 {
		for _, key := range keys {
			rows = append(rows, dashboardUsageRow{
				APIKeyID:     key.ID,
				APIKeyName:   key.Name,
				TotalTokens:  0,
				TotalCost:    0,
				CostDisplay:  "$0.00",
				RequestCount: 0,
			})
		}
	}

	w.Header().Set("Content-Type", "text/html; charset=utf-8")
	if err := dashboardTemplates.ExecuteTemplate(w, "partial-usage", dashboardAnalyticsData{
		TotalRequests:    totalRequests,
		TotalTokens:      totalTokens,
		TotalCostDisplay: formatUSD(totalCostCents),
		Days:             days,
		ByEndpoint:       byEndpoint,
		ByModel:          byModel,
		Rows:             rows,
	}); err != nil {
		log.Printf("failed to render dashboard usage: %v", err)
		WriteOpenAIError(w, http.StatusInternalServerError, "internal server error", "server_error", "dashboard_usage_render_failed")
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

func (a *App) handleDashboardBudgets(w http.ResponseWriter, r *http.Request) {
	setNoStoreHeaders(w)
	if r.Method != http.MethodGet {
		WriteMethodNotAllowed(w)
		return
	}
	if a.identity == nil || a.billingRepo == nil {
		WriteOpenAIError(w, http.StatusNotImplemented, "budget tracking not configured", "server_error", "not_configured")
		return
	}
	tenantID, ok := a.dashboardTenantID(w, r)
	if !ok {
		return
	}

	keys, err := a.identity.ListAPIKeys(r.Context(), tenantID)
	if err != nil {
		log.Printf("failed to load dashboard budget keys: %v", err)
		WriteOpenAIError(w, http.StatusInternalServerError, "internal server error", "server_error", "dashboard_budgets_keys_failed")
		return
	}

	rows := make([]dashboardBudgetRow, 0, len(keys))
	var totalUsedCents int64
	var totalLimitCents int64
	var overCount int
	var activeKeys int

	// Per-key budget limits moved to stored billing budgets; the section
	// reports active keys with lifetime settled usage from billing
	// usage_records keyed by the stored key UUID.
	for _, key := range keys {
		subject := billing.Subject{Type: billing.BudgetSubjectAPIKey, ID: key.ID}
		_, usedMicros, err := a.usedForPeriod(r.Context(), tenantID, subject, "total", nil, time.Now().UTC())
		if err != nil {
			log.Printf("failed to load usage for key %s: %v", key.ID, err)
			continue
		}

		usedCents := microsToCents(usedMicros)

		name := strings.TrimSpace(key.Name)
		if name == "" {
			name = "API Key " + key.ID
		}

		rows = append(rows, dashboardBudgetRow{
			APIKeyID:         key.ID,
			APIKeyName:       name,
			BudgetLimitCents: nil,
			BudgetPeriod:     "total",
			UsedCents:        usedCents,
			UsedPercent:      0,
			UsedDisplay:      formatUSD(usedCents),
			LimitDisplay:     "—",
			IsActive:         key.IsActive,
			IsOverBudget:     false,
		})

		totalUsedCents += usedCents
		if key.IsActive {
			activeKeys++
		}
	}

	w.Header().Set("Content-Type", "text/html; charset=utf-8")
	if err := dashboardTemplates.ExecuteTemplate(w, "partial-budgets", dashboardBudgetsData{
		Rows:       rows,
		TotalUsed:  formatUSD(totalUsedCents),
		TotalLimit: formatUSD(totalLimitCents),
		OverCount:  overCount,
		ActiveKeys: activeKeys,
	}); err != nil {
		log.Printf("failed to render dashboard budgets: %v", err)
		WriteOpenAIError(w, http.StatusInternalServerError, "internal server error", "server_error", "dashboard_budgets_render_failed")
	}
}

func (a *App) handleDashboardExtract(w http.ResponseWriter, r *http.Request) {
	setNoStoreHeaders(w)
	if r.Method != http.MethodGet {
		WriteMethodNotAllowed(w)
		return
	}

	p, ok := a.extractPrincipal(w, r)
	if !ok {
		return
	}

	var recentJobs int64
	var totalJobs int64
	var templates []extract.Template
	if a.extractSvc != nil {
		var err error
		since := time.Now().UTC().Add(-24 * time.Hour)
		recentJobs, totalJobs, err = a.extractSvc.JobStats(r.Context(), p, since)
		if err != nil {
			log.Printf("failed to load extract job stats: %v", err)
			WriteOpenAIError(w, http.StatusInternalServerError, "internal server error", "server_error", "dashboard_extract_stats_failed")
			return
		}
		templates, err = a.extractSvc.ListTemplates(r.Context(), p)
		if err != nil {
			log.Printf("failed to load extract templates: %v", err)
			WriteOpenAIError(w, http.StatusInternalServerError, "internal server error", "server_error", "dashboard_extract_templates_failed")
			return
		}
	}

	w.Header().Set("Content-Type", "text/html; charset=utf-8")
	if err := dashboardTemplates.ExecuteTemplate(w, "partial-extract", dashboardExtractData{
		RecentJobs: recentJobs,
		TotalJobs:  totalJobs,
		Templates:  templates,
	}); err != nil {
		log.Printf("failed to render dashboard extract: %v", err)
		WriteOpenAIError(w, http.StatusInternalServerError, "internal server error", "server_error", "dashboard_extract_render_failed")
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

func formatUSD(cents int64) string {
	return fmt.Sprintf("$%.2f", float64(cents)/100)
}

func percentInt(value, total int64) int {
	if total <= 0 {
		return 0
	}
	return int((float64(value) / float64(total)) * 100)
}
