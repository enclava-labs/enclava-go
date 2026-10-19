// Package billing holds the canonical pricing, budget, reservation and usage
// contract types. Money is int64 micros of a currency unit — this package
// contains no float64. Pure types only.
package billing

import (
	"errors"
	"time"
)

// UnitKind is a billable unit the price model covers. Named extension consts
// may be added only for units a configured endpoint actually advertises;
// never a free-text kind.
type UnitKind string

const (
	UnitInput       UnitKind = "input"
	UnitOutput      UnitKind = "output"
	UnitCachedInput UnitKind = "cached_input"
)

// Price subject types.
const (
	PriceSubjectEndpoint = "endpoint"
	PriceSubjectModel    = "model"
)

// Price is an exact rational rate: RateMicros currency-micros per
// UnitQuantity of Unit — sub-micro-per-token prices are representable.
type Price struct {
	SubjectType   string // endpoint|model
	SubjectID     string
	Capability    string
	Unit          UnitKind
	RateMicros    int64
	UnitQuantity  int64
	Currency      string
	EffectiveFrom time.Time
}

// SnapshotRate is one persisted rate row inside a PriceSnapshot.
type SnapshotRate struct {
	Unit         UnitKind `json:"unit"`
	RateMicros   int64    `json:"rate_micros"`
	UnitQuantity int64    `json:"unit_quantity"`
}

// Rounding rules. Ceil is the documented rule for charge.
const (
	RoundingCeil  = "ceil"
	RoundingFloor = "floor"
)

// PriceSnapshot is persisted at reservation time as
// {rates:[{unit,rate_micros,unit_quantity}],currency,rounding_rule} JSON and
// used verbatim at settle. EVERY selected unit rate is captured — no implicit
// free units and no FX: a mixed-currency set is rejected with
// ErrCurrencyMismatch before a snapshot exists.
type PriceSnapshot struct {
	Rates        []SnapshotRate `json:"rates"`
	Currency     string         `json:"currency"`
	RoundingRule string         `json:"rounding_rule"`
}

// Budget subjects.
const (
	BudgetSubjectTenant         = "tenant"
	BudgetSubjectAPIKey         = "api_key"
	BudgetSubjectUser           = "user"
	BudgetSubjectServiceAccount = "service_account"
)

// Budget kinds and periods.
const (
	BudgetKindAmount = "amount"
	BudgetKindTokens = "tokens"

	PeriodDaily   = "daily"
	PeriodWeekly  = "weekly"
	PeriodMonthly = "monthly"
)

// Subject identifies the entity a budget binds.
type Subject struct {
	Type string // BudgetSubject* const set
	ID   string
}

// Budget is an enforceable limit. Amount budgets bind a currency and reject
// incompatible reservations.
type Budget struct {
	Subject           Subject
	Kind              string // BudgetKind* const set
	Period            string // Period* const set
	LimitAmountMicros int64
	Currency          string
}

// Reservation states. Unknown upstream outcomes retain their hold.
const (
	ReservationHeld     = "held"
	ReservationSettled  = "settled"
	ReservationUnknown  = "unknown"
	ReservationReleased = "released"
)

// Reservation is an atomic pre-attempt hold. LimitAmountMicros is the checked
// ceiling bound over all units; reserved per-kind unit quantities are kept in
// Units because token liability cannot be reconstructed from money.
type Reservation struct {
	TenantID          string
	ID                string
	Subject           Subject
	State             string // Reservation* const set
	Price             PriceSnapshot
	LimitAmountMicros int64
	Units             map[UnitKind]int64
	CreatedAt         time.Time
	SettledAt         *time.Time
}

// Usage is raw reported per-kind usage — never clipped. CachedInputUnits is a
// subset of input settled disjointly, never double-counted. AmountMicros is
// Σ per-kind (rate_micros × units ÷ unit_quantity) under the snapshot's
// RoundingRule, computed with checked wide arithmetic; overflow →
// ErrArithmeticOverflow.
type Usage struct {
	InputUnits       int64
	OutputUnits      int64
	CachedInputUnits int64
	OtherUnits       map[UnitKind]int64 // other advertised bounded units
	AmountMicros     int64
}

var (
	ErrBudgetExceeded     = errors.New("billing: budget exceeded")
	ErrPriceMissing       = errors.New("billing: required price missing")
	ErrCurrencyMismatch   = errors.New("billing: mixed-currency price set")
	ErrArithmeticOverflow = errors.New("billing: arithmetic overflow")
)
