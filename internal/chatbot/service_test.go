package chatbot_test

import (
	"context"
	"database/sql"
	"encoding/json"
	"errors"
	"fmt"
	"io"
	"strings"
	"sync/atomic"
	"testing"
	"time"

	"enclava-go/internal/auth"
	"enclava-go/internal/billing"
	"enclava-go/internal/chatbot"
	"enclava-go/internal/inference"
	"enclava-go/internal/jobs"
	"enclava-go/internal/llm"
	"enclava-go/internal/prompts"
	"enclava-go/internal/rag"
	"enclava-go/internal/source"
	"enclava-go/internal/store/postgres"
	"enclava-go/internal/testdb"
)

// fakeChatUpstream is the transport seam the executor dispatches on —
// tests drive the real inference.Service, never a provider shortcut.
type fakeChatUpstream struct {
	lastRequest []byte
	streamBody  io.ReadCloser
	response    []byte
	err         error
	calls       int32
}

func (f *fakeChatUpstream) CreateChatCompletion(ctx context.Context, body []byte) ([]byte, error) {
	atomic.AddInt32(&f.calls, 1)
	f.lastRequest = append([]byte(nil), body...)
	if f.err != nil {
		return nil, f.err
	}
	if f.response != nil {
		return append([]byte(nil), f.response...), nil
	}
	return []byte(`{"id":"chatcmpl_test","object":"chat.completion","model":"chat-test-model","choices":[{"index":0,"message":{"role":"assistant","content":"RAG answer"},"finish_reason":"stop"}],"usage":{"prompt_tokens":7,"completion_tokens":3,"total_tokens":10}}`), nil
}

func (f *fakeChatUpstream) CreateChatCompletionStream(ctx context.Context, body []byte) (io.ReadCloser, error) {
	atomic.AddInt32(&f.calls, 1)
	f.lastRequest = append([]byte(nil), body...)
	if f.streamBody != nil {
		return f.streamBody, nil
	}
	return io.NopCloser(strings.NewReader("")), nil
}

func (f *fakeChatUpstream) CreateEmbeddings(ctx context.Context, body []byte) ([]byte, error) {
	return nil, nil
}

func (f *fakeChatUpstream) DoResponsesRequest(ctx context.Context, method, path string, body []byte) ([]byte, error) {
	return nil, inference.ErrUnsupportedCapability
}

func (f *fakeChatUpstream) DoResponsesStream(ctx context.Context, method, path string, body []byte) (io.ReadCloser, error) {
	return nil, inference.ErrUnsupportedCapability
}

type fakeRAGSearcher struct {
	inputs              []rag.SearchInput
	principals          []auth.Principal
	err                 error
	evidenceID          string
	revoked             bool
	emptyDoc            bool
	deniedCollectionFor map[string]bool
	// evidenceContent is the passage ReadEvidence returns for the attached
	// evidence; evidenceReadDenied drops the passage at execution time
	// (revocation between enqueue and worker read).
	evidenceContent    string
	evidenceReadDenied bool
	reads              []string
}

func (f *fakeRAGSearcher) Search(ctx context.Context, p auth.Principal, input rag.SearchInput) ([]rag.SearchResult, error) {
	f.inputs = append(f.inputs, input)
	f.principals = append(f.principals, p)
	if f.revoked {
		return nil, nil
	}
	if f.err != nil {
		return nil, f.err
	}
	documentID := "doc_1"
	if f.emptyDoc {
		documentID = ""
	}
	return []rag.SearchResult{
		{
			DocumentID: documentID,
			ChunkID:    "chunk_1",
			Score:      0.98,
			Content:    "The phrase maple-vector-citadel identifies the launch note.",
			FileName:   "launch.txt",
			EvidenceID: f.evidenceID,
		},
	}, nil
}

func (f *fakeRAGSearcher) ListCollections(_ context.Context, p auth.Principal) ([]rag.Collection, error) {
	if f.deniedCollectionFor[p.ActorID] {
		return []rag.Collection{{ID: "ragcol_missing"}}, nil
	}
	return []rag.Collection{{ID: "ragcol_test"}, {ID: "ragcol_missing"}, {ID: "collection-a"}}, nil
}
func (f *fakeRAGSearcher) BuildContext(_ context.Context, _ auth.Principal, candidates []rag.SearchResult, limit int) (rag.ContextResult, error) {
	return rag.ContextResult{Text: rag.BuildContext(context.Background(), auth.Principal{}, candidates, limit, allowContext{}).Text}, nil
}

func (f *fakeRAGSearcher) ResolveEvidence(_ context.Context, _ auth.Principal, ids []string) ([]rag.EvidenceResolution, error) {
	if f.revoked || len(ids) == 0 || f.evidenceID == "" {
		return nil, source.ErrDenied
	}
	for _, id := range ids {
		if id != f.evidenceID {
			return nil, source.ErrDenied
		}
	}
	return []rag.EvidenceResolution{{
		ID: f.evidenceID, Status: "current",
		Spans: []source.Span{{Ref: source.ObjectRef{ObjectID: "doc_attached", ObservedSeq: 7}, BlockID: "blk_1"}},
	}}, nil
}

// ReadEvidence is the CURRENT-passage reader the worker uses for
// evidence-attached runs; the fake tracks the executing principal.
func (f *fakeRAGSearcher) ReadEvidence(_ context.Context, p auth.Principal, evidenceID string) (rag.EvidenceText, error) {
	if f.evidenceReadDenied || f.revoked || f.evidenceID == "" || evidenceID != f.evidenceID {
		return rag.EvidenceText{}, source.ErrDenied
	}
	f.reads = append(f.reads, p.ActorID)
	content := f.evidenceContent
	if content == "" {
		content = "attached-passage-bytes"
	}
	return rag.EvidenceText{
		EvidenceID: evidenceID, DocumentID: "doc_attached", CollectionID: "ragcol_test",
		Content: content,
		Spans:   []source.Span{{Ref: source.ObjectRef{ObjectID: "doc_attached", ObservedSeq: 9}, BlockID: "blk_1"}},
	}, nil
}

type allowContext struct{}

func (allowContext) CanReadObject(context.Context, auth.Principal, string) bool { return true }

type chatFixture struct {
	svc         *chatbot.Service
	db          *postgres.DB
	chatRepo    *postgres.ChatbotRepo
	promptRepo  *postgres.PromptsRepo
	sourceRepo  *postgres.SourceRepo
	billingRepo *postgres.BillingRepo
	upstream    *fakeChatUpstream
	rag         *fakeRAGSearcher
	tenantA     string
	tenantB     string
}

// newChatFixture composes the real shared executor over postgres seams with
// the fake transport, a chat-capable endpoint and input/output prices so
// completions exercise the same reservation path as production.
func newChatFixture(t *testing.T, searcher *fakeRAGSearcher, upstream *fakeChatUpstream) *chatFixture {
	t.Helper()
	ctx := context.Background()
	db := testdb.New(t)
	tenantA := testdb.SeedTenant(t, db, "chat-a")
	tenantB := testdb.SeedTenant(t, db, "chat-b")
	chatRepo := postgres.NewChatbotRepo(db)
	promptRepo := postgres.NewPromptsRepo(db)
	sourceRepo := postgres.NewSourceRepo(db)
	billingRepo := postgres.NewBillingRepo(db)
	endpoints := postgres.NewEndpointsRepo(db)
	if upstream == nil {
		upstream = &fakeChatUpstream{}
	}
	if err := endpoints.Create(ctx, inference.Endpoint{
		ID: "ep-chat", TenantID: tenantA, Protocol: "openai_compatible",
		BaseOrigin:   "https://chat.upstream.example",
		Capabilities: []string{inference.CapabilityChat, inference.CapabilityVision, "model:chat-test-model"},
		Enabled:      true,
	}); err != nil {
		t.Fatalf("create endpoint: %v", err)
	}
	for _, unit := range []billing.UnitKind{billing.UnitInput, billing.UnitOutput, billing.UnitCachedInput} {
		if err := billingRepo.PutPrice(ctx, tenantA,
			fmt.Sprintf("p-chat-%s-%d", unit, time.Now().UnixNano()),
			billing.Price{
				SubjectType: billing.PriceSubjectEndpoint, SubjectID: "ep-chat",
				Capability: inference.CapabilityChat, Unit: unit,
				RateMicros: 1, UnitQuantity: 1, Currency: "USD",
			}, "tester", time.Now().UTC().Add(-time.Minute)); err != nil {
			t.Fatalf("put price: %v", err)
		}
	}
	executor, err := inference.NewService(inference.Deps{
		Endpoints:     endpoints,
		Sources:       sourceRepo,
		Authorizer:    source.NewAuthorizer(sourceRepo, 0),
		Billing:       billing.NewService(billingRepo),
		Artifacts:     postgres.NewArtifactRepo(db),
		Conversations: postgres.NewChatbotConversationStore(chatRepo),
	}, inference.WithTransportFactory(func(ctx context.Context, e inference.Endpoint, credential string) (inference.Upstream, error) {
		return upstream, nil
	}))
	if err != nil {
		t.Fatalf("new inference service: %v", err)
	}
	svc := chatbot.NewService(chatRepo, promptRepo, searcher, executor)
	return &chatFixture{
		svc:         svc,
		db:          db,
		chatRepo:    chatRepo,
		promptRepo:  promptRepo,
		sourceRepo:  sourceRepo,
		billingRepo: billingRepo,
		upstream:    upstream,
		rag:         searcher,
		tenantA:     tenantA,
		tenantB:     tenantB,
	}
}

func openPrincipal(tenantID string) auth.Principal {
	return auth.Principal{
		TenantID:            tenantID,
		ActorID:             "key_actor_1",
		ActorKind:           auth.ActorKindServiceAccount,
		AllowedModelsAll:    true,
		AllowedChatbotsAll:  true,
		AllowedAgentsAll:    true,
		AllowedEndpointsAll: true,
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
	// The retrieved chunk references a real readable source object so the
	// executor sees the same evidence the searcher returned.
	seedReadableObject(t, f, p, "doc_1")

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
	if err := json.Unmarshal(f.upstream.lastRequest, &upstream); err != nil {
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

	// The executor's settlement record is the single accounting artifact —
	// no duplicate usage row exists on the assistant path.
	if out.Usage == nil || out.Usage.Units[billing.UnitInput] != 7 || out.Usage.Units[billing.UnitOutput] != 3 {
		t.Fatalf("unexpected settlement evidence: %#v", out.Usage)
	}
}

func TestRestrictedRAGSourceDeniesIncompatibleEndpoint(t *testing.T) {
	ctx := context.Background()
	searcher := &fakeRAGSearcher{}
	f := newChatFixture(t, searcher, nil)
	p := openPrincipal(f.tenantA)
	seedReadableObject(t, f, p, "doc_1")

	// Require "high" assurance on the retrieved object: the seeded
	// openai_compatible endpoint cannot prove it, so dispatch must never
	// happen.
	if err := f.sourceRepo.SetRestriction(ctx, source.SourceRestriction{
		TenantID:          f.tenantA,
		SubjectKind:       source.RestrictionObject,
		SubjectID:         "doc_1",
		RequiredAssurance: "high",
	}); err != nil {
		t.Fatalf("set restriction: %v", err)
	}

	if _, err := f.svc.Create(ctx, p, chatbot.CreateChatbotInput{
		Name:             "Restricted Bot",
		Model:            "chat-test-model",
		ExternalID:       "restricted_bot",
		RAGCollectionIDs: []string{"ragcol_test"},
	}); err != nil {
		t.Fatalf("create chatbot: %v", err)
	}

	_, err := f.svc.CreateChatCompletion(ctx, p, "restricted_bot", []byte(`{
		"messages": [{"role":"user","content":"tell me about the launch note"}]
	}`))
	if err == nil {
		t.Fatal("restricted source reached dispatch")
	}
	if !errors.Is(err, inference.ErrNoRoute) && !errors.Is(err, inference.ErrDenied) {
		t.Fatalf("expected executor denial, got %v", err)
	}
	if atomic.LoadInt32(&f.upstream.calls) != 0 {
		t.Fatalf("upstream dispatched %d times on a denied route", f.upstream.calls)
	}
}

// seedReadableObject inserts a tenant-owned source object the actor may read,
// matching what rag search returns as evidence.
func seedReadableObject(t *testing.T, f *chatFixture, p auth.Principal, id string) {
	t.Helper()
	ctx := context.Background()
	if _, err := f.sourceRepo.UpsertObject(ctx, source.Object{
		TenantID: f.tenantA, ID: id, Kind: "file", NativeID: "n-" + id,
		AuthorizationMode: source.AuthorizationOwner, OwnerActorID: p.ActorID,
		ObservedSeq: 1}); err != nil {
		t.Fatalf("upsert object: %v", err)
	}
	if err := f.sourceRepo.InsertObjectVersion(ctx, f.tenantA, source.ObjectVersion{
		ObjectID: id, ObservedSeq: 1, ArtifactRef: "art-" + id,
		SHA256: "aa", ByteSize: 10, MimeType: "text/plain"}); err != nil {
		t.Fatalf("insert version: %v", err)
	}
}

func TestPrepareChatCompletionSupportsStreamingAndPersistAssistant(t *testing.T) {
	ctx := context.Background()
	f := newChatFixture(t, nil, nil)
	f.upstream.streamBody = io.NopCloser(strings.NewReader(
		"data: {\"choices\":[{\"delta\":{\"content\":\"streamed \"}}]}\n\n" +
			"data: {\"choices\":[{\"delta\":{\"content\":\"answer\"}}],\"usage\":{\"prompt_tokens\":11,\"completion_tokens\":5,\"total_tokens\":16}}\n\n" +
			"data: [DONE]\n\n"))
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
	if prepared.Chatbot.ID != bot.ID {
		t.Fatalf("unexpected prepared chatbot: %#v", prepared.Chatbot.ID)
	}

	var upstream struct {
		Model    string `json:"model"`
		Stream   bool   `json:"stream"`
		Messages []struct {
			Role    string `json:"role"`
			Content string `json:"content"`
		} `json:"messages"`
	}
	if err := json.Unmarshal(prepared.Request.Payload, &upstream); err != nil {
		t.Fatalf("decode upstream request: %v", err)
	}
	if upstream.Model != "chat-test-model" || !upstream.Stream || !prepared.Request.Stream {
		t.Fatalf("unexpected upstream model/stream: model=%q stream=%v", upstream.Model, upstream.Stream)
	}

	messages, err := f.chatRepo.ListMessages(ctx, f.tenantA, prepared.Conversation.ID)
	if err != nil {
		t.Fatalf("list messages after prepare: %v", err)
	}
	if len(messages) != 2 || messages[0].Role != "user" || messages[1].Status != "reserved" || messages[1].Content != "Response was interrupted before completion." {
		t.Fatalf("expected request and reserved assistant message, got %#v", messages)
	}

	// Dispatch runs only through the shared executor.
	res, err := f.svc.ExecutePrepared(ctx, p, prepared)
	if err != nil {
		t.Fatalf("execute prepared: %v", err)
	}
	streamed, err := io.ReadAll(res.Body)
	_ = res.Body.Close()
	if err != nil {
		t.Fatalf("read stream: %v", err)
	}
	if !strings.Contains(string(streamed), "streamed ") {
		t.Fatalf("unexpected stream body: %s", streamed)
	}

	if err := f.svc.PersistAssistantMessage(ctx, p, prepared, "streamed answer"); err != nil {
		t.Fatalf("persist assistant message: %v", err)
	}

	messages, err = f.chatRepo.ListMessages(ctx, f.tenantA, prepared.Conversation.ID)
	if err != nil {
		t.Fatalf("list messages after record: %v", err)
	}
	if len(messages) != 2 {
		t.Fatalf("expected request and assistant messages, got %d", len(messages))
	}
	if messages[1].Role != "assistant" || messages[1].Content != "streamed answer" || messages[1].Status != "complete" || messages[1].ID != prepared.AssistantMessageID {
		t.Fatalf("unexpected assistant message: %#v", messages[1])
	}
}

func TestCreateChatCompletionUpstreamFailurePreservesError(t *testing.T) {
	ctx := context.Background()
	upErr := &llm.UpstreamError{StatusCode: 429, Message: "upstream rate limited"}
	f := newChatFixture(t, nil, &fakeChatUpstream{err: upErr})
	p := openPrincipal(f.tenantA)

	if _, err := f.svc.Create(ctx, p, chatbot.CreateChatbotInput{
		Name:       "Failure Bot",
		Model:      "chat-test-model",
		ExternalID: "failure_bot",
	}); err != nil {
		t.Fatalf("create chatbot: %v", err)
	}

	_, err := f.svc.CreateChatCompletion(ctx, p, "failure_bot", []byte(`{
		"messages": [{"role":"user","content":"will fail"}]
	}`))
	if err == nil {
		t.Fatalf("expected provider error")
	}
	var upstreamErr *llm.UpstreamError
	if !errors.As(err, &upstreamErr) || upstreamErr.StatusCode != 429 {
		t.Fatalf("real upstream error must surface, got %v", err)
	}
	if atomic.LoadInt32(&f.upstream.calls) != 1 {
		t.Fatalf("expected one upstream dispatch, got %d", f.upstream.calls)
	}
	conversations, _, err := f.svc.ListConversations(ctx, p, 10, "")
	if err != nil || len(conversations) != 1 {
		t.Fatalf("list conversations: %v %#v", err, conversations)
	}
	view, err := f.svc.ListConversation(ctx, p, conversations[0].ID)
	if err != nil || len(view.Messages) != 2 || view.Messages[1].Status != "interrupted" || view.Messages[1].Content != "Response was interrupted before completion." {
		t.Fatalf("interrupted conversation: %v %#v", err, view.Messages)
	}
	request := fmt.Sprintf(`{"conversation_id":%q,"messages":[{"role":"user","content":"next question"}]}`, conversations[0].ID)
	prepared, err := f.svc.PrepareChatCompletion(ctx, p, "failure_bot", []byte(request), false)
	if err != nil {
		t.Fatalf("prepare next completion: %v", err)
	}
	if strings.Contains(string(prepared.Request.Payload), "Response was interrupted") {
		t.Fatalf("interrupted response entered history: %s", prepared.Request.Payload)
	}
}

func TestPrepareChatCompletionEnforcesAllowedChatbots(t *testing.T) {
	ctx := context.Background()
	f := newChatFixture(t, nil, nil)

	bot, err := f.svc.Create(ctx, openPrincipal(f.tenantA), chatbot.CreateChatbotInput{
		Name:       "Allowed Bot",
		Model:      "chat-test-model",
		ExternalID: "allowed_bot",
		// This test is about grant allowlists — the tenant-wide audience
		// keeps the audience term out of the picture.
		Audience: []string{"*"},
	})
	if err != nil {
		t.Fatalf("create chatbot: %v", err)
	}
	body := []byte(`{"messages":[{"role":"user","content":"hello"}]}`)
	base := auth.Principal{TenantID: f.tenantA, ActorID: "key_actor"}

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

	// The other actor holds an all-assistants grant but is outside the
	// bot's (owner-only default) audience — denied before the
	// conversation-owner check even runs.
	if _, err := f.svc.PrepareChatCompletion(ctx, other, "resume_bot", resumeBody(t, conversationID, "intrude"), false); !errors.Is(err, chatbot.ErrChatbotNotAllowed) {
		t.Fatalf("expected audience denial for other actor, got %v", err)
	}
	messages, err := f.chatRepo.ListMessages(ctx, f.tenantA, conversationID)
	if err != nil {
		t.Fatalf("list messages: %v", err)
	}
	// A reservation makes the owner's prepare two rows; the security check is
	// that none of them came from the other actor.
	for _, m := range messages {
		if m.Content == "intrude" || m.OwnerActorID == other.ActorID {
			t.Fatalf("other actor inserted a message: %#v", messages)
		}
		if m.OwnerActorID != owner.ActorID {
			t.Fatalf("unexpected message owner: %#v", messages)
		}
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
	var lastUserContent string
	for _, m := range messages {
		if m.Role == "user" {
			lastUserContent = m.Content
		}
	}
	if lastUserContent != "admin" {
		t.Fatalf("last user message = %q, want admin: %#v", lastUserContent, messages)
	}
	for _, m := range messages {
		if m.OwnerActorID != owner.ActorID && m.OwnerActorID != admin.ActorID {
			t.Fatalf("foreign actor message survived resume: %#v", messages)
		}
	}

	// Cross-tenant resume denies identically to a missing conversation.
	cross := openPrincipal(f.tenantB)
	if _, err := f.svc.PrepareChatCompletion(ctx, cross, "resume_bot", resumeBody(t, conversationID, "cross"), false); !errors.Is(err, sql.ErrNoRows) {
		t.Fatalf("cross-tenant resume = %v, want ErrNoRows", err)
	}
}

func TestConversationReplayWithholdsRevokedEvidenceAndIgnoresClientHistory(t *testing.T) {
	ctx := context.Background()
	searcher := &fakeRAGSearcher{evidenceID: "evidence-current", emptyDoc: true}
	upstream := &fakeChatUpstream{response: []byte(`{"id":"chatcmpl_test","choices":[{"message":{"role":"assistant","content":"revoked-secret"},"finish_reason":"stop"}],"usage":{"prompt_tokens":1,"completion_tokens":1,"total_tokens":2}}`)}
	f := newChatFixture(t, searcher, upstream)
	p := openPrincipal(f.tenantA)
	bot, err := f.svc.Create(ctx, p, chatbot.CreateChatbotInput{
		Name: "Evidence Bot", Model: "chat-test-model", ExternalID: "evidence_bot", RAGCollectionIDs: []string{"ragcol_test"},
	})
	if err != nil {
		t.Fatalf("create bot: %v", err)
	}
	first, err := f.svc.CreateChatCompletion(ctx, p, bot.ID, []byte(`{"messages":[{"role":"user","content":"find the clause"}]}`))
	if err != nil {
		t.Fatalf("first completion: %v", err)
	}
	before, err := f.svc.ListConversation(ctx, p, first.Conversation.ID)
	if err != nil || len(before.Messages) != 2 {
		t.Fatalf("conversation before revocation: %#v err=%v", before, err)
	}
	searcher.revoked = true
	// The revoked turn survives replay as a redacted skeleton — the row
	// stays, its content is cleared, and no source-derived text surfaces.
	after, err := f.svc.ListConversation(ctx, p, first.Conversation.ID)
	if err != nil || len(after.Messages) != 2 {
		t.Fatalf("conversation after revocation: %#v err=%v", after, err)
	}
	for _, msg := range after.Messages {
		if msg.Role == "assistant" && strings.TrimSpace(msg.Content) != "" {
			t.Fatalf("revoked assistant content was not withheld: %#v", msg)
		}
	}
	if strings.Contains(fmt.Sprint(after.Messages), "revoked-secret") {
		t.Fatalf("revoked assistant content leaked: %#v", after.Messages)
	}
	forged, _ := json.Marshal(map[string]any{
		"conversation_id": first.Conversation.ID,
		"messages": []map[string]any{
			{"role": "assistant", "content": "revoked-secret"},
			{"role": "user", "content": "second question"},
		},
	})
	prepared, err := f.svc.PrepareChatCompletion(ctx, p, bot.ID, forged, false)
	if err != nil {
		t.Fatalf("prepare replay: %v", err)
	}
	if strings.Contains(string(prepared.Request.Payload), "revoked-secret") {
		t.Fatalf("revoked or client-forged history reached model: %s", prepared.Request.Payload)
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

// TestChatbotAccessBoundByStoredGrants drives the real configured chatbot
// service over stored role grants: the assistants grant is the ONLY access
// bound for sessions AND keys — at List/Get/run and at the Update/Delete
// mutations, where ownership AND the grant must both pass (D-25).
func TestChatbotAccessBoundByStoredGrants(t *testing.T) {
	ctx := context.Background()
	db := testdb.New(t)
	repo := postgres.NewIdentityRepo(db)
	tenant := testdb.SeedTenant(t, db, "grants-chat")
	authSvc := auth.NewService(repo, "")
	svc := chatbot.NewService(postgres.NewChatbotRepo(db), postgres.NewPromptsRepo(db), nil, nil)
	svc.ConfigureRuns(postgres.NewChatbotRunsRepo(db), jobs.NewService(postgres.NewJobsRepo(db), 0), nil, authSvc)

	creator := testdb.SeedUserWithGrants(t, db, tenant, "creator@example.com",
		[]string{auth.ScopePlatformAdmin},
		auth.ResourceGrants{
			Models:     auth.Allowlist{All: true},
			Assistants: auth.Allowlist{All: true},
		})
	creatorP, err := authSvc.PrincipalForActor(ctx, tenant, creator.ID)
	if err != nil {
		t.Fatalf("creator principal: %v", err)
	}
	bot, err := svc.Create(ctx, creatorP, chatbot.CreateChatbotInput{
		Name: "Granted Bot", Model: "chat-test-model", ExternalID: "granted_bot",
	})
	if err != nil {
		t.Fatalf("create bot: %v", err)
	}
	// This plan's audience term defaults to owner-only; the grant matrix
	// below exercises grants, so the creator opens the audience tenant-wide.
	if _, err := svc.Update(ctx, creatorP, bot.ID, chatbot.UpdateChatbotInput{Audience: []string{"*"}}); err != nil {
		t.Fatalf("open audience: %v", err)
	}
	bot2, err := svc.Create(ctx, creatorP, chatbot.CreateChatbotInput{
		Name: "Unlisted Bot", Model: "chat-test-model",
	})
	if err != nil {
		t.Fatalf("create second bot: %v", err)
	}

	// (a) A member whose role lists the bot sees exactly that bot; the
	// unlisted second bot is not-found.
	member := testdb.SeedUserWithGrants(t, db, tenant, "member@example.com", []string{"chatbot"},
		auth.ResourceGrants{Assistants: auth.Allowlist{Items: []string{bot.ID}}})
	memberP, err := authSvc.PrincipalForActor(ctx, tenant, member.ID)
	if err != nil {
		t.Fatalf("member principal: %v", err)
	}
	list, err := svc.List(ctx, memberP)
	if err != nil || len(list) != 1 || list[0].ID != bot.ID {
		t.Fatalf("granted member list = %#v err=%v, want exactly bot %s", list, err, bot.ID)
	}
	if _, err := svc.Get(ctx, memberP, bot2.ID); !errors.Is(err, sql.ErrNoRows) {
		t.Fatalf("unlisted bot get = %v, want ErrNoRows", err)
	}

	// (b) platform.admin scope with explicit empty deny grants NOTHING —
	// List empty, Get not-found, run refused.
	denyAdmin := testdb.SeedUserWithGrants(t, db, tenant, "denyadmin@example.com",
		[]string{auth.ScopePlatformAdmin}, auth.ResourceGrants{})
	denyAdminP, err := authSvc.PrincipalForActor(ctx, tenant, denyAdmin.ID)
	if err != nil {
		t.Fatalf("deny-admin principal: %v", err)
	}
	if list, err := svc.List(ctx, denyAdminP); err != nil || len(list) != 0 {
		t.Fatalf("empty-grant admin list = %#v err=%v, want empty", list, err)
	}
	if _, err := svc.Get(ctx, denyAdminP, bot.ID); !errors.Is(err, sql.ErrNoRows) {
		t.Fatalf("empty-grant admin get = %v, want ErrNoRows", err)
	}
	if _, err := svc.StartRun(ctx, denyAdminP, bot.ID, chatbot.AssistantRunInput{Input: "hello"}); !errors.Is(err, chatbot.ErrChatbotNotAllowed) {
		t.Fatalf("empty-grant admin start run = %v, want ErrChatbotNotAllowed", err)
	}

	// (d) An API-key-shaped platform.admin principal without assistant
	// grants is denied identically — the non-session admin bypass is gone.
	keyAdmin := auth.Principal{
		TenantID: tenant, ActorID: "key_admin",
		CredentialKind:   auth.CredentialKindAPIKey,
		Scopes:           map[string]struct{}{auth.ScopePlatformAdmin: {}},
		AllowedModelsAll: true,
	}
	if list, err := svc.List(ctx, keyAdmin); err != nil || len(list) != 0 {
		t.Fatalf("restricted key admin list = %#v err=%v, want empty", list, err)
	}
	if _, err := svc.Get(ctx, keyAdmin, bot.ID); !errors.Is(err, sql.ErrNoRows) {
		t.Fatalf("restricted key admin get = %v, want ErrNoRows", err)
	}

	// (e) Mutations: the granted owner mutates; a granted non-owner and an
	// empty-grant admin get not-found on both mutations.
	renamed := "Renamed Bot"
	if _, err := svc.Update(ctx, creatorP, bot.ID, chatbot.UpdateChatbotInput{Name: &renamed}); err != nil {
		t.Fatalf("granted owner update: %v", err)
	}
	if _, err := svc.Update(ctx, memberP, bot.ID, chatbot.UpdateChatbotInput{Name: &renamed}); !errors.Is(err, sql.ErrNoRows) {
		t.Fatalf("granted non-owner update = %v, want ErrNoRows", err)
	}
	if err := svc.Delete(ctx, memberP, bot.ID); !errors.Is(err, sql.ErrNoRows) {
		t.Fatalf("granted non-owner delete = %v, want ErrNoRows", err)
	}
	if _, err := svc.Update(ctx, denyAdminP, bot.ID, chatbot.UpdateChatbotInput{Name: &renamed}); !errors.Is(err, sql.ErrNoRows) {
		t.Fatalf("empty-grant admin update = %v, want ErrNoRows", err)
	}
	if err := svc.Delete(ctx, denyAdminP, bot.ID); !errors.Is(err, sql.ErrNoRows) {
		t.Fatalf("empty-grant admin delete = %v, want ErrNoRows", err)
	}

	// (c) The bot's OWNER loses access the request after its role grant is
	// revoked — no owner bypass, no re-login.
	if _, err := db.SQL.ExecContext(ctx,
		`UPDATE roles SET allowed_assistants = '{"mode":"list","items":[]}'::jsonb
		 WHERE tenant_id = $1::uuid AND id IN (SELECT role_id FROM user_roles WHERE user_id = $2)`,
		tenant, creator.ID); err != nil {
		t.Fatalf("revoke owner grant: %v", err)
	}
	revokedOwnerP, err := authSvc.PrincipalForActor(ctx, tenant, creator.ID)
	if err != nil {
		t.Fatalf("revoked owner principal: %v", err)
	}
	if list, err := svc.List(ctx, revokedOwnerP); err != nil || len(list) != 0 {
		t.Fatalf("revoked owner list = %#v err=%v, want empty", list, err)
	}
	if _, err := svc.StartRun(ctx, revokedOwnerP, bot.ID, chatbot.AssistantRunInput{Input: "hello"}); !errors.Is(err, chatbot.ErrChatbotNotAllowed) {
		t.Fatalf("revoked owner start run = %v, want ErrChatbotNotAllowed", err)
	}
	if _, err := svc.Update(ctx, revokedOwnerP, bot.ID, chatbot.UpdateChatbotInput{Name: &renamed}); !errors.Is(err, sql.ErrNoRows) {
		t.Fatalf("revoked owner update = %v, want ErrNoRows", err)
	}
	if err := svc.Delete(ctx, revokedOwnerP, bot.ID); !errors.Is(err, sql.ErrNoRows) {
		t.Fatalf("revoked owner delete = %v, want ErrNoRows", err)
	}
}

// runWorkerFixture wires the real postgres seams (chatbots, runs, jobs) with
// the scripted executor, so worker admission and every privileged refresh
// exercise the CURRENT bot row, never the frozen snapshot.
type runWorkerFixture struct {
	svc      *chatbot.Service
	chatRepo *postgres.ChatbotRepo
	runsRepo *postgres.ChatbotRunsRepo
	jobSvc   *jobs.Service
	executor *scriptExecutor
	tenant   string
	botID    string
}

func newRunWorkerFixture(t *testing.T, responses []string, onDispatch func(n int)) *runWorkerFixture {
	t.Helper()
	return newRunWorkerFixtureWithRAG(t, nil, responses, onDispatch)
}

// newRunWorkerFixtureWithRAG wires the run worker with a fake RAG searcher
// so evidence-attached runs exercise ResolveEvidence + ReadEvidence paths.
func newRunWorkerFixtureWithRAG(t *testing.T, searcher *fakeRAGSearcher, responses []string, onDispatch func(n int)) *runWorkerFixture {
	t.Helper()
	db := testdb.New(t)
	tenant := testdb.SeedTenant(t, db, "run-aud")
	chatRepo := postgres.NewChatbotRepo(db)
	runsRepo := postgres.NewChatbotRunsRepo(db)
	jobSvc := jobs.NewService(postgres.NewJobsRepo(db), 0)
	executor := &scriptExecutor{responses: responses, onDispatch: onDispatch}
	svc := chatbot.NewService(chatRepo, postgres.NewPromptsRepo(db), searcher, executor)
	svc.ConfigureRuns(runsRepo, jobSvc, nil, runPrincipalResolver{})
	return &runWorkerFixture{svc: svc, chatRepo: chatRepo, runsRepo: runsRepo, jobSvc: jobSvc, executor: executor, tenant: tenant}
}

// scriptExecutor is the model-dispatch seam: scripted response bodies plus a
// hook that runs at dispatch time (e.g. a concurrent audience edit). The
// observed route fields mirror what inference.Result reports in production.
type scriptExecutor struct {
	responses        []string
	onDispatch       func(n int)
	dispatches       int32
	observedEndpoint string
	observedModel    string
	requests         []inference.Request
}

func (e *scriptExecutor) Execute(ctx context.Context, p auth.Principal, req inference.Request) (*inference.Result, error) {
	n := atomic.AddInt32(&e.dispatches, 1)
	if e.onDispatch != nil {
		e.onDispatch(int(n))
	}
	e.requests = append(e.requests, req)
	if len(e.responses) == 0 {
		return nil, errors.New("no scripted model response left")
	}
	body := e.responses[0]
	e.responses = e.responses[1:]
	return &inference.Result{Body: io.NopCloser(strings.NewReader(body)), EndpointID: e.observedEndpoint, Model: e.observedModel}, nil
}

// runPrincipalResolver plays the current-authority seam: CurrentPrincipal
// returns the frozen principal unchanged (per-request grant rebuild happens
// upstream of the worker), PrincipalForActor resolves the assistant owner.
type runPrincipalResolver struct{}

func (runPrincipalResolver) CurrentPrincipal(ctx context.Context, frozen auth.Principal) (auth.Principal, error) {
	return frozen, nil
}

func (runPrincipalResolver) PrincipalForActor(ctx context.Context, tenantID, actorID string) (auth.Principal, error) {
	return auth.Principal{TenantID: tenantID, ActorID: actorID}, nil
}

const (
	runWorkerOwner  = "owner_actor_1"
	runWorkerMember = "member_actor_1"
	runWorkerDone   = `{"choices":[{"message":{"role":"assistant","content":"all done"},"finish_reason":"stop"}]}`
	runWorkerTool   = `{"choices":[{"message":{"role":"assistant","content":"","tool_calls":[{"id":"call-1","type":"function","function":{"name":"knowledge.search","arguments":"{\"query\":\"q\"}"}}]}}]}`
)

func runWorkerPrincipals(tenant string) (member, owner, admin auth.Principal) {
	member = auth.Principal{TenantID: tenant, ActorID: runWorkerMember, AllowedModelsAll: true, AllowedChatbotsAll: true, AllowedAgentsAll: true}
	owner = member
	owner.ActorID = runWorkerOwner
	admin = member
	admin.ActorID = "admin_actor_1"
	admin.Scopes = map[string]struct{}{auth.ScopePlatformAdmin: {}}
	return member, owner, admin
}

func seedAudienceBot(t *testing.T, repo *postgres.ChatbotRepo, tenant string, audience []string) chatbot.Instance {
	t.Helper()
	bot := chatbot.Instance{
		TenantID: tenant, ID: newRunTestID("cb"), ExternalID: newRunTestID("ext"),
		Name: "Audience Bot", Model: "chat-test-model", IsEnabled: true,
		OwnerActorID: runWorkerOwner, Audience: audience,
	}
	if err := repo.Create(context.Background(), bot); err != nil {
		t.Fatalf("seed bot: %v", err)
	}
	return bot
}

func newRunTestID(prefix string) string {
	return prefix + "_" + fmt.Sprint(time.Now().UnixNano())
}

// narrowAudience simulates the concurrent admin edit: the member leaves the
// CURRENT stored audience (explicit [] = locked to owner+platform.admin).
func narrowAudience(t *testing.T, repo *postgres.ChatbotRepo, tenant, botID string) {
	t.Helper()
	bot, err := repo.GetByID(context.Background(), tenant, botID)
	if err != nil {
		t.Fatalf("reload bot: %v", err)
	}
	bot.Audience = []string{}
	if err := repo.Update(context.Background(), bot); err != nil {
		t.Fatalf("narrow audience: %v", err)
	}
}

func leaseRunJob(t *testing.T, jobSvc *jobs.Service) *jobs.Claim {
	t.Helper()
	claim, err := jobSvc.Lease(context.Background(), "worker-aud-test", time.Minute)
	if err != nil {
		t.Fatalf("lease job: %v", err)
	}
	if claim == nil {
		t.Fatal("no runnable job leased")
	}
	return claim
}

// TestRunWorkerMidRunAudienceRemovalAbortsBeforeDispatch: the member leaves
// the audience while the FIRST model dispatch is in flight, before its tool
// call may execute. The next refreshPrincipal must fail the run so that
// NEITHER the tool call NOR any further model dispatch executes.
func TestRunWorkerMidRunAudienceRemovalAbortsBeforeDispatch(t *testing.T) {
	ctx := context.Background()
	var f *runWorkerFixture
	f = newRunWorkerFixture(t, []string{runWorkerTool}, func(n int) {
		if n == 1 {
			narrowAudience(t, f.chatRepo, f.tenant, f.botID)
		}
	})
	member, _, _ := runWorkerPrincipals(f.tenant)
	bot := seedAudienceBot(t, f.chatRepo, f.tenant, []string{runWorkerMember})
	f.botID = bot.ID

	run, err := f.svc.StartRun(ctx, member, bot.ID, chatbot.AssistantRunInput{Input: "use the tool"})
	if err != nil {
		t.Fatalf("start run: %v", err)
	}

	_, handlerErr := f.svc.JobHandler()(ctx, leaseRunJob(t, f.jobSvc), nil)
	if handlerErr == nil {
		t.Fatal("mid-run audience removal must fail the run")
	}
	if got := atomic.LoadInt32(&f.executor.dispatches); got != 1 {
		t.Fatalf("post-removal dispatches = %d, want exactly the first dispatch", got)
	}
	attempts, err := f.runsRepo.ListRunAttempts(ctx, f.tenant, run.ID)
	if err != nil || len(attempts) != 1 || attempts[0].Kind != "model" || attempts[0].Status != "succeeded" {
		t.Fatalf("attempt rows after removal = %#v err=%v, want one succeeded model attempt and zero tool rows", attempts, err)
	}
	stored, err := f.runsRepo.GetRun(ctx, f.tenant, run.ID)
	if err != nil || stored.Status != jobs.StatusFailed || !strings.Contains(stored.Error, "not allowed") {
		t.Fatalf("failed run row = %#v err=%v, want failed with denial error", stored, err)
	}
}

// TestRunWorkerAdmissionRejectsAudienceRemoval: a run queued by an audience
// member whose audience is removed before the worker claims the job fails
// at admission — the frozen snapshot never authorizes, and the run row
// never reaches running.
func TestRunWorkerAdmissionRejectsAudienceRemoval(t *testing.T) {
	ctx := context.Background()
	f := newRunWorkerFixture(t, []string{runWorkerDone}, nil)
	member, _, _ := runWorkerPrincipals(f.tenant)
	bot := seedAudienceBot(t, f.chatRepo, f.tenant, []string{runWorkerMember})

	run, err := f.svc.StartRun(ctx, member, bot.ID, chatbot.AssistantRunInput{Input: "queued then narrowed"})
	if err != nil {
		t.Fatalf("start run: %v", err)
	}
	narrowAudience(t, f.chatRepo, f.tenant, bot.ID)

	_, handlerErr := f.svc.JobHandler()(ctx, leaseRunJob(t, f.jobSvc), nil)
	if !errors.Is(handlerErr, chatbot.ErrChatbotNotAllowed) {
		t.Fatalf("admission error = %v, want ErrChatbotNotAllowed", handlerErr)
	}
	if got := atomic.LoadInt32(&f.executor.dispatches); got != 0 {
		t.Fatalf("denied run dispatched %d times", got)
	}
	if attempts, err := f.runsRepo.ListRunAttempts(ctx, f.tenant, run.ID); err != nil || len(attempts) != 0 {
		t.Fatalf("denied run recorded attempts: %#v err=%v", attempts, err)
	}
	// The terminal-exit wrap marks the run failed under the same fencing
	// rules — the persisted status stays truthful for retained reads.
	stored, err := f.runsRepo.GetRun(ctx, f.tenant, run.ID)
	if err != nil || stored.Status != jobs.StatusFailed || !strings.Contains(stored.Error, "not allowed") {
		t.Fatalf("pre-dispatch failed run row = %#v err=%v, want failed with denial error", stored, err)
	}
}

// TestRetainedRunReadsEnforceCurrentAudience: after a completed run, removal
// from the audience hides the retained run, its attempts and the whole
// conversation replay from the former member; owner and platform.admin keep
// access regardless of the audience term.
func TestRetainedRunReadsEnforceCurrentAudience(t *testing.T) {
	ctx := context.Background()
	f := newRunWorkerFixture(t, []string{runWorkerDone}, nil)
	member, owner, admin := runWorkerPrincipals(f.tenant)
	bot := seedAudienceBot(t, f.chatRepo, f.tenant, []string{runWorkerMember})

	run, err := f.svc.StartRun(ctx, member, bot.ID, chatbot.AssistantRunInput{Input: "completed then hidden"})
	if err != nil {
		t.Fatalf("start run: %v", err)
	}
	completedID, handlerErr := f.svc.JobHandler()(ctx, leaseRunJob(t, f.jobSvc), nil)
	if handlerErr != nil || completedID != run.ID {
		t.Fatalf("handler = %q err=%v, want completed run", completedID, handlerErr)
	}
	if _, err := f.svc.GetRun(ctx, member, run.ID); err != nil {
		t.Fatalf("member read before removal: %v", err)
	}
	if attempts, err := f.svc.RunAttempts(ctx, member, run.ID); err != nil || len(attempts) != 1 {
		t.Fatalf("attempts before removal = %#v err=%v", attempts, err)
	}

	narrowAudience(t, f.chatRepo, f.tenant, bot.ID)

	if _, err := f.svc.GetRun(ctx, member, run.ID); !errors.Is(err, chatbot.ErrRunNotFound) {
		t.Fatalf("GetRun after removal = %v, want ErrRunNotFound", err)
	}
	if _, err := f.svc.RunAttempts(ctx, member, run.ID); !errors.Is(err, chatbot.ErrRunNotFound) {
		t.Fatalf("RunAttempts after removal = %v, want ErrRunNotFound", err)
	}
	if _, err := f.svc.ListConversation(ctx, member, run.ConversationID); !errors.Is(err, sql.ErrNoRows) {
		t.Fatalf("replay after removal = %v, want not-found", err)
	}
	if _, err := f.svc.GetRun(ctx, admin, run.ID); err != nil {
		t.Fatalf("platform.admin read after removal: %v", err)
	}
	// The bot owner runs and reads even though they never joined the
	// audience — the audience term never restricts the owner.
	ownerRun, err := f.svc.StartRun(ctx, owner, bot.ID, chatbot.AssistantRunInput{Input: "owner still allowed"})
	if err != nil {
		t.Fatalf("owner start run on locked bot: %v", err)
	}
	if _, err := f.svc.GetRun(ctx, owner, ownerRun.ID); err != nil {
		t.Fatalf("owner read on locked bot: %v", err)
	}
}

// TestRunWorkerPositivePathWithAudienceMember: an audience member in good
// standing runs to completion with real attempt rows.
func TestRunWorkerPositivePathWithAudienceMember(t *testing.T) {
	ctx := context.Background()
	f := newRunWorkerFixture(t, []string{runWorkerDone}, nil)
	member, _, _ := runWorkerPrincipals(f.tenant)
	bot := seedAudienceBot(t, f.chatRepo, f.tenant, []string{runWorkerMember})

	run, err := f.svc.StartRun(ctx, member, bot.ID, chatbot.AssistantRunInput{Input: "hello assistant"})
	if err != nil {
		t.Fatalf("start run: %v", err)
	}
	if run.Status != jobs.StatusQueued {
		t.Fatalf("queued status = %q", run.Status)
	}
	completedID, handlerErr := f.svc.JobHandler()(ctx, leaseRunJob(t, f.jobSvc), nil)
	if handlerErr != nil || completedID != run.ID {
		t.Fatalf("handler = %q err=%v, want completed run", completedID, handlerErr)
	}
	// The direct handler call leaves the JOB transition to the runner; the
	// run row itself is the worker's outcome record.
	got, err := f.runsRepo.GetRun(ctx, f.tenant, run.ID)
	if err != nil {
		t.Fatalf("get completed run: %v", err)
	}
	if got.Status != jobs.StatusSucceeded || got.Output != "all done" {
		t.Fatalf("completed run = %#v", got)
	}
	if _, err := f.svc.GetRun(ctx, member, run.ID); err != nil {
		t.Fatalf("member read of completed run: %v", err)
	}
	if attempts, err := f.svc.RunAttempts(ctx, member, run.ID); err != nil || len(attempts) != 1 || attempts[0].Status != "succeeded" {
		t.Fatalf("attempts = %#v err=%v", attempts, err)
	}
}

// TestAssistantAudienceIntersectsWithGrants: the stored audience is an
// additional restriction on top of D-25 grants — never an expansion. Owner
// and platform.admin bypass the audience term only; grants still bind.
func TestAssistantAudienceIntersectsWithGrants(t *testing.T) {
	ctx := context.Background()
	db := testdb.New(t)
	repo := postgres.NewIdentityRepo(db)
	tenant := testdb.SeedTenant(t, db, "aud-chat")
	authSvc := auth.NewService(repo, "")
	svc := chatbot.NewService(postgres.NewChatbotRepo(db), postgres.NewPromptsRepo(db), nil, nil)
	svc.ConfigureRuns(postgres.NewChatbotRunsRepo(db), jobs.NewService(postgres.NewJobsRepo(db), 0), nil, authSvc)

	creator := testdb.SeedUserWithGrants(t, db, tenant, "aud-creator@example.com",
		[]string{auth.ScopePlatformAdmin},
		auth.ResourceGrants{Models: auth.Allowlist{All: true}, Assistants: auth.Allowlist{All: true}})
	creatorP, err := authSvc.PrincipalForActor(ctx, tenant, creator.ID)
	if err != nil {
		t.Fatalf("creator principal: %v", err)
	}
	userFor := func(t *testing.T, email string, scopes []string, grants auth.ResourceGrants) auth.User {
		t.Helper()
		return testdb.SeedUserWithGrants(t, db, tenant, email, scopes, grants)
	}
	principalOf := func(t *testing.T, user auth.User) auth.Principal {
		t.Helper()
		p, err := authSvc.PrincipalForActor(ctx, tenant, user.ID)
		if err != nil {
			t.Fatalf("principal for %s: %v", user.Email, err)
		}
		return p
	}
	memberScopes := []string{"chatbot"}
	modelsAll := auth.ResourceGrants{Models: auth.Allowlist{All: true}}
	memberGrantedUser := userFor(t, "aud-member@example.com", memberScopes,
		auth.ResourceGrants{Models: auth.Allowlist{All: true}, Assistants: auth.Allowlist{Items: []string{"set-at-create"}}})
	memberGranted := principalOf(t, memberGrantedUser)
	memberAllGrant := principalOf(t, userFor(t, "aud-member-all@example.com", memberScopes,
		auth.ResourceGrants{Models: auth.Allowlist{All: true}, Assistants: auth.Allowlist{All: true}}))
	memberNoGrant := principalOf(t, userFor(t, "aud-member-nogrant@example.com", memberScopes, modelsAll))
	adminAll := principalOf(t, userFor(t, "aud-admin@example.com", []string{auth.ScopePlatformAdmin},
		auth.ResourceGrants{Models: auth.Allowlist{All: true}, Assistants: auth.Allowlist{All: true}}))

	// botShared admits exactly the granted member; botLocked is explicit [];
	// botWildcard is tenant-wide. Omitted audience defaults to owner-only.
	botShared, err := svc.Create(ctx, creatorP, chatbot.CreateChatbotInput{
		Name: "Shared Bot", Model: "chat-test-model",
		Audience: []string{memberGranted.ActorID},
	})
	if err != nil {
		t.Fatalf("create shared bot: %v", err)
	}
	botLocked, err := svc.Create(ctx, creatorP, chatbot.CreateChatbotInput{
		Name: "Locked Bot", Model: "chat-test-model", Audience: []string{},
	})
	if err != nil {
		t.Fatalf("create locked bot: %v", err)
	}
	botWildcard, err := svc.Create(ctx, creatorP, chatbot.CreateChatbotInput{
		Name: "Wildcard Bot", Model: "chat-test-model", Audience: []string{"*"},
	})
	if err != nil {
		t.Fatalf("create wildcard bot: %v", err)
	}
	botDefault, err := svc.Create(ctx, creatorP, chatbot.CreateChatbotInput{
		Name: "Default Bot", Model: "chat-test-model",
	})
	if err != nil {
		t.Fatalf("create default bot: %v", err)
	}
	// Point the granted member's item grant at the real assistant.
	if _, err := db.SQL.ExecContext(ctx,
		`UPDATE roles SET allowed_assistants = $3::jsonb
		 WHERE tenant_id = $1::uuid AND id IN (SELECT role_id FROM user_roles WHERE user_id = $2)`,
		tenant, memberGrantedUser.ID, `{"mode":"list","items":["`+botShared.ID+`"]}`); err != nil {
		t.Fatalf("repoint grant: %v", err)
	}
	memberGranted = principalOf(t, memberGrantedUser)

	// Stored audiences round-trip.
	for _, want := range []struct {
		id       string
		name     string
		audience []string
	}{
		{botShared.ID, botShared.Name, []string{memberGranted.ActorID}},
		{botLocked.ID, botLocked.Name, []string{}},
		{botWildcard.ID, botWildcard.Name, []string{"*"}},
		{botDefault.ID, botDefault.Name, []string{creatorP.ActorID}},
	} {
		got, err := svc.Get(ctx, creatorP, want.id)
		if err != nil {
			t.Fatalf("get %s: %v", want.name, err)
		}
		if fmt.Sprint(got.Audience) != fmt.Sprint(want.audience) {
			t.Fatalf("%s audience = %#v, want %#v", want.name, got.Audience, want.audience)
		}
	}

	// A real item grant INSIDE the audience succeeds everywhere. The member
	// sees exactly the shared bot: the wildcard bot still needs its own
	// grant — the audience never expands the grant side.
	list, err := svc.List(ctx, memberGranted)
	if err != nil || len(list) != 1 || list[0].ID != botShared.ID {
		t.Fatalf("granted member list = %#v err=%v, want exactly the shared bot", list, err)
	}
	if _, err := svc.Get(ctx, memberGranted, botShared.ID); err != nil {
		t.Fatalf("granted member get: %v", err)
	}
	if _, err := svc.StartRun(ctx, memberGranted, botShared.ID, chatbot.AssistantRunInput{Input: "hello"}); err != nil {
		t.Fatalf("audience member start run (fixture must really run): %v", err)
	}

	// The same member outside a restrictive audience is denied on Get, run
	// and list even though the item grant matches... the locked bot admits
	// nobody but owner/admin.
	if _, err := svc.Get(ctx, memberGranted, botLocked.ID); !errors.Is(err, sql.ErrNoRows) {
		t.Fatalf("locked bot get = %v, want ErrNoRows", err)
	}
	if _, err := svc.StartRun(ctx, memberGranted, botLocked.ID, chatbot.AssistantRunInput{Input: "no"}); !errors.Is(err, chatbot.ErrChatbotNotAllowed) {
		t.Fatalf("locked bot start run = %v, want ErrChatbotNotAllowed", err)
	}
	// A role-level ALL-assistants grant does not bypass a restrictive
	// audience either.
	if list, err := svc.List(ctx, memberAllGrant); err != nil || len(list) != 1 || list[0].ID != botWildcard.ID {
		t.Fatalf("all-grant member list = %#v err=%v, want only the wildcard bot", list, err)
	}
	if _, err := svc.Get(ctx, memberAllGrant, botLocked.ID); !errors.Is(err, sql.ErrNoRows) {
		t.Fatalf("all-grant member locked get = %v, want ErrNoRows", err)
	}
	if _, err := svc.StartRun(ctx, memberAllGrant, botLocked.ID, chatbot.AssistantRunInput{Input: "no"}); !errors.Is(err, chatbot.ErrChatbotNotAllowed) {
		t.Fatalf("all-grant member locked start run = %v, want ErrChatbotNotAllowed", err)
	}
	// Audience alone never expands grants: the wildcard bot still denies an
	// ungranted member.
	if _, err := svc.Get(ctx, memberNoGrant, botWildcard.ID); !errors.Is(err, sql.ErrNoRows) {
		t.Fatalf("ungranted member wildcard get = %v, want ErrNoRows", err)
	}
	if _, err := svc.StartRun(ctx, memberNoGrant, botWildcard.ID, chatbot.AssistantRunInput{Input: "no"}); !errors.Is(err, chatbot.ErrChatbotNotAllowed) {
		t.Fatalf("ungranted member wildcard start run = %v, want ErrChatbotNotAllowed", err)
	}
	// platform.admin WITH the stored grant bypasses the audience term.
	if _, err := svc.Get(ctx, adminAll, botLocked.ID); err != nil {
		t.Fatalf("granted admin locked get: %v", err)
	}

	// Owner mutates the audience; empty entries are rejected on the 400 path.
	if _, err := svc.Update(ctx, creatorP, botLocked.ID, chatbot.UpdateChatbotInput{Audience: []string{" new-member ", "new-member", "*"}}); err != nil {
		t.Fatalf("owner audience update: %v", err)
	}
	got, err := svc.Get(ctx, creatorP, botLocked.ID)
	if err != nil || fmt.Sprint(got.Audience) != fmt.Sprint([]string{"new-member", "*"}) {
		t.Fatalf("trimmed/deduped audience = %#v err=%v", got.Audience, err)
	}
	if _, err := svc.Update(ctx, creatorP, botLocked.ID, chatbot.UpdateChatbotInput{Audience: []string{"valid", ""}}); !errors.Is(err, chatbot.ErrInvalidInput) {
		t.Fatalf("empty audience entry = %v, want ErrInvalidInput", err)
	}
	if _, err := svc.Create(ctx, creatorP, chatbot.CreateChatbotInput{
		Name: "Bad Bot", Model: "chat-test-model", Audience: []string{""},
	}); !errors.Is(err, chatbot.ErrInvalidInput) {
		t.Fatalf("empty audience create = %v, want ErrInvalidInput", err)
	}
	// Nil audience on update leaves the stored audience unchanged.
	if _, err := svc.Update(ctx, creatorP, botLocked.ID, chatbot.UpdateChatbotInput{Name: new("Locked Bot Renamed")}); err != nil {
		t.Fatalf("nil-audience update: %v", err)
	}
	if got, err := svc.Get(ctx, creatorP, botLocked.ID); err != nil || fmt.Sprint(got.Audience) != fmt.Sprint([]string{"new-member", "*"}) {
		t.Fatalf("audience changed on nil update = %#v err=%v", got.Audience, err)
	}
}

// seedConversation is the direct-row seam for list tests: conversations and
// messages with explicit ownership, bypassing the run/completion writers.
func seedConversation(t *testing.T, f *chatFixture, chatbotID, owner, firstUserContent string) chatbot.Conversation {
	t.Helper()
	ctx := context.Background()
	conv := chatbot.Conversation{
		TenantID: f.tenantA, ID: "conv_" + newRunTestID("id"), ChatbotID: chatbotID, OwnerActorID: owner,
	}
	if err := f.chatRepo.CreateConversation(ctx, conv); err != nil {
		t.Fatalf("seed conversation: %v", err)
	}
	if firstUserContent != "" {
		if err := f.chatRepo.AddMessage(ctx, chatbot.Message{
			TenantID: f.tenantA, ID: newRunTestID("msg"), ConversationID: conv.ID,
			Role: "user", Content: firstUserContent, OwnerActorID: owner,
		}); err != nil {
			t.Fatalf("seed message: %v", err)
		}
	}
	return conv
}

// TestServiceListConversationsOwnScopeVisibilityAndTitles covers the OA-13
// service contract: own-only for members AND admins, hidden-assistant rows
// dropped, evidence-visibility-suppressed titles, newest-first keyset
// pagination and 400-class malformed cursors.
func TestServiceListConversationsOwnScopeVisibilityAndTitles(t *testing.T) {
	ctx := context.Background()
	searcher := &fakeRAGSearcher{evidenceID: "evidence-current"}
	f := newChatFixture(t, searcher, nil)
	creator := openPrincipal(f.tenantA)
	member := openPrincipal(f.tenantA)
	member.ActorID = "member_actor"
	member.AllowedChatbots = map[string]struct{}{}
	admin := adminPrincipal(f.tenantA)

	botVisible, err := f.svc.Create(ctx, creator, chatbot.CreateChatbotInput{
		Name: "Visible Bot", Model: "chat-test-model", Audience: []string{member.ActorID},
	})
	if err != nil {
		t.Fatalf("create visible bot: %v", err)
	}
	botHidden, err := f.svc.Create(ctx, creator, chatbot.CreateChatbotInput{
		Name: "Hidden Bot", Model: "chat-test-model", Audience: []string{},
	})
	if err != nil {
		t.Fatalf("create hidden bot: %v", err)
	}
	member.AllowedChatbots = map[string]struct{}{botVisible.ID: {}, botHidden.ID: {}}

	convVisible := seedConversation(t, f, botVisible.ID, member.ActorID, "first visible message")
	convHidden := seedConversation(t, f, botHidden.ID, member.ActorID, "hidden history")
	convOther := seedConversation(t, f, botVisible.ID, "someone_else", "not mine")
	long := strings.Repeat("x", 120)
	convLong := seedConversation(t, f, botVisible.ID, member.ActorID, long)
	// The suppressed row's FIRST user message carries input refs without
	// evidence ids — it must never contribute preview text.
	convSuppressed := seedConversation(t, f, botVisible.ID, member.ActorID, "")
	if err := f.chatRepo.AddMessage(ctx, chatbot.Message{
		TenantID: f.tenantA, ID: newRunTestID("msg"), ConversationID: convSuppressed.ID,
		Role: "user", Content: "secret preview", OwnerActorID: member.ActorID,
		InputRefs: []source.ObjectRef{{ObjectID: "doc_1"}},
	}); err != nil {
		t.Fatalf("seed suppressed message: %v", err)
	}
	// The revoked row's FIRST user message carries evidence ids; once the
	// evidence is revoked the preview must vanish.
	convRevoked := seedConversation(t, f, botVisible.ID, member.ActorID, "")
	if err := f.chatRepo.AddMessage(ctx, chatbot.Message{
		TenantID: f.tenantA, ID: newRunTestID("msg"), ConversationID: convRevoked.ID,
		Role: "user", Content: "evidence-backed preview", OwnerActorID: member.ActorID,
		EvidenceIDs: []string{"evidence-current"},
	}); err != nil {
		t.Fatalf("seed evidence message: %v", err)
	}

	// Own-only: the member sees exactly their visible-bot conversations.
	summaries, next, err := f.svc.ListConversations(ctx, member, 50, "")
	if err != nil || next != "" {
		t.Fatalf("member list = %#v next=%q err=%v", summaries, next, err)
	}
	byID := map[string]chatbot.ConversationSummary{}
	for _, summary := range summaries {
		byID[summary.ID] = summary
	}
	if _, ok := byID[convHidden.ID]; ok {
		t.Fatal("hidden-assistant conversation leaked into the list")
	}
	if _, ok := byID[convOther.ID]; ok {
		t.Fatal("another member's conversation leaked into the list")
	}
	visible := byID[convVisible.ID]
	if visible.ChatbotID != botVisible.ID || visible.AssistantName != "Visible Bot" || visible.Title != "first visible message" {
		t.Fatalf("visible summary = %#v", visible)
	}
	if visible.UpdatedAt.Before(visible.CreatedAt) {
		t.Fatalf("updated_at %v before created_at %v", visible.UpdatedAt, visible.CreatedAt)
	}
	if got := byID[convLong.ID].Title; len([]rune(got)) != 80 {
		t.Fatalf("long title truncated to %d runes, want 80", len([]rune(got)))
	}
	// InputRefs without evidence IDs never contribute preview text.
	if got := byID[convSuppressed.ID].Title; got != "" {
		t.Fatalf("suppressed title = %q, want empty", got)
	}
	// Evidence-backed previews vanish once the evidence is revoked.
	if got := byID[convRevoked.ID].Title; got != "evidence-backed preview" {
		t.Fatalf("evidence-backed title = %q", got)
	}
	searcher.revoked = true
	summaries, _, err = f.svc.ListConversations(ctx, member, 50, "")
	if err != nil {
		t.Fatalf("member list after revocation: %v", err)
	}
	for _, summary := range summaries {
		if summary.ID == convRevoked.ID && summary.Title != "" {
			t.Fatalf("revoked-evidence title = %q, want empty", summary.Title)
		}
	}

	// Own-only for admins too: the admin has no conversations of their own.
	adminSummaries, _, err := f.svc.ListConversations(ctx, admin, 20, "")
	if err != nil || len(adminSummaries) != 0 {
		t.Fatalf("admin list = %#v err=%v, want own-only empty", adminSummaries, err)
	}

	// Keyset pagination: newest-first, disjoint pages, deterministic end.
	newest := seedConversation(t, f, botVisible.ID, member.ActorID, "newest one")
	if err := bumpConversationCreatedAt(t, f, []string{convVisible.ID, convHidden.ID, convLong.ID, convSuppressed.ID, convRevoked.ID}); err != nil {
		t.Fatalf("age conversations: %v", err)
	}
	var paged []string
	cursor := ""
	for range 10 {
		summaries, next, err := f.svc.ListConversations(ctx, member, 2, cursor)
		if err != nil {
			t.Fatalf("list page: %v", err)
		}
		for _, summary := range summaries {
			paged = append(paged, summary.ID)
		}
		if next == "" {
			break
		}
		cursor = next
	}
	// Rows aged together share created_at; ties break by id DESC, and ids
	// are allocated from an increasing clock — so the aged block reads in
	// reverse seed order. The hidden bot's conversation never appears.
	wantOrder := []string{newest.ID, convRevoked.ID, convSuppressed.ID, convLong.ID, convVisible.ID}
	if fmt.Sprint(paged) != fmt.Sprint(wantOrder) {
		t.Fatalf("paged order = %v, want %v", paged, wantOrder)
	}
	seen := map[string]bool{}
	for _, id := range paged {
		if seen[id] {
			t.Fatalf("conversation %s appeared on two pages", id)
		}
		seen[id] = true
	}
	if _, _, err := f.svc.ListConversations(ctx, member, 20, "not-a-cursor"); !errors.Is(err, chatbot.ErrInvalidInput) {
		t.Fatalf("malformed cursor = %v, want ErrInvalidInput", err)
	}
}

// bumpConversationCreatedAt ages conversations into the past so the seeded
// newest row sorts first; rows aged together share created_at, making the
// (created_at, id) tie-break deterministic.
func bumpConversationCreatedAt(t *testing.T, f *chatFixture, ids []string) error {
	t.Helper()
	_, err := f.db.SQL.ExecContext(context.Background(),
		`UPDATE conversations SET created_at = '2020-01-01T00:00:00Z'::timestamptz
		 WHERE tenant_id = $1::uuid AND id = ANY($2)`, f.tenantA, ids)
	return err
}

// TestListAssistantToolBindingsVisibility covers the OA-26 used_by read:
// only assistants the caller may see (grant AND audience) contribute names,
// and unknown tools return an empty binding list.
func TestListAssistantToolBindingsVisibility(t *testing.T) {
	ctx := context.Background()
	f := newChatFixture(t, nil, nil)
	creator := openPrincipal(f.tenantA)
	member := openPrincipal(f.tenantA)
	member.ActorID = "member_actor"
	member.AllowedChatbotsAll = false
	member.AllowedAgentsAll = false

	botUsing, err := f.svc.Create(ctx, creator, chatbot.CreateChatbotInput{
		Name: "Tool Bot", Model: "chat-test-model", Audience: []string{member.ActorID},
		Settings: map[string]any{"tools": []any{"mcp_search"}},
	})
	if err != nil {
		t.Fatalf("create tool bot: %v", err)
	}
	if _, err := f.svc.Create(ctx, creator, chatbot.CreateChatbotInput{
		Name: "Hidden Tool Bot", Model: "chat-test-model", Audience: []string{},
		Settings: map[string]any{"tools": []any{"mcp_search"}},
	}); err != nil {
		t.Fatalf("create hidden tool bot: %v", err)
	}

	// An ungranted member sees nothing, even for a tool they "know".
	bindings, err := f.svc.ListAssistantToolBindings(ctx, member, []string{"mcp_search", "other_tool"})
	if err != nil {
		t.Fatalf("ungranted bindings: %v", err)
	}
	if len(bindings) != 0 {
		t.Fatalf("ungranted member bindings = %#v", bindings)
	}

	member.AllowedChatbots = map[string]struct{}{botUsing.ID: {}}
	bindings, err = f.svc.ListAssistantToolBindings(ctx, member, []string{"mcp_search", "other_tool"})
	if err != nil {
		t.Fatalf("granted bindings: %v", err)
	}
	if got := bindings["mcp_search"]; len(got) != 1 || got[0] != "Tool Bot" {
		t.Fatalf("mcp_search bindings = %#v, want exactly the visible tool bot", got)
	}
	if _, ok := bindings["other_tool"]; ok {
		t.Fatalf("unknown tool leaked a binding: %#v", bindings)
	}

	// The owner sees both configured bots for the tool.
	ownerBindings, err := f.svc.ListAssistantToolBindings(ctx, creator, []string{"mcp_search"})
	if err != nil || len(ownerBindings["mcp_search"]) != 2 {
		t.Fatalf("owner bindings = %#v err=%v, want both bots", ownerBindings, err)
	}
}

// ---------------------------------------------------------------------
// OA-14: ordered run steps, evidence-attached runs, truthful route.

// startAndRun enqueues a run and executes it through the REAL worker
// handler on a leased claim, returning the run and the handler error.
func startAndRun(t *testing.T, f *runWorkerFixture, p auth.Principal, botID string, input chatbot.AssistantRunInput) (chatbot.AssistantRun, error) {
	t.Helper()
	run, err := f.svc.StartRun(context.Background(), p, botID, input)
	if err != nil {
		t.Fatalf("start run: %v", err)
	}
	_, handlerErr := f.svc.JobHandler()(context.Background(), leaseRunJob(t, f.jobSvc), nil)
	return run, handlerErr
}

// TestRunEvidenceHandoffFlowsToExecutorAndReplays: an authorized
// evidence_id is resolved server-side at start, persisted on run and user
// message, re-read at worker time through the CURRENT-passage reader, and
// its canonical object ref reaches the executor. The run reports the route
// actually dispatched and replays ordered steps.
func TestRunEvidenceHandoffFlowsToExecutorAndReplays(t *testing.T) {
	ctx := context.Background()
	searcher := &fakeRAGSearcher{evidenceID: "ev_attached", evidenceContent: "attached-passage-bytes"}
	f := newRunWorkerFixtureWithRAG(t, searcher, []string{runWorkerDone}, nil)
	f.executor.observedEndpoint, f.executor.observedModel = "ep-observed", "upstream-model-x"
	member, _, _ := runWorkerPrincipals(f.tenant)
	bot := seedAudienceBot(t, f.chatRepo, f.tenant, []string{runWorkerMember})

	run, handlerErr := startAndRun(t, f, member, bot.ID, chatbot.AssistantRunInput{
		Input: "ask about the clause", EvidenceIDs: []string{"ev_attached"},
	})
	if handlerErr != nil {
		t.Fatalf("worker error: %v", handlerErr)
	}
	if len(f.executor.requests) != 1 {
		t.Fatalf("dispatches = %d", len(f.executor.requests))
	}
	req := f.executor.requests[0]
	if !strings.Contains(string(req.Payload), "attached-passage-bytes") {
		t.Fatalf("attached passage never reached the executor payload: %s", req.Payload)
	}
	canonical := false
	for _, ref := range req.Inputs {
		if ref.ObjectID == "doc_attached" && ref.ObservedSeq == 9 {
			canonical = true
		}
	}
	if !canonical {
		t.Fatalf("canonical read ref (doc_attached@9) missing from executor inputs: %#v", req.Inputs)
	}

	got, err := f.svc.GetRun(ctx, member, run.ID)
	if err != nil {
		t.Fatalf("get run: %v", err)
	}
	if got.EndpointID != "ep-observed" || got.Model != "upstream-model-x" {
		t.Fatalf("observed route = %q/%q, want the dispatched endpoint/model", got.EndpointID, got.Model)
	}
	if len(got.Steps) != 1 || got.Steps[0].Number != 1 || got.Steps[0].Kind != "model" || got.Steps[0].Status != "succeeded" {
		t.Fatalf("steps = %#v, want one succeeded ordered model step", got.Steps)
	}
	if strings.Contains(got.Steps[0].Input, "attached-passage-bytes") || strings.Contains(got.Steps[0].Input, "system") {
		t.Fatalf("model step input carries prompt payload: %q", got.Steps[0].Input)
	}
	if got.Steps[0].Output != runWorkerDone {
		t.Fatalf("step output = %q", got.Steps[0].Output)
	}
	if got.Steps[0].EndedAt == nil {
		t.Fatal("ended step has no ended_at")
	}

	// Both turns link to the run via durable run_id — round-tripped through
	// the repo, never in-memory.
	messages, err := f.chatRepo.ListMessages(ctx, f.tenant, run.ConversationID)
	if err != nil || len(messages) != 2 {
		t.Fatalf("messages = %#v err=%v", messages, err)
	}
	for _, msg := range messages {
		if msg.RunID != run.ID {
			t.Fatalf("%s turn run_id = %q, want the durable linkage", msg.Role, msg.RunID)
		}
	}
	if fmt.Sprint(messages[0].EvidenceIDs) != fmt.Sprint([]string{"ev_attached"}) {
		t.Fatalf("user message evidence = %#v", messages[0].EvidenceIDs)
	}

	// Conversation replay attaches the same steps to the assistant turn.
	view, err := f.svc.ListConversation(ctx, member, run.ConversationID)
	if err != nil {
		t.Fatalf("replay: %v", err)
	}
	var assistant *chatbot.Message
	for i := range view.Messages {
		if view.Messages[i].Role == "assistant" {
			assistant = &view.Messages[i]
		}
	}
	if assistant == nil || assistant.RunID != run.ID || len(assistant.Steps) != 1 || assistant.Steps[0].Number != 1 {
		t.Fatalf("assistant turn steps = %#v", assistant)
	}
	// Attempts and steps stay one projection: RunAttempts delegates to it.
	attempts, err := f.svc.RunAttempts(ctx, member, run.ID)
	if err != nil || len(attempts) != 1 || attempts[0].Number != 1 || attempts[0].Kind != "model" {
		t.Fatalf("attempts = %#v err=%v", attempts, err)
	}
}

// TestRunMultiAttemptStepsStayOrdered checks the failed tool attempt and resumed model call.
func TestRunMultiAttemptStepsStayOrdered(t *testing.T) {
	ctx := context.Background()
	f := newRunWorkerFixture(t, []string{runWorkerTool, runWorkerDone}, nil)
	member, _, _ := runWorkerPrincipals(f.tenant)
	bot := seedAudienceBot(t, f.chatRepo, f.tenant, []string{runWorkerMember})

	run, handlerErr := startAndRun(t, f, member, bot.ID, chatbot.AssistantRunInput{Input: "use the tool"})
	if handlerErr != nil {
		t.Fatalf("run should continue after the tool error: %v", handlerErr)
	}
	got, err := f.svc.GetRun(ctx, member, run.ID)
	if err != nil {
		t.Fatalf("get run: %v", err)
	}
	if len(got.Steps) != 3 || got.Steps[0].Number != 1 || got.Steps[0].Kind != "model" || got.Steps[0].Status != "succeeded" || got.Steps[2].Number != 3 || got.Steps[2].Kind != "model" || got.Steps[2].Status != "succeeded" {
		t.Fatalf("steps = %#v, want model step first", got.Steps)
	}
	tool := got.Steps[1]
	if tool.Number != 2 || tool.Kind != "tool" || tool.Name != "knowledge.search" || tool.Status != "failed" || tool.Error == "" {
		t.Fatalf("tool step = %#v, want ordered failed tool step with error", tool)
	}
	if !strings.Contains(tool.Input, "knowledge.search") && !strings.Contains(tool.Input, `"query"`) {
		t.Fatalf("tool step input = %q, want the tool arguments", tool.Input)
	}
	if strings.Contains(tool.Input, "system") {
		t.Fatalf("tool step input carries prompt payload: %q", tool.Input)
	}
	if tool.EndedAt == nil {
		t.Fatal("finished tool step has no ended_at")
	}
}

// TestStartRunRejectsDeniedEvidence: unknown or denied evidence IDs fail the
// run start under the existing not-found/denied mapping — never stripped.
func TestStartRunRejectsDeniedEvidence(t *testing.T) {
	ctx := context.Background()
	searcher := &fakeRAGSearcher{evidenceID: "ev_attached"}
	f := newRunWorkerFixtureWithRAG(t, searcher, []string{runWorkerDone}, nil)
	member, _, _ := runWorkerPrincipals(f.tenant)
	bot := seedAudienceBot(t, f.chatRepo, f.tenant, []string{runWorkerMember})

	if _, err := f.svc.StartRun(ctx, member, bot.ID, chatbot.AssistantRunInput{Input: "hi", EvidenceIDs: []string{"ev_attached", "ev_other"}}); !errors.Is(err, source.ErrDenied) {
		t.Fatalf("denied evidence start = %v, want source.ErrDenied", err)
	}
	if got, _ := f.chatRepo.CountConversations(ctx, f.tenant, runWorkerMember); got != 0 {
		t.Fatalf("rejected run persisted %d conversations", got)
	}
}

// TestRunEvidenceRevokedBetweenEnqueueAndExecutionDropsPassage: revoking the
// attached evidence after StartRun drops the passage at worker read time —
// the executor payload proves the revoked text never entered context while
// the run still executes.
func TestRunEvidenceRevokedBetweenEnqueueAndExecutionDropsPassage(t *testing.T) {
	ctx := context.Background()
	searcher := &fakeRAGSearcher{evidenceID: "ev_attached", evidenceContent: "revoked-passage-secret"}
	f := newRunWorkerFixtureWithRAG(t, searcher, []string{runWorkerDone}, nil)
	member, _, _ := runWorkerPrincipals(f.tenant)
	bot := seedAudienceBot(t, f.chatRepo, f.tenant, []string{runWorkerMember})

	run, err := f.svc.StartRun(ctx, member, bot.ID, chatbot.AssistantRunInput{
		Input: "summarize", EvidenceIDs: []string{"ev_attached"},
	})
	if err != nil {
		t.Fatalf("start run: %v", err)
	}
	searcher.evidenceReadDenied = true

	if _, handlerErr := f.svc.JobHandler()(ctx, leaseRunJob(t, f.jobSvc), nil); handlerErr != nil {
		t.Fatalf("worker error: %v", handlerErr)
	}
	if len(f.executor.requests) != 1 {
		t.Fatalf("dispatches = %d", len(f.executor.requests))
	}
	if strings.Contains(string(f.executor.requests[0].Payload), "revoked-passage-secret") {
		t.Fatalf("revoked passage reached inference context: %s", f.executor.requests[0].Payload)
	}
	for _, ref := range f.executor.requests[0].Inputs {
		if ref.ObjectID == "doc_attached" && ref.ObservedSeq == 9 {
			t.Fatalf("revoked read ref reached executor inputs: %#v", f.executor.requests[0].Inputs)
		}
	}
	got, err := f.svc.GetRun(ctx, member, run.ID)
	if err != nil || got.Output != "all done" || got.Status == jobs.StatusFailed {
		t.Fatalf("run = %#v err=%v, want a completed run without the passage", got, err)
	}
}

// TestTerminalRunTurnsProjectAfterReload: a REAL failing run (executor error
// through the worker), a run cancelled WHILE QUEUED, and a PRE-DISPATCH
// admission failure each render a durable display-only assistant turn after
// conversation reload — with no persisted assistant message row, no
// fabricated reply, and no entry into future inference history.
func TestTerminalRunTurnsProjectAfterReload(t *testing.T) {
	ctx := context.Background()
	f := newRunWorkerFixture(t, nil, nil) // no scripted responses: the model dispatch itself fails
	member, _, admin := runWorkerPrincipals(f.tenant)
	bot := seedAudienceBot(t, f.chatRepo, f.tenant, []string{runWorkerMember})

	// 1. Real failing run: the executor errors through the worker.
	failed, handlerErr := startAndRun(t, f, member, bot.ID, chatbot.AssistantRunInput{Input: "will fail at dispatch"})
	if handlerErr == nil {
		t.Fatal("executor failure must fail the run")
	}
	stored, err := f.runsRepo.GetRun(ctx, f.tenant, failed.ID)
	if err != nil || stored.Status != jobs.StatusFailed {
		t.Fatalf("failed run row = %#v err=%v", stored, err)
	}

	// 2. Cancelled while queued: never claimed by any worker.
	cancelled, err := f.svc.StartRun(ctx, member, bot.ID, chatbot.AssistantRunInput{
		Input: "cancelled while queued", ConversationID: failed.ConversationID,
	})
	if err != nil {
		t.Fatalf("start cancelled run: %v", err)
	}
	if err := f.svc.CancelRun(ctx, member, cancelled.ID); err != nil {
		t.Fatalf("cancel queued run: %v", err)
	}

	// 3. Pre-dispatch failure: audience narrowed between enqueue and claim.
	preDispatch, err := f.svc.StartRun(ctx, member, bot.ID, chatbot.AssistantRunInput{
		Input: "denied at admission", ConversationID: failed.ConversationID,
	})
	if err != nil {
		t.Fatalf("start pre-dispatch run: %v", err)
	}
	narrowAudience(t, f.chatRepo, f.tenant, bot.ID)
	if _, handlerErr := f.svc.JobHandler()(ctx, leaseRunJob(t, f.jobSvc), nil); !errors.Is(handlerErr, chatbot.ErrChatbotNotAllowed) {
		t.Fatalf("pre-dispatch admission error = %v", handlerErr)
	}

	// The member lost audience access with the narrowing — the admin gate
	// (platform.admin bypasses the audience term) proves the durable read.
	view, err := f.svc.ListConversation(ctx, admin, failed.ConversationID)
	if err != nil {
		t.Fatalf("admin replay: %v", err)
	}
	wantRoles := []string{"user", "assistant", "user", "assistant", "user", "assistant"}
	if len(view.Messages) != len(wantRoles) {
		t.Fatalf("replayed messages = %d (%#v), want the %d-turn shape", len(view.Messages), view.Messages, len(wantRoles))
	}
	runIDs := map[string]string{failed.ID: "will fail at dispatch", cancelled.ID: "cancelled while queued", preDispatch.ID: "denied at admission"}
	for i, msg := range view.Messages {
		if msg.Role != wantRoles[i] {
			t.Fatalf("message[%d] role = %q, want %q", i, msg.Role, wantRoles[i])
		}
		if msg.Role != "assistant" {
			continue
		}
		if strings.TrimSpace(msg.Content) != "" {
			t.Fatalf("projected turn fabricated content: %q", msg.Content)
		}
		run, ok := runIDs[msg.RunID]
		if !ok {
			t.Fatalf("projected turn run_id %q is not one of the terminal runs", msg.RunID)
		}
		delete(runIDs, msg.RunID)
		_ = run
	}
	if len(runIDs) != 0 {
		t.Fatalf("terminal runs without a projected turn: %v", runIDs)
	}

	// No assistant message row exists for any terminal run — the turn is the
	// read-time projection only.
	persisted, err := f.chatRepo.ListMessages(ctx, f.tenant, failed.ConversationID)
	if err != nil {
		t.Fatalf("list persisted messages: %v", err)
	}
	for _, msg := range persisted {
		if msg.Role == "assistant" {
			t.Fatalf("terminal run persisted a fake assistant row: %#v", msg)
		}
	}

	// Steps replay via the shared loader: the failing run keeps its failed
	// model attempt; the never-claimed runs have none.
	turns := 0
	for _, msg := range view.Messages {
		if msg.Role != "assistant" {
			continue
		}
		turns++
		if msg.RunID == failed.ID {
			if len(msg.Steps) != 1 || msg.Steps[0].Status != "failed" || msg.Steps[0].Kind != "model" {
				t.Fatalf("failing run turn steps = %#v, want the failed model attempt", msg.Steps)
			}
		} else if len(msg.Steps) != 0 {
			t.Fatalf("unclaimed run projected steps: %#v", msg.Steps)
		}
	}
	if turns != 3 {
		t.Fatalf("projected turns = %d", turns)
	}

	// The projection never enters inference history: the next run's payload
	// carries only the persisted user turns.
	f.executor.responses = []string{runWorkerDone}
	if _, handlerErr := startAndRun(t, f, admin, bot.ID, chatbot.AssistantRunInput{
		Input: "follow-up question", ConversationID: failed.ConversationID,
	}); handlerErr != nil {
		t.Fatalf("follow-up run: %v", handlerErr)
	}
	payload := string(f.executor.requests[len(f.executor.requests)-1].Payload)
	for _, needle := range []string{"will fail at dispatch", "cancelled while queued", "denied at admission", "follow-up question"} {
		if !strings.Contains(payload, needle) {
			t.Fatalf("payload lost user input %q: %s", needle, payload)
		}
	}
	if strings.Contains(payload, `"role":"assistant"`) {
		t.Fatalf("projected turns entered inference history: %s", payload)
	}

	// All three runs appear in the own-scope failed-run read (the member's
	// own rows, derived from run row and bound job alike).
	failedRuns, err := f.svc.ListFailedRuns(ctx, member, 10)
	if err != nil {
		t.Fatalf("list failed runs: %v", err)
	}
	seen := map[string]bool{}
	for _, run := range failedRuns {
		seen[run.ID] = true
	}
	if !seen[failed.ID] || !seen[cancelled.ID] || !seen[preDispatch.ID] {
		t.Fatalf("failed-run feed = %#v, want all three terminal runs", failedRuns)
	}
	if count, err := f.svc.CountConversations(ctx, member); err != nil || count != 1 {
		t.Fatalf("member conversation count = %d err=%v, want the single own conversation", count, err)
	}
	if count, err := f.svc.CountConversations(ctx, admin); err != nil || count != 0 {
		t.Fatalf("admin conversation count = %d err=%v, want own rows only (zero)", count, err)
	}
}

// TestRevokedEvidenceTurnKeepsMetadataOnlySteps: a completed run whose
// evidence is later revoked keeps its turn at replay with the content
// cleared, the steps reduced to metadata (number/kind/name/status kept,
// input/output/error cleared), and neither text nor refs feeding the next
// run's inference context.
func TestRevokedEvidenceTurnKeepsMetadataOnlySteps(t *testing.T) {
	ctx := context.Background()
	searcher := &fakeRAGSearcher{evidenceID: "ev_attached", evidenceContent: "maple-vector-citadel-secret"}
	f := newRunWorkerFixtureWithRAG(t, searcher, []string{runWorkerDone, runWorkerDone}, nil)
	member, _, _ := runWorkerPrincipals(f.tenant)
	bot := seedAudienceBot(t, f.chatRepo, f.tenant, []string{runWorkerMember})

	first, handlerErr := startAndRun(t, f, member, bot.ID, chatbot.AssistantRunInput{
		Input: "first question", EvidenceIDs: []string{"ev_attached"},
	})
	if handlerErr != nil {
		t.Fatalf("first run worker error: %v", handlerErr)
	}

	// Two-turn replay on the SAME conversation after revocation.
	searcher.revoked = true
	second, handlerErr := startAndRun(t, f, member, bot.ID, chatbot.AssistantRunInput{
		Input: "second question", ConversationID: first.ConversationID,
	})
	if handlerErr != nil {
		t.Fatalf("second run worker error: %v", handlerErr)
	}
	if second.ConversationID != first.ConversationID || len(f.executor.requests) != 2 {
		t.Fatalf("second run = %#v dispatches=%d", second, len(f.executor.requests))
	}
	if strings.Contains(string(f.executor.requests[1].Payload), "maple-vector-citadel-secret") || strings.Contains(string(f.executor.requests[1].Payload), "all done") {
		t.Fatalf("revoked text or redacted reply reached the next run: %s", f.executor.requests[1].Payload)
	}
	if !strings.Contains(string(f.executor.requests[1].Payload), "second question") {
		t.Fatalf("next run lost its own input: %s", f.executor.requests[1].Payload)
	}

	view, err := f.svc.ListConversation(ctx, member, first.ConversationID)
	if err != nil {
		t.Fatalf("replay: %v", err)
	}
	redacted := 0
	for _, msg := range view.Messages {
		if msg.Role != "assistant" || msg.RunID != first.ID {
			continue
		}
		redacted++
		if strings.TrimSpace(msg.Content) != "" {
			t.Fatalf("revoked turn kept content: %q", msg.Content)
		}
		if len(msg.Steps) == 0 {
			t.Fatal("redacted turn lost its steps entirely — metadata must survive")
		}
		step := msg.Steps[0]
		if step.Number != 1 || step.Kind != "model" || step.Name == "" || step.Status != "succeeded" || step.StartedAt.IsZero() {
			t.Fatalf("step metadata = %#v, want number/kind/name/status/timestamps kept", step)
		}
		if step.Input != "" || step.Output != "" || step.Error != "" {
			t.Fatalf("denied step kept source-bearing fields: %#v", step)
		}
	}
	if redacted != 1 {
		t.Fatalf("redacted assistant turns = %d, want exactly the revoked one", redacted)
	}

	// Run reads project the identical metadata-only steps.
	got, err := f.svc.GetRun(ctx, member, first.ID)
	if err != nil || len(got.Steps) != 1 || got.Steps[0].Output != "" || got.Steps[0].Number != 1 {
		t.Fatalf("denied run read = %#v err=%v", got, err)
	}
	if got.Output != "" {
		t.Fatalf("denied run kept retained output: %q", got.Output)
	}
}
