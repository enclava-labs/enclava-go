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
	if err != nil {
		t.Fatal(err)
	}
	conn := source.Connection{TenantID: "tenant-test", Kind: source.ConnectionNotion, CredentialRef: "notion-test", Config: map[string]any{
		"base_url": server.URL + "/v1", "max_depth": float64(8), "max_pages": float64(100),
		"selected_pages": []any{map[string]any{"page_id": "root"}},
	}}
	adapter, err := NewNotionAdapter(conn, resolver)
	if err != nil {
		t.Fatal(err)
	}
	return adapter.(*NotionAdapter), conn
}

func TestNotionRecursivePaginationAndStructuredTables(t *testing.T) {
	mux := http.NewServeMux()
	mux.HandleFunc("/v1/pages/root", func(w http.ResponseWriter, r *http.Request) {
		_ = json.NewEncoder(w).Encode(map[string]any{"id": "root", "url": "https://www.notion.so/root", "last_edited_time": "2026-01-01T00:00:00Z", "properties": map[string]any{"Name": map[string]any{"type": "title", "title": []any{map[string]any{"plain_text": "Root "}, map[string]any{"plain_text": "Page"}}}}})
	})
	mux.HandleFunc("/v1/pages/child", func(w http.ResponseWriter, r *http.Request) {
		_ = json.NewEncoder(w).Encode(map[string]any{"id": "child", "url": "https://www.notion.so/child", "last_edited_time": "2026-01-02T00:00:00Z", "properties": map[string]any{"Title": map[string]any{"type": "title", "title": []any{map[string]any{"plain_text": "Child Page"}}}}})
	})
	mux.HandleFunc("/v1/blocks/root/children", func(w http.ResponseWriter, r *http.Request) {
		if r.URL.Query().Get("start_cursor") == "next" {
			_ = json.NewEncoder(w).Encode(map[string]any{"results": []any{map[string]any{"id": "table-row", "type": "table_row", "last_edited_time": "r1", "table_row": map[string]any{"cells": []any{[]any{map[string]any{"plain_text": "A"}}}}}}, "has_more": false})
			return
		}
		_ = json.NewEncoder(w).Encode(map[string]any{"results": []any{
			map[string]any{"id": "child", "type": "child_page", "has_children": true, "last_edited_time": "r0", "child_page": map[string]any{"title": "Nested"}},
			map[string]any{"id": "table", "type": "table", "has_children": true, "last_edited_time": "r0"},
		}, "has_more": true, "next_cursor": "next"})
	})
	mux.HandleFunc("/v1/blocks/table/children", func(w http.ResponseWriter, r *http.Request) {
		_ = json.NewEncoder(w).Encode(map[string]any{"results": []any{map[string]any{"id": "table-row", "type": "table_row", "last_edited_time": "r1", "table_row": map[string]any{"cells": []any{[]any{map[string]any{"plain_text": "A"}}}}}}, "has_more": false})
	})
	mux.HandleFunc("/v1/blocks/child/children", func(w http.ResponseWriter, r *http.Request) {
		_ = json.NewEncoder(w).Encode(map[string]any{"results": []any{map[string]any{"id": "nested-block", "type": "paragraph", "last_edited_time": "r2", "paragraph": map[string]any{"rich_text": []any{map[string]any{"plain_text": "nested"}}}}}, "has_more": false})
	})
	adapter, conn := notionFixtureAdapter(t, mux)
	page, err := adapter.Content(context.Background(), conn, "", 100)
	if err != nil {
		t.Fatal(err)
	}
	if !page.Snapshot || page.Completeness != CompletenessComplete {
		t.Fatalf("incomplete traversal: %+v", page)
	}
	byID := map[string]ContentItem{}
	for _, item := range page.Items {
		byID[item.NativeID] = item
	}
	for _, id := range []string{"root", "child", "nested-block", "table", "table-row"} {
		if _, ok := byID[id]; !ok {
			t.Errorf("missing %s in items: %+v", id, byID)
		}
	}
	if byID["nested-block"].ParentNativeID != "child" || byID["table-row"].ParentNativeID != "table" {
		t.Fatalf("wrong parents: %+v %+v", byID["nested-block"], byID["table-row"])
	}
	if byID["root"].Title != "Root Page" || byID["table"].Title != "Root Page" || byID["table-row"].Title != "Root Page" || byID["child"].Title != "Child Page" || byID["nested-block"].Title != "Child Page" {
		t.Fatalf("wrong page titles: %+v", byID)
	}
	if byID["table"].ParentKind != "page" || byID["table-row"].ParentKind != "block" || byID["child"].ParentKind != "page" || byID["nested-block"].ParentKind != "page" {
		t.Fatalf("wrong parent kinds: %+v", byID)
	}
	if byID["root"].URL != "https://www.notion.so/root" || byID["nested-block"].NativeRevision != "r2" || !strings.Contains(string(byID["table-row"].Content), "table_row") {
		t.Fatalf("native URL/revision/table structure missing: %+v", byID)
	}
	first, err := adapter.Content(context.Background(), conn, "", 2)
	if err != nil || first.NextCursor == "" || len(first.Items) != 2 {
		t.Fatalf("bounded content page: %+v err=%v", first, err)
	}
	second, err := adapter.Content(context.Background(), conn, first.NextCursor, 2)
	if err != nil || len(second.Items) != 2 {
		t.Fatalf("cursor page: %+v err=%v", second, err)
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
			map[string]any{"id": "a1b2-c3d4", "type": "paragraph", "last_edited_time": "t1"},
		}, "has_more": false})
	})
	adapter, conn := notionFixtureAdapter(t, mux)
	page, err := adapter.Content(context.Background(), conn, "", 100)
	if err != nil {
		t.Fatal(err)
	}
	byID := map[string]ContentItem{}
	for _, item := range page.Items {
		byID[item.NativeID] = item
	}
	if byID["root"].URL != "https://www.notion.so/root-page-abc" {
		t.Fatalf("page url = %q", byID["root"].URL)
	}
	if byID["a1b2-c3d4"].URL != "https://www.notion.so/root-page-abc#a1b2c3d4" {
		t.Fatalf("block url = %q, want page url + dashless anchor", byID["a1b2-c3d4"].URL)
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
