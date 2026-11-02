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

func TestRAGSearchRequiresUtilityModel(t *testing.T) {
	app := &App{ragSvc: rag.NewService(nil, nil, nil, "", nil, nil)}
	for _, crossCollection := range []bool{false, true} {
		req := httptest.NewRequest(http.MethodPost, "/", strings.NewReader(`{"query":"hello","rewrite":true}`))
		response := httptest.NewRecorder()
		if crossCollection {
			app.handleRAGSearchReadable(response, req, auth.Principal{})
		} else {
			app.handleRAGSearch(response, req, auth.Principal{}, "collection")
		}
		var body struct {
			Error struct {
				Code string `json:"code"`
			} `json:"error"`
		}
		if err := json.Unmarshal(response.Body.Bytes(), &body); err != nil || response.Code != http.StatusBadRequest || body.Error.Code != "utility_model_not_configured" {
			t.Fatalf("cross=%v status=%d body=%s error=%v", crossCollection, response.Code, response.Body.String(), err)
		}
	}
}
