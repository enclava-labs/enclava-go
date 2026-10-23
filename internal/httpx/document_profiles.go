package httpx

import (
	"errors"
	"io"
	"mime"
	"net/http"
	"path/filepath"
	"strings"

	"enclava-go/internal/artifacts"
	"enclava-go/internal/auth"
	"enclava-go/internal/extract"
	"enclava-go/internal/jobs"
	"enclava-go/internal/source"
)

type documentProfileView struct {
	ID                 string `json:"id"`
	ProfileID          string `json:"profile_id"`
	Version            int    `json:"version"`
	SizeCodePoints     int    `json:"size_codepoints"`
	OverlapCodePoints  int    `json:"overlap_codepoints"`
	BoundaryStrategy   string `json:"boundary_strategy"`
	IncludeParents     bool   `json:"include_parents"`
	TableHandling      string `json:"table_handling"`
	PreserveClauses    bool   `json:"preserve_clauses"`
	PreserveAuthorship bool   `json:"preserve_authorship"`
}

func profileView(p source.ChunkProfile) documentProfileView {
	return documentProfileView{ID: p.ID, ProfileID: p.ProfileID, Version: p.Version, SizeCodePoints: p.SizeCodePoints, OverlapCodePoints: p.OverlapCodePoints, BoundaryStrategy: p.BoundaryStrategy, IncludeParents: p.IncludeParents, TableHandling: p.TableHandling, PreserveClauses: p.PreserveClauses, PreserveAuthorship: p.PreserveAuthorship}
}

func RegisterDocumentPreviewRoutes(mux *http.ServeMux, app *App, service *extract.PreviewService, maxUploadMB int64) {
	h := &documentPreviewHandlers{service: service, maxUploadMB: maxUploadMB}
	mux.HandleFunc("/api/v1/document-profiles", app.Authenticated("rag", h.profiles))
	mux.HandleFunc("/api/v1/document-profiles/", app.Authenticated("rag", h.profile))
	mux.HandleFunc("/api/v1/document-previews", app.Authenticated("rag", h.previews))
	mux.HandleFunc("/api/v1/document-previews/", app.Authenticated("rag", h.preview))
}

type documentPreviewHandlers struct {
	service     *extract.PreviewService
	maxUploadMB int64
}

func (h *documentPreviewHandlers) profiles(w http.ResponseWriter, r *http.Request, p auth.Principal) {
	setNoStoreHeaders(w)
	if r.Method != http.MethodGet {
		WriteMethodNotAllowed(w)
		return
	}
	profiles, err := h.service.ListProfiles(r.Context(), p)
	if err != nil {
		WriteOpenAIError(w, http.StatusInternalServerError, "internal server error", "server_error", "document_profile_list_failed")
		return
	}
	out := make([]documentProfileView, 0, len(profiles))
	for _, profile := range profiles {
		out = append(out, profileView(profile))
	}
	WriteJSON(w, http.StatusOK, map[string]any{"profiles": out})
}

func (h *documentPreviewHandlers) profile(w http.ResponseWriter, r *http.Request, p auth.Principal) {
	setNoStoreHeaders(w)
	if r.Method != http.MethodGet {
		WriteMethodNotAllowed(w)
		return
	}
	id := strings.Trim(strings.TrimPrefix(r.URL.Path, "/api/v1/document-profiles/"), "/")
	if id == "" || strings.Contains(id, "/") {
		WriteOpenAIError(w, http.StatusNotFound, "document profile not found", "invalid_request_error", "not_found")
		return
	}
	profile, err := h.service.GetProfile(r.Context(), p, id)
	if err != nil {
		WriteOpenAIError(w, http.StatusNotFound, "document profile not found", "invalid_request_error", "not_found")
		return
	}
	WriteJSON(w, http.StatusOK, map[string]any{"profile": profileView(profile)})
}

func (h *documentPreviewHandlers) previews(w http.ResponseWriter, r *http.Request, p auth.Principal) {
	setNoStoreHeaders(w)
	if r.Method != http.MethodPost {
		WriteMethodNotAllowed(w)
		return
	}
	mediaType, _, err := mime.ParseMediaType(r.Header.Get("Content-Type"))
	if err != nil {
		mediaType = ""
	}
	if mediaType == "application/json" {
		var input struct {
			ArtifactRef           string `json:"artifact_ref"`
			ChunkProfileVersionID string `json:"chunk_profile_version_id"`
		}
		if err := DecodeJSON(r, &input); err != nil || input.ArtifactRef == "" || input.ChunkProfileVersionID == "" {
			WriteOpenAIError(w, http.StatusBadRequest, "artifact_ref and chunk_profile_version_id are required", "invalid_request_error", "invalid_preview")
			return
		}
		job, err := h.service.SubmitOwnedArtifact(r.Context(), p, input.ArtifactRef, input.ChunkProfileVersionID)
		if err != nil {
			writePreviewError(w, err)
			return
		}
		WriteJSON(w, http.StatusAccepted, map[string]any{"job_id": job.ID, "status": job.Status})
		return
	}
	if mediaType != "multipart/form-data" {
		WriteOpenAIError(w, http.StatusBadRequest, "multipart file upload or owned artifact reference is required", "invalid_request_error", "invalid_upload")
		return
	}
	maxBytes := int64(h.maxUploadMB) << 20
	if maxBytes <= 0 {
		maxBytes = 32 << 20
	}
	r.Body = http.MaxBytesReader(w, r.Body, maxBytes+1<<20)
	mr, err := r.MultipartReader()
	if err != nil {
		WriteOpenAIError(w, http.StatusBadRequest, "invalid multipart upload", "invalid_request_error", "invalid_upload")
		return
	}
	profileID := r.URL.Query().Get("chunk_profile_version_id")
	var jobID string
	for {
		part, nextErr := mr.NextPart()
		if errors.Is(nextErr, io.EOF) {
			break
		}
		if nextErr != nil {
			WriteOpenAIError(w, http.StatusBadRequest, "invalid multipart upload", "invalid_request_error", "invalid_upload")
			return
		}
		if jobID != "" {
			_ = part.Close()
			_ = h.service.Cancel(r.Context(), p, jobID)
			WriteOpenAIError(w, http.StatusBadRequest, "file must be the final multipart field", "invalid_request_error", "invalid_upload")
			return
		}
		if part.FormName() == "chunk_profile_version_id" {
			data, readErr := io.ReadAll(io.LimitReader(part, 256))
			_ = part.Close()
			if readErr != nil || len(data) == 256 {
				WriteOpenAIError(w, http.StatusBadRequest, "invalid chunk profile version", "invalid_request_error", "invalid_profile")
				return
			}
			profileID = strings.TrimSpace(string(data))
			continue
		}
		if part.FormName() != "file" || part.FileName() == "" || jobID != "" {
			_ = part.Close()
			WriteOpenAIError(w, http.StatusBadRequest, "exactly one file is required", "invalid_request_error", "invalid_upload")
			return
		}
		contentType := part.Header.Get("Content-Type")
		if _, _, err := mime.ParseMediaType(contentType); err != nil {
			contentType = "application/octet-stream"
		}
		job, submitErr := h.service.Submit(r.Context(), p, filepath.Base(part.FileName()), contentType, profileID, part)
		_ = part.Close()
		if submitErr != nil {
			writePreviewError(w, submitErr)
			return
		}
		jobID = job.ID
	}
	if jobID == "" {
		WriteOpenAIError(w, http.StatusBadRequest, "file is required", "invalid_request_error", "invalid_upload")
		return
	}
	// Reject trailing multipart fields after enqueue; the acknowledged request
	// has one immutable file/profile pair and no ignored authority-bearing data.
	WriteJSON(w, http.StatusAccepted, map[string]any{"job_id": jobID, "status": jobs.StatusQueued})
}

func (h *documentPreviewHandlers) preview(w http.ResponseWriter, r *http.Request, p auth.Principal) {
	setNoStoreHeaders(w)
	rest := strings.Trim(strings.TrimPrefix(r.URL.Path, "/api/v1/document-previews/"), "/")
	parts := strings.Split(rest, "/")
	if len(parts) == 0 || parts[0] == "" || len(parts) > 2 {
		WriteOpenAIError(w, http.StatusNotFound, "document preview not found", "invalid_request_error", "not_found")
		return
	}
	id := parts[0]
	if len(parts) == 2 && parts[1] == "original" && r.Method == http.MethodGet {
		reader, filename, contentType, err := h.service.Original(r.Context(), p, id)
		if err != nil {
			WriteOpenAIError(w, http.StatusNotFound, "document preview not found", "invalid_request_error", "not_found")
			return
		}
		defer reader.Close()
		w.Header().Set("Content-Type", contentType)
		w.Header().Set("Content-Disposition", mime.FormatMediaType("attachment", map[string]string{"filename": filename}))
		w.Header().Set("X-Content-Type-Options", "nosniff")
		_, _ = io.Copy(w, reader)
		return
	}
	if len(parts) == 1 && r.Method == http.MethodGet {
		status, err := h.service.Get(r.Context(), p, id)
		if err != nil {
			WriteOpenAIError(w, http.StatusNotFound, "document preview not found", "invalid_request_error", "not_found")
			return
		}
		WriteJSON(w, http.StatusOK, status)
		return
	}
	if len(parts) != 2 {
		WriteMethodNotAllowed(w)
		return
	}
	switch parts[1] {
	case "result":
		if r.Method != http.MethodGet {
			WriteMethodNotAllowed(w)
			return
		}
		result, err := h.service.Result(r.Context(), p, id)
		if err != nil {
			WriteOpenAIError(w, http.StatusNotFound, "document preview result not found", "invalid_request_error", "not_found")
			return
		}
		WriteJSON(w, http.StatusOK, result)
	case "cancel":
		if r.Method != http.MethodPost {
			WriteMethodNotAllowed(w)
			return
		}
		if err := h.service.Cancel(r.Context(), p, id); err != nil {
			WriteOpenAIError(w, http.StatusNotFound, "document preview not found", "invalid_request_error", "not_found")
			return
		}
		WriteJSON(w, http.StatusAccepted, map[string]any{"id": id, "status": "cancel_requested"})
	default:
		WriteOpenAIError(w, http.StatusNotFound, "document preview route not found", "invalid_request_error", "not_found")
	}
}

func writePreviewError(w http.ResponseWriter, err error) {
	if errors.Is(err, extract.ErrNotFound) {
		WriteOpenAIError(w, http.StatusNotFound, "document profile or artifact not found", "invalid_request_error", "not_found")
		return
	}
	if errors.Is(err, extract.ErrPreviewAdmissionLimited) {
		WriteOpenAIError(w, http.StatusTooManyRequests, err.Error(), "rate_limit_error", "preview_admission_limited")
		return
	}
	if errors.Is(err, artifacts.ErrOverLimit) || errors.Is(err, extract.ErrDocumentTooLarge) {
		WriteOpenAIError(w, http.StatusRequestEntityTooLarge, "document exceeds the upload limit", "invalid_request_error", "document_too_large")
		return
	}
	if errors.Is(err, extract.ErrUnsupportedFormat) {
		WriteOpenAIError(w, http.StatusBadRequest, "unsupported document format", "invalid_request_error", "invalid_document")
		return
	}
	if errors.Is(err, extract.ErrForbidden) || errors.Is(err, source.ErrDenied) {
		WriteForbidden(w, "document preview is not authorized")
		return
	}
	WriteOpenAIError(w, http.StatusInternalServerError, "document preview could not be created", "server_error", "internal_error")
}
