// on stored billing state (JOB-03/JOB-04): budgets and liabilities live in
// the postgres billing tables, seeded through BillingRepo.
package httpx

import (
	"context"
	"errors"
	"fmt"
	"net/http"
	"net/http/httptest"
	"strings"
	"sync/atomic"
	"testing"
	"time"

	"enclava-go/internal/auth"
	"enclava-go/internal/billing"
	"enclava-go/internal/store/postgres"
	"enclava-go/internal/testdb"
)

var budgetTestSeq atomic.Int64

type budgetFixture struct {
	db     *postgres.DB
	repo   *postgres.BillingRepo
	tenant string
	app    *App
}

func newBudgetFixture(t *testing.T) budgetFixture {
	t.Helper()
	db := testdb.New(t)
	repo := postgres.NewBillingRepo(db)
	tenant := testdb.SeedTenant(t, db, fmt.Sprintf("bt%d", budgetTestSeq.Add(1)))
	return budgetFixture{db: db, repo: repo, tenant: tenant, app: &App{billingRepo: repo}}
}

// seedSettledUsage writes one settled reservation plus its usage_records row
// attributed to subject, then backdates the usage row's created_at so period
// windows are exercised without sleeping.
func seedSettledUsage(t *testing.T, f budgetFixture, id string, subject billing.Subject, units, amountMicros int64, currency string, at time.Time) {
	t.Helper()
	ctx := context.Background()
	res := billing.Reservation{
		TenantID: f.tenant, ID: "res_" + id, State: billing.ReservationSettled,
		Subject: subject,
		Price: billing.PriceSnapshot{
			Currency: currency, RoundingRule: billing.RoundingCeil,
			Rates: []billing.SnapshotRate{{Unit: billing.UnitInput, RateMicros: 1, UnitQuantity: 1}},
		},
		LimitAmountMicros: amountMicros, Currency: currency,
		Units:      map[billing.UnitKind]int64{billing.UnitInput: units},
		Capability: "chat", Model: "m-test", EndpointID: "ep-test",
	}
	err := f.repo.InTx(ctx, func(tx billing.BillingTx) error {
		if err := tx.InsertReservations(ctx, []billing.Reservation{res}); err != nil {
			return err
		}
		if _, err := tx.Transition(ctx, f.tenant, res.ID, billing.ReservationHeld, billing.ReservationSettled); err != nil {
			return err
		}
		return tx.InsertUsageRecord(ctx, billing.UsageRecord{
			TenantID: f.tenant, ID: "ur_" + id, ReservationID: res.ID,
			Subject: subject, Capability: "chat", Model: "m-test", EndpointID: "ep-test",
			Units:        map[billing.UnitKind]int64{billing.UnitInput: units},
			AmountMicros: amountMicros, Currency: currency, Price: res.Price,
			Outcome: "completed",
		})
	})
	if err != nil {
		t.Fatalf("seed settled usage %s: %v", id, err)
	}
	if _, err := f.db.SQL.ExecContext(ctx,
		`UPDATE usage_records SET created_at = $2 WHERE tenant_id = $1::uuid AND id = $3`,
		f.tenant, at.UTC(), "ur_"+id); err != nil {
		t.Fatalf("backdate usage %s: %v", id, err)
	}
}

func keyPrincipal(tenant, keyID string) auth.Principal {
	return auth.Principal{
		TenantID:           tenant,
		BillingSubjectKind: auth.BillingSubjectAPIKey,
		BillingSubjectID:   keyID,
	}
}

func TestEnforceBudgetDeniesAtStoredAmountLimit(t *testing.T) {
	f := newBudgetFixture(t)
	ctx := context.Background()
	keyID := "key-deny"
	subject := billing.Subject{Type: billing.BudgetSubjectAPIKey, ID: keyID}

	// 1 USD = 1_000_000 micros; seed exactly the 10-micros-of-dollar limit…
	if err := f.repo.CreateBudget(ctx, f.tenant, "b_deny", billing.Budget{
		Name: "deny-at-limit", Subject: subject,
		Kind: billing.BudgetKindAmount, Period: billing.PeriodMonthly,
		LimitAmountMicros: 1_000_000, Currency: "USD",
		StartsAt: time.Now().UTC().Add(-time.Hour),
	}); err != nil {
		t.Fatalf("create budget: %v", err)
	}

	seedSettledUsage(t, f, "deny_under", subject, 100, 999_999, "USD", time.Now().UTC())
	if err := f.app.EnforceBudget(ctx, keyPrincipal(f.tenant, keyID)); err != nil {
		t.Fatalf("below limit should admit: %v", err)
	}

	seedSettledUsage(t, f, "deny_hit", subject, 1, 1, "USD", time.Now().UTC())
	err := f.app.EnforceBudget(ctx, keyPrincipal(f.tenant, keyID))
	if err == nil {
		t.Fatal("expected budget exceeded at the stored limit")
	}
	var budgetErr budgetExceededError
	if !errors.As(err, &budgetErr) {
		t.Fatalf("expected budgetExceededError, got %T %v", err, err)
	}
}

func TestEnforceBudgetDeniesOnTokenBudget(t *testing.T) {
	f := newBudgetFixture(t)
	ctx := context.Background()
	keyID := "key-tokens"
	subject := billing.Subject{Type: billing.BudgetSubjectAPIKey, ID: keyID}

	if err := f.repo.CreateBudget(ctx, f.tenant, "b_tokens", billing.Budget{
		Name: "token-cap", Subject: subject,
		Kind: billing.BudgetKindTokens, Period: billing.PeriodDaily,
		LimitAmountMicros: 500,
		StartsAt:          time.Now().UTC().Add(-time.Hour),
	}); err != nil {
		t.Fatalf("create token budget: %v", err)
	}

	seedSettledUsage(t, f, "tok_under", subject, 499, 0, "USD", time.Now().UTC())
	if err := f.app.EnforceBudget(ctx, keyPrincipal(f.tenant, keyID)); err != nil {
		t.Fatalf("499 < 500 should admit: %v", err)
	}
	seedSettledUsage(t, f, "tok_hit", subject, 1, 0, "USD", time.Now().UTC())
	var budgetErr budgetExceededError
	if err := f.app.EnforceBudget(ctx, keyPrincipal(f.tenant, keyID)); !errors.As(err, &budgetErr) {
		t.Fatalf("expected denial at 500 units, got %v", err)
	}
}

func TestEnforceBudgetPeriodWindow(t *testing.T) {
	f := newBudgetFixture(t)
	ctx := context.Background()
	keyID := "key-window"
	subject := billing.Subject{Type: billing.BudgetSubjectAPIKey, ID: keyID}
	now := time.Now().UTC()
	monthStart := time.Date(now.Year(), now.Month(), 1, 0, 0, 0, 0, time.UTC)
	lastMonth := monthStart.Add(-time.Hour)

	if err := f.repo.CreateBudget(ctx, f.tenant, "b_window", billing.Budget{
		Name: "monthly-window", Subject: subject,
		Kind: billing.BudgetKindAmount, Period: billing.PeriodMonthly,
		LimitAmountMicros: 100, Currency: "USD",
		StartsAt: lastMonth.Add(-time.Hour),
	}); err != nil {
		t.Fatalf("create budget: %v", err)
	}

	// Last month's charge must not count against this month's window.
	seedSettledUsage(t, f, "win_old", subject, 10, 1_000_000, "USD", lastMonth)
	if err := f.app.EnforceBudget(ctx, keyPrincipal(f.tenant, keyID)); err != nil {
		t.Fatalf("previous-period usage must not count: %v", err)
	}
	seedSettledUsage(t, f, "win_new", subject, 1, 100, "USD", time.Now().UTC())
	var budgetErr budgetExceededError
	if err := f.app.EnforceBudget(ctx, keyPrincipal(f.tenant, keyID)); !errors.As(err, &budgetErr) {
		t.Fatalf("current-period usage at the limit must deny, got %v", err)
	}
}

func TestUsedForPeriodReadsSettledAggregates(t *testing.T) {
	f := newBudgetFixture(t)
	ctx := context.Background()
	keyID := "key-usage"
	subject := billing.Subject{Type: billing.BudgetSubjectAPIKey, ID: keyID}
	other := billing.Subject{Type: billing.BudgetSubjectAPIKey, ID: "key-other"}

	seedSettledUsage(t, f, "agg_a", subject, 40, 250_000, "USD", time.Now().UTC())
	seedSettledUsage(t, f, "agg_b", subject, 60, 250_000, "USD", time.Now().UTC())
	// Another subject's usage must not leak into this key's total.
	seedSettledUsage(t, f, "agg_other", other, 999, 9_999_999, "USD", time.Now().UTC())

	units, micros, err := f.app.usedForPeriod(ctx, f.tenant, subject, "total", nil, time.Now().UTC())
	if err != nil {
		t.Fatalf("usedForPeriod: %v", err)
	}
	if units != 100 || micros != 500_000 {
		t.Fatalf("expected 100 units / 500000 micros, got %d / %d", units, micros)
	}
}

func TestCheckedBudgetAdd(t *testing.T) {
	for _, tt := range []struct {
		name string
		a, b int64
		want int64
		ok   bool
	}{
		{name: "positive", a: 7, b: 5, want: 12, ok: true},
		{name: "positive overflow", a: int64(^uint64(0) >> 1), b: 1},
		{name: "negative overflow", a: -int64(^uint64(0)>>1) - 1, b: -1},
	} {
		t.Run(tt.name, func(t *testing.T) {
			got, ok := checkedBudgetAdd(tt.a, tt.b)
			if got != tt.want || ok != tt.ok {
				t.Fatalf("checkedBudgetAdd(%d, %d) = (%d, %v), want (%d, %v)",
					tt.a, tt.b, got, ok, tt.want, tt.ok)
			}
		})
	}
}
func TestDashboardBudgetShowsHeldSettledAndRemaining(t *testing.T) {
	f := newBudgetFixture(t)
	ctx := context.Background()
	keyID := "dashboard-key"
	subject := billing.Subject{Type: billing.BudgetSubjectAPIKey, ID: keyID}
	now := time.Now().UTC()
	if err := f.repo.PutPrice(ctx, f.tenant, "dashboard-price", billing.Price{
		SubjectType: billing.PriceSubjectModel, SubjectID: "dashboard-model", Capability: "chat",
		Unit: billing.UnitInput, RateMicros: 1, UnitQuantity: 1, Currency: "USD",
	}, "tester", now); err != nil {
		t.Fatalf("put price: %v", err)
	}
	if err := f.repo.CreateBudget(ctx, f.tenant, "dashboard-budget", billing.Budget{
		Name: "dashboard", Subject: subject, Kind: billing.BudgetKindAmount, Period: billing.PeriodMonthly,
		LimitAmountMicros: 100, Currency: "USD", StartsAt: now.Add(-time.Hour),
	}); err != nil {
		t.Fatalf("create budget: %v", err)
	}
	principal := keyPrincipal(f.tenant, keyID)
	principal.CredentialKind = auth.CredentialKindAPIKey
	principal.CredentialID = keyID
	service := billing.NewService(f.repo)
	reservation, err := service.Reserve(ctx, principal, billing.ReserveRequest{
		SubjectKeys: []billing.Subject{subject}, BillingSubject: subject, Capability: "chat",
		Model: "dashboard-model", EndpointID: "dashboard-endpoint",
		Estimate: billing.Usage{InputUnits: 40},
		Price:    billing.PriceKey{SubjectType: billing.PriceSubjectModel, SubjectID: "dashboard-model"},
	})
	if err != nil {
		t.Fatalf("reserve: %v", err)
	}
	render := func() string {
		req := httptest.NewRequest(http.MethodGet, "/dashboard/partials/budgets", nil)
		req = req.WithContext(auth.WithPrincipal(req.Context(), principal))
		rec := httptest.NewRecorder()
		f.app.handleDashboardBudgets(rec, req)
		if rec.Code != http.StatusOK {
			t.Fatalf("budget dashboard returned %d: %s", rec.Code, rec.Body.String())
		}
		return rec.Body.String()
	}
	held := render()
	if !strings.Contains(held, "USD 0.000040") || !strings.Contains(held, "USD 0.000100") || !strings.Contains(held, "USD 0.000060") {
		t.Fatalf("held liability or remaining allowance not rendered: %s", held)
	}
	if _, err := service.Settle(ctx, principal, reservation.ID, billing.Usage{InputUnits: 30}); err != nil {
		t.Fatalf("settle: %v", err)
	}
	settled := render()
	if !strings.Contains(settled, "USD 0.000030") || !strings.Contains(settled, "USD 0.000070") {
		t.Fatalf("settled liability or remaining allowance not rendered: %s", settled)
	}
}
