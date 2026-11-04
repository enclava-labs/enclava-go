package httpx

import (
	"strings"
	"testing"
	"time"

	"enclava-go/internal/inference"
)

func TestEndpointUpdateInvalidatesAssuranceOnConfigChanges(t *testing.T) {
	verifiedAt := time.Now().UTC()
	base := inference.Endpoint{
		ID: "endpoint-a", TenantID: "tenant-a", Protocol: "openai_compatible",
		Capabilities: []string{"chat"}, BaseOrigin: "https://provider.example",
		CredentialRef: "provider-key", AssuranceMode: "confidential",
		AssuranceVerifiedAt: &verifiedAt, Enabled: true,
	}
	newOrigin := "https://other.example"
	updated, err := endpointFromUpdateRequest(base, endpointRequest{BaseOrigin: newOrigin})
	if err != nil {
		t.Fatal(err)
	}
	if updated.AssuranceVerifiedAt != nil {
		t.Fatal("origin change retained stale assurance")
	}
	if updated.CredentialRef != base.CredentialRef {
		t.Fatal("origin update unexpectedly changed credential reference")
	}

	updated, err = endpointFromUpdateRequest(base, endpointRequest{CredentialRef: new("")})
	if err != nil {
		t.Fatal(err)
	}
	if updated.CredentialRef != "" || updated.AssuranceVerifiedAt != nil {
		t.Fatal("explicit credential clearing did not clear reference and assurance")
	}

	updated, err = endpointFromUpdateRequest(base, endpointRequest{})
	if err != nil {
		t.Fatal(err)
	}
	if updated.AssuranceVerifiedAt == nil {
		t.Fatal("non-configuration update invalidated assurance")
	}
}

func TestConfidentialSDKEndpointRequiresSDKProvider(t *testing.T) {
	_, err := endpointFromCreateRequest("tenant-a", endpointRequest{
		ID: "endpoint-a", Protocol: "confidential_sdk", BaseOrigin: "https://provider.example",
	})
	if err == nil || !strings.Contains(err.Error(), "sdk_provider is required") {
		t.Fatalf("expected missing sdk_provider validation error, got %v", err)
	}

	endpoint, err := endpointFromCreateRequest("tenant-a", endpointRequest{
		ID: "endpoint-a", Protocol: "confidential_sdk", SDKProvider: "provider-a",
		BaseOrigin: "https://provider.example",
	})
	if err != nil || endpoint.SDKProvider != "provider-a" {
		t.Fatalf("valid confidential SDK endpoint rejected: endpoint=%+v err=%v", endpoint, err)
	}
}
func TestEndpointCreateRejectsCallerClaimedVerification(t *testing.T) {
	verified := true
	_, err := endpointFromCreateRequest("tenant-a", endpointRequest{
		ID: "endpoint-a", Protocol: "openai_compatible", BaseOrigin: "https://provider.example",
		AssuranceVerified: &verified,
	})
	if err == nil {
		t.Fatal("caller-set verification must be rejected")
	}
}

func TestEndpointRequestDefaultsValidateAndPersistAcrossUpdates(t *testing.T) {
	defaults := map[string]any{"temperature": 0.5, "chat_template_kwargs": map[string]any{"enable_thinking": false}}
	endpoint, err := endpointFromCreateRequest("tenant-a", endpointRequest{
		ID: "local", Protocol: "openai_compatible", BaseOrigin: "http://spark01:8888", AuthorizedPrivate: new(true),
		RequestDefaults: &defaults,
	})
	if err != nil || endpoint.RequestDefaults["temperature"] != 0.5 {
		t.Fatalf("create with defaults: endpoint=%+v err=%v", endpoint, err)
	}
	if view := endpointView(endpoint); view.Config["request_defaults"] == nil {
		t.Fatalf("view hides request defaults: %+v", view.Config)
	}
	reserved := map[string]any{"max_tokens": 10}
	if _, err := endpointFromCreateRequest("tenant-a", endpointRequest{
		ID: "local", Protocol: "openai_compatible", BaseOrigin: "http://spark01:8888", AuthorizedPrivate: new(true),
		RequestDefaults: &reserved,
	}); err == nil || !strings.Contains(err.Error(), `may not set "max_tokens"`) {
		t.Fatalf("billed output bound accepted as a default: %v", err)
	}
	kept, err := endpointFromUpdateRequest(endpoint, endpointRequest{Capabilities: []string{"chat/completions"}})
	if err != nil || kept.RequestDefaults["temperature"] != 0.5 {
		t.Fatalf("update without request_defaults dropped them: %+v %v", kept.RequestDefaults, err)
	}
	cleared, err := endpointFromUpdateRequest(endpoint, endpointRequest{RequestDefaults: &map[string]any{}})
	if err != nil || len(cleared.RequestDefaults) != 0 {
		t.Fatalf("empty request_defaults did not clear: %+v %v", cleared.RequestDefaults, err)
	}
}
