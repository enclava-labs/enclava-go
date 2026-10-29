// dashboard_confidential.go — the confidential-status card model (OA-10 +
// B13). Read-only: no VerifyAttestation call, no writes, no probing — the
// rows already fetched feed the configuration-binding check. Endpoint detail
// is withheld unless models.manage (T-07-03-01). Phase 8's attention
// aggregator consumes the SAME projection through this helper.
package httpx

import (
	"context"
	"fmt"
	"log"
	"net/url"
	"time"

	"enclava-go/internal/auth"
	"enclava-go/internal/inference"
)

// confidentialEndpointState is one confidential endpoint's card row. The
// stamp is historical display only ("Checked …ago"), never current proof.
type confidentialEndpointState struct {
	EndpointID, Label, Protocol string
	State                       inference.AttestationState
	VerifiedAt                  *time.Time
	LastError                   string
}

// confidentialSummary drives the card: Verified counts only genuinely
// verified observations — failed-after-verified and unobserved do NOT count
// (P7-01); CheckedAt is the freshest historical stamp across endpoints.
type confidentialSummary struct {
	Total, Verified int
	CheckedAt       *time.Time
	Endpoints       []confidentialEndpointState // populated ONLY when models.manage
}

// MobileTone mirrors the card tone on the mobilebar status dot.
func (s confidentialSummary) MobileTone() string {
	if s.Total > 0 && s.Verified < s.Total {
		return "warn"
	}
	return "good"
}

// StatusTitle is the status dot/card accessible description.
func (s confidentialSummary) StatusTitle() string {
	if s.Total == 0 {
		return "No confidential endpoints"
	}
	return fmt.Sprintf("%d of %d confidential endpoints verified", s.Verified, s.Total)
}

// confidentialEndpoint reports whether the endpoint participates in the
// confidential card: attesting protocols or any configured assurance mode.
func confidentialEndpoint(e inference.Endpoint) bool {
	return e.Protocol == "tinfoil" || e.Protocol == "confidential_sdk" || e.AssuranceMode != ""
}

// summarizeEndpoints is the pure aggregation: observation state absent →
// unobserved; CheckedAt = max(transport VerifiedAt, persisted
// AssuranceVerifiedAt) — historical, never now().
func summarizeEndpoints(eps []inference.Endpoint, obs []inference.AttestationObservation) confidentialSummary {
	byID := make(map[string]inference.AttestationObservation, len(obs))
	for _, o := range obs {
		byID[o.EndpointID] = o
	}
	var s confidentialSummary
	for _, e := range eps {
		if !confidentialEndpoint(e) {
			continue
		}
		s.Total++
		state := inference.AttestationUnobserved
		var stamp *time.Time
		if o, ok := byID[e.ID]; ok {
			state = o.State
			stamp = o.VerifiedAt
		}
		if state == inference.AttestationVerified {
			s.Verified++
		}
		s.CheckedAt = laterStamp(s.CheckedAt, laterStamp(stamp, e.AssuranceVerifiedAt))
	}
	return s
}

// withEndpoints fills the per-endpoint detail rows; the App method calls it
// only under models.manage so member summaries carry no endpoint data
// (T-07-03-01).
func (s confidentialSummary) withEndpoints(eps []inference.Endpoint, obs []inference.AttestationObservation) confidentialSummary {
	byID := make(map[string]inference.AttestationObservation, len(obs))
	for _, o := range obs {
		byID[o.EndpointID] = o
	}
	s.Endpoints = make([]confidentialEndpointState, 0, len(eps))
	for _, e := range eps {
		if !confidentialEndpoint(e) {
			continue
		}
		state := inference.AttestationUnobserved
		var stamp *time.Time
		lastErr := ""
		if o, ok := byID[e.ID]; ok {
			state = o.State
			stamp = o.VerifiedAt
			lastErr = o.LastError
		}
		s.Endpoints = append(s.Endpoints, confidentialEndpointState{
			EndpointID: e.ID,
			Label:      endpointCardLabel(e),
			Protocol:   e.Protocol,
			State:      state,
			VerifiedAt: laterStamp(stamp, e.AssuranceVerifiedAt),
			LastError:  lastErr,
		})
	}
	return s
}

func laterStamp(a, b *time.Time) *time.Time {
	switch {
	case a == nil:
		return b
	case b == nil:
		return a
	case b.After(*a):
		return b
	default:
		return a
	}
}

// endpointCardLabel names protocol + host ("OpenAI-compatible · api.example",
// "Tinfoil · enclave.example").
func endpointCardLabel(e inference.Endpoint) string {
	host := e.BaseOrigin
	if u, err := url.Parse(e.BaseOrigin); err == nil && u.Host != "" {
		host = u.Host
	}
	return endpointProtocolLabel(e.Protocol) + " · " + host
}

// endpointProtocolLabels names the protocols whose mechanical humanization
// reads wrong; the rest (tinfoil, privatemode) humanize to their brand name.
var endpointProtocolLabels = map[string]string{
	"openai_compatible": "OpenAI-compatible",
	"confidential_sdk":  "Confidential SDK",
	"redpill":           "RedPill",
}

// endpointProtocolLabel is the one display name for an inference protocol —
// card labels, the Models table and the endpoint drawer's choices.
func endpointProtocolLabel(protocol string) string {
	if label, ok := endpointProtocolLabels[protocol]; ok {
		return label
	}
	return dashboardHumanize(protocol)
}

func init() { dashboardBaseFuncs["protocolLabel"] = endpointProtocolLabel }

// confidentialSummary assembles the card state for the request's tenant.
// Read-only by contract (B13): List + AttestationSnapshot only.
func (a *App) confidentialSummary(ctx context.Context, p auth.Principal, caps dashboardCaps) confidentialSummary {
	if a.endpoints == nil || p.TenantID == "" {
		return confidentialSummary{}
	}
	eps, err := a.endpoints.List(ctx, p.TenantID)
	if err != nil {
		log.Printf("dashboard confidential endpoints: %v", err)
		return confidentialSummary{}
	}
	var obs []inference.AttestationObservation
	if a.executor != nil {
		obs = a.executor.AttestationSnapshot(p.TenantID, eps)
	}
	s := summarizeEndpoints(eps, obs)
	if caps.ModelsManage {
		s = s.withEndpoints(eps, obs)
	}
	return s
}
