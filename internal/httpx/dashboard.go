package httpx

import (
	"context"
	"crypto/hmac"
	"crypto/sha256"
	"embed"
	"encoding/base64"
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
	"enclava-go/internal/store/sqlite"
)

//go:embed dashboard/templates/*.tmpl
var dashboardTemplateFS embed.FS

//go:embed dashboard/static/*
var dashboardStaticFS embed.FS

var dashboardTemplates = template.Must(template.New("").ParseFS(dashboardTemplateFS, "dashboard/templates/*.tmpl"))

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
	APIKeyID     int64
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
	ByEndpoint       []sqlite.UsageByEndpoint
	ByModel          []sqlite.UsageByModel
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
	AdminEmail    string
	DefaultScopes string
}

type dashboardBudgetRow struct {
	APIKeyID         int64
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
	Templates  []sqlite.ExtractTemplate
}

const dashboardBudgetTotalCents int64 = 10000

const dashboardSessionCookieName = "enclava_dashboard_session"
const dashboardSessionTTL = 12 * time.Hour

func (a *App) registerDashboardRoutes() {
	a.mux.Handle("/dashboard/static/", http.StripPrefix("/dashboard/static/", http.FileServer(http.FS(dashboardStaticFiles))))
	a.mux.HandleFunc("/dashboard/login", a.handleDashboardLogin)
	a.mux.HandleFunc("/dashboard/logout", a.handleDashboardLogout)
	a.mux.HandleFunc("/dashboard/partials/stats", a.dashboardPartialsAuth(a.handleDashboardStats))
	a.mux.HandleFunc("/dashboard/partials/keys", a.dashboardPartialsAuth(a.handleDashboardAPIKeys))
	a.mux.HandleFunc("/dashboard/partials/usage", a.dashboardPartialsAuth(a.handleDashboardUsage))
	a.mux.HandleFunc("/dashboard/partials/budgets", a.dashboardPartialsAuth(a.handleDashboardBudgets))
	a.mux.HandleFunc("/dashboard/partials/extract", a.dashboardPartialsAuth(a.handleDashboardExtract))
	a.mux.HandleFunc("/dashboard/partials/extract/templates", a.dashboardPartialsAuth(a.handleDashboardExtractTemplates))
	a.mux.HandleFunc("/dashboard/partials/extract/templates/", a.dashboardPartialsAuth(a.handleDashboardExtractTemplateByID))
	a.mux.HandleFunc("/dashboard/partials/extract/reset-defaults", a.dashboardPartialsAuth(a.handleDashboardExtractResetDefaults))
	a.mux.HandleFunc("/dashboard/partials/extract/settings", a.dashboardPartialsAuth(a.handleDashboardExtractSettings))
	a.mux.HandleFunc("/dashboard/partials/extract/process", a.dashboardPartialsAuth(a.handleDashboardExtractProcess))
	a.mux.HandleFunc("/dashboard/partials/extract/jobs", a.dashboardPartialsAuth(a.handleDashboardExtractJobs))
	a.mux.HandleFunc("/dashboard/partials/extract/models", a.dashboardPartialsAuth(a.handleDashboardExtractModels))
	a.mux.HandleFunc("/dashboard/partials/providers", a.dashboardPartialsAuth(a.handleDashboardProviders))
	a.mux.HandleFunc("/dashboard/partials/pricing", a.dashboardPartialsAuth(a.handleDashboardPricing))
	a.mux.HandleFunc("/dashboard/partials/billing-audit", a.dashboardPartialsAuth(a.handleDashboardBillingAudit))
	a.mux.HandleFunc("/dashboard/partials/rag", a.dashboardPartialsAuth(a.handleDashboardRAG))
	a.mux.HandleFunc("/dashboard/partials/chatbots", a.dashboardPartialsAuth(a.handleDashboardChatbots))
	a.mux.HandleFunc("/dashboard/partials/prompt-templates", a.dashboardPartialsAuth(a.handleDashboardPromptTemplates))
	a.mux.HandleFunc("/dashboard/partials/connectors", a.dashboardPartialsAuth(a.handleDashboardConnectors))
	a.mux.HandleFunc("/dashboard/rag/documents/", a.dashboardPartialsAuth(a.handleDashboardRAGDocumentDownload))
	a.mux.HandleFunc("/dashboard/extract", a.handleDashboardPageExtract)
	a.mux.HandleFunc("/dashboard/keys", a.handleDashboardPageKeys)
	a.mux.HandleFunc("/dashboard/budgets", a.handleDashboardPageBudgets)
	a.mux.HandleFunc("/dashboard/analytics", a.handleDashboardPageAnalytics)
	a.mux.HandleFunc("/dashboard/providers", a.handleDashboardPageProviders)
	a.mux.HandleFunc("/dashboard/pricing", a.handleDashboardPagePricing)
	a.mux.HandleFunc("/dashboard/billing-audit", a.handleDashboardPageBillingAudit)
	a.mux.HandleFunc("/dashboard/rag", a.handleDashboardPageRAG)
	a.mux.HandleFunc("/dashboard/chatbots", a.handleDashboardPageChatbots)
	a.mux.HandleFunc("/dashboard/prompt-templates", a.handleDashboardPagePromptTemplates)
	a.mux.HandleFunc("/dashboard/connectors", a.handleDashboardPageConnectors)
	a.mux.HandleFunc("/dashboard", a.handleDashboardPage)
	a.mux.HandleFunc("/dashboard/", a.handleDashboardPage)
}

func (a *App) dashboardPageData(activePage, pageTitle string) dashboardPageCommon {
	return dashboardPageCommon{
		ActivePage:    activePage,
		PageTitle:     pageTitle,
		AuthMode:      a.dashboardAuthMode(),
		AdminEmail:    a.adminEmail,
		DefaultScopes: strings.Join(dashboardDefaultScopes, ", "),
	}
}

func (a *App) handleDashboardPage(w http.ResponseWriter, r *http.Request) {
	if r.Method != http.MethodGet {
		WriteMethodNotAllowed(w)
		return
	}
	if a.dashboardPasswordAuthEnabled() && !a.dashboardSessionValid(r) {
		http.Redirect(w, r, "/dashboard/login", http.StatusSeeOther)
		return
	}
	w.Header().Set("Content-Type", "text/html; charset=utf-8")
	if err := dashboardTemplates.ExecuteTemplate(w, "page-dashboard", a.dashboardPageData("dashboard", "Dashboard")); err != nil {
		log.Printf("failed to render dashboard page: %v", err)
		WriteOpenAIError(w, http.StatusInternalServerError, "internal server error", "server_error", "dashboard_render_failed")
	}
}

func (a *App) handleDashboardPageKeys(w http.ResponseWriter, r *http.Request) {
	if r.Method != http.MethodGet {
		WriteMethodNotAllowed(w)
		return
	}
	if a.dashboardPasswordAuthEnabled() && !a.dashboardSessionValid(r) {
		http.Redirect(w, r, "/dashboard/login", http.StatusSeeOther)
		return
	}
	w.Header().Set("Content-Type", "text/html; charset=utf-8")
	if err := dashboardTemplates.ExecuteTemplate(w, "page-keys", a.dashboardPageData("keys", "API Keys")); err != nil {
		log.Printf("failed to render dashboard keys page: %v", err)
		WriteOpenAIError(w, http.StatusInternalServerError, "internal server error", "server_error", "dashboard_keys_page_render_failed")
	}
}

func (a *App) handleDashboardPageAnalytics(w http.ResponseWriter, r *http.Request) {
	if r.Method != http.MethodGet {
		WriteMethodNotAllowed(w)
		return
	}
	if a.dashboardPasswordAuthEnabled() && !a.dashboardSessionValid(r) {
		http.Redirect(w, r, "/dashboard/login", http.StatusSeeOther)
		return
	}
	w.Header().Set("Content-Type", "text/html; charset=utf-8")
	if err := dashboardTemplates.ExecuteTemplate(w, "page-analytics", a.dashboardPageData("analytics", "Analytics")); err != nil {
		log.Printf("failed to render dashboard analytics page: %v", err)
		WriteOpenAIError(w, http.StatusInternalServerError, "internal server error", "server_error", "dashboard_analytics_page_render_failed")
	}
}

func (a *App) handleDashboardPageBudgets(w http.ResponseWriter, r *http.Request) {
	if r.Method != http.MethodGet {
		WriteMethodNotAllowed(w)
		return
	}
	if a.dashboardPasswordAuthEnabled() && !a.dashboardSessionValid(r) {
		http.Redirect(w, r, "/dashboard/login", http.StatusSeeOther)
		return
	}
	w.Header().Set("Content-Type", "text/html; charset=utf-8")
	if err := dashboardTemplates.ExecuteTemplate(w, "page-budgets", a.dashboardPageData("budgets", "Budgets")); err != nil {
		log.Printf("failed to render dashboard budgets page: %v", err)
		WriteOpenAIError(w, http.StatusInternalServerError, "internal server error", "server_error", "dashboard_budgets_page_render_failed")
	}
}

func (a *App) handleDashboardPageExtract(w http.ResponseWriter, r *http.Request) {
	if r.Method != http.MethodGet {
		WriteMethodNotAllowed(w)
		return
	}
	if a.dashboardPasswordAuthEnabled() && !a.dashboardSessionValid(r) {
		http.Redirect(w, r, "/dashboard/login", http.StatusSeeOther)
		return
	}
	w.Header().Set("Content-Type", "text/html; charset=utf-8")
	if err := dashboardTemplates.ExecuteTemplate(w, "page-extract", a.dashboardPageData("extract", "Extract")); err != nil {
		log.Printf("failed to render dashboard extract page: %v", err)
		WriteOpenAIError(w, http.StatusInternalServerError, "internal server error", "server_error", "dashboard_extract_page_render_failed")
	}
}

func (a *App) handleDashboardLogin(w http.ResponseWriter, r *http.Request) {
	setNoStoreHeaders(w)
	if !a.dashboardPasswordAuthEnabled() {
		http.Redirect(w, r, "/dashboard", http.StatusSeeOther)
		return
	}
	switch r.Method {
	case http.MethodGet:
		if a.dashboardSessionValid(r) {
			http.Redirect(w, r, "/dashboard", http.StatusSeeOther)
			return
		}
		a.renderDashboardLogin(w, dashboardLoginData{Email: a.adminEmail})
	case http.MethodPost:
		if err := r.ParseForm(); err != nil {
			a.renderDashboardLogin(w, dashboardLoginData{
				Email: strings.TrimSpace(r.FormValue("email")),
				Error: "Invalid form submission",
			})
			return
		}
		email := strings.TrimSpace(r.FormValue("email"))
		password := r.FormValue("password")
		if !a.dashboardCredentialsValid(email, password) {
			a.renderDashboardLogin(w, dashboardLoginData{
				Email: email,
				Error: "Invalid email or password",
			})
			return
		}
		a.setDashboardSession(w, r, a.adminEmail)
		http.Redirect(w, r, "/dashboard", http.StatusSeeOther)
	default:
		WriteMethodNotAllowed(w)
	}
}

func (a *App) handleDashboardLogout(w http.ResponseWriter, r *http.Request) {
	if r.Method != http.MethodPost {
		WriteMethodNotAllowed(w)
		return
	}
	a.clearDashboardSession(w, r)
	target := "/dashboard"
	if a.dashboardPasswordAuthEnabled() {
		target = "/dashboard/login"
	}
	http.Redirect(w, r, target, http.StatusSeeOther)
}

func (a *App) renderDashboardLogin(w http.ResponseWriter, data dashboardLoginData) {
	w.Header().Set("Content-Type", "text/html; charset=utf-8")
	if err := dashboardTemplates.ExecuteTemplate(w, "login.tmpl", data); err != nil {
		log.Printf("failed to render dashboard login page: %v", err)
		WriteOpenAIError(w, http.StatusInternalServerError, "internal server error", "server_error", "dashboard_login_render_failed")
	}
}

func (a *App) dashboardPartialsAuth(next http.HandlerFunc) http.HandlerFunc {
	return func(w http.ResponseWriter, r *http.Request) {
		if a.dashboardPasswordAuthEnabled() {
			if !a.dashboardSessionValid(r) {
				WriteUnauthorized(w, "dashboard login required")
				return
			}
			next(w, r)
			return
		}
		a.authenticated("platform.admin", func(w http.ResponseWriter, r *http.Request, _ auth.Principal) {
			next(w, r)
		})(w, r)
	}
}

func (a *App) dashboardAuthMode() string {
	if a.dashboardPasswordAuthEnabled() {
		return "password"
	}
	return "api_key"
}

func (a *App) dashboardPasswordAuthEnabled() bool {
	return strings.TrimSpace(a.adminEmail) != "" && strings.TrimSpace(a.adminPassword) != ""
}

func (a *App) dashboardCredentialsValid(email, password string) bool {
	configuredEmail := strings.ToLower(strings.TrimSpace(a.adminEmail))
	inputEmail := strings.ToLower(strings.TrimSpace(email))
	return secureStringEqual(configuredEmail, inputEmail) && secureStringEqual(a.adminPassword, password)
}

func secureStringEqual(left, right string) bool {
	l := sha256.Sum256([]byte(left))
	r := sha256.Sum256([]byte(right))
	return hmac.Equal(l[:], r[:])
}

func (a *App) setDashboardSession(w http.ResponseWriter, r *http.Request, email string) {
	expiresAt := time.Now().UTC().Add(dashboardSessionTTL)
	payload := fmt.Sprintf("%s|%d", strings.TrimSpace(email), expiresAt.Unix())
	signature := a.signDashboardPayload(payload)
	token := base64.RawURLEncoding.EncodeToString([]byte(payload)) + "." + base64.RawURLEncoding.EncodeToString(signature)
	http.SetCookie(w, &http.Cookie{
		Name:     dashboardSessionCookieName,
		Value:    token,
		Path:     "/dashboard",
		Expires:  expiresAt,
		HttpOnly: true,
		SameSite: http.SameSiteLaxMode,
		Secure:   r.TLS != nil,
	})
}

func (a *App) clearDashboardSession(w http.ResponseWriter, r *http.Request) {
	http.SetCookie(w, &http.Cookie{
		Name:     dashboardSessionCookieName,
		Value:    "",
		Path:     "/dashboard",
		MaxAge:   -1,
		Expires:  time.Unix(0, 0),
		HttpOnly: true,
		SameSite: http.SameSiteLaxMode,
		Secure:   r.TLS != nil,
	})
}

func (a *App) dashboardSessionValid(r *http.Request) bool {
	cookie, err := r.Cookie(dashboardSessionCookieName)
	if err != nil || strings.TrimSpace(cookie.Value) == "" {
		return false
	}

	parts := strings.Split(cookie.Value, ".")
	if len(parts) != 2 {
		return false
	}
	payloadBytes, err := base64.RawURLEncoding.DecodeString(parts[0])
	if err != nil {
		return false
	}
	signature, err := base64.RawURLEncoding.DecodeString(parts[1])
	if err != nil {
		return false
	}

	payload := string(payloadBytes)
	expected := a.signDashboardPayload(payload)
	if !hmac.Equal(signature, expected) {
		return false
	}

	sep := strings.LastIndex(payload, "|")
	if sep <= 0 {
		return false
	}
	email := strings.TrimSpace(payload[:sep])
	expRaw := payload[sep+1:]
	expUnix, err := strconv.ParseInt(expRaw, 10, 64)
	if err != nil || expUnix <= 0 {
		return false
	}
	if time.Now().UTC().After(time.Unix(expUnix, 0)) {
		return false
	}
	return secureStringEqual(strings.ToLower(email), strings.ToLower(strings.TrimSpace(a.adminEmail)))
}

func (a *App) signDashboardPayload(payload string) []byte {
	mac := hmac.New(sha256.New, []byte(a.dashboardSessionSecret()))
	_, _ = mac.Write([]byte(payload))
	return mac.Sum(nil)
}

func (a *App) dashboardSessionSecret() string {
	pepper := strings.TrimSpace(a.apiKeyPepper)
	if pepper != "" {
		return pepper + "|" + a.adminPassword
	}
	return "enclava-dashboard|" + a.apiKeyPrefix + "|" + a.adminPassword
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
	if a.extractJobs != nil {
		since := time.Now().UTC().Add(-24 * time.Hour)
		recentExtractJobs, err = a.extractJobs.CountSince(r.Context(), since)
		if err != nil {
			log.Printf("failed to load dashboard recent extract jobs: %v", err)
			WriteOpenAIError(w, http.StatusInternalServerError, "internal server error", "server_error", "dashboard_stats_extract_recent_failed")
			return
		}
		totalExtractJobs, err = a.extractJobs.CountAll(r.Context())
		if err != nil {
			log.Printf("failed to load dashboard total extract jobs: %v", err)
			WriteOpenAIError(w, http.StatusInternalServerError, "internal server error", "server_error", "dashboard_stats_extract_total_failed")
			return
		}
	}

	var usedBudgetCents int64
	if a.usageRepo != nil {
		usage, err := a.usageRepo.SumAll(r.Context(), time.Now().UTC().AddDate(0, 0, -30))
		if err != nil {
			log.Printf("failed to load dashboard stats usage: %v", err)
			WriteOpenAIError(w, http.StatusInternalServerError, "internal server error", "server_error", "dashboard_stats_usage_failed")
			return
		}
		for _, item := range usage {
			usedBudgetCents += item.TotalCostCents
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
	if a.identity == nil || a.usageRepo == nil {
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

	totalRequests, totalTokens, totalCostCents, err := a.usageRepo.SumTotal(r.Context(), since)
	if err != nil {
		log.Printf("failed to load dashboard usage totals: %v", err)
		WriteOpenAIError(w, http.StatusInternalServerError, "internal server error", "server_error", "dashboard_usage_totals_failed")
		return
	}

	byEndpoint, err := a.usageRepo.SumByEndpoint(r.Context(), since)
	if err != nil {
		log.Printf("failed to load dashboard usage by endpoint: %v", err)
		WriteOpenAIError(w, http.StatusInternalServerError, "internal server error", "server_error", "dashboard_usage_endpoint_failed")
		return
	}

	byModel, err := a.usageRepo.SumByModel(r.Context(), since)
	if err != nil {
		log.Printf("failed to load dashboard usage by model: %v", err)
		WriteOpenAIError(w, http.StatusInternalServerError, "internal server error", "server_error", "dashboard_usage_model_failed")
		return
	}

	usage, err := a.usageRepo.SumAll(r.Context(), since)
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

	// usage rows keep the legacy numeric key handle — map each stored key to
	// the deterministic handle auth derives for it (auth.LegacyAPIKeyID).
	nameByID := make(map[int64]string, len(keys))
	for _, key := range keys {
		name := strings.TrimSpace(key.Name)
		if name == "" {
			name = "API Key " + key.ID
		}
		nameByID[auth.LegacyAPIKeyID(tenantID, key.ID)] = name
	}

	rows := make([]dashboardUsageRow, 0, len(usage))
	for _, item := range usage {
		rows = append(rows, dashboardUsageRow{
			APIKeyID:     item.APIKeyID,
			APIKeyName:   nameByID[item.APIKeyID],
			TotalTokens:  item.TotalTokens,
			TotalCost:    item.TotalCostCents,
			CostDisplay:  formatUSD(item.TotalCostCents),
			RequestCount: item.RequestCount,
		})
	}

	sort.Slice(rows, func(i, j int) bool {
		return rows[i].TotalTokens > rows[j].TotalTokens
	})
	if len(rows) == 0 {
		for _, key := range keys {
			rows = append(rows, dashboardUsageRow{
				APIKeyID:     auth.LegacyAPIKeyID(tenantID, key.ID),
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
// the request's authenticated principal. The signed password cookie carries
// no tenant identity, so password-mode calls get a truthful 401 rather than
// a fabricated tenant (the session-cookie principal lands with 01-16/01-20).
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
	if a.identity == nil || a.usageRepo == nil {
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

	// Per-key budget limits are legacy sqlite columns the postgres identity
	// rows do not carry; billing budgets own them after 01-17. The section
	// still reports active keys and their lifetime usage from the retained
	// counters (usage rows keep the legacy numeric key handle).
	for _, key := range keys {
		keyID := auth.LegacyAPIKeyID(tenantID, key.ID)
		_, usedMicrocents, err := a.usedForPeriod(r.Context(), keyID, "total", nil, time.Now().UTC())
		if err != nil {
			log.Printf("failed to load usage for key %s: %v", key.ID, err)
			continue
		}

		usedCents := microcentsToCents(usedMicrocents)

		name := strings.TrimSpace(key.Name)
		if name == "" {
			name = "API Key " + key.ID
		}

		rows = append(rows, dashboardBudgetRow{
			APIKeyID:         keyID,
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

	var recentJobs int64
	var totalJobs int64
	if a.extractJobs != nil {
		var err error
		since := time.Now().UTC().Add(-24 * time.Hour)
		recentJobs, err = a.extractJobs.CountSince(r.Context(), since)
		if err != nil {
			log.Printf("failed to load extract recent jobs: %v", err)
			WriteOpenAIError(w, http.StatusInternalServerError, "internal server error", "server_error", "dashboard_extract_recent_failed")
			return
		}
		totalJobs, err = a.extractJobs.CountAll(r.Context())
		if err != nil {
			log.Printf("failed to load extract total jobs: %v", err)
			WriteOpenAIError(w, http.StatusInternalServerError, "internal server error", "server_error", "dashboard_extract_total_failed")
			return
		}
	}

	var templates []sqlite.ExtractTemplate
	if a.extractTemplates != nil {
		var err error
		templates, err = a.extractTemplates.List(r.Context())
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
