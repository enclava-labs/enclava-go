package llm

import (
	"context"
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
