package httpx

import (
	"encoding/json"
	"errors"
	"net/http"
	"net/http/httptest"
	"strings"
	"testing"

	"enclava-go/internal/extract"
	"enclava-go/internal/source"
)

func TestDocumentProfileViewUsesImmutableVersionWireFields(t *testing.T) {
	profile := profileView(source.ChunkProfile{
		ID: "cpv_1", ProfileID: "legal", Version: 2, SizeCodePoints: 120,
		OverlapCodePoints: 12, BoundaryStrategy: "clause", IncludeParents: true,
		TableHandling: "atomic", PreserveClauses: true,
	})
	encoded, err := json.Marshal(profile)
	if err != nil {
		t.Fatalf("marshal profile response: %v", err)
	}
	var fields map[string]any
	if err := json.Unmarshal(encoded, &fields); err != nil {
		t.Fatalf("decode profile response: %v", err)
	}
	for key, want := range map[string]any{
		"id": "cpv_1", "profile_id": "legal", "version": float64(2),
		"size_codepoints": float64(120), "overlap_codepoints": float64(12),
		"boundary_strategy": "clause", "include_parents": true,
		"table_handling": "atomic", "preserve_clauses": true,
	} {
		if fields[key] != want {
			t.Errorf("profile field %q = %#v, want %#v", key, fields[key], want)
		}
	}
	if _, ok := fields["id"].(string); !ok {
		t.Fatal("profile response omitted the immutable version id")
	}
}

func TestWritePreviewErrorTreatsUnknownFailuresAsInternal(t *testing.T) {
	recorder := httptest.NewRecorder()
	writePreviewError(recorder, errors.New("database unavailable"))
	if recorder.Code != http.StatusInternalServerError {
		t.Fatalf("status = %d, want %d", recorder.Code, http.StatusInternalServerError)
	}
	if strings.Contains(recorder.Body.String(), "database unavailable") {
		t.Fatalf("internal error leaked in response: %s", recorder.Body.String())
	}
	client := httptest.NewRecorder()
	writePreviewError(client, extract.ErrUnsupportedFormat)
	if client.Code != http.StatusBadRequest {
		t.Fatalf("known client error status = %d, want %d", client.Code, http.StatusBadRequest)
	}
}
