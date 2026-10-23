//go:build live

package connectors

import (
	"context"
	"os"
	"testing"

	"enclava-go/internal/auth"
	"enclava-go/internal/source"
)

func TestLiveTeams(t *testing.T) {
	values := map[string]string{}
	for _, key := range []string{"TEST_GRAPH_TENANT_ID", "TEST_GRAPH_CLIENT_ID", "TEST_GRAPH_CLIENT_SECRET", "TEST_GRAPH_TEAM_ID", "TEST_GRAPH_CHAT_ID"} {
		values[key] = os.Getenv(key)
		if values[key] == "" { t.Fatalf("%s is required", key) }
	}
	const tenant = "live-teams-tenant"
	const ref = "live-teams-secret"
	resolver, err := auth.NewSecretResolver([]auth.SecretBinding{{RefID:ref, TenantID:tenant, Purpose:auth.SecretPurposeConnector, EnvName:"TEST_GRAPH_CLIENT_SECRET", Origins:[]string{"https://graph.microsoft.com"}}})
	if err != nil { t.Fatal(err) }
	connection := source.Connection{TenantID:tenant, ID:"live-teams", Kind:source.ConnectionTeams, CredentialRef:ref, Config:map[string]any{"tenant_id":values["TEST_GRAPH_TENANT_ID"], "client_id":values["TEST_GRAPH_CLIENT_ID"], "team_id":values["TEST_GRAPH_TEAM_ID"], "chat_ids":[]any{values["TEST_GRAPH_CHAT_ID"]}}}
	adapter, err := NewTeamsAdapter(connection, resolver)
	if err != nil { t.Fatal(err) }
	page, err := adapter.Content(context.Background(), connection, "", 100)
	if err != nil { t.Fatalf("Graph team/channel/chat content access: %v", err) }
	if len(page.Items) == 0 { t.Fatal("configured team and chat returned no messages") }
	members, err := adapter.Membership(context.Background(), connection, "", 100)
	if err != nil { t.Fatalf("Graph team/channel membership access: %v", err) }
	if members.Completeness != CompletenessComplete { t.Fatalf("membership completeness = %q", members.Completeness) }
}
