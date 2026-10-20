package httpx

import (
	"bytes"
	"net/http"
	"net/http/httptest"
	"testing"

	"enclava-go/internal/auth"
	"enclava-go/internal/rag"
	"enclava-go/internal/store/postgres"
	"enclava-go/internal/testdb"
)

func TestRAGSearchRouteAuthAndErrors(t *testing.T) {
	db := testdb.New(t)
	repo := postgres.NewIdentityRepo(db)
	tenant := testdb.SeedTenant(t, db, "bootstrap")
	user := testdb.SeedUser(t, db, tenant, "rag-search@example.com", nil)
	withScope, _ := testdb.SeedAPIKey(t, db, tenant, testdb.SeedKeySpec{Name: "rag-search", OwnerUserID: user.ID, Scopes: []string{"rag"}})
	withoutScope, _ := testdb.SeedAPIKey(t, db, tenant, testdb.SeedKeySpec{Name: "no-rag", OwnerUserID: user.ID, Scopes: []string{"models.list"}})
	authSvc := auth.NewService(repo, "")
	app := NewApp(authSvc, nil, "", nil, repo, nil, nil, nil, nil, nil, nil, nil, nil, "sha256", "", "enc_", nil, nil, 1, false, nil, nil, GateConfig{}, nil, nil)
	app.ragSvc = &rag.Service{}

	request := func(key, body string) *httptest.ResponseRecorder {
		req := httptest.NewRequest(http.MethodPost, "/api/v1/rag/collections/c1/search", bytes.NewBufferString(body))
		if key != "" {
			req.Header.Set("Authorization", "Bearer "+key)
		}
		rec := httptest.NewRecorder()
		app.Handler().ServeHTTP(rec, req)
		return rec
	}
	if got := request("", `{}`); got.Code != http.StatusUnauthorized {
		t.Fatalf("mounted route should require authentication, got %d", got.Code)
	}
	if got := request(withoutScope, `{}`); got.Code != http.StatusForbidden {
		t.Fatalf("route should enforce rag scope, got %d", got.Code)
	}
	if got := request(withScope, `not-json`); got.Code != http.StatusBadRequest {
		t.Fatalf("invalid request should return 400, got %d body=%s", got.Code, got.Body.String())
	}
	app.ragSvc = nil
	if got := request(withScope, `{}`); got.Code != http.StatusNotImplemented {
		t.Fatalf("unconfigured service should return 501, got %d", got.Code)
	}
}
