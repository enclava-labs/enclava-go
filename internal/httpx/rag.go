package httpx

import (
	"database/sql"
	"errors"
	"fmt"
	"io"
	"net/http"
	"path/filepath"
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
		switch parts[1] {
		case "documents":
			if len(parts) == 2 {
				a.handleRAGCollectionDocuments(w, r, principal, collectionID)
				return
			}
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
	case http.MethodPost:
		if !auth.HasScope(principal, "rag.manage") {
			WriteForbidden(w, "missing required scope: rag.manage")
			return
		}
		r.Body = http.MaxBytesReader(w, r.Body, a.maxUploadMB*1024*1024)
		if err := r.ParseMultipartForm(a.maxUploadMB * 1024 * 1024); err != nil {
			WriteOpenAIError(w, http.StatusBadRequest, "invalid multipart form", "invalid_request_error", "invalid_form")
			return
		}
		file, header, err := r.FormFile("file")
		if err != nil {
			WriteOpenAIError(w, http.StatusBadRequest, "file is required", "invalid_request_error", "missing_file")
			return
		}
		defer func() { _ = file.Close() }()
		content, err := io.ReadAll(file)
		if err != nil {
			WriteOpenAIError(w, http.StatusBadRequest, "failed to read uploaded file", "invalid_request_error", "invalid_file")
			return
		}
		mimeType := header.Header.Get("Content-Type")
		if strings.TrimSpace(mimeType) == "" {
			mimeType = "application/octet-stream"
		}
		out, err := a.ragSvc.UploadDocument(r.Context(), principal, collectionID, rag.UploadDocumentInput{
			FileName:   filepath.Base(header.Filename),
			MimeType:   mimeType,
			Bytes:      content,
			SourceType: strings.TrimSpace(r.FormValue("source_type")),
			SourceID:   strings.TrimSpace(r.FormValue("source_id")),
			SourceURI:  strings.TrimSpace(r.FormValue("source_uri")),
		})
		if err != nil {
			a.writeRAGNotFoundOrBadRequest(w, err, "rag_document_upload_failed")
			return
		}
		WriteJSON(w, http.StatusCreated, out)
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
		if len(parts) == 2 && parts[1] == "reprocess" {
			if r.Method != http.MethodPost {
				WriteMethodNotAllowed(w)
				return
			}
			if !auth.HasScope(principal, "rag.manage") {
				WriteForbidden(w, "missing required scope: rag.manage")
				return
			}
			out, err := a.ragSvc.ReprocessDocument(r.Context(), principal, documentID)
			if err != nil {
				a.writeRAGNotFoundOrBadRequest(w, err, "rag_document_reprocess_failed")
				return
			}
			WriteJSON(w, http.StatusOK, out)
			return
		}
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

func (a *App) handleRAGSearch(w http.ResponseWriter, r *http.Request, principal auth.Principal) {
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
