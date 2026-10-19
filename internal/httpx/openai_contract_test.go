package httpx

import (
	"bytes"
	"context"
	"encoding/json"
	"io"
	"mime/multipart"
	"net/http"
	"net/http/httptest"
	"path/filepath"
	"strings"
	"testing"
	"time"

	"enclava-go/internal/auth"
	"enclava-go/internal/billing"
	"enclava-go/internal/llm"
	"enclava-go/internal/store/postgres"
	"enclava-go/internal/store/sqlite"
	"enclava-go/internal/testdb"
)

type contractProvider struct {
	models                    llm.ModelsResponse
	chatCalls                 int
	embeddingCalls            int
	responseCalls             int
	responseStreamCalls       int
	lastChatRequest           []byte
	lastResponseMethod        string
	lastResponsePath          string
	lastResponseRequest       []byte
	passthroughCalls          int
	lastPassthroughMethod     string
	lastPassthroughPath       string
	lastPassthroughRequest    []byte
	lastPassthroughHeaders    http.Header
	passthroughOverride       bool
	passthroughOverrideBody   string
	passthroughOverrideHeader http.Header
	chatErr                   error
	embeddingErr              error
	responseErr               error
	responseStreamErr         error
	chatStreamBody            string
	responseStreamBody        string
}

func (p *contractProvider) ListModels(ctx context.Context) (llm.ModelsResponse, error) {
	return p.models, nil
}

func (p *contractProvider) GetModel(ctx context.Context, modelID string) (llm.Model, error) {
	return llm.Model{ID: modelID}, nil
}

func (p *contractProvider) CreateChatCompletion(ctx context.Context, body []byte) ([]byte, error) {
	p.chatCalls++
	p.lastChatRequest = append([]byte(nil), body...)
	if p.chatErr != nil {
		return nil, p.chatErr
	}
	return []byte(`{"id":"chatcmpl_test","object":"chat.completion","model":"contract-model","choices":[{"index":0,"message":{"role":"assistant","content":"ok"},"finish_reason":"stop"}],"usage":{"prompt_tokens":3,"completion_tokens":4,"total_tokens":7}}`), nil
}

func (p *contractProvider) CreateChatCompletionStream(ctx context.Context, body []byte) (io.ReadCloser, error) {
	p.lastChatRequest = append([]byte(nil), body...)
	if p.chatErr != nil {
		return nil, p.chatErr
	}
	bodyText := p.chatStreamBody
	if bodyText == "" {
		bodyText = "data: [DONE]\n\n"
	}
	return io.NopCloser(bytes.NewBufferString(bodyText)), nil
}

func (p *contractProvider) CreateEmbeddings(ctx context.Context, body []byte) ([]byte, error) {
	p.embeddingCalls++
	if p.embeddingErr != nil {
		return nil, p.embeddingErr
	}
	return []byte(`{"object":"list","data":[{"object":"embedding","embedding":[0.1],"index":0}],"model":"embed-model","usage":{"prompt_tokens":2,"total_tokens":2}}`), nil
}

func (p *contractProvider) DoResponsesRequest(ctx context.Context, method, path string, body []byte) ([]byte, error) {
	p.responseCalls++
	p.lastResponseMethod = method
	p.lastResponsePath = path
	p.lastResponseRequest = append([]byte(nil), body...)
	if p.responseErr != nil {
		return nil, p.responseErr
	}
	return []byte(`{"id":"resp_test","object":"response","model":"contract-model","output":[],"usage":{"input_tokens":5,"output_tokens":6,"total_tokens":11}}`), nil
}

func (p *contractProvider) DoResponsesStream(ctx context.Context, method, path string, body []byte) (io.ReadCloser, error) {
	p.responseStreamCalls++
	p.lastResponseMethod = method
	p.lastResponsePath = path
	p.lastResponseRequest = append([]byte(nil), body...)
	if p.responseStreamErr != nil {
		return nil, p.responseStreamErr
	}
	bodyText := p.responseStreamBody
	if bodyText == "" {
		bodyText = "event: response.completed\ndata: {\"type\":\"response.completed\",\"response\":{\"id\":\"resp_test\",\"object\":\"response\",\"model\":\"contract-model\",\"usage\":{\"input_tokens\":5,\"output_tokens\":6,\"total_tokens\":11}}}\n\n"
	}
	return io.NopCloser(bytes.NewBufferString(bodyText)), nil
}

func (p *contractProvider) DoPassthroughRequest(ctx context.Context, method, path string, body []byte, headers http.Header) (llm.PassthroughResponse, error) {
	p.passthroughCalls++
	p.lastPassthroughMethod = method
	p.lastPassthroughPath = path
	p.lastPassthroughRequest = append([]byte(nil), body...)
	p.lastPassthroughHeaders = headers.Clone()

	if p.passthroughOverride {
		header := http.Header{}
		if p.passthroughOverrideHeader != nil {
			header = p.passthroughOverrideHeader.Clone()
		}
		return llm.PassthroughResponse{
			StatusCode: http.StatusOK,
			Header:     header,
			Body:       io.NopCloser(strings.NewReader(p.passthroughOverrideBody)),
		}, nil
	}

	header := http.Header{}
	switch {
	case strings.Contains(path, "/content"):
		header.Set("Content-Type", "text/plain")
		return llm.PassthroughResponse{
			StatusCode: http.StatusOK,
			Header:     header,
			Body:       io.NopCloser(strings.NewReader("file bytes")),
		}, nil
	case strings.Contains(strings.ToLower(headers.Get("Accept")), "text/event-stream"):
		header.Set("Content-Type", "text/event-stream")
		return llm.PassthroughResponse{
			StatusCode: http.StatusOK,
			Header:     header,
			Body:       io.NopCloser(strings.NewReader("data: {\"model\":\"contract-model\",\"usage\":{\"prompt_tokens\":4,\"completion_tokens\":5,\"total_tokens\":9}}\n\n")),
		}, nil
	default:
		header.Set("Content-Type", "application/json")
		return llm.PassthroughResponse{
			StatusCode: http.StatusOK,
			Header:     header,
			Body:       io.NopCloser(strings.NewReader(`{"object":"proxied","model":"contract-model","usage":{"prompt_tokens":2,"completion_tokens":3,"total_tokens":5}}`)),
		}, nil
	}
}

func TestOpenAIChatCompletionContractRecordsUsage(t *testing.T) {
	ctx := context.Background()
	db, _, apiKey, app, provider := newOpenAIContractApp(t, ctx)
	defer func() { _ = db.Close() }()

	body := []byte(`{"model":"contract-model","messages":[{"role":"user","content":"hello"}],"temperature":0}`)
	req := httptest.NewRequest(http.MethodPost, "/api/v1/chat/completions", bytes.NewReader(body))
	req.Header.Set("Authorization", "Bearer "+apiKey)
	req.Header.Set("Content-Type", "application/json")
	rec := httptest.NewRecorder()

	app.Handler().ServeHTTP(rec, req)

	if rec.Code != http.StatusOK {
		t.Fatalf("expected status 200, got %d body=%s", rec.Code, rec.Body.String())
	}
	if provider.chatCalls != 1 {
		t.Fatalf("expected one provider call, got %d", provider.chatCalls)
	}
	if !bytes.Equal(provider.lastChatRequest, body) {
		t.Fatalf("expected request body to be forwarded unchanged")
	}
	var usageCount int
	if err := db.SQL.QueryRowContext(ctx, `SELECT COUNT(1) FROM usage_records WHERE endpoint = ? AND total_tokens = 7`, "/api/v1/chat/completions").Scan(&usageCount); err != nil {
		t.Fatalf("query usage: %v", err)
	}
	if usageCount != 1 {
		t.Fatalf("expected one usage record, got %d", usageCount)
	}
}

func TestOpenAIChatCompletionStreamRecordsUsage(t *testing.T) {
	ctx := context.Background()
	db, _, apiKey, app, provider := newOpenAIContractApp(t, ctx)
	defer func() { _ = db.Close() }()
	provider.chatStreamBody = "data: {\"model\":\"contract-model\",\"usage\":{\"prompt_tokens\":8,\"completion_tokens\":9,\"total_tokens\":17}}\n\n"

	body := []byte(`{"model":"contract-model","messages":[{"role":"user","content":"stream"}],"stream":true}`)
	req := httptest.NewRequest(http.MethodPost, "/api/v1/chat/completions", bytes.NewReader(body))
	req.Header.Set("Authorization", "Bearer "+apiKey)
	req.Header.Set("Content-Type", "application/json")
	rec := httptest.NewRecorder()

	app.Handler().ServeHTTP(rec, req)

	if rec.Code != http.StatusOK {
		t.Fatalf("expected status 200, got %d body=%s", rec.Code, rec.Body.String())
	}
	if rec.Header().Get("Content-Type") != "text/event-stream" {
		t.Fatalf("expected SSE content type, got %q", rec.Header().Get("Content-Type"))
	}
	if !bytes.Equal(provider.lastChatRequest, body) {
		t.Fatalf("expected stream request body to be forwarded unchanged")
	}
	var totalTokens int64
	if err := db.SQL.QueryRowContext(ctx, `SELECT total_tokens FROM usage_records WHERE endpoint = ?`, "/api/v1/chat/completions").Scan(&totalTokens); err != nil {
		t.Fatalf("query stream usage: %v", err)
	}
	if totalTokens != 17 {
		t.Fatalf("expected stream usage total 17, got %d", totalTokens)
	}
}

func TestOpenAIChatCompletionUpstreamErrorPreservedAndRecorded(t *testing.T) {
	ctx := context.Background()
	db, _, apiKey, app, provider := newOpenAIContractApp(t, ctx)
	defer func() { _ = db.Close() }()
	provider.chatErr = &llm.UpstreamError{
		StatusCode: http.StatusTooManyRequests,
		Body:       []byte(`{"error":{"message":"slow down","type":"rate_limit_error","code":"upstream_rate_limit"}}`),
	}

	req := httptest.NewRequest(http.MethodPost, "/api/v1/chat/completions", bytes.NewReader([]byte(`{"model":"contract-model","messages":[{"role":"user","content":"hello"}]}`)))
	req.Header.Set("Authorization", "Bearer "+apiKey)
	req.Header.Set("Content-Type", "application/json")
	rec := httptest.NewRecorder()

	app.Handler().ServeHTTP(rec, req)

	if rec.Code != http.StatusTooManyRequests {
		t.Fatalf("expected upstream status 429, got %d body=%s", rec.Code, rec.Body.String())
	}
	if !bytes.Contains(rec.Body.Bytes(), []byte("upstream_rate_limit")) {
		t.Fatalf("expected upstream error envelope to be preserved, got %s", rec.Body.String())
	}
	var statusCode int
	if err := db.SQL.QueryRowContext(ctx, `SELECT status_code FROM usage_records WHERE endpoint = ?`, "/api/v1/chat/completions").Scan(&statusCode); err != nil {
		t.Fatalf("query failed call usage: %v", err)
	}
	if statusCode != http.StatusTooManyRequests {
		t.Fatalf("expected failed usage status 429, got %d", statusCode)
	}
}

func TestOpenAIEmbeddingsContractRecordsUsage(t *testing.T) {
	ctx := context.Background()
	db, _, apiKey, app, provider := newOpenAIContractApp(t, ctx)
	defer func() { _ = db.Close() }()

	req := httptest.NewRequest(http.MethodPost, "/api/v1/embeddings", bytes.NewReader([]byte(`{"model":"embed-model","input":"hello"}`)))
	req.Header.Set("Authorization", "Bearer "+apiKey)
	req.Header.Set("Content-Type", "application/json")
	rec := httptest.NewRecorder()

	app.Handler().ServeHTTP(rec, req)

	if rec.Code != http.StatusOK {
		t.Fatalf("expected status 200, got %d body=%s", rec.Code, rec.Body.String())
	}
	if provider.embeddingCalls != 1 {
		t.Fatalf("expected one embedding call, got %d", provider.embeddingCalls)
	}
	var totalTokens int64
	if err := db.SQL.QueryRowContext(ctx, `SELECT total_tokens FROM usage_records WHERE endpoint = ?`, "/api/v1/embeddings").Scan(&totalTokens); err != nil {
		t.Fatalf("query embedding usage: %v", err)
	}
	if totalTokens != 2 {
		t.Fatalf("expected embedding usage total 2, got %d", totalTokens)
	}
}

func TestOpenAIV1AliasRoutes(t *testing.T) {
	ctx := context.Background()
	db, _, apiKey, app, provider := newOpenAIContractApp(t, ctx)
	defer func() { _ = db.Close() }()

	for _, tc := range []struct {
		method string
		path   string
		body   string
	}{
		{method: http.MethodGet, path: "/v1/models"},
		{method: http.MethodGet, path: "/v1/models/contract-model"},
		{method: http.MethodPost, path: "/v1/chat/completions", body: `{"model":"contract-model","messages":[{"role":"user","content":"hello"}]}`},
		{method: http.MethodPost, path: "/v1/embeddings", body: `{"model":"embed-model","input":"hello"}`},
		{method: http.MethodPost, path: "/v1/responses", body: `{"model":"contract-model","input":"hello"}`},
		{method: http.MethodGet, path: "/v1/responses/resp_123/input_items?limit=1"},
	} {
		req := httptest.NewRequest(tc.method, tc.path, strings.NewReader(tc.body))
		req.Header.Set("Authorization", "Bearer "+apiKey)
		req.Header.Set("Content-Type", "application/json")
		rec := httptest.NewRecorder()

		app.Handler().ServeHTTP(rec, req)

		if rec.Code != http.StatusOK {
			t.Fatalf("%s %s expected 200, got %d body=%s", tc.method, tc.path, rec.Code, rec.Body.String())
		}
	}
	if provider.chatCalls != 1 {
		t.Fatalf("expected one aliased chat call, got %d", provider.chatCalls)
	}
	if provider.embeddingCalls != 1 {
		t.Fatalf("expected one aliased embedding call, got %d", provider.embeddingCalls)
	}
	if provider.responseCalls != 2 {
		t.Fatalf("expected two aliased responses calls, got %d", provider.responseCalls)
	}
	if provider.lastResponsePath != "/responses/resp_123/input_items?limit=1" {
		t.Fatalf("expected aliased responses subroute to preserve upstream path, got %q", provider.lastResponsePath)
	}

	for _, endpoint := range []string{"/api/v1/chat/completions", "/api/v1/embeddings", "/api/v1/responses"} {
		var count int
		if err := db.SQL.QueryRowContext(ctx, `SELECT COUNT(1) FROM usage_records WHERE endpoint = ?`, endpoint).Scan(&count); err != nil {
			t.Fatalf("query usage for %s: %v", endpoint, err)
		}
		if count != 1 {
			t.Fatalf("expected one canonical usage record for %s, got %d", endpoint, count)
		}
	}
}

func TestEndpointRestrictionsTreatOpenAIAliasesAsEquivalent(t *testing.T) {
	ctx := context.Background()
	db, ident, apiKey, app, provider := newOpenAIContractApp(t, ctx)
	defer func() { _ = db.Close() }()
	// allowed_endpoints carries inference endpoint IDs (route families), not
	// URL paths: "chat/completions" and "embeddings" granted; "models" is not.
	updateStoredKey(t, ctx, ident, "allowed_endpoints", listGrantJSON("chat/completions", "embeddings"))

	chatReq := httptest.NewRequest(http.MethodPost, "/v1/chat/completions", strings.NewReader(`{"model":"contract-model","messages":[]}`))
	chatReq.Header.Set("Authorization", "Bearer "+apiKey)
	chatReq.Header.Set("Content-Type", "application/json")
	chatRec := httptest.NewRecorder()
	app.Handler().ServeHTTP(chatRec, chatReq)
	if chatRec.Code != http.StatusOK {
		t.Fatalf("expected /v1 chat to match chat/completions grant, got %d body=%s", chatRec.Code, chatRec.Body.String())
	}

	embedReq := httptest.NewRequest(http.MethodPost, "/api/v1/embeddings", strings.NewReader(`{"model":"embed-model","input":"hello"}`))
	embedReq.Header.Set("Authorization", "Bearer "+apiKey)
	embedReq.Header.Set("Content-Type", "application/json")
	embedRec := httptest.NewRecorder()
	app.Handler().ServeHTTP(embedRec, embedReq)
	if embedRec.Code != http.StatusOK {
		t.Fatalf("expected /api/v1 embeddings to match embeddings grant, got %d body=%s", embedRec.Code, embedRec.Body.String())
	}

	modelsReq := httptest.NewRequest(http.MethodGet, "/v1/models", nil)
	modelsReq.Header.Set("Authorization", "Bearer "+apiKey)
	modelsRec := httptest.NewRecorder()
	app.Handler().ServeHTTP(modelsRec, modelsReq)
	if modelsRec.Code != http.StatusForbidden {
		t.Fatalf("expected unlisted endpoint id to be rejected, got %d body=%s", modelsRec.Code, modelsRec.Body.String())
	}
	if provider.chatCalls != 1 || provider.embeddingCalls != 1 {
		t.Fatalf("unexpected provider calls: chat=%d embeddings=%d", provider.chatCalls, provider.embeddingCalls)
	}
}

func TestOpenAIGenericPassthroughProxiesAdditionalRouteFamilies(t *testing.T) {
	ctx := context.Background()
	db, _, apiKey, app, provider := newOpenAIContractApp(t, ctx)
	defer func() { _ = db.Close() }()

	jsonBody := []byte(`{"model":"contract-model","input":"check this"}`)
	req := httptest.NewRequest(http.MethodPost, "/v1/moderations", bytes.NewReader(jsonBody))
	req.Header.Set("Authorization", "Bearer "+apiKey)
	req.Header.Set("Content-Type", "application/json")
	rec := httptest.NewRecorder()

	app.Handler().ServeHTTP(rec, req)

	if rec.Code != http.StatusOK {
		t.Fatalf("moderations passthrough expected 200, got %d body=%s", rec.Code, rec.Body.String())
	}
	if provider.lastPassthroughMethod != http.MethodPost || provider.lastPassthroughPath != "/moderations" {
		t.Fatalf("unexpected moderation passthrough: method=%s path=%s", provider.lastPassthroughMethod, provider.lastPassthroughPath)
	}
	if !bytes.Equal(provider.lastPassthroughRequest, jsonBody) {
		t.Fatalf("expected JSON passthrough body to be preserved")
	}
	if !strings.Contains(rec.Body.String(), `"proxied"`) {
		t.Fatalf("expected upstream JSON body, got %s", rec.Body.String())
	}

	assistantBody := []byte(`{"model":"contract-model","name":"helper"}`)
	req = httptest.NewRequest(http.MethodPost, "/v1/assistants", bytes.NewReader(assistantBody))
	req.Header.Set("Authorization", "Bearer "+apiKey)
	req.Header.Set("Content-Type", "application/json")
	req.Header.Set("OpenAI-Beta", "assistants=v2")
	rec = httptest.NewRecorder()

	app.Handler().ServeHTTP(rec, req)

	if rec.Code != http.StatusOK {
		t.Fatalf("assistants passthrough expected 200, got %d body=%s", rec.Code, rec.Body.String())
	}
	if provider.lastPassthroughMethod != http.MethodPost || provider.lastPassthroughPath != "/assistants" {
		t.Fatalf("unexpected assistants passthrough: method=%s path=%s", provider.lastPassthroughMethod, provider.lastPassthroughPath)
	}
	if !bytes.Equal(provider.lastPassthroughRequest, assistantBody) {
		t.Fatalf("expected assistants passthrough body to be preserved")
	}

	var upload bytes.Buffer
	multipartWriter := multipart.NewWriter(&upload)
	if err := multipartWriter.WriteField("purpose", "assistants"); err != nil {
		t.Fatalf("write purpose: %v", err)
	}
	filePart, err := multipartWriter.CreateFormFile("file", "contract.jsonl")
	if err != nil {
		t.Fatalf("create file part: %v", err)
	}
	if _, err := filePart.Write([]byte(`{"custom_id":"1"}`)); err != nil {
		t.Fatalf("write file part: %v", err)
	}
	if err := multipartWriter.Close(); err != nil {
		t.Fatalf("close multipart: %v", err)
	}

	req = httptest.NewRequest(http.MethodPost, "/api/v1/files", bytes.NewReader(upload.Bytes()))
	req.Header.Set("Authorization", "Bearer "+apiKey)
	req.Header.Set("Content-Type", multipartWriter.FormDataContentType())
	req.Header.Set("OpenAI-Beta", "assistants=v2")
	rec = httptest.NewRecorder()

	app.Handler().ServeHTTP(rec, req)

	if rec.Code != http.StatusOK {
		t.Fatalf("files passthrough expected 200, got %d body=%s", rec.Code, rec.Body.String())
	}
	if provider.lastPassthroughPath != "/files" {
		t.Fatalf("expected files upstream path, got %q", provider.lastPassthroughPath)
	}
	if !strings.HasPrefix(provider.lastPassthroughHeaders.Get("Content-Type"), "multipart/form-data") {
		t.Fatalf("expected multipart content type forwarded, got %q", provider.lastPassthroughHeaders.Get("Content-Type"))
	}
	if provider.lastPassthroughHeaders.Get("OpenAI-Beta") != "assistants=v2" {
		t.Fatalf("expected OpenAI-Beta header to be forwarded")
	}
	if !bytes.Contains(provider.lastPassthroughRequest, []byte(`{"custom_id":"1"}`)) {
		t.Fatalf("expected multipart body to be preserved")
	}

	req = httptest.NewRequest(http.MethodGet, "/v1/files/file_123/content", nil)
	req.Header.Set("Authorization", "Bearer "+apiKey)
	rec = httptest.NewRecorder()

	app.Handler().ServeHTTP(rec, req)

	if rec.Code != http.StatusOK || rec.Body.String() != "file bytes" {
		t.Fatalf("file content passthrough mismatch: status=%d body=%q", rec.Code, rec.Body.String())
	}
	if rec.Header().Get("Content-Type") != "text/plain" {
		t.Fatalf("expected upstream content type, got %q", rec.Header().Get("Content-Type"))
	}

	req = httptest.NewRequest(http.MethodGet, "/v1/chat/completions?limit=1", nil)
	req.Header.Set("Authorization", "Bearer "+apiKey)
	rec = httptest.NewRecorder()

	app.Handler().ServeHTTP(rec, req)

	if rec.Code != http.StatusOK {
		t.Fatalf("chat completions list passthrough expected 200, got %d body=%s", rec.Code, rec.Body.String())
	}
	if provider.lastPassthroughPath != "/chat/completions?limit=1" {
		t.Fatalf("expected chat completions list upstream path, got %q", provider.lastPassthroughPath)
	}

	req = httptest.NewRequest(http.MethodDelete, "/v1/models/ft:contract", nil)
	req.Header.Set("Authorization", "Bearer "+apiKey)
	rec = httptest.NewRecorder()

	app.Handler().ServeHTTP(rec, req)

	if rec.Code != http.StatusOK {
		t.Fatalf("model delete passthrough expected 200, got %d body=%s", rec.Code, rec.Body.String())
	}
	if provider.lastPassthroughMethod != http.MethodDelete || provider.lastPassthroughPath != "/models/ft:contract" {
		t.Fatalf("expected model delete passthrough, method=%s path=%s", provider.lastPassthroughMethod, provider.lastPassthroughPath)
	}

	req = httptest.NewRequest(http.MethodGet, "/v1/embeddings?limit=1", nil)
	req.Header.Set("Authorization", "Bearer "+apiKey)
	rec = httptest.NewRecorder()

	app.Handler().ServeHTTP(rec, req)

	if rec.Code != http.StatusOK {
		t.Fatalf("embeddings fallback passthrough expected 200, got %d body=%s", rec.Code, rec.Body.String())
	}
	if provider.lastPassthroughMethod != http.MethodGet || provider.lastPassthroughPath != "/embeddings?limit=1" {
		t.Fatalf("expected embeddings fallback passthrough, method=%s path=%s", provider.lastPassthroughMethod, provider.lastPassthroughPath)
	}

	var usageCount int
	if err := db.SQL.QueryRowContext(ctx, `SELECT COUNT(1) FROM usage_records WHERE endpoint IN (?, ?, ?, ?, ?, ?, ?)`, "/api/v1/moderations", "/api/v1/assistants", "/api/v1/files", "/api/v1/files/file_123/content", "/api/v1/chat/completions", "/api/v1/models/ft:contract", "/api/v1/embeddings").Scan(&usageCount); err != nil {
		t.Fatalf("query passthrough usage: %v", err)
	}
	if usageCount != 7 {
		t.Fatalf("expected seven passthrough usage records, got %d", usageCount)
	}
}

func TestOpenAIProxyEndpointRejectsTraversal(t *testing.T) {
	rejected := []string{
		"/chat/completions/../../../latest/meta-data",
		"/chat/completions/%2e%2e/%2E%2E/latest/meta-data",
		"/chat/completions/%2E%2e/secret",
		`/chat/completions/\../latest`,
		"https://evil.example/chat/completions",
		"chat/completions",
		"",
	}
	for _, path := range rejected {
		if openAIProxyPathSafe(path) {
			t.Fatalf("expected %q to be rejected", path)
		}
	}
	if !openAIProxyPathSafe("/chat/completions") {
		t.Fatal("expected /chat/completions to be safe")
	}
	if !openAIProxyPathSafe("/chat/completions/abc") {
		t.Fatal("expected chat completions subpath to be safe")
	}
}

func TestOpenAIPassthroughRecordsTrailingJSONUsageBeyondCaptureLimit(t *testing.T) {
	ctx := context.Background()
	db, _, apiKey, app, provider := newOpenAIContractApp(t, ctx)
	defer func() { _ = db.Close() }()

	padding := strings.Repeat("a", maxSSEEventBytes)
	payload := `{"object":"proxied","model":"contract-model","pad":"` + padding + `","usage":{"prompt_tokens":41,"completion_tokens":42,"total_tokens":83}}`
	if len(payload) <= maxSSEEventBytes {
		t.Fatalf("fixture is not larger than the capture limit: %d", len(payload))
	}
	provider.passthroughOverride = true
	provider.passthroughOverrideBody = payload
	provider.passthroughOverrideHeader = http.Header{"Content-Type": []string{"application/json"}}

	req := httptest.NewRequest(http.MethodPost, "/v1/moderations", strings.NewReader(`{"model":"contract-model","input":"hello"}`))
	req.Header.Set("Authorization", "Bearer "+apiKey)
	req.Header.Set("Content-Type", "application/json")
	rec := httptest.NewRecorder()

	app.Handler().ServeHTTP(rec, req)

	if rec.Code != http.StatusOK {
		t.Fatalf("expected status 200, got %d", rec.Code)
	}
	if rec.Body.Len() != len(payload) || rec.Body.String() != payload {
		t.Fatalf("client body length=%d want %d", rec.Body.Len(), len(payload))
	}
	var promptTokens, completionTokens, totalTokens int64
	if err := db.SQL.QueryRowContext(ctx, `SELECT prompt_tokens, completion_tokens, total_tokens FROM usage_records WHERE endpoint = ?`, "/api/v1/moderations").Scan(&promptTokens, &completionTokens, &totalTokens); err != nil {
		t.Fatalf("query trailing usage: %v", err)
	}
	if promptTokens != 41 || completionTokens != 42 || totalTokens != 83 {
		t.Fatalf("unexpected trailing usage: prompt=%d completion=%d total=%d", promptTokens, completionTokens, totalTokens)
	}
}

func TestOpenAIPassthroughRecordsFinalSSEUsageBeyondCaptureLimit(t *testing.T) {
	ctx := context.Background()
	db, _, apiKey, app, provider := newOpenAIContractApp(t, ctx)
	defer func() { _ = db.Close() }()

	event := "data: {\"choices\":[{\"delta\":{\"content\":\"x\"}}]}\n\n"
	padding := strings.Repeat(event, (maxSSEEventBytes/len(event))+3)
	if len(padding) <= maxSSEEventBytes {
		t.Fatalf("sse padding is not larger than the capture limit: %d", len(padding))
	}
	final := "data: {\"model\":\"contract-model\",\"usage\":{\"prompt_tokens\":17,\"completion_tokens\":19,\"total_tokens\":36}}\n\ndata: [DONE]\n\n"
	payload := padding + final
	provider.passthroughOverride = true
	provider.passthroughOverrideBody = payload
	provider.passthroughOverrideHeader = http.Header{"Content-Type": []string{"text/event-stream"}}

	req := httptest.NewRequest(http.MethodPost, "/v1/moderations", strings.NewReader(`{"model":"contract-model","input":"hello"}`))
	req.Header.Set("Authorization", "Bearer "+apiKey)
	req.Header.Set("Content-Type", "application/json")
	rec := httptest.NewRecorder()

	app.Handler().ServeHTTP(rec, req)

	if rec.Code != http.StatusOK {
		t.Fatalf("expected status 200, got %d", rec.Code)
	}
	if rec.Body.String() != payload {
		t.Fatalf("client body length=%d want %d", rec.Body.Len(), len(payload))
	}
	var promptTokens, completionTokens, totalTokens int64
	if err := db.SQL.QueryRowContext(ctx, `SELECT prompt_tokens, completion_tokens, total_tokens FROM usage_records WHERE endpoint = ?`, "/api/v1/moderations").Scan(&promptTokens, &completionTokens, &totalTokens); err != nil {
		t.Fatalf("query sse usage: %v", err)
	}
	if promptTokens != 17 || completionTokens != 19 || totalTokens != 36 {
		t.Fatalf("unexpected sse usage: prompt=%d completion=%d total=%d", promptTokens, completionTokens, totalTokens)
	}
}

func TestOpenAIGenericPassthroughRequiresProxyScope(t *testing.T) {
	ctx := context.Background()
	db, ident, apiKey, app, provider := newOpenAIContractApp(t, ctx)
	defer func() { _ = db.Close() }()
	updateStoredKey(t, ctx, ident, "scopes", listGrantJSON("models.list", "chat.completions", "embeddings.create", "responses"))

	req := httptest.NewRequest(http.MethodPost, "/v1/moderations", strings.NewReader(`{"model":"contract-model","input":"hello"}`))
	req.Header.Set("Authorization", "Bearer "+apiKey)
	req.Header.Set("Content-Type", "application/json")
	rec := httptest.NewRecorder()

	app.Handler().ServeHTTP(rec, req)

	if rec.Code != http.StatusForbidden {
		t.Fatalf("expected missing proxy scope to be forbidden, got %d body=%s", rec.Code, rec.Body.String())
	}
	if provider.passthroughCalls != 0 {
		t.Fatalf("provider should not be called without proxy scope")
	}
}

func TestOpenAIGenericPassthroughAppliesModelRestrictions(t *testing.T) {
	ctx := context.Background()
	db, ident, apiKey, app, provider := newOpenAIContractApp(t, ctx)
	defer func() { _ = db.Close() }()
	updateStoredKey(t, ctx, ident, "allowed_models", listGrantJSON("contract-model"))

	req := httptest.NewRequest(http.MethodPost, "/v1/audio/transcriptions", strings.NewReader("model=blocked-model&file=fake"))
	req.Header.Set("Authorization", "Bearer "+apiKey)
	req.Header.Set("Content-Type", "application/x-www-form-urlencoded")
	rec := httptest.NewRecorder()

	app.Handler().ServeHTTP(rec, req)

	if rec.Code != http.StatusForbidden {
		t.Fatalf("expected blocked model to be forbidden, got %d body=%s", rec.Code, rec.Body.String())
	}
	if provider.passthroughCalls != 0 {
		t.Fatalf("provider should not be called for blocked model")
	}
}

func TestResponsesAPIContractRecordsUsage(t *testing.T) {
	ctx := context.Background()
	db, _, apiKey, app, provider := newOpenAIContractApp(t, ctx)
	defer func() { _ = db.Close() }()

	body := []byte(`{"model":"contract-model","input":"hello","metadata":{"trace":"abc"}}`)
	req := httptest.NewRequest(http.MethodPost, "/api/v1/responses", bytes.NewReader(body))
	req.Header.Set("Authorization", "Bearer "+apiKey)
	req.Header.Set("Content-Type", "application/json")
	rec := httptest.NewRecorder()

	app.Handler().ServeHTTP(rec, req)

	if rec.Code != http.StatusOK {
		t.Fatalf("expected status 200, got %d body=%s", rec.Code, rec.Body.String())
	}
	if provider.responseCalls != 1 || provider.lastResponseMethod != http.MethodPost || provider.lastResponsePath != "/responses" {
		t.Fatalf("unexpected responses provider call: calls=%d method=%s path=%s", provider.responseCalls, provider.lastResponseMethod, provider.lastResponsePath)
	}
	if !bytes.Equal(provider.lastResponseRequest, body) {
		t.Fatalf("expected responses body to be forwarded unchanged")
	}
	var promptTokens, completionTokens, totalTokens int64
	if err := db.SQL.QueryRowContext(ctx, `SELECT prompt_tokens, completion_tokens, total_tokens FROM usage_records WHERE endpoint = ?`, "/api/v1/responses").Scan(&promptTokens, &completionTokens, &totalTokens); err != nil {
		t.Fatalf("query responses usage: %v", err)
	}
	if promptTokens != 5 || completionTokens != 6 || totalTokens != 11 {
		t.Fatalf("unexpected responses usage: prompt=%d completion=%d total=%d", promptTokens, completionTokens, totalTokens)
	}
}

func TestResponsesAPIStreamsAndRecordsUsage(t *testing.T) {
	ctx := context.Background()
	db, _, apiKey, app, provider := newOpenAIContractApp(t, ctx)
	defer func() { _ = db.Close() }()

	body := []byte(`{"model":"contract-model","input":"hello","stream":true}`)
	req := httptest.NewRequest(http.MethodPost, "/api/v1/responses", bytes.NewReader(body))
	req.Header.Set("Authorization", "Bearer "+apiKey)
	req.Header.Set("Content-Type", "application/json")
	rec := httptest.NewRecorder()

	app.Handler().ServeHTTP(rec, req)

	if rec.Code != http.StatusOK {
		t.Fatalf("expected status 200, got %d body=%s", rec.Code, rec.Body.String())
	}
	if provider.responseStreamCalls != 1 || provider.lastResponsePath != "/responses" {
		t.Fatalf("expected response stream provider call, calls=%d path=%s", provider.responseStreamCalls, provider.lastResponsePath)
	}
	if !strings.Contains(rec.Body.String(), "response.completed") {
		t.Fatalf("expected streamed event body, got %s", rec.Body.String())
	}
	var totalTokens int64
	if err := db.SQL.QueryRowContext(ctx, `SELECT total_tokens FROM usage_records WHERE endpoint = ?`, "/api/v1/responses").Scan(&totalTokens); err != nil {
		t.Fatalf("query responses stream usage: %v", err)
	}
	if totalTokens != 11 {
		t.Fatalf("expected responses stream total 11, got %d", totalTokens)
	}
}

func TestResponsesAPIProxiesDocumentedSubroutes(t *testing.T) {
	ctx := context.Background()
	db, _, apiKey, app, provider := newOpenAIContractApp(t, ctx)
	defer func() { _ = db.Close() }()

	cases := []struct {
		method       string
		path         string
		upstreamPath string
	}{
		{method: http.MethodGet, path: "/api/v1/responses/resp_123?include[]=message.output_text.logprobs", upstreamPath: "/responses/resp_123?include[]=message.output_text.logprobs"},
		{method: http.MethodDelete, path: "/api/v1/responses/resp_123", upstreamPath: "/responses/resp_123"},
		{method: http.MethodGet, path: "/api/v1/responses/resp_123/input_items", upstreamPath: "/responses/resp_123/input_items"},
		{method: http.MethodPost, path: "/api/v1/responses/resp_123/cancel", upstreamPath: "/responses/resp_123/cancel"},
		{method: http.MethodPost, path: "/api/v1/responses/input_tokens", upstreamPath: "/responses/input_tokens"},
		{method: http.MethodPost, path: "/api/v1/responses/compact", upstreamPath: "/responses/compact"},
	}
	for _, tc := range cases {
		body := bytes.NewReader(nil)
		if tc.method == http.MethodPost {
			body = bytes.NewReader([]byte(`{"model":"contract-model","input":"hello"}`))
		}
		req := httptest.NewRequest(tc.method, tc.path, body)
		req.Header.Set("Authorization", "Bearer "+apiKey)
		req.Header.Set("Content-Type", "application/json")
		rec := httptest.NewRecorder()

		app.Handler().ServeHTTP(rec, req)

		if rec.Code != http.StatusOK {
			t.Fatalf("%s %s expected 200, got %d body=%s", tc.method, tc.path, rec.Code, rec.Body.String())
		}
		if provider.lastResponseMethod != tc.method || provider.lastResponsePath != tc.upstreamPath {
			t.Fatalf("%s %s upstream mismatch: method=%s path=%s", tc.method, tc.path, provider.lastResponseMethod, provider.lastResponsePath)
		}
	}
}

func TestResponsesBudgetAppliesOnlyToCreate(t *testing.T) {
	ctx := context.Background()
	db, ident, apiKey, app, provider := newOpenAIContractApp(t, ctx)
	defer func() { _ = db.Close() }()

	cancelBody := []byte(`{"model":"contract-model"}`)
	cancelReq := httptest.NewRequest(http.MethodPost, "/api/v1/responses/resp_123/cancel", bytes.NewReader(cancelBody))
	cancelReq.Header.Set("Authorization", "Bearer "+apiKey)
	cancelReq.Header.Set("Content-Type", "application/json")
	cancelRec := httptest.NewRecorder()
	app.Handler().ServeHTTP(cancelRec, cancelReq)
	if cancelRec.Code != http.StatusOK {
		t.Fatalf("cancel expected 200, got %d body=%s", cancelRec.Code, cancelRec.Body.String())
	}
	if provider.responseCalls != 1 || provider.lastResponsePath != "/responses/resp_123/cancel" {
		t.Fatalf("cancel did not reach upstream: calls=%d path=%s", provider.responseCalls, provider.lastResponsePath)
	}
	var usageCount int
	if err := db.SQL.QueryRowContext(ctx, `SELECT COUNT(1) FROM usage_records`).Scan(&usageCount); err != nil {
		t.Fatalf("query usage: %v", err)
	}
	if usageCount != 0 {
		t.Fatalf("cancel must not insert a usage row, got %d", usageCount)
	}

	if err := postgres.NewBillingRepo(ident.db).CreateBudget(ctx, ident.tenant, "contract_responses_budget", billing.Budget{
		Name:              "zero token budget",
		Subject:           billing.Subject{Type: billing.BudgetSubjectAPIKey, ID: ident.keyID},
		Kind:              billing.BudgetKindTokens,
		Period:            billing.PeriodMonthly,
		LimitAmountMicros: 0,
		StartsAt:          time.Now().UTC().Add(-time.Hour),
	}); err != nil {
		t.Fatalf("create budget: %v", err)
	}

	createReq := httptest.NewRequest(http.MethodPost, "/api/v1/responses", bytes.NewReader([]byte(`{"model":"contract-model","input":"hello"}`)))
	createReq.Header.Set("Authorization", "Bearer "+apiKey)
	createReq.Header.Set("Content-Type", "application/json")
	createRec := httptest.NewRecorder()
	app.Handler().ServeHTTP(createRec, createReq)
	if createRec.Code != http.StatusTooManyRequests {
		t.Fatalf("exhausted key expected 429 on POST /responses, got %d body=%s", createRec.Code, createRec.Body.String())
	}
	if provider.responseCalls != 1 {
		t.Fatalf("create must not reach upstream when budget is exhausted, calls=%d", provider.responseCalls)
	}

	cancelReq = httptest.NewRequest(http.MethodPost, "/api/v1/responses/resp_123/cancel", bytes.NewReader(cancelBody))
	cancelReq.Header.Set("Authorization", "Bearer "+apiKey)
	cancelReq.Header.Set("Content-Type", "application/json")
	cancelRec = httptest.NewRecorder()
	app.Handler().ServeHTTP(cancelRec, cancelReq)
	if cancelRec.Code != http.StatusOK {
		t.Fatalf("exhausted key cancel expected 200, got %d body=%s", cancelRec.Code, cancelRec.Body.String())
	}
	if provider.responseCalls != 2 || provider.lastResponsePath != "/responses/resp_123/cancel" {
		t.Fatalf("cancel should still reach upstream: calls=%d path=%s", provider.responseCalls, provider.lastResponsePath)
	}
	if err := db.SQL.QueryRowContext(ctx, `SELECT COUNT(1) FROM usage_records`).Scan(&usageCount); err != nil {
		t.Fatalf("query usage after budget: %v", err)
	}
	if usageCount != 0 {
		t.Fatalf("cancel must not insert a usage row, got %d", usageCount)
	}
}

func TestResponsesAPIFallsBackToGenericPassthroughForUnknownSubroutes(t *testing.T) {
	ctx := context.Background()
	db, _, apiKey, app, provider := newOpenAIContractApp(t, ctx)
	defer func() { _ = db.Close() }()

	body := []byte(`{"model":"contract-model","metadata":{"trace":"yes"}}`)
	req := httptest.NewRequest(http.MethodPatch, "/api/v1/responses/resp_123/metadata?include[]=usage", bytes.NewReader(body))
	req.Header.Set("Authorization", "Bearer "+apiKey)
	req.Header.Set("Content-Type", "application/json")
	rec := httptest.NewRecorder()

	app.Handler().ServeHTTP(rec, req)

	if rec.Code != http.StatusOK {
		t.Fatalf("responses fallback expected 200, got %d body=%s", rec.Code, rec.Body.String())
	}
	if provider.responseCalls != 0 {
		t.Fatalf("unknown responses subroute should not use typed responses provider, got %d calls", provider.responseCalls)
	}
	if provider.passthroughCalls != 1 || provider.lastPassthroughMethod != http.MethodPatch || provider.lastPassthroughPath != "/responses/resp_123/metadata?include[]=usage" {
		t.Fatalf("unexpected responses passthrough: calls=%d method=%s path=%s", provider.passthroughCalls, provider.lastPassthroughMethod, provider.lastPassthroughPath)
	}
	if !bytes.Equal(provider.lastPassthroughRequest, body) {
		t.Fatalf("expected responses fallback body to be preserved")
	}
}

func TestOpenAIChatCompletionBudgetExceededBeforeProviderCall(t *testing.T) {
	ctx := context.Background()
	db, ident, apiKey, app, provider := newOpenAIContractApp(t, ctx)
	defer func() { _ = db.Close() }()

	if err := postgres.NewBillingRepo(ident.db).CreateBudget(ctx, ident.tenant, "contract_budget", billing.Budget{
		Name:              "zero token budget",
		Subject:           billing.Subject{Type: billing.BudgetSubjectAPIKey, ID: ident.keyID},
		Kind:              billing.BudgetKindTokens,
		Period:            billing.PeriodMonthly,
		LimitAmountMicros: 0,
		StartsAt:          time.Now().UTC().Add(-time.Hour),
	}); err != nil {
		t.Fatalf("create budget: %v", err)
	}

	req := httptest.NewRequest(http.MethodPost, "/api/v1/chat/completions", bytes.NewReader([]byte(`{"model":"contract-model","messages":[]}`)))
	req.Header.Set("Authorization", "Bearer "+apiKey)
	req.Header.Set("Content-Type", "application/json")
	rec := httptest.NewRecorder()

	app.Handler().ServeHTTP(rec, req)

	if rec.Code != http.StatusTooManyRequests {
		t.Fatalf("expected status 429, got %d body=%s", rec.Code, rec.Body.String())
	}
	if provider.chatCalls != 0 {
		t.Fatalf("provider should not be called when budget is exceeded")
	}
	var envelope OpenAIErrorEnvelope
	if err := json.Unmarshal(rec.Body.Bytes(), &envelope); err != nil {
		t.Fatalf("decode error envelope: %v", err)
	}
	if envelope.Error.Code != "budget_exceeded" {
		t.Fatalf("expected budget_exceeded error, got %#v", envelope.Error)
	}
}

func TestOpenAIAPIKeyAllowedIPsSupportsCIDRAndTrustedProxy(t *testing.T) {
	ctx := context.Background()
	db, ident, apiKey, app, provider := newOpenAIContractApp(t, ctx)
	defer func() { _ = db.Close() }()
	app.trustProxy = true

	updateStoredKey(t, ctx, ident, "allowed_ips", listGrantJSON("198.51.100.0/24"))

	req := httptest.NewRequest(http.MethodGet, "/api/v1/models", nil)
	req.RemoteAddr = "203.0.113.10:4567"
	req.Header.Set("Authorization", "Bearer "+apiKey)
	req.Header.Set("X-Forwarded-For", "198.51.100.42, 10.0.0.5")
	rec := httptest.NewRecorder()

	app.Handler().ServeHTTP(rec, req)

	if rec.Code != http.StatusOK {
		t.Fatalf("expected CIDR-allowed proxied source to pass, got %d body=%s", rec.Code, rec.Body.String())
	}
	if provider.passthroughCalls != 0 {
		t.Fatalf("expected models route to use typed provider, got passthrough calls=%d", provider.passthroughCalls)
	}

	blockedReq := httptest.NewRequest(http.MethodGet, "/api/v1/models", nil)
	blockedReq.RemoteAddr = "203.0.113.10:4567"
	blockedReq.Header.Set("Authorization", "Bearer "+apiKey)
	blockedReq.Header.Set("X-Forwarded-For", "203.0.113.9")
	blockedRec := httptest.NewRecorder()

	app.Handler().ServeHTTP(blockedRec, blockedReq)

	if blockedRec.Code != http.StatusForbidden {
		t.Fatalf("expected blocked proxied source, got %d body=%s", blockedRec.Code, blockedRec.Body.String())
	}
}

// contractIdentity carries the real-PostgreSQL identity rows the app
// authenticates against (testdb fixture), alongside the sqlite db that
// still backs unported stores.
type contractIdentity struct {
	db     *postgres.DB
	repo   *postgres.IdentityRepo
	tenant string
	keyID  string
}

func newOpenAIContractApp(t *testing.T, ctx context.Context) (*sqlite.DB, contractIdentity, string, *App, *contractProvider) {
	t.Helper()
	db, err := sqlite.Open(ctx, filepath.Join(t.TempDir(), "openai-contract.db"))
	if err != nil {
		t.Fatalf("open db: %v", err)
	}
	// Authentication resolves against an isolated PostgreSQL identity store.
	pgdb := testdb.New(t)
	tenant := testdb.SeedTenant(t, pgdb, "contract")
	user := testdb.SeedUser(t, pgdb, tenant, "contract@example.com", nil)
	apiKey, keyRec := testdb.SeedAPIKey(t, pgdb, tenant, testdb.SeedKeySpec{
		Name:        "contract",
		OwnerUserID: user.ID,
		Scopes:      []string{"models.list", "chat.completions", "embeddings.create", "responses", "openai.proxy"},
	})
	ident := contractIdentity{
		db:     pgdb,
		repo:   postgres.NewIdentityRepo(pgdb),
		tenant: tenant,
		keyID:  keyRec.ID,
	}
	// Legacy numeric handle: unported sqlite stores (usage_records, budgets)
	// still FK-reference api_keys.id, so mirror the deterministic
	// principal.APIKeyID the service derives for this stored credential.
	seeded, err := auth.NewService(ident.repo, "").Authenticate(ctx, apiKey)
	if err != nil {
		t.Fatalf("authenticate seeded key: %v", err)
	}
	if _, err := db.SQL.ExecContext(ctx,
		`INSERT INTO api_keys (id, name, key_prefix, key_hash, is_active) VALUES (?, 'contract', '', '', 1)`,
		seeded.APIKeyID); err != nil {
		t.Fatalf("mirror legacy key row: %v", err)
	}

	provider := &contractProvider{
		models: llm.ModelsResponse{Object: "list", Data: []llm.Model{{ID: "contract-model"}, {ID: "embed-model", Tasks: []string{"embeddings"}}}},
	}
	app := NewApp(
		auth.NewService(ident.repo, ""),
		provider,
		"contract-provider",
		nil,
		ident.repo,
		nil,
		nil,
		nil,
		nil,
		nil,
		nil,
		nil,
		nil,
		sqlite.NewUsageRepo(db.SQL),
		nil,
		postgres.NewBillingRepo(pgdb),
		nil,
		"sha256",
		"",
		"enc_",
		nil,
		"",
		"",
		5,
		false,
	)
	return db, ident, apiKey, app, provider
}

// apiKeyIDForSecret resolves the deterministic legacy numeric handle the
// auth service derives for the stored credential (Principal.APIKeyID).
func apiKeyIDForSecret(t *testing.T, ctx context.Context, ident contractIdentity, apiKey string) int64 {
	t.Helper()
	principal, err := auth.NewService(ident.repo, "").Authenticate(ctx, apiKey)
	if err != nil {
		t.Fatalf("authenticate key: %v", err)
	}
	return principal.APIKeyID
}

// updateStoredKey mutates a stored grant column on the seeded PostgreSQL
// key row, exercising the real persistence path.
func updateStoredKey(t *testing.T, ctx context.Context, ident contractIdentity, column string, grant json.RawMessage) {
	t.Helper()
	rec, err := ident.repo.GetAPIKey(ctx, ident.tenant, ident.keyID)
	if err != nil {
		t.Fatalf("get key: %v", err)
	}
	switch column {
	case "allowed_models":
		rec.AllowedModels = grant
	case "allowed_endpoints":
		rec.AllowedEndpoints = grant
	case "allowed_ips":
		rec.AllowedIPs = grant
	case "scopes":
		rec.Scopes = grant
	default:
		t.Fatalf("unknown allowlist column %q", column)
	}
	if err := ident.repo.UpdateAPIKey(ctx, ident.tenant, rec); err != nil {
		t.Fatalf("update key: %v", err)
	}
}

func listGrantJSON(items ...string) json.RawMessage {
	raw, err := json.Marshal(struct {
		Mode  string   `json:"mode"`
		Items []string `json:"items"`
	}{Mode: "list", Items: items})
	if err != nil {
		panic(err)
	}
	return raw
}
