package httpx

import (
	"log"
	"net/http"
	"strings"
	"time"

	"enclava-go/internal/auth"
	"enclava-go/internal/billing"
)

// adminPricingCreateRequest creates one billing price row: a rational rate
// (rate_micros per unit_quantity) for one unit kind under a price key.
type adminPricingCreateRequest struct {
	SubjectType   string `json:"subject_type"`
	SubjectID     string `json:"subject_id"`
	Capability    string `json:"capability"`
	Unit          string `json:"unit"`
	RateMicros    int64  `json:"rate_micros"`
	UnitQuantity  int64  `json:"unit_quantity"`
	Currency      string `json:"currency,omitempty"`
	EffectiveFrom string `json:"effective_from,omitempty"`
}

// adminPricingWriteRequest is the pricing POST body: the single-price fields
// above, or a `prices` array written atomically — the dashboard Set-price
// form saves every entered category in one transaction, so a rejected unit
// can never leave a partial price set.
type adminPricingWriteRequest struct {
	adminPricingCreateRequest
	Prices []adminPricingCreateRequest `json:"prices,omitempty"`
}

// maxBatchPrices bounds one atomic write well past the three displayed
// categories — a bigger array is a malformed request, not a workload.
const maxBatchPrices = 12

func (a *App) handleAdminPricing(w http.ResponseWriter, r *http.Request, principal auth.Principal) {
	setNoStoreHeaders(w)
	if a.billingRepo == nil {
		WriteOpenAIError(w, http.StatusNotImplemented, "pricing is not configured", "server_error", "not_configured")
		return
	}
	switch r.Method {
	case http.MethodGet:
		limit, offset := paginationFromRequest(r)
		prices, err := a.billingRepo.ListPrices(r.Context(), principal.TenantID,
			r.URL.Query().Get("subject_type"), r.URL.Query().Get("subject_id"),
			r.URL.Query().Get("capability"), limit, offset)
		if err != nil {
			log.Printf("failed to list pricing: %v", err)
			WriteOpenAIError(w, http.StatusInternalServerError, "internal server error", "server_error", "pricing_list_failed")
			return
		}
		audit, err := a.billingRepo.ListPriceAudit(r.Context(), principal.TenantID, 20, 0)
		if err != nil {
			log.Printf("failed to list pricing audit: %v", err)
			WriteOpenAIError(w, http.StatusInternalServerError, "internal server error", "server_error", "pricing_audit_list_failed")
			return
		}
		WriteJSON(w, http.StatusOK, map[string]any{
			"pricing": prices,
			"audit":   audit,
			"limit":   limit,
			"offset":  offset,
		})
	case http.MethodPost:
		var req adminPricingWriteRequest
		if err := DecodeJSON(r, &req); err != nil {
			WriteOpenAIError(w, http.StatusBadRequest, "invalid JSON payload", "invalid_request_error", "invalid_json")
			return
		}
		if req.Prices != nil {
			a.writePricingBatch(w, r, principal, req.Prices)
			return
		}
		price, effectiveFrom, err := priceFromAdminRequest(req.adminPricingCreateRequest)
		if err != nil {
			WriteOpenAIError(w, http.StatusBadRequest, err.Error(), "invalid_request_error", "pricing_invalid")
			return
		}
		id := NewID("price")
		if err := a.billingRepo.PutPrice(r.Context(), principal.TenantID, id, price,
			principal.ActorID, effectiveFrom); err != nil {
			log.Printf("failed to create pricing: %v", err)
			WriteOpenAIError(w, http.StatusInternalServerError, "internal server error", "server_error", "pricing_create_failed")
			return
		}
		WriteJSON(w, http.StatusCreated, map[string]any{"id": id, "price": price})
	default:
		WriteMethodNotAllowed(w)
	}
}

// writePricingBatch validates every entry before touching the store, then
// inserts all of them inside ONE PutPrices transaction: one invalid unit
// means nothing is written — a partial price set is the failure the batch
// contract exists to prevent. A duplicate (subject, capability, unit) key
// inside one batch is rejected up front rather than failing on the
// uniqueness constraint mid-write.
func (a *App) writePricingBatch(w http.ResponseWriter, r *http.Request, principal auth.Principal, items []adminPricingCreateRequest) {
	if len(items) == 0 {
		WriteOpenAIError(w, http.StatusBadRequest, "prices must contain at least one entry", "invalid_request_error", "pricing_empty")
		return
	}
	if len(items) > maxBatchPrices {
		WriteOpenAIError(w, http.StatusBadRequest, "prices holds too many entries", "invalid_request_error", "pricing_batch_too_large")
		return
	}
	seen := make(map[[4]string]bool, len(items))
	batch := make([]billing.PriceWrite, 0, len(items))
	for _, item := range items {
		price, effectiveFrom, err := priceFromAdminRequest(item)
		if err != nil {
			WriteOpenAIError(w, http.StatusBadRequest, err.Error(), "invalid_request_error", "pricing_invalid")
			return
		}
		key := [4]string{price.SubjectType, price.SubjectID, price.Capability, string(price.Unit)}
		if seen[key] {
			WriteOpenAIError(w, http.StatusBadRequest, "prices repeats a subject/capability/unit pair", "invalid_request_error", "pricing_duplicate")
			return
		}
		seen[key] = true
		batch = append(batch, billing.PriceWrite{
			ID: NewID("price"), Price: price,
			ActorID: principal.ActorID, EffectiveFrom: effectiveFrom,
		})
	}
	if err := a.billingRepo.PutPrices(r.Context(), principal.TenantID, batch); err != nil {
		log.Printf("failed to create pricing batch: %v", err)
		WriteOpenAIError(w, http.StatusInternalServerError, "internal server error", "server_error", "pricing_create_failed")
		return
	}
	ids := make([]string, len(batch))
	for i, entry := range batch {
		ids[i] = entry.ID
	}
	WriteJSON(w, http.StatusCreated, map[string]any{"ids": ids, "count": len(batch)})
}

func (a *App) handleAdminBillingAudit(w http.ResponseWriter, r *http.Request, principal auth.Principal) {
	setNoStoreHeaders(w)
	if a.billingRepo == nil {
		WriteOpenAIError(w, http.StatusNotImplemented, "billing audit is not configured", "server_error", "not_configured")
		return
	}
	if r.Method != http.MethodGet {
		WriteMethodNotAllowed(w)
		return
	}
	limit, offset := paginationFromRequest(r)
	audit, err := a.billingRepo.ListBillingAudit(r.Context(), principal.TenantID,
		strings.TrimSpace(r.URL.Query().Get("subject_type")),
		strings.TrimSpace(r.URL.Query().Get("subject_id")), limit, offset)
	if err != nil {
		log.Printf("failed to list billing audit: %v", err)
		WriteOpenAIError(w, http.StatusInternalServerError, "internal server error", "server_error", "billing_audit_list_failed")
		return
	}
	WriteJSON(w, http.StatusOK, map[string]any{
		"audit":  audit,
		"limit":  limit,
		"offset": offset,
	})
}

// validUnitKinds is the advertised-unit const set — no free-text kinds.
var validUnitKinds = map[billing.UnitKind]bool{
	billing.UnitInput:       true,
	billing.UnitOutput:      true,
	billing.UnitCachedInput: true,
}

func priceFromAdminRequest(req adminPricingCreateRequest) (billing.Price, time.Time, error) {
	req.SubjectType = strings.TrimSpace(req.SubjectType)
	req.SubjectID = strings.TrimSpace(req.SubjectID)
	req.Capability = strings.TrimSpace(req.Capability)
	req.Unit = strings.TrimSpace(req.Unit)
	req.Currency = strings.TrimSpace(req.Currency)
	if req.SubjectType != billing.PriceSubjectEndpoint && req.SubjectType != billing.PriceSubjectModel {
		return billing.Price{}, time.Time{}, errInvalidPricing("subject_type must be endpoint or model")
	}
	if req.SubjectID == "" {
		return billing.Price{}, time.Time{}, errInvalidPricing("subject_id is required")
	}
	if req.Capability == "" {
		return billing.Price{}, time.Time{}, errInvalidPricing("capability is required")
	}
	if !validUnitKinds[billing.UnitKind(req.Unit)] {
		return billing.Price{}, time.Time{}, errInvalidPricing("unit must be a known unit kind")
	}
	if req.RateMicros < 0 {
		return billing.Price{}, time.Time{}, errInvalidPricing("rate_micros must be zero or greater")
	}
	if req.UnitQuantity <= 0 {
		return billing.Price{}, time.Time{}, errInvalidPricing("unit_quantity must be greater than zero")
	}
	if req.Currency == "" {
		req.Currency = "USD"
	}
	if !validCurrencyCode(req.Currency) {
		return billing.Price{}, time.Time{}, errInvalidPricing("currency must be a 3-letter ISO-4217 alpha-3 code the platform bills in")
	}
	effectiveFrom := time.Now().UTC()
	if raw := strings.TrimSpace(req.EffectiveFrom); raw != "" {
		parsed, err := time.Parse(time.RFC3339, raw)
		if err != nil {
			return billing.Price{}, time.Time{}, errInvalidPricing("effective_from must be RFC3339")
		}
		effectiveFrom = parsed.UTC()
	}
	return billing.Price{
		SubjectType:   req.SubjectType,
		SubjectID:     req.SubjectID,
		Capability:    req.Capability,
		Unit:          billing.UnitKind(req.Unit),
		RateMicros:    req.RateMicros,
		UnitQuantity:  req.UnitQuantity,
		Currency:      req.Currency,
		EffectiveFrom: effectiveFrom,
	}, effectiveFrom, nil
}

type invalidPricingError string

func (e invalidPricingError) Error() string { return string(e) }

func errInvalidPricing(msg string) error {
	return invalidPricingError(msg)
}
