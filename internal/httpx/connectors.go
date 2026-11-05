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
		a.recordSecurityAudit(r, principal, "connector.source.created", "connector.source", source.ID, "success")
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
		if parts[1] == "sync" && len(parts) == 2 && r.Method == http.MethodPost {
			job, err := a.connectorSvc.SyncSourceKeyed(r.Context(), principal, sourceID, r.Header.Get("Idempotency-Key"))
			if err != nil {
				if errors.Is(err, connectors.ErrSourceDisabled) {
					WriteOpenAIError(w, http.StatusConflict, err.Error(), "invalid_request_error", "connector_source_disabled")
				} else if errors.Is(err, connectors.ErrInvalidSource) {
					WriteOpenAIError(w, http.StatusBadRequest, err.Error(), "invalid_request_error", "connector_source_invalid")
				} else {
					writeConnectorNotFoundOrError(w, err, "connector_sync_invalid")
				}
				return
			}
			WriteJSON(w, http.StatusAccepted, job)
			return
		}
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
		a.recordSecurityAudit(r, principal, "connector.source.updated", "connector.source", sourceID, "success")
		WriteJSON(w, http.StatusOK, source)
	case http.MethodDelete:
		if err := a.connectorSvc.DeleteSource(r.Context(), principal, sourceID); err != nil {
			writeConnectorNotFoundOrError(w, err, "connector_source_delete_failed")
			return
		}
		a.recordSecurityAudit(r, principal, "connector.source.deleted", "connector.source", sourceID, "success")
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
	rest := strings.Trim(strings.TrimPrefix(r.URL.Path, "/api/v1/connectors/sync-jobs/"), "/")
	parts := strings.Split(rest, "/")
	if rest == "" || parts[0] == "" {
		WriteOpenAIError(w, http.StatusBadRequest, "sync job id is required", "invalid_request_error", "missing_sync_job")
		return
	}
	if len(parts) == 2 && parts[1] == "cancel" && r.Method == http.MethodPost {
		if err := a.connectorSvc.CancelSyncJob(r.Context(), principal, parts[0]); err != nil {
			writeConnectorNotFoundOrError(w, err, "connector_sync_job_cancel_failed")
			return
		}
		WriteJSON(w, http.StatusAccepted, map[string]any{"id": parts[0], "status": connectors.JobStatusCancelled})
		return
	}
	if len(parts) != 1 || r.Method != http.MethodGet {
		WriteMethodNotAllowed(w)
		return
	}
	job, err := a.connectorSvc.GetSyncJob(r.Context(), principal, parts[0])
	if err != nil {
		writeConnectorNotFoundOrError(w, err, "connector_sync_job_fetch_failed")
		return
	}
	WriteJSON(w, http.StatusOK, job)

}

func writeConnectorNotFoundOrError(w http.ResponseWriter, err error, code string) {
	if errors.Is(err, sql.ErrNoRows) || errors.Is(err, sourceapi.ErrDenied) {
		WriteOpenAIError(w, http.StatusNotFound, "connector source not found", "invalid_request_error", "not_found")
		return
	}
	log.Printf("connector error: %v", err)
	WriteOpenAIError(w, http.StatusInternalServerError, "internal server error", "server_error", code)
}

// handleSlackChannels lists the channels a Slack credential is a member of
// for the add-source wizard's picker: GET
// /api/v1/connectors/slack/channels?credential_ref=CONNECTOR_SLACK_…
func (a *App) handleSlackChannels(w http.ResponseWriter, r *http.Request, principal auth.Principal) {
	if a.connectorSvc == nil {
		WriteOpenAIError(w, http.StatusNotImplemented, "connectors are not configured", "server_error", "not_configured")
		return
	}
	if r.Method != http.MethodGet {
		WriteMethodNotAllowed(w)
		return
	}
	setNoStoreHeaders(w)
	q := r.URL.Query()
	channels, err := a.connectorSvc.SlackChannels(r.Context(), principal, q.Get("credential_ref"), q.Get("base_url"))
	if errors.Is(err, sourceapi.ErrDenied) {
		WriteOpenAIError(w, http.StatusForbidden, "connectors.manage is required", "permission_error", "forbidden")
		return
	}
	if errors.Is(err, auth.ErrSecretDenied) {
		WriteOpenAIError(w, http.StatusBadRequest, "no Slack credential with this reference is available", "invalid_request_error", "slack_channels_unavailable")
		return
	}
	if err != nil {
		// Slack refusals (e.g. missing_scope) name the cause themselves.
		WriteOpenAIError(w, http.StatusBadRequest, err.Error(), "invalid_request_error", "slack_channels_unavailable")
		return
	}
	if channels == nil {
		channels = []connectors.SlackChannel{}
	}
	WriteJSON(w, http.StatusOK, map[string]any{"channels": channels})
}
