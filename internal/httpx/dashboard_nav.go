// dashboard_nav.go — the server-built nav model (OA-09, source §3.3): one
// static table, filtered per capability set. Templates never re-decide
// membership (§3.5 rule 3) — a group renders only with ≥1 visible item and a
// missing capability never renders its link.
package httpx

// dashboardNavItem is one nav entry. The static table carries Active=false
// always; dashboardNav sets it on the returned copy so the table stays clean.
type dashboardNavItem struct {
	ID, Label, Icon, Href string
	Cap                   dashboardCap
	Admin                 bool // ADMIN items carry the lockhint
	Active                bool // set per request; matched against the page's nav ID
}

type dashboardNavGroup struct {
	Title string
	Items []dashboardNavItem
}

// dashboardNavGroups is the §3.3 table in frozen order. Knowledge is gated on
// capSearch (read access, not knowledge.manage); Home and Account are
// session-only (Cap:"").
var dashboardNavGroups = []dashboardNavGroup{
	{Title: "WORK", Items: []dashboardNavItem{
		{ID: "home", Label: "Home", Icon: "home", Href: "/dashboard"},
		{ID: "chat", Label: "Chat", Icon: "chat", Href: "/dashboard/chat", Cap: capChat},
		{ID: "search", Label: "Search", Icon: "search", Href: "/dashboard/search", Cap: capSearch},
		{ID: "extract", Label: "Extract", Icon: "extract", Href: "/dashboard/extract", Cap: capExtract},
	}},
	{Title: "BUILD", Items: []dashboardNavItem{
		{ID: "assistants", Label: "Assistants", Icon: "assistant", Href: "/dashboard/assistants", Cap: capAssistantsManage},
		{ID: "knowledge", Label: "Knowledge", Icon: "knowledge", Href: "/dashboard/knowledge", Cap: capSearch},
		{ID: "sources", Label: "Sources", Icon: "sources", Href: "/dashboard/sources", Cap: capSourcesManage},
	}},
	{Title: "ADMIN", Items: []dashboardNavItem{
		{ID: "tools", Label: "Tools", Icon: "tools", Href: "/dashboard/tools", Cap: capToolsManage, Admin: true},
		{ID: "models", Label: "Models", Icon: "models", Href: "/dashboard/models", Cap: capModelsManage, Admin: true},
		{ID: "access", Label: "Access", Icon: "users", Href: "/dashboard/access", Cap: capAccessManage, Admin: true},
		{ID: "usage", Label: "Usage & billing", Icon: "usage", Href: "/dashboard/usage", Cap: capBillingView, Admin: true},
	}},
}

// dashboardNav filters the static table by caps, drops empty groups and marks
// the active item (exactly one when active matches a rendered item ID).
func dashboardNav(caps dashboardCaps, active string) []dashboardNavGroup {
	out := make([]dashboardNavGroup, 0, len(dashboardNavGroups))
	for _, g := range dashboardNavGroups {
		ng := dashboardNavGroup{Title: g.Title}
		for _, it := range g.Items {
			if it.Cap != "" && !caps.grants(it.Cap) {
				continue
			}
			it.Active = it.ID == active
			ng.Items = append(ng.Items, it)
		}
		if len(ng.Items) > 0 {
			out = append(out, ng)
		}
	}
	return out
}
