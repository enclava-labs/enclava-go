package extract

import (
	"archive/zip"
	"bytes"
	"context"
	"os"
	"reflect"
	"strings"
	"testing"
	"unicode/utf8"

	"enclava-go/internal/source"
)

func TestChunkPreservesClauseParentAndExactSpans(t *testing.T) {
	ref := source.ObjectRef{TenantID: "tenant", ObjectID: "object", ObservedSeq: 1}
	doc := source.Document{Ref: ref, Blocks: []source.Block{
		{ID: "h", Ordinal: 0, Kind: "heading", Text: "Exceptions", ByteStart: 0, ByteEnd: 10},
		{ID: "c1", Ordinal: 1, Kind: "paragraph", ParentID: "h", Text: "1. The rule applies.", ByteStart: 11, ByteEnd: 31, Locator: source.Locator{Clause: "1"}},
		{ID: "c2", Ordinal: 2, Kind: "paragraph", ParentID: "h", Text: "1. The rule applies.", ByteStart: 32, ByteEnd: 52, Locator: source.Locator{Clause: "1(a)"}},
	}}
	profile := source.ChunkProfile{ID: "cpv1", ProfileID: "legal", Version: 1, SizeCodePoints: 64,
		BoundaryStrategy: "clause", IncludeParents: true, TableHandling: "atomic", PreserveClauses: true}
	chunks, err := Chunk(doc, profile)
	if err != nil {
		t.Fatal(err)
	}
	if len(chunks) != 2 || chunks[0].Text != "Exceptions\n1. The rule applies." || chunks[1].Text != chunks[0].Text {
		t.Fatalf("clauses/parent context were not retained: %#v", chunks)
	}
	if chunks[0].ID == chunks[1].ID || chunks[0].Spans[len(chunks[0].Spans)-1].BlockID == chunks[1].Spans[len(chunks[1].Spans)-1].BlockID {
		t.Fatalf("similar clauses must retain distinct identities: %#v", chunks)
	}
	for _, chunk := range chunks {
		for _, span := range chunk.Spans {
			var found bool
			for _, block := range doc.Blocks {
				if span.BlockID == block.ID {
					found = true
					if span.ByteStart < 0 || span.ByteEnd > len(block.Text) || block.Text[span.ByteStart:span.ByteEnd] == "" {
						t.Fatalf("span does not resolve inside source block: %+v", span)
					}
				}
			}
			if !found {
				t.Fatalf("fabricated block reference: %+v", span)
			}
		}
	}
}

func TestChunkUnicodeBoundsOverlapAndWarnings(t *testing.T) {
	ref := source.ObjectRef{TenantID: "tenant", ObjectID: "object", ObservedSeq: 4}
	text := "A🙂é界B🙂é界C"
	table := "header\nrow one\nrow two"
	doc := source.Document{Ref: ref, Blocks: []source.Block{
		{ID: "long", Kind: "paragraph", Text: text, ByteStart: 10, ByteEnd: 10 + len(text)},
		{ID: "table", Kind: "table", Text: table, ByteStart: 100, ByteEnd: 100 + len(table)},
	}}
	profile := source.ChunkProfile{ID: "cpv2", ProfileID: "doc", Version: 2, SizeCodePoints: 5,
		OverlapCodePoints: 2, BoundaryStrategy: "paragraph", TableHandling: "atomic"}
	chunks, err := Chunk(doc, profile)
	if err != nil {
		t.Fatal(err)
	}
	if len(chunks) < 4 {
		t.Fatalf("long Unicode blocks should split: %#v", chunks)
	}
	for _, chunk := range chunks {
		if !utf8.ValidString(chunk.Text) || utf8.RuneCountInString(chunk.Text) > profile.SizeCodePoints {
			t.Fatalf("chunk is invalid UTF-8 or exceeds code-point size: %q", chunk.Text)
		}
		for _, span := range chunk.Spans {
			var block source.Block
			for _, b := range doc.Blocks {
				if b.ID == span.BlockID {
					block = b
					break
				}
			}
			part := block.Text[span.ByteStart:span.ByteEnd]
			if part == "" || !utf8.ValidString(part) {
				t.Fatalf("invalid source slice for span %+v", span)
			}
		}
	}
	if chunks[0].Spans[0].ByteEnd == chunks[1].Spans[0].ByteStart {
		t.Fatal("expected overlap spans to repeat exact source bytes")
	}
	foundWarning := false
	for _, chunk := range chunks {
		for _, warning := range chunk.Warnings {
			if strings.Contains(warning, "split") {
				foundWarning = true
			}
		}
	}
	if !foundWarning {
		t.Fatal("oversized clause/table should expose a split warning")
	}
}

func TestChunkSkipsTablesAndRejectsInvalidOffsets(t *testing.T) {
	ref := source.ObjectRef{TenantID: "tenant", ObjectID: "object", ObservedSeq: 1}
	doc := source.Document{Ref: ref, Blocks: []source.Block{{ID: "t", Kind: "table", Text: "cell", ByteStart: 0, ByteEnd: 4}}}
	profile := source.ChunkProfile{ID: "cpv1", ProfileID: "doc", Version: 1, SizeCodePoints: 8,
		BoundaryStrategy: "paragraph", TableHandling: "skip"}
	chunks, err := Chunk(doc, profile)
	if err != nil || len(chunks) != 0 {
		t.Fatalf("skip table: chunks=%v err=%v", chunks, err)
	}
	doc.Blocks[0].ByteStart = -1
	profile.TableHandling = "atomic"
	if _, err := Chunk(doc, profile); err == nil {
		t.Fatal("invalid source offsets must be rejected")
	}
}

func TestChunkTableRowWiseKeepsRowsTogether(t *testing.T) {
	ref := source.ObjectRef{TenantID: "tenant", ObjectID: "table-doc", ObservedSeq: 1}
	text := "h\nr1\nr2"
	doc := source.Document{Ref: ref, Blocks: []source.Block{{ID: "table", Kind: "table", Text: text, ByteStart: 0, ByteEnd: len(text)}}}
	profile := source.ChunkProfile{ID: "cpv", ProfileID: "table", Version: 1,
		SizeCodePoints: 5, BoundaryStrategy: "paragraph", TableHandling: "row-wise"}
	chunks, err := Chunk(doc, profile)
	if err != nil {
		t.Fatal(err)
	}
	if len(chunks) != 2 || chunks[0].Text != "h\nr1\n" || chunks[1].Text != "h\nr2" {
		t.Fatalf("table rows should remain intact with header context: %#v", chunks)
	}
}

// TestChunkFixedPacksShortBlocksUpToSize: a converted e-book with one line
// per paragraph must chunk into size-bounded windows of consecutive lines —
// one chunk per line made every chunk a sentence fragment.
func TestChunkFixedPacksShortBlocksUpToSize(t *testing.T) {
	ref := source.ObjectRef{TenantID: "tenant", ObjectID: "book", ObservedSeq: 1}
	lines := []string{"Manfred's on the road again,", "making strangers rich.", "It's a hot summer Tuesday,", "and he's standing in the plaza.", strings.Repeat("x", 70)}
	var blocks []source.Block
	offset := 0
	for i, line := range lines {
		blocks = append(blocks, source.Block{ID: string(rune('a' + i)), Ordinal: i, Kind: "paragraph", Text: line, ByteStart: offset, ByteEnd: offset + len(line)})
		offset += len(line) + 1
	}
	profile := source.ChunkProfile{ID: "cpv", ProfileID: "fixed", Version: 1, SizeCodePoints: 60, BoundaryStrategy: "fixed", TableHandling: "row-wise"}
	chunks, err := Chunk(source.Document{Ref: ref, Blocks: blocks}, profile)
	if err != nil {
		t.Fatal(err)
	}
	var texts []string
	for _, c := range chunks {
		if utf8.RuneCountInString(c.Text) > 60 {
			t.Fatalf("chunk over size: %q", c.Text)
		}
		texts = append(texts, c.Text)
	}
	want := []string{
		"Manfred's on the road again,\nmaking strangers rich.",
		"It's a hot summer Tuesday,\nand he's standing in the plaza.",
		strings.Repeat("x", 60), strings.Repeat("x", 10),
	}
	if !reflect.DeepEqual(texts, want) {
		t.Fatalf("fixed chunks = %q, want %q", texts, want)
	}
}

func TestChunkBoundaryStrategyControlsGrouping(t *testing.T) {
	ref := source.ObjectRef{TenantID: "tenant", ObjectID: "doc", ObservedSeq: 1}
	doc := source.Document{Ref: ref, Blocks: []source.Block{
		{ID: "h", Kind: "heading", Text: "Title", ByteStart: 0, ByteEnd: 5},
		{ID: "p", Kind: "paragraph", ParentID: "h", Text: "Body", ByteStart: 6, ByteEnd: 10},
	}}
	profile := source.ChunkProfile{ID: "head-v1", ProfileID: "head", Version: 1,
		SizeCodePoints: 20, BoundaryStrategy: "heading", TableHandling: "atomic"}
	headings, err := Chunk(doc, profile)
	if err != nil {
		t.Fatal(err)
	}
	profile.ID, profile.ProfileID, profile.BoundaryStrategy = "paragraph-v1", "paragraph", "paragraph"
	paragraphs, err := Chunk(doc, profile)
	if err != nil {
		t.Fatal(err)
	}
	if len(headings) != 1 || headings[0].Text != "Title\nBody" || len(paragraphs) != 2 {
		t.Fatalf("boundary controls did not affect grouping: heading=%#v paragraph=%#v", headings, paragraphs)
	}
}

func TestChunkClauseAttachesAvailableFootnote(t *testing.T) {
	ref := source.ObjectRef{TenantID: "tenant", ObjectID: "legal", ObservedSeq: 1}
	doc := source.Document{Ref: ref, Blocks: []source.Block{
		{ID: "clause", Kind: "paragraph", Text: "The exception applies.", ByteStart: 0, ByteEnd: 22, Locator: source.Locator{Clause: "4,footnote:7"}},
		{ID: "note", Kind: "paragraph", Text: "Subject to notice.", ByteStart: 23, ByteEnd: 41, Locator: source.Locator{Clause: "footnote:7"}},
	}}
	profile := source.ChunkProfile{ID: "legal-v1", ProfileID: "legal", Version: 1,
		SizeCodePoints: 64, BoundaryStrategy: "clause", TableHandling: "atomic", PreserveClauses: true}
	chunks, err := Chunk(doc, profile)
	if err != nil {
		t.Fatal(err)
	}
	if len(chunks) != 1 || chunks[0].Text != "The exception applies.\nSubject to notice." || len(chunks[0].Spans) != 2 {
		t.Fatalf("available referenced footnote was not attached: %#v", chunks)
	}
}

func TestChunkRealPDFAndDOCXWithVersionedControls(t *testing.T) {
	if os.Getenv("ENCLAVA_NATIVE_INTEGRATION") != "1" {
		t.Skip("native PDF processing requires ENCLAVA_NATIVE_INTEGRATION=1")
	}
	pdf, err := os.ReadFile("testdata/legal-document.pdf")
	if err != nil {
		t.Fatal(err)
	}
	pdfParser, pdfRef := testParser(t, pdf, "application/pdf")
	pdfDoc, err := pdfParser.Parse(context.Background(), pdfRef, ParseOptions{
		Filename: "legal-document.pdf", MIMEType: "application/pdf",
		ObjectRef: source.ObjectRef{TenantID: "tenant", ObjectID: "legal-pdf", ObservedSeq: 1},
	})
	if err != nil {
		t.Fatalf("parse legal PDF: %v", err)
	}
	originalPDF := append([]source.Block(nil), pdfDoc.Blocks...)
	small, large := source.ChunkProfile{
		ID: "legal-v1", ProfileID: "legal", Version: 1, SizeCodePoints: 1,
		BoundaryStrategy: "clause", TableHandling: "row-wise", PreserveClauses: true,
	}, source.ChunkProfile{
		ID: "legal-v2", ProfileID: "legal", Version: 2, SizeCodePoints: 4000,
		BoundaryStrategy: "paragraph", TableHandling: "atomic",
	}
	pdfSmall, err := Chunk(pdfDoc, small)
	if err != nil {
		t.Fatalf("chunk legal PDF with v1: %v", err)
	}
	pdfLarge, err := Chunk(pdfDoc, large)
	if err != nil {
		t.Fatalf("chunk legal PDF with v2: %v", err)
	}
	if len(pdfSmall) <= len(pdfLarge) || !reflect.DeepEqual(originalPDF, pdfDoc.Blocks) {
		t.Fatalf("profile controls failed to vary chunking or changed source evidence: small=%d large=%d", len(pdfSmall), len(pdfLarge))
	}
	assertResolvedChunkSpans(t, pdfDoc, pdfSmall)
	assertResolvedChunkSpans(t, pdfDoc, pdfLarge)

	docx := docxChunkFixture(t)
	docxParser, docxRef := testParser(t, docx, "application/vnd.openxmlformats-officedocument.wordprocessingml.document")
	docxDoc, err := docxParser.Parse(context.Background(), docxRef, ParseOptions{
		Filename: "legal-fixture.docx", MIMEType: "application/vnd.openxmlformats-officedocument.wordprocessingml.document",
		ObjectRef: source.ObjectRef{TenantID: "tenant", ObjectID: "legal-docx", ObservedSeq: 1},
	})
	if err != nil {
		t.Fatalf("parse DOCX fixture: %v", err)
	}
	docxProfile := small
	docxProfile.ID, docxProfile.SizeCodePoints = "legal-docx-v1", 64
	docxChunks, err := Chunk(docxDoc, docxProfile)
	if err != nil {
		t.Fatalf("chunk DOCX with legal profile: %v", err)
	}
	assertResolvedChunkSpans(t, docxDoc, docxChunks)
	attachedFootnote := false
	for _, chunk := range docxChunks {
		if strings.Contains(chunk.Text, "Clause text") && strings.Contains(chunk.Text, "Footnote context") {
			attachedFootnote = true
		}
	}
	if !attachedFootnote {
		t.Fatalf("DOCX footnote context was not attached: %#v", docxChunks)
	}
}

func assertResolvedChunkSpans(t *testing.T, doc source.Document, chunks []source.Chunk) {
	t.Helper()
	for _, chunk := range chunks {
		for _, span := range chunk.Spans {
			resolved := false
			for _, block := range doc.Blocks {
				if block.ID == span.BlockID && span.ByteStart >= 0 && span.ByteEnd <= len(block.Text) &&
					block.Text[span.ByteStart:span.ByteEnd] != "" {
					resolved = true
					break
				}
			}
			if !resolved {
				t.Fatalf("chunk %s has unresolved source span %+v", chunk.ID, span)
			}
		}
	}
}

func docxChunkFixture(t *testing.T) []byte {
	t.Helper()
	var buf bytes.Buffer
	zw := zip.NewWriter(&buf)
	for name, content := range map[string]string{
		"word/document.xml":  `<w:document xmlns:w="http://schemas.openxmlformats.org/wordprocessingml/2006/main"><w:body><w:p><w:pPr><w:numPr><w:numId w:val="4"/><w:ilvl w:val="0"/></w:numPr></w:pPr><w:r><w:t>Clause text</w:t><w:footnoteReference w:id="1"/></w:r></w:p><w:tbl><w:tr><w:tc><w:p><w:r><w:t>Heading cell</w:t></w:r></w:p></w:tc></w:tr></w:tbl></w:body></w:document>`,
		"word/footnotes.xml": `<w:footnotes xmlns:w="http://schemas.openxmlformats.org/wordprocessingml/2006/main"><w:footnote w:id="1"><w:p><w:r><w:t>Footnote context</w:t></w:r></w:p></w:footnote></w:footnotes>`,
	} {
		f, err := zw.Create(name)
		if err != nil {
			t.Fatal(err)
		}
		if _, err := f.Write([]byte(content)); err != nil {
			t.Fatal(err)
		}
	}
	if err := zw.Close(); err != nil {
		t.Fatal(err)
	}
	return buf.Bytes()
}
