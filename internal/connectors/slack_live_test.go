//go:build live

package connectors

import (
	"context"
	"os"
	"testing"

	"enclava-go/internal/source"
)

func TestLiveSlack(t *testing.T) {
	if os.Getenv("TEST_SLACK_BOT_TOKEN") == "" || os.Getenv("TEST_SLACK_CHANNEL_ID") == "" {
		t.Fatal("TEST_SLACK_BOT_TOKEN and TEST_SLACK_CHANNEL_ID are required")
	}
	const ref = "CONNECTOR_SLACK_LIVE_TEST"
	t.Setenv(ref, os.Getenv("TEST_SLACK_BOT_TOKEN"))
	adapter, err := NewSlackAdapter(source.Connection{})
	if err != nil { t.Fatal(err) }
	conn := source.Connection{Kind: source.ConnectionSlack, CredentialRef: ref, Config: map[string]any{"channel_id":os.Getenv("TEST_SLACK_CHANNEL_ID"), "reconcile":"true"}}
	var cursor Cursor
	var content []ContentItem
	pages := 0
	for {
		page, err := adapter.Content(context.Background(), conn, cursor, 1)
		if err != nil { t.Fatal(err) }
		if !page.Snapshot { t.Fatal("live Slack channel listing was not a complete snapshot") }
		pages++
		content = append(content, page.Items...)
		cursor = page.NextCursor
		if cursor == "" { break }
		if pages > 1000 { t.Fatal("live Slack history pagination exceeded bound") }
	}
	foundThread, foundPermalink := false, false
	for _, item := range content {
		if item.ParentNativeID != "" { foundThread = true }
		if item.Kind == "message" && item.URL != "" { foundPermalink = true }
	}
	if pages < 2 { t.Fatalf("live channel did not prove history cursor pagination: pages=%d", pages) }
	if !foundThread || !foundPermalink { t.Fatalf("live channel lacks thread/permalink proof: thread=%v permalink=%v", foundThread, foundPermalink) }
	var memberCursor Cursor
	memberCount := 0
	for {
		members, err := adapter.Membership(context.Background(), conn, memberCursor, 1)
		if err != nil { t.Fatal(err) }
		if members.Completeness != CompletenessComplete { t.Fatalf("incomplete live membership snapshot: %+v", members) }
		memberCount += len(members.Memberships)
		memberCursor = members.NextCursor
		if memberCursor == "" { break }
	}
	if memberCount == 0 { t.Fatal("live Slack channel has no members") }
}
