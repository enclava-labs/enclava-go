package rag

import (
	"strings"
	"unicode/utf8"
)

type chunkBlock struct {
	Kind      string
	Text      string
	Level     int
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

func chunkStructured(blocks []chunkBlock, size, overlap int) []structuredChunk {
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
			headerPath := make([]string, 0, len(headers))
			for _, header := range headers {
				if header != "" {
					headerPath = append(headerPath, header)
				}
			}
			current.HeaderPath = strings.Join(headerPath, " › ")
			out = append(out, current)
			current = structuredChunk{}
		}
	}
	for i, block := range blocks {
		if block.Kind == "heading" {
			if block.Level < 1 || block.Level > 6 {
				return nil
			}
			for len(headers) < block.Level-1 {
				headers = append(headers, "")
			}
			flush()
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

func tokenEstimate(text string) int {
	if text == "" {
		return 0
	}
	return (utf8.RuneCountInString(text) + 3) / 4
}
