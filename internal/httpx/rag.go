package httpx

import (
	"database/sql"
	"errors"
	"fmt"
	"net/http"
	"strconv"
	"strings"

	"enclava-go/internal/auth"
	"enclava-go/internal/billing"
	"enclava-go/internal/rag"
	"enclava-go/internal/source"
)

func (a *App) handleRAGCollections(w http.ResponseWriter, r *http.Request, principal auth.Principal) {
	if a.ragSvc == nil {
		WriteOpenAIError(w, http.StatusNotImplemented, "rag is not configured", "server_error", "not_configured")
		return
	}
	switch r.Method {
	case http.MethodGet:
		collections, err := a.ragSvc.ListCollections(r.Context(), principal)
		if err != nil {
			WriteOpenAIError(w, http.StatusInternalServerError, "internal server error", "server_error", "rag_collection_list_failed")
			return
		}
		WriteJSON(w, http.StatusOK, map[string]any{"collections": collections})
	case http.MethodPost:
		if !auth.HasScope(principal, "rag.manage") {
			WriteForbidden(w, "missing required scope: rag.manage")
			return
		}
		var input rag.CreateCollectionInput
		if err := DecodeJSON(r, &input); err != nil {
			WriteOpenAIError(w, http.StatusBadRequest, "invalid JSON payload", "invalid_request_error", "invalid_json")
			return
		}
		collection, err := a.ragSvc.CreateCollection(r.Context(), principal, input)
		if err != nil {
			WriteOpenAIError(w, http.StatusBadRequest, err.Error(), "invalid_request_error", "rag_collection_invalid")
			return
		}
		WriteJSON(w, http.StatusCreated, collection)
	default:
		WriteMethodNotAllowed(w)
	}
}

func (a *App) handleRAGCollectionByID(w http.ResponseWriter, r *http.Request, principal auth.Principal) {
	if a.ragSvc == nil {
		WriteOpenAIError(w, http.StatusNotImplemented, "rag is not configured", "server_error", "not_configured")
		return
	}
	rest := strings.Trim(strings.TrimPrefix(r.URL.Path, "/api/v1/rag/collections/"), "/")
	if rest == "" {
		WriteOpenAIError(w, http.StatusBadRequest, "collection id is required", "invalid_request_error", "missing_collection")
		return
	}
	parts := strings.Split(rest, "/")
	collectionID := parts[0]
	if len(parts) > 1 {
		if parts[1] == "search" && len(parts) == 2 {
			a.handleRAGSearch(w, r, principal, collectionID)
			return
		}
		// 01-22: document ingest is unmounted — the …/documents sub-route
		// serves metadata GETs only; uploads 404 until executor-backed ingest.
		if parts[1] == "documents" && len(parts) == 2 && r.Method == http.MethodGet {
			a.handleRAGCollectionDocuments(w, r, principal, collectionID)
			return
		}
		WriteOpenAIError(w, http.StatusNotFound, "rag route not found", "invalid_request_error", "not_found")
		return
	}

	switch r.Method {
	case http.MethodGet:
		collection, err := a.ragSvc.GetCollection(r.Context(), principal, collectionID)
		if err != nil {
			writeRAGNotFoundOrError(w, err, "rag_collection_fetch_failed")
			return
		}
		WriteJSON(w, http.StatusOK, collection)
	case http.MethodPut:
		if !auth.HasScope(principal, "rag.manage") {
			WriteForbidden(w, "missing required scope: rag.manage")
			return
		}
		var input rag.UpdateCollectionInput
		if err := DecodeJSON(r, &input); err != nil {
			WriteOpenAIError(w, http.StatusBadRequest, "invalid JSON payload", "invalid_request_error", "invalid_json")
			return
		}
		collection, err := a.ragSvc.UpdateCollection(r.Context(), principal, collectionID, input)
		if err != nil {
			a.writeRAGNotFoundOrBadRequest(w, err, "rag_collection_update_failed")
			return
		}
		WriteJSON(w, http.StatusOK, collection)
	case http.MethodDelete:
		if !auth.HasScope(principal, "rag.manage") {
			WriteForbidden(w, "missing required scope: rag.manage")
			return
		}
		if err := a.ragSvc.DeleteCollection(r.Context(), principal, collectionID); err != nil {
			writeRAGNotFoundOrError(w, err, "rag_collection_delete_failed")
			return
		}
		WriteJSON(w, http.StatusOK, map[string]any{"deleted": true, "id": collectionID})
	default:
		WriteMethodNotAllowed(w)
	}
}

func (a *App) handleRAGCollectionDocuments(w http.ResponseWriter, r *http.Request, principal auth.Principal, collectionID string) {
	switch r.Method {
	case http.MethodGet:
		limit, offset := paginationFromRequest(r)
		docs, err := a.ragSvc.ListDocuments(r.Context(), principal, collectionID, limit, offset)
		if err != nil {
			writeRAGNotFoundOrError(w, err, "rag_document_list_failed")
			return
		}
		WriteJSON(w, http.StatusOK, map[string]any{"documents": docs, "limit": limit, "offset": offset})
	default:
		WriteMethodNotAllowed(w)
	}
}

func (a *App) handleRAGDocumentByID(w http.ResponseWriter, r *http.Request, principal auth.Principal) {
	if a.ragSvc == nil {
		WriteOpenAIError(w, http.StatusNotImplemented, "rag is not configured", "server_error", "not_configured")
		return
	}
	rest := strings.Trim(strings.TrimPrefix(r.URL.Path, "/api/v1/rag/documents/"), "/")
	if rest == "" {
		WriteOpenAIError(w, http.StatusBadRequest, "document id is required", "invalid_request_error", "missing_document")
		return
	}
	parts := strings.Split(rest, "/")
	documentID := parts[0]
	if len(parts) > 1 {
		// 01-22: reprocess dispatch is unmounted — embedding-producing work
		// returns when executor-backed ingest lands.
		WriteOpenAIError(w, http.StatusNotFound, "rag route not found", "invalid_request_error", "not_found")
		return
	}

	switch r.Method {
	case http.MethodGet:
		doc, blob, err := a.ragSvc.DownloadDocument(r.Context(), principal, documentID)
		if err != nil {
			writeRAGNotFoundOrError(w, err, "rag_document_download_failed")
			return
		}
		w.Header().Set("Content-Type", doc.MimeType)
		w.Header().Set("Content-Disposition", fmt.Sprintf("attachment; filename=%q", doc.FileName))
		w.WriteHeader(http.StatusOK)
		_, _ = w.Write(blob)
	case http.MethodDelete:
		if !auth.HasScope(principal, "rag.manage") {
			WriteForbidden(w, "missing required scope: rag.manage")
			return
		}
		if err := a.ragSvc.DeleteDocument(r.Context(), principal, documentID); err != nil {
			writeRAGNotFoundOrError(w, err, "rag_document_delete_failed")
			return
		}
		WriteJSON(w, http.StatusOK, map[string]any{"deleted": true, "id": documentID})
	default:
		WriteMethodNotAllowed(w)
	}
}

func (a *App) handleRAGSearch(w http.ResponseWriter, r *http.Request, principal auth.Principal, collectionID string) {
	if a.ragSvc == nil {
		WriteOpenAIError(w, http.StatusNotImplemented, "rag is not configured", "server_error", "not_configured")
		return
	}
	if r.Method != http.MethodPost {
		WriteMethodNotAllowed(w)
		return
	}
	var input rag.SearchInput
	if err := DecodeJSON(r, &input); err != nil {
		WriteOpenAIError(w, http.StatusBadRequest, "invalid JSON payload", "invalid_request_error", "invalid_json")
		return
	}
	input.CollectionID = collectionID
	results, err := a.ragSvc.Search(r.Context(), principal, input)
	if err != nil {
		a.writeRAGNotFoundOrBadRequest(w, err, "rag_search_failed")
		return
	}
	WriteJSON(w, http.StatusOK, map[string]any{"results": results})
}

func paginationFromRequest(r *http.Request) (int, int) {
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
	return limit, offset
}

func writeRAGNotFoundOrError(w http.ResponseWriter, err error, code string) {
	if errors.Is(err, sql.ErrNoRows) || errors.Is(err, source.ErrDenied) {
		WriteOpenAIError(w, http.StatusNotFound, "not found", "invalid_request_error", "not_found")
		return
	}
	WriteOpenAIError(w, http.StatusInternalServerError, "internal server error", "server_error", code)
}

func (a *App) writeRAGNotFoundOrBadRequest(w http.ResponseWriter, err error, code string) {
	var be budgetExceededError
	if errors.As(err, &be) || errors.Is(err, billing.ErrBudgetExceeded) {
		a.WriteBudgetError(w, err)
		return
	}
	if errors.Is(err, sql.ErrNoRows) || errors.Is(err, source.ErrDenied) {
		WriteOpenAIError(w, http.StatusNotFound, "not found", "invalid_request_error", "not_found")
		return
	}
	WriteOpenAIError(w, http.StatusBadRequest, err.Error(), "invalid_request_error", code)
}
