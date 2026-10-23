package rag

import (
	"context"
	"encoding/json"
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

func (d denyContextObject) CanReadObject(_ context.Context, _ auth.Principal, id string) bool {
	return id != string(d)
}
