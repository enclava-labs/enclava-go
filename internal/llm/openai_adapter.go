package llm

import (
	"bytes"
	"context"
	"encoding/json"
	"fmt"
	"io"
	"mime"
	"net/http"
	"net/url"
	"path"
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

func (a *OpenAICompatibleAdapter) DoPassthroughRequest(ctx context.Context, method, path string, body []byte, headers http.Header) (PassthroughResponse, error) {
	if a.cfgErr != nil {
		return PassthroughResponse{}, fmt.Errorf("endpoint not approved: %w", a.cfgErr)
	}
	client := a.client
	streaming := passthroughStreamClientSelected(path, headers, body)
	if streaming {
		client = a.streamClient
	}
	resp, err := doPassthroughHTTP(ctx, client, a.baseURL, a.apiKey, method, path, body, headers)
	if err != nil {
		return PassthroughResponse{}, err
	}
	if streaming {
		resp.Body = newBoundedStreamBody(resp.Body, a.streamIdle, a.streamWall)
	}
	return resp, nil
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

func doPassthroughHTTP(ctx context.Context, client *http.Client, baseURL, apiKey, method, path string, body []byte, headers http.Header) (PassthroughResponse, error) {
	requestURL, err := resolvePassthroughURL(baseURL, path)
	if err != nil {
		return PassthroughResponse{}, fmt.Errorf("build passthrough request: %w", err)
	}
	req, err := http.NewRequestWithContext(ctx, method, requestURL.String(), noRetryBody(body))
	if err != nil {
		return PassthroughResponse{}, fmt.Errorf("build passthrough request: %w", err)
	}
	req.Header.Set("Authorization", "Bearer "+apiKey)
	copyPassthroughRequestHeaders(req.Header, headers)

	resp, err := client.Do(req)
	if err != nil {
		return PassthroughResponse{}, fmt.Errorf("upstream passthrough request failed: %w", err)
	}
	return PassthroughResponse{
		StatusCode: resp.StatusCode,
		Header:     resp.Header.Clone(),
		Body:       resp.Body,
	}, nil
}

func copyPassthroughRequestHeaders(dst, src http.Header) {
	for name, values := range src {
		if !forwardPassthroughRequestHeader(name) {
			continue
		}
		for _, value := range values {
			dst.Add(name, value)
		}
	}
	if dst.Get("Accept") == "" {
		dst.Set("Accept", "application/json")
	}
}

func forwardPassthroughRequestHeader(name string) bool {
	canonical := http.CanonicalHeaderKey(name)
	switch canonical {
	case "Accept", "Content-Type", "Idempotency-Key", "Openai-Beta", "Openai-Organization", "Openai-Project", "Openai-Version", "Openai-Intent", "X-Client-Request-Id":
		return true
	default:
		return strings.HasPrefix(strings.ToLower(canonical), "openai-")
	}
}

func passthroughShouldUseStreamClient(path string, headers http.Header) bool {
	return passthroughStreamClientSelected(path, headers, nil)
}

func passthroughStreamClientSelected(path string, headers http.Header, body []byte) bool {
	if strings.Contains(strings.ToLower(headers.Get("Accept")), "text/event-stream") {
		return true
	}
	if strings.Contains(strings.ToLower(path), "stream=true") {
		return true
	}
	if !passthroughContentTypeAllowsStreamFlag(headers) {
		return false
	}
	return jsonBodyRequestsStream(body)
}

func passthroughContentTypeAllowsStreamFlag(headers http.Header) bool {
	contentType := strings.TrimSpace(headers.Get("Content-Type"))
	if contentType == "" {
		return true
	}
	mediaType, _, err := mime.ParseMediaType(contentType)
	if err != nil {
		return false
	}
	return strings.EqualFold(mediaType, "application/json")
}

func jsonBodyRequestsStream(body []byte) bool {
	if len(bytes.TrimSpace(body)) == 0 {
		return false
	}
	var payload struct {
		Stream bool `json:"stream"`
	}
	if err := json.Unmarshal(body, &payload); err != nil {
		return false
	}
	return payload.Stream
}

func resolvePassthroughURL(baseURL, rawPath string) (*url.URL, error) {
	base, err := url.Parse(strings.TrimSpace(baseURL))
	if err != nil || base.Scheme == "" || base.Host == "" {
		return nil, fmt.Errorf("invalid provider base url")
	}
	ref, err := url.Parse(rawPath)
	if err != nil {
		return nil, fmt.Errorf("invalid passthrough path")
	}
	if ref.IsAbs() || ref.Host != "" {
		return nil, fmt.Errorf("passthrough path escapes provider base")
	}
	rawPathPart := passthroughRawPath(rawPath)
	if rawPathPart == "" || rawPathPart[0] != '/' || ref.Path == "" || ref.Path[0] != '/' {
		return nil, fmt.Errorf("passthrough path must be absolute")
	}
	if strings.Contains(rawPathPart, `\`) || strings.Contains(ref.Path, `\`) || strings.Contains(rawPathPart, "://") {
		return nil, fmt.Errorf("passthrough path escapes provider base")
	}
	if strings.Contains(strings.ToLower(rawPathPart), "%2e%2e") || strings.Contains(ref.Path, "..") {
		return nil, fmt.Errorf("passthrough path escapes provider base")
	}
	basePath := base.Path
	if basePath == "" {
		basePath = "/"
	}
	joined := basePath
	if !strings.HasSuffix(joined, "/") {
		joined += "/"
	}
	joined += strings.TrimPrefix(ref.Path, "/")
	cleaned := path.Clean(joined)
	if !pathStaysUnderBase(basePath, cleaned) {
		return nil, fmt.Errorf("passthrough path escapes provider base")
	}
	resolved := *base
	resolved.Path = cleaned
	resolved.RawPath = ""
	resolved.RawQuery = ref.RawQuery
	resolved.Fragment = ""
	resolved.RawFragment = ""
	if !strings.EqualFold(resolved.Scheme, base.Scheme) || !strings.EqualFold(resolved.Host, base.Host) {
		return nil, fmt.Errorf("passthrough path escapes provider base")
	}
	return &resolved, nil
}

func passthroughRawPath(rawPath string) string {
	if i := strings.IndexByte(rawPath, '?'); i >= 0 {
		rawPath = rawPath[:i]
	}
	if i := strings.IndexByte(rawPath, '#'); i >= 0 {
		rawPath = rawPath[:i]
	}
	return rawPath
}

func pathStaysUnderBase(basePath, cleaned string) bool {
	baseClean := path.Clean(basePath)
	if baseClean == "." || baseClean == "" {
		baseClean = "/"
	}
	cleaned = path.Clean(cleaned)
	if cleaned == "." || cleaned == "" || !strings.HasPrefix(cleaned, "/") || strings.Contains(cleaned, "..") || strings.Contains(cleaned, `\`) {
		return false
	}
	if baseClean == "/" {
		return true
	}
	return cleaned == baseClean || strings.HasPrefix(cleaned, baseClean+"/")
}
