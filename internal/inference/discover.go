package inference

import (
	"context"
	"encoding/json"
	"errors"
	"fmt"
	"net/http"
	"sort"
	"strings"

	"enclava-go/internal/llm"
)

// DiscoverProbe is one attempted capability probe: the candidate model that
// answered (or refused) the 1-token request and the outcome. An endpoint
// verdict is "yes" when any candidate answered, "unknown" otherwise — a
// refused probe never means the endpoint lacks the capability.
type DiscoverProbe struct {
	Capability string `json:"capability"`
	Model      string `json:"model"`
	Result     string `json:"result"` // "yes" | "no" | "unknown"
	Error      string `json:"error"`
}

// DiscoverReport is observed endpoint truth: the model ids the server lists
// plus the per-candidate capability probes that settled. MCP tool calls have
// no cheap probe and report "unknown" — never a guess.
type DiscoverReport struct {
	Models       []string          `json:"models"`
	ProbeModel   string            `json:"probe_model,omitempty"`
	Capabilities map[string]string `json:"capabilities"`
	Probes       []DiscoverProbe   `json:"probes"`
	Notes        []string          `json:"notes,omitempty"`
}

// Discover lists models from the endpoint's own server and probes the
// capabilities a minimal request can settle. It builds the same transport
// dispatch would use — attestation included — but never routes through
// billing or admission.
func (s *Service) Discover(ctx context.Context, e Endpoint) (DiscoverReport, error) {
	report := DiscoverReport{
		Models: []string{}, Probes: []DiscoverProbe{},
		Capabilities: map[string]string{
			CapabilityChat: "unknown", CapabilityVision: "unknown",
			CapabilityEmbeddings: "unknown", CapabilityResponses: "unknown",
			CapabilityCompact: "unknown", CapabilityMCP: "unknown",
		},
	}
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

	// Each capability probes the candidates that plausibly serve it — at
	// most three — stopping at the first yes. Chat probes the first
	// non-embedding model; that model also drives the responses-surface
	// probes and is reported as probe_model.
	probe := func(capability, model string, run func() error) {
		pr := DiscoverProbe{Capability: capability, Model: model}
		err := run()
		pr.Result = classifyProbe(err)
		pr.Error = probeError(err)
		report.Probes = append(report.Probes, pr)
		if pr.Result == "yes" {
			report.Capabilities[capability] = "yes"
		} else if report.Capabilities[capability] == "" {
			report.Capabilities[capability] = "unknown"
		}
	}
	chatBody := func(model string, vision bool) func() error {
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
		return func() error {
			body, err := json.Marshal(map[string]any{
				"model": model, "max_tokens": 1,
				"messages": []any{map[string]any{"role": "user", "content": content}},
			})
			if err != nil {
				return err
			}
			_, err = up.CreateChatCompletion(ctx, body)
			return err
		}
	}
	embedBody := func(model string) func() error {
		return func() error {
			body := []byte(fmt.Sprintf(`{"model":%q,"input":"ping"}`, model))
			_, err := up.CreateEmbeddings(ctx, body)
			return err
		}
	}
	responsesBody := func(model, path string) func() error {
		return func() error {
			body := []byte(fmt.Sprintf(`{"model":%q,"input":"ping","max_output_tokens":1}`, model))
			_, err := up.DoResponsesRequest(ctx, http.MethodPost, path, body)
			return err
		}
	}

	report.ProbeModel = probeModelFor(report.Models)
	if report.ProbeModel != "" {
		probe(CapabilityChat, report.ProbeModel, chatBody(report.ProbeModel, false))
	}
	for _, m := range probeCandidates(report.Models, looksVision, false) {
		if report.Capabilities[CapabilityVision] == "yes" {
			break
		}
		probe(CapabilityVision, m, chatBody(m, true))
	}
	for _, m := range probeCandidates(report.Models, looksEmbedding, true) {
		if report.Capabilities[CapabilityEmbeddings] == "yes" {
			break
		}
		probe(CapabilityEmbeddings, m, embedBody(m))
	}
	if report.ProbeModel != "" {
		probe(CapabilityResponses, report.ProbeModel, responsesBody(report.ProbeModel, "/responses"))
		probe(CapabilityCompact, report.ProbeModel, responsesBody(report.ProbeModel, "/responses/compact"))
	}
	// MCP tool calls have no cheap distinguishing probe: accepting a tools
	// param does not prove execution. Reported unknown, never guessed.
	report.Capabilities[CapabilityMCP] = "unknown"
	return report, nil
}

// probeCandidates picks up to 3 model ids whose names suggest the
// capability; fallback returns the chat probe model when nothing matches so
// the capability is still probed once (embedding candidates never fall back
// — a chat model cannot answer an embeddings request).
func probeCandidates(models []string, match func(string) bool, noFallback bool) []string {
	var out []string
	for _, m := range models {
		if match(m) {
			out = append(out, m)
			if len(out) == 3 {
				return out
			}
		}
	}
	if len(out) == 0 && !noFallback && probeModelFor(models) != "" {
		out = append(out, probeModelFor(models))
	}
	return out
}

// probeModelFor is the first non-embedding model id, if any.
func probeModelFor(models []string) string {
	for _, m := range models {
		if !looksEmbedding(m) {
			return m
		}
	}
	return ""
}

// looksEmbedding names ids that plausibly serve /embeddings — a substring
// heuristic for probe ordering only, never a capability claim.
func looksEmbedding(id string) bool {
	id = strings.ToLower(id)
	return strings.Contains(id, "embed") ||
		strings.Contains(id, "bge") || strings.Contains(id, "e5") ||
		strings.Contains(id, "minilm") || strings.Contains(id, "gte-")
}

// looksVision names ids that plausibly accept image input — a substring
// heuristic for probe ordering only, never a capability claim.
func looksVision(id string) bool {
	id = strings.ToLower(id)
	return strings.Contains(id, "vision") || strings.Contains(id, "llava") ||
		strings.Contains(id, "pixtral") || strings.Contains(id, "gpt-4o") ||
		strings.Contains(id, "gpt-4-") || strings.Contains(id, "gemini") ||
		strings.Contains(id, "claude") || strings.Contains(id, "gemma") ||
		strings.Contains(id, "-vl") || strings.Contains(id, "vl-")
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

// probeError renders a probe failure for the report, truncated to a
// display-safe length; success and nil-error probes report an empty string.
func probeError(err error) string {
	if err == nil {
		return ""
	}
	var upstream *llm.UpstreamError
	if errors.As(err, &upstream) && upstream.Message != "" {
		err = errors.New(upstream.Message)
	}
	msg := err.Error()
	if len(msg) > 300 {
		msg = msg[:300]
	}
	return msg
}
