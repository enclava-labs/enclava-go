package connectors

import (
	"context"
	"io"
	"net/http"
	"net/http/httptest"
	"strings"
	"testing"

	"enclava-go/internal/auth"
	"enclava-go/internal/source"
)

type teamsRoundTripper func(*http.Request) (*http.Response, error)

func (f teamsRoundTripper) RoundTrip(r *http.Request) (*http.Response, error) { return f(r) }

func TestTeamsAdapterPagesChannelRepliesAndChats(t *testing.T) {
	var requests []string
	var messageRequests int
	server := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		if r.URL.Path == "/v1/teams/team/channels/channel/messages" {
			messageRequests++
			if messageRequests == 1 {
				w.Header().Set("Retry-After", "0")
				w.WriteHeader(http.StatusTooManyRequests)
				return
			}
		}
		requests = append(requests, r.URL.RequestURI())
		if r.Header.Get("Authorization") != "Bearer fixture-token" {
			t.Errorf("authorization header = %q", r.Header.Get("Authorization"))
		}
		switch r.URL.Path {
		case "/v1/teams/team/members":
			io.WriteString(w, `{"value":[{"id":"member","userId":"entra-user","tenantId":"directory"}]}`)
		case "/v1/teams/team/channels":
			if r.URL.Query().Get("page") == "2" {
				_, _ = io.WriteString(w, `{"value":[{"id":"channel2","membershipType":"shared"},{"id":"channel3","membershipType":"standard"}]}`)
			} else {
				io.WriteString(w, `{"value":[{"id":"channel","membershipType":"private"}],"@odata.nextLink":"http://`+r.Host+`/v1/teams/team/channels?page=2"}`)
			}
		case "/v1/teams/team/channels/channel2/messages", "/v1/teams/team/channels/channel3/messages":
			io.WriteString(w, `{"value":[]}`)
		case "/v1/teams/team/channels/channel2/allMembers":
			_, _ = io.WriteString(w, `{"value":[{"id":"shared-member","userId":"shared-user","tenantId":"directory"}]}`)
		case "/v1/teams/team/channels/channel/members":
			io.WriteString(w, `{"value":[{"id":"channel-member","userId":"private-user","tenantId":"directory"}]}`)
		case "/v1/chats/chat/members":
			io.WriteString(w, `{"value":[{"id":"chat-member","userId":"chat-user","tenantId":"directory"}]}`)
		case "/v1/teams/team/channels/channel/messages":
			io.WriteString(w, `{"value":[{"id":"parent","body":{"content":"message"},"webUrl":"https://teams.example/message","createdDateTime":"2026-10-20T00:00:00Z","lastModifiedDateTime":"2026-10-21T00:00:00Z","attachments":[{"id":"file1","name":"a.txt","contentType":"text/plain","contentUrl":"https://files.example/a"}]}]}`)
		case "/v1/teams/team/channels/channel/messages/parent/replies":
			io.WriteString(w, `{"value":[{"id":"reply","body":{"content":"answer"},"webUrl":"https://teams.example/reply"}]}`)
		case "/v1/chats/chat/messages":
			io.WriteString(w, `{"value":[{"id":"chatmsg","body":{"content":"chat"},"webUrl":"https://teams.example/chat"}]}`)
		default:
			http.NotFound(w, r)
		}
	}))
	defer server.Close()
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
	page, err := a.Content(context.Background(), connection, "", 2)
	if err != nil {
		t.Fatal(err)
	}
	if len(page.Items) != 2 || page.NextCursor != "2" || page.Snapshot {
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
	if len(page.Items) != 2 || !page.Snapshot || page.NextCursor != "" {
		t.Fatalf("terminal page = %#v", page)
	}
	if page.Items[0].NativeID != "teams:team:channel:reply" || page.Items[0].ParentNativeID != "channel" || page.Items[0].ParentKind != "teams_channel" || page.Items[0].Title != "" {
		t.Fatalf("reply = %#v", page.Items[0])
	}
	if page.Items[1].NativeID != "teams:chat:chat:chatmsg" || page.Items[1].ParentNativeID != "chat" || page.Items[1].ParentKind != "teams_chat" || page.Items[1].Kind != "teams_chat_message" || page.Items[1].Title != "" {
		t.Fatalf("chat message = %#v", page.Items[1])
	}
	// 429 + page-1 + page-2 listings; the retry itself is proven by the
	// later successful listings (a surfaced 429 would have failed earlier).
	if messageRequests != 3 {
		t.Fatalf("messages requests = %d, want 3 (429 retry + two pages)", messageRequests)
	}
	members, err := a.Membership(context.Background(), connection, "", 2)
	if err != nil {
		t.Fatal(err)
	}
	if len(members.Memberships) != 2 || members.Snapshot || members.NextCursor != "2" || members.Memberships[0].NativePrincipalID != "directory:entra-user" {
		t.Fatalf("first membership page = %#v", members)
	}
	members, err = a.Membership(context.Background(), connection, members.NextCursor, 2)
	if err != nil {
		t.Fatal(err)
	}
	if len(members.Memberships) != 2 || members.Snapshot || members.NextCursor != "4" {
		t.Fatalf("second membership page = %#v", members)
	}
	members, err = a.Membership(context.Background(), connection, members.NextCursor, 2)
	if err != nil {
		t.Fatal(err)
	}
	if len(members.Memberships) != 1 || !members.Snapshot || members.Memberships[0].Scope != "member" {
		t.Fatalf("terminal membership page = %#v", members)
	}
	access, err := a.Access(context.Background(), connection, "", 100)
	if err != nil {
		t.Fatal(err)
	}
	want := map[string]struct{ kind, principal string }{
		"team":     {"teams_team", "directory:entra-user"},
		"channel":  {"teams_channel", "directory:private-user"},
		"channel2": {"teams_channel", "directory:shared-user"},
		"channel3": {"teams_channel", "directory:entra-user"},
		"chat":     {"teams_chat", "directory:chat-user"},
	}
	for _, grant := range access.Grants {
		expected, ok := want[grant.NativeObjectID]
		if !ok || expected.kind != grant.NativeObjectKind || expected.principal != grant.NativePrincipalID || grant.Scope != "member" {
			t.Fatalf("unexpected grant: %#v", grant)
		}
		delete(want, grant.NativeObjectID)
	}
	if len(want) != 0 {
		t.Fatalf("missing grants: %#v", want)
	}
	if len(requests) == 0 {
		t.Fatal("no Graph requests")
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
	if _, err := NewTeamsAdapter(source.Connection{Kind: source.ConnectionSlack}, &auth.SecretResolver{}); err == nil {
		t.Fatal("expected kind validation")
	}
}
