package rag

import (
	"io"
	"path/filepath"
	"strings"
	"unicode"
	"unicode/utf8"

	"golang.org/x/net/html"
)

type parsedBlock struct {
	Kind      string
	Text      string
	Level     int
	Language  string
	ByteStart int
	ByteEnd   int
}

type structuredChunk struct {
	Text         string
	ByteStart    int
	ByteEnd      int
	BlockIndexes []int
	HeaderPath   string
}

func structuredFormat(fileName, mimeType string) string {
	mimeType = strings.ToLower(strings.TrimSpace(mimeType))
	switch strings.ToLower(filepath.Ext(fileName)) {
	case ".md", ".markdown":
		return "markdown"
	case ".html", ".htm":
		return "html"
	}
	if strings.Contains(mimeType, "markdown") {
		return "markdown"
	}
	if strings.Contains(mimeType, "html") {
		return "html"
	}
	return ""
}

func parseStructured(text, format string) []parsedBlock {
	var blocks []parsedBlock
	if format == "html" {
		blocks = parseHTMLBlocks(text)
	} else {
		blocks = parseMarkdownBlocks(text)
	}
	if len(blocks) == 0 {
		return []parsedBlock{{Kind: "paragraph", Text: strings.TrimSpace(text), ByteStart: 0, ByteEnd: len(text)}}
	}
	return blocks
}

func parseMarkdownBlocks(text string) []parsedBlock {
	var blocks []parsedBlock
	for pos := 0; pos < len(text); {
		n := strings.IndexByte(text[pos:], '\n')
		if n < 0 {
			n = len(text) - pos
		} else {
			n++
		}
		lineEnd := pos + n
		line := strings.TrimSuffix(strings.TrimSuffix(text[pos:lineEnd], "\n"), "\r")
		trim := strings.TrimSpace(line)
		if trim == "" {
			pos = lineEnd
			continue
		}
		if strings.HasPrefix(trim, "```") || strings.HasPrefix(trim, "~~~") {
			fence, language, start := trim[:3], strings.TrimSpace(trim[3:]), pos
			pos = lineEnd
			closed := false
			for pos < len(text) {
				n = strings.IndexByte(text[pos:], '\n')
				if n < 0 {
					n = len(text) - pos
				} else {
					n++
				}
				candidate := strings.TrimSpace(strings.TrimSuffix(strings.TrimSuffix(text[pos:pos+n], "\n"), "\r"))
				pos += n
				if strings.HasPrefix(candidate, fence) {
					closed = true
					break
				}
			}
			if !closed {
				return nil
			}
			blocks = append(blocks, parsedBlock{Kind: "code", Text: strings.TrimSpace(text[start:pos]), Language: language, ByteStart: start, ByteEnd: pos})
			continue
		}
		if strings.HasPrefix(trim, "#") {
			level := 0
			for level < len(trim) && trim[level] == '#' {
				level++
			}
			if level <= 6 && level < len(trim) && unicode.IsSpace(rune(trim[level])) {
				title := strings.TrimSpace(trim[level:])
				start := pos + strings.Index(text[pos:lineEnd], title)
				blocks = append(blocks, parsedBlock{Kind: "heading", Text: title, Level: level, ByteStart: start, ByteEnd: start + len(title)})
				pos = lineEnd
				continue
			}
		}
		if isMarkdownList(trim) {
			start := pos + strings.Index(text[pos:lineEnd], trim)
			blocks = append(blocks, parsedBlock{Kind: "list_item", Text: trim, ByteStart: start, ByteEnd: start + len(trim)})
			pos = lineEnd
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
			if candidate == "" || strings.HasPrefix(candidate, "#") || strings.HasPrefix(candidate, "```") || strings.HasPrefix(candidate, "~~~") || isMarkdownList(candidate) {
				break
			}
			pos += n
		}
		if pos == start {
			pos = lineEnd
		}
		body := strings.TrimSpace(text[start:pos])
		if body != "" {
			first := strings.Index(text[start:pos], body)
			blocks = append(blocks, parsedBlock{Kind: "paragraph", Text: body, ByteStart: start + first, ByteEnd: start + first + len(body)})
		}
	}
	return blocks
}

func isMarkdownList(s string) bool {
	if len(s) > 1 && (s[0] == '-' || s[0] == '+' || s[0] == '*') && unicode.IsSpace(rune(s[1])) {
		return true
	}
	i := 0
	for i < len(s) && s[i] >= '0' && s[i] <= '9' {
		i++
	}
	return i > 0 && i+1 < len(s) && (s[i] == '.' || s[i] == ')') && unicode.IsSpace(rune(s[i+1]))
}

func parseHTMLBlocks(text string) []parsedBlock {
	z := html.NewTokenizer(strings.NewReader(text))
	var blocks []parsedBlock
	var blockText strings.Builder
	offset := 0
	var active *parsedBlock
	for {
		kind := z.Next()
		raw := z.Raw()
		end := offset + len(raw)
		switch kind {
		case html.ErrorToken:
			if z.Err() == io.EOF && active == nil {
				return blocks
			}
			return nil
		case html.StartTagToken:
			tag, _ := z.TagName()
			name := strings.ToLower(string(tag))
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
				if active != nil {
					return nil
				}
				blockText.Reset()
				active = &parsedBlock{Kind: blockKind, Level: level, ByteStart: offset}
			}
		case html.TextToken:
			if active != nil {
				blockText.Write(z.Text())
			}
		case html.EndTagToken:
			tag, _ := z.TagName()
			name := strings.ToLower(string(tag))
			if active != nil && ((active.Kind == "heading" && name == "h"+string(rune('0'+active.Level))) || (active.Kind == "paragraph" && name == "p") || (active.Kind == "list_item" && name == "li") || (active.Kind == "code" && name == "pre")) {
				active.ByteEnd = end
				active.Text = strings.TrimSpace(blockText.String())
				if active.Text == "" {
					return nil
				}
				blocks = append(blocks, *active)
				active = nil
			}
		}
		offset = end
	}
}

func chunkStructured(blocks []parsedBlock, size, overlap int) []structuredChunk {
	if size <= 0 {
		size = DefaultChunkSize
	}
	if overlap < 0 || overlap >= size {
		overlap = 0
	}
	var out []structuredChunk
	var current structuredChunk
	headers := make([]string, 0, 6)
	flush := func() {
		if current.Text != "" {
			current.HeaderPath = strings.Join(headers, " › ")
			out = append(out, current)
			current = structuredChunk{}
		}
	}
	for i, block := range blocks {
		if block.Kind == "heading" {
			flush()
			if block.Level < 1 || block.Level > 6 {
				return nil
			}
			headers = headers[:block.Level-1]
			headers = append(headers, block.Text)
		}
		add := block.Text
		if current.Text != "" {
			add = "\n" + add
		}
		if current.Text != "" && utf8.RuneCountInString(current.Text)+utf8.RuneCountInString(add) > size {
			flush()
			if overlap > 0 && block.Kind != "heading" && len(out) > 0 {
				budget := overlap
				if remaining := size - utf8.RuneCountInString(block.Text) - 1; budget > remaining {
					budget = remaining
				}
				if budget > 0 {
					last := out[len(out)-1].BlockIndexes
					var indexes []int
					for n := len(last) - 1; n >= 0; n-- {
						count := utf8.RuneCountInString(blocks[last[n]].Text)
						if count > budget {
							break
						}
						indexes = append(indexes, last[n])
						budget -= count + 1
					}
					for n := len(indexes) - 1; n >= 0; n-- {
						idx := indexes[n]
						if current.Text == "" {
							current.ByteStart = blocks[idx].ByteStart
						} else {
							current.Text += "\n"
						}
						current.Text += blocks[idx].Text
						current.ByteEnd = blocks[idx].ByteEnd
						current.BlockIndexes = append(current.BlockIndexes, idx)
					}
				}
			}
			add = block.Text
			if current.Text != "" {
				add = "\n" + add
			}
		}
		if current.Text == "" {
			current.ByteStart = block.ByteStart
		}
		current.Text += add
		current.ByteEnd = block.ByteEnd
		current.BlockIndexes = append(current.BlockIndexes, i)
	}
	flush()
	return out
}
