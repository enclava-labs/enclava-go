// dashboard_gallery.go — the dev-only component gallery (OA-07, P1.1 "done
// when"): one gated page that renders every UI-SPEC component inside the real
// shell, so the stylesheet and the renderDashboard architecture are provable
// before page phases build on them. The route exists only under
// ENCLAVA_DEV_GALLERY=1 and is deleted with this self-contained file at
// OA-41 (phase 12). Fixtures are plainly synthetic — the only real reads are
// the shell's own view model.
package httpx

import (
	"maps"
	"net/http"
	"os"
	"slices"
	"time"
)

func init() { registerDashboardPage((*App).registerDashboardGalleryRoutes) }

// galleryData is the synthetic fixture model feeding every component block.
// It implements dashboardPageMeta: a title, and an empty module — the gallery
// is static shell markup, behaviors arrive with app.js.
type galleryData struct {
	Now      time.Time
	LastSync time.Time // Now-2h, for relative-time cells
	Spent    int64     // micros, for the money() demo
	Spark    []float64
	Bars     []dashboardBar
	Icons    []string // every icon name, for the icon strip
}

func (g galleryData) DashboardTitle() string  { return "Component gallery" }
func (g galleryData) DashboardModule() string { return "" }

func newGalleryData() galleryData {
	return galleryData{
		Now:      time.Now(),
		LastSync: time.Now().Add(-2 * time.Hour),
		Spent:    1_240_500_000, // $1,240.50
		Spark:    []float64{3, 5, 4, 6, 5.5, 7, 8},
		Bars: []dashboardBar{
			{Label: "Mon", Value: 12}, {Label: "Tue", Value: 18}, {Label: "Wed", Value: 9},
			{Label: "Thu", Value: 22}, {Label: "Fri", Value: 15}, {Label: "Sat", Value: 6},
			{Label: "Sun", Value: 3},
		},
		Icons: slices.Sorted(maps.Keys(dashboardIconPaths)),
	}
}

// registerDashboardGalleryRoutes mounts GET /dashboard/dev/gallery (and its
// drawer fragment) behind capSession — session-only, any signed-in role. The
// env gate is checked at registration time: absent env → nothing registered,
// the phase-6 catch-all 404s.
func (a *App) registerDashboardGalleryRoutes() {
	if os.Getenv("ENCLAVA_DEV_GALLERY") != "1" {
		return
	}
	a.mux.HandleFunc("/dashboard/dev/gallery",
		galleryGetOnly(a.sessionAuth.capSession(func(w http.ResponseWriter, r *http.Request) {
			a.renderDashboard(w, r, "gallery", newGalleryData())
		})))
	a.mux.HandleFunc("/dashboard/dev/gallery/drawer",
		galleryGetOnly(a.sessionAuth.capSession(func(w http.ResponseWriter, r *http.Request) {
			a.renderDashboardFragment(w, r, "gallery", "drawer", newGalleryData())
		})))
}

// galleryGetOnly keeps the showcase read-only: a component gallery has no
// writes (D-17), so every other method is a bare 405.
func galleryGetOnly(next http.HandlerFunc) http.HandlerFunc {
	return func(w http.ResponseWriter, r *http.Request) {
		if r.Method != http.MethodGet {
			WriteMethodNotAllowed(w)
			return
		}
		next(w, r)
	}
}
