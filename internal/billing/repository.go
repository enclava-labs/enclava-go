// repository.go defines the single transactional money/unit liability
// authority the billing service relies on (JOB-03/JOB-04). Admission runs
// budget locks, price selection, liability reconstruction and the held-reserve
// insert inside ONE transaction via InTx — a DB transaction never spans
// upstream work.
package billing

import (
	"context"
	"time"
)

// LiabilityRow is one outstanding (held/unknown reservation) or settled
// (usage_records) charge inside a budget window. AmountMicros is the
// reservation bound or the settled amount; Units is the reserved or settled
// per-kind units so token budgets reconstruct liability even at zero rates.
type LiabilityRow struct {
	State        string
	AmountMicros int64
	Currency     string
	Units        map[UnitKind]int64
}

// BillingTx is the transaction-scoped store used inside InTx.
type BillingTx interface {
	// LockActiveBudgets SELECT FOR UPDATEs every active budget applicable to
	// any of subjects at now, in deterministic (subject, id) order — the lock
	// order every concurrent Reserve shares.
	LockActiveBudgets(ctx context.Context, tenantID string, subjects []Subject, now time.Time) ([]Budget, error)

	// SelectPrices returns the latest price row effective at now per unit
	// kind for one price key (subject_type + subject_id + capability).
	SelectPrices(ctx context.Context, tenantID string, subjectType, subjectID, capability string, now time.Time) ([]Price, error)

	// Liabilities returns outstanding held/unknown reservations plus settled
	// usage attributed to scope within its window. A scope subject of
	// {tenant, <any>} counts tenant-wide usage; any other subject counts
	// rows attributed to that exact subject.
	Liabilities(ctx context.Context, tenantID string, scope Subject, from time.Time) ([]LiabilityRow, error)

	// InsertReservations records the multi-subject liability rows of one
	// reservation: one canonical row per subject key, every row carrying the
	// same id, snapshot, bound, reserved units and dispatch context.
	InsertReservations(ctx context.Context, rows []Reservation) error

	// GetReservationForUpdate locks and returns the canonical reservation row.
	GetReservationForUpdate(ctx context.Context, tenantID, id string) (Reservation, error)

	// Transition flips every row of a reservation from `from` to `to` and
	// reports rows updated. Settled transitions also stamp settled_at.
	Transition(ctx context.Context, tenantID, id, from, to string) (int64, error)

	// MarkDispatched stamps dispatched_at on held rows that have not yet been
	// dispatched; subsequent ReleaseUndispatched refuses them.
	MarkDispatched(ctx context.Context, tenantID, id string) (int64, error)

	// InsertUsageRecord appends the once-only settlement evidence. The
	// database's UNIQUE(tenant_id, reservation_id) enforces once-only settle.
	InsertUsageRecord(ctx context.Context, rec UsageRecord) error

	// GetUsageRecord returns the settlement evidence for a reservation.
	GetUsageRecord(ctx context.Context, tenantID, reservationID string) (UsageRecord, error)
}

// BillingRepository is the tenant-scoped prices/budgets/reservations/usage
// authority. Write/read methods run their own short transactions; InTx gives
// the service one atomic admission/settlement transaction.
type BillingRepository interface {
	InTx(ctx context.Context, fn func(BillingTx) error) error

	// PutPrice inserts one immutable rational price row (new effective_from
	// versions never rewrite history) and audits it.
	PutPrice(ctx context.Context, tenantID, id string, price Price, actorID string, effectiveFrom time.Time) error

	// CreateBudget inserts an enforceable limit row.
	CreateBudget(ctx context.Context, tenantID, id string, b Budget) error

	// GetReservation returns the canonical reservation row.
	GetReservation(ctx context.Context, tenantID, id string) (Reservation, error)

	// GetUsageRecord returns the settlement evidence for a reservation.
	GetUsageRecord(ctx context.Context, tenantID, reservationID string) (UsageRecord, error)
}
