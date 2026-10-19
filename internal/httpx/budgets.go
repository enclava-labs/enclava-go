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
