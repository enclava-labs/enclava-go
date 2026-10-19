package chatbot_test

import (
	"context"
	"database/sql"
	"encoding/json"
	"errors"
	"fmt"
	"io"
	"strings"
	"testing"

	"enclava-go/internal/auth"
	"enclava-go/internal/chatbot"
	"enclava-go/internal/llm"
	"enclava-go/internal/prompts"
	"enclava-go/internal/rag"
	"enclava-go/internal/source"
	"enclava-go/internal/store/postgres"
	"enclava-go/internal/testdb"
)

type fakeChatProvider struct {
	lastRequest []byte
	err         error
}

func (f *fakeChatProvider) ListModels(ctx context.Context) (llm.ModelsResponse, error) {
	return llm.ModelsResponse{Object: "list", Data: []llm.Model{{ID: "chat-test-model"}}}, nil
}

func (f *fakeChatProvider) GetModel(ctx context.Context, modelID string) (llm.Model, error) {
	return llm.Model{ID: modelID, Object: "model"}, nil
}

func (f *fakeChatProvider) CreateChatCompletion(ctx context.Context, body []byte) ([]byte, error) {
	f.lastRequest = append([]byte(nil), body...)
	if f.err != nil {
		return nil, f.err
	}
	return []byte(`{"id":"chatcmpl_test","object":"chat.completion","model":"chat-test-model","choices":[{"index":0,"message":{"role":"assistant","content":"RAG answer"},"finish_reason":"stop"}],"usage":{"prompt_tokens":7,"completion_tokens":3,"total_tokens":10}}`), nil
}

func (f *fakeChatProvider) CreateChatCompletionStream(ctx context.Context, body []byte) (io.ReadCloser, error) {
	return io.NopCloser(strings.NewReader("")), nil
}

func (f *fakeChatProvider) CreateEmbeddings(ctx context.Context, body []byte) ([]byte, error) {
	return nil, nil
}

type fakeRAGSearcher struct {
	inputs     []rag.SearchInput
	principals []auth.Principal
	err        error
}

func (f *fakeRAGSearcher) Search(ctx context.Context, p auth.Principal, input rag.SearchInput) ([]rag.SearchResult, error) {
	f.inputs = append(f.inputs, input)
	f.principals = append(f.principals, p)
	if f.err != nil {
		return nil, f.err
	}
	return []rag.SearchResult{
		{
			DocumentID: "doc_1",
			ChunkID:    "chunk_1",
			Score:      0.98,
			Content:    "The phrase maple-vector-citadel identifies the launch note.",
			FileName:   "launch.txt",
		},
	}, nil
}

type fakeUsageRecorder struct {
	records []chatbot.UsageRecord
}

func (f *fakeUsageRecorder) Record(ctx context.Context, rec chatbot.UsageRecord) error {
	f.records = append(f.records, rec)
	return nil
}

type chatFixture struct {
	svc        *chatbot.Service
	chatRepo   *postgres.ChatbotRepo
	promptRepo *postgres.PromptsRepo
	provider   *fakeChatProvider
	rag        *fakeRAGSearcher
	usage      *fakeUsageRecorder
	tenantA    string
	tenantB    string
}

func newChatFixture(t *testing.T, searcher *fakeRAGSearcher, provider *fakeChatProvider) *chatFixture {
	t.Helper()
	db := testdb.New(t)
	tenantA := testdb.SeedTenant(t, db, "chat-a")
	tenantB := testdb.SeedTenant(t, db, "chat-b")
	chatRepo := postgres.NewChatbotRepo(db)
	promptRepo := postgres.NewPromptsRepo(db)
	if provider == nil {
		provider = &fakeChatProvider{}
	}
	usage := &fakeUsageRecorder{}
	svc := chatbot.NewService(chatRepo, promptRepo, searcher, provider, usage)
	return &chatFixture{
		svc:        svc,
		chatRepo:   chatRepo,
		promptRepo: promptRepo,
		provider:   provider,
		rag:        searcher,
		usage:      usage,
		tenantA:    tenantA,
		tenantB:    tenantB,
	}
}

func openPrincipal(tenantID string) auth.Principal {
	return auth.Principal{
		TenantID:           tenantID,
		ActorID:            "key_actor_1",
		ActorKind:          auth.ActorKindServiceAccount,
		APIKeyID:           7,
		AllowedModelsAll:   true,
		AllowedChatbotsAll: true,
		AllowedAgentsAll:   true,
	}
}

func adminPrincipal(tenantID string) auth.Principal {
	p := openPrincipal(tenantID)
	p.ActorID = "admin_actor"
	p.Scopes = map[string]struct{}{"platform.admin": {}}
	return p
}

func TestCreateChatCompletionInjectsRAGAndPersistsConversation(t *testing.T) {
	ctx := context.Background()
	searcher := &fakeRAGSearcher{}
	f := newChatFixture(t, searcher, nil)
	p := openPrincipal(f.tenantA)

	err := f.promptRepo.Create(ctx, prompts.Template{
		TenantID:     f.tenantA,
		ID:           "test-chatbot-template",
		Name:         "Test Chatbot",
		SystemPrompt: "Use this context for {{.audience}}:\n{{.rag_context}}",
		UserPrompt:   "Question: {{.user_input}}\nContext: {{.rag_context}}",
		IsActive:     true,
	})
	if err != nil {
		t.Fatalf("create prompt template: %v", err)
	}

	bot, err := f.svc.Create(ctx, p, chatbot.CreateChatbotInput{
		Name:             "Support Bot",
		PromptTemplateID: "test-chatbot-template",
		Model:            "chat-test-model",
		ExternalID:       "support_bot",
		RAGCollectionIDs: []string{"ragcol_test"},
		Settings: map[string]any{
			"rag_limit": 1,
		},
	})
	if err != nil {
		t.Fatalf("create chatbot: %v", err)
	}

	out, err := f.svc.CreateChatCompletion(ctx, p, "support_bot", []byte(`{
		"model": "client-model-ignored",
		"messages": [{"role":"user","content":"What does the launch note say?"}],
		"metadata": {"variables": {"audience": "operators"}}
	}`))
	if err != nil {
		t.Fatalf("create chatbot completion: %v", err)
	}
	if out.Chatbot.ID != bot.ID {
		t.Fatalf("unexpected chatbot id: %s", out.Chatbot.ID)
	}
	if out.Conversation.ID == "" {
		t.Fatalf("expected conversation id")
	}
	if len(searcher.inputs) != 1 || searcher.inputs[0].CollectionID != "ragcol_test" {
		t.Fatalf("unexpected RAG searches: %#v", searcher.inputs)
	}
	if len(searcher.principals) != 1 || searcher.principals[0].ActorID != p.ActorID {
		t.Fatalf("RAG search must carry the request principal, got %#v", searcher.principals)
	}
	if auth.HasScope(searcher.principals[0], "platform.admin") {
		t.Fatal("RAG search must not fabricate platform.admin")
	}

	var upstream struct {
		Model    string `json:"model"`
		Messages []struct {
			Role    string `json:"role"`
			Content string `json:"content"`
		} `json:"messages"`
	}
	if err := json.Unmarshal(f.provider.lastRequest, &upstream); err != nil {
		t.Fatalf("decode upstream request: %v", err)
	}
	if upstream.Model != "chat-test-model" {
		t.Fatalf("expected chatbot model override, got %q", upstream.Model)
	}
	if len(upstream.Messages) < 2 || upstream.Messages[0].Role != "system" {
		t.Fatalf("expected injected system prompt, got %#v", upstream.Messages)
	}
	if !strings.Contains(upstream.Messages[0].Content, "maple-vector-citadel") || !strings.Contains(upstream.Messages[0].Content, "operators") {
		t.Fatalf("system prompt missing context/variables: %q", upstream.Messages[0].Content)
	}
	if !strings.Contains(upstream.Messages[len(upstream.Messages)-1].Content, "maple-vector-citadel") {
		t.Fatalf("user prompt missing RAG context: %q", upstream.Messages[len(upstream.Messages)-1].Content)
	}

	var response map[string]any
	if err := json.Unmarshal(out.Response, &response); err != nil {
		t.Fatalf("decode response: %v", err)
	}
	if response["chatbot_id"] != bot.ID || response["conversation_id"] != out.Conversation.ID {
		t.Fatalf("response missing chatbot metadata: %#v", response)
	}

	messages, err := f.chatRepo.ListMessages(ctx, f.tenantA, out.Conversation.ID)
	if err != nil {
		t.Fatalf("list messages: %v", err)
	}
	if len(messages) != 2 {
		t.Fatalf("expected request and assistant messages, got %d", len(messages))
	}
	if messages[0].Role != "user" || messages[0].Content != "What does the launch note say?" {
		t.Fatalf("unexpected persisted request message: %#v", messages[0])
	}
	if messages[1].Role != "assistant" || messages[1].Content != "RAG answer" {
		t.Fatalf("unexpected persisted assistant message: %#v", messages[1])
	}

	if len(f.usage.records) != 1 {
		t.Fatalf("expected one usage record, got %d", len(f.usage.records))
	}
	rec := f.usage.records[0]
	if rec.ChatbotID != bot.ID || rec.TotalTokens != 10 || rec.APIKeyID != p.APIKeyID {
		t.Fatalf("unexpected usage record: %#v", rec)
	}
}

func TestPrepareChatCompletionSupportsStreamingAndRecordCompletion(t *testing.T) {
	ctx := context.Background()
	f := newChatFixture(t, nil, nil)
	p := openPrincipal(f.tenantA)

	bot, err := f.svc.Create(ctx, p, chatbot.CreateChatbotInput{
		Name:       "Streaming Bot",
		Model:      "chat-test-model",
		ExternalID: "streaming_bot",
	})
	if err != nil {
		t.Fatalf("create chatbot: %v", err)
	}

	prepared, err := f.svc.PrepareChatCompletion(ctx, p, "streaming_bot", []byte(`{
		"model": "ignored-client-model",
		"stream": false,
		"messages": [{"role":"user","content":"Stream this answer."}]
	}`), true)
	if err != nil {
		t.Fatalf("prepare streaming completion: %v", err)
	}
	if prepared.Chatbot.ID != bot.ID || prepared.APIKeyID != p.APIKeyID {
		t.Fatalf("unexpected prepared metadata: %#v", prepared)
	}

	var upstream struct {
		Model    string `json:"model"`
		Stream   bool   `json:"stream"`
		Messages []struct {
			Role    string `json:"role"`
			Content string `json:"content"`
		} `json:"messages"`
	}
	if err := json.Unmarshal(prepared.UpstreamRequest, &upstream); err != nil {
		t.Fatalf("decode upstream request: %v", err)
	}
	if upstream.Model != "chat-test-model" || !upstream.Stream {
		t.Fatalf("unexpected upstream model/stream: model=%q stream=%v", upstream.Model, upstream.Stream)
	}

	messages, err := f.chatRepo.ListMessages(ctx, f.tenantA, prepared.Conversation.ID)
	if err != nil {
		t.Fatalf("list messages after prepare: %v", err)
	}
	if len(messages) != 1 || messages[0].Role != "user" {
		t.Fatalf("expected only request message after prepare, got %#v", messages)
	}

	record := f.svc.RecordCompletion(ctx, p, prepared, "streamed answer", 11, 5, 16, 200)
	if record.APIKeyID != p.APIKeyID || record.ChatbotID != bot.ID || record.TotalTokens != 16 {
		t.Fatalf("unexpected usage record: %#v", record)
	}

	messages, err = f.chatRepo.ListMessages(ctx, f.tenantA, prepared.Conversation.ID)
	if err != nil {
		t.Fatalf("list messages after record: %v", err)
	}
	if len(messages) != 2 {
		t.Fatalf("expected request and assistant messages, got %d", len(messages))
	}
	if messages[1].Role != "assistant" || messages[1].Content != "streamed answer" {
		t.Fatalf("unexpected assistant message: %#v", messages[1])
	}
}

func TestCreateChatCompletionProviderFailureRecordsUsage(t *testing.T) {
	ctx := context.Background()
	f := newChatFixture(t, nil, &fakeChatProvider{err: &llm.UpstreamError{StatusCode: 429, Message: "upstream rate limited"}})
	p := openPrincipal(f.tenantA)

	bot, err := f.svc.Create(ctx, p, chatbot.CreateChatbotInput{
		Name:       "Failure Bot",
		Model:      "chat-test-model",
		ExternalID: "failure_bot",
	})
	if err != nil {
		t.Fatalf("create chatbot: %v", err)
	}

	_, err = f.svc.CreateChatCompletion(ctx, p, "failure_bot", []byte(`{
		"messages": [{"role":"user","content":"will fail"}]
	}`))
	if err == nil {
		t.Fatalf("expected provider error")
	}

	if len(f.usage.records) != 1 {
		t.Fatalf("expected one usage record, got %d", len(f.usage.records))
	}
	rec := f.usage.records[0]
	if rec.ChatbotID != bot.ID || rec.StatusCode != 429 || rec.TotalTokens != 0 {
		t.Fatalf("unexpected failed usage record: %#v", rec)
	}
}

func TestPrepareChatCompletionEnforcesAllowedChatbots(t *testing.T) {
	ctx := context.Background()
	f := newChatFixture(t, nil, nil)

	bot, err := f.svc.Create(ctx, openPrincipal(f.tenantA), chatbot.CreateChatbotInput{
		Name:       "Allowed Bot",
		Model:      "chat-test-model",
		ExternalID: "allowed_bot",
	})
	if err != nil {
		t.Fatalf("create chatbot: %v", err)
	}
	body := []byte(`{"messages":[{"role":"user","content":"hello"}]}`)
	base := auth.Principal{TenantID: f.tenantA, ActorID: "key_actor", APIKeyID: 9}

	restricted := base
	restricted.AllowedChatbots = map[string]struct{}{"other_bot": {}}
	if _, err := f.svc.PrepareChatCompletion(ctx, restricted, "allowed_bot", body, false); !errors.Is(err, chatbot.ErrChatbotNotAllowed) {
		t.Fatalf("expected ErrChatbotNotAllowed, got %v", err)
	}
	byExternal := base
	byExternal.AllowedChatbots = map[string]struct{}{bot.ExternalID: {}}
	byExternal.AllowedModelsAll = true
	if _, err := f.svc.PrepareChatCompletion(ctx, byExternal, "allowed_bot", body, false); err != nil {
		t.Fatalf("expected external id to be allowed: %v", err)
	}
	otherAgent := base
	otherAgent.AllowedAgents = map[string]struct{}{"other_agent": {}}
	if _, err := f.svc.PrepareChatCompletion(ctx, otherAgent, "allowed_bot", body, false); !errors.Is(err, chatbot.ErrChatbotNotAllowed) {
		t.Fatalf("expected ErrChatbotNotAllowed for disallowed agent, got %v", err)
	}
	byAgent := base
	byAgent.AllowedAgents = map[string]struct{}{bot.ID: {}}
	byAgent.AllowedModelsAll = true
	if _, err := f.svc.PrepareChatCompletion(ctx, byAgent, "allowed_bot", body, false); err != nil {
		t.Fatalf("expected agent allowlist to accept chatbot id: %v", err)
	}

	// Empty allowlist denies everything.
	empty := base
	if _, err := f.svc.PrepareChatCompletion(ctx, empty, "allowed_bot", body, false); !errors.Is(err, chatbot.ErrChatbotNotAllowed) {
		t.Fatalf("expected ErrChatbotNotAllowed for empty allowlist, got %v", err)
	}
}

func TestResumeConversationRequiresOwnerOrAdmin(t *testing.T) {
	ctx := context.Background()
	f := newChatFixture(t, nil, nil)
	owner := openPrincipal(f.tenantA)
	other := openPrincipal(f.tenantA)
	other.ActorID = "key_actor_2"
	other.APIKeyID = 8
	admin := adminPrincipal(f.tenantA)

	if _, err := f.svc.Create(ctx, owner, chatbot.CreateChatbotInput{
		Name:       "Resume Bot",
		Model:      "chat-test-model",
		ExternalID: "resume_bot",
	}); err != nil {
		t.Fatalf("create chatbot: %v", err)
	}

	started, err := f.svc.PrepareChatCompletion(ctx, owner, "resume_bot", resumeBody(t, "", "first"), false)
	if err != nil {
		t.Fatalf("start conversation: %v", err)
	}
	if started.Conversation.OwnerActorID != owner.ActorID {
		t.Fatalf("conversation owner = %q, want %q", started.Conversation.OwnerActorID, owner.ActorID)
	}
	conversationID := started.Conversation.ID

	if _, err := f.svc.PrepareChatCompletion(ctx, other, "resume_bot", resumeBody(t, conversationID, "intrude"), false); !errors.Is(err, sql.ErrNoRows) {
		t.Fatalf("expected sql.ErrNoRows for other actor, got %v", err)
	}
	messages, err := f.chatRepo.ListMessages(ctx, f.tenantA, conversationID)
	if err != nil {
		t.Fatalf("list messages: %v", err)
	}
	if len(messages) != 1 || messages[0].Content != "first" {
		t.Fatalf("other actor inserted a message: %#v", messages)
	}

	resumed, err := f.svc.PrepareChatCompletion(ctx, owner, "resume_bot", resumeBody(t, conversationID, "owner again"), false)
	if err != nil {
		t.Fatalf("owner resume: %v", err)
	}
	if resumed.Conversation.ID != conversationID {
		t.Fatalf("owner resume conversation = %s, want %s", resumed.Conversation.ID, conversationID)
	}

	if _, err := f.svc.PrepareChatCompletion(ctx, admin, "resume_bot", resumeBody(t, conversationID, "admin"), false); err != nil {
		t.Fatalf("admin resume: %v", err)
	}
	messages, err = f.chatRepo.ListMessages(ctx, f.tenantA, conversationID)
	if err != nil {
		t.Fatalf("list messages after admin: %v", err)
	}
	if len(messages) != 3 || messages[2].Content != "admin" {
		t.Fatalf("unexpected messages after admin resume: %#v", messages)
	}

	// Cross-tenant resume denies identically to a missing conversation.
	cross := openPrincipal(f.tenantB)
	if _, err := f.svc.PrepareChatCompletion(ctx, cross, "resume_bot", resumeBody(t, conversationID, "cross"), false); !errors.Is(err, sql.ErrNoRows) {
		t.Fatalf("cross-tenant resume = %v, want ErrNoRows", err)
	}
}

func TestRetrieveRAGUsesBotAttachmentAccess(t *testing.T) {
	ctx := context.Background()
	searcher := &fakeRAGSearcher{err: fmt.Errorf("get collection: %w", sql.ErrNoRows)}
	f := newChatFixture(t, searcher, nil)
	p := openPrincipal(f.tenantA)

	if _, err := f.svc.Create(ctx, p, chatbot.CreateChatbotInput{
		Name:             "RAG Bot",
		Model:            "chat-test-model",
		ExternalID:       "rag_bot",
		RAGCollectionIDs: []string{"ragcol_missing"},
	}); err != nil {
		t.Fatalf("create chatbot: %v", err)
	}
	body := []byte(`{"messages":[{"role":"user","content":"where is the collection?"}]}`)
	_, err := f.svc.PrepareChatCompletion(ctx, p, "rag_bot", body, false)
	if !errors.Is(err, chatbot.ErrCollectionNotFound) {
		t.Fatalf("expected ErrCollectionNotFound, got %v", err)
	}
	if errors.Is(err, sql.ErrNoRows) {
		t.Fatal("missing collection must not surface as sql.ErrNoRows")
	}
	if len(searcher.principals) != 1 || searcher.principals[0].ActorID != p.ActorID {
		t.Fatalf("RAG search principal = %#v", searcher.principals)
	}

	searcher.err = errors.New("qdrant down http://qdrant.internal/secret-collection")
	_, err = f.svc.PrepareChatCompletion(ctx, p, "rag_bot", body, false)
	if errors.Is(err, chatbot.ErrCollectionNotFound) || errors.Is(err, sql.ErrNoRows) {
		t.Fatalf("infrastructure RAG error was rewritten: %v", err)
	}
	if err == nil || !strings.Contains(err.Error(), "secret-collection") {
		t.Fatalf("expected original RAG error, got %v", err)
	}
	last := searcher.principals[len(searcher.principals)-1]
	if auth.HasScope(last, "platform.admin") {
		t.Fatalf("attachment search forged admin scope: %#v", last)
	}
}

func TestServiceTenantIsolation(t *testing.T) {
	ctx := context.Background()
	f := newChatFixture(t, nil, nil)
	a := openPrincipal(f.tenantA)
	b := openPrincipal(f.tenantB)

	bot, err := f.svc.Create(ctx, a, chatbot.CreateChatbotInput{
		Name:       "Tenant Bot",
		Model:      "chat-test-model",
		ExternalID: "tenant_bot",
	})
	if err != nil {
		t.Fatalf("create chatbot: %v", err)
	}
	if bot.TenantID != f.tenantA || bot.OwnerActorID != a.ActorID {
		t.Fatalf("tenant/owner not stamped: %#v", bot)
	}

	listB, err := f.svc.List(ctx, b)
	if err != nil || len(listB) != 0 {
		t.Fatalf("tenant b list = %v %#v", err, listB)
	}
	if _, err := f.svc.Get(ctx, b, bot.ID); !errors.Is(err, sql.ErrNoRows) {
		t.Fatalf("cross-tenant get = %v, want ErrNoRows", err)
	}
	if _, err := f.svc.Update(ctx, b, bot.ID, chatbot.UpdateChatbotInput{}); !errors.Is(err, sql.ErrNoRows) {
		t.Fatalf("cross-tenant update = %v, want ErrNoRows", err)
	}
	if err := f.svc.Delete(ctx, b, bot.ID); !errors.Is(err, sql.ErrNoRows) {
		t.Fatalf("cross-tenant delete = %v, want ErrNoRows", err)
	}
	// External-id ref is also tenant scoped.
	if _, err := f.svc.PrepareChatCompletion(ctx, b, "tenant_bot",
		[]byte(`{"messages":[{"role":"user","content":"hi"}]}`), false); !errors.Is(err, sql.ErrNoRows) {
		t.Fatalf("cross-tenant completion = %v, want ErrNoRows", err)
	}
	// Missing tenant is denied before any repo access.
	noTenant := openPrincipal("")
	if _, err := f.svc.List(ctx, noTenant); !errors.Is(err, source.ErrDenied) {
		t.Fatalf("missing tenant list = %v, want ErrDenied", err)
	}
}

func resumeBody(t *testing.T, conversationID, content string) []byte {
	t.Helper()
	payload := map[string]any{
		"messages": []map[string]any{{"role": "user", "content": content}},
	}
	if conversationID != "" {
		payload["conversation_id"] = conversationID
	}
	body, err := json.Marshal(payload)
	if err != nil {
		t.Fatalf("marshal resume body: %v", err)
	}
	return body
}
