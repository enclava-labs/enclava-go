// dashboard_assistants.go — Assistants + Instructions (OA-24, plan 09-03):
// the manager-facing assistants table with the configure drawer, and the
// prompt-template Instructions tab whose editor serializes labeled variable
// rows into variables_schema — no raw JSON input anywhere (source §12/OA-34).
//
// Obsolete-surface handoff: this plan replaces /dashboard/chatbots and
// /dashboard/prompt-templates, but the old chatbots/prompt-templates family
// (handleDashboardPageChatbots, handleDashboardPagePromptTemplates,
// handleDashboardChatbots, handleDashboardPromptTemplates,
// handleDashboardChatbotAction, handleDashboardPromptTemplateAction,
// partial_chatbots.tmpl, page_agents.tmpl, partial_prompt_templates.tmpl,
// agents.js) stays live until phase-10 plan 10-01's Work cutover deletes it —
// NOT 09-10. These handlers render only; every write goes through
// /api/v1 via Enclava.api (D-17).
package httpx

import (
	"context"
	"database/sql"
	"encoding/json"
	"errors"
	"fmt"
	"log"
	"net/http"
	"sort"
	"strings"
	"time"

	"enclava-go/internal/auth"
	"enclava-go/internal/chatbot"
	"enclava-go/internal/inference"
	"enclava-go/internal/prompts"
)

func init() { registerDashboardPage((*App).registerDashboardAssistantsRoutes) }

// --- page + fragment view models ---------------------------------------------

type assistantsPageData struct {
	Table assistantsTableData
	Tabs  []dashboardTab
	Empty dashboardEmpty
}

func (d assistantsPageData) DashboardTitle() string  { return "Assistants" }
func (d assistantsPageData) DashboardModule() string { return "assistants" }

type assistantsTableData struct {
	Rows []assistantRow
}

type assistantRow struct {
	ID, Name, Description string
	Initials, AvatarColor string
	Model                 string
	Endpoint              string // pinned endpoint label; "" = route picked at request time
	ConfState             string // "verified" | "unverified" | "" (no chip: unpinned or not confidential)
	Knowledge             []string
	Tools                 []string
	Audience              string // the stored audience rendered verbatim
	StatusState           string // "active" | "disabled"
	Conversations         int64
}

type instructionsPageData struct {
	List  instructionsListData
	Tabs  []dashboardTab
	Empty dashboardEmpty
}

func (d instructionsPageData) DashboardTitle() string  { return "Instructions" }
func (d instructionsPageData) DashboardModule() string { return "assistants" }

type instructionsListData struct {
	Templates []instructionRow
}

type instructionRow struct {
	ID, Name, Description string
	IsDefault             bool
	Variables             int
	UpdatedAt             time.Time
}

// assistantDrawerData backs the configure drawer fragment: every select is
// populated server-side, the stored state preselected, and non-tool settings
// carried through hidden preserving inputs so a drawer save never drops them.
type assistantDrawerData struct {
	Assistant *chatbot.Instance

	Models       []assistantModelOption
	CurrentModel string // stored model absent from the priced list — kept selectable, never silently switched

	Templates []assistantTemplateOption
	Knowledge []assistantKnowledgeOption
	Tools     []assistantToolOption
	Members   []assistantMember

	Preserved []assistantSetting

	AudienceAll bool
}

type assistantModelOption struct {
	ID      string
	Current bool
}

type assistantTemplateOption struct {
	ID, Name     string
	SystemPrompt string
	UserPrompt   string
	Selected     bool
}

type assistantKnowledgeOption struct {
	ID, Name string
	Selected bool
}

type assistantToolOption struct {
	ID, Label string
	Approved  bool // false → disabled checkbox with "Awaiting approval"
	Selected  bool
}

type assistantMember struct {
	ID, Label string
	Selected  bool
}

type assistantSetting struct {
	Key, JSON string
}

// instructionEditorData backs the instructions editor fragment. Variables are
// labeled rows; the page module serializes them into the variables_schema
// shape prompts.Service stores ({type:object,properties:{…}}).
type instructionEditorData struct {
	Template *prompts.Template
	Vars     []instructionVarRow
}

type instructionVarRow struct {
	Name, Description string
	Required          bool
}

// --- routes ------------------------------------------------------------------

func (a *App) registerDashboardAssistantsRoutes() {
	a.mux.HandleFunc("/dashboard/assistants", assistantsGetOnly(a.sessionAuth.requireCap(capAssistantsManage, a.handleDashboardAssistantsPage)))
	a.mux.HandleFunc("/dashboard/assistants/instructions", assistantsGetOnly(a.sessionAuth.requireCap(capAssistantsManage, a.handleDashboardInstructionsPage)))
	a.mux.HandleFunc("/dashboard/partials/assistants/table", assistantsGetOnly(a.sessionAuth.requireCap(capAssistantsManage, a.handleDashboardAssistantsTable)))
	a.mux.HandleFunc("/dashboard/partials/assistants/drawer", assistantsGetOnly(a.sessionAuth.requireCap(capAssistantsManage, a.handleDashboardAssistantDrawer)))
	a.mux.HandleFunc("/dashboard/partials/assistants/instructions", assistantsGetOnly(a.sessionAuth.requireCap(capAssistantsManage, a.handleDashboardInstructionsList)))
	a.mux.HandleFunc("/dashboard/partials/assistants/instructions/editor", assistantsGetOnly(a.sessionAuth.requireCap(capAssistantsManage, a.handleDashboardInstructionEditor)))
}

// assistantsGetOnly keeps the surface read-only: renders never mutate (D-17),
// every other method is a bare 405.
func assistantsGetOnly(next http.HandlerFunc) http.HandlerFunc {
	return func(w http.ResponseWriter, r *http.Request) {
		if r.Method != http.MethodGet {
			WriteMethodNotAllowed(w)
			return
		}
		next(w, r)
	}
}

// assistantsTabs is the shared Assistants · Instructions tab strip (real URLs,
// aria-current via the tabs component).
func assistantsTabs(active string) []dashboardTab {
	return []dashboardTab{
		{Label: "Assistants", Href: "/dashboard/assistants", Active: active == "assistants"},
		{Label: "Instructions", Href: "/dashboard/assistants/instructions", Active: active == "instructions"},
	}
}

// --- pages -------------------------------------------------------------------

func (a *App) handleDashboardAssistantsPage(w http.ResponseWriter, r *http.Request) {
	p, _ := auth.PrincipalFromContext(r.Context())
	data, err := a.assistantsTableData(r.Context(), p)
	if err != nil {
		log.Printf("dashboard assistants: %v", err)
		WriteOpenAIError(w, http.StatusInternalServerError, "internal server error", "server_error", "dashboard_assistants_failed")
		return
	}
	data.Tabs = assistantsTabs("assistants")
	a.renderDashboard(w, r, "assistants", data)
}

func (a *App) handleDashboardInstructionsPage(w http.ResponseWriter, r *http.Request) {
	p, _ := auth.PrincipalFromContext(r.Context())
	data, err := a.instructionsListData(r.Context(), p)
	if err != nil {
		log.Printf("dashboard instructions: %v", err)
		WriteOpenAIError(w, http.StatusInternalServerError, "internal server error", "server_error", "dashboard_instructions_failed")
		return
	}
	data.Tabs = assistantsTabs("instructions")
	// Sub-page of Assistants: render pages/assistants_instructions.tmpl but
	// keep the sidebar's Assistants item active (the set/page split mirrors
	// renderDashboard's own two arguments).
	set, err := dashboardTemplateSet("assistants_instructions")
	if err != nil {
		a.renderDashboardTemplateError(w, "assistants_instructions", err)
		return
	}
	a.renderDashboardInto(w, r, set, "page", "assistants", data)
}

// --- fragments ---------------------------------------------------------------

func (a *App) handleDashboardAssistantsTable(w http.ResponseWriter, r *http.Request) {
	p, _ := auth.PrincipalFromContext(r.Context())
	data, err := a.assistantsTableData(r.Context(), p)
	if err != nil {
		log.Printf("dashboard assistants table: %v", err)
		WriteOpenAIError(w, http.StatusInternalServerError, "internal server error", "server_error", "dashboard_assistants_failed")
		return
	}
	a.renderDashboardFragment(w, r, "assistants", "assistants-table", data)
}

func (a *App) handleDashboardAssistantDrawer(w http.ResponseWriter, r *http.Request) {
	p, _ := auth.PrincipalFromContext(r.Context())
	id := strings.TrimSpace(r.URL.Query().Get("id"))
	if id != "" && a.chatbotSvc == nil {
		http.NotFound(w, r)
		return
	}
	data, err := a.assistantDrawerData(r.Context(), p, id)
	if err != nil {
		if errors.Is(err, sql.ErrNoRows) {
			http.NotFound(w, r)
			return
		}
		log.Printf("dashboard assistant drawer: %v", err)
		WriteOpenAIError(w, http.StatusInternalServerError, "internal server error", "server_error", "dashboard_assistant_drawer_failed")
		return
	}
	a.renderDashboardFragment(w, r, "assistants", "assistant-drawer", data)
}

func (a *App) handleDashboardInstructionsList(w http.ResponseWriter, r *http.Request) {
	p, _ := auth.PrincipalFromContext(r.Context())
	data, err := a.instructionsListData(r.Context(), p)
	if err != nil {
		log.Printf("dashboard instructions list: %v", err)
		WriteOpenAIError(w, http.StatusInternalServerError, "internal server error", "server_error", "dashboard_instructions_failed")
		return
	}
	a.renderDashboardFragment(w, r, "assistants_instructions", "instructions-list", data)
}

func (a *App) handleDashboardInstructionEditor(w http.ResponseWriter, r *http.Request) {
	p, _ := auth.PrincipalFromContext(r.Context())
	id := strings.TrimSpace(r.URL.Query().Get("id"))
	data := instructionEditorData{}
	if id != "" {
		if a.promptSvc == nil {
			http.NotFound(w, r)
			return
		}
		t, err := a.promptSvc.Get(r.Context(), p, id)
		if err != nil {
			if errors.Is(err, sql.ErrNoRows) {
				http.NotFound(w, r)
				return
			}
			log.Printf("dashboard instruction editor: %v", err)
			WriteOpenAIError(w, http.StatusInternalServerError, "internal server error", "server_error", "dashboard_instruction_editor_failed")
			return
		}
		data.Template = &t
		data.Vars = instructionVariableRows(t.VariablesSchema)
	}
	a.renderDashboardFragment(w, r, "assistants_instructions", "instruction-editor", data)
}

// --- data assembly -----------------------------------------------------------

func (a *App) assistantsTableData(ctx context.Context, p auth.Principal) (assistantsPageData, error) {
	data := assistantsPageData{Empty: dashboardEmpty{Icon: "assistant", Text: "No assistants yet."}}
	if caps, _ := capsFromContext(ctx); caps.AssistantsManage {
		data.Empty.ActionLabel = "New assistant"
		data.Empty.ActionHref = "/dashboard/assistants?new=1"
	}
	if a.chatbotSvc == nil {
		return data, nil // no service wired: a truthful empty table, never fake rows
	}
	bots, err := a.chatbotSvc.List(ctx, p)
	if err != nil {
		return data, err
	}
	ids := make([]string, 0, len(bots))
	for _, b := range bots {
		ids = append(ids, b.ID)
	}
	counts := map[string]int64{}
	if len(ids) > 0 {
		if counts, err = a.chatbotSvc.ListConversationCountsByAssistant(ctx, p, ids); err != nil {
			return data, err
		}
	}
	names := map[string]string{}
	if a.ragSvc != nil {
		collections, err := a.ragSvc.ListCollections(ctx, p)
		if err != nil {
			log.Printf("dashboard assistants: collections: %v", err)
		} else {
			for _, c := range collections {
				names[c.ID] = c.Name
			}
		}
	}
	members := map[string]string{}
	if a.identity != nil {
		if users, err := a.identity.ListUsers(ctx, p.TenantID); err == nil {
			for _, u := range users {
				label := strings.TrimSpace(u.DisplayName)
				if label == "" {
					label = strings.TrimSpace(u.Email)
				}
				if label != "" {
					members[u.ID] = label
				}
			}
		}
	}
	states, endpoints := a.assistantEndpointAssurance(ctx, p)
	data.Table.Rows = make([]assistantRow, 0, len(bots))
	for _, b := range bots {
		knowledge := make([]string, 0, len(b.RAGCollectionIDs))
		for _, id := range b.RAGCollectionIDs {
			if name, ok := names[id]; ok {
				knowledge = append(knowledge, name)
			} else {
				knowledge = append(knowledge, "Unavailable collection")
			}
		}
		row := assistantRow{
			ID:            b.ID,
			Name:          b.Name,
			Description:   b.Description,
			Model:         b.Model,
			Knowledge:     knowledge,
			Tools:         assistantToolLabels(b),
			Audience:      audienceText(b.Audience, members),
			StatusState:   "disabled",
			Conversations: counts[b.ID],
		}
		row.Initials, row.AvatarColor = assistantAvatar(b.Name)
		if b.IsEnabled {
			row.StatusState = "active"
		}
		if b.ProviderID != "" {
			if e, ok := endpoints[b.ProviderID]; ok {
				row.Endpoint = endpointCardLabel(e)
				if confidentialEndpoint(e) {
					if states[b.ProviderID] == inference.AttestationVerified {
						row.ConfState = "verified"
					} else {
						row.ConfState = "unverified"
					}
				}
			} else {
				row.Endpoint = "Unavailable endpoint"
			}
		}
		data.Table.Rows = append(data.Table.Rows, row)
	}
	return data, nil
}

func (a *App) instructionsListData(ctx context.Context, p auth.Principal) (instructionsPageData, error) {
	data := instructionsPageData{Empty: dashboardEmpty{Icon: "file", Text: "No instruction templates yet."}}
	if caps, _ := capsFromContext(ctx); caps.AssistantsManage {
		data.Empty.ActionLabel = "New template"
		data.Empty.ActionHref = "/dashboard/assistants/instructions?new=1"
	}
	if a.promptSvc == nil {
		return data, nil
	}
	templates, err := a.promptSvc.List(ctx, p)
	if err != nil {
		return data, err
	}
	data.List.Templates = make([]instructionRow, 0, len(templates))
	for _, t := range templates {
		data.List.Templates = append(data.List.Templates, instructionRow{
			ID:          t.ID,
			Name:        t.Name,
			Description: t.Description,
			IsDefault:   t.IsDefault,
			Variables:   instructionVariableCount(t.VariablesSchema),
			UpdatedAt:   t.UpdatedAt,
		})
	}
	return data, nil
}

func (a *App) assistantDrawerData(ctx context.Context, p auth.Principal, id string) (assistantDrawerData, error) {
	data := assistantDrawerData{}
	var bot chatbot.Instance
	if id != "" {
		b, err := a.chatbotSvc.Get(ctx, p, id)
		if err != nil {
			return data, err
		}
		bot = b
		data.Assistant = &bot
	}
	// Models: priced + routable only — the shared selector already applies
	// the caller's model/endpoint grants and complete price coverage on the
	// route execution (unpinned routing, "" endpoint). Loaded for create and
	// edit alike.
	if a.executor != nil {
		models, err := a.executor.PricedModelSelector(ctx, p, "")
		if err != nil {
			log.Printf("dashboard assistants: priced models: %v", err)
		}
		for _, m := range models {
			data.Models = append(data.Models, assistantModelOption{
				ID:      m.ID,
				Current: data.Assistant != nil && data.Assistant.Model == m.ID,
			})
		}
	}
	if data.Assistant != nil && data.Assistant.Model != "" && !data.modelsCurrent(data.Assistant.Model) {
		// A stored model whose route lost pricing stays visible and
		// selected: saving the drawer must never silently switch the
		// assistant's model.
		data.CurrentModel = data.Assistant.Model
	}
	if a.promptSvc != nil {
		templates, err := a.promptSvc.List(ctx, p)
		if err != nil {
			log.Printf("dashboard assistants: templates: %v", err)
		} else {
			for _, t := range templates {
				data.Templates = append(data.Templates, assistantTemplateOption{
					ID: t.ID, Name: t.Name, SystemPrompt: t.SystemPrompt, UserPrompt: t.UserPrompt,
					Selected: data.Assistant != nil && data.Assistant.PromptTemplateID == t.ID,
				})
			}
		}
	}
	if a.ragSvc != nil {
		collections, err := a.ragSvc.ListCollections(ctx, p)
		if err != nil {
			log.Printf("dashboard assistants: collections: %v", err)
		} else {
			for _, c := range collections {
				data.Knowledge = append(data.Knowledge, assistantKnowledgeOption{
					ID: c.ID, Name: c.Name,
					Selected: data.Assistant != nil && chatbotCollectionSelected(*data.Assistant, c.ID),
				})
			}
		}
	}
	selected := map[string]bool{}
	if data.Assistant != nil {
		for _, t := range assistantToolIDs(*data.Assistant) {
			selected[t] = true
		}
	}
	data.Tools = a.assistantToolOptions(ctx, p, selected)
	data.AudienceAll = data.Assistant != nil && chatbotAudienceAll(*data.Assistant)
	memberSelected := map[string]bool{}
	if data.Assistant != nil {
		for _, entry := range data.Assistant.Audience {
			if entry != "*" {
				memberSelected[entry] = true
			}
		}
	}
	if a.identity != nil {
		users, err := a.identity.ListUsers(ctx, p.TenantID)
		if err != nil {
			log.Printf("dashboard assistants: members: %v", err)
		} else {
			for _, u := range users {
				label := u.DisplayName
				if strings.TrimSpace(label) == "" {
					label = u.Email
				}
				data.Members = append(data.Members, assistantMember{ID: u.ID, Label: label, Selected: memberSelected[u.ID]})
				delete(memberSelected, u.ID)
			}
		}
	}
	// Preserve selections for removed members without exposing their IDs as labels.
	for entry := range memberSelected {
		data.Members = append(data.Members, assistantMember{ID: entry, Label: "Unavailable member", Selected: true})
	}
	sort.Slice(data.Members, func(i, j int) bool { return data.Members[i].Label < data.Members[j].Label })
	// Non-tool settings ride hidden preserving inputs: the checklist owns
	// "tools" only, the rest survive a drawer save untouched.
	if data.Assistant != nil {
		for k, v := range data.Assistant.Settings {
			if k == "tools" {
				continue
			}
			raw, err := json.Marshal(v)
			if err != nil {
				continue
			}
			data.Preserved = append(data.Preserved, assistantSetting{Key: k, JSON: string(raw)})
		}
		sort.Slice(data.Preserved, func(i, j int) bool { return data.Preserved[i].Key < data.Preserved[j].Key })
	}
	return data, nil
}

// modelsCurrent reports whether the stored model is among the priced
// options (the extra "current" option is rendered when it is not).
func (d assistantDrawerData) modelsCurrent(model string) bool {
	for _, m := range d.Models {
		if m.ID == model {
			return true
		}
	}
	return false
}

// assistantToolOptions builds the drawer checklist: the four built-ins, the
// enabled servers' approved tools by model_name, then the phase-8 queue's
// pending/changed rows as disabled "Awaiting approval" entries.
func (a *App) assistantToolOptions(ctx context.Context, p auth.Principal, selected map[string]bool) []assistantToolOption {
	options := make([]assistantToolOption, 0, len(assistantBuiltinTools))
	for _, t := range assistantBuiltinTools {
		options = append(options, assistantToolOption{ID: t.ID, Label: t.Label, Approved: true, Selected: selected[t.ID]})
	}
	if a.mcpRepo == nil {
		return options
	}
	servers, err := a.mcpRepo.ListServers(ctx, p.TenantID)
	if err != nil {
		log.Printf("dashboard assistants: mcp servers: %v", err)
		return options
	}
	enabled := map[string]bool{}
	for _, s := range servers {
		if s.Enabled {
			enabled[s.ID] = true
		}
	}
	seen := map[string]bool{}
	for serverID := range enabled {
		approvals, err := a.mcpRepo.ListApprovals(ctx, p.TenantID, serverID)
		if err != nil {
			log.Printf("dashboard assistants: mcp approvals: %v", err)
			continue
		}
		for _, approval := range approvals {
			name := strings.TrimSpace(approval.ModelName)
			if name == "" || seen[name] || assistantBuiltinToolIDs[name] {
				continue
			}
			seen[name] = true
			options = append(options, assistantToolOption{ID: name, Label: name, Approved: true, Selected: selected[name]})
		}
	}
	pending, err := a.mcpRepo.ListToolsByState(ctx, p.TenantID, "pending", "changed")
	if err != nil {
		log.Printf("dashboard assistants: mcp queue: %v", err)
		return options
	}
	for _, row := range pending {
		if !enabled[row.ServerID] || seen[row.ToolID] {
			continue
		}
		seen[row.ToolID] = true
		label := strings.TrimSpace(row.RemoteName)
		if label == "" {
			label = row.ToolID
		}
		// No model_name exists before approval — the entry is informational
		// and cannot be checked into a configuration.
		options = append(options, assistantToolOption{Label: label, Approved: false})
	}
	return options
}

// assistantEndpointAssurance resolves the observed attestation state per
// endpoint through the phase-7 projection — read-only, configuration-bound,
// never probed and never an editable field.
func (a *App) assistantEndpointAssurance(ctx context.Context, p auth.Principal) (map[string]inference.AttestationState, map[string]inference.Endpoint) {
	states := map[string]inference.AttestationState{}
	endpoints := map[string]inference.Endpoint{}
	if a.endpoints == nil {
		return states, endpoints
	}
	list, err := a.endpoints.List(ctx, p.TenantID)
	if err != nil {
		log.Printf("dashboard assistants: endpoints: %v", err)
		return states, endpoints
	}
	confidential := make([]inference.Endpoint, 0, len(list))
	for _, e := range list {
		endpoints[e.ID] = e
		if confidentialEndpoint(e) {
			confidential = append(confidential, e)
		}
	}
	if a.executor == nil || len(confidential) == 0 {
		return states, endpoints
	}
	for _, o := range a.executor.AttestationSnapshot(p.TenantID, confidential) {
		states[o.EndpointID] = o.State
	}
	return states, endpoints
}

// --- helpers -----------------------------------------------------------------

// assistantBuiltinTools is the built-in tool set the runner offers
// (chatbot/builtins.go); labels are UI wording, IDs are the stored names.
var assistantBuiltinTools = []struct{ ID, Label string }{
	{"knowledge.search", "Knowledge search"},
	{"evidence.read", "Evidence read"},
	{"extract.submit", "Extract submit"},
	{"extract.status", "Extract status"},
}

var assistantBuiltinToolIDs = func() map[string]bool {
	m := make(map[string]bool, len(assistantBuiltinTools))
	for _, t := range assistantBuiltinTools {
		m[t.ID] = true
	}
	return m
}()

// assistantToolIDs reads the stored settings["tools"] shape chatbot.Service
// configures — string IDs, built-ins by name and MCP tools by model_name.
func assistantToolIDs(b chatbot.Instance) []string {
	values, ok := b.Settings["tools"].([]any)
	if !ok {
		return nil
	}
	ids := make([]string, 0, len(values))
	for _, v := range values {
		if s, ok := v.(string); ok && strings.TrimSpace(s) != "" {
			ids = append(ids, strings.TrimSpace(s))
		}
	}
	return ids
}

// assistantToolLabels maps the stored tool IDs to their display names for the
// table's Tools column: built-ins by label, MCP tools by model_name.
func assistantToolLabels(b chatbot.Instance) []string {
	ids := assistantToolIDs(b)
	labels := make([]string, 0, len(ids))
	byID := map[string]string{}
	for _, t := range assistantBuiltinTools {
		byID[t.ID] = t.Label
	}
	for _, id := range ids {
		if label, ok := byID[id]; ok {
			labels = append(labels, label)
		} else {
			labels = append(labels, id)
		}
	}
	return labels
}

// audienceText resolves stored audience IDs to member names when available.
func audienceText(entries []string, members map[string]string) string {
	if len(entries) == 0 {
		return "No members"
	}
	labels := make([]string, 0, len(entries))
	for _, entry := range entries {
		if entry == "*" {
			labels = append(labels, "Everyone")
		} else if name := members[entry]; name != "" {
			labels = append(labels, name)
		} else {
			labels = append(labels, "Unavailable member")
		}
	}
	return strings.Join(labels, ", ")
}

// assistantAvatar derives the colored-initials avatar: stable hue from the
// name, up to two initials.
func assistantAvatar(name string) (string, string) {
	initials := ""
	for _, field := range strings.Fields(strings.TrimSpace(name)) {
		runes := []rune(strings.ToUpper(field))
		if len(runes) > 0 {
			initials += string(runes[0])
		}
		if len(initials) == 2 {
			break
		}
	}
	if initials == "" {
		initials = "A"
	}
	hue := 0
	for _, r := range name {
		hue = (hue*31 + int(r)) % 360
	}
	return initials, fmt.Sprintf("hsl(%d, 30%%, 26%%)", hue)
}

// chatbotCollectionSelected reports whether the assistant's stored knowledge
// list contains the collection (nil = no knowledge attached — the service
// retrieves nothing when the list is empty, never "all knowledge").
func chatbotCollectionSelected(b chatbot.Instance, id string) bool {
	for _, c := range b.RAGCollectionIDs {
		if c == id {
			return true
		}
	}
	return false
}

// chatbotAudienceAll reports the stored "*" tenant-wide audience.
func chatbotAudienceAll(b chatbot.Instance) bool {
	for _, entry := range b.Audience {
		if entry == "*" {
			return true
		}
	}
	return false
}

// instructionVariableCount reads the properties size from the stored schema.
func instructionVariableCount(schema map[string]any) int {
	if props, ok := schema["properties"].(map[string]any); ok {
		return len(props)
	}
	return 0
}

// instructionVariableRows parses the stored variables_schema into labeled
// editor rows (name, description, required) — the inverse of the module's
// row serializer.
func instructionVariableRows(schema map[string]any) []instructionVarRow {
	rows := []instructionVarRow{}
	if schema == nil {
		return rows
	}
	props, _ := schema["properties"].(map[string]any)
	required := map[string]bool{}
	if list, ok := schema["required"].([]any); ok {
		for _, v := range list {
			if s, ok := v.(string); ok {
				required[s] = true
			}
		}
	}
	names := make([]string, 0, len(props))
	for name := range props {
		names = append(names, name)
	}
	sort.Strings(names)
	for _, name := range names {
		row := instructionVarRow{Name: name, Required: required[name]}
		if prop, ok := props[name].(map[string]any); ok {
			if d, ok := prop["description"].(string); ok {
				row.Description = d
			}
		}
		rows = append(rows, row)
	}
	return rows
}
