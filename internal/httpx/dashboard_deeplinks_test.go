// dashboard_deeplinks_test.go — v1.2 dashboard coverage: the source
// deep-link gate (http(s) or same-origin path only) and the search-row /
// chat-citation link selection built on top of it.
package httpx

import (
	"strings"
	"testing"

	"enclava-go/internal/rag"
)

// TestSafeSourceLink pins the dashboard's outbound-link gate: absolute
// http(s) and same-origin absolute paths pass; every other scheme,
// protocol-relative host and backslash trick stays rejected.
func TestSafeSourceLink(t *testing.T) {
	for raw, want := range map[string]string{
		"https://github.com/o/r/pull/1#discussion_r9": "https://github.com/o/r/pull/1#discussion_r9",
		"http://intranet/wiki/page":                   "http://intranet/wiki/page",
		"/api/v1/rag/evidence/ev_1/copy":              "/api/v1/rag/evidence/ev_1/copy",
		"/api/v1/rag/evidence/ev_1/copy#page=3":       "/api/v1/rag/evidence/ev_1/copy#page=3",
		"  https://a.example/x  ":                     "https://a.example/x",
		"//evil.example/phish":                        "",
		"/\\evil.example":                             "",
		"javascript:alert(1)":                         "",
		"data:text/html,<script>x</script>":           "",
		"slack://channel/C/1":                         "",
		"urn:test:obj":                                "",
		"api/v1/rag/evidence/ev_1/copy":               "",
		"":                                            "",
	} {
		if got := safeSourceLink(raw); got != want {
			t.Fatalf("safeSourceLink(%q) = %q, want %q", raw, got, want)
		}
	}
}

// TestSearchResultRowForDeepLinks pins the fallback order: the chunk deep
// link wins, then the document URL, then the legacy source URI — each only
// when it survives the URL gate; the container link appears only when it
// differs from the primary target.
func TestSearchResultRowForDeepLinks(t *testing.T) {
	cases := []struct {
		name       string
		res        rag.SearchResult
		wantTitle  string
		wantSource string
		wantLabel  string
		wantStamp  string
	}{
		{
			name: "deep link wins, container link secondary",
			res: rag.SearchResult{
				FileName: "digest.txt", SourceKind: "slack", EvidenceID: "ev1",
				Link:            "https://workspace.slack.com/archives/C1/p1715000000000001",
				SourceURL:       "https://workspace.slack.com/archives/C1",
				Author:          "Ana",
				Container:       "#eng",
				SourceUpdatedAt: "2026-05-08T09:42:00Z",
				SourceCreatedAt: "2026-05-01T10:00:00Z",
			},
			wantTitle:  "https://workspace.slack.com/archives/C1/p1715000000000001",
			wantSource: "https://workspace.slack.com/archives/C1",
			wantLabel:  "Open thread",
			wantStamp:  "2026-05-08T09:42:00Z",
		},
		{
			name: "no deep link — document URL is the title target",
			res: rag.SearchResult{
				FileName: "issue.txt", SourceKind: "github", EvidenceID: "ev1",
				SourceURL: "https://github.com/o/r/issues/42",
			},
			wantTitle: "https://github.com/o/r/issues/42",
			wantLabel: "",
		},
		{
			name: "legacy source URI is the last resort",
			res: rag.SearchResult{
				FileName: "old.txt", SourceKind: "upload", EvidenceID: "ev1",
				SourceURI: "https://files.example.com/old.txt",
			},
			wantTitle: "https://files.example.com/old.txt",
			wantLabel: "",
		},
		{
			name: "unsafe deep link falls through to the document URL",
			res: rag.SearchResult{
				FileName: "bad.txt", SourceKind: "notion", EvidenceID: "ev1",
				Link:      "javascript:alert(1)",
				SourceURL: "https://www.notion.so/page-1",
			},
			wantTitle: "https://www.notion.so/page-1",
			wantLabel: "",
		},
		{
			name: "same-origin retained copy link passes the gate",
			res: rag.SearchResult{
				FileName: "upload.txt", SourceKind: "upload", EvidenceID: "ev1",
				Link:      "/api/v1/rag/evidence/ev1/copy",
				SourceURI: "urn:test:obj",
			},
			wantTitle: "/api/v1/rag/evidence/ev1/copy",
			wantLabel: "",
		},
		{
			name: "identical link and source URL collapse the secondary",
			res: rag.SearchResult{
				FileName: "same.txt", SourceKind: "jira", EvidenceID: "ev1",
				Link:      "https://acme.atlassian.net/browse/ENG-7",
				SourceURL: "https://acme.atlassian.net/browse/ENG-7",
			},
			wantTitle: "https://acme.atlassian.net/browse/ENG-7",
			wantLabel: "",
		},
		{
			name: "created timestamp is the fallback stamp",
			res: rag.SearchResult{
				FileName: "created.txt", SourceKind: "upload",
				SourceCreatedAt: "2026-04-01T08:00:00Z",
				SourceUpdatedAt: "not-a-timestamp",
			},
			wantTitle: "",
			wantLabel: "",
			wantStamp: "2026-04-01T08:00:00Z",
		},
		{
			name: "unparseable stamps never reach the template",
			res: rag.SearchResult{
				FileName: "bad-stamp.txt", SourceKind: "upload",
				SourceUpdatedAt: "yesterday",
				SourceCreatedAt: "2026-13-99",
			},
			wantTitle: "",
			wantLabel: "",
			wantStamp: "",
		},
	}
	for _, tc := range cases {
		row := searchResultRowFor(tc.res)
		if row.TitleHref != tc.wantTitle {
			t.Fatalf("%s: TitleHref = %q, want %q", tc.name, row.TitleHref, tc.wantTitle)
		}
		if row.SourceHref != tc.wantSource {
			t.Fatalf("%s: SourceHref = %q, want %q", tc.name, row.SourceHref, tc.wantSource)
		}
		if row.SourceLabel != tc.wantLabel {
			t.Fatalf("%s: SourceLabel = %q, want %q", tc.name, row.SourceLabel, tc.wantLabel)
		}
		if row.Stamp != tc.wantStamp {
			t.Fatalf("%s: Stamp = %q, want %q", tc.name, row.Stamp, tc.wantStamp)
		}
	}

	// Container-link labels by kind.
	for kind, want := range map[string]string{
		"slack":        "Open thread",
		"github":       "Open issue",
		"gitlab":       "Open issue",
		"jira":         "Open issue",
		"linear":       "Open issue",
		"notion":       "Open page",
		"teams":        "Open in Microsoft Teams",
		"google_drive": "Open in Google Drive",
		"upload":       "Open in source",
	} {
		if got := sourceContainerLinkLabel(kind, sourceProviderLabel(kind)); got != want {
			t.Fatalf("sourceContainerLinkLabel(%q) = %q, want %q", kind, got, want)
		}
	}
}

// TestSearchResultsFragmentDeepLinks renders the shared results fragment:
// the title anchors to the deep link in a new tab, the meta line carries
// author/container/timestamp, and unsafe hrefs never reach the markup.
func TestSearchResultsFragmentDeepLinks(t *testing.T) {
	rows := []searchResultRow{
		searchResultRowFor(rag.SearchResult{
			FileName: "standup.pdf", SourceKind: "slack", EvidenceID: "ev_slack",
			CollectionName:  "Comms",
			Link:            "https://workspace.slack.com/archives/C1/p1715000000000001",
			SourceURL:       "https://workspace.slack.com/archives/C1",
			Author:          "Ana Ruiz",
			Container:       "#eng",
			SourceUpdatedAt: "2026-05-08T09:42:00Z",
			Content:         "deploy window",
		}),
		searchResultRowFor(rag.SearchResult{
			FileName: "unsafe.txt", SourceKind: "upload", EvidenceID: "ev_bad",
			Link:      "javascript:alert(1)",
			SourceURI: "data:text/html,x",
			Content:   "nothing safe",
		}),
	}
	rendered := renderFrag(t, "search", "search-results", searchViewData{
		Query: "deploy", Mode: "hybrid",
		Results: &searchResultsData{Query: "deploy", Rows: rows},
	}, dashboardCaps{})

	for _, want := range []string{
		`<a href="https://workspace.slack.com/archives/C1/p1715000000000001" target="_blank" rel="noopener noreferrer">standup.pdf</a>`,
		`href="https://workspace.slack.com/archives/C1" target="_blank" rel="noopener noreferrer">Open thread</a>`,
		"Comms", "Ana Ruiz", "#eng", "<time datetime=\"2026-05-08T09:42:00Z\"",
	} {
		if !strings.Contains(rendered, want) {
			t.Fatalf("results fragment missing %q:\n%s", want, rendered)
		}
	}
	if strings.Contains(rendered, "javascript:") || strings.Contains(rendered, "data:text/html") {
		t.Fatalf("unsafe href rendered: %s", rendered)
	}
	// The unsafe row's title stays plain text — no anchor around it.
	if strings.Contains(rendered, `>unsafe.txt</a>`) {
		t.Fatalf("unsafe title became a link: %s", rendered)
	}
}

// TestChatSourceDeepLinkRendering pins the citation side: the sources rows
// and the evidence card render the resolved deep link — including the
// same-origin retained-copy path — through the shared URL gate.
func TestChatSourceDeepLinkRendering(t *testing.T) {
	copyURL := "/api/v1/rag/evidence/ev_copy/copy"
	out := renderFrag(t, "chat", "chat-sources", []chatSourceRow{
		{EvidenceID: "ev_copy", Title: "Upload.txt", OpenHref: copyURL},
		{EvidenceID: "ev_bad", Title: "Bad.txt", OpenHref: "javascript:alert(1)"},
	}, dashboardCaps{})
	if !strings.Contains(out, `href="`+copyURL+`"`) ||
		!strings.Contains(out, `target="_blank" rel="noopener noreferrer"`) {
		t.Fatalf("chat sources deep link missing: %s", out)
	}
	if strings.Contains(out, "javascript:") {
		t.Fatalf("unsafe link rendered: %s", out)
	}

	card := renderFrag(t, "chat", "evidence", evidenceCardData{
		EvidenceID: "ev_copy", Document: "Upload.txt", OpenHref: copyURL,
	}, dashboardCaps{})
	if !strings.Contains(card, `href="`+copyURL+`"`) {
		t.Fatalf("evidence card deep link missing: %s", card)
	}
}
