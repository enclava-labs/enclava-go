package llm

import (
	"context"
	"encoding/json"
	"fmt"
	"io"
	"log"
	"net/http"
	"net/url"
	"strings"
	"sync"
	"time"

	"github.com/openai/openai-go/v3/option"
	tinfoil "github.com/tinfoilsh/tinfoil-go"
	verifierclient "github.com/tinfoilsh/tinfoil-go/verifier/client"
	"golang.org/x/sync/singleflight"
)

var (
	newTinfoilClient           = tinfoil.NewClient
	newTinfoilClientWithParams = tinfoil.NewClientWithParams
)

type TinfoilProvider struct {
	clientMu      sync.Mutex
	client        *tinfoil.Client
	apiKey        string
	enclave       string
	repo          string
	baseURL       string
	jsonClient    *http.Client
	streamClient  *http.Client
	timeout       time.Duration
	verifyPerCall bool
	verifyMaxAge  time.Duration
	verifyGroup   singleflight.Group
	// verifyCtx is the application shutdown context: verification work is
	// bounded by verifyTimeout and survives a single cancelled caller.
	verifyCtx     context.Context
	verifyTimeout time.Duration
	streamIdle    time.Duration
	streamWall    time.Duration

	mu     sync.RWMutex
	status AttestationStatus
}

func NewTinfoilProvider(
	apiKey string,
	enclave string,
	repo string,
	timeout time.Duration,
	verifyAtStart bool,
	verifyPerCall bool,
	verifyMaxAge time.Duration,
) (*TinfoilProvider, error) {
	if strings.TrimSpace(apiKey) == "" {
		return nil, fmt.Errorf("TINFOIL_API_KEY is required for tinfoil provider")
	}

	hasEnclave := strings.TrimSpace(enclave) != ""
	hasRepo := strings.TrimSpace(repo) != ""
	if hasEnclave != hasRepo {
		return nil, fmt.Errorf("both TINFOIL_ENCLAVE and TINFOIL_REPO must be set together")
	}
	if !verifyPerCall && verifyMaxAge <= 0 {
		verifyMaxAge = 10 * time.Minute
		log.Printf("warning: TINFOIL_VERIFY_MAX_AGE is 0 with TINFOIL_VERIFY_PER_CALL=false; defaulting to %v", verifyMaxAge)
	}

	provider := &TinfoilProvider{
		apiKey:        apiKey,
		enclave:       strings.TrimSpace(enclave),
		repo:          strings.TrimSpace(repo),
		timeout:       timeout,
		verifyPerCall: verifyPerCall,
		verifyMaxAge:  verifyMaxAge,
		verifyCtx:     context.Background(),
		verifyTimeout: 60 * time.Second,
		streamIdle:    defaultStreamIdle,
		streamWall:    defaultStreamWall,
		status: AttestationStatus{
			Enabled:  true,
			Verified: false,
			Provider: "tinfoil",
			Enclave:  strings.TrimSpace(enclave),
			Repo:     strings.TrimSpace(repo),
		},
	}

	if verifyAtStart {
		if _, err := provider.VerifyAttestation(context.Background()); err != nil {
			return nil, err
		}
	}

	return provider, nil
}

// WithShutdownContext binds shared verification work to the application
// shutdown lifecycle instead of any single caller's context.
func (p *TinfoilProvider) WithShutdownContext(ctx context.Context) *TinfoilProvider {
	if ctx != nil {
		p.verifyCtx = ctx
	}
	return p
}

// WithStreamLimits bounds streaming reads: idle is the max gap between reads,
// wall the max lifetime of one stream. Zero disables a limit.
func (p *TinfoilProvider) WithStreamLimits(idle, wall time.Duration) *TinfoilProvider {
	p.streamIdle = idle
	p.streamWall = wall
	return p
}

func (p *TinfoilProvider) AttestationStatus() AttestationStatus {
	p.mu.RLock()
	defer p.mu.RUnlock()
	return p.status
}

func (p *TinfoilProvider) VerifyAttestation(ctx context.Context) (AttestationStatus, error) {
	status := p.AttestationStatus()
	if status.Verified && !p.verifyPerCall && p.verifyMaxAge > 0 && time.Since(status.VerifiedAt) < p.verifyMaxAge {
		return status, nil
	}

	ch := p.verifyGroup.DoChan("verify", func() (any, error) {
		// Verification runs on the shutdown context with a bounded deadline so a
		// cancelled waiter neither kills the shared work nor leaks a goroutine.
		ctx := p.verifyCtx
		if ctx == nil {
			ctx = context.Background()
		}
		vctx, cancel := context.WithTimeout(ctx, p.verifyTimeout)
		defer cancel()
		return p.verifyAttestationOnce(vctx)
	})

	select {
	case res := <-ch:
		if res.Err != nil {
			return p.AttestationStatus(), res.Err
		}
		return res.Val.(AttestationStatus), nil
	case <-ctx.Done():
		// Don't flip global attestation status on caller cancellation. Callers should treat ctx errors
		// as verification failures for this operation, but the last known good status may still hold.
		return p.AttestationStatus(), ctx.Err()
	}
}

func (p *TinfoilProvider) verifyAttestationOnce(ctx context.Context) (AttestationStatus, error) {
	client, err := p.ensureClient(ctx, nil)
	status := p.AttestationStatus()
	status.Enabled = true
	status.Provider = "tinfoil"
	status.Enclave = p.enclave
	status.Repo = p.repo

	if err != nil {
		status.Verified = false
		status.LastError = err.Error()
		p.setStatus(status)
		return status, err
	}

	groundTruth, err := verifyWithContext(ctx, client)
	if err != nil {
		status.Verified = false
		status.LastError = err.Error()
		p.setStatus(status)
		return status, fmt.Errorf("tinfoil attestation verification failed: %w", err)
	}

	status.Verified = true
	status.LastError = ""
	status.VerifiedAt = time.Now().UTC()
	status.Enclave = client.Enclave()
	status.Repo = client.Repo()
	if groundTruth != nil {
		status.Digest = groundTruth.Digest
		status.CodeFingerprint = groundTruth.CodeFingerprint
		status.EnclaveFingerprint = groundTruth.EnclaveFingerprint
	}
	p.setStatus(status)
	return status, nil
}

// verifyWithContext runs the SDK's blocking Verify in a goroutine bounded by
// ctx. If Verify itself hangs past the deadline it still leaks one goroutine,
// but callers and the singleflight group are released truthfully.
func verifyWithContext(ctx context.Context, client *tinfoil.Client) (*verifierclient.GroundTruth, error) {
	type result struct {
		gt  *verifierclient.GroundTruth
		err error
	}
	ch := make(chan result, 1)
	go func() {
		gt, err := client.Verify()
		ch <- result{gt, err}
	}()
	select {
	case r := <-ch:
		return r.gt, r.err
	case <-ctx.Done():
		return nil, ctx.Err()
	}
}

func (p *TinfoilProvider) ListModels(ctx context.Context) (ModelsResponse, error) {
	if err := p.ensureVerified(ctx); err != nil {
		return ModelsResponse{}, err
	}
	body, err := p.doJSON(ctx, http.MethodGet, "/models", nil)
	if err != nil {
		return ModelsResponse{}, err
	}
	var out ModelsResponse
	if err := json.Unmarshal(body, &out); err != nil {
		return ModelsResponse{}, fmt.Errorf("decode models response: %w", err)
	}
	return NormalizeModels(out, "tinfoil"), nil
}

func (p *TinfoilProvider) GetModel(ctx context.Context, modelID string) (Model, error) {
	if err := p.ensureVerified(ctx); err != nil {
		return Model{}, err
	}
	body, err := p.doJSON(ctx, http.MethodGet, "/models/"+url.PathEscape(modelID), nil)
	if err != nil {
		return Model{}, err
	}
	var out Model
	if err := json.Unmarshal(body, &out); err != nil {
		return Model{}, fmt.Errorf("decode model response: %w", err)
	}
	return NormalizeModel(out, "tinfoil"), nil
}

func (p *TinfoilProvider) Health(ctx context.Context) error {
	_, err := p.ListModels(ctx)
	return err
}

func (p *TinfoilProvider) CreateChatCompletion(ctx context.Context, body []byte) ([]byte, error) {
	if err := p.ensureVerified(ctx); err != nil {
		return nil, err
	}
	return p.doJSON(ctx, http.MethodPost, "/chat/completions", body)
}

func (p *TinfoilProvider) CreateEmbeddings(ctx context.Context, body []byte) ([]byte, error) {
	if err := p.ensureVerified(ctx); err != nil {
		return nil, err
	}
	return p.doJSON(ctx, http.MethodPost, "/embeddings", body)
}

func (p *TinfoilProvider) CreateChatCompletionStream(ctx context.Context, body []byte) (io.ReadCloser, error) {
	if err := p.ensureVerified(ctx); err != nil {
		return nil, err
	}
	return p.doStream(ctx, http.MethodPost, "/chat/completions", body)
}

func (p *TinfoilProvider) DoResponsesRequest(ctx context.Context, method, path string, body []byte) ([]byte, error) {
	if err := p.ensureVerified(ctx); err != nil {
		return nil, err
	}
	return p.doJSON(ctx, method, path, body)
}

func (p *TinfoilProvider) DoResponsesStream(ctx context.Context, method, path string, body []byte) (io.ReadCloser, error) {
	if err := p.ensureVerified(ctx); err != nil {
		return nil, err
	}
	return p.doStream(ctx, method, path, body)
}

func (p *TinfoilProvider) doStream(ctx context.Context, method, path string, body []byte) (io.ReadCloser, error) {
	_, streamClient, err := p.requestClients()
	if err != nil {
		return nil, err
	}
	requestURL := p.baseURL + path
	req, err := http.NewRequestWithContext(ctx, method, requestURL, noRetryBody(body))
	if err != nil {
		return nil, fmt.Errorf("build stream request: %w", err)
	}
	req.Header.Set("Authorization", "Bearer "+p.apiKey)
	req.Header.Set("Content-Type", "application/json")
	req.Header.Set("Accept", "text/event-stream")

	resp, err := streamClient.Do(req)
	if err != nil {
		return nil, fmt.Errorf("upstream chat stream request failed: %w", err)
	}
	if resp.StatusCode >= 300 {
		upstreamBody, _ := readBoundedBody(resp.Body, maxErrorBodyBytes)
		return nil, &UpstreamError{StatusCode: resp.StatusCode, Body: upstreamBody}
	}
	return newBoundedStreamBody(resp.Body, p.streamIdle, p.streamWall), nil
}

func (p *TinfoilProvider) ensureVerified(ctx context.Context) error {
	status := p.AttestationStatus()
	isStale := p.verifyMaxAge > 0 && status.Verified && time.Since(status.VerifiedAt) >= p.verifyMaxAge
	if p.verifyPerCall || !status.Verified || isStale {
		if _, err := p.VerifyAttestation(ctx); err != nil {
			return fmt.Errorf("tinfoil attestation required: %w", err)
		}
	}
	return nil
}

func (p *TinfoilProvider) doJSON(ctx context.Context, method, path string, body []byte) ([]byte, error) {
	jsonClient, _, err := p.requestClients()
	if err != nil {
		return nil, err
	}
	requestURL := p.baseURL + path
	req, err := http.NewRequestWithContext(ctx, method, requestURL, noRetryBody(body))
	if err != nil {
		return nil, fmt.Errorf("build request: %w", err)
	}
	req.Header.Set("Authorization", "Bearer "+p.apiKey)
	if body != nil {
		req.Header.Set("Content-Type", "application/json")
	}

	resp, err := jsonClient.Do(req)
	if err != nil {
		return nil, fmt.Errorf("upstream request failed: %w", err)
	}

	respBody, err := readBoundedBody(resp.Body, 64*1024*1024)
	if err != nil {
		return nil, err
	}
	if resp.StatusCode >= 300 {
		return nil, &UpstreamError{StatusCode: resp.StatusCode, Body: respBody}
	}
	return respBody, nil
}

func (p *TinfoilProvider) setStatus(status AttestationStatus) {
	p.mu.Lock()
	defer p.mu.Unlock()
	p.status = status
}

func (p *TinfoilProvider) ensureClient(ctx context.Context, opts []option.RequestOption) (*tinfoil.Client, error) {
	p.clientMu.Lock()
	defer p.clientMu.Unlock()
	if p.client != nil {
		return p.client, nil
	}
	if opts == nil {
		// WithMaxRetries(0): the SDK must never replay a possibly accepted
		// mutation or a partially observed stream on the verified enclave.
		opts = []option.RequestOption{option.WithAPIKey(p.apiKey), option.WithMaxRetries(0)}
	}

	var (
		tinfoilClient *tinfoil.Client
		err           error
	)
	if p.enclave != "" {
		tinfoilClient, err = newTinfoilClientWithParams(p.enclave, p.repo, opts...)
	} else {
		tinfoilClient, err = newTinfoilClient(opts...)
	}
	if err != nil {
		return nil, fmt.Errorf("create tinfoil client: %w", err)
	}

	httpClient := tinfoilClient.HTTPClient()
	if p.timeout > 0 {
		httpClient.Timeout = p.timeout
	}

	streamClient := *httpClient
	// Streaming client has no overall timeout: SSE streams are bounded by the
	// idle/wall limits applied to the returned body instead.
	streamClient.Timeout = 0
	// Enclave-bound clients never follow redirects to another origin: the EHBP
	// transport already refuses non-enclave hosts, and credentials must not
	// leak to a redirect target.
	httpClient.CheckRedirect = sameOriginRedirectPolicy
	streamClient.CheckRedirect = sameOriginRedirectPolicy

	p.client = tinfoilClient
	p.enclave = tinfoilClient.Enclave()
	p.repo = tinfoilClient.Repo()
	p.baseURL = fmt.Sprintf("https://%s/v1", tinfoilClient.Enclave())
	p.jsonClient = httpClient
	p.streamClient = &streamClient

	status := p.AttestationStatus()
	status.Enclave = p.enclave
	status.Repo = p.repo
	p.setStatus(status)
	return p.client, nil
}

func (p *TinfoilProvider) requestClients() (*http.Client, *http.Client, error) {
	if _, err := p.ensureClient(context.Background(), nil); err != nil {
		return nil, nil, err
	}

	p.clientMu.Lock()
	defer p.clientMu.Unlock()
	return p.jsonClient, p.streamClient, nil
}
