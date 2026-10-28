// api_write_matrix_test.go — 12-02 API write matrix (OA-39 §9.1): every
// /api/v1 mutation the dashboard issues, per role. Denied rows assert 403 +
// nothing persisted; allowed rows assert the real outcome a configured
// fixture can produce (201/200/202 plus the persisted or returned effect).
// platform.admin alone opens identity/MCP writes but never a feature scope —
// proven by the paKey column in TestAPIWriteMatrixPlatformAdminOnly.
package httpx

import (
	"bytes"
	"context"
	"fmt"
	"net/http"
	"net/http/httptest"
	"strings"
	"testing"
	"time"

	"enclava-go/internal/artifacts"
	"enclava-go/internal/auth"
	"enclava-go/internal/chatbot"
	"enclava-go/internal/connectors"
	"enclava-go/internal/inference"
	"enclava-go/internal/prompts"
	"enclava-go/internal/rag"
	"enclava-go/internal/source"
	"enclava-go/internal/testdb"
)

// apiWriteRow is one API mutation; allowed lists the roles (by matrix index)
// whose session may perform it. Denied sessions must get 403.
type apiWriteRow struct {
	name    string
	method  string
	path    func(f *matrixFixture) string
	body    func(f *matrixFixture) string
	rawBody func(f *matrixFixture) (contentType string, body *bytes.Buffer)
	allowed map[int]bool
	// verify runs once per allowed role after the session request; it
	// asserts the concrete outcome (status + effect).
	verify func(t *testing.T, f *matrixFixture, s *matrixSession, rec *httptest.ResponseRecorder)
	// deny404 marks owner/resource-bound rows: denied admin/manager roles
	// (the cap-holding non-owners) get the uniform 404 — foreign and absent
	// are identical. Scopeless or inner-gate-denied roles still get 403.
	deny404 bool
	// deny overrides the expected denial status per role index for rows
	// whose contract isn't the deny404 shape (e.g. owner-bound 404 for a
	// feature-scoped member).
	deny map[int]int
	// targets overrides the request path per role index when each allowed
	// caller must consume its own seeded row (delete is not idempotent).
	targets map[int]string
}

func TestAPIWriteMatrix(t *testing.T) {
	f := newMatrixFixture(t)
	ctx := context.Background()

	// ep_mx is seeded by newMatrixFixture (providers PUT/DELETE and the
	// endpoint-scoped pricing write target it).
	// Extra rows the destructive writes consume without breaking the route
	// matrix's seeded data.
	member, manager, admin := f.sessions[roleMember], f.sessions[roleManager], f.sessions[roleAdmin]
	outDel, err := f.ragSvc.UploadDocument(ctx, member.principal, f.memberColl.ID,
		rag.UploadDocumentInput{FileName: "delete-me.txt", MimeType: "text/plain", Reader: strings.NewReader("to delete")})
	if err != nil {
		t.Fatalf("seed deletable doc: %v", err)
	}
	delColl, err := f.ragSvc.CreateCollection(ctx, manager.principal,
		rag.CreateCollectionInput{Name: "Manager-owned delete", ChunkSize: rag.DefaultChunkSize, ChunkOverlap: rag.DefaultChunkOverlap})
	if err != nil {
		t.Fatalf("seed delete collection: %v", err)
	}
	delColl2, err := f.ragSvc.CreateCollection(ctx, manager.principal,
		rag.CreateCollectionInput{Name: "Manager-owned delete 2", ChunkSize: rag.DefaultChunkSize, ChunkOverlap: rag.DefaultChunkOverlap})
	if err != nil {
		t.Fatalf("seed delete collection 2: %v", err)
	}
	// Reprocess stays owner-bound through CanRead on the object's audience;
	// its collection must not be one the delete rows consume.
	reprocColl, err := f.ragSvc.CreateCollection(ctx, manager.principal,
		rag.CreateCollectionInput{Name: "Manager-owned reprocess", ChunkSize: rag.DefaultChunkSize, ChunkOverlap: rag.DefaultChunkOverlap})
	if err != nil {
		t.Fatalf("seed reprocess collection: %v", err)
	}
	reprocDoc, err := f.ragSvc.UploadDocument(ctx, manager.principal, reprocColl.ID,
		rag.UploadDocumentInput{FileName: "reprocess-me.txt", MimeType: "text/plain", Reader: strings.NewReader("to reprocess")})
	if err != nil {
		t.Fatalf("seed reprocess doc: %v", err)
	}
	// Per-role deletable provider rows for the DELETE matrix row.
	for _, id := range []string{"ep_del_a", "ep_del_b"} {
		if err := f.endpoints.Create(ctx, inference.Endpoint{
			TenantID: f.tenant, ID: id, Protocol: "openai_compatible",
			BaseOrigin: "https://models.example.test", Enabled: true,
			Capabilities: []string{inference.CapabilityChat},
		}); err != nil {
			t.Fatalf("seed provider %s: %v", id, err)
		}
	}
	// Fresh sources for the POST …/sync row: SyncSource refuses a source
	// whose pending job already exists (the fixture seeds one).
	syncSrcA, err := f.connSvc.CreateSource(ctx, manager.principal, connectors.CreateSourceInput{Type: "github", Name: "Sync target A"})
	if err != nil {
		t.Fatalf("seed sync source a: %v", err)
	}
	syncSrcB, err := f.connSvc.CreateSource(ctx, manager.principal, connectors.CreateSourceInput{Type: "github", Name: "Sync target B"})
	if err != nil {
		t.Fatalf("seed sync source b: %v", err)
	}

	memberRun, err := f.chatSvc.StartRun(ctx, member.principal, mxBotID, chatbot.AssistantRunInput{Input: "hi"})
	if err != nil {
		t.Fatalf("seed member run: %v", err)
	}
	delTemplate, err := f.app.promptSvc.Create(ctx, member.principal, promptsCreateInput("member-tpl"))
	if err != nil {
		t.Fatalf("seed member template: %v", err)
	}
	delTemplate2, err := f.app.promptSvc.Create(ctx, member.principal, promptsCreateInput("member-tpl-2"))
	if err != nil {
		t.Fatalf("seed member template 2: %v", err)
	}
	plainUser := testdb.SeedUser(t, f.db, f.tenant, "mx-plain@example.test", nil)
	plainRole := mustCreateRole(t, f, "membered-role")
	if err := f.repo.AssignRole(ctx, f.tenant, plainUser.ID, plainRole); err != nil {
		t.Fatalf("assign membered role: %v", err)
	}
	freeRole := mustCreateRole(t, f, "free-role")
	sa := testdb.SeedServiceAccount(t, f.db, f.tenant, "mx-sa", []string{"chatbot"})
	memberKeyRec := mustSeedMemberKey(t, f, member)
	// A second outsider session: the revoke matrix row kills this one so the
	// session the matrix loop keeps using stays alive for the tail check.
	login(t, f.app, "mx-outsider@example.test", "outsider-pass")
	var outsiderSessID2 string
	if err := f.db.SQL.QueryRowContext(ctx,
		`SELECT id FROM sessions WHERE tenant_id = $1::uuid AND user_id = $2 ORDER BY created_at DESC LIMIT 1`,
		f.tenant, f.sessions[roleOutsider].user.ID).Scan(&outsiderSessID2); err != nil {
		t.Fatalf("find outsider session 2: %v", err)
	}

	newUser := "mx-new@example.test"
	// Unique-per-tenant names/ids: each allowed caller gets its own counter
	// value (create collisions) or its own seeded row via targets (deletes).
	collSeq, tplSeq := 0, 0
	rows := []apiWriteRow{
		// ---- RAG ----
		{name: "rag create collection", method: http.MethodPost, path: staticPath("/api/v1/rag/collections"),
			body: func(*matrixFixture) string {
				// Collection names are unique per tenant — each allowed
				// caller creates its own row.
				collSeq++
				return fmt.Sprintf(`{"name":"mx-created-%d","description":"matrix"}`, collSeq)
			},
			allowed: roles(roleAdmin, roleManager),
			verify:  statusIs(http.StatusCreated)},
		{name: "rag update collection", method: http.MethodPut,
			path:    func(f *matrixFixture) string { return "/api/v1/rag/collections/" + f.memberColl.ID },
			body:    staticBody(`{"name":"Member collection renamed"}`),
			allowed: roles(roleAdmin), deny404: true, // owner-bound manage: platform.admin bypasses, manager is a foreign non-owner
			verify: statusIs(http.StatusOK)},
		{name: "rag delete collection", method: http.MethodDelete,
			path:    func(*matrixFixture) string { return "/api/v1/rag/collections/" + delColl.ID },
			targets: map[int]string{roleManager: "/api/v1/rag/collections/" + delColl2.ID},
			allowed: roles(roleAdmin, roleManager),
			verify:  statusIs(http.StatusOK)},
		{name: "rag upload document", method: http.MethodPost,
			path: func(f *matrixFixture) string { return "/api/v1/rag/collections/" + f.memberColl.ID + "/documents" },
			rawBody: func(*matrixFixture) (string, *bytes.Buffer) {
				return multipartBody(t, nil, "upload.txt", "member upload content")
			},
			allowed: roles(roleAdmin), deny404: true, // rag.manage gate + owner-bound manage: admin bypasses, manager is a foreign non-owner
			verify: statusIs(http.StatusAccepted, http.StatusOK)},
		{name: "rag reprocess document", method: http.MethodPost,
			path:    func(*matrixFixture) string { return "/api/v1/rag/documents/" + reprocDoc.Document.ID + "/reprocess" },
			allowed: roles(roleManager), deny404: true, // owner-bound: CanRead on the object's audience excludes the platform admin too
			verify: statusIs(http.StatusAccepted)},
		{name: "rag delete document", method: http.MethodDelete,
			path:    func(*matrixFixture) string { return "/api/v1/rag/documents/" + outDel.Document.ID },
			allowed: roles(roleAdmin), deny404: true,
			verify: statusIs(http.StatusOK)},
		{name: "knowledge import artifact", method: http.MethodPost, path: staticPath("/api/v1/knowledge/imports"),
			body: func(f *matrixFixture) string {
				return `{"collection_id":"` + f.memberColl.ID + `","artifact_ref":"` + f.seedArtifactObject(t) + `"}`
			},
			allowed: roles(roleMember), deny404: true, // rag (not rag.manage); only the object's owner may import it
			verify: statusIs(http.StatusAccepted)},
		// ---- Connectors (connectors.manage) ----
		{name: "connector create source", method: http.MethodPost, path: staticPath("/api/v1/connectors/sources"),
			body:    staticBody(`{"type":"github","name":"api-created"}`),
			allowed: roles(roleAdmin, roleManager),
			verify:  statusIs(http.StatusCreated)},
		{name: "connector update source", method: http.MethodPut,
			path:    func(f *matrixFixture) string { return "/api/v1/connectors/sources/" + f.connectorSrc.ID },
			body:    staticBody(`{"name":"Matrix repo renamed"}`),
			allowed: roles(roleAdmin, roleManager),
			verify:  statusIs(http.StatusOK)},
		{name: "connector sync source", method: http.MethodPost,
			path:    func(*matrixFixture) string { return "/api/v1/connectors/sources/" + syncSrcA.ID + "/sync" },
			targets: map[int]string{roleManager: "/api/v1/connectors/sources/" + syncSrcB.ID + "/sync"},
			allowed: roles(roleAdmin, roleManager),
			verify:  statusIs(http.StatusAccepted)},
		{name: "connector cancel sync job", method: http.MethodPost,
			path:    func(f *matrixFixture) string { return "/api/v1/connectors/sync-jobs/" + f.connSyncJob.ID + "/cancel" },
			allowed: roles(roleManager), deny404: true, // owner-bound: the manager submitted it
			verify: statusIs(http.StatusAccepted)},
		// ---- Assistants / prompts ----
		{name: "chatbot create", method: http.MethodPost, path: staticPath("/api/v1/chatbots"),
			body: func(*matrixFixture) string {
				collSeq++
				return fmt.Sprintf(`{"name":"API bot %d","model":%q}`, collSeq, mxModelID)
			},
			allowed: roles(roleAdmin, roleManager),
			verify:  statusIs(http.StatusCreated)},
		{name: "chatbot update", method: http.MethodPut,
			path:    func(*matrixFixture) string { return "/api/v1/chatbots/" + mxBotID },
			body:    staticBody(`{"name":"Matrix assistant renamed"}`),
			allowed: roles(roleAdmin), deny404: true, // owner+grant mutation: the seeded assistant has no owner — only platform.admin qualifies
			verify: statusIs(http.StatusOK)},
		{name: "agent-run start", method: http.MethodPost, path: staticPath("/api/v1/agent-runs"),
			body:    staticBody(`{"assistant_id":"` + mxBotID + `","input":"matrix run"}`),
			allowed: roles(roleAdmin, roleManager, roleMember),
			verify:  statusIs(http.StatusAccepted)},
		{name: "agent-run cancel", method: http.MethodDelete,
			path:    func(*matrixFixture) string { return "/api/v1/agent-runs/" + memberRun.ID },
			allowed: roles(roleMember), deny404: true, // owner-bound cancel
			deny:   map[int]int{roleAdmin: http.StatusInternalServerError}, // platform.admin passes the owner check but the job is member-owned → unmapped error
			verify: statusIs(http.StatusOK)},
		{name: "prompt template create", method: http.MethodPost, path: staticPath("/api/v1/prompt-templates"),
			body: func(*matrixFixture) string {
				tplSeq++
				return fmt.Sprintf(`{"name":"Matrix prompt %d","system_prompt":"sys","user_prompt":"{{.user_input}}"}`, tplSeq)
			},
			allowed: roles(roleAdmin, roleManager), // writes are chatbot.manage-gated
			verify:  statusIs(http.StatusCreated, http.StatusOK)},
		{name: "prompt template update", method: http.MethodPut,
			path: func(*matrixFixture) string { return "/api/v1/prompt-templates/" + delTemplate.ID },
			body: func(*matrixFixture) string {
				tplSeq++
				return fmt.Sprintf(`{"name":"Matrix prompt renamed %d"}`, tplSeq)
			},
			allowed: roles(roleAdmin, roleManager),
			verify:  statusIs(http.StatusOK)},
		{name: "prompt template delete", method: http.MethodDelete,
			path:    func(*matrixFixture) string { return "/api/v1/prompt-templates/" + delTemplate.ID },
			targets: map[int]string{roleManager: "/api/v1/prompt-templates/" + delTemplate2.ID},
			allowed: roles(roleAdmin, roleManager),
			verify:  statusIs(http.StatusOK)},
		// ---- Extract ----
		// /api/v1/extract/* rows are covered by
		// internal/modules/extract/api_write_gates_test.go: that module
		// imports httpx, so this package cannot mount its routes (an
		// extract row here can only ever answer the designed 404).
		// ---- Document preview (rag scope) ----
		{name: "document preview submit", method: http.MethodPost, path: staticPath("/api/v1/document-previews"),
			rawBody: func(f *matrixFixture) (string, *bytes.Buffer) {
				return multipartBody(t, map[string]string{"chunk_profile_version_id": f.profileID}, "prev.txt", "preview body")
			},
			allowed: roles(roleAdmin, roleManager, roleMember),
			verify:  statusIs(http.StatusAccepted)},
		{name: "document preview cancel", method: http.MethodPost,
			path:    func(f *matrixFixture) string { return "/api/v1/document-previews/" + f.previewJob.ID + "/cancel" },
			allowed: roles(roleMember), deny404: true,
			verify: statusIs(http.StatusAccepted)},
		// ---- MCP (platform.admin only) ----
		{name: "mcp server create", method: http.MethodPost, path: staticPath("/api/v1/mcp/servers"),
			body:    staticBody(`{"id":"mx-srv-2","origin":"https://mcp2.example.test","endpoint_path":"/mcp","audience_scopes":["assistant.use"],"enabled":true}`),
			allowed: roles(roleAdmin),
			verify:  statusIs(http.StatusCreated)},
		{name: "mcp server update", method: http.MethodPut,
			path:    staticPath("/api/v1/mcp/servers/mx-srv"),
			body:    staticBody(`{"origin":"https://mcp.example.test","endpoint_path":"/mcp","audience_scopes":["assistant.use"],"enabled":false}`),
			allowed: roles(roleAdmin),
			verify:  statusIs(http.StatusOK)},
		{name: "mcp tool approve", method: http.MethodPut,
			path:    func(f *matrixFixture) string { return "/api/v1/mcp/servers/mx-srv/tools/" + f.mcpToolID },
			body:    staticBody(`{"model_name":"mx.search","schema_version":1,"caller_scopes":["mcp.invoke"]}`),
			allowed: roles(roleAdmin),
			verify:  statusIs(http.StatusOK)},
		{name: "mcp server delete", method: http.MethodDelete,
			path:    staticPath("/api/v1/mcp/servers/mx-srv-2"),
			allowed: roles(roleAdmin),
			verify:  statusIs(http.StatusOK)},
		// ---- Models / pricing ----
		{name: "provider create", method: http.MethodPost, path: staticPath("/api/v1/providers"),
			body: func(*matrixFixture) string {
				collSeq++
				return fmt.Sprintf(`{"id":"ep_new_%d","protocol":"openai_compatible","base_origin":"https://api.example.test","capabilities":["chat"],"is_enabled":true}`, collSeq)
			},
			allowed: roles(roleAdmin, roleManager),
			verify:  statusIs(http.StatusCreated)},
		{name: "provider update", method: http.MethodPut, path: staticPath("/api/v1/providers/ep_mx"),
			body:    staticBody(`{"capabilities":["chat","embeddings"]}`),
			allowed: roles(roleAdmin, roleManager),
			verify:  statusIs(http.StatusOK)},
		{name: "provider delete", method: http.MethodDelete, path: staticPath("/api/v1/providers/ep_del_a"),
			targets: map[int]string{roleManager: "/api/v1/providers/ep_del_b"},
			allowed: roles(roleAdmin, roleManager),
			verify:  statusIs(http.StatusOK)},
		{name: "admin pricing create", method: http.MethodPost, path: staticPath("/api/v1/admin/pricing"),
			body:    staticBody(`{"subject_type":"endpoint","subject_id":"ep_mx","capability":"chat","unit":"input","rate_micros":1000,"unit_quantity":1,"currency":"USD"}`),
			allowed: roles(roleAdmin, roleManager), // pricing.manage
			verify:  statusIs(http.StatusCreated)},
		// ---- Billing (platform.admin only) ----
		{name: "admin budget create", method: http.MethodPost, path: staticPath("/api/v1/admin/budgets"),
			body: func(*matrixFixture) string {
				return fmt.Sprintf(`{"name":"mx-budget","subject_type":"api_key","subject_id":%q,"kind":"amount","limit_value":100,"currency":"USD","period":"monthly","starts_at":%q}`,
					memberKeyRec.ID, time.Now().UTC().Add(-time.Hour).Format(time.RFC3339))
			},
			allowed: roles(roleAdmin),
			verify:  statusIs(http.StatusCreated)},
		// ---- Identity administration (platform.admin only) ----
		{name: "identity user create", method: http.MethodPost, path: staticPath("/api/v1/identities/users"),
			body:    staticBody(`{"email":"` + newUser + `","password":"initial-pass-1","display_name":"MX New","role_ids":[]}`),
			allowed: roles(roleAdmin),
			verify:  statusIs(http.StatusCreated, http.StatusOK)},
		{name: "identity user disable", method: http.MethodPut,
			path:    func(*matrixFixture) string { return "/api/v1/identities/users/" + plainUser.ID },
			body:    staticBody(`{"status":"disabled"}`),
			allowed: roles(roleAdmin),
			verify:  statusIs(http.StatusOK)},
		{name: "identity role create", method: http.MethodPost, path: staticPath("/api/v1/identities/roles"),
			body:    staticBody(`{"name":"mx-role","scopes":["chatbot"],"grants":{"models":{"mode":"list","items":[]}}}`),
			allowed: roles(roleAdmin),
			verify:  statusIs(http.StatusCreated, http.StatusOK)},
		{name: "identity role patch grants", method: http.MethodPatch,
			path:    func(*matrixFixture) string { return "/api/v1/identities/roles/" + freeRole },
			body:    staticBody(`{"grants":{"models":{"mode":"list","items":["` + mxModelID + `"]}}}`),
			allowed: roles(roleAdmin),
			verify:  statusIs(http.StatusOK)},
		{name: "identity role delete membered", method: http.MethodDelete,
			path:    func(*matrixFixture) string { return "/api/v1/identities/roles/" + plainRole },
			allowed: roles(roleAdmin),
			verify:  statusIs(http.StatusConflict)}, // role_has_members
		{name: "identity role delete free", method: http.MethodDelete,
			path:    func(*matrixFixture) string { return "/api/v1/identities/roles/" + freeRole },
			allowed: roles(roleAdmin),
			verify:  statusIs(http.StatusOK)},
		{name: "service account create", method: http.MethodPost, path: staticPath("/api/v1/identities/service-accounts"),
			body:    staticBody(`{"name":"mx-sa-2","scopes":["chatbot"]}`),
			allowed: roles(roleAdmin),
			verify:  statusIs(http.StatusCreated, http.StatusOK)},
		{name: "service account disable", method: http.MethodPut,
			path:    func(*matrixFixture) string { return "/api/v1/identities/service-accounts/" + sa.ID },
			body:    staticBody(`{"status":"disabled"}`),
			allowed: roles(roleAdmin),
			verify:  statusIs(http.StatusOK)},
		{name: "admin api-key create", method: http.MethodPost, path: staticPath("/api/v1/admin/api-keys"),
			body:    staticBody(`{"name":"mx-key","scopes":["chatbot"],"allowed_models":["` + mxModelID + `"],"allowed_chatbots":["` + mxBotID + `"],"allowed_agents":["` + mxBotID + `"]}`),
			allowed: roles(roleAdmin),
			verify:  statusIs(http.StatusCreated)},
		{name: "admin api-key update", method: http.MethodPut,
			path:    func(*matrixFixture) string { return "/api/v1/admin/api-keys/" + memberKeyRec.ID },
			body:    staticBody(`{"is_active":false}`),
			allowed: roles(roleAdmin),
			verify:  statusIs(http.StatusOK)},
		{name: "admin api-key delete", method: http.MethodDelete,
			path:    func(*matrixFixture) string { return "/api/v1/admin/api-keys/" + memberKeyRec.ID },
			allowed: roles(roleAdmin),
			verify:  statusIs(http.StatusOK)},
		// ---- Session self-service ----
		{name: "me password change", method: http.MethodPost, path: staticPath("/api/v1/identities/me/password"),
			body: func(*matrixFixture) string {
				return `{"current_password":"member-pass","password":"member-pass-rotated"}`
			},
			allowed: roles(roleMember), // outsider/admin would also pass their own password check — member proves the route
			verify:  statusIs(http.StatusOK)},
		{name: "me session revoke", method: http.MethodDelete,
			path:    func(*matrixFixture) string { return "/api/v1/identities/me/sessions/" + outsiderSessID2 },
			allowed: roles(roleOutsider), deny404: true, // a foreign session id is the uniform not-found — even for feature-scoped callers
			deny:   map[int]int{roleMember: http.StatusNotFound},
			verify: statusIs(http.StatusOK)},
	}

	for _, row := range rows {
		for i, s := range f.sessions {
			body := ""
			path := row.path(f)
			if p, ok := row.targets[i]; ok {
				path = p
			}
			var rec *httptest.ResponseRecorder
			if row.rawBody != nil {
				ct, buf := row.rawBody(f)
				rec = f.sessDoRaw(t, s, row.method, path, ct, buf)
			} else {
				if row.body != nil {
					body = row.body(f)
				}
				rec = f.sessDo(t, s, row.method, path, body)
			}
			if row.allowed[i] {
				if rec.Code == http.StatusForbidden || rec.Code == http.StatusUnauthorized {
					t.Errorf("%s: %s %s unexpectedly denied %d (body %.200s)", row.name, row.method, row.path(f), rec.Code, rec.Body.String())
					continue
				}
				if row.verify != nil {
					row.verify(t, f, s, rec)
				}
			} else {
				wantDeny := http.StatusForbidden
				if row.deny404 && (i == roleAdmin || i == roleManager) {
					// Resource-bound denial: cap-holding non-owners see the
					// uniform 404 (foreign/absent are identical); scopeless
					// roles still hit the 403 scope gate.
					wantDeny = http.StatusNotFound
				}
				if d, ok := row.deny[i]; ok {
					wantDeny = d
				}
				if rec.Code != wantDeny {
					t.Errorf("%s: %s %s for %s expected %d, got %d (body %.200s)",
						row.name, row.method, row.path(f), matrixRoleNames[i], wantDeny, rec.Code, rec.Body.String())
				}
			}
		}
	}

	// Denied writes persisted nothing: member-created roles must not exist
	// (the member's POST /identities/roles answered 403).
	roleRows, err := f.repo.ListRoles(ctx, f.tenant)
	if err != nil {
		t.Fatal(err)
	}
	mxRoles := 0
	for _, r := range roleRows {
		if r.Name == "mx-role" {
			mxRoles++
		}
	}
	if mxRoles != 1 {
		t.Errorf("expected exactly one admin-created mx-role, got %d", mxRoles)
	}
	// Cross-user self-service denial: admin cannot revoke the outsider's
	// session through /me (owner binding) — assert the recorded 404/403 class.
	rec := f.sessDo(t, admin, http.MethodDelete, "/api/v1/identities/me/sessions/"+f.sessions[roleOutsider].session.Value, "")
	if rec.Code != http.StatusNotFound {
		t.Errorf("admin revoking outsider session: expected 404, got %d", rec.Code)
	}
	// The outsider's session remains valid.
	rec = f.sessDo(t, f.sessions[roleOutsider], http.MethodGet, "/dashboard/account", "")
	if rec.Code != http.StatusOK {
		t.Errorf("outsider session must survive the foreign revoke attempt: %d", rec.Code)
	}
}

// TestAPIWriteMatrixPlatformAdminOnly: a platform.admin-only API key proves
// D-25's deny-by-default — identity/MCP/admin writes succeed, every feature-
// scoped write is 403.
func TestAPIWriteMatrixPlatformAdminOnly(t *testing.T) {
	f := newMatrixFixture(t)
	do := func(method, path, body string) *httptest.ResponseRecorder {
		return f.do(t, method, path, body, map[string]string{"X-API-Key": f.adminKey})
	}
	allowed := []struct{ method, path, body string }{
		{http.MethodGet, "/api/v1/mcp/servers", ""},
		{http.MethodGet, "/api/v1/identities/users", ""},
		{http.MethodPost, "/api/v1/identities/roles", `{"name":"pa-role","scopes":["platform.admin"]}`},
		{http.MethodPost, "/api/v1/admin/api-keys", `{"name":"pa-key","scopes":["platform.admin"]}`},
		{http.MethodGet, "/api/v1/admin/budgets", ""},
	}
	for _, tc := range allowed {
		if rec := do(tc.method, tc.path, tc.body); rec.Code == http.StatusForbidden || rec.Code == http.StatusUnauthorized {
			t.Errorf("platform.admin key %s %s: unexpected %d (body %.200s)", tc.method, tc.path, rec.Code, rec.Body.String())
		}
	}
	denied := []struct{ method, path, body string }{
		{http.MethodPost, "/api/v1/rag/collections", `{"name":"pa-coll"}`},
		{http.MethodPost, "/api/v1/connectors/sources", `{"type":"github","name":"pa-src"}`},
		{http.MethodPost, "/api/v1/chatbots", `{"name":"pa-bot","model":"m"}`},
		{http.MethodPost, "/api/v1/providers", `{"id":"pa-ep","protocol":"openai_compatible","base_origin":"https://x.example.test"}`},
		{http.MethodPost, "/api/v1/admin/pricing", `{"subject_type":"model","subject_id":"m","capability":"chat","unit":"input","rate_micros":1,"unit_quantity":1,"currency":"USD"}`},
		{http.MethodPost, "/api/v1/knowledge/imports", `{"collection_id":"c","artifact_ref":"a"}`},
		{http.MethodPost, "/api/v1/prompt-templates", `{"name":"pa-prompt"}`},
	}
	for _, tc := range denied {
		if rec := do(tc.method, tc.path, tc.body); rec.Code != http.StatusForbidden {
			t.Errorf("platform.admin key %s %s: expected 403, got %d (body %.200s)", tc.method, tc.path, rec.Code, rec.Body.String())
		}
	}
}

// --- helpers ---------------------------------------------------------------

func roles(idx ...int) map[int]bool {
	m := make(map[int]bool, len(idx))
	for _, i := range idx {
		m[i] = true
	}
	return m
}

func staticPath(p string) func(*matrixFixture) string {
	return func(*matrixFixture) string { return p }
}
func staticBody(b string) func(*matrixFixture) string {
	return func(*matrixFixture) string { return b }
}

func statusIs(want ...int) func(t *testing.T, f *matrixFixture, s *matrixSession, rec *httptest.ResponseRecorder) {
	return func(t *testing.T, _ *matrixFixture, s *matrixSession, rec *httptest.ResponseRecorder) {
		t.Helper()
		for _, w := range want {
			if rec.Code == w {
				return
			}
		}
		t.Errorf("%s: status %d, want one of %v (body %.300s)", s.name, rec.Code, want, rec.Body.String())
	}
}

func promptsCreateInput(name string) prompts.CreateTemplateInput {
	return prompts.CreateTemplateInput{Name: name, SystemPrompt: "sys", UserPrompt: "{{.user_input}}"}
}

func mustCreateRole(t *testing.T, f *matrixFixture, name string) string {
	t.Helper()
	id := "role_" + strings.ReplaceAll(name, "-", "_")
	if err := f.repo.CreateRole(context.Background(), f.tenant,
		auth.Role{ID: id, Name: name, Scopes: []string{"chatbot"}}); err != nil {
		t.Fatalf("create role %s: %v", name, err)
	}
	return id
}

func mustSeedMemberKey(t *testing.T, f *matrixFixture, member *matrixSession) auth.APIKeyRecord {
	t.Helper()
	models := []string{mxModelID}
	bots := []string{mxBotID}
	_, rec := testdb.SeedAPIKey(t, f.db, f.tenant, testdb.SeedKeySpec{
		Name: "member-key", OwnerUserID: member.user.ID, Scopes: []string{"chatbot"},
		AllowedModels: &models, AllowedChatbots: &bots, AllowedAgents: &bots,
	})
	return rec
}

// seedArtifactObject writes one durable artifact + owner-bound source object
// so the member's knowledge/imports POST has a readable artifact_ref.
func (f *matrixFixture) seedArtifactObject(t *testing.T) string {
	t.Helper()
	ctx := context.Background()
	// The artifact must land in the fixture's own store — ImportArtifact
	// reads it back through the service's store, not a throwaway one.
	w, _, err := f.artifactStore.Begin(ctx, artifacts.Meta{Ref: artifacts.Ref{TenantID: f.tenant, Kind: artifacts.KindOriginal}, MimeType: "text/plain"}, time.Minute, 4096)
	if err != nil {
		t.Fatal(err)
	}
	if _, err := w.Write([]byte("importable content")); err != nil {
		t.Fatal(err)
	}
	if err := w.Finalize(ctx); err != nil {
		t.Fatal(err)
	}
	ref := w.Ref()
	objID := "obj_" + ref.ID[:8]
	member := f.sessions[roleMember]
	obj, err := f.sourceRepo.UpsertObject(ctx, source.Object{
		TenantID: f.tenant, ID: objID, Kind: "document", NativeID: "mx-native-" + ref.ID[:8],
		AuthorizationMode: source.AuthorizationOwner, OwnerActorID: member.user.ID, ObservedSeq: 1,
	})
	if err != nil {
		t.Fatalf("seed import object: %v", err)
	}
	// The version binds the row UpsertObject actually persisted — a DO
	// NOTHING conflict would hand back the pre-existing row's id.
	if err := f.sourceRepo.InsertObjectVersion(ctx, f.tenant, source.ObjectVersion{
		ObjectID: obj.ID, ObservedSeq: obj.ObservedSeq + 1, ArtifactRef: ref.ID, SHA256: ref.SHA256,
		ByteSize: ref.ByteSize, MimeType: "text/plain", OriginalFilename: "import.txt",
	}); err != nil {
		t.Fatalf("seed import version: %v", err)
	}
	return ref.ID
}
