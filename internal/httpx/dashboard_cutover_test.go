// dashboard_cutover_test.go — 09-10 cutover matrix (D-19): every dashboard
// URL phase 9 owned is gone — no rendered page, no redirect, no alias — while
// the new self-registered route sets and the auth/static shell stay live.
// Dead URLs are asserted for all three roles: scope gates die WITH the
// registration, so a deleted URL 404s for admin, manager and member alike.

package httpx

import (
	"net/http"
	"testing"

	"enclava-go/internal/auth"
)

func TestDashboardCutover(t *testing.T) {
	f := newIdentityFixture(t)

	memberSess, memberCSRF := loginUser(t, f, "cutover-member@example.com", []string{"chatbot"}, "member-pass")
	managerSess, managerCSRF := loginUser(t, f, "cutover-manager@example.com", []string{"pricing.manage", "connectors.manage", "rag", "rag.manage", "extract"}, "manager-pass")
	adminSess, adminCSRF := loginUser(t, f, "cutover-admin@example.com", []string{auth.ScopePlatformAdmin}, "admin-pass")
	roles := []struct {
		name          string
		session, csrf *http.Cookie
	}{
		{"member", memberSess, memberCSRF},
		{"manager", managerSess, managerCSRF},
		{"admin", adminSess, adminCSRF},
	}

	// Dead set: every pre-phase-9 page + partial route. The extract and
	// billing URLs died in 09-04/09-08's cutovers and the rag/chatbots/
	// prompt-templates/stats family died with phase-10's Home cutover —
	// all re-asserted here. POSTs carry CSRF so a 404 is never a masked
	// 403; sessionForm only attaches the token for mutations.
	dead := []struct{ method, path string }{
		{http.MethodGet, "/dashboard/documents"},
		{http.MethodGet, "/dashboard/keys"},
		{http.MethodGet, "/dashboard/partials/keys"},
		{http.MethodPost, "/dashboard/partials/keys"},
		{http.MethodGet, "/dashboard/identity"},
		{http.MethodGet, "/dashboard/providers"},
		{http.MethodGet, "/dashboard/partials/providers"},
		{http.MethodGet, "/dashboard/pricing"},
		{http.MethodGet, "/dashboard/partials/pricing"},
		{http.MethodGet, "/dashboard/connectors"},
		{http.MethodGet, "/dashboard/partials/connectors"},
		{http.MethodGet, "/dashboard/mcp"},
		{http.MethodGet, "/dashboard/partials/mcp"},
		{http.MethodGet, "/dashboard/partials/extract"},
		{http.MethodGet, "/dashboard/partials/extract/proxy"},
		{http.MethodGet, "/dashboard/analytics"},
		{http.MethodGet, "/dashboard/budgets"},
		{http.MethodGet, "/dashboard/billing-audit"},
		{http.MethodGet, "/dashboard/partials/usage"},
		{http.MethodGet, "/dashboard/partials/budgets"},
		{http.MethodGet, "/dashboard/partials/billing-audit"},
		{http.MethodGet, "/dashboard/partials/stats"},
		{http.MethodGet, "/dashboard/rag"},
		{http.MethodGet, "/dashboard/partials/rag"},
		{http.MethodGet, "/dashboard/rag/documents/doc-1/download"},
		{http.MethodGet, "/dashboard/chatbots"},
		{http.MethodGet, "/dashboard/partials/chatbots"},
		{http.MethodGet, "/dashboard/prompt-templates"},
		{http.MethodGet, "/dashboard/partials/prompt-templates"},
	}
	for _, tc := range dead {
		for _, role := range roles {
			rec := sessionForm(t, f.app, tc.method, tc.path, nil, role.session, role.csrf)
			if rec.Code != http.StatusNotFound {
				t.Fatalf("%s %s %s: expected the designed 404, got %d body=%s",
					role.name, tc.method, tc.path, rec.Code, rec.Body.String())
			}
		}
	}

	// Retained shell entries: session-required GETs serve 200 for any
	// signed-in role; static + login stay reachable.
	retained := []struct {
		method, path string
		want         int
	}{
		{http.MethodGet, "/dashboard", http.StatusOK},
		{http.MethodGet, "/dashboard/", http.StatusOK},
		{http.MethodGet, "/dashboard/partials/home/running", http.StatusOK},
	}
	for _, tc := range retained {
		for _, role := range roles {
			rec := sessionForm(t, f.app, tc.method, tc.path, nil, role.session, role.csrf)
			if rec.Code != tc.want {
				t.Fatalf("%s %s %s: expected %d, got %d body=%s",
					role.name, tc.method, tc.path, tc.want, rec.Code, rec.Body.String())
			}
		}
	}
	// Login stays public: anonymous GET renders 200; an already-authenticated
	// GET takes the convenience redirect to Home (sessionauth.go:81).
	anonRec := sessionForm(t, f.app, http.MethodGet, "/dashboard/login", nil, nil, nil)
	if anonRec.Code != http.StatusOK {
		t.Fatalf("anonymous GET /dashboard/login: expected 200, got %d", anonRec.Code)
	}
	sessRec := sessionForm(t, f.app, http.MethodGet, "/dashboard/login", nil, memberSess, nil)
	if sessRec.Code != http.StatusSeeOther || sessRec.Result().Header.Get("Location") != "/dashboard" {
		t.Fatalf("authenticated GET /dashboard/login: expected 303 to /dashboard, got %d %q", sessRec.Code, sessRec.Result().Header.Get("Location"))
	}
	for _, path := range []string{"/dashboard/static/app.css", "/dashboard/static/htmx.min.js"} {
		rec := sessionForm(t, f.app, http.MethodGet, path, nil, memberSess, nil)
		if rec.Code != http.StatusOK {
			t.Fatalf("GET %s: expected 200, got %d", path, rec.Code)
		}
	}

	// New route sets: member is refused every capability-gated surface but
	// keeps Home and Account; admin passes the platform.admin-gated ones
	// only — pricing.manage and connectors.manage stay exact.
	liveMember := []string{"/dashboard", "/dashboard/account", "/dashboard/chat"}
	for _, path := range liveMember {
		rec := sessionForm(t, f.app, http.MethodGet, path, nil, memberSess, memberCSRF)
		if rec.Code == http.StatusForbidden {
			t.Fatalf("member %s: unexpected 403", path)
		}
	}
	memberDenied := []string{
		"/dashboard/search", "/dashboard/extract", "/dashboard/knowledge", "/dashboard/sources",
		"/dashboard/assistants", "/dashboard/tools", "/dashboard/models", "/dashboard/access", "/dashboard/usage",
	}
	for _, path := range memberDenied {
		rec := sessionForm(t, f.app, http.MethodGet, path, nil, memberSess, memberCSRF)
		if rec.Code != http.StatusForbidden {
			t.Fatalf("member %s: expected 403, got %d", path, rec.Code)
		}
	}
	adminOK := []string{"/dashboard/tools", "/dashboard/access", "/dashboard/usage"}
	for _, path := range adminOK {
		rec := sessionForm(t, f.app, http.MethodGet, path, nil, adminSess, adminCSRF)
		if rec.Code == http.StatusForbidden {
			t.Fatalf("admin %s: unexpected 403", path)
		}
	}
	for _, path := range []string{"/dashboard/models", "/dashboard/sources"} {
		rec := sessionForm(t, f.app, http.MethodGet, path, nil, adminSess, adminCSRF)
		if rec.Code != http.StatusForbidden {
			t.Fatalf("admin without manage scope %s: expected 403, got %d", path, rec.Code)
		}
	}
	managerOK := []string{"/dashboard/models", "/dashboard/sources", "/dashboard/knowledge", "/dashboard/search", "/dashboard/extract"}
	for _, path := range managerOK {
		rec := sessionForm(t, f.app, http.MethodGet, path, nil, managerSess, managerCSRF)
		if rec.Code == http.StatusForbidden {
			t.Fatalf("manager %s: unexpected 403", path)
		}
	}
}
