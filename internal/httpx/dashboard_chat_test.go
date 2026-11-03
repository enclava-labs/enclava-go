// dashboard_chat_test.go — the Chat page suite (plan 10-02): server-rendered
// history/thread truth, grant isolation, deep-link guards, the run-status
// markers and both evidence-partial not-found branches. Members carry
// explicit mode:list stored grants; the grantless fixture proves deny-default
// (D-25) — never an admin/wildcard stand-in.
package httpx

import (
	"context"
	"net/http"
	"net/http/httptest"
	"net/url"
	"strings"
	"testing"

	"enclava-go/internal/artifacts"
	"enclava-go/internal/auth"
	"enclava-go/internal/chatbot"
	"enclava-go/internal/inference"
	"enclava-go/internal/jobs"
	"enclava-go/internal/rag"
	"enclava-go/internal/store/postgres"
)

// chatFixture is the chat-suite app: real services over an isolated schema,
// durable runs wired, artifacts configured so the evidence card's retained
// copy link resolves, session login through the real browser form.
type chatFixture struct {
	*identityFixture
	chatSvc   *chatbot.Service
	chatRepo  *postgres.ChatbotRepo
	runsRepo  *postgres.ChatbotRunsRepo
	ragRepo   *postgres.RAGRepo
	jobsSvc   *jobs.Service
	adminAuth auth.Principal
}

func newChatFixture(t *testing.T) *chatFixture {
	f := &chatFixture{identityFixture: newIdentityFixture(t)}
	f.chatRepo = postgres.NewChatbotRepo(f.db)
	f.runsRepo = postgres.NewChatbotRunsRepo(f.db)
	f.ragRepo = postgres.NewRAGRepo(f.db)
	f.jobsSvc = jobs.NewService(postgres.NewJobsRepo(f.db), 0)
	// Artifacts back the retained-copy link on the evidence card.
	store, err := artifacts.NewStore(t.TempDir(), t.TempDir(), postgres.NewArtifactRepo(f.db))
	if err != nil {
		t.Fatalf("artifact store: %v", err)
	}
	ragSvc := rag.NewService(f.ragRepo, nil, nil, "", store, f.jobsSvc)
	f.app.ragSvc = ragSvc
	f.chatSvc = chatbot.NewService(f.chatRepo, postgres.NewPromptsRepo(f.db), ragSvc, nil)
	f.chatSvc.ConfigureRuns(f.runsRepo, f.jobsSvc, nil, nil)
	f.app.chatbotSvc = f.chatSvc
	f.adminAuth = auth.Principal{
		TenantID:            f.tenant,
		ActorID:             "chat_admin",
		UserID:              "chat_admin",
		ActorKind:           auth.ActorKindServiceAccount,
		Scopes:              map[string]struct{}{auth.ScopePlatformAdmin: {}},
		AllowedModelsAll:    true,
		AllowedChatbotsAll:  true,
		AllowedAgentsAll:    true,
		AllowedEndpointsAll: true,
	}
	return f
}

// memberSession logs a member in with explicit list grants and returns the
// session + CSRF cookies along with the user id (the conversation-owner key).
// An empty botID seeds the grantless deny-default fixture.
func (f *chatFixture) memberSession(t *testing.T, email, botID, model string) (session, csrf *http.Cookie, userID string) {
	t.Helper()
	grants := auth.ResourceGrants{
		Models:     auth.Allowlist{Items: []string{model}},
		Assistants: auth.Allowlist{Items: []string{botID}},
	}
	if botID == "" {
		grants = auth.ResourceGrants{}
	}
	session, csrf = loginUserWithGrants(t, f.identityFixture, email, []string{"chatbot", "rag"}, grants, "chat-pass")
	if err := f.db.SQL.QueryRowContext(context.Background(),
		`SELECT id::text FROM users WHERE tenant_id = $1::uuid AND email = $2`,
		f.tenant, email).Scan(&userID); err != nil {
		t.Fatalf("read member id: %v", err)
	}
	return session, csrf, userID
}

func (f *chatFixture) seedAssistant(t *testing.T) chatbot.Instance {
	t.Helper()
	bot, err := f.chatSvc.Create(context.Background(), f.adminAuth, chatbot.CreateChatbotInput{
		Name: "Contract Reviewer", Model: "chat-model-1", Audience: []string{"*"},
	})
	if err != nil {
		t.Fatalf("seed assistant: %v", err)
	}
	return bot
}

func (f *chatFixture) seedConversation(t *testing.T, id, owner, botID, content string) {
	t.Helper()
	ctx := context.Background()
	if err := f.chatRepo.CreateConversation(ctx, chatbot.Conversation{
		TenantID: f.tenant, ID: id, ChatbotID: botID, OwnerActorID: owner,
	}); err != nil {
		t.Fatalf("seed conversation %s: %v", id, err)
	}
	if err := f.chatRepo.AddMessage(ctx, chatbot.Message{
		TenantID: f.tenant, ID: id + "_msg1", ConversationID: id,
		Role: "user", Content: content, OwnerActorID: owner,
	}); err != nil {
		t.Fatalf("seed message %s: %v", id, err)
	}
}

func (f *chatFixture) chatGet(t *testing.T, session, csrf *http.Cookie, path string) *httptest.ResponseRecorder {
	t.Helper()
	return sessionDo(t, f.app, http.MethodGet, path, "", session, csrf)
}

// TestDashboardChatPageHistoryIsolationAndGuards covers plan Test 1: the page
// renders the member's own two conversations newest-first under Today/Earlier,
// another owner's deep link 404s, and a member without the chat capability
// gets the empty 403 on the history partial. Unauthenticated deep links carry
// the requested URL back through login.
func TestDashboardChatPageHistoryIsolationAndGuards(t *testing.T) {
	f := newChatFixture(t)
	bot := f.seedAssistant(t)
	sess, csrf, memberID := f.memberSession(t, "chat-member@example.com", bot.ID, "chat-model-1")

	f.seedConversation(t, "conv_today", memberID, bot.ID, "what changed in section nine")
	f.seedConversation(t, "conv_earlier", memberID, bot.ID, "older question about pricing")
	other := f.seedAssistant(t)
	f.seedConversation(t, "conv_foreign", "someone_else", other.ID, "not your conversation")
	if _, err := f.db.SQL.ExecContext(context.Background(),
		`UPDATE conversations SET created_at = '2020-01-02T00:00:00Z'::timestamptz WHERE id = 'conv_earlier'`); err != nil {
		t.Fatalf("backdate conversation: %v", err)
	}
	if _, err := f.db.SQL.ExecContext(context.Background(),
		`UPDATE messages SET created_at = '2020-01-02T00:00:00Z'::timestamptz WHERE conversation_id = 'conv_earlier'`); err != nil {
		t.Fatalf("backdate messages: %v", err)
	}

	rec := f.chatGet(t, sess, csrf, "/dashboard/chat")
	if rec.Code != http.StatusOK {
		t.Fatalf("page status = %d body=%s", rec.Code, rec.Body.String())
	}
	body := rec.Body.String()
	if !strings.Contains(body, "conv_today") || !strings.Contains(body, "what changed in section nine") {
		t.Fatalf("history missing own conversation: %s", body)
	}
	if !strings.Contains(body, "Today") || !strings.Contains(body, "Earlier") {
		t.Fatalf("history missing Today/Earlier groups: %s", body)
	}
	if strings.Contains(body, "not your conversation") || strings.Contains(body, "conv_foreign") {
		t.Fatal("another owner's conversation leaked into the member history")
	}
	if today, earlier := strings.Index(body, "conv_today"), strings.Index(body, "conv_earlier"); today > earlier {
		t.Fatalf("history not newest-first: today@%d earlier@%d", today, earlier)
	}

	// Deep link to another owner's conversation: the uniform 404 page.
	rec = f.chatGet(t, sess, csrf, "/dashboard/chat/conv_foreign")
	if rec.Code != http.StatusNotFound {
		t.Fatalf("foreign deep link = %d, want 404 body=%s", rec.Code, rec.Body.String())
	}

	// Member without the chat capability: empty-body 403 on the partial.
	ragOnly, ragOnlyCSRF := loginUser(t, f.identityFixture, "rag-only@example.com", []string{"rag"}, "chat-pass")
	rec = f.chatGet(t, ragOnly, ragOnlyCSRF, "/dashboard/partials/chat/history")
	if rec.Code != http.StatusForbidden {
		t.Fatalf("no-chat-cap partial = %d, want 403", rec.Code)
	}
	if strings.TrimSpace(rec.Body.String()) != "" {
		t.Fatalf("partial denial must be empty-body, got %q", rec.Body.String())
	}

	// Unauthenticated deep link lands back on the requested URL after login.
	req := httptest.NewRequest(http.MethodGet, "/dashboard/chat/conv_today", nil)
	rec = httptest.NewRecorder()
	f.app.Handler().ServeHTTP(rec, req)
	if rec.Code != http.StatusSeeOther {
		t.Fatalf("unauthenticated deep link = %d", rec.Code)
	}
	if got := rec.Header().Get("Location"); got != "/dashboard/login?return_to="+url.QueryEscape("/dashboard/chat/conv_today") {
		t.Fatalf("login redirect = %q, want return_to=/dashboard/chat/conv_today", got)
	}
}

// TestDashboardChatThreadRender covers plan Tests 2+3: assistant header from
// the run's persisted endpoint_id/model with the confidential chip only for
// a confidential endpoint, steps as <details class="toolstep">, the pending
// run marker carrying the live status, and revoked-evidence turns staying
// suppressed server-side.
func TestDashboardChatThreadRender(t *testing.T) {
	f := newChatFixture(t)
	ctx := context.Background()
	bot := f.seedAssistant(t)
	sess, csrf, memberID := f.memberSession(t, "thread-member@example.com", bot.ID, "chat-model-1")

	if err := f.app.endpoints.Create(ctx, inference.Endpoint{
		ID: "ep_conf", TenantID: f.tenant, Protocol: "tinfoil",
		BaseOrigin: "https://enclave.example", AssuranceMode: "hardware",
	}); err != nil {
		t.Fatalf("seed endpoint: %v", err)
	}

	f.seedConversation(t, "conv_thread", memberID, bot.ID, "summarise the contract")
	// endpoint_id/model are OBSERVED route fields — the worker persists them
	// through UpdateRun after dispatch, never at CreateRun.
	runDone := chatbot.AssistantRun{
		TenantID: f.tenant, ID: "run_done", ChatbotID: bot.ID, ConversationID: "conv_thread",
		OwnerActorID: memberID, Status: jobs.StatusQueued, Input: "summarise the contract",
	}
	if err := f.runsRepo.CreateRun(ctx, runDone); err != nil {
		t.Fatalf("seed done run: %v", err)
	}
	runDone.EndpointID, runDone.Model, runDone.Status = "ep_conf", "chat-model-1", jobs.StatusSucceeded
	if err := f.runsRepo.UpdateRun(ctx, runDone); err != nil {
		t.Fatalf("observe done run route: %v", err)
	}
	// Real readable evidence so the citation turn survives the visibility
	// gate with its content and data-evidence-ids intact.
	collection, err := f.app.ragSvc.CreateCollection(ctx, auth.Principal{
		TenantID: f.tenant, ActorID: memberID, UserID: memberID, ActorKind: auth.ActorKindUser,
	}, rag.CreateCollectionInput{Name: "thread-docs", ACL: map[string]any{"public": true}})
	if err != nil {
		t.Fatalf("seed thread collection: %v", err)
	}
	df := ragDocsFixture{
		app: f.app, db: f.db, repo: f.ragRepo, svc: f.app.ragSvc, tenant: f.tenant,
		ownerID: memberID, memberID: memberID, collectionID: collection.ID,
	}
	evidenceID := df.stageEvidenceObject(t, "obj_thread_ev", memberID)
	if err := f.chatRepo.AddMessage(ctx, chatbot.Message{
		TenantID: f.tenant, ID: "conv_thread_asst", ConversationID: "conv_thread",
		Role: "assistant", Content: "Section nine changed [1].", OwnerActorID: memberID,
		RunID: "run_done", EvidenceIDs: []string{evidenceID},
	}); err != nil {
		t.Fatalf("seed assistant turn: %v", err)
	}
	// One real tool-step attempt for the assistant turn's collapsible block.
	if _, err := f.db.SQL.ExecContext(ctx,
		`INSERT INTO chatbot_run_attempts (tenant_id, run_id, number, kind, name, status, output, started_at, ended_at)
		 VALUES ($1::uuid, 'run_done', 1, 'tool', 'knowledge search', 'succeeded', '3 passages', now(), now())`,
		f.tenant); err != nil {
		t.Fatalf("seed run attempt: %v", err)
	}

	// A second open run: its user turn carries the pending marker.
	if err := f.runsRepo.CreateRun(ctx, chatbot.AssistantRun{
		TenantID: f.tenant, ID: "run_open", ChatbotID: bot.ID, ConversationID: "conv_thread",
		OwnerActorID: memberID, Status: jobs.StatusQueued, Input: "follow-up",
	}); err != nil {
		t.Fatalf("seed open run: %v", err)
	}
	if err := f.chatRepo.AddMessage(ctx, chatbot.Message{
		TenantID: f.tenant, ID: "conv_thread_pend", ConversationID: "conv_thread",
		Role: "user", Content: "follow-up", OwnerActorID: memberID, RunID: "run_open",
	}); err != nil {
		t.Fatalf("seed pending turn: %v", err)
	}

	rec := f.chatGet(t, sess, csrf, "/dashboard/chat/conv_thread")
	if rec.Code != http.StatusOK {
		t.Fatalf("thread page = %d body=%s", rec.Code, rec.Body.String())
	}
	body := rec.Body.String()

	if !strings.Contains(body, "Contract Reviewer") || !strings.Contains(body, "chat-model-1") ||
		!strings.Contains(body, "Tinfoil · enclave.example") {
		t.Fatalf("assistant header missing model/endpoint truth: %s", body)
	}
	if !strings.Contains(body, "Confidential · Tinfoil") {
		t.Fatalf("confidential chip missing: %s", body)
	}
	if !strings.Contains(body, `<details class="toolstep">`) {
		t.Fatalf("assistant turn missing toolstep details: %s", body)
	}
	if !strings.Contains(body, `data-run-id="run_open"`) || !strings.Contains(body, `data-run-status="queued"`) {
		t.Fatalf("pending run marker missing: %s", body)
	}
	if !strings.Contains(body, `data-evidence-ids="`+evidenceID+`"`) || !strings.Contains(body, "[1]") {
		t.Fatalf("citation marker/evidence ids missing: %s", body)
	}
	if !strings.Contains(body, `data-confirm="Stop run?"`) {
		t.Fatalf("stop trigger missing shared confirm: %s", body)
	}

	// A run without observed route metadata shows no chip — never inferred.
	f.seedConversation(t, "conv_plain", memberID, bot.ID, "plain question")
	if err := f.runsRepo.CreateRun(ctx, chatbot.AssistantRun{
		TenantID: f.tenant, ID: "run_plain", ChatbotID: bot.ID, ConversationID: "conv_plain",
		OwnerActorID: memberID, Status: jobs.StatusSucceeded, Input: "plain",
	}); err != nil {
		t.Fatalf("seed plain run: %v", err)
	}
	if err := f.chatRepo.AddMessage(ctx, chatbot.Message{
		TenantID: f.tenant, ID: "conv_plain_asst", ConversationID: "conv_plain",
		Role: "assistant", Content: "plain answer", OwnerActorID: memberID, RunID: "run_plain",
	}); err != nil {
		t.Fatalf("seed plain assistant turn: %v", err)
	}
	// Revoked/unknown evidence stays suppressed exactly as the service
	// dictates: the turn survives, its text never renders.
	if err := f.chatRepo.AddMessage(ctx, chatbot.Message{
		TenantID: f.tenant, ID: "conv_plain_redacted", ConversationID: "conv_plain",
		Role: "user", Content: "secret turn text", OwnerActorID: memberID,
		EvidenceIDs: []string{"ev_revoked_unknown"},
	}); err != nil {
		t.Fatalf("seed redacted turn: %v", err)
	}
	rec = f.chatGet(t, sess, csrf, "/dashboard/chat/conv_plain")
	if rec.Code != http.StatusOK {
		t.Fatalf("plain thread page = %d", rec.Code)
	}
	body = rec.Body.String()
	// No route truth → no header subline and no chip: the only conf-pop
	// triggers are the sidebar card and the layout's shared popover template.
	if strings.Contains(body, "chatAssistantSubline") || strings.Count(body, "conf-pop") != 2 {
		t.Fatalf("chip or route subline inferred without route metadata: %s", body)
	}
	if strings.Contains(body, "secret turn text") {
		t.Fatal("revoked-evidence turn text rendered — suppression broken")
	}
}

// TestDashboardChatTerminalLines covers the cancelled/unknown/failed tombstone
// lines — the thread handler resolves each run's live status server-side at
// render (an unknown job overlays its run row; a bare unknown run row renders
// its own line).
func TestDashboardChatTerminalLines(t *testing.T) {
	f := newChatFixture(t)
	ctx := context.Background()
	bot := f.seedAssistant(t)
	sess, csrf, memberID := f.memberSession(t, "terminal-member@example.com", bot.ID, "chat-model-1")

	seed := func(conv, runID, status string) {
		t.Helper()
		f.seedConversation(t, conv, memberID, bot.ID, "q")
		if err := f.runsRepo.CreateRun(ctx, chatbot.AssistantRun{
			TenantID: f.tenant, ID: runID, ChatbotID: bot.ID, ConversationID: conv,
			OwnerActorID: memberID, Status: status, Input: "q",
		}); err != nil {
			t.Fatalf("seed run %s: %v", runID, err)
		}
		if err := f.chatRepo.AddMessage(ctx, chatbot.Message{
			TenantID: f.tenant, ID: conv + "_asst", ConversationID: conv,
			Role: "assistant", OwnerActorID: memberID, RunID: runID,
		}); err != nil {
			t.Fatalf("seed tombstone turn %s: %v", runID, err)
		}
	}
	seed("conv_cancelled", "run_cancelled", jobs.StatusCancelled)
	seed("conv_failed", "run_failed", jobs.StatusFailed)
	seed("conv_unknown", "run_unknown", jobs.StatusUnknown)

	for _, tc := range []struct{ conv, want string }{
		{"conv_cancelled", "Stopped by you"},
		{"conv_failed", "This run failed."},
		{"conv_unknown", "outcome unknown"},
	} {
		rec := f.chatGet(t, sess, csrf, "/dashboard/chat/"+tc.conv)
		if rec.Code != http.StatusOK {
			t.Fatalf("%s page = %d", tc.conv, rec.Code)
		}
		if !strings.Contains(rec.Body.String(), tc.want) {
			t.Fatalf("%s missing terminal line %q: %s", tc.conv, tc.want, rec.Body.String())
		}
	}
}

// TestDashboardChatComposerStates covers plan Tests 4+5: ?assistant= preselects
// and ?evidence= preloads the composer; an ungranted assistant never appears;
// the grantless member sees the empty picker + grant-needed help + disabled
// Send, and the server still refuses their run (agent_run_denied).
func TestDashboardChatComposerStates(t *testing.T) {
	f := newChatFixture(t)
	bot := f.seedAssistant(t)
	hidden := f.seedAssistant(t)
	sess, csrf, _ := f.memberSession(t, "composer-member@example.com", bot.ID, "chat-model-1")

	rec := f.chatGet(t, sess, csrf, "/dashboard/chat?assistant="+url.QueryEscape(bot.ID)+"&evidence=ev_a%2Cev_b")
	if rec.Code != http.StatusOK {
		t.Fatalf("page = %d body=%s", rec.Code, rec.Body.String())
	}
	body := rec.Body.String()
	if !strings.Contains(body, `value="`+bot.ID+`" selected`) {
		t.Fatalf("assistant not preselected: %s", body)
	}
	if !strings.Contains(body, `data-evidence="ev_a,ev_b"`) {
		t.Fatalf("evidence ids not preloaded into composer: %s", body)
	}
	if strings.Contains(body, `value="`+hidden.ID+`"`) {
		t.Fatal("ungranted assistant rendered as an option")
	}

	// Grantless member (scopes only, stored grants deny): empty picker, help
	// text, ONE empty state over the thread and the composer-note in place
	// of the composer (UX-M04) — and the server still refuses their run.
	gsess, gcsrf, _ := f.memberSession(t, "grantless@example.com", "", "")
	rec = f.chatGet(t, gsess, gcsrf, "/dashboard/chat")
	if rec.Code != http.StatusOK {
		t.Fatalf("grantless page = %d body=%s", rec.Code, rec.Body.String())
	}
	body = rec.Body.String()
	if strings.Contains(body, "<option") {
		t.Fatal("grantless member saw assistant options")
	}
	if !strings.Contains(body, "ask an admin to grant access") {
		t.Fatalf("grant-needed help text missing: %s", body)
	}
	if strings.Contains(body, `data-action="chat:send"`) || !strings.Contains(body, "composer-note") {
		t.Fatalf("grantless member still saw the composer instead of the note: %s", body)
	}

	// Server re-check: an ungranted assistant id is agent_run_denied 403.
	sess2, csrf2, _ := f.memberSession(t, "composer-member-2@example.com", bot.ID, "chat-model-1")
	req := httptest.NewRequest(http.MethodPost, "/api/v1/agent-runs",
		strings.NewReader(`{"assistant_id":"`+hidden.ID+`","input":"hi"}`))
	req.Header.Set("Content-Type", "application/json")
	req.AddCookie(sess2)
	req.AddCookie(csrf2)
	req.Header.Set("X-CSRF-Token", csrf2.Value)
	rec = httptest.NewRecorder()
	f.app.Handler().ServeHTTP(rec, req)
	if rec.Code != http.StatusForbidden || !strings.Contains(rec.Body.String(), "agent_run_denied") {
		t.Fatalf("ungranted run = %d body=%s, want 403 agent_run_denied", rec.Code, rec.Body.String())
	}
}

// TestDashboardChatEvidencePartial covers plan Test 5b: current evidence shows
// collection · document · location · Current pill · quote · both links; a
// superseded row keeps metadata + Changed pill + links with the truthful
// passage-unavailable note; an unknown id renders the generic card; a session
// without rag gets the empty 403.
func TestDashboardChatEvidencePartial(t *testing.T) {
	f := newChatFixture(t)
	ctx := context.Background()
	ownerSess, ownerCSRF := loginUser(t, f.identityFixture, "chat-ev-owner@example.com", []string{"chatbot", "rag"}, "chat-pass")
	var ownerID string
	if err := f.db.SQL.QueryRowContext(ctx,
		`SELECT id::text FROM users WHERE tenant_id = $1::uuid AND email = $2`,
		f.tenant, "chat-ev-owner@example.com").Scan(&ownerID); err != nil {
		t.Fatalf("read owner id: %v", err)
	}
	collection, err := f.app.ragSvc.CreateCollection(ctx, auth.Principal{
		TenantID: f.tenant, ActorID: ownerID, UserID: ownerID, ActorKind: auth.ActorKindUser,
	}, rag.CreateCollectionInput{Name: "chat-docs", ACL: map[string]any{"public": true}})
	if err != nil {
		t.Fatalf("seed collection: %v", err)
	}
	// Reuse the shared 08-03 staging recipe: a real published generation with
	// a span-backed chunk and an evidence row the partial reads.
	df := ragDocsFixture{
		app: f.app, db: f.db, repo: f.ragRepo, svc: f.app.ragSvc, tenant: f.tenant,
		ownerID: ownerID, memberID: ownerID, collectionID: collection.ID,
	}
	objectID := "obj_chat_ev"
	evidenceID := df.stageEvidenceObject(t, objectID, ownerID)
	// A native provenance URL so the card carries "Open in source".
	if _, err := f.db.SQL.ExecContext(ctx,
		`UPDATE source_object_versions SET provenance = $3
		 WHERE tenant_id = $1::uuid AND object_id = $2`,
		f.tenant, objectID, `{"url":"https://docs.example/contract.pdf"}`); err != nil {
		t.Fatalf("set provenance: %v", err)
	}

	rec := f.chatGet(t, ownerSess, ownerCSRF, "/dashboard/partials/evidence/"+evidenceID)
	if rec.Code != http.StatusOK {
		t.Fatalf("evidence partial = %d body=%s", rec.Code, rec.Body.String())
	}
	body := rec.Body.String()
	for _, want := range []string{
		"the retained passage text", "Current", "chat-docs", objectID + ".txt",
		"Open in source", "View retained copy", "Page 7",
	} {
		if !strings.Contains(body, want) {
			t.Fatalf("evidence card missing %q: %s", want, body)
		}
	}

	// Supersede the cited generation: metadata survives, passage 404s.
	if err := f.ragRepo.MarkGenerationState(ctx, f.tenant, collection.ID, objectID+"#1", 1, "superseded"); err != nil {
		t.Fatalf("supersede generation: %v", err)
	}
	rec = f.chatGet(t, ownerSess, ownerCSRF, "/dashboard/partials/evidence/"+evidenceID)
	if rec.Code != http.StatusOK {
		t.Fatalf("superseded partial = %d body=%s", rec.Code, rec.Body.String())
	}
	body = rec.Body.String()
	for _, want := range []string{"Changed", objectID + ".txt", "Open in source", "View retained copy", "passage is unavailable"} {
		if !strings.Contains(body, want) {
			t.Fatalf("superseded card missing %q: %s", want, body)
		}
	}
	if strings.Contains(body, "the retained passage text") {
		t.Fatal("superseded card fabricated quote text")
	}

	// Unknown (and by the uniform rule forbidden/revoked/tombstoned) renders
	// the generic no-metadata card.
	rec = f.chatGet(t, ownerSess, ownerCSRF, "/dashboard/partials/evidence/ev_unknown")
	if rec.Code != http.StatusOK {
		t.Fatalf("unknown partial = %d body=%s", rec.Code, rec.Body.String())
	}
	body = rec.Body.String()
	if !strings.Contains(body, "no longer available") {
		t.Fatalf("generic card missing: %s", body)
	}
	if strings.Contains(body, objectID) {
		t.Fatalf("generic card leaked metadata: %s", body)
	}

	// No rag capability: empty-body 403.
	chatOnly, chatOnlyCSRF := loginUser(t, f.identityFixture, "chat-only@example.com", []string{"chatbot"}, "chat-pass")
	rec = f.chatGet(t, chatOnly, chatOnlyCSRF, "/dashboard/partials/evidence/"+evidenceID)
	if rec.Code != http.StatusForbidden || strings.TrimSpace(rec.Body.String()) != "" {
		t.Fatalf("no-rag partial = %d body=%q, want empty 403", rec.Code, rec.Body.String())
	}
}

// TestDashboardChatThreadFragmentIsolation keeps the ?c= fragment behind the
// same owner-or-404 rule as the deep link.
func TestDashboardChatThreadFragmentIsolation(t *testing.T) {
	f := newChatFixture(t)
	bot := f.seedAssistant(t)
	sess, csrf, memberID := f.memberSession(t, "frag-member@example.com", bot.ID, "chat-model-1")
	f.seedConversation(t, "conv_frag", memberID, bot.ID, "fragment question")
	f.seedConversation(t, "conv_frag_other", "someone_else", bot.ID, "foreign question")

	rec := f.chatGet(t, sess, csrf, "/dashboard/partials/chat/thread?c=conv_frag")
	if rec.Code != http.StatusOK || !strings.Contains(rec.Body.String(), "fragment question") {
		t.Fatalf("own thread fragment = %d body=%s", rec.Code, rec.Body.String())
	}
	rec = f.chatGet(t, sess, csrf, "/dashboard/partials/chat/thread?c=conv_frag_other")
	if rec.Code != http.StatusNotFound {
		t.Fatalf("foreign thread fragment = %d, want 404", rec.Code)
	}
	rec = f.chatGet(t, sess, csrf, "/dashboard/partials/chat/thread")
	if rec.Code != http.StatusOK || !strings.Contains(rec.Body.String(), "No conversation open yet") {
		t.Fatalf("empty thread fragment = %d body=%s", rec.Code, rec.Body.String())
	}
}

// TestDashboardChatCursorPagination covers the history fragment's keyset
// "Show earlier" link carrying the opaque cursor.
func TestDashboardChatCursorPagination(t *testing.T) {
	f := newChatFixture(t)
	bot := f.seedAssistant(t)
	sess, csrf, memberID := f.memberSession(t, "cursor-member@example.com", bot.ID, "chat-model-1")
	// The service pages at 20; seed 21 so the first page carries a cursor.
	const total = 21
	for i := range total {
		f.seedConversation(t, "conv_c"+string(rune('a'+i)), memberID, bot.ID, "question number "+string(rune('a'+i)))
	}
	rec := f.chatGet(t, sess, csrf, "/dashboard/partials/chat/history")
	if rec.Code != http.StatusOK {
		t.Fatalf("history fragment = %d", rec.Code)
	}
	body := rec.Body.String()
	if !strings.Contains(body, "Show earlier") {
		t.Fatalf("first page should offer Show earlier: %s", body)
	}
	if strings.Contains(body, "question number a") {
		t.Fatalf("first page leaked beyond the page boundary: %s", body)
	}
	start := strings.Index(body, "cursor=")
	if start == -1 {
		t.Fatal("cursor link missing")
	}
	raw := body[start+len("cursor="):]
	if end := strings.IndexAny(raw, `"'`); end >= 0 {
		raw = raw[:end]
	}
	cursor, err := url.QueryUnescape(raw)
	if err != nil {
		t.Fatalf("cursor decode: %v", err)
	}
	rec = f.chatGet(t, sess, csrf, "/dashboard/partials/chat/history?cursor="+url.QueryEscape(cursor))
	if rec.Code != http.StatusOK || !strings.Contains(rec.Body.String(), "question number a") {
		t.Fatalf("cursor page = %d body=%s", rec.Code, rec.Body.String())
	}
	// Malformed cursors render an empty page, never a crash or rows.
	rec = f.chatGet(t, sess, csrf, "/dashboard/partials/chat/history?cursor=not-a-cursor")
	if rec.Code != http.StatusOK || strings.Contains(rec.Body.String(), "conv_c") {
		t.Fatalf("malformed cursor must render an empty page, got %d body=%s", rec.Code, rec.Body.String())
	}
}

// TestDashboardChatConversationSelectsItsAssistant proves AST-I02: a
// deep-linked conversation renders the picker on ITS OWN assistant, not the
// list's first row — chat.js then posts the thread's real assistant id on
// follow-ups.
func TestDashboardChatConversationSelectsItsAssistant(t *testing.T) {
	f := newChatFixture(t)
	bot := f.seedAssistant(t)
	other, err := f.chatSvc.Create(context.Background(), f.adminAuth, chatbot.CreateChatbotInput{
		Name: "Second Reviewer", Model: "chat-model-1", Audience: []string{"*"},
	})
	if err != nil {
		t.Fatalf("seed second assistant: %v", err)
	}
	sess, csrf := loginUserWithGrants(t, f.identityFixture, "select-member@example.com",
		[]string{"chatbot", "rag"},
		auth.ResourceGrants{
			Models:     auth.Allowlist{Items: []string{"chat-model-1"}},
			Assistants: auth.Allowlist{Items: []string{bot.ID, other.ID}},
		}, "chat-pass")
	var memberID string
	if err := f.db.SQL.QueryRowContext(context.Background(),
		`SELECT id::text FROM users WHERE tenant_id = $1::uuid AND email = $2`,
		f.tenant, "select-member@example.com").Scan(&memberID); err != nil {
		t.Fatalf("read member id: %v", err)
	}
	f.seedConversation(t, "conv_second", memberID, other.ID, "follow-up here")

	rec := f.chatGet(t, sess, csrf, "/dashboard/chat/conv_second")
	if rec.Code != http.StatusOK {
		t.Fatalf("conversation page = %d body=%s", rec.Code, rec.Body.String())
	}
	body := rec.Body.String()
	// bot sorts first in the picker but conv_second's assistant must be the
	// selected option and the composer's pinned data-assistant.
	if !strings.Contains(body, `<option value="`+other.ID+`" selected>`) {
		t.Fatalf("conversation's assistant not selected: %s", body)
	}
	if strings.Contains(body, `<option value="`+bot.ID+`" selected>`) {
		t.Fatal("the first assistant in the list was selected instead")
	}
	if !strings.Contains(body, `data-assistant="`+other.ID+`"`) {
		t.Fatalf("composer missing pinned data-assistant: %s", body)
	}
}
