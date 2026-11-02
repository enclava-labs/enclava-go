// budgets_status_test.go — the full-list budget lifecycle (scheduled,
// active, ended, inactive), DELETE deactivation, the tenant-currency binding
// rule for amount budgets and the IANA timezone validation.
package httpx

import (
	"context"
	"encoding/json"
	"fmt"
	"net/http"
	"net/http/httptest"
	"strings"
	"testing"
	"time"

	"enclava-go/internal/auth"
	"enclava-go/internal/billing"
)

// listBudgetStatuses returns id → status for every row the admin list emits.
func listBudgetStatuses(t *testing.T, f budgetFixture, admin auth.Principal) map[string]string {
	t.Helper()
	rec := doBudgetHandler(f.app.handleAdminBudgets, admin,
		httptest.NewRequest(http.MethodGet, "/api/v1/admin/budgets", nil))
	if rec.Code != http.StatusOK {
		t.Fatalf("list = %d: %s", rec.Code, rec.Body.String())
	}
	var out struct {
		Budgets []budgetView `json:"budgets"`
	}
	if err := json.Unmarshal(rec.Body.Bytes(), &out); err != nil {
		t.Fatalf("decode: %v", err)
	}
	statuses := map[string]string{}
	for _, v := range out.Budgets {
		statuses[v.Budget.ID] = v.Budget.Status
	}
	return statuses
}

func TestAdminBudgetListStatusesAndDelete(t *testing.T) {
	f := newBudgetFixture(t)
	admin := f.adminPrincipal()
	ctx := context.Background()
	now := time.Now().UTC()

	create := func(id string, b billing.Budget) {
		t.Helper()
		b.ID = id
		if err := f.repo.CreateBudget(ctx, f.tenant, id, b); err != nil {
			t.Fatalf("create %s: %v", id, err)
		}
	}
	past := now.Add(-time.Hour)
	future := now.Add(time.Hour)
	later := now.Add(2 * time.Hour)
	create("b-active", billing.Budget{Name: "a", Subject: billing.Subject{Type: billing.BudgetSubjectTenant, ID: f.tenant},
		Kind: billing.BudgetKindTokens, Period: billing.PeriodMonthly, LimitAmountMicros: 100, StartsAt: past})
	create("b-scheduled", billing.Budget{Name: "s", Subject: billing.Subject{Type: billing.BudgetSubjectTenant, ID: f.tenant},
		Kind: billing.BudgetKindTokens, Period: billing.PeriodMonthly, LimitAmountMicros: 100, StartsAt: future})
	create("b-ended", billing.Budget{Name: "e", Subject: billing.Subject{Type: billing.BudgetSubjectTenant, ID: f.tenant},
		Kind: billing.BudgetKindTokens, Period: billing.PeriodMonthly, LimitAmountMicros: 100, StartsAt: past, EndsAt: &later})
	endedAt := now.Add(-time.Minute)
	create("b-ended2", billing.Budget{Name: "e2", Subject: billing.Subject{Type: billing.BudgetSubjectTenant, ID: f.tenant},
		Kind: billing.BudgetKindTokens, Period: billing.PeriodMonthly, LimitAmountMicros: 100, StartsAt: past, EndsAt: &endedAt})

	statuses := listBudgetStatuses(t, f, admin)
	if statuses["b-active"] != "active" || statuses["b-scheduled"] != "scheduled" ||
		statuses["b-ended2"] != "ended" {
		t.Fatalf("statuses wrong: %+v", statuses)
	}
	// ends_at in the future is not "ended" until it passes.
	if statuses["b-ended"] != "active" {
		t.Fatalf("future ends_at must still be active, got %q", statuses["b-ended"])
	}

	// A binding budget refuses admission; after DELETE it no longer does.
	svc := billing.NewService(f.repo)
	if err := f.repo.PutPrice(ctx, f.tenant, "del-price", billing.Price{
		SubjectType: billing.PriceSubjectModel, SubjectID: "m", Capability: "chat",
		Unit: billing.UnitInput, RateMicros: 1, UnitQuantity: 1, Currency: "USD",
	}, "tester", past); err != nil {
		t.Fatalf("put price: %v", err)
	}
	if _, err := f.db.SQL.ExecContext(ctx,
		`UPDATE budgets SET limit_value = 1 WHERE tenant_id = $1::uuid AND id = 'b-active'`, f.tenant); err != nil {
		t.Fatalf("shrink limit: %v", err)
	}
	reserve := func() error {
		_, err := svc.Reserve(ctx, admin, billing.ReserveRequest{
			Capability: "chat", Model: "m", EndpointID: "ep",
			Estimate: billing.Usage{InputUnits: 50},
			Price:    billing.PriceKey{SubjectType: billing.PriceSubjectModel, SubjectID: "m"},
		})
		return err
	}
	if err := reserve(); err == nil {
		t.Fatal("expected the tiny budget to refuse the reservation")
	}

	deleteReq := httptest.NewRequest(http.MethodDelete, "/api/v1/admin/budgets/b-active", nil)
	rec := doBudgetHandler(f.app.handleAdminBudgetByID, admin, deleteReq)
	if rec.Code != http.StatusOK {
		t.Fatalf("delete = %d: %s", rec.Code, rec.Body.String())
	}
	if got := decodeBudgetView(t, rec.Body.String()).Budget.Status; got != "inactive" {
		t.Fatalf("deleted status = %q, want inactive", got)
	}
	// Idempotent: the second delete is a quiet 200 with one audit row.
	rec = doBudgetHandler(f.app.handleAdminBudgetByID, admin,
		httptest.NewRequest(http.MethodDelete, "/api/v1/admin/budgets/b-active", nil))
	if rec.Code != http.StatusOK {
		t.Fatalf("second delete = %d: %s", rec.Code, rec.Body.String())
	}
	var auditN int
	if err := f.db.SQL.QueryRowContext(ctx,
		`SELECT count(*) FROM billing_audit WHERE tenant_id = $1::uuid AND action = 'budget.deactivated'`,
		f.tenant).Scan(&auditN); err != nil || auditN != 1 {
		t.Fatalf("deactivation audit rows = %d (%v), want 1", auditN, err)
	}
	statuses = listBudgetStatuses(t, f, admin)
	if statuses["b-active"] != "inactive" {
		t.Fatalf("list must keep the deactivated row: %+v", statuses)
	}
	if err := reserve(); err != nil {
		t.Fatalf("deactivated budget must not refuse admission: %v", err)
	}
	if rec := doBudgetHandler(f.app.handleAdminBudgetByID, admin,
		httptest.NewRequest(http.MethodDelete, "/api/v1/admin/budgets/b_nope", nil)); rec.Code != http.StatusNotFound {
		t.Fatalf("delete unknown = %d, want 404", rec.Code)
	}
}

// An ended budget projects over its final applicable window only: settled
// usage inside that window counts, anything recorded after ends_at — or in
// the current period — must not leak into the row.
func TestAdminBudgetListEndedBudgetFinalWindow(t *testing.T) {
	f := newBudgetFixture(t)
	admin := f.adminPrincipal()
	ctx := context.Background()
	now := time.Now().UTC()
	endsAt := now.Add(-24 * time.Hour)

	budget := billing.Budget{
		Name: "ended", Subject: billing.Subject{Type: billing.BudgetSubjectTenant, ID: f.tenant},
		Kind: billing.BudgetKindTokens, Period: billing.PeriodMonthly,
		LimitAmountMicros: 1_000_000, StartsAt: now.Add(-72 * time.Hour), EndsAt: &endsAt,
	}
	if err := f.repo.CreateBudget(ctx, f.tenant, "ended-b", budget); err != nil {
		t.Fatalf("create: %v", err)
	}
	subject := billing.Subject{Type: billing.BudgetSubjectTenant, ID: f.tenant}
	// Inside the budget's final period but before ends_at vs recorded after it.
	insideAt := endsAt.Add(-time.Hour)
	if from := billing.LiabilityFrom(budget, endsAt.Add(-time.Nanosecond)); insideAt.Before(from) {
		insideAt = from
	}
	seedSettledUsage(t, f, "eb-in", subject, 5, 50, "USD", insideAt)
	seedSettledUsage(t, f, "eb-out", subject, 7, 70, "USD", endsAt.Add(time.Hour))

	rec := doBudgetHandler(f.app.handleAdminBudgets, admin,
		httptest.NewRequest(http.MethodGet, "/api/v1/admin/budgets", nil))
	if rec.Code != http.StatusOK {
		t.Fatalf("list = %d: %s", rec.Code, rec.Body.String())
	}
	var out struct {
		Budgets []budgetView `json:"budgets"`
	}
	if err := json.Unmarshal(rec.Body.Bytes(), &out); err != nil {
		t.Fatalf("decode: %v", err)
	}
	var got *budgetView
	for i := range out.Budgets {
		if out.Budgets[i].Budget.ID == "ended-b" {
			got = &out.Budgets[i]
		}
	}
	if got == nil {
		t.Fatal("ended budget missing from list")
	}
	if got.Budget.Status != "ended" {
		t.Fatalf("status = %q, want ended", got.Budget.Status)
	}
	if got.Progress.Counted != 5 {
		t.Fatalf("counted = %d, want 5 — post-ends_at usage must not count", got.Progress.Counted)
	}
}

func TestAdminBudgetCreateCurrencyMustMatchTenantPrices(t *testing.T) {
	f := newBudgetFixture(t)
	admin := f.adminPrincipal()
	ctx := context.Background()
	startsAt := time.Now().UTC().Add(-time.Hour).Format(time.RFC3339)
	body := func(cur string) string {
		return fmt.Sprintf(
			`{"name":"cur","subject_type":"tenant","kind":"amount","limit_value":100,"currency":%q,"period":"monthly","starts_at":%q}`,
			cur, startsAt)
	}

	// No prices yet: any valid currency binds.
	if rec := f.postBudget(t, admin, body("EUR")); rec.Code != http.StatusCreated {
		t.Fatalf("no-price create = %d: %s", rec.Code, rec.Body.String())
	}
	if err := f.repo.PutPrice(ctx, f.tenant, "p-usd", billing.Price{
		SubjectType: billing.PriceSubjectModel, SubjectID: "m", Capability: "chat",
		Unit: billing.UnitInput, RateMicros: 1, UnitQuantity: 1, Currency: "USD",
	}, "tester", time.Now().UTC().Add(-time.Minute)); err != nil {
		t.Fatalf("put price: %v", err)
	}
	if rec := f.postBudget(t, admin, body("USD")); rec.Code != http.StatusCreated {
		t.Fatalf("matching currency = %d: %s", rec.Code, rec.Body.String())
	}
	if rec := f.postBudget(t, admin, body("GBP")); rec.Code != http.StatusBadRequest {
		t.Fatalf("unpriced currency = %d, want 400: %s", rec.Code, rec.Body.String())
	}
}

func TestAdminBudgetCreateTimezone(t *testing.T) {
	f := newBudgetFixture(t)
	admin := f.adminPrincipal()
	startsAt := time.Now().UTC().Add(-time.Hour).Format(time.RFC3339)
	body := func(tz string) string {
		extra := ""
		if tz != "" {
			extra = fmt.Sprintf(`,"timezone":%q`, tz)
		}
		return fmt.Sprintf(
			`{"name":"tz","subject_type":"tenant","kind":"tokens","limit_value":100,"period":"daily","starts_at":%q%s}`,
			startsAt, extra)
	}

	if rec := f.postBudget(t, admin, body("Europe/Berlin")); rec.Code != http.StatusCreated {
		t.Fatalf("valid timezone = %d: %s", rec.Code, rec.Body.String())
	} else if view := decodeBudgetView(t, rec.Body.String()); view.Budget.Timezone != "Europe/Berlin" {
		t.Fatalf("timezone = %q, want Europe/Berlin", view.Budget.Timezone)
	}
	if rec := f.postBudget(t, admin, body("Not/AZone")); rec.Code != http.StatusBadRequest {
		t.Fatalf("invalid timezone = %d, want 400", rec.Code)
	}
	if rec := f.postBudget(t, admin, body("")); rec.Code != http.StatusCreated {
		t.Fatalf("omitted timezone = %d: %s", rec.Code, rec.Body.String())
	} else if view := decodeBudgetView(t, rec.Body.String()); view.Budget.Timezone != "UTC" {
		t.Fatalf("default timezone = %q, want UTC", view.Budget.Timezone)
	}
}

// A timezone-aware daily window: a budget bound to a zone whose midnight is
// still ahead sees the day boundary in its own zone, not UTC.
func TestAdminBudgetTimezoneWindow(t *testing.T) {
	f := newBudgetFixture(t)
	ctx := context.Background()
	now := time.Now().UTC()

	loc, err := time.LoadLocation("Pacific/Kiritimati") // UTC+14
	if err != nil {
		t.Skipf("zone unavailable: %v", err)
	}
	local := now.In(loc)
	// Liability created just after midnight Kiritimati-time is inside that
	// zone's window even when it predates the UTC day boundary.
	at := time.Date(local.Year(), local.Month(), local.Day(), 0, 30, 0, 0, loc)
	if err := f.repo.CreateBudget(ctx, f.tenant, "tz-budget", billing.Budget{
		Name: "tz", Subject: billing.Subject{Type: billing.BudgetSubjectTenant, ID: f.tenant},
		Kind: billing.BudgetKindTokens, Period: billing.PeriodDaily,
		LimitAmountMicros: 1000, Timezone: "Pacific/Kiritimati",
		StartsAt: now.Add(-48 * time.Hour),
	}); err != nil {
		t.Fatalf("create: %v", err)
	}
	row, err := f.repo.ReportGetBudget(ctx, f.tenant, "tz-budget")
	if err != nil {
		t.Fatalf("get: %v", err)
	}
	if row.Budget.Timezone != "Pacific/Kiritimati" {
		t.Fatalf("persisted timezone = %q", row.Budget.Timezone)
	}
	if from := billing.LiabilityFrom(row.Budget, now); from.After(at) {
		t.Fatalf("Kiritimati window start %v must cover usage at %v", from, at)
	}
	// The same instant under a UTC daily window sits before today's UTC
	// midnight only when the UTC day hasn't rolled yet (UTC hour < 10);
	// outside that gap the contrast isn't assertable, so keep it conditional.
	utcRow := row.Budget
	utcRow.Timezone = "UTC"
	if fromUTC := billing.LiabilityFrom(utcRow, now); now.Hour() < 10 && !fromUTC.After(at) {
		t.Fatalf("UTC window start %v should exclude %v", fromUTC, at)
	}
}

func TestAdminAPIKeyLegacyBudgetFields(t *testing.T) {
	f := newIdentityFixture(t)
	// Full resource authority so the created key's default all-mode grants
	// stay inside the caller's coverage (omitted grants mint as mode:all).
	sess, csrf := loginUserWithGrants(t, f, "legacy-admin@example.com",
		[]string{auth.ScopePlatformAdmin},
		auth.ResourceGrants{
			Models:           auth.Allowlist{All: true},
			Endpoints:        auth.Allowlist{All: true},
			ExtractTemplates: auth.Allowlist{All: true},
			Assistants:       auth.Allowlist{All: true},
		}, "pass")

	// Positive legacy caps are refused with the budgets-API pointer.
	for _, field := range []string{"budget_limit_tokens", "budget_limit_cents"} {
		rec := sessionDo(t, f.app, http.MethodPost, "/api/v1/admin/api-keys",
			fmt.Sprintf(`{"name":"k","scopes":[],"%s":5}`, field), sess, csrf)
		if rec.Code != http.StatusBadRequest || !strings.Contains(rec.Body.String(), "/api/v1/admin/budgets") {
			t.Fatalf("%s = %d, want 400 with budgets pointer: %s", field, rec.Code, rec.Body.String())
		}
	}
	// Compatibility no-ops still mint a key.
	rec := sessionDo(t, f.app, http.MethodPost, "/api/v1/admin/api-keys",
		`{"name":"k","scopes":[],"is_unlimited":true,"budget_period":"monthly","budget_limit_tokens":0,"budget_limit_cents":0}`,
		sess, csrf)
	if rec.Code != http.StatusCreated {
		t.Fatalf("compat create = %d: %s", rec.Code, rec.Body.String())
	}
}
