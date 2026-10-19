// billing_repo_test.go proves the postgres liability authority against a real
// database: transactional visibility, price selection, reservation groups and
// once-only settle evidence.
package postgres

import (
	"context"
	"errors"
	"fmt"
	"sync/atomic"
	"testing"
	"time"

	"enclava-go/internal/billing"
)

var tenantSeq atomic.Int64

// seedBillingTenant inserts a tenant with a generated UUID and returns it.
func seedBillingTenant(t *testing.T, ctx context.Context, db *DB) string {
	t.Helper()
	var id string
	if err := db.SQL.QueryRowContext(ctx,
		`INSERT INTO tenants (id, slug, display_name)
		 VALUES (gen_random_uuid(), $1, $2) RETURNING id::text`,
		fmt.Sprintf("bt%d", tenantSeq.Add(1)), "billing tenant").Scan(&id); err != nil {
		t.Fatalf("seed tenant: %v", err)
	}
	return id
}

func TestBillingRepoTransactionsAndReads(t *testing.T) {
	db := RequireTestDB(t)
	repo := NewBillingRepo(db)
	ctx := context.Background()
	tenant := seedBillingTenant(t, ctx, db)

	err := repo.PutPrice(ctx, tenant, "p1", billing.Price{
		SubjectType: billing.PriceSubjectModel, SubjectID: "m1",
		Capability: "chat", Unit: billing.UnitInput,
		RateMicros: 3, UnitQuantity: 1000, Currency: "USD",
	}, "actor-1", time.Now().UTC().Add(-time.Minute))
	if err != nil {
		t.Fatalf("put price: %v", err)
	}
	// Audit row recorded.
	var auditN int
	if err := db.SQL.QueryRowContext(ctx,
		`SELECT count(*) FROM billing_price_audit WHERE tenant_id = $1::uuid AND price_id = 'p1'`,
		tenant).Scan(&auditN); err != nil || auditN != 1 {
		t.Fatalf("expected 1 audit row, n=%d err=%v", auditN, err)
	}

	if err := repo.CreateBudget(ctx, tenant, "b1", billing.Budget{
		Subject: billing.Subject{Type: billing.BudgetSubjectAPIKey, ID: "k1"},
		Kind:    billing.BudgetKindTokens, Period: billing.PeriodMonthly,
		LimitAmountMicros: 5000, StartsAt: time.Now().UTC().Add(-time.Hour),
	}); err != nil {
		t.Fatalf("create budget: %v", err)
	}

	// Inside one transaction: budgets lock, prices select, liability is empty.
	err = repo.InTx(ctx, func(tx billing.BillingTx) error {
		now := time.Now().UTC()
		budgets, err := tx.LockActiveBudgets(ctx, tenant, []billing.Subject{
			{Type: billing.BudgetSubjectAPIKey, ID: "k1"},
		}, now)
		if err != nil {
			return err
		}
		if len(budgets) != 1 || budgets[0].ID != "b1" {
			return fmt.Errorf("expected b1, got %+v", budgets)
		}
		prices, err := tx.SelectPrices(ctx, tenant, billing.PriceSubjectModel, "m1", "chat", now)
		if err != nil {
			return err
		}
		if len(prices) != 1 || prices[0].Unit != billing.UnitInput || prices[0].RateMicros != 3 {
			return fmt.Errorf("expected input rate 3/1000 USD, got %+v", prices)
		}
		liab, err := tx.Liabilities(ctx, tenant, billing.Subject{Type: billing.BudgetSubjectAPIKey, ID: "k1"}, now.Add(-time.Hour))
		if err != nil {
			return err
		}
		if len(liab) != 0 {
			return fmt.Errorf("expected no liability, got %+v", liab)
		}
		return nil
	})
	if err != nil {
		t.Fatalf("admission tx: %v", err)
	}

	// Multi-subject insert: canonical + attribution rows; group transition.
	res := billing.Reservation{
		TenantID: tenant, ID: "res-1", State: billing.ReservationHeld,
		Subject: billing.Subject{Type: billing.BudgetSubjectAPIKey, ID: "k1"},
		Price: billing.PriceSnapshot{
			Currency: "USD", RoundingRule: billing.RoundingCeil,
			Rates: []billing.SnapshotRate{{Unit: billing.UnitInput, RateMicros: 3, UnitQuantity: 1000}},
		},
		LimitAmountMicros: 30, Currency: "USD",
		Units:      map[billing.UnitKind]int64{billing.UnitInput: 10},
		Capability: "chat", Model: "m1", EndpointID: "ep1",
	}
	attributed := res
	attributed.ID = res.ID + "/tenant:" + tenant
	attributed.Subject = billing.Subject{Type: billing.BudgetSubjectTenant, ID: tenant}
	err = repo.InTx(ctx, func(tx billing.BillingTx) error {
		if err := tx.InsertReservations(ctx, []billing.Reservation{res, attributed}); err != nil {
			return err
		}
		got, err := tx.GetReservationForUpdate(ctx, tenant, res.ID)
		if err != nil {
			return err
		}
		if got.State != billing.ReservationHeld || got.LimitAmountMicros != 30 || got.Units[billing.UnitInput] != 10 {
			return fmt.Errorf("bad canonical row: %+v", got)
		}
		return nil
	})
	if err != nil {
		t.Fatalf("insert reservations: %v", err)
	}

	// Held liability reconstructs from persisted reserved_units.
	err = repo.InTx(ctx, func(tx billing.BillingTx) error {
		rows, err := tx.Liabilities(ctx, tenant, billing.Subject{Type: billing.BudgetSubjectAPIKey, ID: "k1"}, time.Now().Add(-time.Hour))
		if err != nil {
			return err
		}
		if len(rows) != 1 || rows[0].Units[billing.UnitInput] != 10 || rows[0].AmountMicros != 30 {
			return fmt.Errorf("held liability = %+v", rows)
		}
		// Tenant scope counts the canonical row only once.
		rows, err = tx.Liabilities(ctx, tenant, billing.Subject{Type: billing.BudgetSubjectTenant, ID: tenant}, time.Now().Add(-time.Hour))
		if err != nil {
			return err
		}
		if len(rows) != 1 || rows[0].AmountMicros != 30 {
			return fmt.Errorf("tenant liability = %+v", rows)
		}
		// Settle: transition + usage record.
		if n, err := tx.MarkDispatched(ctx, tenant, res.ID); err != nil || n == 0 {
			return fmt.Errorf("mark dispatched n=%d err=%v", n, err)
		}
		n, err := tx.Transition(ctx, tenant, res.ID, billing.ReservationHeld, billing.ReservationSettled)
		if err != nil || n != 2 {
			return fmt.Errorf("transition n=%d err=%v (want 2 rows)", n, err)
		}
		return tx.InsertUsageRecord(ctx, billing.UsageRecord{
			TenantID: tenant, ID: "ur-1", ReservationID: res.ID,
			Subject: res.Subject, Capability: "chat", Model: "m1",
			EndpointID: "ep1", Units: map[billing.UnitKind]int64{billing.UnitInput: 9},
			AmountMicros: 27, Currency: "USD", Price: res.Price, Outcome: "completed",
		})
	})
	if err != nil {
		t.Fatalf("settle tx: %v", err)
	}

	// Once-only settle: a second usage_records row for the reservation fails.
	err = repo.InTx(ctx, func(tx billing.BillingTx) error {
		return tx.InsertUsageRecord(ctx, billing.UsageRecord{
			TenantID: tenant, ID: "ur-2", ReservationID: res.ID,
			Subject: res.Subject, Capability: "chat", Model: "m1",
			Units: map[billing.UnitKind]int64{}, AmountMicros: 1,
			Currency: "USD", Price: res.Price, Outcome: "completed",
		})
	})
	if err == nil {
		t.Fatal("duplicate usage_records insert must violate UNIQUE(tenant,reservation)")
	}

	got, err := repo.GetReservation(ctx, tenant, res.ID)
	if err != nil {
		t.Fatalf("get reservation: %v", err)
	}
	if got.State != billing.ReservationSettled || got.DispatchedAt == nil || got.SettledAt == nil {
		t.Fatalf("reservation after settle: %+v", got)
	}
	ur, err := repo.GetUsageRecord(ctx, tenant, res.ID)
	if err != nil {
		t.Fatalf("get usage record: %v", err)
	}
	if ur.AmountMicros != 27 || ur.Units[billing.UnitInput] != 9 || ur.Model != "m1" {
		t.Fatalf("usage record: %+v", ur)
	}
}

func TestBillingRepoTransitionsEnforceFromState(t *testing.T) {
	db := RequireTestDB(t)
	repo := NewBillingRepo(db)
	ctx := context.Background()
	tenant := seedBillingTenant(t, ctx, db)

	res := billing.Reservation{
		TenantID: tenant, ID: "res-9", State: billing.ReservationHeld,
		Subject:  billing.Subject{Type: billing.BudgetSubjectAPIKey, ID: "k9"},
		Price:    billing.PriceSnapshot{Currency: "USD", RoundingRule: billing.RoundingCeil},
		Currency: "USD", Units: map[billing.UnitKind]int64{billing.UnitInput: 1},
	}
	if err := repo.InTx(ctx, func(tx billing.BillingTx) error {
		return tx.InsertReservations(ctx, []billing.Reservation{res})
	}); err != nil {
		t.Fatalf("insert: %v", err)
	}
	// held→settled only from held.
	err := repo.InTx(ctx, func(tx billing.BillingTx) error {
		n, err := tx.Transition(ctx, tenant, res.ID, billing.ReservationReleased, billing.ReservationSettled)
		if err != nil {
			return err
		}
		if n != 0 {
			return fmt.Errorf("transition from released state must move 0 rows, moved %d", n)
		}
		return nil
	})
	if err != nil {
		t.Fatal(err)
	}
	if _, err := repo.GetReservation(ctx, tenant, "missing"); !errors.Is(err, billing.ErrReservationNotFound) {
		t.Fatalf("expected ErrReservationNotFound, got %v", err)
	}
}
