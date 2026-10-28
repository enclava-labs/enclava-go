// dashboard_extract.go — the Extract Work page (OA-25, plan 09-04): Process,
// Jobs and Templates tabs rendered from the phase-8 extract producers.
//
// Cutover record (this plan owns the extract removal atomically): the old
// JSON-proxy file content is gone — handleDashboardExtractTemplates/ByID/
// ResetDefaults/Settings/Process/Jobs/Models, dashboardMergeTemplate and
// extractPrincipal died with the rewrite; dashboard.go lost the nine old
// `/dashboard/extract` + `/dashboard/partials/extract*` registrations plus
// handleDashboardPageExtract/handleDashboardExtract/dashboardExtractData;
// templates/page_extract.tmpl, templates/partial_extract.tmpl and
// static/extraction.js are deleted; the five dashboard_resources.go callers
// of extractPrincipal now use the file-local legacyPrincipal. Nothing
// extract-related remains for 09-10 to sweep.
//
// Handlers render only (D-17): every write is a page-module fetch to the
// /api/v1/extract API through Enclava.api, and fragments re-render through
// htmx `refresh from:body` after each write.
package httpx

import (
	"context"
	"encoding/json"
	"fmt"
	"log"
	"net/http"
	"sort"
	"strings"

	"enclava-go/internal/auth"
	"enclava-go/internal/extract"
	"enclava-go/internal/jobs"
)

// extractCollectionOption is one writable knowledge collection in the result
// drawer's "Add to knowledge" select.
type extractCollectionOption struct {
	ID, Name string
}

// extractFieldRow is one result field. Location is filled only where the
// stored Data actually carries provider-emitted per-field location data —
// absent data renders "—", never an invented value.
type extractFieldRow struct {
	Name, Value, Location string
}

// extractView is the model for the Extract page and its three fragments; one
// struct keeps a tab identical whether the page renders it inline or a
// partial route re-renders it after a write.
type extractView struct {
	Tabs        []dashboardTab
	Tab         string
	Templates   []extract.Template // Process picker: execution-grant intersection (D-25)
	Jobs        []extract.JobView  // caller's own extraction jobs only
	Total       int
	Live        bool // any queued/running job → the fragment polls every 3s
	Manage      []extract.Template
	Settings    extract.Settings
	Models      []string
	Job         *extract.JobView
	Result      *extract.ResultView
	Fields      []extractFieldRow
	Writable    []extractCollectionOption
	MaxUploadMB int64
}

func (extractView) DashboardTitle() string  { return "Extract" }
func (extractView) DashboardModule() string { return "extract" }

// registerDashboardExtractRoutes self-registers via init() below (frozen
// phase-7 registrar); page plans never touch dashboard_shell.go.
func (a *App) registerDashboardExtractRoutes() {
	a.mux.HandleFunc("/dashboard/extract", a.sessionAuth.requireCap(capExtract, a.handleDashboardExtractPage))
	a.mux.HandleFunc("/dashboard/partials/extract/jobs", a.sessionAuth.requireCap(capExtract, a.handleDashboardExtractJobsFragment))
	a.mux.HandleFunc("/dashboard/partials/extract/result", a.sessionAuth.requireCap(capExtract, a.handleDashboardExtractResultFragment))
	a.mux.HandleFunc("/dashboard/partials/extract/templates", a.sessionAuth.requireCap(capExtractManage, a.handleDashboardExtractTemplatesFragment))
}

func init() { registerDashboardPage((*App).registerDashboardExtractRoutes) }

func (a *App) handleDashboardExtractPage(w http.ResponseWriter, r *http.Request) {
	if r.Method != http.MethodGet {
		WriteMethodNotAllowed(w)
		return
	}
	caps, _ := capsFromContext(r.Context())
	tab := r.URL.Query().Get("tab")
	if tab != "jobs" && tab != "templates" {
		tab = "process"
	}
	// The Templates tab stays manager-only even by URL (§6.5).
	if tab == "templates" && !caps.grants(capExtractManage) {
		a.sessionAuth.renderDashboardForbidden(w, r)
		return
	}
	p, _ := auth.PrincipalFromContext(r.Context())
	a.renderDashboard(w, r, "extract", a.buildExtractView(r.Context(), tab, p, caps))
}

func (a *App) handleDashboardExtractJobsFragment(w http.ResponseWriter, r *http.Request) {
	if r.Method != http.MethodGet {
		WriteMethodNotAllowed(w)
		return
	}
	p, _ := auth.PrincipalFromContext(r.Context())
	caps, _ := capsFromContext(r.Context())
	a.renderDashboardFragment(w, r, "extract", "extract-jobs", a.buildExtractView(r.Context(), "jobs", p, caps))
}

func (a *App) handleDashboardExtractResultFragment(w http.ResponseWriter, r *http.Request) {
	if r.Method != http.MethodGet {
		WriteMethodNotAllowed(w)
		return
	}
	jobID := strings.TrimSpace(r.URL.Query().Get("job"))
	if jobID == "" {
		WriteOpenAIError(w, http.StatusBadRequest, "job is required", "invalid_request_error", "missing_job")
		return
	}
	p, _ := auth.PrincipalFromContext(r.Context())
	v := extractView{Tab: "jobs"}
	if a.extractSvc != nil {
		detail, err := a.extractSvc.GetJob(r.Context(), p, jobID)
		if err != nil {
			log.Printf("dashboard extract: load job: %v", err)
			WriteOpenAIError(w, http.StatusNotFound, "extraction job not found", "invalid_request_error", "job_not_found")
			return
		}
		v.Job = &detail.Job
		v.Result = detail.Result
		v.Fields = extractFieldRows(detail.Result)
		if detail.Result != nil {
			v.Writable = a.extractWritableCollections(r.Context(), p)
		}
	}
	a.renderDashboardFragment(w, r, "extract", "extract-result", v)
}

func (a *App) handleDashboardExtractTemplatesFragment(w http.ResponseWriter, r *http.Request) {
	if r.Method != http.MethodGet {
		WriteMethodNotAllowed(w)
		return
	}
	p, _ := auth.PrincipalFromContext(r.Context())
	caps, _ := capsFromContext(r.Context())
	a.renderDashboardFragment(w, r, "extract", "extract-templates", a.buildExtractView(r.Context(), "templates", p, caps))
}

// buildExtractView assembles the per-tab model; a nil extract service yields
// the truthful empty states instead of fabricated rows.
func (a *App) buildExtractView(ctx context.Context, tab string, p auth.Principal, caps dashboardCaps) extractView {
	v := extractView{Tab: tab, Tabs: extractTabs(caps, tab), MaxUploadMB: a.maxUploadMB}
	if a.extractSvc == nil {
		return v
	}
	var err error
	switch tab {
	case "jobs":
		v.Jobs, v.Total, err = a.extractSvc.ListJobs(ctx, p, "", 50, 0)
		if err != nil {
			log.Printf("dashboard extract: list jobs: %v", err)
		}
		v.Live = extractHasLive(v.Jobs)
	case "templates":
		v.Manage, err = a.extractSvc.ListTemplates(ctx, p)
		if err != nil {
			log.Printf("dashboard extract: list templates: %v", err)
		}
		v.Settings, err = a.extractSvc.GetSettings(ctx, p)
		if err != nil {
			log.Printf("dashboard extract: load settings: %v", err)
		}
		v.Models = a.extractModels(ctx, p)
	default:
		v.Templates, err = a.extractSvc.ListExecutableTemplates(ctx, p)
		if err != nil {
			log.Printf("dashboard extract: list executable templates: %v", err)
		}
	}
	return v
}

// extractTabs: Process · Jobs · Templates, the Templates link only under
// extract.manage (member never sees it).
func extractTabs(caps dashboardCaps, active string) []dashboardTab {
	tabs := []dashboardTab{
		{Label: "Process", Href: "/dashboard/extract?tab=process", Active: active == "process"},
		{Label: "Jobs", Href: "/dashboard/extract?tab=jobs", Active: active == "jobs"},
	}
	if caps.grants(capExtractManage) {
		tabs = append(tabs, dashboardTab{Label: "Templates", Href: "/dashboard/extract?tab=templates", Active: active == "templates"})
	}
	return tabs
}

func extractHasLive(jobViews []extract.JobView) bool {
	for _, j := range jobViews {
		if j.Status == jobs.StatusQueued || j.Status == jobs.StatusRunning {
			return true
		}
	}
	return false
}

// extractModels lists the model aliases the tenant's vision-capable endpoints
// expose, narrowed to what the caller may actually select — the same rule the
// settings PUT enforces (auth.ModelAllowed).
func (a *App) extractModels(ctx context.Context, p auth.Principal) []string {
	models, err := a.extractSvc.ModelsForExtract(ctx, p.TenantID)
	if err != nil {
		log.Printf("dashboard extract: list models: %v", err)
		return nil
	}
	out := make([]string, 0, len(models))
	for _, m := range models {
		if auth.ModelAllowed(p, m.ID) {
			out = append(out, m.ID)
		}
	}
	sort.Strings(out)
	return out
}

// extractWritableCollections mirrors the writability rule the knowledge
// import endpoint enforces (rag.collectionManageable: owner or
// platform.admin) so the drawer only offers collections the import would
// accept.
func (a *App) extractWritableCollections(ctx context.Context, p auth.Principal) []extractCollectionOption {
	if a.ragSvc == nil {
		return nil
	}
	collections, err := a.ragSvc.ListCollections(ctx, p)
	if err != nil {
		log.Printf("dashboard extract: list collections: %v", err)
		return nil
	}
	out := make([]extractCollectionOption, 0, len(collections))
	for _, c := range collections {
		if auth.HasScope(p, auth.ScopePlatformAdmin) || (c.OwnerActorID != "" && c.OwnerActorID == p.ActorID) {
			out = append(out, extractCollectionOption{ID: c.ID, Name: c.Name})
		}
	}
	sort.Slice(out, func(i, j int) bool { return out[i].Name < out[j].Name })
	return out
}

// extractFieldRows projects result Data into one row per field. A Location is
// filled only where the stored Data carries provider-emitted per-field
// location data ({"value":…,"location":…}) — no producer exists today
// (service.go persists Data + validation only), so cells stay "—" rather
// than an invented location.
func extractFieldRows(result *extract.ResultView) []extractFieldRow {
	if result == nil || len(result.Data) == 0 {
		return nil
	}
	keys := make([]string, 0, len(result.Data))
	for k := range result.Data {
		keys = append(keys, k)
	}
	sort.Strings(keys)
	rows := make([]extractFieldRow, 0, len(keys))
	for _, k := range keys {
		row := extractFieldRow{Name: k, Value: extractFieldValue(result.Data[k]), Location: "—"}
		if m, ok := result.Data[k].(map[string]any); ok {
			if loc, ok := m["location"].(string); ok && strings.TrimSpace(loc) != "" {
				row.Location = loc
				if val, has := m["value"]; has {
					row.Value = extractFieldValue(val)
				}
			}
		}
		rows = append(rows, row)
	}
	return rows
}

// extractFieldValue renders a decoded Data value plainly; GetJob decodes with
// json.Number, so numbers keep their exact stored digits.
func extractFieldValue(v any) string {
	switch t := v.(type) {
	case nil:
		return ""
	case json.Number:
		return t.String()
	case string:
		return t
	case map[string]any, []any:
		b, err := json.Marshal(t)
		if err != nil {
			return fmt.Sprint(t)
		}
		return string(b)
	default:
		return fmt.Sprint(t)
	}
}
