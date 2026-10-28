// api_write_gates_test.go — the extract-manage rows of the 06-04 write-gate
// audit (OA-05). They live in the owning module because extract imports
// httpx, so package httpx cannot mount these routes without an import cycle.
// Same contract as TestAPIWriteGateMatrix: real mux, real app, every denied
// row exactly 403, plus one unauthenticated 401.
package extractmodule

import (
	"net/http"
	"net/http/httptest"
	"strings"
	"testing"

	"enclava-go/internal/auth"
	"enclava-go/internal/extract"
	"enclava-go/internal/httpx"
	"enclava-go/internal/jobs"
	"enclava-go/internal/store/postgres"
	"enclava-go/internal/testdb"
)

func TestExtractWriteGateMatrix(t *testing.T) {
	db := testdb.New(t)
	identityRepo := postgres.NewIdentityRepo(db)
	tenant := testdb.SeedTenant(t, db, "extract-gates")
	user := testdb.SeedUser(t, db, tenant, "extract-gates@example.com", nil)

	extractRepo := postgres.NewExtractRepo(db)
	jobsRepo := postgres.NewJobsRepo(db)
	svc := extract.NewService(extractRepo, extractRepo, jobsRepo, jobs.NewService(jobsRepo, 0), nil, nil, postgres.NewEndpointsRepo(db), 5)
	app := httpx.NewApp(
		auth.NewService(identityRepo, ""), nil, "extract-gates", nil, identityRepo,
		svc,
		nil, nil, nil, nil,
		nil, nil, postgres.NewEndpointsRepo(db),
		"sha256", "", "enc_", nil, nil,
		5, false, nil, nil, httpx.GateConfig{}, nil, nil,
	)
	New(svc, nil, 5).Register(app.Mux(), app)

	extractOnly, _ := testdb.SeedAPIKey(t, db, tenant, testdb.SeedKeySpec{
		Name: "extract-only", OwnerUserID: user.ID, Scopes: []string{"extract"},
	})
	// Restricted key holding the tpl_x template grant but no extract.manage:
	// HandleTemplateByID checks the grant BEFORE the manage check, so this
	// key proves the grant pass never subsumes the scope check.
	grantedNoManage, _ := testdb.SeedAPIKey(t, db, tenant, testdb.SeedKeySpec{
		Name: "granted-no-manage", OwnerUserID: user.ID, Scopes: []string{"extract"},
		AllowedTemplates: &[]string{"tpl_x"},
	})

	do := func(method, path, body, apiKey string) *httptest.ResponseRecorder {
		var r *http.Request
		if body != "" {
			r = httptest.NewRequest(method, path, strings.NewReader(body))
			r.Header.Set("Content-Type", "application/json")
		} else {
			r = httptest.NewRequest(method, path, nil)
		}
		if apiKey != "" {
			r.Header.Set("X-API-Key", apiKey)
		}
		rec := httptest.NewRecorder()
		app.Handler().ServeHTTP(rec, r)
		return rec
	}

	rows := []struct {
		name, method, path, body, key, gate string
	}{
		// extract.manage inner check in HandleTemplates — outer scope is only
		// "extract", which this key holds.
		{"create template", http.MethodPost, "/api/v1/extract/templates", `{"id":"tpl_new","name":"gate"}`, extractOnly, "extract.manage"},
		{"update template", http.MethodPut, "/api/v1/extract/templates/tpl_x", `{}`, extractOnly, "extract.manage"},
		{"delete template", http.MethodDelete, "/api/v1/extract/templates/tpl_x", ``, extractOnly, "extract.manage"},
		// Same write as above with the grant granted first: still 403 manage.
		{"granted key on template update", http.MethodPut, "/api/v1/extract/templates/tpl_x", `{}`, grantedNoManage, "extract.manage"},
		// Outer scope is extract.manage on reset-defaults — the one denial
		// that fires before the handler body.
		{"reset defaults", http.MethodPost, "/api/v1/extract/templates/reset-defaults", ``, extractOnly, "extract.manage"},
		// extract.manage inner check in HandleSettings.
		{"update settings", http.MethodPut, "/api/v1/extract/settings", `{}`, extractOnly, "extract.manage"},
	}
	for _, tc := range rows {
		rec := do(tc.method, tc.path, tc.body, tc.key)
		if rec.Code != http.StatusForbidden {
			t.Fatalf("%s %s %s (requires %s): expected 403, got %d body=%s",
				tc.name, tc.method, tc.path, tc.gate, rec.Code, rec.Body.String())
		}
		if !strings.Contains(rec.Body.String(), "extract.manage") {
			t.Fatalf("%s: 403 body should name extract.manage, got %q", tc.name, rec.Body.String())
		}
	}

	// No credential: authentication fails before any scope check.
	if rec := do(http.MethodPost, "/api/v1/extract/templates", `{"id":"tpl_new"}`, ""); rec.Code != http.StatusUnauthorized {
		t.Fatalf("unauthenticated extract write expected 401, got %d body=%s", rec.Code, rec.Body.String())
	}
}
