package rag

import (
	"context"
	"encoding/json"
	"net/http"
	"net/http/httptest"
	"strings"
	"testing"
	"time"
)

func TestUpsertUsesQdrantNativeBM25ForIndexedText(t *testing.T) {
	var got map[string]any
	server := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		switch {
		case r.Method == http.MethodGet && strings.HasSuffix(r.URL.Path, "/collections/test"):
			_, _ = w.Write([]byte(`{"result":{"config":{"params":{"vectors":{"dense":{"size":2,"distance":"Cosine"}},"sparse_vectors":{"text":{"modifier":"idf"}}}}}}`))
		case r.Method == http.MethodPut && strings.HasSuffix(r.URL.Path, "/points"):
			if err := json.NewDecoder(r.Body).Decode(&got); err != nil {
				t.Errorf("decode upsert request: %v", err)
			}
			w.WriteHeader(http.StatusOK)
		default:
			http.NotFound(w, r)
		}
	}))
	defer server.Close()

	client := NewQdrantClient(server.URL, time.Second)
	if err := client.UpsertPoints(context.Background(), "test", []QdrantPoint{{
		ID: "point-1", Vector: []float64{1, 0}, Payload: map[string]any{"content": "native text"},
	}}); err != nil {
		t.Fatal(err)
	}
	points, ok := got["points"].([]any)
	if !ok || len(points) != 1 {
		t.Fatalf("upsert points = %#v", got["points"])
	}
	point := points[0].(map[string]any)
	vectors := point["vector"].(map[string]any)
	text := vectors["text"].(map[string]any)
	if text["model"] != "qdrant/bm25" || text["text"] != "native text" {
		t.Fatalf("native sparse vector = %#v", text)
	}
}
