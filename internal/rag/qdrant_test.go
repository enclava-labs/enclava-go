package rag

import (
	"context"
	"encoding/json"
	"errors"
	"fmt"
	"net/http"
	"net/http/httptest"
	"strings"
	"testing"
	"time"
)

// TestHybridPrefetchIgnoresOffset: every page of a hybrid query must fuse the
// same candidate set. When prefetch limits grew with offset, page two fused a
// different ranking than page one — points were skipped and duplicated across
// concatenated pages.
func TestHybridPrefetchIgnoresOffset(t *testing.T) {
	var requests []map[string]any
	server := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		var request map[string]any
		if err := json.NewDecoder(r.Body).Decode(&request); err != nil {
			t.Error(err)
		}
		requests = append(requests, request)
		_, _ = w.Write([]byte(`{"result":{"points":[]}}`))
	}))
	defer server.Close()
	client := NewQdrantClient(server.URL, time.Second)
	for _, offset := range []int{0, 10, 200, 400} {
		if _, err := client.Query(context.Background(), "test", QueryInput{Mode: QueryHybrid, DenseVector: []float64{1}, LexicalText: "hello", Limit: 10, Offset: offset}); err != nil {
			t.Fatal(err)
		}
	}
	prefetchLimit := func(request map[string]any) float64 {
		var got float64
		for _, branch := range request["prefetch"].([]any) {
			limit := branch.(map[string]any)["limit"].(float64)
			if got == 0 {
				got = limit
			} else if limit != got {
				t.Fatalf("dense and lexical prefetch limits differ: %#v", request)
			}
		}
		return got
	}
	first := prefetchLimit(requests[0])
	deepest := searchFetchRounds * searchFetchMax
	for i, request := range requests {
		if got := prefetchLimit(request); got != first || got < float64(deepest) {
			t.Fatalf("request %d prefetch limit = %v, want %d for every offset: %#v", i, got, deepest, request)
		}
	}
}

// TestEnsureCollectionCreateRaceAccepted: when a concurrent creator wins
// (409 on PUT), EnsureCollection re-verifies the winner's schema and
// succeeds — never fails the embed attempt on an already-existing index.
func TestEnsureCollectionCreateRaceAccepted(t *testing.T) {
	var indexCreates []string
	gets := 0
	server := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		path := r.URL.Path
		switch {
		case r.Method == http.MethodGet && strings.HasSuffix(path, "/collections/race"):
			gets++
			if gets == 1 {
				http.NotFound(w, r) // loser reads before the winner's create
				return
			}
			_, _ = w.Write([]byte(`{"result":{"config":{"params":{"vectors":{"dense":{"size":2,"distance":"Cosine"}},"sparse_vectors":{"text":{"modifier":"idf"}}}}}}`))
		case r.Method == http.MethodPut && strings.HasSuffix(path, "/collections/race"):
			w.WriteHeader(http.StatusConflict)
			_, _ = w.Write([]byte(`{"error":"collection already exists"}`))
		case r.Method == http.MethodPut && strings.HasSuffix(path, "/index"):
			var body map[string]any
			if err := json.NewDecoder(r.Body).Decode(&body); err != nil {
				t.Errorf("decode index request: %v", err)
			}
			indexCreates = append(indexCreates, fmt.Sprint(body["field_name"]))
			w.WriteHeader(http.StatusConflict) // concurrent creator made the same index
		default:
			http.NotFound(w, r)
		}
	}))
	defer server.Close()
	client := NewQdrantClient(server.URL, time.Second)
	if err := client.EnsureCollection(context.Background(), "race", 2); err != nil {
		t.Fatalf("racing ensure = %v, want success on compatible winner schema", err)
	}
	if gets != 2 || len(indexCreates) != 4 {
		t.Fatalf("gets=%d index creates=%#v, want a schema re-verify and 4 payload indexes", gets, indexCreates)
	}
}

// TestEnsureCollectionCreateRaceIncompatible: a 409 winner with the wrong
// schema is still a failure — the race path never weakens the schema check.
func TestEnsureCollectionCreateRaceIncompatible(t *testing.T) {
	gets := 0
	server := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		switch r.Method {
		case http.MethodGet:
			gets++
			if gets == 1 {
				http.NotFound(w, r) // first attempt sees the index as absent
				return
			}
			_, _ = w.Write([]byte(`{"result":{"config":{"params":{"vectors":{"dense":{"size":99,"distance":"Cosine"}},"sparse_vectors":{"text":{"modifier":"idf"}}}}}}`))
		case http.MethodPut:
			w.WriteHeader(http.StatusConflict)
			_, _ = w.Write([]byte(`{"error":"already exists"}`))
		default:
			http.NotFound(w, r)
		}
	}))
	defer server.Close()
	client := NewQdrantClient(server.URL, time.Second)
	if err := client.EnsureCollection(context.Background(), "race", 2); err == nil {
		t.Fatal("incompatible winner schema must fail")
	}
}

func TestEnsureCollectionRetriesTransientSchemaAfterConflict(t *testing.T) {
	gets, creates := 0, 0
	server := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		switch {
		case r.Method == http.MethodGet:
			gets++
			switch gets {
			case 1:
				http.NotFound(w, r)
			case 2:
				http.Error(w, "creating", http.StatusInternalServerError)
			default:
				_, _ = w.Write([]byte(`{"result":{"config":{"params":{"vectors":{"dense":{"size":2,"distance":"Cosine"}},"sparse_vectors":{"text":{"modifier":"idf"}}}}}}`))
			}
		case r.Method == http.MethodPut && strings.HasSuffix(r.URL.Path, "/index"):
			w.WriteHeader(http.StatusOK)
		case r.Method == http.MethodPut:
			creates++
			w.WriteHeader(http.StatusConflict)
		default:
			http.NotFound(w, r)
		}
	}))
	defer server.Close()
	if err := NewQdrantClient(server.URL, time.Second).EnsureCollection(context.Background(), "race", 2); err != nil {
		t.Fatal(err)
	}
	if gets != 3 || creates != 1 {
		t.Fatalf("gets=%d creates=%d, want 3 and 1", gets, creates)
	}
}

func TestEnsureCollectionTransientFailureHonorsContext(t *testing.T) {
	server := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, _ *http.Request) {
		http.Error(w, "unavailable", http.StatusInternalServerError)
	}))
	defer server.Close()
	ctx, cancel := context.WithTimeout(context.Background(), 20*time.Millisecond)
	defer cancel()
	err := NewQdrantClient(server.URL, time.Second).EnsureCollection(ctx, "race", 2)
	if !errors.Is(err, context.DeadlineExceeded) {
		t.Fatalf("got %v, want context deadline", err)
	}
}

func TestQdrantTransientErrorClassification(t *testing.T) {
	server := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, _ *http.Request) {
		http.Error(w, "unavailable", http.StatusServiceUnavailable)
	}))
	defer server.Close()
	_, _, err := NewQdrantClient(server.URL, time.Second).do(context.Background(), http.MethodPut, "/points", nil)
	if !errors.Is(err, ErrVectorStoreUnavailable) {
		t.Fatalf("got %v, want vector store unavailable", err)
	}
}

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
		ID: "point-1", Vector: []float64{1, 0}, Payload: map[string]any{"content": "native text", "search_text": "title and keywords"},
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
	if text["model"] != "qdrant/bm25" || text["text"] != "title and keywords" {
		t.Fatalf("native sparse vector = %#v", text)
	}
	if err := client.UpsertPoints(context.Background(), "test", []QdrantPoint{{
		ID: "point-2", Vector: []float64{1, 0}, Payload: map[string]any{"content": "fallback"},
	}}); err != nil {
		t.Fatal(err)
	}
	point = got["points"].([]any)[0].(map[string]any)
	text = point["vector"].(map[string]any)["text"].(map[string]any)
	if text["text"] != "fallback" {
		t.Fatalf("fallback sparse vector = %#v", text)
	}
}

func TestCountPoints(t *testing.T) {
	server := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		if r.Method != http.MethodPost || r.URL.Path != "/collections/test/points/count" {
			http.NotFound(w, r)
			return
		}
		var body struct {
			Exact bool `json:"exact"`
		}
		if err := json.NewDecoder(r.Body).Decode(&body); err != nil || !body.Exact {
			t.Errorf("expected exact count, got %#v, %v", body, err)
		}
		_, _ = w.Write([]byte(`{"result":{"count":42}}`))
	}))
	defer server.Close()
	client := NewQdrantClient(server.URL, time.Second)
	count, err := client.CountPoints(context.Background(), "test")
	if err != nil || count != 42 {
		t.Fatalf("count = %d, %v", count, err)
	}
	if _, err := client.CountPoints(context.Background(), "missing"); err == nil {
		t.Fatal("missing collection must fail")
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

// TestEnsureCollectionRememberedAcrossEmbeds: every embed used to re-check
// the schema and re-create the 4 payload indexes; after the first successful
// ensure the client must answer from memory and leave the server alone.
func TestEnsureCollectionRememberedAcrossEmbeds(t *testing.T) {
	gets, indexPuts, collectionPuts := 0, 0, 0
	server := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		switch {
		case r.Method == http.MethodGet && strings.HasSuffix(r.URL.Path, "/collections/docs"):
			gets++
			_, _ = w.Write([]byte(`{"result":{"config":{"params":{"vectors":{"dense":{"size":2,"distance":"Cosine"}},"sparse_vectors":{"text":{"modifier":"idf"}}}}}}`))
		case r.Method == http.MethodPut && strings.HasSuffix(r.URL.Path, "/index"):
			indexPuts++
			w.WriteHeader(http.StatusOK)
		case r.Method == http.MethodPut:
			collectionPuts++
			w.WriteHeader(http.StatusConflict)
		default:
			http.NotFound(w, r)
		}
	}))
	defer server.Close()
	client := NewQdrantClient(server.URL, time.Second)
	for i := range 2 {
		if err := client.EnsureCollection(context.Background(), "docs", 2); err != nil {
			t.Fatalf("ensure %d: %v", i, err)
		}
	}
	if gets != 1 || indexPuts != 4 || collectionPuts != 0 {
		t.Fatalf("gets=%d index puts=%d collection puts=%d, want 1/4/0 for two ensures", gets, indexPuts, collectionPuts)
	}
}

// TestEnsureCollectionRefreshedAfterDeleteCollection: deleting a collection
// invalidates the memory — the next ensure verifies the schema again.
func TestEnsureCollectionRefreshedAfterDeleteCollection(t *testing.T) {
	gets, indexPuts := 0, 0
	server := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		switch {
		case r.Method == http.MethodGet && strings.HasSuffix(r.URL.Path, "/collections/docs"):
			gets++
			_, _ = w.Write([]byte(`{"result":{"config":{"params":{"vectors":{"dense":{"size":2,"distance":"Cosine"}},"sparse_vectors":{"text":{"modifier":"idf"}}}}}}`))
		case r.Method == http.MethodPut && strings.HasSuffix(r.URL.Path, "/index"):
			indexPuts++
			w.WriteHeader(http.StatusOK)
		case r.Method == http.MethodDelete:
			w.WriteHeader(http.StatusOK)
		default:
			http.NotFound(w, r)
		}
	}))
	defer server.Close()
	client := NewQdrantClient(server.URL, time.Second)
	if err := client.EnsureCollection(context.Background(), "docs", 2); err != nil {
		t.Fatal(err)
	}
	if err := client.DeleteCollection(context.Background(), "docs"); err != nil {
		t.Fatal(err)
	}
	if err := client.EnsureCollection(context.Background(), "docs", 2); err != nil {
		t.Fatal(err)
	}
	if gets != 2 || indexPuts != 8 {
		t.Fatalf("gets=%d index puts=%d, want 2/8 after a delete", gets, indexPuts)
	}
}

// TestUpsert404ForgetsEnsuredCollection: a collection can vanish between the
// remembered ensure and the upsert. The upsert must report the store as
// unavailable so the embed job retries, and the retry's ensure must verify
// the schema again instead of answering from memory.
func TestUpsert404ForgetsEnsuredCollection(t *testing.T) {
	gets, indexPuts, pointPuts := 0, 0, 0
	gone := false
	server := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		switch {
		case r.Method == http.MethodGet && strings.HasSuffix(r.URL.Path, "/collections/docs"):
			gets++
			if gone {
				http.NotFound(w, r)
				return
			}
			_, _ = w.Write([]byte(`{"result":{"config":{"params":{"vectors":{"dense":{"size":2,"distance":"Cosine"}},"sparse_vectors":{"text":{"modifier":"idf"}}}}}}`))
		case r.Method == http.MethodPut && strings.HasSuffix(r.URL.Path, "/index"):
			indexPuts++
			w.WriteHeader(http.StatusOK)
		case r.Method == http.MethodPut && strings.HasSuffix(r.URL.Path, "/points"):
			pointPuts++
			w.WriteHeader(http.StatusNotFound) // collection dropped after the schema check
		default:
			http.NotFound(w, r)
		}
	}))
	defer server.Close()
	client := NewQdrantClient(server.URL, time.Second)
	if err := client.EnsureCollection(context.Background(), "docs", 2); err != nil {
		t.Fatal(err)
	}
	err := client.UpsertPoints(context.Background(), "docs", []QdrantPoint{{ID: "p-1", Vector: []float64{1, 0}}})
	if !errors.Is(err, ErrVectorStoreUnavailable) {
		t.Fatalf("upsert against a deleted collection = %v, want ErrVectorStoreUnavailable", err)
	}
	if err := client.EnsureCollection(context.Background(), "docs", 2); err != nil {
		t.Fatal(err)
	}
	// The upsert also forgets on a schema GET 404 (collection deleted before
	// the schema read could answer).
	gone = true
	err = client.UpsertPoints(context.Background(), "docs", []QdrantPoint{{ID: "p-2", Vector: []float64{1, 0}}})
	if !errors.Is(err, ErrVectorStoreUnavailable) {
		t.Fatalf("upsert against a missing collection = %v, want ErrVectorStoreUnavailable", err)
	}
	gone = false
	if err := client.EnsureCollection(context.Background(), "docs", 2); err != nil {
		t.Fatal(err)
	}
	if gets != 5 || indexPuts != 12 || pointPuts != 1 {
		t.Fatalf("gets=%d index puts=%d point puts=%d, want 5/12/1", gets, indexPuts, pointPuts)
	}
}

// TestDeleteConnectionPoints: removing a synced source deletes only its own
// points (connection_id filter) and treats a missing collection as nothing
// left to delete.
func TestDeleteConnectionPoints(t *testing.T) {
	var filter map[string]any
	var method, path, wait string
	missing := false
	server := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		method, path, wait = r.Method, r.URL.Path, r.URL.Query().Get("wait")
		if missing {
			http.NotFound(w, r)
			return
		}
		var body map[string]any
		if err := json.NewDecoder(r.Body).Decode(&body); err != nil {
			t.Errorf("decode delete request: %v", err)
		}
		filter, _ = body["filter"].(map[string]any)
		w.WriteHeader(http.StatusOK)
	}))
	defer server.Close()
	client := NewQdrantClient(server.URL, time.Second)
	if err := client.DeleteConnectionPoints(context.Background(), "docs", "conn-1"); err != nil {
		t.Fatal(err)
	}
	if method != http.MethodPost || path != "/collections/docs/points/delete" || wait != "true" {
		t.Fatalf("request %s %s?wait=%s, want POST /collections/docs/points/delete?wait=true", method, path, wait)
	}
	must, _ := filter["must"].([]any)
	if len(must) != 1 {
		t.Fatalf("filter %#v, want exactly one must clause", filter)
	}
	clause, _ := must[0].(map[string]any)
	match, _ := clause["match"].(map[string]any)
	if clause["key"] != "connection_id" || match["value"] != "conn-1" {
		t.Fatalf("filter %#v, want a connection_id match on conn-1", filter)
	}
	missing = true
	if err := client.DeleteConnectionPoints(context.Background(), "docs", "conn-1"); err != nil {
		t.Fatalf("delete on a missing collection = %v, want success", err)
	}
}
