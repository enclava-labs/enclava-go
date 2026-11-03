// attestation_snapshot.go — read-only, configuration-bound projection of the
// cached transports' observed attestation state (B13/OA-10). The snapshot
// never probes, dispatches, verifies or writes: it recomputes the stored
// composite transport signature (the same local credential/config reads
// transportFor performs per dispatch) and reports what the cached transport
// actually observed. Classification is tri-state on real evidence only —
// an evidence-less static status (confidential_sdk is never runtime-verified)
// is unobserved, never failed.
package inference

import (
	"crypto/sha256"
	"fmt"
	"os"
	"strings"
	"time"

	"enclava-go/internal/llm"
)

// AttestationState is the observed assurance classification for one endpoint.
type AttestationState string

// String lets templates pass the state to plain-string helpers (statusPill).
func (s AttestationState) String() string { return string(s) }

const (
	AttestationVerified   AttestationState = "verified"
	AttestationFailed     AttestationState = "failed"
	AttestationUnobserved AttestationState = "unobserved"
)

// AttestationObservation is one endpoint's current observed attestation state
// from its cached transport. VerifiedAt carries the transport-reported stamp
// when present — historical display only, never current proof.
type AttestationObservation struct {
	EndpointID string
	State      AttestationState
	VerifiedAt *time.Time
	LastError  string
}

// AttestationSnapshot projects the cached transports belonging to tenantID
// onto the given current endpoint rows. Entries are skipped unless the cache
// key's tenant segment matches (endpoint ids are only tenant-unique — an
// unscoped snapshot would let one tenant's observation set another's status)
// AND the stored signature equals the row's full composite transport
// signature, so an edited endpoint (origin, credential_ref, credential value,
// assurance, capabilities, SDK config) reads unobserved until its next
// dispatch rebuilds the transport and genuinely re-verifies.
func (s *Service) AttestationSnapshot(tenantID string, current []Endpoint) []AttestationObservation {
	if s == nil {
		return nil
	}
	rows := make(map[string]Endpoint, len(current))
	for _, e := range current {
		rows[e.ID] = e
	}
	var out []AttestationObservation
	s.mu.Lock()
	defer s.mu.Unlock()
	for key, ent := range s.transports {
		sep := strings.IndexByte(key, 0)
		if sep < 0 || key[:sep] != tenantID {
			continue
		}
		row, ok := rows[key[sep+1:]]
		if !ok {
			continue
		}
		sig, err := s.transportCompositeSignature(row)
		if err != nil || sig != ent.signature {
			continue // edited configuration — the cached transport is not proof
		}
		st, ok := ent.up.(interface{ AttestationStatus() llm.AttestationStatus })
		if !ok {
			continue // non-attesting transport
		}
		status := st.AttestationStatus()
		switch {
		case status.Verified:
			obs := AttestationObservation{EndpointID: row.ID, State: AttestationVerified}
			if !status.VerifiedAt.IsZero() {
				stamp := status.VerifiedAt
				obs.VerifiedAt = &stamp
			}
			out = append(out, obs)
		case status.LastError != "" || !status.VerifiedAt.IsZero():
			// Real evidence of an attempted verification (incl.
			// failed-after-verified): report failed so a stale persisted row
			// stamp can never inflate the verified count (P7-01).
			obs := AttestationObservation{EndpointID: row.ID, State: AttestationFailed, LastError: status.LastError}
			if !status.VerifiedAt.IsZero() {
				stamp := status.VerifiedAt
				obs.VerifiedAt = &stamp
			}
			out = append(out, obs)
		}
		// Evidence-less static status (confidential_sdk's AttestationStatus
		// is {Enabled, Provider} — never runtime-verified): emits nothing, so
		// the endpoint reads unobserved, never failed.
	}
	return out
}

// transportCompositeSignature recomputes exactly the signature transportFor
// stamps on a freshly built transport: the endpoint fingerprint, plus the
// sha256 of the resolved confidential SDK config file for confidential_sdk
// endpoints, plus the sha256 of the resolved credential. Resolution is a
// local secrets-store/config read, not endpoint probing.
func (s *Service) transportCompositeSignature(e Endpoint) (string, error) {
	sig := transportSignature(e)
	if e.Protocol == "confidential_sdk" {
		file, err := llm.ConfidentialSDKConfigFile(os.Getenv("CONFIDENTIAL_SDK_CONFIG"), e.SDKProvider)
		if err != nil {
			return "", err
		}
		if file != "" {
			config, err := os.ReadFile(file) //nolint:gosec // path resolved from operator config via ConfidentialSDKConfigFile
			if err != nil {
				return "", fmt.Errorf("read confidential SDK config: %w", err)
			}
			sig += "|" + fmt.Sprintf("%x", sha256.Sum256(config))
		}
	}
	credential, err := s.resolveCredential(e)
	if err != nil {
		return "", err
	}
	return sig + "|" + fmt.Sprintf("%x", sha256.Sum256([]byte(credential))), nil
}
