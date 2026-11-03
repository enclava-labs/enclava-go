package httpx

import (
	"context"
	"encoding/json"
	"net/http"
	"net/http/httptest"
	"net/url"
	"strings"
	"testing"

	"enclava-go/internal/auth"
	"enclava-go/internal/store/postgres"
	"enclava-go/internal/testdb"
)

type securityAuditTestRecorder struct {
	rows   []postgres.SecurityAuditRow
	tenant string
	before string
	limit  int
}

func (s *securityAuditTestRecorder) Insert(_ context.Context, row postgres.SecurityAuditRow) error {
	s.rows = append(s.rows, row)
	return nil
}

func (s *securityAuditTestRecorder) List(_ context.Context, tenant string, limit int, before string) ([]postgres.SecurityAuditRow, error) {
	s.tenant, s.limit, s.before = tenant, limit, before
	start := 0
	if before != "" {
		for start < len(s.rows) && s.rows[start].ID != before {
			start++
		}
		if start < len(s.rows) {
			start++
		}
	}
	end := start + limit
	if end > len(s.rows) {
		end = len(s.rows)
	}
	return s.rows[start:end], nil
}

func TestSecurityAuditHandlerUsesPrincipalAndCursor(t *testing.T) {
	recorder := &securityAuditTestRecorder{rows: []postgres.SecurityAuditRow{{ID: "new"}, {ID: "middle"}, {ID: "old"}}}
	app := &App{}
	app.SetSecurityAudit(recorder)
	principal := auth.Principal{TenantID: "tenant-1"}
	for _, tc := range []struct{ query, want string }{{"?limit=1", "new"}, {"?limit=1&before=new", "middle"}} {
		w := httptest.NewRecorder()
		app.handleSecurityAudit(w, httptest.NewRequest(http.MethodGet, "/api/v1/admin/security-audit"+tc.query, nil), principal)
		if w.Code != http.StatusOK {
			t.Fatalf("status = %d: %s", w.Code, w.Body.String())
		}
		var body struct {
			Data []postgres.SecurityAuditRow `json:"data"`
		}
		if err := json.Unmarshal(w.Body.Bytes(), &body); err != nil || len(body.Data) != 1 || body.Data[0].ID != tc.want {
			t.Fatalf("response = %s, err = %v", w.Body.String(), err)
		}
		if recorder.tenant != principal.TenantID || recorder.limit != 1 {
			t.Fatalf("tenant/limit = %q/%d", recorder.tenant, recorder.limit)
		}
	}
	if recorder.before != "new" {
		t.Fatalf("before = %q", recorder.before)
	}
	w := httptest.NewRecorder()
	app.handleSecurityAudit(w, httptest.NewRequest(http.MethodGet, "/api/v1/admin/security-audit", nil), auth.Principal{})
	if w.Code != http.StatusForbidden {
		t.Fatalf("missing principal tenant: status = %d", w.Code)
	}
	w = httptest.NewRecorder()
	app.handleSecurityAudit(w, httptest.NewRequest(http.MethodGet, "/api/v1/admin/security-audit?limit=201", nil), principal)
	if w.Code != http.StatusBadRequest {
		t.Fatalf("oversized limit: status = %d", w.Code)
	}
}

func TestSecurityAuditAPIKeyLifecycle(t *testing.T) {
	f := newIdentityFixture(t)
	repo := postgres.NewSecurityAuditRepo(f.db)
	f.app.SetSecurityAudit(repo)
	session, csrf := loginUserWithGrants(t, f, "audit-admin@example.com",
		[]string{auth.ScopePlatformAdmin}, auth.ResourceGrants{}, "audit-password")
	rec := sessionDo(t, f.app, http.MethodPost, "/api/v1/admin/api-keys",
		`{"name":"audit-key","scopes":[],"allowed_models":[],"allowed_chatbots":[],"allowed_agents":[],"allowed_endpoints":[],"allowed_extract_templates":[]}`, session, csrf)
	if rec.Code != http.StatusCreated {
		t.Fatalf("create: %d %s", rec.Code, rec.Body.String())
	}
	rows, err := repo.List(context.Background(), f.tenant, 200, "")
	if err != nil || len(rows) != 1 || rows[0].Action != "apikey.created" || rows[0].TargetID == "" {
		t.Fatalf("creation audit: %+v, %v", rows, err)
	}
	id := rows[0].TargetID
	rec = sessionDo(t, f.app, http.MethodDelete, "/api/v1/admin/api-keys/"+id, "", session, csrf)
	if rec.Code != http.StatusOK {
		t.Fatalf("revoke: %d %s", rec.Code, rec.Body.String())
	}
	rows, err = repo.List(context.Background(), f.tenant, 200, "")
	if err != nil || len(rows) != 2 || rows[0].Action != "apikey.revoked" || rows[0].TargetID != id {
		t.Fatalf("revocation audit: %+v, %v", rows, err)
	}
}

func TestLoginAuditAndThrottling(t *testing.T) {
	f := newIdentityFixture(t)
	repo := postgres.NewSecurityAuditRepo(f.db)
	f.app.sessionAuth.SetSecurityAudit(repo)
	user := testdb.SeedUser(t, f.db, f.tenant, "audit-login@example.com", nil)
	setPassword(t, f.repo, f.tenant, user.ID, "correct-password")
	post := func(email, password string) *httptest.ResponseRecorder {
		form := url.Values{"email": {email}, "password": {password}}
		r := httptest.NewRequest(http.MethodPost, "/dashboard/login", strings.NewReader(form.Encode()))
		r.Header.Set("Content-Type", "application/x-www-form-urlencoded")
		w := httptest.NewRecorder()
		f.app.Handler().ServeHTTP(w, r)
		return w
	}
	if rec := post("audit-login@example.com", "correct-password"); rec.Code != http.StatusSeeOther {
		t.Fatalf("successful login: %d %s", rec.Code, rec.Body.String())
	}
	for i := range 5 {
		if rec := post("missing@example.com", "wrong"); rec.Code == http.StatusTooManyRequests {
			t.Fatalf("attempt %d throttled early", i+1)
		}
	}
	if rec := post("missing@example.com", "wrong"); rec.Code != http.StatusTooManyRequests || !strings.Contains(rec.Body.String(), "Too many sign-in attempts. Try again in") {
		t.Fatalf("sixth failed login: %d %s", rec.Code, rec.Body.String())
	}
	rows, err := repo.List(context.Background(), f.tenant, 100, "")
	if err != nil {
		t.Fatal(err)
	}
	counts := map[string]int{}
	for _, row := range rows {
		counts[row.Action]++
	}
	if counts["auth.login.succeeded"] != 1 || counts["auth.login.failed"] != 5 || counts["auth.login.throttled"] != 1 {
		t.Fatalf("login audits: %v", counts)
	}
}
