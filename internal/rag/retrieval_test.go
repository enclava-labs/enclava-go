package rag

import (
	"context"
	"encoding/json"
	"errors"
	"fmt"
	"net/http"
	"net/http/httptest"
	"reflect"
	"strings"
	"testing"

	"enclava-go/internal/auth"
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
	client := NewQdrantClient(server.URL, 0)
	filter := FilterBuilder("tenant-1", []IndexGeneration{{ObjectVersionID: "version-1", Generation: 7}}, nil)
	if _, err := client.Query(context.Background(), "c", QueryInput{Mode: QueryHybrid, DenseVector: []float64{1, 2}, LexicalText: "hello", Filter: filter, Limit: 3}); err != nil {
		t.Fatal(err)
	}
	prefetch := request["prefetch"].([]any)
	if len(prefetch) != 2 || !reflect.DeepEqual(prefetch[0].(map[string]any)["filter"], prefetch[1].(map[string]any)["filter"]) || request["query"].(map[string]any)["fusion"] != "rrf" {
		t.Fatalf("unexpected hybrid body: %#v", request)
	}
	clauses := prefetch[0].(map[string]any)["filter"].(map[string]any)["must"].([]any)
	if clauses[0].(map[string]any)["key"] != "tenant_id" || clauses[1].(map[string]any)["key"] != "access_enabled" || clauses[2].(map[string]any)["should"] == nil {
		t.Fatalf("missing server-scoped filter: %#v", clauses)
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

func TestFilterBuilderCannotOmitServerScope(t *testing.T) {
	filter := FilterBuilder("tenant-1", []IndexGeneration{{ObjectVersionID: "v1", Generation: 2}}, map[string]any{"key": "kind", "match": map[string]any{"value": "file"}})
	must := filter["must"].([]any)
	if len(must) != 4 {
		t.Fatalf("expected tenant, access, generation and narrowing clauses, got %#v", must)
	}
	if must[0].(map[string]any)["key"] != "tenant_id" || must[1].(map[string]any)["key"] != "access_enabled" || must[2].(map[string]any)["should"] == nil {
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
	candidate := func(provenance, connectionID string) authorizedCandidate {
		return authorizedCandidate{
			object:  source.Object{TenantID: "t", ID: "o", Kind: "document", ConnectionID: connectionID},
			version: source.ObjectVersion{Provenance: provenance},
		}
	}
	cache := map[string]string{"conn_kind": "slack"}
	cases := []struct {
		name      string
		candidate authorizedCandidate
		cache     map[string]string
		want      string
	}{
		{"provenance source_kind wins", candidate(`{"source_kind":"github"}`, "conn_kind"), cache, "github"},
		{"connection kind next", candidate(`{}`, "conn_kind"), cache, "slack"},
		{"provenance kind last", candidate(`{"kind":"generated"}`, ""), cache, "generated"},
		{"unknown never object.Kind", candidate(`{}`, ""), cache, "unknown"},
	}
	for _, tc := range cases {
		if got := svc.sourceKindFor(context.Background(), "t", tc.candidate, tc.cache); got != tc.want {
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
	// Equal RRF contributions tie-break by list order then rank — the fused
	// order never depends on map iteration or raw cross-collection scores.
	if first[0].DocumentID != "a" || second[0].DocumentID != "b" {
		t.Fatalf("top rank must follow list order tie-break: %s / %s", first[0].DocumentID, second[0].DocumentID)
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
