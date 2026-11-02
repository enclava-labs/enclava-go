// billing_reports.go — read/report queries behind the usage screens and the
// platform.admin reconciliation API. Every helper is prefixed Report* so the
// file stays disjoint from billing_repo.go's admission/settlement surface.
// The liability window rule matches the admission read exactly: held
// reservations always count, unknown reservations count only when created
// inside the window, settled usage counts when recorded inside it. Money
// sums are per-currency SQL aggregates — never merged across currencies.
package postgres

import (
	"context"
	"database/sql"
	"encoding/base64"
	"errors"
	"fmt"
	"strconv"
	"strings"
	"time"

	"enclava-go/internal/billing"
)

// ReportUnsettledRow is one per-(subject, currency) liability line over the
// outstanding reservation set: held always counts, unknown counts when the
// reservation was created at or after the caller's window start. Attributed
// marks api_key attribution rows (id "<res>/api_key:<key>") that sit under
// the key, not the owner — callers summing tenant totals use canonical rows
// (Attributed == false) only, so nothing double-counts.
type ReportUnsettledRow struct {
	SubjectType   string
	SubjectID     string
	Currency      string
	HeldMicros    int64
	UnknownMicros int64
	Units         int64
	Attributed    bool
}

// reportUnitsTotalSQL folds one reservation's reserved_units map the way
// liability enforcement does (billing_repo.go): legacy rows that persist both
// prompt kinds count max(input, cached_input) once, never input+cached_input.
const reportUnitsTotalSQL = `COALESCE((
	SELECT SUM(v::bigint) FROM jsonb_each_text(reserved_units) AS e(k, v)
	WHERE e.k <> 'cached_input' OR NOT reserved_units ? 'input'), 0)
	+ CASE WHEN reserved_units ? 'input' AND reserved_units ? 'cached_input'
	       THEN GREATEST((reserved_units->>'input')::bigint, (reserved_units->>'cached_input')::bigint)
	           - (reserved_units->>'input')::bigint
	       ELSE 0 END`

// ReportUnsettledBySubject returns held+unknown liability grouped by
// (subject, currency): canonical reservation rows (id carries no '/')
// grouped by their billing subject plus api_key attribution rows (id
// '<res>/api_key:<key>') grouped under the key. Held rows count regardless
// of age; unknown rows count only when created_at >= since — the liability
// window rule the budget read shares.
func (r *BillingRepo) ReportUnsettledBySubject(ctx context.Context, tenantID string, since time.Time) ([]ReportUnsettledRow, error) {
	rows, err := r.db.QueryContext(ctx, `
		SELECT subject_type, subject_id, currency::text,
		       COALESCE(SUM(limit_amount_micros) FILTER (WHERE state = 'held'), 0),
		       COALESCE(SUM(limit_amount_micros) FILTER (WHERE state = 'unknown'), 0),
		       COALESCE(SUM(units_total), 0), attributed
		FROM (
			SELECT subject_type, subject_id, currency, state,
			       limit_amount_micros, `+reportUnitsTotalSQL+` AS units_total,
			       false AS attributed
			FROM reservations
			WHERE tenant_id = $1::uuid AND id NOT LIKE '%/%'
			  AND (state = 'held' OR (state = 'unknown' AND created_at >= $2))
			UNION ALL
			SELECT subject_type, subject_id, currency, state,
			       limit_amount_micros, `+reportUnitsTotalSQL+` AS units_total,
			       true AS attributed
			FROM reservations
			WHERE tenant_id = $1::uuid AND id LIKE '%/%' AND subject_type = 'api_key'
			  AND (state = 'held' OR (state = 'unknown' AND created_at >= $2))
		) r
		GROUP BY subject_type, subject_id, currency, attributed
		ORDER BY attributed, subject_type, subject_id, currency`, tenantID, since)
	if err != nil {
		return nil, fmt.Errorf("unsettled by subject: %w", err)
	}
	defer rows.Close()
	var out []ReportUnsettledRow
	for rows.Next() {
		var row ReportUnsettledRow
		if err := rows.Scan(&row.SubjectType, &row.SubjectID, &row.Currency,
			&row.HeldMicros, &row.UnknownMicros, &row.Units, &row.Attributed); err != nil {
			return nil, fmt.Errorf("scan unsettled: %w", err)
		}
		out = append(out, row)
	}
	return out, rows.Err()
}

// ReportBudgetRow is one budget row plus its persisted active flag — the
// full-list admin read needs every row regardless of window or activation,
// where LockActiveBudgets/ListAllActiveBudgets serve admission only.
type ReportBudgetRow struct {
	Budget billing.Budget
	Active bool
}

const reportBudgetCols = `
	id, name, subject_type, subject_id, kind, period, limit_value,
	COALESCE(currency, ''), COALESCE(timezone, ''), starts_at, ends_at, active`

// ReportListBudgets returns every tenant budget — scheduled, active, ended
// and deactivated — in deterministic subject/id order.
func (r *BillingRepo) ReportListBudgets(ctx context.Context, tenantID string) ([]ReportBudgetRow, error) {
	rows, err := r.db.QueryContext(ctx, `
		SELECT `+reportBudgetCols+`
		FROM budgets
		WHERE tenant_id = $1::uuid
		ORDER BY subject_type, subject_id, id`, tenantID)
	if err != nil {
		return nil, fmt.Errorf("list budgets: %w", err)
	}
	return scanReportBudgets(rows)
}

// ReportGetBudget returns one budget row with its active flag; unknown and
// cross-tenant ids are one ErrBudgetNotFound, matching GetBudget.
func (r *BillingRepo) ReportGetBudget(ctx context.Context, tenantID, id string) (ReportBudgetRow, error) {
	row := r.db.QueryRowContext(ctx, `
		SELECT `+reportBudgetCols+`
		FROM budgets
		WHERE tenant_id = $1::uuid AND id = $2`, tenantID, id)
	b, err := scanReportBudgetRow(row)
	if errors.Is(err, sql.ErrNoRows) {
		return ReportBudgetRow{}, ErrBudgetNotFound
	}
	return b, err
}

// ReportDeactivateBudget flips active to false. It reports true only when
// the row actually transitioned — an already-inactive budget is the
// idempotent no-op, an unknown/cross-tenant id is ErrBudgetNotFound.
func (r *BillingRepo) ReportDeactivateBudget(ctx context.Context, tenantID, id string) (bool, error) {
	var deactivated string
	err := r.db.QueryRowContext(ctx, `
		UPDATE budgets SET active = false
		WHERE tenant_id = $1::uuid AND id = $2 AND active
		RETURNING id`, tenantID, id).Scan(&deactivated)
	if err == nil {
		return true, nil
	}
	if !errors.Is(err, sql.ErrNoRows) {
		return false, fmt.Errorf("deactivate budget: %w", err)
	}
	var exists int
	if qerr := r.db.QueryRowContext(ctx, `
		SELECT 1 FROM budgets WHERE tenant_id = $1::uuid AND id = $2`,
		tenantID, id).Scan(&exists); qerr != nil {
		if errors.Is(qerr, sql.ErrNoRows) {
			return false, ErrBudgetNotFound
		}
		return false, fmt.Errorf("deactivate budget lookup: %w", qerr)
	}
	return false, nil
}

// ReportPriceCurrencies returns the distinct currencies the tenant's price
// rows bill in — the validity set for amount-budget currency binding. An
// empty set means the tenant prices nothing yet, and any currency binds.
func (r *BillingRepo) ReportPriceCurrencies(ctx context.Context, tenantID string) ([]string, error) {
	rows, err := r.db.QueryContext(ctx, `
		SELECT DISTINCT currency::text FROM billing_prices
		WHERE tenant_id = $1::uuid ORDER BY 1`, tenantID)
	if err != nil {
		return nil, fmt.Errorf("price currencies: %w", err)
	}
	defer rows.Close()
	var out []string
	for rows.Next() {
		var c string
		if err := rows.Scan(&c); err != nil {
			return nil, fmt.Errorf("scan price currency: %w", err)
		}
		out = append(out, c)
	}
	return out, rows.Err()
}

// ReportReservationRow is the canonical reservation projection behind the
// admin reconciliation list — bound micros, the persisted per-kind reserved
// units map (JSON text, decoded by the caller through decodeUnits) and the
// dispatch marker.
type ReportReservationRow struct {
	ID            string
	SubjectType   string
	SubjectID     string
	Capability    string
	Model         string
	EndpointID    string
	BoundMicros   int64
	Currency      string
	ReservedUnits string // raw reserved_units JSON
	Dispatched    bool
	CreatedAt     time.Time
}

// ReportListReservations returns canonical reservation rows (never
// subject-attribution rows) in the given state, newest first over the
// (created_at DESC, id DESC) keyset. It fetches limit+1 so a full page can
// emit the opaque next cursor; an unparseable cursor is ErrInvalidCursor.
func (r *BillingRepo) ReportListReservations(ctx context.Context, tenantID, state string, limit int, cursor string) ([]ReportReservationRow, string, error) {
	if limit <= 0 {
		limit = 500 // page-cap fallback: LIMIT <= 0 would fetch unbounded
	}
	query := `
		SELECT id, subject_type, subject_id, capability, model,
		       COALESCE(endpoint_id, ''), limit_amount_micros, currency::text,
		       reserved_units::text, dispatched_at IS NOT NULL, created_at
		FROM reservations
		WHERE tenant_id = $1::uuid AND state = $2 AND id NOT LIKE '%/%'`
	args := []any{tenantID, state, limit + 1}
	if cursor != "" {
		afterCreatedAt, afterID, err := decodeReservationCursor(cursor)
		if err != nil {
			return nil, "", err
		}
		query += ` AND (created_at, id) < ($4, $5)`
		args = append(args, afterCreatedAt, afterID)
	}
	query += ` ORDER BY created_at DESC, id DESC LIMIT $3`

	rows, err := r.db.QueryContext(ctx, query, args...)
	if err != nil {
		return nil, "", fmt.Errorf("list reservations: %w", err)
	}
	defer rows.Close()
	var out []ReportReservationRow
	for rows.Next() {
		var row ReportReservationRow
		if err := rows.Scan(&row.ID, &row.SubjectType, &row.SubjectID,
			&row.Capability, &row.Model, &row.EndpointID, &row.BoundMicros,
			&row.Currency, &row.ReservedUnits, &row.Dispatched, &row.CreatedAt); err != nil {
			return nil, "", fmt.Errorf("scan reservation report: %w", err)
		}
		out = append(out, row)
	}
	if err := rows.Err(); err != nil {
		return nil, "", err
	}
	var next string
	if len(out) > limit {
		out = out[:limit]
		last := out[len(out)-1]
		next = encodeReservationCursor(last.CreatedAt, last.ID)
	}
	return out, next, nil
}

// ErrInvalidCursor marks a malformed reservation-list keyset cursor; the
// handler maps it to a 400, never ignores it.
var ErrInvalidCursor = errors.New("postgres: invalid reservation cursor")

// encodeReservationCursor makes the opaque keyset cursor: the exact
// (created_at, id) pair of the last returned row, base64-wrapped.
func encodeReservationCursor(createdAt time.Time, id string) string {
	return base64.RawURLEncoding.EncodeToString([]byte(strconv.FormatInt(createdAt.UnixNano(), 10) + "\x00" + id))
}

func decodeReservationCursor(cursor string) (time.Time, string, error) {
	raw, err := base64.RawURLEncoding.DecodeString(cursor)
	if err != nil {
		return time.Time{}, "", fmt.Errorf("%w: %v", ErrInvalidCursor, err)
	}
	parts := strings.SplitN(string(raw), "\x00", 2)
	if len(parts) != 2 || parts[0] == "" || parts[1] == "" {
		return time.Time{}, "", fmt.Errorf("%w: malformed payload", ErrInvalidCursor)
	}
	nanos, err := strconv.ParseInt(parts[0], 10, 64)
	if err != nil {
		return time.Time{}, "", fmt.Errorf("%w: %v", ErrInvalidCursor, err)
	}
	return time.Unix(0, nanos).UTC(), parts[1], nil
}

func scanReportBudgets(rows *sql.Rows) ([]ReportBudgetRow, error) {
	defer rows.Close()
	var out []ReportBudgetRow
	for rows.Next() {
		row, err := scanReportBudgetRow(rows)
		if err != nil {
			return nil, err
		}
		out = append(out, row)
	}
	return out, rows.Err()
}

func scanReportBudgetRow(row rowScanner) (ReportBudgetRow, error) {
	var out ReportBudgetRow
	var endsAt sql.NullTime
	err := row.Scan(&out.Budget.ID, &out.Budget.Name, &out.Budget.Subject.Type,
		&out.Budget.Subject.ID, &out.Budget.Kind, &out.Budget.Period,
		&out.Budget.LimitAmountMicros, &out.Budget.Currency, &out.Budget.Timezone,
		&out.Budget.StartsAt, &endsAt, &out.Active)
	if err != nil {
		return ReportBudgetRow{}, fmt.Errorf("scan budget report: %w", err)
	}
	if endsAt.Valid {
		e := endsAt.Time
		out.Budget.EndsAt = &e
	}
	return out, nil
}
