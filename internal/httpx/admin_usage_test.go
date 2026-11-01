// admin_usage_test.go — GET /api/v1/admin/usage/summary: total_units counts
// canonical settled usage once (per-key attribution rows never inflate it)
// and the unsettled liability payload splits held/unknown money per subject
// and per currency.
package httpx

import (
	"context"
	"encoding/json"
	"net/http"
	"net/http/httptest"
	"testing"
	"time"

	"enclava-go/internal/auth"
	"enclava-go/internal/billing"
)

func TestAdminUsageSummaryTotalsAndUnsettled(t *testing.T) {
	f := newBudgetFixture(t)
	svc := billing.NewService(f.repo)
	admin := f.adminPrincipal()
	ctx := context.Background()
	key := seedOwnedKey(t, f, "summ")

	if err := f.repo.PutPrice(ctx, f.tenant, "sum-price", billing.Price{
		SubjectType: billing.PriceSubjectModel, SubjectID: "m", Capability: "chat",
		Unit: billing.UnitInput, RateMicros: 100, UnitQuantity: 1, Currency: "USD",
	}, "tester", time.Now().UTC().Add(-time.Minute)); err != nil {
		t.Fatalf("put price: %v", err)
	}
	keyPrincipal := auth.Principal{
		TenantID: f.tenant, CredentialKind: auth.CredentialKindAPIKey, CredentialID: key.ID,
		BillingSubjectKind: auth.BillingSubjectUser, BillingSubjectID: "owner-1",
	}
	keySubject := billing.Subject{Type: billing.BudgetSubjectAPIKey, ID: key.ID}
	reserve := func(units int64) billing.Reservation {
		t.Helper()
		res, err := svc.Reserve(ctx, keyPrincipal, billing.ReserveRequest{
			SubjectKeys:    []billing.Subject{keySubject},
			BillingSubject: billing.Subject{Type: billing.BudgetSubjectUser, ID: "owner-1"},
			Capability:     "chat", Model: "m", EndpointID: "ep",
			Estimate: billing.Usage{InputUnits: units},
			Price:    billing.PriceKey{SubjectType: billing.PriceSubjectModel, SubjectID: "m"},
		})
		if err != nil {
			t.Fatalf("reserve: %v", err)
		}
		return res
	}

	// One settled key-admitted charge (30 units → 3000 micros), one held
	// (40 → 4000) and one unknown (60 → 6000). Settling writes a usage
	// record under the owner plus the credential attribution — the
	// attribution arm is exactly what used to double-count total_units.
	settled := reserve(30)
	if _, err := svc.Settle(ctx, keyPrincipal, settled.ID, billing.Usage{InputUnits: 30}); err != nil {
		t.Fatalf("settle: %v", err)
	}
	held := reserve(40)
	unknown := reserve(60)
	if err := svc.MarkUnknown(ctx, keyPrincipal, unknown.ID); err != nil {
		t.Fatalf("mark unknown: %v", err)
	}
	// An unknown reservation created before the window does not count.
	old := reserve(50)
	if err := svc.MarkUnknown(ctx, keyPrincipal, old.ID); err != nil {
		t.Fatalf("mark unknown old: %v", err)
	}
	resUpd, err := f.db.SQL.ExecContext(ctx,
		`UPDATE reservations SET created_at = $3 WHERE tenant_id = $1::uuid AND (id = $2 OR id LIKE $2 || '/%')`,
		f.tenant, old.ID, time.Now().UTC().AddDate(0, 0, -60))
	if err != nil {
		t.Fatalf("backdate unknown: %v", err)
	}
	if n, _ := resUpd.RowsAffected(); n == 0 {
		t.Fatalf("backdate matched no rows for %q", old.ID)
	}

	req := httptest.NewRequest(http.MethodGet, "/api/v1/admin/usage/summary", nil)
	rec := doBudgetHandler(f.app.handleAdminUsageSummary, admin, req)
	if rec.Code != http.StatusOK {
		t.Fatalf("summary = %d: %s", rec.Code, rec.Body.String())
	}
	var body struct {
		Usage []struct {
			SubjectType string `json:"SubjectType"`
			SubjectID   string `json:"SubjectID"`
			TotalUnits  int64  `json:"TotalUnits"`
		} `json:"usage"`
		TotalUnits int64                 `json:"total_units"`
		Unsettled  []unsettledSubjectDTO `json:"unsettled"`
		Totals     []unsettledSubjectDTO `json:"unsettled_totals"`
	}
	if err := json.Unmarshal(rec.Body.Bytes(), &body); err != nil {
		t.Fatalf("decode: %v (%s)", err, rec.Body.String())
	}
	// Canonical settled usage is 30 units exactly once — the api_key
	// attribution row adds its own 30 in the usage list but must not reach
	// the total.
	if body.TotalUnits != 30 {
		t.Fatalf("total_units = %d, want 30 (canonical only)", body.TotalUnits)
	}
	var ownerUnits, keyUnits int64
	for _, u := range body.Usage {
		switch {
		case u.SubjectType == "user" && u.SubjectID == "owner-1":
			ownerUnits = u.TotalUnits
		case u.SubjectType == "api_key" && u.SubjectID == key.ID:
			keyUnits = u.TotalUnits
		}
	}
	if ownerUnits != 30 || keyUnits != 30 {
		t.Fatalf("per-subject usage wrong: owner=%d key=%d", ownerUnits, keyUnits)
	}

	// Unsettled: the owner subject sees held 4000 + in-window unknown 6000
	// (the backdated unknown is out of window); the key sees the same money
	// through its attribution rows; tenant totals count canonical rows only.
	find := func(rows []unsettledSubjectDTO, typ, id string) *unsettledSubjectDTO {
		for i := range rows {
			if rows[i].SubjectType == typ && rows[i].SubjectID == id {
				return &rows[i]
			}
		}
		return nil
	}
	owner := find(body.Unsettled, "user", "owner-1")
	if owner == nil || owner.HeldMicros != 4000 || owner.UnknownMicros != 6000 || owner.UnsettledMicros != 10000 {
		t.Fatalf("owner unsettled wrong: %+v", owner)
	}
	keyRow := find(body.Unsettled, "api_key", key.ID)
	if keyRow == nil || keyRow.HeldMicros != 4000 || keyRow.UnknownMicros != 6000 {
		t.Fatalf("key unsettled wrong: %+v", keyRow)
	}
	if len(body.Totals) != 1 || body.Totals[0].Currency != "USD" || body.Totals[0].UnsettledMicros != 10000 {
		t.Fatalf("unsettled totals wrong: %+v", body.Totals)
	}
	_ = held
}
