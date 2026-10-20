package llm

import (
	"context"
	"encoding/json"
	"errors"
	"fmt"
	"io"
	"net/url"
	"os"
	"strings"
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
	return capability == "chat/completions" || capability == "vision" ||
		capability == "embeddings" || capability == "responses" || capability == "responses/compact"
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

func (p *ConfidentialSDK) CreateChatCompletionStream(ctx context.Context, body []byte) (io.ReadCloser, error) {
	var req any
	if err := json.Unmarshal(body, &req); err != nil {
		return nil, err
	}
	if err := ctx.Err(); err != nil {
		return nil, err
	}
	stream, err := p.client.StartStream(req)
	if err != nil {
		return nil, sdkOperationError(err)
	}
	first, err := nextSDKStreamEvent(ctx, stream)
	if err != nil {
		closeSDKStream(stream)
		return nil, sdkOperationError(err)
	}
	if first["type"] != "verdict" {
		closeSDKStream(stream)
		return nil, sdkOperationError(sdkStreamEventError(first))
	}
	verdict, ok := first["verdict"].(map[string]any)
	if !ok || verdict["status"] != "verified" {
		closeSDKStream(stream)
		return nil, errors.New("confidential SDK stream verdict not verified")
	}
	streamCtx, cancel := context.WithCancel(ctx)
	reader, writer := io.Pipe()
	go func() {
		defer cancel()
		p.writeChatCompletionStream(streamCtx, stream, first, writer)
	}()
	return &cancellableStreamReader{PipeReader: reader, cancel: cancel}, nil
}

func nextSDKStreamEvent(ctx context.Context, stream *ci.Stream) (map[string]any, error) {
	for {
		if err := ctx.Err(); err != nil {
			stream.Cancel()
			return nil, err
		}
		event, err := stream.Next(250 * time.Millisecond)
		if err != nil || event != nil {
			return event, err
		}
	}
}

type cancellableStreamReader struct {
	*io.PipeReader
	cancel context.CancelFunc
}

func (r *cancellableStreamReader) Close() error {
	r.cancel()
	return r.PipeReader.Close()
}

// closeSDKStream waits for Next to observe cancellation before freeing the
// handle; the SDK returns StatusBusy rather than freeing an in-flight stream.
func closeSDKStream(stream *ci.Stream) {
	stream.Cancel()
	for {
		event, err := stream.Next(250 * time.Millisecond)
		if err != nil {
			break
		}
		if event == nil {
			continue
		}
		switch event["type"] {
		case "done", "cancelled", "closed", "error":
			_ = stream.Close()
			return
		}
	}
	_ = stream.Close()
}

func sdkStreamEventError(event map[string]any) error {
	detail, _ := event["error"].(map[string]any)
	if detail == nil {
		return errors.New("confidential SDK stream failed")
	}
	message, _ := detail["message"].(string)
	if message == "" {
		message = "confidential SDK stream failed"
	}
	code, _ := detail["code"].(string)
	if code == "" {
		code = "stream_failed"
	}
	dispatch, _ := detail["dispatch"].(string)
	status := 0
	if value, ok := detail["http_status"].(float64); ok {
		status = int(value)
	}
	return &ci.Error{
		Status: ci.StatusOperationFailed, Code: code, Message: message,
		Dispatch: dispatch, HTTPStatus: status,
	}
}

func (p *ConfidentialSDK) writeChatCompletionStream(ctx context.Context, stream *ci.Stream, first map[string]any, writer *io.PipeWriter) {
	defer closeSDKStream(stream)
	var responseID, model string
	var created any
	seenUsage := false
	writeEvent := func(value any) error {
		data, err := json.Marshal(value)
		if err != nil {
			return err
		}
		_, err = fmt.Fprintf(writer, "data: %s\n\n", data)
		return err
	}
	for event := first; event != nil; {
		switch event["type"] {
		case "chunk":
			chunk, ok := event["chunk"].(map[string]any)
			if !ok {
				_ = writer.CloseWithError(errors.New("confidential SDK stream chunk missing"))
				return
			}
			if id, ok := chunk["id"].(string); ok {
				responseID = id
			}
			if name, ok := chunk["model"].(string); ok {
				model = name
			}
			if value, ok := chunk["created"]; ok {
				created = value
			}
			seenUsage = seenUsage || chunk["usage"] != nil
			if err := writeEvent(chunk); err != nil {
				_ = writer.CloseWithError(err)
				return
			}
		case "usage":
			if !seenUsage {
				final := map[string]any{
					"object": "chat.completion.chunk", "choices": []any{}, "usage": event["usage"],
				}
				if responseID != "" {
					final["id"] = responseID
				}
				if model != "" {
					final["model"] = model
				}
				if created != nil {
					final["created"] = created
				}
				if err := writeEvent(final); err != nil {
					_ = writer.CloseWithError(err)
					return
				}
				seenUsage = true
			}
		case "done":
			if _, err := io.WriteString(writer, "data: [DONE]\n\n"); err != nil {
				_ = writer.CloseWithError(err)
			} else {
				_ = writer.Close()
			}
			return
		case "error":
			_ = writer.CloseWithError(sdkOperationError(sdkStreamEventError(event)))
			return
		case "cancelled", "closed":
			_ = writer.CloseWithError(errors.New("confidential SDK stream ended without terminal usage"))
			return
		}
		next, err := nextSDKStreamEvent(ctx, stream)
		if err != nil {
			_ = writer.CloseWithError(sdkOperationError(err))
			return
		}
		event = next
	}
}
func (p *ConfidentialSDK) DoResponsesRequest(ctx context.Context, method, path string, body []byte) ([]byte, error) {
	if method == "POST" && path == "/responses" {
		return p.execute(ctx, body, func(req any, timeout time.Duration) (map[string]any, error) {
			return p.client.CreateResponse(req, timeout)
		})
	}
	req, err := responseManageRequest(method, path, body, p.provider)
	if err != nil {
		return nil, err
	}
	timeout := time.Duration(0)
	if deadline, ok := ctx.Deadline(); ok {
		timeout = time.Until(deadline)
		if timeout <= 0 {
			return nil, ctx.Err()
		}
	}
	result, err := p.client.ResponseManage(req, timeout)
	if ctx.Err() != nil {
		return nil, ctx.Err()
	}
	if err != nil {
		return nil, sdkOperationError(err)
	}
	return responseManageResult(result)
}

func responseManageResult(result map[string]any) ([]byte, error) {
	verdict, ok := result["verdict"].(map[string]any)
	if !ok || verdict["status"] != "verified" {
		return nil, errors.New("confidential SDK verdict not verified after execution")
	}
	response, ok := result["response"]
	if !ok {
		return nil, errors.New("confidential SDK response missing after execution")
	}
	return json.Marshal(response)
}

func responseManageRequest(method, path string, body []byte, provider string) (map[string]any, error) {
	u, err := url.ParseRequestURI(path)
	if err != nil {
		return nil, errors.New("unsupported capability")
	}
	parts := strings.Split(strings.TrimPrefix(u.Path, "/responses"), "/")
	if u.Path != "/responses" && !strings.HasPrefix(u.Path, "/responses/") {
		return nil, errors.New("unsupported capability")
	}
	op, responseID := "", ""
	switch {
	case u.Path == "/responses" && method == "POST":
		return nil, errors.New("unsupported capability")
	case u.Path == "/responses/input_tokens" && method == "POST":
		op = "input_tokens"
	case u.Path == "/responses/compact" && method == "POST":
		op = "compact"
	case len(parts) == 2 && parts[0] == "" && parts[1] != "":
		responseID = parts[1]
		switch method {
		case "GET":
			op = "retrieve"
		case "DELETE":
			op = "delete"
		}
	case len(parts) == 3 && parts[0] == "" && parts[1] != "":
		responseID = parts[1]
		switch {
		case parts[2] == "input_items" && method == "GET":
			op = "list_input_items"
		case parts[2] == "cancel" && method == "POST":
			op = "cancel"
		}
	}
	if op == "" {
		return nil, errors.New("unsupported capability")
	}
	params := map[string]any{}
	switch op {
	case "retrieve":
		for _, key := range []string{"starting_after"} {
			if values, ok := u.Query()[key]; ok && len(values) == 1 {
				params[key] = values[0]
			}
		}
	case "list_input_items":
		for _, key := range []string{"limit", "order", "after", "before"} {
			if values, ok := u.Query()[key]; ok && len(values) == 1 {
				params[key] = values[0]
			}
		}
	}
	if len(body) != 0 {
		var bodyParams map[string]any
		if err := json.Unmarshal(body, &bodyParams); err != nil {
			return nil, err
		}
		for key, value := range bodyParams {
			params[key] = value
		}
	}
	return map[string]any{
		"op": op, "provider": provider, "provider_response_id": responseID, "params": params,
	}, nil
}

func (p *ConfidentialSDK) DoResponsesStream(context.Context, string, string, []byte) (io.ReadCloser, error) {
	return nil, errors.New("unsupported capability")
}
