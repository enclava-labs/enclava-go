package httpx

import (
	"net/http"
	"net/http/httptest"
	"testing"

	"enclava-go/internal/auth"
	"enclava-go/internal/store/postgres"
	"enclava-go/internal/testdb"
)

func TestConnectorSourceSyncSubRoute(t *testing.T) {
	db := testdb.New(t)
	repo := postgres.NewIdentityRepo(db)
	tenant := testdb.SeedTenant(t, db, "bootstrap")
	user := testdb.SeedUser(t, db, tenant, "sync-route@example.com", nil)
	withScope, _ := testdb.SeedAPIKey(t, db, tenant, testdb.SeedKeySpec{Name: "sync-route", OwnerUserID: user.ID, Scopes: []string{"connectors.manage"}})
	authSvc := auth.NewService(repo, "")
	app := NewApp(authSvc, nil, "", nil, repo, nil, nil, nil, nil, nil, nil, nil, nil, "sha256", "", "enc_", nil, nil, 1, false, nil, nil, GateConfig{}, nil, nil)

	request := func(key, method, path string) *httptest.ResponseRecorder {
		req := httptest.NewRequest(method, path, nil)
		if key != "" {
			req.Header.Set("Authorization", "Bearer "+key)
		}
		rec := httptest.NewRecorder()
		app.Handler().ServeHTTP(rec, req)
		return rec
	}

	// Unauthenticated and wrong-scope requests never reach the service.
	if got := request("", http.MethodPost, "/api/v1/connectors/sources/src_1/sync"); got.Code != http.StatusUnauthorized {
		t.Fatalf("sync should require authentication, got %d", got.Code)
	}
	// connectors.manage is the mounted scope; a rag-only key is forbidden.
	ragOnly, _ := testdb.SeedAPIKey(t, db, tenant, testdb.SeedKeySpec{Name: "sync-rag", OwnerUserID: user.ID, Scopes: []string{"rag"}})
	if got := request(ragOnly, http.MethodPost, "/api/v1/connectors/sources/src_1/sync"); got.Code != http.StatusForbidden {
		t.Fatalf("sync should enforce connectors.manage scope, got %d", got.Code)
	}
	// With the service unconfigured the mounted route answers 501, not 404.
	if got := request(withScope, http.MethodPost, "/api/v1/connectors/sources/src_1/sync"); got.Code != http.StatusNotImplemented {
		t.Fatalf("mounted sync route should answer 501 when connectors are unconfigured, got %d body=%s", got.Code, got.Body.String())
	}
	// The nil-service check runs before path routing, so these also answer
	// 501; what must hold is that they never dispatch a sync (202).
	for _, tc := range []struct{ method, path string }{
		{http.MethodGet, "/api/v1/connectors/sources/src_1/sync"},
		{http.MethodPost, "/api/v1/connectors/sources/src_1/other"},
		{http.MethodPost, "/api/v1/connectors/sources/src_1/sync/extra"},
	} {
		if got := request(withScope, tc.method, tc.path); got.Code == http.StatusAccepted {
			t.Fatalf("%s %s must never dispatch a sync", tc.method, tc.path)
		}
	}
}
