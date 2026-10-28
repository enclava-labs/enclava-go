// dashboard_auth_pages_test.go — phase-7 auth/error page contract: the
// re-skinned 403/404 render through dashboardAuthTemplates behind the frozen
// phase-6 signatures, and the login template keeps every form control named
// (the OIDC link step's password fix, OA-12).
package httpx

import (
	"bytes"
	"net/http"
	"net/http/httptest"
	"strings"
	"testing"

	"enclava-go/internal/auth"
)

func TestRenderDashboardForbiddenSkin(t *testing.T) {
	s := &SessionAuth{tenantSlug: "northwind"}
	rec := httptest.NewRecorder()
	s.renderDashboardForbidden(rec, httptest.NewRequest(http.MethodGet, "/dashboard/models", nil))
	res := rec.Result()
	if res.StatusCode != http.StatusForbidden {
		t.Fatalf("expected 403, got %d", res.StatusCode)
	}
	if ct := res.Header.Get("Content-Type"); !strings.HasPrefix(ct, "text/html") {
		t.Fatalf("expected text/html, got %q", ct)
	}
	body := rec.Body.String()
	for _, want := range []string{
		`class="authwrap"`,
		"You don't have access to this page",
		"Northwind",
		`href="/dashboard"`,
	} {
		if !strings.Contains(body, want) {
			t.Errorf("403 body missing %q; got %s", want, body)
		}
	}
}

func TestRenderDashboardNotFoundSkin(t *testing.T) {
	s := &SessionAuth{tenantSlug: "northwind"}
	rec := httptest.NewRecorder()
	s.renderDashboardNotFound(rec, httptest.NewRequest(http.MethodGet, "/dashboard/nope", nil))
	res := rec.Result()
	if res.StatusCode != http.StatusNotFound {
		t.Fatalf("expected 404, got %d", res.StatusCode)
	}
	body := rec.Body.String()
	for _, want := range []string{"authwrap", "Page not found", "Northwind"} {
		if !strings.Contains(body, want) {
			t.Errorf("404 body missing %q; got %s", want, body)
		}
	}
}

// TestLoginTemplateNamedFields pins the link-step fix: both the sign-in and
// the OIDC link confirmation forms submit a named password, errors are
// announced, and provider links carry the view-root return target.
func TestLoginTemplateNamedFields(t *testing.T) {
	set := dashboardAuthTemplates()
	if !dashboardAuthLoaded {
		t.Fatal("auth templates did not parse; dashboard/templates/app/auth/*.tmpl missing")
	}
	render := func(mutate func(*dashboardLoginView)) string {
		t.Helper()
		var buf bytes.Buffer
		view := dashboardLoginView{
			dashboardLoginData: dashboardLoginData{
				ReturnTo: "/dashboard/models",
				Error:    "Invalid email or password",
			},
			Org:   "Northwind",
			Theme: "light",
		}
		if mutate != nil {
			mutate(&view)
		}
		if err := set.ExecuteTemplate(&buf, "login.tmpl", view); err != nil {
			t.Fatalf("login.tmpl did not execute: %v", err)
		}
		return buf.String()
	}

	// Sign-in step: named controls, announced errors, provider links carrying
	// the view-root return target.
	signIn := render(func(v *dashboardLoginView) {
		v.Providers = []auth.OIDCProvider{{ID: "work-sso", Name: "Work SSO"}}
	})
	for _, want := range []string{
		`name="password"`, `name="email"`, `role="alert"`, "Invalid email or password",
		`href="/auth/oidc/work-sso/start?return_to=%2Fdashboard%2Fmodels"`,
		`class="authwrap"`, `class="light"`, "Northwind", `autocomplete="current-password"`,
	} {
		if !strings.Contains(signIn, want) {
			t.Errorf("sign-in render missing %q; got %s", want, signIn)
		}
	}

	// Link step: confirmation form with named credentials; the local sign-in
	// form and its duplicate controls stay hidden.
	link := render(func(v *dashboardLoginView) {
		v.LinkToken = "tok"
		v.LinkProviderName = "Work SSO"
		v.LinkEmail = "maja@northwind.example"
	})
	for _, want := range []string{
		`name="password"`, `name="email"`, `name="link_token"`, `value="tok"`,
		`action="/auth/oidc/link"`, `autocomplete="current-password"`,
	} {
		if !strings.Contains(link, want) {
			t.Errorf("link render missing %q; got %s", want, link)
		}
	}
	if strings.Contains(link, `action="/dashboard/login"`) {
		t.Errorf("link step must not render the local sign-in form; got %s", link)
	}
}
