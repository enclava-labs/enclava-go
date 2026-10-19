package httpx

import (
	"context"
	"errors"
	"log"
	"math"
	"net/http"
	"time"

	"enclava-go/internal/auth"
)

const microcentsPerCent int64 = 1_000_000

func (a *App) EnforceBudget(ctx context.Context, principal auth.Principal) error {
	if principal.IsUnlimited {
		return nil
	}
	if a.usageRepo == nil {
		return nil
	}

	now := time.Now().UTC()
	if principal.BudgetLimitTokens != nil || principal.BudgetLimitCents != nil {
		usedTokens, usedMicrocents, err := a.usedForPeriod(ctx, principal.APIKeyID, principal.BudgetPeriod, nil, now)
		if err != nil {
			return err
		}
		if principal.BudgetLimitTokens != nil && usedTokens >= *principal.BudgetLimitTokens {
			return errBudgetExceeded("token budget exceeded")
		}
		if principal.BudgetLimitCents != nil && usedMicrocents >= centsToMicrocents(*principal.BudgetLimitCents) {
			return errBudgetExceeded("cost budget exceeded")
		}
	}

	if a.budgetsRepo == nil {
		return nil
	}
	budgets, err := a.budgetsRepo.ListActiveForAPIKey(ctx, principal.APIKeyID, now)
	if err != nil {
		return err
	}
	for _, budget := range budgets {
		usedTokens, usedMicrocents, err := a.usedForPeriod(ctx, principal.APIKeyID, budget.Period, budget.StartsAt, now)
		if err != nil {
			return err
		}
		if budget.LimitTokens != nil && usedTokens >= *budget.LimitTokens {
			return errBudgetExceeded("token budget exceeded for " + budget.Name)
		}
		if budget.LimitCents != nil && usedMicrocents >= centsToMicrocents(*budget.LimitCents) {
			return errBudgetExceeded("cost budget exceeded for " + budget.Name)
		}
	}
	return nil
}

func (a *App) usedForPeriod(ctx context.Context, apiKeyID int64, period string, startsAt *time.Time, now time.Time) (int64, int64, error) {
	now = now.UTC()
	since, lifetime := periodWindowStart(period, now)
	if startsAt != nil && !startsAt.IsZero() {
		start := startsAt.UTC()
		if lifetime || start.After(since) {
			since = start
		}
		lifetime = false
	}
	if lifetime {
		return a.usageRepo.KeyTotals(ctx, apiKeyID)
	}
	return a.usageRepo.SumSince(ctx, apiKeyID, since)
}

func periodWindowStart(period string, now time.Time) (time.Time, bool) {
	switch period {
	case "monthly":
		return time.Date(now.Year(), now.Month(), 1, 0, 0, 0, 0, time.UTC), false
	case "daily":
		return time.Date(now.Year(), now.Month(), now.Day(), 0, 0, 0, 0, time.UTC), false
	default:
		return time.Time{}, true
	}
}

func centsToMicrocents(cents int64) int64 {
	if cents > math.MaxInt64/microcentsPerCent {
		return math.MaxInt64
	}
	if cents < math.MinInt64/microcentsPerCent {
		return math.MinInt64
	}
	return cents * microcentsPerCent
}

func microcentsToCents(micro int64) int64 {
	if micro <= 0 {
		return 0
	}
	return (micro + microcentsPerCent/2) / microcentsPerCent
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
