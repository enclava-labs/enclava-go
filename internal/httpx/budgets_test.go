// on stored billing state (JOB-03/JOB-04): budgets and liabilities live in
// the postgres billing tables, seeded through BillingRepo.
package httpx

import (
	"context"
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
	principal := auth.Principal{
		TenantID: f.tenant, CredentialKind: auth.CredentialKindAPIKey,
		CredentialID: keyID, BillingSubjectKind: auth.BillingSubjectAPIKey,
		BillingSubjectID: keyID,
	}
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
