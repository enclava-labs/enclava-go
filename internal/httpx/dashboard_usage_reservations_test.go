package httpx

import (
	"context"
	"fmt"
	"html"
	"net/http"
	"net/http/httptest"
	"regexp"
	"strings"
	"testing"

	"enclava-go/internal/billing"
)

func TestDashboardReservationsListAndResolve(t *testing.T) {
	f := newReservationFixture(t)
	ctx := context.Background()
	admin := f.adminPrincipal()
	held := seedHeldReservation(t, f, f.app.billingSvc, 40)
	unknown := seedHeldReservation(t, f, f.app.billingSvc, 60)
	if err := f.app.billingSvc.MarkUnknown(ctx, admin, unknown.ID); err != nil {
		t.Fatal(err)
	}
	render := func(state string) string {
		return renderUsagePage(t, f, f.app.handleDashboardUsageReservationsPage, "/dashboard/usage/reservations?state="+state)
	}
	unknownPage := render("unknown")
	if !strings.Contains(unknownPage, unknown.ID) || strings.Contains(unknownPage, held.ID) || !strings.Contains(unknownPage, "Resolve") {
		t.Fatalf("unknown view missing reservation or action: %s", unknownPage)
	}
	heldPage := render("held")
	if !strings.Contains(heldPage, held.ID) || strings.Contains(heldPage, unknown.ID) || !strings.Contains(heldPage, "reservation-drawer?id="+held.ID) {
		t.Fatalf("held view missing reservation or resolve action: %s", heldPage)
	}
	drawer := renderUsagePage(t, f, f.app.handleDashboardUsageReservationDrawer, "/dashboard/partials/usage/reservation-drawer?id="+unknown.ID)
	for _, want := range []string{unknown.ID, "input_tokens", "cached_input_tokens", "output_tokens", "Resolve reservation"} {
		if !strings.Contains(drawer, want) {
			t.Fatalf("drawer missing %q: %s", want, drawer)
		}
	}
	req := httptest.NewRequest(http.MethodPost, "/api/v1/admin/reservations/"+unknown.ID+"/resolve", strings.NewReader(`{"input_tokens":12,"cached_input_tokens":0,"output_tokens":0}`))
	rec := doBudgetHandler(f.app.handleAdminReservationByID, admin, req)
	if rec.Code != http.StatusOK {
		t.Fatalf("resolve = %d: %s", rec.Code, rec.Body.String())
	}
	if page := render("unknown"); strings.Contains(page, unknown.ID) {
		t.Fatalf("settled reservation remains listed: %s", page)
	}
	reservation, err := f.app.billingSvc.GetReservation(ctx, admin, unknown.ID)
	if err != nil || reservation.State != billing.ReservationSettled {
		t.Fatalf("reservation = %+v, err %v; want settled", reservation, err)
	}
	record, err := f.repo.GetUsageRecord(ctx, f.tenant, unknown.ID)
	if err != nil || record.Units[billing.UnitInput] != 12 || record.AmountMicros != 1200 || record.Outcome != billing.OutcomeReconciled {
		t.Fatalf("usage = %+v, err %v; want reconciled 12 input units and 1200 micros", record, err)
	}
	req = httptest.NewRequest(http.MethodPost, "/api/v1/admin/reservations/"+held.ID+"/resolve", strings.NewReader(`{"input_tokens":5,"cached_input_tokens":0,"output_tokens":0}`))
	rec = doBudgetHandler(f.app.handleAdminReservationByID, admin, req)
	if rec.Code != http.StatusOK {
		t.Fatalf("resolve held = %d: %s", rec.Code, rec.Body.String())
	}
	if page := render("held"); strings.Contains(page, held.ID) {
		t.Fatalf("settled held reservation remains listed: %s", page)
	}
	reservation, err = f.app.billingSvc.GetReservation(ctx, admin, held.ID)
	if err != nil || reservation.State != billing.ReservationSettled {
		t.Fatalf("held reservation = %+v, err %v; want settled", reservation, err)
	}
	record, err = f.repo.GetUsageRecord(ctx, f.tenant, held.ID)
	if err != nil || record.Units[billing.UnitInput] != 5 || record.AmountMicros != 500 || record.Outcome != billing.OutcomeReconciled {
		t.Fatalf("held usage = %+v, err %v; want reconciled 5 input units and 500 micros", record, err)
	}
}

func TestDashboardReservationsCursorPagination(t *testing.T) {
	f := newReservationFixture(t)
	const count = reservationsListCap + 3
	if _, err := f.db.SQL.ExecContext(context.Background(), `
		INSERT INTO reservations
			(tenant_id, id, subject_type, subject_id, price_snapshot, reserved_units,
			 limit_amount_micros, currency, state, capability, model)
		SELECT $1::uuid, 'res_view_' || lpad(g::text, 4, '0'), 'api_key', 'key',
		       '{"rates":[],"currency":"USD","rounding_rule":"ceil"}'::jsonb,
		       '{"input":1}'::jsonb, 1, 'USD', 'unknown', 'chat', 'm'
		FROM generate_series(1, $2) g`, f.tenant, count); err != nil {
		t.Fatalf("seed reservations: %v", err)
	}
	first := renderUsagePage(t, f, f.app.handleDashboardUsageReservationsPage, "/dashboard/usage/reservations?state=unknown")
	match := regexp.MustCompile(`href="([^"]+)">Next page</a>`).FindStringSubmatch(first)
	if len(match) != 2 {
		t.Fatalf("first page missing next link: %s", first)
	}
	next := html.UnescapeString(match[1])
	if !strings.Contains(next, "state=unknown") || !strings.Contains(next, "cursor=") {
		t.Fatalf("next link lost state or cursor: %s", next)
	}
	second := renderUsagePage(t, f, f.app.handleDashboardUsageReservationsPage, next)
	if strings.Contains(second, ">Next page</a>") {
		t.Fatalf("last page has next link: %s", second)
	}
	for i := 1; i <= count; i++ {
		id := fmt.Sprintf("res_view_%04d", i)
		// Each row displays its id and references it in the drawer URL.
		if strings.Count(first, id)+strings.Count(second, id) != 2 {
			t.Fatalf("reservation %s is missing or repeated across pages", id)
		}
	}
}

func TestDashboardReservationsRequiresBillingAccess(t *testing.T) {
	f := newIdentityFixture(t)
	session, _ := loginUser(t, f, "reservations-member@example.com", []string{"chat.completions"}, "member-pass")
	for _, path := range []string{
		"/dashboard/usage/reservations",
		"/dashboard/partials/usage/reservations",
		"/dashboard/partials/usage/reservation-drawer?id=res_example",
	} {
		rec := sessionDo(t, f.app, http.MethodGet, path, "", session, nil)
		if rec.Code != http.StatusForbidden {
			t.Fatalf("member GET %s = %d, want 403: %s", path, rec.Code, rec.Body.String())
		}
	}
}
