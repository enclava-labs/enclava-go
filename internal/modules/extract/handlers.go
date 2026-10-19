package extractmodule

import (
	"context"
	"encoding/json"
	"errors"
	"io"
	"net/http"
	"path/filepath"
	"strconv"
	"strings"
	"time"

	"enclava-go/internal/auth"
	"enclava-go/internal/extract"
	"enclava-go/internal/httpx"
	"enclava-go/internal/llm"
)

type Handlers struct {
	svc         *extract.Service
	maxUploadMB int64
	platform    *httpx.App
}

func NewHandlers(svc *extract.Service, maxUploadMB int64, platform *httpx.App) *Handlers {
	return &Handlers{svc: svc, maxUploadMB: maxUploadMB, platform: platform}
}

func (h *Handlers) HandleProcess(w http.ResponseWriter, r *http.Request, principal auth.Principal) {
	if r.Method != http.MethodPost {
		httpx.WriteMethodNotAllowed(w)
		return
	}
	if !h.platform.ProviderReady() {
		httpx.WriteOpenAIError(w, http.StatusServiceUnavailable, "provider not ready", "server_error", "provider_not_ready")
		return
	}
	if err := h.platform.EnforceBudget(r.Context(), principal); err != nil {
		h.platform.WriteBudgetError(w, err)
		return
	}

	r.Body = http.MaxBytesReader(w, r.Body, h.maxUploadMB*1024*1024)
	if err := r.ParseMultipartForm(h.maxUploadMB * 1024 * 1024); err != nil {
		httpx.WriteOpenAIError(w, http.StatusBadRequest, "invalid multipart form", "invalid_request_error", "invalid_form")
		return
	}
	templateID := extractTemplateIDFromRequest(r)
	if templateID == "" {
		httpx.WriteOpenAIError(w, http.StatusBadRequest, "template_id or template is required", "invalid_request_error", "missing_template")
		return
	}
	if !auth.ExtractTemplateAllowed(principal, templateID) {
		httpx.WriteForbidden(w, "template is not allowed for this API key")
		return
	}

	file, header, err := r.FormFile("file")
	if err != nil {
		httpx.WriteOpenAIError(w, http.StatusBadRequest, "file is required", "invalid_request_error", "missing_file")
		return
	}
	defer func() {
		_ = file.Close()
	}()

	content, err := io.ReadAll(file)
	if err != nil {
		httpx.WriteOpenAIError(w, http.StatusBadRequest, "failed to read uploaded file", "invalid_request_error", "invalid_file")
		return
	}
	mimeType := header.Header.Get("Content-Type")
	if strings.TrimSpace(mimeType) == "" {
		mimeType = "application/octet-stream"
	}

	contextValues := map[string]any{}
	contextRaw := strings.TrimSpace(r.FormValue("context"))
	if contextRaw == "" {
		contextRaw = strings.TrimSpace(r.FormValue("buyer_context"))
	}
	if contextRaw != "" {
		if err := json.Unmarshal([]byte(contextRaw), &contextValues); err != nil {
			httpx.WriteOpenAIError(w, http.StatusBadRequest, "context must be valid JSON object", "invalid_request_error", "invalid_context")
			return
		}
	}

	result, err := h.svc.Process(r.Context(), principal, extract.ProcessInput{
		TemplateID:     templateID,
		Context:        contextValues,
		FileName:       filepath.Base(header.Filename),
		MimeType:       mimeType,
		Bytes:          content,
		IdempotencyKey: strings.TrimSpace(r.FormValue("idempotency_key")),
	})
	if err != nil {
		if errors.Is(err, extract.ErrNotFound) {
			httpx.WriteOpenAIError(w, http.StatusNotFound, err.Error(), "invalid_request_error", "not_found")
			return
		}
		if errors.Is(err, extract.ErrForbidden) {
			httpx.WriteForbidden(w, err.Error())
			return
		}
		lowerErr := strings.ToLower(err.Error())
		if strings.Contains(lowerErr, "provider chat completion") {
			httpx.WriteOpenAIError(w, http.StatusBadGateway, "provider request failed", "server_error", "provider_error")
			return
		}
		if strings.Contains(lowerErr, "unsupported") || strings.Contains(lowerErr, "template") {
			httpx.WriteOpenAIError(w, http.StatusUnprocessableEntity, err.Error(), "invalid_request_error", "extract_failed")
			return
		}
		httpx.WriteOpenAIError(w, http.StatusBadRequest, err.Error(), "invalid_request_error", "extract_failed")
		return
	}
	// Billing is settled inside the attempt through reserve/settle — no
	// platform RecordUsage call here.
	httpx.WriteJSON(w, http.StatusOK, result)
}

func extractTemplateIDFromRequest(r *http.Request) string {
	for _, key := range []string{"template_id", "template", "extract_template_id"} {
		if value := strings.TrimSpace(r.FormValue(key)); value != "" {
			return value
		}
	}
	return ""
}

func (h *Handlers) HandleJobsList(w http.ResponseWriter, r *http.Request, principal auth.Principal) {
	if r.Method != http.MethodGet {
		httpx.WriteMethodNotAllowed(w)
		return
	}
	limit := 20
	offset := 0
	if raw := r.URL.Query().Get("limit"); raw != "" {
		if n, err := strconv.Atoi(raw); err == nil {
			limit = n
		}
	}
	if raw := r.URL.Query().Get("offset"); raw != "" {
		if n, err := strconv.Atoi(raw); err == nil {
			offset = n
		}
	}
	jobsList, total, err := h.svc.ListJobs(r.Context(), principal, limit, offset)
	if err != nil {
		httpx.WriteOpenAIError(w, http.StatusInternalServerError, err.Error(), "server_error", "jobs_list_failed")
		return
	}
	httpx.WriteJSON(w, http.StatusOK, map[string]any{
		"jobs":   jobsList,
		"total":  total,
		"limit":  limit,
		"offset": offset,
	})
}

func (h *Handlers) HandleJobDetail(w http.ResponseWriter, r *http.Request, principal auth.Principal) {
	if r.Method != http.MethodGet {
		httpx.WriteMethodNotAllowed(w)
		return
	}
	jobID := strings.TrimPrefix(r.URL.Path, "/api/v1/extract/jobs/")
	if strings.TrimSpace(jobID) == "" {
		httpx.WriteOpenAIError(w, http.StatusBadRequest, "job id is required", "invalid_request_error", "missing_job_id")
		return
	}
	detail, err := h.svc.GetJob(r.Context(), principal, jobID)
	if err != nil {
		if errors.Is(err, extract.ErrNotFound) {
			httpx.WriteOpenAIError(w, http.StatusNotFound, "job not found", "invalid_request_error", "not_found")
			return
		}
		httpx.WriteOpenAIError(w, http.StatusInternalServerError, err.Error(), "server_error", "job_fetch_failed")
		return
	}
	httpx.WriteJSON(w, http.StatusOK, detail)
}

func (h *Handlers) HandleTemplates(w http.ResponseWriter, r *http.Request, principal auth.Principal) {
	switch r.Method {
	case http.MethodGet:
		templates, err := h.svc.ListTemplates(r.Context(), principal)
		if err != nil {
			httpx.WriteOpenAIError(w, http.StatusInternalServerError, err.Error(), "server_error", "template_list_failed")
			return
		}
		httpx.WriteJSON(w, http.StatusOK, map[string]any{"templates": templates})
	case http.MethodPost:
		if !auth.HasScope(principal, "extract.manage") {
			httpx.WriteForbidden(w, "missing required scope: extract.manage")
			return
		}
		var payload extract.Template
		if err := httpx.DecodeJSON(r, &payload); err != nil {
			httpx.WriteOpenAIError(w, http.StatusBadRequest, "invalid JSON payload", "invalid_request_error", "invalid_json")
			return
		}
		if err := h.svc.UpsertTemplate(r.Context(), principal, payload); err != nil {
			httpx.WriteOpenAIError(w, http.StatusBadRequest, err.Error(), "invalid_request_error", "template_invalid")
			return
		}
		tpl, err := h.svc.GetTemplate(r.Context(), principal, payload.ID)
		if err != nil {
			httpx.WriteOpenAIError(w, http.StatusInternalServerError, err.Error(), "server_error", "template_fetch_failed")
			return
		}
		httpx.WriteJSON(w, http.StatusCreated, tpl)
	default:
		httpx.WriteMethodNotAllowed(w)
	}
}

func (h *Handlers) HandleTemplateByID(w http.ResponseWriter, r *http.Request, principal auth.Principal) {
	templateID := strings.TrimPrefix(r.URL.Path, "/api/v1/extract/templates/")
	if strings.TrimSpace(templateID) == "" {
		httpx.WriteOpenAIError(w, http.StatusBadRequest, "template id is required", "invalid_request_error", "missing_template_id")
		return
	}
	if !auth.ExtractTemplateAllowed(principal, templateID) {
		httpx.WriteForbidden(w, "template is not allowed for this API key")
		return
	}

	switch r.Method {
	case http.MethodGet:
		tpl, err := h.svc.GetTemplate(r.Context(), principal, templateID)
		if err != nil {
			if errors.Is(err, extract.ErrNotFound) {
				httpx.WriteOpenAIError(w, http.StatusNotFound, "template not found", "invalid_request_error", "not_found")
				return
			}
			httpx.WriteOpenAIError(w, http.StatusInternalServerError, err.Error(), "server_error", "template_fetch_failed")
			return
		}
		httpx.WriteJSON(w, http.StatusOK, tpl)
	case http.MethodPut:
		if !auth.HasScope(principal, "extract.manage") {
			httpx.WriteForbidden(w, "missing required scope: extract.manage")
			return
		}
		existing, err := h.svc.GetTemplate(r.Context(), principal, templateID)
		if err != nil {
			if errors.Is(err, extract.ErrNotFound) {
				httpx.WriteOpenAIError(w, http.StatusNotFound, "template not found", "invalid_request_error", "not_found")
				return
			}
			httpx.WriteOpenAIError(w, http.StatusInternalServerError, err.Error(), "server_error", "template_fetch_failed")
			return
		}
		var patch map[string]any
		if err := httpx.DecodeJSON(r, &patch); err != nil {
			httpx.WriteOpenAIError(w, http.StatusBadRequest, "invalid JSON payload", "invalid_request_error", "invalid_json")
			return
		}
		merged := mergeTemplate(existing, patch)
		if err := h.svc.UpsertTemplate(r.Context(), principal, merged); err != nil {
			httpx.WriteOpenAIError(w, http.StatusBadRequest, err.Error(), "invalid_request_error", "template_invalid")
			return
		}
		tpl, err := h.svc.GetTemplate(r.Context(), principal, templateID)
		if err != nil {
			httpx.WriteOpenAIError(w, http.StatusInternalServerError, err.Error(), "server_error", "template_fetch_failed")
			return
		}
		httpx.WriteJSON(w, http.StatusOK, tpl)
	case http.MethodDelete:
		if !auth.HasScope(principal, "extract.manage") {
			httpx.WriteForbidden(w, "missing required scope: extract.manage")
			return
		}
		if err := h.svc.DeleteTemplate(r.Context(), principal, templateID); err != nil {
			httpx.WriteOpenAIError(w, http.StatusInternalServerError, err.Error(), "server_error", "template_delete_failed")
			return
		}
		httpx.WriteJSON(w, http.StatusOK, map[string]any{"deleted": true, "id": templateID})
	default:
		httpx.WriteMethodNotAllowed(w)
	}
}

func (h *Handlers) HandleTemplatesResetDefaults(w http.ResponseWriter, r *http.Request, principal auth.Principal) {
	if r.Method != http.MethodPost {
		httpx.WriteMethodNotAllowed(w)
		return
	}
	if err := h.svc.ResetDefaultTemplates(r.Context(), principal); err != nil {
		httpx.WriteOpenAIError(w, http.StatusInternalServerError, err.Error(), "server_error", "template_reset_failed")
		return
	}
	httpx.WriteJSON(w, http.StatusOK, map[string]any{"ok": true})
}

func (h *Handlers) HandleModels(w http.ResponseWriter, r *http.Request, principal auth.Principal) {
	if r.Method != http.MethodGet {
		httpx.WriteMethodNotAllowed(w)
		return
	}
	if !h.platform.ProviderReady() {
		httpx.WriteOpenAIError(w, http.StatusServiceUnavailable, "provider not ready", "server_error", "provider_not_ready")
		return
	}
	models, err := h.svc.ModelsForExtract(r.Context())
	if err != nil {
		h.platform.WriteProviderError(w, err)
		return
	}
	if len(principal.AllowedModels) > 0 {
		filtered := make([]llm.Model, 0, len(models))
		for _, m := range models {
			if auth.ModelAllowed(principal, m.ID) {
				filtered = append(filtered, m)
			}
		}
		models = filtered
	}
	httpx.WriteJSON(w, http.StatusOK, map[string]any{"models": models})
}

func (h *Handlers) HandleSettings(w http.ResponseWriter, r *http.Request, principal auth.Principal) {
	switch r.Method {
	case http.MethodGet:
		settings, err := h.svc.GetSettings(r.Context(), principal)
		if err != nil {
			httpx.WriteOpenAIError(w, http.StatusInternalServerError, err.Error(), "server_error", "settings_fetch_failed")
			return
		}
		httpx.WriteJSON(w, http.StatusOK, settings)
	case http.MethodPut:
		if !auth.HasScope(principal, "extract.manage") {
			httpx.WriteForbidden(w, "missing required scope: extract.manage")
			return
		}
		var payload struct {
			DefaultModel  string `json:"default_model"`
			MaxFileSizeMB int64  `json:"max_file_size_mb"`
		}
		if err := httpx.DecodeJSON(r, &payload); err != nil {
			httpx.WriteOpenAIError(w, http.StatusBadRequest, "invalid JSON payload", "invalid_request_error", "invalid_json")
			return
		}
		if payload.DefaultModel != "" && !auth.ModelAllowed(principal, payload.DefaultModel) {
			httpx.WriteForbidden(w, "model is not allowed for this API key")
			return
		}
		updated, err := h.svc.UpdateSettings(r.Context(), principal, extract.Settings{
			DefaultModel:  payload.DefaultModel,
			MaxFileSizeMB: payload.MaxFileSizeMB,
		})
		if err != nil {
			httpx.WriteOpenAIError(w, http.StatusBadRequest, err.Error(), "invalid_request_error", "settings_update_failed")
			return
		}
		httpx.WriteJSON(w, http.StatusOK, updated)
	default:
		httpx.WriteMethodNotAllowed(w)
	}
}

func (h *Handlers) HandleHealth(w http.ResponseWriter, r *http.Request, principal auth.Principal) {
	if r.Method != http.MethodGet {
		httpx.WriteMethodNotAllowed(w)
		return
	}
	if !h.platform.ProviderReady() {
		httpx.WriteJSON(w, http.StatusOK, map[string]any{
			"status":   "degraded",
			"provider": h.platform.ProviderSnapshot(),
		})
		return
	}
	ctx, cancel := context.WithTimeout(r.Context(), 10*time.Second)
	defer cancel()
	_, err := h.platform.Provider().ListModels(ctx)
	if err != nil {
		httpx.WriteJSON(w, http.StatusOK, map[string]any{"status": "degraded", "error": err.Error()})
		return
	}
	httpx.WriteJSON(w, http.StatusOK, map[string]any{"status": "ok"})
}

func mergeTemplate(existing extract.Template, patch map[string]any) extract.Template {
	if v, ok := patch["name"].(string); ok {
		existing.Name = v
	}
	if v, ok := patch["description"].(string); ok {
		existing.Description = v
	}
	if v, ok := patch["system_prompt"].(string); ok {
		existing.SystemPrompt = v
	}
	if v, ok := patch["user_prompt"].(string); ok {
		existing.UserPrompt = v
	}
	if v, ok := patch["model"].(string); ok {
		existing.DefaultModel = v
	}
	if v, ok := patch["is_default"].(bool); ok {
		existing.IsDefault = v
	}
	if v, ok := patch["is_active"].(bool); ok {
		existing.Enabled = v
	}
	if v, ok := patch["context_schema"].(map[string]any); ok {
		existing.ContextSchema = v
	}
	if v, ok := patch["output_schema"].(map[string]any); ok {
		existing.OutputSchema = v
	}
	return existing
}
