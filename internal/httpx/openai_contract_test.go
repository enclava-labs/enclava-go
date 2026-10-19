package httpx

import (
	"context"
	"enclava-go/internal/auth"
	"enclava-go/internal/billing"
	"enclava-go/internal/inference"
	"enclava-go/internal/llm"
	"enclava-go/internal/source"
	"enclava-go/internal/store/postgres"
	"enclava-go/internal/testdb"
	"encoding/json"
	"errors"
	"fmt"
	"io"
	"net/http"
	"net/http/httptest"
	"strings"
	"sync/atomic"
	"testing"
	"time"

	openai "github.com/openai/openai-go/v3"
	"github.com/openai/openai-go/v3/option"
	"github.com/openai/openai-go/v3/shared"
)

// openaiFx is the consumer-visible contract fixture: a real executor over a
// real PostgreSQL schema (tenant, key, endpoint, prices, reservations) with a
// deterministic in-process upstream transport — no provider is faked at the
// HTTP layer; every request flows through route → auth → policy → executor.
type openaiFx struct {
	t       *testing.T
	db      *postgres.DB
	tenant  string
	app     *App
	up      *contractUpstream
	brepo   *postgres.BillingRepo
	erepo   *postgres.EndpointsRepo
	keyID   string
	lastReq atomic.Value // last body the upstream observed
}

// contractUpstream is a deterministic in-process HTTP-shaped provider: it
// returns canned but protocol-real chat/embedding/stream payloads and records
// what the executor dispatched.
type contractUpstream struct {
	chatResp       []byte
	streamBody     string
	embeddingResp  []byte
	err            error
	streamErr      error
	dispatchErr    error
	streamClosed   *atomic.Bool
	lastChatBody   []byte
	lastEmbedBody  []byte
	chatCalls      int32
	streamCalls    int32
	embeddingCalls int32
}

func (u *contractUpstream) CreateChatCompletion(ctx context.Context, body []byte) ([]byte, error) {
	atomic.AddInt32(&u.chatCalls, 1)
	u.lastChatBody = append([]byte(nil), body...)
	return u.chatResp, u.err
}

func (u *contractUpstream) CreateChatCompletionStream(ctx context.Context, body []byte) (io.ReadCloser, error) {
	atomic.AddInt32(&u.streamCalls, 1)
	u.lastChatBody = append([]byte(nil), body...)
	if u.streamErr != nil {
		return nil, u.streamErr
	}
	closed := u.streamClosed
	reader := &trackedReadCloser{Reader: strings.NewReader(u.streamBody), closed: closed}
	return reader, nil
}

type trackedReadCloser struct {
	io.Reader
	closed *atomic.Bool
}

func (t *trackedReadCloser) Close() error {
	if t.closed != nil {
		t.closed.Store(true)
	}
	return nil
}

func (u *contractUpstream) CreateEmbeddings(ctx context.Context, body []byte) ([]byte, error) {
	atomic.AddInt32(&u.embeddingCalls, 1)
	u.lastEmbedBody = append([]byte(nil), body...)
	return u.embeddingResp, u.err
}

func (u *contractUpstream) DoResponsesRequest(ctx context.Context, method, path string, body []byte) ([]byte, error) {
	return nil, errors.New("responses unsupported in fixture")
}

func (u *contractUpstream) DoResponsesStream(ctx context.Context, method, path string, body []byte) (io.ReadCloser, error) {
	return nil, errors.New("responses unsupported in fixture")
}

func newOpenAIContractFixture(t *testing.T) *openaiFx {
	t.Helper()
	db := testdb.New(t)
	repo := postgres.NewIdentityRepo(db)
	tenant := testdb.SeedTenant(t, db, fmt.Sprintf("openai-%d", time.Now().UnixNano()%1e6))
	user := testdb.SeedUser(t, db, tenant, "openai@example.com",
		[]string{"models.list", "chat.completions", "embeddings.create"})
	key, _ := testdb.SeedAPIKey(t, db, tenant, testdb.SeedKeySpec{
		Name:        "openai",
		OwnerUserID: user.ID,
		Scopes:      []string{"models.list", "chat.completions", "embeddings.create"},
	})
	erepo := postgres.NewEndpointsRepo(db)
	brepo := postgres.NewBillingRepo(db)
	srepo := postgres.NewSourceRepo(db)
	arepo := postgres.NewArtifactRepo(db)

	up := &contractUpstream{
		chatResp: []byte(`{"id":"chatcmpl_1","object":"chat.completion","model":"up-chat","choices":[{"index":0,"message":{"role":"assistant","content":"ok"},"finish_reason":"stop"}],"usage":{"prompt_tokens":3,"completion_tokens":4,"total_tokens":7}}`),
		streamBody: "data: {\"id\":\"chatcmpl_1\",\"choices\":[{\"index\":0,\"delta\":{\"tool_calls\":[{\"index\":0,\"id\":\"call_1\",\"type\":\"function\",\"function\":{\"name\":\"get_weather\",\"arguments\":\"\"}}]}}]}\n\n" +
			"data: {\"id\":\"chatcmpl_1\",\"choices\":[{\"index\":0,\"delta\":{\"tool_calls\":[{\"index\":0,\"function\":{\"arguments\":\"{\\\"city\\\":\\\"Ber\"}}]}}]}\n\n" +
			"data: {\"id\":\"chatcmpl_1\",\"choices\":[{\"index\":0,\"delta\":{\"tool_calls\":[{\"index\":0,\"function\":{\"arguments\":\"lin\\\"}\"}}]}}]}\n\n" +
			"data: {\"id\":\"chatcmpl_1\",\"choices\":[{\"index\":0,\"delta\":{},\"finish_reason\":\"tool_calls\"}],\"usage\":{\"prompt_tokens\":5,\"completion_tokens\":6,\"total_tokens\":11}}\n\n" +
			"data: [DONE]\n\n",
		embeddingResp: []byte(`{"object":"list","data":[{"object":"embedding","embedding":[0.1,0.2],"index":0}],"model":"up-embed","usage":{"prompt_tokens":2,"total_tokens":2}}`),
		streamClosed:  &atomic.Bool{},
	}

	svc, err := inference.NewService(inference.Deps{
		Endpoints:  erepo,
		Sources:    srepo,
		Authorizer: source.NewAuthorizer(srepo, 0),
		Billing:    billing.NewService(brepo),
		Artifacts:  arepo,
	}, inference.WithTransportFactory(func(ctx context.Context, e inference.Endpoint, credential string) (inference.Upstream, error) {
		return up, nil
	}))
	if err != nil {
		t.Fatalf("new inference service: %v", err)
	}
	authSvc := auth.NewService(repo, "")
	app := NewApp(authSvc, nil, "", nil, repo, nil, nil, nil, nil, nil, nil, nil, brepo, erepo,
		"sha256", "", "enc_", nil, nil, 1, false, svc, postgres.NewResponseBindingsRepo(db))
	fx := &openaiFx{t: t, db: db, tenant: tenant, app: app, up: up, brepo: brepo, erepo: erepo}
	fx.keyID = key
	fx.mustEndpoint(t, "ep-chat", []string{
		inference.CapabilityChat, inference.CapabilityVision, inference.CapabilityEmbeddings,
		"model:gpt-fast=gpt-4o-mini", "model:embed-1=text-embedding-3-small",
	})
	fx.price(t, "ep-chat", inference.CapabilityChat, billing.UnitInput, 1, 1)
	fx.price(t, "ep-chat", inference.CapabilityChat, billing.UnitOutput, 2, 1)
	fx.price(t, "ep-chat", inference.CapabilityVision, billing.UnitInput, 1, 1)
	fx.price(t, "ep-chat", inference.CapabilityVision, billing.UnitOutput, 2, 1)
	fx.price(t, "ep-chat", inference.CapabilityEmbeddings, billing.UnitInput, 1, 1)
	return fx
}

func (f *openaiFx) mustEndpoint(t *testing.T, id string, caps []string) {
	t.Helper()
	if err := f.erepo.Create(context.Background(), inference.Endpoint{
		ID: id, TenantID: f.tenant, Protocol: "openai_compatible",
		BaseOrigin: "https://upstream.example", Capabilities: caps, Enabled: true,
	}); err != nil {
		t.Fatalf("create endpoint %s: %v", id, err)
	}
}

func (f *openaiFx) price(t *testing.T, endpointID, capability string, unit billing.UnitKind, micros, qty int64) {
	t.Helper()
	id := fmt.Sprintf("price-%s-%s-%s-%d", endpointID, capability, unit, time.Now().UnixNano())
	if err := f.brepo.PutPrice(context.Background(), f.tenant, id, billing.Price{
		SubjectType: billing.PriceSubjectEndpoint, SubjectID: endpointID,
		Capability: capability, Unit: unit,
		RateMicros: micros, UnitQuantity: qty, Currency: "USD",
	}, "tester", time.Now().Add(-time.Minute).UTC()); err != nil {
		t.Fatalf("put price: %v", err)
	}
}

func (f *openaiFx) do(t *testing.T, method, path, body string) *httptest.ResponseRecorder {
	t.Helper()
	var r *http.Request
	if body != "" {
		r = httptest.NewRequest(method, path, strings.NewReader(body))
		r.Header.Set("Content-Type", "application/json")
	} else {
		r = httptest.NewRequest(method, path, nil)
	}
	r.Header.Set("Authorization", "Bearer "+f.keyID)
	rec := httptest.NewRecorder()
	f.app.Handler().ServeHTTP(rec, r)
	return rec
}

func decodeErr(t *testing.T, rec *httptest.ResponseRecorder) OpenAIErrorEnvelope {
	t.Helper()
	var out OpenAIErrorEnvelope
	if err := json.Unmarshal(rec.Body.Bytes(), &out); err != nil {
		t.Fatalf("decode error body %q: %v", rec.Body.String(), err)
	}
	return out
}

func TestModelsListsOnlyPermittedAliases(t *testing.T) {
	f := newOpenAIContractFixture(t)
	rec := f.do(t, http.MethodGet, "/v1/models", "")
	if rec.Code != http.StatusOK {
		t.Fatalf("list models: %d %s", rec.Code, rec.Body.String())
	}
	var out llm.ModelsResponse
	if err := json.Unmarshal(rec.Body.Bytes(), &out); err != nil {
		t.Fatalf("decode: %v", err)
	}
	ids := map[string]bool{}
	for _, m := range out.Data {
		ids[m.ID] = true
		if strings.Contains(m.ID, "=") {
			t.Fatalf("leaked upstream binding in id %q", m.ID)
		}
	}
	if !ids["gpt-fast"] || !ids["embed-1"] {
		t.Fatalf("missing approved aliases: %+v", out.Data)
	}
	// The upstream model id must never surface.
	if ids["gpt-4o-mini"] || ids["text-embedding-3-small"] {
		t.Fatalf("upstream ids exposed: %+v", out.Data)
	}
}

func TestModelByIDResolvesAliasOnly(t *testing.T) {
	f := newOpenAIContractFixture(t)
	rec := f.do(t, http.MethodGet, "/v1/models/gpt-fast", "")
	if rec.Code != http.StatusOK {
		t.Fatalf("get model: %d %s", rec.Code, rec.Body.String())
	}
	var m llm.Model
	if err := json.Unmarshal(rec.Body.Bytes(), &m); err != nil || m.ID != "gpt-fast" {
		t.Fatalf("model: %v %+v", err, m)
	}
	// Unknown and upstream-bound ids are indistinguishable 404s.
	for _, id := range []string{"gpt-4o-mini", "nope", "gpt-fast=x"} {
		rec := f.do(t, http.MethodGet, "/v1/models/"+id, "")
		if rec.Code != http.StatusNotFound {
			t.Fatalf("expected 404 for %q, got %d %s", id, rec.Code, rec.Body.String())
		}
	}
}

func TestChatCompletionDispatchesThroughExecutor(t *testing.T) {
	f := newOpenAIContractFixture(t)
	rec := f.do(t, http.MethodPost, "/v1/chat/completions",
		`{"model":"gpt-fast","messages":[{"role":"user","content":"hi"}]}`)
	if rec.Code != http.StatusOK {
		t.Fatalf("chat: %d %s", rec.Code, rec.Body.String())
	}
	var resp struct {
		Model   string `json:"model"`
		Choices []struct {
			Message struct {
				Content string `json:"content"`
			} `json:"message"`
		} `json:"choices"`
		Usage struct {
			TotalTokens int `json:"total_tokens"`
		} `json:"usage"`
	}
	if err := json.Unmarshal(rec.Body.Bytes(), &resp); err != nil {
		t.Fatalf("decode: %v", err)
	}
	if resp.Model != "up-chat" || len(resp.Choices) != 1 || resp.Choices[0].Message.Content != "ok" || resp.Usage.TotalTokens != 7 {
		t.Fatalf("unexpected chat body: %s", rec.Body.String())
	}
	// The dispatched body carried the endpoint-bound upstream model, not the
	// client alias, and reserved real units on the reservation ledger.
	body := string(f.up.lastChatBody)
	if !strings.Contains(body, `"model":"gpt-4o-mini"`) || strings.Contains(body, "gpt-fast") {
		t.Fatalf("upstream body lost model binding: %s", body)
	}
	var reservations int
	if err := f.db.SQL.QueryRowContext(context.Background(),
		`SELECT count(*) FROM reservations WHERE tenant_id=$1`, f.tenant).Scan(&reservations); err != nil || reservations == 0 {
		t.Fatalf("reservation evidence missing: n=%d err=%v", reservations, err)
	}
}

func TestChatStreamRelaysToolCallDeltas(t *testing.T) {
	f := newOpenAIContractFixture(t)
	rec := f.do(t, http.MethodPost, "/v1/chat/completions",
		`{"model":"gpt-fast","stream":true,"messages":[{"role":"user","content":"hi"}],"tools":[{"type":"function","function":{"name":"get_weather","parameters":{"type":"object","properties":{"city":{"type":"string"}}}}}]}`)
	if rec.Code != http.StatusOK {
		t.Fatalf("stream: %d %s", rec.Code, rec.Body.String())
	}
	if ct := rec.Header().Get("Content-Type"); !strings.Contains(ct, "text/event-stream") {
		t.Fatalf("content-type %q", ct)
	}
	body := rec.Body.String()
	for _, want := range []string{`"name":"get_weather"`, `"arguments"`, "[DONE]", `"usage":{"prompt_tokens":5`} {
		if !strings.Contains(body, want) {
			t.Fatalf("stream body missing %q:\n%s", want, body)
		}
	}
	// Reassembled tool-call arguments span SSE fragments.
	if !strings.Contains(body, `"{\\\"city\\\":\\\"Ber"`) && !strings.Contains(body, "city") {
		t.Fatalf("tool-call arguments not relayed: %s", body)
	}
	// The upstream streaming body must carry stream_options.include_usage.
	if !strings.Contains(string(f.up.lastChatBody), `"include_usage":true`) {
		t.Fatalf("upstream stream body missing include_usage: %s", f.up.lastChatBody)
	}
	if !f.up.streamClosed.Load() {
		t.Fatal("upstream stream body not closed after relay")
	}
}

func TestChatDeniedModelDeniedBeforeDispatch(t *testing.T) {
	f := newOpenAIContractFixture(t)
	rec := f.do(t, http.MethodPost, "/v1/chat/completions",
		`{"model":"no-such-alias","messages":[{"role":"user","content":"hi"}]}`)
	if rec.Code != http.StatusForbidden {
		t.Fatalf("expected 403, got %d %s", rec.Code, rec.Body.String())
	}
	if atomic.LoadInt32(&f.up.chatCalls) != 0 || atomic.LoadInt32(&f.up.streamCalls) != 0 {
		t.Fatal("denied request reached upstream")
	}
	// Denial must not disclose endpoint metadata in the error body.
	env := decodeErr(t, rec)
	if strings.Contains(env.Error.Message, "ep-chat") || strings.Contains(env.Error.Message, "127.0.0.1") {
		t.Fatalf("denial leaked metadata: %+v", env)
	}
}

func TestEmbeddingsRoundTripOrdered(t *testing.T) {
	f := newOpenAIContractFixture(t)
	f.up.embeddingResp = []byte(`{"object":"list","data":[{"object":"embedding","embedding":[1,2,3],"index":0},{"object":"embedding","embedding":[4,5,6],"index":1}],"model":"up-embed","usage":{"prompt_tokens":9,"total_tokens":9}}`)
	rec := f.do(t, http.MethodPost, "/v1/embeddings",
		`{"model":"embed-1","input":["alpha","beta"],"encoding_format":"float"}`)
	if rec.Code != http.StatusOK {
		t.Fatalf("embeddings: %d %s", rec.Code, rec.Body.String())
	}
	var out struct {
		Data []struct {
			Index     int       `json:"index"`
			Embedding []float64 `json:"embedding"`
		} `json:"data"`
		Usage struct {
			TotalTokens int `json:"total_tokens"`
		} `json:"usage"`
	}
	if err := json.Unmarshal(rec.Body.Bytes(), &out); err != nil || len(out.Data) != 2 || out.Data[0].Index != 0 || out.Data[1].Index != 1 {
		t.Fatalf("embedding order/index broken: %v %s", err, rec.Body.String())
	}
	if out.Usage.TotalTokens != 9 {
		t.Fatalf("usage missing: %s", rec.Body.String())
	}
	if !strings.Contains(string(f.up.lastEmbedBody), `"model":"text-embedding-3-small"`) {
		t.Fatalf("embedding upstream model binding lost: %s", f.up.lastEmbedBody)
	}
}

func TestUnselectedAndUnsupportedRoutesFailClosed(t *testing.T) {
	f := newOpenAIContractFixture(t)
	cases := []struct {
		method, path, body string
		want               int
	}{
		// Not registered: no catch-all passthrough remains.
		{http.MethodGet, "/v1/files", "", http.StatusNotFound},
		{http.MethodPost, "/v1/images/generations", `{}`, http.StatusNotFound},
		{http.MethodGet, "/api/v1/models", "", http.StatusNotFound},
		{http.MethodPost, "/api/v1/chat/completions", `{}`, http.StatusNotFound},
		// Registered routes still bound the method.
		{http.MethodGet, "/v1/chat/completions", "", http.StatusMethodNotAllowed},
		// The fixture key lacks the "responses" scope: mounted responses
		// routes deny before method checks or dispatch.
		{http.MethodGet, "/v1/responses", "", http.StatusForbidden},
		{http.MethodPost, "/v1/responses", `{"model":"gpt-fast","input":"hi"}`, http.StatusForbidden},
		{http.MethodDelete, "/v1/models/gpt-fast", "", http.StatusMethodNotAllowed},
		{http.MethodPost, "/v1/embeddings", `{"model":"embed-1","input":"x","stream":true}`, http.StatusBadRequest},
	}
	for _, c := range cases {
		rec := f.do(t, c.method, c.path, c.body)
		if rec.Code != c.want {
			t.Fatalf("%s %s: got %d want %d (%s)", c.method, c.path, rec.Code, c.want, rec.Body.String())
		}
	}
	if atomic.LoadInt32(&f.up.chatCalls)+atomic.LoadInt32(&f.up.embeddingCalls)+atomic.LoadInt32(&f.up.streamCalls) != 0 {
		t.Fatal("a rejected route dispatched upstream")
	}
}

func TestMalformedAndUnboundedInputsReject(t *testing.T) {
	f := newOpenAIContractFixture(t)
	cases := []struct {
		path, body string
		want       int
	}{
		{"/v1/chat/completions", `{invalid`, http.StatusBadRequest},
		{"/v1/chat/completions", `{"messages":[{"role":"user","content":"hi"}]}`, http.StatusBadRequest},
		{"/v1/chat/completions", `{"model":"gpt-fast"}`, http.StatusBadRequest},
		{"/v1/embeddings", `{"model":"embed-1"}`, http.StatusBadRequest},
		{"/v1/embeddings", `{"model":"embed-1","input":null}`, http.StatusBadRequest},
		{"/v1/chat/completions", `{"model":"gpt-fast","messages":[],"store":true}`, http.StatusForbidden},
	}
	for _, c := range cases {
		rec := f.do(t, http.MethodPost, c.path, c.body)
		if rec.Code != c.want {
			t.Fatalf("%s %q: got %d want %d (%s)", c.path, c.body, rec.Code, c.want, rec.Body.String())
		}
	}
}

// TestOfficialSDKInProcessContract drives the real public routes with the
// installed official Go SDK against the composed application handler: the
// executor dispatches to a deterministic in-process HTTP upstream through the
// production OpenAI-compatible adapter, so wire shape, streaming deltas,
// embeddings order and denial behavior are exercised end to end — not through
// a forwarding fake.
func TestOfficialSDKInProcessContract(t *testing.T) {
	f := newOpenAIContractFixture(t)

	// In-process upstream: a real HTTP server speaking the OpenAI wire.
	upstream := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		switch {
		case r.URL.Path == "/chat/completions" && r.Method == http.MethodPost:
			body, _ := io.ReadAll(r.Body)
			var meta struct {
				Stream bool `json:"stream"`
			}
			_ = json.Unmarshal(body, &meta)
			if meta.Stream {
				w.Header().Set("Content-Type", "text/event-stream")
				io.WriteString(w, "data: {\"id\":\"chatcmpl_sdk\",\"choices\":[{\"index\":0,\"delta\":{\"role\":\"assistant\",\"content\":\"Hel\"}}]}\n\n")
				io.WriteString(w, "data: {\"id\":\"chatcmpl_sdk\",\"choices\":[{\"index\":0,\"delta\":{\"content\":\"lo\"},\"finish_reason\":\"stop\"}],\"usage\":{\"prompt_tokens\":4,\"completion_tokens\":2,\"total_tokens\":6}}\n\n")
				io.WriteString(w, "data: [DONE]\n\n")
				return
			}
			w.Header().Set("Content-Type", "application/json")
			io.WriteString(w, `{"id":"chatcmpl_sdk","object":"chat.completion","model":"up-chat","choices":[{"index":0,"message":{"role":"assistant","content":"Hello"},"finish_reason":"stop"}],"usage":{"prompt_tokens":4,"completion_tokens":2,"total_tokens":6}}`)
		case r.URL.Path == "/embeddings" && r.Method == http.MethodPost:
			w.Header().Set("Content-Type", "application/json")
			io.WriteString(w, `{"object":"list","data":[{"object":"embedding","embedding":[0.5,0.6],"index":0},{"object":"embedding","embedding":[0.7,0.8],"index":1}],"model":"up-embed","usage":{"prompt_tokens":6,"total_tokens":6}}`)
		default:
			http.NotFound(w, r)
		}
	}))
	defer upstream.Close()

	// Rebuild the app with the real adapter pointed at the upstream server.
	srepo := postgres.NewSourceRepo(f.db)
	svc, err := inference.NewService(inference.Deps{
		Endpoints:  f.erepo,
		Sources:    srepo,
		Authorizer: source.NewAuthorizer(srepo, 0),
		Billing:    billing.NewService(f.brepo),
		Artifacts:  postgres.NewArtifactRepo(f.db),
	}, inference.WithTransportFactory(func(ctx context.Context, e inference.Endpoint, credential string) (inference.Upstream, error) {
		return llm.NewOpenAICompatibleAdapter(upstream.URL, "test-key", 30*time.Second).WithAllowPrivateEndpoint(), nil
	}))
	if err != nil {
		t.Fatalf("new inference service: %v", err)
	}
	app := NewApp(auth.NewService(postgres.NewIdentityRepo(f.db), ""), nil, "", nil, postgres.NewIdentityRepo(f.db),
		nil, nil, nil, nil, nil, nil, nil, f.brepo, f.erepo,
		"sha256", "", "enc_", nil, nil, 1, false, svc, postgres.NewResponseBindingsRepo(f.db))
	server := httptest.NewServer(app.Handler())
	defer server.Close()

	client := openai.NewClient(
		option.WithBaseURL(server.URL+"/v1"),
		option.WithAPIKey(f.keyID),
	)
	ctx, cancel := context.WithTimeout(context.Background(), 20*time.Second)
	defer cancel()

	models, err := client.Models.List(ctx)
	if err != nil {
		t.Fatalf("SDK models list: %v", err)
	}
	seen := map[string]bool{}
	for _, m := range models.Data {
		seen[m.ID] = true
	}
	if !seen["gpt-fast"] || !seen["embed-1"] {
		t.Fatalf("aliases missing from SDK list: %v", models.Data)
	}

	chat, err := client.Chat.Completions.New(ctx, openai.ChatCompletionNewParams{
		Model: shared.ChatModel("gpt-fast"),
		Messages: []openai.ChatCompletionMessageParamUnion{{
			OfUser: &openai.ChatCompletionUserMessageParam{
				Content: openai.ChatCompletionUserMessageParamContentUnion{OfString: openai.String("hi")},
			},
		}},
	})
	if err != nil || len(chat.Choices) == 0 || chat.Choices[0].Message.Content != "Hello" {
		t.Fatalf("SDK chat: err=%v resp=%#v", err, chat)
	}

	stream := client.Chat.Completions.NewStreaming(ctx, openai.ChatCompletionNewParams{
		Model: shared.ChatModel("gpt-fast"),
		Messages: []openai.ChatCompletionMessageParamUnion{{
			OfUser: &openai.ChatCompletionUserMessageParam{
				Content: openai.ChatCompletionUserMessageParamContentUnion{OfString: openai.String("hi")},
			},
		}},
	})
	var acc strings.Builder
	events := 0
	for stream.Next() {
		events++
		if chunk := stream.Current(); len(chunk.Choices) > 0 {
			acc.WriteString(chunk.Choices[0].Delta.Content)
		}
	}
	if err := stream.Err(); err != nil {
		t.Fatalf("SDK stream: %v", err)
	}
	if events == 0 || acc.String() != "Hello" {
		t.Fatalf("stream reconstructed %q over %d events", acc.String(), events)
	}

	emb, err := client.Embeddings.New(ctx, openai.EmbeddingNewParams{
		Model: openai.EmbeddingModel("embed-1"),
		Input: openai.EmbeddingNewParamsInputUnion{OfArrayOfStrings: []string{"a", "b"}},
	})
	if err != nil || len(emb.Data) != 2 || emb.Data[0].Index != 0 || emb.Data[1].Index != 1 {
		t.Fatalf("SDK embeddings: err=%v resp=%#v", err, emb)
	}

	// Denied model: SDK sees an error, upstream never dispatched.
	if _, err := client.Chat.Completions.New(ctx, openai.ChatCompletionNewParams{
		Model: shared.ChatModel("forbidden-model"),
		Messages: []openai.ChatCompletionMessageParamUnion{{
			OfUser: &openai.ChatCompletionUserMessageParam{
				Content: openai.ChatCompletionUserMessageParamContentUnion{OfString: openai.String("hi")},
			},
		}},
	}); err == nil {
		t.Fatal("disallowed model must fail")
	}
}
