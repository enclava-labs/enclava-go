// dashboard_gallery_test.go — the dev gallery is the phase tracer: the gate
// is proven through the real mux with real sessions (P7-14), and the page
// renders every UI-SPEC component through the real dashboardTemplateSet.
package httpx

import (
	"bytes"
	"net/http"
	"net/http/httptest"
	"net/url"
	"strings"
	"testing"

	"enclava-go/internal/auth"
)

// gallerySession logs the seeded user in through the real login route and
// returns the session cookie.
func gallerySession(t *testing.T, app *App) *http.Cookie {
	t.Helper()
	form := url.Values{"email": {"gallery@example.com"}, "password": {"correct-horse"}}
	req := httptest.NewRequest(http.MethodPost, "/dashboard/login", strings.NewReader(form.Encode()))
	req.Header.Set("Content-Type", "application/x-www-form-urlencoded")
	rec := httptest.NewRecorder()
	app.Handler().ServeHTTP(rec, req)
	if rec.Code != http.StatusSeeOther {
		t.Fatalf("login expected 303, got %d body=%s", rec.Code, rec.Body.String())
	}
	return sessionCookie(t, rec, sessionCookieName)
}

func TestGalleryGate(t *testing.T) {
	t.Run("on", func(t *testing.T) {
		t.Setenv("ENCLAVA_DEV_GALLERY", "1")
		app, repo, tenant := newSessionApp(t)
		seedLoginUser(t, repo, tenant, "gallery@example.com", "correct-horse")

		// No session → the guard redirects to login with a return_to.
		req := httptest.NewRequest(http.MethodGet, "/dashboard/dev/gallery", nil)
		rec := httptest.NewRecorder()
		app.Handler().ServeHTTP(rec, req)
		if rec.Code != http.StatusSeeOther ||
			rec.Header().Get("Location") != "/dashboard/login?return_to=%2Fdashboard%2Fdev%2Fgallery" {
			t.Fatalf("anonymous gallery = %d %q, want 303 to login", rec.Code, rec.Header().Get("Location"))
		}

		session := gallerySession(t, app)

		// Session → the full shell page through the real renderer.
		req = httptest.NewRequest(http.MethodGet, "/dashboard/dev/gallery", nil)
		req.AddCookie(session)
		rec = httptest.NewRecorder()
		app.Handler().ServeHTTP(rec, req)
		if rec.Code != http.StatusOK {
			t.Fatalf("gallery expected 200, got %d body=%s", rec.Code, rec.Body.String())
		}
		if ct := rec.Header().Get("Content-Type"); !strings.HasPrefix(ct, "text/html") {
			t.Fatalf("gallery content type = %q", ct)
		}
		body := rec.Body.String()
		for _, want := range []string{
			`href="/dashboard/static/app.css"`, // real shell served
			`<h1>Component gallery</h1>`,
			`data-action="theme"`,
			`class="thread-layout"`,
			`class="tablewrap"`,
		} {
			if !strings.Contains(body, want) {
				t.Errorf("gallery body missing %q", want)
			}
		}

		// The drawer fragment serves drawer markup, not a second shell.
		req = httptest.NewRequest(http.MethodGet, "/dashboard/dev/gallery/drawer", nil)
		req.AddCookie(session)
		rec = httptest.NewRecorder()
		app.Handler().ServeHTTP(rec, req)
		if rec.Code != http.StatusOK {
			t.Fatalf("gallery drawer expected 200, got %d", rec.Code)
		}
		body = rec.Body.String()
		for _, want := range []string{`class="drawer-head"`, `class="stepper"`, `class="tilegrid"`, `class="drawer-actions"`} {
			if !strings.Contains(body, want) {
				t.Errorf("drawer fragment missing %q", want)
			}
		}
		if strings.Contains(body, `id="sidebar"`) {
			t.Errorf("drawer fragment rendered a full shell")
		}

		// Read-only showcase: a write method is a bare 405, never a render.
		req = httptest.NewRequest(http.MethodPost, "/dashboard/dev/gallery", strings.NewReader("x"))
		req.AddCookie(session)
		rec = httptest.NewRecorder()
		app.Handler().ServeHTTP(rec, req)
		if rec.Code != http.StatusMethodNotAllowed {
			t.Fatalf("POST gallery = %d, want 405", rec.Code)
		}
	})

	t.Run("off", func(t *testing.T) {
		t.Setenv("ENCLAVA_DEV_GALLERY", "")
		app, repo, tenant := newSessionApp(t)
		seedLoginUser(t, repo, tenant, "gallery@example.com", "correct-horse")
		session := gallerySession(t, app)

		// Signed in, gate off → the route does not exist; the phase-6
		// catch-all owns the 404. Never a shipped page.
		for _, path := range []string{"/dashboard/dev/gallery", "/dashboard/dev/gallery/drawer"} {
			req := httptest.NewRequest(http.MethodGet, path, nil)
			req.AddCookie(session)
			rec := httptest.NewRecorder()
			app.Handler().ServeHTTP(rec, req)
			if rec.Code != http.StatusNotFound {
				t.Errorf("%s with gate off = %d, want 404", path, rec.Code)
			}
		}
	})
}

// TestGalleryTemplate renders the page through dashboardTemplateSet — the
// same cached set renderDashboard consumes — and walks the component
// inventory in the output.
func TestGalleryTemplate(t *testing.T) {
	set, err := dashboardTemplateSet("gallery")
	if err != nil {
		t.Fatalf("gallery template set: %v", err)
	}
	app := &App{}
	req := httptest.NewRequest(http.MethodGet, "/dashboard/dev/gallery", nil)
	ctx := auth.WithPrincipal(req.Context(), auth.Principal{TenantID: "tenant-1", UserID: "user-1"})
	ctx = withCaps(ctx, dashboardCaps{Chat: true, Search: true})
	rec := httptest.NewRecorder()
	app.renderDashboardInto(rec, req.WithContext(ctx), set, "page", "gallery", newGalleryData())
	if rec.Code != http.StatusOK {
		t.Fatalf("gallery render = %d body=%s", rec.Code, rec.Body.String())
	}
	body := rec.Body.String()

	for _, want := range []string{ // inventory markers, in template order
		`class="head"`, `class="crumb"`, `class="right-mobile"`, `class="tabs"`,
		`class="chip active"`, `aria-pressed="true"`,
		`class="field-error"`, `class="help"`, `class="label"`,
		`class="pill good"`, `class="pill warn"`, `class="pill bad"`, `class="pill neutral"`,
		`class="pill accent"`, `class="pill blue"`, `class="dot pulse"`,
		`class="progress"`, `style="width:42%"`,
		`class="grid four"`, `class="kpi-value"`, `class="spark"`, `class="bar"`,
		`class="card-title"`, `class="attention"`, `class="activity-row"`, `class="result-snippet"`,
		`<mark>wind</mark>`,
		`class="tablewrap"`, `scope="col"`, `data-label="Collection"`, `class="subline"`,
		`class="stack"`, `class="empty"`,
		`data-action="open-drawer"`, `data-drawer-url="/dashboard/dev/gallery/drawer"`,
		`data-action="conf-pop"`, `data-action="user-menu"`,
		`class="mobile-chats-list"`,
		`class="history-item selected"`, `class="bubble"`, `class="toolstep"`, `class="runline"`,
		`class="composer"`, `class="citation"`, `class="quote"`, `class="evidence-panel"`,
	} {
		if !strings.Contains(body, want) {
			t.Errorf("gallery output missing %q", want)
		}
	}
	// Forbidden markup: the gallery's own content is static shell markup
	// only (D-20) — scan the content define alone, since the layout head
	// legitimately links deferred scripts.
	var contentOnly bytes.Buffer
	if err := set.ExecuteTemplate(&contentOnly, "content", app.buildDashboardView(req.WithContext(ctx), "gallery", newGalleryData())); err != nil {
		t.Fatalf("content-only execute: %v", err)
	}
	for _, banned := range []string{"<script", "hx-on", "onclick=", "oninput=", "Northwind", "Maja"} {
		if strings.Contains(contentOnly.String(), banned) {
			t.Errorf("gallery output contains banned %q", banned)
		}
	}
	// Every td carries a data-label equal to its column header.
	if tds := strings.Count(contentOnly.String(), "<td "); tds != strings.Count(contentOnly.String(), "<td data-label=") {
		t.Errorf("tds = %d, with data-label = %d", tds, strings.Count(contentOnly.String(), "<td data-label="))
	}

	// The drawer fragment renders through the same set.
	rec = httptest.NewRecorder()
	app.renderDashboardInto(rec, req.WithContext(ctx), set, "frag-drawer", "gallery", newGalleryData())
	if rec.Code != http.StatusOK {
		t.Fatalf("drawer fragment render = %d", rec.Code)
	}
	for _, want := range []string{`class="drawer-head"`, `class="step active"`, `class="tile selected"`, `class="drawer-actions"`} {
		if !strings.Contains(rec.Body.String(), want) {
			t.Errorf("drawer fragment missing %q", want)
		}
	}
}
