// dashboard_tools.go — the Tools page (OA-26, source §6.9): the durable
// discovery store drives everything. The approval queue renders ONLY
// pending/changed rows persisted by phase 8's discovery reads (never a
// re-discovery), each with a field-level schema diff and an Approve action
// bound to the row's durable schema_version — the anti-TOCTOU control whose
// 409 stale-review path re-renders the queue. All writes go through the
// existing platform.admin /api/v1/mcp surface from the page module.
package httpx

import (
	"context"
	"encoding/json"
	"log"
	"net/http"
	"sort"
	"strings"
	"time"

	"enclava-go/internal/auth"
	"enclava-go/internal/mcp"
)

// toolsCallerScopeChoices is the page's editable caller-scope list: the
// scopes the platform's own tool-dispatch authorization actually reads
// (server audiences and approval caller scopes gate every invocation).
// Approve defaults to the full list; there is no free-text scope input.
var toolsCallerScopeChoices = []string{"assistant.use", "mcp.use"}

// toolsDefaultCallerScope is the audience-scope checkbox preselected when
// registering a server (the established tool-call scope).
const toolsDefaultCallerScope = "mcp.use"

type toolsScopeChoice struct {
	Name    string
	Checked bool
}

func toolsScopeChoices(checked []string) []toolsScopeChoice {
	want := make(map[string]bool, len(checked))
	for _, s := range checked {
		want[strings.TrimSpace(s)] = true
	}
	out := make([]toolsScopeChoice, 0, len(toolsCallerScopeChoices))
	for _, s := range toolsCallerScopeChoices {
		out = append(out, toolsScopeChoice{Name: s, Checked: want[s]})
	}
	return out
}

// toolsSchemaProp is one top-level input-schema property for display.
type toolsSchemaProp struct {
	Field, Type string
}

// toolsDiffRow is one changed field rendered old→new; Old/New are human
// type encodings ("—" where the field is absent on that side).
type toolsDiffRow struct {
	Field    string
	Change   string // "added" | "removed" | "type_changed" from diff_fields
	Old, New string
}

type toolsQueueRow struct {
	ServerID, ToolID string
	Name, Origin     string
	State            string
	SchemaVersion    int64
	ModelName        string // prior approval's model name; blank on first review
	RemoteName       string
	ReadOnly         bool
	WritesData       bool
	DiscoveredAt     time.Time
	Diff             []toolsDiffRow
	Props            []toolsSchemaProp // current input schema fields
	InputUnchanged   bool              // changed with no top-level input diffs
}

type toolsServerRow struct {
	ID, Origin, EndpointPath, CredentialRef string
	Enabled                                 bool
	DiscoveredAt                            *time.Time
	DiscoveryError                          string
	Approved, Total                         int
}

type toolsToolRow struct {
	ServerOrigin string
	ToolID       string
	ModelName    string
	RemoteName   string
	Builtin      bool
	ReadOnly     bool
	WritesData   bool
	UsedByList   string
}

// toolsData is the page model; it implements dashboardPageMeta.
type toolsData struct {
	Head struct {
		Org string
	}
	Queue                                []toolsQueueRow
	Servers                              []toolsServerRow
	Tools                                []toolsToolRow
	Scopes                               []toolsScopeChoice
	QueueEmpty, ServersEmpty, ToolsEmpty dashboardEmpty
}

func (toolsData) DashboardTitle() string  { return "Tools" }
func (toolsData) DashboardModule() string { return "tools" }

// toolsEffect reads the untrusted display hints exactly as the API wire
// surface does: read-only is opt-in, destructive defaults to writes-data.
func toolsEffect(an mcp.ToolAnnotations) (readOnly, writesData bool) {
	return an.ReadOnlyHint != nil && *an.ReadOnlyHint,
		an.DestructiveHint == nil || *an.DestructiveHint
}

// toolsTypeDisplay renders a schema "type" keyword compactly: "string",
// ["string","null"] kept as-is, anything non-JSON verbatim.
func toolsTypeDisplay(raw json.RawMessage) string {
	if len(raw) == 0 {
		return ""
	}
	var v any
	if err := json.Unmarshal(raw, &v); err != nil {
		return string(raw)
	}
	if s, ok := v.(string); ok {
		return s
	}
	c, err := json.Marshal(v)
	if err != nil {
		return string(raw)
	}
	return string(c)
}

// toolsSchemaProps lists the top-level input-schema properties sorted by
// field — the display twin of the diff producer's property scan.
func toolsSchemaProps(schema json.RawMessage) []toolsSchemaProp {
	var parsed struct {
		Properties map[string]json.RawMessage `json:"properties"`
	}
	if json.Unmarshal(schema, &parsed) != nil {
		return nil
	}
	out := make([]toolsSchemaProp, 0, len(parsed.Properties))
	for field, raw := range parsed.Properties {
		var prop struct {
			Type json.RawMessage `json:"type"`
		}
		_ = json.Unmarshal(raw, &prop)
		out = append(out, toolsSchemaProp{Field: field, Type: toolsTypeDisplay(prop.Type)})
	}
	sort.Slice(out, func(i, j int) bool { return out[i].Field < out[j].Field })
	return out
}

// toolsDiffRows renders diff_fields old→new against the persisted prior and
// current input schemas. Field-by-field truth comes only from the stored
// rows; nothing is inferred from a fresh discovery.
func toolsDiffRows(prior, current, diffFields json.RawMessage) []toolsDiffRow {
	var diffs []struct {
		Field  string `json:"field"`
		Change string `json:"change"`
	}
	if len(diffFields) > 0 {
		_ = json.Unmarshal(diffFields, &diffs)
	}
	changes := make(map[string]string, len(diffs))
	fields := make([]string, 0, len(diffs))
	for _, d := range diffs {
		if d.Field == "" {
			continue
		}
		if _, seen := changes[d.Field]; !seen {
			fields = append(fields, d.Field)
		}
		changes[d.Field] = d.Change
	}
	sort.Strings(fields)
	priorTypes := make(map[string]string)
	for _, p := range toolsSchemaProps(prior) {
		priorTypes[p.Field] = p.Type
	}
	currentTypes := make(map[string]string)
	for _, p := range toolsSchemaProps(current) {
		currentTypes[p.Field] = p.Type
	}
	out := make([]toolsDiffRow, 0, len(fields))
	for _, f := range fields {
		oldType, newType := priorTypes[f], currentTypes[f]
		if oldType == "" {
			oldType = "—"
		}
		if newType == "" {
			newType = "—"
		}
		out = append(out, toolsDiffRow{Field: f, Change: changes[f], Old: oldType, New: newType})
	}
	return out
}

// dashboardToolsData assembles the whole page from ONE durable read (all
// states) plus the server list, so queue, counts and the approved list can
// never disagree with the persisted store.
func (a *App) dashboardToolsData(ctx context.Context, p auth.Principal) (toolsData, error) {
	data := toolsData{Scopes: toolsScopeChoices(nil)}
	data.QueueEmpty = dashboardEmpty{Icon: "check", Text: "No tools pending approval."}
	data.ServersEmpty = dashboardEmpty{Icon: "tools", Text: "No servers registered yet."}
	data.ToolsEmpty = dashboardEmpty{Icon: "tools", Text: "No approved tools yet."}
	if caps, _ := capsFromContext(ctx); caps.ToolsManage {
		data.ServersEmpty.ActionLabel = "Add server"
		data.ServersEmpty.ActionHref = "/dashboard/tools?new=1"
	}
	data.Head.Org = a.dashboardOrgName(ctx, p.TenantID)
	if a.mcpRepo == nil {
		return data, nil
	}
	rows, err := a.mcpRepo.ListToolsByState(ctx, p.TenantID)
	if err != nil {
		return data, err
	}
	servers, err := a.mcpRepo.ListServers(ctx, p.TenantID)
	if err != nil {
		return data, err
	}

	origins := make(map[string]string, len(servers))
	for _, s := range servers {
		origins[s.ID] = s.Origin
	}
	approvedByServer, totalByServer := map[string]int{}, map[string]int{}
	approvedIDs := make([]string, 0, len(rows))
	for _, row := range rows {
		totalByServer[row.ServerID]++
		if row.State == "approved" {
			approvedByServer[row.ServerID]++
			approvedIDs = append(approvedIDs, row.ToolID)
		}
	}

	// Approval queue first: pending|changed only, with the durable version
	// the Approve PUT must echo.
	for _, row := range rows {
		if row.State != "pending" && row.State != "changed" {
			continue
		}
		name := row.Title
		if name == "" {
			name = row.RemoteName
		}
		readOnly, writesData := toolsEffect(row.Annotations)
		q := toolsQueueRow{
			ServerID: row.ServerID, ToolID: row.ToolID, Name: name,
			Origin: origins[row.ServerID], State: row.State,
			SchemaVersion: row.SchemaVersion, ModelName: row.ModelName,
			RemoteName: row.RemoteName, ReadOnly: readOnly, WritesData: writesData,
			DiscoveredAt: row.LastSeenAt,
		}
		if row.State == "changed" && len(row.PriorInputSchema) > 0 {
			q.Diff = toolsDiffRows(row.PriorInputSchema, row.InputSchema, row.DiffFields)
			q.Props = toolsSchemaProps(row.InputSchema)
			q.InputUnchanged = len(q.Diff) == 0
		} else {
			q.Props = toolsSchemaProps(row.InputSchema)
		}
		data.Queue = append(data.Queue, q)
	}

	for _, s := range servers {
		row := toolsServerRow{
			ID: s.ID, Origin: s.Origin, EndpointPath: s.EndpointPath,
			CredentialRef: s.CredentialRef, Enabled: s.Enabled,
			Approved: approvedByServer[s.ID], Total: totalByServer[s.ID],
		}
		if !s.DiscoveryAt.IsZero() {
			discovered := s.DiscoveryAt
			row.DiscoveredAt = &discovered
			row.DiscoveryError = s.DiscoveryError
		}
		data.Servers = append(data.Servers, row)
	}

	// Approved tool list with real assistant bindings; "—" when none.
	var usedBy map[string][]string
	if a.chatbotSvc != nil && len(approvedIDs) > 0 {
		if usedBy, err = a.chatbotSvc.ListAssistantToolBindings(ctx, p, approvedIDs); err != nil {
			return data, err
		}
	}
	for _, row := range rows {
		if row.State != "approved" {
			continue
		}
		readOnly, writesData := toolsEffect(row.Annotations)
		data.Tools = append(data.Tools, toolsToolRow{
			ServerOrigin: origins[row.ServerID], ToolID: row.ToolID,
			ModelName: row.ModelName, RemoteName: row.RemoteName,
			Builtin:  mcpBuiltinName(row.ModelName),
			ReadOnly: readOnly, WritesData: writesData,
			UsedByList: strings.Join(usedBy[row.ToolID], ", "),
		})
	}
	return data, nil
}

func (a *App) handleDashboardToolsPage(w http.ResponseWriter, r *http.Request) {
	if r.Method != http.MethodGet {
		WriteMethodNotAllowed(w)
		return
	}
	p, _ := auth.PrincipalFromContext(r.Context())
	data, err := a.dashboardToolsData(r.Context(), p)
	if err != nil {
		log.Printf("dashboard tools: %v", err)
		WriteOpenAIError(w, http.StatusInternalServerError, "internal server error", "server_error", "dashboard_tools_failed")
		return
	}
	a.renderDashboard(w, r, "tools", data)
}

// toolsFragment serves one queue/servers/list section — the same data the
// page rendered, so a refresh never shows a divergent view.
func (a *App) toolsFragment(w http.ResponseWriter, r *http.Request, name string) {
	if r.Method != http.MethodGet {
		WriteMethodNotAllowed(w)
		return
	}
	p, _ := auth.PrincipalFromContext(r.Context())
	data, err := a.dashboardToolsData(r.Context(), p)
	if err != nil {
		log.Printf("dashboard tools fragment %s: %v", name, err)
		WriteOpenAIError(w, http.StatusInternalServerError, "internal server error", "server_error", "dashboard_tools_failed")
		return
	}
	a.renderDashboardFragment(w, r, "tools", name, data)
}

func (a *App) handleDashboardToolsQueue(w http.ResponseWriter, r *http.Request) {
	a.toolsFragment(w, r, "tools-queue")
}

func (a *App) handleDashboardToolsServers(w http.ResponseWriter, r *http.Request) {
	a.toolsFragment(w, r, "tools-servers")
}

func (a *App) handleDashboardToolsList(w http.ResponseWriter, r *http.Request) {
	a.toolsFragment(w, r, "tools-list")
}

// handleDashboardToolsRegister renders the create drawer, or the edit drawer
// prefilled from the persisted server when ?server=<id> is present.
func (a *App) handleDashboardToolsRegister(w http.ResponseWriter, r *http.Request) {
	if r.Method != http.MethodGet {
		WriteMethodNotAllowed(w)
		return
	}
	var data struct {
		Server *mcpServerResponse
		Scopes []toolsScopeChoice
		Empty  dashboardEmpty
	}
	data.Empty = dashboardEmpty{Icon: "tools", Text: "No permissions available."}
	if id := strings.TrimSpace(r.URL.Query().Get("server")); id != "" && a.mcpRepo != nil {
		p, _ := auth.PrincipalFromContext(r.Context())
		server, err := a.mcpRepo.GetServer(r.Context(), p.TenantID, id)
		if err != nil {
			http.NotFound(w, r)
			return
		}
		jsonServer := mcpServerJSON(server)
		data.Server = &jsonServer
		data.Scopes = toolsScopeChoices(server.AudienceScopes)
	} else {
		data.Scopes = toolsScopeChoices([]string{toolsDefaultCallerScope})
	}
	a.renderDashboardFragment(w, r, "tools", "tools-register", data)
}

func (a *App) registerDashboardToolsRoutes() {
	a.mux.HandleFunc("/dashboard/tools", a.sessionAuth.requireCap(capToolsManage, a.handleDashboardToolsPage))
	a.mux.HandleFunc("/dashboard/partials/tools/queue", a.sessionAuth.requireCap(capToolsManage, a.handleDashboardToolsQueue))
	a.mux.HandleFunc("/dashboard/partials/tools/servers", a.sessionAuth.requireCap(capToolsManage, a.handleDashboardToolsServers))
	a.mux.HandleFunc("/dashboard/partials/tools/list", a.sessionAuth.requireCap(capToolsManage, a.handleDashboardToolsList))
	a.mux.HandleFunc("/dashboard/partials/tools/register", a.sessionAuth.requireCap(capToolsManage, a.handleDashboardToolsRegister))
}

func init() { registerDashboardPage((*App).registerDashboardToolsRoutes) }
