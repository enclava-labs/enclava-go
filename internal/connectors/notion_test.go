package connectors

import (
	"context"
	"encoding/json"
	"net/http"
	"net/http/httptest"
	"strings"
	"testing"

	"enclava-go/internal/auth"
	"enclava-go/internal/source"
)

func notionFixtureAdapter(t *testing.T, handler http.Handler) (*NotionAdapter, source.Connection) {
	return notionFixtureAdapterConfig(t, handler, nil)
}

func notionFixtureAdapterConfig(t *testing.T, handler http.Handler, overrides map[string]any) (*NotionAdapter, source.Connection) {
	t.Helper()
	server := httptest.NewServer(handler)
	t.Cleanup(server.Close)
	t.Setenv("NOTION_FIXTURE_TOKEN", "fixture-token")
	resolver, err := auth.NewSecretResolver([]auth.SecretBinding{{
		RefID: "notion-test", TenantID: "tenant-test", Purpose: auth.SecretPurposeConnector,
		Origins: []string{server.URL}, EnvName: "NOTION_FIXTURE_TOKEN",
	}})
	if err != nil {
		t.Fatal(err)
	}
	config := map[string]any{
		"base_url": server.URL + "/v1", "max_depth": float64(8), "max_pages": float64(100),
		"selected_pages": []any{map[string]any{"page_id": "root"}},
	}
	for key, value := range overrides {
		config[key] = value
	}
	conn := source.Connection{TenantID: "tenant-test", Kind: source.ConnectionNotion, CredentialRef: "notion-test", Config: config}
	adapter, err := NewNotionAdapter(conn, resolver)
	if err != nil {
		t.Fatal(err)
	}
	return adapter.(*NotionAdapter), conn
}

func notionPageJSON(id, title, pageURL string) map[string]any {
	return map[string]any{
		"id": id, "url": pageURL,
		"created_time": "2026-01-01T00:00:00.000Z", "last_edited_time": "2026-02-01T00:00:00.000Z",
		"last_edited_by": map[string]any{"object": "user", "id": "user-9"},
		"properties": map[string]any{"Name": map[string]any{
			"type": "title", "title": []any{map[string]any{"plain_text": title}},
		}},
	}
}

func notionBlockJSON(id, typ string, payload any) map[string]any {
	block := map[string]any{
		"id": id, "type": typ, "has_children": false,
		"created_time": "2026-01-03T00:00:00.000Z", "last_edited_time": "2026-02-03T00:00:00.000Z",
		"last_edited_by": map[string]any{"object": "user", "id": "user-1"},
	}
	if payload != nil {
		block[typ] = payload
	}
	return block
}

func notionPlain(parts ...string) []any {
	out := make([]any, 0, len(parts))
	for _, part := range parts {
		out = append(out, map[string]any{"plain_text": part})
	}
	return out
}

func notionParagraph(id string, parts ...string) map[string]any {
	return notionBlockJSON(id, "paragraph", map[string]any{"rich_text": notionPlain(parts...)})
}

func notionChildren(blocks ...any) http.HandlerFunc {
	return func(w http.ResponseWriter, r *http.Request) {
		_ = json.NewEncoder(w).Encode(map[string]any{"results": blocks, "has_more": false})
	}
}

func TestNotionPagesCarryStructuredBlockDocuments(t *testing.T) {
	rootURL := "https://www.notion.so/root-page"
	mux := http.NewServeMux()
	mux.HandleFunc("/v1/pages/root", func(w http.ResponseWriter, r *http.Request) {
		_ = json.NewEncoder(w).Encode(notionPageJSON("root", "Root Page", rootURL))
	})
	mux.HandleFunc("/v1/pages/child", func(w http.ResponseWriter, r *http.Request) {
		_ = json.NewEncoder(w).Encode(notionPageJSON("child", "Child Page", "https://www.notion.so/child-page"))
	})
	mux.HandleFunc("/v1/pages/grand", func(w http.ResponseWriter, r *http.Request) {
		_ = json.NewEncoder(w).Encode(notionPageJSON("grand", "Grand Page", "https://www.notion.so/grand-page"))
	})
	mux.HandleFunc("/v1/users/user-1", func(w http.ResponseWriter, r *http.Request) {
		_ = json.NewEncoder(w).Encode(map[string]any{"id": "user-1", "name": "Ada", "type": "person"})
	})
	mux.HandleFunc("/v1/users/user-9", func(w http.ResponseWriter, r *http.Request) {
		_ = json.NewEncoder(w).Encode(map[string]any{"id": "user-9", "name": "Grace", "type": "person"})
	})

	toggle := notionBlockJSON("toggle-1", "toggle", map[string]any{"rich_text": notionPlain("details")})
	toggle["has_children"] = true
	table := notionBlockJSON("table-1", "table", map[string]any{"table_width": 2, "has_column_header": true, "has_row_header": false})
	table["has_children"] = true
	childPage := notionBlockJSON("child", "child_page", map[string]any{"title": "Child"})
	childPage["has_children"] = true
	unknownAuthor := notionParagraph("p-unknown", "mystery editor")
	unknownAuthor["last_edited_by"] = map[string]any{"object": "user", "id": "user-404"}

	mux.HandleFunc("/v1/blocks/root/children", notionChildren(
		notionBlockJSON("h-1", "heading_1", map[string]any{"rich_text": notionPlain("Root Title")}),
		notionBlockJSON("a1b2-c3d4-e5f6-g7h8", "paragraph", map[string]any{"rich_text": notionPlain("Hello ", "world")}),
		notionBlockJSON("b-1", "bulleted_list_item", map[string]any{"rich_text": notionPlain("bullet")}),
		notionBlockJSON("n-1", "numbered_list_item", map[string]any{"rich_text": notionPlain("numbered")}),
		notionBlockJSON("t-1", "to_do", map[string]any{"rich_text": notionPlain("done task"), "checked": true}),
		notionBlockJSON("t-2", "to_do", map[string]any{"rich_text": notionPlain("open task"), "checked": false}),
		notionBlockJSON("c-1", "code", map[string]any{"rich_text": notionPlain("fmt.Println()"), "language": "go"}),
		notionBlockJSON("q-1", "quote", map[string]any{"rich_text": notionPlain("wise words")}),
		notionBlockJSON("ca-1", "callout", map[string]any{"rich_text": notionPlain("note this")}),
		toggle,
		notionBlockJSON("img-1", "image", map[string]any{"type": "external", "external": map[string]any{"url": "https://example.com/x.png"}}),
		notionParagraph("p-empty"),
		table,
		childPage,
		notionBlockJSON("d-1", "divider", nil),
		unknownAuthor,
	))
	mux.HandleFunc("/v1/blocks/toggle-1/children", notionChildren(
		notionParagraph("p-nested", "nested detail"),
	))
	mux.HandleFunc("/v1/blocks/table-1/children", notionChildren(
		notionBlockJSON("row-1", "table_row", map[string]any{"cells": []any{
			notionPlain("Name"), notionPlain("Qty"),
		}}),
		notionBlockJSON("row-2", "table_row", map[string]any{"cells": []any{
			notionPlain("Apple"), notionPlain("3"),
		}}),
	))
	mux.HandleFunc("/v1/blocks/child/children", notionChildren(
		notionParagraph("p-child", "child body"),
		func() map[string]any {
			grand := notionBlockJSON("grand", "child_page", map[string]any{"title": "Grand"})
			grand["has_children"] = true
			return grand
		}(),
	))
	mux.HandleFunc("/v1/blocks/grand/children", notionChildren(
		notionBlockJSON("gh-1", "heading_2", map[string]any{"rich_text": notionPlain("Grand")}),
	))

	adapter, _ := notionFixtureAdapter(t, mux)
	items, stats, err := adapter.traverseSelections(context.Background(), adapter.selections)
	if err != nil {
		t.Fatal(err)
	}
	if len(items) != 3 {
		t.Fatalf("want one item per page, got %d: %+v", len(items), items)
	}
	byID := map[string]ContentItem{}
	for _, item := range items {
		byID[item.NativeID] = item
		if item.MIMEType == "application/json" {
			t.Fatalf("raw JSON content emitted for %s", item.NativeID)
		}
		if len(item.Blocks) == 0 && !strings.HasPrefix(string(item.Content), `{"blocks":`) {
			t.Fatalf("item %s has neither blocks nor a blocks document", item.NativeID)
		}
	}

	root := byID["root"]
	if root.Kind != "page" || root.Title != "Root Page" || root.URL != rootURL {
		t.Fatalf("bad page item: %+v", root)
	}
	if root.NativeRevision != "2026-02-01T00:00:00.000Z" || root.CreatedAt == nil || root.UpdatedAt == nil {
		t.Fatalf("page timestamps/revision missing: %+v", root)
	}
	if len(root.Authors) != 1 || root.Authors[0] != "Grace" {
		t.Fatalf("page authors = %v", root.Authors)
	}
	if root.Container != "" {
		t.Fatalf("root container = %q", root.Container)
	}

	type want struct {
		kind string
		text string
	}
	wants := []want{
		{source.StructuredHeading, "Root Title"},
		{source.StructuredParagraph, "Hello world"},
		{source.StructuredListItem, "bullet"},
		{source.StructuredListItem, "numbered"},
		{source.StructuredListItem, "[x] done task"},
		{source.StructuredListItem, "[ ] open task"},
		{source.StructuredCode, "fmt.Println()"},
		{source.StructuredParagraph, "wise words"},
		{source.StructuredParagraph, "note this"},
		{source.StructuredParagraph, "details"},
		{source.StructuredParagraph, "nested detail"},
		{source.StructuredTableHeader, "Name | Qty"},
		{source.StructuredTableRow, "Apple | 3"},
		{source.StructuredParagraph, "mystery editor"},
	}
	if len(root.Blocks) != len(wants) {
		t.Fatalf("root blocks = %+v", root.Blocks)
	}
	for i, w := range wants {
		got := root.Blocks[i]
		if got.Kind != w.kind || got.Text != w.text {
			t.Fatalf("block %d = %s %q, want %s %q", i, got.Kind, got.Text, w.kind, w.text)
		}
	}
	if root.Blocks[0].Level != 1 {
		t.Fatalf("heading level = %d", root.Blocks[0].Level)
	}
	if code := root.Blocks[6]; code.Language != "go" {
		t.Fatalf("code language = %q", code.Language)
	}
	para := root.Blocks[1]
	if para.NativeID != "a1b2-c3d4-e5f6-g7h8" || para.URL != rootURL+"#a1b2c3d4e5f6g7h8" {
		t.Fatalf("block anchor = %+v", para)
	}
	if para.Author != "Ada" || para.CreatedAt != "2026-01-03T00:00:00.000Z" || para.UpdatedAt != "2026-02-03T00:00:00.000Z" {
		t.Fatalf("block provenance = %+v", para)
	}
	if got := root.Blocks[13].Author; got != "" {
		t.Fatalf("unresolvable author = %q, want empty", got)
	}
	if stats.unsupportedBlocks != 2 {
		t.Fatalf("unsupported blocks = %d, want 2 (image, divider)", stats.unsupportedBlocks)
	}

	child := byID["child"]
	if child.ParentNativeID != "root" || child.ParentKind != "page" || child.Container != "Root Page" || child.Title != "Child Page" {
		t.Fatalf("child page item = %+v", child)
	}
	if len(child.Blocks) != 1 || child.Blocks[0].Text != "child body" || child.Blocks[0].URL != "https://www.notion.so/child-page#pchild" {
		t.Fatalf("child blocks = %+v", child.Blocks)
	}
	grand := byID["grand"]
	if grand.Container != "Root Page / Child Page" || len(grand.Blocks) != 1 || grand.Blocks[0].Kind != source.StructuredHeading || grand.Blocks[0].Level != 2 {
		t.Fatalf("grandchild item = %+v", grand)
	}
	if stats.truncated {
		t.Fatal("unexpected truncation")
	}
}

func TestNotionContentPagesSnapshotAndCompleteness(t *testing.T) {
	mux := http.NewServeMux()
	mux.HandleFunc("/v1/pages/root", func(w http.ResponseWriter, r *http.Request) {
		_ = json.NewEncoder(w).Encode(notionPageJSON("root", "Root Page", "https://www.notion.so/root"))
	})
	mux.HandleFunc("/v1/pages/child", func(w http.ResponseWriter, r *http.Request) {
		_ = json.NewEncoder(w).Encode(notionPageJSON("child", "Child Page", "https://www.notion.so/child"))
	})
	childPage := notionBlockJSON("child", "child_page", map[string]any{"title": "Child"})
	childPage["has_children"] = true
	mux.HandleFunc("/v1/blocks/root/children", notionChildren(
		notionParagraph("p-1", "one"),
		childPage,
	))
	mux.HandleFunc("/v1/blocks/child/children", notionChildren(
		notionParagraph("p-child", "child body"),
	))
	adapter, conn := notionFixtureAdapter(t, mux)

	first, err := adapter.Content(context.Background(), conn, "", 1)
	if err != nil || len(first.Items) != 1 || first.NextCursor == "" {
		t.Fatalf("first content page: %+v err=%v", first, err)
	}
	if !first.Snapshot || first.Completeness != CompletenessComplete {
		t.Fatalf("complete snapshot expected: %+v", first)
	}
	second, err := adapter.Content(context.Background(), conn, first.NextCursor, 1)
	if err != nil || len(second.Items) != 1 || second.NextCursor != "" {
		t.Fatalf("second content page: %+v err=%v", second, err)
	}
	if second.Items[0].NativeID != "child" {
		t.Fatalf("second page item = %s", second.Items[0].NativeID)
	}
}

func TestNotionIncompleteTraversalCannotAuthorizeDeletion(t *testing.T) {
	mux := http.NewServeMux()
	mux.HandleFunc("/v1/pages/root", func(w http.ResponseWriter, r *http.Request) {
		_ = json.NewEncoder(w).Encode(notionPageJSON("root", "Root Page", "https://www.notion.so/root"))
	})
	mux.HandleFunc("/v1/blocks/root/children", func(w http.ResponseWriter, r *http.Request) {
		http.Error(w, "boom", http.StatusInternalServerError)
	})
	adapter, conn := notionFixtureAdapter(t, mux)
	page, err := adapter.Content(context.Background(), conn, "", 100)
	if err == nil {
		t.Fatal("expected traversal error")
	}
	if page.Completeness != CompletenessIncomplete {
		t.Fatalf("incomplete traversal must not authorize deletion: %+v", page)
	}
}

func TestNotionEmptyPageSyncsEmptyBlocksDocument(t *testing.T) {
	mux := http.NewServeMux()
	mux.HandleFunc("/v1/pages/root", func(w http.ResponseWriter, r *http.Request) {
		_ = json.NewEncoder(w).Encode(notionPageJSON("root", "Empty Page", "https://www.notion.so/empty"))
	})
	mux.HandleFunc("/v1/blocks/root/children", notionChildren(
		notionBlockJSON("img-1", "image", map[string]any{"type": "external", "external": map[string]any{"url": "https://example.com/x.png"}}),
	))
	adapter, _ := notionFixtureAdapter(t, mux)
	items, stats, err := adapter.traverseSelections(context.Background(), adapter.selections)
	if err != nil {
		t.Fatal(err)
	}
	if len(items) != 1 {
		t.Fatalf("items = %+v", items)
	}
	item := items[0]
	if len(item.Blocks) != 0 || item.MIMEType != source.BlocksMIMEType || item.Filename != "Empty Page.blocks.json" {
		t.Fatalf("empty page item = %+v", item)
	}
	var doc source.BlocksDocument
	if err := json.Unmarshal(item.Content, &doc); err != nil || len(doc.Blocks) != 0 {
		t.Fatalf("empty blocks document = %s err=%v", item.Content, err)
	}
	if stats.unsupportedBlocks != 1 {
		t.Fatalf("unsupported = %d", stats.unsupportedBlocks)
	}
}

func TestNotionDepthLimitReportsTruncation(t *testing.T) {
	mux := http.NewServeMux()
	mux.HandleFunc("/v1/pages/root", func(w http.ResponseWriter, r *http.Request) {
		_ = json.NewEncoder(w).Encode(notionPageJSON("root", "Root Page", "https://www.notion.so/root"))
	})
	top := notionBlockJSON("top", "paragraph", map[string]any{"rich_text": notionPlain("top")})
	top["has_children"] = true
	mid := notionBlockJSON("mid", "paragraph", map[string]any{"rich_text": notionPlain("mid")})
	mid["has_children"] = true
	mux.HandleFunc("/v1/blocks/root/children", notionChildren(top))
	mux.HandleFunc("/v1/blocks/top/children", notionChildren(mid))
	mux.HandleFunc("/v1/blocks/mid/children", func(w http.ResponseWriter, r *http.Request) {
		t.Error("children below the depth limit were fetched")
		_ = json.NewEncoder(w).Encode(map[string]any{"results": []any{}, "has_more": false})
	})
	adapter, _ := notionFixtureAdapterConfig(t, mux, map[string]any{"max_depth": float64(1)})
	items, stats, err := adapter.traverseSelections(context.Background(), adapter.selections)
	if err != nil {
		t.Fatal(err)
	}
	if !stats.truncated {
		t.Fatal("depth truncation not reported")
	}
	if len(items) != 1 || len(items[0].Blocks) != 2 {
		t.Fatalf("items = %+v", items)
	}
	if items[0].Blocks[0].Text != "top" || items[0].Blocks[1].Text != "mid" {
		t.Fatalf("blocks = %+v", items[0].Blocks)
	}
}

func TestNotionOnlyTraversesSelectedRoots(t *testing.T) {
	mux := http.NewServeMux()
	mux.HandleFunc("/v1/pages/root", func(w http.ResponseWriter, r *http.Request) {
		_ = json.NewEncoder(w).Encode(map[string]any{"id": "root", "url": "https://www.notion.so/root"})
	})
	mux.HandleFunc("/v1/blocks/root/children", func(w http.ResponseWriter, r *http.Request) {
		_ = json.NewEncoder(w).Encode(map[string]any{"results": []any{map[string]any{"id": "linked", "type": "link_to_page", "link_to_page": map[string]any{"page_id": "not-selected"}}}, "has_more": false})
	})
	adapter, conn := notionFixtureAdapter(t, mux)
	page, err := adapter.Content(context.Background(), conn, "", 100)
	if err != nil {
		t.Fatal(err)
	}
	for _, item := range page.Items {
		if item.NativeID == "not-selected" {
			t.Fatal("linked unselected page was ingested")
		}
	}
	if _, err := notionSelections([]any{map[string]any{"page_id": ""}}); err == nil {
		t.Fatal("empty page_id selection was accepted")
	}
	if _, err := notionSelections([]any{map[string]any{"page_id": "root"}}); err != nil {
		t.Fatalf("page_id-only selection rejected: %v", err)
	}
}

func TestNotionRejectsRedirects(t *testing.T) {
	redirect := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		http.Redirect(w, r, "https://example.org", http.StatusFound)
	}))
	t.Cleanup(redirect.Close)
	adapter, conn := notionFixtureAdapter(t, http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) { http.Redirect(w, r, redirect.URL, http.StatusFound) }))
	if _, err := adapter.Content(context.Background(), conn, "", 10); err == nil {
		t.Fatal("followed upstream redirect")
	}
}

func TestNotionBlockURLsAnchorToEnclosingPage(t *testing.T) {
	mux := http.NewServeMux()
	mux.HandleFunc("/v1/pages/root", func(w http.ResponseWriter, r *http.Request) {
		_ = json.NewEncoder(w).Encode(map[string]any{"id": "root", "url": "https://www.notion.so/root-page-abc", "last_edited_time": "t0", "properties": map[string]any{}})
	})
	mux.HandleFunc("/v1/blocks/root/children", func(w http.ResponseWriter, r *http.Request) {
		_ = json.NewEncoder(w).Encode(map[string]any{"results": []any{
			notionBlockJSON("a1b2-c3d4", "paragraph", map[string]any{"rich_text": notionPlain("anchored")}),
		}, "has_more": false})
	})
	adapter, _ := notionFixtureAdapter(t, mux)
	items, _, err := adapter.traverseSelections(context.Background(), adapter.selections)
	if err != nil {
		t.Fatal(err)
	}
	if len(items) != 1 || len(items[0].Blocks) != 1 {
		t.Fatalf("items = %+v", items)
	}
	if items[0].URL != "https://www.notion.so/root-page-abc" {
		t.Fatalf("page url = %q", items[0].URL)
	}
	if got := items[0].Blocks[0].URL; got != "https://www.notion.so/root-page-abc#a1b2c3d4" {
		t.Fatalf("block url = %q, want page url + dashless anchor", got)
	}
}

func TestValidNotionPageURLHosts(t *testing.T) {
	for raw, want := range map[string]bool{
		"https://www.notion.so/Page-abc":       true,
		"https://app.notion.com/Page-abc":      true,
		"https://acme.notion.site/Page-abc":    true,
		"http://app.notion.com/Page-abc":       false,
		"https://notion.com.evil.example/Page": false,
		"https://evilnotion.com/Page":          false,
	} {
		if got := validNotionPageURL(raw); got != want {
			t.Errorf("validNotionPageURL(%q) = %v, want %v", raw, got, want)
		}
	}
}
