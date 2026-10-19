package httpx

import (
	"net/http"
	"net/http/httptest"
	"testing"
)

func TestRootRedirectsToDashboard(t *testing.T) {
	app := NewApp(nil, nil, "", nil, nil, nil, nil, nil, nil, nil, nil, nil, nil, nil, nil, nil, nil, "", "", "", nil, "", "", 1, false)

	req := httptest.NewRequest(http.MethodGet, "/", nil)
	rec := httptest.NewRecorder()

	app.Handler().ServeHTTP(rec, req)

	if rec.Code != http.StatusSeeOther {
		t.Fatalf("expected status 303, got %d", rec.Code)
	}
	if got := rec.Header().Get("Location"); got != "/dashboard" {
		t.Fatalf("expected redirect to /dashboard, got %q", got)
	}
}

func TestRootCatchAllStillReturnsNotFound(t *testing.T) {
	app := NewApp(nil, nil, "", nil, nil, nil, nil, nil, nil, nil, nil, nil, nil, nil, nil, nil, nil, "", "", "", nil, "", "", 1, false)

	req := httptest.NewRequest(http.MethodGet, "/missing", nil)
	rec := httptest.NewRecorder()

	app.Handler().ServeHTTP(rec, req)

	if rec.Code != http.StatusNotFound {
		t.Fatalf("expected status 404, got %d", rec.Code)
	}
}
