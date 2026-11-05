// dashboard_chat_sources_test.go — the per-answer Sources fragment: rows
// resolve through ResolveEvidence so unreadable ids vanish, ids cap at
// chatSourcesLimit, and the outbound link stays behind the http(s) gate the
// knowledge/search surfaces share.
package httpx

import (
	"context"
	"net/http"
	"strings"
	"testing"

	"enclava-go/internal/auth"
	"enclava-go/internal/rag"
)

// TestDashboardChatSourcesPartial stages one real evidence row with native
// provenance: the fragment renders title, provider label, the native link
// target and the View evidence action; an unreadable id is silently omitted.
func TestDashboardChatSourcesPartial(t *testing.T) {
	f := newChatFixture(t)
	ctx := context.Background()
	ownerSess, ownerCSRF := loginUser(t, f.identityFixture, "chat-src-owner@example.com", []string{"chatbot", "rag"}, "chat-pass")
	var ownerID string
	if err := f.db.SQL.QueryRowContext(ctx,
		`SELECT id::text FROM users WHERE tenant_id = $1::uuid AND email = $2`,
		f.tenant, "chat-src-owner@example.com").Scan(&ownerID); err != nil {
		t.Fatalf("read owner id: %v", err)
	}
	collection, err := f.app.ragSvc.CreateCollection(ctx, auth.Principal{
		TenantID: f.tenant, ActorID: ownerID, UserID: ownerID, ActorKind: auth.ActorKindUser,
	}, rag.CreateCollectionInput{Name: "src-docs", ACL: map[string]any{"public": false}})
	if err != nil {
		t.Fatalf("seed collection: %v", err)
	}
	df := ragDocsFixture{
		app: f.app, db: f.db, repo: f.ragRepo, svc: f.app.ragSvc, tenant: f.tenant,
		ownerID: ownerID, memberID: ownerID, collectionID: collection.ID,
	}
	evidenceID := df.stageEvidenceObject(t, "obj_chat_src", ownerID)
	if _, err := f.db.SQL.ExecContext(ctx,
		`UPDATE source_object_versions SET provenance = $3
		 WHERE tenant_id = $1::uuid AND object_id = $2`,
		f.tenant, "obj_chat_src",
		`{"url":"https://workspace.slack.com/archives/C1/p1","source_kind":"slack","title":"Quarterly thread"}`); err != nil {
		t.Fatalf("set provenance: %v", err)
	}

	rec := f.chatGet(t, ownerSess, ownerCSRF, "/dashboard/partials/chat/sources?ids="+evidenceID+",ev_missing")
	if rec.Code != http.StatusOK {
		t.Fatalf("sources partial = %d body=%s", rec.Code, rec.Body.String())
	}
	body := rec.Body.String()
	for _, want := range []string{
		"Sources", "Quarterly thread", "Slack", "Page 7",
		`href="https://workspace.slack.com/archives/C1/p1"`, "Open in Slack",
		`data-action="evidence:open"`, `data-evidence-id="` + evidenceID + `"`,
		`target="_blank"`, `rel="noopener noreferrer"`,
	} {
		if !strings.Contains(body, want) {
			t.Fatalf("sources fragment missing %q: %s", want, body)
		}
	}
	if strings.Count(body, "source-row") != 1 {
		t.Fatalf("unreadable id leaked a row: %s", body)
	}

	// A user who cannot open the collection sees an empty block, never an
	// error card.
	otherSess, otherCSRF := loginUser(t, f.identityFixture, "chat-src-other@example.com", []string{"chatbot", "rag"}, "chat-pass")
	rec = f.chatGet(t, otherSess, otherCSRF, "/dashboard/partials/chat/sources?ids="+evidenceID)
	if rec.Code != http.StatusOK || strings.Contains(rec.Body.String(), "source-row") {
		t.Fatalf("denied sources = %d body=%s", rec.Code, rec.Body.String())
	}
}

// TestDashboardChatSourcesCap bounds the fragment to chatSourcesLimit rows —
// later resolvable ids drop off.
func TestDashboardChatSourcesCap(t *testing.T) {
	f := newChatFixture(t)
	ctx := context.Background()
	ownerSess, ownerCSRF := loginUser(t, f.identityFixture, "chat-cap-owner@example.com", []string{"chatbot", "rag"}, "chat-pass")
	var ownerID string
	if err := f.db.SQL.QueryRowContext(ctx,
		`SELECT id::text FROM users WHERE tenant_id = $1::uuid AND email = $2`,
		f.tenant, "chat-cap-owner@example.com").Scan(&ownerID); err != nil {
		t.Fatalf("read owner id: %v", err)
	}
	collection, err := f.app.ragSvc.CreateCollection(ctx, auth.Principal{
		TenantID: f.tenant, ActorID: ownerID, UserID: ownerID, ActorKind: auth.ActorKindUser,
	}, rag.CreateCollectionInput{Name: "cap-docs", ACL: map[string]any{"public": true}})
	if err != nil {
		t.Fatalf("seed collection: %v", err)
	}
	df := ragDocsFixture{
		app: f.app, db: f.db, repo: f.ragRepo, svc: f.app.ragSvc, tenant: f.tenant,
		ownerID: ownerID, memberID: ownerID, collectionID: collection.ID,
	}
	ids := make([]string, 0, chatSourcesLimit+1)
	for i := 0; i <= chatSourcesLimit; i++ {
		objectID := "obj_cap_" + string(rune('a'+i))
		ids = append(ids, df.stageEvidenceObject(t, objectID, ownerID))
	}
	rec := f.chatGet(t, ownerSess, ownerCSRF, "/dashboard/partials/chat/sources?ids="+strings.Join(ids, ","))
	if rec.Code != http.StatusOK {
		t.Fatalf("capped sources = %d body=%s", rec.Code, rec.Body.String())
	}
	body := rec.Body.String()
	if got := strings.Count(body, "source-row"); got != chatSourcesLimit {
		t.Fatalf("sources rows = %d, want %d", got, chatSourcesLimit)
	}
	if strings.Contains(body, ids[chatSourcesLimit]) {
		t.Fatalf("row past the cap rendered: %s", body)
	}
}

// TestDashboardSourceLinkSafety covers the shared outbound-link gate and the
// knowledge/search renderings: only http(s) becomes a link.
func TestDashboardSourceLinkSafety(t *testing.T) {
	for raw, want := range map[string]string{
		"https://github.com/o/r/blob/sha/f.go": "https://github.com/o/r/blob/sha/f.go",
		"http://intranet/doc":                  "http://intranet/doc",
		"urn:test:obj":                         "",
		"javascript:alert(1)":                  "",
		"slack://channel/C/1":                  "",
		"":                                     "",
	} {
		if got := safeExternalLink(raw); got != want {
			t.Fatalf("safeExternalLink(%q) = %q, want %q", raw, got, want)
		}
	}
	if got := sourceProviderLabel("github"); got != "GitHub" {
		t.Fatalf("provider label = %q", got)
	}
	if got := sourceProviderLabel("upload"); got != "" {
		t.Fatalf("upload provider = %q, want none", got)
	}

	// Knowledge: safe URI links out, unsafe stays plain text.
	rows := knowledgeDocRows([]rag.Document{
		{ID: "d1", FileName: "a.go", Status: "ready", SourceType: "github", SourceURI: "https://github.com/o/r/blob/s/a.go"},
		{ID: "d2", FileName: "b.txt", Status: "ready", SourceURI: "urn:test:b"},
	})
	rendered := renderFrag(t, "knowledge_collection", "knowledge-documents",
		knowledgeCollectionData{
			Collection: rag.Collection{ID: "c1"},
			Status:     "all", Docs: rows,
			Empty: dashboardEmpty{Icon: "knowledge", Text: "No documents."},
		}, dashboardCaps{})
	if !strings.Contains(rendered, `href="https://github.com/o/r/blob/s/a.go"`) ||
		!strings.Contains(rendered, "Open in GitHub") {
		t.Fatalf("knowledge link missing: %s", rendered)
	}
	if !strings.Contains(rendered, "urn:test:b") || strings.Contains(rendered, `href="urn:test:b"`) {
		t.Fatalf("unsafe uri rendered wrong: %s", rendered)
	}

	// Search: the row's link renders only when OpenHref was populated.
	rendered = renderFrag(t, "search", "search-results", searchViewData{
		Query: "q", Mode: "hybrid",
		Results: &searchResultsData{Query: "q", Rows: []searchResultRow{
			{Title: "Hit", EvidenceID: "ev1", SourceKind: "github",
				OpenHref: "https://github.com/o/r/blob/s/a.go", Provider: "GitHub"},
			{Title: "Plain", EvidenceID: "ev2", SourceKind: "upload"},
		}},
	}, dashboardCaps{})
	if !strings.Contains(rendered, `href="https://github.com/o/r/blob/s/a.go"`) ||
		!strings.Contains(rendered, "Open in GitHub") {
		t.Fatalf("search link missing: %s", rendered)
	}
	if strings.Count(rendered, "Open in") != 1 {
		t.Fatalf("link rendered without a safe href: %s", rendered)
	}
}
