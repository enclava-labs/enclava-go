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
	// Unknown reservations settle late with the reconciled usage.
	rec, err := f.svc.Settle(ctx, f.principal, res.ID, billing.Usage{InputUnits: 1})
	if err != nil {
		t.Fatalf("late settle of unknown must succeed, got %v", err)
	}
	if rec.AmountMicros != 1 || rec.Outcome != billing.OutcomeCompleted {
		t.Fatalf("late settle record: %+v", rec)
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

func TestReleaseUnsentFreesDispatchedHold(t *testing.T) {
	f := newFixture(t)
	ctx := context.Background()
	f.flatPrices(t)
	f.budget(t, "b1", billing.BudgetSubjectAPIKey, f.keyID, billing.BudgetKindAmount, 200, "USD")

	res, err := f.svc.Reserve(ctx, f.principal, f.req(billing.Usage{InputUnits: 60}))
	if err != nil {
		t.Fatalf("reserve: %v", err)
	}
	if err := f.svc.MarkDispatched(ctx, f.principal, res.ID); err != nil {
		t.Fatalf("mark dispatched: %v", err)
	}
	// Dispatch was marked but no request bytes reached the provider.
	if err := f.svc.ReleaseUnsent(ctx, f.principal, res.ID); err != nil {
		t.Fatalf("release unsent on dispatched hold: %v", err)
	}
	if err := f.svc.ReleaseUnsent(ctx, f.principal, res.ID); err != nil {
		t.Fatalf("release unsent must be idempotent: %v", err)
	}
	// Freed liability admits again.
	if _, err := f.svc.Reserve(ctx, f.principal, f.req(billing.Usage{InputUnits: 100})); err != nil {
		t.Fatalf("released hold must free budget, got %v", err)
	}

	// Settled and unknown reservations refuse.
	res2, err := f.svc.Reserve(ctx, f.principal, f.req(billing.Usage{InputUnits: 1}))
	if err != nil {
		t.Fatalf("reserve2: %v", err)
	}
	if _, err := f.svc.Settle(ctx, f.principal, res2.ID, billing.Usage{InputUnits: 1}); err != nil {
		t.Fatalf("settle2: %v", err)
	}
	if err := f.svc.ReleaseUnsent(ctx, f.principal, res2.ID); !errors.Is(err, billing.ErrReservationState) {
		t.Fatalf("release unsent of settled must fail, got %v", err)
	}
	res3, err := f.svc.Reserve(ctx, f.principal, f.req(billing.Usage{InputUnits: 1}))
	if err != nil {
		t.Fatalf("reserve3: %v", err)
	}
	if err := f.svc.MarkUnknown(ctx, f.principal, res3.ID); err != nil {
		t.Fatalf("mark unknown: %v", err)
	}
	if err := f.svc.ReleaseUnsent(ctx, f.principal, res3.ID); !errors.Is(err, billing.ErrReservationState) {
		t.Fatalf("release unsent of unknown must fail, got %v", err)
	}
}

func TestResolveUnknown(t *testing.T) {
	f := newFixture(t)
	ctx := context.Background()
	f.flatPrices(t)

	// Reconcile with actual usage: charged under the snapshot.
	res, err := f.svc.Reserve(ctx, f.principal, f.req(billing.Usage{InputUnits: 50}))
	if err != nil {
		t.Fatalf("reserve: %v", err)
	}
	if err := f.svc.MarkUnknown(ctx, f.principal, res.ID); err != nil {
		t.Fatalf("mark unknown: %v", err)
	}
	rec, err := f.svc.ResolveUnknown(ctx, f.principal, res.ID, billing.Usage{InputUnits: 42})
	if err != nil {
		t.Fatalf("resolve unknown: %v", err)
	}
	if rec.AmountMicros != 42 || rec.Outcome != billing.OutcomeReconciled {
		t.Fatalf("reconciled record: %+v", rec)
	}

	// Write-off: zero usage frees the hold entirely.
	res2, err := f.svc.Reserve(ctx, f.principal, f.req(billing.Usage{InputUnits: 10}))
	if err != nil {
		t.Fatalf("reserve2: %v", err)
	}
	if err := f.svc.MarkUnknown(ctx, f.principal, res2.ID); err != nil {
		t.Fatalf("mark unknown 2: %v", err)
	}
	rec2, err := f.svc.ResolveUnknown(ctx, f.principal, res2.ID, billing.Usage{})
	if err != nil {
		t.Fatalf("write-off: %v", err)
	}
	if rec2.AmountMicros != 0 || rec2.Outcome != billing.OutcomeReconciled {
		t.Fatalf("write-off record: %+v", rec2)
	}

	// Held and released reservations refuse.
	res3, err := f.svc.Reserve(ctx, f.principal, f.req(billing.Usage{InputUnits: 1}))
	if err != nil {
		t.Fatalf("reserve3: %v", err)
	}
	if _, err := f.svc.ResolveUnknown(ctx, f.principal, res3.ID, billing.Usage{}); !errors.Is(err, billing.ErrReservationState) {
		t.Fatalf("resolve held must fail, got %v", err)
	}
	if err := f.svc.ReleaseUndispatched(ctx, f.principal, res3.ID); err != nil {
		t.Fatalf("release3: %v", err)
	}
	if _, err := f.svc.ResolveUnknown(ctx, f.principal, res3.ID, billing.Usage{}); !errors.Is(err, billing.ErrReservationState) {
		t.Fatalf("resolve released must fail, got %v", err)
	}
}

func TestSweepStaleHolds(t *testing.T) {
	f := newFixture(t)
	ctx := context.Background()
	f.flatPrices(t)

	stale, err := f.svc.Reserve(ctx, f.principal, f.req(billing.Usage{InputUnits: 10}))
	if err != nil {
		t.Fatalf("reserve stale: %v", err)
	}
	staleDisp, err := f.svc.Reserve(ctx, f.principal, f.req(billing.Usage{InputUnits: 10}))
	if err != nil {
		t.Fatalf("reserve stale dispatched: %v", err)
	}
	if err := f.svc.MarkDispatched(ctx, f.principal, staleDisp.ID); err != nil {
		t.Fatalf("mark dispatched: %v", err)
	}
	fresh, err := f.svc.Reserve(ctx, f.principal, f.req(billing.Usage{InputUnits: 10}))
	if err != nil {
		t.Fatalf("reserve fresh: %v", err)
	}
	// Backdate the two stale holds.
	cutoff := time.Now().UTC().Add(-time.Hour)
	for _, id := range []string{stale.ID, staleDisp.ID} {
		if _, err := f.db.SQL.ExecContext(ctx,
			`UPDATE reservations SET created_at = $2 WHERE tenant_id = $1::uuid AND (id = $3 OR id LIKE $3 || '/%')`,
			f.tenant, cutoff.Add(-time.Hour), id); err != nil {
			t.Fatalf("backdate %s: %v", id, err)
		}
	}

	released, unknown, err := f.svc.SweepStaleHolds(ctx, cutoff)
	if err != nil {
		t.Fatalf("sweep: %v", err)
	}
	// Each reservation writes a canonical api_key row plus a tenant row.
	if released != 2 || unknown != 2 {
		t.Fatalf("sweep counts released=%d unknown=%d, want 2/2", released, unknown)
	}
	got, err := f.svc.GetReservation(ctx, f.principal, stale.ID)
	if err != nil || got.State != billing.ReservationReleased {
		t.Fatalf("stale undispatched must release, got %+v err=%v", got, err)
	}
	got, err = f.svc.GetReservation(ctx, f.principal, staleDisp.ID)
	if err != nil || got.State != billing.ReservationUnknown {
		t.Fatalf("stale dispatched must become unknown, got %+v err=%v", got, err)
	}
	got, err = f.svc.GetReservation(ctx, f.principal, fresh.ID)
	if err != nil || got.State != billing.ReservationHeld {
		t.Fatalf("fresh hold untouched, got %+v err=%v", got, err)
	}
}

func TestLiabilityWindowDropsStaleUnknown(t *testing.T) {
	f := newFixture(t)
	ctx := context.Background()
	f.flatPrices(t)
	f.budget(t, "b1", billing.BudgetSubjectAPIKey, f.keyID, billing.BudgetKindAmount, 100, "USD")

	old, err := f.svc.Reserve(ctx, f.principal, f.req(billing.Usage{InputUnits: 90}))
	if err != nil {
		t.Fatalf("reserve old: %v", err)
	}
	if err := f.svc.MarkUnknown(ctx, f.principal, old.ID); err != nil {
		t.Fatalf("mark unknown: %v", err)
	}
	// Backdate the unknown row out of the monthly window — it stops counting.
	if _, err := f.db.SQL.ExecContext(ctx,
		`UPDATE reservations SET created_at = $2 WHERE tenant_id = $1::uuid AND (id = $3 OR id LIKE $3 || '/%')`,
		f.tenant, time.Now().UTC().AddDate(0, -2, 0), old.ID); err != nil {
		t.Fatalf("backdate: %v", err)
	}
	if _, err := f.svc.Reserve(ctx, f.principal, f.req(billing.Usage{InputUnits: 90})); err != nil {
		t.Fatalf("aged-out unknown must not count, got %v", err)
	}

	// A held row inside the same scope always counts regardless of age.
	held, err := f.svc.Reserve(ctx, f.principal, f.req(billing.Usage{InputUnits: 5}))
	if err != nil {
		t.Fatalf("reserve held: %v", err)
	}
	if _, err := f.db.SQL.ExecContext(ctx,
		`UPDATE reservations SET created_at = $2 WHERE tenant_id = $1::uuid AND (id = $3 OR id LIKE $3 || '/%')`,
		f.tenant, time.Now().UTC().AddDate(0, -2, 0), held.ID); err != nil {
		t.Fatalf("backdate held: %v", err)
	}
	if _, err := f.svc.Reserve(ctx, f.principal, f.req(billing.Usage{InputUnits: 10})); !errors.Is(err, billing.ErrBudgetExceeded) {
		t.Fatalf("aged held must still count (90+5+10>100), got %v", err)
	}
}

func TestPromptBoundPricedOnceAtHigherRate(t *testing.T) {
	f := newFixture(t)
	ctx := context.Background()
	f.modelPrice(t, "p-in", billing.UnitInput, 10, 1, "USD")
	f.modelPrice(t, "p-cin", billing.UnitCachedInput, 3, 1, "USD")
	f.modelPrice(t, "p-out", billing.UnitOutput, 5, 1, "USD")
	f.budget(t, "bt", billing.BudgetSubjectAPIKey, f.keyID, billing.BudgetKindTokens, 150, "")

	res, err := f.svc.Reserve(ctx, f.principal,
		f.req(billing.Usage{InputUnits: 100, CachedInputUnits: 100, OutputUnits: 10}))
	if err != nil {
		t.Fatalf("reserve: %v", err)
	}
	// Bound = 100 prompt tokens at the higher input rate + 10 output +
	// 1 micro rounding headroom for the two independently rounded prompt
	// kinds: 100*10 + 10*5 + 1, never (100*10)+(100*3)+(10*5).
	if res.LimitAmountMicros != 1051 {
		t.Fatalf("bound %d, want 1051", res.LimitAmountMicros)
	}
	// Token budget counted 110 (100 prompt once + 10 output): 40 headroom.
	if _, err := f.svc.Reserve(ctx, f.principal, f.req(billing.Usage{InputUnits: 41})); !errors.Is(err, billing.ErrBudgetExceeded) {
		t.Fatalf("110+41 must exceed 150 token budget, got %v", err)
	}
	if _, err := f.svc.Reserve(ctx, f.principal, f.req(billing.Usage{InputUnits: 40})); err != nil {
		t.Fatalf("110+40 must fit 150 token budget, got %v", err)
	}
	// Settle stays disjoint: real usage reports input minus cached.
	rec, err := f.svc.Settle(ctx, f.principal, res.ID,
		billing.Usage{InputUnits: 60, CachedInputUnits: 30, OutputUnits: 5})
	if err != nil {
		t.Fatalf("settle: %v", err)
	}
	if rec.AmountMicros != 60*10+30*3+5*5 {
		t.Fatalf("settled %d, want 715", rec.AmountMicros)
	}
}

func TestPromptBoundCoversSeparateSettlementRounding(t *testing.T) {
	f := newFixture(t)
	ctx := context.Background()
	// 1 micro per 100 tokens for both prompt kinds: a 50+50 split settles
	// as ceil(50/100)+ceil(50/100) = 2, so the folded bound needs the
	// per-term rounding headroom, not just the once-rounded 1.
	f.modelPrice(t, "p-in", billing.UnitInput, 1, 100, "USD")
	f.modelPrice(t, "p-cin", billing.UnitCachedInput, 1, 100, "USD")
	f.budget(t, "b1", billing.BudgetSubjectAPIKey, f.keyID, billing.BudgetKindAmount, 3, "USD")

	res, err := f.svc.Reserve(ctx, f.principal,
		f.req(billing.Usage{InputUnits: 100, CachedInputUnits: 100}))
	if err != nil {
		t.Fatalf("reserve: %v", err)
	}
	if res.LimitAmountMicros != 2 {
		t.Fatalf("bound %d, want 2 (worst-case settlement within the prompt bound)", res.LimitAmountMicros)
	}

	// Only one prompt kind declared: the once-rounded bound is exact, no
	// headroom is charged.
	single, err := f.svc.Reserve(ctx, f.principal, f.req(billing.Usage{InputUnits: 100}))
	if err != nil {
		t.Fatalf("single-kind reserve: %v", err)
	}
	if single.LimitAmountMicros != 1 {
		t.Fatalf("single-kind bound %d, want 1", single.LimitAmountMicros)
	}

	// The reservation covers the worst split: 50 uncached + 50 cached
	// settles at 2, never above the held bound.
	rec, err := f.svc.Settle(ctx, f.principal, res.ID,
		billing.Usage{InputUnits: 50, CachedInputUnits: 50})
	if err != nil {
		t.Fatalf("settle: %v", err)
	}
	if rec.AmountMicros != 2 || rec.AmountMicros > res.LimitAmountMicros {
		t.Fatalf("settled %d, want 2 <= bound %d", rec.AmountMicros, res.LimitAmountMicros)
	}

	// 1-micro budget: existing liability of 3 plus a proposed bound of 2
	// must not admit.
	g := newFixture(t)
	g.modelPrice(t, "p-in", billing.UnitInput, 1, 100, "USD")
	g.modelPrice(t, "p-cin", billing.UnitCachedInput, 1, 100, "USD")
	g.budget(t, "b1", billing.BudgetSubjectAPIKey, g.keyID, billing.BudgetKindAmount, 1, "USD")
	if _, err := g.svc.Reserve(ctx, g.principal,
		g.req(billing.Usage{InputUnits: 100, CachedInputUnits: 100})); !errors.Is(err, billing.ErrBudgetExceeded) {
		t.Fatalf("expected ErrBudgetExceeded, got %v", err)
	}
}

func TestSettleUnpricedUnitsRecorded(t *testing.T) {
	f := newFixture(t)
	ctx := context.Background()
	f.flatPrices(t)

	res, err := f.svc.Reserve(ctx, f.principal, f.req(billing.Usage{InputUnits: 10}))
	if err != nil {
		t.Fatalf("reserve: %v", err)
	}
	// Report a unit kind the snapshot never priced: settle must not fail,
	// units are recorded, only priced kinds charge.
	rec, err := f.svc.Settle(ctx, f.principal, res.ID, billing.Usage{
		InputUnits: 8,
		OtherUnits: map[billing.UnitKind]int64{"images": 2},
	})
	if err != nil {
		t.Fatalf("settle with unpriced kind: %v", err)
	}
	if rec.Outcome != billing.OutcomeUnpricedUnits {
		t.Fatalf("outcome %q, want unpriced_units", rec.Outcome)
	}
	if rec.AmountMicros != 8 || rec.Units["images"] != 2 || rec.Units[billing.UnitInput] != 8 {
		t.Fatalf("unpriced settle record: %+v", rec)
	}
}

func TestBudgetTimezoneLiabilityWindow(t *testing.T) {
	f := newFixture(t)
	ctx := context.Background()
	f.flatPrices(t)

	// A daily budget on Europe/Ljubljana (UTC+1/UTC+2): the liability window
	// starts at the Ljubljana day boundary, not the UTC one.
	if err := f.repo.CreateBudget(ctx, f.tenant, "btz", billing.Budget{
		Subject:           billing.Subject{Type: billing.BudgetSubjectAPIKey, ID: f.keyID},
		Kind:              billing.BudgetKindAmount,
		Period:            billing.PeriodDaily,
		LimitAmountMicros: 100,
		Currency:          "USD",
		Timezone:          "Europe/Ljubljana",
		StartsAt:          time.Now().UTC().Add(-time.Hour * 48),
	}); err != nil {
		t.Fatalf("create tz budget: %v", err)
	}
	got, err := f.repo.GetBudget(ctx, f.tenant, "btz")
	if err != nil || got.Timezone != "Europe/Ljubljana" {
		t.Fatalf("timezone round-trip: %+v err=%v", got, err)
	}

	loc, err := time.LoadLocation("Europe/Ljubljana")
	if err != nil {
		t.Skipf("zoneinfo unavailable: %v", err)
	}
	now := time.Now().UTC()
	localNow := now.In(loc)
	dayStart := time.Date(localNow.Year(), localNow.Month(), localNow.Day(),
		0, 0, 0, 0, loc)
	if from := billing.LiabilityFrom(got, now); !from.Equal(dayStart) {
		t.Fatalf("liability from %v, want Ljubljana day start %v", from, dayStart)
	}

	// Usage settled just before the Ljubljana day start must not consume the
	// fresh day's budget even though it may share the same UTC day.
	res, err := f.svc.Reserve(ctx, f.principal, f.req(billing.Usage{InputUnits: 90}))
	if err != nil {
		t.Fatalf("reserve old: %v", err)
	}
	if _, err := f.svc.Settle(ctx, f.principal, res.ID, billing.Usage{InputUnits: 90}); err != nil {
		t.Fatalf("settle old: %v", err)
	}
	if _, err := f.db.SQL.ExecContext(ctx,
		`UPDATE usage_records SET created_at = $2 WHERE tenant_id = $1::uuid`,
		f.tenant, dayStart.UTC().Add(-time.Minute)); err != nil {
		t.Fatalf("backdate usage: %v", err)
	}
	if _, err := f.svc.Reserve(ctx, f.principal, f.req(billing.Usage{InputUnits: 90})); err != nil {
		t.Fatalf("new Ljubljana day must admit, got %v", err)
	}

	// An invalid zone name falls back to UTC.
	bad := got
	bad.Timezone = "Not/AZone"
	utcStart := time.Date(now.Year(), now.Month(), now.Day(), 0, 0, 0, 0, time.UTC)
	if from := billing.LiabilityFrom(bad, now); !from.Equal(utcStart) {
		t.Fatalf("invalid zone must fall back to UTC day start, got %v", from)
	}
}
