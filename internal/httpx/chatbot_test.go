package httpx

import (
	"bytes"
	"context"
	"database/sql"
	"encoding/json"
	"errors"
	"io"
	"net/http"
	"net/http/httptest"
	"net/url"
	"strings"
	"sync"
	"testing"
	"time"

	"enclava-go/internal/auth"
	"enclava-go/internal/chatbot"
	"enclava-go/internal/inference"
	"enclava-go/internal/llm"
	"enclava-go/internal/prompts"
	"enclava-go/internal/rag"
	"enclava-go/internal/store/postgres"
	"enclava-go/internal/testdb"
)

// stubExecutor stands in for the shared executor — the HTTP layer never
// calls a provider directly, so completions exercise the chatbot.Executor
// seam.
type stubExecutor struct {
	result *inference.Result
	err    error
}

func (s *stubExecutor) Execute(ctx context.Context, p auth.Principal, req inference.Request) (*inference.Result, error) {
	return s.result, s.err
}

// streamResult builds an executor result wrapping a fixed SSE body.
func streamResult(body string) *inference.Result {
	return &inference.Result{Body: io.NopCloser(strings.NewReader(body))}
}

// completionProvider stubs the raw provider used by non-assistant route tests.
type completionProvider struct {
	err error
}

func (p *completionProvider) ListModels(context.Context) (llm.ModelsResponse, error) {
	return llm.ModelsResponse{}, nil
}

func (p *completionProvider) GetModel(context.Context, string) (llm.Model, error) {
	return llm.Model{}, nil
}

func (p *completionProvider) CreateChatCompletion(context.Context, []byte) ([]byte, error) {
	if p.err != nil {
		return nil, p.err
	}
	return []byte(`{"choices":[{"message":{"role":"assistant","content":"ok"}}]}`), nil
}

func (p *completionProvider) CreateChatCompletionStream(context.Context, []byte) (io.ReadCloser, error) {
	if p.err != nil {
		return nil, p.err
	}
	return io.NopCloser(strings.NewReader("data: [DONE]\n\n")), nil
}

func (p *completionProvider) CreateEmbeddings(context.Context, []byte) ([]byte, error) {
	return nil, p.err
}

type staticRAG struct {
	err error
}

func (s staticRAG) Search(context.Context, auth.Principal, rag.SearchInput) ([]rag.SearchResult, error) {
	return nil, s.err
}
func (s staticRAG) BuildContext(context.Context, auth.Principal, []rag.SearchResult, int) (rag.ContextResult, error) {
	return rag.ContextResult{}, s.err
}

type dialNetError struct {
	msg string
}

func (e dialNetError) Error() string   { return e.msg }
func (e dialNetError) Timeout() bool   { return true }
func (e dialNetError) Temporary() bool { return false }

func openChatPrincipal(tenantID string) auth.Principal {
	return auth.Principal{
		TenantID:           tenantID,
		ActorID:            "http_key_actor",
		ActorKind:          auth.ActorKindServiceAccount,
		AllowedModelsAll:   true,
		AllowedChatbotsAll: true,
	}
}

func TestExternalChatbotCompletionHidesServiceErrors(t *testing.T) {
	ctx := context.Background()
	db := testdb.New(t)
	tenantID := testdb.SeedTenant(t, db, "chatbot-http")
	promptRepo := postgres.NewPromptsRepo(db)

	const secret = "supersecret-qdrant-dsn"
	chatSvc := chatbot.NewService(postgres.NewChatbotRepo(db), promptRepo, staticRAG{
		err: errors.New("qdrant search failed at " + secret),
	}, &stubExecutor{err: errors.New("sql: " + secret + " no such table")})
	p := openChatPrincipal(tenantID)
	if _, err := chatSvc.Create(ctx, p, chatbot.CreateChatbotInput{
		Name:             "RAG Bot",
		Model:            "chat-test-model",
		ExternalID:       "rag_http_bot",
		RAGCollectionIDs: []string{"ragcol_http"},
	}); err != nil {
		t.Fatalf("create rag chatbot: %v", err)
	}
	if _, err := chatSvc.Create(ctx, p, chatbot.CreateChatbotInput{
		Name:       "Plain Bot",
		Model:      "chat-test-model",
		ExternalID: "plain_http_bot",
	}); err != nil {
		t.Fatalf("create plain chatbot: %v", err)
	}
	app := &App{chatbotSvc: chatSvc}
	valid := `{"messages":[{"role":"user","content":"hello"}]}`
	stream := `{"stream":true,"messages":[{"role":"user","content":"hello"}]}`
	assertCompletionStatus(t, app, p, "rag_http_bot", valid, http.StatusInternalServerError, secret)
	assertCompletionStatus(t, app, p, "rag_http_bot", stream, http.StatusInternalServerError, secret)
	assertCompletionStatus(t, app, p, "plain_http_bot", valid, http.StatusInternalServerError, secret)
	assertCompletionStatus(t, app, p, "plain_http_bot", "{", http.StatusBadRequest, secret)

	malformed := postChatbotCompletion(t, app, p, "plain_http_bot", "{")
	if !strings.Contains(malformed.Body.String(), "invalid JSON") {
		t.Fatalf("malformed body status %d, body %s", malformed.Code, malformed.Body.String())
	}
}

func TestExternalChatbotCompletionStreamUsesExecutor(t *testing.T) {
	ctx := context.Background()
	db := testdb.New(t)
	tenantID := testdb.SeedTenant(t, db, "chatbot-stream")
	promptRepo := postgres.NewPromptsRepo(db)
	exec := &stubExecutor{result: streamResult(
		"data: {\"choices\":[{\"delta\":{\"content\":\"hi\"}}]}\n\ndata: [DONE]\n\n")}
	chatSvc := chatbot.NewService(postgres.NewChatbotRepo(db), promptRepo, nil, exec)
	p := openChatPrincipal(tenantID)
	if _, err := chatSvc.Create(ctx, p, chatbot.CreateChatbotInput{
		Name:       "Stream Bot",
		Model:      "chat-test-model",
		ExternalID: "stream_http_bot",
	}); err != nil {
		t.Fatalf("create stream chatbot: %v", err)
	}
	app := &App{chatbotSvc: chatSvc}
	rec := postChatbotCompletion(t, app, p, "stream_http_bot",
		`{"stream":true,"messages":[{"role":"user","content":"hello"}]}`)
	if rec.Code != http.StatusOK {
		t.Fatalf("stream status = %d, body = %s", rec.Code, rec.Body.String())
	}
	if !strings.Contains(rec.Body.String(), "data: {\"choices\"") {
		t.Fatalf("stream body missing SSE data: %s", rec.Body.String())
	}
}

func TestStreamingReservesThenFinalizesAssistant(t *testing.T) {
	ctx := context.Background()
	db := testdb.New(t)
	tenant := testdb.SeedTenant(t, db, "chatbot-slow-stream")
	reader, writer := io.Pipe()
	defer func() { _ = writer.Close() }()
	repo := postgres.NewChatbotRepo(db)
	svc := chatbot.NewService(repo, postgres.NewPromptsRepo(db), nil, &stubExecutor{result: &inference.Result{Body: reader}})
	p := openChatPrincipal(tenant)
	if _, err := svc.Create(ctx, p, chatbot.CreateChatbotInput{Name: "Slow", Model: "model", ExternalID: "slow"}); err != nil {
		t.Fatal(err)
	}
	app := &App{chatbotSvc: svc}
	rec := httptest.NewRecorder()
	req := httptest.NewRequest(http.MethodPost, "/", strings.NewReader(`{"stream":true,"messages":[{"role":"user","content":"hello"}]}`))
	done := make(chan struct{})
	go func() {
		app.handleExternalChatbotCompletion(rec, req, p, "slow")
		close(done)
	}()
	if _, err := io.WriteString(writer, "data: {\"choices\":[{\"delta\":{\"content\":\"hello \"}}]}\n\n"); err != nil {
		t.Fatal(err)
	}
	conversations, _, err := svc.ListConversations(ctx, p, 10, "")
	if err != nil || len(conversations) != 1 {
		t.Fatalf("conversations: %v %#v", err, conversations)
	}
	messages, err := repo.ListMessages(ctx, tenant, conversations[0].ID)
	if err != nil || len(messages) != 2 || messages[1].Status != "reserved" {
		t.Fatalf("during stream: %v %#v", err, messages)
	}
	reservedID := messages[1].ID
	if _, err := io.WriteString(writer, "data: {\"choices\":[{\"delta\":{\"content\":\"world\"}}]}\n\ndata: [DONE]\n\n"); err != nil {
		t.Fatal(err)
	}
	if err := writer.Close(); err != nil {
		t.Fatal(err)
	}
	<-done
	messages, err = repo.ListMessages(ctx, tenant, conversations[0].ID)
	if err != nil || len(messages) != 2 || messages[1].ID != reservedID || messages[1].Status != "complete" || messages[1].Content != "hello world" {
		t.Fatalf("after stream: %v %#v", err, messages)
	}
}

func TestStreamingFailureLeavesInterruptedMessage(t *testing.T) {
	ctx := context.Background()
	db := testdb.New(t)
	tenant := testdb.SeedTenant(t, db, "chatbot-failed-stream")
	repo := postgres.NewChatbotRepo(db)
	svc := chatbot.NewService(repo, postgres.NewPromptsRepo(db), nil, &stubExecutor{
		result: streamResult("data: {\"choices\":[{\"delta\":{\"content\":\"partial\"}}]}\n\n"),
	})
	p := openChatPrincipal(tenant)
	if _, err := svc.Create(ctx, p, chatbot.CreateChatbotInput{Name: "Broken", Model: "model", ExternalID: "broken"}); err != nil {
		t.Fatal(err)
	}
	app := &App{chatbotSvc: svc}
	postChatbotCompletion(t, app, p, "broken", `{"stream":true,"messages":[{"role":"user","content":"hello"}]}`)
	conversations, _, err := svc.ListConversations(ctx, p, 10, "")
	if err != nil || len(conversations) != 1 {
		t.Fatalf("conversations: %v %#v", err, conversations)
	}
	view, err := svc.ListConversation(ctx, p, conversations[0].ID)
	if err != nil || len(view.Messages) != 2 || view.Messages[1].Status != "interrupted" || view.Messages[1].Content != "Response was interrupted before completion." {
		t.Fatalf("interrupted stream: %v %#v", err, view.Messages)
	}
}

type doneNotifyingWriter struct {
	rec  *httptest.ResponseRecorder
	done chan<- struct{}
	once sync.Once
}

func (w *doneNotifyingWriter) Header() http.Header  { return w.rec.Header() }
func (w *doneNotifyingWriter) WriteHeader(code int) { w.rec.WriteHeader(code) }
func (w *doneNotifyingWriter) Write(b []byte) (int, error) {
	if bytes.Contains(b, []byte("[DONE]")) {
		w.once.Do(func() { close(w.done) })
	}
	return w.rec.Write(b)
}

func TestStreamingClientDisconnectAfterDoneKeepsComplete(t *testing.T) {
	db := testdb.New(t)
	tenant := testdb.SeedTenant(t, db, "chatbot-disconnect-after-done")
	repo := postgres.NewChatbotRepo(db)
	reader, writer := io.Pipe()
	defer func() { _ = writer.Close() }()
	svc := chatbot.NewService(repo, postgres.NewPromptsRepo(db), nil, &stubExecutor{
		result: &inference.Result{Body: reader},
	})
	p := openChatPrincipal(tenant)
	if _, err := svc.Create(context.Background(), p, chatbot.CreateChatbotInput{Name: "Done", Model: "model", ExternalID: "done"}); err != nil {
		t.Fatal(err)
	}
	app := &App{chatbotSvc: svc}
	rec := httptest.NewRecorder()
	sawDone := make(chan struct{})
	reqCtx, cancel := context.WithCancel(context.Background())
	req := httptest.NewRequest(http.MethodPost, "/", strings.NewReader(`{"stream":true,"messages":[{"role":"user","content":"hello"}]}`)).WithContext(reqCtx)
	done := make(chan struct{})
	go func() {
		app.handleExternalChatbotCompletion(&doneNotifyingWriter{rec: rec, done: sawDone}, req, p, "done")
		close(done)
	}()
	if _, err := io.WriteString(writer, "data: {\"choices\":[{\"delta\":{\"content\":\"hello \"}}]}\n\ndata: {\"choices\":[{\"delta\":{\"content\":\"world\"}}]}\n\ndata: [DONE]\n\n"); err != nil {
		t.Fatal(err)
	}
	<-sawDone
	cancel() // client closed right after the terminal event
	if err := writer.Close(); err != nil {
		t.Fatal(err)
	}
	<-done
	conversations, _, err := svc.ListConversations(context.Background(), p, 10, "")
	if err != nil || len(conversations) != 1 {
		t.Fatalf("conversations: %v %#v", err, conversations)
	}
	messages, err := repo.ListMessages(context.Background(), tenant, conversations[0].ID)
	if err != nil || len(messages) != 2 || messages[1].Status != "complete" || messages[1].Content != "hello world" {
		t.Fatalf("disconnect after [DONE] downgraded the answer: %v %#v", err, messages)
	}
}

func TestWriteChatbotCompletionErrorClassifiesFailures(t *testing.T) {
	secret := "secret-provider.example"
	expired, cancel := context.WithDeadline(context.Background(), time.Unix(1, 0))
	defer cancel()
	if !errors.Is(expired.Err(), context.DeadlineExceeded) {
		t.Fatal("expected an expired deadline context")
	}

	app := &App{}
	cases := []struct {
		name    string
		ctx     context.Context
		err     error
		status  int
		want    string
		notWant string
	}{
		{
			name:    "deadline",
			ctx:     context.Background(),
			err:     context.DeadlineExceeded,
			status:  http.StatusGatewayTimeout,
			want:    "upstream request timed out",
			notWant: "deadline exceeded",
		},
		{
			name:    "canceled as deadline",
			ctx:     expired,
			err:     context.Canceled,
			status:  http.StatusGatewayTimeout,
			want:    "upstream request timed out",
			notWant: "context canceled",
		},
		{
			name:    "client canceled",
			ctx:     context.Background(),
			err:     context.Canceled,
			status:  http.StatusInternalServerError,
			want:    "internal server error",
			notWant: "context canceled",
		},
		{
			name:    "dial text",
			ctx:     context.Background(),
			err:     errors.New("dial tcp 10.9.8.7:443: connect: connection refused"),
			status:  http.StatusBadGateway,
			want:    "upstream provider unavailable",
			notWant: "10.9.8.7",
		},
		{
			name:    "net error",
			ctx:     context.Background(),
			err:     errors.Join(errors.New("upstream request failed"), dialNetError{msg: "read tcp " + secret + ":443: i/o timeout"}),
			status:  http.StatusBadGateway,
			notWant: secret,
		},
		{
			name:    "url error",
			ctx:     context.Background(),
			err:     &url.Error{Op: "Post", URL: "https://" + secret + "/v1/chat/completions", Err: errors.New("boom")},
			status:  http.StatusBadGateway,
			want:    "upstream provider unavailable",
			notWant: secret,
		},
		{
			name:    "url error wrapping deadline",
			ctx:     context.Background(),
			err:     &url.Error{Op: "Post", URL: "https://" + secret + "/v1/chat/completions", Err: context.DeadlineExceeded},
			status:  http.StatusGatewayTimeout,
			notWant: secret,
		},
		{
			name:    "sql",
			ctx:     context.Background(),
			err:     errors.New("sql: " + secret + " no such table"),
			status:  http.StatusInternalServerError,
			want:    "internal server error",
			notWant: secret,
		},
		{
			name:    "missing collection",
			ctx:     context.Background(),
			err:     chatbot.ErrCollectionNotFound,
			status:  http.StatusNotFound,
			want:    "knowledge collection was not found",
			notWant: "chatbot not found",
		},
		{
			name:   "missing chatbot",
			ctx:    context.Background(),
			err:    sql.ErrNoRows,
			status: http.StatusNotFound,
			want:   "chatbot not found",
		},
		{
			name:   "invalid input",
			ctx:    context.Background(),
			err:    chatbot.ErrInvalidInput,
			status: http.StatusBadRequest,
			want:   "invalid chatbot input",
		},
		{
			name:    "upstream envelope",
			ctx:     context.Background(),
			err:     &llm.UpstreamError{StatusCode: http.StatusTooManyRequests, Message: secret, Body: []byte(`{"error":{"message":"rate limited","type":"rate_limit_error"}}`)},
			status:  http.StatusTooManyRequests,
			want:    "rate limited",
			notWant: secret,
		},
	}
	for _, tc := range cases {
		t.Run(tc.name, func(t *testing.T) {
			rec := httptest.NewRecorder()
			app.writeChatbotCompletionError(rec, tc.ctx, tc.err)
			if rec.Code != tc.status {
				t.Fatalf("status = %d, want %d, body = %s", rec.Code, tc.status, redactSecret(rec.Body.String(), secret))
			}
			body := rec.Body.String()
			if tc.want != "" && !strings.Contains(body, tc.want) {
				t.Fatalf("body %s does not contain %q", body, tc.want)
			}
			if tc.notWant != "" && strings.Contains(body, tc.notWant) {
				t.Fatalf("body leaked %q", tc.notWant)
			}
		})
	}
}

func TestChatbotAndPromptReadsHonorAllowlist(t *testing.T) {
	ctx := context.Background()
	db := testdb.New(t)
	tenantID := testdb.SeedTenant(t, db, "chatbot-reads")

	promptRepo := postgres.NewPromptsRepo(db)
	chatSvc := chatbot.NewService(postgres.NewChatbotRepo(db), promptRepo, nil, nil)
	promptSvc := prompts.NewService(promptRepo, chatSvc)
	open := openChatPrincipal(tenantID)

	allowedTemplate, err := promptSvc.Create(ctx, open, prompts.CreateTemplateInput{
		ID:         "tmpl_allowed",
		Name:       "Allowed",
		UserPrompt: "{{.user_input}}",
	})
	if err != nil {
		t.Fatalf("create allowed template: %v", err)
	}
	hiddenTemplate, err := promptSvc.Create(ctx, open, prompts.CreateTemplateInput{
		ID:         "tmpl_hidden",
		Name:       "Hidden",
		UserPrompt: "{{.user_input}}",
	})
	if err != nil {
		t.Fatalf("create hidden template: %v", err)
	}
	allowedBot, err := chatSvc.Create(ctx, open, chatbot.CreateChatbotInput{
		Name:             "Allowed Bot",
		Model:            "chat-test-model",
		ExternalID:       "allowed_ext",
		PromptTemplateID: allowedTemplate.ID,
		// This test is about grant allowlists; a tenant-wide audience keeps
		// the audience term out of the picture.
		Audience: []string{"*"},
	})
	if err != nil {
		t.Fatalf("create allowed bot: %v", err)
	}
	hiddenBot, err := chatSvc.Create(ctx, open, chatbot.CreateChatbotInput{
		Name:             "Hidden Bot",
		Model:            "chat-test-model",
		ExternalID:       "hidden_ext",
		PromptTemplateID: hiddenTemplate.ID,
		Audience:         []string{"*"},
	})
	if err != nil {
		t.Fatalf("create hidden bot: %v", err)
	}
	app := &App{chatbotSvc: chatSvc, promptSvc: promptSvc}
	byExternal := auth.Principal{TenantID: tenantID, AllowedChatbots: map[string]struct{}{"allowed_ext": {}}}
	byInternal := auth.Principal{TenantID: tenantID, AllowedChatbots: map[string]struct{}{hiddenBot.ID: {}}}

	if got := listChatbotIDs(t, app, byExternal); !sameIDs(got, []string{allowedBot.ID}) {
		t.Fatalf("external allowlist chatbots = %#v", got)
	}
	if got := listPromptIDs(t, app, byExternal); !sameIDs(got, []string{allowedTemplate.ID}) {
		t.Fatalf("external allowlist prompts = %#v", got)
	}
	if rec := getChatbot(t, app, byExternal, allowedBot.ID); rec.Code != http.StatusOK {
		t.Fatalf("allowed get status = %d body = %s", rec.Code, rec.Body.String())
	}
	missing := getChatbot(t, app, byExternal, "missing_bot")
	hidden := getChatbot(t, app, byExternal, hiddenBot.ID)
	if hidden.Code != http.StatusNotFound || hidden.Body.String() != missing.Body.String() {
		t.Fatalf("hidden bot status %d body %s, missing status %d body %s", hidden.Code, hidden.Body.String(), missing.Code, missing.Body.String())
	}
	if rec := getPrompt(t, app, byExternal, hiddenTemplate.ID); rec.Code != http.StatusNotFound || !strings.Contains(rec.Body.String(), "prompt template not found") {
		t.Fatalf("hidden template status %d body %s", rec.Code, rec.Body.String())
	}
	if rec := getPrompt(t, app, byExternal, allowedTemplate.ID); rec.Code != http.StatusOK {
		t.Fatalf("allowed template status %d body %s", rec.Code, rec.Body.String())
	}

	if got := listChatbotIDs(t, app, byInternal); !sameIDs(got, []string{hiddenBot.ID}) {
		t.Fatalf("internal allowlist chatbots = %#v", got)
	}
	if got := listPromptIDs(t, app, byInternal); !sameIDs(got, []string{hiddenTemplate.ID}) {
		t.Fatalf("internal allowlist prompts = %#v", got)
	}
	if got := listChatbotIDs(t, app, open); !sameIDs(got, []string{allowedBot.ID, hiddenBot.ID}) {
		t.Fatalf("open allowlist chatbots = %#v", got)
	}
	if got := listPromptIDs(t, app, open); !sameIDs(got, []string{allowedTemplate.ID, hiddenTemplate.ID}) {
		t.Fatalf("open allowlist prompts = %#v", got)
	}

	bare := &App{promptSvc: prompts.NewService(promptRepo, nil)}
	if got := listPromptIDs(t, bare, byExternal); len(got) != 0 {
		t.Fatalf("nil chatbot visibility returned templates to a restricted key: %#v", got)
	}
	if rec := getPrompt(t, bare, byExternal, allowedTemplate.ID); rec.Code != http.StatusNotFound {
		t.Fatalf("nil chatbot visibility get status = %d body = %s", rec.Code, rec.Body.String())
	}
	if got := listPromptIDs(t, bare, open); !sameIDs(got, []string{allowedTemplate.ID, hiddenTemplate.ID}) {
		t.Fatalf("nil chatbot visibility open allowlist prompts = %#v", got)
	}
}

func postChatbotCompletion(t *testing.T, app *App, principal auth.Principal, ref, body string) *httptest.ResponseRecorder {
	t.Helper()
	req := httptest.NewRequest(http.MethodPost, "/api/v1/chatbot/external/"+ref+"/chat/completions", strings.NewReader(body))
	rec := httptest.NewRecorder()
	app.handleExternalChatbotCompletion(rec, req, principal, ref)
	return rec
}

func assertCompletionStatus(t *testing.T, app *App, principal auth.Principal, ref, body string, status int, secret string) {
	t.Helper()
	rec := postChatbotCompletion(t, app, principal, ref, body)
	if rec.Code != status {
		t.Fatalf("%s status = %d, want %d, body = %s", ref, rec.Code, status, redactSecret(rec.Body.String(), secret))
	}
	if strings.Contains(rec.Body.String(), secret) {
		t.Fatalf("%s response leaked internal error", ref)
	}
}

func redactSecret(body, secret string) string {
	if secret == "" {
		return body
	}
	return strings.ReplaceAll(body, secret, "<secret>")
}

func listChatbotIDs(t *testing.T, app *App, principal auth.Principal) []string {
	t.Helper()
	req := httptest.NewRequest(http.MethodGet, "/api/v1/chatbots", nil)
	rec := httptest.NewRecorder()
	app.handleChatbots(rec, req, principal)
	if rec.Code != http.StatusOK {
		t.Fatalf("list chatbots status = %d body = %s", rec.Code, rec.Body.String())
	}
	var payload struct {
		Chatbots []struct {
			ID string `json:"id"`
		} `json:"chatbots"`
	}
	if err := json.Unmarshal(rec.Body.Bytes(), &payload); err != nil {
		t.Fatalf("decode chatbots: %v", err)
	}
	ids := make([]string, 0, len(payload.Chatbots))
	for _, bot := range payload.Chatbots {
		ids = append(ids, bot.ID)
	}
	return ids
}

func getChatbot(t *testing.T, app *App, principal auth.Principal, id string) *httptest.ResponseRecorder {
	t.Helper()
	req := httptest.NewRequest(http.MethodGet, "/api/v1/chatbots/"+id, nil)
	rec := httptest.NewRecorder()
	app.handleChatbotByID(rec, req, principal)
	return rec
}

func listPromptIDs(t *testing.T, app *App, principal auth.Principal) []string {
	t.Helper()
	req := httptest.NewRequest(http.MethodGet, "/api/v1/prompt-templates", nil)
	rec := httptest.NewRecorder()
	app.handlePromptTemplates(rec, req, principal)
	if rec.Code != http.StatusOK {
		t.Fatalf("list prompts status = %d body = %s", rec.Code, rec.Body.String())
	}
	var payload struct {
		Templates []struct {
			ID string `json:"id"`
		} `json:"prompt_templates"`
	}
	if err := json.Unmarshal(rec.Body.Bytes(), &payload); err != nil {
		t.Fatalf("decode prompts: %v", err)
	}
	ids := make([]string, 0, len(payload.Templates))
	for _, template := range payload.Templates {
		ids = append(ids, template.ID)
	}
	return ids
}

func getPrompt(t *testing.T, app *App, principal auth.Principal, id string) *httptest.ResponseRecorder {
	t.Helper()
	req := httptest.NewRequest(http.MethodGet, "/api/v1/prompt-templates/"+id, nil)
	rec := httptest.NewRecorder()
	app.handlePromptTemplateByID(rec, req, principal)
	return rec
}

func sameIDs(got, want []string) bool {
	if len(got) != len(want) {
		return false
	}
	seen := make(map[string]int, len(got))
	for _, id := range got {
		seen[id]++
	}
	for _, id := range want {
		seen[id]--
		if seen[id] < 0 {
			return false
		}
	}
	for _, n := range seen {
		if n != 0 {
			return false
		}
	}
	return true
}

func TestAgentRunErrorsMapToClientStatuses(t *testing.T) {
	for _, tc := range []struct {
		err  error
		code int
	}{
		{chatbot.ErrRunNotFound, http.StatusNotFound},
		{chatbot.ErrInvalidInput, http.StatusBadRequest},
		{chatbot.ErrChatbotNotAllowed, http.StatusForbidden},
	} {
		rec := httptest.NewRecorder()
		writeChatbotNotFoundOrError(rec, tc.err, "agent_run_failed")
		if rec.Code != tc.code {
			t.Errorf("%v status=%d, want %d", tc.err, rec.Code, tc.code)
		}
	}
}

// conversationsPayload mirrors the frozen OA-13 envelope.
type conversationsPayload struct {
	Conversations []chatbot.ConversationSummary `json:"conversations"`
	NextCursor    string                        `json:"next_cursor"`
}

func getConversations(t *testing.T, app *App, principal auth.Principal, query string) *httptest.ResponseRecorder {
	t.Helper()
	req := httptest.NewRequest(http.MethodGet, "/api/v1/conversations"+query, nil)
	rec := httptest.NewRecorder()
	app.handleConversations(rec, req, principal)
	return rec
}

// TestConversationsHandlerOwnOnlyAndPaging proves the OA-13 handler: own
// conversations only (member AND admin), hidden assistants filtered, the
// frozen payload shape, disjoint keyset pages and a 400 on malformed
// cursors.
func TestConversationsHandlerOwnOnlyAndPaging(t *testing.T) {
	ctx := context.Background()
	db := testdb.New(t)
	tenantID := testdb.SeedTenant(t, db, "conversations-http")
	chatRepo := postgres.NewChatbotRepo(db)
	chatSvc := chatbot.NewService(chatRepo, postgres.NewPromptsRepo(db), nil, nil)
	app := &App{chatbotSvc: chatSvc}

	creator := openChatPrincipal(tenantID)
	botVisible, err := chatSvc.Create(ctx, creator, chatbot.CreateChatbotInput{
		Name: "Visible Bot", Model: "chat-test-model", Audience: []string{"member_actor"},
	})
	if err != nil {
		t.Fatalf("create visible bot: %v", err)
	}
	botHidden, err := chatSvc.Create(ctx, creator, chatbot.CreateChatbotInput{
		Name: "Hidden Bot", Model: "chat-test-model", Audience: []string{},
	})
	if err != nil {
		t.Fatalf("create hidden bot: %v", err)
	}
	member := auth.Principal{TenantID: tenantID, ActorID: "member_actor",
		AllowedChatbots: map[string]struct{}{botVisible.ID: {}, botHidden.ID: {}}}

	seed := func(id, owner, botID, content string) {
		t.Helper()
		if err := chatRepo.CreateConversation(ctx, chatbot.Conversation{
			TenantID: tenantID, ID: id, ChatbotID: botID, OwnerActorID: owner,
		}); err != nil {
			t.Fatalf("seed conversation %s: %v", id, err)
		}
		if err := chatRepo.AddMessage(ctx, chatbot.Message{
			TenantID: tenantID, ID: id + "_msg", ConversationID: id,
			Role: "user", Content: content, OwnerActorID: owner,
		}); err != nil {
			t.Fatalf("seed message %s: %v", id, err)
		}
	}
	seed("conv_new", "member_actor", botVisible.ID, "newer conversation")
	seed("conv_old", "member_actor", botVisible.ID, "older conversation")
	seed("conv_hidden", "member_actor", botHidden.ID, "hidden history")
	seed("conv_foreign", "someone_else", botVisible.ID, "not mine")
	// conv_new is the newest; age conv_old so the order is deterministic.
	if _, err := db.SQL.ExecContext(ctx,
		`UPDATE conversations SET created_at = '2020-01-01T00:00:00Z'::timestamptz
		 WHERE tenant_id = $1::uuid AND id = 'conv_old'`, tenantID); err != nil {
		t.Fatalf("age conversation: %v", err)
	}

	// Member: own visible rows only, in the frozen envelope shape.
	rec := getConversations(t, app, member, "?limit=10")
	if rec.Code != http.StatusOK {
		t.Fatalf("member list status = %d body = %s", rec.Code, rec.Body.String())
	}
	var payload conversationsPayload
	if err := json.Unmarshal(rec.Body.Bytes(), &payload); err != nil {
		t.Fatalf("decode member list: %v", err)
	}
	if len(payload.Conversations) != 2 || payload.Conversations[0].ID != "conv_new" || payload.Conversations[1].ID != "conv_old" {
		t.Fatalf("member conversations = %#v", payload.Conversations)
	}
	first := payload.Conversations[0]
	if first.ChatbotID != botVisible.ID || first.AssistantName != "Visible Bot" || first.Title != "newer conversation" {
		t.Fatalf("summary shape = %#v", first)
	}
	if first.CreatedAt.IsZero() || first.UpdatedAt.IsZero() {
		t.Fatalf("summary timestamps missing: %#v", first)
	}
	if payload.NextCursor != "" {
		t.Fatalf("next_cursor = %q on a complete page", payload.NextCursor)
	}

	// Cursor paging: two disjoint pages over limit=1.
	rec = getConversations(t, app, member, "?limit=1")
	if err := json.Unmarshal(rec.Body.Bytes(), &payload); err != nil || len(payload.Conversations) != 1 {
		t.Fatalf("page 1 = %s err=%v", rec.Body.String(), err)
	}
	if payload.NextCursor == "" {
		t.Fatal("page 1 must carry a next_cursor")
	}
	firstPage := payload.Conversations[0].ID
	rec = getConversations(t, app, member, "?limit=1&cursor="+url.QueryEscape(payload.NextCursor))
	if err := json.Unmarshal(rec.Body.Bytes(), &payload); err != nil || len(payload.Conversations) != 1 {
		t.Fatalf("page 2 = %s err=%v", rec.Body.String(), err)
	}
	if payload.Conversations[0].ID == firstPage {
		t.Fatal("page 2 repeated page 1's row")
	}
	if payload.NextCursor != "" {
		t.Fatalf("page 2 must end the list, next_cursor = %q", payload.NextCursor)
	}

	// Malformed cursor is a 400, never an ignored parameter (plain chars
	// survive query parsing but fail the cursor payload check).
	if rec := getConversations(t, app, member, "?cursor=not-a-cursor"); rec.Code != http.StatusBadRequest {
		t.Fatalf("malformed cursor status = %d body = %s", rec.Code, rec.Body.String())
	}

	// Admin sees only their own conversations — none seeded, none leaked.
	admin := auth.Principal{TenantID: tenantID, ActorID: "admin_actor",
		Scopes: map[string]struct{}{auth.ScopePlatformAdmin: {}}, AllowedChatbotsAll: true}
	rec = getConversations(t, app, admin, "")
	if rec.Code != http.StatusOK {
		t.Fatalf("admin list status = %d", rec.Code)
	}
	if err := json.Unmarshal(rec.Body.Bytes(), &payload); err != nil || len(payload.Conversations) != 0 {
		t.Fatalf("admin conversations = %s err=%v, want own-only empty", rec.Body.String(), err)
	}

	// Non-GET is a 405; a nil chatbot service is a 501 not_configured.
	req := httptest.NewRequest(http.MethodPost, "/api/v1/conversations", strings.NewReader("{}"))
	rec = httptest.NewRecorder()
	app.handleConversations(rec, req, member)
	if rec.Code != http.StatusMethodNotAllowed {
		t.Fatalf("POST status = %d", rec.Code)
	}
	rec = httptest.NewRecorder()
	(&App{}).handleConversations(rec, httptest.NewRequest(http.MethodGet, "/api/v1/conversations", nil), member)
	if rec.Code != http.StatusNotImplemented || !strings.Contains(rec.Body.String(), "not_configured") {
		t.Fatalf("nil service status = %d body = %s", rec.Code, rec.Body.String())
	}
}

// TestConversationsRouteSessionAuth drives the registered route over the
// real mux: unauthenticated requests are refused by session auth and a
// granted member session gets exactly their own rows.
func TestConversationsRouteSessionAuth(t *testing.T) {
	f := newIdentityFixture(t)
	ctx := context.Background()
	chatRepo := postgres.NewChatbotRepo(f.db)
	if err := chatRepo.Create(ctx, chatbot.Instance{
		TenantID: f.tenant, ID: "bot_conv", ExternalID: "bot_conv_ext",
		Name: "Conv Bot", Model: "m", IsEnabled: true,
		OwnerActorID: "seed_owner", Audience: []string{"*"},
	}); err != nil {
		t.Fatalf("seed bot: %v", err)
	}

	sess, csrf := loginUserWithGrants(t, f, "conv-member@example.com", []string{"chatbot"},
		auth.ResourceGrants{Assistants: auth.Allowlist{Items: []string{"bot_conv"}}}, "member-pass")
	var memberID string
	if err := f.db.SQL.QueryRowContext(ctx,
		`SELECT id::text FROM users WHERE tenant_id = $1::uuid AND email = $2`,
		f.tenant, "conv-member@example.com").Scan(&memberID); err != nil {
		t.Fatalf("read member id: %v", err)
	}
	for _, conv := range []struct{ id, owner string }{
		{"conv_member_1", memberID},
		{"conv_foreign", "someone_else"},
	} {
		if err := chatRepo.CreateConversation(ctx, chatbot.Conversation{
			TenantID: f.tenant, ID: conv.id, ChatbotID: "bot_conv", OwnerActorID: conv.owner,
		}); err != nil {
			t.Fatalf("seed conversation %s: %v", conv.id, err)
		}
		if err := chatRepo.AddMessage(ctx, chatbot.Message{
			TenantID: f.tenant, ID: conv.id + "_msg", ConversationID: conv.id,
			Role: "user", Content: "hello from " + conv.owner, OwnerActorID: conv.owner,
		}); err != nil {
			t.Fatalf("seed message %s: %v", conv.id, err)
		}
	}

	// No credential at all: authentication fails before any scope check.
	if rec := f.do(t, http.MethodGet, "/api/v1/conversations", "", nil); rec.Code != http.StatusUnauthorized {
		t.Fatalf("unauthenticated GET = %d body=%s", rec.Code, rec.Body.String())
	}

	rec := sessionDo(t, f.app, http.MethodGet, "/api/v1/conversations", "", sess, csrf)
	if rec.Code != http.StatusOK {
		t.Fatalf("member session GET = %d body=%s", rec.Code, rec.Body.String())
	}
	var payload conversationsPayload
	if err := json.Unmarshal(rec.Body.Bytes(), &payload); err != nil {
		t.Fatalf("decode session list: %v", err)
	}
	if len(payload.Conversations) != 1 || payload.Conversations[0].ID != "conv_member_1" {
		t.Fatalf("session conversations = %#v, want only conv_member_1", payload.Conversations)
	}
	if payload.Conversations[0].AssistantName != "Conv Bot" || payload.Conversations[0].Title != "hello from "+memberID {
		t.Fatalf("session summary = %#v", payload.Conversations[0])
	}
}
