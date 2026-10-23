package extract

import (
	"archive/zip"
	"bytes"
	"context"
	"encoding/binary"
	"encoding/json"
	"errors"
	"io"
	"sync"
	"testing"
	"time"

	"enclava-go/internal/artifacts"
	"enclava-go/internal/source"
)

func TestMarkdownBlocksKeepExactSpansAndStableIdentity(t *testing.T) {
	text := "# Title\n\nA paragraph.\n\n- one\n\n```go\nfmt.Println(1)\n```"
	blocks := markdownBlocks(text)
	if len(blocks) != 4 || text[blocks[0].start:blocks[0].end] != "Title" || text[blocks[1].start:blocks[1].end] != "A paragraph." || text[blocks[2].start:blocks[2].end] != "- one" {
		t.Fatalf("unexpected markdown source spans: %#v", blocks)
	}
	ref := artifacts.Ref{SHA256: "version-hash"}
	object := source.ObjectRef{TenantID: "tenant", ObjectID: "object", ObservedSeq: 3}
	first, second := makeDocument(object, ref, blocks), makeDocument(object, ref, markdownBlocks(text))
	for i := range first.Blocks {
		if first.Blocks[i].ID != second.Blocks[i].ID || first.Blocks[i].Ordinal != i || first.Blocks[i].Text != second.Blocks[i].Text {
			t.Fatalf("parse %d changed identity/order: %#v vs %#v", i, first.Blocks[i], second.Blocks[i])
		}
	}
	if first.Blocks[0].ParentID != "" || first.Blocks[1].ParentID != first.Blocks[0].ID {
		t.Fatalf("heading parent relationship lost: %#v", first.Blocks)
	}
}

func TestHTMLBlocksDoNotIncludeScriptsOrFetchResources(t *testing.T) {
	text := `<h1>Title</h1><script>secret()</script><p>R&amp;D</p><img src="https://invalid.example/asset">`
	blocks := htmlBlocks(text)
	if len(blocks) != 2 || blocks[0].text != "Title" || blocks[1].text != "R&D" {
		t.Fatalf("unexpected HTML blocks: %#v", blocks)
	}
	for _, block := range blocks {
		if block.start < 0 || block.end > len(text) || block.start >= block.end {
			t.Fatalf("invalid source span: %#v", block)
		}
	}
}

func TestCSVRowsPreserveOrderAndByteSpans(t *testing.T) {
	input := []byte("clause,amount\r\nfirst,12.50\r\nsecond,18.75\r\n")
	blocks, err := csvBlocks(input)
	if err != nil || len(blocks) != 3 {
		t.Fatalf("CSV blocks=%#v err=%v", blocks, err)
	}
	if blocks[1].text != "first\t12.50" || blocks[2].text != "second\t18.75" || string(input[blocks[1].start:blocks[1].end]) != "first,12.50\r\n" {
		t.Fatalf("CSV order/locator span lost: %#v", blocks)
	}
}

func TestJSONNumbersAreNotRounded(t *testing.T) {
	blocks, err := jsonBlocks([]byte(`{"value":9007199254740993}`), false)
	if err != nil || len(blocks) != 1 {
		t.Fatalf("parse JSON blocks: %#v, %v", blocks, err)
	}
	var decoded map[string]any
	dec := json.NewDecoder(bytes.NewBufferString(blocks[0].text))
	dec.UseNumber()
	if err := dec.Decode(&decoded); err != nil || decoded["value"].(json.Number).String() != "9007199254740993" {
		t.Fatalf("numeric value changed: %s (%v)", blocks[0].text, err)
	}
}

func TestMalformedStructuredInputsFail(t *testing.T) {
	if _, err := jsonBlocks([]byte(`{"x":`), false); !errors.Is(err, ErrMalformedDocument) {
		t.Fatalf("malformed JSON error = %v", err)
	}
	if _, err := jsonBlocks([]byte(`{} {}`), false); !errors.Is(err, ErrMalformedDocument) {
		t.Fatalf("multiple JSON values error = %v", err)
	}
	if _, err := csvBlocks([]byte("a,\"unterminated")); !errors.Is(err, ErrMalformedDocument) {
		t.Fatalf("malformed CSV error = %v", err)
	}
	if _, err := docxBlocks([]byte("not a zip")); !errors.Is(err, ErrMalformedDocument) {
		t.Fatalf("malformed DOCX error = %v", err)
	}
}

func TestDOCXParagraphsNumberingTablesAndNotesKeepOrder(t *testing.T) {
	var buf bytes.Buffer
	zw := zip.NewWriter(&buf)
	parts := map[string]string{
		"word/document.xml":  `<w:document xmlns:w="urn:w"><w:body><w:p><w:pPr><w:numPr><w:ilvl w:val="0"/><w:numId w:val="4"/></w:numPr></w:pPr><w:r><w:t>Clause 1</w:t><w:footnoteReference w:id="1"/></w:r></w:p><w:tbl><w:tr><w:tc><w:p><w:r><w:t>Cell A</w:t></w:r></w:p></w:tc><w:tc><w:p><w:r><w:t>Cell B</w:t></w:r></w:p></w:tc></w:tr></w:tbl></w:body></w:document>`,
		"word/header1.xml":   `<w:hdr xmlns:w="urn:w"><w:p><w:r><w:t>Header</w:t></w:r></w:p></w:hdr>`,
		"word/footnotes.xml": `<w:footnotes xmlns:w="urn:w"><w:footnote w:id="1"><w:p><w:r><w:t>Footnote</w:t></w:r></w:p></w:footnote></w:footnotes>`,
		"word/endnotes.xml":  `<w:endnotes xmlns:w="urn:w"><w:endnote w:id="1"><w:p><w:r><w:t>Endnote</w:t></w:r></w:p></w:endnote></w:endnotes>`,
	}
	for name, text := range parts {
		f, err := zw.Create(name)
		if err != nil {
			t.Fatal(err)
		}
		if _, err := io.WriteString(f, text); err != nil {
			t.Fatal(err)
		}
	}
	if err := zw.Close(); err != nil {
		t.Fatal(err)
	}
	blocks, err := docxBlocks(buf.Bytes())
	if err != nil || len(blocks) != 5 {
		t.Fatalf("DOCX parse blocks=%#v err=%v", blocks, err)
	}
	if blocks[0].kind != "list_item" || blocks[0].locator.Clause != "numId=4;ilvl=0,footnote:1" || blocks[1].kind != "table_row" || blocks[1].text != "Cell A\tCell B" {
		t.Fatalf("numbered clause or ordered table row lost: %#v", blocks)
	}
	if blocks[2].text != "Header" || blocks[3].text != "Footnote" || blocks[3].locator.Clause != "footnote:1" || blocks[4].text != "Endnote" || blocks[4].locator.Clause != "endnote:1" {
		t.Fatalf("header and note text or relationships lost: %#v", blocks)
	}
}

func TestDOCXExpandedSizeLimit(t *testing.T) {
	var buf bytes.Buffer
	zw := zip.NewWriter(&buf)
	f, err := zw.Create("word/document.xml")
	if err != nil {
		t.Fatal(err)
	}
	_, _ = io.WriteString(f, `<w:document xmlns:w="urn:w"><w:body><w:p><w:r><w:t>x</w:t></w:r></w:p></w:body></w:document>`)
	if err := zw.Close(); err != nil {
		t.Fatal(err)
	}
	entry := bytes.Index(buf.Bytes(), []byte("PK\x01\x02"))
	if entry < 0 {
		t.Fatal("ZIP central directory missing")
	}
	binary.LittleEndian.PutUint32(buf.Bytes()[entry+24:entry+28], uint32(maxExpandedBytes+1))
	if _, err := docxBlocks(buf.Bytes()); !errors.Is(err, ErrDocumentTooLarge) {
		t.Fatalf("expanded DOCX size error=%v", err)
	}
}

func TestParserReadsOwnedImmutableArtifact(t *testing.T) {
	content := []byte("# Section\n\nBody")
	parser, ref := testParser(t, content, "text/markdown")
	doc, err := parser.Parse(context.Background(), ref, ParseOptions{Filename: "evidence.md", MIMEType: "text/markdown", ObjectRef: source.ObjectRef{TenantID: "tenant", ObjectID: "object", ObservedSeq: 2}})
	if err != nil {
		t.Fatal(err)
	}
	if len(doc.Blocks) != 2 || doc.Blocks[0].Kind != "heading" || doc.Blocks[1].ParentID != doc.Blocks[0].ID {
		t.Fatalf("unexpected parsed document: %#v", doc)
	}
	_, err = parser.Parse(context.Background(), ref, ParseOptions{Filename: "evidence.md", MIMEType: "text/markdown", ObjectRef: source.ObjectRef{TenantID: "other", ObjectID: "object", ObservedSeq: 2}})
	if err == nil {
		t.Fatal("parser accepted a source ref from another tenant")
	}
}
func TestMalformedHTMLRetainsPreviousPlainTextFallback(t *testing.T) {
	parser, ref := testParser(t, []byte("<h1>unterminated"), "text/html")
	doc, err := parser.Parse(context.Background(), ref, ParseOptions{Filename: "bad.html", MIMEType: "text/html", ObjectRef: source.ObjectRef{TenantID: "tenant", ObjectID: "bad", ObservedSeq: 1}})
	if err != nil || len(doc.Blocks) != 1 || doc.Blocks[0].Kind != "paragraph" || doc.Blocks[0].Text != "<h1>unterminated" {
		t.Fatalf("malformed HTML fallback=%#v err=%v", doc, err)
	}
}

type parserMetadata struct {
	mu   sync.Mutex
	rows map[string]artifacts.ArtifactRow
}

func (m *parserMetadata) BeginInsert(_ context.Context, row artifacts.ArtifactRow) error {
	m.mu.Lock()
	defer m.mu.Unlock()
	m.rows[row.TenantID+"/"+row.ID] = row
	return nil
}
func (m *parserMetadata) GetArtifact(_ context.Context, tenant, id string) (artifacts.ArtifactRow, error) {
	m.mu.Lock()
	defer m.mu.Unlock()
	row, ok := m.rows[tenant+"/"+id]
	if !ok {
		return artifacts.ArtifactRow{}, artifacts.ErrNotFound
	}
	return row, nil
}
func (m *parserMetadata) FinalizeCommit(_ context.Context, tenant, id, owner string, fence int64, hash string, size int64) error {
	m.mu.Lock()
	defer m.mu.Unlock()
	row := m.rows[tenant+"/"+id]
	if row.State != artifacts.StateWriting || row.LeaseOwner != owner || row.FenceToken != fence {
		return artifacts.ErrLeaseLost
	}
	row.State, row.SHA256, row.ByteSize = artifacts.StateFinal, hash, size
	m.rows[tenant+"/"+id] = row
	return nil
}
func (m *parserMetadata) UpdateState(_ context.Context, tenant, id, state, reason string) error {
	m.mu.Lock()
	defer m.mu.Unlock()
	row := m.rows[tenant+"/"+id]
	row.State, row.TombstoneReason = state, reason
	m.rows[tenant+"/"+id] = row
	return nil
}
func (m *parserMetadata) DeleteRow(_ context.Context, tenant, id string) error {
	m.mu.Lock()
	defer m.mu.Unlock()
	delete(m.rows, tenant+"/"+id)
	return nil
}
func (m *parserMetadata) ListFinalPaths(context.Context) ([]artifacts.ArtifactRow, error) {
	return nil, nil
}
func (m *parserMetadata) ListExpiredWriting(context.Context, time.Time) ([]artifacts.ArtifactRow, error) {
	return nil, nil
}
func (m *parserMetadata) InvalidateWriting(context.Context, string, string, string, int64, string) (bool, error) {
	return false, nil
}

func testParser(t *testing.T, content []byte, contentType string) (*Parser, artifacts.Ref) {
	t.Helper()
	root := t.TempDir()
	meta := &parserMetadata{rows: make(map[string]artifacts.ArtifactRow)}
	store, err := artifacts.NewStore(root+"/artifacts", root+"/scratch", meta)
	if err != nil {
		t.Fatal(err)
	}
	writer, _, err := store.Begin(context.Background(), artifacts.Meta{Ref: artifacts.Ref{TenantID: "tenant", Kind: artifacts.KindOriginal}, MimeType: contentType}, time.Minute, maxParseBytes)
	if err != nil {
		t.Fatal(err)
	}
	if _, err = writer.Write(content); err != nil {
		t.Fatal(err)
	}
	if err = writer.Finalize(context.Background()); err != nil {
		t.Fatal(err)
	}
	return NewParser(store), writer.Ref()
}
