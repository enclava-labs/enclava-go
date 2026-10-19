package httpx

import (
	"context"
	"database/sql"
	"encoding/json"
	"errors"
	"io"
	"net/http"
	"net/http/httptest"
	"net/url"
	"strings"
	"testing"
	"time"

	"enclava-go/internal/auth"
	"enclava-go/internal/chatbot"
	"enclava-go/internal/llm"
	"enclava-go/internal/prompts"
	"enclava-go/internal/rag"
	"enclava-go/internal/store/postgres"
	"enclava-go/internal/testdb"
)

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
		APIKeyID:           42,
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
	provider := &completionProvider{err: errors.New("sql: " + secret + " no such table")}
	chatSvc := chatbot.NewService(postgres.NewChatbotRepo(db), promptRepo, staticRAG{
		err: errors.New("qdrant search failed at " + secret),
	}, provider, nil)
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
	app := &App{chatbotSvc: chatSvc, provider: provider}

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
	chatSvc := chatbot.NewService(postgres.NewChatbotRepo(db), promptRepo, nil, &completionProvider{}, nil)
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
	})
	if err != nil {
		t.Fatalf("create allowed bot: %v", err)
	}
	hiddenBot, err := chatSvc.Create(ctx, open, chatbot.CreateChatbotInput{
		Name:             "Hidden Bot",
		Model:            "chat-test-model",
		ExternalID:       "hidden_ext",
		PromptTemplateID: hiddenTemplate.ID,
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
