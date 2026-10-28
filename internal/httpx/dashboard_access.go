// dashboard_access.go — Access (OA-28, source §6.11): the tenant's five-tab
// identity-administration page — People, Roles, Service accounts, API keys and
// a strictly read-only Sign-in tab. Handlers render only (D-17): every write
// goes through the /api/v1 identities and admin key APIs from pages/access.js.
//
// Bounded authority (D-25, T-09-07-01/02): the roles editor's capability
// checklist lists only scopes the editing caller holds, and each AI-resource
// grant picker lists only the caller's own grantable items — models via the
// priced model selector, endpoints filtered by the caller's endpoint
// allowlist (id-only summaries: never credential refs, origins or runtime
// metadata), assistants and extract templates via their grant-intersected
// lists. The key drawer's scope checklist is the caller's own scope set. The
// APIs re-check everything server-side (scopesSubsetOfCaller,
// roleGrantsSubsetOfCaller, assignRoleChecked, principalCoversKeyGrants).
package httpx

import (
	"context"
	"log"
	"net/http"
	"sort"
	"strings"
	"time"

	"enclava-go/internal/auth"
)

// accessScopeOrder fixes the capability checklist order; accessScopeLabels
// renders scope strings as the plain words of source §3.1 — raw scope strings
// never reach the UI. platform.admin unlocks Tools, Access and Usage & billing
// and implies no feature scope, so it stays one plain entry.
var accessScopeOrder = []string{
	"chatbot", "rag", "rag.manage", "connectors.manage", "extract",
	"extract.manage", "chatbot.manage", "pricing.manage", auth.ScopePlatformAdmin,
}

var accessScopeLabels = map[string]string{
	"chatbot":               "Chat",
	"rag":                   "Search",
	"rag.manage":            "Knowledge manage",
	"connectors.manage":     "Sources manage",
	"extract":               "Extract",
	"extract.manage":        "Extract manage",
	"chatbot.manage":        "Assistants manage",
	"pricing.manage":        "Models manage",
	auth.ScopePlatformAdmin: "Administration (Tools, Access, Usage)",
}

// accessScopeLabel maps one scope to its plain word; scopes outside the
// §3.1 table humanize ("models.list" → "Models list") instead of leaking raw.
func accessScopeLabel(scope string) string {
	if label, ok := accessScopeLabels[scope]; ok {
		return label
	}
	return dashboardHumanize(scope)
}

// --- view model -------------------------------------------------------------

type accessScopeOption struct {
	Scope, Label string
	Checked      bool
}

type accessGrantItem struct {
	ID, Label string
	Checked   bool
}

// accessGrantSection is one AI-resource grant picker: an "All" toggle plus an
// item checklist. All renders checked only for a stored mode:"all" grant and
// is rendered only when the caller may grant all (CallerAll); new non-admin
// roles render every section deny-all.
type accessGrantSection struct {
	Kind, Label string
	All         bool
	CallerAll   bool
	Items       []accessGrantItem
}

type accessPerson struct {
	ID, Name, Email, Team, Status string
	Roles                         []string
	LastLogin                     *time.Time
}

type accessRoleChoice struct {
	ID, Name string
	Assigned bool
}

type accessSessionRow struct {
	ID          string
	Created     time.Time
	IdleExpires time.Time
	Current     bool
}

type accessUserDrawer struct {
	Create   bool
	ID       string
	Name     string
	Email    string
	Status   string
	Disabled bool
	Roles    []accessRoleChoice
	Sessions []accessSessionRow
}

type accessRoleRow struct {
	ID          string
	Name        string
	Members     int
	ScopeLabels []string
}

type accessRoleEditor struct {
	RoleID, Name string
	// Overreach marks a stored role carrying scopes or grants beyond the
	// editing caller's authority — saving it as rendered is refused by the
	// API, so the editor says so instead of silently dropping authority.
	Overreach bool
	Scopes    []accessScopeOption
	Grants    []accessGrantSection
}

type accessAccountRow struct {
	ID, Name, Status string
	Created          time.Time
}

type accessKeyRow struct {
	ID, Name, Prefix string
	ScopeLabels      []string
	Expires          *time.Time
	LastUsed         *time.Time
	Active           bool
}

type accessOwnerOption struct {
	ID, Label, Kind string // Kind "user" | "service_account"
}

type accessKeyDrawer struct {
	Owners []accessOwnerOption
	Scopes []accessScopeOption
}

type accessProviderRow struct{ Name, Issuer string }

// accessView is the Access page model; exactly one tab section is populated.
type accessView struct {
	Tab          string
	Tabs         []dashboardTab
	Unconfigured bool

	People      []accessPerson
	UserDrawer  *accessUserDrawer
	Roles       []accessRoleRow
	RoleEditor  *accessRoleEditor
	Accounts    []accessAccountRow
	AccountForm []accessScopeOption
	Keys        []accessKeyRow
	KeyDrawer   *accessKeyDrawer
	Providers   []accessProviderRow
}

func (v *accessView) DashboardTitle() string  { return "Access" }
func (v *accessView) DashboardModule() string { return "access" }

// --- people -----------------------------------------------------------------

// accessPersonStatus derives the pill state: disabled wins, then the
// outstanding-invite state (active row, never given a password).
func accessPersonStatus(u auth.User) string {
	if u.Status == "disabled" {
		return "disabled"
	}
	if u.PasswordSetAt == nil {
		return "invited"
	}
	return "active"
}

func accessPersonName(u auth.User) string {
	if name := strings.TrimSpace(u.DisplayName); name != "" {
		return name
	}
	return u.Email
}

func (a *App) accessPeople(ctx context.Context, tenantID string) ([]accessPerson, error) {
	users, err := a.identity.ListUsers(ctx, tenantID)
	if err != nil {
		return nil, err
	}
	out := make([]accessPerson, 0, len(users))
	for _, u := range users {
		roles, err := a.identity.RolesForUser(ctx, tenantID, u.ID)
		if err != nil {
			return nil, err
		}
		names := make([]string, 0, len(roles))
		for _, r := range roles {
			names = append(names, r.Name)
		}
		out = append(out, accessPerson{
			ID:        u.ID,
			Name:      accessPersonName(u),
			Email:     u.Email,
			Team:      u.Team,
			Status:    accessPersonStatus(u),
			Roles:     names,
			LastLogin: u.LastLoginAt,
		})
	}
	return out, nil
}

// --- roles ------------------------------------------------------------------

func (a *App) accessRoleRows(ctx context.Context, tenantID string) ([]accessRoleRow, error) {
	roles, err := a.identity.ListRoles(ctx, tenantID)
	if err != nil {
		return nil, err
	}
	out := make([]accessRoleRow, 0, len(roles))
	for _, r := range roles {
		members, err := a.identity.RoleUsers(ctx, tenantID, r.ID)
		if err != nil {
			return nil, err
		}
		labels := make([]string, 0, len(r.Scopes))
		for _, s := range r.Scopes {
			labels = append(labels, accessScopeLabel(s))
		}
		sort.Strings(labels)
		out = append(out, accessRoleRow{
			ID: r.ID, Name: r.Name, Members: len(members), ScopeLabels: labels,
		})
	}
	return out, nil
}

// accessScopeOptions lists the caller's own scopes as checklist entries —
// checked reflects the checked set, never the full catalog (D-25: an admin
// cannot grant what they do not hold).
func accessScopeOptions(p auth.Principal, checked []string) []accessScopeOption {
	set := make(map[string]bool, len(checked))
	for _, s := range checked {
		set[s] = true
	}
	out := make([]accessScopeOption, 0, len(p.Scopes))
	seen := make(map[string]bool, len(accessScopeOrder))
	for _, s := range accessScopeOrder {
		if !auth.HasScope(p, s) {
			continue
		}
		out = append(out, accessScopeOption{Scope: s, Label: accessScopeLabel(s), Checked: set[s]})
		seen[s] = true
	}
	extra := make([]string, 0, len(p.Scopes))
	for s := range p.Scopes {
		if !seen[s] {
			extra = append(extra, s)
		}
	}
	sort.Strings(extra)
	for _, s := range extra {
		out = append(out, accessScopeOption{Scope: s, Label: accessScopeLabel(s), Checked: set[s]})
	}
	return out
}

func allowlistHas(al auth.Allowlist, id string) bool {
	if al.All {
		return true
	}
	for _, item := range al.Items {
		if item == id {
			return true
		}
	}
	return false
}

// accessGrantSections builds the four AI-resource grant pickers. Item sources
// are caller-scoped reads; a failed source renders an empty section (logged),
// never a fabricated catalog. Item summaries carry id + display name only.
func (a *App) accessGrantSections(ctx context.Context, p auth.Principal, grants auth.ResourceGrants) []accessGrantSection {
	models := make([]accessGrantItem, 0, 8)
	if a.executor != nil {
		list, err := a.executor.PricedModelSelector(ctx, p, "")
		if err != nil {
			log.Printf("access roles: priced model selector: %v", err)
		}
		for _, m := range list {
			models = append(models, accessGrantItem{ID: m.ID, Label: m.ID, Checked: allowlistHas(grants.Models, m.ID)})
		}
		sort.Slice(models, func(i, j int) bool { return models[i].Label < models[j].Label })

	}
	endpoints := make([]accessGrantItem, 0, 8)
	if a.endpoints != nil {
		list, err := a.endpoints.List(ctx, p.TenantID)
		if err != nil {
			log.Printf("access roles: endpoint list: %v", err)
		}
		for _, e := range list {
			// Endpoint summaries stay id-only: safe grantable facts, never
			// credential refs, origins or provider runtime metadata (D-25).
			if !auth.EndpointAllowed(p, e.ID) {
				continue
			}
			endpoints = append(endpoints, accessGrantItem{ID: e.ID, Label: e.ID, Checked: allowlistHas(grants.Endpoints, e.ID)})
		}
	}
	assistants := make([]accessGrantItem, 0, 8)
	if a.chatbotSvc != nil {
		bots, err := a.chatbotSvc.List(ctx, p)
		if err != nil {
			log.Printf("access roles: chatbot list: %v", err)
		}
		for _, b := range bots {
			assistants = append(assistants, accessGrantItem{ID: b.ID, Label: b.Name, Checked: allowlistHas(grants.Assistants, b.ID)})
		}
	}
	templates := make([]accessGrantItem, 0, 8)
	if a.extractSvc != nil {
		list, err := a.extractSvc.ListExecutableTemplates(ctx, p)
		if err != nil {
			log.Printf("access roles: extract template list: %v", err)
		}
		for _, t := range list {
			templates = append(templates, accessGrantItem{ID: t.ID, Label: t.Name, Checked: allowlistHas(grants.ExtractTemplates, t.ID)})
		}
	}
	sortGrantItems := func(items []accessGrantItem) {
		sort.Slice(items, func(i, j int) bool { return items[i].Label < items[j].Label })
	}
	sortGrantItems(endpoints)
	sortGrantItems(assistants)
	sortGrantItems(templates)
	return []accessGrantSection{
		{Kind: "models", Label: "Models", All: grants.Models.All, CallerAll: p.AllowedModelsAll, Items: models},
		{Kind: "endpoints", Label: "Endpoints", All: grants.Endpoints.All, CallerAll: p.AllowedEndpointsAll, Items: endpoints},
		{Kind: "assistants", Label: "Assistants", All: grants.Assistants.All, CallerAll: p.AllowedChatbotsAll && p.AllowedAgentsAll, Items: assistants},
		{Kind: "extract_templates", Label: "Extraction templates", All: grants.ExtractTemplates.All, CallerAll: p.AllowedExtractTemplatesAll, Items: templates},
	}
}

func (a *App) accessRoleEditor(ctx context.Context, p auth.Principal, roleID string) (*accessRoleEditor, error) {
	e := &accessRoleEditor{}
	role := auth.Role{}
	if roleID != "" {
		stored, err := a.identity.GetRole(ctx, p.TenantID, roleID)
		if err != nil {
			return nil, err
		}
		role = stored
		e.RoleID = role.ID
		e.Name = role.Name
		e.Overreach = !scopesSubsetOfCaller(p, role.Scopes) || !roleGrantsSubsetOfCaller(p, role.Grants)
	}
	e.Scopes = accessScopeOptions(p, role.Scopes)
	e.Grants = a.accessGrantSections(ctx, p, role.Grants)
	return e, nil
}

// --- service accounts / api keys / providers --------------------------------

func (a *App) accessAccounts(ctx context.Context, tenantID string) ([]accessAccountRow, error) {
	accounts, err := a.identity.ListServiceAccounts(ctx, tenantID)
	if err != nil {
		return nil, err
	}
	out := make([]accessAccountRow, 0, len(accounts))
	for _, sa := range accounts {
		out = append(out, accessAccountRow{ID: sa.ID, Name: sa.Name, Status: sa.Status, Created: sa.CreatedAt})
	}
	return out, nil
}

func (a *App) accessKeys(ctx context.Context, tenantID string) ([]accessKeyRow, error) {
	keys, err := a.identity.ListAPIKeys(ctx, tenantID)
	if err != nil {
		return nil, err
	}
	out := make([]accessKeyRow, 0, len(keys))
	for _, k := range keys {
		if k.RevokedAt != nil {
			continue // revoked keys are removed from the operator view
		}
		items, all := auth.DecodeAllowlist(k.Scopes)
		var labels []string
		if all {
			labels = append(labels, "All scopes")
		}
		for s := range items {
			labels = append(labels, accessScopeLabel(s))
		}
		sort.Strings(labels)
		out = append(out, accessKeyRow{
			ID: k.ID, Name: k.Name, Prefix: k.KeyPrefix,
			ScopeLabels: labels, Expires: k.ExpiresAt, LastUsed: k.LastUsedAt,
			Active: k.IsActive,
		})
	}
	return out, nil
}

func (a *App) accessKeyDrawer(ctx context.Context, p auth.Principal) (*accessKeyDrawer, error) {
	d := &accessKeyDrawer{Scopes: accessScopeOptions(p, nil)}
	users, err := a.identity.ListUsers(ctx, p.TenantID)
	if err != nil {
		return nil, err
	}
	for _, u := range users {
		d.Owners = append(d.Owners, accessOwnerOption{ID: u.ID, Label: accessPersonName(u), Kind: "user"})
	}
	accounts, err := a.identity.ListServiceAccounts(ctx, p.TenantID)
	if err != nil {
		return nil, err
	}
	for _, sa := range accounts {
		d.Owners = append(d.Owners, accessOwnerOption{ID: sa.ID, Label: sa.Name, Kind: "service_account"})
	}
	sort.Slice(d.Owners, func(i, j int) bool {
		if d.Owners[i].Kind != d.Owners[j].Kind {
			return d.Owners[i].Kind < d.Owners[j].Kind
		}
		return d.Owners[i].Label < d.Owners[j].Label
	})
	return d, nil
}

func (a *App) accessProviders(ctx context.Context, tenantID string) []accessProviderRow {
	if a.sessionAuth == nil || a.sessionAuth.oidc == nil {
		return nil
	}
	providers, err := a.sessionAuth.oidc.EnabledProviders(ctx, tenantID)
	if err != nil {
		log.Printf("access sign-in: provider list: %v", err)
		return nil
	}
	out := make([]accessProviderRow, 0, len(providers))
	for _, pr := range providers {
		out = append(out, accessProviderRow{Name: pr.Name, Issuer: pr.Issuer})
	}
	return out
}

// --- view assembly + routes --------------------------------------------------

func (a *App) accessViewModel(r *http.Request, tab string) *accessView {
	v := &accessView{Tab: tab, Tabs: dashboardAccessTabs(tab)}
	p, _ := auth.PrincipalFromContext(r.Context())
	if a.identity == nil {
		v.Unconfigured = true
		return v
	}
	ctx := r.Context()
	var err error
	switch tab {
	case "people":
		v.People, err = a.accessPeople(ctx, p.TenantID)
	case "roles":
		v.Roles, err = a.accessRoleRows(ctx, p.TenantID)
	case "service-accounts":
		v.Accounts, err = a.accessAccounts(ctx, p.TenantID)
		if err == nil {
			v.AccountForm = accessScopeOptions(p, nil)
		}
	case "api-keys":
		v.Keys, err = a.accessKeys(ctx, p.TenantID)
	case "sign-in":
		v.Providers = a.accessProviders(ctx, p.TenantID)
	}
	if err != nil {
		log.Printf("access tab %s: %v", tab, err)
		v.Unconfigured = true
	}
	return v
}

// dashboardAccessTabs lists the five real tab URLs (§4); /dashboard/access
// renders the People view and the canonical links stay /dashboard/access/<tab>.
func dashboardAccessTabs(active string) []dashboardTab {
	tab := func(id, label string) dashboardTab {
		return dashboardTab{Label: label, Href: "/dashboard/access/" + id, Active: id == active}
	}
	return []dashboardTab{
		tab("people", "People"),
		tab("roles", "Roles"),
		tab("service-accounts", "Service accounts"),
		tab("api-keys", "API keys"),
		tab("sign-in", "Sign-in"),
	}
}

func (a *App) handleDashboardAccessTab(tab string) http.HandlerFunc {
	return a.sessionAuth.requireCap(capAccessManage, func(w http.ResponseWriter, r *http.Request) {
		if r.Method != http.MethodGet {
			WriteMethodNotAllowed(w)
			return
		}
		a.renderDashboard(w, r, "access", a.accessViewModel(r, tab))
	})
}

func (a *App) handleDashboardAccessPartial(tab string) http.HandlerFunc {
	return a.sessionAuth.requireCap(capAccessManage, func(w http.ResponseWriter, r *http.Request) {
		if r.Method != http.MethodGet {
			WriteMethodNotAllowed(w)
			return
		}
		a.renderDashboardFragment(w, r, "access", "access-"+tab, a.accessViewModel(r, tab))
	})
}

// handleDashboardAccessUserDrawer serves the invite/create drawer (no id) and
// the per-person manage drawer: roles checklist (PUT/DELETE …/roles/{roleID}
// via assignRoleChecked), password reset, enable/disable and the live session
// list with revocation — every write through the identities subroutes.
func (a *App) handleDashboardAccessUserDrawer(w http.ResponseWriter, r *http.Request) {
	if r.Method != http.MethodGet {
		WriteMethodNotAllowed(w)
		return
	}
	p, _ := auth.PrincipalFromContext(r.Context())
	if a.identity == nil {
		WriteOpenAIError(w, http.StatusNotImplemented, "identity management not configured", "server_error", "not_configured")
		return
	}
	ctx := r.Context()
	d := &accessUserDrawer{Create: true}
	roles, err := a.identity.ListRoles(ctx, p.TenantID)
	if err != nil {
		log.Printf("access user drawer: list roles: %v", err)
		WriteOpenAIError(w, http.StatusInternalServerError, "internal server error", "server_error", "roles_list_failed")
		return
	}
	sort.Slice(roles, func(i, j int) bool { return roles[i].Name < roles[j].Name })
	if id := strings.TrimSpace(r.FormValue("id")); id != "" {
		user, err := a.identity.GetUser(ctx, p.TenantID, id)
		if err != nil {
			a.writeIdentityError(w, err, "user_fetch_failed")
			return
		}
		d.Create = false
		d.ID, d.Name, d.Email = user.ID, accessPersonName(user), user.Email
		d.Status = accessPersonStatus(user)
		d.Disabled = user.Status == "disabled"
		assigned, err := a.identity.RolesForUser(ctx, p.TenantID, user.ID)
		if err != nil {
			log.Printf("access user drawer: roles for user: %v", err)
			WriteOpenAIError(w, http.StatusInternalServerError, "internal server error", "server_error", "roles_list_failed")
			return
		}
		assignedSet := make(map[string]bool, len(assigned))
		for _, ar := range assigned {
			assignedSet[ar.ID] = true
		}
		for _, ro := range roles {
			d.Roles = append(d.Roles, accessRoleChoice{ID: ro.ID, Name: ro.Name, Assigned: assignedSet[ro.ID]})
		}
		sessions, err := a.identity.ListSessionsForUser(ctx, p.TenantID, user.ID)
		if err != nil {
			log.Printf("access user drawer: sessions: %v", err)
			WriteOpenAIError(w, http.StatusInternalServerError, "internal server error", "server_error", "sessions_list_failed")
			return
		}
		for _, s := range sessions {
			if s.RevokedAt != nil {
				continue
			}
			d.Sessions = append(d.Sessions, accessSessionRow{
				ID: s.ID, Created: s.CreatedAt, IdleExpires: s.IdleExpiresAt,
				Current: s.ID == p.SessionID,
			})
		}
	} else {
		for _, ro := range roles {
			d.Roles = append(d.Roles, accessRoleChoice{ID: ro.ID, Name: ro.Name})
		}
	}
	a.renderDashboardFragment(w, r, "access", "access-user-drawer", &accessView{
		Tab: "people", UserDrawer: d,
	})
}

// handleDashboardAccessRoleEditor serves the role create/edit drawer: the
// capability checklist bounded to the caller's scopes and the four grant
// sections bounded to the caller's grantable items (D-25).
func (a *App) handleDashboardAccessRoleEditor(w http.ResponseWriter, r *http.Request) {
	if r.Method != http.MethodGet {
		WriteMethodNotAllowed(w)
		return
	}
	p, _ := auth.PrincipalFromContext(r.Context())
	if a.identity == nil {
		WriteOpenAIError(w, http.StatusNotImplemented, "identity management not configured", "server_error", "not_configured")
		return
	}
	editor, err := a.accessRoleEditor(r.Context(), p, strings.TrimSpace(r.FormValue("id")))
	if err != nil {
		a.writeIdentityError(w, err, "role_fetch_failed")
		return
	}
	a.renderDashboardFragment(w, r, "access", "access-role-editor", &accessView{
		Tab: "roles", RoleEditor: editor,
	})
}

// handleDashboardAccessKeyDrawer serves the key-create drawer: scope checklist
// ⊆ the caller's scopes (always submitted; the API re-checks with
// principalCoversKeyGrants and 403s anything wider) and an owner pick over the
// tenant's users and service accounts.
func (a *App) handleDashboardAccessKeyDrawer(w http.ResponseWriter, r *http.Request) {
	if r.Method != http.MethodGet {
		WriteMethodNotAllowed(w)
		return
	}
	p, _ := auth.PrincipalFromContext(r.Context())
	if a.identity == nil {
		WriteOpenAIError(w, http.StatusNotImplemented, "identity management not configured", "server_error", "not_configured")
		return
	}
	drawer, err := a.accessKeyDrawer(r.Context(), p)
	if err != nil {
		log.Printf("access key drawer: %v", err)
		WriteOpenAIError(w, http.StatusInternalServerError, "internal server error", "server_error", "api_keys_list_failed")
		return
	}
	a.renderDashboardFragment(w, r, "access", "access-key-drawer", &accessView{
		Tab: "api-keys", KeyDrawer: drawer,
	})
}

// registerDashboardAccessRoutes registers the five tab pages (People is the
// default /dashboard/access view), their refresh fragments and the three
// drawer fragments — every route under access.manage (T-09-07-03).
func (a *App) registerDashboardAccessRoutes() {
	for _, tab := range []string{"people", "roles", "service-accounts", "api-keys", "sign-in"} {
		a.mux.HandleFunc("/dashboard/access/"+tab, a.handleDashboardAccessTab(tab))
		a.mux.HandleFunc("/dashboard/partials/access/"+tab, a.handleDashboardAccessPartial(tab))
	}
	a.mux.HandleFunc("/dashboard/access", a.handleDashboardAccessTab("people"))
	a.mux.HandleFunc("/dashboard/partials/access/user-drawer", a.sessionAuth.requireCap(capAccessManage, a.handleDashboardAccessUserDrawer))
	a.mux.HandleFunc("/dashboard/partials/access/role-editor", a.sessionAuth.requireCap(capAccessManage, a.handleDashboardAccessRoleEditor))
	a.mux.HandleFunc("/dashboard/partials/access/key-drawer", a.sessionAuth.requireCap(capAccessManage, a.handleDashboardAccessKeyDrawer))
}

func init() {
	registerDashboardPage((*App).registerDashboardAccessRoutes)
}
