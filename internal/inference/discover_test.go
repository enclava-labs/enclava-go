package inference_test

import (
	"reflect"
	"testing"

	"enclava-go/internal/inference"
	"enclava-go/internal/llm"
)

// Discovery reads the endpoint's own model list and settles capabilities by
// 1-token probe: success enables, 400/404 disables, anything else (here the
// 401 on responses) stays unknown instead of being recorded as absent.
func TestDiscoverListsModelsSortedAndClassifiesProbes(t *testing.T) {
	f := newFx(t)
	f.upstream.models = []llm.Model{{ID: "zeta"}, {ID: "alpha"}}
	f.upstream.embedErr = &llm.UpstreamError{StatusCode: 400, Message: "embeddings not supported"}
	f.upstream.responsesErr = &llm.UpstreamError{StatusCode: 401, Message: "bad key"}
	f.upstream.visionErr = &llm.UpstreamError{StatusCode: 400, Message: "image not supported"}
	f.upstream.compactErr = &llm.UpstreamError{StatusCode: 404, Message: "no compact surface"}
	f.build(t)

	report, err := f.svc.Discover(f.ctx, f.mkEndpoint(t))
	if err != nil {
		t.Fatalf("discover: %v", err)
	}
	if !reflect.DeepEqual(report.Models, []string{"alpha", "zeta"}) {
		t.Fatalf("models not sorted: %v", report.Models)
	}
	if report.ProbeModel != "alpha" {
		t.Fatalf("probe model = %q", report.ProbeModel)
	}
	want := map[string]string{
		inference.CapabilityChat:       "yes",
		inference.CapabilityEmbeddings: "no",
		inference.CapabilityResponses:  "unknown",
		inference.CapabilityVision:     "no",
		inference.CapabilityCompact:    "no",

		inference.CapabilityMCP:        "unknown",
	}
	if !reflect.DeepEqual(report.Capabilities, want) {
		t.Fatalf("capabilities = %v", report.Capabilities)
	}
}
