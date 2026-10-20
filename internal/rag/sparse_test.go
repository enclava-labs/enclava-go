package rag

import (
	"context"
	"encoding/json"
	"net/http"
	"net/http/httptest"
	"reflect"
	"strconv"
	"strings"
	"testing"
)

func TestSparseVectorTokenization(t *testing.T) {
	got := sparseVector("A Héllo, world! HELLO 7 x")
	if len(got.Indices) != 3 || len(got.Values) != 3 {
		t.Fatalf("expected three unique valid terms, got %#v", got)
	}
	for i, index := range got.Indices {
		if i > 0 && got.Indices[i-1] >= index {
			t.Fatalf("indices are not unique and sorted: %#v", got.Indices)
		}
		if got.Values[i] != 1 {
			t.Fatalf("unexpected sparse weight: %#v", got.Values)
		}
	}
	tokens := make([]string, 600)
	for i := range tokens {
		tokens[i] = strconv.Itoa(i) + "x"
	}
	if got := sparseVector(strings.Join(tokens, " ")); len(got.Indices) != 512 {
		t.Fatalf("token cap should retain 512 terms, got %d", len(got.Indices))
	}
}

func TestQdrantHybridRequestShape(t *testing.T) {
	var request map[string]any
	server := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		if r.Method == http.MethodGet {
			_, _ = w.Write([]byte(`{"result":{"config":{"params":{"sparse_vectors":{"text":{}}}}}}`))
			return
		}
		if err := json.NewDecoder(r.Body).Decode(&request); err != nil {
			t.Fatal(err)
		}
		_, _ = w.Write([]byte(`{"result":[]}`))
	}))
	defer server.Close()
	client := NewQdrantClient(server.URL, 0)
	filter := map[string]any{"must": []any{map[string]any{"key": "tenant_id", "match": map[string]any{"value": "t1"}}}}
	if _, err := client.SearchHybrid(context.Background(), "c", []float64{1, 2}, SparseVector{Indices: []uint32{4}, Values: []float32{1}}, 3, filter); err != nil {
		t.Fatal(err)
	}
	prefetch, ok := request["prefetch"].([]any)
	if !ok || len(prefetch) != 2 || request["query"].(map[string]any)["fusion"] != "rrf" || request["with_payload"] != true {
		t.Fatalf("unexpected hybrid request: %#v", request)
	}
	for _, item := range prefetch {
		if !reflect.DeepEqual(item.(map[string]any)["filter"], filter) {
			t.Fatalf("missing scoped filter: %#v", request)
		}
	}
}

func TestMergeSearchSections(t *testing.T) {
	point := func(doc, id string, index float64, text string) QdrantSearchPoint {
		return QdrantSearchPoint{ID: id, Score: 1, Payload: map[string]any{"document_id": doc, "chunk_id": id, "chunk_index": index, "content": text, "source_uri": "https://source"}}
	}
	got := mergeSearchSections([]QdrantSearchPoint{
		point("d1", "c2", 2, "two"), point("d2", "x0", 0, "other"), point("d1", "c1", 1, "one"), point("d1", "c4", 4, "four"), point("d3", "z0", 0, "limit"),
	}, 3)
	if len(got) != 3 || got[0].ChunkID != "c2" || got[0].ChunkStart != 1 || got[0].ChunkEnd != 2 || got[0].Content != "onetwo" || got[0].SourceURI != "https://source" || got[2].ChunkStart != 4 {
		t.Fatalf("unexpected merged sections: %#v", got)
	}
	if capped := mergeSearchSections([]QdrantSearchPoint{point("d1", "a", 0, "a"), point("d2", "b", 0, "b")}, 1); len(capped) != 1 {
		t.Fatalf("section limit not enforced: %#v", capped)
	}
	bridged := mergeSearchSections([]QdrantSearchPoint{
		point("d", "c0", 0, "zero"), point("d", "c2", 2, "two"), point("d", "c1", 1, "one"),
	}, 3)
	if len(bridged) != 1 || bridged[0].ChunkStart != 0 || bridged[0].ChunkEnd != 2 || bridged[0].Content != "zeroonetwo" {
		t.Fatalf("adjacent sections were not fully coalesced: %#v", bridged)
	}
}
