package inference_test

import (
	"testing"

	"enclava-go/internal/llm"
)

func TestModelContextLengthResolvesRouteAlias(t *testing.T) {
	f := newFx(t)
	f.mkEndpoint(t)
	f.upstream.models = []llm.Model{{ID: "gpt-4o-mini", ContextLength: 8192}}
	got, err := f.svc.ModelContextLength(f.ctx, f.principal, "gpt-fast", "")
	if err != nil || got != 8192 {
		t.Fatalf("context length = %d, err = %v", got, err)
	}
	f.upstream.models = nil
	got, err = f.svc.ModelContextLength(f.ctx, f.principal, "gpt-fast", "ep-1")
	if err != nil || got != 8192 {
		t.Fatalf("cached context length = %d, err = %v", got, err)
	}
	f.build(t)
	got, err = f.svc.ModelContextLength(f.ctx, f.principal, "gpt-fast", "ep-1")
	if err != nil || got != 0 {
		t.Fatalf("unknown context length = %d, err = %v", got, err)
	}
	foreign := f.principal
	foreign.TenantID = "other-tenant"
	if _, err := f.svc.ModelContextLength(f.ctx, foreign, "gpt-fast", "ep-1"); err == nil {
		t.Fatal("cross-tenant endpoint accepted")
	}
}
