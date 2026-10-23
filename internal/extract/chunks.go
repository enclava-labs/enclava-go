package extract

import (
	"crypto/sha256"
	"encoding/hex"
	"fmt"
	"strings"
	"unicode/utf8"

	"enclava-go/internal/source"
)

// Chunk creates deterministic, span-backed chunks from canonical parser blocks.
func Chunk(document source.Document, profile source.ChunkProfile) ([]source.Chunk, error) {
	if err := ValidateChunkProfile(profile); err != nil {
		return nil, err
	}
	if document.Ref.TenantID == "" || document.Ref.ObjectID == "" || document.Ref.ObservedSeq < 1 {
		return nil, fmt.Errorf("extract: document source reference is incomplete")
	}
	blocks := make(map[string]source.Block, len(document.Blocks))
	for _, block := range document.Blocks {
		if block.ID == "" || block.ByteStart < 0 || block.ByteEnd < block.ByteStart || !utf8.ValidString(block.Text) {
			return nil, fmt.Errorf("extract: invalid source block %q offsets", block.ID)
		}
		blocks[block.ID] = block
	}
	for _, block := range document.Blocks {
		if block.ParentID != "" {
			if _, ok := blocks[block.ParentID]; !ok {
				return nil, fmt.Errorf("extract: block %q references missing parent %q", block.ID, block.ParentID)
			}
		}
	}
	var chunks []source.Chunk
	var current source.Chunk
	flush := func() {
		if current.Text == "" {
			return
		}
		current.Ordinal = len(chunks)
		current.ID = chunkID(document.Ref, profile.ID, current.Ordinal, current.Spans)
		chunks = append(chunks, current)
		current = source.Chunk{ObjectRef: document.Ref, ProfileID: profile.ID}
	}
	for _, group := range boundaryGroups(document.Blocks, profile.BoundaryStrategy) {
		if profile.TableHandling == "skip" {
			filtered := group[:0]
			for _, block := range group {
				if !isTableBlock(block) {
					filtered = append(filtered, block)
				}
			}
			group = filtered
		}
		if len(group) == 0 {
			continue
		}
		if chunk, ok := groupedChunk(group, blocks, document.Ref, profile); ok {
			current = chunk
			flush()
			continue
		}
		for _, block := range group {
			parent := parentBlock(block, blocks, profile.IncludeParents)
			totalRunes := utf8.RuneCountInString(block.Text)
			for startRune, start := 0, 0; startRune < totalRunes; {
				end := boundedEnd(block.Text, start, profile.SizeCodePoints)
				if isTableBlock(block) && profile.TableHandling == "row-wise" && end < len(block.Text) {
					if newline := strings.LastIndexByte(block.Text[start:end], '\n'); newline >= 0 {
						end = start + newline + 1
					}
				}
				piece := block.Text[start:end]
				parentText := ""
				var contextSpan *source.Span
				if parent.ID != "" && utf8.RuneCountInString(parent.Text)+utf8.RuneCountInString(piece)+1 <= profile.SizeCodePoints {
					parentText = parent.Text + "\n"
					contextSpan = &source.Span{Ref: document.Ref, BlockID: parent.ID, ByteStart: 0, ByteEnd: len(parent.Text)}
				}
				if isTableBlock(block) && profile.TableHandling == "row-wise" && startRune > 0 && parentText == "" {
					if headerEnd := strings.IndexByte(block.Text, '\n'); headerEnd > 0 {
						header := block.Text[:headerEnd]
						if utf8.RuneCountInString(header)+utf8.RuneCountInString(piece)+1 <= profile.SizeCodePoints {
							parentText = header + "\n"
							contextSpan = &source.Span{Ref: document.Ref, BlockID: block.ID, ByteStart: 0, ByteEnd: headerEnd}
						}
					}
				}
				current = source.Chunk{ObjectRef: document.Ref, ProfileID: profile.ID}
				if contextSpan != nil {
					current.Text = parentText
					current.Spans = append(current.Spans, *contextSpan)
				}
				current.Text += piece
				current.Spans = append(current.Spans, source.Span{Ref: document.Ref, BlockID: block.ID, ByteStart: start, ByteEnd: end})
				if endRune := startRune + utf8.RuneCountInString(piece); endRune < totalRunes {
					warning := "source block split at configured Unicode code-point size"
					if isTableBlock(block) && profile.TableHandling == "atomic" {
						warning = "atomic table exceeded configured size and was split"
					} else if profile.PreserveClauses && (block.Locator.Clause != "" || profile.BoundaryStrategy == "clause") {
						warning = "clause exceeded configured size and was split"
					}
					current.Warnings = append(current.Warnings, warning)
				}
				flush()
				endRune := startRune + utf8.RuneCountInString(piece)
				if endRune == totalRunes {
					break
				}
				next := endRune - profile.OverlapCodePoints
				if next <= startRune {
					next = endRune
				}
				startRune, start = next, end
				if next < endRune {
					start = previousRuneOffset(block.Text, end, endRune-next)
				}
			}
		}
	}
	return chunks, nil
}

func boundaryGroups(blocks []source.Block, strategy string) [][]source.Block {
	var groups [][]source.Block
	var current []source.Block
	emit := func() {
		if len(current) > 0 {
			groups = append(groups, current)
			current = nil
		}
	}
	for _, block := range blocks {
		switch strategy {
		case "heading":
			if block.Kind == "heading" {
				emit()
				current = append(current, block)
			} else {
				current = append(current, block)
			}
		case "clause":
			if block.Locator.Clause != "" && len(current) > 0 {
				for i := len(current) - 1; i >= 0; i-- {
					if prior := current[i]; prior.Locator.Clause != "" {
						if prior.Locator.Clause != block.Locator.Clause || prior.ParentID != block.ParentID {
							emit()
						}
						break
					}
				}
			}
			current = append(current, block)
		case "thread":
			if block.Kind != "message" {
				emit()
				groups = append(groups, []source.Block{block})
				continue
			}
			if block.ParentID == "" {
				emit()
				groups = append(groups, []source.Block{block})
				continue
			}
			if len(current) > 0 && current[0].ParentID != block.ParentID {
				emit()
			}
			current = append(current, block)
		case "code_lines":
			if block.Kind != "code" {
				emit()
				groups = append(groups, []source.Block{block})
				continue
			}
			if len(current) > 0 && (current[0].Kind != "code" || current[0].ParentID != block.ParentID) {
				emit()
			}
			current = append(current, block)
		default:
			emit()
			groups = append(groups, []source.Block{block})
		}
	}
	emit()
	if strategy == "clause" {
		groups = attachFootnotes(groups)
	}
	return groups
}

func attachFootnotes(groups [][]source.Block) [][]source.Block {
	removed := make([]bool, len(groups))
	for i, group := range groups {
		if removed[i] {
			continue
		}
		for _, block := range group {
			for _, ref := range strings.Split(block.Locator.Clause, ",") {
				ref = strings.TrimSpace(ref)
				if !strings.HasPrefix(ref, "footnote:") && !strings.HasPrefix(ref, "endnote:") {
					continue
				}
				for j, candidateGroup := range groups {
					if i == j || removed[j] {
						continue
					}
					found := false
					for _, candidate := range candidateGroup {
						for _, locator := range strings.Split(candidate.Locator.Clause, ",") {
							if strings.TrimSpace(locator) == ref {
								found = true
								break
							}
						}
						if found {
							groups[i] = append(groups[i], candidateGroup...)
							removed[j] = true
							break
						}
					}
					if found {
						break
					}
				}
			}
		}
	}
	out := make([][]source.Block, 0, len(groups))
	for i, group := range groups {
		if !removed[i] {
			out = append(out, group)
		}
	}
	return out
}

func groupedChunk(group []source.Block, blocks map[string]source.Block, ref source.ObjectRef, profile source.ChunkProfile) (source.Chunk, bool) {
	if len(group) < 2 {
		return source.Chunk{}, false
	}
	var chunk source.Chunk
	chunk.ObjectRef, chunk.ProfileID = ref, profile.ID
	seen := make(map[string]bool, len(group))
	total := 0
	for i, block := range group {
		seen[block.ID] = true
		if isTableBlock(block) && profile.TableHandling == "row-wise" && len(group) > 1 {
			return source.Chunk{}, false
		}
		if isTableBlock(block) && profile.TableHandling == "atomic" && utf8.RuneCountInString(block.Text) > profile.SizeCodePoints {
			return source.Chunk{}, false
		}
		if i > 0 {
			total++
			chunk.Text += "\n"
		}
		total += utf8.RuneCountInString(block.Text)
		chunk.Text += block.Text
	}
	if profile.IncludeParents {
		parent := parentBlock(group[0], blocks, true)
		if parent.ID != "" && !seen[parent.ID] {
			parentRunes := utf8.RuneCountInString(parent.Text) + 1
			if total+parentRunes <= profile.SizeCodePoints {
				chunk.Text = parent.Text + "\n" + chunk.Text
				chunk.Spans = append(chunk.Spans, source.Span{Ref: ref, BlockID: parent.ID, ByteStart: 0, ByteEnd: len(parent.Text)})
				total += parentRunes
			}
		}
	}
	if total > profile.SizeCodePoints {
		return source.Chunk{}, false
	}
	for _, block := range group {
		chunk.Spans = append(chunk.Spans, source.Span{Ref: ref, BlockID: block.ID, ByteStart: 0, ByteEnd: len(block.Text)})
	}
	return chunk, true
}

func isTableBlock(block source.Block) bool {
	return block.Kind == "table" || block.Kind == "table_row"
}

func boundedEnd(text string, start, maxRunes int) int {
	end := start
	for count := 0; end < len(text) && count < maxRunes; count++ {
		_, size := utf8.DecodeRuneInString(text[end:])
		end += size
	}
	return end
}

func previousRuneOffset(text string, end, count int) int {
	for range count {
		end--
		for end > 0 && text[end]&0xc0 == 0x80 {
			end--
		}
	}
	return end
}

func parentBlock(block source.Block, blocks map[string]source.Block, include bool) source.Block {
	if !include || block.ParentID == "" {
		return source.Block{}
	}
	return blocks[block.ParentID]
}

func chunkID(ref source.ObjectRef, profileID string, ordinal int, spans []source.Span) string {
	key := fmt.Sprintf("%s:%s:%d:%d:%s", ref.ObjectID, profileID, ref.ObservedSeq, ordinal, ref.TenantID)
	for _, span := range spans {
		key += fmt.Sprintf(":%s:%d:%d", span.BlockID, span.ByteStart, span.ByteEnd)
	}
	h := sha256.Sum256([]byte(key))
	return "chunk_" + hex.EncodeToString(h[:12])
}
