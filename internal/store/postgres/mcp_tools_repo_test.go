package postgres

import (
	"context"
	"encoding/json"
	"errors"
	"testing"
)

func TestMCPApprovalTenantSchemaRevisionAndRevocation(t *testing.T) {
	db := RequireTestDB(t)
	ctx := context.Background()
	seedTenant(t, ctx, db, idTenantA)
	seedTenant(t, ctx, db, idTenantB)
	r := NewMCPToolsRepo(db)
	s := MCPServer{TenantID: idTenantA, ID: "srv", Origin: "https://mcp.example", EndpointPath: "/mcp", CredentialRef: "opaque", AudienceScopes: []string{"mcp.invoke"}, Enabled: true}
	if err := r.PutServer(ctx, s); err != nil {
		t.Fatal(err)
	}
	server, err := r.GetServer(ctx, idTenantA, "srv")
	if err != nil || server.EndpointPath != "/mcp" {
		t.Fatalf("server endpoint path = %q, error = %v", server.EndpointPath, err)
	}
	changed := s
	changed.Origin = "https://different.example"
	if err := r.PutServer(ctx, changed); err == nil {
		t.Fatal("server origin changed without a new approval identity")
	}
	if _, err := r.GetServer(ctx, idTenantB, "srv"); !errors.Is(err, ErrMCPApprovalNotFound) {
		t.Fatalf("foreign tenant server lookup = %v", err)
	}
	a := MCPToolApproval{TenantID: idTenantA, ServerID: "srv", ToolID: "tool1", ModelName: "approved.search", RemoteName: "search", InputSchema: json.RawMessage(`{"type":"object","properties":{"q":{"type":"string"}},"required":["q"]}`), OutputSchema: json.RawMessage(`{"type":"object"}`), SchemaVersion: 1, CallerScopes: []string{"mcp.invoke"}, Enabled: true}
	if err := r.Approve(ctx, a); err != nil {
		t.Fatal(err)
	}
	got, err := r.GetApproval(ctx, idTenantA, "srv", "tool1")
	if err != nil {
		t.Fatal(err)
	}
	if got.Revision != 1 || got.InputSchemaHash == "" {
		t.Fatalf("bad approval: %+v", got)
	}
	if err := r.Approve(ctx, a); err != nil {
		t.Fatal(err)
	}
	got, err = r.GetApproval(ctx, idTenantA, "srv", "tool1")
	if err != nil {
		t.Fatal(err)
	}
	if got.Revision != 2 {
		t.Fatalf("revision=%d want 2", got.Revision)
	}
	if _, err = r.GetApproval(ctx, idTenantB, "srv", "tool1"); !errors.Is(err, ErrMCPApprovalNotFound) {
		t.Fatalf("foreign tenant approval lookup = %v", err)
	}
	if err = r.Revoke(ctx, idTenantA, "srv", "tool1"); err != nil {
		t.Fatal(err)
	}
	if _, err = r.GetApproval(ctx, idTenantA, "srv", "tool1"); !errors.Is(err, ErrMCPApprovalNotFound) {
		t.Fatalf("revoked approval = %v", err)
	}
	var revisions int
	if err = db.SQL.QueryRowContext(ctx, `SELECT count(*) FROM mcp_tool_approval_history WHERE tenant_id=$1::uuid AND server_id='srv' AND tool_id='tool1'`, idTenantA).Scan(&revisions); err != nil {
		t.Fatal(err)
	}
	if revisions != 3 {
		t.Fatalf("history revisions=%d want 3", revisions)
	}
}

func TestMCPServerRequiresBoundHTTPSOrigin(t *testing.T) {
	db := RequireTestDB(t)
	ctx := context.Background()
	seedTenant(t, ctx, db, idTenantA)
	r := NewMCPToolsRepo(db)
	for _, origin := range []string{"https://evil.example/path", "https://user@host.example", "http://mcp.example", "http://192.168.1.4:80"} {
		err := r.PutServer(ctx, MCPServer{TenantID: idTenantA, ID: "bad", Origin: origin, Enabled: true})
		if err == nil {
			t.Errorf("accepted origin %q", origin)
		}
	}
}
