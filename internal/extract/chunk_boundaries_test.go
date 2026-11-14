package extract

import (
	"strings"
	"testing"
)

func TestSentenceEnds(t *testing.T) {
	cases := []struct {
		name string
		text string
		want []string // whitespace-trimmed pieces the ends delimit
	}{
		{"plain", "Hello world. Next one!", []string{"Hello world.", "Next one!"}},
		{"abbreviations", "Dr. Smith arrived. He left.", []string{"Dr. Smith arrived.", "He left."}},
		{"decimal", "Cost 3.14 euros (approx.). Then v1.2 shipped.", []string{"Cost 3.14 euros (approx.).", "Then v1.2 shipped."}},
		{"latin", "E.g. this. i.e. that.", []string{"E.g. this.", "i.e. that."}},
		{"initials", "A. B. Smith went home.", []string{"A. B. Smith went home."}},
		{"us", "U.S. exports fell. Mr. Smith spoke.", []string{"U.S. exports fell.", "Mr. Smith spoke."}},
		{"numbered", "No. 5 arrived. Fig. 3 shows it.", []string{"No. 5 arrived.", "Fig. 3 shows it."}},
		{"filename", "file.go is fine. ok.", []string{"file.go is fine.", "ok."}},
		{"no space", "stop.now", nil},
		{"cjk", "第一句。第二句！", []string{"第一句。", "第二句！"}},
		{"multiline", "line one\nline two\n", []string{"line one", "line two"}},
		{"quotes", "He said \"stop!\" Then left. Yes?", []string{`He said "stop!"`, "Then left.", "Yes?"}},
	}
	for _, tc := range cases {
		t.Run(tc.name, func(t *testing.T) {
			ends := sentenceEnds(tc.text)
			if len(ends) != len(tc.want) {
				t.Fatalf("sentenceEnds(%q) = %v, want %d pieces", tc.text, ends, len(tc.want))
			}
			prev := 0
			for i, end := range ends {
				if piece := strings.TrimSpace(tc.text[prev:end]); piece != tc.want[i] {
					t.Fatalf("piece %d = %q, want %q", i, piece, tc.want[i])
				}
				prev = end
			}
		})
	}
}

func TestSentenceEndsMultiByte(t *testing.T) {
	text := "Čebela leti. Šola je zaprta."
	ends := sentenceEnds(text)
	if len(ends) != 2 {
		t.Fatalf("sentenceEnds = %v, want 2 ends", ends)
	}
	if text[:ends[0]] != "Čebela leti." || text[ends[0]:ends[1]] != " Šola je zaprta." {
		t.Fatalf("multibyte ends misplaced: %q / %q", text[:ends[0]], text[ends[0]:ends[1]])
	}
}

func TestSplitPoint(t *testing.T) {
	cases := []struct {
		name     string
		text     string
		start    int
		maxRunes int
		end      int
		clean    bool
	}{
		{"exact fit", "abc def", 0, 7, 7, true},
		{"whole remainder", "abc def ghi", 0, 100, 11, true},
		{"hard cut no whitespace", strings.Repeat("x", 50), 0, 10, 10, false},
		{"hard cut between words", "aaaaaa bbbbbb cccccc", 0, 7, 7, true},
		{"word boundary", "aaa bbb " + strings.Repeat("c", 40), 0, 20, 8, true},
		{"min length skips early sentence", "Ab. " + strings.Repeat("w", 60), 0, 45, 45, false},
		{"progress at tail", "abc xy", 5, 1, 6, true},
	}
	for _, tc := range cases {
		t.Run(tc.name, func(t *testing.T) {
			end, clean := splitPoint(tc.text, tc.start, tc.maxRunes)
			if end != tc.end || clean != tc.clean {
				t.Fatalf("splitPoint = (%d, %v), want (%d, %v)", end, clean, tc.end, tc.clean)
			}
		})
	}
}

func TestSplitPointPrefersBoundaries(t *testing.T) {
	var sb strings.Builder
	for i := range 3 {
		sb.WriteString("Sentence number twenty")
		sb.WriteByte(byte('a' + i))
		sb.WriteString(" ends here. ")
	}
	text := sb.String()
	end, clean := splitPoint(text, 0, 40)
	if !clean || end <= 0 || end > len(text) {
		t.Fatalf("unexpected split: %d %v", end, clean)
	}
	if s := strings.TrimRight(text[:end], " \t\n"); !strings.HasSuffix(s, ".") {
		t.Fatalf("piece %q does not end at a sentence end", s)
	}
	// paragraph break beats a later sentence end
	para := "First para here. More text.\n\nSecond paragraph starts now. And runs on past."
	end, _ = splitPoint(para, 0, 45)
	if para[:end] != "First para here. More text.\n\n" {
		t.Fatalf("paragraph split = %q", para[:end])
	}
	// never splits a rune
	multi := "Čebela leti. Šola je zaprta. Konec je tu."
	for maxRunes := 1; maxRunes <= 45; maxRunes++ {
		end, _ := splitPoint(multi, 0, maxRunes)
		if end < len(multi) && multi[end]&0xc0 == 0x80 {
			t.Fatalf("split inside rune at %d (maxRunes %d)", end, maxRunes)
		}
	}
}

func TestOverlapStart(t *testing.T) {
	text := "First sentence here. Second one follows. Third sentence ends now. Last one."
	end := len(text)
	if got := overlapStart(text, end, 0); got != end {
		t.Fatalf("zero overlap must return end, got %d", got)
	}
	// whole trailing sentences within budget: "Last one." is 9 runes
	if got := overlapStart(text, end, 12); got != strings.Index(text, "Last one.") {
		t.Fatalf("overlapStart = %d, want start of last sentence", got)
	}
	// two trailing sentences fit in 36 runes
	if got := overlapStart(text, end, 36); got != strings.Index(text, "Third") {
		t.Fatalf("overlapStart = %d, want start of third sentence", got)
	}
	// budget smaller than the last sentence: word boundary fallback, at most
	// one word past the budget
	got := overlapStart(text, end, 5)
	if want := strings.LastIndex(text[:70], " ") + 1; got != want {
		t.Fatalf("word fallback = %d, want %d", got, want)
	}
	// whole piece shorter than the budget repeats it all
	if got := overlapStart("Short.", len("Short."), 50); got != 0 {
		t.Fatalf("short piece overlap = %d, want 0", got)
	}
	// always makes progress
	long := strings.Repeat("word ", 100)
	if got := overlapStart(long, len(long), 30); got >= len(long) {
		t.Fatalf("no progress: %d", got)
	}
}
