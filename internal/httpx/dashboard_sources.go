// dashboard_sources.go — Sources page + add-source wizard (OA-23, plan 09-02).
// Replaces /dashboard/connectors (old handlers in dashboard_resources.go are
// obsolete after this lands — 09-10 deletes: handleDashboardPageConnectors,
// handleDashboardConnectors, handleDashboardConnectorAction, routes
// /dashboard/connectors + /dashboard/partials/connectors,
// templates/partial_connectors.tmpl, static/connectors.js).
// D-17: handlers render only — every write goes through Enclava.api against
// /api/v1/connectors/* from static/pages/sources.js. D-18: one capability
// per route; the credentials tab is separately gated on access.manage.
package httpx

import (
	"context"
	"fmt"
	"html/template"
	"log"
	"net/http"
	"net/url"
	"strconv"
	"strings"
	"time"

	"enclava-go/internal/auth"
	"enclava-go/internal/connectors"
	"enclava-go/internal/credentials"
	"enclava-go/internal/rag"
)

// dashboardSourcesLimit bounds the table the same way the old page did.
const dashboardSourcesLimit = 100

// dashboardSourceProviders is the wizard's five families in tile order
// (D-13: every canonical connector kind, none silently dropped).
var dashboardSourceProviders = []struct {
	ID, Label, Icon, Subtitle string
	CredentialRequired        bool
}{
	{"slack", "Slack", "chat", "Channel messages", true},
	{"teams", "Microsoft Teams", "users", "Channels and chats", true},
	{"github", "GitHub", "command", "Repository files", false},
	{"notion", "Notion", "file", "Pages and subpages", true},
	{"google_drive", "Google Drive", "upload", "Shared folders", true},
}

// sourceProviderLabel maps a provenance source_kind to its display label;
// uploads and unknown kinds have no provider ("", never jargon).
func sourceProviderLabel(kind string) string {
	for _, p := range dashboardSourceProviders {
		if p.ID == strings.TrimSpace(kind) {
			return p.Label
		}
	}
	return ""
}

// safeExternalLink admits only http(s) targets for outbound "Open in …"
// links rendered outside the rag service's own gate.
func safeExternalLink(raw string) string {
	parsed, err := url.Parse(strings.TrimSpace(raw))
	if err != nil || parsed.Host == "" {
		return ""
	}
	switch strings.ToLower(parsed.Scheme) {
	case "https", "http":
		return parsed.String()
	}
	return ""
}

// dashboardSourceRule is the single read rule every synced object follows:
// the destination collection's readers, and only they, can read what the
// source syncs. Rendered verbatim; <collection> interpolates the name.
const dashboardSourceRuleFmt = "Everyone who can open %s can read everything this source syncs."

// dashboardSourceRule renders the rule for a row: the resolved collection
// name when present, "the collection" otherwise.
func dashboardSourceRule(collection string) string {
	name := strings.TrimSpace(collection)
	if name == "" {
		name = "the collection"
	}
	return fmt.Sprintf(dashboardSourceRuleFmt, name)
}

// dashboardSourcesData is the page/table view model.
type dashboardSourcesData struct {
	Header     dashboardHeader
	Tab        string // "sources" | "credentials" — selects the rendered section
	Rows       []dashboardSourceRow
	Status     string // active filter bucket: ""|syncing|healthy|attention|paused
	AnySyncing bool
	Empty      dashboardEmpty
}

func (d dashboardSourcesData) DashboardTitle() string  { return "Sources" }
func (d dashboardSourcesData) DashboardModule() string { return "sources" }

// dashboardSourceRow is one rendered table row: real Source/SyncJob fields
// only, plus server-resolved projections (collection name, next sync, and the
// IndexProgress rollup behind the progress bar and failure line).
type dashboardSourceRow struct {
	Source      connectors.Source
	Provider    string // display label, e.g. "Slack"
	Monogram    string // one-letter badge from the source name
	Scope       string
	Collection  string
	Permissions string // collection read rule with the collection name filled in
	State       string // statusPill state: healthy|syncing|warning|paused
	LastError   string

	NextEnabled bool
	NextAt      time.Time

	HasItems      bool
	Items         int64
	Indexed       int64
	Failed        int64
	Processing    int64
	Fetching      bool   // the content stream is still listing items
	FailureReason string // most common failed-item message, escaped at render
	FailureCount  int64
	HasProgress   bool
	ProgressPct   int
}

// sourceStatusFilter buckets rows for the filter chips (handler-side filter).
var sourceStatusFilter = map[string]func(dashboardSourceRow) bool{
	"syncing":   func(r dashboardSourceRow) bool { return r.State == "syncing" },
	"healthy":   func(r dashboardSourceRow) bool { return r.State == "healthy" },
	"attention": func(r dashboardSourceRow) bool { return r.State == "warning" },
	"paused":    func(r dashboardSourceRow) bool { return r.State == "paused" },
}

func (a *App) registerDashboardSourcesRoutes() {
	a.mux.HandleFunc("/dashboard/sources", a.sessionAuth.requireCap(capSourcesManage, a.handleDashboardPageSources))
	a.mux.HandleFunc("/dashboard/sources/credentials", a.sessionAuth.requireCap(capAccessManage, a.handleDashboardPageCredentials))
	a.mux.HandleFunc("/dashboard/partials/sources/table", a.sessionAuth.requireCap(capSourcesManage, a.handleDashboardSourcesTable))
	a.mux.HandleFunc("/dashboard/partials/sources/credentials", a.sessionAuth.requireCap(capAccessManage, a.handleDashboardSourcesCredentials))
	a.mux.HandleFunc("/dashboard/partials/sources/drawer", a.sessionAuth.requireCap(capSourcesManage, a.handleDashboardSourcesDrawer))
}

func init() { registerDashboardPage((*App).registerDashboardSourcesRoutes) }

func (a *App) handleDashboardPageSources(w http.ResponseWriter, r *http.Request) {
	if r.Method != http.MethodGet {
		WriteMethodNotAllowed(w)
		return
	}
	p, _ := auth.PrincipalFromContext(r.Context())
	caps, _ := capsFromContext(r.Context())
	rows, anySyncing := a.dashboardSourceRows(r.Context(), p)
	status := r.URL.Query().Get("status")
	rows = filterSourceRows(rows, status)
	anySyncing = anySyncing && len(rows) > 0 && anyRowSyncing(rows)
	data := dashboardSourcesData{
		Header: dashboardHeader{
			Crumb: "Sources",
			Title: "Sources",
			Desc:  "Connected content, synced with the right permissions.",
			Actions: template.HTML(`<button type="button" class="btn primary" data-action="open-drawer" data-drawer-url="/dashboard/partials/sources/drawer">` + //nolint:gosec // Fixed markup and trusted icon only.
				string(dashboardIconPlus()) + `Add source</button>`),
		},
		Rows:       rows,
		Tab:        "sources",
		Status:     status,
		AnySyncing: anySyncing,
		Empty:      sourcesEmpty(status, caps.SourcesManage),
	}
	a.renderDashboard(w, r, "sources", data)
}

func (a *App) handleDashboardSourcesTable(w http.ResponseWriter, r *http.Request) {
	if r.Method != http.MethodGet {
		WriteMethodNotAllowed(w)
		return
	}
	p, _ := auth.PrincipalFromContext(r.Context())
	caps, _ := capsFromContext(r.Context())
	rows, _ := a.dashboardSourceRows(r.Context(), p)
	status := r.URL.Query().Get("status")
	if _, ok := sourceStatusFilter[status]; !ok && status != "" {
		status = ""
	}
	rows = filterSourceRows(rows, status)
	anySyncing := anyRowSyncing(rows)
	a.renderDashboardFragment(w, r, "sources", "sources-table", dashboardSourcesData{Rows: rows, Status: status, AnySyncing: anySyncing, Empty: sourcesEmpty(status, caps.SourcesManage)})
}

// ── Credentials tab (access.manage-gated) ────────────────────────────────

// dashboardCredentialsData feeds the Credentials tab: the secrets store's
// own listing plus the one-shot OAuth return notices (?connected/?error).
type dashboardCredentialsData struct {
	Header       dashboardHeader
	Tab          string
	Enabled      bool
	RedirectBase string
	Cards        []dashboardCredentialCard
	Connected    string // credential ID from a completed OAuth redirect
	Error        string
	LoadError    bool // Listing failed — degrade to the notice, hide forms
}

func (d dashboardCredentialsData) DashboardTitle() string  { return "Credentials" }
func (d dashboardCredentialsData) DashboardModule() string { return "sources" }

// dashboardCredentialCard is one provider card: stored credentials,
// environment-bound refs and the OAuth-app state grouped for render.
type dashboardCredentialCard struct {
	Provider         string
	Label            string
	Hint             string
	Credentials      []credentials.CredentialView
	EnvRefs          []string
	OAuthSupported   bool
	OAuthConfigured  bool
	OAuthClientID    string
	OAuthRedirectURI string
	CanConnect       bool // app configured AND a public base URL is set
}

// dashboardCredentialHints is the short "what to paste" line per provider —
// static product facts, like dashboardSourcePermissions.
var dashboardCredentialHints = map[string]string{
	"github":       "A fine-grained or classic personal access token with repo read access.",
	"slack":        "A bot token (xoxb-…) with channels:history, groups:history, channels:read, groups:read, users:read and files:read.",
	"notion":       "An internal integration secret from the Notion integration's settings.",
	"teams":        "The app registration's client secret — tenant and client IDs stay on each source.",
	"google_drive": "OAuth is recommended — access tokens expire after about an hour.",
}

// handleDashboardPageCredentials renders the Credentials tab page; the
// fragment route below re-renders just the section for htmx refreshes.
func (a *App) handleDashboardPageCredentials(w http.ResponseWriter, r *http.Request) {
	if r.Method != http.MethodGet {
		WriteMethodNotAllowed(w)
		return
	}
	data := a.dashboardCredentialsView(r)
	data.Header = dashboardHeader{
		Crumb: "Sources",
		Title: "Connector credentials",
		Desc:  "API tokens and OAuth apps the tenant's sources connect with.",
	}
	a.renderDashboard(w, r, "sources", data)
}

func (a *App) handleDashboardSourcesCredentials(w http.ResponseWriter, r *http.Request) {
	if r.Method != http.MethodGet {
		WriteMethodNotAllowed(w)
		return
	}
	a.renderDashboardFragment(w, r, "sources", "sources-credentials", a.dashboardCredentialsView(r))
}

// dashboardCredentialsView assembles the per-provider cards from the real
// credentials listing. A missing service or a listing error degrades to the
// disabled/failed notice — never a fabricated provider state.
func (a *App) dashboardCredentialsView(r *http.Request) dashboardCredentialsData {
	p, _ := auth.PrincipalFromContext(r.Context())
	q := r.URL.Query()
	data := dashboardCredentialsData{
		Tab:       "credentials",
		Connected: strings.TrimSpace(q.Get("connected")),
		Error:     strings.TrimSpace(q.Get("error")),
	}
	var listing credentials.Listing
	if a.credentials == nil {
		data.LoadError = true
	} else if l, err := a.credentials.List(r.Context(), p); err != nil {
		log.Printf("dashboard sources credentials: %v", err)
		data.LoadError = true
	} else {
		listing = l
	}
	data.Enabled = listing.Enabled
	data.RedirectBase = listing.RedirectBase
	for _, provider := range dashboardSourceProviders {
		card := dashboardCredentialCard{
			Provider: provider.ID,
			Label:    provider.Label,
			Hint:     dashboardCredentialHints[provider.ID],
		}
		for _, cred := range listing.Credentials {
			if cred.Provider == provider.ID {
				card.Credentials = append(card.Credentials, cred)
			}
		}
		for _, env := range listing.Environment {
			if env.Provider == provider.ID {
				card.EnvRefs = append(card.EnvRefs, env.ID)
			}
		}
		for _, app := range listing.OAuth {
			if app.Provider != provider.ID {
				continue
			}
			card.OAuthSupported = app.Supported
			card.OAuthConfigured = app.Configured
			card.OAuthClientID = app.ClientID
			card.OAuthRedirectURI = app.RedirectURI
		}
		card.CanConnect = card.OAuthSupported && card.OAuthConfigured && data.RedirectBase != ""
		data.Cards = append(data.Cards, card)
	}
	return data
}

func sourcesEmpty(status string, canManage bool) dashboardEmpty {
	empty := dashboardEmpty{Icon: "sources", Text: "No sources connected."}
	if status != "" {
		empty.Text = "No sources match this filter."
	} else if canManage {
		empty.ActionLabel, empty.ActionHref = "Add source", "/dashboard/sources?new=1"
	}
	return empty
}

// filterSourceRows narrows to the chip's status bucket server-side; unknown
// or empty buckets render everything.
func filterSourceRows(rows []dashboardSourceRow, status string) []dashboardSourceRow {
	pred, ok := sourceStatusFilter[status]
	if !ok {
		return rows
	}
	out := make([]dashboardSourceRow, 0, len(rows))
	for _, row := range rows {
		if pred(row) {
			out = append(out, row)
		}
	}
	return out
}

func anyRowSyncing(rows []dashboardSourceRow) bool {
	for _, row := range rows {
		if row.State == "syncing" {
			return true
		}
	}
	return false
}

// dashboardSourcesDrawerData feeds the five-step wizard. CredentialRefs are
// the server-filtered, env-backed names per provider (never secret values).
type dashboardSourcesDrawerData struct {
	CredentialRefs   map[string][]auth.SecretRefMeta
	Collections      []rag.Collection
	PresetCollection string
	EditSource       *connectors.Source
}

func (a *App) handleDashboardSourcesDrawer(w http.ResponseWriter, r *http.Request) {
	if r.Method != http.MethodGet {
		WriteMethodNotAllowed(w)
		return
	}
	p, _ := auth.PrincipalFromContext(r.Context())
	data := dashboardSourcesDrawerData{CredentialRefs: map[string][]auth.SecretRefMeta{}}
	if id := strings.TrimSpace(r.URL.Query().Get("source")); id != "" {
		if a.connectorSvc == nil {
			WriteOpenAIError(w, http.StatusNotImplemented, "connectors are not configured", "server_error", "not_configured")
			return
		}
		src, err := a.connectorSvc.GetSource(r.Context(), p, id)
		if err != nil {
			writeConnectorNotFoundOrError(w, err, "connector_source_fetch_failed")
			return
		}
		data.EditSource = &src
	}
	if a.connectorSvc != nil {
		for _, provider := range dashboardSourceProviders {
			data.CredentialRefs[provider.ID] = a.connectorSvc.CredentialRefs(p, provider.ID)
		}
	}
	if a.ragSvc != nil {
		// Writable collections only: the caller owns them or is a tenant
		// administrator (mirrors rag's collectionManageable predicate).
		if collections, err := a.ragSvc.ListCollections(r.Context(), p); err == nil {
			admin := auth.HasScope(p, auth.ScopePlatformAdmin)
			for _, c := range collections {
				if admin || (c.OwnerActorID != "" && c.OwnerActorID == p.ActorID) {
					data.Collections = append(data.Collections, c)
				}
			}
		} else {
			log.Printf("dashboard sources drawer: collections: %v", err)
		}
	}
	data.PresetCollection = strings.TrimSpace(r.URL.Query().Get("collection"))
	a.renderDashboardFragment(w, r, "sources", "sources-drawer", data)
}

// dashboardSourceRows projects the caller's sources into table rows: the
// latest sync job per source (bounded fan-out, same pattern as the attention
// aggregator — covers scheduled syncs submitted under the scheduler's
// principal, which the submitter-bound unscoped listing cannot see).
func (a *App) dashboardSourceRows(ctx context.Context, p auth.Principal) ([]dashboardSourceRow, bool) {
	if a.connectorSvc == nil {
		return nil, false
	}
	sources, err := a.connectorSvc.ListSources(ctx, p, "", dashboardSourcesLimit, 0)
	if err != nil {
		log.Printf("dashboard sources: list: %v", err)
		return nil, false
	}
	names := map[string]string{}
	if a.ragSvc != nil {
		if collections, err := a.ragSvc.ListCollections(ctx, p); err == nil {
			for _, c := range collections {
				names[c.ID] = c.Name
			}
		}
	}
	rows := make([]dashboardSourceRow, 0, len(sources))
	anySyncing := false
	for _, src := range sources {
		var latest *connectors.SyncJob
		if jobs, err := a.connectorSvc.ListSyncJobs(ctx, p, src.ID, "", 1, 0); err == nil && len(jobs) > 0 {
			latest = &jobs[0]
		} else if err != nil {
			log.Printf("dashboard sources: jobs for %s: %v", src.ID, err)
		}
		row := dashboardSourceRow{
			Source:      src,
			Provider:    dashboardSourceProviderLabel(src.Type),
			Monogram:    dashboardSourceMonogram(src.Name, src.Type),
			Scope:       dashboardSourceScope(src),
			Collection:  dashboardSourceCollection(src.TargetCollectionID, names),
			Permissions: dashboardSourceRule(names[src.TargetCollectionID]),
			State:       dashboardSourceState(src, latest),
			LastError:   dashboardSourceError(latest),
		}
		if row.LastError == "" && row.State != "syncing" {
			// The content stream's own error covers failures the job row
			// does not carry.
			if state, err := a.connectorSvc.GetStreamState(ctx, p, src.ID, string(connectors.StreamContent)); err == nil && state.LastError != "" {
				row.LastError = state.LastError
				if src.IsEnabled {
					row.State = "warning"
				}
			}
		}
		row.NextEnabled, row.NextAt = dashboardNextSync(src.Config, src.LastSyncAt, src.CreatedAt, src.IsEnabled)
		row.HasProgress, row.ProgressPct = dashboardStatPercent(latest)
		row.HasItems, row.Items = dashboardStatItems(latest)
		if progress, err := a.connectorSvc.IndexProgress(ctx, p, src.ID); err == nil {
			row.Fetching = progress.Fetching
			row.HasItems = true
			row.Items = progress.Items
			row.Indexed = progress.Indexed
			row.Failed = progress.Failed
			row.Processing = progress.Processing
			row.FailureReason = progress.FailureReason
			row.FailureCount = progress.FailureCount
			if progress.Items > 0 {
				row.HasProgress = true
				row.ProgressPct = int((progress.Indexed + progress.Failed) * 100 / progress.Items)
			}
			if progress.Fetching || progress.Processing > 0 {
				row.State = "syncing"
			} else if progress.Failed > 0 && row.State == "healthy" {
				row.State = "warning" // items that never indexed need someone's action
			}
		} else {
			log.Printf("dashboard sources: index progress for %s: %v", src.ID, err)
		}
		if row.State == "syncing" {
			anySyncing = true
		}
		rows = append(rows, row)
	}
	return rows, anySyncing
}

// dashboardSourceState derives the status pill state from real fields only:
// paused wins over everything, an active sync next, a failed last sync
// renders Needs attention, everything else is healthy.
func dashboardSourceState(src connectors.Source, latest *connectors.SyncJob) string {
	switch {
	case !src.IsEnabled:
		return "paused"
	case latest != nil && (latest.Status == connectors.JobStatusQueued || latest.Status == connectors.JobStatusRunning):
		return "syncing"
	case latest != nil && latest.Status == connectors.JobStatusFailed:
		return "warning"
	default:
		return "healthy"
	}
}

func dashboardSourceError(latest *connectors.SyncJob) string {
	if latest != nil && latest.Status == connectors.JobStatusFailed {
		return latest.ErrorMessage
	}
	return ""
}

// dashboardSourceProviderLabel maps a canonical kind to its tile label.
func dashboardSourceProviderLabel(kind string) string {
	for _, p := range dashboardSourceProviders {
		if p.ID == kind {
			return p.Label
		}
	}
	return dashboardHumanize(kind)
}

// dashboardSourceMonogram is the one-letter badge: the source name's first
// letter, falling back to the provider label.
func dashboardSourceMonogram(name, kind string) string {
	for _, r := range strings.TrimSpace(name) {
		return strings.ToUpper(string(r))
	}
	label := dashboardSourceProviderLabel(kind)
	for _, r := range label {
		return strings.ToUpper(string(r))
	}
	return "S"
}

// dashboardSourceCollection resolves a target to its readable name.
func dashboardSourceCollection(id string, names map[string]string) string {
	if id == "" {
		return "—"
	}
	if name := names[id]; name != "" {
		return name
	}
	return "Collection unavailable"
}

// dashboardNextSync mirrors the scheduler's syncDue contract (the connectors
// package keeps parseSyncInterval unexported): default 1h, "0"/"off"/disabled
// or unparseable disables scheduling, seconds-as-number accepted. The next
// run is base+interval from the last sync, or from creation when never
// synced. Disabled sources are never scheduled.
func dashboardNextSync(config map[string]any, last *time.Time, created time.Time, enabled bool) (bool, time.Time) {
	if !enabled {
		return false, time.Time{}
	}
	interval, ok := func() (time.Duration, bool) {
		const def = time.Hour
		switch v := config["sync_interval"].(type) {
		case nil:
			return def, true
		case string:
			s := strings.TrimSpace(strings.ToLower(v))
			switch s {
			case "":
				return def, true
			case "0", "off", "disabled":
				return 0, false
			}
			if secs, err := strconv.ParseFloat(s, 64); err == nil {
				if secs <= 0 {
					return 0, false
				}
				return max(time.Duration(secs*float64(time.Second)), 5*time.Minute), true
			}
			if d, err := time.ParseDuration(s); err == nil {
				if d <= 0 {
					return 0, false
				}
				return max(d, 5*time.Minute), true
			}
			return 0, false
		case float64:
			if v <= 0 {
				return 0, false
			}
			return max(time.Duration(v*float64(time.Second)), 5*time.Minute), true
		case int:
			if v <= 0 {
				return 0, false
			}
			return max(time.Duration(v)*time.Second, 5*time.Minute), true
		default:
			return 0, false
		}
	}()
	if !ok {
		return false, time.Time{}
	}
	base := created
	if last != nil && !last.IsZero() {
		base = *last
	}
	if base.IsZero() {
		return false, time.Time{}
	}
	return true, base.Add(interval)
}

// dashboardStatPercent derives progress only from reported counts.
func dashboardStatPercent(latest *connectors.SyncJob) (bool, int) {
	if latest == nil {
		return false, 0
	}
	processed, okProcessed := latest.Stats["processed"].(float64)
	total, okTotal := latest.Stats["total"].(float64)
	if okProcessed && okTotal && total > 0 && processed >= 0 && processed <= total {
		return true, int(processed * 100 / total)
	}
	return false, 0
}

// dashboardStatItems reads an "items" count from the latest sync job's stats
// when one is reported; absent counts render "—", never a zero.
func dashboardStatItems(latest *connectors.SyncJob) (bool, int64) {
	if latest == nil {
		return false, 0
	}
	if v, ok := latest.Stats["items"].(float64); ok && v >= 0 {
		return true, int64(v)
	}
	return false, 0
}

// dashboardSourceScope summarizes the per-provider scope from real config
// keys (§6.8 "scope" column). Unknown shapes render "—".
func dashboardSourceScope(src connectors.Source) string {
	cfg := src.Config
	str := func(key string) string { return strings.TrimSpace(dashboardStringFrom(cfg[key])) }
	list := func(key string) []string { return dashboardStringListFrom(cfg[key]) }
	switch src.Type {
	case connectors.SourceTypeSlack:
		scope := "All member channels"
		if ch := str("channel_id"); ch != "" {
			scope = "#" + ch
		}
		if str("reconcile") == "true" {
			scope += " · reconcile"
		}
		return scope
	case connectors.SourceTypeTeams:
		teams := []string{}
		if t := str("team_id"); t != "" {
			teams = append(teams, t)
		}
		teams = append(teams, list("team_ids")...)
		if len(teams) == 0 {
			break
		}
		parts := []string{"Teams " + strings.Join(teams, ", ")}
		if allowed, ok := cfg["channel_ids"].(map[string]any); ok {
			n := 0
			for _, v := range allowed {
				n += len(dashboardStringListFrom(v))
			}
			parts = append(parts, strconv.Itoa(n)+" channels")
		}
		if chats := list("chat_ids"); len(chats) > 0 {
			parts = append(parts, strconv.Itoa(len(chats))+" chats")
		}
		return strings.Join(parts, " · ")
	case connectors.SourceTypeGitHub:
		owner, repo := str("owner"), str("repo")
		if owner == "" && repo == "" {
			break
		}
		ref := str("ref")
		if ref == "" {
			ref = "HEAD"
		}
		scope := owner + "/" + repo + "@" + ref
		if paths := list("paths"); len(paths) > 0 {
			scope += fmt.Sprintf(" · %d paths", len(paths))
		}
		return scope
	case connectors.SourceTypeNotion:
		pages, _ := cfg["selected_pages"].([]any)
		if len(pages) == 0 {
			break
		}
		return fmt.Sprintf("%d pages", len(pages))
	case connectors.SourceTypeGoogleDrive:
		if folder := str("folder_id"); folder != "" {
			scope := "Folder " + folder
			if drive := firstNonEmptyStr(str("shared_drive_id"), str("drive_id")); drive != "" {
				scope += " · drive " + drive
			}
			return scope
		}
	}
	return "—"
}

// dashboardStringFrom / dashboardStringListFrom read values back out of the
// JSON-decoded config map (the connectors package keeps its coercers
// unexported).
func dashboardStringFrom(v any) string {
	s, _ := v.(string)
	return s
}

func dashboardStringListFrom(v any) []string {
	raw, ok := v.([]any)
	if !ok {
		return nil
	}
	out := make([]string, 0, len(raw))
	for _, item := range raw {
		if s, ok := item.(string); ok && strings.TrimSpace(s) != "" {
			out = append(out, strings.TrimSpace(s))
		}
	}
	return out
}

func firstNonEmptyStr(values ...string) string {
	for _, v := range values {
		if strings.TrimSpace(v) != "" {
			return v
		}
	}
	return ""
}

// dashboardIconPlus keeps the header action's SVG inline without importing
// the icon set into Go string concatenation beyond the frozen helper.
func dashboardIconPlus() template.HTML { return icon("plus") }
