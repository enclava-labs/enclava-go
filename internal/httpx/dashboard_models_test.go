// dashboard_models_test.go — Models + Pricing page tests (OA-27): role
// denial on every route, observed-state truth in the endpoints table
// (unknown ≠ healthy, verified needs an observation), the pricing matrix
// coverage rule (endpoint set, model fallback, Not-priced cells), the
// read-only assurance drawer contract and the price-history drawer.
package httpx

import (
	"context"
	"encoding/json"
	"net/http"
	"net/http/httptest"
	"regexp"
	"strconv"
	"strings"
	"testing"
	"time"

	"enclava-go/internal/auth"
	"enclava-go/internal/billing"
	"enclava-go/internal/inference"
	"enclava-go/internal/source"
	"enclava-go/internal/store/postgres"
	"enclava-go/internal/testdb"
)

type modelsFixture struct {
	db        *postgres.DB
	billing   *postgres.BillingRepo
	endpoints *postgres.EndpointsRepo
	identity  *postgres.IdentityRepo
	tenant    string
	app       *App
}

func newModelsFixture(t *testing.T) *modelsFixture {
	t.Helper()
	db := testdb.New(t)
	tenant := testdb.SeedTenant(t, db, "models-page")
	identity := postgres.NewIdentityRepo(db)
	billingRepo := postgres.NewBillingRepo(db)
	endpoints := postgres.NewEndpointsRepo(db)
	sourceRepo := postgres.NewSourceRepo(db)
	executor, err := inference.NewService(inference.Deps{
		Endpoints:  endpoints,
		Sources:    sourceRepo,
		Authorizer: source.NewAuthorizer(sourceRepo, time.Hour),
		Billing:    billing.NewService(billingRepo),
	})
	if err != nil {
		t.Fatalf("inference service: %v", err)
	}
	authSvc := auth.NewService(identity, "")
	sessionAuth := NewSessionAuth(authSvc, auth.NewOIDCService(identity, nil), time.Hour, DashboardAuthTemplates(), "models-page")
	app := NewApp(authSvc, nil, "", nil, identity, nil, nil, nil, nil, nil, nil, billingRepo, endpoints,
		"sha256", "", "enc_", nil, sessionAuth, 1, false, executor, nil, GateConfig{}, sourceRepo, postgres.NewArtifactRepo(db))
	return &modelsFixture{db: db, billing: billingRepo, endpoints: endpoints, identity: identity, tenant: tenant, app: app}
}

// modelsLogin seeds a scoped user (a real explicit role — never admin or
// wildcard authority) and returns its session + CSRF cookies.
func modelsLogin(t *testing.T, f *modelsFixture, email string, scopes ...string) (*http.Cookie, *http.Cookie) {
	t.Helper()
	user := testdb.SeedUser(t, f.db, f.tenant, email, scopes)
	setPassword(t, f.identity, f.tenant, user.ID, "correct-horse")
	return login(t, f.app, email, "correct-horse")
}

func modelsSeedEndpoint(t *testing.T, f *modelsFixture, e inference.Endpoint) {
	t.Helper()
	e.TenantID = f.tenant
	if err := f.endpoints.Create(context.Background(), e); err != nil {
		t.Fatalf("seed endpoint %s: %v", e.ID, err)
	}
}

func modelsSeedPrice(t *testing.T, f *modelsFixture, at time.Time, subjectType, subjectID, unit string, micros int64) {
	t.Helper()
	err := f.billing.PutPrice(context.Background(), f.tenant,
		"price_"+subjectID+"_"+unit+"_"+subjectType+"_"+strconv.FormatInt(at.UnixNano(), 36),
		billing.Price{
			SubjectType: subjectType, SubjectID: subjectID,
			Capability: inference.CapabilityChat, Unit: billing.UnitKind(unit),
			RateMicros: micros, UnitQuantity: 1000000, Currency: "USD",
			EffectiveFrom: at,
		}, "seeder", at)
	if err != nil {
		t.Fatalf("seed price %s/%s/%s: %v", subjectType, subjectID, unit, err)
	}
}

func modelsGet(t *testing.T, f *modelsFixture, path string, session, csrf *http.Cookie) *httptest.ResponseRecorder {
	t.Helper()
	return sessionForm(t, f.app, http.MethodGet, path, nil, session, csrf)
}

// TestDashboardModelsMemberDenied: only models.manage reaches any of the six
// routes — member and knowledge-manager sessions get the designed 403.
func TestDashboardModelsMemberDenied(t *testing.T) {
	f := newModelsFixture(t)
	member, memberCSRF := modelsLogin(t, f, "member@models.test", "chatbot")
	manager, managerCSRF := modelsLogin(t, f, "kmanager@models.test", "rag", "rag.manage")

	routes := []string{
		"/dashboard/models",
		"/dashboard/models/pricing",
		"/dashboard/partials/models/endpoints",
		"/dashboard/partials/models/pricing",
		"/dashboard/partials/models/endpoint-drawer",
		"/dashboard/partials/models/price-history",
	}
	for _, who := range []struct{ session, csrf *http.Cookie }{{member, memberCSRF}, {manager, managerCSRF}} {
		for _, path := range routes {
			rec := modelsGet(t, f, path, who.session, who.csrf)
			if rec.Code != http.StatusForbidden {
				t.Fatalf("GET %s: expected 403 without models.manage, got %d body=%s", path, rec.Code, rec.Body.String())
			}
		}
	}
}

// TestDashboardModelsEndpointsRuntime: health/assurance cells render only
// observed state — a never-dispatched endpoint shows the unobserved pill
// (never Ready), a configured-but-unverified assurance reads Unverified, no
// assurance mode reads Not confidential, and the verified cell (hand-built
// row through the real fragment set) shows Verified · TEE · checked time.
func TestDashboardModelsEndpointsRuntime(t *testing.T) {
	f := newModelsFixture(t)
	session, csrf := modelsLogin(t, f, "admin@models.test", "pricing.manage")
	verifiable := time.Now().UTC().Add(-2 * time.Minute)
	modelsSeedEndpoint(t, f, inference.Endpoint{
		ID: "ep-unknown", Protocol: "openai_compatible", BaseOrigin: "https://unknown.example",
		Capabilities: []string{inference.CapabilityChat}, Enabled: true,
	})
	modelsSeedEndpoint(t, f, inference.Endpoint{
		ID: "ep-unverified", Protocol: "tinfoil", BaseOrigin: "https://unverified.example",
		AssuranceMode: "high", Capabilities: []string{inference.CapabilityChat}, Enabled: true,
	})
	modelsSeedEndpoint(t, f, inference.Endpoint{
		ID: "ep-open", Protocol: "openai_compatible", BaseOrigin: "https://open.example",
		Capabilities: []string{inference.CapabilityChat}, Enabled: true,
	})
	modelsSeedEndpoint(t, f, inference.Endpoint{
		ID: "ep-off", Protocol: "openai_compatible", BaseOrigin: "https://off.example",
		Capabilities: []string{inference.CapabilityChat}, Enabled: false,
	})

	rec := modelsGet(t, f, "/dashboard/models", session, csrf)
	if rec.Code != http.StatusOK {
		t.Fatalf("GET /dashboard/models: expected 200, got %d body=%s", rec.Code, rec.Body.String())
	}
	body := rec.Body.String()
	if strings.Contains(body, ">Ready<") {
		t.Fatalf("no endpoint was ever dispatched — Ready must not render without an observation:\n%s", body)
	}
	if !strings.Contains(body, `<td data-label="Health"><span class="pill neutral"><i class="dot"></i>Unobserved</span>`) {
		t.Fatalf("never-dispatched endpoint must show unobserved health:\n%s", body)
	}
	for _, want := range []string{"Unobserved", "Unverified", "Not confidential", "Disabled", "ep-unknown", "ep-unverified"} {
		if !strings.Contains(body, want) {
			t.Fatalf("endpoints table missing %q\n%s", want, body)
		}
	}

	// The verified cell renders through the real template set: an observed
	// attestation shows Verified + TEE + checked time; observed latency in ms.
	set, err := dashboardTemplateSet("models")
	if err != nil {
		t.Fatalf("template set: %v", err)
	}
	view := dashboardView{Data: modelsData{Tab: "endpoints", Endpoints: []modelsEndpointRow{{
		ID: "ep-v", Protocol: "tinfoil", Origin: "https://v.example", Enabled: true,
		AssuranceState: "verified", AssuranceChecked: &verifiable,
		Runtime: "ready", Latency: "42 ms",
	}}}}
	var buf strings.Builder
	if err := set.ExecuteTemplate(&buf, "frag-models-endpoints", view); err != nil {
		t.Fatalf("render endpoints fragment: %v", err)
	}
	frag := buf.String()
	for _, want := range []string{"Verified", "TEE · checked", "Ready", "42 ms p50"} {
		if !strings.Contains(frag, want) {
			t.Fatalf("verified row missing %q\n%s", want, frag)
		}
	}
}

// TestDashboardModelsPricingMatrix: rows price every model binding for each
// priced capability its endpoint advertises; the endpoint set wins when
// complete, otherwise the whole model set supplies the rates (never a
// per-unit merge); unpriced required cells carry the exact refusal copy with
// a Set price control bound to that unit.
func TestDashboardModelsPricingMatrix(t *testing.T) {
	f := newModelsFixture(t)
	session, csrf := modelsLogin(t, f, "admin@models.test", "pricing.manage")
	past := time.Now().UTC().Add(-time.Hour)

	modelsSeedEndpoint(t, f, inference.Endpoint{
		ID: "ep-a", Protocol: "openai_compatible", BaseOrigin: "https://a.example", Enabled: true,
		Capabilities: []string{inference.CapabilityChat, inference.CapabilityVision, "model:ma=up-a"},
	})
	for _, unit := range []string{"input", "cached_input", "output"} {
		modelsSeedPrice(t, f, past, "endpoint", "ep-a", unit, 1000000)
	}
	modelsSeedEndpoint(t, f, inference.Endpoint{
		ID: "ep-b", Protocol: "openai_compatible", BaseOrigin: "https://b.example", Enabled: true,
		Capabilities: []string{inference.CapabilityChat, "model:mb=up-b"},
	})
	modelsSeedPrice(t, f, past, "endpoint", "ep-b", "input", 1111111)
	for _, unit := range []string{"input", "cached_input", "output"} {
		modelsSeedPrice(t, f, past, "model", "up-b", unit, 2500000)
	}
	modelsSeedEndpoint(t, f, inference.Endpoint{
		ID: "ep-c", Protocol: "openai_compatible", BaseOrigin: "https://c.example", Enabled: true,
		Capabilities: []string{inference.CapabilityChat, "model:mc"},
	})
	modelsSeedEndpoint(t, f, inference.Endpoint{
		ID: "ep-e", Protocol: "openai_compatible", BaseOrigin: "https://e.example", Enabled: true,
		Capabilities: []string{inference.CapabilityEmbeddings, "model:me=up-e"},
	})
	for _, unit := range []string{"input", "cached_input"} {
		price := `{"subject_type":"model","subject_id":"up-e","capability":"embeddings","unit":"` + unit + `","rate_micros":500000,"unit_quantity":1000000}`
		if rec := sessionDo(t, f.app, http.MethodPost, "/api/v1/admin/pricing", price, session, csrf); rec.Code != http.StatusCreated {
			t.Fatalf("seed embeddings price: %d %s", rec.Code, rec.Body.String())
		}
	}

	rec := modelsGet(t, f, "/dashboard/models/pricing", session, csrf)
	if rec.Code != http.StatusOK {
		t.Fatalf("GET /dashboard/models/pricing: expected 200, got %d body=%s", rec.Code, rec.Body.String())
	}
	body := rec.Body.String()
	if got := strings.Count(body, "not priced — requests refused"); got != 6 {
		t.Fatalf("unpriced cells = %d, want 6 (ep-c chat + ep-a vision required units)\n%s", got, body)
	}
	if !strings.Contains(body, `$1.00`) || !strings.Contains(body, "via endpoint") {
		t.Fatalf("complete endpoint set must render its own rates via endpoint\n%s", body)
	}
	if !strings.Contains(body, "$2.50") || !strings.Contains(body, "via model") {
		t.Fatalf("incomplete endpoint set must fall back to the model set via model\n%s", body)
	}
	if !strings.Contains(body, "per 1,000,000 tokens") {
		t.Fatalf("cells must state the per-1M basis\n%s", body)
	}
	// One row-level Set price control carries the write binding; the form it
	// opens covers every category of that row.
	for _, want := range []string{
		`data-endpoint="ep-c" data-model="mc" data-capability="chat/completions"`,
		`data-endpoint="ep-a" data-model="ma" data-capability="vision"`,
	} {
		if !strings.Contains(body, want) {
			t.Fatalf("Set price control missing binding %q\n%s", want, body)
		}
	}
	embedRow := regexp.MustCompile(`(?s)data-price-row="ep-e\|me\|embeddings">.*?</tr>`).FindString(body)
	if strings.Count(embedRow, "$0.50") != 2 || strings.Contains(embedRow, "requests refused") {
		t.Fatalf("embeddings row must show its two priced units and no refusal: %q\n%s", embedRow, body)
	}
}

// TestDashboardModelsSetPriceShowsInCell: a save made through a cell's price
// control — Set price on a gap, Change on a priced cell — shows in that cell
// right away. The control targets the price set the cell reads; a one-unit
// write into an incomplete endpoint set is ignored by the whole-set rule and
// would leave the cell "Not priced".
func TestDashboardModelsSetPriceShowsInCell(t *testing.T) {
	f := newModelsFixture(t)
	session, csrf := modelsLogin(t, f, "admin@models.test", "pricing.manage")
	hourAgo := time.Now().UTC().Add(-time.Hour)
	// ep-s: an incomplete endpoint set, so its cells read the (empty) model set.
	modelsSeedEndpoint(t, f, inference.Endpoint{
		ID: "ep-s", Protocol: "openai_compatible", BaseOrigin: "https://s.example", Enabled: true,
		Capabilities: []string{inference.CapabilityChat, "model:ms=up-s"},
	})
	modelsSeedPrice(t, f, hourAgo, "endpoint", "ep-s", "input", 1000000)
	// ep-f: a complete endpoint set, so its cells read the endpoint set.
	modelsSeedEndpoint(t, f, inference.Endpoint{
		ID: "ep-f", Protocol: "openai_compatible", BaseOrigin: "https://f.example", Enabled: true,
		Capabilities: []string{inference.CapabilityChat, "model:mf=up-f"},
	})
	for _, unit := range []string{"input", "cached_input", "output"} {
		modelsSeedPrice(t, f, hourAgo, "endpoint", "ep-f", unit, 1000000)
	}

	cellOf := func(endpoint, unit string) string {
		body := modelsGet(t, f, "/dashboard/partials/models/pricing", session, csrf).Body.String()
		cell := regexp.MustCompile(`(?s)data-price-cell="` + endpoint + `\|` + unit + `"[^>]*>.*?</td>`).FindString(body)
		if cell == "" {
			t.Fatalf("no %s|%s cell\n%s", endpoint, unit, body)
		}
		return cell
	}
	// save posts what models.js submitPrice builds from the row's Set-price
	// control: one atomic batch entry for the target unit.
	save := func(endpoint, unit string, micros int64) string {
		body := modelsGet(t, f, "/dashboard/partials/models/pricing", session, csrf).Body.String()
		var row string
		for _, tr := range regexp.MustCompile(`(?s)<tr data-price-row="[^"]*">.*?</tr>`).FindAllString(body, -1) {
			if strings.Contains(tr, `data-price-cell="`+endpoint+`|`+unit+`"`) {
				row = tr
			}
		}
		if row == "" {
			t.Fatalf("no row holds %s|%s\n%s", endpoint, unit, body)
		}
		button := regexp.MustCompile(`<button[^>]*data-action="models:price"[^>]*>`).FindString(row)
		attr := func(name string) string {
			m := regexp.MustCompile(` ` + name + `="([^"]*)"`).FindStringSubmatch(button)
			if m == nil {
				t.Fatalf("%s|%s price control has no %s: %q", endpoint, unit, name, row)
			}
			return m[1]
		}
		payload, _ := json.Marshal(map[string]any{
			"prices": []map[string]any{{
				"subject_type": attr("data-subject-type"), "subject_id": attr("data-subject-id"),
				"capability": attr("data-capability"), "unit": unit,
				"rate_micros": micros, "unit_quantity": 1000000, "currency": "USD",
			}},
		})
		if rec := sessionDo(t, f.app, http.MethodPost, "/api/v1/admin/pricing", string(payload), session, csrf); rec.Code != http.StatusCreated {
			t.Fatalf("save %s|%s: %d %s", endpoint, unit, rec.Code, rec.Body.String())
		}
		return cellOf(endpoint, unit)
	}

	for _, tc := range []struct {
		endpoint, unit string
		micros         int64
		rate, basis    string
	}{
		{"ep-s", "output", 3000000, "$3.00", "via model"},   // Set price on a gap
		{"ep-s", "output", 4000000, "$4.00", "via model"},   // Change a model-set price
		{"ep-f", "input", 2000000, "$2.00", "via endpoint"}, // Change an endpoint-set price
	} {
		cell := save(tc.endpoint, tc.unit, tc.micros)
		if !strings.Contains(cell, tc.rate) || !strings.Contains(cell, tc.basis) || strings.Contains(cell, "not priced") {
			t.Fatalf("%s|%s saved %d: cell must show %s %s: %q", tc.endpoint, tc.unit, tc.micros, tc.rate, tc.basis, cell)
		}
	}
}

// TestDashboardModelsDrawer: the drawer form carries only endpointRequest
// fields — no assurance_verified control exists anywhere in the markup —
// renders the read-only assurance status line and the private-authorization
// help; an unknown id gets the drawer's sentence-only empty state (200),
// never a bare 404.
func TestDashboardModelsDrawer(t *testing.T) {
	f := newModelsFixture(t)
	session, csrf := modelsLogin(t, f, "admin@models.test", "pricing.manage")

	rec := modelsGet(t, f, "/dashboard/partials/models/endpoint-drawer", session, csrf)
	if rec.Code != http.StatusOK {
		t.Fatalf("create drawer: expected 200, got %d body=%s", rec.Code, rec.Body.String())
	}
	create := rec.Body.String()
	if strings.Contains(create, "assurance_verified") {
		t.Fatalf("drawer must not contain an assurance_verified control\n%s", create)
	}
	for _, want := range []string{`data-models-endpoint-form`, "Authorized private", "explicit authorization", `name="protocol"`} {
		if !strings.Contains(create, want) {
			t.Fatalf("create drawer missing %q\n%s", want, create)
		}
	}

	modelsSeedEndpoint(t, f, inference.Endpoint{
		ID: "ep-edit", Protocol: "confidential_sdk", SDKProvider: "wasm",
		BaseOrigin: "https://edit.example", AuthorizedPrivate: true, Enabled: true,
		AssuranceMode: "high",
		Capabilities:  []string{inference.CapabilityChat, "model:mx=up-x"},
	})
	rec = modelsGet(t, f, "/dashboard/partials/models/endpoint-drawer?id=ep-edit", session, csrf)
	if rec.Code != http.StatusOK {
		t.Fatalf("edit drawer: expected 200, got %d body=%s", rec.Code, rec.Body.String())
	}
	edit := rec.Body.String()
	if strings.Contains(edit, "assurance_verified") {
		t.Fatalf("edit drawer must not contain an assurance_verified control\n%s", edit)
	}
	for _, want := range []string{
		`value="ep-edit"`, `value="confidential_sdk" selected`, "Unverified",
		"never set from this form", `data-models-disable`, "Disable endpoint ep-edit?",
		`name="authorized_private" checked`, `value="mx=up-x"`, `data-endpoint-id="ep-edit"`,
	} {
		if !strings.Contains(edit, want) {
			t.Fatalf("edit drawer missing %q\n%s", want, edit)
		}
	}

	rec = modelsGet(t, f, "/dashboard/partials/models/endpoint-drawer?id=ep-nope", session, csrf)
	if rec.Code != http.StatusOK || !strings.Contains(rec.Body.String(), "That endpoint is no longer available.") {
		t.Fatalf("unknown drawer id: expected 200 with the drawer empty state, got %d body=%s", rec.Code, rec.Body.String())
	}

	// The page module must actually be served — the old pricing bug class.
	rec = modelsGet(t, f, "/dashboard/models", session, csrf)
	if !strings.Contains(rec.Body.String(), "/dashboard/static/pages/models.js") {
		t.Fatalf("page must load the models module\n%s", rec.Body.String())
	}
	rec = modelsGet(t, f, "/dashboard/static/pages/models.js", session, csrf)
	if rec.Code != http.StatusOK {
		t.Fatalf("models.js: expected 200, got %d", rec.Code)
	}
}

// TestDashboardPriceHistory: the drawer renders real ListPrices rows for the
// pair — endpoint and model subjects, money and per-1M basis, newest
// effective first; a missing endpoint is a 400.
func TestDashboardPriceHistory(t *testing.T) {
	f := newModelsFixture(t)
	session, csrf := modelsLogin(t, f, "admin@models.test", "pricing.manage")
	old := time.Now().UTC().Add(-48 * time.Hour)
	fresh := time.Now().UTC().Add(-time.Hour)
	modelsSeedEndpoint(t, f, inference.Endpoint{
		ID: "ep-h", Protocol: "openai_compatible", BaseOrigin: "https://h.example", Enabled: true,
		Capabilities: []string{inference.CapabilityChat, "model:mh=up-h"},
	})
	modelsSeedPrice(t, f, old, "endpoint", "ep-h", "input", 1000000)
	modelsSeedPrice(t, f, fresh, "endpoint", "ep-h", "input", 1250000)
	modelsSeedPrice(t, f, fresh, "model", "up-h", "output", 2500000)

	rec := modelsGet(t, f, "/dashboard/partials/models/price-history", session, csrf)
	if rec.Code != http.StatusBadRequest {
		t.Fatalf("missing endpoint: expected 400, got %d", rec.Code)
	}

	rec = modelsGet(t, f, "/dashboard/partials/models/price-history?endpoint=ep-h&model=mh", session, csrf)
	if rec.Code != http.StatusOK {
		t.Fatalf("history drawer: expected 200, got %d body=%s", rec.Code, rec.Body.String())
	}
	body := rec.Body.String()
	if got := strings.Count(body, `data-label="Rate"`); got != 3 {
		t.Fatalf("rate rows = %d, want 3 (2 endpoint + 1 model subject rows)\n%s", got, body)
	}
	if !strings.Contains(body, "$1.25") || !strings.Contains(body, "$1.00") || !strings.Contains(body, "$2.50") {
		t.Fatalf("history must render real ListPrices rates\n%s", body)
	}
	if strings.Count(body, ">model<") != 1 || strings.Count(body, ">endpoint<") != 2 {
		t.Fatalf("history must label both price subjects\n%s", body)
	}
	// Newest effective first: the fresh input rate appears before the old one.
	if strings.Index(body, "$1.25") > strings.Index(body, "$1.00") {
		t.Fatalf("history must sort newest effective first\n%s", body)
	}
}
