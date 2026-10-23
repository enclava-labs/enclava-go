//go:build live

package connectors

import (
	"context"
	"os"
	"strings"
	"testing"

	"enclava-go/internal/auth"
	"enclava-go/internal/source"
)

func TestLiveGitHub(t *testing.T) {
	token,repo:=os.Getenv("TEST_GITHUB_TOKEN"),os.Getenv("TEST_GITHUB_REPO")
	if token==""{t.Fatal("TEST_GITHUB_TOKEN is required")};if repo==""{t.Fatal("TEST_GITHUB_REPO is required")}
	parts:=strings.SplitN(repo,"/",2);if len(parts)!=2||parts[0]==""||parts[1]==""{t.Fatalf("TEST_GITHUB_REPO must be owner/name")}
	const tenant,ref="live-github-tenant","live-github-token"
	resolver,err:=auth.NewSecretResolver([]auth.SecretBinding{{RefID:ref,TenantID:tenant,Purpose:auth.SecretPurposeConnector,EnvName:"TEST_GITHUB_TOKEN",Origins:[]string{"https://api.github.com"}}});if err!=nil{t.Fatal(err)}
	conn:=source.Connection{TenantID:tenant,ID:"live-github",Kind:source.ConnectionGitHub,CredentialRef:ref,Config:map[string]any{"owner":parts[0],"repo":parts[1]}}
	adapter,err:=NewGitHubAdapter(conn,resolver);if err!=nil{t.Fatal(err)}
	seen := map[string]bool{}
	cursor := Cursor("")
	for {
		page, err := adapter.Content(context.Background(), conn, cursor, 100)
		if err != nil { t.Fatalf("GitHub content access: %v", err) }
		for _, item := range page.Items { if seen[item.NativeID] { t.Fatalf("duplicate identity: %s", item.NativeID) }; seen[item.NativeID] = true }
		if page.NextCursor == "" { if page.Completeness != CompletenessComplete { t.Fatalf("final page incomplete: %q", page.Completeness) }; break }
		cursor = page.NextCursor
	}
	if len(seen) == 0 { t.Fatal("repository returned no content/discussion items") }
	again, err := adapter.Content(context.Background(), conn, "", 100)
	if err != nil { t.Fatalf("second content pass: %v", err) }
	for _, item := range again.Items { if !seen[item.NativeID] { t.Fatalf("second pass introduced identity %s", item.NativeID) } }
	access, err := adapter.Access(context.Background(), conn, "", 100)
	if err != nil { t.Fatalf("collaborator listing/capability: %v", err) }
	if access.Completeness != CompletenessComplete { t.Fatalf("access completeness=%q", access.Completeness) }
}
