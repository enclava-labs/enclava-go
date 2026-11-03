// dashboard_matrix_test.go — 12-02 verification: one exhaustive route×role
// matrix over EVERY new dashboard URL (pages + partial fragments), the old-URL
// designed-404 table and the unauthenticated page→return_to / fragment→401
// contracts (D-17/D-19/OA-39). One shared fixture (newMatrixFixture) feeds
// this file, api_write_matrix_test.go and dashboard_render_test.go: the real
// configured stack — identity, sessions/CSRF, rag/connectors/chatbot/prompt/
// extract/preview/mcp services over an isolated schema — with the seeded
// Admin, Knowledge-manager and Member roles carrying D-25 stored grants.
package httpx

import (
	"bytes"
	"context"
	"encoding/json"
	"mime/multipart"
	"net/http"
	"net/http/httptest"
	"net/url"
	"strings"
	"testing"
	"time"

	"enclava-go/internal/artifacts"
	"enclava-go/internal/auth"
	"enclava-go/internal/billing"
	"enclava-go/internal/chatbot"
	"enclava-go/internal/connectors"
	"enclava-go/internal/extract"
	"enclava-go/internal/inference"
	"enclava-go/internal/jobs"
	"enclava-go/internal/mcp"
	"enclava-go/internal/prompts"
	"enclava-go/internal/rag"
	"enclava-go/internal/source"
	"enclava-go/internal/store/postgres"
	"enclava-go/internal/testdb"
)

// Matrix role order — every table row's status list follows it.
const (
	roleAdmin = iota
	roleManager
	roleMember
	roleOutsider
)

var matrixRoleNames = []string{"admin", "manager", "member", "outsider"}

// Seeded-resource ids shared by every matrix file (template ids are
// package-fixed so role grants can name them).
const (
	mxBotID      = "cb_matrix_bot"
	mxModelID    = "m-mx-chat"
	mxTemplateID = "mx_invoice"
)

var (
	// managerScopes mirrors the seeded Knowledge-manager role (CONTEXT §3.1).
	managerScopes = []string{
		"chatbot", "chatbot.manage", "rag", "rag.manage",
		"extract", "extract.manage", "connectors.manage", "pricing.manage",
	}
	memberScopes = []string{"chatbot", "rag", "extract"}
	adminScopes  = append(append([]string{}, managerScopes...),
		"platform.admin", "models.list", "chat.completions", "responses", "embeddings.create", "openai.proxy")

	managerGrants = auth.ResourceGrants{
		Models:           auth.Allowlist{Items: []string{mxModelID}},
		Endpoints:        auth.Allowlist{Items: []string{"ep_mx"}},
		Assistants:       auth.Allowlist{Items: []string{mxBotID}},
		ExtractTemplates: auth.Allowlist{Items: []string{mxTemplateID}},
	}
	memberGrants = managerGrants
)

// matrixFixture wires every service the matrices exercise; the
// identityFixture inside supplies app/repo/db/tenant plus the
// loginUserWithGrants/sessionDo helpers.
type matrixFixture struct {
	identityFixture
	ctx           context.Context
	jobsSvc       *jobs.Service
	jobsRepo      *postgres.JobsRepo
	ragSvc        *rag.Service
	extractSvc    *extract.Service
	previewSvc    *extract.PreviewService
	chatRepo      *postgres.ChatbotRepo
	connSvc       *connectors.Service
	chatSvc       *chatbot.Service
	mcpRepo       *postgres.MCPToolsRepo
	billingRepo   *postgres.BillingRepo
	endpoints     *postgres.EndpointsRepo
	sourceRepo    *postgres.SourceRepo
	artifactRepo  *postgres.ArtifactRepo
	artifactStore *artifacts.Store
	profilesRepo  *postgres.ChunkProfilesRepo
	sessions      []*matrixSession
	// Seeded write targets.
	memberColl   rag.Collection
	memberDoc    rag.Document
	memberConv   chatbot.Conversation
	memberJob    jobs.Job // extract.process job owned by the member
	previewJob   jobs.Job // document.preview job owned by the member
	connectorSrc connectors.Source
	connSyncJob  connectors.SyncJob
	mcpToolID    string
	profileID    string
	// Second tenant + its session for cross-tenant denial rows.
	otherTenant   string
	otherUser     auth.User
	otherSess     *matrixSession
	otherUserColl rag.Collection
	otherUserJob  jobs.Job
}

// matrixSession is a logged-in principal: cookies for the wire plus the
// resolved principal for direct service seeding (job enqueue, uploads).
type matrixSession struct {
	apiKey    string
	name      string
	user      auth.User
	principal auth.Principal
	session   *http.Cookie
	csrf      *http.Cookie
}

// matrixStubAdapter satisfies connectors.Adapter with one enabled content
// stream so SyncSource enqueues a durable job; no worker runs it.
type matrixStubAdapter struct{}

func (matrixStubAdapter) Capabilities() connectors.AdapterCapabilities {
	return connectors.AdapterCapabilities{Content: true}
}
func (matrixStubAdapter) Content(context.Context, source.Connection, connectors.Cursor, int) (connectors.ContentPage, error) {
	return connectors.ContentPage{Completeness: connectors.CompletenessComplete, Snapshot: true}, nil
}
func (matrixStubAdapter) Access(context.Context, source.Connection, connectors.Cursor, int) (connectors.AccessPage, error) {
	return connectors.AccessPage{Completeness: connectors.CompletenessComplete, Snapshot: true}, nil
}
func (matrixStubAdapter) Membership(context.Context, source.Connection, connectors.Cursor, int) (connectors.MembershipPage, error) {
	return connectors.MembershipPage{Completeness: connectors.CompletenessComplete, Snapshot: true}, nil
}

func newMatrixFixture(t *testing.T) *matrixFixture {
	t.Helper()
	f := &matrixFixture{identityFixture: *newIdentityFixture(t)}
	f.ctx = context.Background()
	f.jobsRepo = postgres.NewJobsRepo(f.db)
	f.jobsSvc = jobs.NewService(f.jobsRepo, 0)
	f.sourceRepo = postgres.NewSourceRepo(f.db)
	f.artifactRepo = postgres.NewArtifactRepo(f.db)
	store, err := artifacts.NewStore(t.TempDir(), t.TempDir(), f.artifactRepo)
	if err != nil {
		t.Fatalf("artifact store: %v", err)
	}
	f.artifactStore = store
	f.ragSvc = rag.NewService(postgres.NewRAGRepo(f.db), nil, nil, "", store, f.jobsSvc)
	f.connSvc = connectors.NewService(f.sourceRepo, f.jobsSvc, f.jobsRepo)
	factories := connectors.AdapterFactories{}
	for _, kind := range []connectors.SourceKind{
		connectors.SourceTypeGitHub, connectors.SourceTypeSlack, connectors.SourceTypeTeams,
		connectors.SourceTypeNotion, connectors.SourceTypeGoogleDrive,
	} {
		factories[kind] = func(source.Connection) (connectors.Adapter, error) { return matrixStubAdapter{}, nil }
	}
	f.connSvc.SetSyncDependencies(factories, postgres.NewConnectorObservationsRepo(f.db), store, f.sourceRepo)
	chatRepo := postgres.NewChatbotRepo(f.db)
	f.chatSvc = chatbot.NewService(chatRepo, postgres.NewPromptsRepo(f.db), f.ragSvc, nil)
	f.chatSvc.ConfigureRuns(postgres.NewChatbotRunsRepo(f.db), f.jobsSvc, nil, nil)
	promptSvc := prompts.NewService(postgres.NewPromptsRepo(f.db), f.chatSvc)
	f.billingRepo = postgres.NewBillingRepo(f.db)
	f.endpoints = postgres.NewEndpointsRepo(f.db)
	extractRepo := postgres.NewExtractRepo(f.db)
	f.profilesRepo = postgres.NewChunkProfilesRepo(f.db)
	f.extractSvc = extract.NewService(extractRepo, extractRepo, f.jobsRepo, f.jobsSvc, store, nil, f.endpoints, 8)
	f.previewSvc = extract.NewPreviewService(f.profilesRepo, f.jobsRepo, f.jobsSvc, store, f.sourceRepo, 8)
	f.mcpRepo = postgres.NewMCPToolsRepo(f.db)
	executor, err := inference.NewService(inference.Deps{
		Endpoints:  f.endpoints,
		Sources:    f.sourceRepo,
		Authorizer: source.NewAuthorizer(f.sourceRepo, time.Hour),
		Billing:    billing.NewService(f.billingRepo),
		Artifacts:  f.artifactRepo,
	})
	if err != nil {
		t.Fatalf("inference service: %v", err)
	}
	// Re-wire the identityFixture app with the richer stack (the fixture's
	// own wiring keeps nils where the matrices need services).
	app := NewApp(f.app.authSvc, nil, "", nil, f.repo, f.extractSvc, f.ragSvc, promptSvc,
		f.chatSvc, f.connSvc, billing.NewService(f.billingRepo), f.billingRepo, f.endpoints,
		"sha256", "", "enc_", nil, f.app.sessionAuth, 1, false, executor, nil, GateConfig{}, f.sourceRepo, f.artifactRepo)
	app.ConfigureActivity(f.jobsSvc)
	app.ConfigureMCP(f.mcpRepo, nil)
	RegisterDocumentPreviewRoutes(app.Mux(), app, f.previewSvc, 8)
	app.rl = nil // the matrices issue hundreds of authenticated calls; the limiter is not under test
	f.app = app
	// Sessions: seeded roles with the D-25 stored grants the exercised
	// surfaces need — never mode:all, never platform.admin beyond its own
	// row.
	mk := func(name, email string, scopes []string, grants auth.ResourceGrants) *matrixSession {
		user := testdb.SeedUserWithGrants(t, f.db, f.tenant, email, scopes, grants)
		setPassword(t, f.repo, f.tenant, user.ID, name+"-pass")
		sess, csrf := login(t, f.app, email, name+"-pass")
		// The wire principal — AuthenticateSession binds the session as the
		// credential, so owner-bound job rows seeded through this principal
		// carry the same submitter key the wire requests recompute.
		p, err := f.app.authSvc.AuthenticateSession(f.ctx, sess.Value)
		if err != nil {
			t.Fatalf("session principal for %s: %v", name, err)
		}
		ms := &matrixSession{name: name, user: user, principal: p, session: sess, csrf: csrf}
		f.sessions = append(f.sessions, ms)
		return ms
	}
	admin := mk("admin", "mx-admin@example.test", adminScopes, auth.ResourceGrants{
		Models:           auth.Allowlist{All: true},
		Endpoints:        auth.Allowlist{All: true},
		Assistants:       auth.Allowlist{All: true},
		ExtractTemplates: auth.Allowlist{All: true},
	})
	manager := mk("manager", "mx-manager@example.test", managerScopes, managerGrants)
	member := mk("member", "mx-member@example.test", memberScopes, memberGrants)
	mk("outsider", "mx-outsider@example.test", nil, auth.ResourceGrants{})
	_ = admin

	// --- seeded domain rows ------------------------------------------------
	f.chatRepo = chatRepo
	if err := f.chatRepo.Create(f.ctx, chatbot.Instance{
		TenantID: f.tenant, ID: mxBotID, Name: "Matrix assistant", Model: mxModelID,
		Audience: []string{"*"}, ExternalID: "matrix-bot", IsEnabled: true,
	}); err != nil {
		t.Fatalf("seed chatbot: %v", err)
	}
	f.memberColl, err = f.ragSvc.CreateCollection(f.ctx, member.principal,
		rag.CreateCollectionInput{Name: "Member collection", ACL: map[string]any{"public": true},
			ChunkSize: rag.DefaultChunkSize, ChunkOverlap: rag.DefaultChunkOverlap})
	if err != nil {
		t.Fatalf("seed member collection: %v", err)
	}
	up, err := f.ragSvc.UploadDocument(f.ctx, member.principal, f.memberColl.ID, rag.UploadDocumentInput{
		FileName: "seeded.txt", MimeType: "text/plain", Reader: strings.NewReader("seeded matrix document body"),
	})
	if err != nil {
		t.Fatalf("seed member document: %v", err)
	}
	f.memberDoc = up.Document
	f.memberConv = chatbot.Conversation{TenantID: f.tenant, ID: "chatconv_matrix",
		ChatbotID: mxBotID, OwnerActorID: member.user.ID}
	if err := f.chatRepo.CreateConversation(f.ctx, f.memberConv); err != nil {
		t.Fatalf("seed conversation: %v", err)
	}
	f.memberJob = f.enqueue(t, member.principal, extract.OperationProcess, "mx-member-job")
	// A queued preview job: Submit needs a real immutable profile version.
	profile, err := f.profilesRepo.Create(f.ctx, f.tenant, source.ChunkProfile{
		ProfileID: "mx-profile", SizeCodePoints: 500, OverlapCodePoints: 50,
		BoundaryStrategy: "heading", TableHandling: "atomic",
	}, member.user.ID)
	if err != nil {
		t.Fatalf("seed chunk profile: %v", err)
	}
	f.profileID = profile.ID
	if err := f.endpoints.Create(f.ctx, inference.Endpoint{
		TenantID: f.tenant, ID: "ep_mx", Protocol: "openai_compatible",
		BaseOrigin: "https://models.example.test", Enabled: true,
		Capabilities: []string{inference.CapabilityChat},
	}); err != nil {
		t.Fatalf("seed endpoint: %v", err)
	}
	f.previewJob, err = f.previewSvc.Submit(f.ctx, member.principal, "preview.txt", "text/plain", f.profileID, strings.NewReader("preview body"))
	if err != nil {
		t.Fatalf("seed preview job: %v", err)
	}
	f.connectorSrc, err = f.connSvc.CreateSource(f.ctx, manager.principal, connectors.CreateSourceInput{
		Type: "github", Name: "Matrix repo", Config: map[string]any{"owner": "acme", "repo": "docs"},
	})
	if err != nil {
		t.Fatalf("seed connector source: %v", err)
	}
	f.connSyncJob, err = f.connSvc.SyncSource(f.ctx, manager.principal, f.connectorSrc.ID)
	if err != nil {
		t.Fatalf("seed sync job: %v", err)
	}
	if err := f.mcpRepo.PutServer(f.ctx, sourceTenantServer(f.tenant)); err != nil {
		t.Fatalf("seed mcp server: %v", err)
	}
	rawSchema := json.RawMessage(`{"type":"object","properties":{"q":{"type":"string"}}}`)
	if err := f.mcpRepo.RecordDiscovery(f.ctx, f.tenant, "mx-srv", []postgres.DiscoveredToolSnapshot{
		{Name: "search", InputSchema: rawSchema, OutputSchema: json.RawMessage(`{"type":"object"}`)},
	}); err != nil {
		t.Fatalf("record discovery: %v", err)
	}
	f.mcpToolID = "search"
	if err := f.extractSvc.UpsertTemplate(f.ctx, admin.principal, extract.Template{
		ID: mxTemplateID, Name: "Invoice", SystemPrompt: "sys", UserPrompt: "user",
		Enabled: true,
	}); err != nil {
		t.Fatalf("seed extract template: %v", err)
	}

	// Second tenant: user+source object/job for cross-tenant denial rows.
	f.otherTenant = testdb.SeedTenant(t, f.db, "mx-other")
	f.otherUser = testdb.SeedUser(t, f.db, f.otherTenant, "other-owner@example.test", nil)
	otherPrincipal := auth.Principal{TenantID: f.otherTenant, ActorID: f.otherUser.ID, UserID: f.otherUser.ID,
		ActorKind: auth.ActorKindUser, Scopes: map[string]struct{}{"rag": {}, "rag.manage": {}, "extract": {}}}
	f.otherUserColl, err = f.ragSvc.CreateCollection(f.ctx, otherPrincipal,
		rag.CreateCollectionInput{Name: "Other tenant collection", ChunkSize: rag.DefaultChunkSize, ChunkOverlap: rag.DefaultChunkOverlap})
	if err != nil {
		t.Fatalf("seed other collection: %v", err)
	}
	f.otherUserJob = f.enqueueIn(t, otherPrincipal, f.otherTenant, extract.OperationProcess, "mx-other-job")
	f.otherSess = &matrixSession{name: "otherTenant", user: f.otherUser, principal: otherPrincipal}
	otherKey, _ := testdb.SeedAPIKey(t, f.db, f.otherTenant, testdb.SeedKeySpec{
		OwnerUserID: f.otherUser.ID, Scopes: []string{"rag", "rag.manage", "extract"},
	})
	f.otherSess.apiKey = otherKey
	return f
}

// sourceTenantServer is the durable MCP server row the fixture's discovery
// snapshot hangs on.
func sourceTenantServer(tenantID string) mcp.Server {
	return mcp.Server{TenantID: tenantID, ID: "mx-srv", Origin: "https://mcp.example.test",
		EndpointPath: "/mcp", AudienceScopes: []string{"assistant.use"}, Enabled: true}
}

func (f *matrixFixture) enqueue(t *testing.T, p auth.Principal, op, key string) jobs.Job {
	t.Helper()
	return f.enqueueIn(t, p, p.TenantID, op, key)
}

func (f *matrixFixture) enqueueIn(t *testing.T, p auth.Principal, tenantID, op, key string) jobs.Job {
	t.Helper()
	job, _, err := f.jobsSvc.Enqueue(f.ctx, p, jobs.EnqueueRequest{
		TenantID: tenantID, Operation: op, IdempotencyKey: key,
		InputRef: "mx-input", InputHash: "mx-hash", Policy: map[string]any{},
	})
	if err != nil {
		t.Fatalf("enqueue %s: %v", key, err)
	}
	return job
}

// sessDo issues a session request for one matrix role; CSRF rides on every
// mutation just like the dashboard fetch client.
func (f *matrixFixture) sessDo(t *testing.T, s *matrixSession, method, path, body string) *httptest.ResponseRecorder {
	t.Helper()
	if s.apiKey != "" {
		// Second-tenant principals cannot hold a session on this app (its
		// session auth binds one tenant), so their rows authenticate with a
		// real stored API key instead.
		var r *http.Request
		if body != "" {
			r = httptest.NewRequest(method, path, strings.NewReader(body))
			r.Header.Set("Content-Type", "application/json")
		} else {
			r = httptest.NewRequest(method, path, nil)
		}
		r.Header.Set("Authorization", "Bearer "+s.apiKey)
		rec := httptest.NewRecorder()
		f.app.Handler().ServeHTTP(rec, r)
		return rec
	}
	return sessionDo(t, f.app, method, path, body, s.session, s.csrf)
}

// anonDo issues an unauthenticated request; hx marks the htmx partial
// contract (HX-Request header).
func (f *matrixFixture) anonDo(t *testing.T, method, path string, hx bool) *httptest.ResponseRecorder {
	t.Helper()
	r := httptest.NewRequest(method, path, nil)
	if hx {
		r.Header.Set("HX-Request", "true")
	}
	rec := httptest.NewRecorder()
	f.app.Handler().ServeHTTP(rec, r)
	return rec
}

// multipartBody builds one file upload body (fields first, file last — the
// bounded upload parsers require the trailing file part).
func multipartBody(t *testing.T, fields map[string]string, filename, content string) (string, *bytes.Buffer) {
	t.Helper()
	var buf bytes.Buffer
	w := multipart.NewWriter(&buf)
	for k, v := range fields {
		if err := w.WriteField(k, v); err != nil {
			t.Fatal(err)
		}
	}
	part, err := w.CreateFormFile("file", filename)
	if err != nil {
		t.Fatal(err)
	}
	if _, err := part.Write([]byte(content)); err != nil {
		t.Fatal(err)
	}
	if err := w.Close(); err != nil {
		t.Fatal(err)
	}
	return w.FormDataContentType(), &buf
}

// sessDoRaw issues a session request with an explicit content type (multipart
// uploads); body==nil means no body.
func (f *matrixFixture) sessDoRaw(t *testing.T, s *matrixSession, method, path, contentType string, body *bytes.Buffer) *httptest.ResponseRecorder {
	t.Helper()
	var r *http.Request
	if body != nil {
		r = httptest.NewRequest(method, path, body)
	} else {
		r = httptest.NewRequest(method, path, nil)
	}
	if contentType != "" {
		r.Header.Set("Content-Type", contentType)
	}
	r.AddCookie(s.session)
	if s.csrf != nil && method != http.MethodGet && method != http.MethodHead {
		r.AddCookie(s.csrf)
		r.Header.Set("X-CSRF-Token", s.csrf.Value)
	}
	rec := httptest.NewRecorder()
	f.app.Handler().ServeHTTP(rec, r)
	return rec
}

// --- route×role matrix ------------------------------------------------------

// matrixRow is one URL × the four role sessions; want lists statuses in
// roleAdmin/roleManager/roleMember/roleOutsider order.
type matrixRow struct {
	path string
	want [4]int
}

func TestDashboardRouteRoleMatrix(t *testing.T) {
	f := newMatrixFixture(t)
	collID := f.memberColl.ID
	convID := f.memberConv.ID

	ok, deny := http.StatusOK, http.StatusForbidden
	nf := http.StatusNotFound

	rows := []matrixRow{
		// Session-only pages and their refresh fragments.
		{"/dashboard", [4]int{ok, ok, ok, ok}},
		{"/dashboard/", [4]int{ok, ok, ok, ok}},
		{"/dashboard/account", [4]int{ok, ok, ok, ok}},
		{"/dashboard/partials/home/running", [4]int{ok, ok, ok, ok}},
		{"/dashboard/partials/home/attention", [4]int{ok, ok, ok, ok}},
		{"/dashboard/partials/account/sessions", [4]int{ok, ok, ok, ok}},
		// API reference (cap api — any API-calling scope; the outsider holds none).
		{"/dashboard/api", [4]int{ok, ok, ok, deny}},
		// Chat (cap chat): owner-bound thread — only the member owns the
		// seeded conversation (platform.admin bypasses ownership), so the
		// manager sees the designed 404.
		{"/dashboard/chat", [4]int{ok, ok, ok, deny}},
		{"/dashboard/chat/" + convID, [4]int{ok, nf, ok, deny}},
		{"/dashboard/chat/conv_bogus", [4]int{nf, nf, nf, deny}},
		{"/dashboard/partials/chat/history", [4]int{ok, ok, ok, deny}},
		{"/dashboard/partials/chat/thread?c=" + convID, [4]int{ok, nf, ok, deny}},
		{"/dashboard/partials/chat/thread", [4]int{ok, ok, ok, deny}},
		{"/dashboard/partials/evidence/ev_bogus", [4]int{ok, ok, ok, deny}},
		// Search (cap rag).
		{"/dashboard/search", [4]int{ok, ok, ok, deny}},
		{"/dashboard/partials/search/results?q=seeded", [4]int{ok, ok, ok, deny}},
		// Extract (cap extract / extract.manage).
		{"/dashboard/extract", [4]int{ok, ok, ok, deny}},
		{"/dashboard/extract?tab=jobs", [4]int{ok, ok, ok, deny}},
		{"/dashboard/extract?tab=templates", [4]int{ok, ok, deny, deny}},
		{"/dashboard/partials/extract/jobs", [4]int{ok, ok, ok, deny}},
		{"/dashboard/partials/extract/result?job=" + f.memberJob.ID, [4]int{nf, nf, ok, deny}},
		{"/dashboard/partials/extract/result?job=job_bogus", [4]int{nf, nf, nf, deny}},
		{"/dashboard/partials/extract/templates", [4]int{ok, ok, deny, deny}},
		// Knowledge (cap rag for read; rag.manage/sources.manage inner gates).
		{"/dashboard/knowledge", [4]int{ok, ok, ok, deny}},
		{"/dashboard/knowledge/profiles", [4]int{ok, ok, deny, deny}},
		{"/dashboard/knowledge/" + collID, [4]int{ok, ok, ok, deny}},
		{"/dashboard/knowledge/" + collID + "/documents", [4]int{ok, ok, ok, deny}},
		{"/dashboard/knowledge/" + collID + "/search", [4]int{ok, ok, ok, deny}},
		{"/dashboard/knowledge/" + collID + "/sources", [4]int{ok, ok, deny, deny}},
		{"/dashboard/knowledge/" + collID + "/settings", [4]int{ok, ok, deny, deny}},
		{"/dashboard/knowledge/" + collID + "/bogus", [4]int{nf, nf, nf, deny}},
		{"/dashboard/knowledge/col_bogus", [4]int{nf, nf, nf, deny}},
		{"/dashboard/partials/knowledge/collections", [4]int{ok, ok, ok, deny}},
		{"/dashboard/partials/knowledge/documents?collection=" + collID, [4]int{ok, ok, ok, deny}},
		{"/dashboard/partials/knowledge/documents", [4]int{nf, nf, nf, deny}}, // missing/unknown collection id answers the uniform 404, not a service error
		{"/dashboard/partials/knowledge/sources?collection=" + collID, [4]int{ok, ok, deny, deny}},
		{"/dashboard/partials/knowledge/profiles", [4]int{ok, ok, deny, deny}},
		// Sources (cap connectors.manage; bindings is access.manage).
		{"/dashboard/sources", [4]int{ok, ok, deny, deny}},
		{"/dashboard/sources?new=1", [4]int{ok, ok, deny, deny}},
		{"/dashboard/partials/sources/table", [4]int{ok, ok, deny, deny}},
		{"/dashboard/partials/sources/drawer", [4]int{ok, ok, deny, deny}},
		{"/dashboard/partials/sources/bindings?source=" + f.connectorSrc.ID, [4]int{ok, deny, deny, deny}},
		{"/dashboard/partials/sources/bindings?source=connsrc_bogus", [4]int{nf, deny, deny, deny}},
		// Assistants (cap chatbot.manage).
		{"/dashboard/assistants", [4]int{ok, ok, deny, deny}},
		{"/dashboard/assistants/instructions", [4]int{ok, ok, deny, deny}},
		{"/dashboard/partials/assistants/table", [4]int{ok, ok, deny, deny}},
		{"/dashboard/partials/assistants/drawer", [4]int{ok, ok, deny, deny}},
		{"/dashboard/partials/assistants/instructions", [4]int{ok, ok, deny, deny}},
		{"/dashboard/partials/assistants/instructions/editor", [4]int{ok, ok, deny, deny}},
		// Tools (platform.admin only).
		{"/dashboard/tools", [4]int{ok, deny, deny, deny}},
		{"/dashboard/partials/tools/servers", [4]int{ok, deny, deny, deny}},
		{"/dashboard/partials/tools/queue", [4]int{ok, deny, deny, deny}},
		{"/dashboard/partials/tools/list", [4]int{ok, deny, deny, deny}},
		{"/dashboard/partials/tools/register", [4]int{ok, deny, deny, deny}},
		// Models (cap pricing.manage).
		{"/dashboard/models", [4]int{ok, ok, deny, deny}},
		{"/dashboard/models/pricing", [4]int{ok, ok, deny, deny}},
		{"/dashboard/partials/models/endpoints", [4]int{ok, ok, deny, deny}},
		{"/dashboard/partials/models/pricing", [4]int{ok, ok, deny, deny}},
		{"/dashboard/partials/models/endpoint-drawer", [4]int{ok, ok, deny, deny}},
		{"/dashboard/partials/models/price-history", [4]int{http.StatusBadRequest, http.StatusBadRequest, deny, deny}},
		// Access (platform.admin only) — five tab pages + refresh fragments
		// + the three drawers.
		{"/dashboard/access", [4]int{ok, deny, deny, deny}},
		{"/dashboard/access/people", [4]int{ok, deny, deny, deny}},
		{"/dashboard/access/roles", [4]int{ok, deny, deny, deny}},
		{"/dashboard/access/service-accounts", [4]int{ok, deny, deny, deny}},
		{"/dashboard/access/api-keys", [4]int{ok, deny, deny, deny}},
		{"/dashboard/access/sign-in", [4]int{ok, deny, deny, deny}},
		{"/dashboard/partials/access/people", [4]int{ok, deny, deny, deny}},
		{"/dashboard/partials/access/roles", [4]int{ok, deny, deny, deny}},
		{"/dashboard/partials/access/service-accounts", [4]int{ok, deny, deny, deny}},
		{"/dashboard/partials/access/api-keys", [4]int{ok, deny, deny, deny}},
		{"/dashboard/partials/access/sign-in", [4]int{ok, deny, deny, deny}},
		{"/dashboard/partials/access/user-drawer", [4]int{ok, deny, deny, deny}},
		{"/dashboard/partials/access/role-editor", [4]int{ok, deny, deny, deny}},
		{"/dashboard/partials/access/key-drawer", [4]int{ok, deny, deny, deny}},
		// Usage & billing (platform.admin only).
		{"/dashboard/usage", [4]int{ok, deny, deny, deny}},
		{"/dashboard/usage/budgets", [4]int{ok, deny, deny, deny}},
		{"/dashboard/usage/audit", [4]int{ok, deny, deny, deny}},
		{"/dashboard/partials/usage/overview", [4]int{ok, deny, deny, deny}},
		{"/dashboard/partials/usage/budgets", [4]int{ok, deny, deny, deny}},
		{"/dashboard/partials/usage/audit", [4]int{ok, deny, deny, deny}},
		{"/dashboard/partials/usage/budget-drawer", [4]int{ok, deny, deny, deny}},
		// Unknown / unregistered dashboard paths: designed 404 for any
		// session (the /dashboard/ subtree catch-all), including the
		// designed 404 row the deleted gallery leaves behind (12-01).
		{"/dashboard/not-a-real-page", [4]int{nf, nf, nf, nf}},
		{"/dashboard/dev/gallery", [4]int{nf, nf, nf, nf}},
		{"/dashboard/dev/gallery/components", [4]int{nf, nf, nf, nf}},
		{"/dashboard/partials/access/bogus", [4]int{nf, nf, nf, nf}},
	}
	for _, row := range rows {
		for i, s := range f.sessions {
			rec := f.sessDo(t, s, http.MethodGet, row.path, "")
			if rec.Code != row.want[i] {
				t.Errorf("%s %s: got %d, want %d (body %.200s)", matrixRoleNames[i], row.path, rec.Code, row.want[i], rec.Body.String())
			}
		}
	}
}

// TestDashboardOldURLsReturn404 is the §5.1 old-URL table verbatim (D-17/D-19):
// every deleted page and partial answers 404 for every session — never a
// redirect, never an alias. Retained redesigned extract fragments are the
// live rows in TestDashboardRouteRoleMatrix above, not here.
func TestDashboardOldURLsReturn404(t *testing.T) {
	f := newMatrixFixture(t)
	old := []string{
		// Pages.
		"/dashboard/keys", "/dashboard/budgets", "/dashboard/analytics", "/dashboard/providers",
		"/dashboard/pricing", "/dashboard/billing-audit", "/dashboard/rag", "/dashboard/chatbots",
		"/dashboard/prompt-templates", "/dashboard/connectors", "/dashboard/mcp",
		"/dashboard/identity", "/dashboard/documents", "/dashboard/rag/documents/doc_1",
		// Partials (old families).
		"/dashboard/partials/stats", "/dashboard/partials/keys", "/dashboard/partials/usage",
		"/dashboard/partials/budgets", "/dashboard/partials/billing-audit",
		"/dashboard/partials/providers", "/dashboard/partials/pricing", "/dashboard/partials/rag",
		"/dashboard/partials/chatbots", "/dashboard/partials/prompt-templates",
		"/dashboard/partials/mcp", "/dashboard/partials/connectors",
		// Old extract endpoints — exact paths only; the retained redesigned
		// fragments live in the route matrix.
		"/dashboard/partials/extract", "/dashboard/partials/extract/templates/tpl_1",
		"/dashboard/partials/extract/reset-defaults", "/dashboard/partials/extract/settings",
		"/dashboard/partials/extract/process", "/dashboard/partials/extract/models",
		// Deleted dev gallery (12-01).
		"/dashboard/dev/gallery", "/dashboard/dev/gallery/buttons",
	}
	for _, path := range old {
		for i, s := range f.sessions {
			rec := f.sessDo(t, s, http.MethodGet, path, "")
			if rec.Code != http.StatusNotFound {
				t.Errorf("%s %s: expected the designed 404, got %d", matrixRoleNames[i], path, rec.Code)
			}
		}
		// A mutation on a dead URL with a valid CSRF pair still 404s —
		// never a masked 403.
		rec := f.sessDo(t, f.sessions[roleMember], http.MethodPost, path, "")
		if rec.Code != http.StatusNotFound {
			t.Errorf("POST %s (member): expected 404, got %d", path, rec.Code)
		}
	}
}

// TestDashboardUnauthenticatedContracts pins the signed-out paths: pages
// redirect to login preserving return_to; HTMX fetches (pages and partials)
// answer 401 with HX-Redirect; JSON API calls answer 401 with no session
// cookie minted.
func TestDashboardUnauthenticatedContracts(t *testing.T) {
	f := newMatrixFixture(t)

	for _, path := range []string{"/dashboard", "/dashboard/knowledge", "/dashboard/access/api-keys"} {
		rec := f.anonDo(t, http.MethodGet, path, false)
		want := "/dashboard/login?return_to=" + url.QueryEscape(path)
		if rec.Code != http.StatusSeeOther || rec.Header().Get("Location") != want {
			t.Errorf("GET %s unauth: expected 303 %q, got %d %q", path, want, rec.Code, rec.Header().Get("Location"))
		}
	}
	for _, path := range []string{"/dashboard/partials/knowledge/collections", "/dashboard/chat", "/dashboard/partials/access/people"} {
		rec := f.anonDo(t, http.MethodGet, path, true)
		if rec.Code != http.StatusUnauthorized {
			t.Errorf("HX GET %s unauth: expected 401, got %d", path, rec.Code)
		}
		if loc := rec.Header().Get("HX-Redirect"); loc != "/dashboard/login?return_to=%2Fdashboard" {
			t.Errorf("HX GET %s unauth: HX-Redirect=%q, want /dashboard/login?return_to=%%2Fdashboard", path, loc)
		}
	}
	// Mutations without a session are 401 (session/CSRF fail closed), never 403.
	for _, method := range []string{http.MethodPost, http.MethodPut, http.MethodDelete} {
		rec := f.anonDo(t, method, "/api/v1/admin/api-keys", false)
		if rec.Code != http.StatusUnauthorized {
			t.Errorf("%s /api/v1/admin/api-keys unauth: expected 401, got %d", method, rec.Code)
		}
	}
	// Static assets and login stay public.
	for _, path := range []string{"/dashboard/login", "/dashboard/static/app.css", "/dashboard/static/htmx.min.js", "/dashboard/static/app.js"} {
		rec := f.anonDo(t, http.MethodGet, path, false)
		if rec.Code != http.StatusOK {
			t.Errorf("GET %s unauth: expected 200, got %d", path, rec.Code)
		}
	}
}

// TestDashboardCrossTenantIsolation: one session from a second tenant cannot
// reach the fixture tenant's resources through any owner-bound surface —
// the foreign id answers the designed not-found (404), never the content.
func TestDashboardCrossTenantIsolation(t *testing.T) {
	f := newMatrixFixture(t)
	cases := []struct {
		method, path string
	}{
		{http.MethodGet, "/api/v1/rag/collections/" + f.otherUserColl.ID},
		{http.MethodDelete, "/api/v1/rag/collections/" + f.otherUserColl.ID},
		{http.MethodGet, "/dashboard/knowledge/" + f.otherUserColl.ID},
		{http.MethodGet, "/dashboard/partials/extract/result?job=" + f.otherUserJob.ID},
		{http.MethodPost, "/api/v1/extract/jobs/" + f.otherUserJob.ID + "/cancel"},
		{http.MethodDelete, "/api/v1/extract/jobs/" + f.otherUserJob.ID},
	}
	for _, tc := range cases {
		// Same-tenant member: foreign tenant id is indistinguishable from absent.
		rec := f.sessDo(t, f.sessions[roleMember], tc.method, tc.path, "")
		if rec.Code != http.StatusNotFound && rec.Code != http.StatusForbidden {
			t.Errorf("member %s %s (other tenant id): expected 404/403, got %d body %.200s",
				tc.method, tc.path, rec.Code, rec.Body.String())
		}
		// The other tenant's own session can never see fixture-tenant data:
		// its member collection list contains only its own tenant's rows.
		rec = f.sessDo(t, f.otherSess, http.MethodGet, "/api/v1/rag/collections/"+f.memberColl.ID, "")
		if rec.Code == http.StatusOK {
			t.Errorf("other-tenant session read fixture collection %s: got 200", f.memberColl.ID)
		}
	}
	// Other-tenant session reads only its own collections.
	rec := f.sessDo(t, f.otherSess, http.MethodGet, "/api/v1/rag/collections", "")
	if rec.Code != http.StatusOK {
		t.Fatalf("other-tenant collections list: %d", rec.Code)
	}
	var payload struct {
		Collections []rag.Collection `json:"collections"`
	}
	if err := json.Unmarshal(rec.Body.Bytes(), &payload); err != nil {
		t.Fatal(err)
	}
	for _, c := range payload.Collections {
		if c.ID == f.memberColl.ID {
			t.Fatalf("other-tenant list leaked fixture collection %s", c.ID)
		}
	}
}
