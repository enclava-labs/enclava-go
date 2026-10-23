package connectors

import (
	"context"
	"fmt"
	"net/http"
	"net/http/httptest"
	"testing"

	"enclava-go/internal/source"
)

func TestSlackThreadsAndCursors(t *testing.T) {
	var history int
	server := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		if r.Header.Get("Authorization") != "Bearer test-token" { t.Errorf("authorization = %q", r.Header.Get("Authorization")) }
		switch r.URL.Path {
		case "/conversations.history":
			history++
			if r.URL.Query().Get("cursor") == "next" {
				fmt.Fprint(w, `{"ok":true,"messages":[{"user":"U2","text":"later","ts":"2.000002"}],"has_more":false}`)
			} else {
				fmt.Fprint(w, `{"ok":true,"messages":[{"user":"U1","text":"root","ts":"1.000001","reply_count":1}],"has_more":true,"response_metadata":{"next_cursor":"next"}}`)
			}
		case "/conversations.replies":
			fmt.Fprint(w, `{"ok":true,"messages":[{"user":"U1","text":"root","ts":"1.000001"},{"user":"U2","text":"reply","ts":"1.000002","thread_ts":"1.000001"}],"has_more":false}`)
		case "/chat.getPermalink":
			fmt.Fprintf(w, `{"ok":true,"permalink":"https://workspace.slack.com/archives/%s/p%s"}`, r.URL.Query().Get("channel"), "1000001")
		default: t.Errorf("unexpected endpoint %s", r.URL.Path); http.NotFound(w,r)
		}
	}))
	defer server.Close()
	t.Setenv("CONNECTOR_SLACK_TEST", "test-token")
	adapter, err := NewSlackAdapter(source.Connection{})
	if err != nil { t.Fatal(err) }
	a := adapter.(slackAdapter)
	a.client = server.Client()
	conn := source.Connection{Kind: source.ConnectionSlack, CredentialRef: "CONNECTOR_SLACK_TEST", Config: map[string]any{"channel_id":"C1", "base_url":server.URL}}
	page, err := a.Content(context.Background(), conn, "", 10)
	if err != nil { t.Fatal(err) }
	if page.Completeness != CompletenessComplete { t.Fatalf("completeness = %q", page.Completeness) }
	if len(page.Items) != 2 || page.Items[0].NativeID != "slack:C1:1.000001" || page.Items[1].ParentNativeID != "slack:C1:1.000001" || page.NextCursor != "next" { t.Fatalf("unexpected first page: %+v", page) }
	page, err = a.Content(context.Background(), conn, page.NextCursor, 10)
	if err != nil { t.Fatal(err) }
	if history != 2 || len(page.Items) != 1 || page.Items[0].Text != "[2.000002 U2] later" { t.Fatalf("unexpected incremental page: %+v", page) }
}

func TestSlackMembershipDeniedIsIncomplete(t *testing.T) {
	server := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		w.WriteHeader(http.StatusForbidden)
		fmt.Fprint(w, `{"ok":false,"error":"not_in_channel"}`)
	}))
	defer server.Close()
	t.Setenv("CONNECTOR_SLACK_TEST", "test-token")
	a := slackAdapter{client: server.Client()}
	conn := source.Connection{Kind: source.ConnectionSlack, CredentialRef: "CONNECTOR_SLACK_TEST", Config: map[string]any{"channel_id":"C1", "base_url":server.URL}}
	page, err := a.Membership(context.Background(), conn, "", 10)
	if err == nil || page.Completeness != CompletenessIncomplete { t.Fatalf("page=%+v err=%v", page, err) }
}

func TestSlackRateLimitReportsRetryAfter(t *testing.T) {
	server := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		w.Header().Set("Retry-After", "0")
		w.WriteHeader(http.StatusTooManyRequests)
	}))
	defer server.Close()
	t.Setenv("CONNECTOR_SLACK_TEST", "test-token")
	a := slackAdapter{client: server.Client()}
	conn := source.Connection{Kind: source.ConnectionSlack, CredentialRef: "CONNECTOR_SLACK_TEST", Config: map[string]any{"channel_id":"C1", "base_url":server.URL}}
	_, err := a.Content(context.Background(), conn, "", 10)
	if err == nil || err.Error() != "slack conversations.history rate limited (Retry-After 0)" { t.Fatalf("error=%v", err) }
}


func TestSlackFileAccessUsesFilesShares(t *testing.T) {
	server := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		switch r.URL.Path {
		case "/conversations.members":
			fmt.Fprint(w, `{"ok":true,"members":["U1"]}`)
		case "/conversations.history":
			fmt.Fprint(w, `{"ok":true,"messages":[{"user":"U1","text":"attached","ts":"1.000001","files":[{"id":"F1"}]}],"has_more":false}`)
		case "/files.info":
			fmt.Fprint(w, `{"ok":true,"file":{"id":"F1","shares":{"public":{"C2":[{"channel_id":"C2"}]}}}}`)
		default:
			t.Errorf("unexpected endpoint %s", r.URL.Path)
			http.NotFound(w, r)
		}
	}))
	defer server.Close()
	t.Setenv("CONNECTOR_SLACK_TEST", "test-token")
	a := slackAdapter{client: server.Client()}
	conn := source.Connection{Kind: source.ConnectionSlack, CredentialRef: "CONNECTOR_SLACK_TEST", Config: map[string]any{"channel_id":"C1", "base_url":server.URL}}
	page, err := a.Access(context.Background(), conn, "", 10)
	if err != nil { t.Fatal(err) }
	if len(page.Grants) != 2 || page.Grants[0].NativeObjectKind != "channel" || page.Grants[1].NativeObjectID != "slack:file:F1" || page.Grants[1].NativeGroupID != "C2" {
		t.Fatalf("file access was not sourced independently: %+v", page.Grants)
	}
}
