package httpx

import (
	"encoding/json"
	"net/http"
	"net/http/httptest"
	"testing"

	"enclava-go/internal/extract"
)

// TestReadyzKnowledgeReportsExtractionWorker pins the resilient-ingest
// readiness field: reported only when a worker socket is configured, and
// never a reason for the status code to change.
func TestReadyzKnowledgeReportsExtractionWorker(t *testing.T) {
	app := readyzFixture(t)
	app.ConfigureVectorStoreProbe(ProbeHTTP(closedPortURL(t)))

	extract.SetNativeWorker("")
	deps := readyzKnowledgeDeps(t, app)
	if _, ok := deps["extraction_worker"]; ok {
		t.Fatalf("no socket configured: extraction_worker = %v, want absent", deps["extraction_worker"])
	}

	extract.SetNativeWorker(t.TempDir() + "/absent-worker.sock")
	t.Cleanup(func() { extract.SetNativeWorker("") })
	deps = readyzKnowledgeDeps(t, app)
	if got, _ := deps["extraction_worker"].(string); got != "unavailable" {
		t.Fatalf("dead socket: extraction_worker = %v, want unavailable", deps["extraction_worker"])
	}

	// Worker down must not change the status-code semantics: the verdict
	// stays the vector store's alone.
	if rec := serve(app, "/readyz/knowledge"); rec.Code != http.StatusServiceUnavailable {
		t.Fatalf("vector store down = %d, want 503 regardless of worker", rec.Code)
	}
	up := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, _ *http.Request) {
		w.WriteHeader(http.StatusOK)
	}))
	defer up.Close()
	app.ConfigureVectorStoreProbe(ProbeHTTP(up.URL))
	if rec := serve(app, "/readyz/knowledge"); rec.Code != http.StatusOK {
		t.Fatalf("worker down + vector store up = %d, want 200", rec.Code)
	}
}

// readyzKnowledgeDeps decodes the /readyz/knowledge dependencies map.
func readyzKnowledgeDeps(t *testing.T, app *App) map[string]any {
	t.Helper()
	rec := serve(app, "/readyz/knowledge")
	var body struct {
		Dependencies map[string]any `json:"dependencies"`
	}
	if err := json.Unmarshal(rec.Body.Bytes(), &body); err != nil {
		t.Fatalf("readyz body: %v", err)
	}
	return body.Dependencies
}
