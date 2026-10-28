// dashboard_usage_test.go — the Usage & billing page (OA-29, plan 09-08).
// Handoff note for 09-10: this plan ALREADY deleted the six legacy billing
// registrations (dashboard.go: /dashboard/{analytics,budgets,billing-audit}
// + /dashboard/partials/{usage,budgets,billing-audit}), their handler funcs
// (handleDashboardPageAnalytics, handleDashboardPageBudgets,
// handleDashboardUsage, handleDashboardBudgets,
// handleDashboardPageBillingAudit, handleDashboardBillingAudit), the
// dashboardAnalyticsData/dashboardBudgetsData/dashboardBillingAuditData
// models and the five legacy templates (page_analytics, page_budgets,
// partial_usage, partial_budgets, partial_billing_audit). budgets_test.go
// and dashboard_authz_test.go were migrated in the same change — nothing
// billing-related remains in the legacy sweep (Home/stats went with 10-01).
package httpx

import (
	"bytes"
	"context"
	"fmt"
	"net/http"
	"net/http/httptest"
	"strings"
	"testing"
	"time"

	"enclava-go/internal/auth"
	"enclava-go/internal/billing"
	"enclava-go/internal/store/postgres"
)

// usageAdminPrincipal is a platform.admin actor for direct handler calls;
// the session guard is exercised separately in TestDashboardUsageMemberDenied.
func usageAdminPrincipal(tenantID string) auth.Principal {
	return auth.Principal{
		TenantID: tenantID, ActorID: "admin-1", ActorKind: auth.ActorKindUser,
		UserID: "admin-1", Scopes: map[string]struct{}{auth.ScopePlatformAdmin: {}},
	}
}

// renderUsagePage drives one usage page/fragment handler directly with an
// admin principal (registration-time guards need no session here).
func renderUsagePage(t *testing.T, f budgetFixture, handler func(http.ResponseWriter, *http.Request), path string) string {
	t.Helper()
	req := httptest.NewRequest(http.MethodGet, path, nil)
	req = req.WithContext(auth.WithPrincipal(req.Context(), usageAdminPrincipal(f.tenant)))
	rec := httptest.NewRecorder()
	handler(rec, req)
	if rec.Code != http.StatusOK {
		t.Fatalf("GET %s = %d: %s", path, rec.Code, rec.Body.String())
	}
	return rec.Body.String()
}

func TestDashboardUsageMemberDenied(t *testing.T) {
	f := newIdentityFixture(t)
	f.app.billingRepo = postgres.NewBillingRepo(f.db)

	memberSess, _ := loginUser(t, f, "usage-member@example.com", []string{"chat.completions"}, "member-pass")
	// A manager holds feature/admin-adjacent scopes that never map to
	// billing.view (only platform.admin does per §3.1).
	managerSess, _ := loginUser(t, f, "usage-manager@example.com",
		[]string{"chatbot.manage", "rag.manage", "pricing.manage", "extract.manage", "connectors.manage"},
		"manager-pass")

	usageRoutes := []string{
		"/dashboard/usage",
		"/dashboard/usage/budgets",
		"/dashboard/usage/audit",
		"/dashboard/partials/usage/overview",
		"/dashboard/partials/usage/budgets",
		"/dashboard/partials/usage/audit",
		"/dashboard/partials/usage/budget-drawer",
	}
	for _, path := range usageRoutes {
		for _, who := range []struct {
			name string
			sess *http.Cookie
		}{{"member", memberSess}, {"manager", managerSess}} {
			rec := sessionDo(t, f.app, http.MethodGet, path, "", who.sess, nil)
			if rec.Code != http.StatusForbidden {
				t.Fatalf("%s GET %s = %d, want 403: %s", who.name, path, rec.Code, rec.Body.String())
			}
		}
	}

	// The old member-readable budgets partial died with the cutover: unknown
	// dashboard paths are the designed 404, never a resurrected surface.
	rec := sessionDo(t, f.app, http.MethodGet, "/dashboard/partials/budgets", "", memberSess, nil)
	if rec.Code == http.StatusOK || rec.Code == http.StatusForbidden {
		t.Fatalf("deleted /dashboard/partials/budgets must not serve, got %d", rec.Code)
	}

	// platform.admin opens the page.
	adminSess, _ := loginUser(t, f, "usage-admin@example.com", []string{auth.ScopePlatformAdmin}, "admin-pass")
	rec = sessionDo(t, f.app, http.MethodGet, "/dashboard/usage", "", adminSess, nil)
	if rec.Code != http.StatusOK {
		t.Fatalf("admin GET /dashboard/usage = %d: %s", rec.Code, rec.Body.String())
	}
	if !strings.Contains(rec.Body.String(), "Usage &amp; billing") {
		t.Fatalf("admin usage page did not render: %s", rec.Body.String())
	}
}

func TestDashboardUsageOverviewRanges(t *testing.T) {
	f := newBudgetFixture(t)
	now := time.Now().UTC()
	subject := billing.Subject{Type: billing.BudgetSubjectTenant, ID: f.tenant}
	// One settled usage row on day -1, -10, -40 and -80; units 1000 each.
	for i, age := range []time.Duration{24 * time.Hour, 10 * 24 * time.Hour, 40 * 24 * time.Hour, 80 * 24 * time.Hour} {
		seedSettledUsage(t, f, fmt.Sprintf("range-%d", i), subject, 1000, 1_000_000, "USD", now.Add(-age))
	}

	render := func(query string) string {
		return renderUsagePage(t, f, f.app.handleDashboardUsagePage, "/dashboard/usage"+query)
	}

	default30 := render("")
	if !strings.Contains(default30, `href="/dashboard/usage?range=30" aria-pressed="true"`) {
		t.Fatalf("default range must be 30: %s", default30)
	}
	if got := strings.Count(default30, "<rect"); got != 2 {
		t.Fatalf("30-day chart bars = %d, want 2 (day -1 and -10): %s", got, default30)
	}
	if !strings.Contains(default30, ">2,000<") {
		t.Fatalf("requests KPI must aggregate exactly the 30d window (2 rows): %s", default30)
	}

	seven := render("?range=7")
	if !strings.Contains(seven, `href="/dashboard/usage?range=7" aria-pressed="true"`) {
		t.Fatalf("range=7 chip must be active: %s", seven)
	}
	if got := strings.Count(seven, "<rect"); got != 1 {
		t.Fatalf("7-day chart bars = %d, want 1: %s", got, seven)
	}
	if strings.Contains(seven, ">2,000<") {
		t.Fatalf("7-day KPI must not leak the 30-day total: %s", seven)
	}

	ninety := render("?range=90")
	if got := strings.Count(ninety, "<rect"); got != 4 {
		t.Fatalf("90-day chart bars = %d, want 4: %s", got, ninety)
	}
	if !strings.Contains(ninety, ">4,000<") {
		t.Fatalf("90-day requests KPI must aggregate all four rows: %s", ninety)
	}
	// Each bar's native tooltip names its UTC day and its request count
	// (one settled usage record per seeded day).
	if !strings.Contains(ninety, now.Add(-24*time.Hour).Format("2006-01-02")+" · 1 request") {
		t.Fatalf("daily bar title missing day+count: %s", ninety)
	}
	// Spend by model/team/key tables render real rows from the same window.
	if !strings.Contains(ninety, "m-test") || !strings.Contains(ninety, "tenant:"+f.tenant) {
		t.Fatalf("spend-by tables must render from the billing reads: %s", ninety)
	}
}

func TestDashboardUsageBudgetThresholds(t *testing.T) {
	f := newBudgetFixture(t)
	ctx := context.Background()
	now := time.Now().UTC()

	// Exact §6.12 boundaries on the pure projection.
	for _, tc := range []struct {
		counted, limit int64
		want           string
	}{
		{0, 100, ""},
		{79, 100, ""},
		{80, 100, "warn"},
		{94, 100, "warn"},
		{95, 100, "bad"},
		{200, 100, "bad"},
		{50, 0, ""}, // unlimited budgets never tone
	} {
		if got := usageBudgetTone(tc.counted, tc.limit); got != tc.want {
			t.Fatalf("usageBudgetTone(%d,%d) = %q, want %q", tc.counted, tc.limit, got, tc.want)
		}
	}

	// Rendered rows: a budget at exactly 80% tones warn, at 95% tones bad.
	// Each budget binds its OWN user subject — budgets sharing a subject
	// would see each other's liability in counted.
	for _, tc := range []struct {
		id      string
		subject billing.Subject
		pct     int64
		tone    string
	}{
		{"budget-warn", billing.Subject{Type: billing.BudgetSubjectUser, ID: "warn-user"}, 80, "warn"},
		{"budget-bad", billing.Subject{Type: billing.BudgetSubjectUser, ID: "bad-user"}, 95, "bad"},
	} {
		if err := f.repo.CreateBudget(ctx, f.tenant, tc.id, billing.Budget{
			Name: tc.id, Subject: tc.subject, Kind: billing.BudgetKindAmount,
			Period: billing.PeriodMonthly, LimitAmountMicros: 100, Currency: "USD",
			StartsAt: now.Add(-time.Hour),
		}); err != nil {
			t.Fatalf("create %s: %v", tc.id, err)
		}
		seedSettledUsage(t, f, tc.id, tc.subject, 1, tc.pct, "USD", now.Add(-time.Minute))
	}

	body := renderUsagePage(t, f, f.app.handleDashboardUsageBudgetsFragment, "/dashboard/partials/usage/budgets")
	if !strings.Contains(body, `class="progress warn"`) || !strings.Contains(body, `aria-valuenow="80"`) {
		t.Fatalf("80%% budget must render the warn tone: %s", body)
	}
	if !strings.Contains(body, `class="progress bad"`) || !strings.Contains(body, `aria-valuenow="95"`) {
		t.Fatalf("95%% budget must render the bad tone: %s", body)
	}
	if !strings.Contains(body, "Held $0.00") || !strings.Contains(body, "Settled $0.00") {
		t.Fatalf("informational held/settled liability rows must render: %s", body)
	}

	// Unlimited progress renders "No limit" instead of a bar.
	set, err := dashboardTemplateSet("usage")
	if err != nil {
		t.Fatalf("template set: %v", err)
	}
	var buf bytes.Buffer
	view := usageView{Budgets: []usageBudgetRow{{ID: "b1", Name: "open", Unlimited: true}}}
	if err := set.ExecuteTemplate(&buf, "frag-usage-budgets", dashboardView{Data: view}); err != nil {
		t.Fatalf("render unlimited budget row: %v", err)
	}
	unlimited := buf.String()
	if !strings.Contains(unlimited, "No limit") || strings.Contains(unlimited, "progressbar") {
		t.Fatalf("unlimited budget must show No limit and no bar: %s", unlimited)
	}
}

func TestDashboardUsageAuditFilters(t *testing.T) {
	f := newBudgetFixture(t)
	ctx := context.Background()
	now := time.Now().UTC()

	// Two billing events with different subjects and actors…
	if err := f.repo.CreateBillingAudit(ctx, f.tenant, postgres.BillingAuditRow{
		ID: NewID("billaudit"), SubjectType: billing.BudgetSubjectAPIKey, SubjectID: "key-1",
		Action: "api_key.created", ActorType: "user", ActorID: "admin-1",
		Detail: map[string]any{}, CreatedAt: now,
	}); err != nil {
		t.Fatalf("seed api key audit: %v", err)
	}
	if err := f.repo.CreateBillingAudit(ctx, f.tenant, postgres.BillingAuditRow{
		ID: NewID("billaudit"), SubjectType: billing.BudgetSubjectTenant, SubjectID: f.tenant,
		Action: "budget.created", ActorType: "user", ActorID: "admin-2",
		Detail: map[string]any{"limit_value": 5000, "currency": "USD"}, CreatedAt: now,
	}); err != nil {
		t.Fatalf("seed budget audit: %v", err)
	}
	// …one backdated billing event (CreateBillingAudit stamps created_at at
	// insert, so the row is backdated explicitly like the usage seeder)…
	backdated := postgres.BillingAuditRow{
		ID: NewID("billaudit"), SubjectType: billing.BudgetSubjectTenant, SubjectID: f.tenant,
		Action: "budget.limit_updated", ActorType: "user", ActorID: "admin-2",
		Detail: map[string]any{"limit_value": 9000, "currency": "USD"}, CreatedAt: now.Add(-72 * time.Hour),
	}
	if err := f.repo.CreateBillingAudit(ctx, f.tenant, backdated); err != nil {
		t.Fatalf("seed backdated audit: %v", err)
	}
	if _, err := f.db.SQL.ExecContext(ctx,
		`UPDATE billing_audit SET created_at = $2 WHERE tenant_id = $1::uuid AND id = $3`,
		f.tenant, backdated.CreatedAt, backdated.ID); err != nil {
		t.Fatalf("backdate billing audit: %v", err)
	}
	// …and one pricing event — the merged source ListPriceAudit covers.
	if err := f.repo.PutPrice(ctx, f.tenant, "audit-price", billing.Price{
		SubjectType: billing.PriceSubjectModel, SubjectID: "m", Capability: "chat",
		Unit: billing.UnitInput, RateMicros: 1_000_000, UnitQuantity: 1, Currency: "USD",
	}, "admin-1", now); err != nil {
		t.Fatalf("put price (writes pricing audit): %v", err)
	}

	renderAudit := func(query string) string {
		return renderUsagePage(t, f, f.app.handleDashboardUsageAuditFragment, "/dashboard/usage/audit"+query)
	}

	all := renderAudit("")
	// Billing AND pricing events appear in one list, kinds offered as chips.
	for _, kind := range []string{"api_key.created", "budget.created", "budget.limit_updated", "price.create"} {
		if !strings.Contains(all, kind) {
			t.Fatalf("merged audit must contain %s: %s", kind, all)
		}
		if !strings.Contains(all, fmt.Sprintf(">%s</a>", kind)) {
			t.Fatalf("kind chip for %s missing: %s", kind, all)
		}
	}
	if !strings.Contains(all, "$1.00") {
		t.Fatalf("price event must surface its rate amount: %s", all)
	}

	kindOnly := renderAudit("?kind=price.create")
	// The chip rail lists every kind; the filtered table must hold only the
	// pricing event.
	if !strings.Contains(kindOnly, "Price create") || strings.Contains(kindOnly, "Api key created") {
		t.Fatalf("kind filter must narrow table rows to pricing events: %s", kindOnly)
	}

	subjectOnly := renderAudit("?subject_id=key-1")
	if !strings.Contains(subjectOnly, "Api key created") || strings.Contains(subjectOnly, "Budget created") {
		t.Fatalf("subject filter must narrow rows: %s", subjectOnly)
	}

	actorOnly := renderAudit("?actor=admin-2")
	if !strings.Contains(actorOnly, "Budget created") || strings.Contains(actorOnly, "Api key created") {
		t.Fatalf("actor filter must narrow rows: %s", actorOnly)
	}

	today := renderAudit("?from=" + now.Format("2006-01-02"))
	if strings.Contains(today, "<b>budget.limit_updated</b>") {
		t.Fatalf("date range filter must exclude the backdated event: %s", today)
	}

	// limit/offset paging over the merged result — never a capped silent list.
	paged := renderAudit("?limit=2")
	if !strings.Contains(paged, "4 events") || !strings.Contains(paged, "Older") {
		t.Fatalf("paging must expose total and an Older link: %s", paged)
	}
	nextHref := usageAuditHref(usageAuditFilters{}, 2, 2)
	// href attributes render HTML-escaped (&amp;) — same resolved URL.
	escaped := strings.ReplaceAll(nextHref, "&", "&amp;")
	if !strings.Contains(paged, `href="`+escaped+`"`) {
		t.Fatalf("Older link must carry offset paging: %s (want %s)", paged, nextHref)
	}
}
