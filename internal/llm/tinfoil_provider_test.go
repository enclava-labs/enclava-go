package llm

import (
	"context"
	"errors"
	"testing"
	"time"

	"github.com/openai/openai-go/v3/option"
	tinfoil "github.com/tinfoilsh/tinfoil-go"
)

func TestNewTinfoilProviderDefersExplicitClientWhenVerifyAtStartDisabled(t *testing.T) {
	original := newTinfoilClientWithParams
	t.Cleanup(func() {
		newTinfoilClientWithParams = original
	})

	called := false
	newTinfoilClientWithParams = func(_ string, _ string, _ ...option.RequestOption) (*tinfoil.Client, error) {
		called = true
		return nil, errors.New("client construction should be deferred")
	}

	provider, err := NewTinfoilProvider(
		"test-api-key",
		"example.invalid",
		"tinfoilsh/confidential-model-router",
		time.Second,
		false,
		false,
		time.Minute,
	)
	if err != nil {
		t.Fatalf("NewTinfoilProvider() error = %v", err)
	}
	if called {
		t.Fatal("NewTinfoilProvider() constructed the Tinfoil client before verification was requested")
	}

	status := provider.AttestationStatus()
	if status.Verified {
		t.Fatal("AttestationStatus().Verified = true before verification")
	}
	if status.Enclave != "example.invalid" {
		t.Fatalf("AttestationStatus().Enclave = %q, want example.invalid", status.Enclave)
	}
	if status.Repo != "tinfoilsh/confidential-model-router" {
		t.Fatalf("AttestationStatus().Repo = %q, want tinfoilsh/confidential-model-router", status.Repo)
	}
}

func TestNewTinfoilProviderDefersDefaultClientWhenVerifyAtStartDisabled(t *testing.T) {
	original := newTinfoilClient
	t.Cleanup(func() {
		newTinfoilClient = original
	})

	called := false
	newTinfoilClient = func(_ ...option.RequestOption) (*tinfoil.Client, error) {
		called = true
		return nil, errors.New("default client construction should be deferred")
	}

	provider, err := NewTinfoilProvider(
		"test-api-key",
		"",
		"",
		time.Second,
		false,
		false,
		time.Minute,
	)
	if err != nil {
		t.Fatalf("NewTinfoilProvider() error = %v", err)
	}
	if called {
		t.Fatal("NewTinfoilProvider() constructed the default Tinfoil client before verification was requested")
	}
	if provider.AttestationStatus().Verified {
		t.Fatal("AttestationStatus().Verified = true before verification")
	}
}

func TestTinfoilVerificationFailureDeniesDispatch(t *testing.T) {
	original := newTinfoilClient
	t.Cleanup(func() { newTinfoilClient = original })
	newTinfoilClient = func(_ ...option.RequestOption) (*tinfoil.Client, error) {
		return nil, errors.New("attestation unavailable")
	}

	provider, err := NewTinfoilProvider("test-api-key", "", "", time.Second, false, false, time.Minute)
	if err != nil {
		t.Fatalf("NewTinfoilProvider() error = %v", err)
	}
	if _, err := provider.CreateChatCompletion(context.Background(), []byte(`{"model":"m"}`)); err == nil {
		t.Fatal("expected dispatch denied when verification fails")
	}
	if _, err := provider.ListModels(context.Background()); err == nil {
		t.Fatal("expected ListModels denied when verification fails")
	}
	if _, err := provider.CreateChatCompletionStream(context.Background(), []byte(`{}`)); err == nil {
		t.Fatal("expected stream denied when verification fails")
	}
	status := provider.AttestationStatus()
	if status.Verified {
		t.Fatal("failed verification must not mark endpoint verified")
	}
	if provider.baseURL != "" {
		t.Fatalf("failed verification must not yield an ordinary HTTP target: %q", provider.baseURL)
	}
}

func TestTinfoilCancelledWaiterDoesNotPoisonStatus(t *testing.T) {
	original := newTinfoilClient
	t.Cleanup(func() { newTinfoilClient = original })
	release := make(chan struct{})
	newTinfoilClient = func(_ ...option.RequestOption) (*tinfoil.Client, error) {
		<-release
		return nil, errors.New("still unavailable")
	}

	provider, err := NewTinfoilProvider("test-api-key", "", "", time.Second, false, false, time.Minute)
	if err != nil {
		t.Fatalf("NewTinfoilProvider() error = %v", err)
	}
	provider.verifyTimeout = 2 * time.Second

	ctx, cancel := context.WithTimeout(context.Background(), 50*time.Millisecond)
	defer cancel()
	if _, err := provider.VerifyAttestation(ctx); !errors.Is(err, context.DeadlineExceeded) {
		t.Fatalf("expected caller ctx error, got %v", err)
	}
	// Let the shared (bounded) verification finish before cleanup restores the stub.
	close(release)
	_, _ = provider.VerifyAttestation(context.Background())
	// Cancelled waiter: last-known status, not a fabricated failure or success.
	status := provider.AttestationStatus()
	if status.Verified {
		t.Fatal("cancelled verification must not mark verified")
	}
}

func TestTinfoilProvidersHaveIndependentState(t *testing.T) {
	original := newTinfoilClientWithParams
	t.Cleanup(func() { newTinfoilClientWithParams = original })
	fail := true
	newTinfoilClientWithParams = func(enclave, repo string, _ ...option.RequestOption) (*tinfoil.Client, error) {
		if fail && enclave == "bad.invalid" {
			return nil, errors.New("verification refused")
		}
		return nil, errors.New("network unavailable")
	}

	bad, err := NewTinfoilProvider("k", "bad.invalid", "r", time.Second, false, false, time.Minute)
	if err != nil {
		t.Fatalf("bad provider: %v", err)
	}
	other, err := NewTinfoilProvider("k", "other.invalid", "r", time.Second, false, false, time.Minute)
	if err != nil {
		t.Fatalf("other provider: %v", err)
	}

	if _, err := bad.VerifyAttestation(context.Background()); err == nil {
		t.Fatal("expected verification failure")
	}
	if bad.AttestationStatus().Verified {
		t.Fatal("bad endpoint marked verified")
	}
	if other.AttestationStatus().LastError != "" || other.AttestationStatus().Verified {
		t.Fatal("one endpoint's verification state leaked into another instance")
	}
}
