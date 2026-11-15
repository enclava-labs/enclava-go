package rag

import (
	"errors"
	"reflect"
	"testing"
	"time"

	"enclava-go/internal/source"
)

func TestParseMetadataFilters(t *testing.T) {
	if f, err := parseMetadataFilters(" ", "", "  ", ""); f != nil || err != nil {
		t.Fatalf("no filters set: filters=%#v err=%v, want nil/nil", f, err)
	}
	after := time.Date(2026, 1, 1, 0, 0, 0, 0, time.UTC).Unix()
	f, err := parseMetadataFilters(" #eng ", " alice ", " 2026-01-01T00:00:00Z ", "")
	if err != nil {
		t.Fatal(err)
	}
	if f.container != "#eng" || f.author != "alice" {
		t.Fatalf("exact-match values must be trimmed: %#v", f)
	}
	if f.updatedAfter == nil || *f.updatedAfter != after || f.updatedBefore != nil {
		t.Fatalf("updated_after bound wrong: %#v", f)
	}
	for _, bad := range []string{"not-a-date", "2026-01-01", "01/02/2026", "2026-13-01T00:00:00Z"} {
		for _, field := range []string{"updated_after", "updated_before"} {
			after, before := bad, ""
			if field == "updated_before" {
				after, before = "", bad
			}
			if _, err := parseMetadataFilters("", "", after, before); !errors.Is(err, ErrInvalidSearchFilter) {
				t.Fatalf("%s=%q error = %v, want ErrInvalidSearchFilter", field, bad, err)
			}
		}
	}
}

func TestMetadataFilterQdrantClause(t *testing.T) {
	var none *searchMetadataFilters
	if none.qdrantClause() != nil {
		t.Fatal("no filters must build no clause")
	}
	after, before := int64(1700000000), int64(1800000000)
	f := &searchMetadataFilters{container: "#eng", author: "alice", updatedAfter: &after, updatedBefore: &before}
	want := []any{
		map[string]any{"key": "container", "match": map[string]any{"value": "#eng"}},
		map[string]any{"key": "authors", "match": map[string]any{"value": "alice"}},
		map[string]any{"key": "source_updated_ts", "range": map[string]any{"gte": after, "lt": before}},
	}
	got, ok := f.qdrantClause()["must"].([]any)
	if !ok || !reflect.DeepEqual(got, want) {
		t.Fatalf("clause = %#v, want %#v", f.qdrantClause(), want)
	}
}

// TestSearchNarrowerAndsWithAccessFilter: the metadata filters reach Qdrant
// as one extra must clause appended AFTER the access clauses — never as a
// replacement, so a filter can only narrow what authorization allows.
func TestSearchNarrowerAndsWithAccessFilter(t *testing.T) {
	raw := map[string]any{"must": []any{map[string]any{"key": "file_name", "match": map[string]any{"value": "a.txt"}}}}
	narrower, err := searchNarrower(SearchInput{Filter: raw, Container: "#eng", UpdatedAfter: "2026-01-01T00:00:00Z"})
	if err != nil {
		t.Fatal(err)
	}
	combined := narrower["must"].([]any)
	if len(combined) != 2 || !reflect.DeepEqual(combined[0], any(raw)) {
		t.Fatalf("raw filter must survive beside the metadata clause: %#v", narrower)
	}
	meta := combined[1].(map[string]any)["must"].([]any)
	if len(meta) != 2 || meta[0].(map[string]any)["key"] != "container" || meta[1].(map[string]any)["key"] != "source_updated_ts" {
		t.Fatalf("metadata clause malformed: %#v", meta)
	}

	// Only metadata set: the clause stands alone as the narrower filter.
	narrower, err = searchNarrower(SearchInput{Author: "dave"})
	if err != nil || narrower["must"].([]any)[0].(map[string]any)["key"] != "authors" {
		t.Fatalf("author-only narrower = %#v err=%v", narrower, err)
	}
	// Only the raw filter set: passthrough must be unchanged.
	narrower, err = searchNarrower(SearchInput{Filter: raw})
	if err != nil || !reflect.DeepEqual(narrower, raw) {
		t.Fatalf("raw-only narrower must pass through: %#v err=%v", narrower, err)
	}
	if _, err := searchNarrower(SearchInput{UpdatedBefore: "yesterday"}); !errors.Is(err, ErrInvalidSearchFilter) {
		t.Fatalf("malformed bound must fail validation, got %v", err)
	}

	filter := AccessFilter("tenant-1", []string{"tok-1"}, narrower)
	must := filter["must"].([]any)
	keys := make([]string, 0, len(must))
	for _, clause := range must {
		key, _ := clause.(map[string]any)["key"].(string)
		keys = append(keys, key)
	}
	// tenant_id, access_enabled, the acl should-clause, the published
	// should-clause, then the combined narrower clause last.
	if len(keys) != 5 || keys[0] != "tenant_id" || keys[1] != "access_enabled" || keys[4] != "" {
		t.Fatalf("access clauses must precede the narrower clause: %#v", keys)
	}
	if _, ok := must[2].(map[string]any)["should"]; !ok {
		t.Fatalf("acl should-clause missing: %#v", must[2])
	}
	if _, ok := must[3].(map[string]any)["should"]; !ok {
		t.Fatalf("published should-clause missing: %#v", must[3])
	}
}

func TestChunkAuthorsAndTimestampHelpers(t *testing.T) {
	authors := authorList(any([]any{"alice", " bob ", "", 7}))
	if len(authors) != 2 || authors[0] != "alice" || authors[1] != " bob " {
		t.Fatalf("authorList = %#v", authors)
	}
	if got := authorList([]string{"carol"}); len(got) != 1 || got[0] != "carol" {
		t.Fatalf("authorList []string = %#v", got)
	}
	if got := authorList("alice"); got != nil {
		t.Fatalf("authorList non-list = %#v", got)
	}

	links := []source.ChunkLink{{Author: "alice"}, {Author: " dave "}, {Author: ""}}
	got := chunkAuthors([]string{"alice", "carol"}, links)
	if len(got) != 3 || got[0] != "alice" || got[1] != "carol" || got[2] != "dave" {
		t.Fatalf("chunkAuthors = %#v, want document authors first then new link authors", got)
	}
	if got := chunkAuthors(nil, nil); len(got) != 0 {
		t.Fatalf("chunkAuthors empty = %#v", got)
	}

	if ts, ok := rfc3339Unix("2026-02-01T00:00:00Z"); !ok || ts != time.Date(2026, 2, 1, 0, 0, 0, 0, time.UTC).Unix() {
		t.Fatalf("rfc3339Unix = %d %v", ts, ok)
	}
	if _, ok := rfc3339Unix("not-a-date"); ok {
		t.Fatal("unparsable timestamps must write nothing")
	}
	if _, ok := rfc3339Unix("  "); ok {
		t.Fatal("empty timestamps must write nothing")
	}
}
