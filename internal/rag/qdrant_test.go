package rag

import (
	"context"
	"encoding/json"
	"fmt"
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

// TestUpsertSplitsLargeDocumentsUnderQdrantBodyLimit: a book-sized point set
// reaches Qdrant in bounded requests — one request carrying every point of a
// 2560-dimension document exceeded Qdrant's 32 MiB body limit.
func TestUpsertSplitsLargeDocumentsUnderQdrantBodyLimit(t *testing.T) {
	var sizes []int
	ids := map[string]bool{}
	server := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		switch {
		case r.Method == http.MethodGet && strings.HasSuffix(r.URL.Path, "/collections/test"):
			_, _ = w.Write([]byte(`{"result":{"config":{"params":{"vectors":{"dense":{"size":2560,"distance":"Cosine"}}}}}}`))
		case r.Method == http.MethodPut && strings.HasSuffix(r.URL.Path, "/points"):
			var body struct {
				Points []struct {
					ID string `json:"id"`
				} `json:"points"`
			}
			if r.ContentLength > 32<<20 {
				w.WriteHeader(http.StatusBadRequest)
				return
			}
			if err := json.NewDecoder(r.Body).Decode(&body); err != nil {
				t.Errorf("decode upsert request: %v", err)
			}
			sizes = append(sizes, len(body.Points))
			for _, p := range body.Points {
				ids[p.ID] = true
			}
			w.WriteHeader(http.StatusOK)
		default:
			http.NotFound(w, r)
		}
	}))
	defer server.Close()

	vector := make([]float64, 2560)
	for i := range vector {
		vector[i] = -0.0219345092773437 // a realistic ~20-byte JSON float
	}
	points := make([]QdrantPoint, 1247)
	for i := range points {
		points[i] = QdrantPoint{ID: fmt.Sprintf("p-%d", i), Vector: vector, Payload: map[string]any{"content": strings.Repeat("x", 1000)}}
	}
	client := NewQdrantClient(server.URL, 30*time.Second)
	if err := client.UpsertPoints(context.Background(), "test", points); err != nil {
		t.Fatalf("upsert 1247 points: %v (request sizes %v)", err, sizes)
	}
	if len(ids) != len(points) {
		t.Fatalf("delivered %d distinct points, want %d (request sizes %v)", len(ids), len(points), sizes)
	}
}
