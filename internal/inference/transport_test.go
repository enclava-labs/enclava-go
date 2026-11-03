package inference

import (
	"context"
	"testing"
	"time"

	"enclava-go/internal/llm"
)

func TestTinfoilTransportConstruction(t *testing.T) {
	s := &Service{attemptTimeout: time.Second}
	for _, tc := range []struct {
		origin, repo, enclave string
	}{
		{"https://inference.tinfoil.sh", "", ""},
		{"https://enclave.example.com", "owner/repo", "enclave.example.com"},
	} {
		up, err := s.defaultTransport(context.Background(), Endpoint{Protocol: "tinfoil", BaseOrigin: tc.origin, EnclaveRepo: tc.repo}, "key")
		if err != nil {
			t.Fatal(err)
		}
		status := up.(*llm.TinfoilProvider).AttestationStatus()
		if status.Enclave != tc.enclave || status.Repo != tc.repo {
			t.Fatalf("unexpected configuration: %+v", status)
		}
	}
	if _, err := s.defaultTransport(context.Background(), Endpoint{Protocol: "tinfoil", BaseOrigin: "https://enclave.example.com"}, "key"); err == nil {
		t.Fatal("dedicated enclave without repo must fail")
	}
}
