package extract

import (
	"bytes"
	"context"
	"errors"
	"fmt"
	"strings"
	"testing"
	"unicode/utf8"

	"enclava-go/internal/source"
)

func blocksBody(t *testing.T, blocks []source.StructuredBlock) []byte {
	t.Helper()
	data, err := source.EncodeBlocks(blocks)
	if err != nil {
		t.Fatalf("encode blocks: %v", err)
	}
	return data
}

func TestBlocksFormatValidation(t *testing.T) {
	if _, _, err := blocksBlocks([]byte("{\"blocks\":[{\"kind\":\"paragraph\",\"text\":\"x\",\xff}]}")); !errors.Is(err, ErrMalformedDocument) {
		t.Fatalf("invalid UTF-8 error = %v", err)
	}
	if _, _, err := blocksBlocks([]byte(`{"blocks":[`)); !errors.Is(err, ErrMalformedDocument) {
		t.Fatalf("malformed JSON error = %v", err)
	}
	if _, _, err := blocksBlocks(bytes.Repeat([]byte(" "), maxBlocksBytes+1)); !errors.Is(err, ErrDocumentTooLarge) {
		t.Fatalf("oversize body error = %v", err)
	}
	tooMany := make([]source.StructuredBlock, maxBlocksCount+1)
	for i := range tooMany {
		tooMany[i] = source.StructuredBlock{Kind: "paragraph", Text: "x"}
	}
	if _, _, err := blocksBlocks(blocksBody(t, tooMany)); !errors.Is(err, ErrDocumentTooLarge) {
		t.Fatalf("too many blocks error = %v", err)
	}

	blocks, warnings, err := blocksBlocks(blocksBody(t, []source.StructuredBlock{
		{Kind: "widget", Text: "unknown kind is dropped"},
		{Kind: "paragraph", Text: "kept", URL: "javascript:alert(1)"},
		{Kind: "heading", Text: "too deep", Level: 9},
		{Kind: "message", Text: "hello", Author: "alice", CreatedAt: "not-a-stamp"},
		{Kind: "message", Text: "reply", ParentNativeID: "missing-root"},
		{Kind: "message", Text: "self", NativeID: "m", ParentNativeID: "m"},
		{Kind: "paragraph", Text: "   "},
	}))
	if err != nil {
		t.Fatalf("degradable defects must not fail the parse: %v", err)
	}
	if len(blocks) != 5 {
		t.Fatalf("blocks = %d, want 5 (unknown kind and blank dropped)", len(blocks))
	}
	if blocks[0].locator.URL != "" {
		t.Fatalf("javascript: URL stored: %q", blocks[0].locator.URL)
	}
	if blocks[1].level != 6 {
		t.Fatalf("heading level not clamped to 6: %d", blocks[1].level)
	}
	if blocks[3].parent != "" || blocks[4].parent != "" {
		t.Fatalf("dangling/self parent kept: %q %q", blocks[3].parent, blocks[4].parent)
	}
	joined := strings.Join(warnings, "; ")
	for _, want := range []string{"unknown kind", "unsupported scheme", "heading level", "unresolved parent_native_id", "self-referencing"} {
		if !strings.Contains(joined, want) {
			t.Fatalf("warnings missing %q: %v", want, warnings)
		}
	}
}

func TestBlocksProvenanceFields(t *testing.T) {
	blocks, warnings, err := blocksBlocks(blocksBody(t, []source.StructuredBlock{
		{Kind: "heading", Text: "Title", Level: 1, NativeID: "issue-12", URL: "https://github.com/acme/repo/issues/12"},
		{Kind: "paragraph", Text: "Body text.", URL: "https://github.com/acme/repo/issues/12", CreatedAt: "2026-05-01T09:15:00+02:00", UpdatedAt: "2026-05-02T08:00:00Z"},
		{Kind: "code", Text: "fmt.Println(1)", Language: "go"},
		{Kind: "message", Text: "comment", Author: "bob", CreatedAt: "2026-05-03T10:30:00Z", URL: "https://github.com/acme/repo/issues/12#issuecomment-1", NativeID: "c-1", ParentNativeID: "issue-12"},
	}))
	if err != nil || len(warnings) != 0 {
		t.Fatalf("clean parse: %v %v", warnings, err)
	}
	if len(blocks) != 4 {
		t.Fatalf("blocks = %d", len(blocks))
	}
	if blocks[0].locator.NativeID != "issue-12" || blocks[0].locator.URL == "" || blocks[0].level != 1 {
		t.Fatalf("heading locator = %#v", blocks[0].locator)
	}
	// CreatedAt wins over UpdatedAt and is carried RFC 3339 as-is.
	if blocks[1].locator.Timestamp != "2026-05-01T09:15:00+02:00" {
		t.Fatalf("timestamp fallback wrong: %q", blocks[1].locator.Timestamp)
	}
	if blocks[2].language != "go" || blocks[2].level != 0 {
		t.Fatalf("code language/level = %q/%d", blocks[2].language, blocks[2].level)
	}
	if blocks[3].text != "bob (2026-05-03 10:30): comment" {
		t.Fatalf("message not rendered: %q", blocks[3].text)
	}
	if blocks[3].parent != "issue-12" {
		t.Fatalf("parent_native_id lost: %q", blocks[3].parent)
	}
	// Byte offsets are monotone over the emitted blocks.
	for i := 1; i < len(blocks); i++ {
		if blocks[i].start <= blocks[i-1].start {
			t.Fatalf("block offsets not monotone: %d then %d", blocks[i-1].start, blocks[i].start)
		}
	}
}

func TestBlocksMessageRendering(t *testing.T) {
	cases := []struct{ text, author, ts, want string }{
		{"hello", "alice", "2026-05-01T09:15:00Z", "alice (2026-05-01 09:15): hello"},
		{"hello", "alice", "", "alice: hello"},
		{"hello", "", "2026-05-01T09:15:00Z", "(2026-05-01 09:15): hello"},
		{"hello", "", "", "hello"},
		{"hello", "alice", "garbage", "alice: hello"}, // unparseable stamp is omitted
	}
	for _, tc := range cases {
		if got := renderMessage(tc.text, tc.author, tc.ts); got != tc.want {
			t.Fatalf("renderMessage(%q,%q,%q) = %q, want %q", tc.text, tc.author, tc.ts, got, tc.want)
		}
	}
}

func TestBlocksSafeLinkRule(t *testing.T) {
	allowed := []string{
		"https://github.com/a/b/issues/1", "http://slack.com/archives/C/p1",
		"slack://channel/C123", "notion://www.notion.so/Page-abc",
	}
	for _, u := range allowed {
		if got := safeStructuredLink(u); got == "" {
			t.Fatalf("allowed URL %q rejected", u)
		}
	}
	rejected := []string{
		"javascript:alert(1)", "data:text/html;base64,AAA", "file:///etc/passwd",
		"//host/no-scheme", "https://", "   ", "chrome://settings",
	}
	for _, u := range rejected {
		if got := safeStructuredLink(u); got != "" {
			t.Fatalf("URL %q accepted as %q", u, got)
		}
	}
}

// TestBlocksParseEndToEnd runs a structured document through the real Parse
// entry: format detection, hierarchy from heading levels, parent_native_id →
// block hierarchy, provenance locators, and warnings surfaced on the document.
func TestBlocksParseEndToEnd(t *testing.T) {
	body := blocksBody(t, []source.StructuredBlock{
		{Kind: "heading", Text: "Worker crashes", Level: 1, URL: "https://github.com/acme/repo/issues/12", NativeID: "issue-12"},
		{Kind: "paragraph", Text: "First body paragraph.", URL: "https://github.com/acme/repo/issues/12"},
		{Kind: "code", Text: "panic: oom", Language: "go", URL: "https://github.com/acme/repo/issues/12"},
		{Kind: "widget", Text: "dropped"},
		{Kind: "message", Text: "Same here.", Author: "bob", CreatedAt: "2026-05-03T10:30:00Z", URL: "https://github.com/acme/repo/issues/12#c1", NativeID: "c-1", ParentNativeID: "issue-12"},
	})
	parser, ref := testParser(t, body, source.BlocksMIMEType)
	doc, err := parser.Parse(context.Background(), ref, ParseOptions{
		Filename: "document.blocks.json", MIMEType: source.BlocksMIMEType,
		ObjectRef: source.ObjectRef{TenantID: "tenant", ObjectID: "object", ObservedSeq: 1},
	})
	if err != nil {
		t.Fatalf("parse blocks document: %v", err)
	}
	if len(doc.Blocks) != 4 {
		t.Fatalf("blocks = %d, want 4", len(doc.Blocks))
	}
	title, bodyBlock, code, comment := doc.Blocks[0], doc.Blocks[1], doc.Blocks[2], doc.Blocks[3]
	if title.Kind != "heading" || title.Locator.HeadingLevel != 1 || title.Locator.NativeID != "issue-12" {
		t.Fatalf("title = %#v", title)
	}
	if bodyBlock.ParentID != title.ID || code.ParentID != title.ID {
		t.Fatalf("heading hierarchy lost: %#v", doc.Blocks)
	}
	// parent_native_id resolves to the title heading's block id (its NativeID
	// is the issue id), overriding the heading stack.
	if comment.ParentID != title.ID || comment.Kind != "message" {
		t.Fatalf("comment parent = %q kind = %q", comment.ParentID, comment.Kind)
	}
	if code.Locator.Language != "go" || comment.Locator.Author != "bob" || comment.Locator.Timestamp != "2026-05-03T10:30:00Z" {
		t.Fatalf("locators lost: %#v", doc.Blocks)
	}
	if len(doc.ParseWarnings) != 1 || !strings.Contains(doc.ParseWarnings[0], "unknown kind") {
		t.Fatalf("warnings = %v", doc.ParseWarnings)
	}
	if comment.Text != "bob (2026-05-03 10:30): Same here." {
		t.Fatalf("comment text = %q", comment.Text)
	}
}

// TestBlocksThreadPacksRootWithReplies: one Slack-like thread document, root
// plus replies, chunks into a single run; every message's link points at the
// start of its rendered prefix.
func TestBlocksThreadPacksRootWithReplies(t *testing.T) {
	permalink := func(id string) string { return "https://hooks.example.test/archives/C1/p" + id }
	blocks := []source.StructuredBlock{
		{Kind: "message", Text: "Deploying the new worker now.", Author: "alice", CreatedAt: "2026-05-01T09:15:00Z", URL: permalink("m1"), NativeID: "m1"},
		{Kind: "message", Text: "Watching the logs.", Author: "bob", CreatedAt: "2026-05-01T09:16:00Z", URL: permalink("m2"), NativeID: "m2", ParentNativeID: "m1"},
		{Kind: "message", Text: "Looks healthy.", Author: "carol", CreatedAt: "2026-05-01T09:17:00Z", URL: permalink("m3"), NativeID: "m3", ParentNativeID: "m1"},
	}
	parser, ref := testParser(t, blocksBody(t, blocks), source.BlocksMIMEType)
	doc, err := parser.Parse(context.Background(), ref, ParseOptions{Filename: "document.blocks.json", MIMEType: source.BlocksMIMEType,
		ObjectRef: source.ObjectRef{TenantID: "tenant", ObjectID: "object", ObservedSeq: 1}})
	if err != nil {
		t.Fatal(err)
	}
	profile := autoProfile(400, 0)
	chunks, err := Chunk(doc, profile)
	if err != nil {
		t.Fatal(err)
	}
	if len(chunks) != 1 {
		t.Fatalf("thread split into %d chunks: %v", len(chunks), chunksTexts(chunks))
	}
	chunk := chunks[0]
	if len(chunk.Links) != 3 {
		t.Fatalf("links = %v", chunk.Links)
	}
	for i, link := range chunk.Links {
		want := fmt.Sprintf("%s (%s): ", []string{"alice", "bob", "carol"}[i], []string{"2026-05-01 09:15", "2026-05-01 09:16", "2026-05-01 09:17"}[i])
		boff := 0
		for range link.Offset {
			_, size := utf8.DecodeRuneInString(chunk.Text[boff:])
			boff += size
		}
		if !strings.HasPrefix(chunk.Text[boff:], want) {
			t.Fatalf("link %d offset %d does not point at %q: %q", i, link.Offset, want, chunk.Text[boff:])
		}
		if link.URL != permalink(fmt.Sprintf("m%d", i+1)) || link.NativeID != fmt.Sprintf("m%d", i+1) {
			t.Fatalf("link %d = %+v", i, link)
		}
	}
}

// TestBlocksTableGroupingAndLinks: structured table header + rows chunk as a
// table group and every row keeps its deep link.
func TestBlocksTableGroupingAndLinks(t *testing.T) {
	blocks := []source.StructuredBlock{
		{Kind: "heading", Text: "Report", Level: 1, URL: "https://docs.example.test/report"},
		{Kind: "table_header", Text: "name|value", URL: "https://docs.example.test/report#table"},
		{Kind: "table_row", Text: "alpha|one", URL: "https://docs.example.test/report#r1", NativeID: "r1"},
		{Kind: "table_row", Text: "beta|two", URL: "https://docs.example.test/report#r2", NativeID: "r2"},
	}
	parser, ref := testParser(t, blocksBody(t, blocks), source.BlocksMIMEType)
	doc, err := parser.Parse(context.Background(), ref, ParseOptions{Filename: "document.blocks.json", MIMEType: source.BlocksMIMEType,
		ObjectRef: source.ObjectRef{TenantID: "tenant", ObjectID: "object", ObservedSeq: 1}})
	if err != nil {
		t.Fatal(err)
	}
	if doc.Blocks[1].Locator.Table == "" || doc.Blocks[1].Locator.Table != doc.Blocks[2].Locator.Table || doc.Blocks[2].Locator.Table != doc.Blocks[3].Locator.Table {
		t.Fatalf("table ids not grouped: %#v", doc.Blocks)
	}
	chunks, err := Chunk(doc, autoProfile(500, 0))
	if err != nil {
		t.Fatal(err)
	}
	var tableChunk source.Chunk
	for _, c := range chunks {
		if strings.Contains(c.Text, "alpha|one") {
			tableChunk = c
		}
	}
	if tableChunk.Text == "" || len(tableChunk.Links) < 2 {
		t.Fatalf("table chunk = %q links %v", tableChunk.Text, tableChunk.Links)
	}
	checkChunkLinks(t, tableChunk, blocksByID(t, doc))
}

// TestBlocksMixedDocumentChunks covers the fifth auto mode: heading + body
// sections first, then comment messages packed with the title as heading-path
// prefix. Consecutive body blocks sharing the issue URL collapse to one link.
func TestBlocksMixedDocumentChunks(t *testing.T) {
	issue := "https://github.com/acme/repo/issues/12"
	blocks := []source.StructuredBlock{
		{Kind: "heading", Text: "Worker crashes on large PDFs", Level: 1, URL: issue, NativeID: "issue-12"},
		{Kind: "paragraph", Text: strings.Repeat("The worker dies when a PDF exceeds 2000 pages. ", 8), URL: issue},
		{Kind: "paragraph", Text: strings.Repeat("Logs show an out-of-memory panic in the layout pass. ", 8), URL: issue},
		{Kind: "message", Text: "Same on our instance.", Author: "bob", CreatedAt: "2026-05-03T10:30:00Z", URL: issue + "#c1", NativeID: "c1", ParentNativeID: "issue-12"},
		{Kind: "message", Text: "Fix incoming.", Author: "carol", CreatedAt: "2026-05-03T11:00:00Z", URL: issue + "#c2", NativeID: "c2", ParentNativeID: "issue-12"},
	}
	parser, ref := testParser(t, blocksBody(t, blocks), source.BlocksMIMEType)
	doc, err := parser.Parse(context.Background(), ref, ParseOptions{Filename: "document.blocks.json", MIMEType: source.BlocksMIMEType,
		ObjectRef: source.ObjectRef{TenantID: "tenant", ObjectID: "object", ObservedSeq: 1}})
	if err != nil {
		t.Fatal(err)
	}
	if mode := autoMode(doc.Blocks); mode != "mixed" {
		t.Fatalf("autoMode = %q, want mixed", mode)
	}
	chunks, err := Chunk(doc, autoProfile(300, 0))
	if err != nil {
		t.Fatal(err)
	}
	byID := blocksByID(t, doc)
	for _, chunk := range chunks {
		checkChunkLinks(t, chunk, byID)
		message := false
		for _, span := range chunk.Spans {
			if byID[span.BlockID].Kind == "message" {
				message = true
			}
		}
		if message {
			if !strings.HasPrefix(chunk.Text, "Worker crashes on large PDFs\n") {
				t.Fatalf("message chunk lacks title prefix: %q", chunk.Text)
			}
			if strings.Contains(chunk.Text, "out-of-memory panic") && strings.Contains(chunk.Text, "Same on our instance.") {
				// allowed only if the packer merged sections into the message
				// chunk, which must not happen: the phases split at the run
				t.Fatalf("message chunk swallowed body sections: %q", chunk.Text)
			}
			if len(chunk.Links) != 3 {
				t.Fatalf("comment links = %v", chunk.Links)
			}
			// the title-heading context span anchors the chunk, then each
			// comment's permalink follows at its rendered prefix
			if chunk.Links[0].URL != issue || chunk.Links[0].NativeID != "issue-12" || chunk.Links[0].Offset != 0 {
				t.Fatalf("title link = %+v", chunk.Links[0])
			}
			if chunk.Links[1].URL != issue+"#c1" || chunk.Links[2].URL != issue+"#c2" {
				t.Fatalf("comment link urls = %v", chunk.Links)
			}
		}
	}
	// body paragraphs share the issue URL with the title-heading context:
	// consecutive same-URL entries collapse to one per chunk
	for _, chunk := range chunks {
		if strings.Contains(chunk.Text, "out-of-memory panic") {
			if len(chunk.Links) != 1 || chunk.Links[0].URL != issue {
				t.Fatalf("body chunk links not collapsed: %v", chunk.Links)
			}
		}
	}
}

func TestAutoModeDecisionsUnchangedAndMixed(t *testing.T) {
	msg := func(n int, parent string) source.Block {
		return source.Block{ID: fmt.Sprintf("m%d", n), Kind: "message", ParentID: parent, Text: "hi"}
	}
	cases := []struct {
		name   string
		blocks []source.Block
		want   string
	}{
		{"all messages", []source.Block{msg(1, ""), msg(2, "m1")}, "messages"},
		{"single message", []source.Block{msg(1, "")}, "messages"},
		{"interleaved messages are not mixed", []source.Block{
			{ID: "h", Kind: "heading", Text: "T"}, msg(1, ""), {ID: "p", Kind: "paragraph", Text: "x"}, msg(2, ""),
		}, "sections"},
		{"trailing messages are mixed", []source.Block{
			{ID: "h", Kind: "heading", Text: "T"}, {ID: "p", Kind: "paragraph", Text: "x"}, msg(1, ""), msg(2, "m1"),
		}, "mixed"},
		{"sections unchanged", []source.Block{
			{ID: "h", Kind: "heading", Text: "T"}, {ID: "p", Kind: "paragraph", Text: "x"},
		}, "sections"},
		{"prose unchanged", []source.Block{{ID: "p", Kind: "paragraph", Text: "x"}}, "prose"},
	}
	for _, tc := range cases {
		if got := autoMode(tc.blocks); got != tc.want {
			t.Fatalf("%s: autoMode = %q, want %q", tc.name, got, tc.want)
		}
	}
}

// TestChunkLinksAcrossStrategies runs structured documents through the
// non-auto strategies and verifies the Links property everywhere, including
// parent-context prefixes in fixed mode.
func TestChunkLinksAcrossStrategies(t *testing.T) {
	blocks := []source.Block{
		{ID: "h", Kind: "heading", Text: "Release notes", Locator: source.Locator{URL: "https://docs.example.test/rel", NativeID: "rel"}},
		{ID: "p1", Kind: "paragraph", ParentID: "h", Text: "First paragraph with a deep link.", Locator: source.Locator{URL: "https://docs.example.test/rel#p1"}},
		{ID: "p2", Kind: "paragraph", ParentID: "h", Text: "Second paragraph with another deep link.", Locator: source.Locator{URL: "https://docs.example.test/rel#p2"}},
		{ID: "m1", Kind: "message", ParentID: "h", Text: "alice (2026-05-01 09:15): shipped", Locator: source.Locator{URL: "https://slack.com/p1", Author: "alice", Timestamp: "2026-05-01T09:15:00Z", NativeID: "m1"}},
	}
	doc := source.Document{Ref: source.ObjectRef{TenantID: "tenant", ObjectID: "object", ObservedSeq: 1}, Blocks: blocks}
	byID := blocksByID(t, doc)
	strategies := []source.ChunkProfile{
		{ID: "fixed-v1", ProfileID: "fixed", Version: 1, SizeCodePoints: 40, BoundaryStrategy: "fixed", TableHandling: "atomic"},
		{ID: "head-v1", ProfileID: "heading", Version: 1, SizeCodePoints: 40, BoundaryStrategy: "heading", TableHandling: "atomic"},
		{ID: "thread-v1", ProfileID: "thread", Version: 1, SizeCodePoints: 40, BoundaryStrategy: "thread", TableHandling: "atomic"},
		{ID: "par-v1", ProfileID: "paragraph", Version: 1, SizeCodePoints: 40, BoundaryStrategy: "paragraph", TableHandling: "atomic"},
	}
	for _, profile := range strategies {
		chunks, err := Chunk(doc, profile)
		if err != nil {
			t.Fatalf("%s: %v", profile.BoundaryStrategy, err)
		}
		if len(chunks) == 0 {
			t.Fatalf("%s: no chunks", profile.BoundaryStrategy)
		}
		linked := 0
		for _, chunk := range chunks {
			checkChunkLinks(t, chunk, byID)
			linked += len(chunk.Links)
		}
		if linked == 0 {
			t.Fatalf("%s: no links on any chunk", profile.BoundaryStrategy)
		}
	}
}

// TestFixedStrategyParentContextLink: the fixed strategy prepends a parent
// heading as context; the context span's link must anchor at offset 0 and the
// body link after it.
func TestFixedStrategyParentContextLink(t *testing.T) {
	title := "Incident review"
	doc := source.Document{Ref: source.ObjectRef{TenantID: "tenant", ObjectID: "object", ObservedSeq: 1}, Blocks: []source.Block{
		{ID: "h", Kind: "heading", Text: title, Locator: source.Locator{URL: "https://docs.example.test/postmortem", NativeID: "pm"}},
		{ID: "p", Kind: "paragraph", ParentID: "h", Text: "The outage lasted forty minutes.", Locator: source.Locator{URL: "https://docs.example.test/postmortem#body"}},
	}}
	profile := source.ChunkProfile{ID: "fixed-v1", ProfileID: "fixed", Version: 1, SizeCodePoints: 60,
		BoundaryStrategy: "fixed", TableHandling: "atomic", IncludeParents: true}
	chunks, err := Chunk(doc, profile)
	if err != nil || len(chunks) == 0 {
		t.Fatalf("chunks = %v, %v", chunks, err)
	}
	chunk := chunks[0]
	if !strings.HasPrefix(chunk.Text, title+"\n") {
		t.Fatalf("parent context missing: %q", chunk.Text)
	}
	if len(chunk.Links) != 2 || chunk.Links[0].Offset != 0 {
		t.Fatalf("links = %v", chunk.Links)
	}
	checkChunkLinks(t, chunk, blocksByID(t, doc))
}

// TestUploadDocumentsKeepEmptyLinks pins the rule that parsed uploads (no
// locator provenance) never grow link entries.
func TestUploadDocumentsKeepEmptyLinks(t *testing.T) {
	doc, _ := autoDoc(t, []source.Block{
		{ID: "h", Kind: "heading", Text: "Guide"},
		{ID: "p", Kind: "paragraph", ParentID: "h", Text: strings.Repeat("Plain uploaded content. ", 20)},
	})
	for _, profile := range []source.ChunkProfile{autoProfile(60, 0), autoProfile(200, 40)} {
		chunks, err := Chunk(doc, profile)
		if err != nil {
			t.Fatal(err)
		}
		for _, chunk := range chunks {
			if len(chunk.Links) != 0 {
				t.Fatalf("upload chunk grew links: %v", chunk.Links)
			}
		}
	}
}

func blocksByID(t *testing.T, doc source.Document) map[string]source.Block {
	t.Helper()
	byID := make(map[string]source.Block, len(doc.Blocks))
	for _, block := range doc.Blocks {
		byID[block.ID] = block
	}
	return byID
}
