// dashboard_routes_test.go — the requireCap/capSession guards over a real
// session: permitted requests get the capabilities stashed on their context,
// missing capabilities deny pages with the 403 page and partials/htmx with
// an empty 403, and unknown capability names deny.
package httpx

import (
	"net/http"
	"net/http/httptest"
	"strings"
	"testing"
)

func TestRequireCapDeniesAndStashesCaps(t *testing.T) {
	f := newIdentityFixture(t)
	s := f.app.sessionAuth

	chatSession, _ := loginUser(t, f, "caps-chat@example.com", []string{"chatbot"}, "chat-pass")
	ragSession, _ := loginUser(t, f, "caps-rag@example.com", []string{"rag"}, "rag-pass")

	stashed := false
	permitted := s.requireCap(capChat, func(w http.ResponseWriter, r *http.Request) {
		caps, ok := capsFromContext(r.Context())
		if !ok || !caps.Chat || caps.ToolsManage {
			t.Errorf("stashed caps = %+v (ok=%v), want Chat set and ToolsManage clear", caps, ok)
		}
		stashed = true
		w.WriteHeader(http.StatusOK)
	})

	req := httptest.NewRequest(http.MethodGet, "/dashboard/chat", nil)
	req.AddCookie(chatSession)
	rec := httptest.NewRecorder()
	permitted(rec, req)
	if rec.Code != http.StatusOK || !stashed {
		t.Fatalf("permitted page: got %d stashed=%v, want 200 with caps stashed", rec.Code, stashed)
	}

	deniedHandler := func(w http.ResponseWriter, r *http.Request) {
		t.Error("denied handler must not run")
	}
	denied := s.requireCap(capChat, deniedHandler)

	// Page request → the 403 page: status, HTML, no-store, non-empty body.
	req = httptest.NewRequest(http.MethodGet, "/dashboard/chat", nil)
	req.AddCookie(ragSession)
	rec = httptest.NewRecorder()
	denied(rec, req)
	if rec.Code != http.StatusForbidden || rec.Body.Len() == 0 {
		t.Fatalf("denied page: got %d len=%d, want 403 with a body", rec.Code, rec.Body.Len())
	}
	if ct := rec.Header().Get("Content-Type"); !strings.HasPrefix(ct, "text/html") {
		t.Fatalf("denied page Content-Type = %q, want text/html", ct)
	}
	if rec.Header().Get("Cache-Control") == "" {
		t.Fatal("denied page missing no-store Cache-Control")
	}

	// Partial path → empty-body 403.
	req = httptest.NewRequest(http.MethodGet, "/dashboard/partials/chat", nil)
	req.AddCookie(ragSession)
	rec = httptest.NewRecorder()
	denied(rec, req)
	if rec.Code != http.StatusForbidden || rec.Body.Len() != 0 {
		t.Fatalf("denied partial: got %d len=%d, want empty 403", rec.Code, rec.Body.Len())
	}

	// HX-Request → empty-body 403 even on a page path.
	req = httptest.NewRequest(http.MethodGet, "/dashboard/chat", nil)
	req.Header.Set("HX-Request", "true")
	req.AddCookie(ragSession)
	rec = httptest.NewRecorder()
	denied(rec, req)
	if rec.Code != http.StatusForbidden || rec.Body.Len() != 0 {
		t.Fatalf("denied htmx: got %d len=%d, want empty 403", rec.Code, rec.Body.Len())
	}

	// Unknown capability names deny a fully signed-in session.
	unknown := s.requireCap(dashboardCap("nonexistent"), deniedHandler)
	req = httptest.NewRequest(http.MethodGet, "/dashboard/chat", nil)
	req.AddCookie(chatSession)
	rec = httptest.NewRecorder()
	unknown(rec, req)
	if rec.Code != http.StatusForbidden {
		t.Fatalf("unknown cap: got %d, want 403", rec.Code)
	}

	// capSession stashes caps without a capability gate.
	sessionOnlyRan := false
	sessionOnly := s.capSession(func(w http.ResponseWriter, r *http.Request) {
		caps, ok := capsFromContext(r.Context())
		if !ok || !caps.Search || caps.Chat {
			t.Errorf("capSession caps = %+v (ok=%v), want Search set and Chat clear", caps, ok)
		}
		sessionOnlyRan = true
		w.WriteHeader(http.StatusOK)
	})
	req = httptest.NewRequest(http.MethodGet, "/dashboard", nil)
	req.AddCookie(ragSession)
	rec = httptest.NewRecorder()
	sessionOnly(rec, req)
	if rec.Code != http.StatusOK || !sessionOnlyRan {
		t.Fatalf("capSession page: got %d ran=%v, want 200 with caps stashed", rec.Code, sessionOnlyRan)
	}
}
