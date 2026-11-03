// dashboard_status.go — D-21/D-7 single owner of the status vocabulary and
// the plain-language glossary. Templates call statusPill/statusLabel/
// statusTone and term; they never hardcode labels or tones.
package httpx

import (
	"encoding/json"
	"html/template"
	"net/url"
	"regexp"
	"strconv"
	"strings"
)

type dashboardStatusRow struct {
	Label       string
	Tone        string
	NonTerminal bool // live state — fragments keep polling while it holds
}

// dashboardStatusLabels keys the exact API state value to its UI label and
// pill tone — every row of the UI-SPEC status vocabulary (D-7). NonTerminal
// marks the only states a fragment may still update on; "unknown" is
// deliberately terminal (a lost worker is never requeued — polling it would
// poll forever) and unlisted states fail closed the same way.
var dashboardStatusLabels = map[string]dashboardStatusRow{
	"healthy":      {Label: "Ready", Tone: "good"},
	"ready":        {Label: "Ready", Tone: "good"},
	"active":       {Label: "Ready", Tone: "good"},
	"succeeded":    {Label: "Ready", Tone: "good"},
	"verified":     {Label: "Verified", Tone: "good"},
	"approved":     {Label: "Approved", Tone: "good"},
	"connected":    {Label: "Connected", Tone: "good"},
	"current":      {Label: "Current", Tone: "good"},
	"syncing":      {Label: "Syncing", Tone: "warn", NonTerminal: true},
	"running":      {Label: "Processing", Tone: "warn", NonTerminal: true},
	"processing":   {Label: "Processing", Tone: "warn", NonTerminal: true},
	"warning":      {Label: "Needs attention", Tone: "warn"},
	"review":       {Label: "Needs attention", Tone: "warn"},
	"needs_review": {Label: "Needs review", Tone: "warn"},
	"pending":      {Label: "Awaiting approval", Tone: "warn", NonTerminal: true},
	"degraded":     {Label: "Degraded", Tone: "warn"},
	"unverified":   {Label: "Unverified", Tone: "warn"},
	"changed":      {Label: "Changed", Tone: "warn"},
	"invited":      {Label: "Invited", Tone: "warn"},
	"queued":       {Label: "Queued", Tone: "neutral", NonTerminal: true},
	"cancelled":    {Label: "Stopped", Tone: "neutral"},
	"stopped":      {Label: "Stopped", Tone: "neutral"},
	"paused":       {Label: "Paused", Tone: "neutral"},
	"none":         {Label: "Not confidential", Tone: "neutral"},
	"disabled":     {Label: "Disabled", Tone: "neutral"},
	"failed":       {Label: "Failed", Tone: "bad"},
	"blocked":      {Label: "Blocked", Tone: "bad"},
	"not-approved": {Label: "Not approved", Tone: "bad"},
	"unreachable":  {Label: "Unreachable", Tone: "bad"},
	"revoked":      {Label: "Removed from search", Tone: "bad"},
	"deleted":      {Label: "Removed", Tone: "bad"},
	"unknown":      {Label: "Lost contact", Tone: "bad"},
}

// dashboardNonTerminal reports whether a fragment rendering this state still
// holds a live item worth polling. It reads the same vocabulary table as the
// pills so the two can never diverge; anything unlisted is terminal.
func dashboardNonTerminal(status string) bool {
	row, ok := dashboardStatusLabels[strings.ToLower(strings.TrimSpace(status))]
	return ok && row.NonTerminal
}

// statusTone returns the pill tone class for a state: good|warn|bad|neutral
// (accent|blue are reserved tones the table may grow). Unknown states are
// neutral — never an error.
func statusTone(state string) string {
	if row, ok := dashboardStatusLabels[state]; ok {
		return row.Tone
	}
	return "neutral"
}

// statusLabel is the text variant of statusPill. `kind` (e.g. "document") is
// reserved for future per-kind overrides — today every kind shares one table.
// Unknown states humanize ("weird_state" → "Weird state"), never raw jargon.
func statusLabel(kind, state string) string {
	if row, ok := dashboardStatusLabels[state]; ok {
		return row.Label
	}
	return dashboardHumanize(state)
}

// statusPill renders <span class="pill {tone}"><i class="dot"></i>Label</span>
// with the label escaped.
func statusPill(kind, state string) template.HTML {
	return template.HTML(`<span class="pill ` + statusTone(state) + `"><i class="dot"></i>` +
		template.HTMLEscapeString(statusLabel(kind, state)) + `</span>`)
}

// dashboardHumanize turns an API state/key into readable words: separators
// become spaces, first letter capitalised, the rest left alone.
func dashboardHumanize(s string) string {
	s = strings.NewReplacer("_", " ", "-", " ", ".", " ").Replace(strings.TrimSpace(s))
	if s == "" {
		return s
	}
	return strings.ToUpper(s[:1]) + s[1:]
}

// dashboardGlossary maps technical keys to the plain words the UI shows (D-7).
// Lookup is case-insensitive. "micros" is deliberately absent — it is the
// internal pricing unit and never user-facing (money() renders it).
var dashboardGlossary = map[string]string{
	"rag":           "knowledge",
	"qdrant":        "search index",
	"mcp":           "tool",
	"tinfoil":       "confidential endpoint",
	"api":           "service",
	"micros":        "per-million-token price",
	"json":          "data",
	"uuid":          "identifier",
	"assistant.use": "use assistants",
	"mcp.use":       "use tools",
	"readonly":      "Read-only",
	"writes-data":   "Writes data",
}

// term returns the plain word for a glossary key ("rag" → "knowledge");
// unknown keys humanize instead of leaking jargon.
func term(key string) string {
	if word, ok := dashboardGlossary[strings.ToLower(strings.TrimSpace(key))]; ok {
		return word
	}
	return dashboardHumanize(key)
}

// dashboardCitation joins the evidence locators that exist — "Document ·
// Page 3 · Clause 4.2" — and never invents a missing one (CONTRACTS.md).
// Returns "" only when the document title itself is absent.
func dashboardCitation(document string, page int, section string) string {
	parts := []string{}
	if s := strings.TrimSpace(document); s != "" {
		parts = append(parts, s)
	}
	if page > 0 {
		parts = append(parts, "Page "+strconv.Itoa(page))
	}
	if s := strings.TrimSpace(section); s != "" {
		parts = append(parts, s)
	}
	return strings.Join(parts, " · ")
}

// dashboardErrorActionResult is one row of the server-side error→next-step
// map: the explanatory sentence, and when a fix destination exists and the
// caller's capability holds, a short label + relative href for it.
type dashboardErrorActionResult struct {
	Text  string
	Label string
	Href  string
}

// dashboardSourceIDPattern pulls the "source:<id>" token callers append to a
// persisted auth-failure message so the Reconnect link can deep-link the row.
var dashboardSourceIDPattern = regexp.MustCompile(`(?i)\bsource:([a-zA-Z0-9_-]+)`)

const dashboardAdminFallback = " — your administrator can fix this."

// dashboardErrorAction maps normalized error text (envelope messages and the
// persisted run.Error / JobView.error_message / LastError payloads alike) to
// the next step (OA-35, CONTEXT decision 9). Gated destinations only get an
// href when the matching capability holds; otherwise the row degrades to the
// sentence plus the administrator fallback — never a link that 403s.
func dashboardErrorAction(message string, caps dashboardCaps) dashboardErrorActionResult {
	msg := strings.ToLower(strings.TrimSpace(message))
	switch {
	case strings.Contains(msg, "required price missing"):
		text := "The model isn't priced for every unit this request bills"
		if caps.ModelsManage {
			return dashboardErrorActionResult{Text: text + ".", Label: "Set price", Href: "/dashboard/models/pricing"}
		}
		return dashboardErrorActionResult{Text: text + dashboardAdminFallback}
	case strings.Contains(msg, "credential"), strings.Contains(msg, "authorization"), strings.Contains(msg, "auth failed"):
		text := "The source's credentials were rejected — reconnect it to resume syncing"
		if caps.SourcesManage {
			href := "/dashboard/sources"
			if m := dashboardSourceIDPattern.FindStringSubmatch(message); m != nil {
				href = "/dashboard/sources?edit=" + url.QueryEscape(m[1])
			}
			return dashboardErrorActionResult{Text: text + ".", Label: "Reconnect", Href: href}
		}
		return dashboardErrorActionResult{Text: text + dashboardAdminFallback}
	case strings.Contains(msg, "budget exceeded"):
		text := "This request exceeds the configured budget"
		if caps.BillingView {
			return dashboardErrorActionResult{Text: text + ".", Label: "View budgets", Href: "/dashboard/usage/budgets"}
		}
		return dashboardErrorActionResult{Text: text + dashboardAdminFallback}
	case strings.Contains(msg, "no eligible endpoint"):
		text := "No model endpoint can serve this request right now"
		if caps.ModelsManage {
			return dashboardErrorActionResult{Text: text + ".", Label: "Review models", Href: "/dashboard/models"}
		}
		return dashboardErrorActionResult{Text: text + dashboardAdminFallback}
	case strings.Contains(msg, "rate limit"), strings.Contains(msg, "overloaded"), strings.Contains(msg, "timed out"), strings.Contains(msg, "timeout"):
		return dashboardErrorActionResult{Text: "That request couldn't complete — try again."}
	case strings.Contains(msg, "schema"):
		return dashboardErrorActionResult{Text: "The tool's schema changed — refresh to see the latest before approving."}
	case strings.Contains(msg, "forbidden"), strings.Contains(msg, "not permitted"), strings.Contains(msg, "permission"):
		return dashboardErrorActionResult{Text: "You don't have access to do that — check with your administrator if you should."}
	case strings.Contains(msg, "not found"):
		return dashboardErrorActionResult{Text: "That item is no longer available."}
	}
	return dashboardErrorActionResult{Text: message}
}

// dashboardCapsJSON serializes the session capability booleans for the
// body's data-caps hook; the client map filters fix links through it. It
// emits capability names only — never scope strings. It returns a plain
// string (not template.JS): inside a data-* attribute html/template
// HTML-escapes the quotes, and dataset.caps decodes them back to JSON.
func dashboardCapsJSON(caps dashboardCaps) string {
	out := map[string]bool{}
	for _, c := range []dashboardCap{capChat, capSearch, capKnowledgeManage, capSourcesManage,
		capExtract, capExtractManage, capAssistantsManage, capToolsManage,
		capModelsManage, capAccessManage, capBillingView} {
		out[string(c)] = caps.grants(c)
	}
	b, err := json.Marshal(out)
	if err != nil {
		return "{}"
	}
	return string(b)
}

// 11-01 registers the extended vocabulary helpers on the shared FuncMap from
// its own file — dashboard_templates.go stays untouched.
func init() {
	dashboardBaseFuncs["dashboardNonTerminal"] = dashboardNonTerminal
	dashboardBaseFuncs["dashboardCitation"] = dashboardCitation
	dashboardBaseFuncs["dashboardErrorAction"] = dashboardErrorAction
	dashboardBaseFuncs["dashboardCapsJSON"] = dashboardCapsJSON
}
