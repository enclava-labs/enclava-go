package extract

import (
	"fmt"
	"strings"
	"testing"

	"enclava-go/internal/source"
)

func lineBlock(page int, box, text string) parseBlock {
	p := page
	return parseBlock{kind: "paragraph", text: text, locator: source.Locator{Page: &p, Box: box}}
}

// popplerLine builds an edge box (xMin,yMin,xMax,yMax) for a line of
// height h at the given y, formatted like pdftotext -bbox (%f).
func popplerLine(page int, x0, y0, x1, h float64, text string) parseBlock {
	return lineBlock(page, fmt.Sprintf("%f,%f,%f,%f", x0, y0, x1, y0+h), text)
}

func texts(blocks []parseBlock) []string {
	out := make([]string, len(blocks))
	for i, b := range blocks {
		out[i] = b.text
	}
	return out
}

func TestMergeParagraphLines(t *testing.T) {
	const h = 12.0 // line height
	mk := func(page int, x0, y, x1 float64, text string) parseBlock {
		return popplerLine(page, x0, y, x1, h, text)
	}

	t.Run("two paragraphs separated by a gap", func(t *testing.T) {
		lines := []parseBlock{
			mk(1, 72, 100, 300, "First paragraph line one."),
			mk(1, 72, 100+h+4, 300, "First paragraph line two."),
			mk(1, 72, 100+3*h+20, 300, "Second paragraph starts here."),
			mk(1, 72, 100+4*h+20, 300, "and continues."),
		}
		got := texts(mergeParagraphLines(lines, false))
		want := []string{"First paragraph line one. First paragraph line two.", "Second paragraph starts here. and continues."}
		if strings.Join(got, "|") != strings.Join(want, "|") {
			t.Fatalf("got %v want %v", got, want)
		}
	})

	t.Run("indent starts a new paragraph", func(t *testing.T) {
		lines := []parseBlock{
			mk(1, 72, 100, 300, "Body text reaching the right edge of the column."),
			mk(1, 72, 100+h+4, 300, "More body text."),
			mk(1, 72+2*h, 100+2*h+8, 300, "Indented new paragraph."),
			mk(1, 72, 100+3*h+12, 300, "Back to margin."),
		}
		got := texts(mergeParagraphLines(lines, false))
		// The indented line starts a new paragraph; the following margin
		// line continues it (first-line indent only).
		want := []string{
			"Body text reaching the right edge of the column. More body text.",
			"Indented new paragraph. Back to margin.",
		}
		if strings.Join(got, "|") != strings.Join(want, "|") {
			t.Fatalf("got %v want %v", got, want)
		}
	})

	t.Run("dehyphenation", func(t *testing.T) {
		cases := []struct{ prev, next, want string }{
			{"infor-", "mation", "information"},
			{"Anglo-", "American", "Anglo-American"},
			{"soft\u00ad", "ware", "software"},
			{"end.", "Next", "end. Next"},
		}
		for _, c := range cases {
			lines := []parseBlock{
				mk(1, 72, 100, 300, c.prev),
				mk(1, 72, 100+h+4, 300, c.next),
			}
			got := mergeParagraphLines(lines, false)
			if len(got) != 1 || got[0].text != c.want {
				t.Fatalf("%q + %q = %v, want %q", c.prev, c.next, texts(got), c.want)
			}
		}
	})

	t.Run("heading in larger font stays separate", func(t *testing.T) {
		lines := []parseBlock{
			popplerLine(1, 72, 100, 300, 24, "A Big Heading"),
			mk(1, 72, 100+30, 300, "Body text follows the heading here."),
			mk(1, 72, 100+30+h+4, 300, "and continues."),
		}
		got := texts(mergeParagraphLines(lines, false))
		want := []string{"A Big Heading", "Body text follows the heading here. and continues."}
		if strings.Join(got, "|") != strings.Join(want, "|") {
			t.Fatalf("got %v want %v", got, want)
		}
	})

	t.Run("two columns do not merge", func(t *testing.T) {
		lines := []parseBlock{
			mk(1, 72, 100, 300, "Left column line one."),
			mk(1, 72, 100+h+4, 300, "Left column line two."),
			mk(1, 320, 100, 540, "Right column starts higher up."),
			mk(1, 320, 100+h+4, 540, "Right column continues."),
		}
		got := texts(mergeParagraphLines(lines, false))
		// Each column's lines merge internally; the column jump breaks.
		want := []string{
			"Left column line one. Left column line two.",
			"Right column starts higher up. Right column continues.",
		}
		if strings.Join(got, "|") != strings.Join(want, "|") {
			t.Fatalf("got %v want %v", got, want)
		}
	})

	t.Run("page break never joins", func(t *testing.T) {
		lines := []parseBlock{
			mk(1, 72, 100, 300, "End of page one."),
			mk(2, 72, 100, 300, "Start of page two."),
		}
		got := mergeParagraphLines(lines, false)
		if len(got) != 2 {
			t.Fatalf("got %v", texts(got))
		}
	})

	t.Run("list items start new paragraphs", func(t *testing.T) {
		lines := []parseBlock{
			mk(1, 72, 100, 300, "Intro paragraph."),
			mk(1, 72, 100+h+4, 300, "• First item"),
			mk(1, 72, 100+2*h+8, 300, "• Second item"),
			mk(1, 72, 100+3*h+12, 300, "1. Numbered item"),
			mk(1, 72, 100+4*h+16, 300, "(a) Lettered item"),
		}
		got := texts(mergeParagraphLines(lines, false))
		if len(got) != 5 {
			t.Fatalf("got %v", got)
		}
	})

	t.Run("tesseract sized boxes", func(t *testing.T) {
		p := 1
		sized := func(left, top, w, hgt float64, text string) parseBlock {
			return parseBlock{kind: "paragraph", text: text, locator: source.Locator{
				Page: &p, Box: fmt.Sprintf("%.0f,%.0f,%.0f,%.0f", left, top, w, hgt)}}
		}
		lines := []parseBlock{
			sized(50, 100, 400, 14, "OCR paragraph first line."),
			sized(50, 118, 400, 14, "OCR paragraph second line."),
		}
		got := mergeParagraphLines(lines, true)
		if len(got) != 1 || got[0].text != "OCR paragraph first line. OCR paragraph second line." ||
			got[0].locator.Box != "50,100,400,32" {
			t.Fatalf("got %#v", got)
		}
	})

	t.Run("4000 rune cap", func(t *testing.T) {
		long := strings.Repeat("a", 3000)
		lines := []parseBlock{
			mk(1, 72, 100, 300, long),
			mk(1, 72, 100+h+4, 300, long),
			mk(1, 72, 100+2*h+8, 300, long),
		}
		got := mergeParagraphLines(lines, false)
		if len(got) != 3 {
			t.Fatalf("got %d blocks", len(got))
		}
		for i, b := range got {
			if len(b.text) != 3000 {
				t.Fatalf("block %d len %d", i, len(b.text))
			}
		}
	})

	t.Run("unparsable box passes through", func(t *testing.T) {
		lines := []parseBlock{
			mk(1, 72, 100, 300, "Good line."),
			lineBlock(1, "not,a,box", "Bad box line."),
			mk(1, 72, 100+h+4, 300, "Another good line."),
		}
		got := texts(mergeParagraphLines(lines, false))
		want := []string{"Good line.", "Bad box line.", "Another good line."}
		if strings.Join(got, "|") != strings.Join(want, "|") {
			t.Fatalf("got %v want %v", got, want)
		}
	})

	t.Run("offsets renumbered", func(t *testing.T) {
		lines := []parseBlock{
			mk(1, 72, 100, 300, "one"),
			mk(1, 72, 100+h+4, 300, "two"),
			mk(1, 72, 200, 300, "three"),
		}
		got := mergeParagraphLines(lines, false)
		if len(got) != 2 || got[0].start != 0 || got[0].end != 7 || got[1].start != 8 || got[1].end != 13 {
			t.Fatalf("got %#v", got)
		}
	})
}
