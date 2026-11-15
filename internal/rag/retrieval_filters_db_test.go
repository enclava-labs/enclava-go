package rag_test

import (
	"context"
	"errors"
	"testing"
	"time"

	"enclava-go/internal/rag"
)

// TestSearchMetadataFiltersReachTheStore: with filters set, a lexical
// search hands the store the access must-clauses PLUS the metadata clause
// — the lexical (BM25) branch filters exactly like the dense branch.
func TestSearchMetadataFiltersReachTheStore(t *testing.T) {
	f := newFixture(t)
	ctx := context.Background()
	coll := f.collection(t)
	f.stageSearchDoc(t, coll, "obj_meta_1", "metadata filter reach barnacle", `{"container":"#eng","authors":["alice"]}`)
	f.stageSearchDoc(t, coll, "obj_meta_2", "metadata filter reach cuttlefish", `{"container":"#legal","authors":["bob"]}`)

	input := rag.SearchInput{
		CollectionID: coll.ID, Query: "metadata filter reach", Mode: rag.QueryLexical,
		Container: "#eng", Author: "alice", UpdatedAfter: "2026-01-01T00:00:00Z", UpdatedBefore: "2027-01-01T00:00:00Z",
	}
	results, err := f.svc.Search(ctx, f.principal, input)
	if err != nil {
		t.Fatal(err)
	}
	// The fake store ignores metadata clauses, so both staged documents
	// come back; this test pins the FILTER the store receives.
	must := f.vector.lastFilter["must"].([]any)
	if len(must) != 5 {
		t.Fatalf("store filter must carry 4 access clauses + 1 narrower clause: %#v", must)
	}
	narrower := must[4].(map[string]any)["must"].([]any)
	if len(narrower) != 3 {
		t.Fatalf("narrower clause = %#v, want container+authors+one range", narrower)
	}
	if narrower[0].(map[string]any)["key"] != "container" ||
		narrower[1].(map[string]any)["key"] != "authors" ||
		narrower[2].(map[string]any)["key"] != "source_updated_ts" {
		t.Fatalf("narrower clause keys wrong: %#v", narrower)
	}
	bounds := narrower[2].(map[string]any)["range"].(map[string]any)
	if bounds["gte"] != time.Date(2026, 1, 1, 0, 0, 0, 0, time.UTC).Unix() || bounds["lt"] != time.Date(2027, 1, 1, 0, 0, 0, 0, time.UTC).Unix() {
		t.Fatalf("range bounds = %#v", bounds)
	}
	if len(results) == 0 {
		t.Fatal("lexical search returned nothing")
	}
}

// TestEmbedPayloadCarriesFilterFields: the full upload → embed pipeline
// writes the authors keyword array and the numeric source_updated_ts into
// every point, so filters can match without joining PostgreSQL.
func TestEmbedPayloadCarriesFilterFields(t *testing.T) {
	f := newFixture(t)
	ctx := context.Background()
	c := f.collection(t)
	upload, err := f.svc.UploadDocument(ctx, f.principal, c.ID, rag.UploadDocumentInput{
		FileName: "filtered.txt", MimeType: "text/plain",
		Bytes: []byte("filter payload container authors timestamp"),
	})
	if err != nil {
		t.Fatal(err)
	}
	f.runNextJob(t)
	if upload.Document.ID == "" {
		t.Fatal("upload returned no document id")
	}
	points := f.vector.points[c.QdrantCollection]
	if len(points) == 0 {
		t.Fatal("embed job produced no points")
	}
	for _, point := range points {
		authors, ok := point.Payload["authors"].([]string)
		if !ok || len(authors) != 1 || authors[0] != f.principal.ActorID {
			t.Fatalf("point authors = %#v, want the uploader", point.Payload["authors"])
		}
		if point.Payload["container"] != c.Name {
			t.Fatalf("point container = %#v, want the collection name", point.Payload["container"])
		}
		ts, ok := point.Payload["source_updated_ts"].(int64)
		if !ok || ts <= 0 {
			t.Fatalf("point source_updated_ts = %#v, want a unix timestamp", point.Payload["source_updated_ts"])
		}
		if _, isString := point.Payload["source_updated_at"].(string); !isString {
			t.Fatalf("point source_updated_at = %#v, want an RFC 3339 string", point.Payload["source_updated_at"])
		}
	}
}

// TestSearchRejectsMalformedFilters: validation precedes any store work on
// both the per-collection and the cross-collection search path.
func TestSearchRejectsMalformedFilters(t *testing.T) {
	f := newFixture(t)
	ctx := context.Background()
	coll := f.collection(t)
	for _, tc := range []struct{ after, before string }{
		{after: "not-a-date"},
		{before: "2026-01-01"},
	} {
		if _, err := f.svc.Search(ctx, f.principal, rag.SearchInput{CollectionID: coll.ID, Query: "x", UpdatedAfter: tc.after, UpdatedBefore: tc.before}); !errors.Is(err, rag.ErrInvalidSearchFilter) {
			t.Fatalf("Search(updated_after=%q updated_before=%q) = %v", tc.after, tc.before, err)
		}
		if _, err := f.svc.SearchReadableWithOptions(ctx, f.principal, "x", rag.QueryLexical, nil, 5, rag.SearchReadableOptions{UpdatedAfter: tc.after, UpdatedBefore: tc.before}); !errors.Is(err, rag.ErrInvalidSearchFilter) {
			t.Fatalf("SearchReadableWithOptions(updated_after=%q updated_before=%q) = %v", tc.after, tc.before, err)
		}
	}
}
