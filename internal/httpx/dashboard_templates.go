// dashboard_templates.go — phase-7 app template architecture (D-22): the app
// tree parses once into a base set (layout + components + partials), and each
// page gets a Clone with its pages/<page>.tmpl parsed in. Every template error
// names its page. The frozen FuncMap names — statusPill statusLabel statusTone
// term relTime absTime money count icon spark bars mark — are the only helpers
// templates may call (D-21). The legacy dashboardTemplateFS/dashboardTemplates
// set is untouched until OA-41 deletes it.
package httpx

import (
	"embed"
	"fmt"
	"html/template"
	"io/fs"
	"os"
	"sync"
)

//go:embed dashboard/templates/app
var dashboardAppTemplateFS embed.FS

// Shared component args consumed by components.tmpl (dashboard_view.go, which
// lands in 07-03, holds the page-level dashboardView).
type dashboardTab struct {
	Label, Href string
	Active      bool
}

type dashboardEmpty struct {
	Icon, Text, ActionLabel, ActionHref string
}

type dashboardHeader struct {
	Crumb, Title, Desc string
	Actions            template.HTML
}

// dashboardConfirm is the arg of the "confirm-btn" component: the visible
// label and the data-confirm question ("verb noun?").
type dashboardConfirm struct {
	Label, Confirm string
}

// dashboardBaseFuncs registers the frozen helper names; all must exist before
// layout/components parse (templates reference them at parse time).
var dashboardBaseFuncs = template.FuncMap{
	"statusPill":  statusPill,
	"statusLabel": statusLabel,
	"statusTone":  statusTone,
	"term":        term,
	"relTime":     relTime,
	"absTime":     absTime,
	"money":       money,
	"count":       count,
	"icon":        icon,
	"spark":       spark,
	"bars":        bars,
	"mark":        mark,
}

var (
	dashboardBaseOnce sync.Once
	dashboardBaseSet  *template.Template
	dashboardBaseErr  error
)

// dashboardBaseTemplates parses the shared app tree once: layout.tmpl plus the
// components/ and partials/ globs. Globs matching zero files are skipped —
// partials/ legitimately has no files until 07-03, and ParseFS rejects empty
// matches. Layout.tmpl not existing yet is fine: the base builds lazily at the
// first page request.
func dashboardBaseTemplates() (*template.Template, error) {
	dashboardBaseOnce.Do(func() {
		globs := []string{
			"dashboard/templates/app/layout.tmpl",
			"dashboard/templates/app/components/*.tmpl",
			"dashboard/templates/app/partials/*.tmpl",
		}
		var present []string
		for _, g := range globs {
			if m, err := fs.Glob(dashboardAppTemplateFS, g); err == nil && len(m) > 0 {
				present = append(present, g)
			}
		}
		if len(present) == 0 {
			dashboardBaseErr = fmt.Errorf("dashboard app templates: no layout, components or partials found")
			return
		}
		t := template.New("app").Funcs(dashboardBaseFuncs)
		for _, g := range present {
			if t, dashboardBaseErr = t.ParseFS(dashboardAppTemplateFS, g); dashboardBaseErr != nil {
				return
			}
		}
		dashboardBaseSet = t
	})
	return dashboardBaseSet, dashboardBaseErr
}

type dashboardTemplateSetEntry struct {
	set *template.Template
	err error
}

var dashboardTemplateSets sync.Map // page name → dashboardTemplateSetEntry

// dashboardTemplateSet is the single entry page handlers (07-03
// renderDashboard/renderDashboardFragment) call through: parse-once base,
// clone per page, page file parsed into the clone. Safe for concurrent use —
// at most one build per page, and a parse error is cached (restart to pick up
// template fixes). 07-03 executes the result into a buffer before any 200
// (P7-10); the cloned set is the primitive that contract needs.
func dashboardTemplateSet(page string) (*template.Template, error) {
	if v, ok := dashboardTemplateSets.Load(page); ok {
		e := v.(dashboardTemplateSetEntry)
		return e.set, e.err
	}
	e := dashboardBuildTemplateSet(page)
	dashboardTemplateSets.Store(page, e)
	return e.set, e.err
}

func dashboardBuildTemplateSet(page string) dashboardTemplateSetEntry {
	pagePath := "dashboard/templates/app/pages/" + page + ".tmpl"
	// Stat the page file FIRST so a missing page names the page even before
	// layout.tmpl exists (D-22: a template error names its page).
	if _, err := fs.Stat(dashboardAppTemplateFS, pagePath); err != nil {
		return dashboardTemplateSetEntry{err: fmt.Errorf("dashboard page %q: %w", page, os.ErrNotExist)}
	}
	base, err := dashboardBaseTemplates()
	if err != nil {
		return dashboardTemplateSetEntry{err: fmt.Errorf("dashboard page %q: %w", page, err)}
	}
	set, err := base.Clone()
	if err != nil {
		return dashboardTemplateSetEntry{err: fmt.Errorf("dashboard page %q: %w", page, err)}
	}
	src, err := fs.ReadFile(dashboardAppTemplateFS, pagePath)
	if err != nil {
		return dashboardTemplateSetEntry{err: fmt.Errorf("dashboard page %q: %w", page, err)}
	}
	if _, err := set.Parse(string(src)); err != nil {
		return dashboardTemplateSetEntry{err: fmt.Errorf("dashboard page %q: %w", page, err)}
	}
	return dashboardTemplateSetEntry{set: set}
}

var (
	dashboardAuthOnce   sync.Once
	dashboardAuthSet    *template.Template
	dashboardAuthLoaded bool
)

// dashboardAuthTemplates parses dashboard/templates/app/auth/*.tmpl with the
// base funcs. The files land in 07-05; until then an empty (unparsed) set is
// returned and ExecuteTemplate fails into the caller's logging — panic-free by
// design so NewSessionAuth wiring cannot take the process down.
func dashboardAuthTemplates() *template.Template {
	dashboardAuthOnce.Do(func() {
		t := template.New("auth").Funcs(dashboardBaseFuncs)
		if m, err := fs.Glob(dashboardAppTemplateFS, "dashboard/templates/app/auth/*.tmpl"); err == nil && len(m) > 0 {
			if t, err = t.ParseFS(dashboardAppTemplateFS, "dashboard/templates/app/auth/*.tmpl"); err == nil {
				dashboardAuthLoaded = true
			}
		}
		dashboardAuthSet = t
	})
	return dashboardAuthSet
}

// DashboardAuthTemplates is the exported twin the NewSessionAuth wiring in
// cmd/enclava/main.go switches to in 07-05.
func DashboardAuthTemplates() *template.Template {
	return dashboardAuthTemplates()
}
