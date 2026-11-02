// dashboard_knowledge.go — the Knowledge pages (OA-22, plan 09-01):
// /dashboard/knowledge (readable collections, §3.4 member column split),
// /dashboard/knowledge/{id} with Documents/Sources/Search/Settings tabs and
// /dashboard/knowledge/profiles (read-only + the document-preview tool).
// D-17: handlers render only — every write goes through Enclava.api against
// /api/v1 from static/pages/knowledge.js. The Search tab consumes 10-03's
// shared fragment scoped by ?collection= (D-24: no second result renderer).
package httpx

import (
	"context"
	"database/sql"
	"errors"
	"html/template"
	"log"
	"net/http"
	"net/url"
	"sort"
	"strconv"
	"strings"
	"time"

	"enclava-go/internal/auth"
	"enclava-go/internal/connectors"
	"enclava-go/internal/inference"
	"enclava-go/internal/rag"
	"enclava-go/internal/source"
)

// dashboardKnowledgeLimit bounds the per-collection document table and the
// per-row count read the same way the sources table bounds itself.
const dashboardKnowledgeLimit = 100

// knowledgeListData is the list-page view model; EmbeddingModels and the
// vector-size fields feed the New-collection drawer (managers only ever see
// the control).
type knowledgeListData struct {
	Header                     dashboardHeader
	Rows                       []knowledgeCollectionRow
	EmbeddingModels            []string
	DefaultEmbeddingModel      string
	DefaultEmbeddingDimensions int
	DimensionChoices           []int
	Empty                      dashboardEmpty
}

func (d knowledgeListData) DashboardTitle() string  { return "Knowledge" }
func (d knowledgeListData) DashboardModule() string { return "knowledge" }

// knowledgeCollectionRow is one list row: member columns are Collection,
// Documents and Updated; the manager-only projections (Health, Sources,
// Access) render strictly behind .Can.KnowledgeManage.
type knowledgeCollectionRow struct {
	Collection rag.Collection
	DocCount   int
	DocMore    bool
	LastUpdate time.Time
	Health     string // statusPill state: healthy|syncing|warning
	Sources    int
	Access     string
}

// knowledgeCollectionData is the collection page AND tab-fragment model.
type knowledgeCollectionData struct {
	Header           dashboardHeader
	Collection       rag.Collection
	Tabs             []dashboardTab
	Tab              string // documents|sources|search|settings
	Status           string // active document filter bucket
	Docs             []rag.Document
	DocMore          bool
	AnyProcessing    bool
	ReembedPending   int64
	ReembedTotal     int64
	Empty            dashboardEmpty
	SourcesEmpty     dashboardEmpty
	Sources          []connectors.Source
	EmbeddingModels  []string
	DimensionChoices []int
	Everyone         bool // audience contains "*" — the settings access select
}

func (d knowledgeCollectionData) DashboardTitle() string  { return d.Collection.Name }
func (d knowledgeCollectionData) DashboardModule() string { return "knowledge" }

// knowledgeProfilesData is the read-only profiles + preview-tool page.
type knowledgeProfilesData struct {
	Header dashboardHeader
	Empty  dashboardEmpty
}

func (d knowledgeProfilesData) DashboardTitle() string  { return "Processing profiles" }
func (d knowledgeProfilesData) DashboardModule() string { return "knowledge" }

func init() { registerDashboardPage((*App).registerDashboardKnowledgeRoutes) }

func (a *App) registerDashboardKnowledgeRoutes() {
	a.mux.HandleFunc("GET /dashboard/knowledge", a.sessionAuth.requireCap(capSearch, a.handleDashboardKnowledgePage))
	a.mux.HandleFunc("GET /dashboard/knowledge/profiles", a.sessionAuth.requireCap(capKnowledgeManage, a.handleDashboardKnowledgeProfiles))
	a.mux.HandleFunc("GET /dashboard/knowledge/{id}", a.sessionAuth.requireCap(capSearch, a.handleDashboardKnowledgeCollection))
	a.mux.HandleFunc("GET /dashboard/knowledge/{id}/{tab}", a.sessionAuth.requireCap(capSearch, a.handleDashboardKnowledgeCollection))
	a.mux.HandleFunc("GET /dashboard/partials/knowledge/collections", a.sessionAuth.requireCap(capSearch, a.handleDashboardKnowledgeCollections))
	a.mux.HandleFunc("GET /dashboard/partials/knowledge/documents", a.sessionAuth.requireCap(capSearch, a.handleDashboardKnowledgeDocuments))
	a.mux.HandleFunc("GET /dashboard/partials/knowledge/sources", a.sessionAuth.requireCap(capSourcesManage, a.handleDashboardKnowledgeSources))
	a.mux.HandleFunc("GET /dashboard/partials/knowledge/profiles", a.sessionAuth.requireCap(capKnowledgeManage, a.handleDashboardKnowledgeProfilesPartial))
}

func (a *App) handleDashboardKnowledgePage(w http.ResponseWriter, r *http.Request) {
	if a.ragSvc == nil {
		a.sessionAuth.renderDashboardNotFound(w, r)
		return
	}
	p, _ := auth.PrincipalFromContext(r.Context())
	caps, _ := capsFromContext(r.Context())
	data, ok := a.knowledgeListView(w, r, p, caps)
	if !ok {
		return
	}
	a.renderDashboard(w, r, "knowledge", data)
}

func (a *App) handleDashboardKnowledgeCollections(w http.ResponseWriter, r *http.Request) {
	p, _ := auth.PrincipalFromContext(r.Context())
	caps, _ := capsFromContext(r.Context())
	data, ok := a.knowledgeListView(w, r, p, caps)
	if !ok {
		return
	}
	a.renderDashboardFragment(w, r, "knowledge", "knowledge-collections", data)
}

// knowledgeListView reads the caller's readable collections through the same
// service call the API wraps — no parallel authorization. Manager columns
// (health, sources count, access) are only computed for managers; member
// markup carries neither the cells nor the New-collection control.
func (a *App) knowledgeListView(w http.ResponseWriter, r *http.Request, p auth.Principal, caps dashboardCaps) (knowledgeListData, bool) {
	ctx := r.Context()
	collections, err := a.ragSvc.ListCollections(ctx, p)
	if err != nil {
		log.Printf("dashboard knowledge collections: %v", err)
		http.Error(w, "internal server error", http.StatusInternalServerError)
		return knowledgeListData{}, false
	}
	data := knowledgeListData{
		Header: dashboardHeader{
			Crumb: "Knowledge",
			Title: "Knowledge",
			Desc:  "Collections of documents your assistants can search and cite.",
		},
		Empty: dashboardEmpty{Icon: "knowledge", Text: "No collections you can read yet."},
	}
	if caps.KnowledgeManage {
		data.Empty = dashboardEmpty{Icon: "knowledge", Text: "No collections yet.", ActionLabel: "New collection", ActionHref: "/dashboard/knowledge?new=1"}
		data.Header.Actions = template.HTML(`<button type="button" class="btn primary" data-action="knowledge:new">` +
			string(dashboardIconPlus()) + `New collection</button>`)
		data.EmbeddingModels = a.knowledgeEmbeddingModels(ctx, p.TenantID)
		data.DefaultEmbeddingModel, data.DefaultEmbeddingDimensions = a.ragSvc.DefaultEmbedding()
		data.DimensionChoices = knowledgeDimensionChoices(0)
	}
	// Sources are read once and bucketed per target collection; the count is
	// a manager column, so members never pay for it (or see it).
	sourcesByCollection := map[string]int{}
	if caps.KnowledgeManage && a.connectorSvc != nil {
		if srcs, err := a.connectorSvc.ListSources(ctx, p, "", dashboardSourcesLimit, 0); err == nil {
			for _, s := range srcs {
				if s.TargetCollectionID != "" {
					sourcesByCollection[s.TargetCollectionID]++
				}
			}
		} else {
			log.Printf("dashboard knowledge sources: %v", err)
		}
	}
	for _, c := range collections {
		row := knowledgeCollectionRow{Collection: c, LastUpdate: c.UpdatedAt, Access: knowledgeAccessLabel(c)}
		docs, derr := a.ragSvc.ListDocuments(ctx, p, c.ID, rag.DocumentStatusAll, dashboardKnowledgeLimit+1, 0)
		if derr != nil {
			log.Printf("dashboard knowledge documents %s: %v", c.ID, derr)
			http.Error(w, "internal server error", http.StatusInternalServerError)
			return knowledgeListData{}, false
		}
		row.DocMore = len(docs) > dashboardKnowledgeLimit
		if row.DocMore {
			docs = docs[:dashboardKnowledgeLimit]
		}
		row.DocCount = len(docs)
		row.Health = "healthy"
		for _, d := range docs {
			if d.UpdatedAt.After(row.LastUpdate) {
				row.LastUpdate = d.UpdatedAt
			}
			switch d.Status {
			case rag.DocumentStatusFailed:
				row.Health = "warning"
			case rag.DocumentStatusProcessing:
				if row.Health != "warning" {
					row.Health = "syncing"
				}
			}
		}
		row.Sources = sourcesByCollection[c.ID]
		data.Rows = append(data.Rows, row)
	}
	return data, true
}

// handleDashboardKnowledgeCollection serves /dashboard/knowledge/{id} and the
// /{tab} subroutes. The sources and settings tabs carry their own capability
// gates — members get the designed 403 page there, and the tab is never
// offered in the member tab bar.
func (a *App) handleDashboardKnowledgeCollection(w http.ResponseWriter, r *http.Request) {
	if a.ragSvc == nil {
		a.sessionAuth.renderDashboardNotFound(w, r)
		return
	}
	p, _ := auth.PrincipalFromContext(r.Context())
	caps, _ := capsFromContext(r.Context())
	tab := r.PathValue("tab")
	switch tab {
	case "", "documents":
		tab = "documents"
	case "sources":
		if !caps.SourcesManage {
			a.sessionAuth.renderDashboardForbidden(w, r)
			return
		}
	case "search":
	case "settings":
		if !caps.KnowledgeManage {
			a.sessionAuth.renderDashboardForbidden(w, r)
			return
		}
	default:
		a.sessionAuth.renderDashboardNotFound(w, r)
		return
	}
	c, ok := a.knowledgeCollection(w, r, p)
	if !ok {
		return
	}
	data := knowledgeCollectionData{
		Header: dashboardHeader{
			Crumb: "Knowledge",
			Title: c.Name,
			Desc:  c.Description,
		},
		Collection: c,
		Tab:        tab,
		Tabs:       knowledgeTabs(c.ID, tab, caps),
		Everyone:   knowledgeAccessLabel(c) == "Everyone",
	}
	data.Empty = knowledgeDocumentsEmpty(knowledgeDocumentStatus(r.URL.Query().Get("status")), caps)
	data.SourcesEmpty = knowledgeSourcesEmpty(c.ID, caps)
	ctx := r.Context()
	pending, total, err := a.ragSvc.ReembedProgress(ctx, p, c.ID)
	if err != nil && !errors.Is(err, source.ErrDenied) {
		log.Printf("dashboard knowledge re-embed progress: %v", err)
		http.Error(w, "internal server error", http.StatusInternalServerError)
		return
	}
	data.ReembedPending, data.ReembedTotal = pending, total
	switch tab {
	case "documents":
		status := knowledgeDocumentStatus(r.URL.Query().Get("status"))
		data.Status = status
		docs, more, ok := a.knowledgeDocuments(w, ctx, p, c.ID, status)
		if !ok {
			return
		}
		data.Docs, data.DocMore = docs, more
		data.AnyProcessing = knowledgeAnyProcessing(docs)
	case "sources":
		data.Sources = a.knowledgeCollectionSources(ctx, p, c.ID)
	case "settings":
		data.EmbeddingModels = a.knowledgeEmbeddingModels(ctx, p.TenantID)
		data.DimensionChoices = knowledgeDimensionChoices(c.EmbeddingDimensions)
	}
	a.renderDashboard(w, r, "knowledge_collection", data)
}

// knowledgeCollection resolves {id} through the service's readable gate; a
// denied or foreign id renders the 404 page — same public outcome as absent.
func (a *App) knowledgeCollection(w http.ResponseWriter, r *http.Request, p auth.Principal) (rag.Collection, bool) {
	c, err := a.ragSvc.GetCollection(r.Context(), p, r.PathValue("id"))
	if err != nil {
		if errors.Is(err, sql.ErrNoRows) || errors.Is(err, source.ErrDenied) {
			a.sessionAuth.renderDashboardNotFound(w, r)
			return rag.Collection{}, false
		}
		log.Printf("dashboard knowledge collection: %v", err)
		http.Error(w, "internal server error", http.StatusInternalServerError)
		return rag.Collection{}, false
	}
	return c, true
}

// knowledgeTabs is the §3.4 tab bar: Documents and Search for every reader,
// Sources gated on sources.manage and Settings on knowledge.manage.
func knowledgeTabs(collectionID, active string, caps dashboardCaps) []dashboardTab {
	base := "/dashboard/knowledge/" + collectionID
	tabs := []dashboardTab{
		{Label: "Documents", Href: base + "/documents", Active: active == "documents"},
	}
	if caps.SourcesManage {
		tabs = append(tabs, dashboardTab{Label: "Sources", Href: base + "/sources", Active: active == "sources"})
	}
	tabs = append(tabs, dashboardTab{Label: "Search", Href: base + "/search", Active: active == "search"})
	if caps.KnowledgeManage {
		tabs = append(tabs, dashboardTab{Label: "Settings", Href: base + "/settings", Active: active == "settings"})
	}
	return tabs
}

// knowledgeDocumentStatus whitelists the §6.7 filter chips; unknown/absent
// falls back to all (same contract the API enforces).

func knowledgeDocumentStatus(raw string) string {
	switch strings.ToLower(strings.TrimSpace(raw)) {
	case rag.DocumentStatusReady, rag.DocumentStatusProcessing,
		rag.DocumentStatusFailed, rag.DocumentStatusRemoved:
		return strings.ToLower(strings.TrimSpace(raw))
	default:
		return rag.DocumentStatusAll
	}
}

// knowledgeDocuments returns the caller's authorized page plus a truncated
// flag; service authorization happens before any row (or count) reaches the
// markup (OA-21/T-09-01-01).
func (a *App) knowledgeDocuments(w http.ResponseWriter, ctx context.Context, p auth.Principal, collectionID, status string) ([]rag.Document, bool, bool) {
	docs, err := a.ragSvc.ListDocuments(ctx, p, collectionID, status, dashboardKnowledgeLimit+1, 0)
	if err != nil {
		log.Printf("dashboard knowledge documents %s: %v", collectionID, err)
		http.Error(w, "internal server error", http.StatusInternalServerError)
		return nil, false, false
	}
	more := len(docs) > dashboardKnowledgeLimit
	if more {
		docs = docs[:dashboardKnowledgeLimit]
	}
	return docs, more, true
}

// knowledgeCollectionSources is the client-side filter the plan specifies:
// sources whose target collection is this id, read through the authorized
// connector listing.
func (a *App) knowledgeCollectionSources(ctx context.Context, p auth.Principal, collectionID string) []connectors.Source {
	if a.connectorSvc == nil {
		return nil
	}
	srcs, err := a.connectorSvc.ListSources(ctx, p, "", dashboardSourcesLimit, 0)
	if err != nil {
		log.Printf("dashboard knowledge sources %s: %v", collectionID, err)
		return nil
	}
	out := make([]connectors.Source, 0, len(srcs))
	for _, s := range srcs {
		if s.TargetCollectionID == collectionID {
			out = append(out, s)
		}
	}
	return out
}

// knowledgeAnyProcessing reports whether any row is non-terminal — the only
// condition under which the fragment carries the 3s poll trigger.
func knowledgeAnyProcessing(docs []rag.Document) bool {
	for _, d := range docs {
		if dashboardNonTerminal(d.Status) {
			return true
		}
	}
	return false
}

func knowledgeDocumentsEmpty(status string, caps dashboardCaps) dashboardEmpty {
	empty := dashboardEmpty{Icon: "knowledge", Text: "No documents match this filter."}
	if status == rag.DocumentStatusAll && caps.KnowledgeManage {
		empty.Text = "No documents yet."
		empty.ActionLabel, empty.ActionHref = "Upload document", "#knowledge-upload-file"
	}
	return empty
}

func knowledgeSourcesEmpty(collectionID string, caps dashboardCaps) dashboardEmpty {
	empty := dashboardEmpty{Icon: "sources", Text: "No sources feeding this collection."}
	if caps.SourcesManage {
		empty.ActionLabel = "Add source"
		empty.ActionHref = "/dashboard/sources?new=1&collection=" + url.QueryEscape(collectionID)
	}
	return empty
}

// knowledgeAccessLabel summarizes the collection's read audience in plain
// words: "*" → Everyone, a counted member list → "N members", otherwise the
// owner-only default the service enforces.
func knowledgeAccessLabel(c rag.Collection) string {
	for _, aud := range c.Audience {
		if aud == "*" {
			return "Everyone"
		}
	}
	if len(c.Audience) > 0 {
		return strconv.Itoa(len(c.Audience)) + " " + plural(len(c.Audience), "member", "members")
	}
	return "Owner only"
}

// knowledgeEmbeddingModels lists the model aliases bound on enabled
// embeddings-capable endpoints — the same configuration-truth read the
// extract model list performs (never an upstream probe).
func (a *App) knowledgeEmbeddingModels(ctx context.Context, tenantID string) []string {
	if a.endpoints == nil {
		return nil
	}
	endpoints, err := a.endpoints.ListCapabilities(ctx, tenantID, inference.CapabilityEmbeddings)
	if err != nil {
		log.Printf("dashboard knowledge embedding models: %v", err)
		return nil
	}
	seen := map[string]struct{}{}
	var models []string
	for _, e := range endpoints {
		if !e.Enabled {
			continue
		}
		for _, cap := range e.Capabilities {
			_, alias, _, isModel := modelsBindingParts(cap)
			if !isModel || alias == "" {
				continue
			}
			if _, dup := seen[alias]; !dup {
				seen[alias] = struct{}{}
				models = append(models, alias)
			}
		}
	}
	sort.Strings(models)
	return models
}

// knowledgeDimensionChoices is the vector-size menu, always carrying the
// current size so a settings save never silently changes it.
func knowledgeDimensionChoices(current int) []int {
	choices := []int{2048, 1536, 1024, 768, 512, 256}
	for _, n := range choices {
		if n == current {
			return choices
		}
	}
	if current > 0 {
		choices = append(choices, current)
		sort.Sort(sort.Reverse(sort.IntSlice(choices)))
	}
	return choices
}

// --- tab fragments ---------------------------------------------------------

func (a *App) handleDashboardKnowledgeDocuments(w http.ResponseWriter, r *http.Request) {
	p, _ := auth.PrincipalFromContext(r.Context())
	caps, _ := capsFromContext(r.Context())
	collectionID := strings.TrimSpace(r.URL.Query().Get("collection"))
	status := knowledgeDocumentStatus(r.URL.Query().Get("status"))
	docs, more, ok := a.knowledgeDocuments(w, r.Context(), p, collectionID, status)
	if !ok {
		return
	}
	pending, total, err := a.ragSvc.ReembedProgress(r.Context(), p, collectionID)
	if err != nil && !errors.Is(err, source.ErrDenied) {
		log.Printf("dashboard knowledge re-embed progress: %v", err)
		http.Error(w, "internal server error", http.StatusInternalServerError)
		return
	}
	data := knowledgeCollectionData{
		Collection:     rag.Collection{ID: collectionID},
		Status:         status,
		Docs:           docs,
		DocMore:        more,
		AnyProcessing:  knowledgeAnyProcessing(docs),
		ReembedPending: pending,
		ReembedTotal:   total,
		Empty:          knowledgeDocumentsEmpty(status, caps),
	}
	a.renderDashboardFragment(w, r, "knowledge_collection", "knowledge-documents", data)
}

func (a *App) handleDashboardKnowledgeSources(w http.ResponseWriter, r *http.Request) {
	p, _ := auth.PrincipalFromContext(r.Context())
	caps, _ := capsFromContext(r.Context())
	collectionID := strings.TrimSpace(r.URL.Query().Get("collection"))
	data := knowledgeCollectionData{
		Collection:   rag.Collection{ID: collectionID},
		Sources:      a.knowledgeCollectionSources(r.Context(), p, collectionID),
		SourcesEmpty: knowledgeSourcesEmpty(collectionID, caps),
	}
	a.renderDashboardFragment(w, r, "knowledge_collection", "knowledge-sources", data)
}

// --- profiles page ---------------------------------------------------------

func (a *App) handleDashboardKnowledgeProfiles(w http.ResponseWriter, r *http.Request) {
	data := knowledgeProfilesData{
		Header: dashboardHeader{
			Crumb: "Knowledge",
			Title: "Processing profiles",
			Desc:  "Immutable chunking profiles collections process documents with — read-only here; the preview tool below shows exactly what a profile does to a document.",
		},
		Empty: dashboardEmpty{Icon: "knowledge", Text: "No processing profiles."},
	}
	a.renderDashboard(w, r, "knowledge_profiles", data)
}

func (a *App) handleDashboardKnowledgeProfilesPartial(w http.ResponseWriter, r *http.Request) {
	a.renderDashboardFragment(w, r, "knowledge_profiles", "knowledge-profiles", knowledgeProfilesData{Empty: dashboardEmpty{Icon: "knowledge", Text: "No processing profiles."}})
}
