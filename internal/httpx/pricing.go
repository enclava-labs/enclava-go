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
		var req adminPricingCreateRequest
		if err := DecodeJSON(r, &req); err != nil {
			WriteOpenAIError(w, http.StatusBadRequest, "invalid JSON payload", "invalid_request_error", "invalid_json")
			return
		}
		price, effectiveFrom, err := priceFromAdminRequest(req)
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
	req.Currency = strings.ToUpper(strings.TrimSpace(req.Currency))
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
	if len(req.Currency) != 3 {
		return billing.Price{}, time.Time{}, errInvalidPricing("currency must be a three-letter code")
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
