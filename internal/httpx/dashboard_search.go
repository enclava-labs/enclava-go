// dashboard_search.go — the Search page (OA-33, plan 10-03): cross-collection
// readable search with URL state, a mode segmented control, post-authorization
// source-kind facets and mark-highlighted snippets. The results fragment
// (GET /dashboard/partials/search/results + partials/search_results.tmpl) is
// the SOLE §6.4 result renderer — the page embeds it and 09-01's Knowledge
// Search tab consumes the same fragment scoped via collection= (D-24: no
// second renderer, no per-collection POST fallback). Ask-about-this hands
// only the evidence id to Chat via ?evidence= — re-authorized at StartRun.
// Routes self-register; the legacy rag surface rides 10-01's teardown commit.
package httpx

import (
	"context"
	"crypto/sha256"
	"encoding/hex"
	"errors"
	"log"
	"net/http"
	"strings"

	"enclava-go/internal/auth"
	"enclava-go/internal/rag"
	"enclava-go/internal/source"
)

func init() { registerDashboardPage((*App).registerDashboardSearchRoutes) }

// registerDashboardSearchRoutes mounts the search page and the shared results
// fragment behind the `rag` capability (D-19). GET /dashboard/search/extra
// falls through to the designed 404 rather than the legacy catch-all.
func (a *App) registerDashboardSearchRoutes() {
	a.mux.HandleFunc("GET /dashboard/search", a.sessionAuth.requireCap(capSearch, a.handleDashboardSearchPage))
	a.mux.HandleFunc("GET /dashboard/search/", a.sessionAuth.requireCap(capSearch, a.sessionAuth.renderDashboardNotFound))
	a.mux.HandleFunc("GET /dashboard/partials/search/results", a.sessionAuth.requireCap(capSearch, a.handleDashboardSearchResults))
}

// searchViewData is the page AND fragment model — one type backs both so the
// embedded page render and the htmx fragment emit identical markup. RecentsKey
// is an opaque per-tenant+actor bucket the page module reads to namespace
// localStorage recents (a different browser user never shares the bucket).
type searchViewData struct {
	Query       string
	Mode        string // canonical hybrid|lexical|semantic — always whitelisted
	Collection  string // resolved only against the caller's readable set
	RecentsKey  string
	Results     *searchResultsData // nil while the query is empty
	Collections []rag.Collection   // readable list for filter + empty-state cards
	// Shared empty states (sentence-only on this surface — no create action).
	ResultsEmpty     dashboardEmpty
	CollectionsEmpty dashboardEmpty
}

func (d searchViewData) DashboardTitle() string  { return "Search" }
func (d searchViewData) DashboardModule() string { return "search" }

// searchResultsData is the fused, post-authorization result set the fragment
// renders. Error carries a bounded-service outcome (work bound exceeded);
// other service failures are 500s, never silent empty states.
type searchResultsData struct {
	Query      string
	Rows       []searchResultRow
	Facets     []rag.SourceFacet
	NextCursor string // opaque continuation token for the "More results" control
	Error      string
	Hint       string // secondary line under Error (e.g. lexical still works)
}

// searchResultRow is one fused result projected for the template: label,
// location line, snippet and the evidence id — the ONLY thing Ask-about-this
// carries across to Chat (passage text never crosses client-side, D-17).
type searchResultRow struct {
	Title       string
	TitleHref   string // safe deep link for the title (Link → SourceURL → SourceURI)
	Collection  string
	Location    string
	Author      string
	Container   string
	Stamp       string // RFC3339 source timestamp (updated, else created); "" when absent
	Content     string
	SourceKind  string
	EvidenceID  string
	SourceHref  string // secondary container link, only when it differs from TitleHref
	SourceLabel string // "Open thread"/"Open issue"/"Open page"/"Open in <provider>"
}

// searchResultRowFor projects one service result into its row: title and
// location stay text-only, and every outbound href passes safeSourceLink so
// only http(s) or same-origin paths ever reach the template.
func searchResultRowFor(res rag.SearchResult) searchResultRow {
	title := strings.TrimSpace(res.FileName)
	if title == "" {
		title = "Untitled document" // raw ids are never primary labels (OA-34)
	}
	collection := res.CollectionName
	if collection == "" {
		collection = "Collection"
	}
	row := searchResultRow{
		Title:      title,
		Collection: collection,
		Location:   evidenceLocationLine(res.Locations),
		Author:     strings.TrimSpace(res.Author),
		Content:    res.Content,
		SourceKind: res.SourceKind,
		EvidenceID: res.EvidenceID,
	}
	// Uploads name the collection as their container — repeating it in the
	// meta line reads as a duplicate, so it is dropped (case/trim-insensitive).
	if container := strings.TrimSpace(res.Container); container != "" && !strings.EqualFold(container, collection) {
		row.Container = container
	}
	// The title opens the chunk's deep link first, then the document URL,
	// then the legacy source URI — the first one that survives the gate.
	row.TitleHref = firstSafeSourceLink(res.Link, res.SourceURL, res.SourceURI)
	// The container-level link earns its own action only when it lands
	// somewhere different from the title's target.
	if secondary := safeSourceLink(res.SourceURL); secondary != "" && secondary != row.TitleHref {
		row.SourceHref = secondary
		row.SourceLabel = sourceContainerLinkLabel(res.SourceKind, sourceProviderLabel(res.SourceKind))
	}
	// Updated wins over created; either must parse as a timestamp before it
	// reaches relTime (which would render a bare "—" for junk).
	for _, stamp := range []string{res.SourceUpdatedAt, res.SourceCreatedAt} {
		if _, ok := dashboardAsTime(strings.TrimSpace(stamp)); ok {
			row.Stamp = strings.TrimSpace(stamp)
			break
		}
	}
	return row
}

// looksLikeUserID reports whether an author value is an internal user id
// (user_<hex>, as issued by the identity store) rather than a human name
// carried through from a connector.
func looksLikeUserID(author string) bool {
	rest, ok := strings.CutPrefix(author, "user_")
	if !ok || rest == "" {
		return false
	}
	for _, r := range rest {
		if !(r >= '0' && r <= '9') && !(r >= 'a' && r <= 'f') && !(r >= 'A' && r <= 'F') {
			return false
		}
	}
	return true
}

// resolveSearchAuthors swaps internal user ids in row authors for the person's
// display name — one tenant lookup per result page, never one query per row.
// An id that resolves to nothing is omitted: raw internal ids are never
// primary labels (OA-34). Human names from connectors pass through untouched.
func (a *App) resolveSearchAuthors(ctx context.Context, tenantID string, rows []searchResultRow) {
	ids := map[string]struct{}{}
	for _, row := range rows {
		if looksLikeUserID(row.Author) {
			ids[row.Author] = struct{}{}
		}
	}
	if len(ids) == 0 {
		return
	}
	names := map[string]string{}
	if a.identity != nil {
		if users, err := a.identity.ListUsers(ctx, tenantID); err == nil {
			for _, u := range users {
				label := strings.TrimSpace(u.DisplayName)
				if label == "" {
					label = strings.TrimSpace(u.Email)
				}
				if label != "" {
					names[u.ID] = label
				}
			}
		} else {
			log.Printf("dashboard search author lookup: %v", err)
		}
	}
	for i := range rows {
		if looksLikeUserID(rows[i].Author) {
			rows[i].Author = names[rows[i].Author] // unresolvable → omitted
		}
	}
}

// firstSafeSourceLink returns the first candidate that survives the
// scheme/relative-path gate, "" when none does.
func firstSafeSourceLink(candidates ...string) string {
	for _, candidate := range candidates {
		if safe := safeSourceLink(candidate); safe != "" {
			return safe
		}
	}
	return ""
}

// sourceContainerLinkLabel names the secondary link by source kind:
// threads for chat sources, issues for trackers, pages for documents;
// everything else falls back to the provider name.
func sourceContainerLinkLabel(kind, provider string) string {
	switch strings.TrimSpace(kind) {
	case "slack":
		return "Open thread"
	case "github", "gitlab", "jira", "linear":
		return "Open issue"
	case "notion":
		return "Open page"
	}
	if provider != "" {
		return "Open in " + provider
	}
	return "Open in source"
}

// searchMode whitelist: invalid/absent falls back to hybrid (D-19 contract —
// never erroring, never widening).
func searchMode(raw string) string {
	switch strings.TrimSpace(raw) {
	case string(rag.QueryLexical):
		return string(rag.QueryLexical)
	case string(rag.QuerySemantic):
		return string(rag.QuerySemantic)
	default:
		return string(rag.QueryHybrid)
	}
}

// recentsBucket derives the namespacing key for localStorage recents —
// opaque on the page (hash of tenant:actor; raw ids never primary labels).
func recentsBucket(p auth.Principal) string {
	sum := sha256.Sum256([]byte("recents\x00" + p.TenantID + "\x00" + p.ActorID))
	return hex.EncodeToString(sum[:8])
}

// handleDashboardSearchPage renders /dashboard/search — the form plus the
// embedded results fragment so deep links land painted.
func (a *App) handleDashboardSearchPage(w http.ResponseWriter, r *http.Request) {
	if a.ragSvc == nil {
		a.sessionAuth.renderDashboardNotFound(w, r)
		return
	}
	data, ok := a.buildSearchView(w, r)
	if !ok {
		return
	}
	a.renderDashboard(w, r, "search", data)
}

// handleDashboardSearchResults serves the shared fragment — the single result
// renderer (D-24). A foreign ?collection= id produces the same public outcome
// as an empty selection: an honest empty result set, never an error, never a
// silent cross-tenant read.
func (a *App) handleDashboardSearchResults(w http.ResponseWriter, r *http.Request) {
	if a.ragSvc == nil {
		a.sessionAuth.renderDashboardNotFound(w, r)
		return
	}
	data, ok := a.buildSearchView(w, r)
	if !ok {
		return
	}
	a.renderDashboardFragment(w, r, "search", "search-results", data)
}

// buildSearchView resolves ?q=&mode=&collection= server-side and runs the
// read through SearchReadable directly (D-15 — the same authorization/fusion/
// facet contract POST /api/v1/rag/search wraps; never a local substitute).
// false means the response is already committed.
func (a *App) buildSearchView(w http.ResponseWriter, r *http.Request) (searchViewData, bool) {
	p, _ := auth.PrincipalFromContext(r.Context())
	ctx := r.Context()
	q := r.URL.Query()
	view := searchViewData{
		Query:      strings.TrimSpace(q.Get("q")),
		Mode:       searchMode(q.Get("mode")),
		Collection: strings.TrimSpace(q.Get("collection")),
		RecentsKey: recentsBucket(p),
		ResultsEmpty: dashboardEmpty{Icon: "search",
			Text: "Nothing matched your search — try different words or a different collection."},
		CollectionsEmpty: dashboardEmpty{Icon: "knowledge",
			Text: "No collections you can search yet — your administrator grants access."},
	}
	collections, err := a.ragSvc.ListCollections(ctx, p)
	if err != nil {
		log.Printf("dashboard search collections: %v", err)
		http.Error(w, "internal server error", http.StatusInternalServerError)
		return view, false
	}
	view.Collections = collections
	if view.Query == "" {
		return view, true // empty query → recents + readable-collection cards
	}
	// collection= is intersected with the readable set before the service is
	// even asked: a foreign/denied id is indistinguishable from an empty set.
	var selected []string
	if view.Collection != "" {
		for _, c := range collections {
			if c.ID == view.Collection {
				selected = []string{c.ID}
				break
			}
		}
		if len(selected) == 0 {
			view.Results = &searchResultsData{Query: view.Query}
			return view, true
		}
	}
	out, err := a.ragSvc.SearchReadableWithOptions(ctx, p, view.Query, rag.QueryMode(view.Mode), selected, 0, rag.SearchReadableOptions{Cursor: q.Get("cursor")})
	if err != nil {
		switch {
		case errors.Is(err, rag.ErrSearchWorkExceeded):
			view.Results = &searchResultsData{Query: view.Query,
				Error: "That search covered more documents than the allowed scope — narrow the collection or the query."}
			return view, true
		case errors.Is(err, rag.ErrSearchBindingDenied):
			view.Results = &searchResultsData{Query: view.Query,
				Error: "Semantic search needs an embedding model granted to your role — ask your administrator.",
				Hint:  "Keyword search still works."}
			return view, true
		case errors.Is(err, source.ErrDenied), errors.Is(err, rag.ErrInvalidInput):
			view.Results = &searchResultsData{Query: view.Query}
			return view, true
		default:
			log.Printf("dashboard search %q: %v", view.Query, err)
			http.Error(w, "internal server error", http.StatusInternalServerError)
			return view, false
		}
	}
	results := &searchResultsData{Query: view.Query, Facets: out.Facets, NextCursor: out.NextCursor}
	results.Rows = make([]searchResultRow, 0, len(out.Results))
	for _, res := range out.Results {
		results.Rows = append(results.Rows, searchResultRowFor(res))
	}
	a.resolveSearchAuthors(ctx, p.TenantID, results.Rows)
	view.Results = results
	return view, true
}
