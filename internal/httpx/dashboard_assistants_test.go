// dashboard_assistants_test.go — Assistants + Instructions render coverage
// (OA-24, plan 09-03): per-role gates on the two pages and all four
// partials, truthful table columns incl. the REAL per-assistant conversation
// count (nonzero on a seeded fixture), the drawer's priced/routable model
// select and pending-tool state, and the instructions editor's labeled
// variable rows (never a JSON field).
//
// Obsolete-surface handoff: the old chatbots/prompt-templates family
// (/dashboard/chatbots, /dashboard/prompt-templates, /dashboard/partials/
// chatbots, /dashboard/partials/prompt-templates, handleDashboardPageChatbots,
// handleDashboardPagePromptTemplates, handleDashboardChatbots,
// handleDashboardPromptTemplates, handleDashboardChatbotAction,
// handleDashboardPromptTemplateAction, partial_chatbots.tmpl, page_agents.tmpl,
// partial_prompt_templates.tmpl, agents.js) is owned by plan 10-01 — the old
// surfaces stay live until phase-10's Work cutover, NOT 09-10.
package httpx

import (
	"context"
	"encoding/json"
	"net/http"
	"strings"
	"testing"
	"time"

	"enclava-go/internal/auth"
	"enclava-go/internal/billing"
	"enclava-go/internal/chatbot"
	"enclava-go/internal/inference"
	"enclava-go/internal/jobs"
	"enclava-go/internal/prompts"
	"enclava-go/internal/rag"
	"enclava-go/internal/source"
	"enclava-go/internal/store/postgres"
	"enclava-go/internal/testdb"
)

type assistantsFixture struct {
	app       *App
	db        *postgres.DB
	repo      *postgres.IdentityRepo
	tenant    string
	chatRepo  *postgres.ChatbotRepo
	prompts   *postgres.PromptsRepo
	brepo     *postgres.BillingRepo
	endpoints *postgres.EndpointsRepo
	mcpRepo   *postgres.MCPToolsRepo
	ragSvc    *rag.Service
	ctx       context.Context
}

// newAssistantsFixture mirrors newIdentityFixture but wires the real
// executor (the drawer's priced-model select) and the MCP repo (the tools
// checklist) over an isolated schema.
func newAssistantsFixture(t *testing.T) *assistantsFixture {
	t.Helper()
	db := testdb.New(t)
	tenant := testdb.SeedTenant(t, db, "assistants-dash")
	repo := postgres.NewIdentityRepo(db)
	authSvc := auth.NewService(repo, "")
	sessionAuth := NewSessionAuth(authSvc, auth.NewOIDCService(repo, nil), time.Hour, DashboardAuthTemplates(), "assistants-dash")
	jobsSvc := jobs.NewService(postgres.NewJobsRepo(db), 0)
	sourceRepo := postgres.NewSourceRepo(db)
	ragSvc := rag.NewService(postgres.NewRAGRepo(db), nil, nil, "", nil, jobsSvc)
	chatRepo := postgres.NewChatbotRepo(db)
	promptsRepo := postgres.NewPromptsRepo(db)
	chatSvc := chatbot.NewService(chatRepo, promptsRepo, ragSvc, nil)
	promptSvc := prompts.NewService(promptsRepo, chatSvc)
	brepo := postgres.NewBillingRepo(db)
	endpoints := postgres.NewEndpointsRepo(db)
	mcpRepo := postgres.NewMCPToolsRepo(db)
	executor, err := inference.NewService(inference.Deps{
		Endpoints:  endpoints,
		Sources:    sourceRepo,
		Authorizer: source.NewAuthorizer(sourceRepo, time.Hour),
		Billing:    billing.NewService(brepo),
	})
	if err != nil {
		t.Fatalf("inference service: %v", err)
	}
	artifactRepo := postgres.NewArtifactRepo(db)
	app := NewApp(authSvc, nil, "", nil, repo, nil, ragSvc, promptSvc, chatSvc, nil, nil, brepo, endpoints,
		"sha256", "", "enc_", nil, sessionAuth, 1, false, executor, nil, GateConfig{}, sourceRepo, artifactRepo)
	app.ConfigureMCP(mcpRepo, nil)
	return &assistantsFixture{
		app: app, db: db, repo: repo, tenant: tenant, chatRepo: chatRepo,
		prompts: promptsRepo, brepo: brepo, endpoints: endpoints,
		mcpRepo: mcpRepo, ragSvc: ragSvc, ctx: context.Background(),
	}
}

// assistantsLogin seeds a user (with optional stored grants), sets a known
// password and returns the session + CSRF cookies.
func assistantsLogin(t *testing.T, f *assistantsFixture, email, password string, scopes []string, grants auth.ResourceGrants) (*http.Cookie, *http.Cookie) {
	t.Helper()
	user := testdb.SeedUserWithGrants(t, f.db, f.tenant, email, scopes, grants)
	setPassword(t, f.repo, f.tenant, user.ID, password)
	return login(t, f.app, email, password)
}

func assistantsUserID(t *testing.T, f *assistantsFixture, email string) string {
	t.Helper()
	var id string
	if err := f.db.SQL.QueryRowContext(f.ctx,
		`SELECT id::text FROM users WHERE tenant_id = $1::uuid AND email = $2`,
		f.tenant, email).Scan(&id); err != nil {
		t.Fatalf("read user id for %s: %v", email, err)
	}
	return id
}

var assistantsManageGrants = auth.ResourceGrants{
	Assistants: auth.Allowlist{All: true},
	Models:     auth.Allowlist{All: true},
	Endpoints:  auth.Allowlist{All: true},
}

// assistantsRoutes enumerates the six routes this plan registers: two pages
// (rendered 403) and four partials (empty 403).
func assistantsRoutes() []string {
	return []string{
		"/dashboard/assistants",
		"/dashboard/assistants/instructions",
		"/dashboard/partials/assistants/table",
		"/dashboard/partials/assistants/drawer",
		"/dashboard/partials/assistants/instructions",
		"/dashboard/partials/assistants/instructions/editor",
	}
}

func TestDashboardAssistantsMemberDenied(t *testing.T) {
	f := newAssistantsFixture(t)
	sess, _ := assistantsLogin(t, f, "member@assistants.test", "member-pass", []string{"chat"}, auth.ResourceGrants{})
	for _, path := range assistantsRoutes() {
		rec := sessionDo(t, f.app, http.MethodGet, path, "", sess, nil)
		if rec.Code != http.StatusForbidden {
			t.Fatalf("GET %s as member: expected 403, got %d", path, rec.Code)
		}
		if strings.HasPrefix(path, "/dashboard/partials/") {
			if rec.Body.Len() != 0 {
				t.Fatalf("partial %s must 403 with an empty body, got %q", path, rec.Body.String())
			}
			continue
		}
		if !strings.Contains(rec.Body.String(), "don't have access") {
			t.Fatalf("page %s must render the designed 403 page, got %q", path, rec.Body.String())
		}
	}
}

// seedAssistantsTenant wires the shared fixture rows under the given owner:
// a confidential endpoint (tinfoil → chip applies), a priced route for
// gpt-fast plus an unpriced decoy alias, an approved and a pending MCP tool,
// a readable collection and two assistants, one with conversations.
func seedAssistantsTenant(t *testing.T, f *assistantsFixture, owner string) {
	t.Helper()
	if err := f.endpoints.Create(f.ctx, inference.Endpoint{
		ID: "ep-1", TenantID: f.tenant, Protocol: "tinfoil",
		BaseOrigin:   "https://api.upstream.example",
		Capabilities: []string{inference.CapabilityChat, "model:gpt-fast=gpt-4o-mini"},
		Enabled:      true,
	}); err != nil {
		t.Fatalf("create ep-1: %v", err)
	}
	if err := f.endpoints.Create(f.ctx, inference.Endpoint{
		ID: "ep-2", TenantID: f.tenant, Protocol: "openai_compatible",
		BaseOrigin:   "https://api.upstream.example",
		Capabilities: []string{inference.CapabilityChat, "model:gpt-unpriced=gpt-4o-mini"},
		Enabled:      true,
	}); err != nil {
		t.Fatalf("create ep-2: %v", err)
	}
	price := func(unit billing.UnitKind, micros int64) {
		t.Helper()
		if err := f.brepo.PutPrice(f.ctx, f.tenant,
			"price-ep1-chat-"+string(unit), billing.Price{
				SubjectType: billing.PriceSubjectEndpoint, SubjectID: "ep-1",
				Capability: inference.CapabilityChat, Unit: unit,
				RateMicros: micros, UnitQuantity: 1, Currency: "USD",
			}, "tester", time.Now().UTC().Add(-time.Minute)); err != nil {
			t.Fatalf("put price %s: %v", unit, err)
		}
	}
	price(billing.UnitInput, 1)
	price(billing.UnitCachedInput, 1)
	price(billing.UnitOutput, 2)

	if err := f.mcpRepo.PutServer(f.ctx, postgres.MCPServer{
		TenantID: f.tenant, ID: "srv", Origin: "https://mcp.example.com",
		EndpointPath: "/mcp", AudienceScopes: []string{"assistant.use"}, Enabled: true,
	}); err != nil {
		t.Fatalf("put mcp server: %v", err)
	}
	if err := f.mcpRepo.RecordDiscovery(f.ctx, f.tenant, "srv", []postgres.DiscoveredToolSnapshot{
		{Name: "web_lookup", InputSchema: json.RawMessage(`{"type":"object"}`), OutputSchema: json.RawMessage(`{"type":"object"}`)},
		{Name: "pending_tool", InputSchema: json.RawMessage(`{"type":"object"}`), OutputSchema: json.RawMessage(`{"type":"object"}`)},
	}); err != nil {
		t.Fatalf("record discovery: %v", err)
	}
	if _, err := f.mcpRepo.ApproveDiscovery(f.ctx, f.tenant, "srv", "web_lookup", 1, "web_lookup", []string{"assistant.use"}); err != nil {
		t.Fatalf("approve discovery: %v", err)
	}

	ownerP := auth.Principal{TenantID: f.tenant, ActorID: owner, ActorKind: auth.ActorKindUser, UserID: owner}
	collection, err := f.ragSvc.CreateCollection(f.ctx, ownerP, rag.CreateCollectionInput{Name: "Contracts"})
	if err != nil {
		t.Fatalf("create collection: %v", err)
	}

	if err := f.chatRepo.Create(f.ctx, chatbot.Instance{
		TenantID: f.tenant, ID: "bot_review", ExternalID: "bot_review_ext",
		Name: "Contract Reviewer", Description: "Reviews supplier contracts",
		ProviderID:       "ep-1",
		Model:            "gpt-fast",
		RAGCollectionIDs: []string{collection.ID},
		Settings:         map[string]any{"tools": []any{"knowledge.search", "web_lookup"}},
		Audience:         []string{"*"},
		IsEnabled:        true,
		OwnerActorID:     owner,
	}); err != nil {
		t.Fatalf("seed bot_review: %v", err)
	}
	if err := f.chatRepo.Create(f.ctx, chatbot.Instance{
		TenantID: f.tenant, ID: "bot_idle", ExternalID: "bot_idle_ext",
		Name: "Idle Bot", Model: "gpt-fast", IsEnabled: false,
		Audience: []string{"*"}, OwnerActorID: owner,
	}); err != nil {
		t.Fatalf("seed bot_idle: %v", err)
	}
	for i := range 2 {
		if err := f.chatRepo.CreateConversation(f.ctx, chatbot.Conversation{
			TenantID: f.tenant, ID: "conv_review_" + string(rune('a'+i)),
			ChatbotID: "bot_review", OwnerActorID: owner,
		}); err != nil {
			t.Fatalf("seed conversation: %v", err)
		}
	}
}

func TestDashboardAssistantsTableColumns(t *testing.T) {
	f := newAssistantsFixture(t)
	sess, _ := assistantsLogin(t, f, "manager@assistants.test", "manager-pass",
		[]string{"chatbot", "chatbot.manage"}, assistantsManageGrants)
	seedAssistantsTenant(t, f, assistantsUserID(t, f, "manager@assistants.test"))
	rec := sessionDo(t, f.app, http.MethodGet, "/dashboard/assistants", "", sess, nil)
	if rec.Code != http.StatusOK {
		t.Fatalf("assistants page: expected 200, got %d body=%s", rec.Code, rec.Body.String())
	}
	body := rec.Body.String()
	// Real per-assistant conversation counts: two for the busy assistant,
	// zero for the idle one — the column is never omitted.
	if !strings.Contains(body, `<td data-label="Conversations">2</td>`) {
		t.Fatalf("expected a nonzero Conversations count for the seeded assistant, got %s", body)
	}
	if !strings.Contains(body, `<td data-label="Conversations">0</td>`) {
		t.Fatalf("expected the zero count to render, never an omitted column: %s", body)
	}
	// Assistant identity, model and the confidential chip from observed
	// assurance (tinfoil endpoint, unobserved this boot → unverified warn).
	if !strings.Contains(body, "Contract Reviewer") {
		t.Fatalf("expected the seeded assistant name, got %s", body)
	}
	if !strings.Contains(body, "gpt-fast") {
		t.Fatalf("expected the assistant's model, got %s", body)
	}
	if !strings.Contains(body, "Confidential ·") {
		t.Fatalf("expected the confidential chip on the tinfoil endpoint, got %s", body)
	}
	// Knowledge resolved to the collection name; stored tools by label/name.
	if !strings.Contains(body, "Contracts") {
		t.Fatalf("expected the resolved collection name, got %s", body)
	}
	if !strings.Contains(body, "Knowledge search") || !strings.Contains(body, "web_lookup") {
		t.Fatalf("expected the stored tools rendered, got %s", body)
	}
	// Audience rendered in plain words (glossary): the wildcard audience is
	// "Everyone", never the raw scope sentinel.
	if !strings.Contains(body, `<td data-label="Audience">Everyone</td>`) {
		t.Fatalf("expected the wildcard audience in plain words, got %s", body)
	}
	if !strings.Contains(body, "Ready") || !strings.Contains(body, "Disabled") {
		t.Fatalf("expected status pills for enabled and disabled assistants, got %s", body)
	}
}

func TestDashboardAssistantsDrawerSelects(t *testing.T) {
	f := newAssistantsFixture(t)
	sess, _ := assistantsLogin(t, f, "manager@assistants.test", "manager-pass",
		[]string{"chatbot", "chatbot.manage"}, assistantsManageGrants)
	seedAssistantsTenant(t, f, assistantsUserID(t, f, "manager@assistants.test"))
	rec := sessionDo(t, f.app, http.MethodGet, "/dashboard/partials/assistants/drawer?id=bot_review", "", sess, nil)
	if rec.Code != http.StatusOK {
		t.Fatalf("drawer: expected 200, got %d body=%s", rec.Code, rec.Body.String())
	}
	body := rec.Body.String()
	// Only priced+routable models are offered: gpt-fast (priced on ep-1) in,
	// gpt-unpriced (unpriced ep-2 route) absent.
	if !strings.Contains(body, `<option value="gpt-fast"`) {
		t.Fatalf("expected the priced model option, got %s", body)
	}
	if strings.Contains(body, "gpt-unpriced") {
		t.Fatalf("unpriced model must be absent from the select, got %s", body)
	}
	// The approved MCP tool is checkable; the pending one is a disabled
	// checkbox with the awaiting-approval label.
	if !strings.Contains(body, `value="web_lookup"`) {
		t.Fatalf("expected the approved tool checkbox, got %s", body)
	}
	if !strings.Contains(body, `name="tools" value="" disabled`) || !strings.Contains(body, "pending_tool") {
		t.Fatalf("expected the pending tool rendered as a disabled checkbox, got %s", body)
	}
	if !strings.Contains(body, "Awaiting approval") {
		t.Fatalf("expected the pending tool label, got %s", body)
	}
	// The drawer carries the audience and enabled controls the save submits.
	if !strings.Contains(body, `name="audience-mode"`) || !strings.Contains(body, `name="is_enabled"`) {
		t.Fatalf("expected audience and enabled controls, got %s", body)
	}
}

func TestDashboardInstructionsEditor(t *testing.T) {
	f := newAssistantsFixture(t)
	sess, _ := assistantsLogin(t, f, "manager@assistants.test", "manager-pass",
		[]string{"chatbot", "chatbot.manage"}, assistantsManageGrants)
	if err := f.prompts.Create(f.ctx, prompts.Template{
		TenantID:     f.tenant,
		ID:           "prompt_review",
		Name:         "Contract review",
		Description:  "Reviews contracts against a checklist",
		SystemPrompt: "You are a contract reviewer. Due date: {{.due_date}}",
		UserPrompt:   "{{.user_input}}",
		VariablesSchema: map[string]any{
			"type": "object",
			"properties": map[string]any{
				"due_date": map[string]any{"type": "string", "description": "When the contract is due"},
			},
			"required": []any{"due_date"},
		},
		IsDefault:    true,
		IsActive:     true,
		OwnerActorID: "user_seed",
	}); err != nil {
		t.Fatalf("seed template: %v", err)
	}

	rec := sessionDo(t, f.app, http.MethodGet, "/dashboard/partials/assistants/instructions/editor?id=prompt_review", "", sess, nil)
	if rec.Code != http.StatusOK {
		t.Fatalf("editor: expected 200, got %d body=%s", rec.Code, rec.Body.String())
	}
	body := rec.Body.String()
	// Labeled variable rows carry the parsed schema — no raw JSON field.
	if !strings.Contains(body, `value="due_date"`) || !strings.Contains(body, `data-var-required`) {
		t.Fatalf("expected labeled variable rows, got %s", body)
	}
	if !strings.Contains(body, `value="When the contract is due"`) {
		t.Fatalf("expected the variable description row, got %s", body)
	}
	if strings.Contains(body, "variables_schema") {
		t.Fatalf("editor must never render a raw variables_schema field, got %s", body)
	}
	if !strings.Contains(body, `name="is_default"`) {
		t.Fatalf("expected the default flag, got %s", body)
	}

	// Create variant renders the empty editor (?new=1, no id).
	rec = sessionDo(t, f.app, http.MethodGet, "/dashboard/partials/assistants/instructions/editor?new=1", "", sess, nil)
	if rec.Code != http.StatusOK {
		t.Fatalf("new editor: expected 200, got %d", rec.Code)
	}
	if !strings.Contains(rec.Body.String(), `data-var-template`) {
		t.Fatalf("expected the add-variable row template, got %s", rec.Body.String())
	}

	// The instructions list renders the seeded template with its editor link.
	rec = sessionDo(t, f.app, http.MethodGet, "/dashboard/partials/assistants/instructions", "", sess, nil)
	if rec.Code != http.StatusOK {
		t.Fatalf("instructions list: expected 200, got %d", rec.Code)
	}
	listBody := rec.Body.String()
	if !strings.Contains(listBody, "Contract review") || !strings.Contains(listBody, "/dashboard/partials/assistants/instructions/editor?id=prompt_review") {
		t.Fatalf("expected the seeded template row with its editor link, got %s", listBody)
	}
}
