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
