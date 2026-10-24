package httpx

import (
	"context"
	"encoding/json"
	"fmt"
	"net/http"
	"net/http/httptest"
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
			input := `{"type":"object","additionalProperties":false}`
			if changed.Load() {
				input = `{"type":"object","properties":{"q":{"type":"string"}},"required":["q"],"additionalProperties":false}`
			}
			fmt.Fprintf(w, `{"jsonrpc":"2.0","id":%d,"result":{"tools":[{"name":"remote","inputSchema":%s,"outputSchema":{"type":"object"}}]}}`, request.ID, input)
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
	discovery := call(http.MethodPost, "/api/v1/mcp/servers/server/discovery", "")
	if discovery.Code != http.StatusOK || !strings.Contains(discovery.Body.String(), `"input_schema"`) {
		t.Fatalf("discovery status=%d body=%s", discovery.Code, discovery.Body.String())
	}
	if approvals, err := repo.ListAllApprovals(context.Background(), tenant, "server"); err != nil || len(approvals) != 0 {
		t.Fatalf("discovery auto-approved tools=%v err=%v", approvals, err)
	}
	approve := `{"model_name":"approved.lookup","caller_scopes":["mcp.use"]}`
	if got := call(http.MethodPut, "/api/v1/mcp/servers/server/tools/remote", approve); got.Code != http.StatusOK {
		t.Fatalf("approval status=%d body=%s", got.Code, got.Body.String())
	}
	approvalList := call(http.MethodGet, "/api/v1/mcp/servers/server/tools", "")
	if approvalList.Code != http.StatusOK || !strings.Contains(approvalList.Body.String(), `"model_name":"approved.lookup"`) || !strings.Contains(approvalList.Body.String(), `"input_schema"`) {
		t.Fatalf("approval list status=%d body=%s", approvalList.Code, approvalList.Body.String())
	}
	current, err := repo.GetApproval(context.Background(), tenant, "server", "remote")
	if err != nil || current.SchemaVersion != 1 {
		t.Fatalf("initial approval=%+v err=%v", current, err)
	}
	changed.Store(true)
	if got := call(http.MethodPost, "/api/v1/mcp/servers/server/discovery", ""); got.Code != http.StatusOK {
		t.Fatalf("changed-schema discovery status=%d body=%s", got.Code, got.Body.String())
	}
	current, err = repo.GetApprovalAny(context.Background(), tenant, "server", "remote")
	if err != nil || current.Enabled {
		t.Fatalf("changed schema remained enabled: %+v err=%v", current, err)
	}
	if got := call(http.MethodPut, "/api/v1/mcp/servers/server/tools/remote", approve); got.Code != http.StatusOK {
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

func TestMCPDashboardIsAdminOnlyAndUsesPublicAPIs(t *testing.T) {
	app := &App{}
	nonAdmin := httptest.NewRequest(http.MethodGet, "/dashboard/partials/mcp", nil)
	nonAdminRec := httptest.NewRecorder()
	app.handleDashboardMCP(nonAdminRec, nonAdmin)
	if nonAdminRec.Code != http.StatusForbidden {
		t.Fatalf("non-admin partial status=%d", nonAdminRec.Code)
	}
	admin := auth.Principal{TenantID: "tenant", Scopes: map[string]struct{}{"platform.admin": {}}}
	request := httptest.NewRequest(http.MethodGet, "/dashboard/partials/mcp", nil)
	request = request.WithContext(auth.WithPrincipal(request.Context(), admin))
	rec := httptest.NewRecorder()
	app.handleDashboardMCP(rec, request)
	if rec.Code != http.StatusOK || !strings.Contains(rec.Body.String(), "mcp-server-form") || !strings.Contains(rec.Body.String(), "/dashboard/static/mcp.js") {
		t.Fatalf("admin partial status=%d body=%s", rec.Code, rec.Body.String())
	}
}

func TestMCPScopesAreTrimmedAtHTTPBoundary(t *testing.T) {
	got := normalizeMCPScopeList([]string{" mcp.use ", "assistant.use "})
	if len(got) != 2 || got[0] != "mcp.use" || got[1] != "assistant.use" {
		t.Fatalf("normalized scopes = %#v", got)
	}
}
