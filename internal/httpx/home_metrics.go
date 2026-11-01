// home_metrics.go — the Home KPI payload (OA-31): real 30-day requests and
// per-currency spend, the per-day UTC series behind the sparkline (totals
// alone cannot reconstruct it), authorized document/conversation counts and
// the org-budget progress scalar. Absent producers yield zero/empty — never
// fabricated values.
package httpx

import (
	"context"
	"log"
	"sort"
	"time"

	"enclava-go/internal/auth"
	"enclava-go/internal/billing"
	"enclava-go/internal/store/postgres"
)

// MoneyAmount is one currency's exact spend — integer micros, never summed
// across currencies (D-02/CONTRACTS).
type MoneyAmount struct {
	Currency     string `json:"currency"`
	AmountMicros int64  `json:"amount_micros"`
}

// BudgetProgress is the admission-matching scalar shared with the budgets
// API (08-07): threshold bars and attention divide Counted by LimitValue.
type BudgetProgress = budgetProgressDTO

// HomeMetrics is the KPI payload the Home page renders.
type HomeMetrics struct {
	Requests30d       int64
	Spend30d          []MoneyAmount
	Unsettled30d      []MoneyAmount
	Series30d         []postgres.DailyUsageRow
	DocumentTotal     int64
	ConversationTotal int64
	OrgBudget         *BudgetProgress
}

// homeMetricsWindow is the rendered KPI window.
const homeMetricsWindow = 30 * 24 * time.Hour

// dashboardHomeMetrics composes the KPIs. Spend/series/budget are gated to
// billing.view; the document/conversation counts are per-caller authorized
// reads that every role gets for its own visible set.
func (a *App) dashboardHomeMetrics(ctx context.Context, p auth.Principal, caps dashboardCaps) (HomeMetrics, error) {
	m := HomeMetrics{Spend30d: []MoneyAmount{}, Series30d: []postgres.DailyUsageRow{}}

	if a.ragSvc != nil {
		n, err := a.ragSvc.CountDocuments(ctx, p)
		if err != nil {
			log.Printf("dashboard home: count documents: %v", err)
		} else {
			m.DocumentTotal = n
		}
	}
	if a.chatbotSvc != nil {
		n, err := a.chatbotSvc.CountConversations(ctx, p)
		if err != nil {
			log.Printf("dashboard home: count conversations: %v", err)
		} else {
			m.ConversationTotal = n
		}
	}

	if !caps.BillingView || a.billingRepo == nil {
		return m, nil
	}
	now := time.Now().UTC()
	since := now.Add(-homeMetricsWindow)

	totals, err := a.billingRepo.UsageTotals(ctx, p.TenantID, since)
	if err != nil {
		log.Printf("dashboard home: usage totals: %v", err)
	} else {
		for _, t := range totals {
			m.Requests30d += t.RequestCount
			m.Spend30d = append(m.Spend30d, MoneyAmount{Currency: t.Currency, AmountMicros: t.AmountMicros})
		}
	}
	// Outstanding liability over the same window: held reservations always
	// count, unknown count when created inside it — canonical rows only so
	// api-key attribution never double-counts.
	unsettled, err := a.billingRepo.ReportUnsettledBySubject(ctx, p.TenantID, since)
	if err != nil {
		log.Printf("dashboard home: unsettled liability: %v", err)
	} else {
		byCur := map[string]int64{}
		order := []string{}
		for _, row := range unsettled {
			if row.Attributed {
				continue
			}
			if _, seen := byCur[row.Currency]; !seen {
				order = append(order, row.Currency)
			}
			byCur[row.Currency] += row.HeldMicros + row.UnknownMicros
		}
		sort.Strings(order)
		for _, cur := range order {
			m.Unsettled30d = append(m.Unsettled30d, MoneyAmount{Currency: cur, AmountMicros: byCur[cur]})
		}
	}

	series, err := a.billingRepo.ListDailyUsage(ctx, p.TenantID, since)
	if err != nil {
		log.Printf("dashboard home: daily usage: %v", err)
	} else {
		m.Series30d = series
	}

	// The first active tenant-subject budget's admission-matching progress —
	// the same projection the budgets GET renders (deterministic repo order).
	budgets, err := a.billingRepo.ListAllActiveBudgets(ctx, p.TenantID, now)
	if err != nil {
		log.Printf("dashboard home: budgets: %v", err)
	} else {
		for _, b := range budgets {
			if b.Subject.Type != billing.BudgetSubjectTenant {
				continue
			}
			progress, _, err := a.budgetProgress(ctx, p.TenantID, b, now)
			if err != nil {
				log.Printf("dashboard home: org budget progress: %v", err)
				break
			}
			m.OrgBudget = &progress
			break
		}
	}
	return m, nil
}
