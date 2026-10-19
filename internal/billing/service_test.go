// service_test.go exercises admission, settlement and retained liability
// against real PostgreSQL — atomic reservations, immutable snapshots,
// once-only settle and unknown-outcome retention (JOB-03/JOB-04).
package billing_test

import (
	"context"
	"errors"
	"fmt"
	"math"
	"sync"
	"sync/atomic"
	"testing"
	"time"

	"enclava-go/internal/auth"
	"enclava-go/internal/billing"
	"enclava-go/internal/store/postgres"
	"enclava-go/internal/testdb"
)

var slugSeq atomic.Int64

type fixture struct {
	svc       *billing.Service
	repo      *postgres.BillingRepo
	db        *postgres.DB
	dsn       string
	tenant    string
	keyID     string
	principal auth.Principal
}

func newFixture(t *testing.T) fixture {
	t.Helper()
	db, dsn := testdb.NewWithDSN(t)
	repo := postgres.NewBillingRepo(db)
	var tenant string
	if err := db.SQL.QueryRowContext(context.Background(),
		`INSERT INTO tenants (id, slug, display_name)
		 VALUES (gen_random_uuid(), $1, $2) RETURNING id::text`,
		fmt.Sprintf("t%d", slugSeq.Add(1)), "test tenant").Scan(&tenant); err != nil {
		t.Fatalf("seed tenant: %v", err)
	}
	f := fixture{
		svc:    billing.NewService(repo),
		repo:   repo,
		db:     db,
		dsn:    dsn,
		tenant: tenant,
		keyID:  "key-1",
	}
	f.principal = auth.Principal{
		TenantID:           tenant,
		ActorID:            "tester",
		BillingSubjectKind: auth.BillingSubjectAPIKey,
		BillingSubjectID:   f.keyID,
	}
	return f
}

// putPrice installs one immutable rate row.
func (f fixture) putPrice(t *testing.T, id string, p billing.Price) {
	t.Helper()
	if err := f.repo.PutPrice(context.Background(), f.tenant, id, p, "tester", time.Now().UTC()); err != nil {
		t.Fatalf("put price %s: %v", id, err)
	}
}

func (f fixture) modelPrice(t *testing.T, id string, unit billing.UnitKind, micros, qty int64, currency string) {
	t.Helper()
	f.putPrice(t, id, billing.Price{
		SubjectType: billing.PriceSubjectModel, SubjectID: "m1",
		Capability: "chat", Unit: unit,
		RateMicros: micros, UnitQuantity: qty, Currency: currency,
	})
}

func (f fixture) budget(t *testing.T, id, subjectType, subjectID, kind string, limit int64, currency string) {
	t.Helper()
	err := f.repo.CreateBudget(context.Background(), f.tenant, id, billing.Budget{
		Subject:           billing.Subject{Type: subjectType, ID: subjectID},
		Kind:              kind,
		Period:            billing.PeriodMonthly,
		LimitAmountMicros: limit,
		Currency:          currency,
		StartsAt:          time.Now().UTC().Add(-time.Hour),
	})
	if err != nil {
		t.Fatalf("create budget %s: %v", id, err)
	}
}

func (f fixture) req(units billing.Usage) billing.ReserveRequest {
	return billing.ReserveRequest{
		SubjectKeys: []billing.Subject{{Type: billing.BudgetSubjectAPIKey, ID: f.keyID}},
		Capability:  "chat",
		Model:       "m1",
		EndpointID:  "ep1",
		Estimate:    units,
		Price:       billing.PriceKey{SubjectType: billing.PriceSubjectModel, SubjectID: "m1"},
	}
}

// flatPrices installs 1 micro per unit input/output rates (charge = units).
func (f fixture) flatPrices(t *testing.T) {
	t.Helper()
	f.modelPrice(t, "p-in", billing.UnitInput, 1, 1, "USD")
	f.modelPrice(t, "p-out", billing.UnitOutput, 1, 1, "USD")
}

func TestReserveDeniesWhenProposedBoundExceedsRemaining(t *testing.T) {
	f := newFixture(t)
	ctx := context.Background()
	f.flatPrices(t)
	f.budget(t, "b1", billing.BudgetSubjectAPIKey, f.keyID, billing.BudgetKindAmount, 100, "USD")

	// Existing liability: 90 settled.
	r1, err := f.svc.Reserve(ctx, f.principal, f.req(billing.Usage{InputUnits: 90}))
	if err != nil {
		t.Fatalf("first reserve: %v", err)
	}
	if _, err := f.svc.Settle(ctx, f.principal, r1.ID, billing.Usage{InputUnits: 90}); err != nil {
		t.Fatalf("settle: %v", err)
	}

	// Below limit but insufficient remaining: 90 + 20 > 100 → denied; the
	// proposed bound itself counts.
	if _, err := f.svc.Reserve(ctx, f.principal, f.req(billing.Usage{InputUnits: 20})); !errors.Is(err, billing.ErrBudgetExceeded) {
		t.Fatalf("expected ErrBudgetExceeded, got %v", err)
	}
	// A fitting bound still admits.
	if _, err := f.svc.Reserve(ctx, f.principal, f.req(billing.Usage{InputUnits: 10})); err != nil {
		t.Fatalf("expected admission at remaining 10, got %v", err)
	}
}

func TestConcurrentReserveNeverOverAdmits(t *testing.T) {
	f := newFixture(t)
	ctx := context.Background()
	f.flatPrices(t)
	f.budget(t, "b1", billing.BudgetSubjectAPIKey, f.keyID, billing.BudgetKindAmount, 100, "USD")

	var wg sync.WaitGroup
	reserved := make(chan error, 2)
	for range 2 {
		wg.Add(1)
		go func() {
			defer wg.Done()
			_, err := f.svc.Reserve(ctx, f.principal, f.req(billing.Usage{InputUnits: 60}))
			reserved <- err
		}()
	}
	wg.Wait()
	close(reserved)
	var ok, denied int
	for err := range reserved {
		if err == nil {
			ok++
		} else if errors.Is(err, billing.ErrBudgetExceeded) {
			denied++
		} else {
			t.Fatalf("unexpected reserve error: %v", err)
		}
	}
	if ok != 1 || denied != 1 {
		t.Fatalf("expected exactly one admission, got ok=%d denied=%d", ok, denied)
	}
}

func TestTokenBudgetDenialReconstructsUnitsAcrossProcesses(t *testing.T) {
	f := newFixture(t)
	// Second "process": another pool over the same schema.
	svc2 := billing.NewService(postgres.NewBillingRepo(testdb.Secondary(t, f.dsn)))

	ctx := context.Background()
	// Zero-rate prices still reserve units — token liability never relies on
	// money.
	f.putPrice(t, "p-in0", billing.Price{SubjectType: billing.PriceSubjectModel, SubjectID: "m1",
		Capability: "chat", Unit: billing.UnitInput, RateMicros: 0, UnitQuantity: 1, Currency: "USD"})
	f.budget(t, "bt", billing.BudgetSubjectAPIKey, f.keyID, billing.BudgetKindTokens, 1000, "")

	if _, err := f.svc.Reserve(ctx, f.principal, f.req(billing.Usage{InputUnits: 800})); err != nil {
		t.Fatalf("process A reserve: %v", err)
	}
	if _, err := svc2.Reserve(ctx, f.principal, f.req(billing.Usage{InputUnits: 300})); !errors.Is(err, billing.ErrBudgetExceeded) {
		t.Fatalf("expected cross-process denial, got %v", err)
	}
}

func TestCrossCurrencyReservationDeniedByAmountBudget(t *testing.T) {
	f := newFixture(t)
	ctx := context.Background()
	f.modelPrice(t, "p-eur", billing.UnitInput, 5, 1, "EUR")
	f.budget(t, "b-usd", billing.BudgetSubjectAPIKey, f.keyID, billing.BudgetKindAmount, 1_000_000, "USD")

	_, err := f.svc.Reserve(ctx, f.principal, f.req(billing.Usage{InputUnits: 10}))
	if !errors.Is(err, billing.ErrCurrencyMismatch) {
		t.Fatalf("expected ErrCurrencyMismatch, got %v", err)
	}
}

func TestMissingRateRefusesDispatch(t *testing.T) {
	f := newFixture(t)
	f.modelPrice(t, "p-in", billing.UnitInput, 1, 1, "USD")
	// No output rate — an estimate containing output must refuse, never free.
	_, err := f.svc.Reserve(context.Background(), f.principal,
		f.req(billing.Usage{InputUnits: 10, OutputUnits: 5}))
	if !errors.Is(err, billing.ErrPriceMissing) {
		t.Fatalf("expected ErrPriceMissing, got %v", err)
	}
}

func TestMixedCurrencyPriceSetRejected(t *testing.T) {
	f := newFixture(t)
	f.modelPrice(t, "p-in", billing.UnitInput, 1, 1, "USD")
	f.modelPrice(t, "p-out", billing.UnitOutput, 1, 1, "EUR")
	_, err := f.svc.Reserve(context.Background(), f.principal,
		f.req(billing.Usage{InputUnits: 10, OutputUnits: 5}))
	if !errors.Is(err, billing.ErrCurrencyMismatch) {
		t.Fatalf("expected ErrCurrencyMismatch, got %v", err)
	}
}

func TestDistinctAndFractionalRatesSettleExactly(t *testing.T) {
	f := newFixture(t)
	ctx := context.Background()
	// input: 2 micros/unit; output: 150000 micros per 1_000_000 units
	// (0.15 micro/unit fractional rate) — ceil rounding applies per kind.
	f.modelPrice(t, "p-in", billing.UnitInput, 2, 1, "USD")
	f.modelPrice(t, "p-out", billing.UnitOutput, 150000, 1_000_000, "USD")

	res, err := f.svc.Reserve(ctx, f.principal, f.req(billing.Usage{InputUnits: 100, OutputUnits: 50}))
	if err != nil {
		t.Fatalf("reserve: %v", err)
	}
	// bound = 100*2 + ceil(50*150000/1000000) = 200 + 8 = 208
	if res.LimitAmountMicros != 208 {
		t.Fatalf("bound = %d, want 208", res.LimitAmountMicros)
	}
	rec, err := f.svc.Settle(ctx, f.principal, res.ID, billing.Usage{InputUnits: 90, OutputUnits: 33})
	if err != nil {
		t.Fatalf("settle: %v", err)
	}
	// settle = 90*2 + ceil(33*150000/1000000) = 180 + 5 = 185
	if rec.AmountMicros != 185 {
		t.Fatalf("settled amount = %d, want 185", rec.AmountMicros)
	}
	if rec.Units[billing.UnitInput] != 90 || rec.Units[billing.UnitOutput] != 33 {
		t.Fatalf("raw units not preserved: %v", rec.Units)
	}
}

func TestCachedInputSettlesDisjointly(t *testing.T) {
	f := newFixture(t)
	ctx := context.Background()
	f.modelPrice(t, "p-in", billing.UnitInput, 10, 1, "USD")
	f.modelPrice(t, "p-cin", billing.UnitCachedInput, 1, 1, "USD")

	res, err := f.svc.Reserve(ctx, f.principal,
		f.req(billing.Usage{InputUnits: 50, CachedInputUnits: 100}))
	if err != nil {
		t.Fatalf("reserve: %v", err)
	}
	rec, err := f.svc.Settle(ctx, f.principal, res.ID,
		billing.Usage{InputUnits: 40, CachedInputUnits: 80})
	if err != nil {
		t.Fatalf("settle: %v", err)
	}
	// cached_input counts ONLY at its own rate: 40*10 + 80*1 = 480, never
	// 40*10 + 80*10.
	if rec.AmountMicros != 480 {
		t.Fatalf("cached input double-counted or mispriced: got %d, want 480", rec.AmountMicros)
	}
}

func TestOverrunSettleStoresTrueAmount(t *testing.T) {
	f := newFixture(t)
	ctx := context.Background()
	f.flatPrices(t)
	f.budget(t, "b1", billing.BudgetSubjectAPIKey, f.keyID, billing.BudgetKindAmount, 1_000_000, "USD")

	res, err := f.svc.Reserve(ctx, f.principal, f.req(billing.Usage{InputUnits: 100}))
	if err != nil {
		t.Fatalf("reserve: %v", err)
	}
	rec, err := f.svc.Settle(ctx, f.principal, res.ID, billing.Usage{InputUnits: 250})
	if err != nil {
		t.Fatalf("settle: %v", err)
	}
	if rec.AmountMicros != 250 {
		t.Fatalf("overrun clipped to bound %d: stored %d", res.LimitAmountMicros, rec.AmountMicros)
	}
}

func TestSnapshotHonoredAfterPriceChange(t *testing.T) {
	f := newFixture(t)
	ctx := context.Background()
	f.flatPrices(t)

	res, err := f.svc.Reserve(ctx, f.principal, f.req(billing.Usage{InputUnits: 100}))
	if err != nil {
		t.Fatalf("reserve: %v", err)
	}
	// Later price change: 5 micros/unit — must NOT alter this settlement.
	time.Sleep(10 * time.Millisecond)
	f.modelPrice(t, "p-in2", billing.UnitInput, 5, 1, "USD")

	rec, err := f.svc.Settle(ctx, f.principal, res.ID, billing.Usage{InputUnits: 90})
	if err != nil {
		t.Fatalf("settle: %v", err)
	}
	if rec.AmountMicros != 90 {
		t.Fatalf("snapshot not honored: settled %d, want 90", rec.AmountMicros)
	}
}

func TestSettleIsOnceOnly(t *testing.T) {
	f := newFixture(t)
	ctx := context.Background()
	f.flatPrices(t)

	res, err := f.svc.Reserve(ctx, f.principal, f.req(billing.Usage{InputUnits: 10}))
	if err != nil {
		t.Fatalf("reserve: %v", err)
	}
	r1, err := f.svc.Settle(ctx, f.principal, res.ID, billing.Usage{InputUnits: 7})
	if err != nil {
		t.Fatalf("first settle: %v", err)
	}
	r2, err := f.svc.Settle(ctx, f.principal, res.ID, billing.Usage{InputUnits: 99})
	if err != nil {
		t.Fatalf("second settle must be idempotent, got %v", err)
	}
	if r2.ID != r1.ID || r2.AmountMicros != 7 {
		t.Fatalf("double settle did not return the original record: %+v", r2)
	}
}

func TestOverflowEstimateRejected(t *testing.T) {
	f := newFixture(t)
	f.modelPrice(t, "p-in", billing.UnitInput, math.MaxInt64, 1, "USD")
	_, err := f.svc.Reserve(context.Background(), f.principal,
		f.req(billing.Usage{InputUnits: math.MaxInt64}))
	if !errors.Is(err, billing.ErrArithmeticOverflow) {
		t.Fatalf("expected ErrArithmeticOverflow, got %v", err)
	}
}

func TestMarkUnknownRetainsLiability(t *testing.T) {
	f := newFixture(t)
	ctx := context.Background()
	f.flatPrices(t)
	f.budget(t, "b1", billing.BudgetSubjectAPIKey, f.keyID, billing.BudgetKindAmount, 100, "USD")

	res, err := f.svc.Reserve(ctx, f.principal, f.req(billing.Usage{InputUnits: 60}))
	if err != nil {
		t.Fatalf("reserve: %v", err)
	}
	if err := f.svc.MarkDispatched(ctx, f.principal, res.ID); err != nil {
		t.Fatalf("mark dispatched: %v", err)
	}
	if err := f.svc.MarkUnknown(ctx, f.principal, res.ID); err != nil {
		t.Fatalf("mark unknown: %v", err)
	}
	// Liability retained: 60 unknown + proposed 50 > 100 → denied.
	if _, err := f.svc.Reserve(ctx, f.principal, f.req(billing.Usage{InputUnits: 50})); !errors.Is(err, billing.ErrBudgetExceeded) {
		t.Fatalf("unknown reservation must still count, got %v", err)
	}
	// Fitting proposal admits.
	if _, err := f.svc.Reserve(ctx, f.principal, f.req(billing.Usage{InputUnits: 40})); err != nil {
		t.Fatalf("remaining 40 must admit, got %v", err)
	}
	// Unknown reservations never settle.
	if _, err := f.svc.Settle(ctx, f.principal, res.ID, billing.Usage{InputUnits: 1}); !errors.Is(err, billing.ErrReservationState) {
		t.Fatalf("settle of unknown must fail, got %v", err)
	}
}

func TestReleaseUndispatched(t *testing.T) {
	f := newFixture(t)
	ctx := context.Background()
	f.flatPrices(t)
	f.budget(t, "b1", billing.BudgetSubjectAPIKey, f.keyID, billing.BudgetKindAmount, 100, "USD")

	res, err := f.svc.Reserve(ctx, f.principal, f.req(billing.Usage{InputUnits: 60}))
	if err != nil {
		t.Fatalf("reserve: %v", err)
	}
	if err := f.svc.ReleaseUndispatched(ctx, f.principal, res.ID); err != nil {
		t.Fatalf("release undispatched: %v", err)
	}
	// Released liability is freed.
	freed, err := f.svc.Reserve(ctx, f.principal, f.req(billing.Usage{InputUnits: 100}))
	if err != nil {
		t.Fatalf("released hold must free budget, got %v", err)
	}
	if err := f.svc.ReleaseUndispatched(ctx, f.principal, freed.ID); err != nil {
		t.Fatalf("release freed: %v", err)
	}

	// A dispatched reservation can never be released.
	res2, err := f.svc.Reserve(ctx, f.principal, f.req(billing.Usage{InputUnits: 1}))
	if err != nil {
		t.Fatalf("second reserve: %v", err)
	}
	if err := f.svc.MarkDispatched(ctx, f.principal, res2.ID); err != nil {
		t.Fatalf("mark dispatched: %v", err)
	}
	if err := f.svc.ReleaseUndispatched(ctx, f.principal, res2.ID); !errors.Is(err, billing.ErrReservationState) {
		t.Fatalf("release after dispatch must fail, got %v", err)
	}
}

func TestTenantBudgetAggregatesAcrossBillingSubjects(t *testing.T) {
	f := newFixture(t)
	ctx := context.Background()
	f.flatPrices(t)
	f.budget(t, "bt", billing.BudgetSubjectTenant, f.tenant, billing.BudgetKindAmount, 100, "USD")

	if _, err := f.svc.Reserve(ctx, f.principal, f.req(billing.Usage{InputUnits: 70})); err != nil {
		t.Fatalf("reserve under key-1: %v", err)
	}
	// Another key under the same tenant sees the tenant liability.
	p2 := f.principal
	p2.BillingSubjectID = "key-2"
	req2 := f.req(billing.Usage{InputUnits: 50})
	req2.SubjectKeys = []billing.Subject{{Type: billing.BudgetSubjectAPIKey, ID: "key-2"}}
	if _, err := f.svc.Reserve(ctx, p2, req2); !errors.Is(err, billing.ErrBudgetExceeded) {
		t.Fatalf("tenant budget must count key-1's hold tenant-wide, got %v", err)
	}
}
