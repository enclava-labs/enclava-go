// dashboard_feedback_test.go — the 11-01 language/feedback engine: the
// price_missing write paths (authority: status + code + unit in message),
// the server error→action map incl. capability gating, the terminal-state
// poll helper, and the extended empty component's action contract. Tests
// assert behavior, never wording or serialization details.
package httpx

import (
	"bytes"
	"encoding/json"
	"errors"
	"fmt"
	"net/http"
	"net/http/httptest"
	"strings"
	"testing"

	"enclava-go/internal/billing"
)

// TestPriceMissingEnvelopes drives every synchronous write path that can see
// billing.ErrPriceMissing: each must answer 422 + code price_missing with the
// unit token surviving in the message (the client map keys on it).
func TestPriceMissingEnvelopes(t *testing.T) {
	err := fmt.Errorf("%w: %s", billing.ErrPriceMissing, "cached_input")

	t.Run("inference writer", func(t *testing.T) {
		rec := httptest.NewRecorder()
		(&App{}).writeInferenceError(rec, err)
		assertPriceMissingEnvelope(t, rec, "cached_input")
	})

	t.Run("chatbot completion writer", func(t *testing.T) {
		rec := httptest.NewRecorder()
		(&App{}).writeChatbotCompletionError(rec, nil, err)
		assertPriceMissingEnvelope(t, rec, "cached_input")
	})

	t.Run("other units keep their token", func(t *testing.T) {
		for _, unit := range []string{"input", "output", "vision_input"} {
			rec := httptest.NewRecorder()
			(&App{}).writeInferenceError(rec, fmt.Errorf("%w: %s", billing.ErrPriceMissing, unit))
			assertPriceMissingEnvelope(t, rec, unit)
		}
	})

	t.Run("unrelated errors keep their existing mapping", func(t *testing.T) {
		rec := httptest.NewRecorder()
		(&App{}).writeInferenceError(rec, errors.New("driver exploded"))
		if rec.Code != http.StatusBadGateway {
			t.Fatalf("default inference error status = %d, want 502", rec.Code)
		}
		var env OpenAIErrorEnvelope
		if err := json.Unmarshal(rec.Body.Bytes(), &env); err != nil {
			t.Fatal(err)
		}
		if env.Error.Code == "price_missing" {
			t.Fatal("unrelated error must not claim the price_missing code")
		}
	})
}

func assertPriceMissingEnvelope(t *testing.T, rec *httptest.ResponseRecorder, unit string) {
	t.Helper()
	if rec.Code != http.StatusUnprocessableEntity {
		t.Fatalf("status = %d, want 422 (body %s)", rec.Code, rec.Body.String())
	}
	var env OpenAIErrorEnvelope
	if err := json.Unmarshal(rec.Body.Bytes(), &env); err != nil {
		t.Fatalf("envelope decode: %v", err)
	}
	if env.Error.Code != "price_missing" {
		t.Fatalf("code = %q, want price_missing", env.Error.Code)
	}
	if !strings.Contains(env.Error.Message, unit) {
		t.Fatalf("message %q must retain the unit token %q", env.Error.Message, unit)
	}
	if strings.Contains(env.Error.Message, "billing:") {
		t.Fatalf("internal prefix leaked into message %q", env.Error.Message)
	}
}

// TestDashboardNonTerminal pins the poll contract: only live states keep a
// fragment polling; unknown (lost worker, never requeued) and every
// unlisted state are terminal so polling self-stops.
func TestDashboardNonTerminal(t *testing.T) {
	for _, live := range []string{"queued", "running", "syncing", "processing", "pending"} {
		if !dashboardNonTerminal(live) {
			t.Errorf("%q must be non-terminal (fragment keeps polling)", live)
		}
	}
	for _, dead := range []string{
		"unknown", "succeeded", "failed", "cancelled", "stopped", "approved",
		"revoked", "ready", "healthy", "verified", "connected", "degraded",
		"blocked", "unreachable", "deleted", "weird-state", "",
	} {
		if dashboardNonTerminal(dead) {
			t.Errorf("%q must be terminal (polling stops)", dead)
		}
	}
}

// TestDashboardCitation verifies locators join only when present — absent
// parts are omitted, never invented.
func TestDashboardCitation(t *testing.T) {
	if got := dashboardCitation("Lease.pdf", 3, "Clause 4.2"); got != "Lease.pdf · Page 3 · Clause 4.2" {
		t.Fatalf("full citation = %q", got)
	}
	for name, got := range map[string]string{
		"no locators":   dashboardCitation("Lease.pdf", 0, ""),
		"page only":     dashboardCitation("Lease.pdf", 2, ""),
		"section only":  dashboardCitation("Lease.pdf", 0, "Appendix A"),
		"empty doc":     dashboardCitation("", 3, "Clause 4.2"),
		"negative page": dashboardCitation("Doc", -1, ""),
	} {
		if strings.Contains(got, "Page -") || strings.Contains(got, "Page 0") {
			t.Errorf("%s invented a page locator: %q", name, got)
		}
		if got != "" && strings.Contains(got, "··") {
			t.Errorf("%s left a dangling separator: %q", name, got)
		}
	}
	if dashboardCitation("Lease.pdf", 0, "") != "Lease.pdf" {
		t.Fatal("document alone must render without trailing separators")
	}
	if dashboardCitation("", 0, "") != "" {
		t.Fatal("empty evidence must produce an empty citation")
	}
}

// TestDashboardErrorAction pins the message→next-step rows and, critically,
// the capability gate: a caller without the managing capability gets the
// sentence + administrator fallback and NO href.
func TestDashboardErrorAction(t *testing.T) {
	admin := dashboardCaps{ModelsManage: true, SourcesManage: true, BillingView: true}
	member := dashboardCaps{}

	t.Run("price missing maps for envelope and persisted payloads alike", func(t *testing.T) {
		for _, msg := range []string{
			"required price missing: cached_input",                 // rejected-envelope message
			"run failed: required price missing: output",           // persisted chat run.Error
			"job admission refused: required price missing: input", // persisted extract JobView.error_message
		} {
			got := dashboardErrorAction(msg, admin)
			if got.Href != "/dashboard/models/pricing" || got.Label == "" {
				t.Fatalf("%q: want Set price link, got %+v", msg, got)
			}
		}
	})

	t.Run("capability-off rows degrade to administrator fallback, no href", func(t *testing.T) {
		for _, msg := range []string{
			"required price missing: cached_input",
			"sync stopped: credential rejected source:src-9",
			"budget exceeded",
		} {
			got := dashboardErrorAction(msg, member)
			if got.Href != "" || got.Label != "" {
				t.Fatalf("%q leaked a gated link to a member: %+v", msg, got)
			}
			if got.Text == "" || !strings.Contains(got.Text, "administrator") {
				t.Fatalf("%q must explain + name the administrator fallback, got %+v", msg, got)
			}
		}
	})

	t.Run("source auth failure deep-links the row edit", func(t *testing.T) {
		got := dashboardErrorAction("authorization failed for connector source:src-42", admin)
		if got.Href != "/dashboard/sources?edit=src-42" {
			t.Fatalf("Reconnect must deep-link the source edit drawer, got %q", got.Href)
		}
		got = dashboardErrorAction("credential rejected", admin)
		if got.Href != "/dashboard/sources" {
			t.Fatalf("Reconnect without an id links the sources page, got %q", got.Href)
		}
	})

	t.Run("budget exceeded links usage budgets", func(t *testing.T) {
		got := dashboardErrorAction("billing: budget exceeded", admin)
		if got.Href != "/dashboard/usage/budgets" {
			t.Fatalf("budget row href = %q", got.Href)
		}
	})

	t.Run("transient and denied families are text-only", func(t *testing.T) {
		for _, msg := range []string{
			"rate limit exceeded", "upstream request timed out", "service overloaded",
			"forbidden", "agent run is not permitted", "job not found",
		} {
			got := dashboardErrorAction(msg, admin)
			if got.Href != "" {
				t.Fatalf("%q must never produce a link, got %+v", msg, got)
			}
			if got.Text == "" {
				t.Fatalf("%q produced no explanation", msg)
			}
		}
	})
}

// TestDashboardEmptyContract renders the shared define: icon + sentence
// always, the action only when the caller supplies it — the OA-37 pattern
// with caller-side capability gating.
func TestDashboardEmptyContract(t *testing.T) {
	base, err := dashboardBaseTemplates()
	if err != nil {
		t.Fatalf("base set parse: %v", err)
	}
	render := func(e dashboardEmpty) string {
		var buf bytes.Buffer
		if err := base.ExecuteTemplate(&buf, "empty", e); err != nil {
			t.Fatalf("empty render: %v", err)
		}
		return buf.String()
	}

	withLink := render(dashboardEmpty{Icon: "chat", Text: "Nothing yet.", ActionLabel: "New chat", ActionHref: "/dashboard/chat?new=1"})
	if !strings.Contains(withLink, `href="/dashboard/chat?new=1"`) || !strings.Contains(withLink, "New chat") {
		t.Fatalf("link action missing: %s", withLink)
	}

	withDrawer := render(dashboardEmpty{Icon: "billing", Text: "Nothing yet.", ActionDrawerLabel: "New budget", ActionDrawerURL: "/dashboard/partials/usage/budget"})
	if !strings.Contains(withDrawer, `data-action="open-drawer"`) || !strings.Contains(withDrawer, "New budget") {
		t.Fatalf("drawer action missing: %s", withDrawer)
	}
	if strings.Contains(withDrawer, "href=") {
		t.Fatalf("drawer action must be a button, not a link: %s", withDrawer)
	}

	sentenceOnly := render(dashboardEmpty{Icon: "chat", Text: "Nothing yet."})
	if strings.Contains(sentenceOnly, "btn") || strings.Contains(sentenceOnly, "data-action") {
		t.Fatalf("action-less empty must render no affordance: %s", sentenceOnly)
	}
}

// TestDashboardCapsJSONKeys verifies the capability projection carries
// booleans keyed by the §3.1 names the client map filters on.
func TestDashboardCapsJSONKeys(t *testing.T) {
	raw := dashboardCapsJSON(dashboardCaps{Chat: true, ModelsManage: true})
	var got map[string]bool
	if err := json.Unmarshal([]byte(raw), &got); err != nil {
		t.Fatalf("projection is not JSON: %v", err)
	}
	if !got["chat"] || !got["models.manage"] || got["billing.view"] {
		t.Fatalf("capability flags wrong: %v", got)
	}
	if _, ok := got["platform.admin"]; ok {
		t.Fatal("scope strings must never reach the projection")
	}
}
