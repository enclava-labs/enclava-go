package extract

import (
	"encoding/json"
	"fmt"
	"net/url"
	"sort"
	"strings"
	"time"
	"unicode/utf8"

	"enclava-go/internal/source"
)

// Caps for structured connector documents: a runaway connector must not
// turn one sync into an unbounded parse.
const (
	maxBlocksBytes = 16 << 20
	maxBlocksCount = 20000
)

// blocksBlocks parses a source.BlocksMIMEType body into parseBlocks. The
// producer is a connector, so the shape is validated defensively: unknown
// kinds, unusable URLs and dangling parent references degrade to parse
// warnings (the content is kept where possible), never fail the document.
func blocksBlocks(data []byte) ([]parseBlock, []string, error) {
	if len(data) > maxBlocksBytes {
		return nil, nil, fmt.Errorf("%w: structured document exceeds %d MB", ErrDocumentTooLarge, maxBlocksBytes>>20)
	}
	if !utf8.Valid(data) {
		return nil, nil, fmt.Errorf("%w: structured document is not valid UTF-8", ErrMalformedDocument)
	}
	var doc source.BlocksDocument
	if err := json.Unmarshal(data, &doc); err != nil {
		return nil, nil, fmt.Errorf("%w: structured document: %v", ErrMalformedDocument, err)
	}
	if len(doc.Blocks) > maxBlocksCount {
		return nil, nil, fmt.Errorf("%w: structured document has %d blocks, limit is %d", ErrDocumentTooLarge, len(doc.Blocks), maxBlocksCount)
	}
	var (
		out         []parseBlock
		nativeKnown = make(map[string]bool, len(doc.Blocks))
		tableID     string
		tables      int
		dropped     map[string]int
		offset      int // byte offsets into the extracted text: blocks joined by "\n"
	)
	for i := range doc.Blocks {
		sb := doc.Blocks[i]
		if strings.TrimSpace(sb.Text) == "" {
			continue // blank blocks carry nothing; no warning (connectors pad)
		}
		kind, ok := structuredKind(sb.Kind)
		if !ok {
			dropped = countDrop(dropped, "unknown kind")
			continue
		}
		text := strings.TrimSpace(sb.Text)
		level := sb.Level
		if kind == "heading" {
			if level < 1 || level > 6 {
				// makeDocument's heading stack only understands 1..6
				level = min(max(level, 1), 6)
				dropped = countDrop(dropped, "out-of-range heading level")
			}
		} else {
			level = 0
		}
		url := safeStructuredLink(sb.URL)
		if url != sb.URL {
			dropped = countDrop(dropped, "URL with unsupported scheme")
		}
		timestamp := sb.CreatedAt
		if timestamp == "" {
			timestamp = sb.UpdatedAt
		}
		if kind == "message" {
			text = renderMessage(text, sb.Author, timestamp)
		}
		block := parseBlock{
			kind:     kind,
			text:     text,
			level:    level,
			language: sb.Language,
			start:    offset,
			end:      offset + len(text),
			locator: source.Locator{
				URL:       url,
				Author:    sb.Author,
				Timestamp: timestamp,
				NativeID:  sb.NativeID,
			},
			parent: sb.ParentNativeID,
		}
		offset += len(text) + 1
		if kind == "table_header" || kind == "table_row" {
			// consecutive header+rows share a table id so the chunker's
			// existing table grouping (header repeats, atomic/row-wise)
			// applies unchanged
			if kind == "table_header" || !isTableKind(prevKind(out)) {
				tables++
				tableID = fmt.Sprintf("structured-table-%d", tables)
			}
			block.locator.Table = tableID
		}
		if sb.NativeID != "" {
			nativeKnown[sb.NativeID] = true
		}
		if block.parent != "" && block.parent == block.locator.NativeID {
			block.parent = ""
			dropped = countDrop(dropped, "self-referencing parent_native_id")
		}
		out = append(out, block)
	}
	var warnings []string
	for i := range out {
		if out[i].parent != "" && !nativeKnown[out[i].parent] {
			out[i].parent = ""
			dropped = countDrop(dropped, "unresolved parent_native_id")
		}
	}
	warnings = dropWarnings(dropped)
	if len(out) == 0 {
		return nil, warnings, ErrMalformedDocument
	}
	return out, warnings, nil
}

func structuredKind(kind string) (string, bool) {
	switch kind {
	case source.StructuredHeading:
		return "heading", true
	case source.StructuredParagraph:
		return "paragraph", true
	case source.StructuredListItem:
		return "list_item", true
	case source.StructuredCode:
		return "code", true
	case source.StructuredMessage:
		return "message", true
	case source.StructuredTableRow:
		return "table_row", true
	case source.StructuredTableHeader:
		return "table_header", true
	}
	return "", false
}

func isTableKind(kind string) bool {
	return kind == "table_row" || kind == "table_header"
}

func prevKind(out []parseBlock) string {
	if len(out) == 0 {
		return ""
	}
	return out[len(out)-1].kind
}

func countDrop(dropped map[string]int, reason string) map[string]int {
	if dropped == nil {
		dropped = make(map[string]int)
	}
	dropped[reason]++
	return dropped
}

// dropWarnings turns the per-reason drop counters into one parse warning per
// reason, so a huge document cannot flood ParseWarnings.
func dropWarnings(dropped map[string]int) []string {
	if len(dropped) == 0 {
		return nil
	}
	reasons := make([]string, 0, len(dropped))
	for reason := range dropped {
		reasons = append(reasons, reason)
	}
	sort.Strings(reasons)
	warnings := make([]string, 0, len(reasons))
	for _, reason := range reasons {
		warnings = append(warnings, fmt.Sprintf("dropped %s on %d structured blocks", reason, dropped[reason]))
	}
	return warnings
}

// safeStructuredLink is the parse-time twin of internal/rag's native-link
// rule: a connector-supplied URL is stored on a block only with an
// allow-listed scheme and a host, so javascript:/data:/relative URLs can
// never reach a chunk link or the evidence API. Keep the scheme list in
// sync with rag.safeNativeLink.
func safeStructuredLink(raw string) string {
	parsed, err := url.Parse(strings.TrimSpace(raw))
	if err != nil || parsed.Host == "" {
		return ""
	}
	switch strings.ToLower(parsed.Scheme) {
	case "https", "http", "slack", "notion":
		return parsed.String()
	}
	return ""
}

// renderMessage prefixes a message block with its author and timestamp so
// chunk text stays self-explanatory once connectors move the header line
// out of the message text. UTC, minute precision; missing or unparseable
// parts are omitted.
func renderMessage(text, author, timestamp string) string {
	prefix := author
	if t, err := time.Parse(time.RFC3339, timestamp); err == nil {
		stamp := t.UTC().Format("2006-01-02 15:04")
		if prefix != "" {
			prefix += " (" + stamp + ")"
		} else {
			prefix = "(" + stamp + ")"
		}
	}
	if prefix == "" {
		return text
	}
	return prefix + ": " + text
}
