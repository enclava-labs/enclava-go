package extractmodule

import (
	"net/http"

	"enclava-go/internal/extract"
	"enclava-go/internal/httpx"
)

type Module struct {
	svc         *extract.Service
	previews    *extract.PreviewService
	maxUploadMB int64
}

func New(svc *extract.Service, previews *extract.PreviewService, maxUploadMB int64) *Module {
	return &Module{svc: svc, previews: previews, maxUploadMB: maxUploadMB}
}

func (m *Module) Name() string { return "extract" }

func (m *Module) Register(mux *http.ServeMux, platform *httpx.App) {
	h := NewHandlers(m.svc, m.maxUploadMB, platform)
	mux.HandleFunc("/api/v1/extract/process", platform.Authenticated("extract", h.HandleProcess))
	mux.HandleFunc("/api/v1/extract/jobs", platform.Authenticated("extract", h.HandleJobsList))
	mux.HandleFunc("/api/v1/extract/jobs/", platform.Authenticated("extract", h.HandleJobDetail))
	mux.HandleFunc("/api/v1/extract/templates/reset-defaults", platform.Authenticated("extract.manage", h.HandleTemplatesResetDefaults))
	mux.HandleFunc("/api/v1/extract/templates/", platform.Authenticated("extract", h.HandleTemplateByID))
	mux.HandleFunc("/api/v1/extract/templates", platform.Authenticated("extract", h.HandleTemplates))
	mux.HandleFunc("/api/v1/extract/models", platform.Authenticated("extract", h.HandleModels))
	mux.HandleFunc("/api/v1/extract/settings", platform.Authenticated("extract", h.HandleSettings))
	mux.HandleFunc("/api/v1/extract/health", platform.Authenticated("extract", h.HandleHealth))
	httpx.RegisterDocumentPreviewRoutes(mux, platform, m.previews, m.maxUploadMB)
}
