package chatbot_test

import (
	"context"
	"encoding/json"
	"testing"

	"enclava-go/internal/chatbot"
)

// TestKnowledgeSearchAcceptsMetadataFilters: the assistant tool accepts the
// v1.2 metadata filters and forwards them to the rag search untouched —
// validation happens in the rag service.
func TestKnowledgeSearchAcceptsMetadataFilters(t *testing.T) {
	ctx := context.Background()
	p := openPrincipal("tenant-a")
	searcher := &fakeRAGSearcher{}
	bot := chatbot.Instance{RAGCollectionIDs: []string{"collection-a"}, Settings: map[string]any{"tools": []any{"knowledge.search"}}}
	builtins := chatbot.Builtins{RAG: searcher}
	args := `{"collection_id":"collection-a","query":"hello","container":"#eng","author":"alice","updated_after":"2026-01-01T00:00:00Z","updated_before":"2027-01-01T00:00:00Z"}`
	if _, err := builtins.Invoke(ctx, p, bot, "knowledge.search", json.RawMessage(args), nil); err != nil {
		t.Fatalf("search tool with filters: %v", err)
	}
	if len(searcher.inputs) != 1 {
		t.Fatalf("search calls = %d", len(searcher.inputs))
	}
	in := searcher.inputs[0]
	if in.Container != "#eng" || in.Author != "alice" || in.UpdatedAfter != "2026-01-01T00:00:00Z" || in.UpdatedBefore != "2027-01-01T00:00:00Z" {
		t.Fatalf("filters not forwarded: %#v", in)
	}
}
