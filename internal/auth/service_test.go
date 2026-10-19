package auth

import (
	"context"
	"encoding/json"
	"net/http/httptest"
	"testing"
)

type fakeRepo struct{}

func (f fakeRepo) FindCandidatesByPrefix(ctx context.Context, prefix string) ([]APIKeyRecord, error) {
	_, hash, usedAlgo, err := HashForStorage("enc_test_key_123456", HashAlgoSHA256, "")
	if err != nil {
		return nil, err
	}
	return []APIKeyRecord{{
		ID:            1,
		Name:          "test",
		KeyHash:       hash,
		KeyHashAlgo:   usedAlgo,
		KeyPrefix:     "enc_test_",
		IsActive:      true,
		Scopes:        []string{"chat.completions"},
		AllowedAgents: []string{"agent_allowed"},
	}}, nil
}

func (f fakeRepo) TouchUsage(ctx context.Context, id int64) error { return nil }

func TestParseAPIKey(t *testing.T) {
	r := httptest.NewRequest("GET", "/", nil)
	r.Header.Set("Authorization", "Bearer abc123")
	if got := ParseAPIKey(r); got != "abc123" {
		t.Fatalf("expected abc123, got %s", got)
	}
}

func TestAuthenticate(t *testing.T) {
	svc := NewService(fakeRepo{}, "")
	principal, err := svc.Authenticate(context.Background(), "enc_test_key_123456")
	if err != nil {
		t.Fatalf("unexpected error: %v", err)
	}
	if !HasScope(principal, "chat.completions") {
		t.Fatalf("expected scope")
	}
	if !AgentAllowed(principal, "agent_allowed") {
		t.Fatalf("expected allowed agent")
	}
	if AgentAllowed(principal, "agent_blocked") {
		t.Fatalf("expected blocked agent")
	}
}

func TestPrincipalAllowlists(t *testing.T) {
	set := func(items ...string) map[string]struct{} {
		m := make(map[string]struct{}, len(items))
		for _, i := range items {
			m[i] = struct{}{}
		}
		return m
	}

	// Explicit all-mode grant allows any value.
	allPrincipal := Principal{
		AllowedModelsAll:           true,
		AllowedExtractTemplatesAll: true,
		AllowedChatbotsAll:         true,
		AllowedAgentsAll:           true,
		AllowedEndpointsAll:        true,
		AllowedIPsAll:              true,
	}
	if !ModelAllowed(allPrincipal, "any-model") ||
		!ExtractTemplateAllowed(allPrincipal, "any-template") ||
		!ChatbotAllowed(allPrincipal, "any-bot") ||
		!AgentAllowed(allPrincipal, "any-agent") ||
		!EndpointAllowed(allPrincipal, "any-endpoint") ||
		!IPAllowed(allPrincipal, "203.0.113.7") {
		t.Fatalf("expected all-mode grant to allow every resource")
	}

	// List-mode grant allows listed values and denies unlisted ones.
	listPrincipal := Principal{
		AllowedModels:           set("model-a"),
		AllowedExtractTemplates: set("tpl-a"),
		AllowedChatbots:         set("bot-a"),
		AllowedAgents:           set("agent-a"),
		AllowedEndpoints:        set("ep-a"),
		AllowedIPs:              set("198.51.100.1"),
	}
	if !ModelAllowed(listPrincipal, "model-a") ||
		!ExtractTemplateAllowed(listPrincipal, "tpl-a") ||
		!ChatbotAllowed(listPrincipal, "bot-a") ||
		!AgentAllowed(listPrincipal, "agent-a") ||
		!EndpointAllowed(listPrincipal, "ep-a") ||
		!IPAllowed(listPrincipal, "198.51.100.1") {
		t.Fatalf("expected listed values to be allowed")
	}
	if ModelAllowed(listPrincipal, "model-b") ||
		ExtractTemplateAllowed(listPrincipal, "tpl-b") ||
		ChatbotAllowed(listPrincipal, "bot-b") ||
		AgentAllowed(listPrincipal, "agent-b") ||
		EndpointAllowed(listPrincipal, "ep-b") ||
		IPAllowed(listPrincipal, "198.51.100.2") {
		t.Fatalf("expected unlisted values to be denied")
	}

	// Empty list (All false + empty map) denies everything — an empty map
	// alone never means unrestricted.
	deny := Principal{}
	if ModelAllowed(deny, "model-a") ||
		ExtractTemplateAllowed(deny, "tpl-a") ||
		ChatbotAllowed(deny, "bot-a") ||
		AgentAllowed(deny, "agent-a") ||
		EndpointAllowed(deny, "ep-a") ||
		IPAllowed(deny, "198.51.100.1") {
		t.Fatalf("expected empty-list grant to deny every resource")
	}

	// Decode contract: {"mode":"all"} grants, {"mode":"list"} lists,
	// absent/malformed input denies.
	if items, all := DecodeAllowlist(json.RawMessage(`{"mode":"all"}`)); !all || len(items) != 0 {
		t.Fatalf("mode:all must set all=true with empty items, got all=%v items=%v", all, items)
	}
	if items, all := DecodeAllowlist(json.RawMessage(`{"mode":"list","items":["a","b"]}`)); all || len(items) != 2 {
		t.Fatalf("mode:list must populate items with all=false, got all=%v items=%v", all, items)
	}
	if items, all := DecodeAllowlist(nil); all || len(items) != 0 {
		t.Fatalf("absent grant must deny, got all=%v items=%v", all, items)
	}
	if items, all := DecodeAllowlist(json.RawMessage(`{not json`)); all || len(items) != 0 {
		t.Fatalf("malformed grant must deny, got all=%v items=%v", all, items)
	}
}
