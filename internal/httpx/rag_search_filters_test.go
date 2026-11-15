package httpx

import (
	"encoding/json"
	"net/http"
	"net/http/httptest"
	"strings"
	"testing"

	"enclava-go/internal/auth"
	"enclava-go/internal/rag"
)

// TestRAGSearchRejectsMalformedMetadataFilters: a bad RFC 3339 date bound
// is a 400 `invalid_search_filter` on both search endpoints, before any
// store work (the zero service would fail later calls).
func TestRAGSearchRejectsMalformedMetadataFilters(t *testing.T) {
	app := &App{ragSvc: rag.NewService(nil, nil, nil, "", nil, nil)}
	for _, tc := range []struct{ after, before string }{
		{after: "not-a-date"},
		{after: "2026-01-01"},
		{before: "01/02/2026"},
	} {
		body, err := json.Marshal(map[string]string{"query": "hello", "updated_after": tc.after, "updated_before": tc.before})
		if err != nil {
			t.Fatal(err)
		}
		for _, crossCollection := range []bool{false, true} {
			req := httptest.NewRequest(http.MethodPost, "/", strings.NewReader(string(body)))
			response := httptest.NewRecorder()
			if crossCollection {
				app.handleRAGSearchReadable(response, req, auth.Principal{})
			} else {
				app.handleRAGSearch(response, req, auth.Principal{}, "collection")
			}
			var payload struct {
				Error struct {
					Code string `json:"code"`
				} `json:"error"`
			}
			if err := json.Unmarshal(response.Body.Bytes(), &payload); err != nil || response.Code != http.StatusBadRequest || payload.Error.Code != "invalid_search_filter" {
				t.Fatalf("cross=%v after=%q before=%q status=%d body=%s error=%v", crossCollection, tc.after, tc.before, response.Code, response.Body.String(), err)
			}
		}
	}
}
