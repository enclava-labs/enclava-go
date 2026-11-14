package extract

import (
	"fmt"
	"strings"
	"unicode/utf8"

	"enclava-go/internal/source"
)

// The "auto" boundary strategy picks one packing mode per document (sections,
// messages, code or prose) and builds every chunk through autoBuilder: a
// shared packer that tracks the code-point budget, the heading-path context
// prefix, the overlap carry and span order, so chunk text is always exactly
// the spans' block slices joined in order ("\n", or " > " between two heading
// blocks, matching the heading-path prefix).

const splitWarning = "source block split at configured Unicode code-point size"

type autoRecord struct {
	chunk    source.Chunk // set only for table chunks routed through emitRaw
	raw      bool
	spans    []source.Span
	warnings []string
	ctxLen   int // leading heading-path context spans
	carryLen int // leading overlap-repeat spans after the context prefix
}

type autoBuilder struct {
	ref     source.ObjectRef
	profile source.ChunkProfile
	blocks  map[string]source.Block
	records []autoRecord

	spans    []source.Span // current chunk: context prefix, overlap carry, body
	ctxLen   int
	carryLen int
	pending  []source.Span // overlap carry seeded for the next chunk
	runStart int           // first record of the current run (tiny-tail fixes)
}

// chunkAuto implements the "auto" boundary strategy. Table groups go through
// the existing table handling; everything else is packed per autoMode.
func chunkAuto(document source.Document, blocks map[string]source.Block, profile source.ChunkProfile) ([]source.Chunk, error) {
	b := &autoBuilder{ref: document.Ref, profile: profile, blocks: blocks}
	mode := autoMode(document.Blocks)
	bs := document.Blocks
	for i := 0; i < len(bs); {
		block := bs[i]
		if block.Kind == "table_header" {
			end := i + 1
			for end < len(bs) && bs[end].Kind == "table_row" && bs[end].Locator.Table == block.Locator.Table {
				end++
			}
			b.closeTextRun()
			if profile.TableHandling != "skip" {
				tableChunks(bs[i:end], document.Ref, profile, b.emitRaw)
			}
			b.runStart = len(b.records)
			i = end
			continue
		}
		if isTableBlock(block) {
			b.closeTextRun()
			b.tableBlock(block)
			b.runStart = len(b.records)
			i++
			continue
		}
		switch mode {
		case "sections":
			b.addSectionBlock(block)
		case "messages":
			b.addMessageBlock(block)
		case "code":
			b.addCodeBlock(block)
		default:
			b.addBlock(block)
		}
		i++
	}
	b.closeTextRun()
	return b.finalize()
}

// autoMode picks the first matching packing mode for the whole document. A
// document that is (nearly) all headings — how connector "page" items map —
// has no sections to build and falls through to prose.
func autoMode(blocks []source.Block) string {
	headings, nonHeadings, messages, code := 0, 0, 0, 0
	for i := range blocks {
		switch blocks[i].Kind {
		case "heading":
			headings++
		case "message":
			messages++
			nonHeadings++
		case "code":
			code++
			nonHeadings++
		default:
			nonHeadings++
		}
	}
	if headings > 0 && nonHeadings > 0 && headings <= 4*nonHeadings {
		return "sections"
	}
	if messages*2 > len(blocks) {
		return "messages"
	}
	if code*2 > len(blocks) {
		return "code"
	}
	return "prose"
}

// closeTextRun ends a packed run: the current chunk is flushed, a tiny tail is
// merged or rebalanced, and no overlap crosses the run boundary.
func (b *autoBuilder) closeTextRun() {
	b.flushCurrent()
	b.closeRun()
	b.pending = nil
}

func (b *autoBuilder) addBlock(block source.Block) {
	b.seedCarry()
	n := utf8.RuneCountInString(block.Text)
	if n > b.profile.SizeCodePoints {
		b.splitBlock(block) // a block alone over the size is cut, never packed
		return
	}
	if len(b.spans) > 0 && b.runes()+b.sepCost(block)+n > b.profile.SizeCodePoints {
		if len(b.spans) > b.carryLen {
			tail := b.cleanBreakTail(n)
			b.flushCurrent()
			b.seedCarry()
			if len(tail) > 0 {
				if isHeadingBlock(b.blocks[tail[0].BlockID]) {
					// a chunk starting at a heading repeats no overlap
					b.spans, b.carryLen, b.pending = nil, 0, nil
				} else if len(b.spans) == 0 {
					b.addContext(b.blocks[tail[0].BlockID])
				}
				b.spans = append(b.spans, tail...)
			}
		}
		// the seeded overlap may not fit next to this block: drop it, keeping
		// any body moved over by the clean break
		if len(b.spans) > 0 && b.runes()+b.sepCost(block)+n > b.profile.SizeCodePoints {
			b.spans = append(b.spans[:b.ctxLen:b.ctxLen], b.spans[b.ctxLen+b.carryLen:]...)
			b.carryLen, b.pending = 0, nil
			if len(b.spans) == 0 {
				b.spans, b.ctxLen = nil, 0
			}
		}
		if b.ctxLen > 0 && b.runes()+b.sepCost(block)+n > b.profile.SizeCodePoints {
			b.spans, b.ctxLen = b.spans[b.ctxLen:], 0
		}
	}
	if block.Kind == "heading" && len(b.spans) == b.carryLen {
		// a chunk starting at a heading never repeats the previous section's
		// tail and carries no heading-path prefix
		b.spans, b.carryLen, b.pending = nil, 0, nil
	}
	if len(b.spans) == 0 {
		b.addContext(block)
	}
	b.spans = append(b.spans, source.Span{Ref: b.ref, BlockID: block.ID, ByteStart: 0, ByteEnd: len(block.Text)})
	if b.ctxLen > 0 && b.runes() > b.profile.SizeCodePoints {
		// the context prefix does not fit next to this block: drop it
		b.spans, b.ctxLen = b.spans[b.ctxLen:], 0
	}
}

// cleanBreakTail is called when the chunk is full and the next block (next
// code points) does not fit. Blocks are often single lines (PDF text, e-books
// converted from PDF), so the last packed block may stop mid-sentence, or be
// a heading whose text follows in the next chunk. When an earlier block in
// the last 40% of the chunk ends a sentence, the blocks after it are removed
// from the chunk and returned so they open the next one.
func (b *autoBuilder) cleanBreakTail(next int) []source.Span {
	first, last := b.ctxLen+b.carryLen, len(b.spans)-1
	if last <= first || b.endsSentence(b.spans[last]) {
		return nil
	}
	size := b.profile.SizeCodePoints
	for j := last - 1; j >= first; j-- {
		tailRunes := spanRunes(b.spans[j+1:], b.blocks)
		if tailRunes*5 > size*2 || tailRunes+1+next > size {
			return nil
		}
		if b.endsSentence(b.spans[j]) {
			tail := append([]source.Span{}, b.spans[j+1:]...)
			b.spans = b.spans[:j+1]
			return tail
		}
	}
	return nil
}

// endsSentence reports whether the span's text ends with sentence-final
// punctuation, ignoring closing quotes and brackets. Headings never do.
func (b *autoBuilder) endsSentence(span source.Span) bool {
	block := b.blocks[span.BlockID]
	if isHeadingBlock(block) {
		return false
	}
	text := strings.TrimRight(block.Text[span.ByteStart:span.ByteEnd], " \t\r\n\"'”’»)]")
	r, _ := utf8.DecodeLastRuneInString(text)
	return strings.ContainsRune(".!?…。！？", r)
}

// addSectionBlock packs section-aware: a new chunk always starts at a heading
// once the current chunk holds at least ~50% of the size; smaller sections
// merge into the current chunk.
func (b *autoBuilder) addSectionBlock(block source.Block) {
	if block.Kind == "heading" && len(b.spans) > b.carryLen && b.runes()*2 >= b.profile.SizeCodePoints {
		b.flushCurrent()
		b.pending = nil
	}
	b.addBlock(block)
}

// addMessageBlock packs thread-wise: consecutive messages sharing a ParentID
// pack together and never merge (or overlap) across threads.
func (b *autoBuilder) addMessageBlock(block source.Block) {
	if b.sameThread(block) {
		b.addBlock(block)
		return
	}
	b.closeTextRun()
	b.addBlock(block)
}

// sameThread reports whether block can extend the chunk being built (or its
// seeded carry) without crossing a thread boundary.
func (b *autoBuilder) sameThread(block source.Block) bool {
	if block.Kind != "message" {
		return false
	}
	var lastID string
	switch {
	case len(b.spans) > 0:
		lastID = b.spans[len(b.spans)-1].BlockID
	case len(b.pending) > 0:
		lastID = b.pending[len(b.pending)-1].BlockID
	default:
		return true
	}
	last := b.blocks[lastID]
	return last.Kind == "message" && last.ParentID == block.ParentID
}

// addCodeBlock packs consecutive code blocks, cutting oversized ones at line
// ends (splitPoint prefers blank lines, then lines).
func (b *autoBuilder) addCodeBlock(block source.Block) {
	if block.Kind != "code" && (len(b.spans) > 0 || len(b.pending) > 0) {
		b.closeTextRun()
	}
	b.addBlock(block)
}

// addContext prepends the heading path of the block starting a fresh chunk,
// reserving at least half the budget for content.
func (b *autoBuilder) addContext(block source.Block) {
	if block.Kind == "heading" {
		return // the chunk starts with its own section heading
	}
	path, runes := b.headingPath(block.ID)
	if len(path) == 0 || runes+1+b.profile.SizeCodePoints/2 > b.profile.SizeCodePoints {
		return
	}
	b.spans = append(b.spans, path...)
	b.ctxLen = len(path)
}

// headingPath returns spans of block's ancestor heading blocks, top-most
// first, plus their rune count as the one-line " > " path.
func (b *autoBuilder) headingPath(blockID string) ([]source.Span, int) {
	var chain []source.Block
	seen := map[string]bool{}
	block, ok := b.blocks[blockID]
	if !ok {
		return nil, 0
	}
	for block.ParentID != "" {
		parent, ok := b.blocks[block.ParentID]
		if !ok || seen[parent.ID] || parent.Kind != "heading" {
			break
		}
		chain = append(chain, parent)
		seen[parent.ID] = true
		block = parent
	}
	spans := make([]source.Span, len(chain))
	runes := 0
	for i := range chain {
		h := chain[len(chain)-1-i] // top-most first
		spans[i] = source.Span{Ref: b.ref, BlockID: h.ID, ByteStart: 0, ByteEnd: len(h.Text)}
		if i > 0 {
			runes += 3
		}
		runes += utf8.RuneCountInString(h.Text)
	}
	return spans, runes
}

// splitBlock cuts a block that alone exceeds the size at sentence-aware
// boundaries. Intermediate pieces become their own chunks (with overlap
// carry); the final piece stays open so following blocks pack into it.
func (b *autoBuilder) splitBlock(block source.Block) {
	text := block.Text
	size := b.profile.SizeCodePoints
	var prefix []source.Span
	prefixRunes := 0
	if block.Kind != "heading" {
		if path, runes := b.headingPath(block.ID); len(path) > 0 && runes+1+size/2 <= size {
			prefix, prefixRunes = path, runes+1
		}
	}
	if len(b.spans) > b.carryLen {
		// a packed chunk holding only section headings is not emitted on its
		// own: when those headings are part of the block's path they become
		// the heading-path prefix of every piece instead
		if len(prefix) > 0 && b.headingOnly() && spansCoveredBy(b.spans, prefix) {
			b.spans, b.ctxLen = nil, 0
		} else {
			b.flushCurrent()
		}
	} else {
		b.spans, b.ctxLen = nil, 0 // carry-only or empty: keep b.pending
	}
	carry := b.pending
	b.pending = nil
	for start := 0; start < len(text); {
		maxRunes := size - prefixRunes
		if len(carry) > 0 {
			if cr := spanRunes(carry, b.blocks) + 1; maxRunes-cr >= size/2 {
				maxRunes -= cr
			} else {
				carry = nil
			}
		}
		end, clean := splitPoint(text, start, maxRunes)
		piece := source.Span{Ref: b.ref, BlockID: block.ID, ByteStart: start, ByteEnd: end}
		b.spans = append(append(append([]source.Span{}, prefix...), carry...), piece)
		b.ctxLen, b.carryLen = len(prefix), len(carry)
		if end >= len(text) {
			return // keep the tail piece open for further packing
		}
		var warns []string
		if !clean {
			warns = []string{splitWarning}
		}
		b.flushCurrent(warns...)
		carry = b.pending
		b.pending = nil
		start = end
	}
}

func (b *autoBuilder) headingOnly() bool {
	for i := range b.spans {
		if !isHeadingBlock(b.blocks[b.spans[i].BlockID]) {
			return false
		}
	}
	return len(b.spans) > 0
}

// spansCoveredBy reports whether every span's block appears in other.
func spansCoveredBy(spans, other []source.Span) bool {
	ids := make(map[string]bool, len(other))
	for i := range other {
		ids[other[i].BlockID] = true
	}
	for i := range spans {
		if !ids[spans[i].BlockID] {
			return false
		}
	}
	return true
}

// tableBlock handles a standalone table block: packed whole when it fits,
// otherwise split at line ends, repeating the header line when row-wise.
func (b *autoBuilder) tableBlock(block source.Block) {
	if b.profile.TableHandling == "skip" {
		return
	}
	text := block.Text
	size := b.profile.SizeCodePoints
	if utf8.RuneCountInString(text) <= size {
		b.emitRaw(source.Chunk{ObjectRef: b.ref, ProfileID: b.profile.ID, Text: text,
			Spans: []source.Span{{Ref: b.ref, BlockID: block.ID, ByteEnd: len(text)}}})
		return
	}
	var header string
	var headerSpan *source.Span
	if b.profile.TableHandling == "row-wise" {
		if i := strings.IndexByte(text, '\n'); i > 0 && utf8.RuneCountInString(text[:i])+1 < size {
			header = text[:i]
			headerSpan = &source.Span{Ref: b.ref, BlockID: block.ID, ByteStart: 0, ByteEnd: i}
		}
	}
	for start := 0; start < len(text); {
		maxRunes := size
		if headerSpan != nil {
			maxRunes = size - utf8.RuneCountInString(header) - 1
		}
		end, _ := splitPoint(text, start, maxRunes)
		chunk := source.Chunk{ObjectRef: b.ref, ProfileID: b.profile.ID}
		if start > 0 && headerSpan != nil {
			chunk.Text = header + "\n"
			chunk.Spans = append(chunk.Spans, *headerSpan)
		}
		chunk.Text += text[start:end]
		chunk.Spans = append(chunk.Spans, source.Span{Ref: b.ref, BlockID: block.ID, ByteStart: start, ByteEnd: end})
		if end < len(text) {
			chunk.Warnings = append(chunk.Warnings, splitWarning)
		}
		b.emitRaw(chunk)
		start = end
	}
}

func (b *autoBuilder) emitRaw(chunk source.Chunk) {
	if chunk.Text == "" {
		return
	}
	b.records = append(b.records, autoRecord{raw: true, chunk: chunk})
}

func (b *autoBuilder) flushCurrent(warnings ...string) {
	if len(b.spans) == 0 {
		return
	}
	spans := make([]source.Span, len(b.spans))
	copy(spans, b.spans)
	ctxLen := b.ctxLen
	b.records = append(b.records, autoRecord{spans: spans, warnings: warnings,
		ctxLen: ctxLen, carryLen: b.carryLen})
	if b.profile.OverlapCodePoints > 0 {
		b.pending = b.carrySpans(spans, ctxLen)
	} else {
		b.pending = nil
	}
	b.spans, b.ctxLen, b.carryLen = nil, 0, 0
}

func (b *autoBuilder) seedCarry() {
	if len(b.spans) == 0 && len(b.pending) > 0 {
		b.spans = append([]source.Span{}, b.pending...)
		b.carryLen = len(b.pending)
	}
}

// carrySpans returns the trailing spans of a flushed chunk, repeated as
// overlap: whole short blocks from the end, then whole trailing sentences of
// the first block that does not fit. Never crosses a heading or table.
func (b *autoBuilder) carrySpans(spans []source.Span, ctxLen int) []source.Span {
	budget := b.profile.OverlapCodePoints
	var carry []source.Span
	used := 0
	for i := len(spans) - 1; i >= ctxLen; i-- {
		span := spans[i]
		block := b.blocks[span.BlockID]
		if block.Kind == "heading" || isTableBlock(block) {
			break
		}
		n := utf8.RuneCountInString(block.Text[span.ByteStart:span.ByteEnd])
		if used+1+n <= budget {
			carry = append([]source.Span{span}, carry...)
			used += 1 + n
			continue
		}
		os := overlapStart(block.Text, span.ByteEnd, budget-used-1)
		if os < span.ByteStart {
			os = span.ByteStart
		}
		if os < span.ByteEnd && used+1+utf8.RuneCountInString(block.Text[os:span.ByteEnd]) <= budget {
			carry = append([]source.Span{{Ref: b.ref, BlockID: block.ID, ByteStart: os, ByteEnd: span.ByteEnd}}, carry...)
		}
		break
	}
	return carry
}

// closeRun avoids a tiny trailing chunk: merged into the previous chunk when
// it fits, otherwise the last two chunks are rebalanced roughly even.
func (b *autoBuilder) closeRun() {
	defer func() { b.runStart = len(b.records) }()
	recs := b.records[b.runStart:]
	if len(recs) < 2 {
		return
	}
	last, prev := &recs[len(recs)-1], &recs[len(recs)-2]
	if last.raw || prev.raw {
		return
	}
	size := b.profile.SizeCodePoints
	lastRunes := spanRunes(last.spans, b.blocks)
	if lastRunes*5 >= size {
		return
	}
	prevRunes := spanRunes(prev.spans, b.blocks)
	if prevRunes+1+lastRunes <= size {
		body := last.spans[last.ctxLen+last.carryLen:]
		prev.spans = append(prev.spans, body...)
		prev.warnings = append(prev.warnings, last.warnings...)
		b.records = b.records[:len(b.records)-1]
		return
	}
	b.rebalanceTail(prev, last, prevRunes, lastRunes)
}

// rebalanceTail moves the split point of the block the two chunks share so
// both end up roughly even.
func (b *autoBuilder) rebalanceTail(prev, last *autoRecord, prevRunes, lastRunes int) {
	if last.ctxLen+last.carryLen >= len(last.spans) {
		return
	}
	first := last.spans[last.ctxLen+last.carryLen]
	if firstBlock := b.blocks[first.BlockID]; firstBlock.Kind == "heading" && first.ByteStart == 0 {
		return // would break section order
	}
	if len(prev.spans) <= prev.ctxLen+prev.carryLen {
		return
	}
	s := prev.spans[len(prev.spans)-1]
	block := b.blocks[s.BlockID]
	if block.Kind == "heading" || isTableBlock(block) {
		return
	}
	from, to := s.ByteStart, s.ByteEnd
	j := len(last.spans)
	for j > last.ctxLen && last.spans[j-1].BlockID == s.BlockID && last.spans[j-1].ByteStart >= from {
		to = last.spans[j-1].ByteEnd
		j--
	}
	regionRunes := utf8.RuneCountInString(block.Text[from:to])
	if regionRunes < 2 {
		return
	}
	target := (prevRunes + lastRunes + 1) / 2
	keep := target - (prevRunes - utf8.RuneCountInString(block.Text[s.ByteStart:s.ByteEnd]))
	if keep < 1 {
		keep = 1
	}
	if keep >= regionRunes {
		keep = regionRunes - 1
	}
	end, clean := splitPoint(block.Text, from, keep)
	if end <= from || end >= to {
		return
	}
	prev.spans = append(prev.spans[:len(prev.spans)-1],
		source.Span{Ref: b.ref, BlockID: block.ID, ByteStart: from, ByteEnd: end})
	last.spans = append(last.spans[:j],
		source.Span{Ref: b.ref, BlockID: block.ID, ByteStart: end, ByteEnd: to})
	prev.warnings = nil
	if !clean && end < len(block.Text) {
		prev.warnings = []string{splitWarning}
	}
}

func (b *autoBuilder) runes() int {
	return spanRunes(b.spans, b.blocks)
}

func (b *autoBuilder) sepCost(block source.Block) int {
	if len(b.spans) > 0 && isHeadingBlock(b.blocks[b.spans[len(b.spans)-1].BlockID]) && isHeadingBlock(block) {
		return 3
	}
	return 1
}

func isHeadingBlock(block source.Block) bool {
	return block.Kind == "heading"
}

func spanRunes(spans []source.Span, blocks map[string]source.Block) int {
	total := 0
	for i := range spans {
		if i > 0 {
			if isHeadingBlock(blocks[spans[i-1].BlockID]) && isHeadingBlock(blocks[spans[i].BlockID]) {
				total += 3
			} else {
				total++
			}
		}
		total += utf8.RuneCountInString(blocks[spans[i].BlockID].Text[spans[i].ByteStart:spans[i].ByteEnd])
	}
	return total
}

// spanText joins the span slices the way auto chunks are built: "\n" between
// pieces, " > " between two heading blocks (the heading-path prefix).
func spanText(spans []source.Span, blocks map[string]source.Block) string {
	var sb strings.Builder
	for i := range spans {
		if i > 0 {
			if isHeadingBlock(blocks[spans[i-1].BlockID]) && isHeadingBlock(blocks[spans[i].BlockID]) {
				sb.WriteString(" > ")
			} else {
				sb.WriteByte('\n')
			}
		}
		sb.WriteString(blocks[spans[i].BlockID].Text[spans[i].ByteStart:spans[i].ByteEnd])
	}
	return sb.String()
}

func (b *autoBuilder) finalize() ([]source.Chunk, error) {
	chunks := make([]source.Chunk, 0, len(b.records))
	for i := range b.records {
		if i >= MaxChunksPerDocument {
			break
		}
		rec := &b.records[i]
		if rec.raw {
			rec.chunk.Ordinal = i
			rec.chunk.ID = chunkID(b.ref, b.profile.ID, i, rec.chunk.Spans)
			chunks = append(chunks, rec.chunk)
			continue
		}
		chunk := source.Chunk{ObjectRef: b.ref, ProfileID: b.profile.ID, Ordinal: i,
			Text: spanText(rec.spans, b.blocks), Spans: rec.spans, Warnings: rec.warnings}
		chunk.ID = chunkID(b.ref, b.profile.ID, i, chunk.Spans)
		chunks = append(chunks, chunk)
	}
	if len(b.records) > MaxChunksPerDocument {
		last := &chunks[len(chunks)-1]
		last.Warnings = append(last.Warnings,
			fmt.Sprintf("truncated: document produced %d chunks; indexed the first %d", len(b.records), MaxChunksPerDocument))
	}
	return chunks, nil
}
