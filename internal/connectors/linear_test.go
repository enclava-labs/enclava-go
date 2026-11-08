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

func linearFixture(t *testing.T, handler http.HandlerFunc) (Adapter, source.Connection) {
	t.Helper()
	server := httptest.NewServer(handler)
	t.Cleanup(server.Close)
	t.Setenv("LINEAR_TEST_KEY", "linear-secret")
	resolver, err := auth.NewSecretResolver([]auth.SecretBinding{{RefID: "linear-key", TenantID: "tenant", Purpose: auth.SecretPurposeConnector, Origins: []string{server.URL}, EnvName: "LINEAR_TEST_KEY"}})
	if err != nil {
		t.Fatal(err)
	}
	conn := source.Connection{ID: "conn", TenantID: "tenant", Kind: source.ConnectionLinear, CredentialRef: "linear-key", Config: map[string]any{"base_url": server.URL, "team_keys": "ENG, OPS"}}
	adapter, err := NewLinearAdapter(conn, resolver)
	if err != nil {
		t.Fatal(err)
	}
	return adapter, conn
}

func linearTestIssue(id, identifier string, overflow bool) map[string]any {
	return map[string]any{
		"id": id, "identifier": identifier, "title": "Example", "description": "description", "url": "https://linear.app/example", "priorityLabel": "High", "createdAt": "2026-01-01T00:00:00Z", "updatedAt": "2026-01-02T00:00:00Z",
		"team": map[string]any{"key": "ENG", "name": "Engineering"}, "state": map[string]any{"name": "Started"}, "creator": map[string]any{"name": "Alice"},
		"comments": map[string]any{"nodes": []any{map[string]any{"id": "c1", "body": "first comment", "createdAt": "2026-01-01T01:00:00Z", "user": nil}}, "pageInfo": map[string]any{"hasNextPage": overflow, "endCursor": "comment-next"}},
	}
}

func TestLinearFullAndDelta(t *testing.T) {
	issueCalls, commentCalls, overflowCalls := 0, 0, 0
	adapter, conn := linearFixture(t, func(w http.ResponseWriter, r *http.Request) {
		if r.Method != http.MethodPost || r.URL.Path != "/graphql" || r.Header.Get("Authorization") != "linear-secret" {
			t.Errorf("bad request %s %s auth=%q", r.Method, r.URL.Path, r.Header.Get("Authorization"))
		}
		var req struct {
			Query     string         `json:"query"`
			Variables map[string]any `json:"variables"`
		}
		if err := json.NewDecoder(r.Body).Decode(&req); err != nil {
			t.Error(err)
			return
		}
		var data any
		switch {
		case strings.Contains(req.Query, "query LinearIssueComments"):
			overflowCalls++
			if req.Variables["id"] != "one" || req.Variables["after"] != "comment-next" {
				t.Errorf("bad overflow variables: %v", req.Variables)
			}
			data = map[string]any{"issue": map[string]any{"comments": map[string]any{"nodes": []any{map[string]any{"id": "c2", "body": "second comment", "createdAt": "2026-01-01T02:00:00Z", "user": map[string]any{"name": "Bob"}}}, "pageInfo": map[string]any{"hasNextPage": false}}}}
		case strings.Contains(req.Query, "query LinearComments"):
			commentCalls++
			filter := req.Variables["filter"].(map[string]any)
			if filter["updatedAt"].(map[string]any)["gt"] == "" || filter["issue"].(map[string]any)["team"] == nil {
				t.Errorf("bad comment filter: %v", filter)
			}
			data = map[string]any{"comments": map[string]any{"nodes": []any{map[string]any{"issue": map[string]any{"id": "two"}}, map[string]any{"issue": map[string]any{"id": "two"}}}, "pageInfo": map[string]any{"hasNextPage": false}}}
		case strings.Contains(req.Query, "query LinearIssues"):
			issueCalls++
			filter := req.Variables["filter"].(map[string]any)
			var nodes []any
			next := false
			switch issueCalls {
			case 1, 2:
				team := filter["team"].(map[string]any)["key"].(map[string]any)["in"].([]any)
				if len(team) != 2 || team[0] != "ENG" || team[1] != "OPS" {
					t.Errorf("bad team filter: %v", filter)
				}
				if issueCalls == 1 {
					nodes = []any{linearTestIssue("one", "ENG-1", true)}
					next = true
				} else {
					if req.Variables["after"] != "page-2" {
						t.Errorf("missing issues cursor")
					}
					nodes = []any{linearTestIssue("two", "ENG-2", false)}
				}
			case 3:
				if filter["updatedAt"].(map[string]any)["gt"] == "" || filter["team"] == nil {
					t.Errorf("bad delta filter: %v", filter)
				}
				nodes = []any{linearTestIssue("three", "ENG-3", false)}
			case 4:
				ids := filter["id"].(map[string]any)["in"].([]any)
				if len(ids) != 1 || ids[0] != "two" {
					t.Errorf("bad ids: %v", ids)
				}
				nodes = []any{linearTestIssue("two", "ENG-2", false)}
			default:
				t.Errorf("unexpected issues call %d", issueCalls)
			}
			data = map[string]any{"issues": map[string]any{"nodes": nodes, "pageInfo": map[string]any{"hasNextPage": next, "endCursor": "page-2"}}}
		default:
			t.Errorf("unexpected query %s", req.Query)
		}
		_ = json.NewEncoder(w).Encode(map[string]any{"data": data})
	})
	ctx := context.Background()
	first, err := adapter.Content(ctx, conn, "", 1)
	if err != nil || !first.Snapshot || first.NextCursor == "" || len(first.Items) != 1 {
		t.Fatalf("first: %+v %v", first, err)
	}
	item := first.Items[0]
	if item.NativeID != "linear:conn:one" || item.ParentNativeID != "linear:conn:team:ENG" || item.ParentKind != "team" || item.Title != "ENG-1 Example" || item.Filename != "ENG-1.md" || item.MIMEType != "text/markdown" || item.CreatedAt == nil || item.UpdatedAt == nil || !strings.Contains(item.Text, "**Unknown**") || !strings.Contains(item.Text, "second comment") || strings.Index(item.Text, "first comment") > strings.Index(item.Text, "second comment") {
		t.Fatalf("bad item: %+v", item)
	}
	second, err := adapter.Content(ctx, conn, first.NextCursor, 1)
	if err != nil || !second.Snapshot || second.Checkpoint == "" || second.NextCursor != "" || len(second.Items) != 1 {
		t.Fatalf("second: %+v %v", second, err)
	}
	delta, err := adapter.Content(ctx, conn, second.Checkpoint, 1)
	if err != nil || delta.Snapshot || delta.NextCursor == "" || len(delta.Items) != 1 || delta.Items[0].NativeID != "linear:conn:three" {
		t.Fatalf("delta: %+v %v", delta, err)
	}
	changed, err := adapter.Content(ctx, conn, delta.NextCursor, 1)
	if err != nil || changed.Snapshot || changed.Checkpoint == "" || len(changed.Items) != 1 || changed.Items[0].NativeID != "linear:conn:two" {
		t.Fatalf("changed: %+v %v", changed, err)
	}
	if issueCalls != 4 || commentCalls != 1 || overflowCalls != 1 {
		t.Fatalf("calls: %d %d %d", issueCalls, commentCalls, overflowCalls)
	}
}

func TestLinearRetryAndGraphQLError(t *testing.T) {
	calls := 0
	adapter, conn := linearFixture(t, func(w http.ResponseWriter, r *http.Request) {
		calls++
		if calls == 1 {
			w.WriteHeader(http.StatusTooManyRequests)
			return
		}
		if calls == 2 {
			_, _ = fmt.Fprint(w, `{"data":{"issues":{"nodes":[],"pageInfo":{"hasNextPage":false}}}}`)
			return
		}
		_, _ = fmt.Fprint(w, `{"errors":[{"message":"private error"}]}`)
	})
	_, err := adapter.Content(context.Background(), conn, "", 100)
	if err != nil || calls != 2 {
		t.Fatalf("retry calls=%d err=%v", calls, err)
	}
	_, err = adapter.Content(context.Background(), conn, "", 100)
	if err == nil || strings.Contains(err.Error(), "private error") {
		t.Fatalf("GraphQL error: %v", err)
	}
}

func TestLinearGraphQLRateLimitRetry(t *testing.T) {
	calls := 0
	adapter, conn := linearFixture(t, func(w http.ResponseWriter, r *http.Request) {
		calls++
		if calls == 1 {
			_, _ = fmt.Fprint(w, `{"errors":[{"extensions":{"code":"RATELIMITED"}}]}`)
			return
		}
		_, _ = fmt.Fprint(w, `{"data":{"issues":{"nodes":[],"pageInfo":{"hasNextPage":false}}}}`)
	})
	_, err := adapter.Content(context.Background(), conn, "", 100)
	if err != nil || calls != 2 {
		t.Fatalf("GraphQL retry calls=%d err=%v", calls, err)
	}
}

func TestLinearConfigValidation(t *testing.T) {
	conn := source.Connection{Kind: source.ConnectionLinear, CredentialRef: "key", Config: map[string]any{}}
	if _, err := NewLinearAdapter(conn, nil); err == nil {
		t.Fatal("missing resolver accepted")
	}
	conn.CredentialRef = ""
	if _, err := NewLinearAdapter(conn, nil); err == nil {
		t.Fatal("missing credential accepted")
	}
	conn.Kind = "other"
	if _, err := NewLinearAdapter(conn, nil); err == nil {
		t.Fatal("wrong kind accepted")
	}
	resolver, err := auth.NewSecretResolver(nil)
	if err != nil {
		t.Fatal(err)
	}
	conn.Kind, conn.CredentialRef = source.ConnectionLinear, "key"
	conn.Config["base_url"] = "http://example.com"
	if _, err := NewLinearAdapter(conn, resolver); err == nil {
		t.Fatal("insecure origin accepted")
	}
	delete(conn.Config, "base_url")
	conn.Config["team_keys"] = []any{"ENG", 3}
	if _, err := NewLinearAdapter(conn, resolver); err == nil || !strings.Contains(err.Error(), "team_keys") {
		t.Fatalf("invalid teams accepted: %v", err)
	}
}
