// dashboard_status.go — D-21/D-7 single owner of the status vocabulary and
// the plain-language glossary. Templates call statusPill/statusLabel/
// statusTone and term; they never hardcode labels or tones.
package httpx

import (
	"html/template"
	"strings"
)

type dashboardStatusRow struct {
	Label string
	Tone  string
}

// dashboardStatusLabels keys the exact API state value to its UI label and
// pill tone — every row of the UI-SPEC status vocabulary (D-7).
var dashboardStatusLabels = map[string]dashboardStatusRow{
	"healthy":      {"Ready", "good"},
	"ready":        {"Ready", "good"},
	"active":       {"Ready", "good"},
	"succeeded":    {"Ready", "good"},
	"verified":     {"Verified", "good"},
	"approved":     {"Approved", "good"},
	"connected":    {"Connected", "good"},
	"current":      {"Current", "good"},
	"syncing":      {"Syncing", "warn"},
	"running":      {"Processing", "warn"},
	"processing":   {"Processing", "warn"},
	"warning":      {"Needs attention", "warn"},
	"review":       {"Needs attention", "warn"},
	"pending":      {"Awaiting approval", "warn"},
	"degraded":     {"Degraded", "warn"},
	"unverified":   {"Unverified", "warn"},
	"changed":      {"Changed", "warn"},
	"invited":      {"Invited", "warn"},
	"queued":       {"Queued", "neutral"},
	"cancelled":    {"Stopped", "neutral"},
	"stopped":      {"Stopped", "neutral"},
	"paused":       {"Paused", "neutral"},
	"none":         {"Not confidential", "neutral"},
	"disabled":     {"Disabled", "neutral"},
	"failed":       {"Failed", "bad"},
	"blocked":      {"Blocked", "bad"},
	"not-approved": {"Not approved", "bad"},
	"unreachable":  {"Unreachable", "bad"},
	"revoked":      {"Removed from search", "bad"},
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
	"rag":         "knowledge",
	"qdrant":      "index",
	"mcp":         "tool",
	"tinfoil":     "confidential endpoint",
	"api":         "service",
	"readonly":    "Read-only",
	"writes-data": "Writes data",
}

// term returns the plain word for a glossary key ("rag" → "knowledge");
// unknown keys humanize instead of leaking jargon.
func term(key string) string {
	if word, ok := dashboardGlossary[strings.ToLower(strings.TrimSpace(key))]; ok {
		return word
	}
	return dashboardHumanize(key)
}
