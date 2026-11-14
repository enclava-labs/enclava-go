package extract

import (
	"fmt"
	"reflect"
	"strings"
	"testing"
	"unicode/utf8"

	"enclava-go/internal/source"
)

func autoProfile(size, overlap int) source.ChunkProfile {
	return source.ChunkProfile{ID: "auto-v1", ProfileID: "auto", Version: 1,
		SizeCodePoints: size, OverlapCodePoints: overlap,
		BoundaryStrategy: "auto", TableHandling: "atomic"}
}

func autoDoc(t *testing.T, blocks []source.Block) (source.Document, map[string]source.Block) {
	t.Helper()
	offset := 0
	for i := range blocks {
		blocks[i].Ordinal = i
		blocks[i].ByteStart = offset
		blocks[i].ByteEnd = offset + len(blocks[i].Text)
		offset += len(blocks[i].Text) + 1
	}
	doc := source.Document{Ref: source.ObjectRef{TenantID: "tenant", ObjectID: "object", ObservedSeq: 1}, Blocks: blocks}
	if _, err := Chunk(doc, autoProfile(1024, 0)); err != nil {
		t.Fatalf("invalid fixture: %v", err)
	}
	blocksByID := make(map[string]source.Block, len(blocks))
	for _, block := range blocks {
		blocksByID[block.ID] = block
	}
	return doc, blocksByID
}

// autoJoin mirrors the span-to-text contract of the auto strategy.
func autoJoin(spans []source.Span, blocks map[string]source.Block) string {
	var sb strings.Builder
	for i, span := range spans {
		if i > 0 {
			if blocks[spans[i-1].BlockID].Kind == "heading" && blocks[span.BlockID].Kind == "heading" {
				sb.WriteString(" > ")
			} else {
				sb.WriteByte('\n')
			}
		}
		sb.WriteString(blocks[span.BlockID].Text[span.ByteStart:span.ByteEnd])
	}
	return sb.String()
}

// checkAutoInvariants verifies budget, text/span agreement, coverage,
// determinism and ID uniqueness for one document/profile pair.
func checkAutoInvariants(t *testing.T, doc source.Document, profile source.ChunkProfile) {
	t.Helper()
	chunks, err := Chunk(doc, profile)
	if err != nil {
		t.Fatalf("Chunk: %v", err)
	}
	blocks := make(map[string]source.Block, len(doc.Blocks))
	for _, block := range doc.Blocks {
		blocks[block.ID] = block
	}
	ids := map[string]bool{}
	covered := map[string][][2]int{} // block id -> byte ranges (non-heading content only)
	for i, chunk := range chunks {
		if utf8.RuneCountInString(chunk.Text) > profile.SizeCodePoints {
			t.Fatalf("chunk %d over budget (%d > %d): %q", i, utf8.RuneCountInString(chunk.Text), profile.SizeCodePoints, chunk.Text)
		}
		if chunk.Ordinal != i {
			t.Fatalf("chunk %d has ordinal %d", i, chunk.Ordinal)
		}
		if chunk.ID == "" || ids[chunk.ID] {
			t.Fatalf("missing or duplicate chunk id at %d", i)
		}
		ids[chunk.ID] = true
		if chunk.Text != autoJoin(chunk.Spans, blocks) {
			t.Fatalf("chunk %d text is not the join of its spans: %q", i, chunk.Text)
		}
		for _, span := range chunk.Spans {
			if _, ok := blocks[span.BlockID]; !ok {
				t.Fatalf("fabricated block reference %q", span.BlockID)
			}
			if span.ByteStart < 0 || span.ByteEnd > len(blocks[span.BlockID].Text) {
				t.Fatalf("span outside block: %+v", span)
			}
		}
		if profile.OverlapCodePoints == 0 {
			for _, span := range chunk.Spans {
				if block := blocks[span.BlockID]; !isTableBlock(block) && block.Kind != "heading" {
					covered[span.BlockID] = append(covered[span.BlockID], [2]int{span.ByteStart, span.ByteEnd})
				}
			}
		}
	}
	if profile.OverlapCodePoints == 0 {
		for _, block := range doc.Blocks {
			if isTableBlock(block) || block.Kind == "heading" || len(block.Text) == 0 {
				continue
			}
			pos := 0
			for _, r := range covered[block.ID] {
				if r[0] != pos {
					t.Fatalf("block %q: gap or duplication at %d (range %v)", block.ID, pos, covered[block.ID])
				}
				pos = r[1]
			}
			if pos != len(block.Text) {
				t.Fatalf("block %q covered to %d of %d: %v", block.ID, pos, len(block.Text), covered[block.ID])
			}
		}
	}
	if profile.OverlapCodePoints > 0 {
		for _, block := range doc.Blocks {
			if isTableBlock(block) || block.Kind == "heading" || len(block.Text) == 0 {
				continue
			}
			seen := make([]bool, len(block.Text))
			for _, chunk := range chunks {
				for _, span := range chunk.Spans {
					if span.BlockID == block.ID {
						for i := span.ByteStart; i < span.ByteEnd; i++ {
							seen[i] = true
						}
					}
				}
			}
			for i, s := range seen {
				if !s {
					t.Fatalf("block %q byte %d lost with overlap", block.ID, i)
				}
			}
		}
	}
	again, _ := Chunk(doc, profile)
	if !reflect.DeepEqual(chunks, again) {
		t.Fatal("chunking is not deterministic")
	}
}

func autoDocs() map[string][]source.Block {
	sentence := func(n int) string {
		return fmt.Sprintf("Sentence %02d carries plain words for testing. ", n)
	}
	paragraph := func(from, n int) string {
		var sb strings.Builder
		for i := from; i < from+n; i++ {
			sb.WriteString(sentence(i))
		}
		return sb.String()
	}
	prose := func() []source.Block {
		blocks := make([]source.Block, 0, 12)
		for i := range 12 {
			blocks = append(blocks, source.Block{ID: fmt.Sprintf("p%d", i), Kind: "paragraph", Text: paragraph(i*4, 4)})
		}
		return blocks
	}
	headed := func() []source.Block {
		return []source.Block{
			{ID: "h1", Kind: "heading", Text: "Installation guide"},
			{ID: "h2", Kind: "heading", ParentID: "h1", Text: "Linux setup"},
			{ID: "p1", Kind: "paragraph", ParentID: "h2", Text: paragraph(0, 12)},
			{ID: "p2", Kind: "paragraph", ParentID: "h2", Text: paragraph(12, 10)},
			{ID: "h3", Kind: "heading", ParentID: "h1", Text: "macOS setup"},
			{ID: "p3", Kind: "paragraph", ParentID: "h3", Text: paragraph(22, 3)},
			{ID: "p4", Kind: "paragraph", ParentID: "h3", Text: paragraph(25, 12)},
		}
	}
	messages := func() []source.Block {
		blocks := make([]source.Block, 0, 10)
		blocks = append(blocks, source.Block{ID: "t1", Kind: "message", Text: "Thread one starts here."})
		for i := range 4 {
			blocks = append(blocks, source.Block{ID: fmt.Sprintf("m%d", i), Kind: "message", ParentID: "t1", Text: paragraph(i*3, 3)})
		}
		blocks = append(blocks, source.Block{ID: "t2", Kind: "message", Text: "Thread two starts here."})
		for i := range 4 {
			blocks = append(blocks, source.Block{ID: fmt.Sprintf("n%d", i), Kind: "message", ParentID: "t2", Text: paragraph(20+i*3, 3)})
		}
		return blocks
	}
	code := func() []source.Block {
		blocks := make([]source.Block, 0, 6)
		for i := range 5 {
			blocks = append(blocks, source.Block{ID: fmt.Sprintf("c%d", i), Kind: "code",
				Text: fmt.Sprintf("func example%d() {\n\t// does something meaningful\n\treturn %d\n}\n\n", i, i)})
		}
		blocks = append(blocks, source.Block{ID: "note", Kind: "paragraph", Text: paragraph(40, 2)})
		return blocks
	}
	tableMiddle := func() []source.Block {
		return []source.Block{
			{ID: "before", Kind: "paragraph", Text: paragraph(0, 4)},
			{ID: "th", Kind: "table_header", Locator: source.Locator{Table: "t1"}, Text: "name|value"},
			{ID: "r1", Kind: "table_row", Locator: source.Locator{Table: "t1"}, Text: "alpha|one"},
			{ID: "r2", Kind: "table_row", Locator: source.Locator{Table: "t1"}, Text: "beta|two"},
			{ID: "after", Kind: "paragraph", Text: paragraph(10, 4)},
		}
	}
	multibyte := func() []source.Block {
		text := "Čebela leti nad travnikom. Šola je danes zaprta. Vreme je lepo in vetra ni. Konec zgodbe je tu."
		blocks := make([]source.Block, 0, 6)
		for i := range 6 {
			blocks = append(blocks, source.Block{ID: fmt.Sprintf("s%d", i), Kind: "paragraph", Text: text})
		}
		return blocks
	}
	monolith := func() []source.Block {
		return []source.Block{{ID: "big", Kind: "paragraph", Text: strings.Repeat("x", 20000)}}
	}
	allHeadings := func() []source.Block {
		blocks := make([]source.Block, 0, 20)
		for i := range 20 {
			blocks = append(blocks, source.Block{ID: fmt.Sprintf("a%d", i), Kind: "heading", Text: fmt.Sprintf("Notion style line %02d of the page", i)})
		}
		return blocks
	}
	return map[string][]source.Block{
		"prose": prose(), "headed": headed(), "messages": messages(), "code": code(),
		"table-middle": tableMiddle(), "multibyte": multibyte(), "monolith": monolith(),
		"all-headings": allHeadings(),
	}
}

func TestAutoPropertiesAcrossDocumentsAndProfiles(t *testing.T) {
	docs := autoDocs()
	profiles := []source.ChunkProfile{autoProfile(120, 0), autoProfile(200, 40), autoProfile(64, 8), autoProfile(500, 100)}
	for name, blocks := range docs {
		for _, profile := range profiles {
			t.Run(fmt.Sprintf("%s/%d-%d", name, profile.SizeCodePoints, profile.OverlapCodePoints), func(t *testing.T) {
				doc, _ := autoDoc(t, append([]source.Block{}, blocks...))
				checkAutoInvariants(t, doc, profile)
			})
		}
	}
}

func TestAutoSentenceQuality(t *testing.T) {
	var sb strings.Builder
	for i := range 40 {
		fmt.Fprintf(&sb, "Testing sentence %02d is long enough to matter here. ", i)
	}
	doc, _ := autoDoc(t, []source.Block{{ID: "long", Kind: "paragraph", Text: sb.String()}})
	chunks, err := Chunk(doc, autoProfile(150, 0))
	if err != nil {
		t.Fatal(err)
	}
	if len(chunks) < 3 {
		t.Fatalf("expected multiple chunks, got %d", len(chunks))
	}
	for i, chunk := range chunks {
		if len(chunk.Warnings) > 0 {
			t.Fatalf("sentence split must not warn (chunk %d): %v", i, chunk.Warnings)
		}
		if i == len(chunks)-1 {
			continue
		}
		body := strings.TrimRight(chunk.Text, " \t\n")
		if !strings.HasSuffix(body, ".") {
			t.Fatalf("chunk %d does not end at a sentence end: %q", i, body)
		}
	}
}

func TestAutoSections(t *testing.T) {
	blocks := []source.Block{
		{ID: "h1", Kind: "heading", Text: "Top"},
		{ID: "h2", Kind: "heading", ParentID: "h1", Text: "Nested"},
		{ID: "p1", Kind: "paragraph", ParentID: "h2", Text: strings.Repeat("Long body sentence with detail. ", 5)},
		{ID: "p1b", Kind: "paragraph", ParentID: "h2", Text: strings.Repeat("More body sentence with facts. ", 5)},
		{ID: "h2b", Kind: "heading", ParentID: "h1", Text: "Second"},
		{ID: "p2", Kind: "paragraph", ParentID: "h2b", Text: strings.Repeat("Other body sentence with facts. ", 12)},
	}
	doc, _ := autoDoc(t, blocks)
	profile := autoProfile(200, 0)
	chunks, err := Chunk(doc, profile)
	if err != nil {
		t.Fatal(err)
	}
	if first := chunks[0].Spans[0]; first.BlockID != "h1" || first.ByteStart != 0 {
		t.Fatalf("document chunking must start at the top heading: %q", chunks[0].Text)
	}
	nested, second := false, false
	for _, chunk := range chunks {
		if strings.HasPrefix(chunk.Text, "Top > Nested\n") {
			nested = true
		}
		if strings.HasPrefix(chunk.Text, "Top > Second\n") {
			second = true
		}
	}
	if !nested {
		t.Fatalf("no continuation chunk carries the heading path: %q", chunksTexts(chunks))
	}
	if !second {
		t.Fatalf("the second section's chunks do not start at its heading path: %q", chunksTexts(chunks))
	}
	// small sections merge: h2b's section fits with p1's tail only if packing
	// merges across the heading boundary somewhere
	small := []source.Block{
		{ID: "s1", Kind: "heading", Text: "One"},
		{ID: "c1", Kind: "paragraph", ParentID: "s1", Text: "tiny body"},
		{ID: "s2", Kind: "heading", Text: "Two"},
		{ID: "c2", Kind: "paragraph", ParentID: "s2", Text: "tiny body too"},
	}
	docSmall, _ := autoDoc(t, small)
	merged, err := Chunk(docSmall, autoProfile(200, 0))
	if err != nil {
		t.Fatal(err)
	}
	if len(merged) != 1 || !strings.Contains(merged[0].Text, "One") || !strings.Contains(merged[0].Text, "Two") {
		t.Fatalf("small sections were not merged: %q", chunksTexts(merged))
	}
}

func TestAutoAllHeadingsBehavesAsProse(t *testing.T) {
	blocks := make([]source.Block, 0, 20)
	for i := range 20 {
		blocks = append(blocks, source.Block{ID: fmt.Sprintf("a%d", i), Kind: "heading", Text: fmt.Sprintf("Notion style line %02d", i)})
	}
	doc, _ := autoDoc(t, blocks)
	chunks, err := Chunk(doc, autoProfile(200, 0))
	if err != nil {
		t.Fatal(err)
	}
	if len(chunks) >= len(blocks) {
		t.Fatalf("all-heading page became one chunk per heading: %d chunks", len(chunks))
	}
	for _, chunk := range chunks {
		if utf8.RuneCountInString(chunk.Text) > 200 {
			t.Fatalf("chunk over budget: %q", chunk.Text)
		}
	}
}

func TestAutoMessagesNeverMergeAcrossThreads(t *testing.T) {
	blocks := make([]source.Block, 0, 8)
	blocks = append(blocks, source.Block{ID: "t1", Kind: "message", Text: "Thread one starts."})
	for i := range 3 {
		blocks = append(blocks, source.Block{ID: fmt.Sprintf("a%d", i), Kind: "message", ParentID: "t1", Text: fmt.Sprintf("Thread one message %d with a little body.", i)})
	}
	blocks = append(blocks, source.Block{ID: "t2", Kind: "message", Text: "Thread two starts."})
	for i := range 3 {
		blocks = append(blocks, source.Block{ID: fmt.Sprintf("b%d", i), Kind: "message", ParentID: "t2", Text: fmt.Sprintf("Thread two message %d with a little body.", i)})
	}
	doc, byID := autoDoc(t, blocks)
	for _, profile := range []source.ChunkProfile{autoProfile(400, 0), autoProfile(60, 10)} {
		chunks, err := Chunk(doc, profile)
		if err != nil {
			t.Fatal(err)
		}
		for _, chunk := range chunks {
			thread := ""
			for _, span := range chunk.Spans {
				block := byID[span.BlockID]
				if block.Kind != "message" {
					continue
				}
				if thread != "" && thread != block.ParentID {
					t.Fatalf("chunk mixes threads %q and %q: %q", thread, block.ParentID, chunk.Text)
				}
				thread = block.ParentID
			}
		}
		if len(chunks) < 2 {
			t.Fatalf("threads were merged: %q", chunksTexts(chunks))
		}
	}
}

func TestAutoOverlapRepeatsTrailingSentences(t *testing.T) {
	var sb strings.Builder
	for i := range 30 {
		fmt.Fprintf(&sb, "Overlap sentence %02d carries real content. ", i)
	}
	doc, _ := autoDoc(t, []source.Block{{ID: "p", Kind: "paragraph", Text: sb.String()}})
	chunks, err := Chunk(doc, autoProfile(160, 60))
	if err != nil {
		t.Fatal(err)
	}
	repeated := 0
	for i := 1; i < len(chunks); i++ {
		prevEnds := sentenceEnds(chunks[i-1].Text)
		if len(prevEnds) == 0 {
			continue
		}
		last := prevEnds[len(prevEnds)-1]
		sentenceStart := 0
		for _, e := range prevEnds[:len(prevEnds)-1] {
			sentenceStart = e
		}
		tail := strings.TrimSpace(chunks[i-1].Text[sentenceStart:last])
		if tail != "" && strings.HasPrefix(chunks[i].Text, tail) {
			repeated++
		}
	}
	if repeated == 0 {
		t.Fatalf("no chunk repeats the previous chunk's last sentence: %q", chunksTexts(chunks))
	}
}

func TestAutoNoOverlapAcrossHeadingBoundary(t *testing.T) {
	blocks := []source.Block{
		{ID: "h1", Kind: "heading", Text: "First section"},
		{ID: "p1", Kind: "paragraph", ParentID: "h1", Text: strings.Repeat("Body text for the first section. ", 10)},
		{ID: "h2", Kind: "heading", Text: "Second section"},
		{ID: "p2", Kind: "paragraph", ParentID: "h2", Text: strings.Repeat("Body text for the second section. ", 10)},
	}
	doc, _ := autoDoc(t, blocks)
	chunks, err := Chunk(doc, autoProfile(120, 50))
	if err != nil {
		t.Fatal(err)
	}
	for _, chunk := range chunks {
		// overlap must never cross the section boundary: a chunk carrying
		// second-section content cannot repeat first-section sentences
		if strings.Contains(chunk.Text, "Body text for the second section.") &&
			strings.Contains(chunk.Text, "Body text for the first section.") {
			t.Fatalf("overlap crossed a heading boundary: %q", chunk.Text)
		}
	}
}

func TestAutoTablesUseExistingHandling(t *testing.T) {
	blocks := []source.Block{
		{ID: "before", Kind: "paragraph", Text: "Alpha paragraph before the table ends here. "},
		{ID: "th", Kind: "table_header", Locator: source.Locator{Table: "t1"}, Text: "name|value"},
		{ID: "r1", Kind: "table_row", Locator: source.Locator{Table: "t1"}, Text: "alpha|one"},
		{ID: "r2", Kind: "table_row", Locator: source.Locator{Table: "t1"}, Text: "beta|two"},
		{ID: "after", Kind: "paragraph", Text: "Beta paragraph after the table ends here. "},
	}
	doc, byID := autoDoc(t, blocks)
	profile := autoProfile(300, 0)
	profile.TableHandling = "row-wise"
	chunks, err := Chunk(doc, profile)
	if err != nil {
		t.Fatal(err)
	}
	var tableChunks, textChunks int
	for _, chunk := range chunks {
		allTable := true
		for _, span := range chunk.Spans {
			if !isTableBlock(byID[span.BlockID]) {
				allTable = false
			}
		}
		if allTable && strings.HasPrefix(chunk.Text, "name|value") {
			tableChunks++
		} else {
			textChunks++
			if chunk.Text != "Alpha paragraph before the table ends here. " && chunk.Text != "Beta paragraph after the table ends here. " {
				t.Fatalf("unexpected text chunk: %q", chunk.Text)
			}
		}
	}
	if tableChunks != 2 || textChunks != 2 {
		t.Fatalf("table chunks=%d text chunks=%d: %q", tableChunks, textChunks, chunksTexts(chunks))
	}
	// no overlap crosses the table: the chunk after it starts fresh
	overlapping, _ := Chunk(doc, func() source.ChunkProfile {
		p := autoProfile(300, 50)
		p.TableHandling = "row-wise"
		return p
	}())
	for i, chunk := range overlapping {
		if i > 0 && strings.HasPrefix(chunk.Text, "Alpha paragraph") {
			t.Fatalf("carry crossed a table boundary: %q", chunk.Text)
		}
	}
}

func TestAutoTinyTailIsMergedOrRebalanced(t *testing.T) {
	// merge case: 60+1+10 fits in 100
	big, small := strings.Repeat("a", 60), strings.Repeat("b", 10)
	doc, _ := autoDoc(t, []source.Block{
		{ID: "x0", Kind: "paragraph", Text: big},
		{ID: "x1", Kind: "paragraph", Text: big},
		{ID: "x2", Kind: "paragraph", Text: small},
	})
	merged, err := Chunk(doc, autoProfile(100, 0))
	if err != nil {
		t.Fatal(err)
	}
	if len(merged) != 2 {
		t.Fatalf("tiny tail was not merged: %q", chunksTexts(merged))
	}
	// rebalance case: 90, 90, 15 cannot merge, so the last two split evenly
	big90, tail15 := strings.Repeat("word ", 18), "short tail text"
	doc2, _ := autoDoc(t, []source.Block{
		{ID: "y0", Kind: "paragraph", Text: big90},
		{ID: "y1", Kind: "paragraph", Text: big90},
		{ID: "y2", Kind: "paragraph", Text: tail15},
	})
	balanced, err := Chunk(doc2, autoProfile(100, 0))
	if err != nil {
		t.Fatal(err)
	}
	last := balanced[len(balanced)-1]
	if utf8.RuneCountInString(last.Text)*5 < 100 {
		t.Fatalf("tiny tail left unbalanced: %q", chunksTexts(balanced))
	}
}

func TestAutoOversizedHardCutWarns(t *testing.T) {
	doc, _ := autoDoc(t, []source.Block{{ID: "big", Kind: "paragraph", Text: strings.Repeat("x", 250)}})
	chunks, err := Chunk(doc, autoProfile(100, 0))
	if err != nil {
		t.Fatal(err)
	}
	if len(chunks) != 3 {
		t.Fatalf("expected 3 chunks, got %d", len(chunks))
	}
	for i, chunk := range chunks {
		if utf8.RuneCountInString(chunk.Text) > 100 {
			t.Fatalf("chunk %d over budget", i)
		}
	}
	if len(chunks[0].Warnings) != 1 || chunks[0].Warnings[0] != "source block split at configured Unicode code-point size" {
		t.Fatalf("missing hard-cut warning: %v", chunks[0].Warnings)
	}
	if len(chunks[2].Warnings) != 0 {
		t.Fatalf("last piece must not warn: %v", chunks[2].Warnings)
	}
}

func TestAutoCodeSplitsAtLineEnds(t *testing.T) {
	code := "func one() {\n\treturn 1\n}\n\nfunc two() {\n\treturn 2\n}\n\n" + strings.Repeat("// padding comment line\n", 20)
	doc, _ := autoDoc(t, []source.Block{{ID: "c", Kind: "code", Text: code}})
	chunks, err := Chunk(doc, autoProfile(120, 0))
	if err != nil {
		t.Fatal(err)
	}
	for i, chunk := range chunks[:len(chunks)-1] {
		if !strings.HasSuffix(strings.TrimRight(chunk.Text, "\n"), "}") && !strings.HasSuffix(chunk.Text, "\n") {
			t.Fatalf("code chunk %d not cut at a line end: %q", i, chunk.Text)
		}
	}
}

func chunksTexts(chunks []source.Chunk) []string {
	texts := make([]string, 0, len(chunks))
	for _, c := range chunks {
		texts = append(texts, c.Text)
	}
	return texts
}

// Documents whose blocks are single lines (PDF text, e-books converted from
// PDF) must still end chunks at a sentence end, and a heading line must not
// be left dangling at the end of a chunk.
func TestAutoLineBlocksBreakAfterSentenceEnd(t *testing.T) {
	lines := []string{"A heading line"}
	for i := range 60 {
		lines = append(lines,
			fmt.Sprintf("Sentence %d starts on this line and keeps going past", i),
			"the line end before it finally stops here.",
			"A short one follows. And another begins and",
			"wraps onto the next line.")
		if i%7 == 6 {
			lines = append(lines, fmt.Sprintf("%d. Dangling Heading", i))
		}
	}
	var blocks []source.Block
	for i, line := range lines {
		blocks = append(blocks, source.Block{ID: fmt.Sprintf("b%d", i), Ordinal: i, Kind: "paragraph", Text: line})
	}
	doc, _ := autoDoc(t, blocks)
	for _, overlap := range []int{0, 80} {
		profile := autoProfile(400, overlap)
		checkAutoInvariants(t, doc, profile)
		chunks, err := Chunk(doc, profile)
		if err != nil {
			t.Fatal(err)
		}
		if len(chunks) < 10 {
			t.Fatalf("expected many chunks, got %d", len(chunks))
		}
		for i, chunk := range chunks[:len(chunks)-1] {
			if !strings.HasSuffix(chunk.Text, ".") || strings.HasSuffix(chunk.Text, "Heading") {
				t.Fatalf("overlap %d: chunk %d does not end at a sentence end: %q", overlap, i, chunk.Text[max(0, len(chunk.Text)-80):])
			}
		}
	}
}
