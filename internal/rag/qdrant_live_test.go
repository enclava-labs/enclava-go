//go:build live

package rag

import (
	"context"
	"fmt"
	"net/http"
	"os"
	"strings"
	"testing"
	"time"
)

func TestLiveQdrantHybridLexicalAndFilters(t *testing.T) {
	baseURL := strings.TrimSpace(os.Getenv("QDRANT_INTEGRATION_URL"))
	if baseURL == "" { t.Fatal("QDRANT_INTEGRATION_URL is required") }
	ctx, cancel := context.WithTimeout(context.Background(), 30*time.Second)
	defer cancel()
	client := NewQdrantClient(baseURL, 5*time.Second)
	collection := fmt.Sprintf("rag_live_%d", time.Now().UnixNano())
	defer func() { _ = client.DeleteCollection(context.Background(), collection) }()
	if err := client.EnsureCollection(ctx, collection, 3); err != nil { t.Fatalf("create collection: %v", err) }
	for _, field := range []string{"content", "tenant_id", "access_enabled"} {
		schema := "keyword"
		if field == "content" { schema = "text" }
		status, response, err := client.do(ctx, http.MethodPut, "/collections/"+collection+"/index", []byte(`{"field_name":"`+field+`","field_schema":"`+schema+`"}`))
		if err != nil || status >= 300 { t.Fatalf("create %s payload index failed: status=%d err=%v body=%s", field, status, err, response) }
	}
	if err := client.UpsertPoints(ctx, collection, []QdrantPoint{{
		ID: "0190f1b2-3c4d-7e8f-9012-3456789abcde",
		Vector: []float64{0.1, 0.2, 0.3},
		Sparse: SparseVector{Indices: []uint32{1}, Values: []float32{1}},
		Payload: map[string]any{"content": "orchid legal retrieval fixture", "tenant_id": "tenant-live", "object_version_id": "version-live", "generation": 1, "access_enabled": true},
	}}); err != nil { t.Fatalf("upsert fixture: %v", err) }
	filter := FilterBuilder("tenant-live", []IndexGeneration{{ObjectVersionID: "version-live", Generation: 1}}, nil)
	if points, err := client.Query(ctx, collection, QueryInput{Mode: QueryHybrid, DenseVector: []float64{0.1, 0.2, 0.3}, LexicalText: "orchid legal", Filter: filter, Limit: 3}); err != nil || len(points) != 1 { t.Fatalf("hybrid prefetch+RRF: points=%#v err=%v", points, err) }
	if points, err := client.Query(ctx, collection, QueryInput{Mode: QueryLexical, LexicalText: "orchid legal", Filter: filter, Limit: 3}); err != nil || len(points) != 1 { t.Fatalf("lexical query: points=%#v err=%v", points, err) }
	narrowed := FilterBuilder("tenant-live", []IndexGeneration{{ObjectVersionID: "version-live", Generation: 1}}, map[string]any{"key": "tenant_id", "match": map[string]any{"value": "other-tenant"}})
	if points, err := client.Query(ctx, collection, QueryInput{Mode: QueryLexical, LexicalText: "orchid legal", Filter: narrowed, Limit: 3}); err != nil || len(points) != 0 { t.Fatalf("narrowing filter: points=%#v err=%v", points, err) }
}
