// billing_reservations_test.go — the platform.admin reconciliation API: a
// tenant-scoped list over canonical reservation rows plus resolve-through-
// billing.Service.ResolveOutstanding (reconcile or write off). A resolved
// reservation leaves the outstanding set, so it stops counting as unsettled
// liability everywhere the reports read it.
package httpx

import (
	"context"
	"encoding/json"
	"fmt"
	"net/http"
	"net/http/httptest"
	"net/url"
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

	// A held reservation can be reconciled and leaves the outstanding list.
	if rec := resolve(held.ID, `{"input_tokens":1,"cached_input_tokens":0,"output_tokens":0}`); rec.Code != http.StatusOK {
		t.Fatalf("resolve held = %d: %s", rec.Code, rec.Body.String())
	}
	if rows := listIDs("?state=held"); len(rows) != 0 {
		t.Fatalf("resolved held reservation still listed: %+v", rows)
	}
	// Negative and missing token counts are refused.
	if rec := resolve(unknown.ID, `{"input_tokens":-1,"cached_input_tokens":0,"output_tokens":0}`); rec.Code != http.StatusBadRequest {
		t.Fatalf("negative tokens = %d, want 400", rec.Code)
	}
	if rec := resolve(unknown.ID, `{"input_tokens":3}`); rec.Code != http.StatusBadRequest {
		t.Fatalf("missing fields = %d, want 400", rec.Code)
	}
	// Cached is a subset of input — never larger.
	if rec := resolve(unknown.ID, `{"input_tokens":3,"cached_input_tokens":4,"output_tokens":0}`); rec.Code != http.StatusBadRequest {
		t.Fatalf("cached > input = %d, want 400: %s", rec.Code, rec.Body.String())
	}
	// Unknown id is one indistinguishable 404.
	if rec := resolve("res_nope", `{"input_tokens":0,"cached_input_tokens":0,"output_tokens":0}`); rec.Code != http.StatusNotFound {
		t.Fatalf("resolve missing = %d, want 404", rec.Code)
	}

	// Reconcile the unknown reservation: it settles and leaves the list.
	// input_tokens carries its cached subset (60 = 4 cached + 56 uncached).
	rec := resolve(unknown.ID, `{"input_tokens":60,"cached_input_tokens":4,"output_tokens":10}`)
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

func TestAdminReservationsListPaginatesCursor(t *testing.T) {
	f := newReservationFixture(t)
	admin := f.adminPrincipal()
	ctx := context.Background()

	// Seed more outstanding rows than the fixed page cap in one bulk insert;
	// the shared created_at exercises the id-DESC keyset tiebreak.
	const seeded = reservationsListCap + 3
	if _, err := f.db.SQL.ExecContext(ctx, `
		INSERT INTO reservations
			(tenant_id, id, subject_type, subject_id, price_snapshot,
			 reserved_units, limit_amount_micros, currency, state, capability, model)
		SELECT $1::uuid, 'res_page_' || g, 'api_key', 'k-page',
		       '{"rates":[],"currency":"USD","rounding_rule":"ceil"}'::jsonb,
		       '{"input":1}'::jsonb, 1, 'USD', 'unknown', 'chat', 'm'
		FROM generate_series(1, $2) g`, f.tenant, seeded); err != nil {
		t.Fatalf("bulk seed reservations: %v", err)
	}

	type listBody struct {
		Reservations []struct {
			ID string `json:"id"`
		} `json:"reservations"`
		NextCursor string `json:"next_cursor"`
	}
	get := func(query string) listBody {
		t.Helper()
		req := httptest.NewRequest(http.MethodGet, "/api/v1/admin/reservations?state=unknown"+query, nil)
		rec := doBudgetHandler(f.app.handleAdminReservations, admin, req)
		if rec.Code != http.StatusOK {
			t.Fatalf("list %s = %d: %s", query, rec.Code, rec.Body.String())
		}
		var body listBody
		if err := json.Unmarshal(rec.Body.Bytes(), &body); err != nil {
			t.Fatalf("decode page: %v", err)
		}
		return body
	}

	first := get("")
	if len(first.Reservations) != reservationsListCap || first.NextCursor == "" {
		t.Fatalf("page 1 = %d rows cursor %q, want a capped page with next_cursor",
			len(first.Reservations), first.NextCursor)
	}
	second := get("&cursor=" + url.QueryEscape(first.NextCursor))
	if len(second.Reservations) != seeded-reservationsListCap || second.NextCursor != "" {
		t.Fatalf("page 2 = %d rows cursor %q, want the remainder with no next_cursor",
			len(second.Reservations), second.NextCursor)
	}
	seen := map[string]bool{}
	for _, page := range []listBody{first, second} {
		for _, row := range page.Reservations {
			if seen[row.ID] {
				t.Fatalf("row repeated across pages: %s", row.ID)
			}
			seen[row.ID] = true
		}
	}
	if len(seen) != seeded {
		t.Fatalf("pages covered %d rows, want all %d", len(seen), seeded)
	}

	// A malformed cursor is a 400, never an ignored parameter.
	req := httptest.NewRequest(http.MethodGet, "/api/v1/admin/reservations?state=unknown&cursor=not-a-cursor", nil)
	if rec := doBudgetHandler(f.app.handleAdminReservations, admin, req); rec.Code != http.StatusBadRequest {
		t.Fatalf("malformed cursor = %d, want 400: %s", rec.Code, rec.Body.String())
	}
}

// TestAdminReservationResolveReconcilesCachedSubset: resolve treats
// input_tokens exactly like the upstream usage object — cached_input_tokens
// is a subset of it, settled disjointly as input-minus-cached, so cached
// tokens are never billed twice.
func TestAdminReservationResolveReconcilesCachedSubset(t *testing.T) {
	f := newReservationFixture(t)
	svc := f.app.billingSvc
	admin := f.adminPrincipal()
	ctx := context.Background()

	for id, p := range map[string]billing.Price{
		"resv-sub-in":  {SubjectType: billing.PriceSubjectModel, SubjectID: "m", Capability: "chat", Unit: billing.UnitInput, RateMicros: 5, UnitQuantity: 1, Currency: "USD"},
		"resv-sub-cin": {SubjectType: billing.PriceSubjectModel, SubjectID: "m", Capability: "chat", Unit: billing.UnitCachedInput, RateMicros: 1, UnitQuantity: 1, Currency: "USD"},
		"resv-sub-out": {SubjectType: billing.PriceSubjectModel, SubjectID: "m", Capability: "chat", Unit: billing.UnitOutput, RateMicros: 3, UnitQuantity: 1, Currency: "USD"},
	} {
		if err := f.repo.PutPrice(ctx, f.tenant, id, p, "tester", time.Now().UTC().Add(-time.Minute)); err != nil {
			t.Fatalf("put price %s: %v", id, err)
		}
	}
	p := auth.Principal{TenantID: f.tenant}
	res, err := svc.Reserve(ctx, p, billing.ReserveRequest{
		Capability: "chat", Model: "m", EndpointID: "ep",
		Estimate: billing.Usage{InputUnits: 10, CachedInputUnits: 5, OutputUnits: 10},
		Price:    billing.PriceKey{SubjectType: billing.PriceSubjectModel, SubjectID: "m"},
	})
	if err != nil {
		t.Fatalf("reserve: %v", err)
	}
	if err := svc.MarkUnknown(ctx, admin, res.ID); err != nil {
		t.Fatalf("mark unknown: %v", err)
	}

	// {input 12, cached 4, output 8} must settle {input 8, cached_input 4,
	// output 8} — 8×5 + 4×1 + 8×3 = 68 micros, not input-verbatim's 88.
	req := httptest.NewRequest(http.MethodPost, "/api/v1/admin/reservations/"+res.ID+"/resolve",
		strings.NewReader(`{"input_tokens":12,"cached_input_tokens":4,"output_tokens":8}`))
	rec := doBudgetHandler(f.app.handleAdminReservationByID, admin, req)
	if rec.Code != http.StatusOK {
		t.Fatalf("resolve = %d: %s", rec.Code, rec.Body.String())
	}
	var body struct {
		UsageRecord struct {
			Units        map[string]int64 `json:"units"`
			AmountMicros int64            `json:"amountMicros"`
			Outcome      string           `json:"outcome"`
		} `json:"usage_record"`
	}
	if err := json.Unmarshal(rec.Body.Bytes(), &body); err != nil {
		t.Fatalf("decode resolve body: %v", err)
	}
	want := map[string]int64{"input": 8, "cached_input": 4, "output": 8}
	for k, v := range want {
		if body.UsageRecord.Units[k] != v {
			t.Fatalf("units[%q] = %d, want %d (all units %+v)", k, body.UsageRecord.Units[k], v, body.UsageRecord.Units)
		}
	}
	if body.UsageRecord.AmountMicros != 68 || body.UsageRecord.Outcome != billing.OutcomeReconciled {
		t.Fatalf("reconciled record = %+v, want amount 68/outcome reconciled", body.UsageRecord)
	}
}
