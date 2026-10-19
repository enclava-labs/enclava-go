package httpx

import (
	"database/sql"
	"errors"
	"log"
	"net/http"
	"strings"

	"enclava-go/internal/auth"
	"enclava-go/internal/connectors"
	sourceapi "enclava-go/internal/source"
)

func (a *App) handleConnectorSources(w http.ResponseWriter, r *http.Request, principal auth.Principal) {
	if a.connectorSvc == nil {
		WriteOpenAIError(w, http.StatusNotImplemented, "connectors are not configured", "server_error", "not_configured")
		return
	}
	switch r.Method {
	case http.MethodGet:
		limit, offset := paginationFromRequest(r)
		sources, err := a.connectorSvc.ListSources(r.Context(), principal, r.URL.Query().Get("type"), limit, offset)
		if err != nil {
			log.Printf("failed to list connector sources: %v", err)
			WriteOpenAIError(w, http.StatusInternalServerError, "internal server error", "server_error", "connector_sources_list_failed")
			return
		}
		WriteJSON(w, http.StatusOK, map[string]any{"sources": sources, "limit": limit, "offset": offset})
	case http.MethodPost:
		var input connectors.CreateSourceInput
		if err := DecodeJSON(r, &input); err != nil {
			WriteOpenAIError(w, http.StatusBadRequest, "invalid JSON payload", "invalid_request_error", "invalid_json")
			return
		}
		source, err := a.connectorSvc.CreateSource(r.Context(), principal, input)
		if err != nil {
			WriteOpenAIError(w, http.StatusBadRequest, err.Error(), "invalid_request_error", "connector_source_invalid")
			return
		}
		WriteJSON(w, http.StatusCreated, source)
	default:
		_ = principal
		WriteMethodNotAllowed(w)
	}
}

func (a *App) handleConnectorSourceByID(w http.ResponseWriter, r *http.Request, principal auth.Principal) {
	if a.connectorSvc == nil {
		WriteOpenAIError(w, http.StatusNotImplemented, "connectors are not configured", "server_error", "not_configured")
		return
	}
	rest := strings.Trim(strings.TrimPrefix(r.URL.Path, "/api/v1/connectors/sources/"), "/")
	if rest == "" {
		WriteOpenAIError(w, http.StatusBadRequest, "connector source id is required", "invalid_request_error", "missing_source")
		return
	}
	parts := strings.Split(rest, "/")
	sourceID := parts[0]
	if len(parts) > 1 {
		// 01-22: the sync dispatch sub-route is unmounted — no job producer
		// is wired to executor-backed handlers yet; POST …/sync 404s until
		// re-mount.
		WriteOpenAIError(w, http.StatusNotFound, "connector route not found", "invalid_request_error", "not_found")
		return
	}
	switch r.Method {
	case http.MethodGet:
		source, err := a.connectorSvc.GetSource(r.Context(), principal, sourceID)
		if err != nil {
			writeConnectorNotFoundOrError(w, err, "connector_source_fetch_failed")
			return
		}
		WriteJSON(w, http.StatusOK, source)
	case http.MethodPut:
		var input connectors.UpdateSourceInput
		if err := DecodeJSON(r, &input); err != nil {
			WriteOpenAIError(w, http.StatusBadRequest, "invalid JSON payload", "invalid_request_error", "invalid_json")
			return
		}
		source, err := a.connectorSvc.UpdateSource(r.Context(), principal, sourceID, input)
		if err != nil {
			if errors.Is(err, sql.ErrNoRows) || errors.Is(err, sourceapi.ErrDenied) {
				WriteOpenAIError(w, http.StatusNotFound, "connector source not found", "invalid_request_error", "not_found")
				return
			}
			WriteOpenAIError(w, http.StatusBadRequest, err.Error(), "invalid_request_error", "connector_source_invalid")
			return
		}
		WriteJSON(w, http.StatusOK, source)
	case http.MethodDelete:
		if err := a.connectorSvc.DeleteSource(r.Context(), principal, sourceID); err != nil {
			writeConnectorNotFoundOrError(w, err, "connector_source_delete_failed")
			return
		}
		WriteJSON(w, http.StatusOK, map[string]any{"deleted": true, "id": sourceID})
	default:
		WriteMethodNotAllowed(w)
	}
}

func (a *App) handleConnectorSyncJobs(w http.ResponseWriter, r *http.Request, principal auth.Principal) {
	if a.connectorSvc == nil {
		WriteOpenAIError(w, http.StatusNotImplemented, "connectors are not configured", "server_error", "not_configured")
		return
	}
	if r.Method != http.MethodGet {
		WriteMethodNotAllowed(w)
		return
	}
	limit, offset := paginationFromRequest(r)
	jobs, err := a.connectorSvc.ListSyncJobs(r.Context(), principal, r.URL.Query().Get("source_id"), r.URL.Query().Get("status"), limit, offset)
	if err != nil {
		log.Printf("failed to list connector sync jobs: %v", err)
		WriteOpenAIError(w, http.StatusInternalServerError, "internal server error", "server_error", "connector_sync_jobs_list_failed")
		return
	}
	WriteJSON(w, http.StatusOK, map[string]any{"sync_jobs": jobs, "limit": limit, "offset": offset})
}

func (a *App) handleConnectorSyncJobByID(w http.ResponseWriter, r *http.Request, principal auth.Principal) {
	if a.connectorSvc == nil {
		WriteOpenAIError(w, http.StatusNotImplemented, "connectors are not configured", "server_error", "not_configured")
		return
	}
	if r.Method != http.MethodGet {
		WriteMethodNotAllowed(w)
		return
	}
	id := strings.Trim(strings.TrimPrefix(r.URL.Path, "/api/v1/connectors/sync-jobs/"), "/")
	if id == "" {
		WriteOpenAIError(w, http.StatusBadRequest, "sync job id is required", "invalid_request_error", "missing_sync_job")
		return
	}
	job, err := a.connectorSvc.GetSyncJob(r.Context(), principal, id)
	if err != nil {
		writeConnectorNotFoundOrError(w, err, "connector_sync_job_fetch_failed")
		return
	}
	WriteJSON(w, http.StatusOK, job)
}

func writeConnectorSyncError(w http.ResponseWriter, err error, job any) {
	if errors.Is(err, sql.ErrNoRows) || errors.Is(err, sourceapi.ErrDenied) {
		WriteOpenAIError(w, http.StatusNotFound, "connector source not found", "invalid_request_error", "not_found")
		return
	}
	WriteJSON(w, http.StatusBadRequest, map[string]any{
		"error": OpenAIError{
			Message: err.Error(),
			Type:    "invalid_request_error",
			Code:    "connector_sync_failed",
		},
		"sync_job": job,
	})
}

func writeConnectorNotFoundOrError(w http.ResponseWriter, err error, code string) {
	if errors.Is(err, sql.ErrNoRows) || errors.Is(err, sourceapi.ErrDenied) {
		WriteOpenAIError(w, http.StatusNotFound, "connector source not found", "invalid_request_error", "not_found")
		return
	}
	log.Printf("connector error: %v", err)
	WriteOpenAIError(w, http.StatusInternalServerError, "internal server error", "server_error", code)
}
