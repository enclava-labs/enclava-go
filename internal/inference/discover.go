package inference

import (
	"context"
	"errors"
	"fmt"
	"net/http"
	"sort"

	"enclava-go/internal/llm"
)

// DiscoverReport is observed endpoint truth: the model ids the server lists
// plus which capabilities answered a 1-token probe. Capabilities without a
// cheap probe (vision, responses/compact, mcp) report "unknown" — never a
// guess, never silently enabled.
type DiscoverReport struct {
	Models       []string          `json:"models"`
	ProbeModel   string            `json:"probe_model,omitempty"`
	Capabilities map[string]string `json:"capabilities"`
	Notes        []string          `json:"notes,omitempty"`
}

// Discover lists models from the endpoint's own server and probes the
// capabilities a minimal request can settle. It builds the same transport
// dispatch would use — attestation included — but never routes through
// billing or admission.
func (s *Service) Discover(ctx context.Context, e Endpoint) (DiscoverReport, error) {
	report := DiscoverReport{Capabilities: map[string]string{}}
	up, err := s.transportFor(ctx, e)
	if err != nil {
		return report, err
	}
	lister, ok := up.(interface {
		ListModels(context.Context) (llm.ModelsResponse, error)
	})
	if !ok {
		return report, errors.New("inference: transport does not support model listing")
	}
	models, err := lister.ListModels(ctx)
	if err != nil {
		return report, fmt.Errorf("list models: %w", err)
	}
	for _, m := range models.Data {
		if m.ID != "" {
			report.Models = append(report.Models, m.ID)
		}
	}
	sort.Strings(report.Models)
	if len(report.Models) == 0 {
		report.Notes = append(report.Notes, "server listed no models")
		return report, nil
	}
	report.ProbeModel = report.Models[0]
	report.Capabilities[CapabilityChat] = classifyProbe(func() error {
		body := []byte(fmt.Sprintf(`{"model":%q,"messages":[{"role":"user","content":"ping"}],"max_tokens":1}`, report.ProbeModel))
		_, err := up.CreateChatCompletion(ctx, body)
		return err
	}())
	report.Capabilities[CapabilityEmbeddings] = classifyProbe(func() error {
		body := []byte(fmt.Sprintf(`{"model":%q,"input":"ping"}`, report.ProbeModel))
		_, err := up.CreateEmbeddings(ctx, body)
		return err
	}())
	report.Capabilities[CapabilityResponses] = classifyProbe(func() error {
		body := []byte(fmt.Sprintf(`{"model":%q,"input":"ping","max_output_tokens":1}`, report.ProbeModel))
		_, err := up.DoResponsesRequest(ctx, http.MethodPost, "/responses", body)
		return err
	}())
	for _, capability := range []string{CapabilityVision, CapabilityCompact, CapabilityMCP} {
		report.Capabilities[capability] = "unknown"
	}
	return report, nil
}

// classifyProbe settles a capability probe: success means supported, a
// 400/404 means the server rejected the capability itself, anything else
// (auth, rate limits, timeouts) stays unknown rather than recorded absent.
func classifyProbe(err error) string {
	if err == nil {
		return "yes"
	}
	var upstream *llm.UpstreamError
	if errors.As(err, &upstream) && (upstream.StatusCode == http.StatusBadRequest || upstream.StatusCode == http.StatusNotFound) {
		return "no"
	}
	return "unknown"
}
