package rag

import "testing"

func TestStructuredChunkGroupingKeepsHeadingWithFollowingBlock(t *testing.T) {
	blocks := []chunkBlock{
		{Kind: "heading", Text: "Section", Level: 1, ByteStart: 0, ByteEnd: 7},
		{Kind: "paragraph", Text: "body", ByteStart: 8, ByteEnd: 12},
		{Kind: "heading", Text: "Next", Level: 1, ByteStart: 13, ByteEnd: 17},
		{Kind: "paragraph", Text: "more", ByteStart: 18, ByteEnd: 22},
	}
	chunks := chunkStructured(blocks, 12, 0)
	if len(chunks) != 2 || chunks[0].Text != "Section\nbody" || chunks[0].HeaderPath != "Section" || chunks[1].Text != "Next\nmore" || chunks[1].HeaderPath != "Next" {
		t.Fatalf("unexpected chunks: %#v", chunks)
	}
}

func TestStructuredChunkOverlapRetainsWholeBlocks(t *testing.T) {
	blocks := []chunkBlock{
		{Kind: "paragraph", Text: "aa", ByteStart: 0, ByteEnd: 2},
		{Kind: "paragraph", Text: "bb", ByteStart: 3, ByteEnd: 5},
		{Kind: "paragraph", Text: "cc", ByteStart: 6, ByteEnd: 8},
	}
	chunks := chunkStructured(blocks, 6, 2)
	if len(chunks) != 2 || chunks[1].Text != "bb\ncc" || len(chunks[1].BlockIndexes) != 2 {
		t.Fatalf("overlap did not preserve whole block: %#v", chunks)
	}
}

func TestStructuredChunkHandlesSkippedHeadingLevels(t *testing.T) {
	chunks := chunkStructured([]chunkBlock{{Kind: "heading", Text: "Deep", Level: 3}, {Kind: "paragraph", Text: "body"}}, 20, 0)
	if len(chunks) != 1 || chunks[0].HeaderPath != "Deep" || chunks[0].Text != "Deep\nbody" {
		t.Fatalf("skipped heading level produced bad path: %#v", chunks)
	}
}
