// attestation_snapshot_test.go — AttestationSnapshot classification and
// configuration-binding regressions. Seeded signatures are composed with the
// FULL production composite transportFor stamps (transportSignature +
// confidential_sdk config-file sha256 + credential sha256) — a bare
// transportSignature matches nothing in production and would mask the defect
// (the bare-seeded case pins exactly that: it must read UNOBSERVED).
package inference

import (
	"context"
	"crypto/sha256"
	"fmt"
	"io"
	"os"
	"path/filepath"
	"testing"
	"time"

	"enclava-go/internal/auth"
	"enclava-go/internal/llm"
)

// snapUpstream is a minimal Upstream that also reports attestation status.
type snapUpstream struct{ st llm.AttestationStatus }

func (snapUpstream) CreateChatCompletion(context.Context, []byte) ([]byte, error) { return nil, nil }
func (snapUpstream) CreateChatCompletionStream(context.Context, []byte) (io.ReadCloser, error) {
	return nil, nil
}
func (snapUpstream) CreateEmbeddings(context.Context, []byte) ([]byte, error) { return nil, nil }
func (snapUpstream) DoResponsesRequest(context.Context, string, string, []byte) ([]byte, error) {
	return nil, nil
}
func (snapUpstream) DoResponsesStream(context.Context, string, string, []byte) (io.ReadCloser, error) {
	return nil, nil
}
func (u snapUpstream) AttestationStatus() llm.AttestationStatus { return u.st }

// snapPlainUpstream implements Upstream with no attestation path.
type snapPlainUpstream struct{}

func (snapPlainUpstream) CreateChatCompletion(context.Context, []byte) ([]byte, error) {
	return nil, nil
}
func (snapPlainUpstream) CreateChatCompletionStream(context.Context, []byte) (io.ReadCloser, error) {
	return nil, nil
}
func (snapPlainUpstream) CreateEmbeddings(context.Context, []byte) ([]byte, error) { return nil, nil }
func (snapPlainUpstream) DoResponsesRequest(context.Context, string, string, []byte) ([]byte, error) {
	return nil, nil
}
func (snapPlainUpstream) DoResponsesStream(context.Context, string, string, []byte) (io.ReadCloser, error) {
	return nil, nil
}

// snapCompose builds the FULL composite signature transportFor stamps.
func snapCompose(t *testing.T, e Endpoint, credential string) string {
	t.Helper()
	sig := transportSignature(e)
	if e.Protocol == "confidential_sdk" {
		file, err := llm.ConfidentialSDKConfigFile(os.Getenv("CONFIDENTIAL_SDK_CONFIG"), e.SDKProvider)
		if err != nil {
			t.Fatalf("resolve SDK config file: %v", err)
		}
		if file != "" {
			b, err := os.ReadFile(file)
			if err != nil {
				t.Fatalf("read SDK config: %v", err)
			}
			sig += "|" + fmt.Sprintf("%x", sha256.Sum256(b))
		}
	}
	return sig + "|" + fmt.Sprintf("%x", sha256.Sum256([]byte(credential)))
}

func snapSeed(t *testing.T, s *Service, tenantID, endpointID string, e Endpoint, up Upstream, composedSig string) {
	t.Helper()
	s.transports[tenantID+"\x00"+endpointID] = transportEntry{signature: composedSig, up: up}
}

func snapStates(observations []AttestationObservation) map[string]AttestationObservation {
	out := make(map[string]AttestationObservation, len(observations))
	for _, o := range observations {
		out[o.EndpointID] = o
	}
	return out
}

func TestAttestationSnapshotClassification(t *testing.T) {
	verifiedAt := time.Now().UTC().Add(-2 * time.Minute)
	failedAt := time.Now().UTC().Add(-5 * time.Minute)

	verifiedRow := Endpoint{ID: "ep-verified", TenantID: "tenantA", Protocol: "tinfoil", BaseOrigin: "https://a.example"}
	failedRow := Endpoint{ID: "ep-failed", TenantID: "tenantA", Protocol: "tinfoil", BaseOrigin: "https://b.example"}
	staticRow := Endpoint{ID: "ep-static", TenantID: "tenantA", Protocol: "confidential_sdk", SDKProvider: "wasm", BaseOrigin: "https://c.example"}
	plainRow := Endpoint{ID: "ep-plain", TenantID: "tenantA", Protocol: "tinfoil", BaseOrigin: "https://d.example"}
	bareRow := Endpoint{ID: "ep-bare", TenantID: "tenantA", Protocol: "tinfoil", BaseOrigin: "https://e.example"}

	s := &Service{transports: map[string]transportEntry{}}
	snapSeed(t, s, "tenantA", "ep-verified", verifiedRow,
		snapUpstream{st: llm.AttestationStatus{Enabled: true, Verified: true, VerifiedAt: verifiedAt}},
		snapCompose(t, verifiedRow, ""))
	snapSeed(t, s, "tenantA", "ep-failed", failedRow,
		snapUpstream{st: llm.AttestationStatus{Enabled: true, Verified: false, VerifiedAt: failedAt, LastError: "attest boom"}},
		snapCompose(t, failedRow, ""))
	// The real ConfidentialSDK.AttestationStatus shape: no evidence of an
	// attempted verification — must emit NOTHING (unobserved, never failed).
	snapSeed(t, s, "tenantA", "ep-static", staticRow,
		snapUpstream{st: llm.AttestationStatus{Enabled: true, Provider: "wasm"}},
		snapCompose(t, staticRow, ""))
	snapSeed(t, s, "tenantA", "ep-plain", plainRow, snapPlainUpstream{}, snapCompose(t, plainRow, ""))
	// Regression pin: production never stores a bare transportSignature, so a
	// bare-seeded entry matches nothing and must read UNOBSERVED.
	snapSeed(t, s, "tenantA", "ep-bare", bareRow, snapUpstream{st: llm.AttestationStatus{Verified: true, VerifiedAt: verifiedAt}}, transportSignature(bareRow))

	got := snapStates(s.AttestationSnapshot("tenantA", []Endpoint{verifiedRow, failedRow, staticRow, plainRow, bareRow}))
	if len(got) != 2 {
		t.Fatalf("expected exactly 2 observations (verified+failed), got %d: %+v", len(got), got)
	}
	if o := got["ep-verified"]; o.State != AttestationVerified || o.LastError != "" || o.VerifiedAt == nil || !o.VerifiedAt.Equal(verifiedAt) {
		t.Errorf("ep-verified: got %+v, want verified with stamp %v", o, verifiedAt)
	}
	if o := got["ep-failed"]; o.State != AttestationFailed || o.LastError != "attest boom" || o.VerifiedAt == nil || !o.VerifiedAt.Equal(failedAt) {
		t.Errorf("ep-failed: got %+v, want failed with evidence", o)
	}
	for _, id := range []string{"ep-static", "ep-plain", "ep-bare"} {
		if o, ok := got[id]; ok {
			t.Errorf("%s: emitted %+v, want absent (unobserved)", id, o)
		}
	}
}

func TestAttestationSnapshotStaleConfigUnobserved(t *testing.T) {
	dir := t.TempDir()
	cfg := filepath.Join(dir, "sdk.json")
	if err := os.WriteFile(cfg, []byte(`{"v":1}`), 0o600); err != nil {
		t.Fatal(err)
	}
	t.Setenv("CONFIDENTIAL_SDK_CONFIG", cfg)

	row := Endpoint{ID: "ep-sdk", TenantID: "tenantA", Protocol: "confidential_sdk", SDKProvider: "wasm", BaseOrigin: "https://sdk.example"}
	s := &Service{transports: map[string]transportEntry{}}
	// Stamp the transport under config v1…
	snapSeed(t, s, "tenantA", "ep-sdk", row,
		snapUpstream{st: llm.AttestationStatus{Enabled: true, Provider: "wasm"}},
		snapCompose(t, row, ""))

	// The operator config changes after verification: the row's transport is
	// stale — never proof.
	if err := os.WriteFile(cfg, []byte(`{"v":2}`), 0o600); err != nil {
		t.Fatal(err)
	}
	// A control stamped under the CURRENT config with real evidence proves
	// signature-matched entries still reach the classifier.
	ctrl := Endpoint{ID: "ep-sdk-ctrl", TenantID: "tenantA", Protocol: "confidential_sdk", SDKProvider: "wasm", BaseOrigin: "https://sdk2.example"}
	snapSeed(t, s, "tenantA", "ep-sdk-ctrl", ctrl,
		snapUpstream{st: llm.AttestationStatus{Enabled: true, Verified: true, VerifiedAt: time.Now().UTC()}},
		snapCompose(t, ctrl, ""))
	got := snapStates(s.AttestationSnapshot("tenantA", []Endpoint{row, ctrl}))
	if o, ok := got["ep-sdk"]; ok {
		t.Errorf("ep-sdk: emitted %+v after config edit, want absent (unobserved)", o)
	}
	if o := got["ep-sdk-ctrl"]; o.State != AttestationVerified {
		t.Errorf("ep-sdk-ctrl: got %+v, want verified (same-config control)", o)
	}
}

func TestAttestationSnapshotRotatedCredentialUnobserved(t *testing.T) {
	t.Setenv("TEST_ATTEST_CRED", "v1")
	resolver, err := auth.NewSecretResolver([]auth.SecretBinding{{
		RefID: "cred-1", TenantID: "tenantA", Purpose: auth.SecretPurposeInference,
		EnvName: "TEST_ATTEST_CRED", Origins: []string{"https://rot.example"},
	}})
	if err != nil {
		t.Fatal(err)
	}
	row := Endpoint{ID: "ep-rot", TenantID: "tenantA", Protocol: "tinfoil", BaseOrigin: "https://rot.example", CredentialRef: "cred-1"}
	s := &Service{secrets: resolver, transports: map[string]transportEntry{}}
	// Stamped while the binding resolved "v1"…
	snapSeed(t, s, "tenantA", "ep-rot", row,
		snapUpstream{st: llm.AttestationStatus{Enabled: true, Verified: true, VerifiedAt: time.Now().UTC()}},
		snapCompose(t, row, "v1"))

	// …then the credential rotates: the stamped transport is never proof.
	t.Setenv("TEST_ATTEST_CRED", "v2")
	got := snapStates(s.AttestationSnapshot("tenantA", []Endpoint{row}))
	if o, ok := got["ep-rot"]; ok {
		t.Errorf("ep-rot: emitted %+v after credential rotation, want absent (unobserved)", o)
	}

	// A transport stamped under the ROTATED value genuinely matches again.
	row2 := Endpoint{ID: "ep-rot2", TenantID: "tenantA", Protocol: "tinfoil", BaseOrigin: "https://rot.example", CredentialRef: "cred-1"}
	snapSeed(t, s, "tenantA", "ep-rot2", row2,
		snapUpstream{st: llm.AttestationStatus{Enabled: true, Verified: true, VerifiedAt: time.Now().UTC()}},
		snapCompose(t, row2, "v2"))
	got = snapStates(s.AttestationSnapshot("tenantA", []Endpoint{row, row2}))
	if o := got["ep-rot2"]; o.State != AttestationVerified {
		t.Errorf("ep-rot2: got %+v, want verified after re-stamp on rotated credential", o)
	}
}

func TestAttestationSnapshotTenantIsolation(t *testing.T) {
	rowA := Endpoint{ID: "shared", TenantID: "tenantA", Protocol: "tinfoil", BaseOrigin: "https://iso.example"}
	rowB := Endpoint{ID: "shared", TenantID: "tenantB", Protocol: "tinfoil", BaseOrigin: "https://iso.example"}
	s := &Service{transports: map[string]transportEntry{}}
	stamp := time.Now().UTC().Add(-time.Minute)
	snapSeed(t, s, "tenantA", "shared", rowA,
		snapUpstream{st: llm.AttestationStatus{Verified: true, VerifiedAt: stamp}}, snapCompose(t, rowA, ""))
	snapSeed(t, s, "tenantB", "shared", rowB,
		snapUpstream{st: llm.AttestationStatus{Verified: false, LastError: "tenantB failure", VerifiedAt: stamp}}, snapCompose(t, rowB, ""))

	gotA := snapStates(s.AttestationSnapshot("tenantA", []Endpoint{rowA}))
	if len(gotA) != 1 {
		t.Fatalf("tenantA: got %+v, want exactly its own observation", gotA)
	}
	if o := gotA["shared"]; o.State != AttestationVerified {
		t.Errorf("tenantA shared: got %+v, want verified — tenantB's failed state must never leak", o)
	}
	gotB := snapStates(s.AttestationSnapshot("tenantB", []Endpoint{rowB}))
	if o := gotB["shared"]; o.State != AttestationFailed || o.LastError != "tenantB failure" {
		t.Errorf("tenantB shared: got %+v, want failed with its own evidence", o)
	}
}
