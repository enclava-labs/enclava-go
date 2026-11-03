// activity_test.go — the Home producers (OA-18/OA-19/OA-31) against real
// dependencies: capability matrix incl. custom roles, authorization-before-
// limit failed documents, real progress projection, and the seeded-usage
// series. Every item asserted is backed by a real fixture row.
package httpx

import (
	"context"
	"encoding/json"
	"errors"
	"fmt"
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
	"enclava-go/internal/rag"
	"enclava-go/internal/source"
	"enclava-go/internal/store/postgres"
	"enclava-go/internal/testdb"
)

type activityFixture struct {
	budgetFixture // db, repo (billing), identity, tenant, app
	jobsSvc       *jobs.Service
	ragSvc        *rag.Service
	connSvc       *connectors.Service
	chatRepo      *postgres.ChatbotRepo
	runsRepo      *postgres.ChatbotRunsRepo
	mcpRepo       *postgres.MCPToolsRepo
	endpoints     *postgres.EndpointsRepo
	sourceRepo    *postgres.SourceRepo
	ctx           context.Context
}

func newActivityFixture(t *testing.T) *activityFixture {
	t.Helper()
	db := testdb.New(t)
	ctx := context.Background()
	tenant := testdb.SeedTenant(t, db, "activity-home")
	jobsRepo := postgres.NewJobsRepo(db)
	jobsSvc := jobs.NewService(jobsRepo, 0)
	artifactRepo := postgres.NewArtifactRepo(db)
	store, err := artifacts.NewStore(t.TempDir(), t.TempDir(), artifactRepo)
	if err != nil {
		t.Fatalf("artifact store: %v", err)
	}
	ragSvc := rag.NewService(postgres.NewRAGRepo(db), nil, nil, "", store, jobsSvc)
	sourceRepo := postgres.NewSourceRepo(db)
	connSvc := connectors.NewService(sourceRepo, jobsSvc, jobsRepo)
	chatRepo := postgres.NewChatbotRepo(db)
	runsRepo := postgres.NewChatbotRunsRepo(db)
	chatSvc := chatbot.NewService(chatRepo, nil, ragSvc, nil)
	chatSvc.ConfigureRuns(runsRepo, jobsSvc, nil, nil)
	billingRepo := postgres.NewBillingRepo(db)
	identity := postgres.NewIdentityRepo(db)
	endpoints := postgres.NewEndpointsRepo(db)
	mcpRepo := postgres.NewMCPToolsRepo(db)
	executor, err := inference.NewService(inference.Deps{
		Endpoints:  endpoints,
		Sources:    sourceRepo,
		Authorizer: source.NewAuthorizer(sourceRepo, time.Hour),
		Billing:    billing.NewService(billingRepo),
		Artifacts:  artifactRepo,
	})
	if err != nil {
		t.Fatalf("inference service: %v", err)
	}
	app := NewApp(auth.NewService(identity, ""), nil, "", nil, identity, nil, ragSvc, nil,
		chatSvc, connSvc, nil, billingRepo, endpoints, "sha256", "", "enc_", nil, nil,
		1, false, executor, nil, GateConfig{}, sourceRepo, artifactRepo)
	app.ConfigureActivity(jobsSvc)
	app.ConfigureMCP(mcpRepo, nil)
	return &activityFixture{
		budgetFixture: budgetFixture{db: db, repo: billingRepo, identity: identity, tenant: tenant, app: app},
		jobsSvc:       jobsSvc, ragSvc: ragSvc, connSvc: connSvc, chatRepo: chatRepo,
		runsRepo: runsRepo, mcpRepo: mcpRepo,
		endpoints: endpoints, sourceRepo: sourceRepo, ctx: ctx,
	}
}

// activityPrincipal is a custom-role principal: exactly the given scopes —
// the per-request capability set derives through dashboardCapabilities.
func activityPrincipal(t *testing.T, f *activityFixture, name string, scopes ...string) auth.Principal {
	t.Helper()
	user := testdb.SeedUser(t, f.db, f.tenant, name+"@activity.test", nil)
	scopesMap := make(map[string]struct{}, len(scopes))
	for _, s := range scopes {
		scopesMap[s] = struct{}{}
	}
	return auth.Principal{
		TenantID: f.tenant, ActorID: user.ID, UserID: user.ID,
		ActorKind: auth.ActorKindUser, Scopes: scopesMap,
	}
}

func activityKinds(items []AttentionItem) []string {
	kinds := make([]string, 0, len(items))
	for _, it := range items {
		kinds = append(kinds, it.Kind)
	}
	return kinds
}

func containsAttentionKind(items []AttentionItem, kind string) bool {
	for _, it := range items {
		if it.Kind == kind {
			return true
		}
	}
	return false
}

// --- fixtures -------------------------------------------------------------

// uploadFailedDocument seeds one failed document via the real pipeline: a
// whitespace-only upload fails the embed job with "no extractable text".
func (f *activityFixture) uploadFailedDocument(t *testing.T, uploader auth.Principal, name, collectionID string, audience []string) string {
	t.Helper()
	var body strings.Builder
	for _, b := range []byte(name) {
		body.WriteString(strings.Repeat(" ", int(b)+1))
		body.WriteByte('\t')
	}
	out, err := f.ragSvc.UploadDocument(f.ctx, uploader, collectionID, rag.UploadDocumentInput{
		FileName: name, MimeType: "text/plain", Bytes: []byte(body.String()), Audience: audience,
	})
	if err != nil {
		t.Fatalf("upload %s: %v", name, err)
	}
	return out.Document.ID
}

// runNextEmbedFailure leases the next queued job and settles it failed the
// way the real worker would — completing any bystander job first.
func (f *activityFixture) runNextEmbedFailure(t *testing.T) {
	t.Helper()
	for {
		claim, err := f.jobsSvc.Lease(f.ctx, "activity-worker", time.Minute)
		if err != nil {
			t.Fatalf("lease: %v", err)
		}
		if claim == nil {
			t.Fatal("no queued job to fail")
		}
		if claim.Job().Operation != rag.OperationEmbed && claim.Job().Operation != rag.OperationIngest {
			if _, err := claim.Complete(f.ctx, "{}"); err != nil {
				t.Fatalf("complete bystander: %v", err)
			}
			continue
		}
		if _, err := f.ragSvc.JobHandler()(f.ctx, claim, nil); err == nil {
			_ = claim.Fail(f.ctx, errors.New("expected failure"), jobs.FailureFatal)
			t.Fatal("expected embed attempt to fail")
		} else if err := claim.Fail(f.ctx, err, jobs.FailureFatal); err != nil {
			t.Fatalf("settle failed job: %v", err)
		}
		return
	}
}

// failExtraction enqueues and fails one extraction job through the real
// fenced worker path.
func (f *activityFixture) failExtraction(t *testing.T, p auth.Principal, key string) jobs.Job {
	t.Helper()
	job, _, err := f.jobsSvc.Enqueue(f.ctx, p, jobs.EnqueueRequest{
		TenantID: f.tenant, Operation: extract.OperationProcess, IdempotencyKey: key,
		InputRef: "art-" + key, InputHash: "hash-" + key,
	})
	if err != nil {
		t.Fatalf("enqueue extraction: %v", err)
	}
	for {
		claim, err := f.jobsSvc.Lease(f.ctx, "activity-worker", time.Minute)
		if err != nil {
			t.Fatalf("lease: %v", err)
		}
		if claim == nil {
			t.Fatalf("extraction job %s never claimed", job.ID)
		}
		if claim.Job().ID != job.ID {
			if _, err := claim.Complete(f.ctx, "{}"); err != nil {
				t.Fatalf("complete bystander: %v", err)
			}
			continue
		}
		if err := claim.Fail(f.ctx, errors.New("template not allowed"), jobs.FailureFatal); err != nil {
			t.Fatalf("fail extraction: %v", err)
		}
		return job
	}
}

// seedFailedRun inserts one owner-scoped failed run row (no bound job — the
// run row's own terminal status surfaces in the owner feed).
func (f *activityFixture) seedFailedRun(t *testing.T, owner auth.Principal, runID, convID string) {
	t.Helper()
	if err := f.chatRepo.Create(f.ctx, chatbot.Instance{
		TenantID: f.tenant, ID: "bot-home", ExternalID: "bot-home-ext", Name: "Home Bot", Model: "m", IsEnabled: true,
	}); err != nil && !strings.Contains(err.Error(), "SQLSTATE 23505") { // idempotent fixture bot
		t.Fatalf("create bot: %v", err)
	}
	if err := f.chatRepo.CreateConversation(f.ctx, chatbot.Conversation{
		TenantID: f.tenant, ID: convID, ChatbotID: "bot-home", OwnerActorID: owner.ActorID,
	}); err != nil {
		t.Fatalf("create conversation: %v", err)
	}
	if err := f.runsRepo.CreateRun(f.ctx, chatbot.AssistantRun{
		TenantID: f.tenant, ID: runID, ChatbotID: "bot-home", ConversationID: convID,
		OwnerActorID: owner.ActorID, Status: jobs.StatusFailed, Input: "tell me a joke",
		Error: "admission denied",
	}); err != nil {
		t.Fatalf("create run: %v", err)
	}
}

// seedSource creates one connector source row.
func (f *activityFixture) seedSource(t *testing.T, id, name string) {
	t.Helper()
	conn := source.Connection{
		TenantID: f.tenant, ID: id, Kind: source.ConnectionSlack, Name: name,
		CredentialRef: "CONNECTOR_SLACK_TEST", Config: map[string]any{},
		TargetCollectionID: "collection-x", Status: "active", CreatedBy: "actor-seed",
	}
	if err := f.sourceRepo.CreateConnection(f.ctx, conn); err != nil {
		t.Fatalf("create source %s: %v", id, err)
	}
}

// failSync enqueues and fails one connector.sync job against sourceID.
func (f *activityFixture) failSync(t *testing.T, p auth.Principal, key, sourceID, cause string) {
	t.Helper()
	if _, _, err := f.jobsSvc.Enqueue(f.ctx, p, jobs.EnqueueRequest{
		TenantID: f.tenant, Operation: connectors.OperationSync, IdempotencyKey: key,
		Subject:  jobs.Subject{ConnectionID: sourceID, ObjectID: "o", ObservedRevision: 1},
		InputRef: "art-" + key, InputHash: "hash-" + key,
	}); err != nil {
		t.Fatalf("enqueue sync: %v", err)
	}
	for {
		claim, err := f.jobsSvc.Lease(f.ctx, "activity-worker", time.Minute)
		if err != nil {
			t.Fatalf("lease: %v", err)
		}
		if claim == nil {
			t.Fatalf("sync job %s never claimed", key)
		}
		if claim.Job().Operation != connectors.OperationSync || claim.Job().Subject.ConnectionID != sourceID {
			if _, err := claim.Complete(f.ctx, "{}"); err != nil {
				t.Fatalf("complete bystander: %v", err)
			}
			continue
		}
		if err := claim.Fail(f.ctx, errors.New(cause), jobs.FailureFatal); err != nil {
			t.Fatalf("fail sync: %v", err)
		}
		return
	}
}

// seedConfidentialEndpoint creates one enabled confidential endpoint whose
// chat capability is bound to a model alias but carries no prices.
func (f *activityFixture) seedConfidentialEndpoint(t *testing.T) {
	t.Helper()
	err := f.endpoints.Create(f.ctx, inference.Endpoint{
		ID: "ep-conf", TenantID: f.tenant, Protocol: "confidential_sdk",
		Capabilities:  []string{"model:gpt-fast", inference.CapabilityChat},
		BaseOrigin:    "https://enclave.example",
		AssuranceMode: "strict", Enabled: true,
	})
	if err != nil {
		t.Fatalf("create endpoint: %v", err)
	}
}

// seedToolSchemas produces one pending and one changed discovery row.
func (f *activityFixture) seedToolSchemas(t *testing.T) {
	t.Helper()
	if err := f.mcpRepo.PutServer(f.ctx, postgres.MCPServer{
		TenantID: f.tenant, ID: "srv", Origin: "https://mcp.example", EndpointPath: "/mcp",
		AudienceScopes: []string{"mcp.invoke"}, Enabled: true,
	}); err != nil {
		t.Fatalf("put server: %v", err)
	}
	s1 := json.RawMessage(`{"type":"object","properties":{"q":{"type":"string"}}}`)
	s2 := json.RawMessage(`{"type":"object","properties":{"q":{"type":"integer"}}}`)
	out := json.RawMessage(`{"type":"object"}`)
	if err := f.mcpRepo.RecordDiscovery(f.ctx, f.tenant, "srv", []postgres.DiscoveredToolSnapshot{
		{Name: "search", InputSchema: s1, OutputSchema: out, Annotations: mcp.ToolAnnotations{Title: "S"}},
	}); err != nil {
		t.Fatalf("record discovery: %v", err)
	}
	if _, err := f.mcpRepo.ApproveDiscovery(f.ctx, f.tenant, "srv", "search", 1, "approved.search", []string{"mcp.invoke"}); err != nil {
		t.Fatalf("approve: %v", err)
	}
	if err := f.mcpRepo.RecordDiscovery(f.ctx, f.tenant, "srv", []postgres.DiscoveredToolSnapshot{
		{Name: "search", InputSchema: s2, OutputSchema: out},
		{Name: "brand-new", InputSchema: s1, OutputSchema: out},
	}); err != nil {
		t.Fatalf("record changed discovery: %v", err)
	}
}

// seedTenantBudget creates one tenant-subject amount budget.
func (f *activityFixture) seedTenantBudget(t *testing.T, limit int64) {
	t.Helper()
	if err := f.repo.CreateBudget(f.ctx, f.tenant, "budget-org", billing.Budget{
		Name: "org", Subject: billing.Subject{Type: billing.BudgetSubjectTenant, ID: f.tenant},
		Kind: billing.BudgetKindAmount, Period: billing.PeriodMonthly,
		LimitAmountMicros: limit, Currency: "USD", StartsAt: time.Now().UTC().Add(-time.Hour),
	}); err != nil {
		t.Fatalf("create budget: %v", err)
	}
}

// --- activity -------------------------------------------------------------

func TestDashboardActivityMemberAndManagerScope(t *testing.T) {
	f := newActivityFixture(t)
	alice := activityPrincipal(t, f, "alice", "extract")
	bob := activityPrincipal(t, f, "bob", "extract")
	manager := activityPrincipal(t, f, "extract-manager", "extract", "extract.manage")
	admin := activityPrincipal(t, f, "admin", auth.ScopePlatformAdmin)

	aliceJob, _, err := f.jobsSvc.Enqueue(f.ctx, alice, jobs.EnqueueRequest{
		TenantID: f.tenant, Operation: extract.OperationProcess, IdempotencyKey: "alice-1",
		InputRef: "art-alice", InputHash: "hash-alice",
	})
	if err != nil {
		t.Fatal(err)
	}
	bobJob, _, err := f.jobsSvc.Enqueue(f.ctx, bob, jobs.EnqueueRequest{
		TenantID: f.tenant, Operation: extract.OperationProcess, IdempotencyKey: "bob-1",
		InputRef: "art-bob", InputHash: "hash-bob",
	})
	if err != nil {
		t.Fatal(err)
	}

	// Member: own queued/running only.
	items, err := f.app.dashboardActivity(f.ctx, alice, dashboardCapabilities(alice))
	if err != nil {
		t.Fatalf("member activity: %v", err)
	}
	if len(items) != 1 || items[0].ID != aliceJob.ID {
		t.Fatalf("member activity = %#v, want only own job %s", items, aliceJob.ID)
	}
	if items[0].Operation != extract.OperationProcess || items[0].Status != jobs.StatusQueued {
		t.Fatalf("activity projection = %+v", items[0])
	}
	if items[0].Label == "" || items[0].Href != "/dashboard/extract" {
		t.Fatalf("activity label/href = %q/%q", items[0].Label, items[0].Href)
	}
	if items[0].Progress != nil {
		t.Fatalf("non-counted job must carry no progress: %q", items[0].Progress)
	}

	// Manager: the tenant's jobs in the managed group — both members' jobs.
	managed, err := f.app.dashboardActivity(f.ctx, manager, dashboardCapabilities(manager))
	if err != nil {
		t.Fatalf("manager activity: %v", err)
	}
	ids := map[string]bool{}
	for _, it := range managed {
		ids[it.ID] = true
	}
	if !ids[aliceJob.ID] || !ids[bobJob.ID] {
		t.Fatalf("manager activity = %#v, want the tenant's managed jobs", managed)
	}

	// Admin: platform.admin grants no feature/manage capability — the feed
	// falls back to own-actor rows, of which the admin has none.
	adminItems, err := f.app.dashboardActivity(f.ctx, admin, dashboardCapabilities(admin))
	if err != nil || len(adminItems) != 0 {
		t.Fatalf("admin activity = %#v err=%v, want own-actor fallback only", adminItems, err)
	}
}

func TestDashboardActivityCarriesRealProgressVerbatim(t *testing.T) {
	f := newActivityFixture(t)
	p := activityPrincipal(t, f, "uploader", "extract")

	counted, _, err := f.jobsSvc.Enqueue(f.ctx, p, jobs.EnqueueRequest{
		TenantID: f.tenant, Operation: extract.OperationProcess, IdempotencyKey: "counted",
		InputRef: "art-counted", InputHash: "hash-counted",
	})
	if err != nil {
		t.Fatal(err)
	}
	for {
		claim, err := f.jobsSvc.Lease(f.ctx, "activity-worker", time.Minute)
		if err != nil || claim == nil {
			t.Fatalf("lease: claim=%v err=%v", claim, err)
		}
		if claim.Job().ID != counted.ID {
			_, _ = claim.Complete(f.ctx, "{}")
			continue
		}
		if err := claim.Progress(f.ctx, 42, "halfway"); err != nil {
			t.Fatalf("progress: %v", err)
		}
		break
	}

	items, err := f.app.dashboardActivity(f.ctx, p, dashboardCapabilities(p))
	if err != nil {
		t.Fatalf("activity: %v", err)
	}
	var countedItem *ActivityItem
	for i := range items {
		if items[i].ID == counted.ID {
			countedItem = &items[i]
		}
	}
	if countedItem == nil {
		t.Fatalf("running job missing from activity: %#v", items)
	}
	if countedItem.Progress == nil || countedItem.Status != jobs.StatusRunning {
		t.Fatalf("counted running job = %+v", countedItem)
	}
	var doc struct {
		Percent int    `json:"percent"`
		Message string `json:"message"`
	}
	if err := json.Unmarshal(countedItem.Progress, &doc); err != nil || doc.Percent != 42 || doc.Message != "halfway" {
		t.Fatalf("progress = %q (decoded %+v err=%v), want the stored counts verbatim", countedItem.Progress, doc, err)
	}
}

// --- attention --------------------------------------------------------------

func TestDashboardAttentionCapabilityMatrix(t *testing.T) {
	f := newActivityFixture(t)
	uploader := activityPrincipal(t, f, "uploader", "rag")
	member := activityPrincipal(t, f, "member", "rag")
	manager := activityPrincipal(t, f, "kmanager", "rag", "rag.manage")
	searchOnly := activityPrincipal(t, f, "search-only", "rag")
	extractOnly := activityPrincipal(t, f, "extract-only", "extract")
	chatOnly := activityPrincipal(t, f, "chat-only", "chatbot")
	revoked := activityPrincipal(t, f, "revoked")
	admin := activityPrincipal(t, f, "admin", auth.ScopePlatformAdmin, "pricing.manage")

	collection, err := f.ragSvc.CreateCollection(f.ctx, uploader, rag.CreateCollectionInput{
		Name: "Docs", ChunkSize: 100, ChunkOverlap: 10, ACL: map[string]any{"public": true},
	})
	if err != nil {
		t.Fatal(err)
	}
	// Three restricted failures (uploader-only audience) uploaded BEFORE the
	// one accessible failure — authorization must happen before the limit.
	for i := range 3 {
		f.uploadFailedDocument(t, uploader, fmt.Sprintf("restricted-%d.txt", i), collection.ID, []string{uploader.ActorID})
		f.runNextEmbedFailure(t)
	}
	f.uploadFailedDocument(t, uploader, "shared-fail.txt", collection.ID,
		[]string{uploader.ActorID, member.ActorID, manager.ActorID})
	f.runNextEmbedFailure(t)

	f.failExtraction(t, uploader, "failed-extract-1")
	f.failExtraction(t, extractOnly, "failed-extract-member")
	f.seedFailedRun(t, uploader, "run-home-1", "conv-home-1")
	f.seedFailedRun(t, chatOnly, "run-home-2", "conv-home-2")
	f.seedSource(t, "src-auth", "Auth Source")
	f.failSync(t, uploader, "sync-auth-1", "src-auth", "slack: status 401 unauthorized")
	f.seedConfidentialEndpoint(t)
	f.seedToolSchemas(t)
	f.seedTenantBudget(t, 100_000_000)
	seedSettledUsage(t, f.budgetFixture, "att-90", billing.Subject{Type: billing.BudgetSubjectTenant, ID: f.tenant}, 1000, 90_000_000, "USD", time.Now().UTC())

	// Member with audience on the shared document: exactly that failure, and
	// none of the restricted ones — restricted rows precede it in the window.
	memberItems, err := f.app.dashboardAttention(f.ctx, member, dashboardCapabilities(member))
	if err != nil {
		t.Fatalf("member attention: %v", err)
	}
	if containsAttentionKind(memberItems, "failed_document") {
		t.Fatalf("member lacks knowledge.manage yet saw failed_document: %#v", activityKinds(memberItems))
	}
	// Own failures only under their own feature caps: member holds neither
	// extract nor chat → no failure kind at all.
	for _, kind := range []string{"failed_extraction", "failed_run"} {
		if containsAttentionKind(memberItems, kind) {
			t.Fatalf("cap-less member saw %s: %#v", kind, activityKinds(memberItems))
		}
	}

	// search-only member: NEITHER failure kind.
	searchItems, err := f.app.dashboardAttention(f.ctx, searchOnly, dashboardCapabilities(searchOnly))
	if err != nil || len(searchItems) != 0 {
		t.Fatalf("search-only attention = %#v err=%v, want nothing (no failure caps)", searchItems, err)
	}

	// extract-only member: own failed extraction, not the chat kind — and
	// ONLY the member's own job, never the uploader's.
	extractItems, err := f.app.dashboardAttention(f.ctx, extractOnly, dashboardCapabilities(extractOnly))
	if err != nil {
		t.Fatalf("extract-only attention: %v", err)
	}
	extractionCount := 0
	for _, it := range extractItems {
		if it.Kind == "failed_extraction" {
			extractionCount++
		}
	}
	if extractionCount != 1 || containsAttentionKind(extractItems, "failed_run") {
		t.Fatalf("extract-only attention = %#v, want exactly the member's own failed_extraction", activityKinds(extractItems))
	}
	for _, it := range extractItems {
		if it.Kind == "failed_extraction" && it.ActionHref != "/dashboard/extract" {
			t.Fatalf("failed extraction href = %q", it.ActionHref)
		}
	}

	// chat-only member: own failed run, not the extraction kind.
	chatItems, err := f.app.dashboardAttention(f.ctx, chatOnly, dashboardCapabilities(chatOnly))
	if err != nil {
		t.Fatalf("chat-only attention: %v", err)
	}
	if !containsAttentionKind(chatItems, "failed_run") || containsAttentionKind(chatItems, "failed_extraction") {
		t.Fatalf("chat-only attention = %#v, want failed_run only", activityKinds(chatItems))
	}
	for _, it := range chatItems {
		if it.Kind == "failed_run" && it.ActionHref != "/dashboard/chat/conv-home-2" {
			t.Fatalf("failed run href = %q, want the member's own conversation", it.ActionHref)
		}
	}

	// Revoked: nothing survives.
	revokedItems, err := f.app.dashboardAttention(f.ctx, revoked, dashboardCapabilities(revoked))
	if err != nil || len(revokedItems) != 0 {
		t.Fatalf("revoked attention = %#v err=%v, want empty", revokedItems, err)
	}

	// Knowledge manager WITH audience membership sees the accessible failure
	// (never the restricted ones); href points at its collection.
	managerItems, err := f.app.dashboardAttention(f.ctx, manager, dashboardCapabilities(manager))
	if err != nil {
		t.Fatalf("manager attention: %v", err)
	}
	docItems := 0
	for _, it := range managerItems {
		if it.Kind != "failed_document" {
			continue
		}
		docItems++
		if it.ActionHref != "/dashboard/knowledge/"+collection.ID {
			t.Fatalf("failed document href = %q", it.ActionHref)
		}
		if it.Severity != "bad" || it.Text == "" {
			t.Fatalf("failed document item = %+v", it)
		}
	}
	if docItems != 1 {
		t.Fatalf("manager failed-document items = %d, want exactly the 1 accessible failure", docItems)
	}

	// Admin: the manager kinds their capabilities gate — tool schemas (both
	// states), unverified endpoint, unpriced units, ≥80% budget — and NO
	// member failure kinds (platform.admin grants no feature capabilities).
	adminItems, err := f.app.dashboardAttention(f.ctx, admin, dashboardCapabilities(admin))
	if err != nil {
		t.Fatalf("admin attention: %v", err)
	}
	for _, kind := range []string{"tool_schema_changed", "tool_schema_pending", "unverified_endpoint", "unpriced_units", "budget"} {
		if !containsAttentionKind(adminItems, kind) {
			t.Fatalf("admin attention missing %s: %#v", kind, activityKinds(adminItems))
		}
	}
	for _, kind := range []string{"failed_extraction", "failed_run", "failed_document", "sync_failed"} {
		if containsAttentionKind(adminItems, kind) {
			t.Fatalf("admin must not receive member/ungated kind %s: %#v", kind, activityKinds(adminItems))
		}
	}
	unpricedRows := 0
	for _, it := range adminItems {
		switch it.Kind {
		case "unverified_endpoint":
			if !strings.Contains(it.Text, "1 of 1") {
				t.Fatalf("unverified endpoint text = %q, want the tri-state count", it.Text)
			}
		case "unpriced_units":
			// Plain words, never the raw capability or unit ids (glossary).
			unpricedRows++
			if !strings.Contains(it.Text, "requests that need them are refused") || strings.Contains(it.Text, "chat/completions") || strings.Contains(it.Text, "cached_input") || it.ActionHref != "/dashboard/models/pricing" {
				t.Fatalf("unpriced item = %+v", it)
			}
		case "budget":
			if !strings.Contains(it.Text, "90%") || it.ActionHref != "/dashboard/usage/budgets" {
				t.Fatalf("budget item = %+v", it)
			}
		case "tool_schema_changed":
			if !strings.Contains(it.Text, "1 approved tool") || it.ActionHref != "/dashboard/tools" {
				t.Fatalf("tool changed item = %+v", it)
			}
		case "tool_schema_pending":
			if !strings.Contains(it.Text, "1 newly discovered") {
				t.Fatalf("tool pending item = %+v", it)
			}
		}
	}
	// One row per endpoint, never one per capability of the same endpoint.
	if unpricedRows != 1 {
		t.Fatalf("unpriced rows = %d, want one for the single seeded endpoint", unpricedRows)
	}
}

func TestDashboardAttentionSyncFailureClassification(t *testing.T) {
	f := newActivityFixture(t)
	manager := activityPrincipal(t, f, "sources-manager", "connectors.manage")
	uploader := activityPrincipal(t, f, "sync-uploader", "connectors.manage")

	f.seedSource(t, "src-ok", "Healthy Source")
	f.seedSource(t, "src-auth", "Auth Failed Source")
	f.seedSource(t, "src-stalled", "Stalled Source")
	// The healthy source's sync runs and completes — its newest job is
	// terminal-successful, so the source speaks no attention.
	if _, _, err := f.jobsSvc.Enqueue(f.ctx, uploader, jobs.EnqueueRequest{
		TenantID: f.tenant, Operation: connectors.OperationSync, IdempotencyKey: "sync-ok-1",
		Subject:  jobs.Subject{ConnectionID: "src-ok", ObjectID: "o", ObservedRevision: 1},
		InputRef: "art-ok", InputHash: "hash-ok",
	}); err != nil {
		t.Fatal(err)
	}
	f.failSync(t, uploader, "sync-auth-2", "src-auth", "google_drive export request failed: status 403 Forbidden")
	// Stalled: running job started over a day ago.
	stalled, _, err := f.jobsSvc.Enqueue(f.ctx, uploader, jobs.EnqueueRequest{
		TenantID: f.tenant, Operation: connectors.OperationSync, IdempotencyKey: "sync-stall-1",
		Subject:  jobs.Subject{ConnectionID: "src-stalled", ObjectID: "o", ObservedRevision: 1},
		InputRef: "art-stall", InputHash: "hash-stall",
	})
	if err != nil {
		t.Fatal(err)
	}
	if _, err := f.jobsSvc.Lease(f.ctx, "activity-worker", time.Minute); err != nil {
		t.Fatalf("lease stalled job: %v", err)
	}
	if _, err := f.db.SQL.ExecContext(f.ctx,
		`UPDATE jobs SET submitted_at = submitted_at - interval '2 days' WHERE tenant_id = $1::uuid AND id = $2`,
		f.tenant, stalled.ID); err != nil {
		t.Fatalf("backdate stalled: %v", err)
	}

	items, err := f.app.dashboardAttention(f.ctx, manager, dashboardCapabilities(manager))
	if err != nil {
		t.Fatalf("attention: %v", err)
	}
	var authItem, stallItem *AttentionItem
	for i := range items {
		if items[i].Kind == "sync_failed" && strings.Contains(items[i].Text, "Auth Failed Source") {
			authItem = &items[i]
		}
		if items[i].Kind == "sync_stalled" {
			stallItem = &items[i]
		}
	}
	if authItem == nil || !strings.Contains(authItem.Text, "authentication") || authItem.ActionHref != "/dashboard/sources/src-auth" {
		t.Fatalf("auth-failed sync item missing: %#v", activityKinds(items))
	}
	if stallItem == nil || !strings.Contains(stallItem.Text, "Stalled Source") || !strings.Contains(stallItem.Text, "over a day") {
		t.Fatalf("stalled sync item missing: %#v", activityKinds(items))
	}
	for _, it := range items {
		if it.Kind == "sync_failed" && strings.Contains(it.Text, "Healthy Source") {
			t.Fatalf("healthy source produced an attention item: %#v", items)
		}
	}
}

func TestDashboardAttentionAndActivityEmptyProducersProduceNothing(t *testing.T) {
	f := newActivityFixture(t)
	p := activityPrincipal(t, f, "plain", "rag", "extract", "chat", "rag.manage", "connectors.manage", "extract.manage", "chatbot.manage")
	caps := dashboardCapabilities(p)

	items, err := f.app.dashboardAttention(f.ctx, p, caps)
	if err != nil || len(items) != 0 {
		t.Fatalf("attention on empty producers = %#v err=%v, want nothing invented", items, err)
	}
	activity, err := f.app.dashboardActivity(f.ctx, p, caps)
	if err != nil || len(activity) != 0 {
		t.Fatalf("activity on empty producers = %#v err=%v, want empty", activity, err)
	}
	metrics, err := f.app.dashboardHomeMetrics(f.ctx, p, caps)
	if err != nil {
		t.Fatalf("metrics: %v", err)
	}
	if metrics.Requests30d != 0 || metrics.DocumentTotal != 0 || metrics.ConversationTotal != 0 ||
		len(metrics.Spend30d) != 0 || len(metrics.Series30d) != 0 || metrics.OrgBudget != nil {
		t.Fatalf("metrics on empty producers = %+v, want zero/empty", metrics)
	}
}

// TestDashboardHomeMetricsRealReads seeds multi-day usage, a document set
// and conversations and asserts the KPI payload against them.
func TestDashboardHomeMetricsRealReads(t *testing.T) {
	f := newActivityFixture(t)
	uploader := activityPrincipal(t, f, "metrics-uploader", "rag")
	member := activityPrincipal(t, f, "metrics-member", "rag")
	admin := activityPrincipal(t, f, "metrics-admin", auth.ScopePlatformAdmin)

	collection, err := f.ragSvc.CreateCollection(f.ctx, uploader, rag.CreateCollectionInput{
		Name: "Metrics", ChunkSize: 100, ChunkOverlap: 10, ACL: map[string]any{"public": true},
	})
	if err != nil {
		t.Fatal(err)
	}
	for i := range 3 {
		if _, err := f.ragSvc.UploadDocument(f.ctx, uploader, collection.ID, rag.UploadDocumentInput{
			FileName: fmt.Sprintf("doc-%d.txt", i), MimeType: "text/plain",
			Bytes: []byte(fmt.Sprintf("real document body %d", i)), Audience: []string{uploader.ActorID, member.ActorID},
		}); err != nil {
			t.Fatal(err)
		}
	}
	f.seedFailedRun(t, member, "run-metrics", "conv-metrics")

	now := time.Now().UTC()
	f.seedTenantBudget(t, 100_000_000)
	seedSettledUsage(t, f.budgetFixture, "metrics-90", billing.Subject{Type: billing.BudgetSubjectTenant, ID: f.tenant}, 1000, 90_000_000, "USD", now)
	seedSettledUsage(t, f.budgetFixture, "metrics-eu", billing.Subject{Type: billing.BudgetSubjectTenant, ID: f.tenant}, 500, 40_000_000, "EUR", now.Add(-48*time.Hour))

	metrics, err := f.app.dashboardHomeMetrics(f.ctx, admin, dashboardCapabilities(admin))
	if err != nil {
		t.Fatalf("admin metrics: %v", err)
	}
	// Two settled usage records across two currencies, never merged.
	if metrics.Requests30d != 2 {
		t.Fatalf("Requests30d = %d, want 2", metrics.Requests30d)
	}
	spend := map[string]int64{}
	for _, m := range metrics.Spend30d {
		spend[m.Currency] = m.AmountMicros
	}
	if spend["USD"] != 90_000_000 || spend["EUR"] != 40_000_000 {
		t.Fatalf("Spend30d = %+v, want per-currency rows", metrics.Spend30d)
	}
	if len(metrics.Series30d) != 2 {
		t.Fatalf("Series30d = %+v, want one UTC-day row per day per currency", metrics.Series30d)
	}
	days := map[string]int64{}
	for _, row := range metrics.Series30d {
		days[row.Day.UTC().Format("2006-01-02")+row.Currency] = row.AmountMicros
	}
	if days[now.Format("2006-01-02")+"USD"] != 90_000_000 || days[now.AddDate(0, 0, -2).Format("2006-01-02")+"EUR"] != 40_000_000 {
		t.Fatalf("Series30d days = %v, want the seeded per-day values", days)
	}
	if metrics.OrgBudget == nil || metrics.OrgBudget.Counted != 90_000_000 || metrics.OrgBudget.LimitValue != 100_000_000 {
		t.Fatalf("OrgBudget = %+v, want the tenant budget progress scalar", metrics.OrgBudget)
	}

	// The admin's own authorized document count is zero — rag.manage-derived
	// capabilities never bypass source ACLs, and admin holds no audience.
	if metrics.DocumentTotal != 0 {
		t.Fatalf("admin DocumentTotal = %d, want the ACL-authorized count (0)", metrics.DocumentTotal)
	}
	if metrics.ConversationTotal != 0 {
		t.Fatalf("admin ConversationTotal = %d, want own rows only (0)", metrics.ConversationTotal)
	}

	// Member without billing.view: counts present, billing fields absent.
	memberMetrics, err := f.app.dashboardHomeMetrics(f.ctx, member, dashboardCapabilities(member))
	if err != nil {
		t.Fatalf("member metrics: %v", err)
	}
	if memberMetrics.Requests30d != 0 || len(memberMetrics.Spend30d) != 0 ||
		len(memberMetrics.Series30d) != 0 || memberMetrics.OrgBudget != nil {
		t.Fatalf("member billing fields leaked: %+v", memberMetrics)
	}
	if memberMetrics.DocumentTotal != 3 {
		t.Fatalf("member DocumentTotal = %d, want the 3 shared documents", memberMetrics.DocumentTotal)
	}
	if memberMetrics.ConversationTotal != 1 {
		t.Fatalf("member ConversationTotal = %d, want the member's own conversation", memberMetrics.ConversationTotal)
	}
}

func TestDashboardProducersDegradeWithoutJobsWiring(t *testing.T) {
	f := newActivityFixture(t)
	p := activityPrincipal(t, f, "unwired", "extract")
	caps := dashboardCapabilities(p)
	f.app.jobsSvc = nil // the un-wired app

	activity, err := f.app.dashboardActivity(f.ctx, p, caps)
	if err != nil || len(activity) != 0 {
		t.Fatalf("un-wired activity = %#v err=%v, want empty degradation", activity, err)
	}
	items, err := f.app.dashboardAttention(f.ctx, p, caps)
	if err != nil {
		t.Fatalf("un-wired attention: %v", err)
	}
	if containsAttentionKind(items, "failed_extraction") {
		t.Fatalf("un-wired attention produced job items: %#v", activityKinds(items))
	}
}
