package extract

import (
	"fmt"
	"strings"
	"testing"

	"enclava-go/internal/source"
)

// smallCapsFixtureWords are real pdftotext -bbox word boxes from page 14 of
// warfightning.pdf (MCDP 1-A "Warfighting"), where section headings are set
// in small caps and Poppler splits each word after its full-size initial:
// "W AR DEFINED" is WAR DEFINED and "L EVELS OF WAR" is LEVELS OF WAR. The
// initial's box is ~20pt tall, the small caps ~12pt, and the gap between
// them is zero — versus ~2-3pt between real words.
var smallCapsFixtureWords = []parseBlock{
	popplerLine(1, 60.4200, 214.5889, 78.4200, 19.9260, "W"),
	popplerLine(1, 78.4200, 220.8437, 94.2707, 12.1549, "AR"),
	popplerLine(1, 97.0069, 220.8437, 146.4510, 12.1549, "DEFINED"),
	popplerLine(1, 60.4200, 320.8489, 72.4260, 19.9260, "L"),
	popplerLine(1, 72.4200, 327.1037, 108.4146, 12.1549, "EVELS"),
	popplerLine(1, 111.1552, 327.1037, 126.4043, 12.1549, "OF"),
	popplerLine(1, 128.5772, 327.1037, 154.2023, 12.1549, "WAR"),
}

// sizedWord builds a word block with a Tesseract-style box
// (left,top,width,height), the sized form mergeLineWords also accepts.
func sizedWord(page, left, top, w, h int, text string) parseBlock {
	p := page
	return parseBlock{kind: "paragraph", text: text, locator: source.Locator{Page: &p, Box: fmt.Sprintf("%d,%d,%d,%d", left, top, w, h)}}
}

func TestMergeLineWordsSmallCaps(t *testing.T) {
	t.Run("small caps initials join their tail", func(t *testing.T) {
		got := texts(mergeLineWords(smallCapsFixtureWords, false))
		want := []string{"WAR DEFINED", "LEVELS OF WAR"}
		if strings.Join(got, "|") != strings.Join(want, "|") {
			t.Fatalf("got %v want %v", got, want)
		}
	})

	t.Run("sized boxes join the same way", func(t *testing.T) {
		words := []parseBlock{
			sizedWord(1, 60, 321, 12, 20, "L"),
			sizedWord(1, 72, 327, 36, 12, "EVELS"),
			sizedWord(1, 111, 327, 15, 12, "OF"),
			sizedWord(1, 129, 327, 25, 12, "WAR"),
		}
		got := texts(mergeLineWords(words, true))
		want := []string{"LEVELS OF WAR"}
		if strings.Join(got, "|") != strings.Join(want, "|") {
			t.Fatalf("got %v want %v", got, want)
		}
	})

	t.Run("small caps mid-line", func(t *testing.T) {
		words := []parseBlock{
			popplerLine(1, 60, 100, 85, 12, "see"),
			popplerLine(1, 88, 98, 92, 16, "T"),
			popplerLine(1, 92, 102, 110, 10, "HE"),
			popplerLine(1, 113, 102, 140, 10, "END"),
		}
		got := texts(mergeLineWords(words, false))
		want := []string{"see THE END"}
		if strings.Join(got, "|") != strings.Join(want, "|") {
			t.Fatalf("got %v want %v", got, want)
		}
	})

	t.Run("real one-letter words keep their space", func(t *testing.T) {
		for _, tc := range []struct {
			name  string
			words []parseBlock
			want  []string
		}{
			{
				// "A WORD" in body text: equal heights, normal word gap.
				name: "article before all-caps word",
				words: []parseBlock{
					popplerLine(1, 60, 100, 66, 12, "A"),
					popplerLine(1, 70, 100, 120, 12, "WORD"),
				},
				want: []string{"A WORD"},
			},
			{
				// Even a suspiciously tight gap cannot merge letters that
				// are set at the same height.
				name: "tight gap but equal heights",
				words: []parseBlock{
					popplerLine(1, 60, 100, 66, 12, "I"),
					popplerLine(1, 67, 100, 110, 12, "AM"),
				},
				want: []string{"I AM"},
			},
			{
				// The spaced-letter logo on page 1 of the fixture PDF:
				// "R" and "FIDE" are the same height, so no join.
				name: "letter-spaced logo",
				words: []parseBlock{
					popplerLine(1, 193.6926, 208.5599, 196.7937, 5.8918, "R"),
					popplerLine(1, 197.9729, 208.8371, 208.3626, 5.8918, "FIDE"),
				},
				want: []string{"R FIDE"},
			},
			{
				// A tall initial far from its tail is two words, not
				// small caps.
				name: "wide gap",
				words: []parseBlock{
					popplerLine(1, 60, 100, 72, 20, "L"),
					popplerLine(1, 80, 104, 116, 12, "EVELS"),
				},
				want: []string{"L EVELS"},
			},
			{
				name: "lowercase tail",
				words: []parseBlock{
					popplerLine(1, 60, 100, 66, 12, "A"),
					popplerLine(1, 67, 102, 110, 10, "word"),
				},
				want: []string{"A word"},
			},
			{
				name: "lowercase initial",
				words: []parseBlock{
					popplerLine(1, 60, 102, 66, 10, "a"),
					popplerLine(1, 66, 100, 110, 12, "WORD"),
				},
				want: []string{"a WORD"},
			},
		} {
			t.Run(tc.name, func(t *testing.T) {
				got := texts(mergeLineWords(tc.words, false))
				if strings.Join(got, "|") != strings.Join(tc.want, "|") {
					t.Fatalf("got %v want %v", got, tc.want)
				}
			})
		}
	})
}

// TestParsePDFBBoxSmallCaps feeds the same words through the real
// pdftotext -bbox parser, exercising parsePDFBBox → mergeLineWords →
// mergeParagraphLines end to end.
func TestParsePDFBBoxSmallCaps(t *testing.T) {
	raw := `<doc><page width="396.000000" height="576.000000">
<word xMin="60.420000" yMin="214.588900" xMax="78.420000" yMax="234.514900">W</word>
<word xMin="78.420000" yMin="220.843700" xMax="94.270700" yMax="232.998600">AR</word>
<word xMin="97.006900" yMin="220.843700" xMax="146.451000" yMax="232.998600">DEFINED</word>
<word xMin="60.420000" yMin="320.848900" xMax="72.426000" yMax="340.774900">L</word>
<word xMin="72.420000" yMin="327.103700" xMax="108.414600" yMax="339.258600">EVELS</word>
<word xMin="111.155200" yMin="327.103700" xMax="126.404300" yMax="339.258600">OF</word>
<word xMin="128.577200" yMin="327.103700" xMax="154.202300" yMax="339.258600">WAR</word>
</page></doc>`
	blocks, err := parsePDFBBox([]byte(raw), 1)
	if err != nil {
		t.Fatalf("parsePDFBBox: %v", err)
	}
	got := texts(blocks)
	want := []string{"WAR DEFINED", "LEVELS OF WAR"}
	if strings.Join(got, "|") != strings.Join(want, "|") {
		t.Fatalf("got %v want %v", got, want)
	}
	for _, b := range blocks {
		if b.locator.Page == nil || *b.locator.Page != 1 {
			t.Fatalf("block %q lost its page locator: %+v", b.text, b.locator)
		}
	}
}
