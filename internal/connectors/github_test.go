package connectors

import (
	"context"
	"encoding/base64"
	"encoding/json"
	"net/http"
	"net/http/httptest"
	"testing"

	"enclava-go/internal/source"
)

func TestGitHubContentPinsIdentityToConnectionAndRevision(t *testing.T) {
	mux := http.NewServeMux()
	issueRevision := "2026-01-01T00:00:00Z"
	mux.HandleFunc("/repos/acme/docs/issues", func(w http.ResponseWriter, r *http.Request) {
		json.NewEncoder(w).Encode([]any{map[string]any{"id": 99, "number": 7, "title": "Bug", "body": "details", "updated_at": issueRevision, "html_url": "https://github.com/acme/docs/issues/7"}})
	})
	mux.HandleFunc("/repos/acme/docs/issues/7/comments", func(w http.ResponseWriter, r *http.Request) {
		json.NewEncoder(w).Encode([]any{map[string]any{"id": 100, "body": "comment", "updated_at": "2026-01-02T00:00:00Z", "html_url": "https://github.com/acme/docs/issues/7#issuecomment-100"}})
	})
	mux.HandleFunc("/repos/acme/docs/pulls", func(w http.ResponseWriter, r *http.Request) { json.NewEncoder(w).Encode([]any{}) })
	mux.HandleFunc("/repos/acme/docs/commits/HEAD", func(w http.ResponseWriter, r *http.Request) {
		json.NewEncoder(w).Encode(map[string]string{"sha": "commit1"})
	})
	mux.HandleFunc("/repos/acme/docs/git/trees/commit1", func(w http.ResponseWriter, r *http.Request) {
		json.NewEncoder(w).Encode(map[string]any{"tree": []any{map[string]any{"path": "guide.md", "type": "blob", "sha": "blob1", "size": 3}}})
	})
	mux.HandleFunc("/repos/acme/docs/contents/guide.md", func(w http.ResponseWriter, r *http.Request) {
		json.NewEncoder(w).Encode(map[string]string{"content": base64.StdEncoding.EncodeToString([]byte("doc")), "encoding": "base64", "sha": "blob1"})
	})
	srv := httptest.NewTLSServer(mux)
	defer srv.Close()
	conn := source.Connection{ID: "conn1", TenantID: "t1", Kind: "github", Config: map[string]any{"owner": "acme", "repo": "docs", "api_url": srv.URL}}
	adapter, err := NewGitHubAdapter(conn, nil)
	if err != nil {
		t.Fatal(err)
	}
	g := adapter.(*GitHubAdapter)
	g.client = srv.Client()
	page, err := g.Content(context.Background(), conn, "", 100)
	if err != nil {
		t.Fatal(err)
	}
	if len(page.Items) != 1 || string(page.Items[0].Content) != "doc" || page.Items[0].NativeID != "github:conn1:acme/docs:guide.md" || page.Items[0].NativeRevision != "commit1:blob1" {
		t.Fatalf("unexpected content page: %#v", page)
	}
	if page.Items[0].URL != "https://github.com/acme/docs/blob/commit1/guide.md" || page.Completeness != CompletenessIncomplete || page.NextCursor == "" {
		t.Fatalf("unexpected revision/link/completeness: %#v", page)
	}
	page, err = g.Content(context.Background(), conn, page.NextCursor, 100)
	if err != nil {
		t.Fatal(err)
	}
	if len(page.Items) != 2 || page.Items[0].Kind != "issues" || page.Items[1].ParentNativeID != page.Items[0].NativeID || page.Items[0].NativeRevision != issueRevision || page.Items[0].URL == "" {
		t.Fatalf("issue/comment identity or revision missing: %#v", page.Items)
	}
	page, err = g.Content(context.Background(), conn, page.NextCursor, 100)
	if err != nil {
		t.Fatal(err)
	}
	if page.NextCursor != "" || page.Completeness != CompletenessComplete {
		t.Fatalf("expected complete terminal page: %#v", page)
	}
	issueRevision = "2026-02-01T00:00:00Z"
	updated, err := g.Content(context.Background(), conn, "", 100)
	if err != nil {
		t.Fatal(err)
	}
	updated, err = g.Content(context.Background(), conn, updated.NextCursor, 100)
	if err != nil {
		t.Fatal(err)
	}
	if len(updated.Items) < 1 || updated.Items[0].NativeID != "github:conn1:acme/docs:issues:99" || updated.Items[0].NativeRevision != issueRevision {
		t.Fatalf("changed issue did not retain identity and advance revision: %#v", updated.Items)
	}
}

func TestGitHubTruncatedTreeFetchesSubtrees(t *testing.T) {
	mux := http.NewServeMux()
	mux.HandleFunc("/repos/acme/docs/git/trees/root", func(w http.ResponseWriter, r *http.Request) {
		if r.URL.Query().Get("recursive") == "1" {
			json.NewEncoder(w).Encode(map[string]any{"truncated": true})
		} else {
			json.NewEncoder(w).Encode(map[string]any{"tree": []any{map[string]any{"path": "src", "type": "tree", "sha": "sub"}}})
		}
	})
	mux.HandleFunc("/repos/acme/docs/git/trees/sub", func(w http.ResponseWriter, r *http.Request) {
		json.NewEncoder(w).Encode(map[string]any{"tree": []any{map[string]any{"path": "main.go", "type": "blob", "sha": "blob", "size": 3}}})
	})
	srv := httptest.NewTLSServer(mux)
	defer srv.Close()
	g := &GitHubAdapter{owner: "acme", repo: "docs", base: srv.URL, client: srv.Client()}
	entries, err := g.treeEntries(context.Background(), "root")
	if err != nil {
		t.Fatal(err)
	}
	if len(entries) != 1 || entries[0].Path != "src/main.go" {
		t.Fatalf("subtree was not traversed: %#v", entries)
	}
}

func TestGitHubRejectsMissingRepository(t *testing.T) {
	if _, err := NewGitHubAdapter(source.Connection{Config: map[string]any{}}, nil); err == nil {
		t.Fatal("expected configuration error")
	}
}
