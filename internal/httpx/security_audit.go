package httpx

import (
	"context"
	"log"
	"net/http"
	"strconv"
	"time"

	"enclava-go/internal/auth"
	"enclava-go/internal/store/postgres"
)

type SecurityAuditRecorder interface {
	Insert(context.Context, postgres.SecurityAuditRow) error
	List(context.Context, string, int, string) ([]postgres.SecurityAuditRow, error)
}

func (a *App) SetSecurityAudit(r SecurityAuditRecorder) { a.securityAudit = r }

func (s *SessionAuth) SetSecurityAudit(r SecurityAuditRecorder) { s.securityAudit = r }

func recordSecurityAudit(recorder SecurityAuditRecorder, r *http.Request, p auth.Principal, trustProxy bool, action, targetKind, targetID, outcome string, details ...map[string]any) {
	if recorder == nil || p.TenantID == "" {
		return
	}
	row := postgres.SecurityAuditRow{
		TenantID:     p.TenantID,
		ID:           NewID("secaudit"),
		OccurredAt:   time.Now().UTC(),
		ActorID:      p.ActorID,
		ActorKind:    string(p.ActorKind),
		CredentialID: p.CredentialID,
		Action:       action,
		TargetKind:   targetKind,
		TargetID:     targetID,
		Outcome:      outcome,
		ClientIP:     clientIP(r, trustProxy),
		RequestID:    requestIDFromContext(r.Context()),
		Details:      map[string]any{},
	}
	if len(details) != 0 {
		row.Details = details[0]
	}
	if err := recorder.Insert(r.Context(), row); err != nil {
		log.Printf("failed to write security audit event %s: %v", action, err)
	}
}

func (a *App) recordSecurityAudit(r *http.Request, p auth.Principal, action, targetKind, targetID, outcome string, details ...map[string]any) {
	recordSecurityAudit(a.securityAudit, r, p, a.trustProxy, action, targetKind, targetID, outcome, details...)
}

func (s *SessionAuth) recordSecurityAudit(r *http.Request, p auth.Principal, action, targetKind, targetID, outcome string) {
	recordSecurityAudit(s.securityAudit, r, p, s.trustProxy, action, targetKind, targetID, outcome)
}

func (a *App) handleSecurityAudit(w http.ResponseWriter, r *http.Request, p auth.Principal) {
	setNoStoreHeaders(w)
	if r.Method != http.MethodGet {
		WriteMethodNotAllowed(w)
		return
	}
	if p.TenantID == "" {
		WriteForbidden(w, "tenant required")
		return
	}
	limit := 100
	if raw := r.URL.Query().Get("limit"); raw != "" {
		n, err := strconv.Atoi(raw)
		if err != nil || n < 1 || n > 200 {
			WriteOpenAIError(w, http.StatusBadRequest, "limit must be between 1 and 200", "invalid_request_error", "invalid_limit")
			return
		}
		limit = n
	}
	if a.securityAudit == nil {
		WriteOpenAIError(w, http.StatusServiceUnavailable, "security audit unavailable", "server_error", "security_audit_unavailable")
		return
	}
	rows, err := a.securityAudit.List(r.Context(), p.TenantID, limit, r.URL.Query().Get("before"))
	if err != nil {
		log.Printf("failed to list security audit events: %v", err)
		WriteOpenAIError(w, http.StatusInternalServerError, "internal server error", "server_error", "security_audit_failed")
		return
	}
	if rows == nil {
		rows = []postgres.SecurityAuditRow{}
	}
	WriteJSON(w, http.StatusOK, map[string]any{"data": rows})
}
