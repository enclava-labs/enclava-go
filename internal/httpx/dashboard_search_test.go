// dashboard_search_test.go — the Search page suite (plan 10-03): URL-state
// form, the shared results fragment (sole §6.4 renderer), union fusion over
// readable collections, post-authorization facet privacy, the three modes
// (semantic asserted by executor invocation, never skipped), escape-then-mark
// snippets and the new-surface guards. Members read through real ACL
// audiences; a grantless member sees the honest empty state — never an
// admin/wildcard stand-in.
package httpx

import (
	"context"
	"encoding/json"
	"io"
	"net/http"
	"net/http/httptest"
	"strings"
	"testing"

	"enclava-go/internal/auth"
	"enclava-go/internal/inference"
	"enclava-go/internal/jobs"
	"enclava-go/internal/rag"
	"enclava-go/internal/source"
	"enclava-go/internal/store/postgres"
)

// searchStubStore answers vector queries from staged points and records the
// mode each Query call ran under — the proof a semantic search executed the
// real semantic path.
type searchStubStore struct {
	points map[string][]rag.QdrantSearchPoint
	modes  []rag.QueryMode
}

func (s *searchStubStore) EnsureCollection(ctx context.Context, collection string, vectorSize int) error {
	return nil
}
func (s *searchStubStore) UpsertPoints(ctx context.Context, collection string, points []rag.QdrantPoint) error {
	return nil
}
func (s *searchStubStore) DeleteDocumentPoints(ctx context.Context, collection, documentID string) error {
	return nil
}
func (s *searchStubStore) DeleteGenerationPoints(ctx context.Context, collection, versionID string, generation int64) error {
	return nil
}
func (s *searchStubStore) SetPayload(ctx context.Context, collection string, filter map[string]any, payload map[string]any) error {
	return nil
}
func (s *searchStubStore) DeleteCollection(ctx context.Context, collection string) error {
	return nil
}
func (s *searchStubStore) SearchHybrid(ctx context.Context, collection string, vector []float64, sparse rag.SparseVector, limit int, filter map[string]any) ([]rag.QdrantSearchPoint, error) {
	return s.points[collection], nil
}
func (s *searchStubStore) Query(ctx context.Context, collection string, in rag.QueryInput) ([]rag.QdrantSearchPoint, error) {
	s.modes = append(s.modes, in.Mode)
	points := s.points[collection]
	if len(points) > in.Limit {
		points = points[:in.Limit]
	}
	return points, nil
}

// recordingExecutor counts priced embedding dispatches and answers a
// deterministic embedding response — lexical searches must never reach it.
type recordingExecutor struct{ calls int }

func (e *recordingExecutor) Execute(ctx context.Context, p auth.Principal, req inference.Request) (*inference.Result, error) {
	e.calls++
	var payload struct {
		Input any `json:"input"`
	}
	if err := json.Unmarshal(req.Payload, &payload); err != nil {
		return nil, err
	}
	inputs := []string{}
	switch v := payload.Input.(type) {
	case string:
		inputs = append(inputs, v)
	case []any:
		for _, item := range v {
			if s, ok := item.(string); ok {
				inputs = append(inputs, s)
			}
		}
	}
	data := make([]map[string]any, 0, len(inputs))
	for i := range inputs {
		data = append(data, map[string]any{"index": i, "embedding": []float64{1, 1, 1}})
	}
	body, err := json.Marshal(map[string]any{
		"object": "list", "data": data,
		"usage": map[string]any{"prompt_tokens": len(inputs), "total_tokens": len(inputs)},
	})
	if err != nil {
		return nil, err
	}
	return &inference.Result{Body: io.NopCloser(strings.NewReader(string(body)))}, nil
}

// searchFixture is the search-suite app: real rag service over an isolated
// schema with the stub vector store + recording executor, sessions logged in
// through the real browser form.
type searchFixture struct {
	*identityFixture
	svc    *rag.Service
	vector *searchStubStore
	exec   *recordingExecutor
	adminP auth.Principal
}

func newSearchFixture(t *testing.T) *searchFixture {
	f := &searchFixture{identityFixture: newIdentityFixture(t)}
	f.vector = &searchStubStore{points: map[string][]rag.QdrantSearchPoint{}}
	f.exec = &recordingExecutor{}
	f.svc = rag.NewService(postgres.NewRAGRepo(f.db), f.vector, f.exec, "embed-test", nil,
		jobs.NewService(postgres.NewJobsRepo(f.db), 0))
	f.app.ragSvc = f.svc
	f.adminP = auth.Principal{
		TenantID: f.tenant, ActorID: "search_admin", UserID: "search_admin",
		ActorKind:           auth.ActorKindServiceAccount,
		Scopes:              map[string]struct{}{auth.ScopePlatformAdmin: {}},
		AllowedModelsAll:    true,
		AllowedChatbotsAll:  true,
		AllowedEndpointsAll: true,
	}
	return f
}

// member logs a scoped user in through the login form and returns the session
// cookies plus the actor id audience entries must carry.
func (f *searchFixture) member(t *testing.T, email string) (session, csrf *http.Cookie, memberID string) {
	t.Helper()
	session, csrf = loginUserWithGrants(t, f.identityFixture, email, []string{"rag"}, auth.ResourceGrants{}, "search-pass")
	if err := f.db.SQL.QueryRowContext(context.Background(),
		`SELECT id::text FROM users WHERE tenant_id = $1::uuid AND email = $2`,
		f.tenant, email).Scan(&memberID); err != nil {
		t.Fatalf("read member id: %v", err)
	}
	return session, csrf, memberID
}

// collection creates a collection whose stored audience is exactly `audience`
// actor ids (an empty list stays owner-only — the restricted fixture).
func (f *searchFixture) collection(t *testing.T, name string, audience []string) rag.Collection {
	t.Helper()
	c, err := f.svc.CreateCollection(context.Background(), f.adminP, rag.CreateCollectionInput{
		Name: name, ChunkSize: 200, ChunkOverlap: 20,
		ACL: map[string]any{"audience": audience},
	})
	if err != nil {
		t.Fatalf("create collection %s: %v", name, err)
	}
	return c
}

// stageSearchDoc persists a collection-mode object + published generation
// with one span-backed chunk and registers its point — the same real
// retained-chunk shape the JSON search route seeds.
func (f *searchFixture) stageSearchDoc(t *testing.T, collection rag.Collection, objectID, fileName, content, provenance string, audience []string) {
	t.Helper()
	ctx := context.Background()
	repo := postgres.NewRAGRepo(f.db)
	if _, err := repo.UpsertObject(ctx, source.Object{
		TenantID: f.tenant, ID: objectID, Kind: "document", NativeID: "native-" + objectID,
		AuthorizationMode: source.AuthorizationCollection, OwnerActorID: audience[0],
	}); err != nil {
		t.Fatal(err)
	}
	if err := repo.InsertObjectVersion(ctx, f.tenant, source.ObjectVersion{
		ObjectID: objectID, ObservedSeq: 1, ArtifactRef: "art_" + objectID, SHA256: "sha_" + objectID,
		ByteSize: int64(len(content)), MimeType: "text/plain", OriginalFilename: fileName,
		Provenance: provenance,
	}); err != nil {
		t.Fatal(err)
	}
	if err := repo.AddCollectionItem(ctx, f.tenant, collection.ID, objectID, audience[0]); err != nil {
		t.Fatal(err)
	}
	parsed := source.Document{
		Ref: source.ObjectRef{TenantID: f.tenant, ObjectID: objectID, ObservedSeq: 1},
		Blocks: []source.Block{
			{ID: "blk_" + objectID, Ordinal: 0, Kind: "paragraph", Text: content, ByteStart: 0, ByteEnd: len(content),
				Locator: source.Locator{Page: new(2)}},
		},
	}
	if err := repo.SaveParsedDocument(ctx, "rag-structured-v1", parsed); err != nil {
		t.Fatal(err)
	}
	profileID, err := repo.EnsureChunkProfile(ctx, f.tenant, "rag-"+collection.ID+"-structured", 200, 20)
	if err != nil {
		t.Fatal(err)
	}
	chunkID := "chk_" + objectID
	if err := repo.ReplaceVersionChunksAndStage(ctx, f.tenant, collection.ID, objectID, objectID+"#1", 1, 1, profileID, []rag.DocumentChunk{{
		ID: chunkID, DocumentID: objectID, CollectionID: collection.ID, Generation: 1,
		ChunkIndex: 0, Content: content, TokenCount: 4, HeaderPath: "Section",
		SpanRefs: []source.Span{{Ref: parsed.Ref, BlockID: "blk_" + objectID, ByteStart: 0, ByteEnd: 5}},
	}}); err != nil {
		t.Fatal(err)
	}
	if err := repo.MarkGenerationState(ctx, f.tenant, collection.ID, objectID+"#1", 1, "published"); err != nil {
		t.Fatal(err)
	}
	f.vector.points[collection.QdrantCollection] = append(f.vector.points[collection.QdrantCollection], rag.QdrantSearchPoint{
		ID: "pt_" + objectID, Score: 1, Payload: map[string]any{
			"tenant_id": f.tenant, "document_id": objectID, "object_version_id": objectID + "#1",
			"generation": int64(1), "chunk_id": chunkID, "file_name": fileName,
			"source_uri": "urn:test:" + objectID, "content": content, "access_enabled": true,
		},
	})
}

func (f *searchFixture) get(t *testing.T, session, csrf *http.Cookie, path string) *httptest.ResponseRecorder {
	t.Helper()
	return sessionDo(t, f.app, http.MethodGet, path, "", session, csrf)
}

// TestDashboardSearchPageAndPartial covers plan Test 1 + the row-markup rules
// of Test 4: empty query renders recents + collection cards, the fragment
// renders real rows, and a session without the rag scope gets the page 403
// and the empty-403 partial.
func TestDashboardSearchPageAndPartial(t *testing.T) {
	f := newSearchFixture(t)
	sess, csrf, memberID := f.member(t, "search-member@example.test")
	col := f.collection(t, "Contracts", []string{memberID})
	f.stageSearchDoc(t, col, "obj_a", "Agreement.pdf", "the renewal contract clause", `{"kind":"upload"}`, []string{memberID})

	page := f.get(t, sess, csrf, "/dashboard/search")
	if page.Code != http.StatusOK {
		t.Fatalf("search page = %d body=%s", page.Code, page.Body.String())
	}
	body := page.Body.String()
	for _, want := range []string{`id="searchForm"`, `data-recents-key`, `id="searchResults"`, "Contracts", `id="searchRecents"`, `data-mode="hybrid"`} {
		if !strings.Contains(body, want) {
			t.Fatalf("empty-query page missing %q", want)
		}
	}
	if strings.Contains(body, `class="result"`) {
		t.Fatal("empty query must not render result rows")
	}

	partial := f.get(t, sess, csrf, "/dashboard/partials/search/results?q=renewal&mode=hybrid")
	if partial.Code != http.StatusOK || !strings.Contains(partial.Body.String(), `class="result"`) || !strings.Contains(partial.Body.String(), "Agreement.pdf") {
		t.Fatalf("results partial = %d body=%s", partial.Code, partial.Body.String())
	}

	// A session without the rag scope: page 403, empty 403 partial.
	noScope, noCSRF := loginUser(t, f.identityFixture, "search-noscope@example.test", []string{"chatbot"}, "search-pass")
	if got := f.get(t, noScope, noCSRF, "/dashboard/search"); got.Code != http.StatusForbidden {
		t.Fatalf("page without rag scope = %d, want 403", got.Code)
	}
	denied := f.get(t, noScope, noCSRF, "/dashboard/partials/search/results?q=renewal")
	if denied.Code != http.StatusForbidden || strings.Contains(denied.Body.String(), "Agreement.pdf") {
		t.Fatalf("partial without rag scope = %d body=%s", denied.Code, denied.Body.String())
	}
}

// TestDashboardSearchFusionAndACLPrivacy covers plan Test 2: two readable
// collections contribute fused rows, the restricted collection contributes
// neither rows nor facet chips/counts, collection= scopes to the 09-01
// contract, a foreign collection id is indistinguishable from empty, and a
// member with zero readable collections sees the honest empty state.
func TestDashboardSearchFusionAndACLPrivacy(t *testing.T) {
	f := newSearchFixture(t)
	sess, csrf, memberID := f.member(t, "search-fusion@example.test")
	colA := f.collection(t, "Alpha", []string{memberID})
	colB := f.collection(t, "Beta", []string{memberID})
	outsider := "actor_outsider"
	secret := f.collection(t, "Secret", []string{outsider})
	f.stageSearchDoc(t, colA, "obj_aa", "Alpha Doc.pdf", "shared corpus alpha", `{"kind":"upload"}`, []string{memberID})
	f.stageSearchDoc(t, colB, "obj_bb", "Beta Doc.pdf", "shared corpus beta", `{"source_kind":"notion"}`, []string{memberID})
	f.stageSearchDoc(t, secret, "obj_ss", "Secret Doc.pdf", "shared corpus secret", `{"source_kind":"vault"}`, []string{outsider})

	res := f.get(t, sess, csrf, "/dashboard/partials/search/results?q=corpus&mode=lexical")
	if res.Code != http.StatusOK {
		t.Fatalf("fused search = %d body=%s", res.Code, res.Body.String())
	}
	body := res.Body.String()
	if !strings.Contains(body, "Alpha Doc.pdf") || !strings.Contains(body, "Beta Doc.pdf") {
		t.Fatalf("union fusion must carry rows from both readable collections: %s", body)
	}
	if strings.Contains(body, "Secret") || strings.Contains(body, `data-facet="vault"`) {
		t.Fatalf("restricted collection leaked a row or a facet: %s", body)
	}
	if !strings.Contains(body, `data-facet="upload"`) || !strings.Contains(body, `data-facet="notion"`) {
		t.Fatalf("post-authorization facet chips missing: %s", body)
	}

	// The 09-01 shape: collection= scopes the same fragment.
	scoped := f.get(t, sess, csrf, "/dashboard/partials/search/results?q=corpus&mode=lexical&collection="+colA.ID)
	if !strings.Contains(scoped.Body.String(), "Alpha Doc.pdf") || strings.Contains(scoped.Body.String(), "Beta Doc.pdf") {
		t.Fatalf("collection-scoped fragment wrong: %s", scoped.Body.String())
	}
	// A denied or unknown id is the same public outcome as an empty result set.
	for _, id := range []string{secret.ID, "col_foreign"} {
		denied := f.get(t, sess, csrf, "/dashboard/partials/search/results?q=corpus&collection="+id)
		if denied.Code != http.StatusOK || strings.Contains(denied.Body.String(), `class="result"`) || strings.Contains(denied.Body.String(), "Secret") {
			t.Fatalf("foreign collection %s = %d body=%s", id, denied.Code, denied.Body.String())
		}
	}

	// A member with no readable anything: honest empty, never leaks.
	lonely, lonelyCSRF, _ := f.member(t, "search-lonely@example.test")
	page := f.get(t, lonely, lonelyCSRF, "/dashboard/search")
	if page.Code != http.StatusOK || !strings.Contains(page.Body.String(), "No collections you can search yet") {
		t.Fatalf("grantless member empty page = %d body=%s", page.Code, page.Body.String())
	}
	empty := f.get(t, lonely, lonelyCSRF, "/dashboard/partials/search/results?q=corpus")
	if strings.Contains(empty.Body.String(), "corpus") && strings.Contains(empty.Body.String(), `class="result"`) {
		t.Fatalf("grantless member leaked results: %s", empty.Body.String())
	}
}

// TestDashboardSearchModes covers plan Test 3: lexical runs without the
// executor, semantic executes the real semantic path (executor invoked +
// mode recorded), and an invalid mode falls back to hybrid.
func TestDashboardSearchModes(t *testing.T) {
	f := newSearchFixture(t)
	sess, csrf, memberID := f.member(t, "search-modes@example.test")
	col := f.collection(t, "Modes", []string{memberID})
	f.stageSearchDoc(t, col, "obj_m", "Modes.pdf", "semantic benefit vector content", `{"kind":"upload"}`, []string{memberID})

	before := f.exec.calls
	lexical := f.get(t, sess, csrf, "/dashboard/partials/search/results?q=benefit&mode=lexical")
	if lexical.Code != http.StatusOK || !strings.Contains(lexical.Body.String(), "Modes.pdf") {
		t.Fatalf("lexical search = %d body=%s", lexical.Code, lexical.Body.String())
	}
	if f.exec.calls != before {
		t.Fatalf("lexical-only search invoked the embedding executor (%d→%d)", before, f.exec.calls)
	}

	semantic := f.get(t, sess, csrf, "/dashboard/partials/search/results?q=advantage&mode=semantic")
	if semantic.Code != http.StatusOK || !strings.Contains(semantic.Body.String(), "Modes.pdf") {
		t.Fatalf("semantic search = %d body=%s", semantic.Code, semantic.Body.String())
	}
	if f.exec.calls == before {
		t.Fatal("semantic mode never invoked the embedding executor — the semantic path did not run")
	}
	found := false
	for _, m := range f.vector.modes {
		if m == rag.QuerySemantic {
			found = true
		}
	}
	if !found {
		t.Fatalf("vector store never saw a semantic query: %#v", f.vector.modes)
	}

	// Invalid mode falls back to hybrid — never an error, never widened.
	fallback := f.get(t, sess, csrf, "/dashboard/partials/search/results?q=advantage&mode=bogus")
	if fallback.Code != http.StatusOK || !strings.Contains(fallback.Body.String(), "Modes.pdf") {
		t.Fatalf("invalid mode = %d body=%s", fallback.Code, fallback.Body.String())
	}
	page := f.get(t, sess, csrf, "/dashboard/search?q=advantage&mode=bogus")
	if !strings.Contains(page.Body.String(), `value="hybrid"`) {
		t.Fatal("invalid mode must render the hybrid segmented selection")
	}
}

// TestDashboardSearchResultMarkup covers plan Tests 4–5: result rows carry
// the title/location/marked-snippet/kind/evidence/ask-about hooks, and mark
// escapes stored content before highlighting. Page and partial emit the same
// markup — one template.
func TestDashboardSearchResultMarkup(t *testing.T) {
	f := newSearchFixture(t)
	sess, csrf, memberID := f.member(t, "search-markup@example.test")
	col := f.collection(t, "Markup", []string{memberID})
	f.stageSearchDoc(t, col, "obj_x", "Agreement.pdf", "the <script>alert(1)</script> renewal clause", `{"kind":"upload"}`, []string{memberID})

	partial := f.get(t, sess, csrf, "/dashboard/partials/search/results?q=renewal&mode=lexical")
	body := partial.Body.String()
	for _, want := range []string{
		`class="result"`, `result-title`, "Agreement.pdf", `result-meta`, "Page 2",
		`result-snippet`, "<mark>renewal</mark>", `data-source-kind="upload"`,
		`data-action="evidence:open"`, `data-evidence-id=`,
		`data-action="search:ask-about"`, "/dashboard/chat?evidence=",
	} {
		if !strings.Contains(body, want) {
			t.Fatalf("result row missing %q:\n%s", want, body)
		}
	}
	if strings.Contains(body, "<script>alert") || !strings.Contains(body, "&lt;script&gt;") {
		t.Fatalf("stored markup must render escaped: %s", body)
	}

	page := f.get(t, sess, csrf, "/dashboard/search?q=renewal&mode=lexical")
	if !strings.Contains(page.Body.String(), `data-evidence-id=`) || !strings.Contains(page.Body.String(), "<mark>renewal</mark>") {
		t.Fatal("deep-linked page must embed the identical result markup")
	}
}

// TestDashboardSearchGuards covers plan Test 9's new-surface half: unknown
// subpaths fall through to the designed 404 (the old-surface teardown rides
// 10-01's commit, never this plan).
func TestDashboardSearchGuards(t *testing.T) {
	f := newSearchFixture(t)
	sess, csrf, _ := f.member(t, "search-guards@example.test")
	if got := f.get(t, sess, csrf, "/dashboard/search/extra"); got.Code != http.StatusNotFound {
		t.Fatalf("/dashboard/search/extra = %d, want 404", got.Code)
	}
}

func TestDashboardSearchEmbeddingDenialInline(t *testing.T) {
	f := newSearchFixture(t)
	sess, csrf, memberID := f.member(t, "search-no-embedding@example.test")
	col := f.collection(t, "Contracts", []string{memberID})
	f.stageSearchDoc(t, col, "obj_denied", "Agreement.pdf", "renewal contract", `{"kind":"upload"}`, []string{memberID})
	f.app.ragSvc = rag.NewService(postgres.NewRAGRepo(f.db), f.vector, deniedEmbeddingExecutor{}, "embed-test", nil, nil)
	for _, path := range []string{"/dashboard/search", "/dashboard/partials/search/results"} {
		got := f.get(t, sess, csrf, path+"?q=renewal&mode=semantic")
		if got.Code != http.StatusOK || !strings.Contains(got.Body.String(), "Semantic search needs an embedding model granted to your role") || !strings.Contains(got.Body.String(), "Keyword search still works.") {
			t.Fatalf("%s = %d: %s", path, got.Code, got.Body.String())
		}
	}
}
