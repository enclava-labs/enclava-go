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

	"enclava-go/internal/auth"
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

// --- plan 08-07 task 2: usage presentation projections ------------------------

// seedUsageRow inserts one settled reservation+usage pair attributed to
// subject (and credential, when set), backdating the usage row to at.
func seedUsageRow(t *testing.T, ctx context.Context, db *DB, tenant, id string, subject billing.Subject, credential string, units, amountMicros int64, currency string, at time.Time) {
	t.Helper()
	res := billing.Reservation{
		TenantID: tenant, ID: "res-" + id, State: billing.ReservationSettled,
		Subject: subject,
		Price: billing.PriceSnapshot{
			Currency: currency, RoundingRule: billing.RoundingCeil,
			Rates: []billing.SnapshotRate{{Unit: billing.UnitInput, RateMicros: 1, UnitQuantity: 1}},
		},
		LimitAmountMicros: amountMicros, Currency: currency,
		Units:      map[billing.UnitKind]int64{billing.UnitInput: units},
		Capability: "chat", Model: "m-test", EndpointID: "ep-test",
		CredentialID: credential,
	}
	err := NewBillingRepo(db).InTx(ctx, func(tx billing.BillingTx) error {
		if err := tx.InsertReservations(ctx, []billing.Reservation{res}); err != nil {
			return err
		}
		if _, err := tx.Transition(ctx, tenant, res.ID, billing.ReservationHeld, billing.ReservationSettled); err != nil {
			return err
		}
		return tx.InsertUsageRecord(ctx, billing.UsageRecord{
			TenantID: tenant, ID: "ur-" + id, ReservationID: res.ID,
			Subject: subject, Capability: "chat", Model: "m-test", EndpointID: "ep-test",
			Units:        map[billing.UnitKind]int64{billing.UnitInput: units},
			AmountMicros: amountMicros, Currency: currency, Price: res.Price,
			Outcome: "completed", CredentialID: credential,
		})
	})
	if err != nil {
		t.Fatalf("seed usage %s: %v", id, err)
	}
	if _, err := db.SQL.ExecContext(ctx,
		`UPDATE usage_records SET created_at = $2 WHERE tenant_id = $1::uuid AND id = $3`,
		tenant, at.UTC(), "ur-"+id); err != nil {
		t.Fatalf("backdate usage %s: %v", id, err)
	}
}

func TestListDailyUsageUTCBins(t *testing.T) {
	db := RequireTestDB(t)
	ctx := context.Background()
	repo := NewBillingRepo(db)
	tenant := seedBillingTenant(t, ctx, db)

	// Two records either side of UTC midnight 2026-03-10, plus a distinct
	// currency that must never merge into the USD rows.
	day := time.Date(2026, 3, 10, 0, 0, 0, 0, time.UTC)
	subject := billing.Subject{Type: billing.BudgetSubjectUser, ID: "u1"}
	seedUsageRow(t, ctx, db, tenant, "d1", subject, "", 10, 100, "USD", day.Add(-time.Minute))
	seedUsageRow(t, ctx, db, tenant, "d2", subject, "", 7, 70, "USD", day.Add(time.Minute))
	seedUsageRow(t, ctx, db, tenant, "d3", subject, "", 5, 50, "EUR", day.Add(time.Minute))

	// A non-UTC session timezone must not shift the buckets: pin the pool to
	// one connection and shift it well west of UTC.
	db.SQL.SetMaxOpenConns(1)
	defer db.SQL.SetMaxOpenConns(0)
	if _, err := db.SQL.ExecContext(ctx, `SET TIME ZONE 'America/Santiago'`); err != nil {
		t.Fatalf("set session timezone: %v", err)
	}

	rows, err := repo.ListDailyUsage(ctx, tenant, day.Add(-24*time.Hour))
	if err != nil {
		t.Fatalf("daily usage: %v", err)
	}
	want := []DailyUsageRow{
		{Day: day.Add(-24 * time.Hour), RequestCount: 1, TotalUnits: 10, AmountMicros: 100, Currency: "USD"},
		{Day: day, RequestCount: 1, TotalUnits: 5, AmountMicros: 50, Currency: "EUR"},
		{Day: day, RequestCount: 1, TotalUnits: 7, AmountMicros: 70, Currency: "USD"},
	}
	if len(rows) != len(want) {
		t.Fatalf("daily rows = %+v, want %d exact UTC bins", rows, len(want))
	}
	for i := range want {
		if !rows[i].Day.Equal(want[i].Day) || rows[i] != want[i] {
			t.Fatalf("row[%d] = %+v, want %+v", i, rows[i], want[i])
		}
	}
}

func TestListUsageByTeamAndCredentialAttribution(t *testing.T) {
	db := RequireTestDB(t)
	ctx := context.Background()
	repo := NewBillingRepo(db)
	tenant := seedBillingTenant(t, ctx, db)
	ident := NewIdentityRepo(db)
	if err := ident.CreateUser(ctx, tenant, auth.User{
		ID: "user-team-a", TenantID: tenant, Email: "a@example.com", DisplayName: "A", Status: "active", Team: "atlas",
	}); err != nil {
		t.Fatalf("seed user a: %v", err)
	}
	if err := ident.CreateUser(ctx, tenant, auth.User{
		ID: "user-no-team", TenantID: tenant, Email: "b@example.com", DisplayName: "B", Status: "active",
	}); err != nil {
		t.Fatalf("seed user b: %v", err)
	}

	now := time.Now().UTC()
	subjectA := billing.Subject{Type: billing.BudgetSubjectUser, ID: "user-team-a"}
	subjectB := billing.Subject{Type: billing.BudgetSubjectUser, ID: "user-no-team"}
	subjectSA := billing.Subject{Type: billing.BudgetSubjectServiceAccount, ID: "svc-1"}
	seedUsageRow(t, ctx, db, tenant, "t1", subjectA, "", 3, 30, "USD", now)
	seedUsageRow(t, ctx, db, tenant, "t2", subjectB, "", 4, 40, "USD", now)
	seedUsageRow(t, ctx, db, tenant, "t3", subjectSA, "", 5, 50, "USD", now)
	// Key-attributed settled usage: the canonical subject stays the owner;
	// the credential column backs api_key rows and key-scoped liabilities.
	seedUsageRow(t, ctx, db, tenant, "t4", subjectA, "key-xyz", 9, 90, "USD", now)

	teams, err := repo.ListUsageByTeam(ctx, tenant, now.Add(-time.Hour))
	if err != nil {
		t.Fatalf("usage by team: %v", err)
	}
	wantTeams := map[string]UsageGroupSum{
		// atlas = t1 + the key-attributed t4 (canonical subject stays the
		// owner, so both rows land in the owner's team bucket).
		"atlas":                 {Key: "atlas", RequestCount: 2, TotalUnits: 12, AmountMicros: 120, Currency: "USD"},
		"No team":               {Key: "No team", RequestCount: 1, TotalUnits: 4, AmountMicros: 40, Currency: "USD"},
		"service_account:svc-1": {Key: "service_account:svc-1", RequestCount: 1, TotalUnits: 5, AmountMicros: 50, Currency: "USD"},
	}
	if len(teams) != len(wantTeams) {
		t.Fatalf("team rows = %+v, want %+v", teams, wantTeams)
	}
	for _, row := range teams {
		want, ok := wantTeams[row.Key]
		if !ok || row != want {
			t.Fatalf("team row %+v, want %+v", row, want)
		}
	}

	// UsageBySubject keeps canonical rows and adds one api_key row per
	// credential, without double-counting the owner.
	sums, err := repo.UsageBySubject(ctx, tenant, now.Add(-time.Hour))
	if err != nil {
		t.Fatalf("usage by subject: %v", err)
	}
	bySubject := map[string]UsageGroupSum{}
	for _, s := range sums {
		bySubject[s.SubjectType+":"+s.SubjectID] = s
	}
	if got := bySubject["user:user-team-a"]; got.RequestCount != 2 || got.AmountMicros != 120 {
		t.Fatalf("canonical owner row = %+v, want 2 requests / 120 micros", got)
	}
	if got := bySubject["api_key:key-xyz"]; got.RequestCount != 1 || got.AmountMicros != 90 {
		t.Fatalf("credential row = %+v, want 1 request / 90 micros", got)
	}

	// Liabilities for the api_key scope match the seeded key-attributed settle.
	rows, err := repo.Liabilities(ctx, tenant, billing.Subject{Type: billing.BudgetSubjectAPIKey, ID: "key-xyz"}, now.Add(-time.Hour))
	if err != nil {
		t.Fatalf("key liabilities: %v", err)
	}
	var settled int64
	for _, row := range rows {
		if row.State == billing.ReservationSettled {
			settled += row.AmountMicros
		}
	}
	if settled != 90 {
		t.Fatalf("key-scoped settled liability = %d, want 90", settled)
	}

	// SubjectUsageSince resolves the api_key subject through credential_id.
	units, micros, err := repo.SubjectUsageSince(ctx, tenant, billing.Subject{Type: billing.BudgetSubjectAPIKey, ID: "key-xyz"}, now.Add(-time.Hour))
	if err != nil || units != 9 || micros != 90 {
		t.Fatalf("subject usage since = (%d, %d, %v), want (9, 90, nil)", units, micros, err)
	}
}
