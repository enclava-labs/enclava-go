package httpx

// pricing_batch_test.go — the atomic multi-unit pricing write (P-05): one
// POST carries every Set-price row; a single invalid unit rejects the whole
// batch and nothing lands.

import (
	"context"
	"net/http"
	"net/http/httptest"
	"strings"
	"testing"
	"time"

	"enclava-go/internal/auth"
	"enclava-go/internal/billing"
)

func pricingBatchPrincipal(tenant string) auth.Principal {
	return auth.Principal{TenantID: tenant, ActorID: "admin-1", ActorKind: auth.ActorKindUser}
}

func TestAdminPricingBatchAllOrNothing(t *testing.T) {
	f := newBudgetFixture(t)
	ctx := context.Background()
	principal := pricingBatchPrincipal(f.tenant)
	post := func(body string) *httptest.ResponseRecorder {
		req := httptest.NewRequest(http.MethodPost, "/api/v1/admin/pricing", strings.NewReader(body))
		return doBudgetHandler(f.app.handleAdminPricing, principal, req)
	}

	// One invalid unit rejects the entire batch — neither price is written.
	body := `{"prices":[` +
		`{"subject_type":"model","subject_id":"m1","capability":"chat","unit":"input","rate_micros":100,"unit_quantity":1},` +
		`{"subject_type":"model","subject_id":"m1","capability":"chat","unit":"bogus","rate_micros":100,"unit_quantity":1}` +
		`]}`
	if rec := post(body); rec.Code != http.StatusBadRequest {
		t.Fatalf("invalid unit batch = %d, want 400: %s", rec.Code, rec.Body.String())
	}
	got, err := f.repo.LatestPrices(ctx, f.tenant, billing.PriceSubjectModel, "m1", "chat", time.Now().UTC())
	if err != nil {
		t.Fatalf("latest prices: %v", err)
	}
	if len(got) != 0 {
		t.Fatalf("invalid batch wrote %d rows, want 0", len(got))
	}

	// A duplicate key inside one batch is rejected before the store is touched.
	dup := `{"prices":[` +
		`{"subject_type":"model","subject_id":"m1","capability":"chat","unit":"input","rate_micros":100,"unit_quantity":1},` +
		`{"subject_type":"model","subject_id":"m1","capability":"chat","unit":"input","rate_micros":200,"unit_quantity":1}` +
		`]}`
	if rec := post(dup); rec.Code != http.StatusBadRequest {
		t.Fatalf("duplicate batch = %d, want 400: %s", rec.Code, rec.Body.String())
	}

	now := time.Now().UTC()
	p := billing.Price{SubjectType: billing.PriceSubjectModel, SubjectID: "rollback", Capability: "chat", Unit: billing.UnitInput, RateMicros: 1, UnitQuantity: 1, Currency: "USD"}
	invalid := p
	invalid.Unit = billing.UnitOutput
	invalid.UnitQuantity = 0
	err = f.repo.PutPrices(ctx, f.tenant, []billing.PriceWrite{
		{ID: "batch-valid", Price: p, ActorID: principal.ActorID, EffectiveFrom: now},
		{ID: "batch-invalid", Price: invalid, ActorID: principal.ActorID, EffectiveFrom: now},
	})
	if err == nil {
		t.Fatal("invalid second database row must fail")
	}
	rolledBack, err := f.repo.LatestPrices(ctx, f.tenant, billing.PriceSubjectModel, "rollback", "chat", time.Now().UTC())
	if err != nil || len(rolledBack) != 0 {
		t.Fatalf("failed batch left rows %v, err %v", rolledBack, err)
	}

	// Three categories in one request commit together.
	good := `{"prices":[` +
		`{"subject_type":"model","subject_id":"m1","capability":"chat","unit":"input","rate_micros":100,"unit_quantity":1000000},` +
		`{"subject_type":"model","subject_id":"m1","capability":"chat","unit":"cached_input","rate_micros":50,"unit_quantity":1000000},` +
		`{"subject_type":"model","subject_id":"m1","capability":"chat","unit":"output","rate_micros":400,"unit_quantity":1000000}` +
		`]}`
	if rec := post(good); rec.Code != http.StatusCreated {
		t.Fatalf("valid batch = %d, want 201: %s", rec.Code, rec.Body.String())
	}
	got, err = f.repo.LatestPrices(ctx, f.tenant, billing.PriceSubjectModel, "m1", "chat", time.Now().UTC())
	if err != nil {
		t.Fatalf("latest prices: %v", err)
	}
	if len(got) != 3 {
		t.Fatalf("batch wrote %d rows, want 3", len(got))
	}
	rates := map[billing.UnitKind]int64{}
	for _, p := range got {
		rates[p.Unit] = p.RateMicros
	}
	if rates[billing.UnitInput] != 100 || rates[billing.UnitCachedInput] != 50 || rates[billing.UnitOutput] != 400 {
		t.Fatalf("batch rates = %v", rates)
	}
}
