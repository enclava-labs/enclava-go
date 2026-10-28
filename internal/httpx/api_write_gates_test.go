// api_write_gates_test.go — the executable write-gate audit (06-04, OA-05).
// Every row is one /api/v1 write the dashboard calls; the "gate" column names
// the scope that must refuse a principal without it. Denied-path assertions
// are the proof: if a gate below is ever dropped, the row stops being 403 and
// this file fails. All rows run over the real mux with the fully configured
// 06-03 fixture (real chatbot/prompts/rag/connectors services, endpointsRepo,
// ConfigureMCP), so a 501/404 would mean the fixture lied — never a pass.
package httpx

import (
	"net/http"
	"strings"
	"testing"

	"enclava-go/internal/auth"
	"enclava-go/internal/testdb"
)

// apiWriteGateMemberScopes is §3.2's member list (decision 8's role_member
// seed): feature and read scopes only — no *.manage, no platform.admin.
var apiWriteGateMemberScopes = []string{
	"models.list", "chat.completions", "embeddings.create", "responses",
	"chatbot", "rag", "extract",
}

// TestAPIWriteGateMatrix drives every dashboard-called write with a member
// session (cookie + CSRF, exactly what dashboard JS sends) and asserts 403.
// Extract-manage rows live in internal/modules/extract/api_write_gates_test.go
// — the module imports httpx, so this package cannot mount its routes.
func TestAPIWriteGateMatrix(t *testing.T) {
	f := newIdentityFixture(t)
	sess, csrf := loginUser(t, f, "gates-member@example.com", apiWriteGateMemberScopes, "member-pass")

	rows := []struct {
		name, method, path, body, gate string
	}{
		// chatbot.manage — the outer route scope is only "chatbot", which the
		// member holds, so the inner check in handleChatbots must fire;
		// chatbotSvc is real, a nil service would 501 first and fake the row.
		{"create chatbot", http.MethodPost, "/api/v1/chatbots", `{"name":"gate-bot"}`, "chatbot.manage"},
		{"update chatbot", http.MethodPut, "/api/v1/chatbots/cb_x", `{}`, "chatbot.manage"},
		{"delete chatbot", http.MethodDelete, "/api/v1/chatbots/cb_x", ``, "chatbot.manage"},
		// /api/v1/agents is the same handler pair under an alias.
		{"create agent", http.MethodPost, "/api/v1/agents", `{"name":"gate-agent"}`, "chatbot.manage"},
		{"update agent", http.MethodPut, "/api/v1/agents/cb_x", `{}`, "chatbot.manage"},
		{"delete agent", http.MethodDelete, "/api/v1/agents/cb_x", ``, "chatbot.manage"},
		// chatbot.manage in prompts.go.
		{"create prompt template", http.MethodPost, "/api/v1/prompt-templates", `{"name":"gate-template"}`, "chatbot.manage"},
		{"update prompt template", http.MethodPut, "/api/v1/prompt-templates/pt_x", `{}`, "chatbot.manage"},
		{"delete prompt template", http.MethodDelete, "/api/v1/prompt-templates/pt_x", ``, "chatbot.manage"},
		// rag.manage — member holds outer "rag", inner checks must fire.
		{"create collection", http.MethodPost, "/api/v1/rag/collections", `{"name":"gate-collection"}`, "rag.manage"},
		{"update collection", http.MethodPut, "/api/v1/rag/collections/col_x", `{}`, "rag.manage"},
		{"delete collection", http.MethodDelete, "/api/v1/rag/collections/col_x", ``, "rag.manage"},
		{"reprocess document", http.MethodPost, "/api/v1/rag/documents/doc_x/reprocess", ``, "rag.manage"},
		{"delete document", http.MethodDelete, "/api/v1/rag/documents/doc_x", ``, "rag.manage"},
		// connectors.manage — outer scope denies the member outright.
		{"create connector source", http.MethodPost, "/api/v1/connectors/sources", `{"kind":"web","name":"gate-source"}`, "connectors.manage"},
		{"sync connector source", http.MethodPost, "/api/v1/connectors/sources/src_x/sync", ``, "connectors.manage"},
		{"delete connector source", http.MethodDelete, "/api/v1/connectors/sources/src_x", ``, "connectors.manage"},
		// pricing.manage — outer scope denies; platform.admin would NOT pass either (D-18).
		{"create provider", http.MethodPost, "/api/v1/providers", `{"name":"gate-provider"}`, "pricing.manage"},
		{"update provider", http.MethodPut, "/api/v1/providers/ep_x", `{}`, "pricing.manage"},
		{"upsert pricing", http.MethodPost, "/api/v1/admin/pricing", `{"prices":[]}`, "pricing.manage"},
		// platform.admin — MCP routes exist only via ConfigureMCP (called in the fixture).
		{"create mcp server", http.MethodPost, "/api/v1/mcp/servers", `{"id":"srv_x","origin":"https://mcp.example.com"}`, "platform.admin"},
		{"approve mcp tool", http.MethodPut, "/api/v1/mcp/servers/srv_x/tools/tool_x", `{}`, "platform.admin"},
		{"revoke mcp tool", http.MethodDelete, "/api/v1/mcp/servers/srv_x/tools/tool_x", ``, "platform.admin"},
		// platform.admin — tenant key administration.
		{"create admin api key", http.MethodPost, "/api/v1/admin/api-keys", `{"name":"gate-key"}`, "platform.admin"},
		{"delete admin api key", http.MethodDelete, "/api/v1/admin/api-keys/key_x", ``, "platform.admin"},
		// platform.admin — identity administration.
		{"create user", http.MethodPost, "/api/v1/identities/users", `{"email":"gate@example.com","password":"pw1234"}`, "platform.admin"},
		{"patch user", http.MethodPatch, "/api/v1/identities/users/user_x", `{}`, "platform.admin"},
		{"create role", http.MethodPost, "/api/v1/identities/roles", `{"name":"gate-role"}`, "platform.admin"},
		{"create service account", http.MethodPost, "/api/v1/identities/service-accounts", `{"name":"gate-svc"}`, "platform.admin"},
	}
	for _, tc := range rows {
		rec := sessionDo(t, f.app, tc.method, tc.path, tc.body, sess, csrf)
		if rec.Code != http.StatusForbidden {
			t.Fatalf("%s %s %s (requires %s): member session expected 403, got %d body=%s",
				tc.name, tc.method, tc.path, tc.gate, rec.Code, rec.Body.String())
		}
		if !strings.Contains(rec.Body.String(), "scope") && !strings.Contains(rec.Body.String(), "CSRF") {
			t.Fatalf("%s: 403 body should name the missing scope or CSRF, got %q", tc.name, rec.Body.String())
		}
	}

	// No credential at all: authentication fails before any scope check.
	rec := f.do(t, http.MethodPost, "/api/v1/chatbots", `{"name":"anon"}`, nil)
	if rec.Code != http.StatusUnauthorized {
		t.Fatalf("unauthenticated write expected 401, got %d body=%s", rec.Code, rec.Body.String())
	}
}

// TestAPIWriteGateDenials proves the inner *.manage checks and scope
// boundaries with API-key principals (no CSRF needed). The one positive row
// anchors the audit to a real persisted write: platform.admin creating a
// role whose scopes stay inside its own authority.
func TestAPIWriteGateDenials(t *testing.T) {
	f := newIdentityFixture(t)
	owner := testdb.SeedUser(t, f.db, f.tenant, "gates-keys@example.com", nil)

	keyFor := func(spec testdb.SeedKeySpec) string {
		key, _ := testdb.SeedAPIKey(t, f.db, f.tenant, spec)
		return key
	}
	// Member-shaped key: every §3.2 feature scope, still no connectors.manage.
	memberKey := keyFor(testdb.SeedKeySpec{Name: "gates-member-key", OwnerUserID: owner.ID, Scopes: apiWriteGateMemberScopes})
	chatbotKey := keyFor(testdb.SeedKeySpec{Name: "gates-chatbot-key", OwnerUserID: owner.ID, Scopes: []string{"chatbot"}})
	ragKey := keyFor(testdb.SeedKeySpec{Name: "gates-rag-key", OwnerUserID: owner.ID, Scopes: []string{"rag"}})
	adminKey := keyFor(testdb.SeedKeySpec{Name: "gates-admin-key", OwnerUserID: owner.ID, Scopes: []string{auth.ScopePlatformAdmin}})

	rows := []struct {
		name, method, path, body, key, gate string
	}{
		// Feature scope alone must NOT pass the manage write — the outer
		// route scope is the feature scope, so the inner handler check fires.
		{"chatbot-only key on chatbot create", http.MethodPost, "/api/v1/chatbots", `{"name":"gate-bot"}`, chatbotKey, "chatbot.manage"},
		{"rag-only key on collection create", http.MethodPost, "/api/v1/rag/collections", `{"name":"gate-collection"}`, ragKey, "rag.manage"},
		// member key lacks connectors.manage (outer scope).
		{"member key on connector source create", http.MethodPost, "/api/v1/connectors/sources", `{"kind":"web","name":"gate-source"}`, memberKey, "connectors.manage"},
		// D-18 non-implication: platform.admin never implies pricing.manage.
		{"platform.admin key on provider create", http.MethodPost, "/api/v1/providers", `{"name":"gate-provider"}`, adminKey, "pricing.manage"},
		{"platform.admin key on pricing upsert", http.MethodPost, "/api/v1/admin/pricing", `{"prices":[]}`, adminKey, "pricing.manage"},
		// scopesSubsetOfCaller is exact: platform.admin cannot mint a role
		// carrying a scope it does not hold.
		{"platform.admin key minting out-of-subset role", http.MethodPost, "/api/v1/identities/roles", `{"name":"escalated","scopes":["models.list"]}`, adminKey, "scopesSubsetOfCaller"},
	}
	for _, tc := range rows {
		rec := f.do(t, tc.method, tc.path, tc.body, map[string]string{"X-API-Key": tc.key})
		if rec.Code != http.StatusForbidden {
			t.Fatalf("%s (requires %s): expected 403, got %d body=%s",
				tc.name, tc.gate, rec.Code, rec.Body.String())
		}
	}

	// Positive anchor: an exactly in-subset platform.admin role write lands
	// end to end through the real mux against the real IdentityRepo.
	rec := f.do(t, http.MethodPost, "/api/v1/identities/roles",
		`{"name":"gate-anchor","scopes":["platform.admin"]}`, map[string]string{"X-API-Key": adminKey})
	if rec.Code != http.StatusCreated {
		t.Fatalf("role create anchor expected 201, got %d body=%s", rec.Code, rec.Body.String())
	}
	// handleIdentityRoles emits the roleDTO at the top level.
	role := decodeJSON(t, rec)
	id, _ := role["id"].(string)
	if !strings.HasPrefix(id, "role_") {
		t.Fatalf("anchor role id missing, got %v", role["id"])
	}
	if role["name"] != "gate-anchor" {
		t.Fatalf("anchor role name = %v", role["name"])
	}
	grants, _ := role["grants"].(map[string]any)
	models, _ := grants["models"].(map[string]any)
	if models["mode"] != "list" {
		t.Fatalf("anchor grants.models.mode = %v, want canonical deny list", models["mode"])
	}
	if items, ok := models["items"].([]any); !ok || len(items) != 0 {
		t.Fatalf("anchor grants.models.items = %v, want empty", models["items"])
	}
	// The write persisted: read it back through the same gate.
	rec = f.do(t, http.MethodGet, "/api/v1/identities/roles/"+id, "", map[string]string{"X-API-Key": adminKey})
	if rec.Code != http.StatusOK {
		t.Fatalf("anchor role readback expected 200, got %d body=%s", rec.Code, rec.Body.String())
	}
	if got := decodeJSON(t, rec)["name"]; got != "gate-anchor" {
		t.Fatalf("anchor role persisted name = %v", got)
	}
}
