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
