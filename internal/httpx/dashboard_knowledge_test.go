// dashboard_knowledge_test.go — render tests for the Knowledge pages
// (OA-22, plan 09-01): the §3.4 member column split on the list, the
// collection tab bar per capability, member 403s on sources/settings/
// profiles, and OA-21 row privacy — a denied document never reaches member
// markup.

package httpx

import (
	"context"
	"net/http"
	"net/http/httptest"
	"strings"
	"testing"

	"enclava-go/internal/auth"
	"enclava-go/internal/rag"
	"enclava-go/internal/source"
	"enclava-go/internal/store/postgres"
)

// knowledgeFixture reuses the identity fixture's real rag service over an
// isolated schema; collections are created as a platform-admin principal and
// sessions log in through the real browser form.
type knowledgeFixture struct {
	*identityFixture
	repo   *postgres.RAGRepo
	adminP auth.Principal
}

func newKnowledgeFixture(t *testing.T) *knowledgeFixture {
	f := &knowledgeFixture{
		identityFixture: newIdentityFixture(t),
	}
	f.repo = postgres.NewRAGRepo(f.db)
	f.adminP = auth.Principal{
		TenantID:            f.tenant,
		ActorID:             "knowledge_admin",
		UserID:              "knowledge_admin",
		ActorKind:           auth.ActorKindServiceAccount,
		Scopes:              map[string]struct{}{auth.ScopePlatformAdmin: {}},
		AllowedModelsAll:    true,
		AllowedChatbotsAll:  true,
		AllowedEndpointsAll: true,
	}
	return f
}

func (f *knowledgeFixture) get(t *testing.T, session, csrf *http.Cookie, path string) *httptest.ResponseRecorder {
	t.Helper()
	return sessionDo(t, f.app, http.MethodGet, path, "", session, csrf)
}

// TestDashboardKnowledgeTemplateSets parses all three page sets — template
// parse errors surface only at render otherwise.
func TestDashboardKnowledgeTemplateSets(t *testing.T) {
	for _, page := range []string{"knowledge", "knowledge_collection", "knowledge_profiles"} {
		if _, err := dashboardTemplateSet(page); err != nil {
			t.Fatalf("template set %q: %v", page, err)
		}
	}
}

// member logs a rag-scoped member in and returns its cookies + actor id.
func (f *knowledgeFixture) member(t *testing.T, email string) (session, csrf *http.Cookie, memberID string) {
	t.Helper()
	session, csrf = loginUserWithGrants(t, f.identityFixture, email, []string{"rag"}, auth.ResourceGrants{}, "know-pass")
	if err := f.db.SQL.QueryRowContext(context.Background(),
		`SELECT id::text FROM users WHERE tenant_id = $1::uuid AND email = $2`,
		f.tenant, email).Scan(&memberID); err != nil {
		t.Fatalf("read member id: %v", err)
	}
	return session, csrf, memberID
}

// manager logs a rag+rag.manage+connectors.manage session in.
func (f *knowledgeFixture) manager(t *testing.T, email string) (session, csrf *http.Cookie) {
	t.Helper()
	return loginUserWithGrants(t, f.identityFixture, email,
		[]string{"rag", "rag.manage", "connectors.manage"}, auth.ResourceGrants{}, "know-pass")
}

func (f *knowledgeFixture) collection(t *testing.T, name string, audience []string) rag.Collection {
	t.Helper()
	c, err := f.app.ragSvc.CreateCollection(context.Background(), f.adminP, rag.CreateCollectionInput{
		Name:         name,
		ChunkSize:    200,
		ChunkOverlap: 20,
		ACL:          map[string]any{"audience": audience},
	})
	if err != nil {
		t.Fatalf("create collection %s: %v", name, err)
	}
	return c
}

// stageDoc persists a collection-mode object + first version + collection
// item with no published generation — the row renders as Processing.
func (f *knowledgeFixture) stageDoc(t *testing.T, collection rag.Collection, objectID, fileName string) {
	t.Helper()
	ctx := context.Background()
	owner := "knowledge_admin"
	if _, err := f.repo.UpsertObject(ctx, source.Object{
		TenantID: f.tenant, ID: objectID, Kind: "document", NativeID: "native-" + objectID,
		AuthorizationMode: source.AuthorizationCollection, OwnerActorID: owner,
	}); err != nil {
		t.Fatal(err)
	}
	if err := f.repo.InsertObjectVersion(ctx, f.tenant, source.ObjectVersion{
		ObjectID: objectID, ObservedSeq: 1, ArtifactRef: "art_" + objectID, SHA256: "sha_" + objectID,
		ByteSize: 64, MimeType: "text/plain", OriginalFilename: fileName,
		Provenance: `{"kind":"upload"}`,
	}); err != nil {
		t.Fatal(err)
	}
	if err := f.repo.AddCollectionItem(ctx, f.tenant, collection.ID, objectID, owner); err != nil {
		t.Fatal(err)
	}
}

// TestDashboardKnowledgeList covers the §3.4 member/manager column split:
// a member renders only Collection/Documents/Updated — no health pill, no
// sources-count or access cell and no New-collection control; a manager
// renders all of them.
func TestDashboardKnowledgeList(t *testing.T) {
	f := newKnowledgeFixture(t)
	sess, csrf, memberID := f.member(t, "know-member@example.test")
	f.collection(t, "Contracts", []string{memberID})
	f.collection(t, "Org-wide", []string{"*"})

	page := f.get(t, sess, csrf, "/dashboard/knowledge")
	if page.Code != http.StatusOK {
		t.Fatalf("member list = %d body=%s", page.Code, page.Body.String())
	}
	body := page.Body.String()
	for _, want := range []string{"Contracts", `data-label="Collection"`, `data-label="Documents"`, `data-label="Updated"`} {
		if !strings.Contains(body, want) {
			t.Fatalf("member list missing %q", want)
		}
	}
	for _, banned := range []string{`data-label="Health"`, `data-label="Sources"`, `data-label="Access"`, `data-action="knowledge:new"`, `class="pill`} {
		if strings.Contains(body, banned) {
			t.Fatalf("member list must not contain %q", banned)
		}
	}
	if !strings.Contains(body, `/dashboard/static/pages/knowledge.js`) {
		t.Fatal("page must load the knowledge module")
	}

	mgr, mgrCSRF := f.manager(t, "know-manager@example.test")
	mgrPage := f.get(t, mgr, mgrCSRF, "/dashboard/knowledge")
	if mgrPage.Code != http.StatusOK {
		t.Fatalf("manager list = %d body=%s", mgrPage.Code, mgrPage.Body.String())
	}
	mbody := mgrPage.Body.String()
	for _, want := range []string{`data-label="Health"`, `data-label="Sources"`, `data-label="Access"`, `data-action="knowledge:new"`, "Everyone"} {
		if !strings.Contains(mbody, want) {
			t.Fatalf("manager list missing %q", want)
		}
	}
}

// TestDashboardKnowledgeCollectionTabs covers the per-capability tab bar:
// a member sees Documents + Search only — the Search tab emits exactly the
// shared fragment URL with the server-set collection id; a manager also
// sees Sources + Settings.
func TestDashboardKnowledgeCollectionTabs(t *testing.T) {
	f := newKnowledgeFixture(t)
	sess, csrf, _ := f.member(t, "know-tabs-member@example.test")
	col := f.collection(t, "Policies", []string{"*"})
	mgr, mgrCSRF := f.manager(t, "know-tabs-manager@example.test")
	f.stageDoc(t, col, "obj_tab", "Policy.pdf")
	f.stageDoc(t, col, "obj_tab2", "Handbook.pdf")

	base := "/dashboard/knowledge/" + col.ID
	page := f.get(t, sess, csrf, base)
	if page.Code != http.StatusOK {
		t.Fatalf("member collection page = %d body=%s", page.Code, page.Body.String())
	}
	body := page.Body.String()
	for _, want := range []string{"Documents", "Search", "Policy.pdf", `data-label="Document"`} {
		if !strings.Contains(body, want) {
			t.Fatalf("member collection page missing %q", want)
		}
	}
	for _, banned := range []string{base + "/sources", base + "/settings", "knowledge:retry", "knowledge-upload-file", `data-label="Actions"`} {
		if strings.Contains(body, banned) {
			t.Fatalf("member collection page must not contain %q", banned)
		}
	}
	// Non-terminal (processing) row → the fragment carries the 3s poll.
	if !strings.Contains(body, "every 3s") {
		t.Fatal("processing document must carry the 3s refresh trigger")
	}

	search := f.get(t, sess, csrf, base+"/search")
	if search.Code != http.StatusOK {
		t.Fatalf("member search tab = %d body=%s", search.Code, search.Body.String())
	}
	if !strings.Contains(search.Body.String(), `hx-get="/dashboard/partials/search/results?collection=`+col.ID+`"`) {
		t.Fatalf("search tab must emit the shared fragment URL; body=%s", search.Body.String())
	}

	mgrPage := f.get(t, mgr, mgrCSRF, base)
	if mgrPage.Code != http.StatusOK {
		t.Fatalf("manager collection page = %d body=%s", mgrPage.Code, mgrPage.Body.String())
	}
	mbody := mgrPage.Body.String()
	for _, want := range []string{base + "/sources", base + "/settings", "Read-only", "Ask a knowledge manager to grant write access"} {
		if !strings.Contains(mbody, want) {
			t.Fatalf("manager collection page missing %q", want)
		}
	}
	for _, banned := range []string{"knowledge:retry", "knowledge-upload-file", `data-label="Actions"`} {
		if strings.Contains(mbody, banned) {
			t.Fatalf("read-only manager collection page contains %q", banned)
		}
	}
	partial := f.get(t, mgr, mgrCSRF, "/dashboard/partials/knowledge/documents?collection="+col.ID)
	if partial.Code != http.StatusOK || !strings.Contains(partial.Body.String(), "Read-only") || strings.Contains(partial.Body.String(), "knowledge-upload-file") {
		t.Fatalf("read-only manager fragment = %d body=%s", partial.Code, partial.Body.String())
	}
	for _, path := range []string{base + "/sources", base + "/settings"} {
		if got := f.get(t, mgr, mgrCSRF, path); got.Code != http.StatusOK {
			t.Fatalf("manager %s = %d, want 200", path, got.Code)
		}
	}
}

// TestDashboardKnowledgeMemberDenials covers the member 403 pages on the
// manager tabs and the profiles page — the page-variant 403, not a redirect.
func TestDashboardKnowledgeMemberDenials(t *testing.T) {
	f := newKnowledgeFixture(t)
	sess, csrf, memberID := f.member(t, "know-deny@example.test")
	col := f.collection(t, "Research", []string{memberID})
	base := "/dashboard/knowledge/" + col.ID
	for _, path := range []string{"/dashboard/knowledge/profiles", base + "/sources", base + "/settings"} {
		got := f.get(t, sess, csrf, path)
		if got.Code != http.StatusForbidden {
			t.Fatalf("member %s = %d, want 403", path, got.Code)
		}
	}
	// The profiles page itself renders for a manager.
	mgr, mgrCSRF := f.manager(t, "know-prof-mgr@example.test")
	if got := f.get(t, mgr, mgrCSRF, "/dashboard/knowledge/profiles"); got.Code != http.StatusOK {
		t.Fatalf("manager profiles = %d body=%s", got.Code, got.Body.String())
	} else if !strings.Contains(got.Body.String(), "document-preview-form") {
		t.Fatal("profiles page must carry the document-preview tool")
	}
}

// TestDashboardKnowledgeChunkingControls: the New-collection drawer and the
// settings form carry the chunking select (chunk_strategy) that knowledge.js
// posts — Adaptive by default on create, the current value on edit.
func TestDashboardKnowledgeChunkingControls(t *testing.T) {
	f := newKnowledgeFixture(t)
	mgr, mgrCSRF := f.manager(t, "know-chunking@example.test")

	list := f.get(t, mgr, mgrCSRF, "/dashboard/knowledge")
	if list.Code != http.StatusOK {
		t.Fatalf("manager list = %d body=%s", list.Code, list.Body.String())
	}
	for _, want := range []string{
		`name="chunk_strategy"`,
		`<option value="auto" selected>Adaptive (recommended)</option>`,
		`<option value="fixed">Fixed size</option>`,
		"Adaptive splits by headings, paragraphs and sentences. Fixed cuts at an exact length.",
	} {
		if !strings.Contains(list.Body.String(), want) {
			t.Fatalf("create drawer missing %q", want)
		}
	}

	col := f.collection(t, "Chunked", []string{"*"})
	page := f.get(t, mgr, mgrCSRF, "/dashboard/knowledge/"+col.ID+"/settings")
	if page.Code != http.StatusOK {
		t.Fatalf("settings = %d body=%s", page.Code, page.Body.String())
	}
	sbody := page.Body.String()
	if !strings.Contains(sbody, `name="chunk_strategy"`) || !strings.Contains(sbody, `<option value="auto" selected>Adaptive (recommended)</option>`) {
		t.Fatalf("settings form missing the chunking select with the current value:\n%s", sbody)
	}
}

// TestDashboardKnowledgeMemberDocumentRows covers collection-level row
// privacy: the member lists every document of a collection shared with
// them, and a collection they cannot open never leaks its titles.
func TestDashboardKnowledgeMemberDocumentRows(t *testing.T) {
	f := newKnowledgeFixture(t)
	sess, csrf, memberID := f.member(t, "know-rows@example.test")
	col := f.collection(t, "Shared", []string{memberID})
	f.stageDoc(t, col, "obj_shared", "Shared-Report.pdf")
	secret := f.collection(t, "Secret", []string{"other_actor"})
	f.stageDoc(t, secret, "obj_secret", "Secret-Dossier.pdf")

	page := f.get(t, sess, csrf, "/dashboard/knowledge/"+col.ID+"/documents")
	if page.Code != http.StatusOK || !strings.Contains(page.Body.String(), "Shared-Report.pdf") {
		t.Fatalf("member documents = %d body=%s", page.Code, page.Body.String())
	}
	partial := f.get(t, sess, csrf, "/dashboard/partials/knowledge/documents?collection="+col.ID)
	if partial.Code != http.StatusOK || !strings.Contains(partial.Body.String(), "Shared-Report.pdf") {
		t.Fatalf("documents partial = %d body=%s", partial.Code, partial.Body.String())
	}
	for _, path := range []string{"/dashboard/knowledge/" + secret.ID + "/documents", "/dashboard/partials/knowledge/documents?collection=" + secret.ID} {
		if got := f.get(t, sess, csrf, path); got.Code == http.StatusOK || strings.Contains(got.Body.String(), "Secret-Dossier.pdf") {
			t.Fatalf("unshared collection %s = %d body=%s", path, got.Code, got.Body.String())
		}
	}
}

func TestDashboardKnowledgeReembedProgressAndWarnings(t *testing.T) {
	f := newKnowledgeFixture(t)
	sess, csrf, memberID := f.member(t, "know-progress@example.test")
	col := f.collection(t, "Progress", []string{"*"})
	f.stageDoc(t, col, "obj_progress", "Report.txt")

	ctx := context.Background()
	if _, err := f.db.SQL.ExecContext(ctx, `
		UPDATE knowledge_collections SET owner_actor_id=$3 WHERE tenant_id=$1 AND id=$2`,
		f.tenant, col.ID, memberID); err != nil {
		t.Fatalf("set collection owner: %v", err)
	}
	if _, err := f.db.SQL.ExecContext(ctx, `
		INSERT INTO index_generations
			(tenant_id, id, collection_id, object_version_id, generation, state, qdrant_collection, warnings)
		SELECT $1, 'gen_obj_progress', $2, id, 1, 'published', 'old_index', '["Truncated <script>alert(1)</script>"]'::jsonb
		FROM source_object_versions WHERE tenant_id=$1 AND object_id='obj_progress'`,
		f.tenant, col.ID); err != nil {
		t.Fatalf("insert generation: %v", err)
	}

	base := "/dashboard/knowledge/" + col.ID
	for _, path := range []string{base, base + "/search", "/dashboard/partials/knowledge/documents?collection=" + col.ID} {
		got := f.get(t, sess, csrf, path)
		body := got.Body.String()
		if got.Code != http.StatusOK || !strings.Contains(body, "Re-embedding under new settings: 1 of 1 documents left") || !strings.Contains(body, `hx-select="#knowledge-reembed-progress"`) {
			t.Fatalf("pending progress at %s = %d body=%s", path, got.Code, body)
		}
		if path == base && (!strings.Contains(body, "Truncated &lt;script&gt;alert(1)&lt;/script&gt;") || strings.Contains(body, "Truncated <script>") || !strings.Contains(body, "Needs attention") || !strings.Contains(body, `data-label="Status"`)) {
			t.Fatalf("warning not safely rendered: %s", body)
		}
	}
	other, otherCSRF, _ := f.member(t, "know-progress-reader@example.test")
	reader := f.get(t, other, otherCSRF, base)
	if reader.Code != http.StatusOK || strings.Contains(reader.Body.String(), "Re-embedding under new settings:") {
		t.Fatalf("non-owner progress = %d body=%s", reader.Code, reader.Body.String())
	}

	if _, err := f.db.SQL.ExecContext(ctx, `
		UPDATE index_generations g
		SET qdrant_collection=c.qdrant_collection, embed_titles=c.embed_titles, passage_prefix=c.passage_prefix
		FROM knowledge_collections c
		WHERE g.tenant_id=$1 AND g.collection_id=$2 AND c.tenant_id=g.tenant_id AND c.id=g.collection_id`,
		f.tenant, col.ID); err != nil {
		t.Fatalf("complete re-embedding: %v", err)
	}
	for _, path := range []string{base, "/dashboard/partials/knowledge/documents?collection=" + col.ID} {
		got := f.get(t, sess, csrf, path)
		if got.Code != http.StatusOK || strings.Contains(got.Body.String(), "Re-embedding under new settings:") {
			t.Fatalf("completed progress at %s = %d body=%s", path, got.Code, got.Body.String())
		}
	}
}
