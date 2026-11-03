// on stored billing state (JOB-03/JOB-04): budgets and liabilities live in
// the postgres billing tables, seeded through BillingRepo.
package httpx

import (
	"context"
	"encoding/json"
	"fmt"
	"math"
	"net/http"
	"net/http/httptest"
	"strings"
	"sync/atomic"
	"testing"
	"time"

	"enclava-go/internal/auth"
	"enclava-go/internal/billing"
	"enclava-go/internal/store/postgres"
	"enclava-go/internal/testdb"
)

var budgetTestSeq atomic.Int64

type budgetFixture struct {
	db       *postgres.DB
	repo     *postgres.BillingRepo
	identity *postgres.IdentityRepo
	tenant   string
	app      *App
}

func newBudgetFixture(t *testing.T) budgetFixture {
	t.Helper()
	db := testdb.New(t)
	repo := postgres.NewBillingRepo(db)
	identity := postgres.NewIdentityRepo(db)
	tenant := testdb.SeedTenant(t, db, fmt.Sprintf("bt%d", budgetTestSeq.Add(1)))
	return budgetFixture{db: db, repo: repo, identity: identity, tenant: tenant,
		app: &App{billingRepo: repo, identity: identity}}
}

// seedSettledUsage writes one settled reservation plus its usage_records row
// attributed to subject, then backdates the usage row's created_at so period
// windows are exercised without sleeping.
func seedSettledUsage(t *testing.T, f budgetFixture, id string, subject billing.Subject, units, amountMicros int64, currency string, at time.Time) {
	t.Helper()
	ctx := context.Background()
	res := billing.Reservation{
		TenantID: f.tenant, ID: "res_" + id, State: billing.ReservationSettled,
		Subject: subject,
		Price: billing.PriceSnapshot{
			Currency: currency, RoundingRule: billing.RoundingCeil,
			Rates: []billing.SnapshotRate{{Unit: billing.UnitInput, RateMicros: 1, UnitQuantity: 1}},
		},
		LimitAmountMicros: amountMicros, Currency: currency,
		Units:      map[billing.UnitKind]int64{billing.UnitInput: units},
		Capability: "chat", Model: "m-test", EndpointID: "ep-test",
	}
	err := f.repo.InTx(ctx, func(tx billing.BillingTx) error {
		if err := tx.InsertReservations(ctx, []billing.Reservation{res}); err != nil {
			return err
		}
		if _, err := tx.Transition(ctx, f.tenant, res.ID, billing.ReservationHeld, billing.ReservationSettled); err != nil {
			return err
		}
		return tx.InsertUsageRecord(ctx, billing.UsageRecord{
			TenantID: f.tenant, ID: "ur_" + id, ReservationID: res.ID,
			Subject: subject, Capability: "chat", Model: "m-test", EndpointID: "ep-test",
			Units:        map[billing.UnitKind]int64{billing.UnitInput: units},
			AmountMicros: amountMicros, Currency: currency, Price: res.Price,
			Outcome: "completed",
		})
	})
	if err != nil {
		t.Fatalf("seed settled usage %s: %v", id, err)
	}
	if _, err := f.db.SQL.ExecContext(ctx,
		`UPDATE usage_records SET created_at = $2 WHERE tenant_id = $1::uuid AND id = $3`,
		f.tenant, at.UTC(), "ur_"+id); err != nil {
		t.Fatalf("backdate usage %s: %v", id, err)
	}
}

func TestCheckedBudgetAdd(t *testing.T) {
	for _, tt := range []struct {
		name string
		a, b int64
		want int64
		ok   bool
	}{
		{name: "positive", a: 7, b: 5, want: 12, ok: true},
		{name: "positive overflow", a: int64(^uint64(0) >> 1), b: 1},
		{name: "negative overflow", a: -int64(^uint64(0)>>1) - 1, b: -1},
	} {
		t.Run(tt.name, func(t *testing.T) {
			got, ok := checkedBudgetAdd(tt.a, tt.b)
			if got != tt.want || ok != tt.ok {
				t.Fatalf("checkedBudgetAdd(%d, %d) = (%d, %v), want (%d, %v)",
					tt.a, tt.b, got, ok, tt.want, tt.ok)
			}
		})
	}
}
func TestDashboardBudgetShowsHeldSettledAndProgress(t *testing.T) {
	f := newBudgetFixture(t)
	ctx := context.Background()
	keyID := "dashboard-key"
	subject := billing.Subject{Type: billing.BudgetSubjectAPIKey, ID: keyID}
	now := time.Now().UTC()
	// $1 per input unit so the frozen money() formatter shows the exact
	// held/settled amounts at its two-decimal precision.
	if err := f.repo.PutPrice(ctx, f.tenant, "dashboard-price", billing.Price{
		SubjectType: billing.PriceSubjectModel, SubjectID: "dashboard-model", Capability: "chat",
		Unit: billing.UnitInput, RateMicros: 1_000_000, UnitQuantity: 1, Currency: "USD",
	}, "tester", now); err != nil {
		t.Fatalf("put price: %v", err)
	}
	if err := f.repo.CreateBudget(ctx, f.tenant, "dashboard-budget", billing.Budget{
		Name: "dashboard", Subject: subject, Kind: billing.BudgetKindAmount, Period: billing.PeriodMonthly,
		LimitAmountMicros: 100_000_000, Currency: "USD", StartsAt: now.Add(-time.Hour),
	}); err != nil {
		t.Fatalf("create budget: %v", err)
	}
	principal := auth.Principal{
		TenantID: f.tenant, CredentialKind: auth.CredentialKindAPIKey,
		CredentialID: keyID, BillingSubjectKind: auth.BillingSubjectAPIKey,
		BillingSubjectID: keyID,
	}
	service := billing.NewService(f.repo)
	reservation, err := service.Reserve(ctx, principal, billing.ReserveRequest{
		SubjectKeys: []billing.Subject{subject}, BillingSubject: subject, Capability: "chat",
		Model: "dashboard-model", EndpointID: "dashboard-endpoint",
		Estimate: billing.Usage{InputUnits: 40},
		Price:    billing.PriceKey{SubjectType: billing.PriceSubjectModel, SubjectID: "dashboard-model"},
	})
	if err != nil {
		t.Fatalf("reserve: %v", err)
	}
	render := func() string {
		req := httptest.NewRequest(http.MethodGet, "/dashboard/partials/usage/budgets", nil)
		req = req.WithContext(auth.WithPrincipal(req.Context(),
			auth.Principal{TenantID: f.tenant, ActorID: "admin-1", ActorKind: auth.ActorKindUser}))
		rec := httptest.NewRecorder()
		f.app.handleDashboardUsageBudgetsFragment(rec, req)
		if rec.Code != http.StatusOK {
			t.Fatalf("budgets tab returned %d: %s", rec.Code, rec.Body.String())
		}
		return rec.Body.String()
	}
	held := render()
	// Progress counts the held liability (40%); the informational liability
	// row shows the same held amount per currency.
	if !strings.Contains(held, "$40.00") || !strings.Contains(held, "Held $40.00") {
		t.Fatalf("held liability or counted usage not rendered: %s", held)
	}
	if !strings.Contains(held, `aria-valuenow="40"`) {
		t.Fatalf("progress bar must show the admission-matching ratio: %s", held)
	}
	if _, err := service.Settle(ctx, principal, reservation.ID, billing.Usage{InputUnits: 30}); err != nil {
		t.Fatalf("settle: %v", err)
	}
	settled := render()
	if !strings.Contains(settled, "$30.00") || !strings.Contains(settled, "Settled $30.00") {
		t.Fatalf("settled liability or counted usage not rendered: %s", settled)
	}
	if !strings.Contains(settled, `aria-valuenow="30"`) {
		t.Fatalf("progress must follow the settled counted scalar: %s", settled)
	}
}

// --- admin budget write API (plan 08-07 task 1) -------------------------------

// seedOwnedKey seeds one api key owned by a fresh user (api_keys always
// belong to a user or service account).
func seedOwnedKey(t *testing.T, f budgetFixture, name string) auth.APIKeyRecord {
	t.Helper()
	user := testdb.SeedUser(t, f.db, f.tenant, name+"@budgets.example", nil)
	_, rec := testdb.SeedAPIKey(t, f.db, f.tenant, testdb.SeedKeySpec{Name: name, OwnerUserID: user.ID})
	return rec
}

// adminPrincipal is a platform.admin actor; handlers only read tenant and
// actor identity off it — the scope gate is the registered middleware.
func (f budgetFixture) adminPrincipal() auth.Principal {
	return auth.Principal{
		TenantID: f.tenant, ActorID: "admin-1", ActorKind: auth.ActorKindUser,
		UserID: "admin-1",
	}
}

// postBudget posts one create body and returns the recorder.
func (f budgetFixture) postBudget(t *testing.T, principal auth.Principal, body string) *httptest.ResponseRecorder {
	t.Helper()
	req := httptest.NewRequest(http.MethodPost, "/api/v1/admin/budgets", strings.NewReader(body))
	return doBudgetHandler(f.app.handleAdminBudgets, principal, req)
}

func doBudgetHandler(h func(http.ResponseWriter, *http.Request, auth.Principal), principal auth.Principal, req *http.Request) *httptest.ResponseRecorder {
	rec := httptest.NewRecorder()
	h(rec, req.WithContext(auth.WithPrincipal(req.Context(), principal)), principal)
	return rec
}

func decodeBudgetView(t *testing.T, body string) budgetView {
	t.Helper()
	var view budgetView
	if err := json.Unmarshal([]byte(body), &view); err != nil {
		t.Fatalf("decode budget view: %v (%s)", err, body)
	}
	return view
}

func TestAdminBudgetCreateListPatchCycle(t *testing.T) {
	f := newBudgetFixture(t)
	key := seedOwnedKey(t, f, "budgeted")
	admin := f.adminPrincipal()
	startsAt := time.Now().UTC().Add(-time.Hour).Format(time.RFC3339)

	rec := f.postBudget(t, admin, fmt.Sprintf(
		`{"name":"team cap","subject_type":"api_key","subject_id":%q,"kind":"amount","limit_value":1000,"currency":"USD","period":"monthly","starts_at":%q}`,
		key.ID, startsAt))
	if rec.Code != http.StatusCreated {
		t.Fatalf("create = %d: %s", rec.Code, rec.Body.String())
	}
	created := decodeBudgetView(t, rec.Body.String())
	if created.Budget.Name != "team cap" || created.Budget.SubjectID != key.ID ||
		created.Budget.Kind != "amount" || created.Budget.LimitValue != 1000 ||
		created.Budget.Currency != "USD" || created.Budget.Period != "monthly" {
		t.Fatalf("created budget wrong: %+v", created.Budget)
	}
	if created.Progress.Counted != 0 || created.Progress.LimitValue != 1000 || created.Progress.Currency != "USD" {
		t.Fatalf("fresh progress wrong: %+v", created.Progress)
	}

	// PATCH narrows the limit only — the liability window survives.
	patchReq := httptest.NewRequest(http.MethodPatch,
		"/api/v1/admin/budgets/"+created.Budget.ID, strings.NewReader(`{"limit_value":500}`))
	rec = doBudgetHandler(f.app.handleAdminBudgetByID, admin, patchReq)
	if rec.Code != http.StatusOK {
		t.Fatalf("patch = %d: %s", rec.Code, rec.Body.String())
	}
	patched := decodeBudgetView(t, rec.Body.String())
	if patched.Budget.LimitValue != 500 || patched.Budget.StartsAt != created.Budget.StartsAt {
		t.Fatalf("patched budget wrong: %+v", patched.Budget)
	}

	// GET lists every active budget with the projection.
	listReq := httptest.NewRequest(http.MethodGet, "/api/v1/admin/budgets", nil)
	rec = doBudgetHandler(f.app.handleAdminBudgets, admin, listReq)
	if rec.Code != http.StatusOK {
		t.Fatalf("list = %d: %s", rec.Code, rec.Body.String())
	}
	var out struct {
		Budgets []budgetView `json:"budgets"`
	}
	if err := json.Unmarshal(rec.Body.Bytes(), &out); err != nil {
		t.Fatalf("decode list: %v (%s)", err, rec.Body.String())
	}
	if len(out.Budgets) != 1 || out.Budgets[0].Budget.LimitValue != 500 ||
		out.Budgets[0].Progress.LimitValue != 500 {
		t.Fatalf("list wrong: %+v", out.Budgets)
	}

	// Every write recorded a billing-audit row.
	var createdN, patchedN int
	if err := f.db.SQL.QueryRowContext(context.Background(),
		`SELECT count(*) FROM billing_audit WHERE tenant_id = $1::uuid AND action = 'budget.created'`,
		f.tenant).Scan(&createdN); err != nil || createdN != 1 {
		t.Fatalf("budget.created audit rows = %d (%v)", createdN, err)
	}
	if err := f.db.SQL.QueryRowContext(context.Background(),
		`SELECT count(*) FROM billing_audit WHERE tenant_id = $1::uuid AND action = 'budget.limit_updated'`,
		f.tenant).Scan(&patchedN); err != nil || patchedN != 1 {
		t.Fatalf("budget.limit_updated audit rows = %d (%v)", patchedN, err)
	}
}

func TestAdminBudgetMemberForbidden(t *testing.T) {
	f := newBudgetFixture(t)
	user := testdb.SeedUser(t, f.db, f.tenant, "member@example.com", []string{"chat.completions"})
	memberKey, _ := testdb.SeedAPIKey(t, f.db, f.tenant, testdb.SeedKeySpec{
		Name: "member", OwnerUserID: user.ID, Scopes: []string{"chat.completions"},
	})
	svc := auth.NewService(f.identity, "")
	gated := &App{authSvc: svc, identity: f.identity, billingRepo: f.repo,
		mux: http.NewServeMux(), maxUploadMB: 1}
	gated.mux.HandleFunc("/api/v1/admin/budgets", gated.authenticated("platform.admin", gated.handleAdminBudgets))

	req := httptest.NewRequest(http.MethodGet, "/api/v1/admin/budgets", nil)
	req.Header.Set("Authorization", "Bearer "+memberKey)
	rec := httptest.NewRecorder()
	gated.Handler().ServeHTTP(rec, req)
	if rec.Code != http.StatusForbidden {
		t.Fatalf("member list = %d, want 403: %s", rec.Code, rec.Body.String())
	}
}

func TestAdminBudgetValidationMatrix(t *testing.T) {
	f := newBudgetFixture(t)
	admin := f.adminPrincipal()
	startsAt := time.Now().UTC().Add(-time.Hour).Format(time.RFC3339)
	endsAt := time.Now().UTC().Add(2 * time.Hour).Format(time.RFC3339)
	key := seedOwnedKey(t, f, "ok-key")

	base := func(over map[string]any) string {
		body := map[string]any{
			"name": "cap", "subject_type": "api_key", "subject_id": key.ID,
			"kind": "amount", "limit_value": 100, "currency": "USD",
			"period": "monthly", "starts_at": startsAt,
		}
		for k, v := range over {
			if v == nil {
				delete(body, k)
				continue
			}
			body[k] = v
		}
		raw, _ := json.Marshal(body)
		return string(raw)
	}

	cases := []struct {
		name string
		body string
	}{
		{"missing name", base(map[string]any{"name": ""})},
		{"bad subject_type", base(map[string]any{"subject_type": "widget"})},
		{"bad kind", base(map[string]any{"kind": "bits"})},
		{"tokens with currency", base(map[string]any{"kind": "tokens", "currency": "USD"})},
		{"bad period", base(map[string]any{"period": "hourly"})},
		{"bad currency", base(map[string]any{"currency": "usd1"})},
		{"zero limit", base(map[string]any{"limit_value": 0})},
		{"negative limit", base(map[string]any{"limit_value": -5})},
		{"limit over checked bound", base(map[string]any{"limit_value": float64(math.MaxInt64)})},
		{"missing limit", base(map[string]any{"limit_value": nil})},
		{"bad starts_at", base(map[string]any{"starts_at": "yesterday"})},
		{"ends before starts", base(map[string]any{"starts_at": endsAt, "ends_at": startsAt})},
		{"missing api key subject", base(map[string]any{"subject_id": "key_nope"})},
	}
	for _, tc := range cases {
		t.Run(tc.name, func(t *testing.T) {
			if rec := f.postBudget(t, admin, tc.body); rec.Code != http.StatusBadRequest {
				t.Fatalf("expected 400, got %d: %s", rec.Code, rec.Body.String())
			}
		})
	}

	// A revoked key and another tenant's key are the same denial.
	if err := f.identity.RevokeAPIKey(context.Background(), f.tenant, key.ID, time.Now().UTC()); err != nil {
		t.Fatalf("revoke: %v", err)
	}
	if rec := f.postBudget(t, admin, base(map[string]any{"subject_id": key.ID})); rec.Code != http.StatusBadRequest {
		t.Fatalf("revoked key subject = %d, want 400", rec.Code)
	}
	foreignDB := testdb.New(t)
	defer func() { _ = foreignDB.Close() }()
	foreignTenant := testdb.SeedTenant(t, foreignDB, fmt.Sprintf("bt%d", budgetTestSeq.Add(1)))
	foreignUser := testdb.SeedUser(t, foreignDB, foreignTenant, "foreign@budgets.example", nil)
	_, foreign := testdb.SeedAPIKey(t, foreignDB, foreignTenant, testdb.SeedKeySpec{Name: "foreign", OwnerUserID: foreignUser.ID})
	if rec := f.postBudget(t, admin, base(map[string]any{"subject_id": foreign.ID})); rec.Code != http.StatusBadRequest {
		t.Fatalf("foreign key subject = %d, want 400", rec.Code)
	}

	// Cross-tenant and unknown budgets are one indistinguishable 404 on PATCH.
	for _, id := range []string{"budget_nope", foreign.ID + "-ghost"} {
		patchReq := httptest.NewRequest(http.MethodPatch, "/api/v1/admin/budgets/"+id, strings.NewReader(`{"limit_value":10}`))
		if rec := doBudgetHandler(f.app.handleAdminBudgetByID, admin, patchReq); rec.Code != http.StatusNotFound {
			t.Fatalf("patch %s = %d, want 404", id, rec.Code)
		}
	}
	// PATCH without limit_value is 400.
	patchReq := httptest.NewRequest(http.MethodPatch, "/api/v1/admin/budgets/whatever", strings.NewReader(`{}`))
	if rec := doBudgetHandler(f.app.handleAdminBudgetByID, admin, patchReq); rec.Code != http.StatusBadRequest {
		t.Fatalf("empty patch = %d, want 400", rec.Code)
	}
}

// seedKeySettledUsage writes a settled usage row attributed to the api-key
// credential (the canonical subject stays the owner), in the given currency.
func seedKeySettledUsage(t *testing.T, f budgetFixture, id, keyID string, units, amountMicros int64, currency string, at time.Time) {
	t.Helper()
	seedSettledUsage(t, f, id, billing.Subject{Type: billing.BudgetSubjectUser, ID: "owner-1"},
		units, amountMicros, currency, at)
	if _, err := f.db.SQL.ExecContext(context.Background(),
		`UPDATE usage_records SET credential_id = $3 WHERE tenant_id = $1::uuid AND id = $2`,
		f.tenant, "ur_"+id, keyID); err != nil {
		t.Fatalf("stamp credential: %v", err)
	}
}

func TestAdminBudgetLiabilityProjection(t *testing.T) {
	f := newBudgetFixture(t)
	key := seedOwnedKey(t, f, "proj")
	admin := f.adminPrincipal()
	startsAt := time.Now().UTC().Add(-time.Hour).Format(time.RFC3339)
	rec := f.postBudget(t, admin, fmt.Sprintf(
		`{"name":"usd cap","subject_type":"api_key","subject_id":%q,"kind":"amount","limit_value":1000,"currency":"USD","period":"monthly","starts_at":%q}`,
		key.ID, startsAt))
	if rec.Code != http.StatusCreated {
		t.Fatalf("create = %d: %s", rec.Code, rec.Body.String())
	}
	budgetID := decodeBudgetView(t, rec.Body.String()).Budget.ID

	// Held USD, settled USD and settled EUR inside the window: only the USD
	// rows may count toward the progress denominator.
	ctx := context.Background()
	subject := billing.Subject{Type: billing.BudgetSubjectAPIKey, ID: key.ID}
	service := billing.NewService(f.repo)
	if err := f.repo.PutPrice(ctx, f.tenant, "proj-model-price", billing.Price{
		SubjectType: billing.PriceSubjectModel, SubjectID: "m", Capability: "chat",
		Unit: billing.UnitInput, RateMicros: 1, UnitQuantity: 1, Currency: "USD",
	}, "tester", time.Now().UTC().Add(-time.Minute)); err != nil {
		t.Fatalf("put price: %v", err)
	}
	reservation, err := service.Reserve(ctx, auth.Principal{
		TenantID: f.tenant, CredentialKind: auth.CredentialKindAPIKey, CredentialID: key.ID,
		BillingSubjectKind: auth.BillingSubjectAPIKey, BillingSubjectID: key.ID,
	}, billing.ReserveRequest{
		SubjectKeys: []billing.Subject{subject}, BillingSubject: subject, Capability: "chat",
		Model: "m", EndpointID: "ep", Estimate: billing.Usage{InputUnits: 40},
		Price: billing.PriceKey{SubjectType: billing.PriceSubjectModel, SubjectID: "m"},
	})
	if err != nil {
		t.Fatalf("reserve held: %v", err)
	}
	if _, err := service.Settle(ctx, auth.Principal{TenantID: f.tenant,
		CredentialKind: auth.CredentialKindAPIKey, CredentialID: key.ID,
		BillingSubjectKind: auth.BillingSubjectAPIKey, BillingSubjectID: key.ID,
	}, reservation.ID, billing.Usage{InputUnits: 30}); err != nil {
		t.Fatalf("settle: %v", err)
	}
	seedKeySettledUsage(t, f, "eur-1", key.ID, 10, 50, "EUR", time.Now().UTC())

	listReq := httptest.NewRequest(http.MethodGet, "/api/v1/admin/budgets", nil)
	rec = doBudgetHandler(f.app.handleAdminBudgets, admin, listReq)
	if rec.Code != http.StatusOK {
		t.Fatalf("list = %d: %s", rec.Code, rec.Body.String())
	}
	var out struct {
		Budgets []budgetView `json:"budgets"`
	}
	if err := json.Unmarshal(rec.Body.Bytes(), &out); err != nil {
		t.Fatalf("decode: %v (%s)", err, rec.Body.String())
	}
	if len(out.Budgets) != 1 {
		t.Fatalf("budgets = %d, want 1", len(out.Budgets))
	}
	view := out.Budgets[0]
	byCur := map[string]budgetLiabilityRow{}
	for _, row := range view.Liability {
		byCur[row.Currency] = row
	}
	usd, eur := byCur["USD"], byCur["EUR"]
	if usd.SettledMicros != 30 || usd.UnknownMicros != 0 || eur.SettledMicros != 50 {
		t.Fatalf("liability rows wrong: %+v", view.Liability)
	}
	if view.Progress.Counted != 30 || view.Progress.Currency != "USD" {
		t.Fatalf("progress must count same-currency settled only: %+v", view.Progress)
	}

	// Held rows count in their own field and toward counted.
	if _, err := service.Reserve(ctx, auth.Principal{
		TenantID: f.tenant, CredentialKind: auth.CredentialKindAPIKey, CredentialID: key.ID,
		BillingSubjectKind: auth.BillingSubjectAPIKey, BillingSubjectID: key.ID,
	}, billing.ReserveRequest{
		SubjectKeys: []billing.Subject{subject}, BillingSubject: subject, Capability: "chat",
		Model: "m", EndpointID: "ep", Estimate: billing.Usage{InputUnits: 12},
		Price: billing.PriceKey{SubjectType: billing.PriceSubjectModel, SubjectID: "m"},
	}); err != nil {
		t.Fatalf("reserve second: %v", err)
	}
	rec = doBudgetHandler(f.app.handleAdminBudgets, admin, listReq)
	if err := json.Unmarshal(rec.Body.Bytes(), &out); err != nil {
		t.Fatalf("decode: %v", err)
	}
	view = out.Budgets[0]
	if view.Progress.Counted != 42 {
		t.Fatalf("counted = %d, want 42 (held 12 + settled 30, EUR excluded)", view.Progress.Counted)
	}

	// PATCH returns the refreshed projection.
	patchReq := httptest.NewRequest(http.MethodPatch, "/api/v1/admin/budgets/"+budgetID, strings.NewReader(`{"limit_value":50}`))
	rec = doBudgetHandler(f.app.handleAdminBudgetByID, admin, patchReq)
	if rec.Code != http.StatusOK {
		t.Fatalf("patch = %d: %s", rec.Code, rec.Body.String())
	}
	if got := decodeBudgetView(t, rec.Body.String()); got.Progress.Counted != 42 || got.Progress.LimitValue != 50 {
		t.Fatalf("patched progress wrong: %+v", got.Progress)
	}
}

func TestAdminBudgetTokensProjectionCountsUnits(t *testing.T) {
	f := newBudgetFixture(t)
	key := seedOwnedKey(t, f, "tokens")
	admin := f.adminPrincipal()
	startsAt := time.Now().UTC().Add(-time.Hour).Format(time.RFC3339)
	rec := f.postBudget(t, admin, fmt.Sprintf(
		`{"name":"token cap","subject_type":"api_key","subject_id":%q,"kind":"tokens","limit_value":1000,"period":"monthly","starts_at":%q}`,
		key.ID, startsAt))
	if rec.Code != http.StatusCreated {
		t.Fatalf("create = %d: %s", rec.Code, rec.Body.String())
	}
	if view := decodeBudgetView(t, rec.Body.String()); view.Progress.Currency != "" || view.Progress.Kind != "tokens" {
		t.Fatalf("token budget progress wrong: %+v", view.Progress)
	}
	seedKeySettledUsage(t, f, "tok-1", key.ID, 600, 12345, "USD", time.Now().UTC())

	listReq := httptest.NewRequest(http.MethodGet, "/api/v1/admin/budgets", nil)
	rec = doBudgetHandler(f.app.handleAdminBudgets, admin, listReq)
	var out struct {
		Budgets []budgetView `json:"budgets"`
	}
	if err := json.Unmarshal(rec.Body.Bytes(), &out); err != nil {
		t.Fatalf("decode: %v (%s)", err, rec.Body.String())
	}
	if len(out.Budgets) != 1 || out.Budgets[0].Progress.Counted != 600 {
		t.Fatalf("token progress counts units not micros: %+v", out.Budgets)
	}
}
