package httpx

import (
	"net/http"

	"enclava-go/internal/auth"
)

// handleRAGCollectionReprocessFailed answers POST
// /api/v1/rag/collections/{id}/reprocess-failed: the collection-page
// "Retry failed" bulk form of POST /api/v1/rag/documents/{id}/reprocess.
// Same gate (rag.manage), same per-document pipeline; documents the caller
// cannot read are skipped, so {"requeued": n} counts only what this caller
// actually requeued.
func (a *App) handleRAGCollectionReprocessFailed(w http.ResponseWriter, r *http.Request, principal auth.Principal, collectionID string) {
	if a.ragSvc == nil {
		WriteOpenAIError(w, http.StatusNotImplemented, "rag is not configured", "server_error", "not_configured")
		return
	}
	if r.Method != http.MethodPost {
		WriteMethodNotAllowed(w)
		return
	}
	if !auth.HasScope(principal, "rag.manage") {
		WriteForbidden(w, "missing required scope: rag.manage")
		return
	}
	requeued, err := a.ragSvc.ReprocessFailedDocuments(r.Context(), principal, collectionID)
	if err != nil {
		a.writeRAGNotFoundOrBadRequest(w, err, "rag_reprocess_failed_failed")
		return
	}
	WriteJSON(w, http.StatusAccepted, map[string]any{"requeued": requeued})
}
