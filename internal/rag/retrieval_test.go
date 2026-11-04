package rag

import (
	"context"
	"encoding/json"
	"errors"
	"fmt"
	"io"
	"net/http"
	"net/http/httptest"
	"reflect"
	"strings"
	"testing"
	"time"

	"enclava-go/internal/auth"
	"enclava-go/internal/inference"
	"enclava-go/internal/source"
)

func TestQueryWireModesAndFilters(t *testing.T) {
	var request map[string]any
	server := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		request = nil
		if err := json.NewDecoder(r.Body).Decode(&request); err != nil {
			t.Fatal(err)
		}
		_, _ = w.Write([]byte(`{"result":{"points":[]}}`))
	}))
	defer server.Close()
	filter := AccessFilter("tenant-1", []string{"tenant", "actor:a"}, nil)
	client := NewQdrantClient(server.URL, 0)
	if _, err := client.Query(context.Background(), "c", QueryInput{Mode: QueryHybrid, DenseVector: []float64{1, 2}, LexicalText: "hello", Filter: filter, Limit: 3}); err != nil {
		t.Fatal(err)
	}
	prefetch := request["prefetch"].([]any)
	if len(prefetch) != 2 || !reflect.DeepEqual(prefetch[0].(map[string]any)["filter"], prefetch[1].(map[string]any)["filter"]) || request["query"].(map[string]any)["fusion"] != "rrf" {
		t.Fatalf("unexpected hybrid body: %#v", request)
	}
	clauses := prefetch[0].(map[string]any)["filter"].(map[string]any)["must"].([]any)
	if clauses[0].(map[string]any)["key"] != "tenant_id" || clauses[1].(map[string]any)["key"] != "access_enabled" {
		t.Fatalf("missing server-scoped filter: %#v", clauses)
	}
	aclShould := clauses[2].(map[string]any)["should"].([]any)
	if aclShould[0].(map[string]any)["key"] != "acl" || aclShould[1].(map[string]any)["is_empty"] == nil {
		t.Fatalf("missing acl token/unprojected clauses: %#v", aclShould)
	}
	pubShould := clauses[3].(map[string]any)["should"].([]any)
	if pubShould[0].(map[string]any)["key"] != "published" || pubShould[1].(map[string]any)["is_empty"] == nil {
		t.Fatalf("missing published/unprojected clauses: %#v", pubShould)
	}
	if _, ok := prefetch[1].(map[string]any)["query"].(map[string]any); !ok {
		t.Fatalf("missing native BM25 text query: %#v", prefetch[1])
	}

	if _, err := client.Query(context.Background(), "c", QueryInput{Mode: QueryLexical, LexicalText: "hello", Filter: filter}); err != nil {
		t.Fatal(err)
	}
	if _, hasPrefetch := request["prefetch"]; hasPrefetch {
		t.Fatalf("lexical-only query must not prefetch/fuse: %#v", request)
	}
	if request["using"] != "text" || request["query"].(map[string]any)["model"] != "qdrant/bm25" {
		t.Fatalf("unexpected lexical query: %#v", request)
	}
}

func TestAccessFilterCannotOmitServerScope(t *testing.T) {
	filter := AccessFilter("tenant-1", []string{"tenant"}, map[string]any{"key": "kind", "match": map[string]any{"value": "file"}})
	must := filter["must"].([]any)
	if len(must) != 5 {
		t.Fatalf("expected tenant, access, acl, published and narrowing clauses, got %#v", must)
	}
	if must[0].(map[string]any)["key"] != "tenant_id" || must[1].(map[string]any)["key"] != "access_enabled" || must[2].(map[string]any)["should"] == nil || must[3].(map[string]any)["should"] == nil {
		t.Fatalf("missing server scope: %#v", must)
	}
}

func TestBuildContextDeduplicatesExactSpansAndMarksDenied(t *testing.T) {
	principal := auth.Principal{TenantID: "t"}
	span := source.Span{Ref: source.ObjectRef{TenantID: "t", ObjectID: "o", ObservedSeq: 1}, BlockID: "b", ByteStart: 0, ByteEnd: 4}
	distinct := source.Span{Ref: span.Ref, BlockID: "b", ByteStart: 5, ByteEnd: 10}
	candidates := []SearchResult{
		{DocumentID: "o", Generation: 1, ChunkID: "c1", Spans: []source.Span{span}, Content: "clause"},
		{DocumentID: "o", Generation: 1, ChunkID: "c2", Spans: []source.Span{span}, Content: "clause"},
		{DocumentID: "o", Generation: 1, ChunkID: "exception", Spans: []source.Span{span, distinct}, Content: "distinct exception"},
		{DocumentID: "o", Generation: 2, ChunkID: "c3", Spans: []source.Span{span}, Content: "new generation"},
		{DocumentID: "denied", ChunkID: "c4", Content: "secret"},
	}
	got := BuildContext(context.Background(), principal, candidates, 100, denyContextObject("denied"))
	if !got.NeighborWithheld || !strings.Contains(got.Text, "[neighbor withheld]") || strings.Contains(got.Text, "secret") {
		t.Fatalf("denied object was not safely withheld: %#v", got)
	}
	if strings.Count(got.Text, "clause") != 1 || !strings.Contains(got.Text, "new generation") || !strings.Contains(got.Text, "distinct exception") {
		t.Fatalf("dedup must preserve distinct spans and generations: %#v", got)
	}
}

type denyContextObject string

// ---------------------------------------------------------------------
// 08-08 pure units: canonical source kinds, rank fusion, selection rules.

// TestSourceKindFallbackChain pins the canonical-kind resolution: provenance
// source_kind → connection kind → provenance kind → unknown, never
// object.Kind (often just "document").
func TestSourceKindFallbackChain(t *testing.T) {
	svc := &Service{}
	object := func(connectionID string) source.Object {
		return source.Object{TenantID: "t", ID: "o", Kind: "document", ConnectionID: connectionID}
	}
	cache := map[string]string{"conn_kind": "slack"}
	cases := []struct {
		name         string
		provenance   string
		connectionID string
		cache        map[string]string
		want         string
	}{
		{"provenance source_kind wins", `{"source_kind":"github"}`, "conn_kind", cache, "github"},
		{"connection kind next", `{}`, "conn_kind", cache, "slack"},
		{"provenance kind last", `{"kind":"generated"}`, "", cache, "generated"},
		{"unknown never object.Kind", `{}`, "", cache, "unknown"},
	}
	for _, tc := range cases {
		obj := object(tc.connectionID)
		ver := source.ObjectVersion{Provenance: tc.provenance}
		if got := svc.sourceKindFor(context.Background(), "t", obj, ver, tc.cache); got != tc.want {
			t.Fatalf("%s: sourceKindFor=%q want %q", tc.name, got, tc.want)
		}
	}
}

func TestSourceKindFacetsComposition(t *testing.T) {
	results := []SearchResult{
		{SourceKind: "notion"},
		{SourceKind: "upload"},
		{SourceKind: "notion"},
	}
	facets := sourceKindFacets(results)
	if len(facets) != 2 || facets[0] != (SourceFacet{SourceKind: "notion", Count: 2}) || facets[1] != (SourceFacet{SourceKind: "upload", Count: 1}) {
		t.Fatalf("facets must be deterministic and count the exact set: %#v", facets)
	}
}

func TestFuseRankedResultsDeterministic(t *testing.T) {
	makeList := func(ids ...string) rankedSearchList {
		results := make([]SearchResult, 0, len(ids))
		for _, id := range ids {
			results = append(results, SearchResult{DocumentID: id, ChunkID: id + "_chunk"})
		}
		return rankedSearchList{results: results}
	}
	first := fuseRankedResults([]rankedSearchList{makeList("a", "b"), makeList("c", "d")}, 10)
	second := fuseRankedResults([]rankedSearchList{makeList("b", "a"), makeList("d", "c")}, 10)
	if len(first) != 4 || len(second) != 4 {
		t.Fatalf("fused sizes %d/%d, want all four", len(first), len(second))
	}
	// Rank-0 entries win the fused score; equal contributions tie-break on
	// the stable result identity (collection, version, chunk), never on
	// list order — the pagination cursor needs a list-independent order.
	if first[0].DocumentID != "a" || second[0].DocumentID != "b" {
		t.Fatalf("top rank must be a rank-0 entry: %s / %s", first[0].DocumentID, second[0].DocumentID)
	}
	if first[1].DocumentID != "c" || first[2].DocumentID != "b" || second[1].DocumentID != "d" || second[2].DocumentID != "a" {
		t.Fatalf("tied entries must sort by stable identity: %#v / %#v", first, second)
	}
	if capped := fuseRankedResults([]rankedSearchList{makeList("a", "b", "c")}, 2); len(capped) != 2 {
		t.Fatalf("fused set must honor the limit, got %d", len(capped))
	}
}

func TestSelectReadableCollectionsRules(t *testing.T) {
	readable := []Collection{{ID: "a"}, {ID: "b"}}
	// Absent selection means every readable collection.
	selected, err := selectReadableCollections(readable, []string{" ", ""})
	if err != nil || len(selected) != 2 {
		t.Fatalf("absent selection = %#v err=%v", selected, err)
	}
	// Explicit unknown/unauthorized IDs are denied, never silently filtered.
	if _, err := selectReadableCollections(readable, []string{"zzz"}); !errors.Is(err, source.ErrDenied) {
		t.Fatalf("explicit unknown id = %v, want ErrDenied", err)
	}
	// Duplicate explicit IDs collapse before the cap.
	selected, err = selectReadableCollections(readable, []string{"a", "a"})
	if err != nil || len(selected) != 1 {
		t.Fatalf("deduped explicit selection = %#v err=%v", selected, err)
	}
	// Over the collection-count bound is an explicit error.
	many := make([]string, maxSearchCollections+1)
	for i := range many {
		many[i] = fmt.Sprintf("c%d", i)
	}
	if _, err := selectReadableCollections(readable, many); !errors.Is(err, ErrSearchWorkExceeded) {
		t.Fatalf("over-cap explicit ids = %v, want ErrSearchWorkExceeded", err)
	}
	wide := make([]Collection, maxSearchCollections+1)
	for i := range wide {
		wide[i] = Collection{ID: fmt.Sprintf("w%d", i)}
	}
	if _, err := selectReadableCollections(wide, nil); !errors.Is(err, ErrSearchWorkExceeded) {
		t.Fatalf("over-cap readable set = %v, want ErrSearchWorkExceeded", err)
	}
}

func (d denyContextObject) CanReadObject(_ context.Context, _ auth.Principal, id string) bool {
	return id != string(d)
}

type retrievalExecutor struct {
	requests  []inference.Request
	denyModel string
	onChat    func()
}

func (e *retrievalExecutor) Execute(_ context.Context, _ auth.Principal, req inference.Request) (*inference.Result, error) {
	e.requests = append(e.requests, req)
	if req.Model == e.denyModel && e.denyModel != "" {
		return nil, inference.ErrDenied
	}
	body := `{"data":[{"index":0,"embedding":[1,2]}]}`
	if req.Capability == inference.CapabilityChat {
		if e.onChat != nil {
			e.onChat()
		}
		body = `{"choices":[{"message":{"content":"rewritten"}}]}`
	}
	return &inference.Result{Body: io.NopCloser(strings.NewReader(body))}, nil
}

// retrievalVectors records QueryInputs; it returns no points because these
// tests assert binding/fan-out behavior, not hits.
type retrievalVectors struct {
	VectorStore
	queries []QueryInput
}

func (v *retrievalVectors) Query(_ context.Context, _ string, input QueryInput) ([]QdrantSearchPoint, error) {
	v.queries = append(v.queries, input)
	return nil, nil
}

// searchStubRepo supplies the aggregate reads searchCollection needs; any
// document-enumeration call would hit the nil embedded Repository and fail.
type searchStubRepo struct {
	Repository
	bindings    []IndexBinding
	connections []string
}

func (r *searchStubRepo) DistinctIndexBindings(_ context.Context, _, _ string) ([]IndexBinding, error) {
	return r.bindings, nil
}

func (r *searchStubRepo) ListCollectionConnectionIDs(_ context.Context, _, _ string) ([]string, error) {
	return r.connections, nil
}

func TestSearchCollectionPrefixesAndMemo(t *testing.T) {
	exec := &retrievalExecutor{}
	vectors := &retrievalVectors{}
	repo := &searchStubRepo{}
	svc := &Service{repo: repo, executor: exec, vectorStore: vectors, authorizer: source.NewAuthorizer(repo, 0)}
	memo := newQueryEmbeddingMemo()
	for i := range 3 {
		repo.bindings = []IndexBinding{{QdrantCollection: fmt.Sprint(i), EmbeddingModel: "model", EmbeddingDimensions: 2, QueryPrefix: "query: "}}
		if _, err := svc.searchCollection(context.Background(), auth.Principal{TenantID: "t"}, Collection{}, SearchInput{Query: "hello", Mode: QueryHybrid}, memo); err != nil {
			t.Fatal(err)
		}
	}
	if len(exec.requests) != 1 {
		t.Fatalf("embedding calls = %d, want 1", len(exec.requests))
	}
	var payload struct {
		Input []string `json:"input"`
	}
	if err := json.Unmarshal(exec.requests[0].Payload, &payload); err != nil || !reflect.DeepEqual(payload.Input, []string{"query: hello"}) {
		t.Fatalf("embedding input = %#v, %v", payload.Input, err)
	}
	if len(vectors.queries) != 3 {
		t.Fatalf("queries = %d", len(vectors.queries))
	}
	for _, query := range vectors.queries {
		if query.LexicalText != "hello" || len(query.DenseVector) != 2 {
			t.Fatalf("unexpected query: %#v", query)
		}
	}
}

func TestSearchUtilityModel(t *testing.T) {
	exec := &retrievalExecutor{}
	repo := &searchStubRepo{bindings: []IndexBinding{{QdrantCollection: "idx"}}}
	svc := &Service{repo: repo, executor: exec, vectorStore: &retrievalVectors{}, authorizer: source.NewAuthorizer(repo, 0)}
	input := SearchInput{Query: "hello", Mode: QueryLexical, Rewrite: true}
	if err := svc.searchModels(context.Background(), "t", &input); !errors.Is(err, ErrUtilityModelNotConfigured) {
		t.Fatalf("missing model: %v", err)
	}
	// Without UTILITY_MODEL the tenant default model from the Models page applies.
	svc.SetDefaultModelResolver(func(context.Context, string) (string, error) { return "tenant-default", nil })
	defaulted := SearchInput{Query: "hello", Mode: QueryLexical, Rewrite: true}
	if err := svc.searchModels(context.Background(), "t", &defaulted); err != nil || defaulted.RewriteModel != "tenant-default" {
		t.Fatalf("tenant default not used: model=%q err=%v", defaulted.RewriteModel, err)
	}
	svc.SetUtilityModel("utility")
	if err := svc.searchModels(context.Background(), "t", &input); err != nil {
		t.Fatal(err)
	}
	if _, err := svc.searchCollection(context.Background(), auth.Principal{TenantID: "t"}, Collection{}, input, newQueryEmbeddingMemo()); err != nil {
		t.Fatal(err)
	}
	if len(exec.requests) != 1 || exec.requests[0].Model != "utility" || exec.requests[0].Capability != inference.CapabilityChat {
		t.Fatalf("rewrite requests = %#v", exec.requests)
	}
}

func TestSearchCollectionSeparatesQueryPrefixes(t *testing.T) {
	exec := &retrievalExecutor{}
	vectors := &retrievalVectors{}
	repo := &searchStubRepo{bindings: []IndexBinding{
		{QdrantCollection: "index", EmbeddingModel: "model", EmbeddingDimensions: 2, QueryPrefix: "one: "},
		{QdrantCollection: "index", EmbeddingModel: "model", EmbeddingDimensions: 2, QueryPrefix: "two: "},
	}}
	svc := &Service{repo: repo, executor: exec, vectorStore: vectors, authorizer: source.NewAuthorizer(repo, 0)}
	_, err := svc.searchCollection(context.Background(), auth.Principal{TenantID: "t"}, Collection{}, SearchInput{Query: "hello", Mode: QueryHybrid}, newQueryEmbeddingMemo())
	if err != nil || len(exec.requests) != 2 || len(vectors.queries) != 2 {
		t.Fatalf("requests=%d queries=%d err=%v", len(exec.requests), len(vectors.queries), err)
	}
}

type reauthRepo struct {
	Repository
	collection Collection
	generation IndexGeneration
	object     source.Object
	version    source.ObjectVersion
	revoked    bool
	point      *QdrantSearchPoint
	bindings   []IndexBinding
	evidence   []EvidenceRecord
}

func (r *reauthRepo) GetCollection(_ context.Context, _ string, id string) (Collection, error) {
	if id != r.collection.ID {
		return Collection{}, fmt.Errorf("unknown collection %s", id)
	}
	return r.collection, nil
}

func (r *reauthRepo) ListCollections(_ context.Context, _ string) ([]Collection, error) {
	return []Collection{r.collection}, nil
}

func (r *reauthRepo) SaveEvidence(_ context.Context, record EvidenceRecord) error {
	r.evidence = append(r.evidence, record)
	return nil
}

func (r *reauthRepo) DistinctIndexBindings(_ context.Context, _, id string) ([]IndexBinding, error) {
	if r.bindings != nil {
		return r.bindings, nil
	}
	if id != r.collection.ID {
		return nil, nil
	}
	return []IndexBinding{{QdrantCollection: r.generation.QdrantCollection, EmbeddingModel: r.generation.EmbeddingModel}}, nil
}

func (r *reauthRepo) ListCollectionConnectionIDs(_ context.Context, _, _ string) ([]string, error) {
	return nil, nil
}

func (r *reauthRepo) PublishedGenerationForVersion(_ context.Context, _, collectionID, versionID string, generation int64) (bool, error) {
	return collectionID == r.collection.ID && versionID == r.generation.ObjectVersionID && generation == r.generation.Generation, nil
}

func (r *reauthRepo) GetObject(_ context.Context, _, id string) (source.Object, error) {
	if r.revoked {
		now := time.Now()
		obj := r.object
		obj.TombstonedAt = &now
		return obj, nil
	}
	return r.object, nil
}

func (r *reauthRepo) GetObjectVersion(_ context.Context, _, _ string, _ int64) (source.ObjectVersion, error) {
	return r.version, nil
}

func (r *reauthRepo) GetDocument(_ context.Context, _, _ string) (Document, error) {
	return Document{Status: DocumentStatusReady, ObjectVersionID: r.generation.ObjectVersionID}, nil
}

func (r *reauthRepo) ListGenerationChunks(_ context.Context, _, _, _ string, _ int64) ([]DocumentChunk, error) {
	return []DocumentChunk{{ID: "chunk-1", Content: "secret"}}, nil
}

type revokingVectors struct {
	VectorStore
	repo *reauthRepo
}

func (v *revokingVectors) Query(_ context.Context, _ string, _ QueryInput) ([]QdrantSearchPoint, error) {
	v.repo.revoked = true // revoke lands mid-search, after candidates were authorized
	return []QdrantSearchPoint{*v.repo.point}, nil
}

func TestSearchDropsObjectRevokedMidSearch(t *testing.T) {
	repo := &reauthRepo{
		collection: Collection{ID: "col-b", IsActive: true, OwnerActorID: "actor"},
		generation: IndexGeneration{
			CollectionID: "col-b", QdrantCollection: "index-b", ObjectID: "obj-b",
			ObjectVersionID: "obj-b#1", Generation: 1, State: "published",
		},
		object:  source.Object{ID: "obj-b", TenantID: "t", ObservedSeq: 1, AuthorizationMode: source.AuthorizationOwner, OwnerActorID: "actor"},
		version: source.ObjectVersion{ObjectID: "obj-b", ObservedSeq: 1},
		point: &QdrantSearchPoint{Payload: map[string]any{
			"document_id": "obj-b", "object_version_id": "obj-b#1",
			"generation": int64(1), "chunk_id": "chunk-1",
		}},
	}
	svc := &Service{
		repo:        repo,
		vectorStore: &revokingVectors{repo: repo},
		authorizer:  source.NewAuthorizer(repo, 0),
	}
	results, err := svc.Search(context.Background(), auth.Principal{TenantID: "t", ActorID: "actor"}, SearchInput{CollectionID: "col-b", Query: "hello", Mode: QueryLexical})
	if err != nil {
		t.Fatal(err)
	}
	if len(results) != 0 {
		t.Fatalf("revoked object leaked %d results: %#v", len(results), results)
	}
}

type fixedRetrievalVectors struct {
	VectorStore
	point QdrantSearchPoint
}

func (v *fixedRetrievalVectors) Query(_ context.Context, _ string, _ QueryInput) ([]QdrantSearchPoint, error) {
	return []QdrantSearchPoint{v.point}, nil
}

func searchReauthFixture() (*reauthRepo, auth.Principal) {
	r := &reauthRepo{
		collection: Collection{ID: "col", IsActive: true, OwnerActorID: "actor"},
		generation: IndexGeneration{CollectionID: "col", QdrantCollection: "idx", ObjectID: "obj", ObjectVersionID: "obj#1", Generation: 1},
		object:     source.Object{ID: "obj", TenantID: "t", ObservedSeq: 1, AuthorizationMode: source.AuthorizationOwner, OwnerActorID: "actor"},
		version:    source.ObjectVersion{ObjectID: "obj", ObservedSeq: 1},
		point:      &QdrantSearchPoint{Payload: map[string]any{"document_id": "obj", "object_version_id": "obj#1", "generation": int64(1), "chunk_id": "chunk-1"}},
	}
	return r, auth.Principal{TenantID: "t", ActorID: "actor"}
}

func TestSelectSectionsScopesOnlySentResults(t *testing.T) {
	exec := &retrievalExecutor{}
	svc := &Service{executor: exec}
	results := make([]SearchResult, 21)
	for i := range results {
		results[i] = SearchResult{DocumentID: fmt.Sprint(i / 2), ObservedSeq: 1, Content: "snippet"}
	}
	svc.selectSections(context.Background(), auth.Principal{TenantID: "t"}, results, "model")
	want := make([]source.ObjectRef, 10)
	for i := range want {
		want[i] = source.ObjectRef{TenantID: "t", ObjectID: fmt.Sprint(i), ObservedSeq: 1}
	}
	if len(exec.requests) != 1 || !reflect.DeepEqual(exec.requests[0].Inputs, want) {
		t.Fatalf("selection inputs = %#v, want %#v", exec.requests, want)
	}
}

func TestSearchFinalAuthorizationAfterSelection(t *testing.T) {
	for _, readable := range []bool{false, true} {
		t.Run(fmt.Sprint(readable), func(t *testing.T) {
			repo, principal := searchReauthFixture()
			exec := &retrievalExecutor{onChat: func() { repo.revoked = true }}
			svc := &Service{repo: repo, vectorStore: &fixedRetrievalVectors{point: *repo.point}, executor: exec, authorizer: source.NewAuthorizer(repo, 0)}
			svc.SetUtilityModel("utility")
			if readable {
				got, err := svc.SearchReadableWithOptions(context.Background(), principal, "hello", QueryLexical, nil, 5, SearchReadableOptions{SelectSections: true})
				if err != nil || len(got.Results) != 0 || len(got.Facets) != 0 {
					t.Fatalf("readable results = %#v, err=%v", got, err)
				}
			} else {
				got, err := svc.Search(context.Background(), principal, SearchInput{CollectionID: "col", Query: "hello", Mode: QueryLexical, SelectSections: true})
				if err != nil || len(got) != 0 {
					t.Fatalf("results = %#v, err=%v", got, err)
				}
			}
			if len(exec.requests) != 1 || !repo.revoked || len(repo.evidence) != 0 {
				t.Fatalf("selection=%#v revoked=%v evidence=%#v", exec.requests, repo.revoked, repo.evidence)
			}
		})
	}
}

func TestSearchSkipsDeniedEmbeddingBinding(t *testing.T) {
	repo, principal := searchReauthFixture()
	repo.bindings = []IndexBinding{
		{QdrantCollection: "a", EmbeddingModel: "denied", EmbeddingDimensions: 2},
		{QdrantCollection: "b", EmbeddingModel: "allowed", EmbeddingDimensions: 2},
	}
	exec := &retrievalExecutor{denyModel: "denied"}
	svc := &Service{repo: repo, vectorStore: &fixedRetrievalVectors{point: *repo.point}, executor: exec, authorizer: source.NewAuthorizer(repo, 0)}
	results, err := svc.Search(context.Background(), principal, SearchInput{CollectionID: "col", Query: "hello", Mode: QuerySemantic})
	if err != nil || len(results) != 1 || results[0].DocumentID != "obj" || len(repo.evidence) != 1 {
		t.Fatalf("results = %#v, evidence = %#v, err=%v", results, repo.evidence, err)
	}
	repo.bindings = repo.bindings[:1]
	if _, err := svc.Search(context.Background(), principal, SearchInput{CollectionID: "col", Query: "hello", Mode: QuerySemantic}); !errors.Is(err, inference.ErrDenied) {
		t.Fatalf("all denied = %v", err)
	}
}
