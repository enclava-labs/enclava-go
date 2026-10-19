package llm

import (
	"context"
	"encoding/json"
	"fmt"
	"io"
	"net/http"
	"net/url"
	"strings"
	"time"
)

type OpenAICompatibleAdapter struct {
	baseURL      string
	apiKey       string
	provider     string
	client       *http.Client
	streamClient *http.Client
	cfgErr       error
	allowPrivate bool
	maxBody      int64
	streamIdle   time.Duration
	streamWall   time.Duration
	timeout      time.Duration
}

// NewOpenAICompatibleAdapter builds a provider transport for one approved
// endpoint origin. Private/cleartext targets are denied unless the caller
// marks the endpoint explicitly approved via WithAllowPrivateEndpoint.
func NewOpenAICompatibleAdapter(baseURL, apiKey string, timeout time.Duration) *OpenAICompatibleAdapter {
	baseURL = strings.TrimSuffix(strings.TrimSpace(baseURL), "/")
	a := &OpenAICompatibleAdapter{
		baseURL:    baseURL,
		apiKey:     apiKey,
		provider:   "openai_compatible",
		maxBody:    maxJSONBodyBytes,
		streamIdle: defaultStreamIdle,
		streamWall: defaultStreamWall,
	}
	a.timeout = timeout
	a.rebuildClients(timeout)
	return a
}

func (a *OpenAICompatibleAdapter) rebuildClients(timeout time.Duration) {
	if timeout == 0 {
		timeout = a.timeout
	}
	a.timeout = timeout
	u, err := validateEndpointBaseURL(a.baseURL, a.allowPrivate)
	if err != nil {
		a.cfgErr = err
		a.client = &http.Client{}
		a.streamClient = &http.Client{}
		return
	}
	a.cfgErr = nil
	origin := originOf(u)
	jsonTransport := endpointTransport(origin, a.allowPrivate, defaultResponseHeader)
	streamTransport := endpointTransport(origin, a.allowPrivate, defaultStreamHeader)
	// Streaming client has no overall Timeout: long-running SSE streams are
	// bounded by response-header, idle and wall limits on the body instead.
	a.client = &http.Client{
		Timeout:       timeout,
		Transport:     jsonTransport,
		CheckRedirect: sameOriginRedirectPolicy,
	}
	a.streamClient = &http.Client{
		Timeout:       0,
		Transport:     streamTransport,
		CheckRedirect: sameOriginRedirectPolicy,
	}
}

// WithAllowPrivateEndpoint marks this endpoint's private/cleartext target as
// explicitly approved by administrative configuration. A private URL alone is
// not an access grant: the adapter refuses it until this is set.
func (a *OpenAICompatibleAdapter) WithAllowPrivateEndpoint() *OpenAICompatibleAdapter {
	a.allowPrivate = true
	a.rebuildClients(0)
	return a
}

// WithStreamLimits bounds streaming reads: idle is the max gap between reads,
// wall the max lifetime of one stream. Zero disables a limit.
func (a *OpenAICompatibleAdapter) WithStreamLimits(idle, wall time.Duration) *OpenAICompatibleAdapter {
	a.streamIdle = idle
	a.streamWall = wall
	return a
}

func (a *OpenAICompatibleAdapter) WithProviderName(provider string) *OpenAICompatibleAdapter {
	provider = strings.TrimSpace(provider)
	if provider != "" {
		a.provider = provider
	}
	return a
}

func (a *OpenAICompatibleAdapter) ListModels(ctx context.Context) (ModelsResponse, error) {
	body, err := a.doJSON(ctx, http.MethodGet, "/models", nil)
	if err != nil {
		return ModelsResponse{}, err
	}
	var out ModelsResponse
	if err := json.Unmarshal(body, &out); err != nil {
		return ModelsResponse{}, fmt.Errorf("decode models response: %w", err)
	}
	return NormalizeModels(out, a.provider), nil
}

func (a *OpenAICompatibleAdapter) GetModel(ctx context.Context, modelID string) (Model, error) {
	body, err := a.doJSON(ctx, http.MethodGet, "/models/"+url.PathEscape(modelID), nil)
	if err != nil {
		return Model{}, err
	}
	var out Model
	if err := json.Unmarshal(body, &out); err != nil {
		return Model{}, fmt.Errorf("decode model response: %w", err)
	}
	return NormalizeModel(out, a.provider), nil
}

func (a *OpenAICompatibleAdapter) Health(ctx context.Context) error {
	_, err := a.ListModels(ctx)
	return err
}

func (a *OpenAICompatibleAdapter) CreateChatCompletion(ctx context.Context, body []byte) ([]byte, error) {
	return a.doJSON(ctx, http.MethodPost, "/chat/completions", body)
}

func (a *OpenAICompatibleAdapter) CreateEmbeddings(ctx context.Context, body []byte) ([]byte, error) {
	return a.doJSON(ctx, http.MethodPost, "/embeddings", body)
}

func (a *OpenAICompatibleAdapter) CreateChatCompletionStream(ctx context.Context, body []byte) (io.ReadCloser, error) {
	return a.doStream(ctx, http.MethodPost, "/chat/completions", body)
}

func (a *OpenAICompatibleAdapter) DoResponsesRequest(ctx context.Context, method, path string, body []byte) ([]byte, error) {
	return a.doJSON(ctx, method, path, body)
}

func (a *OpenAICompatibleAdapter) DoResponsesStream(ctx context.Context, method, path string, body []byte) (io.ReadCloser, error) {
	return a.doStream(ctx, method, path, body)
}

func (a *OpenAICompatibleAdapter) doStream(ctx context.Context, method, path string, body []byte) (io.ReadCloser, error) {
	if a.cfgErr != nil {
		return nil, fmt.Errorf("endpoint not approved: %w", a.cfgErr)
	}
	requestURL := a.baseURL + path
	req, err := http.NewRequestWithContext(ctx, method, requestURL, noRetryBody(body))
	if err != nil {
		return nil, fmt.Errorf("build stream request: %w", err)
	}
	req.Header.Set("Authorization", "Bearer "+a.apiKey)
	req.Header.Set("Content-Type", "application/json")
	req.Header.Set("Accept", "text/event-stream")

	resp, err := a.streamClient.Do(req)
	if err != nil {
		return nil, fmt.Errorf("upstream chat stream request failed: %w", err)
	}
	if resp.StatusCode >= 300 {
		upstreamBody, _ := readBoundedBody(resp.Body, maxErrorBodyBytes)
		return nil, &UpstreamError{StatusCode: resp.StatusCode, Body: upstreamBody}
	}
	return newBoundedStreamBody(resp.Body, a.streamIdle, a.streamWall), nil
}

func (a *OpenAICompatibleAdapter) doJSON(ctx context.Context, method, path string, body []byte) ([]byte, error) {
	if a.cfgErr != nil {
		return nil, fmt.Errorf("endpoint not approved: %w", a.cfgErr)
	}
	url := a.baseURL + path
	req, err := http.NewRequestWithContext(ctx, method, url, noRetryBody(body))
	if err != nil {
		return nil, fmt.Errorf("build request: %w", err)
	}
	req.Header.Set("Authorization", "Bearer "+a.apiKey)
	if body != nil {
		req.Header.Set("Content-Type", "application/json")
	}

	resp, err := a.client.Do(req)
	if err != nil {
		return nil, fmt.Errorf("upstream request failed: %w", err)
	}

	limit := a.maxBody
	if resp.StatusCode >= 300 {
		limit = maxErrorBodyBytes
	}
	respBody, err := readBoundedBody(resp.Body, limit)
	if err != nil {
		return nil, err
	}
	if resp.StatusCode >= 300 {
		return nil, &UpstreamError{StatusCode: resp.StatusCode, Body: respBody}
	}

	return respBody, nil
}
