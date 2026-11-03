// dashboard_usage.go — Usage & billing (OA-29, source §6.12, plan 09-08).
// Admin-only: every route sits behind billing.view. Three tabs — overview
// (7/30/90-day KPIs + server-rendered daily SVG histogram + spend by
// model/team/key), budgets (all-subject rows with admission-matching
// progress from budgetProgress, create + limit-only edit through
// /api/v1/admin/budgets) and audit (combined billing + pricing events).
// Handlers render only (D-17): writes go through static/pages/usage.js via
// window.Enclava.api. The six legacy billing registrations/handlers and five
// legacy templates were deleted atomically with this file — nothing
// billing-related remains for 09-10 (stats/Home stay until 10-01).
package httpx

import (
	"context"
	"errors"
	"log"
	"net/http"
	"net/url"
	"sort"
	"strconv"
	"strings"
	"time"

	"enclava-go/internal/billing"
	"enclava-go/internal/store/postgres"
)

// usageView is the page model for all three tabs; the page template
// dispatches on ActiveTab. Fragments render the same view so .Can/.Data
// stay uniform with the shell envelope.
type usageView struct {
	ActiveTab    string
	Tabs         []dashboardTab
	Days         int
	Overview     usageOverviewData
	Budgets      []usageBudgetRow
	BudgetsEmpty dashboardEmpty
	Audit        usageAuditData
	Drawer       usageBudgetDrawer
}

func (usageView) DashboardTitle() string  { return "Usage & billing" }
func (usageView) DashboardModule() string { return "usage" }

func usageTabs(active string) []dashboardTab {
	return []dashboardTab{
		{Label: "Overview", Href: "/dashboard/usage", Active: active == "overview"},
		{Label: "Budgets", Href: "/dashboard/usage/budgets", Active: active == "budgets"},
		{Label: "Audit", Href: "/dashboard/usage/audit", Active: active == "audit"},
	}
}

func (a *App) registerDashboardUsageRoutes() {
	a.mux.HandleFunc("/dashboard/usage", a.sessionAuth.requireCap(capBillingView, a.handleDashboardUsagePage))
	a.mux.HandleFunc("/dashboard/usage/budgets", a.sessionAuth.requireCap(capBillingView, a.handleDashboardUsageBudgetsPage))
	a.mux.HandleFunc("/dashboard/usage/audit", a.sessionAuth.requireCap(capBillingView, a.handleDashboardUsageAuditPage))
	a.mux.HandleFunc("/dashboard/partials/usage/overview", a.sessionAuth.requireCap(capBillingView, a.handleDashboardUsageOverviewFragment))
	a.mux.HandleFunc("/dashboard/partials/usage/budgets", a.sessionAuth.requireCap(capBillingView, a.handleDashboardUsageBudgetsFragment))
	a.mux.HandleFunc("/dashboard/partials/usage/audit", a.sessionAuth.requireCap(capBillingView, a.handleDashboardUsageAuditFragment))
	a.mux.HandleFunc("/dashboard/partials/usage/budget-drawer", a.sessionAuth.requireCap(capBillingView, a.handleDashboardUsageBudgetDrawer))
}

func init() { registerDashboardPage((*App).registerDashboardUsageRoutes) }

// usageConfigured refuses to render when the billing read/write services are
// absent — a 501 instead of a page of fabricated zeros.
func (a *App) usageConfigured(w http.ResponseWriter) bool {
	if a.billingRepo == nil || a.identity == nil {
		WriteOpenAIError(w, http.StatusNotImplemented, "usage tracking is not configured", "server_error", "not_configured")
		return false
	}
	return true
}

// --- overview ---------------------------------------------------------------

// usageGroupRow is one spend-table line. Unsettled carries the per-currency
// held+unknown liability for rows whose subject key is known (per-subject
// and per-key tables); model/team rows leave it empty.
type usageGroupRow struct {
	Label, Sub      string
	Requests, Units int64
	Spend           string
	Unsettled       []MoneyAmount
}

type usageOverviewData struct {
	Days          int
	Requests      int64
	Tokens        int64
	Spend         string // primary currency via the shared money formatter
	SpendFoot     string // remaining currencies, "+ €… + £…", "" when single
	Unsettled     string // outstanding liability, primary currency
	UnsettledFoot string // remaining currencies
	Daily         []dashboardBar
	ByModel       []usageGroupRow
	ByTeam        []usageGroupRow
	BySubject     []usageGroupRow
	ByKey         []usageGroupRow
	Empty         dashboardEmpty
}

// usageRangeDays accepts exactly 7/30/90; anything else is the 30 default.
func usageRangeDays(raw string) int {
	switch raw {
	case "7":
		return 7
	case "90":
		return 90
	default:
		return 30
	}
}

func (a *App) handleDashboardUsagePage(w http.ResponseWriter, r *http.Request) {
	if r.Method != http.MethodGet {
		WriteMethodNotAllowed(w)
		return
	}
	setNoStoreHeaders(w)
	tenantID, ok := a.dashboardTenantID(w, r)
	if !ok || !a.usageConfigured(w) {
		return
	}
	days := usageRangeDays(r.URL.Query().Get("range"))
	overview, err := a.usageOverview(r.Context(), tenantID, days)
	if err != nil {
		log.Printf("failed to load usage overview: %v", err)
		WriteOpenAIError(w, http.StatusInternalServerError, "internal server error", "server_error", "usage_overview_failed")
		return
	}
	a.renderDashboard(w, r, "usage", usageView{
		ActiveTab: "overview", Tabs: usageTabs("overview"), Days: days, Overview: overview,
	})
}

func (a *App) handleDashboardUsageOverviewFragment(w http.ResponseWriter, r *http.Request) {
	if r.Method != http.MethodGet {
		WriteMethodNotAllowed(w)
		return
	}
	setNoStoreHeaders(w)
	tenantID, ok := a.dashboardTenantID(w, r)
	if !ok || !a.usageConfigured(w) {
		return
	}
	days := usageRangeDays(r.URL.Query().Get("range"))
	overview, err := a.usageOverview(r.Context(), tenantID, days)
	if err != nil {
		log.Printf("failed to load usage overview fragment: %v", err)
		WriteOpenAIError(w, http.StatusInternalServerError, "internal server error", "server_error", "usage_overview_failed")
		return
	}
	a.renderDashboardFragment(w, r, "usage", "usage-overview", usageView{
		ActiveTab: "overview", Tabs: usageTabs("overview"), Days: days, Overview: overview,
	})
}

// usageOverview reads every named billing projection for the window
// server-side; nothing on the page is invented.
func (a *App) usageOverview(ctx context.Context, tenantID string, days int) (usageOverviewData, error) {
	out := usageOverviewData{Days: days, Empty: dashboardEmpty{Icon: "usage", Text: "No usage in this range."}}
	since := time.Now().UTC().AddDate(0, 0, -days)

	totals, err := a.billingRepo.UsageTotals(ctx, tenantID, since)
	if err != nil {
		return out, err
	}
	primary := -1
	for i, t := range totals {
		out.Requests += t.RequestCount
		out.Tokens += t.TotalUnits
		if primary < 0 || t.AmountMicros > totals[primary].AmountMicros {
			primary = i
		}
	}
	if primary >= 0 {
		out.Spend = money(totals[primary].AmountMicros, totals[primary].Currency)
		var extras []string
		for i, t := range totals {
			if i != primary {
				extras = append(extras, money(t.AmountMicros, t.Currency))
			}
		}
		out.SpendFoot = strings.Join(extras, " + ")
	}

	daily, err := a.billingRepo.ListDailyUsage(ctx, tenantID, since)
	if err != nil {
		return out, err
	}
	byDay := map[string]int64{}
	daysOrder := make([]string, 0, len(daily))
	for _, row := range daily {
		key := row.Day.Format("2006-01-02")
		if _, seen := byDay[key]; !seen {
			daysOrder = append(daysOrder, key)
		}
		byDay[key] += row.RequestCount // currency-independent count per UTC day
	}
	sort.Strings(daysOrder)
	out.Daily = make([]dashboardBar, 0, len(daysOrder))
	for _, day := range daysOrder {
		n := byDay[day]
		noun := "requests"
		if n == 1 {
			noun = "request"
		}
		out.Daily = append(out.Daily, dashboardBar{
			Label: day + " · " + dashboardCountInt(n) + " " + noun,
			Value: float64(n),
		})
	}

	labels := a.usageSubjectLabels(ctx, tenantID)

	modelRows, err := a.billingRepo.UsageByModel(ctx, tenantID, since)
	if err != nil {
		return out, err
	}
	out.ByModel = usageSpendRows(modelRows, nil, nil)

	teamRows, err := a.billingRepo.ListUsageByTeam(ctx, tenantID, since)
	if err != nil {
		return out, err
	}
	out.ByTeam = usageSpendRows(teamRows, nil, nil)

	subjectRows, err := a.billingRepo.UsageBySubject(ctx, tenantID, since)
	if err != nil {
		return out, err
	}
	// Outstanding (held+unknown) liability, same window rule as admission:
	// held always, unknown created inside the window. Canonical rows give
	// per-subject and tenant totals; api_key attribution rows give per-key.
	unsettledRows, err := a.billingRepo.ReportUnsettledBySubject(ctx, tenantID, since)
	if err != nil {
		return out, err
	}
	canonical := unsettledAmounts(unsettledRows, false)
	perKey := unsettledAmounts(unsettledRows, true)
	{
		// Tenant totals per currency — canonical rows only, so key-attributed
		// money never counts twice. Largest currency leads, code order ties.
		totals := map[string]int64{}
		currencies := []string{}
		for _, amounts := range canonical {
			for _, amt := range amounts {
				if _, seen := totals[amt.Currency]; !seen {
					currencies = append(currencies, amt.Currency)
				}
				totals[amt.Currency] += amt.AmountMicros
			}
		}
		sort.Strings(currencies)
		primary := ""
		for _, cur := range currencies {
			if primary == "" || totals[cur] > totals[primary] {
				primary = cur
			}
		}
		var extras []string
		for _, cur := range currencies {
			if cur == primary {
				out.Unsettled = money(totals[cur], cur)
			} else {
				extras = append(extras, money(totals[cur], cur))
			}
		}
		out.UnsettledFoot = strings.Join(extras, " + ")
	}

	out.BySubject = usageSpendRows(mergeUnsettledRows(subjectRows, canonical, func(key string) (st, id string, keep bool) {
		st, id, _ = strings.Cut(key, "\x00")
		return st, id, st != billing.BudgetSubjectAPIKey
	}), func(g postgres.UsageGroupSum) (string, string, string, bool) {
		if g.SubjectType == billing.BudgetSubjectAPIKey {
			return "", "", "", false
		}
		key := g.SubjectType + "\x00" + g.SubjectID
		label := labels[key]
		if label == "" {
			label = g.SubjectID
		}
		return label, term(g.SubjectType), key, true
	}, canonical)

	out.ByKey = usageSpendRows(mergeUnsettledRows(subjectRows, perKey, func(key string) (st, id string, keep bool) {
		_, id, _ = strings.Cut(key, "\x00")
		return billing.BudgetSubjectAPIKey, id, true
	}), func(g postgres.UsageGroupSum) (string, string, string, bool) {
		if g.SubjectType != billing.BudgetSubjectAPIKey {
			return "", "", "", false
		}
		label := labels["api_key\x00"+g.SubjectID]
		if label == "" {
			label = "API key " + g.SubjectID
		}
		return label, g.SubjectID, "api_key\x00" + g.SubjectID, true
	}, perKey)
	return out, nil
}

// unsettledAmounts groups unsettled rows into a subject-keyed per-currency
// lookup; attributed selects the api_key attribution arm, false the
// canonical billing-subject arm.
func unsettledAmounts(rows []postgres.ReportUnsettledRow, attributed bool) map[string][]MoneyAmount {
	out := map[string][]MoneyAmount{}
	for _, row := range rows {
		if row.Attributed != attributed {
			continue
		}
		key := row.SubjectType + "\x00" + row.SubjectID
		out[key] = append(out[key], MoneyAmount{
			Currency:     row.Currency,
			AmountMicros: row.HeldMicros + row.UnknownMicros,
		})
	}
	return out
}

// mergeUnsettledRows appends zero-spend UsageGroupSum rows for unsettled
// (subject, currency) pairs that settled usage never produced — otherwise a
// held-only subject or key carries KPI liability with no table row. subject
// decodes a "type\x00id" key into the row's subject fields, keep=false drops
// keys the table does not show (canonical api_key rows belong to ByKey).
func mergeUnsettledRows(rows []postgres.UsageGroupSum, unsettled map[string][]MoneyAmount, subject func(key string) (st, id string, keep bool)) []postgres.UsageGroupSum {
	have := map[[3]string]bool{}
	for _, g := range rows {
		have[[3]string{g.SubjectType, g.SubjectID, g.Currency}] = true
	}
	out := rows
	for key, amounts := range unsettled {
		st, id, keep := subject(key)
		if !keep {
			continue
		}
		for _, amt := range amounts {
			if amt.AmountMicros <= 0 || have[[3]string{st, id, amt.Currency}] {
				continue
			}
			have[[3]string{st, id, amt.Currency}] = true
			out = append(out, postgres.UsageGroupSum{SubjectType: st, SubjectID: id, Currency: amt.Currency})
		}
	}
	return out
}

// usageSpendRows maps one UsageGroupSum per row — mixed currencies each keep
// their own row, never merged — sorted by request count desc. The label
// callback's key joins the row to its unsettled per-currency liability.
func usageSpendRows(rows []postgres.UsageGroupSum, label func(postgres.UsageGroupSum) (label, sub, key string, keep bool), unsettled map[string][]MoneyAmount) []usageGroupRow {
	out := make([]usageGroupRow, 0, len(rows))
	for _, g := range rows {
		rowLabel, sub, key := g.Key, "", ""
		if label != nil {
			var keep bool
			if rowLabel, sub, key, keep = label(g); !keep {
				continue
			}
		}
		row := usageGroupRow{
			Label: orDash(rowLabel), Sub: sub,
			Requests: g.RequestCount, Units: g.TotalUnits,
			Spend: money(g.AmountMicros, g.Currency),
		}
		if key != "" {
			for _, amt := range unsettled[key] {
				if amt.Currency == g.Currency {
					row.Unsettled = append(row.Unsettled, amt)
				}
			}
		}
		out = append(out, row)
	}
	sort.SliceStable(out, func(i, j int) bool { return out[i].Requests > out[j].Requests })
	return out
}
func orDash(s string) string {
	if strings.TrimSpace(s) == "" {
		return "—"
	}
	return s
}

// usageSubjectLabels resolves subject ids to display names for budget rows
// and the by-key table; a failed lookup degrades to the raw id, never an
// invented name.
func (a *App) usageSubjectLabels(ctx context.Context, tenantID string) map[string]string {
	labels := map[string]string{}
	if keys, err := a.identity.ListAPIKeys(ctx, tenantID); err == nil {
		for _, k := range keys {
			name := strings.TrimSpace(k.Name)
			if name == "" {
				name = "API key " + k.ID
			}
			labels["api_key\x00"+k.ID] = name
		}
	} else {
		log.Printf("usage label lookup (api keys): %v", err)
	}
	if users, err := a.identity.ListUsers(ctx, tenantID); err == nil {
		for _, u := range users {
			name := strings.TrimSpace(u.DisplayName)
			if name == "" {
				name = u.Email
			}
			labels["user\x00"+u.ID] = name
		}
	} else {
		log.Printf("usage label lookup (users): %v", err)
	}
	if sas, err := a.identity.ListServiceAccounts(ctx, tenantID); err == nil {
		for _, s := range sas {
			labels["service_account\x00"+s.ID] = orDash(s.Name)
		}
	} else {
		log.Printf("usage label lookup (service accounts): %v", err)
	}
	labels["tenant\x00"+tenantID] = a.dashboardOrgName(ctx, tenantID)
	return labels
}

// --- budgets ----------------------------------------------------------------

type usageLiabilityRow struct {
	Currency               string
	Held, Unknown, Settled string
}

type usageBudgetRow struct {
	ID, Name         string
	SubjectType      string
	SubjectID        string
	SubjectLabel     string
	Kind, Period     string
	Timezone         string
	Status           string // scheduled|active|ended|inactive
	LimitDisplay     string
	LimitRaw         string // stored integer limit — the tooltip behind LimitDisplay
	PeriodSuffix     string // "/ day" | "/ week" | "/ month" | ""
	CountedDisplay   string
	Unlimited        bool
	Pct              int
	Tone             string // "", "warn" (≥80%), "bad" (≥95%)
	LiabilitySummary string
	StartsAt         time.Time
	EndsAt           *time.Time
}

// budgetPeriodSuffix renders the limit's recurrence in plain words after the
// human amount ("$10.00 / month"); unknown periods carry no suffix.
func budgetPeriodSuffix(period string) string {
	switch period {
	case billing.PeriodDaily:
		return " / day"
	case billing.PeriodWeekly:
		return " / week"
	case billing.PeriodMonthly:
		return " / month"
	}
	return ""
}

// usageBudgetTone maps counted/limit to the threshold tones at exactly the
// §6.12 boundaries: warn at ≥80%, bad at ≥95% (limit ≤ 0 → no bar).
func usageBudgetTone(counted, limit int64) string {
	if limit <= 0 || counted <= 0 {
		return ""
	}
	switch pct := counted * 100 / limit; {
	case pct >= 95:
		return "bad"
	case pct >= 80:
		return "warn"
	}
	return ""
}

// usageBudgetPct is the bar width: the floored ratio clamped to 0..100.
func usageBudgetPct(counted, limit int64) int {
	if limit <= 0 || counted <= 0 {
		return 0
	}
	pct := int(counted * 100 / limit)
	if pct > 100 {
		pct = 100
	}
	return pct
}

func (a *App) handleDashboardUsageBudgetsPage(w http.ResponseWriter, r *http.Request) {
	if r.Method != http.MethodGet {
		WriteMethodNotAllowed(w)
		return
	}
	setNoStoreHeaders(w)
	tenantID, ok := a.dashboardTenantID(w, r)
	if !ok || !a.usageConfigured(w) {
		return
	}
	rows, err := a.usageBudgetRows(r.Context(), tenantID)
	if err != nil {
		log.Printf("failed to load usage budgets: %v", err)
		WriteOpenAIError(w, http.StatusInternalServerError, "internal server error", "server_error", "usage_budgets_failed")
		return
	}
	caps, _ := capsFromContext(r.Context())
	a.renderDashboard(w, r, "usage", usageBudgetsView(tenantID, rows, caps))
}

func (a *App) handleDashboardUsageBudgetsFragment(w http.ResponseWriter, r *http.Request) {
	if r.Method != http.MethodGet {
		WriteMethodNotAllowed(w)
		return
	}
	setNoStoreHeaders(w)
	tenantID, ok := a.dashboardTenantID(w, r)
	if !ok || !a.usageConfigured(w) {
		return
	}
	rows, err := a.usageBudgetRows(r.Context(), tenantID)
	if err != nil {
		log.Printf("failed to load usage budgets fragment: %v", err)
		WriteOpenAIError(w, http.StatusInternalServerError, "internal server error", "server_error", "usage_budgets_failed")
		return
	}
	caps, _ := capsFromContext(r.Context())
	a.renderDashboardFragment(w, r, "usage", "usage-budgets", usageBudgetsView(tenantID, rows, caps))
}

func usageBudgetsView(tenantID string, rows []usageBudgetRow, caps dashboardCaps) usageView {
	view := usageView{ActiveTab: "budgets", Tabs: usageTabs("budgets"), Budgets: rows}
	view.BudgetsEmpty = dashboardEmpty{Icon: "billing", Text: "No budgets yet."}
	if caps.BillingView {
		view.BudgetsEmpty.ActionDrawerLabel = "New budget"
		view.BudgetsEmpty.ActionDrawerURL = "/dashboard/partials/usage/budget-drawer"
	}
	return view
}

// usageBudgetRows renders the same all-subject read as GET /api/v1/admin/
// budgets: progress comes only from the admission-matching budgetProgress
// projection; per-currency liability rows are informational.
func (a *App) usageBudgetRows(ctx context.Context, tenantID string) ([]usageBudgetRow, error) {
	now := time.Now().UTC()
	budgets, err := a.billingRepo.ReportListBudgets(ctx, tenantID)
	if err != nil {
		return nil, err
	}
	labels := a.usageSubjectLabels(ctx, tenantID)
	rows := make([]usageBudgetRow, 0, len(budgets))
	for _, rb := range budgets {
		b := rb.Budget
		progress, liability, err := a.budgetProgress(ctx, tenantID, b, now)
		if err != nil {
			return nil, err
		}
		row := usageBudgetRow{
			ID: b.ID, Name: b.Name,
			SubjectType: b.Subject.Type, SubjectID: b.Subject.ID,
			SubjectLabel: subjectLabel(labels, b.Subject),
			Kind:         b.Kind, Period: b.Period,
			Timezone:     budgetTimezoneName(b.Timezone),
			Status:       budgetStatus(b, rb.Active, now),
			PeriodSuffix: budgetPeriodSuffix(b.Period),
			StartsAt:     b.StartsAt, EndsAt: b.EndsAt,
		}
		if progress.LimitValue <= 0 {
			row.Unlimited = true
		} else {
			row.LimitDisplay = usageValueDisplay(progress.LimitValue, progress.Currency, progress.Kind)
			row.LimitRaw = strconv.FormatInt(progress.LimitValue, 10)
			row.CountedDisplay = usageValueDisplay(progress.Counted, progress.Currency, progress.Kind)
			row.Pct = usageBudgetPct(progress.Counted, progress.LimitValue)
			row.Tone = usageBudgetTone(progress.Counted, progress.LimitValue)
		}
		row.LiabilitySummary = usageLiabilitySummary(liability, b.Kind)
		rows = append(rows, row)
	}
	sort.SliceStable(rows, func(i, j int) bool { return rows[i].Name < rows[j].Name })
	return rows, nil
}

func subjectLabel(labels map[string]string, subject billing.Subject) string {
	if label := labels[subject.Type+"\x00"+subject.ID]; label != "" {
		return label
	}
	return subject.ID
}

// usageValueDisplay renders amount kinds through money() and token kinds as
// grouped counts — the counted scalar's unit always matches the budget kind.
func usageValueDisplay(value int64, currency, kind string) string {
	if kind == billing.BudgetKindTokens {
		return dashboardCountInt(value) + " units"
	}
	return money(value, currency)
}

// usageLiabilitySummary joins the informational per-currency held/settled
// rows ("Held $1.20 · Settled $3.00 USD; Held 10,00 … EUR") — never summed
// across currencies.
func usageLiabilitySummary(rows []budgetLiabilityRow, kind string) string {
	parts := make([]string, 0, len(rows))
	for _, row := range rows {
		if kind == billing.BudgetKindTokens {
			part := "Held " + dashboardCountInt(row.HeldUnits) + " · Settled " + dashboardCountInt(row.SettledUnits)
			if row.UnknownUnits != 0 {
				part += " · Unknown " + dashboardCountInt(row.UnknownUnits)
			}
			parts = append(parts, part+" units")
			continue
		}
		part := "Held " + money(row.HeldMicros, row.Currency) + " · Settled " + money(row.SettledMicros, row.Currency)
		if row.UnknownMicros != 0 {
			part += " · Unknown " + money(row.UnknownMicros, row.Currency)
		}
		parts = append(parts, part)
	}
	return strings.Join(parts, "; ")
}

// --- budget drawer ----------------------------------------------------------

type usageSubjectOption struct {
	ID, Label string
}

type usageBudgetDrawer struct {
	Edit                                 bool
	BudgetID                             string
	Name                                 string
	Kind                                 string
	KindLabel                            string
	PeriodLabel                          string
	LimitValue                           string // amount budgets: decimal currency ("10.00"); tokens: integer units
	LimitRaw                             string
	Currency                             string
	Symbol                               string // the currency's display symbol for the amount field prefix
	Keys                                 []usageSubjectOption
	Users                                []usageSubjectOption
	Services                             []usageSubjectOption
	KeysEmpty, UsersEmpty, ServicesEmpty dashboardEmpty
	OrgName                              string
	Today                                string
}

// microsDecimal renders stored micros as the decimal amount the drawer
// field accepts — exact, so the edit prefill never rounds the stored limit.
func microsDecimal(micros int64) string {
	neg := micros < 0
	if neg {
		micros = -micros
	}
	s := strconv.FormatInt(micros, 10)
	if len(s) <= 6 {
		s = strings.Repeat("0", 6-len(s)+1) + s
	}
	out := s[:len(s)-6] + "." + strings.TrimRight(s[len(s)-6:], "0")
	if strings.HasSuffix(out, ".") {
		out += "00"
	}
	if neg {
		out = "-" + out
	}
	return out
}

// moneySymbol is the display prefix for the amount field — the same set
// money() uses; an unlisted currency falls back to its code.
func moneySymbol(currency string) string {
	if sym, ok := dashboardMoneySymbols[currency]; ok {
		return sym
	}
	return currency
}

func (a *App) handleDashboardUsageBudgetDrawer(w http.ResponseWriter, r *http.Request) {
	if r.Method != http.MethodGet {
		WriteMethodNotAllowed(w)
		return
	}
	setNoStoreHeaders(w)
	tenantID, ok := a.dashboardTenantID(w, r)
	if !ok || !a.usageConfigured(w) {
		return
	}
	drawer, err := a.usageBudgetDrawerData(r.Context(), tenantID, strings.TrimSpace(r.URL.Query().Get("budget")))
	if err != nil {
		// Unknown and cross-tenant edit ids are one designed not-found.
		if errors.Is(err, postgres.ErrBudgetNotFound) {
			http.NotFound(w, r)
			return
		}
		log.Printf("failed to load budget drawer: %v", err)
		WriteOpenAIError(w, http.StatusInternalServerError, "internal server error", "server_error", "budget_drawer_failed")
		return
	}
	a.renderDashboardFragment(w, r, "usage", "usage-budget-drawer", usageView{
		ActiveTab: "budgets", Tabs: usageTabs("budgets"), Drawer: drawer,
	})
}

// usageBudgetDrawerData builds the create form's bounded subject pickers
// (existing tenant entities only — no free-text ids) and, when budgetID is
// set, the limit-only edit form for that budget.
func (a *App) usageBudgetDrawerData(ctx context.Context, tenantID, budgetID string) (usageBudgetDrawer, error) {
	drawer := usageBudgetDrawer{
		OrgName:       a.dashboardOrgName(ctx, tenantID),
		Today:         time.Now().UTC().Format("2006-01-02"),
		KeysEmpty:     dashboardEmpty{Icon: "key", Text: "No API keys available."},
		UsersEmpty:    dashboardEmpty{Icon: "users", Text: "No people available."},
		ServicesEmpty: dashboardEmpty{Icon: "users", Text: "No service accounts available."},
	}
	if keys, err := a.identity.ListAPIKeys(ctx, tenantID); err == nil {
		for _, k := range keys {
			if k.RevokedAt != nil {
				continue
			}
			name := strings.TrimSpace(k.Name)
			if name == "" {
				name = "Unnamed API key"
			}
			drawer.Keys = append(drawer.Keys, usageSubjectOption{ID: k.ID, Label: name})
		}
	} else {
		return drawer, err
	}
	if users, err := a.identity.ListUsers(ctx, tenantID); err == nil {
		for _, u := range users {
			label := strings.TrimSpace(u.DisplayName)
			if label == "" {
				label = u.Email
			}
			drawer.Users = append(drawer.Users, usageSubjectOption{ID: u.ID, Label: label})
		}
	} else {
		return drawer, err
	}
	if sas, err := a.identity.ListServiceAccounts(ctx, tenantID); err == nil {
		for _, s := range sas {
			drawer.Services = append(drawer.Services, usageSubjectOption{ID: s.ID, Label: orDash(s.Name)})
		}
	} else {
		return drawer, err
	}
	if budgetID == "" {
		return drawer, nil
	}
	b, err := a.billingRepo.GetBudget(ctx, tenantID, budgetID)
	if err != nil {
		return drawer, err
	}
	drawer.Edit = true
	drawer.BudgetID = b.ID
	drawer.Name = b.Name
	drawer.Kind = b.Kind
	drawer.KindLabel = budgetKindLabel(b.Kind)
	drawer.PeriodLabel = b.Period
	drawer.LimitRaw = strconv.FormatInt(b.LimitAmountMicros, 10)
	drawer.Currency = b.Currency
	drawer.Symbol = moneySymbol(b.Currency)
	if b.Kind == billing.BudgetKindTokens {
		drawer.LimitValue = strconv.FormatInt(b.LimitAmountMicros, 10)
	} else {
		drawer.LimitValue = microsDecimal(b.LimitAmountMicros)
	}
	return drawer, nil
}

func budgetKindLabel(kind string) string {
	if kind == billing.BudgetKindTokens {
		return "Tokens"
	}
	return "Amount"
}

// --- audit ------------------------------------------------------------------

// auditFetchCap bounds each source read per render; paging pages the merged,
// filtered projection (the repos have no kind/actor/date pushdown).
const auditFetchCap = 500

type usageAuditFilters struct {
	Kind, SubjectType, SubjectID, Actor, From, To string
}

type usageAuditRow struct {
	Time                                 time.Time
	Kind, ActorType, Actor               string
	SubjectType, SubjectID, SubjectLabel string
	Amount                               string
}

type usageKindChip struct {
	Kind   string
	Label  string
	Active bool
	Href   string
}

type usageSelectOption struct {
	Value, Label string
	Active       bool
}

type usageAuditData struct {
	Rows         []usageAuditRow
	Kinds        []usageKindChip
	SubjectTypes []usageSelectOption
	Total        int
	Limit        int
	Offset       int
	PrevOffset   int // -1 = no newer link
	NextOffset   int // -1 = no older link
	PrevHref     string
	NextHref     string
	ResetHref    string
	AllHref      string // kind-cleared href keeping the other filters (the "All" chip)
	Filters      usageAuditFilters
	Empty        dashboardEmpty
}

var usageAuditSubjectTypes = []usageSelectOption{
	{Value: billing.BudgetSubjectTenant, Label: "Tenant"},
	{Value: billing.BudgetSubjectAPIKey, Label: "API key"},
	{Value: billing.BudgetSubjectUser, Label: "User"},
	{Value: billing.BudgetSubjectServiceAccount, Label: "Service account"},
}

func (a *App) handleDashboardUsageAuditPage(w http.ResponseWriter, r *http.Request) {
	if r.Method != http.MethodGet {
		WriteMethodNotAllowed(w)
		return
	}
	setNoStoreHeaders(w)
	tenantID, ok := a.dashboardTenantID(w, r)
	if !ok || !a.usageConfigured(w) {
		return
	}
	filters, limit, offset := usageAuditFiltersFromRequest(r)
	audit, err := a.usageAuditData(r.Context(), tenantID, filters, limit, offset)
	if err != nil {
		log.Printf("failed to load usage audit: %v", err)
		WriteOpenAIError(w, http.StatusInternalServerError, "internal server error", "server_error", "usage_audit_failed")
		return
	}
	a.renderDashboard(w, r, "usage", usageView{ActiveTab: "audit", Tabs: usageTabs("audit"), Audit: audit})
}

func (a *App) handleDashboardUsageAuditFragment(w http.ResponseWriter, r *http.Request) {
	if r.Method != http.MethodGet {
		WriteMethodNotAllowed(w)
		return
	}
	setNoStoreHeaders(w)
	tenantID, ok := a.dashboardTenantID(w, r)
	if !ok || !a.usageConfigured(w) {
		return
	}
	filters, limit, offset := usageAuditFiltersFromRequest(r)
	audit, err := a.usageAuditData(r.Context(), tenantID, filters, limit, offset)
	if err != nil {
		log.Printf("failed to load usage audit fragment: %v", err)
		WriteOpenAIError(w, http.StatusInternalServerError, "internal server error", "server_error", "usage_audit_failed")
		return
	}
	a.renderDashboardFragment(w, r, "usage", "usage-audit", usageView{ActiveTab: "audit", Tabs: usageTabs("audit"), Audit: audit})
}

// usageAuditFiltersFromRequest parses every filter from the query string;
// limit is clamped into a bounded window (DoS guard), offset stays ≥ 0.
func usageAuditFiltersFromRequest(r *http.Request) (usageAuditFilters, int, int) {
	q := r.URL.Query()
	f := usageAuditFilters{
		Kind:        strings.TrimSpace(q.Get("kind")),
		SubjectType: strings.TrimSpace(q.Get("subject_type")),
		SubjectID:   strings.TrimSpace(q.Get("subject_id")),
		Actor:       strings.TrimSpace(q.Get("actor")),
		From:        strings.TrimSpace(q.Get("from")),
		To:          strings.TrimSpace(q.Get("to")),
	}
	limit, offset := paginationFromRequest(r)
	if limit <= 0 || limit > 100 {
		limit = 20
	}
	if offset < 0 {
		offset = 0
	}
	return f, limit, offset
}

func usageAuditHref(f usageAuditFilters, limit, offset int) string {
	q := url.Values{}
	setIf := func(key, value string) {
		if value != "" {
			q.Set(key, value)
		}
	}
	setIf("kind", f.Kind)
	setIf("subject_type", f.SubjectType)
	setIf("subject_id", f.SubjectID)
	setIf("actor", f.Actor)
	setIf("from", f.From)
	setIf("to", f.To)
	q.Set("limit", strconv.Itoa(limit))
	if offset > 0 {
		q.Set("offset", strconv.Itoa(offset))
	}
	return "/dashboard/usage/audit?" + q.Encode()
}

// usageAuditData merges billing_audit and billing_price_audit rows into one
// desc-sorted projection (09-CONTEXT: the audit read covers billing AND
// pricing), applies the real filters and pages the result with limit/offset.
func (a *App) usageAuditData(ctx context.Context, tenantID string, f usageAuditFilters, limit, offset int) (usageAuditData, error) {
	billingRows, err := a.billingRepo.ListBillingAudit(ctx, tenantID, f.SubjectType, f.SubjectID, auditFetchCap, 0)
	if err != nil {
		return usageAuditData{}, err
	}
	priceRows, err := a.billingRepo.ListPriceAudit(ctx, tenantID, auditFetchCap, 0)
	if err != nil {
		return usageAuditData{}, err
	}
	labels := a.usageSubjectLabels(ctx, tenantID)

	rows := make([]usageAuditRow, 0, len(billingRows)+len(priceRows))
	kinds := map[string]bool{}
	for _, row := range billingRows {
		kinds[row.Action] = true
		// Budget events record kind+currency in their detail, so an
		// amount-budget limit renders as money in its own currency and a
		// token budget as counted units — never bare numbers.
		currency, _ := row.Detail["currency"].(string)
		rows = append(rows, usageAuditRow{
			Time: row.CreatedAt, Kind: row.Action,
			ActorType: row.ActorType, Actor: row.ActorID,
			SubjectType: row.SubjectType, SubjectID: row.SubjectID,
			SubjectLabel: subjectLabel(labels, billing.Subject{Type: row.SubjectType, ID: row.SubjectID}),
			Amount:       usageAuditAmount(row.Detail, currency),
		})
	}
	for _, row := range priceRows {
		subjectType, _ := row.Detail["subject_type"].(string)
		subjectID, _ := row.Detail["subject_id"].(string)
		currency, _ := row.Detail["currency"].(string)
		kind := "price." + row.Action
		kinds[kind] = true
		rows = append(rows, usageAuditRow{
			Time: row.CreatedAt, Kind: kind,
			ActorType: row.ActorType, Actor: row.ActorID,
			SubjectType: subjectType, SubjectID: subjectID,
			SubjectLabel: subjectLabel(labels, billing.Subject{Type: subjectType, ID: subjectID}),
			Amount:       usageAuditAmount(row.Detail, currency),
		})
	}

	// Kind chips list every event kind present in the tenant, independent of
	// the active kind filter.
	out := usageAuditData{
		Filters: f, Limit: limit, Offset: offset,
		PrevOffset: -1, NextOffset: -1,
		ResetHref: usageAuditHref(usageAuditFilters{}, limit, 0),
	}
	for _, kind := range sortedKinds(kinds) {
		chipFilters := f
		chipFilters.Kind = kind
		out.Kinds = append(out.Kinds, usageKindChip{
			Kind: kind, Label: kind, Active: kind == f.Kind,
			Href: usageAuditHref(chipFilters, limit, 0),
		})
	}
	// "All" keeps the active field filters and clears only the kind — the
	// same "back to the default view" link the other pages' All chips are.
	allFilters := f
	allFilters.Kind = ""
	out.AllHref = usageAuditHref(allFilters, limit, 0)
	out.SubjectTypes = append(out.SubjectTypes, usageSelectOption{Label: "Any subject type"})
	for _, st := range usageAuditSubjectTypes {
		st.Active = st.Value == f.SubjectType
		out.SubjectTypes = append(out.SubjectTypes, st)
	}

	from, hasFrom := usageAuditDay(f.From)
	to, hasTo := usageAuditDay(f.To)
	filtered := make([]usageAuditRow, 0, len(rows))
	for _, row := range rows {
		if f.Kind != "" && row.Kind != f.Kind {
			continue
		}
		if f.SubjectType != "" && row.SubjectType != f.SubjectType {
			continue
		}
		if f.SubjectID != "" && row.SubjectID != f.SubjectID {
			continue
		}
		if f.Actor != "" && row.Actor != f.Actor {
			continue
		}
		if hasFrom && row.Time.Before(from) {
			continue
		}
		if hasTo && row.Time.After(to.Add(24*time.Hour-time.Second)) {
			continue
		}
		filtered = append(filtered, row)
	}
	sort.SliceStable(filtered, func(i, j int) bool { return filtered[i].Time.After(filtered[j].Time) })

	out.Total = len(filtered)
	if offset < len(filtered) {
		end := offset + limit
		if end > len(filtered) {
			end = len(filtered)
		}
		out.Rows = filtered[offset:end]
	}
	if offset > 0 {
		out.PrevOffset = offset - limit
		if out.PrevOffset < 0 {
			out.PrevOffset = 0
		}
		out.PrevHref = usageAuditHref(f, limit, out.PrevOffset)
	}
	if offset+limit < out.Total {
		out.NextOffset = offset + limit
		out.NextHref = usageAuditHref(f, limit, out.NextOffset)
	}
	out.Empty = dashboardEmpty{Icon: "audit", Text: "No events in this range."}
	return out, nil
}

func sortedKinds(kinds map[string]bool) []string {
	out := make([]string, 0, len(kinds))
	for kind := range kinds {
		out = append(out, kind)
	}
	sort.Strings(out)
	return out
}

// usageAuditDay parses a YYYY-MM-DD date input into a UTC day boundary.
func usageAuditDay(raw string) (time.Time, bool) {
	if raw == "" {
		return time.Time{}, false
	}
	t, err := time.ParseInLocation("2006-01-02", raw, time.UTC)
	if err != nil {
		return time.Time{}, false
	}
	return t, true
}

// usageAuditAmount surfaces the event's money shape when the detail carries
// one: price rows carry rate_micros + currency; budget rows carry limit_value
// (money for amount budgets, counted units for token budgets). Anything else
// renders "—".
func usageAuditAmount(detail map[string]any, currency string) string {
	if detail == nil {
		return ""
	}
	if rate, ok := detail["rate_micros"].(float64); ok {
		return money(int64(rate), currency)
	}
	limit, ok := detail["limit_value"].(float64)
	if !ok {
		return ""
	}
	if strings.TrimSpace(currency) == "" {
		return dashboardCountInt(int64(limit)) + " units"
	}
	return money(int64(limit), currency)
}
