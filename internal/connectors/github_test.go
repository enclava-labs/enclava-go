package connectors

import (
	"context"
	"encoding/base64"
	"encoding/json"
	"fmt"
	"net/http"
	"net/http/httptest"
	"reflect"
	"strings"
	"sync"
	"sync/atomic"
	"testing"

	"enclava-go/internal/source"
)

func TestGitHubContentPinsIdentityToConnectionAndRevision(t *testing.T) {
	mux := http.NewServeMux()
	issueRevision := "2026-01-01T00:00:00Z"
	mux.HandleFunc("/repos/acme/docs/issues", func(w http.ResponseWriter, r *http.Request) {
		_ = json.NewEncoder(w).Encode([]any{map[string]any{
			"id": 99, "number": 7, "title": "Bug", "body": "details",
			"created_at": "2026-01-01T00:00:00Z", "updated_at": issueRevision,
			"html_url": "https://github.com/acme/docs/issues/7",
			"user":     map[string]any{"login": "alice"},
		}})
	})
	mux.HandleFunc("/repos/acme/docs/issues/7/comments", func(w http.ResponseWriter, r *http.Request) {
		_ = json.NewEncoder(w).Encode([]any{map[string]any{
			"id": 100, "body": "comment", "created_at": "2026-01-02T00:00:00Z",
			"html_url": "https://github.com/acme/docs/issues/7#issuecomment-100",
			"user":     map[string]any{"login": "bob"},
		}})
	})
	mux.HandleFunc("/repos/acme/docs/pulls", func(w http.ResponseWriter, r *http.Request) {
		_ = json.NewEncoder(w).Encode([]any{map[string]any{
			"id": 101, "number": 8, "title": "Fix bug", "body": "fix",
			"created_at": "2026-01-01T00:00:00Z", "updated_at": issueRevision,
			"html_url": "https://github.com/acme/docs/pull/8",
			"user":     map[string]any{"login": "carol"},
		}})
	})
	mux.HandleFunc("/repos/acme/docs/issues/8/comments", func(w http.ResponseWriter, r *http.Request) {
		_ = json.NewEncoder(w).Encode([]any{map[string]any{
			"id": 102, "body": "discussion", "created_at": "2026-01-03T00:00:00Z",
			"html_url": "https://github.com/acme/docs/pull/8#issuecomment-102",
			"user":     map[string]any{"login": "dave"},
		}})
	})
	mux.HandleFunc("/repos/acme/docs/pulls/8/comments", func(w http.ResponseWriter, r *http.Request) {
		_ = json.NewEncoder(w).Encode([]any{map[string]any{
			"id": 103, "body": "review", "created_at": "2026-01-02T00:00:00Z",
			"html_url": "https://github.com/acme/docs/pull/8#discussion_r103",
			"user":     map[string]any{"login": "erin"},
		}})
	})
	mux.HandleFunc("/repos/acme/docs/pulls/8/reviews", func(w http.ResponseWriter, r *http.Request) {
		_ = json.NewEncoder(w).Encode([]any{
			map[string]any{"id": 104, "body": "looks good", "submitted_at": "2026-01-04T00:00:00Z",
				"html_url": "https://github.com/acme/docs/pull/8#pullrequestreview-104",
				"user":     map[string]any{"login": "frank"}},
			map[string]any{"id": 105, "body": "", "submitted_at": "2026-01-05T00:00:00Z",
				"html_url": "https://github.com/acme/docs/pull/8#pullrequestreview-105",
				"user":     map[string]any{"login": "grace"}},
		})
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
	if page.Items[0].URL != "https://github.com/acme/docs/blob/commit1/guide.md" || page.Completeness != CompletenessIncomplete || page.NextCursor == "" || !page.Snapshot {
		t.Fatalf("unexpected revision/link/completeness: %#v", page)
	}
	if page.Items[0].Title != "guide.md" || page.Items[0].ParentNativeID != "github:conn1:acme/docs" || page.Items[0].ParentKind != "repository" {
		t.Fatalf("file title or repository parent missing: %#v", page.Items[0])
	}
	page, err = g.Content(context.Background(), conn, page.NextCursor, 100)
	if err != nil {
		t.Fatal(err)
	}
	// One document per issue: the body and its comments are blocks, not
	// separate items.
	if len(page.Items) != 1 {
		t.Fatalf("issue page must hold one document, got %#v", page.Items)
	}
	item := page.Items[0]
	if item.NativeID != "github:conn1:acme/docs:issues:99" || item.Kind != "issues" || item.Title != "#7: Bug" ||
		item.ParentNativeID != "github:conn1:acme/docs" || item.ParentKind != "repository" ||
		item.NativeRevision != issueRevision || item.URL != "https://github.com/acme/docs/issues/7" ||
		item.Container != "acme/docs" || item.Filename != "issue-7.blocks.json" {
		t.Fatalf("issue identity or metadata missing: %#v", item)
	}
	if item.CreatedAt == nil || item.CreatedAt.Format("2006-01-02") != "2026-01-01" ||
		item.UpdatedAt == nil || item.UpdatedAt.Format("2006-01-02") != "2026-01-01" {
		t.Fatalf("issue timestamps missing: %#v", item)
	}
	if !reflect.DeepEqual(item.Authors, []string{"alice", "bob"}) {
		t.Fatalf("issue authors = %#v", item.Authors)
	}
	blocks := item.Blocks
	if len(blocks) != 3 {
		t.Fatalf("issue blocks = %#v", blocks)
	}
	if blocks[0].Kind != source.StructuredHeading || blocks[0].Level != 1 || blocks[0].Text != "#7: Bug" ||
		blocks[0].URL != item.URL || blocks[0].Author != "alice" || blocks[0].CreatedAt != "2026-01-01T00:00:00Z" {
		t.Fatalf("issue heading block wrong: %#v", blocks[0])
	}
	if blocks[1].Kind != source.StructuredParagraph || blocks[1].Text != "details" || blocks[1].URL != item.URL {
		t.Fatalf("issue body block wrong: %#v", blocks[1])
	}
	if blocks[2].Kind != source.StructuredMessage || blocks[2].NativeID != "comment:100" || blocks[2].Text != "comment" ||
		blocks[2].Author != "bob" || blocks[2].URL != "https://github.com/acme/docs/issues/7#issuecomment-100" ||
		blocks[2].CreatedAt != "2026-01-02T00:00:00Z" {
		t.Fatalf("issue comment block wrong: %#v", blocks[2])
	}
	page, err = g.Content(context.Background(), conn, page.NextCursor, 100)
	if err != nil {
		t.Fatal(err)
	}
	if page.NextCursor != "" || page.Completeness != CompletenessComplete || !page.Snapshot {
		t.Fatalf("expected complete snapshot terminal page: %#v", page)
	}
	if len(page.Items) != 1 {
		t.Fatalf("pull request page must hold one document, got %#v", page.Items)
	}
	pr := page.Items[0]
	if pr.NativeID != "github:conn1:acme/docs:pulls:101" || pr.Kind != "pulls" || pr.Title != "#8: Fix bug" ||
		pr.Filename != "pull-8.blocks.json" || pr.Container != "acme/docs" {
		t.Fatalf("pull request identity or metadata missing: %#v", pr)
	}
	if !reflect.DeepEqual(pr.Authors, []string{"carol", "erin", "dave", "frank"}) {
		t.Fatalf("pull request authors = %#v", pr.Authors)
	}
	if len(pr.Blocks) != 5 {
		t.Fatalf("pull request blocks = %#v", pr.Blocks)
	}
	if pr.Blocks[0].Kind != source.StructuredHeading || pr.Blocks[0].Text != "#8: Fix bug" ||
		pr.Blocks[1].Kind != source.StructuredParagraph || pr.Blocks[1].Text != "fix" {
		t.Fatalf("pull request heading/body blocks wrong: %#v", pr.Blocks[:2])
	}
	// Messages merge issue comments, review comments and reviews in
	// created_at order; the empty-body review is dropped.
	wantMessages := []struct{ nativeID, author, created string }{
		{"review_comment:103", "erin", "2026-01-02T00:00:00Z"},
		{"comment:102", "dave", "2026-01-03T00:00:00Z"},
		{"review:104", "frank", "2026-01-04T00:00:00Z"},
	}
	for i, want := range wantMessages {
		b := pr.Blocks[2+i]
		if b.Kind != source.StructuredMessage || b.NativeID != want.nativeID || b.Author != want.author || b.CreatedAt != want.created {
			t.Fatalf("pull request message block %d = %#v, want %s", i, b, want.nativeID)
		}
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

func TestGitHubUnchangedIssueKeepsRevisionAndBlocks(t *testing.T) {
	mux := http.NewServeMux()
	mux.HandleFunc("/repos/acme/docs/issues/7/comments", func(w http.ResponseWriter, r *http.Request) {
		_ = json.NewEncoder(w).Encode([]any{map[string]any{
			"id": 100, "body": "comment", "created_at": "2026-01-02T00:00:00Z",
			"html_url": "https://github.com/acme/docs/issues/7#issuecomment-100",
			"user":     map[string]any{"login": "bob"},
		}})
	})
	srv := httptest.NewTLSServer(mux)
	defer srv.Close()
	g := &GitHubAdapter{owner: "acme", repo: "docs", base: srv.URL, client: srv.Client()}
	conn := source.Connection{ID: "conn1"}
	row := ghDiscussion{
		Number: 7, ID: 99, Title: "Bug", Body: "details",
		CreatedAt: "2026-01-01T00:00:00Z", UpdatedAt: "2026-01-03T00:00:00Z",
		HTMLURL: "https://github.com/acme/docs/issues/7",
	}
	row.User.Login = "alice"
	first, err := g.discussionItem(context.Background(), conn, "issues", row)
	if err != nil {
		t.Fatal(err)
	}
	second, err := g.discussionItem(context.Background(), conn, "issues", row)
	if err != nil {
		t.Fatal(err)
	}
	if !reflect.DeepEqual(first, second) {
		t.Fatalf("unchanged issue produced different items:\nfirst=%#v\nsecond=%#v", first, second)
	}
	firstBody, err := source.EncodeBlocks(first.Blocks)
	if err != nil {
		t.Fatal(err)
	}
	secondBody, err := source.EncodeBlocks(second.Blocks)
	if err != nil {
		t.Fatal(err)
	}
	if string(firstBody) != string(secondBody) {
		t.Fatal("unchanged issue produced different block bytes")
	}
}

func TestGitHubMarkdownStructureBlocks(t *testing.T) {
	body := "## Details\n\nIntro line one\nline two.\n\n### Steps ##\n\n- alpha\n- beta\n\n```go\nfmt.Println()\n```\n\n1. first\n2. second\n\n####### not a heading\n#notaheading\n\n```unclosed\ncode"
	blocks := markdownStructureBlocks(body, "https://example.test/i/7", "ann", "2026-01-01T00:00:00Z")
	want := []struct {
		kind     string
		text     string
		level    int
		language string
	}{
		{source.StructuredHeading, "Details", 3, ""},
		{source.StructuredParagraph, "Intro line one\nline two.", 0, ""},
		{source.StructuredHeading, "Steps", 4, ""},
		{source.StructuredListItem, "alpha", 0, ""},
		{source.StructuredListItem, "beta", 0, ""},
		{source.StructuredCode, "fmt.Println()", 0, "go"},
		{source.StructuredListItem, "first", 0, ""},
		{source.StructuredListItem, "second", 0, ""},
		{source.StructuredParagraph, "####### not a heading\n#notaheading", 0, ""},
		{source.StructuredCode, "code", 0, "unclosed"},
	}
	if len(blocks) != len(want) {
		t.Fatalf("got %d blocks, want %d: %#v", len(blocks), len(want), blocks)
	}
	for i, w := range want {
		b := blocks[i]
		if b.Kind != w.kind || b.Text != w.text || b.Level != w.level || b.Language != w.language {
			t.Fatalf("block %d = %#v, want kind=%s text=%q level=%d lang=%q", i, b, w.kind, w.text, w.level, w.language)
		}
		if b.URL != "https://example.test/i/7" || b.Author != "ann" || b.CreatedAt != "2026-01-01T00:00:00Z" {
			t.Fatalf("block %d missing issue provenance: %#v", i, b)
		}
	}
	if blocks := markdownStructureBlocks("", "u", "a", "t"); len(blocks) != 0 {
		t.Fatalf("empty body produced blocks: %#v", blocks)
	}
}

func TestGitHubDiscussionCommentsPaginate(t *testing.T) {
	mux := http.NewServeMux()
	var mu sync.Mutex
	var pages []string
	mux.HandleFunc("/repos/acme/docs/issues/7/comments", func(w http.ResponseWriter, r *http.Request) {
		page := r.URL.Query().Get("page")
		mu.Lock()
		pages = append(pages, page)
		mu.Unlock()
		start, n := 1, 100
		if page == "2" {
			start, n = 101, 50
		}
		rows := make([]any, 0, n)
		for i := start; i < start+n; i++ {
			rows = append(rows, map[string]any{
				"id": i, "body": fmt.Sprintf("comment %d", i), "created_at": "2026-01-01T00:00:00Z",
				"html_url": fmt.Sprintf("https://github.com/acme/docs/issues/7#issuecomment-%d", i),
				"user":     map[string]any{"login": "bob"},
			})
		}
		_ = json.NewEncoder(w).Encode(rows)
	})
	srv := httptest.NewTLSServer(mux)
	defer srv.Close()
	g := &GitHubAdapter{owner: "acme", repo: "docs", base: srv.URL, client: srv.Client()}
	conn := source.Connection{ID: "conn1"}
	item, err := g.discussionItem(context.Background(), conn, "issues", ghDiscussion{
		Number: 7, ID: 99, Title: "Busy", CreatedAt: "2026-01-01T00:00:00Z", UpdatedAt: "2026-01-02T00:00:00Z",
		HTMLURL: "https://github.com/acme/docs/issues/7",
	})
	if err != nil {
		t.Fatal(err)
	}
	messages := 0
	for _, b := range item.Blocks {
		if b.Kind == source.StructuredMessage {
			messages++
		}
	}
	if messages != 150 {
		t.Fatalf("got %d message blocks, want 150", messages)
	}
	last := item.Blocks[len(item.Blocks)-1]
	if last.NativeID != "comment:150" {
		t.Fatalf("last message = %#v, want comment:150", last)
	}
	mu.Lock()
	defer mu.Unlock()
	if !reflect.DeepEqual(pages, []string{"1", "2"}) {
		t.Fatalf("comment pages fetched = %#v", pages)
	}
}

func TestGitHubDiscussionCommentsPaginationBound(t *testing.T) {
	mux := http.NewServeMux()
	var calls atomic.Int32
	mux.HandleFunc("/repos/acme/docs/issues/7/comments", func(w http.ResponseWriter, r *http.Request) {
		calls.Add(1)
		rows := make([]any, 100)
		for i := range rows {
			rows[i] = map[string]any{"id": i + 1, "body": "comment"}
		}
		_ = json.NewEncoder(w).Encode(rows)
	})
	srv := httptest.NewTLSServer(mux)
	defer srv.Close()
	g := &GitHubAdapter{owner: "acme", repo: "docs", base: srv.URL, client: srv.Client()}
	_, err := g.discussionComments(context.Background(), srv.URL+"/repos/acme/docs/issues/7/comments")
	if err == nil || !strings.Contains(err.Error(), "exceeded") {
		t.Fatalf("expected pagination bound error, got %v", err)
	}
	if got := calls.Load(); got != ghCommentPageLimit {
		t.Fatalf("comment fetch made %d requests, want %d", got, ghCommentPageLimit)
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
			if item.Kind != "issues" {
				t.Fatalf("comment emitted as separate item: %#v", item)
			}
			if seen[item.NativeID] {
				t.Fatalf("duplicate %s", item.NativeID)
			}
			seen[item.NativeID] = true
		}
		cursor = page.NextCursor
	}
	// One document per issue; its 101 comments are blocks inside it.
	if len(seen) != 100 {
		t.Fatalf("got %d items, want 100", len(seen))
	}
	issue1 := seen["github:conn:acme/docs:issues:1"]
	if !issue1 {
		t.Fatal("issue 1 missing from listing")
	}
}

// newGitHubConnectorFixture wires the real GitHubAdapter (pointed at an
// httptest API) into the durable sync service fixture.
func newGitHubConnectorFixture(t *testing.T, srv *httptest.Server) (*connectorFixture, string) {
	t.Helper()
	f := newConnectorFixture(t)
	ctx := context.Background()
	const connID = "conn-gh"
	conn := source.Connection{
		TenantID: f.tenant, ID: connID, Kind: source.ConnectionGitHub, Name: "GitHub",
		Config:             map[string]any{"owner": "acme", "repo": "docs", "api_url": srv.URL},
		TargetCollectionID: f.connection.TargetCollectionID, Status: "active", CreatedBy: f.principal.ActorID,
	}
	if err := f.sourceRepo.CreateConnection(ctx, conn); err != nil {
		t.Fatal(err)
	}
	adapter := &GitHubAdapter{owner: "acme", repo: "docs", ref: "HEAD", base: srv.URL, client: srv.Client()}
	factories := AdapterFactories{SourceKindGitHub: func(source.Connection) (Adapter, error) { return adapter, nil }}
	f.svc.SetSyncDependencies(factories, f.observations, f.artifacts, f.repo)
	return f, connID
}

// TestGitHubSyncTombstonesLegacyCommentItems covers the upgrade path: items
// emitted per comment before structured documents existed are tombstoned by
// the first complete snapshot sync, while an interrupted listing deletes
// nothing.
func TestGitHubSyncTombstonesLegacyCommentItems(t *testing.T) {
	ctx := context.Background()
	var failPulls atomic.Bool
	failPulls.Store(true)
	mux := http.NewServeMux()
	mux.HandleFunc("/repos/acme/docs/commits/HEAD", func(w http.ResponseWriter, r *http.Request) {
		_ = json.NewEncoder(w).Encode(map[string]string{"sha": "commit1"})
	})
	mux.HandleFunc("/repos/acme/docs/git/trees/commit1", func(w http.ResponseWriter, r *http.Request) {
		_ = json.NewEncoder(w).Encode(map[string]any{"tree": []any{}})
	})
	mux.HandleFunc("/repos/acme/docs/issues", func(w http.ResponseWriter, r *http.Request) {
		_ = json.NewEncoder(w).Encode([]any{map[string]any{
			"id": 99, "number": 7, "title": "Bug", "body": "## Problem\n\nIt breaks.\n\n- fix me",
			"created_at": "2026-01-01T00:00:00Z", "updated_at": "2026-01-03T00:00:00Z",
			"html_url": "https://github.com/acme/docs/issues/7",
			"user":     map[string]any{"login": "alice"},
		}})
	})
	mux.HandleFunc("/repos/acme/docs/issues/7/comments", func(w http.ResponseWriter, r *http.Request) {
		_ = json.NewEncoder(w).Encode([]any{
			map[string]any{"id": 100, "body": "first", "created_at": "2026-01-02T00:00:00Z",
				"html_url": "https://github.com/acme/docs/issues/7#issuecomment-100", "user": map[string]any{"login": "bob"}},
			map[string]any{"id": 101, "body": "second", "created_at": "2026-01-03T00:00:00Z",
				"html_url": "https://github.com/acme/docs/issues/7#issuecomment-101", "user": map[string]any{"login": "alice"}},
		})
	})
	mux.HandleFunc("/repos/acme/docs/pulls", func(w http.ResponseWriter, r *http.Request) {
		if failPulls.Load() {
			w.WriteHeader(http.StatusInternalServerError)
			return
		}
		_ = json.NewEncoder(w).Encode([]any{})
	})
	srv := httptest.NewTLSServer(mux)
	defer srv.Close()
	f, connID := newGitHubConnectorFixture(t, srv)
	if _, err := f.db.SQL.ExecContext(ctx,
		`INSERT INTO knowledge_collections (tenant_id, id, name, qdrant_collection) VALUES ($1, $2, $2, 'qdr_' || $2)`,
		f.tenant, f.connection.TargetCollectionID); err != nil {
		t.Fatal(err)
	}
	// Objects the pre-v1.2 adapter emitted: one per comment.
	legacies := make([]source.Object, 0, 2)
	for i, seed := range []struct{ id, kind, nativeID string }{
		{"legacy-comment-100", "comment", "github:" + connID + ":acme/docs:comment:100"},
		{"legacy-review-comment-900", "review_comment", "github:" + connID + ":acme/docs:review_comment:900"},
	} {
		obj, err := f.sourceRepo.UpsertObject(ctx, source.Object{
			TenantID: f.tenant, ConnectionID: connID, ID: seed.id, Kind: seed.kind, NativeID: seed.nativeID,
			AuthorizationMode: source.AuthorizationCollection, ObservedSeq: int64(i + 1),
		})
		if err != nil {
			t.Fatal(err)
		}
		if err := f.sourceRepo.InsertObjectVersion(ctx, f.tenant, source.ObjectVersion{
			ObjectID: obj.ID, ObservedSeq: int64(i + 1), ArtifactRef: "art-legacy-" + seed.id,
			SHA256: strings.Repeat("a", 64), ByteSize: 1, MimeType: "text/markdown",
			OriginalFilename: "legacy.md", Provenance: "{}",
		}); err != nil {
			t.Fatal(err)
		}
		legacies = append(legacies, obj)
	}
	assertAlive := func(wantDeleted bool, legacies ...source.Object) {
		t.Helper()
		for _, legacy := range legacies {
			stored, err := f.sourceRepo.GetObject(ctx, f.tenant, legacy.ID)
			if err != nil {
				t.Fatal(err)
			}
			if wantDeleted && stored.DeletedAt == nil {
				t.Fatalf("complete sync kept legacy object %s", legacy.NativeID)
			}
			if !wantDeleted && stored.DeletedAt != nil {
				t.Fatalf("incomplete sync deleted legacy object %s", legacy.NativeID)
			}
		}
	}
	// An interrupted listing (the pulls phase fails) deletes nothing it did
	// not replace: the observed issue document retires its own comment
	// (Supersedes), the unseen PR's review comment survives.
	if _, err := f.svc.SyncSourceWindowed(ctx, f.principal, connID, 1); err != nil {
		t.Fatal(err)
	}
	if _, err := f.runNext(t); err == nil {
		t.Fatal("sync with failing pulls phase unexpectedly succeeded")
	}
	assertAlive(true, legacies[0])
	assertAlive(false, legacies[1])
	// The first complete snapshot sync folds comments into the issue
	// document and tombstones the legacy comment items.
	failPulls.Store(false)
	if _, err := f.svc.SyncSourceWindowed(ctx, f.principal, connID, 2); err != nil {
		t.Fatal(err)
	}
	if _, err := f.runNext(t); err != nil {
		t.Fatal(err)
	}
	assertAlive(true, legacies...)
	obj, err := f.sourceRepo.ObjectForNativeID(ctx, f.tenant, connID, "issues", "github:"+connID+":acme/docs:issues:99")
	if err != nil {
		t.Fatal(err)
	}
	if obj.DeletedAt != nil {
		t.Fatal("issue document was tombstoned")
	}
	version, err := f.sourceRepo.LatestObjectVersion(ctx, f.tenant, obj.ID)
	if err != nil {
		t.Fatal(err)
	}
	if version.MimeType != source.BlocksMIMEType || version.OriginalFilename != "issue-7.blocks.json" {
		t.Fatalf("issue version is not a structured document: %+v", version)
	}
	var provenance map[string]any
	if err := json.Unmarshal([]byte(version.Provenance), &provenance); err != nil {
		t.Fatal(err)
	}
	if provenance["container"] != "acme/docs" {
		t.Fatalf("provenance container = %v", provenance["container"])
	}
	if authors, _ := provenance["authors"].([]any); !reflect.DeepEqual(authors, []any{"alice", "bob"}) {
		t.Fatalf("provenance authors = %v", provenance["authors"])
	}
	// Re-syncing unchanged content keeps the same version.
	if _, err := f.svc.SyncSourceWindowed(ctx, f.principal, connID, 3); err != nil {
		t.Fatal(err)
	}
	if _, err := f.runNext(t); err != nil {
		t.Fatal(err)
	}
	again, err := f.sourceRepo.LatestObjectVersion(ctx, f.tenant, obj.ID)
	if err != nil || again.ObservedSeq != version.ObservedSeq {
		t.Fatalf("unchanged issue re-versioned: %+v err=%v", again, err)
	}
}
