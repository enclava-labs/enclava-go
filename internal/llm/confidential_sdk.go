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
	if key != "" {
		cfg["allow_inline_api_keys"] = true
		keys, ok := cfg["api_keys"].(map[string]any)
		if !ok {
			keys = map[string]any{}
		}
		keys[provider] = map[string]any{"inline": key}
		cfg["api_keys"] = keys
	}
	b, err := json.Marshal(cfg)
	if err != nil {
		return nil, err
	}
	client, err := ci.NewClient(b)
	if err != nil {
		return nil, err
	}
	return &ConfidentialSDK{client: client, provider: provider}, nil
}

func (p *ConfidentialSDK) Close() error { return p.client.Close() }

func (p *ConfidentialSDK) Supports(capability string) bool {
	return capability == "chat/completions" || capability == "vision"
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
	result, err := p.client.Chat(req, timeout)
	if ctx.Err() != nil {
		return nil, ctx.Err()
	}
	if err != nil {
		return nil, &UpstreamError{StatusCode: 502, Message: err.Error()}
	}
	verdict, ok := result["verdict"].(map[string]any)
	if !ok || verdict["status"] != "verified" {
		return nil, &UpstreamError{StatusCode: 502, Message: "confidential SDK verdict not verified"}
	}
	response, ok := result["response"]
	if !ok {
		return nil, &UpstreamError{StatusCode: 502, Message: "confidential SDK response missing"}
	}
	return json.Marshal(response)
}

func (p *ConfidentialSDK) CreateChatCompletionStream(context.Context, []byte) (io.ReadCloser, error) {
	return nil, errors.New("unsupported capability")
}
func (p *ConfidentialSDK) CreateEmbeddings(context.Context, []byte) ([]byte, error) {
	return nil, errors.New("unsupported capability")
}
func (p *ConfidentialSDK) DoResponsesRequest(context.Context, string, string, []byte) ([]byte, error) {
	return nil, errors.New("unsupported capability")
}
func (p *ConfidentialSDK) DoResponsesStream(context.Context, string, string, []byte) (io.ReadCloser, error) {
	return nil, errors.New("unsupported capability")
}
