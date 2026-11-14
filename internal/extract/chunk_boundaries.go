package extract

import (
	"strings"
	"unicode"
	"unicode/utf8"
)

// Sentence-boundary helpers for the "auto" boundary strategy. They are
// deliberately simple and deterministic: perfect segmentation is not needed,
// only a strong preference for sentence ends over mid-word cuts.

var sentenceAbbreviations = map[string]bool{
	"e.g": true, "i.e": true, "mr": true, "mrs": true, "dr": true,
	"vs": true, "etc": true, "no": true, "fig": true,
}

func isASCIILetter(b byte) bool {
	return b >= 'a' && b <= 'z' || b >= 'A' && b <= 'Z'
}

func isSentenceTerminator(r rune) bool {
	switch r {
	case '.', '!', '?', '…', '。', '！', '？':
		return true
	}
	return false
}

func isCJKTerminator(r rune) bool {
	return r == '。' || r == '！' || r == '？'
}

func isSentenceCloser(r rune) bool {
	switch r {
	case '"', '\'', ')', ']', '}', '”', '’', '»', '」', '』', '】', '）':
		return true
	}
	return false
}

// sentenceEnds returns byte offsets in text just after each sentence end: a
// terminator rune optionally followed by closing quotes/brackets and then
// whitespace or the end of text, plus the offset after every newline.
func sentenceEnds(text string) []int {
	var ends []int
	for i := 0; i < len(text); {
		r, size := utf8.DecodeRuneInString(text[i:])
		switch {
		case r == '\n':
			ends = append(ends, i+1)
		case isSentenceTerminator(r):
			j := i + size
			for j < len(text) {
				c, csize := utf8.DecodeRuneInString(text[j:])
				if !isSentenceCloser(c) {
					break
				}
				j += csize
			}
			if endsSentence(text, i, j) {
				ends = append(ends, j)
			}
		}
		i += size
	}
	return ends
}

// endsSentence reports whether the terminator at termStart, whose trailing
// closers end at after, really closes a sentence in text.
func endsSentence(text string, termStart, after int) bool {
	if after >= len(text) {
		return true
	}
	next, _ := utf8.DecodeRuneInString(text[after:])
	term, _ := utf8.DecodeRuneInString(text[termStart:])
	if isCJKTerminator(term) {
		return !unicode.IsDigit(next)
	}
	if !unicode.IsSpace(next) {
		return false // "file.go", "3.14a", "Mr.Smith", lowercase continuation
	}
	if term != '.' {
		return true
	}
	if termStart == 0 {
		return false
	}
	prev := text[termStart-1]
	if prev >= '0' && prev <= '9' {
		return false // decimals and versions: 3.14, v1.2
	}
	if !isASCIILetter(prev) {
		return true // e.g. "..." or "end)."
	}
	word := strings.ToLower(asciiWord(text[:termStart]))
	if sentenceAbbreviations[word] || dottedInitials(word) || len(word) == 1 {
		return false // e.g. i.e, U.S, single-letter initials
	}
	return true
}

// asciiWord returns the trailing run of ASCII letters and dots in text.
func asciiWord(text string) string {
	i := len(text)
	for i > 0 {
		c := text[i-1]
		if !isASCIILetter(c) && c != '.' {
			break
		}
		i--
	}
	return text[i:]
}

// dottedInitials reports whether word looks like "u.s": single ASCII letters
// joined by dots.
func dottedInitials(word string) bool {
	if len(word) < 3 || len(word)%2 == 0 {
		return false
	}
	for i := range len(word) {
		if i%2 == 0 && !isASCIILetter(word[i]) || i%2 == 1 && word[i] != '.' {
			return false
		}
	}
	return true
}

// splitPoint returns the best byte offset to end a piece of text that starts
// at start and may hold at most maxRunes code points, preferring paragraph,
// sentence, line and word boundaries over a hard cut and never producing a
// piece shorter than ~40% of maxRunes unless a hard cut is the only option.
// clean is false only for a hard mid-word cut.
func splitPoint(text string, start, maxRunes int) (end int, clean bool) {
	if maxRunes < 1 {
		maxRunes = 1
	}
	hard := boundedEnd(text, start, maxRunes)
	if hard >= len(text) {
		return len(text), true
	}
	window := text[start:hard]
	minRunes := maxRunes * 2 / 5
	fits := func(off int) bool {
		return off > 0 && utf8.RuneCountInString(window[:off]) >= minRunes
	}
	if i := strings.LastIndex(window, "\n\n"); i >= 0 && fits(i+2) {
		return start + i + 2, true
	}
	best := 0
	for _, e := range sentenceEnds(window) {
		if e > best {
			best = e
		}
	}
	if best > 0 {
		off := best
		if off < len(window) && (window[off] == ' ' || window[off] == '\t') {
			off++ // consume the separating space so the next piece starts clean
		}
		if fits(off) {
			return start + off, true
		}
	}
	if i := strings.LastIndexByte(window, '\n'); i >= 0 && fits(i+1) {
		return start + i + 1, true
	}
	if i := strings.LastIndexAny(window, " \t"); i >= 0 && fits(i+1) {
		return start + i + 1, true
	}
	if r, _ := utf8.DecodeRuneInString(text[hard:]); unicode.IsSpace(r) {
		return hard, true // the cut lands between words
	}
	return hard, false
}

// overlapStart returns the byte offset where the next piece should start so
// that it repeats whole trailing sentences of text[:end] totalling at most
// overlapRunes code points. It falls back to a word boundary when the last
// sentence alone exceeds the budget, and always makes progress (< end).
func overlapStart(text string, end, overlapRunes int) int {
	if overlapRunes <= 0 || end <= 0 {
		return end
	}
	// Earlier starts repeat more trailing sentences; counts shrink as the
	//	start advances, so the first fitting start is the maximal set.
	best, found := 0, false
	for _, e := range sentenceEnds(text[:end]) {
		s := skipSpace(text, e, end)
		if s >= end {
			continue
		}
		if utf8.RuneCountInString(text[s:end]) <= overlapRunes {
			best, found = s, true
			break
		}
	}
	if found {
		return best
	}
	// The last sentence alone is longer than the budget: repeat from a word
	// boundary near the end instead.
	os := previousRuneOffset(text, end, overlapRunes)
	if os < 0 {
		os = 0
	}
	if i := strings.LastIndexAny(text[:os], " \t\n"); i >= 0 {
		os = i + 1
	} else {
		os = 0
	}
	if os >= end {
		return end
	}
	return os
}

func skipSpace(text string, i, end int) int {
	for i < end {
		r, size := utf8.DecodeRuneInString(text[i:])
		if !unicode.IsSpace(r) {
			break
		}
		i += size
	}
	return i
}
