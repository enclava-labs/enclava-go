package httpx

import (
	"context"
	"net/http"
	"net/http/httptest"
	"strings"
	"testing"

	"enclava-go/internal/auth"
	"enclava-go/internal/mcp"
	"enclava-go/internal/store/postgres"
	"enclava-go/internal/testdb"
)

func TestFailedDiscoveryPersistsServerHealth(t *testing.T) {
	db := testdb.New(t)
	tenant := testdb.SeedTenant(t, db, "mcp-health")
	upstream := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) { http.Error(w, "failure", http.StatusBadGateway) }))
	defer upstream.Close()
	repo := postgres.NewMCPToolsRepo(db)
	if err := repo.PutServer(context.Background(), mcp.Server{TenantID: tenant, ID: "server", Origin: upstream.URL, EndpointPath: "/mcp", AuthorizedPrivate: true, Enabled: true, AudienceScopes: []string{"assistant.use"}}); err != nil {
		t.Fatal(err)
	}
	app := &App{mcpRepo: repo, mcpTransport: &mcp.StreamableHTTP{}}
	p := auth.Principal{TenantID: tenant, Scopes: map[string]struct{}{"platform.admin": {}}}
	rec := httptest.NewRecorder()
	app.handleMCPServerByID(rec, httptest.NewRequest(http.MethodPost, "/api/v1/mcp/servers/server/discovery", strings.NewReader("")), p)
	server, err := repo.GetServer(context.Background(), tenant, "server")
	if rec.Code != http.StatusBadGateway || err != nil || server.DiscoveryAt.IsZero() || server.DiscoveryError == "" {
		t.Fatalf("status=%d server=%+v err=%v", rec.Code, server, err)
	}
}
