package httpx

import (
	"context"
	"errors"
	"log"
	"math"
	"net/http"
	"time"

	"enclava-go/internal/auth"
	"enclava-go/internal/billing"
)

// microsPerCent converts billing currency-micros to display cents:
// billing.Price/UsageRecord store int64 micros of a currency unit.
const microsPerCent int64 = 10_000

func (a *App) EnforceBudget(ctx context.Context, principal auth.Principal) error {
	if principal.IsUnlimited {
		return nil
	}
	if a.billingRepo == nil || principal.TenantID == "" {
		return nil
	}

	now := time.Now().UTC()
	subject := billingSubjectOf(principal)
	subjects := []billing.Subject{
		subject,
		{Type: billing.BudgetSubjectTenant, ID: principal.TenantID},
	}
	// Per-key budgets bind the credential itself — a key owned by a user
	// bills the user subject but enforces its own api_key budgets too.
	if principal.CredentialKind == auth.CredentialKindAPIKey && principal.CredentialID != "" {
		subjects = append(subjects, billing.Subject{Type: billing.BudgetSubjectAPIKey, ID: principal.CredentialID})
	}

	// Retained legacy principal limits (key-row budget columns) evaluate the
	// same billing usage aggregate the stored budgets do.
	if principal.BudgetLimitTokens != nil || principal.BudgetLimitCents != nil {
		usedUnits, usedMicros, err := a.usedForPeriod(ctx, principal.TenantID, subject, principal.BudgetPeriod, nil, now)
		if err != nil {
			return err
		}
		if principal.BudgetLimitTokens != nil && usedUnits >= *principal.BudgetLimitTokens {
			return errBudgetExceeded("token budget exceeded")
		}
		if principal.BudgetLimitCents != nil && usedMicros >= centsToMicros(*principal.BudgetLimitCents) {
			return errBudgetExceeded("cost budget exceeded")
		}
	}

	budgets, err := a.billingRepo.ListActiveBudgets(ctx, principal.TenantID, subjects, now)
	if err != nil {
		return err
	}
	for _, budget := range budgets {
		usedUnits, usedMicros, err := a.liabilityForBudget(ctx, principal.TenantID, budget, now)
		if err != nil {
			return err
		}
		switch budget.Kind {
		case billing.BudgetKindTokens:
			if usedUnits >= budget.LimitAmountMicros {
				return errBudgetExceeded("token budget exceeded for " + budget.Name)
			}
		default:
			if usedMicros >= budget.LimitAmountMicros {
				return errBudgetExceeded("cost budget exceeded for " + budget.Name)
			}
		}
	}
	return nil
}

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
		micros += row.AmountMicros
		for _, u := range row.Units {
			units += u
		}
	}
	return units, micros, nil
}

// usedForPeriod reads settled usage aggregates for a subject over the period
// window — the display path (dashboard key rows) and legacy principal limits.
func (a *App) usedForPeriod(ctx context.Context, tenantID string, subject billing.Subject, period string, startsAt *time.Time, now time.Time) (int64, int64, error) {
	since := periodStart(period, now.UTC())
	if startsAt != nil && !startsAt.IsZero() && startsAt.UTC().After(since) {
		since = startsAt.UTC()
	}
	return a.billingRepo.SubjectUsageSince(ctx, tenantID, subject, since)
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

func centsToMicros(cents int64) int64 {
	if cents > math.MaxInt64/microsPerCent {
		return math.MaxInt64
	}
	if cents < math.MinInt64/microsPerCent {
		return math.MinInt64
	}
	return cents * microsPerCent
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
