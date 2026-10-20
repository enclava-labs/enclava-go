package rag

import (
	"strings"
	"testing"
)

func TestParseMarkdownBlocksPreservesKindsAndByteSpans(t *testing.T) {
	text := "# Title\n\nA paragraph.\n\n- one\n\n```go\nfmt.Println(1)\n```"
	blocks := parseStructured(text, "markdown")
	if len(blocks) != 4 {
		t.Fatalf("got %d blocks: %#v", len(blocks), blocks)
	}
	for _, block := range blocks {
		if block.ByteStart < 0 || block.ByteEnd > len(text) || block.ByteStart >= block.ByteEnd {
			t.Fatalf("invalid span: %#v", block)
		}
	}
	if blocks[0].Kind != "heading" || blocks[0].Level != 1 || text[blocks[0].ByteStart:blocks[0].ByteEnd] != "Title" {
		t.Fatalf("heading span: %#v", blocks[0])
	}
	if blocks[2].Kind != "list_item" || !strings.Contains(text[blocks[2].ByteStart:blocks[2].ByteEnd], "one") {
		t.Fatalf("list span: %#v", blocks[2])
	}
	if blocks[3].Kind != "code" || blocks[3].Language != "go" {
		t.Fatalf("code block: %#v", blocks[3])
	}
}

func TestParseMalformedMarkdownFallsBack(t *testing.T) {
	text := "ordinary text"
	blocks := parseStructured(text, "markdown")
	if len(blocks) != 1 || blocks[0].Kind != "paragraph" || blocks[0].ByteStart != 0 || blocks[0].ByteEnd != len(text) {
		t.Fatalf("fallback: %#v", blocks)
	}
}

func TestParseHTMLBlocksUsesRawByteSpans(t *testing.T) {
	text := "<h1>Title</h1><p>R&amp;D</p><ul><li>One</li></ul><pre class=\"go\">println(1)</pre>"
	blocks := parseHTMLBlocks(text)
	if len(blocks) != 4 {
		t.Fatalf("got %d blocks: %#v", len(blocks), blocks)
	}
	for _, block := range blocks {
		if got := text[block.ByteStart:block.ByteEnd]; !strings.Contains(got, block.Text) && block.Kind != "paragraph" {
			t.Fatalf("span %q does not cover block %#v", got, block)
		}
		if block.ByteStart < 0 || block.ByteEnd > len(text) || block.ByteStart >= block.ByteEnd {
			t.Fatalf("invalid span: %#v", block)
		}
	}
	if blocks[1].Text != "R&D" {
		t.Fatalf("HTML entity not decoded: %#v", blocks[1])
	}
}

func TestStructuredChunkGroupingKeepsHeadingWithFollowingBlock(t *testing.T) {
	blocks := []parsedBlock{
		{Kind: "heading", Text: "Section", Level: 1, ByteStart: 0, ByteEnd: 7},
		{Kind: "paragraph", Text: "body", ByteStart: 8, ByteEnd: 12},
		{Kind: "heading", Text: "Next", Level: 1, ByteStart: 13, ByteEnd: 17},
		{Kind: "paragraph", Text: "more", ByteStart: 18, ByteEnd: 22},
	}
	chunks := chunkStructured(blocks, 12, 0)
	if len(chunks) != 2 || chunks[0].Text != "Section\nbody" || chunks[0].HeaderPath != "Section" ||
		chunks[1].Text != "Next\nmore" || chunks[1].HeaderPath != "Next" {
		t.Fatalf("unexpected chunks: %#v", chunks)
	}
}

func TestStructuredChunkOverlapRetainsWholeBlocks(t *testing.T) {
	blocks := []parsedBlock{
		{Kind: "paragraph", Text: "aa", ByteStart: 0, ByteEnd: 2},
		{Kind: "paragraph", Text: "bb", ByteStart: 3, ByteEnd: 5},
		{Kind: "paragraph", Text: "cc", ByteStart: 6, ByteEnd: 8},
	}
	chunks := chunkStructured(blocks, 6, 2)
	if len(chunks) != 2 || chunks[1].Text != "bb\ncc" || len(chunks[1].BlockIndexes) != 2 {
		t.Fatalf("overlap did not preserve whole block: %#v", chunks)
	}
}

func TestMalformedStructuredInputFallsBackToFlatBlock(t *testing.T) {
	for _, tc := range []struct{ text, format string }{
		{"```go\nunterminated", "markdown"},
		{"<h1>unterminated", "html"},
	} {
		blocks := parseStructured(tc.text, tc.format)
		if len(blocks) != 1 || blocks[0].Kind != "paragraph" || blocks[0].ByteStart != 0 || blocks[0].ByteEnd != len(tc.text) {
			t.Fatalf("%s fallback: %#v", tc.format, blocks)
		}
	}
}
