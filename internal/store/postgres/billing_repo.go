// billing_repo.go implements billing.BillingRepository over migration 005 —
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
	"fmt"
	"time"

	"enclava-go/internal/billing"
)

// BillingRepo is the postgres-backed billing.BillingRepository.
type BillingRepo struct {
	db *sql.DB
}

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
	return WithTx(ctx, r.db, func(tx *sql.Tx) error {
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
	})
}

// CreateBudget inserts an enforceable limit row. Amount budgets carry their
// bound currency; token budgets store NULL currency.
func (r *BillingRepo) CreateBudget(ctx context.Context, tenantID, id string, b billing.Budget) error {
	var currency any
	if b.Kind == billing.BudgetKindAmount {
		currency = b.Currency
	}
	_, err := r.db.ExecContext(ctx, `
		INSERT INTO budgets
			(tenant_id, id, name, subject_type, subject_id, kind, limit_value,
			 currency, period, starts_at, ends_at)
		VALUES ($1::uuid, $2, $3, $4, $5, $6, $7, $8, $9, $10, $11)`,
		tenantID, id, "budget", b.Subject.Type, b.Subject.ID, b.Kind,
		b.LimitAmountMicros, currency, b.Period, b.StartsAt, b.EndsAt)
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
// deterministic order so concurrent admissions serialise without deadlocks.
func (t billingTx) LockActiveBudgets(ctx context.Context, tenantID string, subjects []billing.Subject, now time.Time) ([]billing.Budget, error) {
	if len(subjects) == 0 {
		return nil, nil
	}
	types := make([]string, len(subjects))
	ids := make([]string, len(subjects))
	for i, s := range subjects {
		types[i], ids[i] = s.Type, s.ID
	}
	rows, err := t.tx.QueryContext(ctx, `
		SELECT id, subject_type, subject_id, kind, period, limit_value,
		       COALESCE(currency, ''), starts_at, ends_at
		FROM budgets
		WHERE tenant_id = $1::uuid
		  AND active
		  AND starts_at <= $2
		  AND (ends_at IS NULL OR ends_at > $2)
		  AND (subject_type, subject_id) IN (SELECT * FROM unnest($3::text[], $4::text[]))
		ORDER BY subject_type, subject_id, id
		FOR UPDATE`, tenantID, now, strArray(types), strArray(ids))
	if err != nil {
		return nil, fmt.Errorf("lock budgets: %w", err)
	}
	defer rows.Close()
	var out []billing.Budget
	for rows.Next() {
		var b billing.Budget
		var endsAt sql.NullTime
		if err := rows.Scan(&b.ID, &b.Subject.Type, &b.Subject.ID, &b.Kind,
			&b.Period, &b.LimitAmountMicros, &b.Currency, &b.StartsAt, &endsAt); err != nil {
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
	defer rows.Close()
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
// scope. Held and unknown reservations always count (liability is retained);
// settled usage counts when recorded inside the window. Tenant scope is
// tenant-wide over canonical rows; other subjects match their own
// attribution rows, then their settled usage via the usage record's subject.
func (t billingTx) Liabilities(ctx context.Context, tenantID string, scope billing.Subject, from time.Time) ([]billing.LiabilityRow, error) {
	rows, err := t.tx.QueryContext(ctx, `
		SELECT limit_amount_micros, currency::text, reserved_units::text
		FROM reservations
		WHERE tenant_id = $1::uuid
		  AND state IN ('held','unknown')
		  AND (CASE WHEN $2 = 'tenant' THEN id NOT LIKE '%/%'
		            ELSE subject_type = $2 AND subject_id = $3 END)`,
		tenantID, scope.Type, scope.ID)
	if err != nil {
		return nil, fmt.Errorf("held liabilities: %w", err)
	}
	out, err := scanLiabilityRows(rows)
	if err != nil {
		return nil, err
	}

	var urows *sql.Rows
	if scope.Type == billing.BudgetSubjectTenant {
		urows, err = t.tx.QueryContext(ctx, `
			SELECT amount_micros, currency::text, units::text
			FROM usage_records
			WHERE tenant_id = $1::uuid AND created_at >= $2`,
			tenantID, from)
	} else {
		urows, err = t.tx.QueryContext(ctx, `
			SELECT amount_micros, currency::text, units::text
			FROM usage_records
			WHERE tenant_id = $1::uuid AND created_at >= $2
			  AND subject_type = $3 AND subject_id = $4`,
			tenantID, from, scope.Type, scope.ID)
	}
	if err != nil {
		return nil, fmt.Errorf("settled liabilities: %w", err)
	}
	more, err := scanLiabilityRows(urows)
	if err != nil {
		return nil, err
	}
	return append(out, more...), nil
}

func scanLiabilityRows(rows *sql.Rows) ([]billing.LiabilityRow, error) {
	defer rows.Close()
	var out []billing.LiabilityRow
	for rows.Next() {
		var l billing.LiabilityRow
		var unitsJSON string
		if err := rows.Scan(&l.AmountMicros, &l.Currency, &unitsJSON); err != nil {
			return nil, fmt.Errorf("scan liability: %w", err)
		}
		units, err := decodeUnits(json.RawMessage(unitsJSON))
		if err != nil {
			return nil, err
		}
		l.Units = units
		out = append(out, l)
	}
	return out, rows.Err()
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
				 currency, state, capability, model, endpoint_id)
			VALUES ($1::uuid, $2, $3, $4, $5, $6, NULL, $7, $8, $9, $10, 'held', $11, $12, $13)`,
			res.TenantID, res.ID, nullStr(res.JobID), nullStr(res.AttemptID),
			res.Subject.Type, res.Subject.ID, string(snap), string(units),
			res.LimitAmountMicros, res.Currency, res.Capability, res.Model,
			nullStr(res.EndpointID))
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

// MarkDispatched stamps dispatched_at on rows not yet dispatched.
func (t billingTx) MarkDispatched(ctx context.Context, tenantID, id string) (int64, error) {
	res, err := t.tx.ExecContext(ctx, `
		UPDATE reservations SET dispatched_at = now()
		WHERE tenant_id = $1::uuid AND (id = $2 OR id LIKE $2 || '/%')
		  AND dispatched_at IS NULL`, tenantID, id)
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
			 price_snapshot, outcome)
		VALUES ($1::uuid, $2, $3, $4, $5, $6, $7, $8, $9, $10, $11, $12, $13)`,
		rec.TenantID, rec.ID, rec.ReservationID, rec.Subject.Type, rec.Subject.ID,
		rec.Capability, rec.Model, nullStr(rec.EndpointID), string(units),
		rec.AmountMicros, rec.Currency, string(snap), rec.Outcome)
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
	capability, model, endpoint_id, job_id, attempt_id`

func scanReservation(row rowScanner) (billing.Reservation, error) {
	var r billing.Reservation
	var snap, units string
	var dispatched, settled, created sql.NullTime
	var capability, model, endpointID, jobID, attemptID sql.NullString
	err := row.Scan(&r.TenantID, &r.ID, &r.Subject.Type, &r.Subject.ID,
		&snap, &units, &r.LimitAmountMicros, &r.Currency, &r.State,
		&dispatched, &settled, &created, &capability, &model, &endpointID,
		&jobID, &attemptID)
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
	currency::text, price_snapshot::text, outcome, created_at`

func scanUsageRecord(row rowScanner) (billing.UsageRecord, error) {
	var r billing.UsageRecord
	var units, snap string
	var endpointID sql.NullString
	err := row.Scan(&r.TenantID, &r.ID, &r.ReservationID, &r.Subject.Type,
		&r.Subject.ID, &r.Capability, &r.Model, &endpointID, &units,
		&r.AmountMicros, &r.Currency, &snap, &r.Outcome, &r.CreatedAt)
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
