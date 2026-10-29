package inference

import (
	"context"
	"encoding/json"
	"errors"
	"fmt"
	"net/http"
	"sort"

	"enclava-go/internal/llm"
)

// DiscoverReport is observed endpoint truth: the model ids the server lists
// plus which capabilities answered a probe (chat, vision, embeddings and
// the responses surfaces are settled by 1-token requests; MCP tool calls
// have no cheap probe and report "unknown" — never a guess).
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
	chat := func(vision bool) string {
		content := any("ping")
		if vision {
			// 1×1 red PNG: multimodal acceptance separates vision models.
			content = []any{map[string]any{"type": "text", "text": "ping"}, map[string]any{
				"type": "image_url",
				"image_url": map[string]string{
					"url": "data:image/png;base64,iVBORw0KGgoAAAANSUhEUgAAAAEAAAABCAYAAAAfFcSJAAAADUlEQVR42mP8z8BQDwAEhQGAhKmMIQAAAABJRU5ErkJggg==",
				},
			}}
		}
		body, err := json.Marshal(map[string]any{
			"model": report.ProbeModel, "max_tokens": 1,
			"messages": []any{map[string]any{"role": "user", "content": content}},
		})
		if err != nil {
			return "unknown"
		}
		_, err = up.CreateChatCompletion(ctx, body)
		return classifyProbe(err)
	}
	report.Capabilities[CapabilityChat] = chat(false)
	report.Capabilities[CapabilityVision] = chat(true)
	report.Capabilities[CapabilityEmbeddings] = classifyProbe(func() error {
		body := []byte(fmt.Sprintf(`{"model":%q,"input":"ping"}`, report.ProbeModel))
		_, err := up.CreateEmbeddings(ctx, body)
		return err
	}())
	responses := func(path string) string {
		body := []byte(fmt.Sprintf(`{"model":%q,"input":"ping","max_output_tokens":1}`, report.ProbeModel))
		_, err := up.DoResponsesRequest(ctx, http.MethodPost, path, body)
		return classifyProbe(err)
	}
	report.Capabilities[CapabilityResponses] = responses("/responses")
	report.Capabilities[CapabilityCompact] = responses("/responses/compact")
	// MCP tool calls have no cheap distinguishing probe: accepting a tools
	// param does not prove execution. Reported unknown, never guessed.
	report.Capabilities[CapabilityMCP] = "unknown"
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
