package llm

import (
	"context"
	"encoding/json"
	"errors"
	"fmt"
	"io"
	"os"
	"time"

	ci "github.com/enclava-labs/confidential-inference-sdk/bindings/go"
)

type ConfidentialSDK struct {
	client   *ci.Client
	provider string
}

func NewConfidentialSDK(path, provider, origin, key string) (*ConfidentialSDK, error) {
	if provider == "" {
		return nil, errors.New("SDK provider id is required")
	}
	b, err := sdkClientConfig(path, provider, key)
	if err != nil {
		return nil, err
	}
	client, err := ci.NewClient(b)
	if err != nil {
		return nil, err
	}
	return &ConfidentialSDK{client: client, provider: provider}, nil
}

// sdkClientConfig loads the shared operator trust config and attaches only the
// endpoint's tenant-resolved credential. api_keys in the shared file (e.g. env
// references) are dropped: they would be an ambient credential every tenant's
// endpoint could use.
func sdkClientConfig(path, provider, key string) ([]byte, error) {
	cfg := map[string]any{}
	if path != "" {
		b, err := os.ReadFile(path)
		if err != nil {
			return nil, fmt.Errorf("read confidential SDK config: %w", err)
		}
		if err := json.Unmarshal(b, &cfg); err != nil {
			return nil, fmt.Errorf("parse confidential SDK config: %w", err)
		}
		if cfg == nil {
			return nil, errors.New("confidential SDK config must be a JSON object")
		}
	}
	delete(cfg, "api_keys")
	delete(cfg, "allow_inline_api_keys")
	if key != "" {
		cfg["allow_inline_api_keys"] = true
		cfg["api_keys"] = map[string]any{provider: map[string]any{"inline": key}}
	}
	return json.Marshal(cfg)
}

func (p *ConfidentialSDK) Close() error { return p.client.Close() }

func (p *ConfidentialSDK) Supports(capability string) bool {
	return capability == "chat/completions" || capability == "vision" || capability == "embeddings"
}

func (p *ConfidentialSDK) VerifyAttestation(ctx context.Context) (AttestationStatus, error) {
	timeout := time.Duration(0)
	if deadline, ok := ctx.Deadline(); ok {
		timeout = time.Until(deadline)
	}
	verdict, err := p.client.Verify(p.provider, "", timeout)
	if ctx.Err() != nil {
		return AttestationStatus{}, ctx.Err()
	}
	if err != nil {
		return AttestationStatus{}, err
	}
	status, _ := verdict["status"].(string)
	if status != "verified" {
		return AttestationStatus{}, fmt.Errorf("confidential SDK verdict not verified")
	}
	return AttestationStatus{Enabled: true, Verified: true, Provider: p.provider, VerifiedAt: time.Now().UTC()}, nil
}

func (p *ConfidentialSDK) AttestationStatus() AttestationStatus {
	return AttestationStatus{Enabled: true, Provider: p.provider}
}

func (p *ConfidentialSDK) CreateChatCompletion(ctx context.Context, body []byte) ([]byte, error) {
	return p.execute(ctx, body, func(req any, timeout time.Duration) (map[string]any, error) {
		return p.client.Chat(req, timeout)
	})
}

func (p *ConfidentialSDK) CreateEmbeddings(ctx context.Context, body []byte) ([]byte, error) {
	return p.execute(ctx, body, func(req any, timeout time.Duration) (map[string]any, error) {
		return p.client.Embeddings(req, timeout)
	})
}

// execute runs one blocking SDK operation under the caller's deadline and
// enforces the verified-verdict contract on its result.
func (p *ConfidentialSDK) execute(ctx context.Context, body []byte, call func(any, time.Duration) (map[string]any, error)) ([]byte, error) {
	var req any
	if err := json.Unmarshal(body, &req); err != nil {
		return nil, err
	}
	timeout := time.Duration(0)
	if deadline, ok := ctx.Deadline(); ok {
		timeout = time.Until(deadline)
		if timeout <= 0 {
			return nil, ctx.Err()
		}
	}
	result, err := call(req, timeout)
	if ctx.Err() != nil {
		return nil, ctx.Err()
	}
	if err != nil {
		return nil, sdkOperationError(err)
	}
	verdict, ok := result["verdict"].(map[string]any)
	if !ok || verdict["status"] != "verified" {
		// The provider already executed this request: not a definitive
		// rejection, so liability must stay retained, never settle as free.
		return nil, errors.New("confidential SDK verdict not verified after execution")
	}
	response, ok := result["response"]
	if !ok {
		return nil, errors.New("confidential SDK response missing after execution")
	}
	return json.Marshal(response)
}

// sdkOperationError maps an SDK failure onto the executor's accounting
// contract: only a definitive provider rejection is an UpstreamError (settled
// with its reported usage). Never-dispatched and unknown outcomes stay plain
// errors so the executor retains liability instead of settling them as free.
func sdkOperationError(err error) error {
	var sdkErr *ci.Error
	if errors.As(err, &sdkErr) && sdkErr.Dispatch == ci.DispatchRejected && sdkErr.HTTPStatus > 0 {
		return &UpstreamError{StatusCode: sdkErr.HTTPStatus, Message: sdkErr.Message}
	}
	return fmt.Errorf("confidential SDK: %w", err)
}

func (p *ConfidentialSDK) CreateChatCompletionStream(context.Context, []byte) (io.ReadCloser, error) {
	return nil, errors.New("unsupported capability")
}
func (p *ConfidentialSDK) DoResponsesRequest(context.Context, string, string, []byte) ([]byte, error) {
	return nil, errors.New("unsupported capability")
}
func (p *ConfidentialSDK) DoResponsesStream(context.Context, string, string, []byte) (io.ReadCloser, error) {
	return nil, errors.New("unsupported capability")
}
