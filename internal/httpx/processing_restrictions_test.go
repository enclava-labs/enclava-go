package httpx

import (
	"context"
	"net/http"
	"net/http/httptest"
	"strings"
	"testing"

	"enclava-go/internal/auth"
	"enclava-go/internal/source"
	"enclava-go/internal/store/postgres"
	"enclava-go/internal/testdb"
)

func TestProcessingRestrictionTenantPersistsAndCannotTargetForeignSubject(t *testing.T) {
	db := testdb.New(t)
	tenant := testdb.SeedTenant(t, db, "restriction-policy")
	app := &App{sourceRepo: postgres.NewSourceRepo(db)}
	principal := auth.Principal{TenantID: tenant}
	request := func(method, body string) *httptest.ResponseRecorder {
		req := httptest.NewRequest(method, "/api/v1/processing-restrictions", strings.NewReader(body))
		req = req.WithContext(auth.WithPrincipal(req.Context(), principal))
		rec := httptest.NewRecorder()
		app.handleProcessingRestrictions(rec, req, principal)
		return rec
	}
	body := `{"subject_kind":"tenant","subject_id":"` + tenant + `","required_assurance":"high","denied_capabilities":["chat/completions"]}`
	if rec := request(http.MethodPut, body); rec.Code != http.StatusOK {
		t.Fatalf("save tenant restriction: %d %s", rec.Code, rec.Body.String())
	}
	get := httptest.NewRequest(http.MethodGet, "/api/v1/processing-restrictions?subject_kind=tenant&subject_id="+tenant, nil)
	get = get.WithContext(auth.WithPrincipal(get.Context(), principal))
	got := httptest.NewRecorder()
	app.handleProcessingRestrictions(got, get, principal)
	if got.Code != http.StatusOK || !strings.Contains(got.Body.String(), "chat/completions") || !strings.Contains(got.Body.String(), "high") {
		t.Fatalf("read saved tenant restriction: %d %s", got.Code, got.Body.String())
	}
	foreignTenant := testdb.SeedTenant(t, db, "restriction-foreign")
	if _, err := app.sourceRepo.UpsertObject(context.Background(), source.Object{
		TenantID: foreignTenant, ID: "foreign-object", Kind: "upload", NativeID: "native", OwnerActorID: "foreign-actor",
	}); err != nil {
		t.Fatalf("seed foreign object: %v", err)
	}
	foreign := httptest.NewRequest(http.MethodPut, "/api/v1/processing-restrictions", strings.NewReader(`{"subject_kind":"object","subject_id":"foreign-object","denied_capabilities":["chat/completions"]}`))
	foreign = foreign.WithContext(auth.WithPrincipal(foreign.Context(), principal))
	foreignRec := httptest.NewRecorder()
	app.handleProcessingRestrictions(foreignRec, foreign, principal)
	if foreignRec.Code != http.StatusNotFound {
		t.Fatalf("foreign object outcome = %d %s", foreignRec.Code, foreignRec.Body.String())
	}
}

func TestSavedTenantRestrictionDeniesSubsequentInference(t *testing.T) {
	fx := newOpenAIContractFixture(t)
	fx.app.sourceRepo = postgres.NewSourceRepo(fx.db)
	principal := auth.Principal{TenantID: fx.tenant}
	body := `{"subject_kind":"tenant","subject_id":"` + fx.tenant + `","required_assurance":"high"}`
	req := httptest.NewRequest(http.MethodPut, "/api/v1/processing-restrictions", strings.NewReader(body))
	req = req.WithContext(auth.WithPrincipal(req.Context(), principal))
	rec := httptest.NewRecorder()
	fx.app.handleProcessingRestrictions(rec, req, principal)
	if rec.Code != http.StatusOK {
		t.Fatalf("save policy: %d %s", rec.Code, rec.Body.String())
	}
	response := fx.do(t, http.MethodPost, "/v1/chat/completions", `{"model":"gpt-fast","messages":[{"role":"user","content":"must not dispatch"}]}`)
	if response.Code != http.StatusForbidden {
		t.Fatalf("expected persisted policy denial, got %d %s", response.Code, response.Body.String())
	}
	if fx.up.chatCalls != 0 {
		t.Fatalf("denied request reached upstream %d times", fx.up.chatCalls)
	}
	// An ordinary request cannot clear the persisted restriction; only the
	// authenticated administration operation above can change saved policy.
	response = fx.do(t, http.MethodPost, "/v1/chat/completions", `{"model":"gpt-fast","messages":[{"role":"user","content":"attempt widen"}],"restriction":{"required_assurance":"standard"}}`)
	if response.Code != http.StatusForbidden {
		t.Fatalf("request option widened saved policy: %d %s", response.Code, response.Body.String())
	}
}
