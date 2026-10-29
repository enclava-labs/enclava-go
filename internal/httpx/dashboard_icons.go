// dashboard_icons.go — the 45 inline-SVG icon path bodies ported verbatim
// from docs/ux-proposals/data.js. Color comes from CSS (stroke uses
// currentColor), size from the width/height attributes icon() emits; unknown
// names render the loud "alert" glyph, never blank.
package httpx

import (
	"html/template"
	"strconv"
)

var dashboardIconPaths = map[string]string{
	"home":         `<path d="M3 10.5 12 3l9 7.5"/><path d="M5 9.5V21h14V9.5"/><path d="M10 21v-6h4v6"/>`,
	"chat":         `<path d="M21 15a2 2 0 0 1-2 2H8l-5 4V5a2 2 0 0 1 2-2h14a2 2 0 0 1 2 2z"/>`,
	"search":       `<circle cx="11" cy="11" r="7"/><path d="m20 20-3.5-3.5"/>`,
	"extract":      `<path d="M14 3H6a2 2 0 0 0-2 2v14a2 2 0 0 0 2 2h12a2 2 0 0 0 2-2V9z"/><path d="M14 3v6h6"/><path d="M8 13h8M8 17h5"/>`,
	"file":         `<path d="M14 3H6a2 2 0 0 0-2 2v14a2 2 0 0 0 2 2h12a2 2 0 0 0 2-2V9z"/><path d="M14 3v6h6"/>`,
	"assistant":    `<rect x="4" y="8" width="16" height="12" rx="3"/><path d="M12 8V4M9 4h6"/><circle cx="9" cy="14" r="1.2"/><circle cx="15" cy="14" r="1.2"/>`,
	"knowledge":    `<ellipse cx="12" cy="5" rx="8" ry="3"/><path d="M4 5v14c0 1.7 3.6 3 8 3s8-1.3 8-3V5"/><path d="M4 12c0 1.7 3.6 3 8 3s8-1.3 8-3"/>`,
	"sources":      `<path d="M9 2v6M15 2v6"/><path d="M6 8h12v4a6 6 0 0 1-12 0z"/><path d="M12 18v4"/>`,
	"tools":        `<path d="M14.7 6.3a1 1 0 0 0 0 1.4l1.6 1.6a1 1 0 0 0 1.4 0l3.77-3.77a6 6 0 0 1-7.94 7.94l-6.91 6.91a2.12 2.12 0 0 1-3-3l6.91-6.91a6 6 0 0 1 7.94-7.94l-3.76 3.76z"/>`,
	"models":       `<rect x="6" y="6" width="12" height="12" rx="2"/><rect x="9.5" y="9.5" width="5" height="5" rx="1"/><path d="M9 2v4M15 2v4M9 18v4M15 18v4M2 9h4M2 15h4M18 9h4M18 15h4"/>`,
	"key":          `<circle cx="7.5" cy="15.5" r="4.5"/><path d="m10.7 12.3 9.3-9.3M17 6l3 3M14 9l2 2"/>`,
	"users":        `<circle cx="9" cy="8" r="4"/><path d="M2 21a7 7 0 0 1 14 0"/><path d="M16 4a4 4 0 0 1 0 8M22 21a7 7 0 0 0-5-6.7"/>`,
	"usage":        `<path d="M3 3v18h18"/><path d="m7 15 4-5 3 3 5-7"/>`,
	"billing":      `<path d="M5 2h14v20l-3-2-2 2-2-2-2 2-2-2-3 2z"/><path d="M9 7h6M9 11h6M9 15h4"/>`,
	"audit":        `<path d="M10 6h11M10 12h11M10 18h11"/><path d="m3 6 1.5 1.5L7 5M3 12l1.5 1.5L7 11M3 18l1.5 1.5L7 17"/>`,
	"settings":     `<path d="M4 6h10M18 6h2M4 12h4M12 12h8M4 18h12"/><circle cx="16" cy="6" r="2"/><circle cx="10" cy="12" r="2"/><circle cx="18" cy="18" r="2"/>`,
	"layers":       `<path d="m12 2 10 5-10 5L2 7z"/><path d="m2 12 10 5 10-5"/><path d="m2 17 10 5 10-5"/>`,
	"shield":       `<path d="M12 3 4 6v6c0 5 3.4 8.5 8 9 4.6-.5 8-4 8-9V6z"/><path d="m9 12 2 2 4-4"/>`,
	"lock":         `<rect x="5" y="11" width="14" height="10" rx="2"/><path d="M8 11V7a4 4 0 0 1 8 0v4"/>`,
	"activity":     `<path d="M3 12h4l3-8 4 16 3-8h4"/>`,
	"bell":         `<path d="M6 8a6 6 0 0 1 12 0c0 7 3 9 3 9H3s3-2 3-9"/><path d="M10.3 21a1.94 1.94 0 0 0 3.4 0"/>`,
	"plus":         `<path d="M12 5v14M5 12h14"/>`,
	"chevronDown":  `<path d="m6 9 6 6 6-6"/>`,
	"chevronRight": `<path d="m9 6 6 6-6 6"/>`,
	"chevronLeft":  `<path d="m15 6-6 6 6 6"/>`,
	"check":        `<path d="m5 12 5 5 9-10"/>`,
	"x":            `<path d="M6 6l12 12M18 6 6 18"/>`,
	"alert":        `<path d="M12 3 2 20h20z"/><path d="M12 9v5M12 17h.01"/>`,
	"info":         `<circle cx="12" cy="12" r="9"/><path d="M12 11v5M12 8h.01"/>`,
	"refresh":      `<path d="M20 11a8 8 0 0 0-14.9-3M4 4v4h4"/><path d="M4 13a8 8 0 0 0 14.9 3M20 20v-4h-4"/>`,
	"external":     `<path d="M14 4h6v6M20 4l-9 9"/><path d="M18 14v5a1 1 0 0 1-1 1H5a1 1 0 0 1-1-1V7a1 1 0 0 1 1-1h5"/>`,
	"filter":       `<path d="M3 5h18l-7 8v6l-4 2v-8z"/>`,
	"upload":       `<path d="M12 16V4M7 9l5-5 5 5"/><path d="M4 16v3a2 2 0 0 0 2 2h12a2 2 0 0 0 2-2v-3"/>`,
	"send":         `<path d="M22 2 11 13"/><path d="M22 2 15 22l-4-9-9-4z"/>`,
	"stop":         `<rect x="7" y="7" width="10" height="10" rx="1.5"/>`,
	"clock":        `<circle cx="12" cy="12" r="9"/><path d="M12 7v5l3 2"/>`,
	"sparkles":     `<path d="m12 3 1.8 4.7 4.7 1.8-4.7 1.8L12 16l-1.8-4.7-4.7-1.8 4.7-1.8z"/><path d="m19 15 .9 2.1 2.1.9-2.1.9L19 21l-.9-2.1-2.1-.9 2.1-.9z"/>`,
	"sun":          `<circle cx="12" cy="12" r="4"/><path d="M12 2v2M12 20v2M4.9 4.9l1.4 1.4M17.7 17.7l1.4 1.4M2 12h2M20 12h2M4.9 19.1l1.4-1.4M17.7 6.3l1.4-1.4"/>`,
	"moon":         `<path d="M21 12.8A9 9 0 1 1 11.2 3a7 7 0 0 0 9.8 9.8z"/>`,
	"menu":         `<path d="M4 6h16M4 12h16M4 18h16"/>`,
	"command":      `<path d="M9 6a3 3 0 1 0-3 3h12a3 3 0 1 0-3-3v12a3 3 0 1 0 3-3H6a3 3 0 1 0 3 3z"/>`,
	"link":         `<path d="M10 13a5 5 0 0 0 7.5.5l3-3a5 5 0 0 0-7-7l-1.7 1.7"/><path d="M14 11a5 5 0 0 0-7.5-.5l-3 3a5 5 0 0 0 7 7l1.7-1.7"/>`,
	"copy":         `<rect x="9" y="9" width="12" height="12" rx="2"/><path d="M5 15H4a1 1 0 0 1-1-1V4a1 1 0 0 1 1-1h10a1 1 0 0 1 1 1v1"/>`,
	"logout":       `<path d="M9 21H5a2 2 0 0 1-2-2V5a2 2 0 0 1 2-2h4"/><path d="m16 17 5-5-5-5M21 12H9"/>`,
	"more":         `<circle cx="5" cy="12" r="1.6" fill="currentColor" stroke="none"/><circle cx="12" cy="12" r="1.6" fill="currentColor" stroke="none"/><circle cx="19" cy="12" r="1.6" fill="currentColor" stroke="none"/>`,
}

// icon renders the named icon as an inline decorative SVG at size px (default
// 16, as in the option-a mockup's icon(name, size = 16)); unknown names fall
// back to the alert glyph (loud failure, T-07-02-02). The width/height
// attributes are what keep an icon from stretching to its container.
func icon(name string, size ...int) template.HTML {
	body, ok := dashboardIconPaths[name]
	if !ok {
		body = dashboardIconPaths["alert"]
	}
	px := 16
	if len(size) > 0 && size[0] > 0 {
		px = size[0]
	}
	dim := strconv.Itoa(px)
	return template.HTML(`<svg width="` + dim + `" height="` + dim + `" viewBox="0 0 24 24" fill="none" stroke="currentColor" stroke-width="1.8" stroke-linecap="round" stroke-linejoin="round" aria-hidden="true">` + body + `</svg>`)
}
