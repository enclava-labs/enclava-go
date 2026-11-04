package httpx

import (
	"errors"
	"log"
	"net/http"
	"strings"

	"enclava-go/internal/auth"
	"enclava-go/internal/inference"
)

// handleModelSettings lists every chat model served by the tenant's
// endpoints with its availability, default and fallback.
func (a *App) handleModelSettings(w http.ResponseWriter, r *http.Request, p auth.Principal) {
	setNoStoreHeaders(w)
	if a.executor == nil {
		WriteOpenAIError(w, http.StatusNotImplemented, "models are not configured", "server_error", "not_configured")
		return
	}
	if r.Method != http.MethodGet {
		WriteMethodNotAllowed(w)
		return
	}
	catalog, err := a.executor.ModelCatalog(r.Context(), p.TenantID)
	if err != nil {
		log.Printf("list model settings: %v", err)
		WriteOpenAIError(w, http.StatusInternalServerError, "internal server error", "server_error", "model_settings_failed")
		return
	}
	WriteJSON(w, http.StatusOK, map[string]any{"models": catalog})
}

// handleModelSettingByID updates one model's availability, default or
// fallback: PUT /api/v1/models/settings/{model} with any of "available",
// "default" and "fallback_model" ("" clears the fallback).
func (a *App) handleModelSettingByID(w http.ResponseWriter, r *http.Request, p auth.Principal) {
	setNoStoreHeaders(w)
	if a.executor == nil {
		WriteOpenAIError(w, http.StatusNotImplemented, "models are not configured", "server_error", "not_configured")
		return
	}
	model := strings.Trim(strings.TrimPrefix(r.URL.Path, "/api/v1/models/settings/"), "/")
	if model == "" {
		WriteOpenAIError(w, http.StatusBadRequest, "model is required", "invalid_request_error", "missing_model")
		return
	}
	if r.Method != http.MethodPut {
		WriteMethodNotAllowed(w)
		return
	}
	var patch inference.ModelSettingPatch
	if err := DecodeJSON(r, &patch); err != nil {
		WriteOpenAIError(w, http.StatusBadRequest, "invalid JSON payload", "invalid_request_error", "invalid_json")
		return
	}
	entry, err := a.executor.UpdateModelSetting(r.Context(), p.TenantID, model, patch)
	switch {
	case errors.Is(err, inference.ErrUnknownModel):
		WriteOpenAIError(w, http.StatusNotFound, "no endpoint serves this chat model", "invalid_request_error", "model_not_found")
		return
	case errors.Is(err, inference.ErrInvalidModelSetting):
		WriteOpenAIError(w, http.StatusBadRequest, err.Error(), "invalid_request_error", "model_setting_invalid")
		return
	case err != nil:
		log.Printf("update model setting: %v", err)
		WriteOpenAIError(w, http.StatusInternalServerError, "internal server error", "server_error", "model_setting_failed")
		return
	}
	a.recordSecurityAudit(r, p, "model.settings.updated", "model", model, "success")
	WriteJSON(w, http.StatusOK, entry)
}
