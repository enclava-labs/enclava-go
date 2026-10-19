package httpx

import (
	"errors"
	"net/http"
	"strings"

	"enclava-go/internal/auth"
	"enclava-go/internal/source"
)

type processingRestrictionRequest struct {
	SubjectKind        string   `json:"subject_kind"`
	SubjectID          string   `json:"subject_id"`
	RequiredAssurance  string   `json:"required_assurance"`
	DeniedCapabilities []string `json:"denied_capabilities"`
	PolicyLabels       []string `json:"policy_labels"`
}

func (a *App) handleProcessingRestrictions(w http.ResponseWriter, r *http.Request, p auth.Principal) {
	setNoStoreHeaders(w)
	if a.sourceRepo == nil {
		WriteOpenAIError(w, http.StatusNotImplemented, "processing policy is not configured", "server_error", "not_configured")
		return
	}
	if strings.TrimSpace(p.TenantID) == "" {
		WriteUnauthorized(w, "authenticated principal required")
		return
	}
	switch r.Method {
	case http.MethodGet:
		kind := strings.TrimSpace(r.URL.Query().Get("subject_kind"))
		id := strings.TrimSpace(r.URL.Query().Get("subject_id"))
		if !validRestrictionSubject(kind, id, p.TenantID) {
			WriteOpenAIError(w, http.StatusBadRequest, "invalid processing restriction subject", "invalid_request_error", "restriction_invalid")
			return
		}
		if err := a.validateRestrictionSubject(r, p.TenantID, kind, id); err != nil {
			WriteOpenAIError(w, http.StatusNotFound, "processing restriction subject not found", "invalid_request_error", "not_found")
			return
		}
		rows, err := a.sourceRepo.RestrictionsForSubject(r.Context(), p.TenantID, kind, id)
		if err != nil {
			WriteOpenAIError(w, http.StatusInternalServerError, "internal server error", "server_error", "restriction_read_failed")
			return
		}
		if len(rows) == 0 {
			rows = []source.SourceRestriction{{TenantID: p.TenantID, SubjectKind: kind, SubjectID: id, DeniedCapabilities: []string{}, PolicyLabels: []string{}}}
		}
		WriteJSON(w, http.StatusOK, map[string]any{"restriction": rows[0]})
	case http.MethodPut:
		var req processingRestrictionRequest
		if err := DecodeJSON(r, &req); err != nil {
			WriteOpenAIError(w, http.StatusBadRequest, "invalid JSON payload", "invalid_request_error", "invalid_json")
			return
		}
		req.SubjectKind, req.SubjectID = strings.TrimSpace(req.SubjectKind), strings.TrimSpace(req.SubjectID)
		req.RequiredAssurance = strings.TrimSpace(req.RequiredAssurance)
		if !validRestrictionSubject(req.SubjectKind, req.SubjectID, p.TenantID) ||
			(req.RequiredAssurance != "" && req.RequiredAssurance != "standard" && req.RequiredAssurance != "high") {
			WriteOpenAIError(w, http.StatusBadRequest, "invalid processing restriction", "invalid_request_error", "restriction_invalid")
			return
		}
		if err := a.validateRestrictionSubject(r, p.TenantID, req.SubjectKind, req.SubjectID); err != nil {
			WriteOpenAIError(w, http.StatusNotFound, "processing restriction subject not found", "invalid_request_error", "not_found")
			return
		}
		for i := range req.DeniedCapabilities {
			req.DeniedCapabilities[i] = strings.TrimSpace(req.DeniedCapabilities[i])
			if !validDeniedCapability(req.DeniedCapabilities[i]) {
				WriteOpenAIError(w, http.StatusBadRequest, "unsupported denied capability", "invalid_request_error", "restriction_invalid")
				return
			}
		}
		row := source.SourceRestriction{TenantID: p.TenantID, SubjectKind: req.SubjectKind, SubjectID: req.SubjectID,
			RequiredAssurance: req.RequiredAssurance, DeniedCapabilities: req.DeniedCapabilities, PolicyLabels: req.PolicyLabels}
		if err := a.sourceRepo.SetRestriction(r.Context(), row); err != nil {
			WriteOpenAIError(w, http.StatusInternalServerError, "internal server error", "server_error", "restriction_write_failed")
			return
		}
		WriteJSON(w, http.StatusOK, map[string]any{"restriction": row})
	default:
		WriteMethodNotAllowed(w)
	}
}

func validRestrictionSubject(kind, id, tenantID string) bool {
	switch kind {
	case source.RestrictionTenant:
		return id == tenantID
	case source.RestrictionConnection, source.RestrictionObject, source.RestrictionArtifact:
		return id != "" && len(id) <= 256
	default:
		return false
	}
}

func (a *App) validateRestrictionSubject(r *http.Request, tenantID, kind, id string) error {
	switch kind {
	case source.RestrictionTenant:
		return nil
	case source.RestrictionConnection:
		_, err := a.sourceRepo.GetConnection(r.Context(), tenantID, id)
		return err
	case source.RestrictionObject:
		_, err := a.sourceRepo.GetObject(r.Context(), tenantID, id)
		return err
	case source.RestrictionArtifact:
		if a.artifactRepo == nil {
			return errors.New("artifact store unavailable")
		}
		_, err := a.artifactRepo.GetArtifact(r.Context(), tenantID, id)
		return err
	default:
		return errors.New("invalid subject")
	}
}

func validDeniedCapability(value string) bool {
	switch value {
	case "chat/completions", "embeddings", "vision", "responses", "responses/compact":
		return true
	default:
		return false
	}
}
