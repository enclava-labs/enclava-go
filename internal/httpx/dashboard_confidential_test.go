// dashboard_confidential_test.go — the B13 confidential card: tri-state
// summary semantics (P7-01), CheckedAt freshness rule and the models.manage
// endpoint-detail gate (T-07-03-01).
package httpx

import (
	"context"
	"net/http"
	"net/http/httptest"
	"strings"
	"testing"
	"time"

	"enclava-go/internal/auth"
	"enclava-go/internal/inference"
	"enclava-go/internal/store/postgres"
	"enclava-go/internal/testdb"
)

func TestSummarizeEndpointsStates(t *testing.T) {
	checked := time.Now().UTC().Add(-2 * time.Minute)
	failedAt := time.Now().UTC().Add(-5 * time.Minute)
	old := time.Now().UTC().Add(-48 * time.Hour)
	eps := []inference.Endpoint{
		{ID: "ep-tinfoil", Protocol: "tinfoil", BaseOrigin: "https://enclave.example"},
		{ID: "ep-openai", Protocol: "openai_compatible", BaseOrigin: "https://api.example"}, // never confidential
		{ID: "ep-sdk", Protocol: "confidential_sdk", SDKProvider: "wasm", BaseOrigin: "https://sdk.example"},
		{ID: "ep-old", Protocol: "tinfoil", BaseOrigin: "https://old.example", AssuranceVerifiedAt: &old}, // unobserved this boot
	}
	obs := []inference.AttestationObservation{
		{EndpointID: "ep-tinfoil", State: inference.AttestationVerified, VerifiedAt: &checked},
		{EndpointID: "ep-sdk", State: inference.AttestationFailed, VerifiedAt: &failedAt, LastError: "attest rejected"},
	}
	s := summarizeEndpoints(eps, obs)
	if s.Total != 3 || s.Verified != 1 {
		t.Fatalf("Total/Verified = %d/%d, want 3/1 — failed-after-verified and unobserved must not count", s.Total, s.Verified)
	}
	if s.CheckedAt == nil || !s.CheckedAt.Equal(checked) {
		t.Fatalf("CheckedAt = %v, want the 2-min-old transport stamp", s.CheckedAt)
	}
}

func TestSummarizeEndpointsZeroConfidential(t *testing.T) {
	// The popover renders states through statusPill via .State.String — a
	// named string type is NOT assignable to a string parameter.
	_ = statusPill("confidential", inference.AttestationUnobserved.String())
	s := summarizeEndpoints([]inference.Endpoint{{ID: "e", Protocol: "openai_compatible", BaseOrigin: "https://x.example"}}, nil)
	if s.Total != 0 || s.Verified != 0 || s.CheckedAt != nil {
		t.Fatalf("neutral summary = %+v, want zero/nil", s)
	}
}

func TestSummarizeEndpointsCheckedAtUsesPersistedStamp(t *testing.T) {
	obsAt := time.Now().UTC().Add(-2 * time.Minute)
	persisted := time.Now().UTC().Add(-1 * time.Hour)
	eps := []inference.Endpoint{
		{ID: "e1", Protocol: "tinfoil", BaseOrigin: "https://a.example", AssuranceVerifiedAt: &persisted},
	}
	s := summarizeEndpoints(eps, []inference.AttestationObservation{
		{EndpointID: "e1", State: inference.AttestationVerified, VerifiedAt: &obsAt},
	})
	if s.CheckedAt == nil || !s.CheckedAt.Equal(obsAt) {
		t.Fatalf("CheckedAt = %v, want the fresher transport stamp %v", s.CheckedAt, obsAt)
	}
	// Unobserved endpoint: the persisted stamp is the historical display only.
	s = summarizeEndpoints(eps, nil)
	if s.Total != 1 || s.Verified != 0 || s.CheckedAt == nil || !s.CheckedAt.Equal(persisted) {
		t.Fatalf("unobserved summary = %+v, want Total=1 with the persisted stamp", s)
	}
}

// TestConfidentialSummaryCapsGate proves members receive counts but never
// endpoint rows, while models.manage does (T-07-03-01).
func TestConfidentialSummaryCapsGate(t *testing.T) {
	db := testdb.New(t)
	ctx := context.Background()
	repo := postgres.NewIdentityRepo(db)
	tenant, err := repo.CreateTenant(ctx, "conf-gate", "Gate Org")
	if err != nil {
		t.Fatal(err)
	}
	endpoints := postgres.NewEndpointsRepo(db)
	for _, e := range []inference.Endpoint{
		{ID: "ep-gate-1", TenantID: tenant, Protocol: "tinfoil", BaseOrigin: "https://gate1.example", Enabled: true},
		{ID: "ep-gate-2", TenantID: tenant, Protocol: "confidential_sdk", SDKProvider: "wasm", BaseOrigin: "https://gate2.example", Enabled: true},
	} {
		if err := endpoints.Create(ctx, e); err != nil {
			t.Fatal(err)
		}
	}
	app := &App{endpoints: endpoints, executor: &inference.Service{}} // zero-value executor → all unobserved
	p := auth.Principal{TenantID: tenant}

	member := app.confidentialSummary(ctx, p, dashboardCaps{})
	if member.Total != 2 || len(member.Endpoints) != 0 {
		t.Fatalf("member summary = %+v, want Total=2 with NO endpoint rows", member)
	}
	manager := app.confidentialSummary(ctx, p, dashboardCaps{ModelsManage: true})
	if len(manager.Endpoints) != 2 {
		t.Fatalf("manager summary rows = %d, want 2", len(manager.Endpoints))
	}
	for _, row := range manager.Endpoints {
		if row.State != inference.AttestationUnobserved {
			t.Errorf("row %q state = %q, want unobserved with no transports", row.EndpointID, row.State)
		}
		if row.Label == "" {
			t.Errorf("row %q must carry a human label", row.EndpointID)
		}
	}

	// A models.manage page renders the conf popover rows through the real
	// layout: .State.String feeds statusPill (named string ≠ string param).
	base, err := dashboardBaseTemplates()
	if err != nil {
		t.Fatal(err)
	}
	set, _ := base.Clone()
	if _, err := set.Parse(`{{define "content"}}x{{end}}`); err != nil {
		t.Fatal(err)
	}
	req := httptest.NewRequest("GET", "/dashboard/models", nil)
	req = req.WithContext(withCaps(auth.WithPrincipal(req.Context(), p), dashboardCaps{ModelsManage: true}))
	rec := httptest.NewRecorder()
	app.renderDashboardInto(rec, req, set, "page", "models", nil)
	if rec.Code != http.StatusOK {
		t.Fatalf("manager shell status = %d", rec.Code)
	}
	body := rec.Body.String()
	for _, want := range []string{"gate1.example", "gate2.example", `class="pill neutral"`, "Unobserved"} {
		if !strings.Contains(body, want) {
			t.Errorf("manager conf popover missing %q", want)
		}
	}
}
