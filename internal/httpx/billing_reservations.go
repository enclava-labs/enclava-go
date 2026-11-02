// billing_reservations.go — the platform.admin unknown-reservation
// reconciliation API (JOB-04): list outstanding canonical reservations by
// state and resolve an unknown hold into reconciled usage (or write it off
// with all-zero tokens). Resolve delegates the transition to
// billing.Service.ResolveUnknown so the settled evidence is written once,
// under the reservation's immutable snapshot.
package httpx

import (
	"encoding/json"
	"errors"
	"log"
	"net/http"
	"strings"
	"time"

	"enclava-go/internal/auth"
	"enclava-go/internal/billing"
	"enclava-go/internal/store/postgres"
)

// reservationsListCap bounds one page of the reconciliation list — newest
// rows first, continued via the opaque `cursor` query param.
const reservationsListCap = 500

// adminReservationDTO is the wire shape of one canonical reservation row.
type adminReservationDTO struct {
	ID            string           `json:"id"`
	SubjectType   string           `json:"subject_type"`
	SubjectID     string           `json:"subject_id"`
	Capability    string           `json:"capability"`
	Model         string           `json:"model"`
	EndpointID    string           `json:"endpoint_id,omitempty"`
	BoundMicros   int64            `json:"bound_micros"`
	Currency      string           `json:"currency"`
	ReservedUnits map[string]int64 `json:"reserved_units"`
	Dispatched    bool             `json:"dispatched"`
	CreatedAt     string           `json:"created_at"`
}

// adminReservationResolveRequest is the POST resolve body: the reconciled
// per-kind token counts. All three are required integers ≥ 0 — an absent
// field can never silently write usage off.
type adminReservationResolveRequest struct {
	InputTokens       *int64 `json:"input_tokens"`
	CachedInputTokens *int64 `json:"cached_input_tokens"`
	OutputTokens      *int64 `json:"output_tokens"`
}

func (a *App) handleAdminReservations(w http.ResponseWriter, r *http.Request, principal auth.Principal) {
	setNoStoreHeaders(w)
	if a.billingRepo == nil {
		WriteOpenAIError(w, http.StatusNotImplemented, "billing not configured", "server_error", "not_configured")
		return
	}
	if r.Method != http.MethodGet {
		WriteMethodNotAllowed(w)
		return
	}
	state := strings.TrimSpace(r.URL.Query().Get("state"))
	if state != billing.ReservationHeld && state != billing.ReservationUnknown {
		WriteOpenAIError(w, http.StatusBadRequest, "state must be 'held' or 'unknown'", "invalid_request_error", "invalid_state")
		return
	}
	rows, next, err := a.billingRepo.ReportListReservations(r.Context(), principal.TenantID, state, reservationsListCap,
		r.URL.Query().Get("cursor"))
	if err != nil {
		if errors.Is(err, postgres.ErrInvalidCursor) {
			WriteOpenAIError(w, http.StatusBadRequest, "invalid cursor", "invalid_request_error", "invalid_cursor")
			return
		}
		log.Printf("failed to list reservations: %v", err)
		WriteOpenAIError(w, http.StatusInternalServerError, "internal server error", "server_error", "reservations_list_failed")
		return
	}
	out := make([]adminReservationDTO, 0, len(rows))
	for _, row := range rows {
		dto, err := adminReservationDTOFor(row)
		if err != nil {
			log.Printf("failed to decode reservation units %s: %v", row.ID, err)
			WriteOpenAIError(w, http.StatusInternalServerError, "internal server error", "server_error", "reservations_list_failed")
			return
		}
		out = append(out, dto)
	}
	WriteJSON(w, http.StatusOK, map[string]any{"reservations": out, "next_cursor": next})
}

func adminReservationDTOFor(row postgres.ReportReservationRow) (adminReservationDTO, error) {
	units, err := decodeReservationUnits(row.ReservedUnits)
	if err != nil {
		return adminReservationDTO{}, err
	}
	return adminReservationDTO{
		ID:            row.ID,
		SubjectType:   row.SubjectType,
		SubjectID:     row.SubjectID,
		Capability:    row.Capability,
		Model:         row.Model,
		EndpointID:    row.EndpointID,
		BoundMicros:   row.BoundMicros,
		Currency:      row.Currency,
		ReservedUnits: units,
		Dispatched:    row.Dispatched,
		CreatedAt:     row.CreatedAt.UTC().Format(time.RFC3339),
	}, nil
}

// decodeReservationUnits reads the persisted {unit_kind: units} JSON map —
// keys already are the plain unit-kind strings the wire carries.
func decodeReservationUnits(raw string) (map[string]int64, error) {
	var units map[string]int64
	if err := json.Unmarshal([]byte(raw), &units); err != nil {
		return nil, err
	}
	if units == nil {
		units = map[string]int64{}
	}
	return units, nil
}

func (a *App) handleAdminReservationByID(w http.ResponseWriter, r *http.Request, principal auth.Principal) {
	setNoStoreHeaders(w)
	if a.billingSvc == nil || a.billingRepo == nil {
		WriteOpenAIError(w, http.StatusNotImplemented, "billing not configured", "server_error", "not_configured")
		return
	}
	rest := strings.TrimPrefix(r.URL.Path, "/api/v1/admin/reservations/")
	parts := strings.SplitN(rest, "/", 2)
	id := parts[0]
	if strings.TrimSpace(id) == "" || len(parts) != 2 || parts[1] != "resolve" {
		WriteOpenAIError(w, http.StatusNotFound, "reservation not found", "invalid_request_error", "not_found")
		return
	}
	if r.Method != http.MethodPost {
		WriteMethodNotAllowed(w)
		return
	}
	a.handleAdminReservationResolve(w, r, principal, id)
}

func (a *App) handleAdminReservationResolve(w http.ResponseWriter, r *http.Request, principal auth.Principal, id string) {
	var req adminReservationResolveRequest
	if err := DecodeJSON(r, &req); err != nil {
		WriteOpenAIError(w, http.StatusBadRequest, err.Error(), "invalid_request_error", "invalid_body")
		return
	}
	for _, field := range []struct {
		name string
		v    *int64
	}{
		{"input_tokens", req.InputTokens},
		{"cached_input_tokens", req.CachedInputTokens},
		{"output_tokens", req.OutputTokens},
	} {
		if field.v == nil || *field.v < 0 {
			WriteOpenAIError(w, http.StatusBadRequest, field.name+" must be a non-negative integer", "invalid_request_error", "invalid_tokens")
			return
		}
	}
	rec, err := a.billingSvc.ResolveUnknown(r.Context(), principal, id, billing.Usage{
		InputUnits:       *req.InputTokens,
		CachedInputUnits: *req.CachedInputTokens,
		OutputUnits:      *req.OutputTokens,
	})
	if err != nil {
		a.writeReservationResolveError(w, err)
		return
	}
	WriteJSON(w, http.StatusOK, map[string]any{"usage_record": rec})
}

// writeReservationResolveError maps the settle-authority errors: unknown or
// cross-tenant ids are one indistinguishable 404, a reservation not in the
// unknown state is the 409 conflict.
func (a *App) writeReservationResolveError(w http.ResponseWriter, err error) {
	switch {
	case errors.Is(err, billing.ErrReservationNotFound), errors.Is(err, postgres.ErrBudgetNotFound):
		WriteOpenAIError(w, http.StatusNotFound, "reservation not found", "invalid_request_error", "not_found")
	case errors.Is(err, billing.ErrReservationState):
		WriteOpenAIError(w, http.StatusConflict, "reservation is not in the unknown state", "invalid_request_error", "invalid_state")
	default:
		log.Printf("failed to resolve reservation: %v", err)
		WriteOpenAIError(w, http.StatusInternalServerError, "internal server error", "server_error", "reservation_resolve_failed")
	}
}
