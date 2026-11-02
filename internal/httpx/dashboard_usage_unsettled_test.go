// dashboard_usage_unsettled_test.go — the Usage & billing renders of the new
// liability surface: the Unsettled KPI and per-subject/key columns on
// Overview, budget lifecycle statuses on the Budgets tab, and money-formatted
// budget events in the Audit tab.
package httpx

import (
	"context"
	"net/http"
	"net/http/httptest"
	"strings"
	"testing"
	"time"

	"enclava-go/internal/auth"
	"enclava-go/internal/billing"
	"enclava-go/internal/store/postgres"
)

func TestDashboardUsageOverviewUnsettled(t *testing.T) {
	f := newBudgetFixture(t)
	svc := billing.NewService(f.repo)
	admin := f.adminPrincipal()
	ctx := context.Background()
	key := seedOwnedKey(t, f, "ov-key")

	if err := f.repo.PutPrice(ctx, f.tenant, "ov-price", billing.Price{
		SubjectType: billing.PriceSubjectModel, SubjectID: "m", Capability: "chat",
		Unit: billing.UnitInput, RateMicros: 100, UnitQuantity: 1, Currency: "USD",
	}, "tester", time.Now().UTC().Add(-time.Minute)); err != nil {
		t.Fatalf("put price: %v", err)
	}
	keyPrincipal := admin
	keyPrincipal.CredentialKind = "api_key"
	keyPrincipal.CredentialID = key.ID
	keyPrincipal.BillingSubjectKind = billing.BudgetSubjectUser
	keyPrincipal.BillingSubjectID = "owner-1"
	res, err := svc.Reserve(ctx, keyPrincipal, billing.ReserveRequest{
		SubjectKeys:    []billing.Subject{{Type: billing.BudgetSubjectAPIKey, ID: key.ID}},
		BillingSubject: billing.Subject{Type: billing.BudgetSubjectUser, ID: "owner-1"},
		Capability:     "chat", Model: "m", EndpointID: "ep",
		Estimate: billing.Usage{InputUnits: 2500},
		Price:    billing.PriceKey{SubjectType: billing.PriceSubjectModel, SubjectID: "m"},
	})
	if err != nil {
		t.Fatalf("reserve: %v", err)
	}
	if err := svc.MarkUnknown(ctx, keyPrincipal, res.ID); err != nil {
		t.Fatalf("mark unknown: %v", err)
	}
	// A settled reservation puts the owner/key into the spend tables — the
	// unsettled column then joins to it.
	settled, err := svc.Reserve(ctx, keyPrincipal, billing.ReserveRequest{
		SubjectKeys:    []billing.Subject{{Type: billing.BudgetSubjectAPIKey, ID: key.ID}},
		BillingSubject: billing.Subject{Type: billing.BudgetSubjectUser, ID: "owner-1"},
		Capability:     "chat", Model: "m", EndpointID: "ep",
		Estimate: billing.Usage{InputUnits: 10},
		Price:    billing.PriceKey{SubjectType: billing.PriceSubjectModel, SubjectID: "m"},
	})
	if err != nil {
		t.Fatalf("reserve settled: %v", err)
	}
	if _, err := svc.Settle(ctx, keyPrincipal, settled.ID, billing.Usage{InputUnits: 10}); err != nil {
		t.Fatalf("settle: %v", err)
	}

	body := renderUsagePage(t, f, f.app.handleDashboardUsagePage, "/dashboard/usage")
	// The KPI shows the 2500-micro unknown bound ($0.25).
	if !strings.Contains(body, "Unsettled") || !strings.Contains(body, "$0.25") {
		t.Fatalf("unsettled KPI missing: %s", body)
	}
	// Per-subject and per-key rows carry the same unsettled money.
	if !strings.Contains(body, "Spend by subject") || !strings.Contains(body, "owner-1") {
		t.Fatalf("subject table must render the owner row: %s", body)
	}
	if !strings.Contains(body, "ov-key") {
		t.Fatalf("key table must render the key row: %s", body)
	}
}

// TestDashboardUsageUnsettledMixedCurrencies covers two review findings:
// the Unsettled KPI must keep each currency's sum separate (€1.00 + $2.00,
// never €3.00), and a subject or API key whose only liability is held —
// zero settled usage — must still render a breakdown row.
func TestDashboardUsageUnsettledMixedCurrencies(t *testing.T) {
	f := newBudgetFixture(t)
	svc := billing.NewService(f.repo)
	admin := f.adminPrincipal()
	ctx := context.Background()
	priced := time.Now().UTC().Add(-time.Minute)

	for id, p := range map[string]billing.Price{
		"mx-usd": {SubjectType: billing.PriceSubjectModel, SubjectID: "m-usd", Capability: "chat",
			Unit: billing.UnitInput, RateMicros: 1_000_000, UnitQuantity: 1, Currency: "USD"},
		"mx-eur": {SubjectType: billing.PriceSubjectModel, SubjectID: "m-eur", Capability: "chat",
			Unit: billing.UnitInput, RateMicros: 1_000_000, UnitQuantity: 1, Currency: "EUR"},
	} {
		if err := f.repo.PutPrice(ctx, f.tenant, id, p, "tester", priced); err != nil {
			t.Fatalf("put price %s: %v", id, err)
		}
	}

	keyUSD := seedOwnedKey(t, f, "usd-key")
	keyEUR := seedOwnedKey(t, f, "eur-key")
	hold := func(key auth.APIKeyRecord, owner, model string, units int64) {
		t.Helper()
		p := admin
		p.CredentialKind = "api_key"
		p.CredentialID = key.ID
		p.BillingSubjectKind = billing.BudgetSubjectUser
		p.BillingSubjectID = owner
		if _, err := svc.Reserve(ctx, p, billing.ReserveRequest{
			SubjectKeys:    []billing.Subject{{Type: billing.BudgetSubjectAPIKey, ID: key.ID}},
			BillingSubject: billing.Subject{Type: billing.BudgetSubjectUser, ID: owner},
			Capability:     "chat", Model: model, EndpointID: "ep",
			Estimate: billing.Usage{InputUnits: units},
			Price:    billing.PriceKey{SubjectType: billing.PriceSubjectModel, SubjectID: model},
		}); err != nil {
			t.Fatalf("reserve %s %s: %v", model, owner, err)
		}
	}
	// $2.00 held under owner-1/usd-key; €1.00 held under owner-2/eur-key.
	// Nothing settles — owner-2 and eur-key have no settled usage at all.
	hold(keyUSD, "owner-1", "m-usd", 2)
	hold(keyEUR, "owner-2", "m-eur", 1)

	body := renderUsagePage(t, f, f.app.handleDashboardUsagePage, "/dashboard/usage")
	if !strings.Contains(body, "$2.00") || !strings.Contains(body, "€1.00") {
		t.Fatalf("unsettled KPI must keep currencies apart: %s", body)
	}
	if strings.Contains(body, "€3.00") || strings.Contains(body, "$3.00") {
		t.Fatalf("unsettled KPI merged currencies into the first code: %s", body)
	}
	// Held-only subject and key get zero-spend rows carrying their liability.
	if !strings.Contains(body, "owner-2") {
		t.Fatalf("held-only subject must render a by-subject row: %s", body)
	}
	if !strings.Contains(body, "eur-key") {
		t.Fatalf("held-only key must render a by-key row: %s", body)
	}
	// Spend-by-subject itself must not claim emptiness beside liability.
	subjectTable := body[strings.Index(body, "Spend by subject"):]
	if i := strings.Index(subjectTable, "Spend by API key"); i >= 0 {
		subjectTable = subjectTable[:i]
	}
	if strings.Contains(subjectTable, "No usage in this range") {
		t.Fatalf("subject table must not claim emptiness beside liability: %s", body)
	}
}

func TestDashboardUsageBudgetStatuses(t *testing.T) {
	f := newBudgetFixture(t)
	ctx := context.Background()
	now := time.Now().UTC()

	create := func(id string, starts, ends *time.Time) {
		t.Helper()
		if err := f.repo.CreateBudget(ctx, f.tenant, id, billing.Budget{
			ID: id, Name: id, Subject: billing.Subject{Type: billing.BudgetSubjectTenant, ID: f.tenant},
			Kind: billing.BudgetKindTokens, Period: billing.PeriodMonthly,
			LimitAmountMicros: 100, StartsAt: *starts, EndsAt: ends,
		}); err != nil {
			t.Fatalf("create %s: %v", id, err)
		}
	}
	past := now.Add(-time.Hour)
	future := now.Add(time.Hour)
	ended := now.Add(-time.Minute)
	create("b-on", &past, nil)
	create("b-later", &future, nil)
	create("b-done", &past, &ended)

	// requireCap normally stashes caps; direct handler calls inject them.
	req := httptest.NewRequest(http.MethodGet, "/dashboard/partials/usage/budgets", nil)
	req = req.WithContext(withCaps(auth.WithPrincipal(req.Context(), usageAdminPrincipal(f.tenant)),
		dashboardCaps{BillingView: true}))
	rec := httptest.NewRecorder()
	f.app.handleDashboardUsageBudgetsFragment(rec, req)
	if rec.Code != http.StatusOK {
		t.Fatalf("budgets fragment = %d: %s", rec.Code, rec.Body.String())
	}
	body := rec.Body.String()
	for _, want := range []string{"Ready", "Scheduled", "Ended", "UTC"} {
		if !strings.Contains(body, want) {
			t.Fatalf("budget row missing %q: %s", want, body)
		}
	}
	if !strings.Contains(body, "Deactivate") {
		t.Fatalf("active budgets must offer deactivate: %s", body)
	}
}

func TestDashboardUsageAuditBudgetMoneyRendering(t *testing.T) {
	f := newBudgetFixture(t)
	ctx := context.Background()
	now := time.Now().UTC()

	// Amount-budget events render their limit as money in the recorded
	// currency; token budgets render counted units.
	if err := f.repo.CreateBillingAudit(ctx, f.tenant, postgres.BillingAuditRow{
		ID: NewID("billaudit"), SubjectType: billing.BudgetSubjectTenant, SubjectID: f.tenant,
		Action: "budget.limit_updated", ActorType: "user", ActorID: "admin-1",
		Detail:    map[string]any{"limit_value": 500000, "kind": "amount", "currency": "USD"},
		CreatedAt: now,
	}); err != nil {
		t.Fatalf("seed amount audit: %v", err)
	}
	if err := f.repo.CreateBillingAudit(ctx, f.tenant, postgres.BillingAuditRow{
		ID: NewID("billaudit"), SubjectType: billing.BudgetSubjectTenant, SubjectID: f.tenant,
		Action: "budget.limit_updated", ActorType: "user", ActorID: "admin-1",
		Detail:    map[string]any{"limit_value": 9000, "kind": "tokens", "currency": ""},
		CreatedAt: now,
	}); err != nil {
		t.Fatalf("seed token audit: %v", err)
	}

	body := renderUsagePage(t, f, f.app.handleDashboardUsageAuditFragment, "/dashboard/usage/audit")
	if !strings.Contains(body, "$0.50") {
		t.Fatalf("amount budget limit must render as money: %s", body)
	}
	if !strings.Contains(body, "9,000 units") {
		t.Fatalf("token budget limit must render as units: %s", body)
	}
}

func TestHomeShowsUnsettled(t *testing.T) {
	f := newBudgetFixture(t)
	svc := billing.NewService(f.repo)
	admin := f.adminPrincipal()
	ctx := context.Background()

	if err := f.repo.PutPrice(ctx, f.tenant, "home-price", billing.Price{
		SubjectType: billing.PriceSubjectModel, SubjectID: "m", Capability: "chat",
		Unit: billing.UnitInput, RateMicros: 100, UnitQuantity: 1, Currency: "USD",
	}, "tester", time.Now().UTC().Add(-time.Minute)); err != nil {
		t.Fatalf("put price: %v", err)
	}
	if _, err := svc.Reserve(ctx, admin, billing.ReserveRequest{
		Capability: "chat", Model: "m", EndpointID: "ep",
		Estimate: billing.Usage{InputUnits: 42},
		Price:    billing.PriceKey{SubjectType: billing.PriceSubjectModel, SubjectID: "m"},
	}); err != nil {
		t.Fatalf("reserve: %v", err)
	}

	m, err := f.app.dashboardHomeMetrics(ctx, admin, dashboardCaps{BillingView: true})
	if err != nil {
		t.Fatalf("metrics: %v", err)
	}
	found := false
	for _, amt := range m.Unsettled30d {
		if amt.Currency == "USD" && amt.AmountMicros == 4200 {
			found = true
		}
	}
	if !found {
		t.Fatalf("home unsettled missing USD 4200: %+v", m.Unsettled30d)
	}
}

// TestAdminUsageSummarySmoke guards the route wiring end to end through the
// registered mux (the handler tests above drive the func directly).
func TestAdminUsageSummarySmoke(t *testing.T) {
	f := newBudgetFixture(t)
	svc := billing.NewService(f.repo)
	if err := f.repo.PutPrice(context.Background(), f.tenant, "smoke-price", billing.Price{
		SubjectType: billing.PriceSubjectModel, SubjectID: "m", Capability: "chat",
		Unit: billing.UnitInput, RateMicros: 100, UnitQuantity: 1, Currency: "USD",
	}, "tester", time.Now().UTC().Add(-time.Minute)); err != nil {
		t.Fatalf("put price: %v", err)
	}
	admin := f.adminPrincipal()
	if _, err := svc.Reserve(context.Background(), admin, billing.ReserveRequest{
		Capability: "chat", Model: "m", EndpointID: "ep",
		Estimate: billing.Usage{InputUnits: 10},
		Price:    billing.PriceKey{SubjectType: billing.PriceSubjectModel, SubjectID: "m"},
	}); err != nil {
		t.Fatalf("reserve: %v", err)
	}
	mux := http.NewServeMux()
	mux.HandleFunc("/api/v1/admin/usage/summary", func(w http.ResponseWriter, r *http.Request) {
		f.app.handleAdminUsageSummary(w, r, admin)
	})
	rec := httptest.NewRecorder()
	mux.ServeHTTP(rec, httptest.NewRequest(http.MethodGet, "/api/v1/admin/usage/summary", nil))
	if rec.Code != http.StatusOK || !strings.Contains(rec.Body.String(), "unsettled") {
		t.Fatalf("summary smoke = %d: %s", rec.Code, rec.Body.String())
	}
}
