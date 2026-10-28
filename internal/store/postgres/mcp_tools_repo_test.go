package postgres

import (
	"context"
	"encoding/json"
	"errors"
	"fmt"
	"strings"
	"sync"
	"testing"

	"enclava-go/internal/mcp"
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

func mcpDiscoveryRowByID(t *testing.T, r *MCPToolsRepo, tenantID, serverID, toolID string) MCPToolDiscovery {
	t.Helper()
	rows, err := r.ListServerTools(context.Background(), tenantID, serverID)
	if err != nil {
		t.Fatal(err)
	}
	for _, row := range rows {
		if row.ToolID == toolID {
			return row
		}
	}
	t.Fatalf("discovery row %s/%s not found", serverID, toolID)
	return MCPToolDiscovery{}
}

func mcpPutServer(t *testing.T, r *MCPToolsRepo, tenantID, serverID string) {
	t.Helper()
	if err := r.PutServer(context.Background(), MCPServer{TenantID: tenantID, ID: serverID, Origin: "https://mcp.example", EndpointPath: "/mcp", AudienceScopes: []string{"mcp.invoke"}, Enabled: true}); err != nil {
		t.Fatal(err)
	}
}

// TestMCPDiscoveryDigestSurvivesJSONBRoundTrip pins the canonical-digest
// contract against the real jsonb column type: however jsonb reformats a
// schema literal, the digest of what comes back must equal the digest stored.
func TestMCPDiscoveryDigestSurvivesJSONBRoundTrip(t *testing.T) {
	db := RequireTestDB(t)
	ctx := context.Background()
	seedTenant(t, ctx, db, idTenantA)
	r := NewMCPToolsRepo(db)
	mcpPutServer(t, r, idTenantA, "srv")
	schema := json.RawMessage(`{"type":"object","properties":{"query":{"type":"string"},"limit":{"type":"integer","default":10}},"required":["query"],"additionalProperties":false}`)
	tools := []DiscoveredToolSnapshot{{Name: "search", Title: "Search", Description: "Finds docs", InputSchema: schema, OutputSchema: json.RawMessage(`{"type":"object"}`), Annotations: mcp.ToolAnnotations{Title: "S", ReadOnlyHint: new(true)}}}
	if err := r.RecordDiscovery(ctx, idTenantA, "srv", tools); err != nil {
		t.Fatal(err)
	}
	var roundTripped string
	if err := db.SQL.QueryRowContext(ctx, `SELECT $1::jsonb::text`, string(schema)).Scan(&roundTripped); err != nil {
		t.Fatal(err)
	}
	if mcp.SchemaDigest(json.RawMessage(roundTripped)) != mcp.SchemaDigest(schema) {
		t.Fatal("digest changed across a real jsonb round-trip")
	}
	var inDigest, outDigest, annotations string
	var title, description string
	if err := db.SQL.QueryRowContext(ctx, `SELECT input_digest, output_digest, annotations::text, title, description FROM mcp_tool_discoveries WHERE tenant_id=$1::uuid AND server_id='srv' AND tool_id='search'`, idTenantA).Scan(&inDigest, &outDigest, &annotations, &title, &description); err != nil {
		t.Fatal(err)
	}
	var storedSchema string
	if err := db.SQL.QueryRowContext(ctx, `SELECT input_schema::text FROM mcp_tool_discoveries WHERE tenant_id=$1::uuid AND server_id='srv' AND tool_id='search'`, idTenantA).Scan(&storedSchema); err != nil {
		t.Fatal(err)
	}
	if mcp.SchemaDigest(json.RawMessage(storedSchema)) != inDigest {
		t.Fatalf("stored digest %s does not match the jsonb payload digest %s", inDigest, mcp.SchemaDigest(json.RawMessage(storedSchema)))
	}
	reordered := json.RawMessage(`{"additionalProperties":false,"properties":{"limit":{"default":10,"type":"integer"},"query":{"type":"string"}},"required":["query"],"type":"object"}`)
	if mcp.SchemaDigest(reordered) != mcp.SchemaDigest(schema) {
		t.Fatal("key order changed the canonical digest")
	}
	if title != "Search" || description != "Finds docs" || !strings.Contains(annotations, `"readOnlyHint"`) {
		t.Fatalf("safe metadata not persisted: title=%q description=%q annotations=%s", title, description, annotations)
	}
}

// TestMCPDiscoveryStateTransitionsAndEmptySnapshotIsolation covers the
// durable queue state machine: pending -> approved -> changed (baseline +
// diff + revocation) -> gone, with schema_version initialized at 1, stable
// on identical rediscovery, +1 on change, and readable after a fresh repo
// handle (simulated restart). An empty snapshot revokes exactly its own
// server's approvals; another server's approved tool is untouched.
func TestMCPDiscoveryStateTransitionsAndEmptySnapshotIsolation(t *testing.T) {
	db := RequireTestDB(t)
	ctx := context.Background()
	seedTenant(t, ctx, db, idTenantA)
	r := NewMCPToolsRepo(db)
	mcpPutServer(t, r, idTenantA, "srv-a")
	mcpPutServer(t, r, idTenantA, "srv-b")
	s1 := json.RawMessage(`{"type":"object","properties":{"q":{"type":"string"}}}`)
	s2 := json.RawMessage(`{"type":"object","properties":{"q":{"type":"string"},"limit":{"type":"integer"}}}`)
	out := json.RawMessage(`{"type":"object"}`)

	if err := r.RecordDiscovery(ctx, idTenantA, "srv-a", []DiscoveredToolSnapshot{{Name: "search", InputSchema: s1, OutputSchema: out}}); err != nil {
		t.Fatal(err)
	}
	row := mcpDiscoveryRowByID(t, r, idTenantA, "srv-a", "search")
	if row.State != "pending" || row.SchemaVersion != 1 || row.ApprovedSchemaVersion.Valid || row.PriorInputSchema != nil {
		t.Fatalf("new tool = %+v", row)
	}
	approval, err := r.ApproveDiscovery(ctx, idTenantA, "srv-a", "search", 1, "approved.search", []string{"mcp.invoke"})
	if err != nil {
		t.Fatal(err)
	}
	if !approval.Enabled || approval.SchemaVersion != 1 || approval.InputSchemaHash != mcp.SchemaDigest(s1) {
		t.Fatalf("approval = %+v", approval)
	}
	if got, err := r.GetApproval(ctx, idTenantA, "srv-a", "search"); err != nil || !got.Enabled {
		t.Fatalf("enabled approval = %+v err=%v", got, err)
	}
	// Identical rediscovery: approved stays approved, version stays 1.
	if err := r.RecordDiscovery(ctx, idTenantA, "srv-a", []DiscoveredToolSnapshot{{Name: "search", InputSchema: s1, OutputSchema: out}}); err != nil {
		t.Fatal(err)
	}
	row = mcpDiscoveryRowByID(t, r, idTenantA, "srv-a", "search")
	if row.State != "approved" || row.SchemaVersion != 1 || !row.ApprovedSchemaVersion.Valid || row.PriorInputSchema != nil {
		t.Fatalf("identical rediscovery = %+v", row)
	}
	// Changed schema: changed + version bump + retained baseline + diff + revocation.
	if err := r.RecordDiscovery(ctx, idTenantA, "srv-a", []DiscoveredToolSnapshot{{Name: "search", InputSchema: s2, OutputSchema: out}}); err != nil {
		t.Fatal(err)
	}
	row = mcpDiscoveryRowByID(t, r, idTenantA, "srv-a", "search")
	if row.State != "changed" || row.SchemaVersion != 2 || !row.ApprovedSchemaVersion.Valid || row.ApprovedSchemaVersion.Int64 != 1 {
		t.Fatalf("changed rediscovery = %+v", row)
	}
	// Changed rows carry the pre-change schema; jsonb may reformat it, so
	// compare by canonical digest, never by literal bytes.
	if row.PriorInputSchema == nil || mcp.SchemaDigest(row.PriorInputSchema) != mcp.SchemaDigest(s1) {
		t.Fatalf("prior baseline = %s", row.PriorInputSchema)
	}
	var diffFields []struct {
		Field  string `json:"field"`
		Change string `json:"change"`
	}
	if err := json.Unmarshal(row.DiffFields, &diffFields); err != nil || len(diffFields) != 1 ||
		diffFields[0].Field != "limit" || diffFields[0].Change != "added" {
		t.Fatalf("diff fields = %s (%v)", row.DiffFields, err)
	}
	if got, err := r.GetApprovalAny(ctx, idTenantA, "srv-a", "search"); err != nil || got.Enabled {
		t.Fatalf("stale approval survived: %+v err=%v", got, err)
	}
	// Simulated restart: a fresh repo handle reads the same durable version.
	restarted := NewMCPToolsRepo(db)
	if row := mcpDiscoveryRowByID(t, restarted, idTenantA, "srv-a", "search"); row.State != "changed" || row.SchemaVersion != 2 {
		t.Fatalf("post-restart row = %+v", row)
	}
	// Another server approves its own tool; an empty snapshot on srv-a must
	// not touch it.
	if err := r.RecordDiscovery(ctx, idTenantA, "srv-b", []DiscoveredToolSnapshot{{Name: "other", InputSchema: s1, OutputSchema: out}}); err != nil {
		t.Fatal(err)
	}
	if _, err := r.ApproveDiscovery(ctx, idTenantA, "srv-b", "other", 1, "approved.other", []string{"mcp.invoke"}); err != nil {
		t.Fatal(err)
	}
	if err := r.RecordDiscovery(ctx, idTenantA, "srv-a", nil); err != nil {
		t.Fatal(err)
	}
	row = mcpDiscoveryRowByID(t, r, idTenantA, "srv-a", "search")
	if row.State != "gone" {
		t.Fatalf("vanished tool = %+v", row)
	}
	if got, err := r.GetApprovalAny(ctx, idTenantA, "srv-a", "search"); err != nil || got.Enabled {
		t.Fatalf("gone tool approval = %+v err=%v", got, err)
	}
	if row := mcpDiscoveryRowByID(t, r, idTenantA, "srv-b", "other"); row.State != "approved" {
		t.Fatalf("other server's approved tool disturbed: %+v", row)
	}
	if got, err := r.GetApproval(ctx, idTenantA, "srv-b", "other"); err != nil || !got.Enabled {
		t.Fatalf("other server's approval = %+v err=%v", got, err)
	}
	if _, err := restarted.ListToolsByState(ctx, idTenantA, "changed", "pending"); err != nil {
		t.Fatal(err)
	}
	byState, err := restarted.ListToolsByState(ctx, idTenantA, "gone")
	if err != nil || len(byState) != 1 || byState[0].ToolID != "search" || byState[0].ServerID != "srv-a" {
		t.Fatalf("ListToolsByState(gone) = %+v err=%v", byState, err)
	}
}

// TestMCPApproveDiscoveryStaleReviewAndRejectMatrix covers 409 stale review,
// durable reject of never-approved tools, idempotent reject, rediscovery of
// rejected tools, and 404s for gone/unknown/rejected approvals.
func TestMCPApproveDiscoveryStaleReviewAndRejectMatrix(t *testing.T) {
	db := RequireTestDB(t)
	ctx := context.Background()
	seedTenant(t, ctx, db, idTenantA)
	r := NewMCPToolsRepo(db)
	mcpPutServer(t, r, idTenantA, "srv")
	s1 := json.RawMessage(`{"type":"object","properties":{"q":{"type":"string"}}}`)
	s2 := json.RawMessage(`{"type":"object","properties":{"q":{"type":"integer"}}}`)
	out := json.RawMessage(`{"type":"object"}`)

	if _, err := r.ApproveDiscovery(ctx, idTenantA, "srv", "search", 1, "approved.search", []string{"mcp.invoke"}); !errors.Is(err, ErrMCPApprovalNotFound) {
		t.Fatalf("unknown tool approval = %v", err)
	}
	if err := r.RecordDiscovery(ctx, idTenantA, "srv", []DiscoveredToolSnapshot{{Name: "search", InputSchema: s1, OutputSchema: out}}); err != nil {
		t.Fatal(err)
	}
	if _, err := r.ApproveDiscovery(ctx, idTenantA, "srv", "search", 99, "approved.search", []string{"mcp.invoke"}); !errors.Is(err, ErrMCPSchemaChanged) {
		t.Fatalf("stale version approval = %v", err)
	}
	// Durable reject of a never-approved tool.
	if err := r.RejectDiscovery(ctx, idTenantA, "srv", "search"); err != nil {
		t.Fatal(err)
	}
	if row := mcpDiscoveryRowByID(t, r, idTenantA, "srv", "search"); row.State != "rejected" {
		t.Fatalf("rejected row = %+v", row)
	}
	if err := r.RejectDiscovery(ctx, idTenantA, "srv", "search"); err != nil {
		t.Fatalf("idempotent reject = %v", err)
	}
	// Rediscovery of a rejected tool keeps it rejected and moves the version.
	if err := r.RecordDiscovery(ctx, idTenantA, "srv", []DiscoveredToolSnapshot{{Name: "search", InputSchema: s2, OutputSchema: out}}); err != nil {
		t.Fatal(err)
	}
	row := mcpDiscoveryRowByID(t, r, idTenantA, "srv", "search")
	if row.State != "rejected" || row.SchemaVersion != 2 {
		t.Fatalf("rediscovered rejected row = %+v", row)
	}
	if _, err := r.ApproveDiscovery(ctx, idTenantA, "srv", "search", 2, "approved.search", []string{"mcp.invoke"}); !errors.Is(err, ErrMCPApprovalNotFound) {
		t.Fatalf("approval of a rejected tool = %v", err)
	}
	// Approved tool: reject revokes the approval and rejects the row.
	if err := r.RecordDiscovery(ctx, idTenantA, "srv", []DiscoveredToolSnapshot{{Name: "search", InputSchema: s2, OutputSchema: out}, {Name: "second", InputSchema: s1, OutputSchema: out}}); err != nil {
		t.Fatal(err)
	}
	if _, err := r.ApproveDiscovery(ctx, idTenantA, "srv", "second", 1, "approved.second", []string{"mcp.invoke"}); err != nil {
		t.Fatal(err)
	}
	if err := r.RejectDiscovery(ctx, idTenantA, "srv", "second"); err != nil {
		t.Fatal(err)
	}
	if got, err := r.GetApprovalAny(ctx, idTenantA, "srv", "second"); err != nil || got.Enabled {
		t.Fatalf("rejected approval = %+v err=%v", got, err)
	}
	if row := mcpDiscoveryRowByID(t, r, idTenantA, "srv", "second"); row.State != "rejected" {
		t.Fatalf("approved-then-rejected row = %+v", row)
	}
	// Gone tools are neither approvable nor rejectable.
	if err := r.RecordDiscovery(ctx, idTenantA, "srv", nil); err != nil {
		t.Fatal(err)
	}
	if err := r.RejectDiscovery(ctx, idTenantA, "srv", "search"); !errors.Is(err, ErrMCPApprovalNotFound) {
		t.Fatalf("gone reject = %v", err)
	}
	if _, err := r.ApproveDiscovery(ctx, idTenantA, "srv", "search", 3, "approved.search", []string{"mcp.invoke"}); !errors.Is(err, ErrMCPApprovalNotFound) {
		t.Fatalf("gone approve = %v", err)
	}
}

// TestMCPApproveDiscoveryRacesDiscoveryAtomically proves the compare and the
// approval install cannot be split by a concurrent discovery: under a
// barrier-released approve/changed-discovery race both orders are legal and
// each lands its own exact state — approve first yields changed@v2 with the
// v1 baseline and a revoked approval; discovery first yields never-approved
// pending@v2 with no baseline and the stale review refused with no install.
func TestMCPApproveDiscoveryRacesDiscoveryAtomically(t *testing.T) {
	db := RequireTestDB(t)
	ctx := context.Background()
	seedTenant(t, ctx, db, idTenantA)
	r := NewMCPToolsRepo(db)
	s1 := json.RawMessage(`{"type":"object","properties":{"q":{"type":"string"}}}`)
	s2 := json.RawMessage(`{"type":"object","properties":{"q":{"type":"string"},"limit":{"type":"integer"}}}`)
	out := json.RawMessage(`{"type":"object"}`)
	for i := 0; i < 25; i++ {
		serverID := fmt.Sprintf("srv-race-%d", i)
		mcpPutServer(t, r, idTenantA, serverID)
		if err := r.RecordDiscovery(ctx, idTenantA, serverID, []DiscoveredToolSnapshot{{Name: "search", InputSchema: s1, OutputSchema: out}}); err != nil {
			t.Fatal(err)
		}
		start := make(chan struct{})
		var wg sync.WaitGroup
		var approveErr, recordErr error
		wg.Add(2)
		go func() {
			defer wg.Done()
			<-start
			_, approveErr = r.ApproveDiscovery(ctx, idTenantA, serverID, "search", 1, fmt.Sprintf("approved.search.%d", i), []string{"mcp.invoke"})
		}()
		go func() {
			defer wg.Done()
			<-start
			recordErr = r.RecordDiscovery(ctx, idTenantA, serverID, []DiscoveredToolSnapshot{{Name: "search", InputSchema: s2, OutputSchema: out}})
		}()
		close(start)
		wg.Wait()
		if recordErr != nil {
			t.Fatalf("iteration %d: discovery race error = %v", i, recordErr)
		}
		row := mcpDiscoveryRowByID(t, r, idTenantA, serverID, "search")
		if approveErr == nil {
			// Approve committed first: the changed discovery then revokes it
			// — changed@v2 carrying the v1 schema as baseline.
			if row.State != "changed" || row.SchemaVersion != 2 || !row.ApprovedSchemaVersion.Valid || row.ApprovedSchemaVersion.Int64 != 1 {
				t.Fatalf("iteration %d: approve-first row = %+v", i, row)
			}
			if row.PriorInputSchema == nil || mcp.SchemaDigest(row.PriorInputSchema) != mcp.SchemaDigest(s1) {
				t.Fatalf("iteration %d: approve-first prior = %s", i, row.PriorInputSchema)
			}
			if got, err := r.GetApprovalAny(ctx, idTenantA, serverID, "search"); err != nil || got.Enabled {
				t.Fatalf("iteration %d: approve-first approval = %+v err=%v", i, got, err)
			}
		} else {
			// Discovery committed first: the row was never approved, so it
			// stays pending@v2 with no baseline and the stale review is a
			// 409 that installed nothing.
			if !errors.Is(approveErr, ErrMCPSchemaChanged) {
				t.Fatalf("iteration %d: approve race error = %v", i, approveErr)
			}
			if row.State != "pending" || row.SchemaVersion != 2 || row.PriorInputSchema != nil {
				t.Fatalf("iteration %d: discover-first row = %+v", i, row)
			}
			if _, err := r.GetApprovalAny(ctx, idTenantA, serverID, "search"); !errors.Is(err, ErrMCPApprovalNotFound) {
				t.Fatalf("iteration %d: discover-first approval = %v", i, err)
			}
		}
	}
	// Identical rediscovery racing an approve never loses the approval.
	mcpPutServer(t, r, idTenantA, "srv-race-stable")
	if err := r.RecordDiscovery(ctx, idTenantA, "srv-race-stable", []DiscoveredToolSnapshot{{Name: "search", InputSchema: s1, OutputSchema: out}}); err != nil {
		t.Fatal(err)
	}
	start := make(chan struct{})
	var wg sync.WaitGroup
	var approveErr, recordErr error
	wg.Add(2)
	go func() {
		defer wg.Done()
		<-start
		_, approveErr = r.ApproveDiscovery(ctx, idTenantA, "srv-race-stable", "search", 1, "approved.search", []string{"mcp.invoke"})
	}()
	go func() {
		defer wg.Done()
		<-start
		recordErr = r.RecordDiscovery(ctx, idTenantA, "srv-race-stable", []DiscoveredToolSnapshot{{Name: "search", InputSchema: s1, OutputSchema: out}})
	}()
	close(start)
	wg.Wait()
	if approveErr != nil || recordErr != nil {
		t.Fatalf("stable race errors = %v / %v", approveErr, recordErr)
	}
	row := mcpDiscoveryRowByID(t, r, idTenantA, "srv-race-stable", "search")
	if row.State != "approved" || row.SchemaVersion != 1 {
		t.Fatalf("stable post-race row = %+v", row)
	}
	if got, err := r.GetApproval(ctx, idTenantA, "srv-race-stable", "search"); err != nil || !got.Enabled {
		t.Fatalf("stable race approval = %+v err=%v", got, err)
	}
}
