package rag

import (
	"context"
	"os"
	"strings"
	"testing"
	"time"
)

func TestQdrantClientIntegration(t *testing.T) {
	qdrantURL := strings.TrimSpace(os.Getenv("ENCLAVA_TEST_QDRANT_URL"))
	if qdrantURL == "" {
		t.Fatalf("ENCLAVA_TEST_QDRANT_URL is not set; tests require a real Qdrant")
	}
	ctx, cancel := context.WithTimeout(context.Background(), 15*time.Second)
	defer cancel()

	client := NewQdrantClient(qdrantURL, 5*time.Second)
	collection := "rag_integration_" + safeCollectionName(newID("test"))
	defer func() { _ = client.DeleteCollection(context.Background(), collection) }()

	if err := client.EnsureCollection(ctx, collection, 3); err != nil {
		t.Fatalf("ensure collection: %v", err)
	}
	if err := client.UpsertPoints(ctx, collection, []QdrantPoint{{
		ID:     newPointID(),
		Vector: []float64{0.1, 0.2, 0.3},
		Payload: map[string]any{
			"document_id": "doc_qdrant_integration",
			"chunk_id":    "chunk_qdrant_integration",
			"content":     "qdrant integration content",
		},
	}}); err != nil {
		t.Fatalf("upsert points: %v", err)
	}
	results, err := client.Search(ctx, collection, []float64{0.1, 0.2, 0.3}, 1, nil)
	if err != nil {
		t.Fatalf("search: %v", err)
	}
	if len(results) != 1 || results[0].Payload["document_id"] != "doc_qdrant_integration" {
		t.Fatalf("unexpected search results: %#v", results)
	}
	if err := client.DeleteDocumentPoints(ctx, collection, "doc_qdrant_integration"); err != nil {
		t.Fatalf("delete document points: %v", err)
	}
	results, err = client.Search(ctx, collection, []float64{0.1, 0.2, 0.3}, 1, nil)
	if err != nil {
		t.Fatalf("search after delete: %v", err)
	}
	if len(results) != 0 {
		t.Fatalf("expected deleted document points to disappear, got %#v", results)
	}
}
