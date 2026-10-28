package httpx

import (
	"context"
	"errors"
	"log"
	"math"
	"net/http"
	"sort"
	"strings"
	"time"

	"enclava-go/internal/auth"
	"enclava-go/internal/billing"
	"enclava-go/internal/store/postgres"
)

// microsPerCent converts billing currency-micros to display cents:
// billing.Price/UsageRecord store int64 micros of a currency unit.
const microsPerCent int64 = 10_000

// billingSubjectOf resolves the principal's billing subject the same way the
// billing service does at reserve time.
func billingSubjectOf(p auth.Principal) billing.Subject {
	if p.BillingSubjectKind != "" && p.BillingSubjectID != "" {
		return billing.Subject{Type: p.BillingSubjectKind, ID: p.BillingSubjectID}
	}
	return billing.Subject{Type: billing.BudgetSubjectTenant, ID: p.TenantID}
}

// liabilityForBudget returns (units, amountMicros) of held+settled liability
// inside the budget's liability window.
func (a *App) liabilityForBudget(ctx context.Context, tenantID string, budget billing.Budget, now time.Time) (int64, int64, error) {
	from := periodStart(budget.Period, now)
	if budget.StartsAt.After(from) {
		from = budget.StartsAt
	}
	rows, err := a.billingRepo.Liabilities(ctx, tenantID, budget.Subject, from)
	if err != nil {
		return 0, 0, err
	}
	var units, micros int64
	for _, row := range rows {
		var ok bool
		micros, ok = checkedBudgetAdd(micros, row.AmountMicros)
		if !ok {
			return math.MaxInt64, math.MaxInt64, nil
		}
		for _, u := range row.Units {
			units, ok = checkedBudgetAdd(units, u)
			if !ok {
				return math.MaxInt64, math.MaxInt64, nil
			}
		}
	}
	return units, micros, nil
}

func checkedBudgetAdd(a, b int64) (int64, bool) {
	if b > 0 && a > math.MaxInt64-b {
		return 0, false
	}
	if b < 0 && a < math.MinInt64-b {
		return 0, false
	}
	return a + b, true
}

// periodStart mirrors billing.periodStart for the read path.
func periodStart(period string, now time.Time) time.Time {
	switch period {
	case billing.PeriodDaily:
		return time.Date(now.Year(), now.Month(), now.Day(), 0, 0, 0, 0, time.UTC)
	case billing.PeriodWeekly:
		d := time.Date(now.Year(), now.Month(), now.Day(), 0, 0, 0, 0, time.UTC)
		return d.AddDate(0, 0, -int((int(now.Weekday())+6)%7))
	case billing.PeriodMonthly:
		return time.Date(now.Year(), now.Month(), 1, 0, 0, 0, 0, time.UTC)
	default:
		return time.Time{}
	}
}

func microsToCents(micro int64) int64 {
	if micro <= 0 {
		return 0
	}
	return (micro + microsPerCent/2) / microsPerCent
}

type budgetExceededError struct {
	msg string
}

func (e budgetExceededError) Error() string { return e.msg }

func errBudgetExceeded(msg string) error {
	return budgetExceededError{msg: msg}
}

func (a *App) WriteBudgetError(w http.ResponseWriter, err error) {
	var be budgetExceededError
	if errors.As(err, &be) {
		WriteOpenAIError(w, http.StatusTooManyRequests, be.Error(), "rate_limit_error", "budget_exceeded")
		return
	}
	log.Printf("budget check failed: %v", err)
	WriteOpenAIError(w, http.StatusInternalServerError, "internal server error", "server_error", "budget_check_failed")
}

// --- admin budget write API (OA-29, plan 08-07) -----------------------------

// budgetDTO is the wire shape of one budget row.
type budgetDTO struct {
	ID          string  `json:"id"`
	Name        string  `json:"name"`
	SubjectType string  `json:"subject_type"`
	SubjectID   string  `json:"subject_id"`
	Kind        string  `json:"kind"`
	LimitValue  int64   `json:"limit_value"`
	Currency    string  `json:"currency,omitempty"`
	Period      string  `json:"period"`
	StartsAt    string  `json:"starts_at"`
	EndsAt      *string `json:"ends_at,omitempty"`
}

// budgetLiabilityRow is one per-currency liability line with per-state
// splits — all integer micros and units, never merged across currencies.
type budgetLiabilityRow struct {
	Currency      string `json:"currency"`
	HeldMicros    int64  `json:"held_micros"`
	UnknownMicros int64  `json:"unknown_micros"`
	SettledMicros int64  `json:"settled_micros"`
	HeldUnits     int64  `json:"held_units"`
	UnknownUnits  int64  `json:"unknown_units"`
	SettledUnits  int64  `json:"settled_units"`
}

// budgetProgressDTO is the admission-matching scalar: threshold bars and the
// attention aggregator divide Counted by LimitValue.
type budgetProgressDTO struct {
	Kind       string `json:"kind"`
	Counted    int64  `json:"counted"`
	LimitValue int64  `json:"limit_value"`
	Currency   string `json:"currency"`
}

// budgetView is one GET/PATCH response element.
type budgetView struct {
	Budget    budgetDTO            `json:"budget"`
	Liability []budgetLiabilityRow `json:"liability"`
	Progress  budgetProgressDTO    `json:"progress"`
}

func newBudgetDTO(b billing.Budget) budgetDTO {
	dto := budgetDTO{
		ID:          b.ID,
		Name:        b.Name,
		SubjectType: b.Subject.Type,
		SubjectID:   b.Subject.ID,
		Kind:        b.Kind,
		LimitValue:  b.LimitAmountMicros,
		Currency:    b.Currency,
		Period:      b.Period,
		StartsAt:    b.StartsAt.UTC().Format(time.RFC3339),
	}
	if b.EndsAt != nil {
		e := b.EndsAt.UTC().Format(time.RFC3339)
		dto.EndsAt = &e
	}
	return dto
}

// currencyLiability accumulates one currency's per-state sums.
type currencyLiability struct {
	held, unknown, settled                int64
	heldUnits, unknownUnits, settledUnits int64
}

// budgetProgress is the state-separated liability projection mirroring
// billing.admit() exactly: the same liability window (period start vs the
// budget's starts_at, whichever is later), held/unknown/settled counted
// separately per currency. For amount budgets only rows in the budget's own
// currency count toward progress — other currencies are returned as
// informational liability rows, never in the denominator; tokens budgets
// count per-kind units across every row. Overflow clamps at MaxInt64 the
// way admit's checked sums refuse past it.
func (a *App) budgetProgress(ctx context.Context, tenantID string, budget billing.Budget, now time.Time) (budgetProgressDTO, []budgetLiabilityRow, error) {
	from := periodStart(budget.Period, now)
	if budget.StartsAt.After(from) {
		from = budget.StartsAt
	}
	rows, err := a.billingRepo.Liabilities(ctx, tenantID, budget.Subject, from)
	if err != nil {
		return budgetProgressDTO{}, nil, err
	}
	byCur := map[string]*currencyLiability{}
	var counted int64
	for _, row := range rows {
		c := byCur[row.Currency]
		if c == nil {
			c = &currencyLiability{}
			byCur[row.Currency] = c
		}
		var micros, units *int64
		switch row.State {
		case billing.ReservationHeld:
			micros, units = &c.held, &c.heldUnits
		case billing.ReservationUnknown:
			micros, units = &c.unknown, &c.unknownUnits
		default:
			micros, units = &c.settled, &c.settledUnits
		}
		var ok bool
		if *micros, ok = checkedBudgetAdd(*micros, row.AmountMicros); !ok {
			return budgetProgressDTO{Kind: budget.Kind, Counted: math.MaxInt64, LimitValue: budget.LimitAmountMicros, Currency: progressCurrency(budget)}, liabilityRows(byCur), nil
		}
		for _, u := range row.Units {
			if *units, ok = checkedBudgetAdd(*units, u); !ok {
				return budgetProgressDTO{Kind: budget.Kind, Counted: math.MaxInt64, LimitValue: budget.LimitAmountMicros, Currency: progressCurrency(budget)}, liabilityRows(byCur), nil
			}
		}
		switch budget.Kind {
		case billing.BudgetKindAmount:
			if row.Currency == budget.Currency {
				if counted, ok = checkedBudgetAdd(counted, row.AmountMicros); !ok {
					counted = math.MaxInt64
				}
			}
		case billing.BudgetKindTokens:
			for _, u := range row.Units {
				if counted, ok = checkedBudgetAdd(counted, u); !ok {
					counted = math.MaxInt64
				}
			}
		}
	}
	progress := budgetProgressDTO{
		Kind:       budget.Kind,
		Counted:    counted,
		LimitValue: budget.LimitAmountMicros,
		Currency:   progressCurrency(budget),
	}
	return progress, liabilityRows(byCur), nil
}

// progressCurrency is the denominator's currency: the bound currency for
// amount budgets, none for tokens.
func progressCurrency(b billing.Budget) string {
	if b.Kind == billing.BudgetKindAmount {
		return b.Currency
	}
	return ""
}

func liabilityRows(byCur map[string]*currencyLiability) []budgetLiabilityRow {
	currencies := make([]string, 0, len(byCur))
	for cur := range byCur {
		currencies = append(currencies, cur)
	}
	sort.Strings(currencies)
	out := make([]budgetLiabilityRow, 0, len(currencies))
	for _, cur := range currencies {
		c := byCur[cur]
		out = append(out, budgetLiabilityRow{
			Currency: cur, HeldMicros: c.held, UnknownMicros: c.unknown,
			SettledMicros: c.settled, HeldUnits: c.heldUnits,
			UnknownUnits: c.unknownUnits, SettledUnits: c.settledUnits,
		})
	}
	return out
}

// --- handlers ----------------------------------------------------------------

var budgetSubjectTypes = map[string]bool{
	billing.BudgetSubjectTenant:         true,
	billing.BudgetSubjectAPIKey:         true,
	billing.BudgetSubjectUser:           true,
	billing.BudgetSubjectServiceAccount: true,
}

// adminBudgetCreateRequest is the POST body. Dates are RFC3339; ends_at is
// optional and must be after starts_at.
type adminBudgetCreateRequest struct {
	Name        string  `json:"name"`
	SubjectType string  `json:"subject_type"`
	SubjectID   string  `json:"subject_id"`
	Kind        string  `json:"kind"`
	LimitValue  *int64  `json:"limit_value"`
	Currency    string  `json:"currency"`
	Period      string  `json:"period"`
	StartsAt    string  `json:"starts_at"`
	EndsAt      *string `json:"ends_at"`
}

type adminBudgetPatchRequest struct {
	LimitValue *int64 `json:"limit_value"`
}

func (a *App) handleAdminBudgets(w http.ResponseWriter, r *http.Request, principal auth.Principal) {
	setNoStoreHeaders(w)
	switch r.Method {
	case http.MethodPost:
		a.handleAdminBudgetCreate(w, r, principal)
	case http.MethodGet:
		a.handleAdminBudgetList(w, r, principal)
	default:
		WriteMethodNotAllowed(w)
	}
}

func (a *App) handleAdminBudgetCreate(w http.ResponseWriter, r *http.Request, principal auth.Principal) {
	if a.identity == nil || a.billingRepo == nil {
		WriteOpenAIError(w, http.StatusNotImplemented, "budget management not configured", "server_error", "not_configured")
		return
	}
	var req adminBudgetCreateRequest
	if err := DecodeJSON(r, &req); err != nil {
		WriteOpenAIError(w, http.StatusBadRequest, err.Error(), "invalid_request_error", "invalid_body")
		return
	}
	name := strings.TrimSpace(req.Name)
	if name == "" {
		WriteOpenAIError(w, http.StatusBadRequest, "name is required", "invalid_request_error", "invalid_name")
		return
	}
	if !budgetSubjectTypes[req.SubjectType] {
		WriteOpenAIError(w, http.StatusBadRequest, "subject_type must be tenant, api_key, user or service_account", "invalid_request_error", "invalid_subject_type")
		return
	}
	if req.SubjectType == billing.BudgetSubjectTenant {
		req.SubjectID = principal.TenantID
	}
	if strings.TrimSpace(req.SubjectID) == "" {
		WriteOpenAIError(w, http.StatusBadRequest, "subject_id is required", "invalid_request_error", "invalid_subject_id")
		return
	}
	if req.Kind != billing.BudgetKindAmount && req.Kind != billing.BudgetKindTokens {
		WriteOpenAIError(w, http.StatusBadRequest, "kind must be amount or tokens", "invalid_request_error", "invalid_kind")
		return
	}
	if req.LimitValue == nil || *req.LimitValue <= 0 || *req.LimitValue > math.MaxInt64/2 {
		WriteOpenAIError(w, http.StatusBadRequest, "limit_value must be a positive integer no greater than MaxInt64/2", "invalid_request_error", "invalid_limit_value")
		return
	}
	switch req.Kind {
	case billing.BudgetKindAmount:
		if !validBudgetCurrency(req.Currency) {
			WriteOpenAIError(w, http.StatusBadRequest, "currency must be a 3-letter ISO-4217 alpha-3 code the platform bills in", "invalid_request_error", "invalid_currency")
			return
		}
	case billing.BudgetKindTokens:
		if strings.TrimSpace(req.Currency) != "" {
			WriteOpenAIError(w, http.StatusBadRequest, "token budgets carry no currency", "invalid_request_error", "invalid_currency")
			return
		}
	}
	if req.Period != billing.PeriodDaily && req.Period != billing.PeriodWeekly && req.Period != billing.PeriodMonthly {
		WriteOpenAIError(w, http.StatusBadRequest, "period must be daily, weekly or monthly", "invalid_request_error", "invalid_period")
		return
	}
	startsAt, err := time.Parse(time.RFC3339, req.StartsAt)
	if err != nil {
		WriteOpenAIError(w, http.StatusBadRequest, "starts_at must be an RFC3339 timestamp", "invalid_request_error", "invalid_starts_at")
		return
	}
	startsAt = startsAt.UTC()
	var endsAt *time.Time
	if req.EndsAt != nil && strings.TrimSpace(*req.EndsAt) != "" {
		t, perr := time.Parse(time.RFC3339, *req.EndsAt)
		if perr != nil {
			WriteOpenAIError(w, http.StatusBadRequest, "ends_at must be an RFC3339 timestamp", "invalid_request_error", "invalid_ends_at")
			return
		}
		t = t.UTC()
		if !t.After(startsAt) {
			WriteOpenAIError(w, http.StatusBadRequest, "ends_at must be after starts_at", "invalid_request_error", "invalid_window")
			return
		}
		endsAt = &t
	}
	if err := a.verifyBudgetSubject(r.Context(), principal.TenantID, req.SubjectType, req.SubjectID); err != nil {
		// Unknown, revoked and foreign subjects are one indistinguishable denial.
		WriteOpenAIError(w, http.StatusBadRequest, "budget subject does not exist in this tenant", "invalid_request_error", "invalid_subject")
		return
	}
	budget := billing.Budget{
		ID:                NewID("budget"),
		Name:              name,
		Subject:           billing.Subject{Type: req.SubjectType, ID: req.SubjectID},
		Kind:              req.Kind,
		Period:            req.Period,
		LimitAmountMicros: *req.LimitValue,
		Currency:          req.Currency,
		StartsAt:          startsAt,
		EndsAt:            endsAt,
	}
	if err := a.billingRepo.CreateBudget(r.Context(), principal.TenantID, budget.ID, budget); err != nil {
		log.Printf("failed to create budget: %v", err)
		WriteOpenAIError(w, http.StatusInternalServerError, "internal server error", "server_error", "budget_create_failed")
		return
	}
	a.recordBudgetBillingAudit(r.Context(), principal, "budget.created", budget, map[string]any{
		"limit_value": budget.LimitAmountMicros,
		"period":      budget.Period,
		"currency":    budget.Currency,
	})
	view, err := a.budgetView(r.Context(), principal.TenantID, budget, time.Now().UTC())
	if err != nil {
		log.Printf("failed to project budget liability: %v", err)
		WriteOpenAIError(w, http.StatusInternalServerError, "internal server error", "server_error", "budget_liability_failed")
		return
	}
	WriteJSON(w, http.StatusCreated, view)
}

func (a *App) handleAdminBudgetList(w http.ResponseWriter, r *http.Request, principal auth.Principal) {
	if a.billingRepo == nil {
		WriteOpenAIError(w, http.StatusNotImplemented, "budget management not configured", "server_error", "not_configured")
		return
	}
	now := time.Now().UTC()
	budgets, err := a.billingRepo.ListAllActiveBudgets(r.Context(), principal.TenantID, now)
	if err != nil {
		log.Printf("failed to list budgets: %v", err)
		WriteOpenAIError(w, http.StatusInternalServerError, "internal server error", "server_error", "budgets_list_failed")
		return
	}
	views := make([]budgetView, 0, len(budgets))
	for _, b := range budgets {
		view, err := a.budgetView(r.Context(), principal.TenantID, b, now)
		if err != nil {
			log.Printf("failed to project budget liability: %v", err)
			WriteOpenAIError(w, http.StatusInternalServerError, "internal server error", "server_error", "budget_liability_failed")
			return
		}
		views = append(views, view)
	}
	WriteJSON(w, http.StatusOK, map[string]any{"budgets": views})
}

func (a *App) handleAdminBudgetByID(w http.ResponseWriter, r *http.Request, principal auth.Principal) {
	setNoStoreHeaders(w)
	if r.Method != http.MethodPatch {
		WriteMethodNotAllowed(w)
		return
	}
	if a.billingRepo == nil {
		WriteOpenAIError(w, http.StatusNotImplemented, "budget management not configured", "server_error", "not_configured")
		return
	}
	id := strings.TrimPrefix(r.URL.Path, "/api/v1/admin/budgets/")
	if strings.TrimSpace(id) == "" {
		WriteOpenAIError(w, http.StatusBadRequest, "budget id is required", "invalid_request_error", "missing_budget_id")
		return
	}
	var req adminBudgetPatchRequest
	if err := DecodeJSON(r, &req); err != nil {
		WriteOpenAIError(w, http.StatusBadRequest, err.Error(), "invalid_request_error", "invalid_body")
		return
	}
	if req.LimitValue == nil {
		WriteOpenAIError(w, http.StatusBadRequest, "limit_value is required", "invalid_request_error", "invalid_limit_value")
		return
	}
	if *req.LimitValue <= 0 || *req.LimitValue > math.MaxInt64/2 {
		WriteOpenAIError(w, http.StatusBadRequest, "limit_value must be a positive integer no greater than MaxInt64/2", "invalid_request_error", "invalid_limit_value")
		return
	}
	budget, err := a.billingRepo.GetBudget(r.Context(), principal.TenantID, id)
	if err != nil {
		// Unknown and cross-tenant ids are one indistinguishable not-found.
		WriteOpenAIError(w, http.StatusNotFound, "budget not found", "invalid_request_error", "budget_not_found")
		return
	}
	if err := a.billingRepo.UpdateBudgetLimit(r.Context(), principal.TenantID, id, *req.LimitValue); err != nil {
		if errors.Is(err, postgres.ErrBudgetNotFound) {
			WriteOpenAIError(w, http.StatusNotFound, "budget not found", "invalid_request_error", "budget_not_found")
			return
		}
		log.Printf("failed to update budget limit: %v", err)
		WriteOpenAIError(w, http.StatusInternalServerError, "internal server error", "server_error", "budget_update_failed")
		return
	}
	budget.LimitAmountMicros = *req.LimitValue
	a.recordBudgetBillingAudit(r.Context(), principal, "budget.limit_updated", budget, map[string]any{
		"limit_value": budget.LimitAmountMicros,
	})
	view, err := a.budgetView(r.Context(), principal.TenantID, budget, time.Now().UTC())
	if err != nil {
		log.Printf("failed to project budget liability: %v", err)
		WriteOpenAIError(w, http.StatusInternalServerError, "internal server error", "server_error", "budget_liability_failed")
		return
	}
	WriteJSON(w, http.StatusOK, view)
}

// budgetView joins one budget row to its admission-matching projection.
func (a *App) budgetView(ctx context.Context, tenantID string, b billing.Budget, now time.Time) (budgetView, error) {
	progress, liability, err := a.budgetProgress(ctx, tenantID, b, now)
	if err != nil {
		return budgetView{}, err
	}
	return budgetView{Budget: newBudgetDTO(b), Liability: liability, Progress: progress}, nil
}

// verifyBudgetSubject refuses subjects that do not exist inside the caller's
// tenant: the key row must exist unrevoked, users/service accounts must
// exist; tenant subjects bind the caller's own tenant only.
func (a *App) verifyBudgetSubject(ctx context.Context, tenantID, subjectType, subjectID string) error {
	switch subjectType {
	case billing.BudgetSubjectTenant:
		if subjectID != tenantID {
			return errors.New("tenant budget binds the caller's own tenant")
		}
		return nil
	case billing.BudgetSubjectAPIKey:
		k, err := a.identity.GetAPIKey(ctx, tenantID, subjectID)
		if err != nil || k.TenantID != tenantID || k.RevokedAt != nil {
			return errors.New("api key subject not found in tenant")
		}
		return nil
	case billing.BudgetSubjectUser:
		if _, err := a.identity.GetUser(ctx, tenantID, subjectID); err != nil {
			return errors.New("user subject not found in tenant")
		}
		return nil
	case billing.BudgetSubjectServiceAccount:
		if _, err := a.identity.GetServiceAccount(ctx, tenantID, subjectID); err != nil {
			return errors.New("service account subject not found in tenant")
		}
		return nil
	default:
		return errors.New("unknown subject type")
	}
}

// validBudgetCurrency accepts 3-letter ISO-4217 alpha-3 codes — the exact
// shape billing prices and usage rows carry (CHAR(3), uppercase A-Z).
func validBudgetCurrency(code string) bool {
	if len(code) != 3 {
		return false
	}
	for _, r := range code {
		if r < 'A' || r > 'Z' {
			return false
		}
	}
	return true
}

// recordBudgetBillingAudit writes the billing-audit row for one budget
// write, mirroring recordAPIKeyBillingAudit's failure tolerance.
func (a *App) recordBudgetBillingAudit(ctx context.Context, actor auth.Principal, action string, b billing.Budget, metadata map[string]any) {
	if a.billingRepo == nil || actor.TenantID == "" {
		return
	}
	if metadata == nil {
		metadata = map[string]any{}
	}
	if err := a.billingRepo.CreateBillingAudit(ctx, actor.TenantID, postgres.BillingAuditRow{
		ID:          NewID("billaudit"),
		SubjectType: b.Subject.Type,
		SubjectID:   b.Subject.ID,
		Action:      action,
		ActorType:   string(actor.ActorKind),
		ActorID:     actor.ActorID,
		Detail:      metadata,
	}); err != nil {
		log.Printf("failed to write billing audit event %s for budget %s: %v", action, b.ID, err)
	}
}
