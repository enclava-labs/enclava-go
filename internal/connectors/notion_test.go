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
	t.Helper()
	server := httptest.NewServer(handler)
	t.Cleanup(server.Close)
	t.Setenv("NOTION_FIXTURE_TOKEN", "fixture-token")
	resolver, err := auth.NewSecretResolver([]auth.SecretBinding{{
		RefID: "notion-test", TenantID: "tenant-test", Purpose: auth.SecretPurposeConnector,
		Origins: []string{server.URL}, EnvName: "NOTION_FIXTURE_TOKEN",
	}})
	if err != nil { t.Fatal(err) }
	conn := source.Connection{TenantID:"tenant-test", Kind:source.ConnectionNotion, CredentialRef:"notion-test", Config:map[string]any{
		"base_url": server.URL+"/v1", "max_depth": float64(8), "max_pages": float64(100),
		"selected_pages": []any{map[string]any{"page_id":"root", "audiences":[]any{map[string]any{"id":"engineering", "members":[]any{"identity-1"}}}}},
	}}
	adapter, err := NewNotionAdapter(conn, resolver)
	if err != nil { t.Fatal(err) }
	return adapter.(*NotionAdapter), conn
}

func TestNotionRecursivePaginationAndStructuredTables(t *testing.T) {
	mux := http.NewServeMux()
	mux.HandleFunc("/v1/pages/root", func(w http.ResponseWriter, r *http.Request) {
		json.NewEncoder(w).Encode(map[string]any{"id":"root", "url":"https://www.notion.so/root", "last_edited_time":"2026-01-01T00:00:00Z"})
	})
	mux.HandleFunc("/v1/pages/child", func(w http.ResponseWriter, r *http.Request) {
		json.NewEncoder(w).Encode(map[string]any{"id":"child", "url":"https://www.notion.so/child", "last_edited_time":"2026-01-02T00:00:00Z"})
	})
	mux.HandleFunc("/v1/blocks/root/children", func(w http.ResponseWriter, r *http.Request) {
		if r.URL.Query().Get("start_cursor") == "next" {
			json.NewEncoder(w).Encode(map[string]any{"results":[]any{map[string]any{"id":"table-row", "type":"table_row", "last_edited_time":"r1", "table_row":map[string]any{"cells":[]any{[]any{map[string]any{"plain_text":"A"}}}}}}, "has_more":false})
			return
		}
		json.NewEncoder(w).Encode(map[string]any{"results":[]any{
			map[string]any{"id":"child", "type":"child_page", "has_children":true, "last_edited_time":"r0", "child_page":map[string]any{"title":"Nested"}},
			map[string]any{"id":"table", "type":"table", "has_children":true, "last_edited_time":"r0"},
		}, "has_more":true, "next_cursor":"next"})
	})
	mux.HandleFunc("/v1/blocks/table/children", func(w http.ResponseWriter, r *http.Request) {
		json.NewEncoder(w).Encode(map[string]any{"results":[]any{map[string]any{"id":"table-row", "type":"table_row", "last_edited_time":"r1", "table_row":map[string]any{"cells":[]any{[]any{map[string]any{"plain_text":"A"}}}}}}, "has_more":false})
	})
	mux.HandleFunc("/v1/blocks/child/children", func(w http.ResponseWriter, r *http.Request) {
		json.NewEncoder(w).Encode(map[string]any{"results":[]any{map[string]any{"id":"nested-block", "type":"paragraph", "last_edited_time":"r2", "paragraph":map[string]any{"rich_text":[]any{map[string]any{"plain_text":"nested"}}}}}, "has_more":false})
	})
	adapter, conn := notionFixtureAdapter(t, mux)
	page, err := adapter.Content(context.Background(), conn, "", 100)
	if err != nil { t.Fatal(err) }
	if !page.Snapshot || page.Completeness != CompletenessComplete { t.Fatalf("incomplete traversal: %+v", page) }
	byID := map[string]ContentItem{}
	for _, item := range page.Items { byID[item.NativeID] = item }
	for _, id := range []string{"root", "child", "nested-block", "table", "table-row"} {
		if _, ok := byID[id]; !ok { t.Errorf("missing %s in items: %+v", id, byID) }
	}
	if byID["nested-block"].ParentNativeID != "child" || byID["table-row"].ParentNativeID != "table" { t.Fatalf("wrong parents: %+v %+v", byID["nested-block"], byID["table-row"]) }
	if byID["root"].URL != "https://www.notion.so/root" || byID["nested-block"].NativeRevision != "r2" || !strings.Contains(string(byID["table-row"].Content), "table_row") { t.Fatalf("native URL/revision/table structure missing: %+v", byID) }
	first, err := adapter.Content(context.Background(), conn, "", 2)
	if err != nil || first.NextCursor == "" || len(first.Items) != 2 { t.Fatalf("bounded content page: %+v err=%v", first, err) }
	second, err := adapter.Content(context.Background(), conn, first.NextCursor, 2)
	if err != nil || len(second.Items) != 2 { t.Fatalf("cursor page: %+v err=%v", second, err) }
}

func TestNotionOnlyTraversesSelectedRootsAndRequiresAudience(t *testing.T) {
	mux := http.NewServeMux()
	mux.HandleFunc("/v1/pages/root", func(w http.ResponseWriter, r *http.Request) { json.NewEncoder(w).Encode(map[string]any{"id":"root", "url":"https://www.notion.so/root"}) })
	mux.HandleFunc("/v1/blocks/root/children", func(w http.ResponseWriter, r *http.Request) {
		json.NewEncoder(w).Encode(map[string]any{"results":[]any{map[string]any{"id":"linked", "type":"link_to_page", "link_to_page":map[string]any{"page_id":"not-selected"}}}, "has_more":false})
	})
	adapter, conn := notionFixtureAdapter(t, mux)
	page, err := adapter.Content(context.Background(), conn, "", 100)
	if err != nil { t.Fatal(err) }
	for _, item := range page.Items { if item.NativeID == "not-selected" { t.Fatal("linked unselected page was ingested") } }
	if _, err := notionSelections([]any{map[string]any{"page_id":"root", "audiences":[]any{}}}); err == nil { t.Fatal("selection without publisher-assigned audience was accepted") }
}

func TestNotionAccessRevokesLostPublisherAndUsesPublishedAudienceGroups(t *testing.T) {
	mux := http.NewServeMux()
	rootReads := 0
	mux.HandleFunc("/v1/pages/root", func(w http.ResponseWriter, r *http.Request) {
		rootReads++
		if rootReads == 1 { http.Error(w, "gone", http.StatusNotFound); return }
		json.NewEncoder(w).Encode(map[string]any{"id":"root", "url":"https://www.notion.so/root"})
	})
	adapter, conn := notionFixtureAdapter(t, mux)
	page, err := adapter.Access(context.Background(), conn, "", 100)
	if err != nil { t.Fatal(err) }
	if page.Completeness != CompletenessComplete || len(page.Grants) != 1 || page.Grants[0].Effect != "deny" || page.Grants[0].Scope != "read" {
		t.Fatalf("publisher access loss not explicit: %+v", page)
	}
	if page.Grants[0].NativeGroupID != "notion:audience:engineering" { t.Fatalf("audience group not source-qualified: %+v", page.Grants[0]) }
	members, err := adapter.Membership(context.Background(), conn, "", 100)
	if err != nil || members.Completeness != CompletenessComplete || len(members.Memberships) != 1 ||
		members.Memberships[0].NativeGroupID != page.Grants[0].NativeGroupID || members.Memberships[0].NativePrincipalID != "identity-1" {
		t.Fatalf("publisher audience membership missing: %+v err=%v", members, err)
	}
}

func TestNotionRejectsRedirects(t *testing.T) {
	redirect := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) { http.Redirect(w, r, "https://example.org", http.StatusFound) }))
	t.Cleanup(redirect.Close)
	adapter, conn := notionFixtureAdapter(t, http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) { http.Redirect(w, r, redirect.URL, http.StatusFound) }))
	if _, err := adapter.Content(context.Background(), conn, "", 10); err == nil { t.Fatal("followed upstream redirect") }
}
func TestNotionAccessGrantsSelectedSubtree(t *testing.T) {
	mux := http.NewServeMux()
	mux.HandleFunc("/v1/pages/root", func(w http.ResponseWriter, r *http.Request) {
		json.NewEncoder(w).Encode(map[string]any{"id":"root", "url":"https://www.notion.so/root"})
	})
	mux.HandleFunc("/v1/blocks/root/children", func(w http.ResponseWriter, r *http.Request) {
		json.NewEncoder(w).Encode(map[string]any{"results":[]any{map[string]any{"id":"para", "type":"paragraph", "last_edited_time":"r1"}}, "has_more":false})
	})
	adapter, conn := notionFixtureAdapter(t, mux)
	page, err := adapter.Access(context.Background(), conn, "", 100)
	if err != nil { t.Fatal(err) }
	if len(page.Grants) != 2 { t.Fatalf("expected root and block grants, got %+v", page.Grants) }
	for _, grant := range page.Grants {
		if grant.Effect != "allow" || grant.Scope != "read" || grant.NativeGroupID != "notion:audience:engineering" {
			t.Fatalf("unexpected published-audience grant: %+v", grant)
		}
	}
}
