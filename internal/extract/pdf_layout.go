package extract

import (
	"fmt"
	"strconv"
	"strings"
	"unicode"
	"unicode/utf8"
)

// maxParagraphRunes caps one paragraph block so a layout misdetection
// cannot create a giant block.
const maxParagraphRunes = 4000

// pdfBox is a line's bounding box in absolute coordinates, whatever the
// source format; sized inputs are converted on parse.
type pdfBox struct{ x0, y0, x1, y1 float64 }

// mergeParagraphLines joins consecutive line blocks (one per text line, as
// produced by mergeLineWords) into one block per paragraph. Thresholds are
// relative to line height so they work for both Poppler edge boxes
// (xMin,yMin,xMax,yMax) and Tesseract sized boxes (left,top,width,height);
// sized marks the latter. Lines whose box cannot be parsed pass through.
// Offsets are renumbered like mergeLineWords does.

func mergeParagraphLines(lines []parseBlock, sized bool) []parseBlock {
	parse := func(raw string) (pdfBox, bool) {
		parts := strings.Split(raw, ",")
		if len(parts) != 4 {
			return pdfBox{}, false
		}
		var v [4]float64
		for i, part := range parts {
			f, err := strconv.ParseFloat(part, 64)
			if err != nil {
				return pdfBox{}, false
			}
			v[i] = f
		}
		if sized {
			return pdfBox{v[0], v[1], v[0] + v[2], v[1] + v[3]}, true
		}
		return pdfBox{v[0], v[1], v[2], v[3]}, true
	}

	out := make([]parseBlock, 0, len(lines)/4+1)
	var para, last pdfBox
	open := false // out's last block is a paragraph that can take more lines
	for _, line := range lines {
		next, ok := parse(line.locator.Box)
		if !ok {
			out = append(out, line)
			open = false
			continue
		}
		if open && samePageNum(out[len(out)-1], line) && joinable(para, last, next, line.text) &&
			utf8.RuneCountInString(out[len(out)-1].text)+1+utf8.RuneCountInString(line.text) <= maxParagraphRunes {
			p := &out[len(out)-1]
			p.text = joinText(p.text, line.text)
			para = pdfBox{min(para.x0, next.x0), min(para.y0, next.y0), max(para.x1, next.x1), max(para.y1, next.y1)}
			if sized {
				p.locator.Box = fmt.Sprintf("%.0f,%.0f,%.0f,%.0f", para.x0, para.y0, para.x1-para.x0, para.y1-para.y0)
			} else {
				p.locator.Box = fmt.Sprintf("%f,%f,%f,%f", para.x0, para.y0, para.x1, para.y1)
			}
			last = next
			continue
		}
		out = append(out, line)
		para, last, open = next, next, next.y1 > next.y0
	}
	offset := 0
	for i := range out {
		out[i].start, out[i].end = offset, offset+len(out[i].text)
		offset = out[i].end + 1
	}
	return out
}

func samePageNum(a, b parseBlock) bool {
	return a.locator.Page != nil && b.locator.Page != nil && *a.locator.Page == *b.locator.Page
}

// joinable reports whether the line at next continues the paragraph whose
// union box is para and whose last line box is last.
func joinable(para, last, next pdfBox, text string) bool {
	lh := last.y1 - last.y0
	nh := next.y1 - next.y0
	if lh <= 0 || nh <= 0 {
		return false
	}
	// The next line sits directly below the previous one, never above: a
	// jump up means a new column, a large gap a new paragraph.
	if next.y0 < last.y1-0.5*lh || next.y0-last.y1 > 0.6*lh {
		return false
	}
	// Similar line height: a heading in a bigger font stays its own block.
	if nh > lh*1.25 || lh > nh*1.25 {
		return false
	}
	// Horizontal overlap with the paragraph's column.
	if min(para.x1, next.x1)-max(para.x0, next.x0) <= 0 {
		return false
	}
	// List items start a new paragraph.
	if listItemStart(text) {
		return false
	}
	// A first-line indent starts a new paragraph when the previous line
	// reached roughly the paragraph's right edge.
	if next.x0-last.x0 >= lh && last.x1 >= para.x1-0.15*(para.x1-para.x0) {
		return false
	}
	// A previous line that ends clearly short of the paragraph's right
	// edge is a "short last line" and ends the paragraph.
	if last.x1 < para.x1-0.2*(para.x1-para.x0) && para.x1-last.x0 > 4*lh {
		return false
	}
	return true
}

var listBullets = []string{"•", "–", "—", "-", "*", "▪", "◦", "‣"}

func listItemStart(text string) bool {
	t := strings.TrimLeft(text, " \t")
	for _, b := range listBullets {
		if strings.HasPrefix(t, b) {
			return true
		}
	}
	r, _ := utf8.DecodeRuneInString(t)
	if unicode.IsDigit(r) {
		i := 1
		for i < len(t) && t[i] >= '0' && t[i] <= '9' {
			i++
		}
		if i < len(t) && (t[i] == '.' || t[i] == ')') {
			return true
		}
	}
	if r == '(' && len(t) > 2 {
		if (unicode.IsLetter(rune(t[1])) || t[1] >= '0' && t[1] <= '9') && t[2] == ')' {
			return true
		}
	}
	if unicode.IsLower(r) && len(t) > 1 && (t[1] == ')' || t[1] == '.') {
		return true
	}
	return false
}

// joinText joins two line texts, handling hyphenation across the break.
// A letter followed by '-' or U+00AD at the end of the previous line joins
// the next line's text without a space when the next line starts lowercase;
// with an uppercase letter or digit the hyphen is kept ("Anglo-American").
// U+00AD is always dropped.
func joinText(prev, next string) string {
	prev = strings.TrimRight(prev, " \t")
	soft := strings.HasSuffix(prev, "\u00ad")
	if soft {
		prev = strings.TrimSuffix(prev, "\u00ad")
	}
	if len(prev) >= 2 && strings.HasSuffix(prev, "-") {
		a, _ := utf8.DecodeLastRuneInString(prev[:len(prev)-1])
		nr, _ := utf8.DecodeRuneInString(next)
		if unicode.IsLetter(a) {
			if unicode.IsLower(nr) {
				return prev[:len(prev)-1] + next
			}
			if unicode.IsUpper(nr) || unicode.IsDigit(nr) {
				return prev + next
			}
		}
	}
	if soft {
		return prev + next
	}
	return prev + " " + next
}
