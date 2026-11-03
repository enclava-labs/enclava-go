//go:build live

package connectors

import (
	"context"
	"os"
	"testing"

	"enclava-go/internal/auth"
	"enclava-go/internal/source"
)

func TestLiveSlack(t *testing.T) {
	if os.Getenv("TEST_SLACK_BOT_TOKEN") == "" || os.Getenv("TEST_SLACK_CHANNEL_ID") == "" {
		t.Fatal("TEST_SLACK_BOT_TOKEN and TEST_SLACK_CHANNEL_ID are required")
	}
	const ref = "CONNECTOR_SLACK_LIVE_TEST"
	resolver, err := auth.NewSecretResolver([]auth.SecretBinding{{RefID: ref, TenantID: "tenant-live", Purpose: auth.SecretPurposeConnector, Origins: []string{"https://slack.com"}, EnvName: "TEST_SLACK_BOT_TOKEN"}})
	if err != nil {
		t.Fatal(err)
	}
	conn := source.Connection{TenantID: "tenant-live", Kind: source.ConnectionSlack, CredentialRef: ref, Config: map[string]any{"channel_id": os.Getenv("TEST_SLACK_CHANNEL_ID"), "reconcile": "true"}}
	adapter, err := NewSlackAdapter(conn, resolver)
	if err != nil {
		t.Fatal(err)
	}
	var cursor Cursor
	var content []ContentItem
	pages := 0
	for {
		page, err := adapter.Content(context.Background(), conn, cursor, 50)
		if err != nil {
			t.Fatal(err)
		}
		if !page.Snapshot || len(page.Items) > 50 {
			t.Fatalf("live Slack page was not a bounded snapshot: items=%d snapshot=%v", len(page.Items), page.Snapshot)
		}
		pages++
		content = append(content, page.Items...)
		cursor = page.NextCursor
		if cursor == "" {
			break
		}
		if pages > 1000 {
			t.Fatal("live Slack history pagination exceeded bound")
		}
	}
	foundThread, foundPermalink := false, false
	for _, item := range content {
		if item.ParentKind == "message" {
			foundThread = true
		}
		if item.Kind == "message" && item.URL != "" {
			foundPermalink = true
		}
	}
	if pages < 2 {
		t.Fatalf("live channel did not prove history cursor pagination: pages=%d", pages)
	}
	if !foundThread || !foundPermalink {
		t.Fatalf("live channel lacks thread/permalink proof: thread=%v permalink=%v", foundThread, foundPermalink)
	}
	var memberCursor Cursor
	memberCount := 0
	for {
		members, err := adapter.Membership(context.Background(), conn, memberCursor, 1)
		if err != nil {
			t.Fatal(err)
		}
		if members.Completeness != CompletenessComplete {
			t.Fatalf("incomplete live membership snapshot: %+v", members)
		}
		memberCount += len(members.Memberships)
		memberCursor = members.NextCursor
		if memberCursor == "" {
			break
		}
	}
	if memberCount == 0 {
		t.Fatal("live Slack channel has no members")
	}
}
