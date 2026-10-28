// dashboard_tools_test.go — Tools page (OA-26): per-role 403 coverage, the
// approval queue rendering ONLY persisted pending/changed rows with their
// field-level diffs, the approve-button schema_version binding the browser
// spec intercepts, and the servers table's ref-name-only credential column.
//
// Obsolete-surface handoff for 09-10 (deletion map row "09-05 Tools"):
// `handleDashboardPageMCP`, `handleDashboardMCP` in dashboard_resources.go;
// routes `/dashboard/mcp`, `/dashboard/partials/mcp`; template
// `partial_mcp.tmpl`; static `mcp.js`.
package httpx

import (
	"context"
	"encoding/json"
	"net/http"
	"net/url"
	"strconv"
	"strings"
	"testing"

	"enclava-go/internal/auth"
	"enclava-go/internal/mcp"
)

func seedToolsServer(t *testing.T, f *identityFixture, id, credential string, enabled bool) {
	t.Helper()
	if err := f.app.mcpRepo.PutServer(context.Background(), mcp.Server{
		TenantID: f.tenant, ID: id, Origin: "https://mcp.example", EndpointPath: "/mcp",
		CredentialRef: credential, AudienceScopes: []string{"assistant.use"}, Enabled: enabled,
	}); err != nil {
		t.Fatalf("seed MCP server %s: %v", id, err)
	}
}

func toolsDiscoverySnapshot(name string, input json.RawMessage, readOnly bool) mcp.DiscoveredTool {
	snapshot := mcp.DiscoveredTool{
		Name: name, Title: name + " tool",
		InputSchema:  input,
		OutputSchema: json.RawMessage(`{}`),
	}
	if readOnly {
		yes := true
		snapshot.Annotations.ReadOnlyHint = &yes
	}
	return snapshot
}

// seedToolsQueue drives a server through the real durable transitions: first
// discovery of `lookup` (pending), an approval, then a rediscovery whose
// changed schema leaves `lookup` in state changed at v2 alongside a fresh
// pending `submit_form` at v1.
func seedToolsQueue(t *testing.T, f *identityFixture, serverID string) {
	t.Helper()
	ctx := context.Background()
	inputV1 := json.RawMessage(`{"type":"object","properties":{"q":{"type":"string"}},"required":["q"]}`)
	inputV2 := json.RawMessage(`{"type":"object","properties":{"limit":{"type":"integer"},"q":{"type":"string"}},"required":["q"]}`)
	if err := f.app.mcpRepo.RecordDiscovery(ctx, f.tenant, serverID, []mcp.DiscoveredTool{
		toolsDiscoverySnapshot("lookup", inputV1, true),
	}); err != nil {
		t.Fatalf("first discovery: %v", err)
	}
	if _, err := f.app.mcpRepo.ApproveDiscovery(ctx, f.tenant, serverID, "lookup", 1, "approved.lookup", []string{"mcp.use"}); err != nil {
		t.Fatalf("approve lookup v1: %v", err)
	}
	if err := f.app.mcpRepo.RecordDiscovery(ctx, f.tenant, serverID, []mcp.DiscoveredTool{
		toolsDiscoverySnapshot("lookup", inputV2, true),
		toolsDiscoverySnapshot("submit_form", json.RawMessage(`{"type":"object","properties":{"text":{"type":"string"}}}`), false),
	}); err != nil {
		t.Fatalf("changed rediscovery: %v", err)
	}
}

func toolsAdminSession(t *testing.T, f *identityFixture) (*http.Cookie, *http.Cookie) {
	t.Helper()
	return loginUser(t, f, "toolsadmin@example.com", []string{auth.ScopePlatformAdmin}, "tools-admin-pass")
}

func TestDashboardToolsMemberDenied(t *testing.T) {
	f := newIdentityFixture(t)
	member, _ := loginUser(t, f, "toolsmember@example.com", []string{"chatbot", "models.list", "rag"}, "tools-member-pass")
	manager, _ := loginUser(t, f, "toolsmanager@example.com",
		[]string{"rag", "rag.manage", "connectors.manage", "chatbot", "chatbot.manage", "extract", "extract.manage"},
		"tools-manager-pass")

	routes := []string{
		"/dashboard/tools",
		"/dashboard/partials/tools/queue",
		"/dashboard/partials/tools/servers",
		"/dashboard/partials/tools/list",
		"/dashboard/partials/tools/register",
	}
	for _, session := range []*http.Cookie{member, manager} {
		for _, path := range routes {
			rec := sessionForm(t, f.app, http.MethodGet, path, nil, session, nil)
			if rec.Code != http.StatusForbidden {
				t.Fatalf("GET %s: expected 403 without tools.manage, got %d", path, rec.Code)
			}
			if path == "/dashboard/tools" {
				if !strings.Contains(rec.Body.String(), "have access to this page") {
					t.Fatalf("page denial must render the designed 403 page, got: %s", rec.Body.String())
				}
			} else if rec.Body.Len() != 0 {
				t.Fatalf("partial %s must deny with an empty 403 body, got %q", path, rec.Body.String())
			}
		}
	}

	// Writes ride the same guard: a member POST is refused before any render.
	rec := sessionForm(t, f.app, http.MethodPost, "/dashboard/tools", url.Values{"x": {"y"}}, member, nil)
	if rec.Code != http.StatusForbidden {
		t.Fatalf("POST /dashboard/tools as member: expected 403, got %d", rec.Code)
	}
}

func TestDashboardToolsQueueDiff(t *testing.T) {
	f := newIdentityFixture(t)
	seedToolsServer(t, f, "srv", "MCP_EXAMPLE_CREDENTIAL", true)
	seedToolsQueue(t, f, "srv")

	sess, _ := toolsAdminSession(t, f)
	rec := sessionForm(t, f.app, http.MethodGet, "/dashboard/tools", nil, sess, nil)
	if rec.Code != http.StatusOK {
		t.Fatalf("GET /dashboard/tools: status %d body=%s", rec.Code, rec.Body.String())
	}
	body := rec.Body.String()

	// The changed row (lookup v2): Changed pill, field-level diff old→new,
	// the durable version the PUT must echo, and the prior approval's model
	// name prefilled.
	if !strings.Contains(body, `data-action="tools:approve" data-server="srv" data-tool="lookup" data-schema-version="2"`) {
		t.Fatalf("changed lookup row missing or wrongly version-bound: %s", body)
	}
	if !strings.Contains(body, "Changed") || !strings.Contains(body, "Awaiting approval") {
		t.Fatalf("queue pills missing: %s", body)
	}
	if !strings.Contains(body, "— → integer") {
		t.Fatalf("field-level diff old→new for added `limit` missing: %s", body)
	}
	if !strings.Contains(body, `value="approved.lookup"`) {
		t.Fatalf("prior approval's model name not prefilled: %s", body)
	}
	if !strings.Contains(body, `data-tool="submit_form" data-schema-version="1"`) {
		t.Fatalf("pending submit_form row missing or wrongly version-bound: %s", body)
	}
	if strings.Contains(body, "Nothing needs approval.") {
		t.Fatalf("empty-state rendered while the queue has rows: %s", body)
	}

	// Approve the changed row and reject the pending one through the real
	// platform.admin API — the queue must then be empty.
	admin := func(method, path, body string) *int {
		rec := f.admin(t, method, path, body)
		return &rec.Code
	}
	if code := admin(http.MethodPut, "/api/v1/mcp/servers/srv/tools/lookup",
		`{"model_name":"approved.lookup","caller_scopes":["mcp.use"],"schema_version":2}`); code == nil || *code != http.StatusOK {
		t.Fatalf("approve lookup v2 status = %v", *code)
	}
	if code := admin(http.MethodDelete, "/api/v1/mcp/servers/srv/tools/submit_form", ""); code == nil || *code != http.StatusOK {
		t.Fatalf("reject submit_form status = %v", *code)
	}
	rec = sessionForm(t, f.app, http.MethodGet, "/dashboard/tools", nil, sess, nil)
	if rec.Code != http.StatusOK || !strings.Contains(rec.Body.String(), "No tools pending approval.") {
		t.Fatalf("queue must render the designed empty state after reviews, status=%d body=%s", rec.Code, rec.Body.String())
	}
}

func TestDashboardToolsApproveContract(t *testing.T) {
	f := newIdentityFixture(t)
	seedToolsServer(t, f, "srv", "MCP_EXAMPLE_CREDENTIAL", true)
	seedToolsQueue(t, f, "srv")

	rows, err := f.app.mcpRepo.ListToolsByState(context.Background(), f.tenant, "pending", "changed")
	if err != nil {
		t.Fatalf("list durable queue rows: %v", err)
	}
	if len(rows) != 2 {
		t.Fatalf("seeded queue rows = %d, want 2", len(rows))
	}

	sess, _ := toolsAdminSession(t, f)
	rec := sessionForm(t, f.app, http.MethodGet, "/dashboard/tools", nil, sess, nil)
	if rec.Code != http.StatusOK {
		t.Fatalf("GET /dashboard/tools: status %d", rec.Code)
	}
	body := rec.Body.String()

	for _, row := range rows {
		// The approve button carries exactly the durable row identity and the
		// schema_version the PUT echoes — the binding the browser spec
		// intercepts and the API compares atomically.
		want := `data-action="tools:approve" data-server="` + row.ServerID + `" data-tool="` + row.ToolID + `" data-schema-version="` +
			strconv.FormatInt(row.SchemaVersion, 10) + `"`
		if !strings.Contains(body, want) {
			t.Fatalf("approve binding for %s/%s v%d missing: %s", row.ServerID, row.ToolID, row.SchemaVersion, body)
		}
		wantReject := `data-action="tools:reject" data-server="` + row.ServerID + `" data-tool="` + row.ToolID + `"`
		if !strings.Contains(body, wantReject) {
			t.Fatalf("reject binding for %s/%s missing", row.ServerID, row.ToolID)
		}
	}
	if got := strings.Count(body, `data-action="tools:approve"`); got != len(rows) {
		t.Fatalf("approve buttons = %d, want %d (queue renders pending|changed only)", got, len(rows))
	}
}

func TestDashboardToolsServersTable(t *testing.T) {
	f := newIdentityFixture(t)
	seedToolsServer(t, f, "alpha", "MCP_ALPHA_CREDENTIAL", true)
	seedToolsServer(t, f, "beta", "", false)
	seedToolsQueue(t, f, "alpha")
	// Approve the changed lookup row through the real API so alpha carries a
	// real approved count (1 of 2 durable rows).
	if rec := f.admin(t, http.MethodPut, "/api/v1/mcp/servers/alpha/tools/lookup",
		`{"model_name":"approved.lookup","caller_scopes":["mcp.use"],"schema_version":2}`); rec.Code != http.StatusOK {
		t.Fatalf("approve lookup on alpha: status %d body=%s", rec.Code, rec.Body.String())
	}

	sess, _ := toolsAdminSession(t, f)
	rec := sessionForm(t, f.app, http.MethodGet, "/dashboard/tools", nil, sess, nil)
	if rec.Code != http.StatusOK {
		t.Fatalf("GET /dashboard/tools: status %d", rec.Code)
	}
	body := rec.Body.String()

	// Credential column shows the ref NAME only.
	if !strings.Contains(body, "MCP_ALPHA_CREDENTIAL") {
		t.Fatalf("credential ref name missing from servers table: %s", body)
	}
	// alpha: 1 approved of 2 durable rows; beta has none discovered yet.
	if !strings.Contains(body, ">1 of 2<") && !strings.Contains(body, "1 of 2") {
		t.Fatalf("alpha approved count wrong: %s", body)
	}
	if !strings.Contains(body, `<td data-label="Approved tools">—</td>`) {
		t.Fatalf("beta must show an absent count, not an invented zero row: %s", body)
	}
	if !strings.Contains(body, "Disabled") {
		t.Fatalf("disabled beta row must render the Disabled pill: %s", body)
	}
	if !strings.Contains(body, "Discovered ") {
		t.Fatalf("alpha row must show the persisted discovery time: %s", body)
	}
	for _, action := range []string{`data-action="tools:discover" data-server="alpha"`, `data-server="beta"`} {
		if !strings.Contains(body, action) {
			t.Fatalf("server row action missing: %s", action)
		}
	}

	// Partials render the same data standalone for the admin session.
	if rec := sessionForm(t, f.app, http.MethodGet, "/dashboard/partials/tools/queue", nil, sess, nil); rec.Code != http.StatusOK || !strings.Contains(rec.Body.String(), "submit_form") {
		t.Fatalf("queue partial: status %d", rec.Code)
	}
	if rec := sessionForm(t, f.app, http.MethodGet, "/dashboard/partials/tools/list", nil, sess, nil); rec.Code != http.StatusOK ||
		!strings.Contains(rec.Body.String(), "approved.lookup") || !strings.Contains(rec.Body.String(), "Read-only") {
		t.Fatalf("list partial must show the approved tool with its Read-only badge: %d %s", rec.Code, rec.Body.String())
	}
	if rec := sessionForm(t, f.app, http.MethodGet, "/dashboard/partials/tools/register", nil, sess, nil); rec.Code != http.StatusOK ||
		!strings.Contains(rec.Body.String(), `data-mode="create"`) {
		t.Fatalf("register drawer (create): status %d %s", rec.Code, rec.Body.String())
	}
	edit := sessionForm(t, f.app, http.MethodGet, "/dashboard/partials/tools/register?server=alpha", nil, sess, nil)
	if edit.Code != http.StatusOK || !strings.Contains(edit.Body.String(), `data-mode="edit"`) ||
		!strings.Contains(edit.Body.String(), `value="alpha"`) {
		t.Fatalf("register drawer (edit) must prefill the persisted server: %d %s", edit.Code, edit.Body.String())
	}
	if missing := sessionForm(t, f.app, http.MethodGet, "/dashboard/partials/tools/register?server=nope", nil, sess, nil); missing.Code != http.StatusNotFound {
		t.Fatalf("edit drawer for an unknown server must 404, got %d", missing.Code)
	}
}
