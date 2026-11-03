package httpx

import (
	"encoding/json"
	"net"
	"net/http"
	"net/http/httptest"
	"strings"
	"testing"
)

// readyzFixture builds the lightest valid App: no services are needed for
// the readiness and API-not-found surfaces under test.
func readyzFixture(t *testing.T) *App {
	t.Helper()
	return NewApp(nil, nil, "", nil, nil, nil, nil, nil, nil, nil, nil, nil, nil,
		"sha256", "", "enc_", nil, nil, 1, false, nil, nil, GateConfig{}, nil, nil)
}

// closedPortURL returns a URL on a port that is guaranteed refused (opened
// then immediately closed) — the vector-store probe's unreachable case.
func closedPortURL(t *testing.T) string {
	t.Helper()
	l, err := net.Listen("tcp", "127.0.0.1:0")
	if err != nil {
		t.Fatalf("reserve port: %v", err)
	}
	addr := l.Addr().String()
	if err := l.Close(); err != nil {
		t.Fatalf("close probe port: %v", err)
	}
	return "http://" + addr
}

func serve(app *App, path string) *httptest.ResponseRecorder {
	rec := httptest.NewRecorder()
	app.Mux().ServeHTTP(rec, httptest.NewRequest(http.MethodGet, path, nil))
	return rec
}

// TestReadyzIncludesDependenciesWithoutGating pins IDOPS-MD-I01: /readyz
// keeps its inference-only gate (nil prober → ready) while reporting every
// configured dependency — the unreachable vector store shows up in the JSON
func TestReadyzIncludesDependenciesWithoutGating(t *testing.T) {
	app := readyzFixture(t)
	app.ConfigureVectorStoreProbe(ProbeHTTP(closedPortURL(t)))
	rec := serve(app, "/readyz")
	if rec.Code != http.StatusOK {
		t.Fatalf("/readyz = %d, want 200 with vector store down", rec.Code)
	}
	var body struct {
		Ready        bool `json:"ready"`
		Dependencies map[string]struct {
			Ready bool `json:"ready"`
		} `json:"dependencies"`
	}
	if err := json.Unmarshal(rec.Body.Bytes(), &body); err != nil {
		t.Fatalf("readyz body: %v", err)
	}
	if !body.Ready {
		t.Fatal("readyz must stay ready on inference")
	}
	if _, ok := body.Dependencies["inference"]; !ok {
		t.Fatal("readyz omitted inference dependency")
	}
	vs, ok := body.Dependencies["vector_store"]
	if !ok {
		t.Fatalf("readyz dependencies = %v, want vector_store detail", body.Dependencies)
	}
	if vs.Ready {
		t.Fatal("vector_store must report ready=false while unreachable")
	}
}

// TestReadyzKnowledge503WhenVectorStoreDown pins the knowledge-only probe:
// 503 while the vector store is unreachable, 200 once reachable.
func TestReadyzKnowledge503WhenVectorStoreDown(t *testing.T) {
	app := readyzFixture(t)

	if rec := serve(app, "/readyz/knowledge"); rec.Code != http.StatusServiceUnavailable {
		t.Fatalf("unconfigured /readyz/knowledge = %d, want 503", rec.Code)
	}

	app.ConfigureVectorStoreProbe(ProbeHTTP(closedPortURL(t)))
	rec := serve(app, "/readyz/knowledge")
	if rec.Code != http.StatusServiceUnavailable {
		t.Fatalf("/readyz/knowledge = %d, want 503 with vector store down", rec.Code)
	}
	var body struct {
		Ready        bool `json:"ready"`
		Dependencies map[string]struct {
			Ready bool `json:"ready"`
		} `json:"dependencies"`
	}
	if err := json.Unmarshal(rec.Body.Bytes(), &body); err != nil {
		t.Fatalf("knowledge readyz body: %v", err)
	}
	if body.Ready {
		t.Fatal("knowledge readyz must report ready=false")
	}
	if _, ok := body.Dependencies["vector_store"]; !ok {
		t.Fatalf("dependencies = %v, want vector_store", body.Dependencies)
	}

	up := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, _ *http.Request) {
		w.WriteHeader(http.StatusOK)
	}))
	defer up.Close()
	app.ConfigureVectorStoreProbe(ProbeHTTP(up.URL))
	if rec := serve(app, "/readyz/knowledge"); rec.Code != http.StatusOK {
		t.Fatalf("/readyz/knowledge = %d, want 200 with vector store up", rec.Code)
	}
}

// TestUnknownAPIRoutesReturnJSONEnvelope pins OPS-I02: unmatched /api/* and
// /v1/* paths answer the JSON error envelope (code not_found), while anonymous
// dashboard requests retain their session redirect.
func TestUnknownAPIRoutesReturnJSONEnvelope(t *testing.T) {
	app := readyzFixture(t)

	for _, path := range []string{"/api/v1/does-not-exist", "/api/nope", "/v1/nope", "/v1/models-typo/x"} {
		rec := serve(app, path)
		if rec.Code != http.StatusNotFound {
			t.Fatalf("%s = %d, want 404", path, rec.Code)
		}
		if ct := rec.Header().Get("Content-Type"); !strings.HasPrefix(ct, "application/json") {
			t.Fatalf("%s content-type = %q, want JSON", path, ct)
		}
		var env struct {
			Error struct {
				Code string `json:"code"`
			} `json:"error"`
		}
		if err := json.Unmarshal(rec.Body.Bytes(), &env); err != nil {
			t.Fatalf("%s body not JSON: %v", path, err)
		}
		if env.Error.Code != "not_found" {
			t.Fatalf("%s error code = %q, want not_found", path, env.Error.Code)
		}
	}

	rec := serve(app, "/dashboard/nope")
	if rec.Code != http.StatusSeeOther {
		t.Fatalf("/dashboard/nope = %d, want login redirect", rec.Code)
	}
	if ct := rec.Header().Get("Content-Type"); strings.HasPrefix(ct, "application/json") {
		t.Fatalf("dashboard redirect must not be JSON, got %q", ct)
	}
}
