package llm

import (
	"context"
	"encoding/json"
	"os"
	"path/filepath"
	"testing"
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

func TestConfidentialSDKUnsupportedCapabilities(t *testing.T) {
	p := &ConfidentialSDK{}
	if !p.Supports("chat/completions") || p.Supports("embeddings") || p.Supports("responses") {
		t.Fatal("capability support declaration is incorrect")
	}
	if _, err := p.CreateEmbeddings(context.Background(), nil); err == nil {
		t.Fatal("embeddings unexpectedly supported")
	}
	if _, err := p.DoResponsesRequest(context.Background(), "POST", "/responses", nil); err == nil {
		t.Fatal("responses unexpectedly supported")
	}
	if _, err := p.CreateChatCompletionStream(context.Background(), nil); err == nil {
		t.Fatal("chat streaming unexpectedly supported")
	}
	if _, err := p.DoResponsesStream(context.Background(), "POST", "/responses", nil); err == nil {
		t.Fatal("responses streaming unexpectedly supported")
	}
}
