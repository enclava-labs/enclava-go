package connectors

import (
	"context"
	"encoding/base64"
	"encoding/json"
	"fmt"
	"net/http"
	"net/http/httptest"
	"testing"

	"enclava-go/internal/source"
)

func TestGitHubContentPinsIdentityToConnectionAndRevision(t *testing.T) {
	mux := http.NewServeMux()
	issueRevision := "2026-01-01T00:00:00Z"
	mux.HandleFunc("/repos/acme/docs/issues", func(w http.ResponseWriter, r *http.Request) {
		_ = json.NewEncoder(w).Encode([]any{map[string]any{"id": 99, "number": 7, "title": "Bug", "body": "details", "updated_at": issueRevision, "html_url": "https://github.com/acme/docs/issues/7"}})
	})
	mux.HandleFunc("/repos/acme/docs/issues/7/comments", func(w http.ResponseWriter, r *http.Request) {
		_ = json.NewEncoder(w).Encode([]any{map[string]any{"id": 100, "body": "comment", "updated_at": "2026-01-02T00:00:00Z", "html_url": "https://github.com/acme/docs/issues/7#issuecomment-100"}})
	})
	mux.HandleFunc("/repos/acme/docs/pulls", func(w http.ResponseWriter, r *http.Request) {
		_ = json.NewEncoder(w).Encode([]any{map[string]any{"id": 101, "number": 8, "title": "Fix bug", "body": "fix", "updated_at": issueRevision}})
	})
	mux.HandleFunc("/repos/acme/docs/issues/8/comments", func(w http.ResponseWriter, r *http.Request) {
		_ = json.NewEncoder(w).Encode([]any{map[string]any{"id": 102, "body": "discussion"}})
	})
	mux.HandleFunc("/repos/acme/docs/pulls/8/comments", func(w http.ResponseWriter, r *http.Request) {
		_ = json.NewEncoder(w).Encode([]any{map[string]any{"id": 103, "body": "review"}})
	})
	mux.HandleFunc("/repos/acme/docs/collaborators", func(w http.ResponseWriter, r *http.Request) {
		_ = json.NewEncoder(w).Encode([]any{map[string]any{"login": "alice", "permissions": map[string]bool{"push": true}}, map[string]any{"id": 42, "permissions": map[string]bool{"pull": true}}})
	})
	mux.HandleFunc("/repos/acme/docs/commits/HEAD", func(w http.ResponseWriter, r *http.Request) {
		_ = json.NewEncoder(w).Encode(map[string]string{"sha": "commit1"})
	})
	mux.HandleFunc("/repos/acme/docs/git/trees/commit1", func(w http.ResponseWriter, r *http.Request) {
		_ = json.NewEncoder(w).Encode(map[string]any{"tree": []any{map[string]any{"path": "guide.md", "type": "blob", "sha": "blob1", "size": 3}}})
	})
	mux.HandleFunc("/repos/acme/docs/contents/guide.md", func(w http.ResponseWriter, r *http.Request) {
		_ = json.NewEncoder(w).Encode(map[string]string{"content": base64.StdEncoding.EncodeToString([]byte("doc")), "encoding": "base64", "sha": "blob1"})
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
	if page.Items[0].Title != "guide.md" || page.Items[0].ParentNativeID != "github:conn1:acme/docs" || page.Items[0].ParentKind != "repository" {
		t.Fatalf("file title or repository parent missing: %#v", page.Items[0])
	}
	page, err = g.Content(context.Background(), conn, page.NextCursor, 100)
	if err != nil {
		t.Fatal(err)
	}
	if len(page.Items) != 2 || page.Items[0].Kind != "issues" || page.Items[1].ParentNativeID != page.Items[0].NativeID || page.Items[0].NativeRevision != issueRevision || page.Items[0].URL == "" {
		t.Fatalf("issue/comment identity or revision missing: %#v", page.Items)
	}
	if page.Items[0].Title != "Bug" || page.Items[0].ParentNativeID != "github:conn1:acme/docs" || page.Items[0].ParentKind != "repository" ||
		page.Items[1].Title != "Bug" || page.Items[1].ParentKind != "issues" || page.Items[1].NativeID != "github:conn1:acme/docs:comment:100" {
		t.Fatalf("issue/comment title or parent missing: %#v", page.Items)
	}
	// Without a markdown type the ingest pipeline rejects issue text as an
	// unsupported document.
	for _, item := range page.Items {
		if item.MIMEType != "text/markdown" || item.Filename != "Bug.md" {
			t.Fatalf("issue/comment not typed as markdown: %#v", item)
		}
	}
	page, err = g.Content(context.Background(), conn, page.NextCursor, 100)
	if err != nil {
		t.Fatal(err)
	}
	if page.NextCursor != "" || page.Completeness != CompletenessComplete {
		t.Fatalf("expected complete terminal page: %#v", page)
	}
	if len(page.Items) != 3 || page.Items[0].NativeID != "github:conn1:acme/docs:pulls:101" ||
		page.Items[0].Title != "Fix bug" || page.Items[0].ParentNativeID != "github:conn1:acme/docs" || page.Items[0].ParentKind != "repository" {
		t.Fatalf("pull request title or repository parent missing: %#v", page.Items)
	}
	for _, item := range page.Items[1:] {
		if item.Title != "Fix bug" || item.ParentNativeID != page.Items[0].NativeID || item.ParentKind != "pulls" {
			t.Fatalf("pull request comment title or parent missing: %#v", item)
		}
	}
	if page.Items[1].NativeID != "github:conn1:acme/docs:comment:102" || page.Items[2].NativeID != "github:conn1:acme/docs:review_comment:103" {
		t.Fatalf("pull request comment IDs changed: %#v", page.Items)
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
			_ = json.NewEncoder(w).Encode(map[string]any{"truncated": true})
		} else {
			_ = json.NewEncoder(w).Encode(map[string]any{"tree": []any{map[string]any{"path": "src", "type": "tree", "sha": "sub"}}})
		}
	})
	mux.HandleFunc("/repos/acme/docs/git/trees/sub", func(w http.ResponseWriter, r *http.Request) {
		_ = json.NewEncoder(w).Encode(map[string]any{"tree": []any{map[string]any{"path": "main.go", "type": "blob", "sha": "blob", "size": 3}}})
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

func TestGitHubDiscussionPagesBoundedWithoutLosingComments(t *testing.T) {
	mux := http.NewServeMux()
	mux.HandleFunc("/repos/acme/docs/issues", func(w http.ResponseWriter, r *http.Request) {
		rows := []any{}
		if r.URL.Query().Get("page") == "1" {
			for i := 1; i <= 100; i++ {
				rows = append(rows, map[string]any{"id": i, "number": i, "title": fmt.Sprint(i)})
			}
		}
		_ = json.NewEncoder(w).Encode(rows)
	})
	mux.HandleFunc("/repos/acme/docs/issues/", func(w http.ResponseWriter, r *http.Request) {
		var n int
		_, _ = fmt.Sscanf(r.URL.Path, "/repos/acme/docs/issues/%d/comments", &n)
		rows := []any{}
		if n == 1 && r.URL.Query().Get("page") == "1" {
			for i := range 100 {
				rows = append(rows, map[string]any{"id": 10000 + i, "body": "comment"})
			}
		} else if n == 1 && r.URL.Query().Get("page") == "2" {
			rows = append(rows, map[string]any{"id": 10100, "body": "comment"})
		} else if n != 1 && r.URL.Query().Get("page") == "1" {
			rows = append(rows, map[string]any{"id": 1000 + n, "body": "comment"})
		}
		_ = json.NewEncoder(w).Encode(rows)
	})
	mux.HandleFunc("/repos/acme/docs/pulls", func(w http.ResponseWriter, r *http.Request) {
		_ = json.NewEncoder(w).Encode([]any{})
	})
	srv := httptest.NewTLSServer(mux)
	defer srv.Close()
	g := &GitHubAdapter{owner: "acme", repo: "docs", base: srv.URL, client: srv.Client()}
	conn := source.Connection{ID: "conn"}
	cursor := Cursor(`{"phase":"issues","page":1}`)
	seen := map[string]bool{}
	for cursor != "" {
		page, err := g.Content(context.Background(), conn, cursor, 100)
		if err != nil {
			t.Fatal(err)
		}
		if len(page.Items) > 100 {
			t.Fatalf("page has %d items", len(page.Items))
		}
		for _, item := range page.Items {
			if seen[item.NativeID] {
				t.Fatalf("duplicate %s", item.NativeID)
			}
			seen[item.NativeID] = true
		}
		cursor = page.NextCursor
	}
	if len(seen) != 300 {
		t.Fatalf("got %d items, want 300", len(seen))
	}
}
