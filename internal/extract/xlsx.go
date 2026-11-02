package extract

import (
	"archive/zip"
	"bytes"
	"encoding/xml"
	"fmt"
	"io"
	"path"
	"strconv"
	"strings"

	"enclava-go/internal/source"
)

const maxSpreadsheetCells = 2000000

type xlsxString struct {
	Text string `xml:"t"`
	Runs []struct {
		Text string `xml:"t"`
	} `xml:"r"`
}

func (s xlsxString) value() string {
	var b strings.Builder
	b.WriteString(s.Text)
	for _, r := range s.Runs {
		b.WriteString(r.Text)
	}
	return b.String()
}

func xlsxBlocks(data []byte) ([]parseBlock, []string, error) {
	zr, err := zip.NewReader(bytes.NewReader(data), int64(len(data)))
	if err != nil {
		return nil, nil, fmt.Errorf("%w: XLSX zip: %v", ErrMalformedDocument, err)
	}
	if len(zr.File) > 4096 {
		return nil, nil, ErrDocumentTooLarge
	}
	files := make(map[string]*zip.File, len(zr.File))
	for _, f := range zr.File {
		files[f.Name] = f
	}
	var expanded int64
	read := func(name string, target any) error {
		f := files[name]
		if f == nil {
			return fmt.Errorf("%w: XLSX missing %s", ErrMalformedDocument, name)
		}
		remaining := maxExpandedBytes - expanded
		if remaining <= 0 || f.UncompressedSize64 > uint64(remaining) {
			return ErrDocumentTooLarge
		}
		r, err := f.Open()
		if err != nil {
			return err
		}
		raw, err := io.ReadAll(io.LimitReader(r, maxExpandedBytes-expanded+1))
		_ = r.Close()
		if err != nil {
			return err
		}
		expanded += int64(len(raw))
		if expanded > maxExpandedBytes {
			return ErrDocumentTooLarge
		}
		if err := xml.Unmarshal(raw, target); err != nil {
			return fmt.Errorf("%w: XLSX XML: %v", ErrMalformedDocument, err)
		}
		return nil
	}
	var workbook struct {
		Sheets []struct {
			Name string `xml:"name,attr"`
			ID   string `xml:"id,attr"`
		} `xml:"sheets>sheet"`
	}
	var rels struct {
		Relationships []struct {
			ID     string `xml:"Id,attr"`
			Target string `xml:"Target,attr"`
			Mode   string `xml:"TargetMode,attr"`
		} `xml:"Relationship"`
	}
	if err := read("xl/workbook.xml", &workbook); err != nil {
		return nil, nil, err
	}
	if err := read("xl/_rels/workbook.xml.rels", &rels); err != nil {
		return nil, nil, err
	}
	targets := make(map[string]string)
	for _, rel := range rels.Relationships {
		if rel.Mode == "External" {
			continue
		}
		target := strings.TrimPrefix(rel.Target, "/")
		if !strings.HasPrefix(rel.Target, "/") {
			target = path.Join("xl", target)
		}
		targets[rel.ID] = path.Clean(target)
	}
	var shared struct {
		Strings []xlsxString `xml:"si"`
	}
	if files["xl/sharedStrings.xml"] != nil {
		if err := read("xl/sharedStrings.xml", &shared); err != nil {
			return nil, nil, err
		}
	}
	var blocks []parseBlock
	var warnings []string
	cells, offset, decoded := 0, 0, 0
	appendBlock := func(b parseBlock) {
		b.start, b.end = offset, offset+len(b.text)
		offset = b.end + 1
		blocks = append(blocks, b)
	}
	for sheetIndex, sheet := range workbook.Sheets {
		var ws struct {
			Rows []struct {
				Number int `xml:"r,attr"`
				Cells  []struct {
					Ref    string     `xml:"r,attr"`
					Type   string     `xml:"t,attr"`
					Value  string     `xml:"v"`
					Inline xlsxString `xml:"is"`
				} `xml:"c"`
			} `xml:"sheetData>row"`
		}
		target := targets[sheet.ID]
		if target == "" {
			return nil, nil, fmt.Errorf("%w: XLSX sheet relationship missing", ErrMalformedDocument)
		}
		if err := read(target, &ws); err != nil {
			return nil, nil, err
		}
		appendBlock(parseBlock{kind: "heading", text: sheet.Name, level: 1})
		table := fmt.Sprintf("xlsx:%d", sheetIndex+1)
		header := false
		for rowIndex, row := range ws.Rows {
			var values []string
			for _, cell := range row.Cells {
				col := 0
				for _, c := range cell.Ref {
					if c < 'A' || c > 'Z' {
						break
					}
					col = col*26 + int(c-'A'+1)
					if col > maxSpreadsheetCells {
						break
					}
				}
				if col == 0 {
					col = len(values) + 1
				}
				if col > maxSpreadsheetCells-cells {
					warnings = append(warnings, fmt.Sprintf("truncated: sheet %s exceeded the cell limit", sheet.Name))
					return blocks, warnings, nil
				}
				for len(values) < col {
					values = append(values, "")
				}
				value := cell.Value
				switch cell.Type {
				case "s":
					i, err := strconv.Atoi(value)
					if err != nil || i < 0 || i >= len(shared.Strings) {
						return nil, nil, fmt.Errorf("%w: XLSX shared string index", ErrMalformedDocument)
					}
					value = shared.Strings[i].value()
				case "inlineStr":
					value = cell.Inline.value()
				case "b":
					switch value {
					case "1":
						value = "true"
					case "0":
						value = "false"
					}
				}
				values[col-1] = value
				// Shared/inline strings expand past the ZIP size guard, so cap resolved text too.
				decoded += len(value)
				if decoded > maxExpandedBytes {
					return nil, nil, ErrDocumentTooLarge
				}
			}
			cells += len(values)
			text := strings.Join(values, "\t")
			if strings.TrimSpace(text) == "" {
				continue
			}
			kind := "table_row"
			if !header {
				kind, header = "table_header", true
			}
			rowNumber := row.Number
			if rowNumber == 0 {
				rowNumber = rowIndex + 1
			}
			appendBlock(parseBlock{kind: kind, text: text, locator: source.Locator{Table: table, Clause: strconv.Itoa(rowNumber)}})
		}
	}
	return blocks, warnings, nil
}
