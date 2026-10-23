//go:build live

package connectors

import (
	"context"
	"os"
	"testing"

	"mini-enclava/internal/source"
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
	page, err := adapter.Content(context.Background(), conn, "", 100)
	if err != nil { t.Fatal(err) }
	if !page.Snapshot { t.Fatal("live Slack channel listing was not a complete snapshot") }
	foundThread, foundPermalink := false, false
	for _, item := range page.Items {
		if item.ParentNativeID != "" { foundThread = true }
		if item.Kind == "message" && item.URL != "" { foundPermalink = true }
	}
	if !foundThread || !foundPermalink { t.Fatalf("live channel lacks thread/permalink proof: thread=%v permalink=%v", foundThread, foundPermalink) }
	members, err := adapter.Membership(context.Background(), conn, "", 200)
	if err != nil { t.Fatal(err) }
	if members.Completeness != CompletenessComplete || len(members.Memberships) == 0 { t.Fatalf("incomplete live membership snapshot: %+v", members) }
}
