package llm

import (
	"context"
	"encoding/json"
	"errors"
	"io"
	"os"
	"path/filepath"
	"testing"

	ci "github.com/enclava-labs/confidential-inference-sdk/bindings/go"
)

func TestConfidentialSDKInvalidConfigFailsConstruction(t *testing.T) {
	path := filepath.Join(t.TempDir(), "sdk.json")
	if err := os.WriteFile(path, []byte("{"), 0o600); err != nil {
		t.Fatal(err)
	}
	if _, err := NewConfidentialSDK(path, "provider", "https://provider.example", ""); err == nil {
		t.Fatal("invalid SDK config was accepted")
	}
}

func TestConfidentialSDKConfigUsesOnlyTenantResolvedCredential(t *testing.T) {
	path := filepath.Join(t.TempDir(), "sdk.json")
	shared := `{"demo_provider":false,"allow_inline_api_keys":true,"api_keys":{"chutes":{"env":"CHUTES_API_KEY"},"other":{"inline":"leak"}}}`
	if err := os.WriteFile(path, []byte(shared), 0o600); err != nil {
		t.Fatal(err)
	}

	noCredential, err := sdkClientConfig(path, "chutes", "")
	if err != nil {
		t.Fatal(err)
	}
	var cfg map[string]any
	if err := json.Unmarshal(noCredential, &cfg); err != nil {
		t.Fatal(err)
	}
	if _, ok := cfg["api_keys"]; ok {
		t.Fatalf("endpoint without credential_ref inherited shared api_keys: %s", noCredential)
	}
	if _, ok := cfg["allow_inline_api_keys"]; ok {
		t.Fatalf("inline keys left enabled without a tenant credential: %s", noCredential)
	}

	withCredential, err := sdkClientConfig(path, "chutes", "tenant-key")
	if err != nil {
		t.Fatal(err)
	}
	cfg = nil
	if err := json.Unmarshal(withCredential, &cfg); err != nil {
		t.Fatal(err)
	}
	keys := cfg["api_keys"].(map[string]any)
	if len(keys) != 1 || keys["chutes"].(map[string]any)["inline"] != "tenant-key" {
		t.Fatalf("api_keys must hold only the tenant-resolved credential: %s", withCredential)
	}
	if cfg["demo_provider"] != false {
		t.Fatalf("operator trust settings were not preserved: %s", withCredential)
	}
}

// Only a definitive provider rejection may settle as an upstream response;
// anything that might have executed must stay a plain error so the executor
// retains liability instead of settling it as free.
func TestSDKOperationErrorAccountingMapping(t *testing.T) {
	for _, tc := range []struct {
		err        *ci.Error
		wantStatus int
	}{
		{&ci.Error{Status: ci.StatusOperationFailed, Code: "provider_chat_failed", Dispatch: ci.DispatchRejected, HTTPStatus: 429}, 429},
		{&ci.Error{Status: ci.StatusOperationFailed, Code: "provider_chat_failed", Dispatch: ci.DispatchUnknown}, 0},
		{&ci.Error{Status: ci.StatusOperationFailed, Code: "policy_denied", Dispatch: ci.DispatchNotDispatched}, 0},
		{&ci.Error{Status: ci.StatusPending, Code: "operation_timeout", Dispatch: ci.DispatchUnknown}, 0},
	} {
		var upstream *UpstreamError
		got := sdkOperationError(tc.err)
		if isUpstream := errors.As(got, &upstream); isUpstream != (tc.wantStatus > 0) {
			t.Fatalf("%s/%s: upstream=%v, want %v", tc.err.Code, tc.err.Dispatch, isUpstream, tc.wantStatus > 0)
		}
		if tc.wantStatus > 0 && upstream.StatusCode != tc.wantStatus {
			t.Fatalf("status = %d, want %d", upstream.StatusCode, tc.wantStatus)
		}
	}
}

func TestSDKStreamErrorPreservesAccountingDisposition(t *testing.T) {
	err := sdkOperationError(sdkStreamEventError(map[string]any{
		"type": "error",
		"error": map[string]any{
			"code": "provider_rejected", "message": "rate limited",
			"dispatch": ci.DispatchRejected, "http_status": float64(429),
		},
	}))
	var upstream *UpstreamError
	if !errors.As(err, &upstream) || upstream.StatusCode != 429 || upstream.Message != "rate limited" {
		t.Fatalf("stream provider rejection not preserved: %v", err)
	}

	err = sdkOperationError(sdkStreamEventError(map[string]any{
		"type": "error",
		"error": map[string]any{
			"code": "stream_failed", "message": "outcome unknown",
			"dispatch": ci.DispatchUnknown,
		},
	}))
	if errors.As(err, &upstream) {
		t.Fatalf("ambiguous stream outcome must remain unsettled: %v", err)
	}
}

func TestConfidentialSDKUnsupportedCapabilities(t *testing.T) {
	p := &ConfidentialSDK{}
	if !p.Supports("chat/completions") || !p.Supports("embeddings") ||
		!p.Supports("responses") || !p.Supports("responses/compact") {
		t.Fatal("capability support declaration is incorrect")
	}
	// Embeddings now go through the SDK; a nil client only fails at call time.
	if _, err := p.CreateEmbeddings(context.Background(), nil); err == nil {
		t.Fatal("embeddings on a nil-client transport must fail")
	}
	if _, err := p.DoResponsesRequest(context.Background(), "POST", "/responses/invalid/operation", nil); err == nil {
		t.Fatal("unknown Responses management operation unexpectedly supported")
	}
	if _, err := p.CreateChatCompletionStream(context.Background(), nil); err == nil {
		t.Fatal("chat streaming unexpectedly supported")
	}
	if _, err := p.DoResponsesStream(context.Background(), "POST", "/responses", nil); err == nil {
		t.Fatal("responses streaming unexpectedly supported")
	}
}
func TestConfidentialSDKResponseManageRequestRouting(t *testing.T) {
	for _, tc := range []struct {
		method, path, body, op, responseID string
	}{
		{"GET", "/responses/resp_provider", "", "retrieve", "resp_provider"},
		{"GET", "/responses/resp_provider/input_items?after=item_provider&limit=10&ignored=leak", "", "list_input_items", "resp_provider"},
		{"POST", "/responses/resp_provider/cancel", "", "cancel", "resp_provider"},
		{"DELETE", "/responses/resp_provider", "", "delete", "resp_provider"},
		{"POST", "/responses/input_tokens", `{"input":"text"}`, "input_tokens", ""},
		{"POST", "/responses/compact", `{"model":"m"}`, "compact", ""},
	} {
		got, err := responseManageRequest(tc.method, tc.path, []byte(tc.body), "provider-a")
		if err != nil {
			t.Fatalf("%s %s: %v", tc.method, tc.path, err)
		}
		if got["op"] != tc.op || got["provider"] != "provider-a" || got["provider_response_id"] != tc.responseID {
			t.Fatalf("%s %s: request identity = %#v", tc.method, tc.path, got)
		}
		params, ok := got["params"].(map[string]any)
		if !ok {
			t.Fatalf("%s %s: params = %#v", tc.method, tc.path, got["params"])
		}
		if tc.op == "list_input_items" {
			if params["after"] != "item_provider" || params["limit"] != "10" {
				t.Fatalf("list params = %#v", params)
			}
			if _, ok := params["ignored"]; ok {
				t.Fatalf("unrecognized query reached SDK: %#v", params)
			}
		}
		if tc.op == "input_tokens" && params["input"] != "text" {
			t.Fatalf("body params = %#v", params)
		}
	}
}

func TestConfidentialSDKResponseManageRejectsCreateAndUnknownRoutes(t *testing.T) {
	for _, tc := range []struct{ method, path string }{
		{"POST", "/responses"},
		{"GET", "/responses"},
		{"GET", "/responses/resp/id"},
		{"POST", "/responses/resp_provider/delete"},
	} {
		if _, err := responseManageRequest(tc.method, tc.path, nil, "provider-a"); err == nil {
			t.Fatalf("%s %s unexpectedly supported", tc.method, tc.path)
		}
	}
}

func TestConfidentialSDKResponseManageRequiresVerifiedVerdict(t *testing.T) {
	if _, err := responseManageResult(map[string]any{
		"verdict":  map[string]any{"status": "demo"},
		"response": map[string]any{"id": "resp_demo"},
	}); err == nil {
		t.Fatal("demo/unverified response was accepted")
	}
	raw, err := responseManageResult(map[string]any{
		"verdict":  map[string]any{"status": "verified"},
		"response": map[string]any{"id": "resp_provider"},
	})
	if err != nil || string(raw) != `{"id":"resp_provider"}` {
		t.Fatalf("verified response = %s, error %v", raw, err)
	}
}

func TestCancellableStreamReaderCloseCancelsContext(t *testing.T) {
	ctx, cancel := context.WithCancel(context.Background())
	reader, writer := io.Pipe()
	defer writer.Close()
	stream := &cancellableStreamReader{PipeReader: reader, cancel: cancel}
	if err := stream.Close(); err != nil {
		t.Fatal(err)
	}
	if !errors.Is(ctx.Err(), context.Canceled) {
		t.Fatalf("context error = %v, want canceled", ctx.Err())
	}
}
