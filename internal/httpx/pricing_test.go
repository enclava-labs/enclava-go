// pricing_test.go — admin price create/update input validation: the
// currency must carry the same ISO-4217 alpha-3 shape the platform bills
// in (uppercase A–Z, CHAR(3)) — the budget writer enforces it through the
// same validator.
package httpx

import (
	"testing"
)

func TestPriceFromAdminRequestValidatesCurrency(t *testing.T) {
	base := adminPricingCreateRequest{
		SubjectType:  "model",
		SubjectID:    "m",
		Capability:   "chat",
		Unit:         "input",
		RateMicros:   1,
		UnitQuantity: 1,
	}

	for _, bad := range []string{"usd", "U$D", "12A", "US", "USDD"} {
		req := base
		req.Currency = bad
		if _, _, err := priceFromAdminRequest(req); err == nil {
			t.Fatalf("currency %q must be rejected", bad)
		}
	}

	req := base
	req.Currency = "USD"
	price, _, err := priceFromAdminRequest(req)
	if err != nil || price.Currency != "USD" {
		t.Fatalf("USD must be accepted, got %v / %+v", err, price)
	}

	// An omitted currency defaults to USD.
	req.Currency = ""
	price, _, err = priceFromAdminRequest(req)
	if err != nil || price.Currency != "USD" {
		t.Fatalf("empty currency must default to USD, got %v / %+v", err, price)
	}
}
