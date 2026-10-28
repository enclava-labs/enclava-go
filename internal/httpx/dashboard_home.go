// dashboard_home.go — Home at /dashboard (OA-31): the capability-composed
// landing page. Sections render in source §6.2 order from real phase-8
// producer reads only — dashboardAttention/dashboardActivity/dashboardHomeMetrics,
// the 08-04 operation-filtered extract job list, the 08-01 audience- and
// grant-enforced chatbot list and the phase-7 confidential summary. Nothing
// is fabricated: an empty producer renders its shared empty state, never
// fake content. The /dashboard/ subtree keeps the phase-6 catch-all role:
// Home only for the exact subtree root, the designed 404 for everything else
// (OA-03 — unknown paths never render Home with 200).
package httpx

import (
	"context"
	"encoding/json"
	"log"
	"net/http"
	"time"

	"enclava-go/internal/auth"
	"enclava-go/internal/chatbot"
	"enclava-go/internal/extract"
	"enclava-go/internal/jobs"
	"enclava-go/internal/rag"
	"enclava-go/internal/store/postgres"
)

func init() { registerDashboardPage((*App).registerDashboardHomeRoutes) }

const (
	homeSparkDays          = 30 // trailing zero-filled window behind the sparkline
	homeConversationsLimit = 5  // recent conversations shown on Home
	homeExtractionsLimit   = 5  // recent extraction rows shown on Home
)

// homeData is Home's page model; DashboardModule is empty (D-20): Home is
// links plus the shared polling machinery — no page module.
type homeData struct {
	Greeting        string
	APIBase         string
	Attention       []AttentionItem
	KPIs            homeKPIs
	Running         homeRunningData
	Conversations   []chatbot.ConversationSummary
	Assistants      []chatbot.Instance
	Extractions     []extract.JobView
	ExtractionTotal int
	Collections     []rag.Collection
}

func (d homeData) DashboardTitle() string  { return "Home" }
func (d homeData) DashboardModule() string { return "" }

// homeKPIs carries the billing.view-gated grid. Spark is the zero-filled
// 30-day request series (floats for the phase-7 spark() helper — no second
// spark implementation); Spend stays per-currency and is never summed.
type homeKPIs struct {
	Requests30d   int64
	Spark         []float64
	Spend         []MoneyAmount
	Budget        *homeBudget
	Documents     int64
	Conversations int64
}

// homeBudget pairs the shared progress scalar with its render percent —
// exact integer math via attentionPercent, never a float recomputation.
type homeBudget struct {
	Progress BudgetProgress
	Percent  int64
}

// homeRunningData feeds the polling fragment (both the embedded section and
// GET /dashboard/partials/home/running). Live reports non-terminal work —
// the fragment emits the 3s trigger only while it holds.
type homeRunningData struct {
	Rows []homeRunningRow
	Live bool
}

// homeRunningRow is one activity row plus its render projections: Percent is
// set only when the stored worker progress carries a real number.
type homeRunningRow struct {
	ActivityItem
	Live       bool
	Percent    int
	HasPercent bool
}

// registerDashboardHomeRoutes mounts the canonical slashless /dashboard (its
// own registration — net/http would 301 it into the subtree before any
// handler ran), the /dashboard/ subtree catch-all and the running partial,
// all behind capSession (D-18: Home needs a session, no capability).
func (a *App) registerDashboardHomeRoutes() {
	a.mux.HandleFunc("/dashboard", a.sessionAuth.capSession(a.handleDashboardHomePage))
	a.mux.HandleFunc("/dashboard/", a.sessionAuth.capSession(a.handleDashboardHomeSubtree))
	a.mux.HandleFunc("/dashboard/partials/home/running", a.sessionAuth.capSession(a.handleDashboardHomeRunning))
}

// handleDashboardHomeSubtree preserves the phase-6 catch-all contract: the
// exact subtree root renders Home; every other /dashboard/* path is the
// designed 404 page — never Home, never the JSON NotFound.
func (a *App) handleDashboardHomeSubtree(w http.ResponseWriter, r *http.Request) {
	if r.URL.Path != "/dashboard/" {
		a.sessionAuth.renderDashboardNotFound(w, r)
		return
	}
	a.handleDashboardHomePage(w, r)
}

// handleDashboardHomePage renders the full §6.2 composition.
func (a *App) handleDashboardHomePage(w http.ResponseWriter, r *http.Request) {
	if r.Method != http.MethodGet {
		WriteMethodNotAllowed(w)
		return
	}
	caps, _ := capsFromContext(r.Context())
	a.renderDashboard(w, r, "home", a.dashboardHomeData(r.Context(), caps))
}

// handleDashboardHomeRunning serves the self-replacing running-now fragment;
// polling stops server-side by omitting the every-3s trigger when terminal.
func (a *App) handleDashboardHomeRunning(w http.ResponseWriter, r *http.Request) {
	if r.Method != http.MethodGet {
		WriteMethodNotAllowed(w)
		return
	}
	caps, _ := capsFromContext(r.Context())
	d := homeData{APIBase: "/v1", Attention: []AttentionItem{}}
	d.Running = a.dashboardHomeRunning(r.Context(), caps)
	a.renderDashboardFragment(w, r, "home", "running", d)
}

// dashboardHomeData composes every section from real producer reads. A
// failing producer logs and degrades its own section — one broken read never
// blanks the page.
func (a *App) dashboardHomeData(ctx context.Context, caps dashboardCaps) homeData {
	p, _ := auth.PrincipalFromContext(ctx)
	d := homeData{
		Greeting:  homeGreeting(time.Now()),
		APIBase:   "/v1", // OpenAI-compatible routes live under /v1, never /api/v1
		Attention: []AttentionItem{},
	}
	if items, err := a.dashboardAttention(ctx, p, caps); err != nil {
		log.Printf("dashboard home: attention: %v", err)
	} else {
		d.Attention = items
	}
	if caps.BillingView {
		if m, err := a.dashboardHomeMetrics(ctx, p, caps); err != nil {
			log.Printf("dashboard home: metrics: %v", err)
		} else {
			d.KPIs = homeKPIs{
				Requests30d:   m.Requests30d,
				Spark:         homeSparkSeries(m.Series30d),
				Spend:         m.Spend30d,
				Documents:     m.DocumentTotal,
				Conversations: m.ConversationTotal,
			}
			if m.OrgBudget != nil {
				d.KPIs.Budget = &homeBudget{
					Progress: *m.OrgBudget,
					Percent:  attentionPercent(m.OrgBudget.Counted, m.OrgBudget.LimitValue),
				}
			}
		}
	}
	d.Running = a.dashboardHomeRunning(ctx, caps)
	if a.chatbotSvc != nil && caps.Chat {
		if convs, _, err := a.chatbotSvc.ListConversations(ctx, p, homeConversationsLimit, ""); err != nil {
			log.Printf("dashboard home: conversations: %v", err)
		} else {
			d.Conversations = convs
		}
		if bots, err := a.chatbotSvc.List(ctx, p); err != nil {
			log.Printf("dashboard home: assistants: %v", err)
		} else {
			d.Assistants = bots
		}
	}
	if caps.Extract && a.extractSvc != nil {
		// The 08-04 operation-filtered read: "" selects the member-facing
		// extraction set (extract.process + extract.vision) — shared jobs
		// under the same owner never inflate the total (no second filter).
		if views, total, err := a.extractSvc.ListJobs(ctx, p, "", homeExtractionsLimit, 0); err != nil {
			log.Printf("dashboard home: extractions: %v", err)
		} else {
			d.Extractions = views
			d.ExtractionTotal = total
		}
	}
	if caps.Search && a.ragSvc != nil {
		if cols, err := a.ragSvc.ListCollections(ctx, p); err != nil {
			log.Printf("dashboard home: collections: %v", err)
		} else {
			d.Collections = cols
		}
	}
	return d
}

// dashboardHomeRunning reads the caller's activity feed and projects the
// polling fragment state.
func (a *App) dashboardHomeRunning(ctx context.Context, caps dashboardCaps) homeRunningData {
	p, _ := auth.PrincipalFromContext(ctx)
	items, err := a.dashboardActivity(ctx, p, caps)
	if err != nil {
		log.Printf("dashboard home: activity: %v", err)
		return homeRunningData{Rows: []homeRunningRow{}}
	}
	rows := make([]homeRunningRow, 0, len(items))
	live := false
	for _, it := range items {
		row := homeRunningRow{
			ActivityItem: it,
			Live:         it.Status == jobs.StatusQueued || it.Status == jobs.StatusRunning,
		}
		if row.Live {
			live = true
		}
		// Progress is the verbatim worker payload; a numeric "percent" is a
		// real count — anything else renders indeterminate, never invented %.
		var doc struct {
			Percent any `json:"percent"`
		}
		if len(it.Progress) > 0 && json.Unmarshal(it.Progress, &doc) == nil {
			if f, ok := doc.Percent.(float64); ok {
				row.Percent = int(f)
				row.HasPercent = true
			}
		}
		rows = append(rows, row)
	}
	return homeRunningData{Rows: rows, Live: live}
}

// homeGreeting picks the daypart word for the greeting card.
func homeGreeting(now time.Time) string {
	switch h := now.Hour(); {
	case h < 12:
		return "Good morning"
	case h < 18:
		return "Good afternoon"
	default:
		return "Good evening"
	}
}

// homeSparkSeries zero-fills the producer's real daily rows into the
// trailing 30-day UTC window ending at today (or the newest observed day,
// so backdated fixtures stay stable): one slot per day, 0 where no usage
// exists — fabricated observations never enter the series.
func homeSparkSeries(series []postgres.DailyUsageRow) []float64 {
	if len(series) == 0 {
		return nil
	}
	perDay := make(map[string]int64, len(series))
	var maxDay time.Time
	for _, row := range series {
		day := row.Day.UTC().Truncate(24 * time.Hour)
		perDay[day.Format("2006-01-02")] += row.RequestCount
		if day.After(maxDay) {
			maxDay = day
		}
	}
	end := time.Now().UTC().Truncate(24 * time.Hour)
	if maxDay.After(end) {
		end = maxDay
	}
	out := make([]float64, homeSparkDays)
	for i := range out {
		day := end.AddDate(0, 0, -(homeSparkDays - 1 - i))
		out[i] = float64(perDay[day.Format("2006-01-02")])
	}
	return out
}
