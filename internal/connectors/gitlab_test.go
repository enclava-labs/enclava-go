package connectors

import (
	"context"
	"encoding/json"
	"fmt"
	"net/http"
	"net/http/httptest"
	"strings"
	"testing"

	"enclava-go/internal/auth"
	"enclava-go/internal/source"
)

func TestGitLabFullAndIncrementalSync(t *testing.T) {
	head := "sha1"
	phase := "full"
	treeCalls, compareCalls, retries, filtered := 0, 0, 0, 0
	server := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		if r.Header.Get("Authorization") != "Bearer fixture-token" {
			t.Errorf("authorization = %q", r.Header.Get("Authorization"))
		}
		if !strings.HasPrefix(r.URL.EscapedPath(), "/api/v4/projects/group%2Fsub%2Fproject") {
			t.Errorf("project not encoded: %s", r.URL.EscapedPath())
		}
		path := strings.TrimPrefix(r.URL.Path, "/api/v4/projects/group/sub/project")
		write := func(v any) {
			if err := json.NewEncoder(w).Encode(v); err != nil {
				t.Error(err)
			}
		}
		switch {
		case path == "":
			if retries == 0 {
				retries++
				w.Header().Set("Retry-After", "0")
				w.WriteHeader(429)
				return
			}
			write(map[string]any{"default_branch": "main", "web_url": "https://gitlab.example/group/sub/project"})
		case path == "/repository/branches/main":
			write(map[string]any{"commit": map[string]string{"id": head}})
		case path == "/repository/tree":
			treeCalls++
			if phase != "full" {
				t.Error("tree called during delta")
			}
			if r.URL.Query().Get("ref") != head || r.URL.Query().Get("recursive") != "true" {
				t.Error("invalid tree query")
			}
			write([]any{map[string]string{"type": "blob", "path": "dir/a.txt"}, map[string]string{"type": "blob", "path": "b.txt"}, map[string]string{"type": "tree", "path": "dir"}})
		case path == "/repository/compare":
			compareCalls++
			if r.URL.Query().Get("from") != "sha1" || r.URL.Query().Get("to") != "sha2" {
				t.Errorf("compare query %s", r.URL.RawQuery)
			}
			write(map[string]any{"diffs": []any{map[string]any{"new_path": "dir/a.txt"}, map[string]any{"new_path": "deleted.txt", "deleted_file": true}}})
		case strings.HasPrefix(path, "/repository/files/"):
			if strings.Contains(path, "dir/a.txt") && !strings.Contains(r.URL.EscapedPath(), "dir%2Fa.txt") {
				t.Errorf("file path not encoded: %s", r.URL.EscapedPath())
			}
			_, _ = fmt.Fprint(w, "file content")
		case path == "/issues" || path == "/merge_requests":
			if phase == "full" && r.URL.Query().Get("updated_after") != "" {
				t.Error("full run filtered")
			}
			if phase != "full" {
				if r.URL.Query().Get("updated_after") == "" {
					t.Error("delta missing filter")
				}
				filtered++
			}
			if phase != "full" && path == "/merge_requests" {
				write([]any{})
				return
			}
			if r.URL.Query().Get("page") != "1" {
				write([]any{})
				return
			}
			iid := 1
			if path == "/merge_requests" {
				iid = 2
			}
			write([]any{map[string]any{"iid": iid, "title": "Title", "description": "Description", "state": "opened", "created_at": "2025-01-01T01:00:00+01:00", "updated_at": "2025-01-02T00:00:00Z", "author": map[string]string{"name": "Author"}, "assignees": []any{map[string]string{"name": "Alice"}}, "labels": []string{"label"}, "milestone": map[string]string{"title": "M1"}, "source_branch": "feature", "target_branch": "main", "web_url": "https://gitlab.example/item"}})
		case strings.HasSuffix(path, "/notes"):
			write([]any{map[string]any{"system": true, "body": "hidden"}, map[string]any{"author": map[string]string{"name": "Bob"}, "created_at": "2025-01-01T00:00:00Z", "body": "comment"}})
		default:
			t.Errorf("unexpected request %s", r.URL.String())
			http.NotFound(w, r)
		}
	}))
	defer server.Close()
	t.Setenv("GITLAB_FIXTURE_TOKEN", "fixture-token")
	resolver, err := auth.NewSecretResolver([]auth.SecretBinding{{RefID: "gitlab-test", TenantID: "tenant", Purpose: auth.SecretPurposeConnector, Origins: []string{server.URL}, EnvName: "GITLAB_FIXTURE_TOKEN"}})
	if err != nil {
		t.Fatal(err)
	}
	conn := source.Connection{ID: "conn", TenantID: "tenant", Kind: source.ConnectionGitLab, CredentialRef: "gitlab-test", Config: map[string]any{"project": "group/sub/project", "base_url": server.URL}}
	adapter, err := NewGitLabAdapter(conn, resolver)
	if err != nil {
		t.Fatal(err)
	}
	run := func(cursor Cursor, snapshot bool) ([]ContentItem, Cursor) {
		t.Helper()
		var items []ContentItem
		for calls := 0; calls < 30; calls++ {
			page, err := adapter.Content(context.Background(), conn, cursor, 1)
			if err != nil {
				t.Fatal(err)
			}
			if page.Snapshot != snapshot || len(page.Items) > 1 {
				t.Fatalf("invalid page: %+v", page)
			}
			items = append(items, page.Items...)
			if page.NextCursor == "" {
				if page.Completeness != CompletenessComplete || page.Checkpoint == "" {
					t.Fatalf("bad terminal page: %+v", page)
				}
				return items, page.Checkpoint
			}
			cursor = page.NextCursor
		}
		t.Fatal("run did not finish")
		return nil, ""
	}
	items, checkpoint := run("", true)
	if len(items) != 4 || items[0].NativeID != "gitlab:conn:group/sub/project:dir/a.txt" {
		t.Fatalf("full items: %+v", items)
	}
	if items[2].Kind != "issue" || items[2].Filename != "issue-1.md" || !strings.Contains(items[2].Text, "**Bob** — 2025-01-01 00:00 UTC") || strings.Contains(items[2].Text, "hidden") || items[2].CreatedAt == nil || items[2].MIMEType != "text/markdown" {
		t.Fatalf("issue: %+v", items[2])
	}
	if items[3].Kind != "merge_request" || !strings.Contains(items[3].Text, "feature → main") {
		t.Fatalf("merge request: %+v", items[3])
	}
	phase = "unchanged"
	items, checkpoint = run(checkpoint, false)
	if len(items) != 1 || items[0].Kind != "issue" || compareCalls != 0 || filtered == 0 {
		t.Fatalf("unchanged delta items=%+v tree=%d compare=%d filtered=%d", items, treeCalls, compareCalls, filtered)
	}
	phase, head = "push", "sha2"
	items, _ = run(checkpoint, false)
	if len(items) != 2 || items[0].NativeID != "gitlab:conn:group/sub/project:dir/a.txt" || items[0].NativeRevision != "sha2" || compareCalls == 0 {
		t.Fatalf("push delta items=%+v compare=%d", items, compareCalls)
	}
}

func TestGitLabPublicProjectAndValidation(t *testing.T) {
	server := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		if r.Header.Get("Authorization") != "" {
			t.Error("public request has authorization")
		}
		switch {
		case strings.HasSuffix(r.URL.Path, "/projects/123"):
			_, _ = fmt.Fprint(w, `{"default_branch":"main","web_url":"https://example.com/p"}`)
		case strings.HasSuffix(r.URL.Path, "/branches/main"):
			_, _ = fmt.Fprint(w, `{"commit":{"id":"sha"}}`)
		case strings.HasSuffix(r.URL.Path, "/repository/tree"):
			_, _ = fmt.Fprint(w, `[]`)
		case strings.HasSuffix(r.URL.Path, "/issues"):
			_, _ = fmt.Fprint(w, `[{"iid":1,"title":"Public issue","description":"Visible"}]`)
		case strings.HasSuffix(r.URL.Path, "/merge_requests"):
			_, _ = fmt.Fprint(w, `[]`)
		case strings.HasSuffix(r.URL.Path, "/notes"):
			t.Error("anonymous request for notes")
			w.WriteHeader(http.StatusUnauthorized)
		default:
			t.Errorf("unexpected request %s", r.URL)
			http.NotFound(w, r)
		}
	}))
	defer server.Close()
	conn := source.Connection{Kind: source.ConnectionGitLab, Config: map[string]any{"project": "123", "base_url": server.URL}}
	adapter, err := NewGitLabAdapter(conn, nil)
	if err != nil {
		t.Fatal(err)
	}
	page, err := adapter.Content(context.Background(), conn, "", 10)
	if err != nil {
		t.Fatal(err)
	}
	page, err = adapter.Content(context.Background(), conn, page.NextCursor, 10)
	if err != nil {
		t.Fatal(err)
	}
	if len(page.Items) != 1 || page.Items[0].Kind != "issue" || !strings.Contains(page.Items[0].Text, "Visible") {
		t.Fatalf("public issue missing: %+v", page.Items)
	}
	for _, bad := range []source.Connection{
		{Kind: source.ConnectionGitLab, Config: map[string]any{}},
		{Kind: source.ConnectionGitHub, Config: map[string]any{"project": "x"}},
		{Kind: source.ConnectionGitLab, Config: map[string]any{"project": "x", "base_url": "http://example.com"}},
		{Kind: source.ConnectionGitLab, CredentialRef: "missing", Config: map[string]any{"project": "x"}},
	} {
		if _, err := NewGitLabAdapter(bad, nil); err == nil {
			t.Errorf("accepted invalid connection: %+v", bad)
		}
	}
}
