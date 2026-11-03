package httpx

import (
	"context"
	"encoding/json"
	"errors"
	"fmt"
	"net/http"
	"net/http/httptest"
	"strconv"
	"strings"
	"sync/atomic"
	"testing"

	"enclava-go/internal/auth"
	"enclava-go/internal/mcp"
	"enclava-go/internal/store/postgres"
	"enclava-go/internal/testdb"
)

func TestMCPAdministrationDiscoveryApprovalRevisionAndRevocation(t *testing.T) {
	db := testdb.New(t)
	tenant := testdb.SeedTenant(t, db, "mcp-admin")
	changed := atomic.Bool{}
	empty := atomic.Bool{}
	server := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		if r.Method == http.MethodDelete {
			w.WriteHeader(http.StatusAccepted)
			return
		}
		var request struct {
			ID     int64  `json:"id"`
			Method string `json:"method"`
		}
		_ = json.NewDecoder(r.Body).Decode(&request)
		if request.Method == "notifications/initialized" {
			w.WriteHeader(http.StatusAccepted)
			return
		}
		w.Header().Set("Content-Type", "application/json")
		switch request.Method {
		case "initialize":
			fmt.Fprintf(w, `{"jsonrpc":"2.0","id":%d,"result":{"protocolVersion":"2025-06-18"}}`, request.ID)
		case "tools/list":
			tools := "[]"
			if !empty.Load() {
				input := `{"type":"object","additionalProperties":false}`
				if changed.Load() {
					input = `{"type":"object","properties":{"q":{"type":"string"}},"required":["q"],"additionalProperties":false}`
				}
				tools = `[{"name":"remote","title":"Remote Tool","inputSchema":` + input + `,"outputSchema":{"type":"object"},"annotations":{"readOnlyHint":true}}]`
			}
			fmt.Fprintf(w, `{"jsonrpc":"2.0","id":%d,"result":{"tools":%s}}`, request.ID, tools)
		default:
			t.Errorf("unexpected MCP method %q", request.Method)
			http.Error(w, "unexpected", http.StatusBadRequest)
		}
	}))
	defer server.Close()

	repo := postgres.NewMCPToolsRepo(db)
	if err := repo.PutServer(context.Background(), postgres.MCPServer{TenantID: tenant, ID: "server", Origin: server.URL, EndpointPath: "/mcp", AuthorizedPrivate: true, AudienceScopes: []string{"assistant.use"}, Enabled: true}); err != nil {
		t.Fatal(err)
	}
	app := &App{mcpRepo: repo, mcpTransport: &mcp.StreamableHTTP{}}
	admin := auth.Principal{TenantID: tenant, Scopes: map[string]struct{}{"platform.admin": {}}}
	serverRequest := httptest.NewRequest(http.MethodPost, "/api/v1/mcp/servers", strings.NewReader(`{"id":"server","origin":"`+server.URL+`","endpoint_path":"/mcp","authorized_private":true,"audience_scopes":["assistant.use"],"enabled":true}`))
	serverResponse := httptest.NewRecorder()
	app.handleMCPServers(serverResponse, serverRequest, admin)
	if serverResponse.Code != http.StatusCreated || !strings.Contains(serverResponse.Body.String(), `"endpoint_path":"/mcp"`) {
		t.Fatalf("server registration status=%d body=%s", serverResponse.Code, serverResponse.Body.String())
	}
	call := func(method, path, body string) *httptest.ResponseRecorder {
		request := httptest.NewRequest(method, path, strings.NewReader(body))
		recorder := httptest.NewRecorder()
		app.handleMCPServerByID(recorder, request, admin)
		return recorder
	}
	// Discovery persists durable pending queue rows — not a transient list.
	discovery := call(http.MethodPost, "/api/v1/mcp/servers/server/discovery", "")
	if discovery.Code != http.StatusOK || !strings.Contains(discovery.Body.String(), `"state":"pending"`) || !strings.Contains(discovery.Body.String(), `"schema_version":1`) || !strings.Contains(discovery.Body.String(), `"input_schema"`) || !strings.Contains(discovery.Body.String(), `"diff_fields":[]`) || !strings.Contains(discovery.Body.String(), `"annotations"`) {
		t.Fatalf("discovery status=%d body=%s", discovery.Code, discovery.Body.String())
	}
	health, err := repo.GetServer(context.Background(), tenant, "server")
	if err != nil || health.DiscoveryAt.IsZero() || health.DiscoveryError != "" {
		t.Fatalf("successful discovery health=%+v err=%v", health, err)
	}
	if approvals, err := repo.ListAllApprovals(context.Background(), tenant, "server"); err != nil || len(approvals) != 0 {
		t.Fatalf("discovery auto-approved tools=%v err=%v", approvals, err)
	}
	// schema_version is required: a PUT without one is a bad request.
	if got := call(http.MethodPut, "/api/v1/mcp/servers/server/tools/remote", `{"model_name":"approved.lookup","caller_scopes":["mcp.use"]}`); got.Code != http.StatusBadRequest {
		t.Fatalf("unversioned approval status=%d body=%s", got.Code, got.Body.String())
	}
	approve := func(version int) string {
		return `{"model_name":"approved.lookup","caller_scopes":["mcp.use"],"schema_version":` + strconv.Itoa(version) + `}`
	}
	if got := call(http.MethodPut, "/api/v1/mcp/servers/server/tools/remote", approve(1)); got.Code != http.StatusOK || !strings.Contains(got.Body.String(), `"schema_version":1`) {
		t.Fatalf("approval status=%d body=%s", got.Code, got.Body.String())
	}
	toolsList := call(http.MethodGet, "/api/v1/mcp/servers/server/tools", "")
	if toolsList.Code != http.StatusOK || !strings.Contains(toolsList.Body.String(), `"model_name":"approved.lookup"`) || !strings.Contains(toolsList.Body.String(), `"state":"approved"`) || !strings.Contains(toolsList.Body.String(), `"input_schema"`) {
		t.Fatalf("tools list status=%d body=%s", toolsList.Code, toolsList.Body.String())
	}
	current, err := repo.GetApproval(context.Background(), tenant, "server", "remote")
	if err != nil || current.SchemaVersion != 1 {
		t.Fatalf("initial approval=%+v err=%v", current, err)
	}
	changed.Store(true)
	if got := call(http.MethodPost, "/api/v1/mcp/servers/server/discovery", ""); got.Code != http.StatusOK || !strings.Contains(got.Body.String(), `"state":"changed"`) || !strings.Contains(got.Body.String(), `"schema_version":2`) || !strings.Contains(got.Body.String(), `"field":"q"`) || !strings.Contains(got.Body.String(), `"prior_schema"`) {
		t.Fatalf("changed-schema discovery status=%d body=%s", got.Code, got.Body.String())
	}
	current, err = repo.GetApprovalAny(context.Background(), tenant, "server", "remote")
	if err != nil || current.Enabled {
		t.Fatalf("changed schema remained enabled: %+v err=%v", current, err)
	}
	// A stale review — the admin still holds version 1 — is a 409, never a
	// blind re-approval.
	if got := call(http.MethodPut, "/api/v1/mcp/servers/server/tools/remote", approve(1)); got.Code != http.StatusConflict || !strings.Contains(got.Body.String(), "schema_changed") {
		t.Fatalf("stale review status=%d body=%s", got.Code, got.Body.String())
	}
	if current, err = repo.GetApprovalAny(context.Background(), tenant, "server", "remote"); err != nil || current.Enabled {
		t.Fatalf("stale review installed an approval: %+v err=%v", current, err)
	}
	if got := call(http.MethodPut, "/api/v1/mcp/servers/server/tools/remote", approve(2)); got.Code != http.StatusOK {
		t.Fatalf("reapproval status=%d body=%s", got.Code, got.Body.String())
	}
	current, err = repo.GetApproval(context.Background(), tenant, "server", "remote")
	if err != nil || current.SchemaVersion != 2 {
		t.Fatalf("reapproved schema=%+v err=%v", current, err)
	}
	if got := call(http.MethodDelete, "/api/v1/mcp/servers/server/tools/remote", ""); got.Code != http.StatusOK {
		t.Fatalf("revoke status=%d body=%s", got.Code, got.Body.String())
	}
	current, err = repo.GetApprovalAny(context.Background(), tenant, "server", "remote")
	if err != nil || current.Enabled {
		t.Fatalf("revoked approval=%+v err=%v", current, err)
	}
	if row, err := repo.ListServerTools(context.Background(), tenant, "server"); err != nil || len(row) != 1 || row[0].State != "rejected" {
		t.Fatalf("rejected queue row = %+v err=%v", row, err)
	}
	// Rejecting an already-rejected tool is idempotent.
	if got := call(http.MethodDelete, "/api/v1/mcp/servers/server/tools/remote", ""); got.Code != http.StatusOK {
		t.Fatalf("idempotent revoke status=%d body=%s", got.Code, got.Body.String())
	}
}

// TestMCPAdministrationEmptySnapshotRevokesOnlyItsServer proves server
// identity survives a legitimately empty tools array: the vanished server's
// approvals revoke and its rows go gone, while another server's approved
// tool on the same tenant is untouched.
func TestMCPAdministrationEmptySnapshotRevokesOnlyItsServer(t *testing.T) {
	db := testdb.New(t)
	tenant := testdb.SeedTenant(t, db, "mcp-empty")
	empty := atomic.Bool{}
	server := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		if r.Method == http.MethodDelete {
			w.WriteHeader(http.StatusAccepted)
			return
		}
		var request struct {
			ID     int64  `json:"id"`
			Method string `json:"method"`
		}
		_ = json.NewDecoder(r.Body).Decode(&request)
		if request.Method == "notifications/initialized" {
			w.WriteHeader(http.StatusAccepted)
			return
		}
		w.Header().Set("Content-Type", "application/json")
		tools := `[{"name":"remote","inputSchema":{"type":"object"},"outputSchema":{"type":"object"}}]`
		if empty.Load() {
			tools = "[]"
		}
		if request.Method == "initialize" {
			fmt.Fprintf(w, `{"jsonrpc":"2.0","id":%d,"result":{"protocolVersion":"2025-06-18"}}`, request.ID)
		} else if request.Method == "tools/list" {
			fmt.Fprintf(w, `{"jsonrpc":"2.0","id":%d,"result":{"tools":%s}}`, request.ID, tools)
		} else {
			t.Errorf("unexpected MCP method %q", request.Method)
			http.Error(w, "unexpected", http.StatusBadRequest)
		}
	}))
	defer server.Close()

	repo := postgres.NewMCPToolsRepo(db)
	app := &App{mcpRepo: repo, mcpTransport: &mcp.StreamableHTTP{}}
	admin := auth.Principal{TenantID: tenant, Scopes: map[string]struct{}{"platform.admin": {}}}
	call := func(method, path, body string) *httptest.ResponseRecorder {
		request := httptest.NewRequest(method, path, strings.NewReader(body))
		recorder := httptest.NewRecorder()
		app.handleMCPServerByID(recorder, request, admin)
		return recorder
	}
	for _, id := range []string{"srv-1", "srv-2"} {
		if err := repo.PutServer(context.Background(), postgres.MCPServer{TenantID: tenant, ID: id, Origin: server.URL, EndpointPath: "/mcp", AuthorizedPrivate: true, AudienceScopes: []string{"assistant.use"}, Enabled: true}); err != nil {
			t.Fatal(err)
		}
		if got := call(http.MethodPost, "/api/v1/mcp/servers/"+id+"/discovery", ""); got.Code != http.StatusOK {
			t.Fatalf("%s discovery status=%d body=%s", id, got.Code, got.Body.String())
		}
		// model_name is tenant-unique (it becomes the invocable tool name),
		// so each server's approval must claim its own name.
		approval := fmt.Sprintf(`{"model_name":"approved.%s","caller_scopes":["mcp.use"],"schema_version":1}`, id)
		if got := call(http.MethodPut, "/api/v1/mcp/servers/"+id+"/tools/remote", approval); got.Code != http.StatusOK {
			t.Fatalf("%s approval status=%d body=%s", id, got.Code, got.Body.String())
		}
	}
	empty.Store(true)
	if got := call(http.MethodPost, "/api/v1/mcp/servers/srv-1/discovery", ""); got.Code != http.StatusOK || !strings.Contains(got.Body.String(), `"state":"gone"`) {
		t.Fatalf("empty snapshot discovery status=%d body=%s", got.Code, got.Body.String())
	}
	if approval, err := repo.GetApprovalAny(context.Background(), tenant, "srv-1", "remote"); err != nil || approval.Enabled {
		t.Fatalf("srv-1 approval = %+v err=%v", approval, err)
	}
	if approval, err := repo.GetApproval(context.Background(), tenant, "srv-2", "remote"); err != nil || !approval.Enabled {
		t.Fatalf("srv-2 approval disturbed by srv-1's empty snapshot: %+v err=%v", approval, err)
	}
	rows, err := repo.ListServerTools(context.Background(), tenant, "srv-2")
	if err != nil || len(rows) != 1 || rows[0].State != "approved" {
		t.Fatalf("srv-2 rows = %+v err=%v", rows, err)
	}
}

// stubMCPCaller admits every executor call; the test targets the digest and
// transport path, not caller policy.
type stubMCPCaller struct{}

func (stubMCPCaller) Authorize(context.Context, auth.Principal, mcp.Server, mcp.Approval, json.RawMessage) error {
	return nil
}

// TestMCPApprovedToolInvokesOverStreamableHTTPWithJSONBDigests runs the full
// approval-to-invocation path against a real StreamableHTTP fixture: the
// approved schema round-trips through real jsonb columns, the fixture serves
// the SAME schema reformatted at call time, and the canonical digest must
// still dispatch — while a genuinely changed schema must be refused.
func TestMCPApprovedToolInvokesOverStreamableHTTPWithJSONBDigests(t *testing.T) {
	db := testdb.New(t)
	tenant := testdb.SeedTenant(t, db, "mcp-invoke")
	reformatted := atomic.Bool{}
	changedSchema := atomic.Bool{}
	server := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		if r.Method == http.MethodDelete {
			w.WriteHeader(http.StatusAccepted)
			return
		}
		var request struct {
			ID     int64  `json:"id"`
			Method string `json:"method"`
		}
		_ = json.NewDecoder(r.Body).Decode(&request)
		if request.Method == "notifications/initialized" {
			w.WriteHeader(http.StatusAccepted)
			return
		}
		w.Header().Set("Content-Type", "application/json")
		switch request.Method {
		case "initialize":
			fmt.Fprintf(w, `{"jsonrpc":"2.0","id":%d,"result":{"protocolVersion":"2025-06-18"}}`, request.ID)
		case "tools/list":
			input := `{"type":"object","properties":{"q":{"type":"string"}},"required":["q"],"additionalProperties":false}`
			if reformatted.Load() {
				input = "{\n  \"required\": [\"q\"],\n  \"additionalProperties\": false,\n  \"properties\": {\"q\": {\"type\": \"string\"}},\n  \"type\": \"object\"\n}"
			}
			if changedSchema.Load() {
				input = `{"type":"object","properties":{"q":{"type":"string"},"audit":{"type":"boolean"}},"required":["q","audit"],"additionalProperties":false}`
			}
			fmt.Fprintf(w, `{"jsonrpc":"2.0","id":%d,"result":{"tools":[{"name":"remote","inputSchema":%s,"outputSchema":{"type":"object"}}]}}`, request.ID, input)
		case "tools/call":
			fmt.Fprintf(w, `{"jsonrpc":"2.0","id":%d,"result":{"structuredContent":{"ok":true}}}`, request.ID)
		default:
			t.Errorf("unexpected MCP method %q", request.Method)
			http.Error(w, "unexpected", http.StatusBadRequest)
		}
	}))
	defer server.Close()

	repo := postgres.NewMCPToolsRepo(db)
	if err := repo.PutServer(context.Background(), postgres.MCPServer{TenantID: tenant, ID: "server", Origin: server.URL, EndpointPath: "/mcp", AuthorizedPrivate: true, AudienceScopes: []string{"assistant.use"}, Enabled: true}); err != nil {
		t.Fatal(err)
	}
	app := &App{mcpRepo: repo, mcpTransport: &mcp.StreamableHTTP{}}
	admin := auth.Principal{TenantID: tenant, Scopes: map[string]struct{}{"platform.admin": {}}}
	request := httptest.NewRequest(http.MethodPost, "/api/v1/mcp/servers/server/discovery", strings.NewReader(""))
	recorder := httptest.NewRecorder()
	app.handleMCPServerByID(recorder, request, admin)
	if recorder.Code != http.StatusOK {
		t.Fatalf("discovery status=%d body=%s", recorder.Code, recorder.Body.String())
	}
	approve := httptest.NewRequest(http.MethodPut, "/api/v1/mcp/servers/server/tools/remote", strings.NewReader(`{"model_name":"approved.lookup","caller_scopes":["mcp.use"],"schema_version":1}`))
	approveRec := httptest.NewRecorder()
	app.handleMCPServerByID(approveRec, approve, admin)
	if approveRec.Code != http.StatusOK {
		t.Fatalf("approval status=%d body=%s", approveRec.Code, approveRec.Body.String())
	}
	// Reload the approval so input_schema/output_schema round-trip through
	// real jsonb columns before the invocation.
	approval, err := repo.GetApproval(context.Background(), tenant, "server", "remote")
	if err != nil || !approval.Enabled {
		t.Fatalf("approval=%+v err=%v", approval, err)
	}
	executor := &mcp.Executor{Repository: repo, Caller: stubMCPCaller{}, Transport: app.mcpTransport}
	principal := auth.Principal{TenantID: tenant, Scopes: map[string]struct{}{"assistant.use": {}, "mcp.use": {}}}

	// The live server now advertises the same schema reformatted; the
	// canonical digest accepts it and the call dispatches.
	reformatted.Store(true)
	got, err := executor.Invoke(context.Background(), principal, "server", "remote", json.RawMessage(`{"q":"x"}`))
	if err != nil || string(got) != `{"ok":true}` {
		t.Fatalf("reformatted-schema invocation=%s err=%v", got, err)
	}

	// A genuinely changed schema (added a required property) is refused.
	changedSchema.Store(true)
	if _, err := executor.Invoke(context.Background(), principal, "server", "remote", json.RawMessage(`{"q":"x"}`)); !errors.Is(err, mcp.ErrDenied) {
		t.Fatalf("changed-schema invocation accepted: err=%v", err)
	}
}

func TestMCPAdministrationRejectsNonAdminBeforeRepositoryAccess(t *testing.T) {
	app := &App{}
	principal := auth.Principal{TenantID: "tenant", Scopes: map[string]struct{}{"chatbot": {}}}
	collection := httptest.NewRecorder()
	app.handleMCPServers(collection, httptest.NewRequest("GET", "/api/v1/mcp/servers", nil), principal)
	if collection.Code != http.StatusForbidden {
		t.Fatalf("collection status=%d", collection.Code)
	}
	for _, path := range []string{"/api/v1/mcp/servers/server", "/api/v1/mcp/servers/server/discovery", "/api/v1/mcp/servers/server/tools/tool"} {
		rec := httptest.NewRecorder()
		app.handleMCPServerByID(rec, httptest.NewRequest("GET", path, nil), principal)
		if rec.Code != http.StatusForbidden {
			t.Fatalf("%s status=%d body=%s", path, rec.Code, rec.Body.String())
		}
	}
}

func TestMCPScopesAreTrimmedAtHTTPBoundary(t *testing.T) {
	got := normalizeMCPScopeList([]string{" mcp.use ", "assistant.use "})
	if len(got) != 2 || got[0] != "mcp.use" || got[1] != "assistant.use" {
		t.Fatalf("normalized scopes = %#v", got)
	}
}
