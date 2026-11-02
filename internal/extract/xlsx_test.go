package extract

import (
	"errors"
	"fmt"
	"strings"
	"testing"

	"enclava-go/internal/artifacts"
	"enclava-go/internal/source"
)

func TestXLSXSheetsAndCellLimit(t *testing.T) {
	workbook := `<workbook xmlns:r="http://schemas.openxmlformats.org/officeDocument/2006/relationships"><sheets><sheet name="First" r:id="r1"/><sheet name="Second" r:id="r2"/></sheets></workbook>`
	rels := `<Relationships><Relationship Id="r1" Target="worksheets/sheet1.xml"/><Relationship Id="r2" Target="worksheets/sheet2.xml"/></Relationships>`
	sheet := `<worksheet><sheetData><row r="1"><c r="A1" t="s"><v>0</v></c><c r="B1" t="inlineStr"><is><t>Amount</t></is></c></row><row r="2"><c r="A2" t="inlineStr"><is><t>Alice</t></is></c><c r="B2"><f>1+1</f><v>2</v></c></row></sheetData></worksheet>`
	files := map[string]string{"xl/workbook.xml": workbook, "xl/_rels/workbook.xml.rels": rels, "xl/sharedStrings.xml": `<sst><si><t>Name</t></si></sst>`, "xl/worksheets/sheet1.xml": sheet, "xl/worksheets/sheet2.xml": sheet}
	blocks, warnings, err := xlsxBlocks(writeZip(t, files))
	if err != nil || len(warnings) != 0 || len(blocks) != 6 || blocks[0].text != "First" || blocks[1].kind != "table_header" || blocks[1].text != "Name\tAmount" || blocks[2].text != "Alice\t2" || blocks[3].text != "Second" {
		t.Fatalf("spreadsheet blocks=%#v warnings=%v err=%v", blocks, warnings, err)
	}
	ref := source.ObjectRef{TenantID: "t", ObjectID: "o", ObservedSeq: 1}
	doc := makeDocument(ref, artifacts.Ref{SHA256: "xlsx"}, blocks)
	profile := source.ChunkProfile{ID: "v", ProfileID: "p", Version: 1, SizeCodePoints: 100, BoundaryStrategy: "fixed", TableHandling: "row-wise"}
	chunks, err := Chunk(doc, profile)
	if err != nil || len(chunks) != 4 || chunks[1].Text != "Name\tAmount\nAlice\t2" || chunks[1].Spans[0].BlockID != doc.Blocks[1].ID {
		t.Fatalf("spreadsheet chunks=%#v err=%v", chunks, err)
	}
	var rows strings.Builder
	for i := 1; i <= 123; i++ {
		fmt.Fprintf(&rows, `<row r="%d"><c r="XFD%d"><v>1</v></c></row>`, i, i)
	}
	files["xl/worksheets/sheet1.xml"] = `<worksheet><sheetData>` + rows.String() + `</sheetData></worksheet>`
	_, warnings, err = xlsxBlocks(writeZip(t, files))
	if err != nil || len(warnings) != 1 || !strings.HasPrefix(warnings[0], "truncated: sheet First exceeded the cell limit") {
		t.Fatalf("spreadsheet cell limit warnings=%v err=%v", warnings, err)
	}
}

func TestXLSXSharedStringExpansionLimit(t *testing.T) {
	workbook := `<workbook xmlns:r="http://schemas.openxmlformats.org/officeDocument/2006/relationships"><sheets><sheet name="Big" r:id="r1"/></sheets></workbook>`
	rels := `<Relationships><Relationship Id="r1" Target="worksheets/sheet1.xml"/></Relationships>`
	shared := `<sst><si><t>` + strings.Repeat("x", 1<<20) + `</t></si></sst>`
	var rows strings.Builder
	for i := 1; i <= 70; i++ {
		fmt.Fprintf(&rows, `<row r="%d"><c r="A%d" t="s"><v>0</v></c></row>`, i, i)
	}
	files := map[string]string{
		"xl/workbook.xml":            workbook,
		"xl/_rels/workbook.xml.rels": rels,
		"xl/sharedStrings.xml":       shared,
		"xl/worksheets/sheet1.xml":   `<worksheet><sheetData>` + rows.String() + `</sheetData></worksheet>`,
	}
	if _, _, err := xlsxBlocks(writeZip(t, files)); !errors.Is(err, ErrDocumentTooLarge) {
		t.Fatalf("shared string expansion error=%v", err)
	}
}
