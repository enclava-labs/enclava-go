package httpx

import (
	"context"
	"encoding/json"
	"net/http"
	"net/http/httptest"
	"strings"
	"testing"
	"time"

	"enclava-go/internal/auth"
	"enclava-go/internal/llm"
	"enclava-go/internal/store/postgres"
	"enclava-go/internal/testdb"
)

// stubListProvider answers handleModels without a real upstream.
type stubListProvider struct{ completionProvider }

func (p stubListProvider) ListModels(context.Context) (llm.ModelsResponse, error) {
	return llm.ModelsResponse{Object: "list", Data: []llm.Model{{ID: "m1"}}}, nil
}

func (p stubListProvider) GetModel(_ context.Context, id string) (llm.Model, error) {
	return llm.Model{ID: id}, nil
}

// newMiddlewareAuthApp wires an App on the real postgres identity store
// (testdb) with a seeded tenant+user so authentication under test exercises
// stored credentials, never a fabricated Principal.
func newMiddlewareAuthApp(t *testing.T) (*postgres.DB, *postgres.IdentityRepo, string, *auth.Service) {
	t.Helper()
	db := testdb.New(t)
	repo := postgres.NewIdentityRepo(db)
	tenant := testdb.SeedTenant(t, db, "middleware")
	testdb.SeedUser(t, db, tenant, "mw@example.com", nil)
	return db, repo, tenant, auth.NewService(repo, "")
}

func setKeyGrant(t *testing.T, repo *postgres.IdentityRepo, tenant, keyID string, apply func(*auth.APIKeyRecord)) {
	t.Helper()
	rec, err := repo.GetAPIKey(context.Background(), tenant, keyID)
	if err != nil {
		t.Fatalf("get key: %v", err)
	}
	apply(&rec)
	if err := repo.UpdateAPIKey(context.Background(), tenant, rec); err != nil {
		t.Fatalf("update key: %v", err)
	}
}

func endpointMiddlewareApp(repo *postgres.IdentityRepo, svc *auth.Service, endpoints *postgres.EndpointsRepo) *App {
	app := &App{authSvc: svc, identity: repo, endpoints: endpoints, mux: http.NewServeMux(), maxUploadMB: 1}
	app.mux.HandleFunc("/api/v1/models", app.authenticated("models.list", app.handleModels))
	app.mux.HandleFunc("/api/v1/probe", app.authenticated("models.list", func(w http.ResponseWriter, r *http.Request, _ auth.Principal) {
		WriteJSON(w, http.StatusOK, map[string]any{"ok": true})
	}))
	return app
}

func TestEndpointGrantEmptyAndMalformedDenyAll(t *testing.T) {
	db, repo, tenant, svc := newMiddlewareAuthApp(t)
	defer func() { _ = db.Close() }()
	user, _ := repo.GetUserByEmail(context.Background(), tenant, "mw@example.com")
	apiKey, rec := testdb.SeedAPIKey(t, db, tenant, testdb.SeedKeySpec{
		Name: "mw", OwnerUserID: user.ID, Scopes: []string{"models.list"},
	})
	app := endpointMiddlewareApp(repo, svc, postgres.NewEndpointsRepo(db))

	for name, grant := range map[string]json.RawMessage{
		"malformed": json.RawMessage(`{"mode":"bogus"}`),
		"empty":     json.RawMessage(`{"mode":"list","items":[]}`),
	} {
		t.Run(name, func(t *testing.T) {
			setKeyGrant(t, repo, tenant, rec.ID, func(k *auth.APIKeyRecord) { k.AllowedEndpoints = grant })
			req := httptest.NewRequest(http.MethodGet, "/api/v1/models", nil)
			req.Header.Set("Authorization", "Bearer "+apiKey)
			res := httptest.NewRecorder()
			app.Handler().ServeHTTP(res, req)
			if res.Code != http.StatusForbidden {
				t.Fatalf("expected 403 for %s endpoint grant, got %d body=%s", name, res.Code, res.Body.String())
			}
		})
	}
}

func TestEndpointGrantListModeDispatchesListedEndpointOnly(t *testing.T) {
	db, repo, tenant, svc := newMiddlewareAuthApp(t)
	defer func() { _ = db.Close() }()
	user, _ := repo.GetUserByEmail(context.Background(), tenant, "mw@example.com")
	apiKey, rec := testdb.SeedAPIKey(t, db, tenant, testdb.SeedKeySpec{
		Name: "mw", OwnerUserID: user.ID, Scopes: []string{"models.list"},
	})
	setKeyGrant(t, repo, tenant, rec.ID, func(k *auth.APIKeyRecord) {
		k.AllowedEndpoints = json.RawMessage(`{"mode":"list","items":["models"]}`)
	})
	app := endpointMiddlewareApp(repo, svc, postgres.NewEndpointsRepo(db))
	app.provider = &stubListProvider{}

	req := httptest.NewRequest(http.MethodGet, "/api/v1/models", nil)
	req.Header.Set("Authorization", "Bearer "+apiKey)
	res := httptest.NewRecorder()
	app.Handler().ServeHTTP(res, req)
	if res.Code != http.StatusOK {
		t.Fatalf("listed endpoint id expected 200, got %d body=%s", res.Code, res.Body.String())
	}

	req = httptest.NewRequest(http.MethodGet, "/api/v1/probe", nil)
	req.Header.Set("Authorization", "Bearer "+apiKey)
	res = httptest.NewRecorder()
	app.Handler().ServeHTTP(res, req)
	if res.Code != http.StatusForbidden {
		t.Fatalf("unlisted endpoint id expected 403, got %d body=%s", res.Code, res.Body.String())
	}
}

func TestIPGrantListModeAllowsListedIPDeniesOthers(t *testing.T) {
	db, repo, tenant, svc := newMiddlewareAuthApp(t)
	defer func() { _ = db.Close() }()
	user, _ := repo.GetUserByEmail(context.Background(), tenant, "mw@example.com")
	apiKey, rec := testdb.SeedAPIKey(t, db, tenant, testdb.SeedKeySpec{
		Name: "mw", OwnerUserID: user.ID, Scopes: []string{"models.list"},
	})
	setKeyGrant(t, repo, tenant, rec.ID, func(k *auth.APIKeyRecord) {
		k.AllowedIPs = json.RawMessage(`{"mode":"list","items":["203.0.113.7"]}`)
	})
	app := endpointMiddlewareApp(repo, svc, postgres.NewEndpointsRepo(db))

	req := httptest.NewRequest(http.MethodGet, "/api/v1/probe", nil)
	req.Header.Set("Authorization", "Bearer "+apiKey)
	req.RemoteAddr = "203.0.113.7:1234"
	res := httptest.NewRecorder()
	app.Handler().ServeHTTP(res, req)
	if res.Code != http.StatusOK {
		t.Fatalf("listed IP expected 200, got %d body=%s", res.Code, res.Body.String())
	}

	req = httptest.NewRequest(http.MethodGet, "/api/v1/probe", nil)
	req.Header.Set("Authorization", "Bearer "+apiKey)
	req.RemoteAddr = "198.51.100.9:1234"
	res = httptest.NewRecorder()
	app.Handler().ServeHTTP(res, req)
	if res.Code != http.StatusForbidden {
		t.Fatalf("unlisted IP expected 403, got %d body=%s", res.Code, res.Body.String())
	}
}

func TestSessionCookieAuthAndCSRFGuard(t *testing.T) {
	db, repo, tenant, svc := newMiddlewareAuthApp(t)
	defer func() { _ = db.Close() }()
	user := testdb.SeedUser(t, db, tenant, "sess@example.com", []string{"models.list"})
	token, csrf, _, err := svc.IssueSession(context.Background(), tenant, user.ID, time.Hour)
	if err != nil {
		t.Fatalf("issue session: %v", err)
	}
	app := endpointMiddlewareApp(repo, svc, postgres.NewEndpointsRepo(db))
	cookie := &http.Cookie{Name: sessionCookieName, Value: token}

	// GET with the session cookie authenticates.
	req := httptest.NewRequest(http.MethodGet, "/api/v1/probe", nil)
	req.AddCookie(cookie)
	res := httptest.NewRecorder()
	app.Handler().ServeHTTP(res, req)
	if res.Code != http.StatusOK {
		t.Fatalf("session GET expected 200, got %d body=%s", res.Code, res.Body.String())
	}

	// POST without CSRF is denied.
	req = httptest.NewRequest(http.MethodPost, "/api/v1/probe", nil)
	req.AddCookie(cookie)
	res = httptest.NewRecorder()
	app.Handler().ServeHTTP(res, req)
	if res.Code != http.StatusForbidden {
		t.Fatalf("session POST without CSRF expected 403, got %d body=%s", res.Code, res.Body.String())
	}

	// POST with a wrong CSRF is denied.
	req = httptest.NewRequest(http.MethodPost, "/api/v1/probe", nil)
	req.AddCookie(cookie)
	req.Header.Set("X-CSRF-Token", "wrong")
	res = httptest.NewRecorder()
	app.Handler().ServeHTTP(res, req)
	if res.Code != http.StatusForbidden {
		t.Fatalf("session POST with bad CSRF expected 403, got %d body=%s", res.Code, res.Body.String())
	}

	// POST with the issued CSRF token passes.
	req = httptest.NewRequest(http.MethodPost, "/api/v1/probe", nil)
	req.AddCookie(cookie)
	req.Header.Set("X-CSRF-Token", csrf)
	res = httptest.NewRecorder()
	app.Handler().ServeHTTP(res, req)
	if res.Code != http.StatusOK {
		t.Fatalf("session POST with CSRF expected 200, got %d body=%s", res.Code, res.Body.String())
	}
}

func TestHandlerAddsRequestIDSecureHeadersAndCORS(t *testing.T) {
	app := &App{
		mux:         http.NewServeMux(),
		corsOrigins: []string{"https://admin.example.com"},
		maxUploadMB: 1,
	}
	app.mux.HandleFunc("/ok", func(w http.ResponseWriter, r *http.Request) {
		if got := requestIDFromContext(r.Context()); got != "req-test" {
			t.Fatalf("expected request id in context, got %q", got)
		}
		WriteJSON(w, http.StatusOK, map[string]any{"ok": true})
	})

	req := httptest.NewRequest(http.MethodGet, "/ok", nil)
	req.Header.Set("Origin", "https://admin.example.com")
	req.Header.Set("X-Request-ID", "req-test")
	rec := httptest.NewRecorder()

	app.Handler().ServeHTTP(rec, req)

	if rec.Code != http.StatusOK {
		t.Fatalf("expected status 200, got %d", rec.Code)
	}
	if got := rec.Header().Get("X-Request-ID"); got != "req-test" {
		t.Fatalf("expected response request id header, got %q", got)
	}
	if got := rec.Header().Get("X-Content-Type-Options"); got != "nosniff" {
		t.Fatalf("expected nosniff header, got %q", got)
	}
	if got := rec.Header().Get("Access-Control-Allow-Origin"); got != "https://admin.example.com" {
		t.Fatalf("expected CORS origin, got %q", got)
	}
}

func TestCORSPreflightShortCircuits(t *testing.T) {
	app := &App{
		mux:         http.NewServeMux(),
		corsOrigins: []string{"https://admin.example.com"},
		maxUploadMB: 1,
	}
	app.mux.HandleFunc("/ok", func(w http.ResponseWriter, r *http.Request) {
		t.Fatalf("preflight should not reach route handler")
	})

	req := httptest.NewRequest(http.MethodOptions, "/ok", nil)
	req.Header.Set("Origin", "https://admin.example.com")
	rec := httptest.NewRecorder()

	app.Handler().ServeHTTP(rec, req)

	if rec.Code != http.StatusNoContent {
		t.Fatalf("expected status 204, got %d", rec.Code)
	}
	if got := rec.Header().Get("Access-Control-Allow-Methods"); !strings.Contains(got, "POST") {
		t.Fatalf("expected CORS methods, got %q", got)
	}
}

func TestCORSPreflightReflectsProxyHeaders(t *testing.T) {
	app := &App{
		mux:         http.NewServeMux(),
		corsOrigins: []string{"https://admin.example.com"},
		maxUploadMB: 1,
	}
	app.mux.HandleFunc("/v1/responses", func(w http.ResponseWriter, r *http.Request) {
		t.Fatalf("preflight should not reach route handler")
	})

	req := httptest.NewRequest(http.MethodOptions, "/v1/responses", nil)
	req.Header.Set("Origin", "https://admin.example.com")
	req.Header.Set("Access-Control-Request-Method", "PATCH")
	req.Header.Set("Access-Control-Request-Headers", "Idempotency-Key, OpenAI-Beta, X-Client-Request-Id, X-Evil")
	rec := httptest.NewRecorder()

	app.Handler().ServeHTTP(rec, req)

	if rec.Code != http.StatusNoContent {
		t.Fatalf("expected status 204, got %d", rec.Code)
	}
	if got := rec.Header().Get("Access-Control-Allow-Methods"); !strings.Contains(got, "PATCH") {
		t.Fatalf("expected PATCH in CORS methods, got %q", got)
	}
	allowHeaders := rec.Header().Get("Access-Control-Allow-Headers")
	for _, name := range []string{"Idempotency-Key", "OpenAI-Beta", "X-Client-Request-Id"} {
		if !strings.Contains(allowHeaders, name) {
			t.Fatalf("expected %s in allow headers, got %q", name, allowHeaders)
		}
	}
	if strings.Contains(allowHeaders, "X-Evil") {
		t.Fatalf("did not expect X-Evil in allow headers, got %q", allowHeaders)
	}
	if strings.Contains(allowHeaders, "Authorization") {
		t.Fatalf("unrequested allow-list header should not be reflected, got %q", allowHeaders)
	}
	vary := strings.Join(rec.Header().Values("Vary"), ", ")
	if !strings.Contains(vary, "Access-Control-Request-Headers") {
		t.Fatalf("expected Vary for request headers, got %q", vary)
	}
}
