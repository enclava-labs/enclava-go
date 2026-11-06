package connectors

import (
	"context"
	"io"
	"net/http"
	"net/http/httptest"
	"strings"
	"testing"
	"time"

	"enclava-go/internal/auth"
	"enclava-go/internal/source"
)

type teamsRoundTripper func(*http.Request) (*http.Response, error)

func (f teamsRoundTripper) RoundTrip(r *http.Request) (*http.Response, error) { return f(r) }

type teamsGraphFixture struct {
	deltaCalls     int
	deltaLinkCalls int
	chatDeltaCalls int
	throttleDelta  bool
	throttled      bool
	changed        bool
	t              *testing.T
}

func (f *teamsGraphFixture) handler(w http.ResponseWriter, r *http.Request) {
	if f.t != nil && r.Header.Get("Authorization") != "Bearer fixture-token" {
		f.t.Errorf("authorization header = %q", r.Header.Get("Authorization"))
	}
	switch r.URL.Path {
	case "/v1/teams/team/channels":
		if r.URL.Query().Get("page") == "2" {
			_, _ = io.WriteString(w, `{"value":[{"id":"channel2","membershipType":"shared"},{"id":"channel3","membershipType":"standard"}]}`)
		} else {
			_, _ = io.WriteString(w, `{"value":[{"id":"channel","membershipType":"private"}],"@odata.nextLink":"http://`+r.Host+`/v1/teams/team/channels?page=2"}`) //nolint:gosec // test fixture: r.Host is the hermetic httptest server
		}
	case "/v1/teams/team/channels/channel2/messages", "/v1/teams/team/channels/channel3/messages":
		_, _ = io.WriteString(w, `{"value":[]}`)
	case "/v1/teams/team/channels/channel/messages":
		_, _ = io.WriteString(w, `{"value":[{"id":"parent","body":{"content":"message"},"webUrl":"https://teams.example/message","createdDateTime":"2026-10-20T00:00:00Z","lastModifiedDateTime":"2026-10-21T00:00:00Z","attachments":[{"id":"file1","name":"a.txt","contentType":"text/plain","contentUrl":"https://files.example/a"}]}]}`)
	case "/v1/teams/team/channels/channel2/messages/delta", "/v1/teams/team/channels/channel3/messages/delta":
		f.deltaCalls++
		_, _ = io.WriteString(w, `{"value":[],"@odata.deltaLink":"`+"http://"+r.Host+`/v1/teams/team/channels/`+strings.Split(r.URL.Path, "/")[5]+`/messages/delta?$deltatoken=empty"}`) //nolint:gosec // test fixture: r.Host is the hermetic httptest server
	case "/v1/teams/team/channels/channel/messages/delta":
		if r.URL.Query().Get("$deltatoken") == "fresh" {
			f.deltaLinkCalls++
			if f.throttleDelta && !f.throttled {
				f.throttled = true
				w.Header().Set("Retry-After", "0")
				w.WriteHeader(http.StatusTooManyRequests)
				return
			}
			if f.changed {
				_, _ = io.WriteString(w, `{"value":[{"id":"parent","body":{"content":"edited"},"webUrl":"https://teams.example/message","lastModifiedDateTime":"2026-11-01T00:00:00Z"},{"id":"ghost","deletedDateTime":"2026-11-01T00:00:00Z"},{"id":"new","body":{"content":"added"},"webUrl":"https://teams.example/new","lastModifiedDateTime":"2026-11-01T01:00:00Z"}],"@odata.deltaLink":"http://`+r.Host+`/v1/teams/team/channels/channel/messages/delta?$deltatoken=fresher"}`) //nolint:gosec // test fixture: r.Host is the hermetic httptest server
			} else {
				_, _ = io.WriteString(w, `{"value":[],"@odata.deltaLink":"http://`+r.Host+`/v1/teams/team/channels/channel/messages/delta?$deltatoken=fresher"}`) //nolint:gosec // test fixture: r.Host is the hermetic httptest server
			}
			return
		}
		f.deltaCalls++
		if f.throttleDelta && !f.throttled {
			f.throttled = true
			w.Header().Set("Retry-After", "0")
			w.WriteHeader(http.StatusTooManyRequests)
			return
		}
		_, _ = io.WriteString(w, `{"value":[{"id":"parent","body":{"content":"message"},"webUrl":"https://teams.example/message","createdDateTime":"2026-10-20T00:00:00Z","lastModifiedDateTime":"2026-10-21T00:00:00Z","attachments":[{"id":"file1","name":"a.txt","contentType":"text/plain","contentUrl":"https://files.example/a"}]}],"@odata.deltaLink":"http://`+r.Host+`/v1/teams/team/channels/channel/messages/delta?$deltatoken=fresh"}`) //nolint:gosec // test fixture: r.Host is the hermetic httptest server
	case "/v1/teams/team/channels/channel/messages/parent/replies":
		_, _ = io.WriteString(w, `{"value":[{"id":"reply","body":{"content":"answer"},"webUrl":"https://teams.example/reply"}]}`)
	case "/v1/teams/team/channels/channel/messages/new/replies":
		_, _ = io.WriteString(w, `{"value":[]}`)
	case "/v1/chats/chat/messages":
		if r.URL.Query().Get("$filter") != "" {
			f.chatDeltaCalls++
			if !strings.Contains(r.URL.Query().Get("$filter"), "lastModifiedDateTime gt 2026-10-31T23:55:00Z") {
				f.t.Errorf("chat delta filter = %q", r.URL.Query().Get("$filter"))
			}
			if got := r.URL.Query().Get("$orderby"); got != "lastModifiedDateTime desc" {
				f.t.Errorf("chat delta orderby = %q", got)
			}
			_, _ = io.WriteString(w, `{"value":[{"id":"chatnew","body":{"content":"fresh"},"webUrl":"https://teams.example/chat-new","lastModifiedDateTime":"2026-11-02T00:00:00Z"}]}`)
			return
		}
		_, _ = io.WriteString(w, `{"value":[{"id":"chatmsg","body":{"content":"chat"},"webUrl":"https://teams.example/chat","lastModifiedDateTime":"2026-11-01T00:00:00Z"}]}`)
	default:
		http.NotFound(w, r)
	}
}

func newTeamsFixture(t *testing.T) (*TeamsAdapter, source.Connection, *teamsGraphFixture) {
	t.Helper()
	fx := &teamsGraphFixture{t: t}
	server := httptest.NewServer(http.HandlerFunc(fx.handler))
	t.Cleanup(server.Close)
	resolver, err := auth.NewSecretResolver([]auth.SecretBinding{{RefID: "teams-fixture", TenantID: "tenant", Purpose: auth.SecretPurposeConnector, EnvName: "TEAMS_FIXTURE_SECRET", Origins: []string{server.URL}}})
	if err != nil {
		t.Fatal(err)
	}
	t.Setenv("TEAMS_FIXTURE_SECRET", "sentinel")
	transport := teamsRoundTripper(func(r *http.Request) (*http.Response, error) {
		if strings.HasPrefix(r.URL.Host, "login.microsoftonline.com") {
			return &http.Response{StatusCode: 200, Header: http.Header{"Content-Type": []string{"application/json"}}, Body: io.NopCloser(strings.NewReader(`{"access_token":"fixture-token"}`)), Request: r}, nil
		}
		r.URL.Scheme = "http"
		r.URL.Host = strings.TrimPrefix(server.URL, "http://")
		return http.DefaultTransport.RoundTrip(r)
	})
	connection := source.Connection{TenantID: "tenant", ID: "conn", Kind: source.ConnectionTeams, CredentialRef: "teams-fixture", Config: map[string]any{"tenant_id": "directory", "client_id": "client", "graph_base_url": server.URL + "/v1", "team_id": "team", "chat_ids": []any{"chat"}}}
	adapter, err := NewTeamsAdapter(connection, resolver)
	if err != nil {
		t.Fatal(err)
	}
	a := adapter.(*TeamsAdapter)
	a.client = &http.Client{Transport: transport}
	return a, connection, fx
}

func TestTeamsAdapterPagesChannelRepliesAndChats(t *testing.T) {
	a, connection, fx := newTeamsFixture(t)
	fx.throttleDelta = true
	page, err := a.Content(context.Background(), connection, "", 2)
	if err != nil {
		t.Fatal(err)
	}
	if len(page.Items) != 2 || page.NextCursor == "" || page.Snapshot {
		t.Fatalf("first page = %#v", page)
	}
	if page.Items[0].NativeID != "teams:team:channel:parent" || page.Items[0].Kind != "teams_channel_message:private" || page.Items[0].URL != "https://teams.example/message" || page.Items[0].Title != "" || page.Items[0].ParentNativeID != "channel" || page.Items[0].ParentKind != "teams_channel" {
		t.Fatalf("channel item = %#v", page.Items[0])
	}
	if page.Items[1].NativeID != "teams:team:channel:parent:attachment:file1" || page.Items[1].ParentNativeID != "channel" || page.Items[1].ParentKind != "teams_channel" || page.Items[1].Title != "a.txt" {
		t.Fatalf("attachment item = %#v", page.Items[1])
	}
	page, err = a.Content(context.Background(), connection, page.NextCursor, 2)
	if err != nil {
		t.Fatal(err)
	}
	if len(page.Items) != 2 || !page.Snapshot || page.NextCursor != "" || page.Checkpoint == "" {
		t.Fatalf("terminal page = %#v", page)
	}
	if page.Items[0].NativeID != "teams:team:channel:reply" || page.Items[0].ParentNativeID != "channel" || page.Items[0].ParentKind != "teams_channel" || page.Items[0].Title != "" {
		t.Fatalf("reply = %#v", page.Items[0])
	}
	if page.Items[1].NativeID != "teams:chat:chat:chatmsg" || page.Items[1].ParentNativeID != "chat" || page.Items[1].ParentKind != "teams_chat" || page.Items[1].Kind != "teams_chat_message" || page.Items[1].Title != "" {
		t.Fatalf("chat message = %#v", page.Items[1])
	}
	checkpoint, err := decodeTeamsContentCursor(page.Checkpoint)
	if err != nil {
		t.Fatalf("checkpoint decode: %v", err)
	}
	if checkpoint.FullAt == "" || len(checkpoint.DeltaLinks) != 3 || !strings.Contains(checkpoint.DeltaLinks["team\x00channel"], "deltatoken=fresh") {
		t.Fatalf("checkpoint = %#v", checkpoint)
	}
	if checkpoint.ChatMarks["chat"] != "2026-11-01T00:00:00Z" {
		t.Fatalf("chat watermark = %#v", checkpoint.ChatMarks)
	}
	// Two Content calls each run one delta-link round per channel; the first also retries a 429.
	if fx.deltaCalls != 7 {
		t.Fatalf("delta-link rounds = %d, want 7 (429 retry + 3 channels x 2 pages)", fx.deltaCalls)
	}
}

func TestTeamsAdapterDeltaRunUsesStoredDeltaLink(t *testing.T) {
	a, connection, fx := newTeamsFixture(t)
	fx.changed = true
	cursor := encodeTeamsContentCursor(teamsContentCursor{
		FullAt:     time.Now().Format(time.RFC3339),
		DeltaLinks: map[string]string{"team\x00channel": a.baseURL + "/teams/team/channels/channel/messages/delta?$deltatoken=fresh"},
		ChatMarks:  map[string]string{"chat": "2026-11-01T00:00:00Z"},
	})
	// Page size 2 forces a multi-page run; the stored deltaLink must stay the
	// run's listing source so every changed item appears exactly once.
	ids := make([]string, 0)
	seen := map[string]int{}
	var last ContentPage
	for range 10 {
		page, err := a.Content(context.Background(), connection, cursor, 2)
		if err != nil {
			t.Fatal(err)
		}
		if page.Snapshot {
			t.Fatalf("delta page set snapshot: %#v", page)
		}
		for _, item := range page.Items {
			seen[item.NativeID]++
			ids = append(ids, item.NativeID)
		}
		last = page
		cursor = page.NextCursor
		if cursor == "" {
			break
		}
	}
	if cursor != "" {
		t.Fatal("delta run did not terminate")
	}
	if last.Checkpoint == "" || last.NextCursor != "" {
		t.Fatalf("terminal delta page = %#v", last)
	}
	if fx.deltaLinkCalls < 2 || fx.deltaCalls == 0 {
		t.Fatalf("delta link calls = %d, fresh delta calls = %d", fx.deltaLinkCalls, fx.deltaCalls)
	}
	if fx.chatDeltaCalls < 1 {
		t.Fatalf("chat delta calls = %d", fx.chatDeltaCalls)
	}
	for id, n := range seen {
		if n != 1 {
			t.Fatalf("item %s emitted %d times", id, n)
		}
	}
	joined := strings.Join(ids, ",")
	if !strings.Contains(joined, "teams:team:channel:parent") || !strings.Contains(joined, "teams:team:channel:new") || strings.Contains(joined, "ghost") || !strings.Contains(joined, "teams:chat:chat:chatnew") {
		t.Fatalf("delta items = %v", ids)
	}
	checkpoint, err := decodeTeamsContentCursor(last.Checkpoint)
	if err != nil {
		t.Fatalf("checkpoint decode: %v", err)
	}
	if !strings.Contains(checkpoint.DeltaLinks["team\x00channel"], "deltatoken=fresher") || checkpoint.ChatMarks["chat"] != "2026-11-02T00:00:00Z" {
		t.Fatalf("delta checkpoint = %#v", checkpoint)
	}
}

func TestTeamsAdapterFullSyncAfterInterval(t *testing.T) {
	a, connection, fx := newTeamsFixture(t)
	cursor := encodeTeamsContentCursor(teamsContentCursor{
		FullAt:     time.Now().Add(-48 * time.Hour).Format(time.RFC3339),
		DeltaLinks: map[string]string{"team\x00channel": a.baseURL + "/teams/team/channels/channel/messages/delta?$deltatoken=fresh"},
		ChatMarks:  map[string]string{"chat": "2026-11-01T00:00:00Z"},
	})
	page, err := a.Content(context.Background(), connection, cursor, 10)
	if err != nil {
		t.Fatal(err)
	}
	if !page.Snapshot || page.Checkpoint == "" {
		t.Fatalf("full rerun page = %#v", page)
	}
	if fx.deltaCalls != 3 || fx.deltaLinkCalls != 0 || fx.chatDeltaCalls != 0 {
		t.Fatalf("delta calls = %d, link calls = %d, chat delta calls = %d", fx.deltaCalls, fx.deltaLinkCalls, fx.chatDeltaCalls)
	}
}

func TestTeamsAdapterLegacyOffsetCursor(t *testing.T) {
	a, connection, _ := newTeamsFixture(t)
	page, err := a.Content(context.Background(), connection, "2", 2)
	if err != nil {
		t.Fatal(err)
	}
	if len(page.Items) != 2 || !page.Snapshot || page.Checkpoint == "" {
		t.Fatalf("legacy cursor page = %#v", page)
	}
	if page.Items[0].NativeID != "teams:team:channel:reply" || page.Items[1].NativeID != "teams:chat:chat:chatmsg" {
		t.Fatalf("legacy cursor items = %#v", page.Items)
	}
}

func TestTeamsAdapterRejectsUnconfiguredScopeAndBadCursor(t *testing.T) {
	connection := source.Connection{TenantID: "tenant", ID: "conn", Kind: source.ConnectionTeams, CredentialRef: "ref"}
	adapter, err := NewTeamsAdapter(connection, &auth.SecretResolver{})
	if err != nil {
		t.Fatal(err)
	}
	if _, err := adapter.Content(context.Background(), connection, "invalid", 10); err == nil {
		t.Fatal("expected bad cursor rejection")
	}
	if _, err := adapter.Content(context.Background(), connection, "content:not-base64!!", 10); err == nil {
		t.Fatal("expected bad encoded cursor rejection")
	}
	if _, err := NewTeamsAdapter(source.Connection{Kind: source.ConnectionSlack}, &auth.SecretResolver{}); err == nil {
		t.Fatal("expected kind validation")
	}
}
