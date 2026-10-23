package extractmodule

import (
	"enclava-go/internal/auth"
	"enclava-go/internal/extract"
	"enclava-go/internal/httpx"
	"enclava-go/internal/inference"
	"enclava-go/internal/llm"
	"encoding/json"
	"errors"
	"io"
	"mime"
	"net/http"
	"path/filepath"
	"strconv"
	"strings"
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
	// No provider-readiness or budget gate here: the shared executor resolves
	// route capability and admits/settles billing for the actual attempt.
	r.Body = http.MaxBytesReader(w, r.Body, h.maxUploadMB*1024*1024+64*1024)
	if err := r.ParseMultipartForm(32 * 1024); err != nil {
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

	defer func() {
		if r.MultipartForm != nil {
			_ = r.MultipartForm.RemoveAll()
		}
	}()
	if r.MultipartForm == nil || len(r.MultipartForm.File) != 1 || len(r.MultipartForm.File["file"]) != 1 {
		httpx.WriteOpenAIError(w, http.StatusBadRequest, "exactly one file is required", "invalid_request_error", "invalid_file_count")
		return
	}
	file, header, err := r.FormFile("file")
	if err != nil {
		httpx.WriteOpenAIError(w, http.StatusBadRequest, "file is required", "invalid_request_error", "missing_file")
		return
	}
	defer file.Close()
	if header.Size <= 0 || header.Size > h.maxUploadMB*1024*1024 {
		httpx.WriteOpenAIError(w, http.StatusRequestEntityTooLarge, "file size is outside the allowed limit", "invalid_request_error", "file_too_large")
		return
	}
	mimeType := strings.TrimSpace(header.Header.Get("Content-Type"))
	if mimeType == "" {
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

	operation := strings.TrimSpace(r.FormValue("operation"))
	if operation == "image-recognition" {
		operation = extract.OperationVision
	}
	if operation == "" {
		operation = extract.OperationProcess
	}
	result, err := h.svc.ProcessReader(r.Context(), principal, extract.ProcessInput{
		Operation:  operation,
		TemplateID: templateID, Context: contextValues,
		FileName: filepath.Base(header.Filename), MimeType: mimeType,
		Reader: file, IdempotencyKey: strings.TrimSpace(r.FormValue("idempotency_key")),
	})
	if err != nil {
		if errors.Is(err, extract.ErrNotFound) {
			httpx.WriteOpenAIError(w, http.StatusNotFound, err.Error(), "invalid_request_error", "not_found")
			return
		}
		if errors.Is(err, extract.ErrForbidden) || errors.Is(err, inference.ErrDenied) {
			httpx.WriteForbidden(w, err.Error())
			return
		}
		if errors.Is(err, extract.ErrExtractionAdmissionLimited) {
			httpx.WriteOpenAIError(w, http.StatusTooManyRequests, err.Error(), "rate_limit_error", "extraction_admission_limited")
			return
		}
		lowerErr := strings.ToLower(err.Error())
		if strings.Contains(lowerErr, "unsupported") || strings.Contains(lowerErr, "template") {
			httpx.WriteOpenAIError(w, http.StatusUnprocessableEntity, err.Error(), "invalid_request_error", "extract_failed")
			return
		}
		httpx.WriteOpenAIError(w, http.StatusBadRequest, err.Error(), "invalid_request_error", "extract_failed")
		return
	}
	httpx.WriteJSON(w, http.StatusAccepted, map[string]any{
		"job_id": result.JobID, "status": result.Status,
		"status_url": "/api/v1/extract/jobs/" + result.JobID,
	})
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
	if r.Method == http.MethodPost {
		h.HandleProcess(w, r, principal)
		return
	}
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
	parts := strings.Split(strings.Trim(strings.TrimPrefix(r.URL.Path, "/api/v1/extract/jobs/"), "/"), "/")
	if len(parts) == 0 || strings.TrimSpace(parts[0]) == "" {
		httpx.WriteOpenAIError(w, http.StatusBadRequest, "job id is required", "invalid_request_error", "missing_job_id")
		return
	}
	jobID := parts[0]
	if len(parts) == 2 && parts[1] == "original" {
		if r.Method != http.MethodGet {
			httpx.WriteMethodNotAllowed(w)
			return
		}
		reader, filename, contentType, err := h.svc.Original(r.Context(), principal, jobID)
		if err != nil {
			h.writeJobError(w, err, "job_original_failed")
			return
		}
		defer reader.Close()
		disposition := mime.FormatMediaType("attachment", map[string]string{"filename": filepath.Base(filename)})
		w.Header().Set("Content-Type", contentType)
		w.Header().Set("Content-Disposition", disposition)
		w.Header().Set("X-Content-Type-Options", "nosniff")
		w.Header().Set("Cache-Control", "no-store")
		_, _ = io.Copy(w, reader)
		return
	}
	if len(parts) == 2 && parts[1] == "cancel" && r.Method == http.MethodPost {
		if err := h.svc.CancelJob(r.Context(), principal, jobID); err != nil {
			h.writeJobError(w, err, "job_cancel_failed")
			return
		}
		httpx.WriteJSON(w, http.StatusAccepted, map[string]string{"job_id": jobID, "status": "cancellation_requested"})
		return
	}
	if len(parts) == 1 && r.Method == http.MethodDelete {
		if err := h.svc.DeleteJob(r.Context(), principal, jobID); err != nil {
			if errors.Is(err, extract.ErrDeletePending) {
				httpx.WriteJSON(w, http.StatusAccepted, map[string]string{"job_id": jobID, "status": "cancellation_requested"})
				return
			}
			h.writeJobError(w, err, "job_delete_failed")
			return
		}
		w.WriteHeader(http.StatusNoContent)
		return
	}
	if len(parts) == 2 && parts[1] != "result" || len(parts) > 2 {
		http.NotFound(w, r)
		return
	}
	if r.Method != http.MethodGet {
		httpx.WriteMethodNotAllowed(w)
		return
	}
	detail, err := h.svc.GetJob(r.Context(), principal, jobID)
	if err != nil {
		h.writeJobError(w, err, "job_fetch_failed")
		return
	}
	if len(parts) == 2 {
		if detail.Result == nil {
			httpx.WriteOpenAIError(w, http.StatusConflict, "job result is not available", "invalid_request_error", "result_unavailable")
			return
		}
		httpx.WriteJSON(w, http.StatusOK, detail.Result)
		return
	}
	httpx.WriteJSON(w, http.StatusOK, detail)
}

func (h *Handlers) writeJobError(w http.ResponseWriter, err error, code string) {
	if errors.Is(err, extract.ErrNotFound) {
		httpx.WriteOpenAIError(w, http.StatusNotFound, "job not found", "invalid_request_error", "not_found")
		return
	}
	httpx.WriteOpenAIError(w, http.StatusInternalServerError, err.Error(), "server_error", code)
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
	models, err := h.svc.ModelsForExtract(r.Context(), principal.TenantID)
	if err != nil {
		httpx.WriteOpenAIError(w, http.StatusInternalServerError, err.Error(), "server_error", "models_list_failed")
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
	// Extraction health is executor-route health: models configured, not a
	// local provider probe.
	models, err := h.svc.ModelsForExtract(r.Context(), principal.TenantID)
	if err != nil || len(models) == 0 {
		httpx.WriteJSON(w, http.StatusOK, map[string]any{"status": "degraded", "error": "no extraction models configured"})
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
