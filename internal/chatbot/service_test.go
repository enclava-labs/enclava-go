package chatbot

import (
	"context"
	"database/sql"
	"encoding/json"
	"errors"
	"fmt"
	"io"
	"path/filepath"
	"strings"
	"testing"

	"enclava-go/internal/llm"
	"enclava-go/internal/prompts"
	"enclava-go/internal/rag"
	"enclava-go/internal/store/sqlite"
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
	inputs   []rag.SearchInput
	accesses []rag.Access
	err      error
}

func (f *fakeRAGSearcher) Search(ctx context.Context, access rag.Access, input rag.SearchInput) ([]rag.SearchResult, error) {
	f.inputs = append(f.inputs, input)
	f.accesses = append(f.accesses, access)
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

func TestCreateChatCompletionInjectsRAGAndPersistsConversation(t *testing.T) {
	ctx := context.Background()
	db, err := sqlite.Open(ctx, filepath.Join(t.TempDir(), "chatbot.db"))
	if err != nil {
		t.Fatalf("open db: %v", err)
	}
	defer func() { _ = db.Close() }()

	keyID, err := sqlite.NewAPIKeysRepo(db.SQL).CreateAPIKey(ctx, "chatbot-test", "enc_test_key_"+newID("secret"), "sha256", "", []string{"chatbot", "chatbot.manage"})
	if err != nil {
		t.Fatalf("create api key: %v", err)
	}
	promptRepo := sqlite.NewPromptTemplatesRepo(db.SQL)
	if err := promptRepo.EnsureDefaults(ctx, prompts.DefaultTemplates()); err != nil {
		t.Fatalf("seed prompts: %v", err)
	}
	template := sqlite.PromptTemplate{
		ID:           "test-chatbot-template",
		Name:         "Test Chatbot",
		SystemPrompt: "Use this context for {{.audience}}:\n{{.rag_context}}",
		UserPrompt:   "Question: {{.user_input}}\nContext: {{.rag_context}}",
		IsActive:     true,
	}
	if err := promptRepo.Create(ctx, template); err != nil {
		t.Fatalf("create prompt template: %v", err)
	}

	provider := &fakeChatProvider{}
	ragSearcher := &fakeRAGSearcher{}
	chatRepo := sqlite.NewChatbotRepo(db.SQL)
	usageRepo := sqlite.NewUsageRepo(db.SQL)
	svc := NewService(chatRepo, promptRepo, ragSearcher, provider, usageRepo)

	bot, err := svc.Create(ctx, CreateChatbotInput{
		Name:             "Support Bot",
		PromptTemplateID: template.ID,
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

	out, err := svc.CreateChatCompletion(ctx, Access{APIKeyID: keyID, AllowedModelsAll: true, AllowedChatbotsAll: true, AllowedAgentsAll: true}, "support_bot", []byte(`{
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
	if len(ragSearcher.inputs) != 1 || ragSearcher.inputs[0].CollectionID != "ragcol_test" {
		t.Fatalf("unexpected RAG searches: %#v", ragSearcher.inputs)
	}
	if len(ragSearcher.accesses) != 1 || ragSearcher.accesses[0].APIKeyID != keyID || !ragSearcher.accesses[0].IsAdmin {
		t.Fatalf("RAG search must use attachment admin access, got %#v", ragSearcher.accesses)
	}

	var upstream struct {
		Model    string `json:"model"`
		Messages []struct {
			Role    string `json:"role"`
			Content string `json:"content"`
		} `json:"messages"`
	}
	if err := json.Unmarshal(provider.lastRequest, &upstream); err != nil {
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

	messages, err := chatRepo.ListMessages(ctx, out.Conversation.ID)
	if err != nil {
		t.Fatalf("list messages: %v", err)
	}
	if len(messages) != 2 {
		t.Fatalf("expected request and assistant messages, got %d", len(messages))
	}
	if messages[0].Role != "user" || messages[0].Content != "What does the launch note say?" {
		t.Fatalf("unexpected persisted request message: %#v", messages[0])
	}
	if messages[1].Role != "assistant" || messages[1].Content != "RAG answer" || messages[1].TotalTokens != 10 {
		t.Fatalf("unexpected persisted assistant message: %#v", messages[1])
	}

	var chatbotID string
	var totalTokens int64
	if err := db.SQL.QueryRowContext(ctx, `SELECT chatbot_id, total_tokens FROM usage_records WHERE api_key_id = ?`, keyID).Scan(&chatbotID, &totalTokens); err != nil {
		t.Fatalf("query usage record: %v", err)
	}
	if chatbotID != bot.ID || totalTokens != 10 {
		t.Fatalf("unexpected usage attribution: chatbot=%s tokens=%d", chatbotID, totalTokens)
	}
}

func TestPrepareChatCompletionSupportsStreamingAndRecordCompletion(t *testing.T) {
	ctx := context.Background()
	db, err := sqlite.Open(ctx, filepath.Join(t.TempDir(), "chatbot.db"))
	if err != nil {
		t.Fatalf("open db: %v", err)
	}
	defer func() { _ = db.Close() }()

	keyID, err := sqlite.NewAPIKeysRepo(db.SQL).CreateAPIKey(ctx, "chatbot-stream-test", "enc_test_key_"+newID("secret"), "sha256", "", []string{"chatbot", "chatbot.manage"})
	if err != nil {
		t.Fatalf("create api key: %v", err)
	}
	promptRepo := sqlite.NewPromptTemplatesRepo(db.SQL)
	if err := promptRepo.EnsureDefaults(ctx, prompts.DefaultTemplates()); err != nil {
		t.Fatalf("seed prompts: %v", err)
	}

	chatRepo := sqlite.NewChatbotRepo(db.SQL)
	usageRepo := sqlite.NewUsageRepo(db.SQL)
	svc := NewService(chatRepo, promptRepo, nil, &fakeChatProvider{}, usageRepo)
	bot, err := svc.Create(ctx, CreateChatbotInput{
		Name:       "Streaming Bot",
		Model:      "chat-test-model",
		ExternalID: "streaming_bot",
	})
	if err != nil {
		t.Fatalf("create chatbot: %v", err)
	}

	prepared, err := svc.PrepareChatCompletion(ctx, Access{APIKeyID: keyID, AllowedModelsAll: true, AllowedChatbotsAll: true, AllowedAgentsAll: true}, "streaming_bot", []byte(`{
		"model": "ignored-client-model",
		"stream": false,
		"messages": [{"role":"user","content":"Stream this answer."}]
	}`), true)
	if err != nil {
		t.Fatalf("prepare streaming completion: %v", err)
	}
	if prepared.Chatbot.ID != bot.ID || prepared.APIKeyID != keyID {
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

	messages, err := chatRepo.ListMessages(ctx, prepared.Conversation.ID)
	if err != nil {
		t.Fatalf("list messages after prepare: %v", err)
	}
	if len(messages) != 1 || messages[0].Role != "user" {
		t.Fatalf("expected only request message after prepare, got %#v", messages)
	}

	record := svc.RecordCompletion(ctx, prepared, "streamed answer", 11, 5, 16, 200)
	if record.APIKeyID != keyID || record.ChatbotID != bot.ID || record.TotalTokens != 16 {
		t.Fatalf("unexpected usage record: %#v", record)
	}

	messages, err = chatRepo.ListMessages(ctx, prepared.Conversation.ID)
	if err != nil {
		t.Fatalf("list messages after record: %v", err)
	}
	if len(messages) != 2 {
		t.Fatalf("expected request and assistant messages, got %d", len(messages))
	}
	if messages[1].Role != "assistant" || messages[1].Content != "streamed answer" || messages[1].TotalTokens != 16 {
		t.Fatalf("unexpected assistant message: %#v", messages[1])
	}
}

func TestCreateChatCompletionProviderFailureRecordsUsage(t *testing.T) {
	ctx := context.Background()
	db, err := sqlite.Open(ctx, filepath.Join(t.TempDir(), "chatbot.db"))
	if err != nil {
		t.Fatalf("open db: %v", err)
	}
	defer func() { _ = db.Close() }()

	keyID, err := sqlite.NewAPIKeysRepo(db.SQL).CreateAPIKey(ctx, "chatbot-failure-test", "enc_test_key_"+newID("secret"), "sha256", "", []string{"chatbot", "chatbot.manage"})
	if err != nil {
		t.Fatalf("create api key: %v", err)
	}
	promptRepo := sqlite.NewPromptTemplatesRepo(db.SQL)
	if err := promptRepo.EnsureDefaults(ctx, prompts.DefaultTemplates()); err != nil {
		t.Fatalf("seed prompts: %v", err)
	}
	chatRepo := sqlite.NewChatbotRepo(db.SQL)
	usageRepo := sqlite.NewUsageRepo(db.SQL)
	provider := &fakeChatProvider{err: &llm.UpstreamError{StatusCode: 429, Message: "upstream rate limited"}}
	svc := NewService(chatRepo, promptRepo, nil, provider, usageRepo)
	bot, err := svc.Create(ctx, CreateChatbotInput{
		Name:       "Failure Bot",
		Model:      "chat-test-model",
		ExternalID: "failure_bot",
	})
	if err != nil {
		t.Fatalf("create chatbot: %v", err)
	}

	_, err = svc.CreateChatCompletion(ctx, Access{APIKeyID: keyID, AllowedModelsAll: true, AllowedChatbotsAll: true, AllowedAgentsAll: true}, "failure_bot", []byte(`{
		"messages": [{"role":"user","content":"will fail"}]
	}`))
	if err == nil {
		t.Fatalf("expected provider error")
	}

	var chatbotID string
	var statusCode int
	var totalTokens int64
	if err := db.SQL.QueryRowContext(ctx, `SELECT chatbot_id, status_code, total_tokens FROM usage_records WHERE api_key_id = ?`, keyID).Scan(&chatbotID, &statusCode, &totalTokens); err != nil {
		t.Fatalf("query failed usage record: %v", err)
	}
	if chatbotID != bot.ID || statusCode != 429 || totalTokens != 0 {
		t.Fatalf("unexpected failed usage record: chatbot=%s status=%d tokens=%d", chatbotID, statusCode, totalTokens)
	}
	var assistantMessages int
	if err := db.SQL.QueryRowContext(ctx, `SELECT COUNT(1) FROM chatbot_messages WHERE role = 'assistant'`).Scan(&assistantMessages); err != nil {
		t.Fatalf("query assistant messages: %v", err)
	}
	if assistantMessages != 0 {
		t.Fatalf("expected no assistant message on provider failure, got %d", assistantMessages)
	}
}

func TestPrepareChatCompletionEnforcesAllowedChatbots(t *testing.T) {
	ctx := context.Background()
	db, err := sqlite.Open(ctx, filepath.Join(t.TempDir(), "chatbot.db"))
	if err != nil {
		t.Fatalf("open db: %v", err)
	}
	defer func() { _ = db.Close() }()

	promptRepo := sqlite.NewPromptTemplatesRepo(db.SQL)
	if err := promptRepo.EnsureDefaults(ctx, prompts.DefaultTemplates()); err != nil {
		t.Fatalf("seed prompts: %v", err)
	}
	chatRepo := sqlite.NewChatbotRepo(db.SQL)
	keyID, err := sqlite.NewAPIKeysRepo(db.SQL).CreateAPIKey(ctx, "allowlist-test", "enc_test_key_allowlist_secret", "sha256", "", []string{"chatbot"})
	if err != nil {
		t.Fatalf("create api key: %v", err)
	}
	svc := NewService(chatRepo, promptRepo, nil, &fakeChatProvider{}, nil)
	bot, err := svc.Create(ctx, CreateChatbotInput{
		Name:       "Allowed Bot",
		Model:      "chat-test-model",
		ExternalID: "allowed_bot",
	})
	if err != nil {
		t.Fatalf("create chatbot: %v", err)
	}
	body := []byte(`{"messages":[{"role":"user","content":"hello"}]}`)
	if _, err := svc.PrepareChatCompletion(ctx, Access{
		APIKeyID:        keyID,
		AllowedChatbots: map[string]struct{}{"other_bot": {}},
	}, "allowed_bot", body, false); !errors.Is(err, ErrChatbotNotAllowed) {
		t.Fatalf("expected ErrChatbotNotAllowed, got %v", err)
	}
	if _, err := svc.PrepareChatCompletion(ctx, Access{
		APIKeyID:         keyID,
		AllowedChatbots:  map[string]struct{}{bot.ExternalID: {}},
		AllowedModelsAll: true,
	}, "allowed_bot", body, false); err != nil {
		t.Fatalf("expected external id to be allowed: %v", err)
	}
	if _, err := svc.PrepareChatCompletion(ctx, Access{
		APIKeyID:      keyID,
		AllowedAgents: map[string]struct{}{"other_agent": {}},
	}, "allowed_bot", body, false); !errors.Is(err, ErrChatbotNotAllowed) {
		t.Fatalf("expected ErrChatbotNotAllowed for disallowed agent, got %v", err)
	}
	if _, err := svc.PrepareChatCompletion(ctx, Access{
		APIKeyID:         keyID,
		AllowedAgents:    map[string]struct{}{bot.ID: {}},
		AllowedModelsAll: true,
	}, "allowed_bot", body, false); err != nil {
		t.Fatalf("expected agent allowlist to accept chatbot id: %v", err)
	}
}

func TestResumeConversationRequiresSameAPIKey(t *testing.T) {
	ctx := context.Background()
	db, err := sqlite.Open(ctx, filepath.Join(t.TempDir(), "chatbot.db"))
	if err != nil {
		t.Fatalf("open db: %v", err)
	}
	defer func() { _ = db.Close() }()

	keys := sqlite.NewAPIKeysRepo(db.SQL)
	keyA, err := keys.CreateAPIKey(ctx, "owner", "enc_test_key_"+newID("a"), "sha256", "", []string{"chatbot"})
	if err != nil {
		t.Fatalf("create owner key: %v", err)
	}
	keyB, err := keys.CreateAPIKey(ctx, "other", "enc_test_key_"+newID("b"), "sha256", "", []string{"chatbot"})
	if err != nil {
		t.Fatalf("create other key: %v", err)
	}
	adminKey, err := keys.CreateAPIKey(ctx, "admin", "enc_test_key_"+newID("admin"), "sha256", "", []string{"chatbot", "platform.admin"})
	if err != nil {
		t.Fatalf("create admin key: %v", err)
	}
	promptRepo := sqlite.NewPromptTemplatesRepo(db.SQL)
	if err := promptRepo.EnsureDefaults(ctx, prompts.DefaultTemplates()); err != nil {
		t.Fatalf("seed prompts: %v", err)
	}
	chatRepo := sqlite.NewChatbotRepo(db.SQL)
	svc := NewService(chatRepo, promptRepo, nil, &fakeChatProvider{}, nil)
	bot, err := svc.Create(ctx, CreateChatbotInput{
		Name:       "Resume Bot",
		Model:      "chat-test-model",
		ExternalID: "resume_bot",
	})
	if err != nil {
		t.Fatalf("create chatbot: %v", err)
	}

	started, err := svc.PrepareChatCompletion(ctx, Access{APIKeyID: keyA, AllowedModelsAll: true, AllowedChatbotsAll: true, AllowedAgentsAll: true}, "resume_bot", resumeBody(t, "", "first"), false)
	if err != nil {
		t.Fatalf("start conversation: %v", err)
	}
	if started.Conversation.APIKeyID == nil || *started.Conversation.APIKeyID != keyA {
		t.Fatalf("conversation owner = %#v, want %d", started.Conversation.APIKeyID, keyA)
	}
	conversationID := started.Conversation.ID

	if _, err := svc.PrepareChatCompletion(ctx, Access{APIKeyID: keyB, AllowedModelsAll: true, AllowedChatbotsAll: true, AllowedAgentsAll: true}, "resume_bot", resumeBody(t, conversationID, "intrude"), false); !errors.Is(err, sql.ErrNoRows) {
		t.Fatalf("expected sql.ErrNoRows for other key, got %v", err)
	}
	messages, err := chatRepo.ListMessages(ctx, conversationID)
	if err != nil {
		t.Fatalf("list messages: %v", err)
	}
	if len(messages) != 1 || messages[0].Content != "first" {
		t.Fatalf("other key inserted a message: %#v", messages)
	}

	resumed, err := svc.PrepareChatCompletion(ctx, Access{APIKeyID: keyA, AllowedModelsAll: true, AllowedChatbotsAll: true, AllowedAgentsAll: true}, "resume_bot", resumeBody(t, conversationID, "owner again"), false)
	if err != nil {
		t.Fatalf("owner resume: %v", err)
	}
	if resumed.Conversation.ID != conversationID {
		t.Fatalf("owner resume conversation = %s, want %s", resumed.Conversation.ID, conversationID)
	}
	messages, err = chatRepo.ListMessages(ctx, conversationID)
	if err != nil {
		t.Fatalf("list messages after owner: %v", err)
	}
	if len(messages) != 2 || messages[1].Content != "owner again" {
		t.Fatalf("unexpected messages after owner resume: %#v", messages)
	}

	if _, err := svc.PrepareChatCompletion(ctx, Access{APIKeyID: adminKey, IsAdmin: true, AllowedModelsAll: true, AllowedChatbotsAll: true, AllowedAgentsAll: true}, "resume_bot", resumeBody(t, conversationID, "admin"), false); err != nil {
		t.Fatalf("admin resume: %v", err)
	}
	messages, err = chatRepo.ListMessages(ctx, conversationID)
	if err != nil {
		t.Fatalf("list messages after admin: %v", err)
	}
	if len(messages) != 3 || messages[2].Content != "admin" {
		t.Fatalf("unexpected messages after admin resume: %#v", messages)
	}

	legacyID := "chatconv_legacy_nil_owner"
	if err := chatRepo.CreateConversation(ctx, sqlite.ChatbotConversation{
		ID:        legacyID,
		ChatbotID: bot.ID,
		Metadata:  map[string]any{},
	}); err != nil {
		t.Fatalf("create legacy conversation: %v", err)
	}
	if _, err := svc.PrepareChatCompletion(ctx, Access{APIKeyID: keyA, AllowedModelsAll: true, AllowedChatbotsAll: true, AllowedAgentsAll: true}, "resume_bot", resumeBody(t, legacyID, "hidden"), false); !errors.Is(err, sql.ErrNoRows) {
		t.Fatalf("expected sql.ErrNoRows for nil owner, got %v", err)
	}
	legacyMessages, err := chatRepo.ListMessages(ctx, legacyID)
	if err != nil {
		t.Fatalf("list legacy messages: %v", err)
	}
	if len(legacyMessages) != 0 {
		t.Fatalf("nil owner was visible to a non-admin key: %#v", legacyMessages)
	}
	if _, err := svc.PrepareChatCompletion(ctx, Access{APIKeyID: adminKey, IsAdmin: true, AllowedModelsAll: true, AllowedChatbotsAll: true, AllowedAgentsAll: true}, "resume_bot", resumeBody(t, legacyID, "admin sees legacy"), false); err != nil {
		t.Fatalf("admin resume of nil owner: %v", err)
	}
}

func TestRetrieveRAGUsesBotAttachmentAccess(t *testing.T) {
	ctx := context.Background()
	db, err := sqlite.Open(ctx, filepath.Join(t.TempDir(), "chatbot.db"))
	if err != nil {
		t.Fatalf("open db: %v", err)
	}
	defer func() { _ = db.Close() }()

	keyID, err := sqlite.NewAPIKeysRepo(db.SQL).CreateAPIKey(ctx, "rag-acl", "enc_test_key_"+newID("rag"), "sha256", "", []string{"chatbot"})
	if err != nil {
		t.Fatalf("create api key: %v", err)
	}
	promptRepo := sqlite.NewPromptTemplatesRepo(db.SQL)
	if err := promptRepo.EnsureDefaults(ctx, prompts.DefaultTemplates()); err != nil {
		t.Fatalf("seed prompts: %v", err)
	}
	searcher := &fakeRAGSearcher{err: fmt.Errorf("get collection: %w", sql.ErrNoRows)}
	svc := NewService(sqlite.NewChatbotRepo(db.SQL), promptRepo, searcher, &fakeChatProvider{}, nil)
	if _, err := svc.Create(ctx, CreateChatbotInput{
		Name:             "RAG Bot",
		Model:            "chat-test-model",
		ExternalID:       "rag_bot",
		RAGCollectionIDs: []string{"ragcol_missing"},
	}); err != nil {
		t.Fatalf("create chatbot: %v", err)
	}
	body := []byte(`{"messages":[{"role":"user","content":"where is the collection?"}]}`)
	_, err = svc.PrepareChatCompletion(ctx, Access{APIKeyID: keyID, AllowedModelsAll: true, AllowedChatbotsAll: true, AllowedAgentsAll: true}, "rag_bot", body, false)
	if !errors.Is(err, ErrCollectionNotFound) {
		t.Fatalf("expected ErrCollectionNotFound, got %v", err)
	}
	if errors.Is(err, sql.ErrNoRows) {
		t.Fatal("missing collection must not surface as sql.ErrNoRows")
	}
	if len(searcher.accesses) != 1 || searcher.accesses[0].APIKeyID != keyID || !searcher.accesses[0].IsAdmin {
		t.Fatalf("RAG search access = %#v", searcher.accesses)
	}

	searcher.err = errors.New("qdrant down http://qdrant.internal/secret-collection")
	_, err = svc.PrepareChatCompletion(ctx, Access{APIKeyID: keyID, AllowedModelsAll: true, AllowedChatbotsAll: true, AllowedAgentsAll: true}, "rag_bot", body, false)
	if errors.Is(err, ErrCollectionNotFound) || errors.Is(err, sql.ErrNoRows) {
		t.Fatalf("infrastructure RAG error was rewritten: %v", err)
	}
	if err == nil || !strings.Contains(err.Error(), "secret-collection") {
		t.Fatalf("expected original RAG error, got %v", err)
	}
	if !searcher.accesses[len(searcher.accesses)-1].IsAdmin {
		t.Fatalf("attachment search dropped admin access: %#v", searcher.accesses)
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
