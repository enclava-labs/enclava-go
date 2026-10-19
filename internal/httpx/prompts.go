package httpx

import (
	"database/sql"
	"errors"
	"log"
	"net/http"
	"strings"

	"enclava-go/internal/auth"
	"enclava-go/internal/prompts"
)

func (a *App) handlePromptTemplates(w http.ResponseWriter, r *http.Request, principal auth.Principal) {
	if a.promptSvc == nil {
		WriteOpenAIError(w, http.StatusNotImplemented, "prompt templates are not configured", "server_error", "not_configured")
		return
	}
	switch r.Method {
	case http.MethodGet:
		templates, err := a.promptSvc.List(r.Context(), principal)
		if err != nil {
			log.Printf("failed to list prompt templates: %v", err)
			WriteOpenAIError(w, http.StatusInternalServerError, "internal server error", "server_error", "prompt_templates_list_failed")
			return
		}
		WriteJSON(w, http.StatusOK, map[string]any{"prompt_templates": templates})
	case http.MethodPost:
		if !auth.HasScope(principal, "chatbot.manage") {
			WriteForbidden(w, "missing required scope: chatbot.manage")
			return
		}
		var input prompts.CreateTemplateInput
		if err := DecodeJSON(r, &input); err != nil {
			WriteOpenAIError(w, http.StatusBadRequest, "invalid JSON payload", "invalid_request_error", "invalid_json")
			return
		}
		t, err := a.promptSvc.Create(r.Context(), principal, input)
		if err != nil {
			WriteOpenAIError(w, http.StatusBadRequest, err.Error(), "invalid_request_error", "prompt_template_invalid")
			return
		}
		WriteJSON(w, http.StatusCreated, t)
	default:
		WriteMethodNotAllowed(w)
	}
}

func (a *App) handlePromptTemplateByID(w http.ResponseWriter, r *http.Request, principal auth.Principal) {
	if a.promptSvc == nil {
		WriteOpenAIError(w, http.StatusNotImplemented, "prompt templates are not configured", "server_error", "not_configured")
		return
	}
	id := strings.Trim(strings.TrimPrefix(r.URL.Path, "/api/v1/prompt-templates/"), "/")
	if id == "" {
		WriteOpenAIError(w, http.StatusBadRequest, "template id is required", "invalid_request_error", "missing_template")
		return
	}
	switch r.Method {
	case http.MethodGet:
		// Visibility is enforced inside the service: hidden and missing
		// templates share the same not-found outcome.
		t, err := a.promptSvc.Get(r.Context(), principal, id)
		if err != nil {
			writePromptNotFoundOrError(w, err, "prompt_template_fetch_failed")
			return
		}
		WriteJSON(w, http.StatusOK, t)
	case http.MethodPut:
		if !auth.HasScope(principal, "chatbot.manage") {
			WriteForbidden(w, "missing required scope: chatbot.manage")
			return
		}
		var input prompts.UpdateTemplateInput
		if err := DecodeJSON(r, &input); err != nil {
			WriteOpenAIError(w, http.StatusBadRequest, "invalid JSON payload", "invalid_request_error", "invalid_json")
			return
		}
		t, err := a.promptSvc.Update(r.Context(), principal, id, input)
		if err != nil {
			if errors.Is(err, sql.ErrNoRows) {
				WriteOpenAIError(w, http.StatusNotFound, "prompt template not found", "invalid_request_error", "not_found")
				return
			}
			WriteOpenAIError(w, http.StatusBadRequest, err.Error(), "invalid_request_error", "prompt_template_invalid")
			return
		}
		WriteJSON(w, http.StatusOK, t)
	case http.MethodDelete:
		if !auth.HasScope(principal, "chatbot.manage") {
			WriteForbidden(w, "missing required scope: chatbot.manage")
			return
		}
		if err := a.promptSvc.Delete(r.Context(), principal, id); err != nil {
			writePromptNotFoundOrError(w, err, "prompt_template_delete_failed")
			return
		}
		WriteJSON(w, http.StatusOK, map[string]any{"deleted": true, "id": id})
	default:
		WriteMethodNotAllowed(w)
	}
}

func writePromptNotFoundOrError(w http.ResponseWriter, err error, code string) {
	if errors.Is(err, sql.ErrNoRows) {
		WriteOpenAIError(w, http.StatusNotFound, "prompt template not found", "invalid_request_error", "not_found")
		return
	}
	log.Printf("prompt template error: %v", err)
	WriteOpenAIError(w, http.StatusInternalServerError, "internal server error", "server_error", code)
}
