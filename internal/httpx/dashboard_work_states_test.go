// dashboard_work_states_test.go — the Work-group (Home/Chat/Search/Extract)
// rendering contract for plan 11-02: self-stopping 3s polling, capability-
// gated shared empty states, citation joins and the shared error→next-step
// map on failed rows. Every assertion is rendered-output semantics —
// presence/absence of the poll trigger, the empty-state action, the fix
// link — never source text or wording pins.
package httpx

import (
	"bytes"
	"strings"
	"testing"
	"time"

	"enclava-go/internal/connectors"
	"enclava-go/internal/extract"
	"enclava-go/internal/jobs"
	"enclava-go/internal/source"
)

// renderFrag executes one app-tree fragment against a synthetic view — the
// same model shape renderDashboardFragment serves.
func renderFrag(t *testing.T, page, name string, data any, caps dashboardCaps) string {
	t.Helper()
	set, err := dashboardTemplateSet(page)
	if err != nil {
		t.Fatalf("template set %q: %v", page, err)
	}
	view := dashboardView{Data: data, Can: caps}
	var buf bytes.Buffer
	if err := set.ExecuteTemplate(&buf, "frag-"+name, view); err != nil {
		t.Fatalf("render frag-%s (%s): %v", name, page, err)
	}
	return buf.String()
}

func pollTrigger(s string) bool { return strings.Contains(s, "every 3s") }

// ── TestWorkPolling: the trigger appears only while a non-terminal row is
// present and vanishes on the all-terminal render; unknown counts terminal. ──

func TestWorkPolling(t *testing.T) {
	running := func(statuses ...string) homeRunningData {
		d := homeRunningData{Empty: homeRunningEmpty}
		for _, st := range statuses {
			d.Rows = append(d.Rows, homeRunningRow{
				ActivityItem: ActivityItem{Status: st, Label: "job"},
				Live:         dashboardNonTerminal(st),
			})
			if dashboardNonTerminal(st) {
				d.Live = true
			}
		}
		return d
	}

	live := renderFrag(t, "home", "running", homeData{Running: running(jobs.StatusRunning)}, dashboardCaps{})
	if !pollTrigger(live) {
		t.Fatalf("live running fragment must poll: %s", live)
	}
	done := renderFrag(t, "home", "running", homeData{Running: running(jobs.StatusSucceeded, jobs.StatusFailed)}, dashboardCaps{})
	if pollTrigger(done) {
		t.Fatalf("terminal running fragment must not poll: %s", done)
	}
	unknown := renderFrag(t, "home", "running", homeData{Running: running(jobs.StatusUnknown)}, dashboardCaps{})
	if pollTrigger(unknown) {
		t.Fatalf("unknown-only fragment must not poll: %s", unknown)
	}

	// Attention fragment: a live (syncing) row polls; an all-terminal feed
	// does not.
	attnLive := renderFrag(t, "home", "attention", homeData{
		Attention:     []AttentionItem{{Kind: "sync_running", Text: "sync", Live: true}},
		AttentionLive: true,
	}, dashboardCaps{})
	if !pollTrigger(attnLive) {
		t.Fatalf("live attention fragment must poll: %s", attnLive)
	}
	attnDone := renderFrag(t, "home", "attention", homeData{
		Attention: []AttentionItem{{Kind: "budget", Text: "warn"}},
	}, dashboardCaps{})
	if pollTrigger(attnDone) {
		t.Fatalf("terminal attention fragment must not poll: %s", attnDone)
	}

	// Chat thread: pending run polls; failed run does not.
	msg := func(status string, pending bool) chatThreadMessage {
		return chatThreadMessage{Role: "user", RunID: "run-1", RunStatus: status, Pending: pending}
	}
	threadLive := renderFrag(t, "chat", "chat-thread", chatViewData{
		ConversationID: "c1",
		Thread:         &chatThreadData{Live: true, Messages: []chatThreadMessage{msg(jobs.StatusRunning, true)}},
	}, dashboardCaps{})
	if !pollTrigger(threadLive) {
		t.Fatalf("live thread must poll: %s", threadLive)
	}
	threadDone := renderFrag(t, "chat", "chat-thread", chatViewData{
		ConversationID: "c1",
		Thread:         &chatThreadData{Messages: []chatThreadMessage{msg(jobs.StatusFailed, false)}},
	}, dashboardCaps{})
	if pollTrigger(threadDone) {
		t.Fatalf("terminal thread must not poll: %s", threadDone)
	}

	// Extract jobs: the Live flag drives the trigger.
	jobsLive := renderFrag(t, "extract", "extract-jobs", extractView{
		Live: true,
		Jobs: []extract.JobView{{Status: jobs.StatusRunning, FileName: "a.pdf"}},
	}, dashboardCaps{})
	if !pollTrigger(jobsLive) {
		t.Fatalf("live extract jobs must poll: %s", jobsLive)
	}
	jobsDone := renderFrag(t, "extract", "extract-jobs", extractView{
		Jobs: []extract.JobView{{Status: jobs.StatusUnknown, FileName: "a.pdf"}},
	}, dashboardCaps{})
	if pollTrigger(jobsDone) {
		t.Fatalf("terminal extract jobs must not poll: %s", jobsDone)
	}
}

// extractHasLive is the flag's producer: assert the predicate itself
// including the unknown-is-terminal rule.
func TestWorkExtractHasLive(t *testing.T) {
	if !extractHasLive([]extract.JobView{{Status: jobs.StatusQueued}}) {
		t.Fatal("queued job must count live")
	}
	if extractHasLive([]extract.JobView{{Status: jobs.StatusUnknown}}) {
		t.Fatal("unknown job must be terminal")
	}
	if extractHasLive([]extract.JobView{{Status: jobs.StatusFailed}, {Status: jobs.StatusSucceeded}}) {
		t.Fatal("all-terminal set must not count live")
	}
}

// ── TestWorkProgress: .progress only with real counts; .pulse otherwise. ──

func TestWorkProgress(t *testing.T) {
	pctRow := homeRunningRow{
		ActivityItem: ActivityItem{Status: jobs.StatusRunning, Label: "job"},
		Live:         true, Percent: 40, HasPercent: true,
	}
	out := renderFrag(t, "home", "running", homeData{
		Running: homeRunningData{Rows: []homeRunningRow{pctRow}, Live: true},
	}, dashboardCaps{})
	if !strings.Contains(out, `class="progress"`) {
		t.Fatalf("real percent must render a progress bar: %s", out)
	}
	noPct := renderFrag(t, "home", "running", homeData{
		Running: homeRunningData{Rows: []homeRunningRow{{
			ActivityItem: ActivityItem{Status: jobs.StatusRunning, Label: "job"},
			Live:         true,
		}}, Live: true},
	}, dashboardCaps{})
	if strings.Contains(noPct, `class="progress"`) || !strings.Contains(noPct, `class="pulse"`) {
		t.Fatalf("countless running row must render indeterminate pulse, not a bar: %s", noPct)
	}
}

// ── TestWorkEmpty: shared empty states render the action only when the
// caller's caps grant it; grantless members get the sentence-only state. ──

func TestWorkEmpty(t *testing.T) {
	empty := func(e dashboardEmpty) string {
		return renderFrag(t, "chat", "chat-history", chatViewData{HistoryEmpty: e}, dashboardCaps{})
	}
	mgr := empty(dashboardEmpty{Icon: "chat", Text: "No conversations yet.", ActionLabel: "New chat", ActionHref: "/dashboard/chat"})
	if !strings.Contains(mgr, `class="empty"`) || !strings.Contains(mgr, `href="/dashboard/chat"`) {
		t.Fatalf("capable empty must render the action link: %s", mgr)
	}
	mem := empty(dashboardEmpty{Icon: "chat", Text: "No conversations yet."})
	if !strings.Contains(mem, `class="empty"`) || strings.Contains(mem, `<a class="btn primary"`) {
		t.Fatalf("grantless empty must render sentence-only: %s", mem)
	}
	// Populated fixture renders no empty state at all.
	full := renderFrag(t, "chat", "chat-history", chatViewData{
		History: chatHistoryData{Groups: []chatHistoryGroup{{Label: "Today",
			Items: []chatHistoryItem{{ID: "c1", Title: "hi"}}}}},
	}, dashboardCaps{})
	if strings.Contains(full, `class="empty"`) {
		t.Fatalf("populated history must not render the empty state: %s", full)
	}

	// Search: zero-row query shows the shared empty, populated rows do not.
	noRows := renderFrag(t, "search", "search-results", searchViewData{
		Query:        "q",
		Results:      &searchResultsData{Query: "q"},
		ResultsEmpty: dashboardEmpty{Icon: "search", Text: "nothing"},
	}, dashboardCaps{})
	if !strings.Contains(noRows, `class="empty"`) {
		t.Fatalf("zero-row results must render the empty state: %s", noRows)
	}
	rows := renderFrag(t, "search", "search-results", searchViewData{
		Query: "q",
		Results: &searchResultsData{Query: "q", Rows: []searchResultRow{{
			Title: "Doc", Collection: "Col", Content: "text", EvidenceID: "e1",
		}}},
		ResultsEmpty: dashboardEmpty{Icon: "search", Text: "nothing"},
	}, dashboardCaps{})
	if strings.Contains(rows, `class="empty"`) {
		t.Fatalf("populated results must not render the empty state: %s", rows)
	}

	// Extract jobs empty → the gated action the builder set.
	jobsEmpty := renderFrag(t, "extract", "extract-jobs", extractView{
		JobsEmpty: dashboardEmpty{Icon: "extract", Text: "none", ActionLabel: "New", ActionHref: "/dashboard/extract?tab=process"},
	}, dashboardCaps{})
	if !strings.Contains(jobsEmpty, `href="/dashboard/extract?tab=process"`) {
		t.Fatalf("extract jobs empty must carry the gated action: %s", jobsEmpty)
	}
}

// Grant-empty (D-25): the assistant picker and extract process picker show
// the grants sentence for a member and the create affordance for a
// manager — the two data-flag branches fillEmptyStates drives.
func TestWorkGrantEmptyStates(t *testing.T) {
	member := chatViewData{}
	member.fillEmptyStates(dashboardCaps{Chat: true})
	if member.AssistantsEmpty.ActionHref != "" {
		t.Fatal("member must not receive a create action")
	}
	manager := chatViewData{}
	manager.fillEmptyStates(dashboardCaps{Chat: true, AssistantsManage: true})
	if manager.AssistantsEmpty.ActionHref == "" {
		t.Fatal("manager must receive the create action")
	}
	ev := extractView{}
	ev.fillEmptyStates(dashboardCaps{Extract: true})
	if ev.ProcessEmpty.ActionHref != "" {
		t.Fatal("grantless member picker must not offer template creation")
	}
	ev.fillEmptyStates(dashboardCaps{Extract: true, ExtractManage: true})
	if ev.ProcessEmpty.ActionHref == "" {
		t.Fatal("manager picker must offer template creation")
	}
}

// ── TestWorkCitationJoin: citations join only real parts. ──

func TestWorkCitationJoin(t *testing.T) {
	page7 := 7
	locators := []source.Locator{{Page: &page7, Section: "Intro"}}
	line := evidenceLocationLine(locators)
	if line == "" || !strings.Contains(line, "7") || !strings.Contains(line, "Intro") {
		t.Fatalf("citation must join page and section: %q", line)
	}
	if got := evidenceLocationLine(nil); got != "" {
		t.Fatalf("empty locators must render nothing, got %q", got)
	}
	if got := dashboardCitation("Report.pdf", page7, "Intro"); !strings.Contains(got, "Report.pdf") ||
		!strings.Contains(got, "7") || !strings.Contains(got, "Intro") {
		t.Fatalf("document citation must join real parts: %q", got)
	}
	if got := dashboardCitation("Report.pdf", 0, ""); got != "Report.pdf" {
		t.Fatalf("document-only citation must drop absent parts: %q", got)
	}
}

// ── TestWorkErrorActions: the shared map drives failed-row rendering
// server-side (attention fix links; failed runline + failed extract job
// sublines through dashboardErrorAction with the caller's caps). ──

func TestWorkErrorActions(t *testing.T) {
	runErr := "required price missing: cached_input"
	memView := renderFrag(t, "chat", "chat-thread", chatViewData{
		ConversationID: "c1",
		Thread: &chatThreadData{Messages: []chatThreadMessage{{
			Role: "assistant", RunID: "r", RunStatus: jobs.StatusFailed, RunError: runErr,
		}}},
	}, dashboardCaps{Chat: true})
	if !strings.Contains(memView, "runline") {
		t.Fatalf("failed run must render the runline: %s", memView)
	}
	if strings.Contains(memView, "/dashboard/models/pricing") {
		t.Fatalf("member must not get the pricing fix link: %s", memView)
	}
	mgrView := renderFrag(t, "chat", "chat-thread", chatViewData{
		ConversationID: "c1",
		Thread: &chatThreadData{Messages: []chatThreadMessage{{
			Role: "assistant", RunID: "r", RunStatus: jobs.StatusFailed, RunError: runErr,
		}}},
	}, dashboardCaps{Chat: true, ModelsManage: true})
	if !strings.Contains(mgrView, "/dashboard/models/pricing") {
		t.Fatalf("manager must get the pricing fix link: %s", mgrView)
	}

	// Failed extract job row: the projected JobView.error_message feeds the
	// same map — member sees text only, manager the fix href.
	job := extract.JobView{Status: jobs.StatusFailed, FileName: "a.pdf",
		ErrorMessage: "required price missing: vision_page"}
	jm := renderFrag(t, "extract", "extract-jobs", extractView{Jobs: []extract.JobView{job}}, dashboardCaps{Extract: true})
	if strings.Contains(jm, "/dashboard/models/pricing") {
		t.Fatalf("member job row must not link pricing: %s", jm)
	}
	ja := renderFrag(t, "extract", "extract-jobs", extractView{Jobs: []extract.JobView{job}},
		dashboardCaps{Extract: true, ModelsManage: true})
	if !strings.Contains(ja, "/dashboard/models/pricing") {
		t.Fatalf("manager job row must link pricing: %s", ja)
	}

	// Result drawer: a failed JobView surfaces the mapped explanation.
	drawer := renderFrag(t, "extract", "extract-result", extractView{Job: &job},
		dashboardCaps{Extract: true, ModelsManage: true})
	if !strings.Contains(drawer, "/dashboard/models/pricing") {
		t.Fatalf("failed drawer must surface the fix link: %s", drawer)
	}

	// Attention rows carry producer-side fix links: auth-failed sync →
	// the reconnect deep link on the affected source.
	syncItem := AttentionItem{Kind: "sync_failed", Text: "sync failed authentication",
		ActionLabel: "Open source", ActionHref: "/dashboard/sources/s1",
		FixText: "authentication", FixLabel: "Reconnect", FixHref: "/dashboard/sources?edit=s1"}
	out := renderFrag(t, "home", "attention", homeData{Attention: []AttentionItem{syncItem}}, dashboardCaps{})
	if !strings.Contains(out, "/dashboard/sources?edit=s1") {
		t.Fatalf("auth-failed row must render the reconnect deep link: %s", out)
	}
}

// Producer side: syncAttention feeds the fix contract — auth failures
// reconnect, plain failures keep the open-source target, running syncs
// mark the row live (the fragment polls while any hold).
func TestWorkSyncAttentionFixes(t *testing.T) {
	src := connectors.Source{ID: "s1", Name: "Drive"}
	item, ok := syncAttention(src, []connectors.SyncJob{{Status: jobs.StatusFailed, ErrorMessage: "401 Unauthorized"}})
	if !ok || item.FixHref == "" || !strings.Contains(item.FixHref, "edit=s1") {
		t.Fatalf("auth-failed sync must carry the reconnect fix: %+v", item)
	}
	plain, ok := syncAttention(src, []connectors.SyncJob{{Status: jobs.StatusFailed, ErrorMessage: "disk quota exceeded"}})
	if !ok || plain.FixHref != "" {
		t.Fatalf("plain sync failure keeps open-source action only: %+v", plain)
	}
	running, ok := syncAttention(src, []connectors.SyncJob{{Status: jobs.StatusRunning, CreatedAt: time.Now()}})
	if !ok || !running.Live {
		t.Fatalf("running sync must be a live attention row: %+v", running)
	}
}
