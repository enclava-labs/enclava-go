// dashboard_status_test.go — every UI-SPEC status vocabulary row maps to its
// label+tone; unknowns humanize safely; the glossary kills jargon (D-7/D-21).
package httpx

import (
	"strings"
	"testing"
)

func TestStatusVocabulary(t *testing.T) {
	// Verbatim UI-SPEC table: state → (label, tone).
	rows := map[string][2]string{
		"healthy":      {"Ready", "good"},
		"ready":        {"Ready", "good"},
		"active":       {"Ready", "good"},
		"succeeded":    {"Ready", "good"},
		"verified":     {"Verified", "good"},
		"approved":     {"Approved", "good"},
		"reachable":    {"Reachable", "good"},
		"not_granted":  {"Not granted", "warn"},
		"no_audience":  {"No audience", "warn"},
		"connected":    {"Connected", "good"},
		"current":      {"Current", "good"},
		"syncing":      {"Syncing", "warn"},
		"running":      {"Processing", "warn"},
		"processing":   {"Processing", "warn"},
		"warning":      {"Needs attention", "warn"},
		"review":       {"Needs attention", "warn"},
		"needs_review": {"Needs review", "warn"},
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
		"deleted":      {"Removed", "bad"},
		"unknown":      {"Lost contact", "bad"},
	}
	if len(dashboardStatusLabels) != len(rows) {
		t.Fatalf("vocabulary has %d rows, UI-SPEC table has %d", len(dashboardStatusLabels), len(rows))
	}
	for state, want := range rows {
		if got := statusLabel("document", state); got != want[0] {
			t.Errorf("statusLabel(%q) = %q, want %q", state, got, want[0])
		}
		if got := statusTone(state); got != want[1] {
			t.Errorf("statusTone(%q) = %q, want %q", state, got, want[1])
		}
	}
}

func TestStatusUnknownStateHumanized(t *testing.T) {
	if got := statusLabel("document", "weird_state"); got != "Weird state" {
		t.Fatalf("unknown state must humanize to %q, got %q", "Weird state", got)
	}
	if got := statusTone("weird_state"); got != "neutral" {
		t.Fatalf("unknown state tone must be neutral, got %q", got)
	}
	// Never the raw snake_case key, never a panic.
	if got := statusLabel("", " totally_broken "); strings.Contains(got, "_") || strings.Contains(got, " totally") {
		t.Fatalf("humanized label must not leak raw key, got %q", got)
	}
}

func TestStatusPillShape(t *testing.T) {
	got := string(statusPill("document", "processing"))
	if got != `<span class="pill warn"><i class="dot"></i>Processing</span>` {
		t.Fatalf("pill shape wrong, got %s", got)
	}
	// Adversarial state value must be escaped, never raw markup (T-07-02-01).
	bad := string(statusPill("document", "<script>alert(1)</script>"))
	// Escaping neutralizes the markup; the inner text itself may remain.
	if strings.Contains(bad, "<script>") || strings.Contains(bad, "</script>") {
		t.Fatalf("pill must escape the label markup, got %s", bad)
	}
	if !strings.Contains(bad, "&lt;script&gt;") {
		t.Fatalf("escaped label expected, got %s", bad)
	}
}

func TestTerm(t *testing.T) {
	cases := map[string]string{
		"rag":         "knowledge",
		"qdrant":      "search index",
		"mcp":         "tool",
		"tinfoil":     "confidential endpoint",
		"api":         "service",
		"readonly":    "Read-only",
		"writes-data": "Writes data",
		"micros":      "per-million-token price",
		"json":        "data",
		"uuid":        "identifier",
		"RAG":         "knowledge", // lookup is case-insensitive
	}
	for key, want := range cases {
		if got := term(key); got != want {
			t.Errorf("term(%q) = %q, want %q", key, got, want)
		}
	}
	if got := term("socket_pool"); got != "Socket pool" {
		t.Errorf("unknown key must humanize, got %q", got)
	}
}

func TestGlossary(t *testing.T) {
	// The banned-jargon keys from the plan must all be covered by the table;
	// the shared engine (11-01) extended it with the additional plain-word
	// rows the copy passes rely on.
	for _, key := range []string{"rag", "qdrant", "mcp", "tinfoil", "api", "readonly", "writes-data", "micros", "json", "uuid", "assistant.use", "mcp.use"} {
		if _, ok := dashboardGlossary[key]; !ok {
			t.Errorf("glossary must cover banned jargon key %q", key)
		}
	}
}
