package main

import (
	"context"
	"encoding/json"
	"fmt"
	"os"
	"os/exec"
	"path/filepath"
	"regexp"
	"strings"
	"testing"
	"time"

	"enclava-go/internal/auth"
	"enclava-go/internal/store/postgres"
	"enclava-go/internal/testdb"
)

// bootstrapBinary builds the enclava binary and returns its path plus the
// minimal environment a bootstrap run needs.
func bootstrapBinary(t *testing.T, dsn string) (string, []string) {
	t.Helper()
	env := []string{
		"PATH=" + os.Getenv("PATH"),
		"DATABASE_URL=" + dsn,
		"ENCLAVA_DATA_DIR=" + t.TempDir(),
		"INFERENCE_PROVIDER=openai_compatible",
		"OPENAI_COMPATIBLE_BASE_URL=http://127.0.0.1:18443",
		"OPENAI_COMPATIBLE_API_KEY=stub-key",
	}
	bin := filepath.Join(t.TempDir(), "enclava")
	build := exec.Command("go", "build", "-o", bin, ".")
	if out, err := build.CombinedOutput(); err != nil {
		t.Fatalf("build enclava binary: %v\n%s", err, out)
	}
	return bin, env
}

func runBootstrapCmd(t *testing.T, bin string, env []string, slug string) (string, error) {
	t.Helper()
	cmd := exec.Command(bin, "bootstrap", "--tenant", slug, "--email", "a@b.c", "--name", "A", "--issue-key")
	cmd.Env = env
	output, err := cmd.CombinedOutput()
	return string(output), err
}

func TestBootstrap(t *testing.T) {
	db, dsn := testdb.NewWithDSN(t)
	slug := fmt.Sprintf("bootstrap-%d", time.Now().UnixNano())
	bin, env := bootstrapBinary(t, dsn)

	output, err := runBootstrapCmd(t, bin, env, slug)
	if err != nil {
		t.Fatalf("first bootstrap: %v\n%s", err, output)
	}
	if !strings.Contains(output, "tenant "+slug+" created") {
		t.Fatalf("bootstrap did not report created tenant: %s", output)
	}

	keyMatch := regexp.MustCompile(`(?m)^api key: (\S+)\s*$`).FindStringSubmatch(output)
	if keyMatch == nil || !strings.HasPrefix(keyMatch[1], "enc_") {
		t.Fatalf("bootstrap API key missing or malformed: %s", output)
	}

	var tenantCount int
	if err := db.SQL.QueryRowContext(context.Background(), "SELECT count(*) FROM tenants WHERE slug = $1", slug).Scan(&tenantCount); err != nil {
		t.Fatalf("query bootstrap tenant: %v", err)
	}
	if tenantCount != 1 {
		t.Fatalf("tenant count = %d, want 1", tenantCount)
	}

	if output, err := runBootstrapCmd(t, bin, env, slug); err == nil {
		t.Fatalf("bootstrap unexpectedly accepted existing slug: %s", output)
	}

	var keyCount int
	if err := db.SQL.QueryRowContext(context.Background(), `SELECT count(*) FROM api_keys k JOIN tenants t ON t.id = k.tenant_id WHERE t.slug = $1`, slug).Scan(&keyCount); err != nil {
		t.Fatalf("query bootstrap API key: %v", err)
	}
	if keyCount != 1 {
		t.Fatalf("API key count = %d, want 1", keyCount)
	}

}

// bootstrapExpectedScopes spells out the literal §3.2 scope sets — never
// derived from the code under test (P6-09), so the bootstrap assertions and
// the capability table's own literals stay two independent expectations.
var bootstrapExpectedScopes = map[string][]string{
	"admin": {
		"platform.admin",
		"models.list", "chat.completions", "embeddings.create", "responses",
		"openai.proxy", "extract", "extract.manage", "rag", "rag.manage",
		"chatbot", "chatbot.manage", "connectors.manage", "pricing.manage",
	},
	"knowledge_manager": {
		"models.list", "chat.completions", "embeddings.create", "responses",
		"chatbot", "rag", "extract",
		"rag.manage", "connectors.manage", "chatbot.manage", "extract.manage",
	},
	"member": {
		"models.list", "chat.completions", "embeddings.create", "responses",
		"chatbot", "rag", "extract",
	},
}

// TestBootstrapSeedsDefaultRoles asserts the ACTUAL stored roles: exactly
// the three §3.2 roles, each with its literal scope set and explicit grant
// documents (admin the only mode:all seed; non-admins explicit deny), then
// the live authority of an authenticated principal per role.
func TestBootstrapSeedsDefaultRoles(t *testing.T) {
	db, dsn := testdb.NewWithDSN(t)
	slug := fmt.Sprintf("bootstrap-roles-%d", time.Now().UnixNano())
	bin, env := bootstrapBinary(t, dsn)
	if output, err := runBootstrapCmd(t, bin, env, slug); err != nil {
		t.Fatalf("bootstrap: %v\n%s", err, output)
	}

	ctx := context.Background()
	var tenantID string
	if err := db.SQL.QueryRowContext(ctx, `SELECT id::text FROM tenants WHERE slug = $1`, slug).Scan(&tenantID); err != nil {
		t.Fatalf("query bootstrap tenant: %v", err)
	}

	rows, err := db.SQL.QueryContext(ctx, `
		SELECT id, name, scopes,
		       allowed_models, allowed_endpoints, allowed_assistants, allowed_extract_templates
		FROM roles WHERE tenant_id = $1::uuid ORDER BY id`, tenantID)
	if err != nil {
		t.Fatalf("query roles: %v", err)
	}
	defer rows.Close()

	type seededRole struct {
		id     string
		scopes map[string]struct{}
		docs   [4]json.RawMessage
	}
	stored := map[string]seededRole{}
	var names []string
	for rows.Next() {
		var name string
		var scopesRaw json.RawMessage
		var r seededRole
		if err := rows.Scan(&r.id, &name, &scopesRaw, &r.docs[0], &r.docs[1], &r.docs[2], &r.docs[3]); err != nil {
			t.Fatalf("scan role: %v", err)
		}
		var scopes []string
		if err := json.Unmarshal(scopesRaw, &scopes); err != nil {
			t.Fatalf("decode scopes for %s: %v", name, err)
		}
		r.scopes = map[string]struct{}{}
		for _, s := range scopes {
			r.scopes[s] = struct{}{}
		}
		stored[name] = r
		names = append(names, name)
	}
	if err := rows.Err(); err != nil {
		t.Fatalf("role rows: %v", err)
	}
	if len(names) != 3 {
		t.Fatalf("expected exactly 3 seeded roles, got %v", names)
	}
	for _, name := range []string{"admin", "knowledge_manager", "member"} {
		if _, ok := stored[name]; !ok {
			t.Fatalf("role %q missing, have %v", name, names)
		}
		want := bootstrapExpectedScopes[name]
		got := stored[name].scopes
		if len(got) != len(want) {
			t.Fatalf("%s stored scope set %v, want exactly %v", name, got, want)
		}
		for _, s := range want {
			if _, ok := got[s]; !ok {
				t.Fatalf("%s stored scope set missing %q: %v", name, s, got)
			}
		}
	}

	// Grant documents: admin decodes mode:all on all four kinds; member/KM
	// decode explicit empty deny lists.
	grantKinds := []string{"allowed_models", "allowed_endpoints", "allowed_assistants", "allowed_extract_templates"}
	for name, wantAll := range map[string]bool{"admin": true, "knowledge_manager": false, "member": false} {
		for i, kind := range grantKinds {
			items, all := auth.DecodeAllowlist(stored[name].docs[i])
			if all != wantAll || len(items) != 0 {
				t.Fatalf("%s %s = %s (all=%v items=%v), want all=%v empty", name, kind, stored[name].docs[i], all, items, wantAll)
			}
		}
	}

	// Live authority: a user assigned each STORED role authenticates with
	// exactly that role's scopes and resource authority.
	repo := postgres.NewIdentityRepo(db)
	authSvc := auth.NewService(repo, "")
	for _, name := range []string{"admin", "knowledge_manager", "member"} {
		user := auth.User{
			ID: "user_boot_" + name, TenantID: tenantID,
			Email: name + "@boot.test", DisplayName: name, Status: "active",
		}
		if err := repo.CreateUser(ctx, tenantID, user); err != nil {
			t.Fatalf("create %s user: %v", name, err)
		}
		hash, err := auth.HashPassword("boot-pass-" + name)
		if err != nil {
			t.Fatalf("hash: %v", err)
		}
		if err := repo.SetPassword(ctx, tenantID, user.ID, hash, time.Now().UTC()); err != nil {
			t.Fatalf("set password: %v", err)
		}
		if err := repo.AssignRole(ctx, tenantID, user.ID, stored[name].id); err != nil {
			t.Fatalf("assign stored role: %v", err)
		}

		principal, err := authSvc.AuthenticatePassword(ctx, tenantID, user.Email, "boot-pass-"+name)
		if err != nil {
			t.Fatalf("authenticate %s: %v", name, err)
		}
		want := bootstrapExpectedScopes[name]
		if len(principal.Scopes) != len(want) {
			t.Fatalf("%s principal scopes %v, want %v", name, principal.Scopes, want)
		}
		for _, s := range want {
			if !auth.HasScope(principal, s) {
				t.Fatalf("%s principal missing scope %q", name, s)
			}
		}
		allowed := name == "admin"
		for _, id := range []string{"any-model", "any-endpoint", "any-bot", "any-agent", "any-template"} {
			have := auth.ModelAllowed(principal, id) || auth.EndpointAllowed(principal, id) ||
				auth.ChatbotAllowed(principal, id) || auth.AgentAllowed(principal, id) ||
				auth.ExtractTemplateAllowed(principal, id)
			if have != allowed {
				t.Fatalf("%s principal resource authority for %q = %v, want %v", name, id, have, allowed)
			}
		}
	}
}
