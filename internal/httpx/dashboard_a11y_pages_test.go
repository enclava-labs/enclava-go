// dashboard_a11y_pages_test.go — the OA-38 page-surface audit for plan
// 11-05: every named page and leaf partial renders through the production
// template set on populated fixtures, and the RENDERED DOM is audited for
// the accessible-name contract — controls named (visible .label text counts;
// aria-label where no visible label exists), td[data-label] on every cell,
// toolsteps as native disclosure, link chips navigating with aria-current,
// toggle chips and tiles carrying aria-pressed, named icon-only buttons,
// real <button> data-confirm controls and steppers that stay presentational.
// Source text and attribute presence alone earn no credit — every
// assertion runs on executed output.
package httpx

import (
	"bytes"
	"fmt"
	"strconv"
	"strings"
	"testing"
	"time"

	"golang.org/x/net/html"

	"enclava-go/internal/auth"
	"enclava-go/internal/chatbot"
	"enclava-go/internal/connectors"
	"enclava-go/internal/extract"
	"enclava-go/internal/inference"
	"enclava-go/internal/jobs"
	"enclava-go/internal/prompts"
	"enclava-go/internal/rag"
	"enclava-go/internal/source"
)

// ── rendered-DOM audit core ───────────────────────────────────────────────

func attr(n *html.Node, key string) (string, bool) {
	for _, a := range n.Attr {
		if a.Key == key {
			return a.Val, true
		}
	}
	return "", false
}

func hasClass(n *html.Node, cls string) bool {
	c, ok := attr(n, "class")
	if !ok {
		return false
	}
	for _, part := range strings.Fields(c) {
		if part == cls {
			return true
		}
	}
	return false
}

func textOf(n *html.Node) string {
	var b strings.Builder
	var walk func(*html.Node)
	walk = func(x *html.Node) {
		if x.Type == html.TextNode {
			b.WriteString(x.Data)
		}
		for c := x.FirstChild; c != nil; c = c.NextSibling {
			if c.Type == html.ElementNode && (c.Data == "input" || c.Data == "select" || c.Data == "textarea" || c.Data == "svg") {
				continue // control/svg contents are not the label's text
			}
			walk(c)
		}
	}
	walk(n)
	return strings.TrimSpace(strings.Join(strings.Fields(b.String()), " "))
}

// accessibleName resolves the rendered accessible name of a control: an
// explicit aria-label, a wrapping <label>, a <label for>, aria-labelledby,
// title, or — for buttons — its own text content. An empty name fails the
// audit; placeholder alone never satisfies the contract.
func accessibleName(n *html.Node, labelsFor map[string]string, idText map[string]string) string {
	if v, ok := attr(n, "aria-label"); ok && strings.TrimSpace(v) != "" {
		return strings.TrimSpace(v)
	}
	if v, ok := attr(n, "aria-labelledby"); ok {
		var parts []string
		for _, id := range strings.Fields(v) {
			parts = append(parts, idText[id])
		}
		if s := strings.TrimSpace(strings.Join(parts, " ")); s != "" {
			return s
		}
	}
	// Wrapping label ancestor.
	for p := n.Parent; p != nil; p = p.Parent {
		if p.Type == html.ElementNode && p.Data == "label" {
			if s := textOf(p); s != "" {
				return s
			}
			break
		}
	}
	if id, ok := attr(n, "id"); ok {
		if s, ok2 := labelsFor[id]; ok2 && s != "" {
			return s
		}
	}
	if v, ok := attr(n, "title"); ok && strings.TrimSpace(v) != "" {
		return strings.TrimSpace(v)
	}
	if n.Data == "button" || n.Data == "a" || n.Data == "summary" {
		if s := textOf(n); s != "" {
			return s
		}
	}
	return ""
}

func elName(n *html.Node) string {
	if id, ok := attr(n, "id"); ok && id != "" {
		return n.Data + "#" + id
	}
	if v, ok := attr(n, "name"); ok && v != "" {
		return n.Data + "[name=" + v + "]"
	}
	return n.Data
}

// inRoleGroup reports whether the node sits inside a role="group" container
// (the chip/segmented-control contract applies to the filter groups, never
// to pagination or one-shot action rows).
func inRoleGroup(n *html.Node) bool {
	for p := n.Parent; p != nil; p = p.Parent {
		if p.Type == html.ElementNode {
			if r, ok := attr(p, "role"); ok && r == "group" {
				return true
			}
		}
	}
	return false
}

// a11yAuditHTML parses a rendered fragment/document and returns every
// contract violation found in the DOM.
func a11yAuditHTML(rendered string) []string {
	doc, err := html.Parse(strings.NewReader(rendered))
	if err != nil {
		return []string{"unparseable rendered output: " + err.Error()}
	}
	var nodes []*html.Node
	var walk func(*html.Node)
	walk = func(n *html.Node) {
		if n.Type == html.ElementNode {
			nodes = append(nodes, n)
		}
		for c := n.FirstChild; c != nil; c = c.NextSibling {
			walk(c)
		}
	}
	walk(doc)

	// Label text tables: for-targeted and per-id text (aria-labelledby).
	labelsFor := map[string]string{}
	idText := map[string]string{}
	for _, n := range nodes {
		if n.Data == "label" {
			if forAttr, ok := attr(n, "for"); ok && forAttr != "" {
				if s := textOf(n); s != "" {
					labelsFor[forAttr] = s
				}
			}
		}
		if id, ok := attr(n, "id"); ok && id != "" {
			if s := textOf(n); s != "" {
				idText[id] = s
			}
		}
	}

	var fails []string
	fail := func(format string, args ...any) {
		fails = append(fails, fmt.Sprintf(format, args...))
	}

	for _, n := range nodes {
		switch n.Data {
		case "th":
			if v, _ := attr(n, "scope"); v != "col" && v != "row" {
				fail("th renders without scope: %q", truncate(textOf(n), 40))
			}
		case "input":
			if t, _ := attr(n, "type"); t == "hidden" {
				continue
			}
			if accessibleName(n, labelsFor, idText) == "" {
				fail("input %s (type=%s) renders without an accessible name", elName(n), firstAttr(n, "type"))
			}
			if hasClass(n, "iconbtn") {
				fail("input must not carry iconbtn styling")
			}
		case "select", "textarea":
			if accessibleName(n, labelsFor, idText) == "" {
				fail("%s %s renders without an accessible name", n.Data, elName(n))
			}
		case "button":
			if accessibleName(n, labelsFor, idText) == "" {
				fail("button %s renders without an accessible name", elName(n))
			}
		case "a":
			if _, ok := attr(n, "href"); ok && accessibleName(n, labelsFor, idText) == "" {
				fail("link %s renders without an accessible name", firstAttr(n, "href"))
			}
		case "td":
			if v, ok := attr(n, "data-label"); !ok || strings.TrimSpace(v) == "" {
				fail("td renders without data-label: %q", truncate(textOf(n), 60))
			}
		case "details":
			// toolstep disclosure is verified below via the class check.
		}

		if hasClass(n, "toolstep") && n.Data != "details" {
			fail(".toolstep renders as <%s>, not native <details> disclosure", n.Data)
		}
		if n.Data == "details" && hasClass(n, "toolstep") {
			found := false
			for c := n.FirstChild; c != nil; c = c.NextSibling {
				if c.Type == html.ElementNode && c.Data == "summary" {
					found = true
				}
			}
			if !found {
				fail(".toolstep <details> renders without a <summary>")
			}
		}
		// Chips that express toggle state inside a group must expose pressed
		// state in both positions. label.chip rows wrap a checkbox — the
		// native checked state IS the toggle semantics. Link chips navigate
		// instead: they mark the active view with aria-current="page" and
		// never carry pressed state.
		if hasClass(n, "chip") && inRoleGroup(n) && n.Data != "label" {
			if n.Data == "a" {
				if _, ok := attr(n, "aria-pressed"); ok {
					fail("a.chip inside role=group carries aria-pressed — links use aria-current")
				}
			} else if _, ok := attr(n, "aria-pressed"); !ok {
				fail("%s.chip inside role=group renders without aria-pressed", n.Data)
			}
		}
		if hasClass(n, "tile") && n.Data == "button" {
			if _, ok := attr(n, "aria-pressed"); !ok {
				fail("button.tile renders without aria-pressed")
			}
		}
		// Steppers stay presentational: dots are spans, never controls.
		if hasClass(n, "step") && (n.Data == "button" || n.Data == "a" || n.Data == "input") {
			fail(".step rendered as interactive <%s> — stepper must stay presentational", n.Data)
		}
		// Two-step destructive confirms are real buttons (keyboard-armed).
		if _, ok := attr(n, "data-confirm"); ok && n.Data != "button" {
			fail("data-confirm rendered on <%s> — must be a real <button>", n.Data)
		}
	}
	// data-label==th verbatim (D-23 card-label contract): each td's
	// data-label must equal its column's th text. Column index tracks
	// colspan so a colspan cell may match any spanned column's th.
	for _, n := range nodes {
		if n.Data != "thead" {
			continue
		}
		var headers []string
		for _, c := range descendants(n) {
			if c.Data == "th" {
				headers = append(headers, textOf(c))
			}
		}
		var table *html.Node
		for p := n.Parent; p != nil && table == nil; p = p.Parent {
			if p.Data == "table" {
				table = p
			}
		}
		if table == nil {
			continue
		}
		for _, c := range descendants(table) {
			if c.Data != "td" {
				continue
			}
			label, _ := attr(c, "data-label")
			col := cellColumn(c)
			span := 1
			if v, ok := attr(c, "colspan"); ok {
				if parsed, err := strconv.Atoi(v); err == nil && parsed > 1 {
					span = parsed
				}
			}
			matched := false
			for k := col; k < col+span && k < len(headers); k++ {
				if headers[k] == label {
					matched = true
				}
			}
			if !matched && col < len(headers) {
				fail("td data-label %q does not match column th %q", label, headers[col])
			}
		}
	}
	return fails
}

// descendants yields every element node under root (root excluded).
func descendants(root *html.Node) []*html.Node {
	var out []*html.Node
	var walk func(*html.Node)
	walk = func(n *html.Node) {
		for c := n.FirstChild; c != nil; c = c.NextSibling {
			if c.Type == html.ElementNode {
				out = append(out, c)
			}
			walk(c)
		}
	}
	walk(root)
	return out
}

// cellColumn returns the zero-based column index of a td/th within its row,
// honoring colspan on the preceding cells.
func cellColumn(n *html.Node) int {
	col := 0
	for s := n.PrevSibling; s != nil; s = s.PrevSibling {
		if s.Type != html.ElementNode || (s.Data != "td" && s.Data != "th") {
			continue
		}
		span := 1
		if v, ok := attr(s, "colspan"); ok {
			if parsed, err := strconv.Atoi(v); err == nil && parsed > 1 {
				span = parsed
			}
		}
		col += span
	}
	return col
}

func firstAttr(n *html.Node, key string) string {
	v, _ := attr(n, key)
	return v
}

func truncate(s string, max int) string {
	if len(s) <= max {
		return s
	}
	return s[:max] + "…"
}

// renderPage executes the page's "content" definition — the same template
// the shell invokes — against a populated view.
func renderPage(t *testing.T, page string, data any, caps dashboardCaps) string {
	t.Helper()
	set, err := dashboardTemplateSet(page)
	if err != nil {
		t.Fatalf("template set %q: %v", page, err)
	}
	var buf bytes.Buffer
	if err := set.ExecuteTemplate(&buf, "content", dashboardView{Data: data, Can: caps, Org: "Acme"}); err != nil {
		t.Fatalf("render content (%s): %v", page, err)
	}
	return buf.String()
}

func auditRendered(t *testing.T, name, rendered string) {
	t.Helper()
	for _, f := range a11yAuditHTML(rendered) {
		t.Errorf("%s: %s", name, f)
	}
}

var adminCaps = dashboardCaps{
	Chat: true, Search: true, KnowledgeManage: true, SourcesManage: true,
	Extract: true, ExtractManage: true, AssistantsManage: true,
	ToolsManage: true, ModelsManage: true, AccessManage: true, BillingView: true,
}

var memberCaps = dashboardCaps{Chat: true, Search: true, Extract: true}

// ── TestA11yWorkPages — Home/Chat/Search/Extract fixtures ────────────────

func TestA11yWorkPages(t *testing.T) {
	now := time.Now()

	home := homeData{
		Running: homeRunningData{
			Rows: []homeRunningRow{
				{ActivityItem: ActivityItem{Status: jobs.StatusRunning, Label: "Sync", UpdatedAt: now}, Live: true},
				{ActivityItem: ActivityItem{Status: jobs.StatusSucceeded, Label: "Extract", UpdatedAt: now}},
			},
			Live:  true,
			Empty: homeRunningEmpty,
		},
		Attention: []AttentionItem{
			{Kind: "sync_running", Text: "Slack sync running", Live: true},
			{Kind: "document", Text: "handbook.pdf failed", FixText: "Extraction failed.", FixLabel: "Open", FixHref: "/dashboard/knowledge/c1/documents"},
		},
		AttentionLive:      true,
		Conversations:      []chatbot.ConversationSummary{{ID: "c1", Title: "Pricing question", UpdatedAt: now}},
		Extractions:        []extract.JobView{{ID: "j1", FileName: "invoice.pdf", Status: "succeeded", CreatedAt: now}},
		Collections:        []rag.Collection{{ID: "c1", Name: "Policies"}},
		AttentionEmpty:     dashboardEmpty{Icon: "check", Text: "Nothing needs attention."},
		ConversationsEmpty: dashboardEmpty{Icon: "chat", Text: "No conversations yet."},
		AssistantsEmpty:    dashboardEmpty{Icon: "assistant", Text: "No assistants yet."},
		ExtractionsEmpty:   dashboardEmpty{Icon: "extract", Text: "No extractions yet."},
		CollectionsEmpty:   dashboardEmpty{Icon: "knowledge", Text: "No collections yet."},
	}
	auditRendered(t, "home page", renderPage(t, "home", home, adminCaps))
	auditRendered(t, "home frag-running", renderFrag(t, "home", "running", home, adminCaps))
	auditRendered(t, "home frag-attention", renderFrag(t, "home", "attention", home, adminCaps))

	thread := &chatThreadData{
		ConversationID: "c1",
		AssistantName:  "Policy assistant",
		Subline:        "gpt-4o @ acme",
		ConfText:       "Confidential",
		Messages: []chatThreadMessage{
			{
				Role: "user", Author: "You", Content: "What is the rate?", CreatedAt: now,
				RunID: "r1", RunStatus: "running", Pending: true,
			},
			{
				Role: "assistant", Author: "Policy assistant",
				Content: "The rate is $5 [1].", CreatedAt: now, EvidAttr: "ev1",
				Steps: []chatbot.AssistantRunStep{{Number: 1, Kind: "search", Name: "search", Status: "succeeded"}},
				RunID: "r0", RunStatus: "succeeded",
			},
		},
		Live: true,
	}
	chat := chatViewData{
		ConversationID:    "c1",
		Assistants:        []chatbot.Instance{{ID: "a1", Name: "Policy assistant"}},
		SelectedAssistant: "a1",
		History: chatHistoryData{Groups: []chatHistoryGroup{
			{Label: "Today", Items: []chatHistoryItem{{ID: "c1", Title: "Pricing", UpdatedAt: now, Selected: true}}},
		}, NextCursor: "cur1"},
		Thread:          thread,
		HistoryEmpty:    dashboardEmpty{Icon: "chat", Text: "No conversations yet."},
		ThreadEmpty:     dashboardEmpty{Icon: "chat", Text: "Ask something to start."},
		AssistantsEmpty: dashboardEmpty{Icon: "assistant", Text: "No assistants granted yet."},
	}
	auditRendered(t, "chat page", renderPage(t, "chat", chat, adminCaps))
	auditRendered(t, "chat frag-chat-thread", renderFrag(t, "chat", "chat-thread", chat, memberCaps))
	auditRendered(t, "chat frag-chat-history", renderFrag(t, "chat", "chat-history", chat, adminCaps))

	evidence := evidenceCardData{
		EvidenceID: "ev1", Status: "current", Collection: "Policies",
		Document: "handbook.pdf", Location: "handbook.pdf · Page 3 · Clause 4.2",
		Quote: "The rate is $5.", OpenHref: "https://example.com/doc", CopyURL: "/dashboard/files/x",
	}
	auditRendered(t, "chat frag-evidence", renderFrag(t, "chat", "evidence", evidence, memberCaps))
	auditRendered(t, "chat frag-evidence unavailable", renderFrag(t, "chat", "evidence", evidenceCardData{EvidenceID: "ev2", Document: "doc", Unavailable: true}, memberCaps))

	search := searchViewData{
		Query: "rate", Mode: "hybrid", RecentsKey: "k1",
		Collections: []rag.Collection{{ID: "c1", Name: "Policies"}},
		Results: &searchResultsData{
			Query: "rate",
			Rows: []searchResultRow{{
				Title: "Policy note", Collection: "Policies", Location: "doc · Page 2",
				Content: "rate details", SourceKind: "upload", EvidenceID: "ev1",
			}},
			Facets: []rag.SourceFacet{{SourceKind: "upload", Count: 1}},
		},
		ResultsEmpty:     dashboardEmpty{Icon: "search", Text: "No matches."},
		CollectionsEmpty: dashboardEmpty{Icon: "knowledge", Text: "No collections yet."},
	}
	auditRendered(t, "search page", renderPage(t, "search", search, memberCaps))
	auditRendered(t, "search frag-search-results", renderFrag(t, "search", "search-results", search, memberCaps))

	job := extract.JobView{ID: "j1", Status: "running", FileName: "invoice.pdf", CreatedAt: now}
	done := extract.JobView{ID: "j2", Status: "succeeded", FileName: "receipt.pdf", CreatedAt: now, ResultRef: "r1"}
	extractTab := func(tab string) extractView {
		return extractView{
			Tabs: []dashboardTab{
				{Label: "Process", Href: "/dashboard/extract", Active: tab == "process"},
				{Label: "Jobs", Href: "/dashboard/extract/jobs", Active: tab == "jobs"},
				{Label: "Templates", Href: "/dashboard/extract/templates", Active: tab == "templates"},
			},
			Tab:            tab,
			Templates:      []extract.Template{{ID: "t1", Name: "Invoice fields", IsDefault: true}},
			Jobs:           []extract.JobView{job, done},
			Live:           true,
			Manage:         []extract.Template{{ID: "t1", Name: "Invoice fields", Enabled: true, IsDefault: true}},
			Settings:       extract.Settings{DefaultModel: "gpt-4o", MaxFileSizeMB: 25},
			Models:         []string{"gpt-4o"},
			MaxUploadMB:    25,
			Job:            &done,
			Result:         &extract.ResultView{JobID: "j2", Generated: true},
			Fields:         []extractFieldRow{{Name: "total", Value: "$12.00", Location: "page 1"}},
			Writable:       []extractCollectionOption{{ID: "c1", Name: "Policies"}},
			ProcessEmpty:   dashboardEmpty{Icon: "extract", Text: "No templates."},
			JobsEmpty:      dashboardEmpty{Icon: "extract", Text: "No jobs yet."},
			TemplatesEmpty: dashboardEmpty{Icon: "extract", Text: "No templates yet."},
			FieldsEmpty:    dashboardEmpty{Icon: "extract", Text: "No fields extracted."},
		}
	}
	auditRendered(t, "extract page process", renderPage(t, "extract", extractTab("process"), adminCaps))
	auditRendered(t, "extract page jobs", renderPage(t, "extract", extractTab("jobs"), adminCaps))
	auditRendered(t, "extract page templates", renderPage(t, "extract", extractTab("templates"), adminCaps))
	auditRendered(t, "extract frag-extract-jobs", renderFrag(t, "extract", "extract-jobs", extractTab("jobs"), memberCaps))
	auditRendered(t, "extract frag-extract-result", renderFrag(t, "extract", "extract-result", extractTab("jobs"), memberCaps))
	auditRendered(t, "extract frag-extract-templates", renderFrag(t, "extract", "extract-templates", extractTab("templates"), adminCaps))
}

// ── TestA11yAdminPages — Build/Admin/Account/auth fixtures ───────────────

func TestA11yAdminPages(t *testing.T) {
	now := time.Now()
	col := rag.Collection{ID: "c1", Name: "Policies", EmbeddingModel: "embed-1"}

	knowledge := knowledgeListData{
		Header:          dashboardHeader{Title: "Knowledge"},
		Rows:            []knowledgeCollectionRow{{Collection: col, DocCount: 3, LastUpdate: now, Health: "healthy"}},
		EmbeddingModels: []string{"embed-1"},
		Empty:           dashboardEmpty{Icon: "knowledge", Text: "No collections yet."},
	}
	auditRendered(t, "knowledge page", renderPage(t, "knowledge", knowledge, adminCaps))
	auditRendered(t, "knowledge frag-knowledge-collections", renderFrag(t, "knowledge", "knowledge-collections", knowledge, adminCaps))

	collection := knowledgeCollectionData{
		Header:     dashboardHeader{Title: "Policies"},
		Collection: col,
		Tabs: []dashboardTab{
			{Label: "Documents", Href: "/dashboard/knowledge/c1/documents", Active: true},
			{Label: "Sources", Href: "/dashboard/knowledge/c1/sources"},
		},
		Tab:    "documents",
		Status: "all",
		Docs: []rag.Document{
			{ID: "d1", FileName: "handbook.pdf", Status: "ready", UpdatedAt: now, CollectionID: "c1"},
			{ID: "d2", FileName: "broken.pdf", Status: "failed", ErrorMessage: "required price missing: cached_input", UpdatedAt: now, CollectionID: "c1"},
		},
		Sources:         []connectors.Source{{ID: "s1", Type: "slack", Name: "Workspace Slack", IsEnabled: true}},
		Empty:           dashboardEmpty{Icon: "knowledge", Text: "No documents."},
		SourcesEmpty:    dashboardEmpty{Icon: "sources", Text: "No sources."},
		EmbeddingModels: []string{"embed-1"},
	}
	auditRendered(t, "knowledge_collection page", renderPage(t, "knowledge_collection", collection, adminCaps))
	auditRendered(t, "knowledge frag-knowledge-documents", renderFrag(t, "knowledge_collection", "knowledge-documents", collection, adminCaps))
	auditRendered(t, "knowledge frag-knowledge-documents member", renderFrag(t, "knowledge_collection", "knowledge-documents", collection, memberCaps))
	auditRendered(t, "knowledge frag-knowledge-sources", renderFrag(t, "knowledge_collection", "knowledge-sources", collection, adminCaps))

	profiles := knowledgeProfilesData{Header: dashboardHeader{Title: "Processing profiles"},
		Empty: dashboardEmpty{Icon: "knowledge", Text: "No processing profiles."}}
	auditRendered(t, "knowledge_profiles page", renderPage(t, "knowledge_profiles", profiles, adminCaps))
	auditRendered(t, "knowledge frag-knowledge-profiles", renderFrag(t, "knowledge_profiles", "knowledge-profiles", profiles, adminCaps))

	sourcesTable := dashboardSourcesData{
		Rows: []dashboardSourceRow{{
			Source:   connectors.Source{ID: "s1", Type: "slack", Name: "Workspace Slack", IsEnabled: true},
			Provider: "Slack", Monogram: "W", Scope: "1 channel", Collection: "Policies",
			Permissions: "membership", State: "syncing", HasItems: true, Items: 12,
		}},
		Status: "", AnySyncing: true,
		Empty: dashboardEmpty{Icon: "sources", Text: "No sources connected.", ActionLabel: "Add source", ActionDrawerLabel: "Add source", ActionDrawerURL: "/dashboard/partials/sources/drawer"},
	}
	auditRendered(t, "sources page", renderPage(t, "sources", sourcesTable, adminCaps))
	auditRendered(t, "sources frag-sources-table", renderFrag(t, "sources", "sources-table", sourcesTable, adminCaps))

	drawer := dashboardSourcesDrawerData{
		CredentialRefs:   map[string][]auth.SecretRefMeta{"slack": {{RefID: "cred/slack-1"}}},
		Collections:      []rag.Collection{col},
		PresetCollection: "c1",
	}
	auditRendered(t, "sources frag-sources-drawer", renderFrag(t, "sources", "sources-drawer", drawer, adminCaps))
	edit := dashboardSourcesDrawerData{
		CredentialRefs: map[string][]auth.SecretRefMeta{"slack": {{RefID: "cred/slack-1"}}},
		EditSource:     &connectors.Source{ID: "s1", Type: "slack", Name: "Workspace Slack", CredentialRef: "cred/slack-1"},
	}
	auditRendered(t, "sources frag-sources-drawer edit", renderFrag(t, "sources", "sources-drawer", edit, adminCaps))
	bindings := dashboardSourceBindingsData{SourceID: "s1", SourceName: "Workspace Slack",
		Bindings: []source.SourceIdentity{{ID: "b1", NativeID: "U123", MappedUserID: "u1"}}}
	auditRendered(t, "sources frag-sources-bindings", renderFrag(t, "sources", "sources-bindings", bindings, adminCaps))

	assistants := assistantsPageData{
		Table: assistantsTableData{Rows: []assistantRow{{
			ID: "a1", Name: "Policy assistant", Description: "Answers policy",
			Initials: "PA", Model: "gpt-4o", Knowledge: []string{"Policies"},
			Audience: "everyone", StatusState: "active", Conversations: 4,
		}}},
		Tabs: []dashboardTab{
			{Label: "Assistants", Href: "/dashboard/assistants", Active: true},
			{Label: "Instructions", Href: "/dashboard/assistants/instructions"},
		},
		Empty: dashboardEmpty{Icon: "assistant", Text: "No assistants yet."},
	}
	auditRendered(t, "assistants page", renderPage(t, "assistants", assistants, adminCaps))
	auditRendered(t, "assistants frag-assistants-table", renderFrag(t, "assistants", "assistants-table", assistants, adminCaps))

	assistantDrawer := assistantDrawerData{
		Assistant:   &chatbot.Instance{ID: "a1", Name: "Policy assistant", Model: "gpt-4o", IsEnabled: true},
		Models:      []assistantModelOption{{ID: "gpt-4o", Current: true}},
		Templates:   []assistantTemplateOption{{ID: "p1", Name: "Policy tone", Selected: true}},
		Knowledge:   []assistantKnowledgeOption{{ID: "c1", Name: "Policies", Selected: true}},
		Tools:       []assistantToolOption{{ID: "t1", Label: "calendar", Approved: false}, {ID: "t2", Label: "search", Approved: true, Selected: true}},
		Members:     []assistantMember{{ID: "u1", Label: "Ada"}},
		Preserved:   []assistantSetting{{Key: "temperature", JSON: "0.2"}},
		AudienceAll: true,
	}
	auditRendered(t, "assistants frag-assistant-drawer", renderFrag(t, "assistants", "assistant-drawer", assistantDrawer, adminCaps))

	instructions := instructionsPageData{
		List: instructionsListData{Templates: []instructionRow{
			{ID: "p1", Name: "Policy tone", IsDefault: true, Variables: 1, UpdatedAt: now},
		}},
		Tabs: []dashboardTab{
			{Label: "Assistants", Href: "/dashboard/assistants"},
			{Label: "Instructions", Href: "/dashboard/assistants/instructions", Active: true},
		},
		Empty: dashboardEmpty{Icon: "assistant", Text: "No instructions yet."},
	}
	auditRendered(t, "assistants_instructions page", renderPage(t, "assistants_instructions", instructions, adminCaps))
	auditRendered(t, "assistants frag-instructions-list", renderFrag(t, "assistants_instructions", "instructions-list", instructions, adminCaps))

	editor := instructionEditorData{
		Template: &prompts.Template{ID: "p1", Name: "Policy tone", SystemPrompt: "sys", UserPrompt: "hi {{.user_input}}"},
		Vars:     []instructionVarRow{{Name: "user_input", Description: "The question", Required: true}},
	}
	auditRendered(t, "assistants frag-instruction-editor", renderFrag(t, "assistants_instructions", "instruction-editor", editor, adminCaps))

	tools := toolsData{
		Queue: []toolsQueueRow{{
			ServerID: "srv1", ToolID: "tool1", Name: "calendar", Origin: "https://mcp.example",
			State: "pending", SchemaVersion: 2, RemoteName: "calendar",
			ReadOnly: true, DiscoveredAt: now,
			Props: []toolsSchemaProp{{Field: "date", Type: "string"}},
		}},
		Servers:      []toolsServerRow{{ID: "srv1", Origin: "https://mcp.example", EndpointPath: "/mcp", Enabled: true, Approved: 1, Total: 2}},
		Tools:        []toolsToolRow{{ServerOrigin: "https://mcp.example", ToolID: "tool1", ModelName: "calendar", RemoteName: "calendar", ReadOnly: true, UsedByList: "Policy assistant"}},
		Scopes:       []toolsScopeChoice{{Name: "chat", Checked: true}, {Name: "search", Checked: true}},
		QueueEmpty:   dashboardEmpty{Icon: "tools", Text: "No tools pending approval."},
		ServersEmpty: dashboardEmpty{Icon: "tools", Text: "No servers."},
		ToolsEmpty:   dashboardEmpty{Icon: "tools", Text: "No tools."},
	}
	auditRendered(t, "tools page", renderPage(t, "tools", tools, adminCaps))
	auditRendered(t, "tools frag-tools-queue", renderFrag(t, "tools", "tools-queue", tools, adminCaps))
	auditRendered(t, "tools frag-tools-queue member", renderFrag(t, "tools", "tools-queue", tools, memberCaps))
	auditRendered(t, "tools frag-tools-servers", renderFrag(t, "tools", "tools-servers", tools, adminCaps))
	auditRendered(t, "tools frag-tools-list", renderFrag(t, "tools", "tools-list", tools, adminCaps))
	auditRendered(t, "tools frag-tools-register", renderFrag(t, "tools", "tools-register", struct {
		Server *mcpServerResponse
		Scopes []toolsScopeChoice
		Empty  dashboardEmpty
	}{
		Server: &mcpServerResponse{ID: "srv1", Origin: "https://mcp.example", EndpointPath: "/mcp", Enabled: true},
		Scopes: tools.Scopes,
		Empty:  dashboardEmpty{Icon: "tools", Text: "No permissions available."},
	}, adminCaps))

	models := modelsData{
		Tab: "endpoints",
		Endpoints: []modelsEndpointRow{{
			ID: "ep1", Protocol: "openai", Origin: "https://api.example", Enabled: true,
			AssuranceState: "verified", Models: []string{"gpt-4o"},
			Caps: []string{"chat"}, Runtime: "ready",
		}},
		Empty: dashboardEmpty{Icon: "models", Text: "No endpoints configured."},
	}
	auditRendered(t, "models page endpoints", renderPage(t, "models", models, adminCaps))
	auditRendered(t, "models frag-models-endpoints", renderFrag(t, "models", "models-endpoints", models, adminCaps))

	pricing := modelsData{
		Tab: "pricing",
		Rows: []modelsPriceRow{{
			EndpointID: "ep1", Model: "gpt-4o", Capability: "chat", Basis: "model", SubjectID: "gpt-4o", Unpriced: true,
			Cells: []modelsPriceCell{
				{Unit: "input", UnitLabel: "Input", Required: true, Priced: true, RateMicros: 500000, UnitQuantity: 1000000, Currency: "USD"},
				{Unit: "cached_input", UnitLabel: "Cached input", Required: true, Priced: false},
				{Unit: "output", UnitLabel: "Output", Required: true, Priced: true, RateMicros: 1500000, UnitQuantity: 1000000, Currency: "USD"},
			},
		}},
		Empty: dashboardEmpty{Icon: "models", Text: "No priced models."},
	}
	auditRendered(t, "models page pricing", renderPage(t, "models", pricing, adminCaps))
	auditRendered(t, "models frag-models-pricing", renderFrag(t, "models", "models-pricing", pricing, adminCaps))

	endpointDrawer := modelsDrawerData{
		Editing: true,
		Endpoint: inference.Endpoint{ID: "ep1", Protocol: "openai", SDKProvider: "acme",
			BaseOrigin: "https://api.example", CredentialRef: "cred/1", Enabled: true},
		Checks:  []modelsCapabilityCheck{{Value: "chat", Label: "Chat", Checked: true}},
		Aliases: "gpt-4o",
		Empty:   dashboardEmpty{Icon: "models", Text: "That endpoint is no longer available."},
	}
	auditRendered(t, "models frag-models-endpoint-drawer", renderFrag(t, "models", "models-endpoint-drawer", endpointDrawer, adminCaps))

	history := modelsHistoryData{
		EndpointID: "ep1", Model: "gpt-4o", Capability: "chat",
		Rows:  []modelsHistoryRow{{Subject: "endpoint", SubjectID: "ep1", UnitLabel: "Input", RateMicros: 500000, UnitQuantity: 1000000, Currency: "USD", EffectiveFrom: now}},
		Empty: dashboardEmpty{Icon: "billing", Text: "No price history."},
	}
	auditRendered(t, "models frag-models-price-history", renderFrag(t, "models", "models-price-history", history, adminCaps))

	access := func(mut func(*accessView)) *accessView {
		v := &accessView{
			Tab: "people",
			Tabs: []dashboardTab{
				{Label: "People", Href: "/dashboard/access/people", Active: true},
				{Label: "Roles", Href: "/dashboard/access/roles"},
				{Label: "Service accounts", Href: "/dashboard/access/service-accounts"},
				{Label: "API keys", Href: "/dashboard/access/api-keys"},
				{Label: "Sign-in", Href: "/dashboard/access/sign-in"},
			},
			People:        []accessPerson{{ID: "u1", Name: "Ada", Email: "ada@x", Team: "Eng", Status: "active", Roles: []string{"Admin"}}},
			Roles:         []accessRoleRow{{ID: "r1", Name: "Admin", Members: 2, ScopeLabels: []string{"platform admin"}}},
			Accounts:      []accessAccountRow{{ID: "sa1", Name: "CI bot", Status: "active", Created: now}},
			AccountForm:   []accessScopeOption{{Scope: "chat", Label: "Chat"}},
			Keys:          []accessKeyRow{{ID: "k1", Name: "deploy", Prefix: "ek-1", ScopeLabels: []string{"chat"}, Active: true}},
			Providers:     []accessProviderRow{{Name: "Okta", Issuer: "https://okta.example"}},
			PeopleEmpty:   dashboardEmpty{Icon: "users", Text: "No people yet."},
			RolesEmpty:    dashboardEmpty{Icon: "users", Text: "No roles yet."},
			AccountsEmpty: dashboardEmpty{Icon: "users", Text: "No service accounts."},
			KeysEmpty:     dashboardEmpty{Icon: "key", Text: "No keys yet."},
			SignInEmpty:   dashboardEmpty{Icon: "key", Text: "No providers."},
		}
		if mut != nil {
			mut(v)
		}
		return v
	}
	auditRendered(t, "access page", renderPage(t, "access", access(nil), adminCaps))
	for _, tab := range []string{"people", "roles", "service-accounts", "api-keys", "sign-in"} {
		auditRendered(t, "access frag-access-"+tab, renderFrag(t, "access", "access-"+tab, access(func(v *accessView) { v.Tab = tab }), adminCaps))
	}
	auditRendered(t, "access frag-access-user-drawer create", renderFrag(t, "access", "access-user-drawer", access(func(v *accessView) {
		v.UserDrawer = &accessUserDrawer{Create: true, Roles: []accessRoleChoice{{ID: "r1", Name: "Admin"}}}
	}), adminCaps))
	auditRendered(t, "access frag-access-user-drawer manage", renderFrag(t, "access", "access-user-drawer", access(func(v *accessView) {
		v.UserDrawer = &accessUserDrawer{
			ID: "u1", Name: "Ada", Email: "ada@x", Status: "active",
			Roles:    []accessRoleChoice{{ID: "r1", Name: "Admin", Assigned: true}},
			Sessions: []accessSessionRow{{ID: "s1", Created: now, Current: true}},
		}
	}), adminCaps))
	auditRendered(t, "access frag-access-role-editor", renderFrag(t, "access", "access-role-editor", access(func(v *accessView) {
		v.RoleEditor = &accessRoleEditor{
			RoleID: "r1", Name: "Admin",
			Scopes: []accessScopeOption{{Scope: "chat", Label: "Chat", Checked: true}},
			Grants: []accessGrantSection{
				{Kind: "assistants", Label: "Assistants", CallerAll: true,
					Items: []accessGrantItem{{ID: "a1", Label: "Policy assistant", Checked: true}}},
				{Kind: "knowledge", Label: "Knowledge", Empty: dashboardEmpty{Icon: "knowledge", Text: "No collections to grant."}},
			},
		}
	}), adminCaps))
	auditRendered(t, "access frag-access-key-drawer", renderFrag(t, "access", "access-key-drawer", access(func(v *accessView) {
		v.KeyDrawer = &accessKeyDrawer{
			Owners: []accessOwnerOption{{ID: "u1", Label: "Ada", Kind: "user"}},
			Scopes: []accessScopeOption{{Scope: "chat", Label: "Chat", Checked: true}},
		}
	}), adminCaps))

	usage := func(mut func(*usageView)) usageView {
		v := usageView{
			ActiveTab: "overview", Tabs: usageTabs("overview"), Days: 30,
			Overview: usageOverviewData{
				Days: 30, Requests: 10, Tokens: 100, Spend: "$1.00",
				Daily:   []dashboardBar{{Label: "Mon", Value: 3}},
				ByModel: []usageGroupRow{{Label: "gpt-4o", Requests: 5, Units: 50, Spend: "$0.50"}},
				ByTeam:  []usageGroupRow{{Label: "Eng", Requests: 5, Units: 50, Spend: "$0.50"}},
				ByKey:   []usageGroupRow{{Label: "deploy", Sub: "ek-1", Requests: 5, Units: 50, Spend: "$0.50"}},
				Empty:   dashboardEmpty{Icon: "billing", Text: "No usage."},
			},
			Budgets: []usageBudgetRow{{
				ID: "b1", Name: "org limit", SubjectType: "tenant", Kind: "spend",
				Period: "monthly", LimitDisplay: "$100.00", CountedDisplay: "$10.00",
				Pct: 10, LiabilitySummary: "open", StartsAt: now,
			}},
			BudgetsEmpty: dashboardEmpty{Icon: "billing", Text: "No budgets."},
			Audit: usageAuditData{
				Rows: []usageAuditRow{{
					Time: now, Kind: "price_set", ActorType: "user", Actor: "ada",
					SubjectType: "endpoint", SubjectID: "ep1", SubjectLabel: "acme endpoint", Amount: "$5.00",
				}},
				Kinds:        []usageKindChip{{Kind: "all", Label: "All", Active: true, Href: "/dashboard/usage/audit"}, {Kind: "price_set", Label: "Price set", Href: "/dashboard/usage/audit?kind=price_set"}},
				SubjectTypes: usageAuditSubjectTypes,
				Total:        1, Limit: 50, PrevOffset: -1, NextOffset: -1,
				ResetHref: "/dashboard/usage/audit",
				Empty:     dashboardEmpty{Icon: "billing", Text: "No events."},
			},
		}
		if mut != nil {
			mut(&v)
		}
		return v
	}
	auditRendered(t, "usage page", renderPage(t, "usage", usage(nil), adminCaps))
	auditRendered(t, "usage frag-usage-overview", renderFrag(t, "usage", "usage-overview", usage(nil), adminCaps))
	auditRendered(t, "usage frag-usage-budgets", renderFrag(t, "usage", "usage-budgets", usage(func(v *usageView) { v.ActiveTab = "budgets" }), adminCaps))
	auditRendered(t, "usage frag-usage-audit", renderFrag(t, "usage", "usage-audit", usage(func(v *usageView) { v.ActiveTab = "audit" }), adminCaps))
	auditRendered(t, "usage frag-usage-budget-drawer", renderFrag(t, "usage", "usage-budget-drawer", usage(func(v *usageView) {
		v.Drawer = usageBudgetDrawer{
			Name: "org limit", Kind: "spend", KindLabel: "Spend", PeriodLabel: "Monthly",
			LimitValue: "100", OrgName: "Acme", Today: "2026-10-28",
			Keys:     []usageSubjectOption{{ID: "k1", Label: "deploy"}},
			Users:    []usageSubjectOption{{ID: "u1", Label: "Ada"}},
			Services: []usageSubjectOption{{ID: "sa1", Label: "CI bot"}},
		}
	}), adminCaps))
	auditRendered(t, "usage frag-usage-budget-drawer edit", renderFrag(t, "usage", "usage-budget-drawer", usage(func(v *usageView) {
		v.Drawer = usageBudgetDrawer{Edit: true, BudgetID: "b1", LimitValue: "100", OrgName: "Acme", Today: "2026-10-28"}
	}), adminCaps))

	account := accountData{
		Profile:         accountProfile{Name: "Ada", Email: "ada@x", Roles: []string{"Admin"}},
		Sessions:        []accountSessionRow{{ID: "sess-1234", ShortID: "sess-12", CreatedAt: now, IdleExpiresAt: now, Current: true}},
		Identities:      []accountIdentityRow{{Issuer: "https://okta.example", Subject: "sub1", LinkedAt: now}},
		SessionsEmpty:   dashboardEmpty{Icon: "users", Text: "No sessions."},
		IdentitiesEmpty: dashboardEmpty{Icon: "key", Text: "No linked sign-ins."},
	}
	auditRendered(t, "account page", renderPage(t, "account", account, adminCaps))
	auditRendered(t, "account page link", renderPage(t, "account", accountData{
		Profile: accountProfile{Name: "Ada"}, LinkToken: "tok1", OIDCReady: true,
	}, adminCaps))
	auditRendered(t, "account frag-account-sessions", renderFrag(t, "account", "account-sessions", account, adminCaps))

	// Standalone auth documents render through the real auth template set.
	authSet := dashboardAuthTemplates()
	var buf bytes.Buffer
	if err := authSet.ExecuteTemplate(&buf, "login.tmpl", dashboardLoginView{
		dashboardLoginData: dashboardLoginData{Error: "Wrong password.", Providers: nil},
		Org:                "Acme",
	}); err != nil {
		t.Fatalf("render login: %v", err)
	}
	auditRendered(t, "auth login", buf.String())
	buf.Reset()
	if err := authSet.ExecuteTemplate(&buf, "login.tmpl", dashboardLoginView{
		dashboardLoginData: dashboardLoginData{LinkToken: "t", LinkProviderName: "Okta", LinkEmail: "ada@x"},
		Org:                "Acme",
	}); err != nil {
		t.Fatalf("render login link: %v", err)
	}
	auditRendered(t, "auth login link", buf.String())
	for _, name := range []string{"error-403", "error-404"} {
		buf.Reset()
		if err := authSet.ExecuteTemplate(&buf, name, dashboardErrorView{Org: "Acme"}); err != nil {
			t.Fatalf("render %s: %v", name, err)
		}
		auditRendered(t, "auth "+name, buf.String())
	}
}

// ── TestA11yFocus — rendered complements to the browser keyboard suite ───
// The DOM-order/focus-flow proofs live in tests/a11y-pages.spec.ts; here we
// assert the rendered preconditions that the flows operate on: citation
// buttons are real buttons named for their evidence, confirm controls are
// real <button>s, the wizard's step controls exist in pane order, and the
// composer Enter contract leaves a plain (non-submit) textarea.
func TestA11yFocus(t *testing.T) {
	chat := chatViewData{
		ConversationID: "c1",
		Assistants:     []chatbot.Instance{{ID: "a1", Name: "Policy assistant"}},
		Thread: &chatThreadData{
			ConversationID: "c1", AssistantName: "Policy assistant",
			Messages: []chatThreadMessage{{
				Role: "user", Author: "You", Content: "hi", CreatedAt: time.Now(),
				RunID: "r1", RunStatus: "running", Pending: true,
			}},
			Live: true,
		},
	}
	thread := renderFrag(t, "chat", "chat-thread", chat, memberCaps)
	doc, err := html.Parse(strings.NewReader(thread))
	if err != nil {
		t.Fatalf("parse thread: %v", err)
	}
	foundStop := false
	var walk func(*html.Node)
	walk = func(n *html.Node) {
		if n.Type == html.ElementNode {
			if _, ok := attr(n, "data-confirm"); ok {
				foundStop = foundStop || n.Data == "button"
			}
		}
		for c := n.FirstChild; c != nil; c = c.NextSibling {
			walk(c)
		}
	}
	walk(doc)
	if !foundStop {
		t.Fatalf("pending run renders no real <button data-confirm> stop control: %s", thread)
	}

	// Composer: the textarea keeps default Enter behavior for chat.js's
	// Enter-send/Shift+Enter-newline contract — it must not sit inside a
	// <form> that would navigate on Enter.
	page := renderPage(t, "chat", chat, adminCaps)
	doc, err = html.Parse(strings.NewReader(page))
	if err != nil {
		t.Fatalf("parse chat page: %v", err)
	}
	var composer *html.Node
	walk = func(n *html.Node) {
		if n.Type == html.ElementNode && n.Data == "textarea" {
			if id, ok := attr(n, "id"); ok && id == "composer" {
				composer = n
			}
		}
		for c := n.FirstChild; c != nil; c = c.NextSibling {
			walk(c)
		}
	}
	walk(doc)
	if composer == nil {
		t.Fatalf("chat page renders no composer textarea")
	}
	for p := composer.Parent; p != nil; p = p.Parent {
		if p.Type == html.ElementNode && p.Data == "form" {
			t.Fatalf("composer textarea inside a <form> — Enter must reach chat.js, not a navigation")
		}
	}
	if accessibleName(composer, nil, nil) == "" {
		t.Fatalf("composer textarea renders without an accessible name")
	}

	// Wizard panes are ordered siblings; Back/Continue stay real buttons.
	wizard := renderFrag(t, "sources", "sources-drawer", dashboardSourcesDrawerData{
		CredentialRefs: map[string][]auth.SecretRefMeta{},
		Collections:    []rag.Collection{{ID: "c1", Name: "Policies"}},
	}, adminCaps)
	doc, err = html.Parse(strings.NewReader(wizard))
	if err != nil {
		t.Fatalf("parse wizard: %v", err)
	}
	var panes int
	var back, next *html.Node
	walk = func(n *html.Node) {
		if n.Type == html.ElementNode {
			if _, ok := attr(n, "data-step-pane"); ok {
				panes++
			}
			if a, ok := attr(n, "data-action"); ok {
				switch a {
				case "sources:back":
					back = n
				case "sources:next":
					next = n
				}
			}
		}
		for c := n.FirstChild; c != nil; c = c.NextSibling {
			walk(c)
		}
	}
	walk(doc)
	if panes != 5 {
		t.Fatalf("wizard renders %d step panes, want 5", panes)
	}
	if back == nil || next == nil || back.Data != "button" || next.Data != "button" {
		t.Fatalf("wizard Back/Continue must be real buttons")
	}
}
