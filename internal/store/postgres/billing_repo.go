// the single transactional money/unit liability authority (JOB-03/JOB-04).
// Admission locks applicable budgets FOR UPDATE in deterministic subject/id
// order, reconstructs liability from persisted reserved_units and settled
// usage, and inserts one reservation row per subject key so every checked
// subject's budgets keep counting the hold across processes.
package postgres

import (
	"context"
	crand "crypto/rand"
	"database/sql"
	"encoding/hex"
	"encoding/json"
	"errors"
	"fmt"
	"time"

	"enclava-go/internal/billing"
)

// BillingRepo is the postgres-backed billing.BillingRepository.
type BillingRepo struct {
	db *sql.DB
}

// ErrBudgetNotFound is returned for unknown or cross-tenant budget ids —
// handlers surface both as one indistinguishable not-found.
var ErrBudgetNotFound = errors.New("postgres: budget not found")

// NewBillingRepo wraps the shared pool.
func NewBillingRepo(db *DB) *BillingRepo {
	return &BillingRepo{db: db.SQL}
}

var _ billing.BillingRepository = (*BillingRepo)(nil)

// billingTx is the transaction-scoped billing.BillingTx.
type billingTx struct {
	tx *sql.Tx
}

var _ billing.BillingTx = (*billingTx)(nil)

// InTx runs fn inside one transaction — the admission/settlement atomicity
// seam. Callers never hold this transaction across upstream work.
func (r *BillingRepo) InTx(ctx context.Context, fn func(billing.BillingTx) error) error {
	return WithTx(ctx, r.db, func(tx *sql.Tx) error {
		return fn(billingTx{tx: tx})
	})
}

// PutPrice inserts an immutable price row and its audit row. A price change
// is a new row with a later effective_from — history is never rewritten.
func (r *BillingRepo) PutPrice(ctx context.Context, tenantID, id string, p billing.Price, actorID string, effectiveFrom time.Time) error {
	return r.PutPrices(ctx, tenantID, []billing.PriceWrite{
		{ID: id, Price: p, ActorID: actorID, EffectiveFrom: effectiveFrom},
	})
}

// PutPrices inserts every batch entry inside ONE transaction — the Set-price
// drawer saves input/cached/output together and a partial matrix is a bug.
func (r *BillingRepo) PutPrices(ctx context.Context, tenantID string, batch []billing.PriceWrite) error {
	return WithTx(ctx, r.db, func(tx *sql.Tx) error {
		for _, w := range batch {
			if err := putPriceTx(ctx, tx, tenantID, w.ID, w.Price, w.ActorID, w.EffectiveFrom); err != nil {
				return err
			}
		}
		return nil
	})
}

func putPriceTx(ctx context.Context, tx *sql.Tx, tenantID, id string, p billing.Price, actorID string, effectiveFrom time.Time) error {
	if _, err := tx.ExecContext(ctx, `
			INSERT INTO billing_prices
				(tenant_id, id, subject_type, subject_id, capability, unit_kind,
				 rate_micros, unit_quantity, currency, effective_from, created_by)
			VALUES ($1::uuid, $2, $3, $4, $5, $6, $7, $8, $9, $10, $11)`,
		tenantID, id, p.SubjectType, p.SubjectID, p.Capability, string(p.Unit),
		p.RateMicros, p.UnitQuantity, p.Currency, effectiveFrom, nullStr(actorID)); err != nil {
		return fmt.Errorf("insert billing price: %w", err)
	}
	detail, err := json.Marshal(map[string]any{
		"subject_type":   p.SubjectType,
		"subject_id":     p.SubjectID,
		"capability":     p.Capability,
		"unit_kind":      string(p.Unit),
		"rate_micros":    p.RateMicros,
		"unit_quantity":  p.UnitQuantity,
		"currency":       p.Currency,
		"effective_from": effectiveFrom.UTC().Format(time.RFC3339Nano),
	})
	if err != nil {
		return fmt.Errorf("marshal price audit detail: %w", err)
	}
	if _, err := tx.ExecContext(ctx, `
			INSERT INTO billing_price_audit
				(tenant_id, id, price_id, action, actor_type, actor_id, detail)
			VALUES ($1::uuid, $2, $3, 'create', 'actor', $4, $5)`,
		tenantID, newRowID("paudit"), id, nullStr(actorID), string(detail)); err != nil {
		return fmt.Errorf("insert billing price audit: %w", err)
	}
	return nil
}

// CreateBudget inserts an enforceable limit row. Amount budgets carry their
// bound currency; token budgets store NULL currency. Timezone selects the
// zone whose day/week/month boundaries define the liability window
// ("" normalizes to UTC).
func (r *BillingRepo) CreateBudget(ctx context.Context, tenantID, id string, b billing.Budget) error {
	var currency any
	if b.Kind == billing.BudgetKindAmount {
		currency = b.Currency
	}
	name := b.Name
	if name == "" {
		name = "budget"
	}
	tz := b.Timezone
	if tz == "" {
		tz = "UTC"
	}
	_, err := r.db.ExecContext(ctx, `
		INSERT INTO budgets
			(tenant_id, id, name, subject_type, subject_id, kind, limit_value,
			 currency, period, timezone, starts_at, ends_at)
		VALUES ($1::uuid, $2, $3, $4, $5, $6, $7, $8, $9, $10, $11, $12)`,
		tenantID, id, name, b.Subject.Type, b.Subject.ID, b.Kind,
		b.LimitAmountMicros, currency, b.Period, tz, b.StartsAt, b.EndsAt)
	if err != nil {
		return fmt.Errorf("insert budget: %w", err)
	}
	return nil
}

// GetReservation returns the canonical reservation row — the one whose id is
// the reservation id (subject-attribution rows carry a suffixed id).
func (r *BillingRepo) GetReservation(ctx context.Context, tenantID, id string) (billing.Reservation, error) {
	return scanReservation(r.db.QueryRowContext(ctx, `
		SELECT `+reservationCols+`
		FROM reservations WHERE tenant_id = $1::uuid AND id = $2`, tenantID, id))
}

// GetUsageRecord returns the once-only settlement evidence for a reservation.
func (r *BillingRepo) GetUsageRecord(ctx context.Context, tenantID, reservationID string) (billing.UsageRecord, error) {
	return scanUsageRecord(r.db.QueryRowContext(ctx, `
		SELECT `+usageCols+`
		FROM usage_records WHERE tenant_id = $1::uuid AND reservation_id = $2`,
		tenantID, reservationID))
}

// --- transaction-scoped -----------------------------------------------------

// LockActiveBudgets locks every active budget applicable to any subject in
// deterministic order so concurrent admissions serialize without deadlocks.
func (t billingTx) LockActiveBudgets(ctx context.Context, tenantID string, subjects []billing.Subject, now time.Time) ([]billing.Budget, error) {
	if len(subjects) == 0 {
		return nil, nil
	}
	types := make([]string, len(subjects))
	ids := make([]string, len(subjects))
	for i, s := range subjects {
		types[i], ids[i] = s.Type, s.ID
	}
	rows, err := t.tx.QueryContext(ctx, budgetSelectSQL+`
		  AND (subject_type, subject_id) IN (SELECT * FROM unnest($3::text[], $4::text[]))
		ORDER BY subject_type, subject_id, id
		FOR UPDATE`, tenantID, now, strArray(types), strArray(ids))
	if err != nil {
		return nil, fmt.Errorf("lock budgets: %w", err)
	}
	return scanBudgets(rows)
}

// budgetCols is the shared budget projection; budgetSelectSQL adds the
// active-window predicates on top of it.
const budgetCols = `
	id, name, subject_type, subject_id, kind, period, limit_value,
	COALESCE(currency, ''), timezone, starts_at, ends_at`

const budgetSelectSQL = `
	SELECT ` + budgetCols + `
	FROM budgets
	WHERE tenant_id = $1::uuid
	  AND active
	  AND starts_at <= $2
	  AND (ends_at IS NULL OR ends_at > $2)
`

func scanBudgets(rows *sql.Rows) ([]billing.Budget, error) {
	defer func() { _ = rows.Close() }()
	var out []billing.Budget
	for rows.Next() {
		var b billing.Budget
		var endsAt sql.NullTime
		if err := rows.Scan(&b.ID, &b.Name, &b.Subject.Type, &b.Subject.ID, &b.Kind,
			&b.Period, &b.LimitAmountMicros, &b.Currency, &b.Timezone,
			&b.StartsAt, &endsAt); err != nil {
			return nil, fmt.Errorf("scan budget: %w", err)
		}
		if endsAt.Valid {
			e := endsAt.Time
			b.EndsAt = &e
		}
		out = append(out, b)
	}
	return out, rows.Err()
}

// SelectPrices returns the latest effective rate row per unit kind for one
// price key at `now`.
func (t billingTx) SelectPrices(ctx context.Context, tenantID, subjectType, subjectID, capability string, now time.Time) ([]billing.Price, error) {
	rows, err := t.tx.QueryContext(ctx, `
		SELECT DISTINCT ON (unit_kind)
		       subject_type, subject_id, capability, unit_kind,
		       rate_micros, unit_quantity, currency, effective_from
		FROM billing_prices
		WHERE tenant_id = $1::uuid AND subject_type = $2 AND subject_id = $3
		  AND capability = $4 AND effective_from <= $5
		ORDER BY unit_kind, effective_from DESC`, tenantID, subjectType, subjectID, capability, now)
	if err != nil {
		return nil, fmt.Errorf("select prices: %w", err)
	}
	defer func() { _ = rows.Close() }()
	var out []billing.Price
	for rows.Next() {
		var p billing.Price
		var unit string
		if err := rows.Scan(&p.SubjectType, &p.SubjectID, &p.Capability, &unit,
			&p.RateMicros, &p.UnitQuantity, &p.Currency, &p.EffectiveFrom); err != nil {
			return nil, fmt.Errorf("scan price: %w", err)
		}
		p.Unit = billing.UnitKind(unit)
		out = append(out, p)
	}
	return out, rows.Err()
}

// Liabilities reconstructs outstanding plus settled liability attributed to
// scope, aggregated in SQL to one row per (state, currency). Held rows
// always count; unknown rows count only when created inside the window
// (stale unknowns age out instead of blocking budgets forever); settled
// usage counts when recorded inside it. Held/unknown unit sums fold the
// prompt kinds once per row — max(input, cached_input) under 'input' —
// matching the reserve bound; settled units stay disjoint. Tenant scope is
// tenant-wide over canonical rows; other subjects match their own
// attribution rows, then their settled usage via the usage record's subject.
func (t billingTx) Liabilities(ctx context.Context, tenantID string, scope billing.Subject, from time.Time) ([]billing.LiabilityRow, error) {
	return queryLiabilities(ctx, t.tx, tenantID, scope, from, time.Time{})
}

// billingQuerier is satisfied by *sql.DB and *sql.Tx.
type billingQuerier interface {
	QueryContext(ctx context.Context, query string, args ...any) (*sql.Rows, error)
}

// queryLiabilities bounds the window at [from, until): a zero until means
// open-ended (the admission read); a real until scopes the projection to
// liability created before it — the ended-budget read path.
func queryLiabilities(ctx context.Context, q billingQuerier, tenantID string, scope billing.Subject, from, until time.Time) ([]billing.LiabilityRow, error) {
	var untilArg any
	if !until.IsZero() {
		untilArg = until
	}
	acc := map[[2]string]*billing.LiabilityRow{}
	order := [][2]string{}
	add := func(state, currency string, amount int64) *billing.LiabilityRow {
		k := [2]string{state, currency}
		l := acc[k]
		if l == nil {
			l = &billing.LiabilityRow{State: state, Currency: currency, Units: map[billing.UnitKind]int64{}}
			acc[k] = l
			order = append(order, k)
		}
		l.AmountMicros += amount
		return l
	}

	// Held always counts; unknown only inside the window.
	rows, err := q.QueryContext(ctx, `
		SELECT state, currency::text, COALESCE(SUM(limit_amount_micros), 0)
		FROM reservations
		WHERE tenant_id = $1::uuid
		  AND ($5::timestamptz IS NULL OR created_at < $5)
		  AND (state = 'held' OR (state = 'unknown' AND created_at >= $4))
		  AND (CASE WHEN $2 = 'tenant' THEN id NOT LIKE '%/%'
		            ELSE subject_type = $2 AND subject_id = $3 END)
		GROUP BY state, currency`,
		tenantID, scope.Type, scope.ID, from, untilArg)
	if err != nil {
		return nil, fmt.Errorf("held liabilities: %w", err)
	}
	if err := scanAmountRows(rows, add); err != nil {
		return nil, err
	}

	// Per-kind unit sums over the same held/unknown rows, folding each row's
	// prompt kinds to max(input, cached_input) once under 'input'.
	rows, err = q.QueryContext(ctx, `
		SELECT state, currency::text, kind, SUM(units) FROM (
			SELECT state, currency,
			       CASE WHEN e.key = 'cached_input' THEN 'input' ELSE e.key END AS kind,
			       CASE WHEN e.key IN ('input','cached_input') AND reserved_units ? 'input' AND reserved_units ? 'cached_input'
			            THEN GREATEST((reserved_units->>'input')::bigint, (reserved_units->>'cached_input')::bigint)
			            ELSE e.value::bigint END AS units
			FROM reservations, LATERAL jsonb_each_text(reserved_units) e
			WHERE NOT (e.key = 'cached_input' AND reserved_units ? 'input')
			  AND tenant_id = $1::uuid
			  AND ($5::timestamptz IS NULL OR created_at < $5)
			  AND (state = 'held' OR (state = 'unknown' AND created_at >= $4))
			  AND (CASE WHEN $2 = 'tenant' THEN id NOT LIKE '%/%'
			            ELSE subject_type = $2 AND subject_id = $3 END)
		) u GROUP BY state, currency, kind`,
		tenantID, scope.Type, scope.ID, from, untilArg)
	if err != nil {
		return nil, fmt.Errorf("held liability units: %w", err)
	}
	if err := scanUnitRows(rows, add); err != nil {
		return nil, err
	}

	// Settled usage inside the window: disjoint plain sums per scope rule.
	var settledScope string
	args := []any{tenantID, from, untilArg}
	switch scope.Type {
	case billing.BudgetSubjectTenant:
		settledScope = ""
	case billing.BudgetSubjectAPIKey:
		// Key-scoped settled usage matches the verified credential
		// attribution column plus rows whose canonical subject is the key —
		// a row is returned once either way, so nothing double-counts.
		settledScope = ` AND (credential_id = $4 OR (subject_type = 'api_key' AND subject_id = $4))`
		args = append(args, scope.ID)
	default:
		settledScope = ` AND subject_type = $4 AND subject_id = $5`
		args = append(args, scope.Type, scope.ID)
	}
	rows, err = q.QueryContext(ctx, `
		SELECT 'settled', currency::text, COALESCE(SUM(amount_micros), 0)
		FROM usage_records
		WHERE tenant_id = $1::uuid AND created_at >= $2
		  AND ($3::timestamptz IS NULL OR created_at < $3)`+settledScope+`
		GROUP BY currency`, args...)
	if err != nil {
		return nil, fmt.Errorf("settled liabilities: %w", err)
	}
	if err := scanAmountRows(rows, add); err != nil {
		return nil, err
	}
	rows, err = q.QueryContext(ctx, `
		SELECT 'settled', currency::text, e.key, SUM(e.value::bigint)
		FROM usage_records, LATERAL jsonb_each_text(units) e
		WHERE tenant_id = $1::uuid AND created_at >= $2
		  AND ($3::timestamptz IS NULL OR created_at < $3)`+settledScope+`
		GROUP BY currency, e.key`, args...)
	if err != nil {
		return nil, fmt.Errorf("settled liability units: %w", err)
	}
	if err := scanUnitRows(rows, add); err != nil {
		return nil, err
	}

	out := make([]billing.LiabilityRow, 0, len(order))
	for _, k := range order {
		out = append(out, *acc[k])
	}
	return out, nil
}

// scanAmountRows folds (state, currency, sum) aggregate rows into the
// accumulator keyed by add.
func scanAmountRows(rows *sql.Rows, add func(state, currency string, amount int64) *billing.LiabilityRow) error {
	defer func() { _ = rows.Close() }()
	for rows.Next() {
		var state, currency string
		var amount int64
		if err := rows.Scan(&state, &currency, &amount); err != nil {
			return fmt.Errorf("scan liability amount: %w", err)
		}
		add(state, currency, amount)
	}
	return rows.Err()
}

// scanUnitRows folds (state, currency, kind, sum) aggregate rows into the
// accumulator.
func scanUnitRows(rows *sql.Rows, add func(state, currency string, amount int64) *billing.LiabilityRow) error {
	defer func() { _ = rows.Close() }()
	for rows.Next() {
		var state, currency, kind string
		var units int64
		if err := rows.Scan(&state, &currency, &kind, &units); err != nil {
			return fmt.Errorf("scan liability units: %w", err)
		}
		add(state, currency, 0).Units[billing.UnitKind(kind)] += units
	}
	return rows.Err()
}

// SweepStaleHolds ages out stale holds across every tenant in two updates:
// never-dispatched stale holds release (no request bytes could have been
// sent); dispatched ones become unknown so liability is retained for
// reconciliation. It returns per-outcome row counts.
func (r *BillingRepo) SweepStaleHolds(ctx context.Context, olderThan time.Time) (released, unknown int, err error) {
	err = WithTx(ctx, r.db, func(tx *sql.Tx) error {
		res, err := tx.ExecContext(ctx, `
			UPDATE reservations SET state = 'released'
			WHERE state = 'held' AND dispatched_at IS NULL AND created_at < $1`,
			olderThan)
		if err != nil {
			return fmt.Errorf("sweep undispatched holds: %w", err)
		}
		n, err := res.RowsAffected()
		if err != nil {
			return err
		}
		released = int(n)
		res, err = tx.ExecContext(ctx, `
			UPDATE reservations SET state = 'unknown'
			WHERE state = 'held' AND dispatched_at IS NOT NULL AND created_at < $1`,
			olderThan)
		if err != nil {
			return fmt.Errorf("sweep dispatched holds: %w", err)
		}
		n, err = res.RowsAffected()
		if err != nil {
			return err
		}
		unknown = int(n)
		return nil
	})
	return released, unknown, err
}

// InsertReservations writes every liability row the service assigned: the
// canonical row (id == reservation id) plus per-subject attribution rows.
func (t billingTx) InsertReservations(ctx context.Context, rows []billing.Reservation) error {
	for _, res := range rows {
		snap, err := json.Marshal(res.Price)
		if err != nil {
			return fmt.Errorf("marshal price snapshot: %w", err)
		}
		units, err := encodeUnits(res.Units)
		if err != nil {
			return err
		}
		_, err = t.tx.ExecContext(ctx, `
			INSERT INTO reservations
				(tenant_id, id, job_id, attempt_id, subject_type, subject_id,
				 price_id, price_snapshot, reserved_units, limit_amount_micros,
				 currency, state, capability, model, endpoint_id, credential_id)
			VALUES ($1::uuid, $2, $3, $4, $5, $6, NULL, $7, $8, $9, $10, 'held', $11, $12, $13, $14)`,
			res.TenantID, res.ID, nullStr(res.JobID), nullStr(res.AttemptID),
			res.Subject.Type, res.Subject.ID, string(snap), units,
			res.LimitAmountMicros, res.Currency, res.Capability, res.Model,
			nullStr(res.EndpointID), res.CredentialID)
		if err != nil {
			return fmt.Errorf("insert reservation: %w", err)
		}
	}
	return nil
}

// GetReservationForUpdate locks the canonical row.
func (t billingTx) GetReservationForUpdate(ctx context.Context, tenantID, id string) (billing.Reservation, error) {
	return scanReservation(t.tx.QueryRowContext(ctx, `
		SELECT `+reservationCols+`
		FROM reservations WHERE tenant_id = $1::uuid AND id = $2
		FOR UPDATE`, tenantID, id))
}

// Transition moves every row of a reservation between states; settle also
// stamps settled_at. Idempotent guard: only rows still in `from` move.
func (t billingTx) Transition(ctx context.Context, tenantID, id, from, to string) (int64, error) {
	q := `UPDATE reservations SET state = $4`
	if to == billing.ReservationSettled {
		q += `, settled_at = now()`
	}
	q += ` WHERE tenant_id = $1::uuid AND (id = $2 OR id LIKE $2 || '/%') AND state = $3`
	res, err := t.tx.ExecContext(ctx, q, tenantID, id, from, to)
	if err != nil {
		return 0, fmt.Errorf("transition reservation %s→%s: %w", from, to, err)
	}
	n, err := res.RowsAffected()
	if err != nil {
		return 0, err
	}
	return n, nil
}

// MarkDispatched stamps dispatched_at on held rows not yet dispatched; the
// state guard refuses rows a sweep already released or moved to unknown.
func (t billingTx) MarkDispatched(ctx context.Context, tenantID, id string) (int64, error) {
	res, err := t.tx.ExecContext(ctx, `
		UPDATE reservations SET dispatched_at = now()
		WHERE tenant_id = $1::uuid AND (id = $2 OR id LIKE $2 || '/%')
		  AND state = 'held' AND dispatched_at IS NULL`, tenantID, id)
	if err != nil {
		return 0, fmt.Errorf("mark dispatched: %w", err)
	}
	return res.RowsAffected()
}

// InsertUsageRecord appends settlement evidence. UNIQUE(tenant_id,
// reservation_id) enforces once-only settle; the service's held→settled guard
// runs first inside the same transaction.
func (t billingTx) InsertUsageRecord(ctx context.Context, rec billing.UsageRecord) error {
	units, err := encodeUnits(rec.Units)
	if err != nil {
		return err
	}
	snap, err := json.Marshal(rec.Price)
	if err != nil {
		return fmt.Errorf("marshal usage snapshot: %w", err)
	}
	_, err = t.tx.ExecContext(ctx, `
		INSERT INTO usage_records
			(tenant_id, id, reservation_id, subject_type, subject_id,
			 capability, model, endpoint_id, units, amount_micros, currency,
			 price_snapshot, outcome, credential_id)
		VALUES ($1::uuid, $2, $3, $4, $5, $6, $7, $8, $9, $10, $11, $12, $13, $14)`,
		rec.TenantID, rec.ID, rec.ReservationID, rec.Subject.Type, rec.Subject.ID,
		rec.Capability, rec.Model, nullStr(rec.EndpointID), units,
		rec.AmountMicros, rec.Currency, string(snap), rec.Outcome, rec.CredentialID)
	if err != nil {
		return fmt.Errorf("insert usage record: %w", err)
	}
	return nil
}

// GetUsageRecord reads settlement evidence inside the transaction.
func (t billingTx) GetUsageRecord(ctx context.Context, tenantID, reservationID string) (billing.UsageRecord, error) {
	return scanUsageRecord(t.tx.QueryRowContext(ctx, `
		SELECT `+usageCols+`
		FROM usage_records WHERE tenant_id = $1::uuid AND reservation_id = $2`,
		tenantID, reservationID))
}

// --- row scanning ------------------------------------------------------------

const reservationCols = `tenant_id::text, id, subject_type, subject_id,
	price_snapshot::text, reserved_units::text, limit_amount_micros,
	currency::text, state, dispatched_at, settled_at, created_at,
	capability, model, endpoint_id, job_id, attempt_id, credential_id`

func scanReservation(row rowScanner) (billing.Reservation, error) {
	var r billing.Reservation
	var snap, units string
	var dispatched, settled, created sql.NullTime
	var capability, model, endpointID, jobID, attemptID sql.NullString
	err := row.Scan(&r.TenantID, &r.ID, &r.Subject.Type, &r.Subject.ID,
		&snap, &units, &r.LimitAmountMicros, &r.Currency, &r.State,
		&dispatched, &settled, &created, &capability, &model, &endpointID,
		&jobID, &attemptID, &r.CredentialID)
	if err != nil {
		if err == sql.ErrNoRows {
			return billing.Reservation{}, billing.ErrReservationNotFound
		}
		return billing.Reservation{}, fmt.Errorf("scan reservation: %w", err)
	}
	if err := json.Unmarshal([]byte(snap), &r.Price); err != nil {
		return billing.Reservation{}, fmt.Errorf("decode price snapshot: %w", err)
	}
	u, err := decodeUnits(json.RawMessage(units))
	if err != nil {
		return billing.Reservation{}, err
	}
	r.Units = u
	if dispatched.Valid {
		tv := dispatched.Time
		r.DispatchedAt = &tv
	}
	if settled.Valid {
		tv := settled.Time
		r.SettledAt = &tv
	}
	r.CreatedAt = created.Time
	r.Capability = capability.String
	r.Model = model.String
	r.EndpointID = endpointID.String
	r.JobID = jobID.String
	r.AttemptID = attemptID.String
	return r, nil
}

const usageCols = `tenant_id::text, id, reservation_id, subject_type,
	subject_id, capability, model, endpoint_id, units::text, amount_micros,
	currency::text, price_snapshot::text, outcome, created_at, credential_id`

func scanUsageRecord(row rowScanner) (billing.UsageRecord, error) {
	var r billing.UsageRecord
	var units, snap string
	var endpointID sql.NullString
	err := row.Scan(&r.TenantID, &r.ID, &r.ReservationID, &r.Subject.Type,
		&r.Subject.ID, &r.Capability, &r.Model, &endpointID, &units,
		&r.AmountMicros, &r.Currency, &snap, &r.Outcome, &r.CreatedAt,
		&r.CredentialID)
	if err != nil {
		if err == sql.ErrNoRows {
			return billing.UsageRecord{}, billing.ErrReservationNotFound
		}
		return billing.UsageRecord{}, fmt.Errorf("scan usage record: %w", err)
	}
	u, err := decodeUnits(json.RawMessage(units))
	if err != nil {
		return billing.UsageRecord{}, err
	}
	r.Units = u
	r.EndpointID = endpointID.String
	if err := json.Unmarshal([]byte(snap), &r.Price); err != nil {
		return billing.UsageRecord{}, fmt.Errorf("decode usage snapshot: %w", err)
	}
	return r, nil
}

// encodeUnits writes the per-kind map as {unit_kind: units} JSON.
func encodeUnits(units map[billing.UnitKind]int64) (string, error) {
	m := make(map[string]int64, len(units))
	for k, v := range units {
		m[string(k)] = v
	}
	raw, err := json.Marshal(m)
	if err != nil {
		return "", fmt.Errorf("marshal units: %w", err)
	}
	return string(raw), nil
}

// decodeUnits reads a {unit_kind: units} JSON map.
func decodeUnits(raw json.RawMessage) (map[billing.UnitKind]int64, error) {
	m := map[string]int64{}
	if err := json.Unmarshal(raw, &m); err != nil {
		return nil, fmt.Errorf("decode units: %w", err)
	}
	out := make(map[billing.UnitKind]int64, len(m))
	for k, v := range m {
		out[billing.UnitKind(k)] = v
	}
	return out, nil
}

// strArray renders a postgres text[] literal for unnest; elements are quoted
// so subject ids containing separators stay literal.
func strArray(in []string) string {
	out := "{"
	for i, s := range in {
		if i > 0 {
			out += ","
		}
		out += `"`
		for _, r := range s {
			if r == '"' || r == '\\' {
				out += `\`
			}
			out += string(r)
		}
		out += `"`
	}
	return out + "}"
}

func newRowID(prefix string) string {
	var b [16]byte
	if _, err := crand.Read(b[:]); err != nil {
		panic(err)
	}
	return prefix + "_" + hex.EncodeToString(b[:])
}

// --- admin/usage reads (httpx billing cutover, 01-17) -----------------------

// PriceRow is one persisted price row with its storage id and provenance.
type PriceRow struct {
	billing.Price
	ID        string
	CreatedBy string
	CreatedAt time.Time
}

// ListPrices returns tenant price rows, optionally filtered by subject,
// newest-effective first.
func (r *BillingRepo) ListPrices(ctx context.Context, tenantID, subjectType, subjectID, capability string, limit, offset int) ([]PriceRow, error) {
	q := `
		SELECT id, subject_type, subject_id, capability, unit_kind,
		       rate_micros, unit_quantity, currency, effective_from,
		       COALESCE(created_by, ''), created_at
		FROM billing_prices
		WHERE tenant_id = $1::uuid`
	args := []any{tenantID}
	n := 1
	if subjectType != "" {
		n++
		q += fmt.Sprintf(" AND subject_type = $%d", n)
		args = append(args, subjectType)
	}
	if subjectID != "" {
		n++
		q += fmt.Sprintf(" AND subject_id = $%d", n)
		args = append(args, subjectID)
	}
	if capability != "" {
		n++
		q += fmt.Sprintf(" AND capability = $%d", n)
		args = append(args, capability)
	}
	q += fmt.Sprintf(" ORDER BY effective_from DESC, id LIMIT $%d OFFSET $%d", n+1, n+2) //nolint:gosec // fragment is constant, values bound as parameters
	args = append(args, limit, offset)
	rows, err := r.db.QueryContext(ctx, q, args...)
	if err != nil {
		return nil, fmt.Errorf("list prices: %w", err)
	}
	defer func() { _ = rows.Close() }()
	var out []PriceRow
	for rows.Next() {
		var p PriceRow
		var unit string
		if err := rows.Scan(&p.ID, &p.SubjectType, &p.SubjectID, &p.Capability,
			&unit, &p.RateMicros, &p.UnitQuantity, &p.Currency,
			&p.EffectiveFrom, &p.CreatedBy, &p.CreatedAt); err != nil {
			return nil, fmt.Errorf("scan price: %w", err)
		}
		p.Unit = billing.UnitKind(unit)
		out = append(out, p)
	}
	return out, rows.Err()
}

// PriceAuditRow is one billing_price_audit row.
type PriceAuditRow struct {
	ID        string
	PriceID   string
	Action    string
	ActorType string
	ActorID   string
	Detail    map[string]any
	CreatedAt time.Time
}

// ListPriceAudit returns pricing audit rows, newest first.
func (r *BillingRepo) ListPriceAudit(ctx context.Context, tenantID string, limit, offset int) ([]PriceAuditRow, error) {
	rows, err := r.db.QueryContext(ctx, `
		SELECT id, price_id, action, actor_type, actor_id, detail::text, created_at
		FROM billing_price_audit
		WHERE tenant_id = $1::uuid
		ORDER BY created_at DESC, id LIMIT $2 OFFSET $3`, tenantID, limit, offset)
	if err != nil {
		return nil, fmt.Errorf("list price audit: %w", err)
	}
	defer func() { _ = rows.Close() }()
	var out []PriceAuditRow
	for rows.Next() {
		var a PriceAuditRow
		var detail string
		if err := rows.Scan(&a.ID, &a.PriceID, &a.Action, &a.ActorType, &a.ActorID, &detail, &a.CreatedAt); err != nil {
			return nil, fmt.Errorf("scan price audit: %w", err)
		}
		a.Detail = map[string]any{}
		if detail != "" {
			if err := json.Unmarshal([]byte(detail), &a.Detail); err != nil {
				return nil, fmt.Errorf("decode price audit detail: %w", err)
			}
		}
		out = append(out, a)
	}
	return out, rows.Err()
}

// BillingAuditRow is one billing_audit event row.
type BillingAuditRow struct {
	ID          string
	SubjectType string
	SubjectID   string
	Action      string
	ActorType   string
	ActorID     string
	Detail      map[string]any
	CreatedAt   time.Time
}

// CreateBillingAudit appends one billing audit event.
func (r *BillingRepo) CreateBillingAudit(ctx context.Context, tenantID string, row BillingAuditRow) error {
	detail, err := json.Marshal(row.Detail)
	if err != nil {
		return fmt.Errorf("marshal billing audit detail: %w", err)
	}
	_, err = r.db.ExecContext(ctx, `
		INSERT INTO billing_audit
			(tenant_id, id, subject_type, subject_id, action, actor_type, actor_id, detail)
		VALUES ($1::uuid, $2, $3, $4, $5, $6, $7, $8)`,
		tenantID, row.ID, row.SubjectType, row.SubjectID, row.Action,
		row.ActorType, row.ActorID, string(detail))
	if err != nil {
		return fmt.Errorf("insert billing audit: %w", err)
	}
	return nil
}

// RecordRequestRefused stores only billing admission metadata.
func (r *BillingRepo) RecordRequestRefused(ctx context.Context, tenantID string, subject billing.Subject, actorType, actorID, model, capability, reason string) error {
	return r.CreateBillingAudit(ctx, tenantID, BillingAuditRow{
		ID: newRowID("baudit"), SubjectType: subject.Type, SubjectID: subject.ID,
		Action: billing.AuditRequestRefused, ActorType: actorType, ActorID: actorID,
		Detail: map[string]any{"reason": reason, "model": model, "capability": capability},
	})
}

// ListBillingAudit returns audit rows, optionally filtered by subject.
func (r *BillingRepo) ListBillingAudit(ctx context.Context, tenantID, subjectType, subjectID string, limit, offset int) ([]BillingAuditRow, error) {
	q := `
		SELECT id, subject_type, subject_id, action, actor_type, actor_id,
		       detail::text, created_at
		FROM billing_audit
		WHERE tenant_id = $1::uuid`
	args := []any{tenantID}
	n := 1
	if subjectType != "" {
		n++
		q += fmt.Sprintf(" AND subject_type = $%d", n)
		args = append(args, subjectType)
	}
	if subjectID != "" {
		n++
		q += fmt.Sprintf(" AND subject_id = $%d", n)
		args = append(args, subjectID)
	}
	q += fmt.Sprintf(" ORDER BY created_at DESC, id LIMIT $%d OFFSET $%d", n+1, n+2) //nolint:gosec // fragment is constant, values bound as parameters
	args = append(args, limit, offset)
	rows, err := r.db.QueryContext(ctx, q, args...)
	if err != nil {
		return nil, fmt.Errorf("list billing audit: %w", err)
	}
	defer func() { _ = rows.Close() }()
	var out []BillingAuditRow
	for rows.Next() {
		var a BillingAuditRow
		var detail string
		if err := rows.Scan(&a.ID, &a.SubjectType, &a.SubjectID, &a.Action,
			&a.ActorType, &a.ActorID, &detail, &a.CreatedAt); err != nil {
			return nil, fmt.Errorf("scan billing audit: %w", err)
		}
		a.Detail = map[string]any{}
		if detail != "" {
			if err := json.Unmarshal([]byte(detail), &a.Detail); err != nil {
				return nil, fmt.Errorf("decode billing audit detail: %w", err)
			}
		}
		out = append(out, a)
	}
	return out, rows.Err()
}

// GetBudget returns one tenant budget row regardless of its active window —
// a budget whose starts_at lies in the future must stay editable.
func (r *BillingRepo) GetBudget(ctx context.Context, tenantID, id string) (billing.Budget, error) {
	rows, err := r.db.QueryContext(ctx, `
		SELECT `+budgetCols+`
		FROM budgets WHERE tenant_id = $1::uuid AND id = $2`, tenantID, id)
	if err != nil {
		return billing.Budget{}, fmt.Errorf("get budget: %w", err)
	}
	budgets, err := scanBudgets(rows)
	if err != nil {
		return billing.Budget{}, err
	}
	if len(budgets) == 0 {
		return billing.Budget{}, ErrBudgetNotFound
	}
	return budgets[0], nil
}

// UpdateBudgetLimit narrows one budget's limit_value — the liability window,
// period, subject and currency columns are never touched.
func (r *BillingRepo) UpdateBudgetLimit(ctx context.Context, tenantID, id string, limitValue int64) error {
	res, err := r.db.ExecContext(ctx, `
		UPDATE budgets SET limit_value = $3
		WHERE tenant_id = $1::uuid AND id = $2`, tenantID, id, limitValue)
	if err != nil {
		return fmt.Errorf("update budget limit: %w", err)
	}
	if n, _ := res.RowsAffected(); n == 0 {
		return ErrBudgetNotFound
	}
	return nil
}

// ListAllActiveBudgets returns every active tenant budget at now — the
// all-subject administrative read behind the budgets screen (platform.admin
// only; member budget reads keep using the subject-filtered list).
func (r *BillingRepo) ListAllActiveBudgets(ctx context.Context, tenantID string, now time.Time) ([]billing.Budget, error) {
	rows, err := r.db.QueryContext(ctx, budgetSelectSQL+`
		ORDER BY subject_type, subject_id, id`, tenantID, now)
	if err != nil {
		return nil, fmt.Errorf("list all budgets: %w", err)
	}
	return scanBudgets(rows)
}

// LatestPrices is the non-transactional SelectPrices twin used by display and
// routing price-coverage checks.
func (r *BillingRepo) LatestPrices(ctx context.Context, tenantID, subjectType, subjectID, capability string, now time.Time) ([]billing.Price, error) {
	rows, err := r.db.QueryContext(ctx, `
		SELECT DISTINCT ON (unit_kind)
		       subject_type, subject_id, capability, unit_kind,
		       rate_micros, unit_quantity, currency, effective_from
		FROM billing_prices
		WHERE tenant_id = $1::uuid AND subject_type = $2 AND subject_id = $3
		  AND capability = $4 AND effective_from <= $5
		ORDER BY unit_kind, effective_from DESC`, tenantID, subjectType, subjectID, capability, now)
	if err != nil {
		return nil, fmt.Errorf("latest prices: %w", err)
	}
	defer func() { _ = rows.Close() }()
	var out []billing.Price
	for rows.Next() {
		var p billing.Price
		var unit string
		if err := rows.Scan(&p.SubjectType, &p.SubjectID, &p.Capability, &unit,
			&p.RateMicros, &p.UnitQuantity, &p.Currency, &p.EffectiveFrom); err != nil {
			return nil, fmt.Errorf("scan price: %w", err)
		}
		p.Unit = billing.UnitKind(unit)
		out = append(out, p)
	}
	return out, rows.Err()
}

// ListActiveBudgets returns active budgets applicable to any of subjects at
// now — the non-transactional twin of LockActiveBudgets for reads.
func (r *BillingRepo) ListActiveBudgets(ctx context.Context, tenantID string, subjects []billing.Subject, now time.Time) ([]billing.Budget, error) {
	if len(subjects) == 0 {
		return nil, nil
	}
	types := make([]string, len(subjects))
	ids := make([]string, len(subjects))
	for i, s := range subjects {
		types[i], ids[i] = s.Type, s.ID
	}
	rows, err := r.db.QueryContext(ctx, budgetSelectSQL+`
		  AND (subject_type, subject_id) IN (SELECT * FROM unnest($3::text[], $4::text[]))
		ORDER BY subject_type, subject_id, id`, tenantID, now, strArray(types), strArray(ids))
	if err != nil {
		return nil, fmt.Errorf("list budgets: %w", err)
	}
	return scanBudgets(rows)
}

// Liabilities is the read-path liability reconstruction used by the retained
// EnforceBudget guard — identical semantics to the transactional read.
func (r *BillingRepo) Liabilities(ctx context.Context, tenantID string, scope billing.Subject, from time.Time) ([]billing.LiabilityRow, error) {
	return queryLiabilities(ctx, r.db, tenantID, scope, from, time.Time{})
}

// LiabilitiesInWindow scopes the same reconstruction to [from, until): a
// zero until is open-ended; a real until drops liability created at or
// after it — the ended-budget final-window read.
func (r *BillingRepo) LiabilitiesInWindow(ctx context.Context, tenantID string, scope billing.Subject, from, until time.Time) ([]billing.LiabilityRow, error) {
	return queryLiabilities(ctx, r.db, tenantID, scope, from, until)
}

// UsageGroupSum aggregates settled usage_records per group key.
type UsageGroupSum struct {
	Key          string
	SubjectType  string
	SubjectID    string
	RequestCount int64
	TotalUnits   int64
	AmountMicros int64
	Currency     string
}

// usageUnitsTotalSQL sums every kind in the per-record units map.
const usageUnitsTotalSQL = `COALESCE(SUM((
	SELECT SUM(v::bigint) FROM jsonb_each_text(units) AS e(k, v))), 0)`

// UsageTotals returns tenant-wide settled usage aggregates since `since`,
// grouped per currency — a mixed-currency tenant yields one row each.
func (r *BillingRepo) UsageTotals(ctx context.Context, tenantID string, since time.Time) ([]UsageGroupSum, error) {
	rows, err := r.db.QueryContext(ctx, `
		SELECT currency::text, COUNT(*), `+usageUnitsTotalSQL+`,
		       COALESCE(SUM(amount_micros), 0)
		FROM usage_records
		WHERE tenant_id = $1::uuid AND created_at >= $2
		GROUP BY currency`, tenantID, since)
	if err != nil {
		return nil, fmt.Errorf("usage totals: %w", err)
	}
	defer func() { _ = rows.Close() }()
	var out []UsageGroupSum
	for rows.Next() {
		var s UsageGroupSum
		if err := rows.Scan(&s.Currency, &s.RequestCount, &s.TotalUnits, &s.AmountMicros); err != nil {
			return nil, fmt.Errorf("scan usage totals: %w", err)
		}
		out = append(out, s)
	}
	return out, rows.Err()
}

// UsageBySubject aggregates settled usage per billing subject, plus per-key
// attribution rows: settled usage carrying a credential_id also emits a
// subject_type='api_key' row keyed by that credential. Canonical subject
// rows stay untouched, so per-owner totals never double-count.
func (r *BillingRepo) UsageBySubject(ctx context.Context, tenantID string, since time.Time) ([]UsageGroupSum, error) {
	rows, err := r.db.QueryContext(ctx, `
		SELECT subject_type, subject_id, COUNT(*), `+usageUnitsTotalSQL+`,
		       COALESCE(SUM(amount_micros), 0), currency::text
		FROM usage_records
		WHERE tenant_id = $1::uuid AND created_at >= $2
		GROUP BY subject_type, subject_id, currency
		UNION ALL
		SELECT 'api_key', credential_id, COUNT(*), `+usageUnitsTotalSQL+`,
		       COALESCE(SUM(amount_micros), 0), currency::text
		FROM usage_records
		WHERE tenant_id = $1::uuid AND created_at >= $2 AND credential_id <> ''
		GROUP BY credential_id, currency
		ORDER BY subject_type, subject_id`, tenantID, since)
	if err != nil {
		return nil, fmt.Errorf("usage by subject: %w", err)
	}
	defer func() { _ = rows.Close() }()
	var out []UsageGroupSum
	for rows.Next() {
		var s UsageGroupSum
		if err := rows.Scan(&s.SubjectType, &s.SubjectID, &s.RequestCount,
			&s.TotalUnits, &s.AmountMicros, &s.Currency); err != nil {
			return nil, fmt.Errorf("scan usage by subject: %w", err)
		}
		out = append(out, s)
	}
	return out, rows.Err()
}

// UsageByEndpoint aggregates settled usage per endpoint id.
func (r *BillingRepo) UsageByEndpoint(ctx context.Context, tenantID string, since time.Time) ([]UsageGroupSum, error) {
	rows, err := r.db.QueryContext(ctx, `
		SELECT COALESCE(endpoint_id, ''), COUNT(*), `+usageUnitsTotalSQL+`,
		       COALESCE(SUM(amount_micros), 0), currency::text
		FROM usage_records
		WHERE tenant_id = $1::uuid AND created_at >= $2
		GROUP BY endpoint_id, currency
		ORDER BY COUNT(*) DESC`, tenantID, since)
	if err != nil {
		return nil, fmt.Errorf("usage by endpoint: %w", err)
	}
	return scanGroupSums(rows)
}

// UsageByModel aggregates settled usage per model.
func (r *BillingRepo) UsageByModel(ctx context.Context, tenantID string, since time.Time) ([]UsageGroupSum, error) {
	rows, err := r.db.QueryContext(ctx, `
		SELECT model, COUNT(*), `+usageUnitsTotalSQL+`,
		       COALESCE(SUM(amount_micros), 0), currency::text
		FROM usage_records
		WHERE tenant_id = $1::uuid AND created_at >= $2
		GROUP BY model, currency
		ORDER BY COUNT(*) DESC`, tenantID, since)
	if err != nil {
		return nil, fmt.Errorf("usage by model: %w", err)
	}
	return scanGroupSums(rows)
}

func scanGroupSums(rows *sql.Rows) ([]UsageGroupSum, error) {
	defer func() { _ = rows.Close() }()
	var out []UsageGroupSum
	for rows.Next() {
		var s UsageGroupSum
		if err := rows.Scan(&s.Key, &s.RequestCount, &s.TotalUnits,
			&s.AmountMicros, &s.Currency); err != nil {
			return nil, fmt.Errorf("scan usage group: %w", err)
		}
		out = append(out, s)
	}
	return out, rows.Err()
}

// SubjectUsageSince aggregates settled usage for one subject since `since`
// — the usage-display half of the retained budget guard. An api_key subject
// resolves through the credential attribution column.
func (r *BillingRepo) SubjectUsageSince(ctx context.Context, tenantID string, subject billing.Subject, since time.Time) (totalUnits, amountMicros int64, err error) {
	var units sql.NullInt64
	var micros sql.NullInt64
	q := `
		SELECT ` + usageUnitsTotalSQL + `, COALESCE(SUM(amount_micros), 0)
		FROM usage_records
		WHERE tenant_id = $1::uuid AND created_at >= $2`
	args := []any{tenantID, since}
	if subject.Type == billing.BudgetSubjectAPIKey {
		q += ` AND (credential_id = $3 OR (subject_type = 'api_key' AND subject_id = $3))`
		args = append(args, subject.ID)
	} else {
		q += ` AND subject_type = $3 AND subject_id = $4`
		args = append(args, subject.Type, subject.ID)
	}
	err = r.db.QueryRowContext(ctx, q, args...).Scan(&units, &micros)
	if err != nil {
		return 0, 0, fmt.Errorf("subject usage since: %w", err)
	}
	return units.Int64, micros.Int64, nil
}

// DailyUsageRow is one UTC day bucket of settled usage for one currency.
type DailyUsageRow struct {
	Day          time.Time
	RequestCount int64
	TotalUnits   int64
	AmountMicros int64
	Currency     string
}

// ListDailyUsage groups settled usage records into explicit UTC day bins
// (date_trunc over created_at AT TIME ZONE 'UTC') per currency, so a non-UTC
// session TimeZone can never shift a record across the day boundary.
func (r *BillingRepo) ListDailyUsage(ctx context.Context, tenantID string, since time.Time) ([]DailyUsageRow, error) {
	rows, err := r.db.QueryContext(ctx, `
		SELECT date_trunc('day', created_at AT TIME ZONE 'UTC'),
		       COUNT(*), `+usageUnitsTotalSQL+`,
		       COALESCE(SUM(amount_micros), 0), currency::text
		FROM usage_records
		WHERE tenant_id = $1::uuid AND created_at >= $2
		GROUP BY 1, currency
		ORDER BY 1 ASC, currency ASC`, tenantID, since)
	if err != nil {
		return nil, fmt.Errorf("daily usage: %w", err)
	}
	defer func() { _ = rows.Close() }()
	var out []DailyUsageRow
	for rows.Next() {
		var row DailyUsageRow
		if err := rows.Scan(&row.Day, &row.RequestCount, &row.TotalUnits,
			&row.AmountMicros, &row.Currency); err != nil {
			return nil, fmt.Errorf("scan daily usage: %w", err)
		}
		out = append(out, row)
	}
	return out, rows.Err()
}

// ListUsageByTeam groups settled usage by the billing subject's team label.
// user subjects join to users.team (empty team = the literal "No team"
// bucket); service accounts and keys group under their own subject-derived
// label — no team is fabricated for them. One row per bucket per currency.
func (r *BillingRepo) ListUsageByTeam(ctx context.Context, tenantID string, since time.Time) ([]UsageGroupSum, error) {
	rows, err := r.db.QueryContext(ctx, `
		SELECT CASE ur.subject_type
		       WHEN 'user' THEN COALESCE(NULLIF(u.team, ''), 'No team')
		       ELSE ur.subject_type || ':' || ur.subject_id
		       END AS bucket,
		       COUNT(*), `+usageUnitsTotalSQL+`,
		       COALESCE(SUM(ur.amount_micros), 0), ur.currency::text
		FROM usage_records ur
		LEFT JOIN users u
		  ON ur.subject_type = 'user' AND u.tenant_id = ur.tenant_id AND u.id = ur.subject_id
		WHERE ur.tenant_id = $1::uuid AND ur.created_at >= $2
		GROUP BY bucket, ur.currency
		ORDER BY bucket, ur.currency`, tenantID, since)
	if err != nil {
		return nil, fmt.Errorf("usage by team: %w", err)
	}
	return scanGroupSums(rows)
}
