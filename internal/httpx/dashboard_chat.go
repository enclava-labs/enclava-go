// dashboard_chat.go — the Chat page (OA-32, plan 10-02): own-conversation
// history, a server-rendered thread with tool steps and citations, and the
// ONE shared evidence fragment `Enclava.evidence.open` fetches. Rendering
// only (D-17): every write goes through /api/v1 from pages/chat.js. Routes
// self-register; dashboard.go and the legacy chatbots surface stay untouched
// (their teardown rides 10-01's consolidated commit).
package httpx

import (
	"context"
	"database/sql"
	"errors"
	"log"
	"net/http"
	"strings"
	"time"

	"enclava-go/internal/auth"
	"enclava-go/internal/chatbot"
	"enclava-go/internal/inference"
	"enclava-go/internal/source"
)

func init() { registerDashboardPage((*App).registerDashboardChatRoutes) }

// registerDashboardChatRoutes mounts the chat page, its two fragments and the
// shared evidence fragment. The evidence endpoint is capSearch-gated because
// Search (10-03) opens the same card; the chat surface itself is capChat.
func (a *App) registerDashboardChatRoutes() {
	a.mux.HandleFunc("GET /dashboard/chat", a.sessionAuth.requireCap(capChat, a.handleDashboardChatPage))
	a.mux.HandleFunc("GET /dashboard/chat/{conversationID}", a.sessionAuth.requireCap(capChat, a.handleDashboardChatConversation))
	// Subtree fall-through: anything deeper than one segment renders the
	// designed 404 instead of landing on the legacy /dashboard/ catch-all.
	a.mux.HandleFunc("GET /dashboard/chat/", a.sessionAuth.requireCap(capChat, a.sessionAuth.renderDashboardNotFound))
	a.mux.HandleFunc("GET /dashboard/partials/chat/history", a.sessionAuth.requireCap(capChat, a.handleDashboardChatHistory))
	a.mux.HandleFunc("GET /dashboard/partials/chat/thread", a.sessionAuth.requireCap(capChat, a.handleDashboardChatThread))
	a.mux.HandleFunc("GET /dashboard/partials/evidence/{evidenceID}",
		a.sessionAuth.requireCap(capSearch, a.handleDashboardEvidencePartial))
	a.mux.HandleFunc("GET /dashboard/partials/evidence/", a.sessionAuth.renderDashboardNotFound)
}

// chatViewData is the page model; it implements dashboardPageMeta so the
// layout injects static/pages/chat.js.
type chatViewData struct {
	ConversationID    string
	Assistants        []chatbot.Instance
	SelectedAssistant string
	EvidenceIDsAttr   string
	History           chatHistoryData
	Thread            *chatThreadData
	// Shared empty states (caller-gated in fillEmptyStates).
	HistoryEmpty    dashboardEmpty
	AssistantsEmpty dashboardEmpty
	ThreadEmpty     dashboardEmpty
}

func (d chatViewData) DashboardTitle() string  { return "Chat" }
func (d chatViewData) DashboardModule() string { return "chat" }

// chatHistoryData is one page of the caller's own conversations, grouped
// Today/Earlier by updated_at.
type chatHistoryData struct {
	Groups     []chatHistoryGroup
	NextCursor string
}

type chatHistoryGroup struct {
	Label string
	Items []chatHistoryItem
}

type chatHistoryItem struct {
	ID        string
	Title     string
	UpdatedAt time.Time
	Selected  bool
}

// chatThreadData carries the thread plus the assistant header truth: model
// and endpoint come ONLY from a run's persisted endpoint_id/model fields —
// never from the configured assistant row.
type chatThreadData struct {
	ConversationID string
	AssistantName  string
	Subline        string
	ConfText       string // "" → no chip; the chip is never inferred
	ConfWarn       bool
	Messages       []chatThreadMessage
	Live           bool // any non-terminal run → the thread fragment polls
}

type chatThreadMessage struct {
	ID        string
	Role      string
	Author    string
	Content   string
	CreatedAt time.Time
	EvidAttr  string // pre-joined data-evidence-ids (no `join` FuncMap func)
	RunID     string
	RunStatus string // live status resolved server-side at render
	RunError  string
	Steps     []chatbot.AssistantRunStep
	Pending   bool
}

const chatHistoryPageSize = 20

// fillEmptyStates sets the shared dashboardEmpty values; the start-a-chat
// assistant picker distinguishes "none exist" (manager cap) from "none
// granted to you" (member, D-25) via the caller's caps.
func (d *chatViewData) fillEmptyStates(caps dashboardCaps) {
	d.HistoryEmpty = dashboardEmpty{Icon: "chat", Text: "No conversations yet.",
		ActionLabel: "New chat", ActionHref: "/dashboard/chat"}
	d.AssistantsEmpty = dashboardEmpty{Icon: "assistant",
		Text: "No assistants you can use yet — ask an admin to grant access."}
	if caps.AssistantsManage {
		d.AssistantsEmpty.Text = "No assistants yet."
		d.AssistantsEmpty.ActionLabel, d.AssistantsEmpty.ActionHref = "New assistant", "/dashboard/assistants?new=1"
	}
	d.ThreadEmpty = dashboardEmpty{Icon: "chat",
		Text: "No conversation open yet — pick an assistant and ask your first question."}
}

// listAssistants returns the audience- AND grant-enforced assistant list
// (chatbotSvc.List verbatim, D-25). An empty result is the truthful deny
// state — the picker stays empty and Send stays disabled.
func (a *App) listAssistants(ctx context.Context, p auth.Principal) []chatbot.Instance {
	if a.chatbotSvc == nil {
		return nil
	}
	bots, err := a.chatbotSvc.List(ctx, p)
	if err != nil {
		log.Printf("dashboard chat assistants: %v", err)
		return nil
	}
	return bots
}

// buildChatHistory groups one ListConversations page under Today/Earlier.
func (a *App) buildChatHistory(ctx context.Context, p auth.Principal, cursor, conversationID string) chatHistoryData {
	data := chatHistoryData{}
	if a.chatbotSvc == nil {
		return data
	}
	summaries, next, err := a.chatbotSvc.ListConversations(ctx, p, chatHistoryPageSize, cursor)
	if err != nil {
		if !errors.Is(err, chatbot.ErrInvalidInput) {
			log.Printf("dashboard chat history: %v", err)
		}
		return data
	}
	data.NextCursor = next
	now := time.Now()
	for _, s := range summaries {
		group := "Earlier"
		y1, m1, d1 := now.Date()
		y2, m2, d2 := s.UpdatedAt.Date()
		if y1 == y2 && m1 == m2 && d1 == d2 {
			group = "Today"
		}
		if len(data.Groups) == 0 || data.Groups[len(data.Groups)-1].Label != group {
			data.Groups = append(data.Groups, chatHistoryGroup{Label: group})
		}
		g := &data.Groups[len(data.Groups)-1]
		title := strings.TrimSpace(s.Title)
		if title == "" {
			title = s.AssistantName
		}
		g.Items = append(g.Items, chatHistoryItem{
			ID: s.ID, Title: title, UpdatedAt: s.UpdatedAt, Selected: s.ID == conversationID,
		})
	}
	return data
}

// handleDashboardChatPage renders /dashboard/chat — history plus the empty
// thread state; ?assistant= preselects, ?evidence= preloads composer ids.
func (a *App) handleDashboardChatPage(w http.ResponseWriter, r *http.Request) {
	p, _ := auth.PrincipalFromContext(r.Context())
	caps, _ := capsFromContext(r.Context())
	q := r.URL.Query()
	data := chatViewData{
		Assistants:        a.listAssistants(r.Context(), p),
		SelectedAssistant: strings.TrimSpace(q.Get("assistant")),
		EvidenceIDsAttr:   chatEvidenceAttr(q.Get("evidence")),
		History:           a.buildChatHistory(r.Context(), p, "", ""),
	}
	data.fillEmptyStates(caps)
	a.renderDashboard(w, r, "chat", data)
}

// handleDashboardChatConversation renders the deep-linked thread server-side
// on first paint. Another owner's (or unknown) id is the uniform not-found
// page — never a rendered fake thread.
func (a *App) handleDashboardChatConversation(w http.ResponseWriter, r *http.Request) {
	p, _ := auth.PrincipalFromContext(r.Context())
	caps, _ := capsFromContext(r.Context())
	id := strings.TrimSpace(r.PathValue("conversationID"))
	data := chatViewData{
		ConversationID: id,
		Assistants:     a.listAssistants(r.Context(), p),
		History:        a.buildChatHistory(r.Context(), p, "", id),
	}
	data.fillEmptyStates(caps)
	if !a.appendChatThread(w, r, &data) {
		return // the 404/500 response is already committed
	}
	a.renderDashboard(w, r, "chat", data)
}

// handleDashboardChatHistory re-renders the history fragment (?cursor= for
// keyset pages, ?c= to mark the open conversation).
func (a *App) handleDashboardChatHistory(w http.ResponseWriter, r *http.Request) {
	p, _ := auth.PrincipalFromContext(r.Context())
	caps, _ := capsFromContext(r.Context())
	q := r.URL.Query()
	conversationID := strings.TrimSpace(q.Get("c"))
	data := chatViewData{
		ConversationID: conversationID,
		History:        a.buildChatHistory(r.Context(), p, strings.TrimSpace(q.Get("cursor")), conversationID),
	}
	data.fillEmptyStates(caps)
	a.renderDashboardFragment(w, r, "chat", "chat-history", data)
}

// handleDashboardChatThread re-renders the thread body for ?c={id}; without
// a conversation id it renders the empty state the composer resets into.
func (a *App) handleDashboardChatThread(w http.ResponseWriter, r *http.Request) {
	caps, _ := capsFromContext(r.Context())
	data := chatViewData{ConversationID: strings.TrimSpace(r.URL.Query().Get("c"))}
	data.fillEmptyStates(caps)
	if data.ConversationID != "" && !a.appendChatThread(w, r, &data) {
		return
	}
	a.renderDashboardFragment(w, r, "chat", "chat-thread", data)
}

// appendChatThread loads + projects the conversation into the view data.
// not-found/forbidden → uniform 404 page (false, response committed);
// other service errors → logged 500.
func (a *App) appendChatThread(w http.ResponseWriter, r *http.Request, data *chatViewData) bool {
	if a.chatbotSvc == nil {
		a.sessionAuth.renderDashboardNotFound(w, r)
		return false
	}
	p, _ := auth.PrincipalFromContext(r.Context())
	view, err := a.chatbotSvc.ListConversation(r.Context(), p, data.ConversationID)
	if err != nil {
		if errors.Is(err, sql.ErrNoRows) || errors.Is(err, chatbot.ErrRunNotFound) {
			a.sessionAuth.renderDashboardNotFound(w, r)
			return false
		}
		log.Printf("dashboard chat thread %q: %v", data.ConversationID, err)
		http.Error(w, "internal server error", http.StatusInternalServerError)
		return false
	}
	data.Thread = a.buildChatThreadData(r.Context(), p, view)
	return true
}

// buildChatThreadData projects the ConversationView into display-safe
// markup data: live run statuses resolved at render (never guessed), steps
// straight from the service projection, and the header route truth from the
// most recent message run that carries persisted endpoint_id/model.
func (a *App) buildChatThreadData(ctx context.Context, p auth.Principal, view chatbot.ConversationView) *chatThreadData {
	t := &chatThreadData{ConversationID: view.Conversation.ID}
	assistantName := "Assistant"
	for _, bot := range a.listAssistants(ctx, p) {
		if bot.ID == view.Conversation.ChatbotID {
			assistantName = bot.Name
			break
		}
	}
	t.AssistantName = assistantName

	runs := map[string]chatbot.AssistantRun{}
	resolve := func(id string) (chatbot.AssistantRun, bool) {
		if run, ok := runs[id]; ok {
			return run, true
		}
		if a.chatbotSvc == nil {
			return chatbot.AssistantRun{}, false
		}
		run, err := a.chatbotSvc.GetRun(ctx, p, id)
		if err != nil {
			return chatbot.AssistantRun{}, false
		}
		runs[id] = run
		return run, true
	}
	var headerModel, headerEndpointID string
	for _, msg := range view.Messages {
		tm := chatThreadMessage{
			ID:        msg.ID,
			Role:      msg.Role,
			Author:    "You",
			Content:   msg.Content,
			CreatedAt: msg.CreatedAt,
			EvidAttr:  strings.Join(msg.EvidenceIDs, ","),
			Steps:     msg.Steps,
		}
		if msg.Role == "assistant" {
			tm.Author = assistantName
		}
		if msg.RunID != "" {
			tm.RunID = msg.RunID
			if run, ok := resolve(msg.RunID); ok {
				tm.RunStatus = run.Status
				tm.RunError = run.Error
				// Per the shared non-terminal table — an unknown run is
				// terminal: it renders the outcome line and polling stops.
				tm.Pending = dashboardNonTerminal(run.Status)
				if tm.Pending {
					t.Live = true
				}
				if run.Model != "" || run.EndpointID != "" {
					headerModel, headerEndpointID = run.Model, run.EndpointID
				}
			}
		}
		t.Messages = append(t.Messages, tm)
	}
	if headerModel != "" || headerEndpointID != "" {
		t.Subline = strings.TrimSpace(strings.Join([]string{headerModel, a.endpointDisplayLabel(ctx, p, headerEndpointID)}, " · "))
		if chip, warn := a.confidentialChip(ctx, p, headerEndpointID); chip != "" {
			t.ConfText, t.ConfWarn = chip, warn
		}
	}
	return t
}

// endpointDisplayLabel humanizes the run's persisted endpoint — "Tinfoil ·
// host" like the confidential card; unknown ids render "" (never a bare id).
func (a *App) endpointDisplayLabel(ctx context.Context, p auth.Principal, endpointID string) string {
	if endpointID == "" || a.endpoints == nil {
		return ""
	}
	e, err := a.endpoints.Get(ctx, p.TenantID, endpointID)
	if err != nil {
		return ""
	}
	return endpointCardLabel(e)
}

// confidentialChip is the thread-head chip: rendered only when the run's
// persisted endpoint is a confidential one; verified observations get the
// good chip, anything else warns. Never inferred from the assistant row.
func (a *App) confidentialChip(ctx context.Context, p auth.Principal, endpointID string) (string, bool) {
	if endpointID == "" || a.endpoints == nil {
		return "", false
	}
	e, err := a.endpoints.Get(ctx, p.TenantID, endpointID)
	if err != nil || !confidentialEndpoint(e) {
		return "", false
	}
	verified := false
	if a.executor != nil {
		for _, o := range a.executor.AttestationSnapshot(p.TenantID, []inference.Endpoint{e}) {
			if o.EndpointID == e.ID && o.State == inference.AttestationVerified {
				verified = true
			}
		}
	}
	label := "Confidential · " + dashboardHumanize(e.Protocol)
	if verified {
		return label + " verified", false
	}
	return label, true
}

// chatEvidenceAttr normalizes the ?evidence= csv into the composer's
// pre-joined data attribute (order preserved, empties dropped).
func chatEvidenceAttr(raw string) string {
	parts := strings.Split(raw, ",")
	ids := make([]string, 0, len(parts))
	for _, part := range parts {
		if id := strings.TrimSpace(part); id != "" {
			ids = append(ids, id)
		}
	}
	return strings.Join(ids, ",")
}

// evidenceCardData is the shared evidence fragment's model. One template
// serves the column, the drawer and the bottom sheet.
type evidenceCardData struct {
	EvidenceID  string
	Status      string // raw resolve status: "current" | "superseded"
	Collection  string
	Document    string
	Location    string
	Quote       string
	Unavailable bool // metadata alive, passage no longer readable
	OpenHref    string
	CopyURL     string
}

// handleDashboardEvidencePartial serves GET /dashboard/partials/evidence/{id}
// — the ONE fragment Enclava.evidence.open fetches. Two distinct not-found
// branches: metadata 404 (unknown=forbidden=revoked=tombstoned, uniform per
// 08-03) renders the generic "no longer available" card; metadata alive but
// passage 404s keeps the real metadata + links with a truthful
// passage-unavailable note — never fabricated quote text. Infrastructure
// failures are 5xx, never flattened into a card.
func (a *App) handleDashboardEvidencePartial(w http.ResponseWriter, r *http.Request) {
	p, _ := auth.PrincipalFromContext(r.Context())
	if a.ragSvc == nil {
		log.Printf("dashboard evidence partial: rag service is not configured")
		http.Error(w, "internal server error", http.StatusInternalServerError)
		return
	}
	id := strings.Trim(r.PathValue("evidenceID"), "/")
	card, err := a.buildEvidenceCard(r.Context(), p, id)
	if err != nil {
		if errors.Is(err, source.ErrDenied) {
			// Uniform metadata miss: the generic card, no metadata.
			a.renderDashboardFragment(w, r, "chat", "evidence", evidenceCardData{})
			return
		}
		log.Printf("dashboard evidence %q: %v", id, err)
		http.Error(w, "internal server error", http.StatusInternalServerError)
		return
	}
	a.renderDashboardFragment(w, r, "chat", "evidence", card)
}

// buildEvidenceCard composes metadata (ResolveEvidence) + passage
// (ReadEvidence) server-side — the client never renders evidence HTML.
func (a *App) buildEvidenceCard(ctx context.Context, p auth.Principal, id string) (evidenceCardData, error) {
	resolved, err := a.ragSvc.ResolveEvidence(ctx, p, []string{id})
	if err != nil {
		return evidenceCardData{}, err
	}
	res := resolved[0]
	status := "current"
	if res.Status == "superseded" {
		status = "changed" // the pill vocabulary reads Changed, never "superseded"
	}
	card := evidenceCardData{
		EvidenceID: res.ID,
		Status:     status,
		OpenHref:   res.NativeLink,
		CopyURL:    res.RetainedCopyURL,
		Location:   evidenceLocationLine(res.Locations),
		Document:   a.evidenceDocumentLabel(ctx, p.TenantID, res.Spans),
	}
	text, err := a.ragSvc.ReadEvidence(ctx, p, id)
	switch {
	case err == nil:
		card.Quote = text.Content
		if name, err := a.ragSvc.GetCollection(ctx, p, text.CollectionID); err == nil {
			card.Collection = name.Name
		}
	case errors.Is(err, source.ErrDenied) || errors.Is(err, sql.ErrNoRows):
		// Superseded/revoked since the answer: metadata stays, quote doesn't.
		card.Unavailable = true
	default:
		return evidenceCardData{}, err
	}
	if card.Document == "" {
		card.Document = "Document"
	}
	return card, nil
}

// evidenceDocumentLabel reads the retained file name for the cited object
// version (display-only; the authorization already happened in the rag
// service). Reachable in both branches — the resolution always carries the
// evidence's span refs.
func (a *App) evidenceDocumentLabel(ctx context.Context, tenantID string, spans []source.Span) string {
	if a.sourceRepo == nil {
		return ""
	}
	for _, span := range spans {
		if span.Ref.ObjectID == "" {
			continue
		}
		version, err := a.sourceRepo.GetObjectVersion(ctx, tenantID, span.Ref.ObjectID, span.Ref.ObservedSeq)
		if err != nil {
			return ""
		}
		return strings.TrimSpace(version.OriginalFilename)
	}
	return ""
}

// evidenceLocationLine renders persisted locators through the shared
// citation join — document · page · section, only real parts, never
// invented fields.
func evidenceLocationLine(locators []source.Locator) string {
	parts := make([]string, 0, len(locators))
	for _, l := range locators {
		page := 0
		if l.Page != nil {
			page = *l.Page
		}
		var extras []string
		if l.Section != "" {
			extras = append(extras, "section "+l.Section)
		}
		if l.Clause != "" {
			extras = append(extras, "clause "+l.Clause)
		}
		if l.Table != "" {
			extras = append(extras, "table "+l.Table)
		}
		if cit := dashboardCitation("", page, strings.Join(extras, " · ")); cit != "" {
			parts = append(parts, cit)
		}
	}
	return strings.Join(parts, " · ")
}
