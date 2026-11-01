// billing_reservations_test.go — the platform.admin reconciliation API: a
// tenant-scoped list over canonical reservation rows plus resolve-through-
// billing.Service.ResolveUnknown (reconcile or write-off). A resolved
// reservation leaves the outstanding set, so it stops counting as unsettled
// liability everywhere the reports read it.
package httpx

import (
	"context"
	"encoding/json"
	"fmt"
	"net/http"
	"net/http/httptest"
	"strings"
	"testing"
	"time"

	"enclava-go/internal/auth"
	"enclava-go/internal/billing"
)

// reservationFixture adds the settle authority the resolve path needs.
func newReservationFixture(t *testing.T) budgetFixture {
	t.Helper()
	f := newBudgetFixture(t)
	f.app.billingSvc = billing.NewService(f.repo)
	return f
}

// seedHeldReservation reserves one real held reservation against a USD
// model price for the tenant billing subject.
func seedHeldReservation(t *testing.T, f budgetFixture, svc *billing.Service, units int64) billing.Reservation {
	t.Helper()
	ctx := context.Background()
	if err := f.repo.PutPrice(ctx, f.tenant, "resv-price-"+fmt.Sprint(units), billing.Price{
		SubjectType: billing.PriceSubjectModel, SubjectID: "m", Capability: "chat",
		Unit: billing.UnitInput, RateMicros: 100, UnitQuantity: 1, Currency: "USD",
	}, "tester", time.Now().UTC().Add(-time.Minute)); err != nil {
		t.Fatalf("put price: %v", err)
	}
	p := auth.Principal{TenantID: f.tenant}
	res, err := svc.Reserve(ctx, p, billing.ReserveRequest{
		Capability: "chat", Model: "m", EndpointID: "ep",
		Estimate: billing.Usage{InputUnits: units},
		Price:    billing.PriceKey{SubjectType: billing.PriceSubjectModel, SubjectID: "m"},
	})
	if err != nil {
		t.Fatalf("reserve: %v", err)
	}
	return res
}

func TestAdminReservationsListAndResolve(t *testing.T) {
	f := newReservationFixture(t)
	svc := f.app.billingSvc
	admin := f.adminPrincipal()
	ctx := context.Background()

	held := seedHeldReservation(t, f, svc, 40)
	unknown := seedHeldReservation(t, f, svc, 60)
	if err := svc.MarkUnknown(ctx, admin, unknown.ID); err != nil {
		t.Fatalf("mark unknown: %v", err)
	}

	get := func(query string) *httptest.ResponseRecorder {
		t.Helper()
		req := httptest.NewRequest(http.MethodGet, "/api/v1/admin/reservations"+query, nil)
		return doBudgetHandler(f.app.handleAdminReservations, admin, req)
	}

	if rec := get(""); rec.Code != http.StatusBadRequest {
		t.Fatalf("missing state = %d, want 400", rec.Code)
	}
	if rec := get("?state=settled"); rec.Code != http.StatusBadRequest {
		t.Fatalf("bad state = %d, want 400", rec.Code)
	}

	type row struct {
		ID            string           `json:"id"`
		SubjectType   string           `json:"subject_type"`
		BoundMicros   int64            `json:"bound_micros"`
		Currency      string           `json:"currency"`
		ReservedUnits map[string]int64 `json:"reserved_units"`
		Dispatched    bool             `json:"dispatched"`
	}
	listIDs := func(query string) []row {
		t.Helper()
		rec := get(query)
		if rec.Code != http.StatusOK {
			t.Fatalf("list %s = %d: %s", query, rec.Code, rec.Body.String())
		}
		var body struct {
			Reservations []row `json:"reservations"`
		}
		if err := json.Unmarshal(rec.Body.Bytes(), &body); err != nil {
			t.Fatalf("decode list: %v", err)
		}
		return body.Reservations
	}

	heldRows := listIDs("?state=held")
	if len(heldRows) != 1 || heldRows[0].ID != held.ID {
		t.Fatalf("held list wrong: %+v", heldRows)
	}
	if heldRows[0].BoundMicros != 4000 || heldRows[0].Currency != "USD" || heldRows[0].ReservedUnits["input"] != 40 {
		t.Fatalf("held row shape wrong: %+v", heldRows[0])
	}
	unknownRows := listIDs("?state=unknown")
	if len(unknownRows) != 1 || unknownRows[0].ID != unknown.ID {
		t.Fatalf("unknown list wrong: %+v", unknownRows)
	}
	// The tenant attribution row must never surface as its own list entry.
	for _, r := range append(heldRows, unknownRows...) {
		if strings.Contains(r.ID, "/") {
			t.Fatalf("attribution row leaked into canonical list: %+v", r)
		}
	}

	resolve := func(id, body string) *httptest.ResponseRecorder {
		t.Helper()
		req := httptest.NewRequest(http.MethodPost, "/api/v1/admin/reservations/"+id+"/resolve", strings.NewReader(body))
		return doBudgetHandler(f.app.handleAdminReservationByID, admin, req)
	}

	// A held reservation is the wrong state.
	if rec := resolve(held.ID, `{"input_tokens":1,"cached_input_tokens":0,"output_tokens":0}`); rec.Code != http.StatusConflict {
		t.Fatalf("resolve held = %d, want 409: %s", rec.Code, rec.Body.String())
	}
	// Negative and missing token counts are refused.
	if rec := resolve(unknown.ID, `{"input_tokens":-1,"cached_input_tokens":0,"output_tokens":0}`); rec.Code != http.StatusBadRequest {
		t.Fatalf("negative tokens = %d, want 400", rec.Code)
	}
	if rec := resolve(unknown.ID, `{"input_tokens":3}`); rec.Code != http.StatusBadRequest {
		t.Fatalf("missing fields = %d, want 400", rec.Code)
	}
	// Unknown id is one indistinguishable 404.
	if rec := resolve("res_nope", `{"input_tokens":0,"cached_input_tokens":0,"output_tokens":0}`); rec.Code != http.StatusNotFound {
		t.Fatalf("resolve missing = %d, want 404", rec.Code)
	}

	// Reconcile the unknown reservation: it settles and leaves the list.
	rec := resolve(unknown.ID, `{"input_tokens":55,"cached_input_tokens":5,"output_tokens":10}`)
	if rec.Code != http.StatusOK {
		t.Fatalf("resolve = %d: %s", rec.Code, rec.Body.String())
	}
	if rows := listIDs("?state=unknown"); len(rows) != 0 {
		t.Fatalf("resolved reservation still listed: %+v", rows)
	}
	rec2 := resolve(unknown.ID, `{"input_tokens":0,"cached_input_tokens":0,"output_tokens":0}`)
	if rec2.Code != http.StatusOK {
		t.Fatalf("resolve settled = %d, want idempotent 200: %s", rec2.Code, rec2.Body.String())
	}

	// A second unknown reservation resolves to a zero-usage write-off.
	writeoff := seedHeldReservation(t, f, svc, 20)
	if err := svc.MarkUnknown(ctx, admin, writeoff.ID); err != nil {
		t.Fatalf("mark unknown 2: %v", err)
	}
	if rec := resolve(writeoff.ID, `{"input_tokens":0,"cached_input_tokens":0,"output_tokens":0}`); rec.Code != http.StatusOK {
		t.Fatalf("write-off = %d: %s", rec.Code, rec.Body.String())
	}
	var state string
	if err := f.db.SQL.QueryRowContext(ctx,
		`SELECT state FROM reservations WHERE tenant_id = $1::uuid AND id = $2`,
		f.tenant, writeoff.ID).Scan(&state); err != nil || state != billing.ReservationSettled {
		t.Fatalf("write-off state = %q (%v), want settled", state, err)
	}
	var amount int64
	if err := f.db.SQL.QueryRowContext(ctx,
		`SELECT amount_micros FROM usage_records WHERE tenant_id = $1::uuid AND reservation_id = $2`,
		f.tenant, writeoff.ID).Scan(&amount); err != nil || amount != 0 {
		t.Fatalf("write-off amount = %d (%v), want 0", amount, err)
	}
}

func TestAdminReservationsCrossTenantHidden(t *testing.T) {
	f := newReservationFixture(t)
	svc := f.app.billingSvc
	admin := f.adminPrincipal()

	res := seedHeldReservation(t, f, svc, 10)
	if err := svc.MarkUnknown(context.Background(), admin, res.ID); err != nil {
		t.Fatalf("mark unknown: %v", err)
	}

	foreignDB := newReservationFixture(t)
	foreign := foreignDB.adminPrincipal()
	req := httptest.NewRequest(http.MethodGet, "/api/v1/admin/reservations?state=unknown", nil)
	rec := doBudgetHandler(foreignDB.app.handleAdminReservations, foreign, req)
	if rec.Code != http.StatusOK || strings.Contains(rec.Body.String(), res.ID) {
		t.Fatalf("foreign tenant must not see the reservation: %d %s", rec.Code, rec.Body.String())
	}
	resolve := httptest.NewRequest(http.MethodPost, "/api/v1/admin/reservations/"+res.ID+"/resolve",
		strings.NewReader(`{"input_tokens":1,"cached_input_tokens":0,"output_tokens":0}`))
	if rec := doBudgetHandler(foreignDB.app.handleAdminReservationByID, foreign, resolve); rec.Code != http.StatusNotFound {
		t.Fatalf("cross-tenant resolve = %d, want 404", rec.Code)
	}
}
