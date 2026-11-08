package connectors

import (
	"context"
	"encoding/base64"
	"encoding/json"
	"fmt"
	"net/http"
	"net/http/httptest"
	"regexp"
	"strconv"
	"strings"
	"testing"

	"enclava-go/internal/auth"
	"enclava-go/internal/source"
)

func jiraTestResolver(t *testing.T, origin, secret string) CredentialResolver {
	t.Helper()
	t.Setenv("JIRA_TEST_SECRET", secret)
	resolver, err := auth.NewSecretResolver([]auth.SecretBinding{{RefID: "jira-test", TenantID: "tenant", Purpose: auth.SecretPurposeConnector, Origins: []string{origin}, EnvName: "JIRA_TEST_SECRET"}})
	if err != nil {
		t.Fatal(err)
	}
	return resolver
}

func TestJiraFullDeltaAndComments(t *testing.T) {
	searches, commentPages, retries := 0, 0, 0
	wantAuth := "Basic " + base64.StdEncoding.EncodeToString([]byte("user@example.com:api-token"))
	server := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		if r.Header.Get("Authorization") != wantAuth {
			t.Errorf("authorization = %q", r.Header.Get("Authorization"))
		}
		switch r.URL.Path {
		case "/rest/api/2/search/jql":
			if retries == 0 {
				retries++
				w.Header().Set("Retry-After", "0")
				w.WriteHeader(http.StatusTooManyRequests)
				return
			}
			searches++
			q := r.URL.Query()
			if q.Get("fields") != "summary,description,status,issuetype,priority,assignee,reporter,labels,components,created,updated" || q.Get("maxResults") != "1" {
				t.Errorf("search query = %v", q)
			}
			jql := q.Get("jql")
			if searches <= 2 && jql != `project = "PROJ" ORDER BY created ASC` {
				t.Errorf("full jql = %q", jql)
			}
			if searches == 3 && !regexp.MustCompile(`^project = "PROJ" AND updated >= "-[0-9]+m" ORDER BY created ASC$`).MatchString(jql) {
				t.Errorf("delta jql = %q", jql)
			}
			key, id := "PROJ-1", "11"
			if searches == 2 {
				key, id = "PROJ-2", "12"
				if q.Get("nextPageToken") != "next-token" {
					t.Errorf("token = %q", q.Get("nextPageToken"))
				}
			}
			if searches == 3 {
				key, id = "PROJ-2", "12"
				if q.Get("nextPageToken") != "" {
					t.Errorf("delta carried token")
				}
			}
			response := map[string]any{"issues": []any{map[string]any{"id": id, "key": key, "fields": map[string]any{"summary": "Summary", "description": "Details", "status": map[string]any{"name": "Open"}, "issuetype": map[string]any{"name": "Task"}, "priority": map[string]any{"name": "High"}, "assignee": map[string]any{"displayName": "Alice"}, "reporter": map[string]any{"displayName": "Bob"}, "labels": []string{"label"}, "components": []any{map[string]any{"name": "API"}}, "created": "2026-01-02T03:04:05.000+0200", "updated": "2026-01-03T03:04:05.000+0200"}}}, "isLast": searches != 1}
			if searches == 1 {
				response["nextPageToken"] = "next-token"
			}
			_ = json.NewEncoder(w).Encode(response)
		case "/rest/api/2/issue/PROJ-1/comment", "/rest/api/2/issue/PROJ-2/comment":
			q := r.URL.Query()
			if q.Get("maxResults") != "100" || q.Get("orderBy") != "created" {
				t.Errorf("comment query = %v", q)
			}
			start, err := strconv.Atoi(q.Get("startAt"))
			if err != nil {
				t.Errorf("comment start = %q", q.Get("startAt"))
			}
			var comments []any
			total := 0
			if r.URL.Path == "/rest/api/2/issue/PROJ-1/comment" {
				commentPages++
				total = 101
				end := start + 100
				if end > total {
					end = total
				}
				for n := start; n < end; n++ {
					comments = append(comments, map[string]any{"id": strconv.Itoa(n), "author": map[string]any{"displayName": "Writer"}, "body": fmt.Sprintf("comment %d", n), "created": "2026-01-02T03:04:05.000+0200"})
				}
			}
			_ = json.NewEncoder(w).Encode(map[string]any{"comments": comments, "total": total})
		default:
			t.Errorf("unexpected path %s", r.URL.Path)
			http.NotFound(w, r)
		}
	}))
	defer server.Close()
	resolver := jiraTestResolver(t, server.URL, "user@example.com:api-token")
	conn := source.Connection{ID: "connection", TenantID: "tenant", Kind: source.ConnectionJira, CredentialRef: "jira-test", Config: map[string]any{"base_url": server.URL, "project": "PROJ"}}
	adapter, err := NewJiraAdapter(conn, resolver)
	if err != nil {
		t.Fatal(err)
	}
	if !adapter.Capabilities().Content {
		t.Fatal("missing content capability")
	}
	first, err := adapter.Content(context.Background(), conn, "", 1)
	if err != nil {
		t.Fatal(err)
	}
	if !first.Snapshot || first.NextCursor == "" || first.Checkpoint != "" || len(first.Items) != 1 {
		t.Fatalf("first page = %+v", first)
	}
	item := first.Items[0]
	if item.NativeID != "jira:connection:11" || item.ParentNativeID != "jira:connection:project:PROJ" || item.ParentKind != "project" || item.Kind != "issue" || item.Title != "PROJ-1 Summary" || item.Filename != "PROJ-1.md" || item.MIMEType != "text/markdown" || item.URL != server.URL+"/browse/PROJ-1" || item.NativeRevision != "2026-01-03T03:04:05.000+0200" {
		t.Fatalf("item = %+v", item)
	}
	if item.CreatedAt == nil || item.CreatedAt.Format("2006-01-02 15:04 MST") != "2026-01-02 01:04 UTC" || item.UpdatedAt == nil {
		t.Fatalf("timestamps = %v %v", item.CreatedAt, item.UpdatedAt)
	}
	for _, expected := range []string{"# PROJ-1 Summary", "Type: Task", "Status: Open", "Priority: High", "Assignee: Alice", "Reporter: Bob", "Labels: label", "Components: API", "Project: PROJ", "Details", "## Comments", "**Writer** — 2026-01-02 01:04 UTC", "comment 100"} {
		if !strings.Contains(item.Text, expected) {
			t.Errorf("missing %q in %s", expected, item.Text)
		}
	}
	if strings.Index(item.Text, "comment 0\n") > strings.Index(item.Text, "comment 100\n") || commentPages != 2 {
		t.Fatalf("comment ordering/pages = %d", commentPages)
	}
	second, err := adapter.Content(context.Background(), conn, first.NextCursor, 1)
	if err != nil {
		t.Fatal(err)
	}
	if !second.Snapshot || second.NextCursor != "" || second.Checkpoint == "" || second.Completeness != CompletenessComplete || len(second.Items) != 1 {
		t.Fatalf("second page = %+v", second)
	}
	delta, err := adapter.Content(context.Background(), conn, second.Checkpoint, 1)
	if err != nil {
		t.Fatal(err)
	}
	if delta.Snapshot || delta.NextCursor != "" || delta.Checkpoint == "" || len(delta.Items) != 1 || delta.Items[0].NativeID != "jira:connection:12" || searches != 3 || retries != 1 {
		t.Fatalf("delta = %+v searches=%d retries=%d", delta, searches, retries)
	}
}

func TestJiraConstructorValidation(t *testing.T) {
	server := httptest.NewServer(http.HandlerFunc(func(http.ResponseWriter, *http.Request) {}))
	defer server.Close()
	resolver := jiraTestResolver(t, server.URL, "invalid")
	base := source.Connection{ID: "conn", TenantID: "tenant", Kind: source.ConnectionJira, CredentialRef: "jira-test"}
	for _, tc := range []struct {
		name   string
		config map[string]any
		want   string
	}{
		{"missing base", map[string]any{"project": "P"}, "base_url"},
		{"missing project", map[string]any{"base_url": server.URL}, "project"},
		{"invalid origin", map[string]any{"base_url": "http://example.com", "project": "P"}, "https"},
		{"path in origin", map[string]any{"base_url": server.URL + "/path", "project": "P"}, "origin"},
		{"project control", map[string]any{"base_url": server.URL, "project": "P\nX"}, "control"},
		{"invalid credential", map[string]any{"base_url": server.URL, "project": "P"}, "email:api-token"},
	} {
		t.Run(tc.name, func(t *testing.T) {
			conn := base
			conn.Config = tc.config
			_, err := NewJiraAdapter(conn, resolver)
			if err == nil || !strings.Contains(err.Error(), tc.want) {
				t.Fatalf("error = %v, want %q", err, tc.want)
			}
		})
	}
}

func TestJiraEscapesProjectJQL(t *testing.T) {
	server := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		if got := r.URL.Query().Get("jql"); got != `project = "A\"B\\C" ORDER BY created ASC` {
			t.Errorf("jql = %q", got)
		}
		_, _ = w.Write([]byte(`{"issues":[],"isLast":true}`))
	}))
	defer server.Close()
	resolver := jiraTestResolver(t, server.URL, "a:b")
	conn := source.Connection{ID: "conn", TenantID: "tenant", Kind: source.ConnectionJira, CredentialRef: "jira-test", Config: map[string]any{"base_url": server.URL, "project": `A"B\C`}}
	adapter, err := NewJiraAdapter(conn, resolver)
	if err != nil {
		t.Fatal(err)
	}
	if _, err := adapter.Content(context.Background(), conn, "", 1); err != nil {
		t.Fatal(err)
	}
}
