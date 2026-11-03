// service.go is the exact-billing admission and settlement authority
// (JOB-03/JOB-04). Reserve locks every applicable budget FOR UPDATE inside one
// transaction, snapshots the immutable rational rate set, enforces
// existing-liability + proposed-bound <= limit, and inserts the held
// reservation before commit — no DB transaction ever spans upstream work.
// Settle charges once from the snapshot without clipping; MarkUnknown retains
// liability; ReleaseUndispatched frees only never-dispatched holds.
package billing

import (
	"context"
	"crypto/rand"
	"encoding/hex"
	"fmt"
	"math"
	"math/bits"
	"sort"
	"time"

	"enclava-go/internal/auth"
)

// Service is the billing admission/settlement authority.
type Service struct {
	repo BillingRepository
	now  func() time.Time
}

// NewService wraps the liability authority in billing policy.
func NewService(repo BillingRepository) *Service {
	return &Service{repo: repo, now: func() time.Time { return time.Now().UTC() }}
}

// RecordRequestRefused records a final admission refusal after price fallback.
func (s *Service) RecordRequestRefused(ctx context.Context, p auth.Principal, model, capability, reason string) error {
	subject := billingSubjectFor(p)
	if p.CredentialKind == auth.CredentialKindAPIKey && p.CredentialID != "" {
		subject = Subject{Type: BudgetSubjectAPIKey, ID: p.CredentialID}
	} else if p.UserID != "" {
		subject = Subject{Type: BudgetSubjectUser, ID: p.UserID}
	}
	return s.repo.RecordRequestRefused(ctx, p.TenantID, subject, string(p.ActorKind), p.ActorID, model, capability, reason)
}

// PriceKey selects the billing_prices row set for a priced attempt: one
// subject (endpoint or model) plus the operation capability, resolved at
// reservation time.
type PriceKey struct {
	SubjectType string // PriceSubjectEndpoint|PriceSubjectModel
	SubjectID   string
}

// ReserveRequest is one priced-attempt admission. SubjectKeys are every
// subject whose budgets must admit the bound (tenant, billing subject and any
// ancestor subject the caller enforces). BillingSubject receives the charge;
// when empty the principal's billing subject (or tenant) is used. Estimate is
// the bounded per-kind upper bound; CachedInputUnits is disjoint from
// InputUnits (never billed twice).
type ReserveRequest struct {
	SubjectKeys    []Subject
	BillingSubject Subject
	Capability     string
	Model          string
	EndpointID     string
	JobID          string
	AttemptID      string
	Estimate       Usage
	Price          PriceKey
}

// Reserve atomically admits one priced attempt and returns the held
// reservation. It refuses when any billed unit kind lacks a rate
// (ErrPriceMissing), when selected rates mix currencies (ErrCurrencyMismatch),
// when an amount budget binds a different currency, or when the proposed bound
// would overflow any applicable limit (ErrBudgetExceeded).
func (s *Service) Reserve(ctx context.Context, p auth.Principal, req ReserveRequest) (Reservation, error) {
	if req.Capability == "" || req.Price.SubjectType == "" || req.Price.SubjectID == "" {
		return Reservation{}, fmt.Errorf("billing: reserve requires capability and a price key")
	}
	estimate, err := unitMap(req.Estimate)
	if err != nil {
		return Reservation{}, err
	}
	if len(estimate) == 0 {
		return Reservation{}, fmt.Errorf("billing: reserve requires a non-empty unit estimate")
	}

	now := s.now()
	billingSubject := req.BillingSubject
	if billingSubject.Type == "" {
		billingSubject = billingSubjectFor(p)
	}
	subjects := subjectsFor(p.TenantID, billingSubject, req.SubjectKeys)

	var out Reservation
	err = s.repo.InTx(ctx, func(tx BillingTx) error {
		// Deterministic lock order: every concurrent Reserve acquires the
		// same budget rows in the same order.
		budgets, err := tx.LockActiveBudgets(ctx, p.TenantID, subjects, now)
		if err != nil {
			return err
		}

		prices, err := tx.SelectPrices(ctx, p.TenantID, req.Price.SubjectType, req.Price.SubjectID, req.Capability, now)
		if err != nil {
			return err
		}
		snapshot, err := snapshotFor(prices, estimate)
		if err != nil {
			return err
		}

		// Prompt tokens are held once: any input token may come back
		// cached, so the bound prices max(input, cached) units at the
		// higher prompt rate. The merged unit map is what token budgets
		// and the persisted reserved_units count.
		boundUnits, boundSnap := boundEstimate(estimate, snapshot)
		bound, err := charge(boundUnits, boundSnap)
		if err != nil {
			return err
		}
		// Both prompt kinds declared: settlement ceils cached and
		// uncached input separately while the fold rounds once, so the
		// hold needs the <1-micro second-term headroom.
		if estimate[UnitInput] > 0 && estimate[UnitCachedInput] > 0 {
			var overflow bool
			bound, overflow = checkedAdd(bound, 1)
			if overflow {
				return ErrArithmeticOverflow
			}
		}

		for _, b := range budgets {
			from := LiabilityFrom(b, now)
			rows, err := tx.Liabilities(ctx, p.TenantID, b.Subject, from)
			if err != nil {
				return err
			}
			if err := admit(b, rows, bound, boundUnits, snapshot.Currency); err != nil {
				return err
			}
		}

		out = Reservation{
			TenantID:          p.TenantID,
			ID:                newID("res"),
			State:             ReservationHeld,
			Price:             snapshot,
			LimitAmountMicros: bound,
			Currency:          snapshot.Currency,
			Units:             boundUnits,
			Capability:        req.Capability,
			Model:             req.Model,
			EndpointID:        req.EndpointID,
			JobID:             req.JobID,
			AttemptID:         req.AttemptID,
			CredentialID:      credentialIDFor(p),
			CreatedAt:         now,
		}
		rows := make([]Reservation, 0, len(subjects))
		for _, subj := range subjects {
			row := out
			row.Subject = subj
			if subj != billingSubject {
				// Attribution row: deterministic derived id, same liability data.
				row.ID = out.ID + "/" + subj.Type + ":" + subj.ID
			}
			rows = append(rows, row)
		}
		return tx.InsertReservations(ctx, rows)
	})
	if err != nil {
		return Reservation{}, err
	}
	out.Subject = billingSubject
	return out, nil
}

// Settle charges the actual reported usage once — raw units are stored
// unclipped and the amount is computed under the reservation's immutable
// snapshot, so a later price change or an overrun above the bound is recorded
// truthfully. Held and unknown (late-arriving upstream outcome) reservations
// both settle; a kind with no snapshot rate is recorded but unpriced
// (Outcome "unpriced_units") rather than failing the settlement. A repeated
// settle of the same reservation returns the existing evidence.
func (s *Service) Settle(ctx context.Context, p auth.Principal, reservationID string, actual Usage) (UsageRecord, error) {
	units, err := unitMap(actual)
	if err != nil {
		return UsageRecord{}, err
	}
	var out UsageRecord
	err = s.repo.InTx(ctx, func(tx BillingTx) error {
		res, err := tx.GetReservationForUpdate(ctx, p.TenantID, reservationID)
		if err != nil {
			return err
		}
		if res.State == ReservationSettled {
			// Once-only: return the existing evidence.
			out, err = tx.GetUsageRecord(ctx, p.TenantID, reservationID)
			return err
		}
		if res.State != ReservationHeld && res.State != ReservationUnknown {
			return fmt.Errorf("%w: %s is %s", ErrReservationState, reservationID, res.State)
		}
		amount, missing, err := chargeLenient(units, res.Price)
		if err != nil {
			return err
		}
		outcome := OutcomeCompleted
		if len(missing) > 0 {
			outcome = OutcomeUnpricedUnits
		}
		n, err := tx.Transition(ctx, p.TenantID, reservationID, res.State, ReservationSettled)
		if err != nil {
			return err
		}
		if n == 0 {
			return fmt.Errorf("%w: %s", ErrReservationNotFound, reservationID)
		}
		rec := UsageRecord{
			TenantID:      p.TenantID,
			ID:            newID("use"),
			ReservationID: reservationID,
			Subject:       res.Subject,
			Capability:    res.Capability,
			Model:         res.Model,
			EndpointID:    res.EndpointID,
			Units:         units,
			AmountMicros:  amount,
			Currency:      res.Currency,
			Price:         res.Price,
			Outcome:       outcome,
			CredentialID:  res.CredentialID,
			CreatedAt:     s.now(),
		}
		if err := tx.InsertUsageRecord(ctx, rec); err != nil {
			return err
		}
		out = rec
		return nil
	})
	return out, err
}

// MarkUnknown moves a held reservation to unknown: liability is retained,
// never auto-refunded or replayed.
func (s *Service) MarkUnknown(ctx context.Context, p auth.Principal, reservationID string) error {
	return s.transition(ctx, p.TenantID, reservationID, ReservationHeld, ReservationUnknown)
}

// MarkDispatched records that the reservation's upstream attempt was
// dispatched; ReleaseUndispatched refuses it afterwards. The reservation
// must still be held — a worker resuming after a hold sweep must not send
// provider bytes against a released liability.
func (s *Service) MarkDispatched(ctx context.Context, p auth.Principal, reservationID string) error {
	return s.repo.InTx(ctx, func(tx BillingTx) error {
		res, err := tx.GetReservationForUpdate(ctx, p.TenantID, reservationID)
		if err != nil {
			return err
		}
		// A second MarkDispatched on a held row is a no-op success.
		if res.State != ReservationHeld {
			return fmt.Errorf("%w: %s is %s", ErrReservationState, reservationID, res.State)
		}
		_, err = tx.MarkDispatched(ctx, p.TenantID, reservationID)
		return err
	})
}

// ReleaseUndispatched frees a reservation only when its attempt was never
// dispatched — a dispatched hold is settled or marked unknown instead.
func (s *Service) ReleaseUndispatched(ctx context.Context, p auth.Principal, reservationID string) error {
	return s.repo.InTx(ctx, func(tx BillingTx) error {
		res, err := tx.GetReservationForUpdate(ctx, p.TenantID, reservationID)
		if err != nil {
			return err
		}
		if res.State == ReservationReleased {
			return nil
		}
		if res.State != ReservationHeld || res.DispatchedAt != nil {
			return fmt.Errorf("%w: %s is %s/dispatched=%v",
				ErrReservationState, reservationID, res.State, res.DispatchedAt != nil)
		}
		_, err = tx.Transition(ctx, p.TenantID, reservationID, ReservationHeld, ReservationReleased)
		return err
	})
}

// ReleaseUnsent frees a held reservation even after dispatch was marked: the
// caller guarantees no request bytes reached the provider (connection or
// write failure before send). Already released is a no-op; settled or
// unknown reservations refuse.
func (s *Service) ReleaseUnsent(ctx context.Context, p auth.Principal, reservationID string) error {
	return s.repo.InTx(ctx, func(tx BillingTx) error {
		res, err := tx.GetReservationForUpdate(ctx, p.TenantID, reservationID)
		if err != nil {
			return err
		}
		if res.State == ReservationReleased {
			return nil
		}
		if res.State != ReservationHeld {
			return fmt.Errorf("%w: %s is %s", ErrReservationState, reservationID, res.State)
		}
		_, err = tx.Transition(ctx, p.TenantID, reservationID, ReservationHeld, ReservationReleased)
		return err
	})
}

// ResolveOutstanding settles a held or unknown reservation with reconciled
// usage; zero usage writes it off. Settled returns its existing record.
func (s *Service) ResolveOutstanding(ctx context.Context, p auth.Principal, reservationID string, actual Usage) (UsageRecord, error) {
	units, err := unitMap(actual)
	if err != nil {
		return UsageRecord{}, err
	}
	var out UsageRecord
	err = s.repo.InTx(ctx, func(tx BillingTx) error {
		res, err := tx.GetReservationForUpdate(ctx, p.TenantID, reservationID)
		if err != nil {
			return err
		}
		if res.State == ReservationSettled {
			out, err = tx.GetUsageRecord(ctx, p.TenantID, reservationID)
			return err
		}
		if res.State != ReservationUnknown && res.State != ReservationHeld {
			return fmt.Errorf("%w: %s is %s", ErrReservationState, reservationID, res.State)
		}
		amount, _, err := chargeLenient(units, res.Price)
		if err != nil {
			return err
		}
		if _, err := tx.Transition(ctx, p.TenantID, reservationID, res.State, ReservationSettled); err != nil {
			return err
		}
		rec := UsageRecord{
			TenantID:      p.TenantID,
			ID:            newID("use"),
			ReservationID: reservationID,
			Subject:       res.Subject,
			Capability:    res.Capability,
			Model:         res.Model,
			EndpointID:    res.EndpointID,
			Units:         units,
			AmountMicros:  amount,
			Currency:      res.Currency,
			Price:         res.Price,
			Outcome:       OutcomeReconciled,
			CredentialID:  res.CredentialID,
			CreatedAt:     s.now(),
		}
		if err := tx.InsertUsageRecord(ctx, rec); err != nil {
			return err
		}
		out = rec
		return nil
	})
	return out, err
}

// SweepStaleHolds is cross-tenant maintenance: a stale hold that was never
// dispatched is released (no bytes could have been sent), while one marked
// dispatched becomes unknown (the upstream outcome is unrecoverable but
// liability is retained).
func (s *Service) SweepStaleHolds(ctx context.Context, olderThan time.Time) (released, unknown int, err error) {
	return s.repo.SweepStaleHolds(ctx, olderThan)
}

// GetReservation exposes the canonical reservation row for tests/dashboards.
func (s *Service) GetReservation(ctx context.Context, p auth.Principal, id string) (Reservation, error) {
	return s.repo.GetReservation(ctx, p.TenantID, id)
}

func (s *Service) transition(ctx context.Context, tenantID, id, from, to string) error {
	return s.repo.InTx(ctx, func(tx BillingTx) error {
		n, err := tx.Transition(ctx, tenantID, id, from, to)
		if err != nil {
			return err
		}
		if n == 0 {
			res, err := tx.GetReservationForUpdate(ctx, tenantID, id)
			if err != nil {
				return err
			}
			return fmt.Errorf("%w: %s is %s", ErrReservationState, id, res.State)
		}
		return nil
	})
}

// billingSubjectFor derives the charged subject from the principal's billing
// context, defaulting to the tenant itself.
func billingSubjectFor(p auth.Principal) Subject {
	if p.BillingSubjectKind != "" && p.BillingSubjectID != "" {
		return Subject{Type: p.BillingSubjectKind, ID: p.BillingSubjectID}
	}
	return Subject{Type: BudgetSubjectTenant, ID: p.TenantID}
}

// credentialIDFor is the verified api-key credential attribution: only an
// api-key credential carries its id; sessions and other kinds write ”.
func credentialIDFor(p auth.Principal) string {
	if p.CredentialKind == auth.CredentialKindAPIKey {
		return p.CredentialID
	}
	return ""
}

// LatestPrices exposes the read-side effective-rate query so display and
// routing checks can evaluate price coverage without reserving.
func (s *Service) LatestPrices(ctx context.Context, tenantID, subjectType, subjectID, capability string, now time.Time) ([]Price, error) {
	return s.repo.LatestPrices(ctx, tenantID, subjectType, subjectID, capability, now)
}

// subjectsFor returns the deduplicated, deterministically ordered subject set:
// the billing subject plus every explicitly requested key.
func subjectsFor(tenantID string, billing Subject, keys []Subject) []Subject {
	seen := map[Subject]struct{}{}
	var out []Subject
	for _, s := range append([]Subject{billing, {Type: BudgetSubjectTenant, ID: tenantID}}, keys...) {
		if s.Type == "" {
			continue
		}
		if s.Type == BudgetSubjectTenant {
			s.ID = tenantID
		}
		if s.ID == "" {
			continue
		}
		if _, ok := seen[s]; ok {
			continue
		}
		seen[s] = struct{}{}
		out = append(out, s)
	}
	sort.Slice(out, func(i, j int) bool {
		if out[i].Type != out[j].Type {
			return out[i].Type < out[j].Type
		}
		return out[i].ID < out[j].ID
	})
	return out
}

// unitMap flattens Usage into a per-kind map. Only kinds with non-zero units
// appear. Negative units are rejected.
func unitMap(u Usage) (map[UnitKind]int64, error) {
	out := map[UnitKind]int64{}
	add := func(k UnitKind, v int64) error {
		if v < 0 {
			return fmt.Errorf("billing: negative units for %s", k)
		}
		if v > 0 {
			out[k] = v
		}
		return nil
	}
	if err := add(UnitInput, u.InputUnits); err != nil {
		return nil, err
	}
	if err := add(UnitOutput, u.OutputUnits); err != nil {
		return nil, err
	}
	if err := add(UnitCachedInput, u.CachedInputUnits); err != nil {
		return nil, err
	}
	for k, v := range u.OtherUnits {
		if k == "" {
			return nil, fmt.Errorf("billing: empty unit kind")
		}
		if err := add(k, v); err != nil {
			return nil, err
		}
	}
	return out, nil
}

// snapshotFor resolves the rate set for every unit kind in the estimate. A
// kind without a selected rate refuses dispatch (ErrPriceMissing) — no unit is
// ever silently free. All rates must share one currency.
func snapshotFor(prices []Price, estimate map[UnitKind]int64) (PriceSnapshot, error) {
	byKind := map[UnitKind]Price{}
	for _, p := range prices {
		if _, ok := byKind[p.Unit]; !ok {
			byKind[p.Unit] = p
		}
	}
	kinds := make([]string, 0, len(estimate))
	for k := range estimate {
		kinds = append(kinds, string(k))
	}
	sort.Strings(kinds)
	snap := PriceSnapshot{RoundingRule: RoundingCeil}
	for _, name := range kinds {
		k := UnitKind(name)
		p, ok := byKind[k]
		if !ok {
			return PriceSnapshot{}, fmt.Errorf("%w: %s", ErrPriceMissing, k)
		}
		// Currency consistency applies to the SELECTED rate set — an unused
		// unit kind's currency under the same price key is irrelevant.
		if snap.Currency == "" {
			snap.Currency = p.Currency
		} else if snap.Currency != p.Currency {
			return PriceSnapshot{}, fmt.Errorf("%w: %s vs %s", ErrCurrencyMismatch, snap.Currency, p.Currency)
		}
		snap.Rates = append(snap.Rates, SnapshotRate{
			Unit:         k,
			RateMicros:   p.RateMicros,
			UnitQuantity: p.UnitQuantity,
		})
	}
	if snap.Currency == "" {
		return PriceSnapshot{}, fmt.Errorf("%w: no rates selected", ErrPriceMissing)
	}
	return snap, nil
}

// boundEstimate merges the prompt kinds: any input token may return cached,
// so the hold prices max(input, cached) prompt units ONCE under the higher
// prompt rate. It returns the merged unit map (what token budgets and
// reserved_units count) and the rate set to charge it with; when the
// surviving rate is cached_input the returned snapshot renames it to input
// so charge prices it (the reservation keeps its own stored snapshot).
func boundEstimate(estimate map[UnitKind]int64, snap PriceSnapshot) (map[UnitKind]int64, PriceSnapshot) {
	in, cached := estimate[UnitInput], estimate[UnitCachedInput]
	if in == 0 && cached == 0 {
		return estimate, snap
	}
	rateOf := map[UnitKind]SnapshotRate{}
	for i := range snap.Rates {
		rateOf[snap.Rates[i].Unit] = snap.Rates[i]
	}
	ir, hasIn := rateOf[UnitInput]
	cr, hasCr := rateOf[UnitCachedInput]
	var promptRate SnapshotRate
	switch {
	case hasIn && hasCr:
		promptRate = ir
		if rateGreater(cr, ir) {
			promptRate = cr
		}
	case hasIn:
		promptRate = ir
	case hasCr:
		promptRate = cr
	default:
		return estimate, snap // no prompt rate: charge() reports it missing
	}
	out := make(map[UnitKind]int64, len(estimate)+1)
	for k, v := range estimate {
		if k != UnitInput && k != UnitCachedInput {
			out[k] = v
		}
	}
	prompt := in
	if cached > prompt {
		prompt = cached
	}
	out[UnitInput] = prompt
	promptRate.Unit = UnitInput
	snap.Rates = append(append([]SnapshotRate{}, snap.Rates...), promptRate)
	return out, snap
}

// rateGreater reports whether rational rate a exceeds b via exact
// cross-multiplication in 128 bits.
func rateGreater(a, b SnapshotRate) bool {
	if a.UnitQuantity <= 0 || b.UnitQuantity <= 0 {
		return false
	}
	ahi, alo := bits.Mul64(uint64(a.RateMicros), uint64(b.UnitQuantity)) //nolint:gosec // operands non-negative; uint64 needed for 128-bit product
	bhi, blo := bits.Mul64(uint64(b.RateMicros), uint64(a.UnitQuantity)) //nolint:gosec // operands non-negative; uint64 needed for 128-bit product
	return ahi > bhi || (ahi == bhi && alo > blo)
}

// charge computes Σ per-kind rate_micros×units ÷ unit_quantity under the
// snapshot's rounding rule with checked 128-bit arithmetic. Overrun is never
// clipped — units larger than the reserved bound produce a larger amount.
// Every unit kind must have a snapshot rate.
func charge(units map[UnitKind]int64, snap PriceSnapshot) (int64, error) {
	total, missing, err := chargeLenient(units, snap)
	if err != nil {
		return 0, err
	}
	if len(missing) > 0 {
		return 0, fmt.Errorf("%w: %s", ErrPriceMissing, missing[0])
	}
	return total, nil
}

// chargeLenient is charge that records kinds missing from the snapshot
// instead of failing: their units are stored but contribute nothing to the
// amount — settlement must never strand usage for a pricing gap.
func chargeLenient(units map[UnitKind]int64, snap PriceSnapshot) (int64, []UnitKind, error) {
	rateOf := map[UnitKind]SnapshotRate{}
	for _, r := range snap.Rates {
		rateOf[r.Unit] = r
	}
	kinds := make([]string, 0, len(units))
	for k := range units {
		kinds = append(kinds, string(k))
	}
	sort.Strings(kinds)
	total := new(big128)
	var missing []UnitKind
	for _, name := range kinds {
		k := UnitKind(name)
		r, ok := rateOf[k]
		if !ok {
			missing = append(missing, k)
			continue
		}
		if r.UnitQuantity <= 0 || r.RateMicros < 0 {
			return 0, nil, fmt.Errorf("billing: invalid snapshot rate for %s", k)
		}
		total.addRatMul(r.RateMicros, units[k], r.UnitQuantity, snap.RoundingRule)
		if total.overflow {
			return 0, nil, ErrArithmeticOverflow
		}
	}
	return total.i64(), missing, nil
}

// LiabilityFrom is the LATER of the budget's period window start and its own
// starts_at — settled usage before it does not count.
func LiabilityFrom(b Budget, now time.Time) time.Time {
	start := periodStart(b.Period, b.Timezone, now)
	if b.StartsAt.After(start) {
		start = b.StartsAt
	}
	return start
}

// periodStart computes the current day/ISO-week/month boundary in the
// budget's IANA timezone; "" or an invalid name falls back to UTC.
func periodStart(period, timezone string, now time.Time) time.Time {
	loc := time.UTC
	if timezone != "" {
		if l, err := time.LoadLocation(timezone); err == nil {
			loc = l
		}
	}
	localNow := now.In(loc)
	switch period {
	case PeriodDaily:
		return localDateStart(loc, localNow.Year(), localNow.Month(), localNow.Day())
	case PeriodWeekly:
		// ISO week starts Monday.
		monday := localNow.AddDate(0, 0, -(int(localNow.Weekday())+6)%7)
		return localDateStart(loc, monday.Year(), monday.Month(), monday.Day())
	case PeriodMonthly:
		return localDateStart(loc, localNow.Year(), localNow.Month(), 1)
	default:
		return time.Time{}
	}
}

// localDateStart returns the first instant whose local calendar date is
// y-m-d in loc. time.Date normalizes a skipped midnight onto the previous
// (or later) day, so the result is stepped forward from the normalized
// instant until the target date is reached — DST gaps are shorter than a
// day and a boundary must never open in the previous local day. Ordinary
// days return their local midnight unchanged.
func localDateStart(loc *time.Location, y int, m time.Month, d int) time.Time {
	target := time.Date(y, m, d, 0, 0, 0, 0, time.UTC)
	for t := time.Date(y, m, d, 0, 0, 0, 0, loc); ; t = t.Add(time.Minute) {
		c := t.In(loc)
		if !time.Date(c.Year(), c.Month(), c.Day(), 0, 0, 0, 0, time.UTC).Before(target) {
			return t
		}
	}
}

// admit enforces one budget: existing liability plus the proposed bound must
// fit the limit — checked addition, no over-admission. Amount budgets bind a
// currency and reject a differently-priced reservation outright.
func admit(b Budget, rows []LiabilityRow, bound int64, units map[UnitKind]int64, currency string) error {
	switch b.Kind {
	case BudgetKindAmount:
		if currency != b.Currency {
			return fmt.Errorf("%w: %w: reservation priced in %s vs %s budget", ErrBudgetCurrencyMismatch, ErrCurrencyMismatch, currency, b.Currency)
		}
		existing := int64(0)
		for _, r := range rows {
			if r.Currency != b.Currency {
				continue
			}
			var overflow bool
			existing, overflow = checkedAdd(existing, r.AmountMicros)
			if overflow {
				return fmt.Errorf("%w: liability overflows %s budget", ErrBudgetExceeded, b.ID)
			}
		}
		if _, overflow := checkedAdd(existing, bound); overflow {
			return fmt.Errorf("%w: %s budget %s", ErrBudgetExceeded, b.Kind, b.ID)
		}
		if existing+bound > b.LimitAmountMicros {
			return fmt.Errorf("%w: %s budget %s has %d remaining, proposed %d",
				ErrBudgetExceeded, b.Kind, b.ID, b.LimitAmountMicros-existing, bound)
		}
	case BudgetKindTokens:
		var existing int64
		for _, r := range rows {
			for _, v := range r.Units {
				var overflow bool
				existing, overflow = checkedAdd(existing, v)
				if overflow {
					return fmt.Errorf("%w: liability overflows %s budget", ErrBudgetExceeded, b.ID)
				}
			}
		}
		proposed := int64(0)
		for _, v := range units {
			var overflow bool
			proposed, overflow = checkedAdd(proposed, v)
			if overflow {
				return fmt.Errorf("%w: proposed units overflow %s budget", ErrBudgetExceeded, b.ID)
			}
		}
		if _, overflow := checkedAdd(existing, proposed); overflow {
			return fmt.Errorf("%w: %s budget %s", ErrBudgetExceeded, b.Kind, b.ID)
		}
		if existing+proposed > b.LimitAmountMicros {
			return fmt.Errorf("%w: %s budget %s has %d units remaining, proposed %d",
				ErrBudgetExceeded, b.Kind, b.ID, b.LimitAmountMicros-existing, proposed)
		}
	default:
		return fmt.Errorf("billing: unknown budget kind %q", b.Kind)
	}
	return nil
}

func checkedAdd(a, b int64) (int64, bool) {
	sum, carry := bits.Add64(uint64(a), uint64(b), 0) //nolint:gosec // deliberate bit reinterpretation; sign checked below
	// Overflow when the signed result wrapped: operands share sign, result differs.
	overflow := (a >= 0 && b >= 0 && int64(sum) < 0) || (a < 0 && b < 0 && int64(sum) >= 0) //nolint:gosec // deliberate bit reinterpretation for wrap detection
	_ = carry
	return int64(sum), overflow //nolint:gosec // deliberate bit reinterpretation for wrap detection
}

// big128 accumulates exact numerators of charge terms (rate_micros×units each
// divided by unit_quantity), then applies one rounding step per term. It uses
// math/big-free fixed 128-bit arithmetic via bits.Mul64/Add64.
type big128 struct {
	hi       uint64
	lo       uint64
	overflow bool
}

// addRatMul adds rat = a*b/q rounded per rule (ceil for charge) into the
// accumulator; all operands are non-negative int64s.
func (t *big128) addRatMul(a, b, q int64, rule string) {
	if b == 0 || a == 0 {
		return
	}
	// Exact numerator a*b as 128-bit.
	hi, lo := bits.Mul64(uint64(a), uint64(b)) //nolint:gosec // operands non-negative; uint64 needed for 128-bit product
	// Divide 128-bit numerator by q (q>0), keeping the remainder for rounding.
	quo, rem := div128(hi, lo, uint64(q)) //nolint:gosec // q > 0 guaranteed by callers; uint64 for 128-bit division
	if rule == RoundingCeil && rem > 0 {
		var carry uint64
		lo2, carry := bits.Add64(quo.lo, 1, 0)
		hi2 := quo.hi + carry
		quo.hi, quo.lo = hi2, lo2
	}
	var carry uint64
	t.lo, carry = bits.Add64(t.lo, quo.lo, 0)
	hi, hiCarry := bits.Add64(t.hi, quo.hi, carry)
	if hiCarry != 0 {
		t.overflow = true
	}
	t.hi = hi
	if t.hi != 0 || t.lo > math.MaxInt64 { // exceeds int64 range
		t.overflow = true
	}
}

type u128 struct{ hi, lo uint64 }

// div128 divides a 128-bit numerator by q via shift-subtract. Bounded inputs
// (rate_micros and units are int64) keep this cheap and exact.
func div128(hi, lo, q uint64) (u128, uint64) {
	// Long division over 128 bits.
	quoHi, quoLo := uint64(0), uint64(0)
	rem := uint64(0)
	for i := 127; i >= 0; i-- {
		rem <<= 1
		bit := uint64(0)
		if i >= 64 {
			bit = (hi >> uint(i-64)) & 1
		} else {
			bit = (lo >> uint(i)) & 1
		}
		rem |= bit
		if rem >= q {
			rem -= q
			if i >= 64 {
				quoHi |= 1 << uint(i-64)
			} else {
				quoLo |= 1 << uint(i)
			}
		}
	}
	return u128{quoHi, quoLo}, rem
}

func (t *big128) i64() int64 {
	if t.hi != 0 || t.lo > math.MaxInt64 {
		return math.MaxInt64
	}
	return int64(t.lo)
}

func newID(prefix string) string {
	var b [16]byte
	if _, err := rand.Read(b[:]); err != nil {
		panic(err)
	}
	return prefix + "_" + hex.EncodeToString(b[:])
}
