package extract

import (
	"archive/zip"
	"bytes"
	"context"
	"crypto/sha256"
	"encoding/csv"
	"encoding/hex"
	"encoding/json"
	"encoding/xml"
	"errors"
	"fmt"
	"io"
	"mime"
	"net/url"
	"path"

	"path/filepath"
	"sort"
	"strconv"
	"strings"
	"unicode"
	"unicode/utf16"
	"unicode/utf8"

	"enclava-go/internal/artifacts"
	"enclava-go/internal/source"
	htmltoken "golang.org/x/net/html"
)

const maxParseBytes = 256 << 20
const maxExpandedBytes = 64 << 20

var (
	ErrUnsupportedFormat = errors.New("extract: unsupported document format")
	ErrMalformedDocument = errors.New("extract: malformed document")
	ErrEncryptedDocument = errors.New("document is password protected")
	ErrDocumentTooLarge  = errors.New("extract: document exceeds size limit")
)

type ParseOptions struct {
	Filename  string
	MIMEType  string
	ObjectRef source.ObjectRef
}

type Parser struct{ artifacts *artifacts.Store }

func NewParser(store *artifacts.Store) *Parser { return &Parser{artifacts: store} }

// Parse reads only the finalized artifact named by ref and returns stable ordered evidence blocks.
func (p *Parser) Parse(ctx context.Context, ref artifacts.Ref, opts ParseOptions) (source.Document, error) {
	if err := ctx.Err(); err != nil {
		return source.Document{}, err
	}
	if p == nil || p.artifacts == nil {
		return source.Document{}, errors.New("extract: artifact store is not configured")
	}
	if ref.Kind != artifacts.KindOriginal || opts.ObjectRef.TenantID == "" || opts.ObjectRef.TenantID != ref.TenantID || opts.ObjectRef.ObjectID == "" || opts.ObjectRef.ObservedSeq < 1 {
		return source.Document{}, errors.New("extract: parser requires a tenant-matched original and source version")
	}
	rc, err := p.artifacts.Read(ctx, ref)
	if err != nil {
		return source.Document{}, err
	}
	defer func() { _ = rc.Close() }()
	data, err := io.ReadAll(io.LimitReader(rc, maxParseBytes+1))
	if err != nil {
		return source.Document{}, fmt.Errorf("extract: read artifact: %w", err)
	}
	if err := ctx.Err(); err != nil {
		return source.Document{}, err
	}
	if len(data) > maxParseBytes {
		return source.Document{}, fmt.Errorf("%w: file is larger than %d MB", ErrDocumentTooLarge, maxParseBytes>>20)
	}
	if ref.SHA256 == "" {
		digest := sha256.Sum256(data)
		ref.SHA256 = hex.EncodeToString(digest[:])
	}
	format := formatFor(opts.Filename, opts.MIMEType)
	var warnings []string
	if format == "text" || format == "markdown" || format == "html" || format == "csv" || format == "json" {
		var warning string
		data, warning = decodeText(data)
		if warning != "" {
			warnings = append(warnings, warning)
		}
	}
	var blocks []parseBlock
	switch format {
	case "text":
		blocks = plainBlocks(string(data))
	case "markdown":
		blocks = markdownBlocks(string(data))
	case "html":
		blocks = htmlBlocks(string(data))
		if blocks == nil {
			blocks = plainBlocks(string(data))
		}
	case "csv":
		blocks, err = csvBlocks(data)
	case "json":
		blocks, err = jsonBlocks(data, strings.EqualFold(filepath.Ext(opts.Filename), ".jsonl") || strings.EqualFold(filepath.Ext(opts.Filename), ".ndjson") || strings.Contains(strings.ToLower(opts.MIMEType), "ndjson"))
	case "docx":
		blocks, err = docxBlocks(data)
	case "xlsx":
		var sheetWarnings []string
		blocks, sheetWarnings, err = xlsxBlocks(data)
		warnings = append(warnings, sheetWarnings...)
	case "epub":
		blocks, err = epubBlocks(data)
	case "pdf", "image":
		return p.parseNative(ctx, ref, opts, data, format)
	default:
		return source.Document{}, ErrUnsupportedFormat
	}
	if err != nil {
		return source.Document{}, err
	}
	if len(blocks) == 0 {
		return source.Document{}, ErrMalformedDocument
	}
	document := makeDocument(opts.ObjectRef, ref, blocks)
	document.ParseWarnings = warnings
	return document, nil
}

type parseBlock struct {
	kind, text, language string
	level, start, end    int
	locator              source.Locator
}

// Supported reports whether Parse accepts a document with this name and
// content type.
func Supported(name, contentType string) bool { return formatFor(name, contentType) != "" }

func formatFor(name, contentType string) string {
	ext := strings.ToLower(filepath.Ext(name))
	media, _, _ := mime.ParseMediaType(strings.ToLower(strings.TrimSpace(contentType)))
	switch {
	case ext == ".md" || ext == ".markdown" || strings.Contains(media, "markdown"):
		return "markdown"
	case ext == ".html" || ext == ".htm" || media == "text/html":
		return "html"
	case ext == ".csv" || media == "text/csv":
		return "csv"
	case ext == ".json" || ext == ".jsonl" || ext == ".ndjson" || media == "application/json" || strings.HasSuffix(media, "+json") || strings.Contains(media, "ndjson"):
		return "json"
	case ext == ".docx" || strings.Contains(media, "wordprocessingml"):
		return "docx"
	case ext == ".xlsx" || media == "application/vnd.openxmlformats-officedocument.spreadsheetml.sheet":
		return "xlsx"
	case ext == ".epub" || media == "application/epub+zip":
		return "epub"
	case ext == ".pdf" || media == "application/pdf":
		return "pdf"
	case strings.HasPrefix(media, "image/") || ext == ".png" || ext == ".jpg" || ext == ".jpeg" || ext == ".tif" || ext == ".tiff":
		return "image"
	case ext == ".txt" || media == "text/plain" || media == "":
		return "text"
	default:
		return ""
	}
}

func decodeText(data []byte) ([]byte, string) {
	if utf8.Valid(data) {
		return data, ""
	}
	if len(data) >= 2 && ((data[0] == 0xff && data[1] == 0xfe) || (data[0] == 0xfe && data[1] == 0xff)) {
		little := data[0] == 0xff
		units := make([]uint16, 0, (len(data)-2)/2)
		for i := 2; i+1 < len(data); i += 2 {
			if little {
				units = append(units, uint16(data[i])|uint16(data[i+1])<<8)
			} else {
				units = append(units, uint16(data[i])<<8|uint16(data[i+1]))
			}
		}
		if len(data)%2 != 0 {
			units = append(units, 0xfffd)
		}
		return []byte(string(utf16.Decode(units))), "decoded as UTF-16: input was not valid UTF-8"
	}
	table := [...]rune{0x20ac, 0x81, 0x201a, 0x192, 0x201e, 0x2026, 0x2020, 0x2021, 0x2c6, 0x2030, 0x160, 0x2039, 0x152, 0x8d, 0x17d, 0x8f, 0x90, 0x2018, 0x2019, 0x201c, 0x201d, 0x2022, 0x2013, 0x2014, 0x2dc, 0x2122, 0x161, 0x203a, 0x153, 0x9d, 0x17e, 0x178}
	var b strings.Builder
	for _, c := range data {
		if c >= 0x80 && c <= 0x9f {
			b.WriteRune(table[c-0x80])
		} else {
			b.WriteRune(rune(c))
		}
	}
	return []byte(b.String()), "decoded as windows-1252: input was not valid UTF-8"
}

func makeDocument(ref source.ObjectRef, artifact artifacts.Ref, in []parseBlock) source.Document {
	out := source.Document{Ref: ref, Blocks: make([]source.Block, 0, len(in))}
	var headings [6]string
	for i, b := range in {
		h := sha256.Sum256([]byte(fmt.Sprintf("%s:%d:%d:%d:%s:%s", artifact.SHA256, i, b.start, b.end, b.kind, b.text)))
		id := "block_" + hex.EncodeToString(h[:12])
		parent := ""
		if b.kind == "heading" && b.level > 0 && b.level <= len(headings) {
			if b.level > 1 {
				parent = headings[b.level-2]
			}
			for j := b.level - 1; j < len(headings); j++ {
				headings[j] = ""
			}
			headings[b.level-1] = id
		} else {
			for j := len(headings) - 1; j >= 0; j-- {
				if headings[j] != "" {
					parent = headings[j]
					break
				}
			}
		}
		loc := b.locator
		loc.HeadingLevel = b.level
		loc.Language = b.language
		out.Blocks = append(out.Blocks, source.Block{ID: id, Ordinal: i, Kind: b.kind, ParentID: parent, Text: b.text, ByteStart: b.start, ByteEnd: b.end, Locator: loc})
	}
	return out
}

func plainBlocks(text string) []parseBlock {
	if strings.TrimSpace(text) == "" {
		return nil
	}
	start := strings.Index(text, strings.TrimSpace(text))
	body := strings.TrimSpace(text)
	return []parseBlock{{kind: "paragraph", text: body, start: start, end: start + len(body)}}
}

func markdownBlocks(text string) []parseBlock {
	var out []parseBlock
	for pos := 0; pos < len(text); {
		n := strings.IndexByte(text[pos:], '\n')
		if n < 0 {
			n = len(text) - pos
		} else {
			n++
		}
		end := pos + n
		line := strings.TrimSuffix(strings.TrimSuffix(text[pos:end], "\n"), "\r")
		trim := strings.TrimSpace(line)
		if trim == "" {
			pos = end
			continue
		}
		if strings.HasPrefix(trim, "```") || strings.HasPrefix(trim, "~~~") {
			fence, lang, start := trim[:3], strings.TrimSpace(trim[3:]), pos
			pos = end
			closed := false
			for pos < len(text) {
				n = strings.IndexByte(text[pos:], '\n')
				if n < 0 {
					n = len(text) - pos
				} else {
					n++
				}
				cand := strings.TrimSpace(strings.TrimSuffix(strings.TrimSuffix(text[pos:pos+n], "\n"), "\r"))
				pos += n
				if strings.HasPrefix(cand, fence) {
					closed = true
					break
				}
			}
			if !closed {
				return plainBlocks(text)
			}
			value := strings.TrimSpace(text[start:pos])
			out = append(out, parseBlock{kind: "code", text: value, language: lang, start: start, end: pos})
			continue
		}
		if strings.HasPrefix(trim, "#") {
			lvl := 0
			for lvl < len(trim) && trim[lvl] == '#' {
				lvl++
			}
			if lvl <= 6 && lvl < len(trim) && unicode.IsSpace(rune(trim[lvl])) {
				title := strings.TrimSpace(trim[lvl:])
				start := pos + strings.Index(text[pos:end], title)
				out = append(out, parseBlock{kind: "heading", text: title, level: lvl, start: start, end: start + len(title)})
				pos = end
				continue
			}
		}
		if markdownList(trim) {
			start := pos + strings.Index(text[pos:end], trim)
			out = append(out, parseBlock{kind: "list_item", text: trim, start: start, end: start + len(trim)})
			pos = end
			continue
		}
		start := pos
		for pos < len(text) {
			n = strings.IndexByte(text[pos:], '\n')
			if n < 0 {
				n = len(text) - pos
			} else {
				n++
			}
			candidate := strings.TrimSpace(strings.TrimSuffix(strings.TrimSuffix(text[pos:pos+n], "\n"), "\r"))
			if candidate == "" || strings.HasPrefix(candidate, "#") || strings.HasPrefix(candidate, "```") || strings.HasPrefix(candidate, "~~~") || markdownList(candidate) {
				break
			}
			pos += n
		}
		if pos == start {
			pos = end
		}
		body := strings.TrimSpace(text[start:pos])
		if body != "" {
			off := strings.Index(text[start:pos], body)
			out = append(out, parseBlock{kind: "paragraph", text: body, start: start + off, end: start + off + len(body)})
		}
	}
	return out
}
func markdownList(s string) bool {
	if len(s) > 1 && (s[0] == '-' || s[0] == '+' || s[0] == '*') && unicode.IsSpace(rune(s[1])) {
		return true
	}
	i := 0
	for i < len(s) && s[i] >= '0' && s[i] <= '9' {
		i++
	}
	return i > 0 && i+1 < len(s) && (s[i] == '.' || s[i] == ')') && unicode.IsSpace(rune(s[i+1]))
}

func htmlBlocks(text string) []parseBlock {
	z := htmltoken.NewTokenizer(strings.NewReader(text))
	var out []parseBlock
	offset := 0
	var active *parseBlock
	var body strings.Builder
	blocked := ""
	suspended := 0
	// Text outside a recognized block (<div>, table cells, …) is still
	// evidence: collect it and flush it as a paragraph at each block-level
	// boundary instead of dropping it.
	var stray strings.Builder
	strayStart := 0
	flushStray := func(end int) {
		if s := strings.TrimSpace(stray.String()); s != "" {
			out = append(out, parseBlock{kind: "paragraph", text: s, start: strayStart, end: end})
		}
		stray.Reset()
	}
	// closeActive ends the open block at end. Whitespace-only blocks (the
	// empty <p> spacers e-book converters emit) are dropped rather than
	// failing the whole member back to raw markup.
	closeActive := func(end int) {
		if active == nil {
			return
		}
		active.end = end
		active.text = strings.TrimSpace(body.String())
		if active.text != "" {
			out = append(out, *active)
		}
		active = nil
	}
	for {
		kind := z.Next()
		raw := z.Raw()
		end := offset + len(raw)
		if blocked != "" {
			if kind == htmltoken.ErrorToken {
				return nil
			}
			if kind == htmltoken.EndTagToken {
				tag, _ := z.TagName()
				if strings.EqualFold(string(tag), blocked) {
					blocked = ""
				}
			}
			offset = end
			continue
		}
		switch kind {
		case htmltoken.ErrorToken:
			if z.Err() == io.EOF && implicitlyClosed(active) {
				// HTML may omit the final </p> or </li>.
				closeActive(offset)
				flushStray(offset)
				return out
			}
			return nil
		case htmltoken.StartTagToken, htmltoken.SelfClosingTagToken:
			tag, _ := z.TagName()
			name := strings.ToLower(string(tag))
			switch name {
			case "head", "script", "style", "iframe", "object":
				if kind == htmltoken.StartTagToken {
					blocked = name
				}
				offset = end
				continue
			}
			if htmlBlockTag(name) {
				flushStray(offset)
			}
			blockKind, level := "", 0
			switch {
			case len(name) == 2 && name[0] == 'h' && name[1] >= '1' && name[1] <= '6':
				blockKind, level = "heading", int(name[1]-'0')
			case name == "p":
				blockKind = "paragraph"
			case name == "li":
				blockKind = "list_item"
			case name == "pre":
				blockKind = "code"
			}
			if blockKind != "" {
				if active != nil && active.kind == "list_item" {
					// A block inside <li> is nested, not a sibling
					// boundary: hold the item open so text after the
					// inner block still lands in it.
					suspended++
					closeActive(offset)
				} else {
					if !implicitlyClosed(active) {
						return nil
					}
					// A new block closes an open <p> or <li>, whose end
					// tags HTML lets authors omit.
					closeActive(offset)
				}
				body.Reset()
				active = &parseBlock{kind: blockKind, level: level, start: offset}
			}
		case htmltoken.TextToken:
			if active != nil {
				body.Write(z.Text())
			} else {
				if stray.Len() == 0 {
					strayStart = offset
				}
				stray.Write(z.Text())
			}
		case htmltoken.EndTagToken:
			tag, _ := z.TagName()
			name := strings.ToLower(string(tag))
			if htmlBlockTag(name) {
				flushStray(end)
			}
			matches := active != nil && ((active.kind == "heading" && name == fmt.Sprintf("h%d", active.level)) ||
				(active.kind == "paragraph" && name == "p") || (active.kind == "list_item" && name == "li") ||
				(active.kind == "code" && name == "pre"))
			if matches {
				closeActive(end)
				if suspended > 0 {
					suspended--
					body.Reset()
					active = &parseBlock{kind: "list_item", start: end}
				}
			}
		}
		offset = end
	}
}

// implicitlyClosed reports whether the open block may end without its end
// tag: <p> and <li> (HTML makes those optional); an unterminated heading or
// <pre> is malformed and keeps the plain-text fallback.
func implicitlyClosed(active *parseBlock) bool {
	return active == nil || active.kind == "paragraph" || active.kind == "list_item"
}

// htmlText is a markup member's tag-stripped body text — the EPUB fallback
// for members with no paragraph structure, so markup is never indexed.
func htmlText(text string) string {
	z := htmltoken.NewTokenizer(strings.NewReader(text))
	var b strings.Builder
	skip := ""
	// sep keeps one blank line between blocks; inline runs stay joined.
	sep := func() {
		if s := b.String(); strings.TrimSpace(s) != "" && !strings.HasSuffix(s, "\n\n") {
			b.WriteString("\n\n")
		}
	}
	for {
		switch z.Next() {
		case htmltoken.ErrorToken:
			return b.String()
		case htmltoken.StartTagToken, htmltoken.SelfClosingTagToken:
			tag, _ := z.TagName()
			name := strings.ToLower(string(tag))
			switch {
			case skip == "" && (name == "head" || name == "script" || name == "style"):
				skip = name
			case skip == "" && htmlBlockTag(name):
				sep()
			}
		case htmltoken.EndTagToken:
			tag, _ := z.TagName()
			name := strings.ToLower(string(tag))
			switch {
			case strings.EqualFold(name, skip):
				skip = ""
			case skip == "" && htmlBlockTag(name):
				sep()
			}
		case htmltoken.TextToken:
			if skip == "" {
				b.Write(z.Text())
			}
		}
	}
}

// htmlBlockTag reports whether tag opens or closes a block-level element —
// where stripped text gets a blank line — as opposed to inline markup that
// must not split a text run (INV-<span>123</span>-A stays INV-123-A).
func htmlBlockTag(name string) bool {
	switch name {
	case "p", "div", "section", "article", "header", "footer", "main", "aside",
		"nav", "blockquote", "pre", "ul", "ol", "li", "dl", "dt", "dd",
		"table", "thead", "tbody", "tfoot", "tr", "td", "th", "caption",
		"form", "fieldset", "figure", "figcaption", "hr", "br", "body":
		return true
	}
	return len(name) == 2 && name[0] == 'h' && name[1] >= '1' && name[1] <= '6'
}

func csvBlocks(data []byte) ([]parseBlock, error) {
	r := csv.NewReader(bytes.NewReader(data))
	r.FieldsPerRecord = -1
	var out []parseBlock
	start, row := 0, 1
	header := false
	for {
		record, err := r.Read()
		if err == io.EOF {
			break
		}
		if err != nil {
			return nil, fmt.Errorf("%w: CSV: %v", ErrMalformedDocument, err)
		}
		end := int(r.InputOffset())
		text := strings.Join(record, "\t")
		if strings.TrimSpace(text) == "" {
			start, row = end, row+1
			continue
		}
		kind := "table_row"
		if !header {
			kind, header = "table_header", true
		}
		out = append(out, parseBlock{kind: kind, text: text, start: start, end: end, locator: source.Locator{Table: "csv", Clause: fmt.Sprint(row)}})
		start, row = end, row+1
	}
	return out, nil
}

func jsonBlocks(data []byte, lines bool) ([]parseBlock, error) {
	var out []parseBlock
	appendValue := func(raw []byte, start int) error {
		dec := json.NewDecoder(bytes.NewReader(raw))
		dec.UseNumber()
		var value any
		if err := dec.Decode(&value); err != nil {
			return err
		}
		var extra any
		if err := dec.Decode(&extra); err != io.EOF {
			return errors.New("multiple JSON values")
		}
		var b bytes.Buffer
		enc := json.NewEncoder(&b)
		enc.SetEscapeHTML(false)
		if err := enc.Encode(value); err != nil {
			return err
		}
		text := strings.TrimSpace(b.String())
		out = append(out, parseBlock{kind: "json", text: text, start: start, end: start + len(raw)})
		return nil
	}
	if lines {
		offset := 0
		for _, line := range bytes.Split(data, []byte("\n")) {
			trimmed := bytes.TrimSpace(line)
			if len(trimmed) > 0 {
				start := offset + bytes.Index(line, trimmed)
				if err := appendValue(trimmed, start); err != nil {
					return nil, fmt.Errorf("%w: JSONL: %v", ErrMalformedDocument, err)
				}
			}
			offset += len(line) + 1
		}
		return out, nil
	}
	if err := appendValue(data, 0); err != nil {
		return nil, fmt.Errorf("%w: JSON: %v", ErrMalformedDocument, err)
	}
	return out, nil
}

func docxBlocks(data []byte) ([]parseBlock, error) {
	zr, err := zip.NewReader(bytes.NewReader(data), int64(len(data)))
	if err != nil {
		return nil, fmt.Errorf("%w: DOCX zip: %v", ErrMalformedDocument, err)
	}
	if len(zr.File) > 4096 {
		return nil, ErrDocumentTooLarge
	}
	files := append([]*zip.File(nil), zr.File...)
	sort.SliceStable(files, func(i, j int) bool {
		priority := func(name string) int {
			switch {
			case name == "word/document.xml":
				return 0
			case strings.HasPrefix(name, "word/header"):
				return 1
			case strings.HasPrefix(name, "word/footer"):
				return 2
			case strings.HasPrefix(name, "word/footnotes"):
				return 3
			case strings.HasPrefix(name, "word/endnotes"):
				return 4
			default:
				return 5
			}
		}
		pi, pj := priority(files[i].Name), priority(files[j].Name)
		if pi != pj {
			return pi < pj
		}
		return files[i].Name < files[j].Name
	})
	var blocks []parseBlock
	expanded := int64(0)
	for _, file := range files {
		name := file.Name
		if !strings.HasPrefix(name, "word/") || !strings.HasSuffix(name, ".xml") ||
			(name != "word/document.xml" && !strings.HasPrefix(name, "word/footnotes") &&
				!strings.HasPrefix(name, "word/endnotes") && !strings.HasPrefix(name, "word/header") &&
				!strings.HasPrefix(name, "word/footer")) {
			continue
		}
		if file.UncompressedSize64 > uint64(maxExpandedBytes-expanded) {
			return nil, ErrDocumentTooLarge
		}
		rc, err := file.Open()
		if err != nil {
			return nil, err
		}
		raw, readErr := io.ReadAll(io.LimitReader(rc, maxExpandedBytes-expanded+1))
		_ = rc.Close()
		if readErr != nil {
			return nil, readErr
		}
		expanded += int64(len(raw))
		if expanded > maxExpandedBytes {
			return nil, ErrDocumentTooLarge
		}
		part, err := parseDocxPart(raw, name)
		if err != nil {
			return nil, fmt.Errorf("%w: DOCX XML: %v", ErrMalformedDocument, err)
		}
		blocks = append(blocks, part...)
	}
	offset := 0
	for i := range blocks {
		blocks[i].start = offset
		blocks[i].end = offset + len(blocks[i].text)
		offset = blocks[i].end + 1
	}
	return blocks, nil
}

func parseDocxPart(raw []byte, part string) ([]parseBlock, error) {
	decoder := xml.NewDecoder(bytes.NewReader(raw))
	var blocks []parseBlock
	var paragraph, cell strings.Builder
	var cells []string
	inParagraph, inCell := false, false
	style, numID, level, noteID := "", "", "", ""
	noteType := ""
	if strings.HasPrefix(part, "word/footnotes") {
		noteType = "footnote"
	} else if strings.HasPrefix(part, "word/endnotes") {
		noteType = "endnote"
	}
	var noteRefs []string
	row := 0
	for {
		token, err := decoder.Token()
		if err == io.EOF {
			return blocks, nil
		}
		if err != nil {
			return nil, err
		}
		switch t := token.(type) {
		case xml.StartElement:
			switch t.Name.Local {
			case "tr":
				cells = cells[:0]
			case "tc":
				inCell = true
				cell.Reset()
			case "p":
				inParagraph = true
				paragraph.Reset()
				style, numID, level = "", "", ""
				noteRefs = noteRefs[:0]
			case "pStyle":
				style = xmlAttr(t.Attr, "val")
			case "numId":
				numID = xmlAttr(t.Attr, "val")
			case "ilvl":
				level = xmlAttr(t.Attr, "val")
			case "footnoteReference":
				noteRefs = append(noteRefs, "footnote:"+xmlAttr(t.Attr, "id"))
			case "endnoteReference":
				noteRefs = append(noteRefs, "endnote:"+xmlAttr(t.Attr, "id"))
			case "footnote", "endnote":
				noteID = xmlAttr(t.Attr, "id")
			}
		case xml.CharData:
			if inParagraph {
				paragraph.Write([]byte(t))
			}
		case xml.EndElement:
			switch t.Name.Local {
			case "p":
				text := paragraph.String()
				if inCell {
					if cell.Len() > 0 {
						cell.WriteByte(' ')
					}
					cell.WriteString(text)
				} else if strings.TrimSpace(text) != "" {
					kind, headingLevel := "paragraph", 0
					if strings.HasPrefix(strings.ToLower(style), "heading") {
						headingLevel, _ = strconv.Atoi(strings.TrimPrefix(strings.ToLower(style), "heading"))
						if headingLevel >= 1 && headingLevel <= 6 {
							kind = "heading"
						}
					} else if numID != "" || strings.Contains(strings.ToLower(style), "list") {
						kind = "list_item"
					}
					var clauses []string
					if numID != "" {
						clauses = append(clauses, "numId="+numID+";ilvl="+level)
					}
					if noteType != "" && noteID != "" {
						clauses = append(clauses, noteType+":"+noteID)
					}
					clauses = append(clauses, noteRefs...)
					blocks = append(blocks, parseBlock{kind: kind, text: text, end: len(text), level: headingLevel, locator: source.Locator{Clause: strings.Join(clauses, ",")}})
				}
				inParagraph = false
			case "tc":
				cells = append(cells, cell.String())
				inCell = false
			case "tr":
				row++
				blocks = append(blocks, parseBlock{kind: "table_row", text: strings.Join(cells, "\t"), locator: source.Locator{Table: part, Clause: fmt.Sprint(row)}})
			}
		}
	}
}

func xmlAttr(attrs []xml.Attr, name string) string {
	for _, attr := range attrs {
		if attr.Name.Local == name {
			return attr.Value
		}
	}
	return ""
}

// epubBlocks extracts reading-ordered blocks from an EPUB: container.xml
// locates the OPF, the OPF spine orders the XHTML members, and each member
// is parsed as HTML. Books with an unusable package fall back to name-sorted
// XHTML members — order is lost, content is not.
func epubBlocks(data []byte) ([]parseBlock, error) {
	zr, err := zip.NewReader(bytes.NewReader(data), int64(len(data)))
	if err != nil {
		return nil, fmt.Errorf("%w: EPUB zip: %v", ErrMalformedDocument, err)
	}
	if len(zr.File) > 4096 {
		return nil, ErrDocumentTooLarge
	}
	names := epubSpine(zr)
	var blocks []parseBlock
	expanded := int64(0)
	for _, name := range names {
		file := epubMember(zr, name)
		if file == nil {
			continue
		}
		if file.UncompressedSize64 > uint64(maxExpandedBytes-expanded) {
			return nil, ErrDocumentTooLarge
		}
		rc, err := file.Open()
		if err != nil {
			return nil, err
		}
		raw, readErr := io.ReadAll(io.LimitReader(rc, maxExpandedBytes-expanded+1))
		_ = rc.Close()
		if readErr != nil {
			return nil, readErr
		}
		expanded += int64(len(raw))
		if expanded > maxExpandedBytes {
			return nil, ErrDocumentTooLarge
		}
		part := htmlBlocks(string(raw))
		if part == nil {
			part = plainBlocks(htmlText(string(raw)))
		}
		for i := range part {
			if part[i].locator.Section == "" {
				part[i].locator.Section = name
			}
		}
		blocks = append(blocks, part...)
	}
	offset := 0
	for i := range blocks {
		blocks[i].start = offset
		blocks[i].end = offset + len(blocks[i].text)
		offset = blocks[i].end + 1
	}
	return blocks, nil
}

// epubSpine resolves container.xml → OPF → spine itemrefs to XHTML member
// names in reading order.
func epubSpine(zr *zip.Reader) []string {
	opfPath := ""
	if raw, ok := epubReadMember(zr, "META-INF/container.xml", 1<<20); ok {
		decoder := xml.NewDecoder(bytes.NewReader(raw))
		for {
			token, err := decoder.Token()
			if err != nil {
				break
			}
			if t, ok := token.(xml.StartElement); ok && t.Name.Local == "rootfile" {
				if p := xmlAttr(t.Attr, "full-path"); p != "" {
					opfPath = p
				}
				break
			}
		}
	}
	if opfPath == "" {
		return epubFallbackNames(zr)
	}
	opf, ok := epubReadMember(zr, opfPath, 4<<20)
	if !ok {
		return epubFallbackNames(zr)
	}
	base := path.Dir(opfPath)
	manifest := map[string]string{}
	var spine []string
	decoder := xml.NewDecoder(bytes.NewReader(opf))
	inManifest, inSpine := false, false
	for {
		token, err := decoder.Token()
		if err != nil {
			break
		}
		switch t := token.(type) {
		case xml.StartElement:
			switch t.Name.Local {
			case "manifest":
				inManifest = true
			case "spine":
				inSpine = true
			case "item":
				if !inManifest {
					break
				}
				href := xmlAttr(t.Attr, "href")
				media := xmlAttr(t.Attr, "media-type")
				if id := xmlAttr(t.Attr, "id"); id != "" && href != "" &&
					(media == "application/xhtml+xml" || media == "text/html") {
					if unescaped, err := url.PathUnescape(href); err == nil {
						href = unescaped
					}
					manifest[id] = path.Join(base, href)
				}
			case "itemref":
				if inSpine {
					if name, ok := manifest[xmlAttr(t.Attr, "idref")]; ok {
						spine = append(spine, name)
					}
				}
			}
		case xml.EndElement:
			switch t.Name.Local {
			case "manifest":
				inManifest = false
			case "spine":
				inSpine = false
			}
		}
	}
	if len(spine) == 0 {
		return epubFallbackNames(zr)
	}
	return spine
}

// epubFallbackNames returns XHTML members in name order when the package
// metadata is missing or unreadable.
func epubFallbackNames(zr *zip.Reader) []string {
	var names []string
	for _, file := range zr.File {
		n := strings.ToLower(file.Name)
		if strings.HasSuffix(n, ".xhtml") || strings.HasSuffix(n, ".html") || strings.HasSuffix(n, ".htm") {
			names = append(names, file.Name)
		}
	}
	sort.Strings(names)
	return names
}

func epubMember(zr *zip.Reader, name string) *zip.File {
	for _, file := range zr.File {
		if file.Name == name {
			return file
		}
	}
	for _, file := range zr.File { // lenient: some producers vary case
		if strings.EqualFold(file.Name, name) {
			return file
		}
	}
	return nil
}

func epubReadMember(zr *zip.Reader, name string, limit int64) ([]byte, bool) {
	file := epubMember(zr, name)
	if file == nil || limit < 0 || file.UncompressedSize64 > uint64(limit) {
		return nil, false
	}
	rc, err := file.Open()
	if err != nil {
		return nil, false
	}
	defer func() { _ = rc.Close() }()
	raw, err := io.ReadAll(io.LimitReader(rc, limit+1))
	if err != nil || int64(len(raw)) > limit {
		return nil, false
	}
	return raw, true
}

func (p *Parser) parseNative(ctx context.Context, ref artifacts.Ref, opts ParseOptions, data []byte, format string) (source.Document, error) {
	return parseNative(ctx, ref, opts, data, format)
}
