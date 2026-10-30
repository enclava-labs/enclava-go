package main

import (
	"context"
	"net/http"
	"net/http/httptest"
	"testing"
	"time"

	"enclava-go/internal/config"
)

// The env provider and the seeded endpoint record share one URL contract:
// the configured value is an origin (the documented ".../v1" form included)
// and the API lives under /v1. Real OpenAI-compatible servers (vLLM, Ollama)
// serve only /v1/models; startup model discovery against them must succeed,
// and the record must not double the /v1 its transport adds.
func TestEnvProviderURLContract(t *testing.T) {
	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		if r.URL.Path != "/v1/models" {
			http.NotFound(w, r)
			return
		}
		w.Header().Set("Content-Type", "application/json")
		_, _ = w.Write([]byte(`{"object":"list","data":[{"id":"nomic-embed-text:latest","object":"model"}]}`))
	}))
	defer srv.Close()

	for _, configured := range []string{srv.URL, srv.URL + "/v1/"} {
		models, err := envAdapter(configured, "key", 5*time.Second, "openai_compatible").ListModels(context.Background())
		if err != nil {
			t.Fatalf("env model discovery with base %q: %v", configured, err)
		}
		if len(models.Data) != 1 || models.Data[0].ID != "nomic-embed-text:latest" {
			t.Fatalf("base %q: models = %+v", configured, models.Data)
		}
		record := runtimeProviderConfig(config.Config{Provider: "openai_compatible", OpenAICompatibleBaseURL: configured})
		if record.BaseOrigin != srv.URL {
			t.Fatalf("base %q: endpoint record origin = %q, want %q", configured, record.BaseOrigin, srv.URL)
		}
	}
}
