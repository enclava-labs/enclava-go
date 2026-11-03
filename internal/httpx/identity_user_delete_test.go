package httpx

import (
	"context"
	"net/http"
	"net/http/httptest"
	"net/url"
	"strings"
	"testing"

	"enclava-go/internal/auth"
	"enclava-go/internal/store/postgres"
	"enclava-go/internal/testdb"
)

func TestIdentityUserDeleteRevokesCredentialsAndPreservesHistory(t *testing.T) {
	f := newIdentityFixture(t)
	ctx := context.Background()
	audit := postgres.NewSecurityAuditRepo(f.db)
	f.app.SetSecurityAudit(audit)
	user := testdb.SeedUser(t, f.db, f.tenant, "delete@example.com", []string{"chatbot"})
	setPassword(t, f.repo, f.tenant, user.ID, "delete-pass")
	session, _ := login(t, f.app, "delete@example.com", "delete-pass")
	key, keyRecord := testdb.SeedAPIKey(t, f.db, f.tenant, testdb.SeedKeySpec{
		Name: "delete-owned", OwnerUserID: user.ID, Scopes: []string{"chatbot"},
	})

	rec := sessionDo(t, f.app, http.MethodGet, "/api/v1/identities/me", "", session, nil)
	if rec.Code != http.StatusOK {
		t.Fatalf("session before delete: %d %s", rec.Code, rec.Body.String())
	}
	rec = f.do(t, http.MethodGet, "/api/v1/identities/me", "", map[string]string{"X-API-Key": key})
	if rec.Code != http.StatusOK {
		t.Fatalf("key before delete: %d %s", rec.Code, rec.Body.String())
	}

	rec = f.admin(t, http.MethodDelete, "/api/v1/identities/users/"+user.ID, "")
	if rec.Code != http.StatusOK {
		t.Fatalf("delete user: %d %s", rec.Code, rec.Body.String())
	}
	if body := decodeJSON(t, rec); body["deleted"] != true || body["id"] != user.ID {
		t.Fatalf("unexpected delete response: %v", body)
	}
	rec = sessionDo(t, f.app, http.MethodGet, "/api/v1/identities/me", "", session, nil)
	if rec.Code != http.StatusUnauthorized {
		t.Fatalf("deleted user's session: expected 401, got %d %s", rec.Code, rec.Body.String())
	}
	rec = f.do(t, http.MethodGet, "/api/v1/identities/me", "", map[string]string{"X-API-Key": key})
	if rec.Code != http.StatusUnauthorized {
		t.Fatalf("deleted user's key: expected 401, got %d %s", rec.Code, rec.Body.String())
	}
	rec = f.admin(t, http.MethodGet, "/api/v1/identities/users", "")
	if rec.Code != http.StatusOK {
		t.Fatalf("list users: %d %s", rec.Code, rec.Body.String())
	}
	for _, item := range decodeJSON(t, rec)["users"].([]any) {
		if item.(map[string]any)["id"] == user.ID {
			t.Fatalf("deleted user appears in list: %s", rec.Body.String())
		}
	}
	rec = f.admin(t, http.MethodGet, "/api/v1/identities/users/"+user.ID, "")
	if rec.Code != http.StatusNotFound {
		t.Fatalf("deleted user lookup: expected 404, got %d %s", rec.Code, rec.Body.String())
	}

	form := url.Values{"email": {"delete@example.com"}, "password": {"delete-pass"}}
	req := httptest.NewRequest(http.MethodPost, "/dashboard/login", strings.NewReader(form.Encode()))
	req.Header.Set("Content-Type", "application/x-www-form-urlencoded")
	rec = httptest.NewRecorder()
	f.app.Handler().ServeHTTP(rec, req)
	if rec.Code != http.StatusOK || !strings.Contains(rec.Body.String(), "Invalid email or password") {
		t.Fatalf("deleted user login: %d %s", rec.Code, rec.Body.String())
	}
	for _, cookie := range rec.Result().Cookies() {
		if cookie.Name == sessionCookieName {
			t.Fatal("deleted user login issued a session")
		}
	}
	rec = f.admin(t, http.MethodPost, "/api/v1/identities/users", `{"email":"delete@example.com","password":"new-pass-123"}`)
	if rec.Code != http.StatusCreated {
		t.Fatalf("reuse deleted email: %d %s", rec.Code, rec.Body.String())
	}
	if decodeJSON(t, rec)["id"] == user.ID {
		t.Fatal("reused email returned the deleted identity")
	}

	var sessions, keys int
	err := f.db.SQL.QueryRowContext(ctx, `SELECT
		(SELECT count(*) FROM sessions s JOIN users u ON u.tenant_id = s.tenant_id AND u.id = s.user_id WHERE s.tenant_id = $1::uuid AND s.user_id = $2),
		(SELECT count(*) FROM api_keys k JOIN users u ON u.tenant_id = k.tenant_id AND u.id = k.owner_user_id WHERE k.tenant_id = $1::uuid AND k.id = $3 AND k.owner_user_id = $2)`,
		f.tenant, user.ID, keyRecord.ID).Scan(&sessions, &keys)
	if err != nil || sessions != 1 || keys != 1 {
		t.Fatalf("historical credentials must still resolve to original user: sessions=%d keys=%d err=%v", sessions, keys, err)
	}
	rows, err := audit.List(ctx, f.tenant, 100, "")
	if err != nil {
		t.Fatal(err)
	}
	found := false
	for _, row := range rows {
		if row.Action == "identity.user.deleted" && row.TargetID == user.ID && row.TenantID == f.tenant && row.Outcome == "success" && row.ActorID != "" {
			found = true
		}
	}
	if !found {
		t.Fatalf("missing user deletion audit event: %+v", rows)
	}
}

func TestIdentityUserDeleteGuards(t *testing.T) {
	f := newIdentityFixture(t)
	ctx := context.Background()
	users, err := f.repo.ListUsers(ctx, f.tenant)
	if err != nil || len(users) != 1 {
		t.Fatalf("fixture users: %v %v", users, err)
	}
	admin := users[0]
	setPassword(t, f.repo, f.tenant, admin.ID, "admin-pass")
	session, csrf := login(t, f.app, admin.Email, "admin-pass")
	for _, rec := range []*httptest.ResponseRecorder{
		f.admin(t, http.MethodDelete, "/api/v1/identities/users/"+admin.ID, ""),
		sessionDo(t, f.app, http.MethodDelete, "/api/v1/identities/users/"+admin.ID, "", session, csrf),
	} {
		if rec.Code != http.StatusConflict || errorCode(t, rec.Body.String()) != "self_delete" {
			t.Fatalf("self delete: %d %s", rec.Code, rec.Body.String())
		}
	}
	other := testdb.SeedTenant(t, f.db, "other-delete")
	foreign := testdb.SeedUser(t, f.db, other, "foreign-delete@example.com", nil)
	rec := f.admin(t, http.MethodDelete, "/api/v1/identities/users/"+foreign.ID, "")
	if rec.Code != http.StatusNotFound {
		t.Fatalf("foreign delete: expected 404, got %d %s", rec.Code, rec.Body.String())
	}
	if _, err := f.repo.GetUser(ctx, other, foreign.ID); err != nil {
		t.Fatalf("foreign user changed: %v", err)
	}

	// A live admin caller is itself a surviving holder; invoke the HTTP handler
	// with a pre-authorized principal to exercise the last-holder guard directly.
	revokeAdminKeys(t, f)
	last := testdb.SeedUser(t, f.db, f.tenant, "last-delete@example.com", []string{auth.ScopePlatformAdmin})
	req := httptest.NewRequest(http.MethodDelete, "/api/v1/identities/users/"+last.ID, nil)
	rec = httptest.NewRecorder()
	f.app.handleIdentityUserByID(rec, req, auth.Principal{TenantID: f.tenant, UserID: admin.ID})
	if rec.Code != http.StatusConflict || errorCode(t, rec.Body.String()) != "last_admin" {
		t.Fatalf("last admin delete: %d %s", rec.Code, rec.Body.String())
	}
	if _, err := f.repo.GetUser(ctx, f.tenant, last.ID); err != nil {
		t.Fatalf("last admin was removed: %v", err)
	}
}
