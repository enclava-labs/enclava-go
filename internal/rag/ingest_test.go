package rag

import (
	"strings"
	"testing"

	"enclava-go/internal/source"
)

func TestApplyConnectorStructure(t *testing.T) {
	cases := []struct {
		name, metadata, kind string
		locator              source.Locator
	}{
		{name: "Slack thread message", metadata: `{"connection_id":"c","kind":"message"}`, kind: "message"},
		{name: "nested Notion block", metadata: `{"connection_id":"c","kind":"block"}`, kind: "paragraph"},
		{name: "Drive sheet tab", metadata: `{"connection_id":"c","kind":"google_drive","locator":{"table":"Budget 2026"}}`, kind: "table", locator: source.Locator{Table: "Budget 2026"}},
	}
	for _, tc := range cases {
		t.Run(tc.name, func(t *testing.T) {
			document := source.Document{Blocks: []source.Block{{ID: "b", Kind: "paragraph", Text: "content"}}}
			got := applyConnectorStructure(document, source.ObjectVersion{Provenance: tc.metadata})
			// blocks.parent_id references blocks: a connector item's parent object must never become a block parent.
			if got.Blocks[0].ParentID != "" || got.Blocks[0].Kind != tc.kind || got.Blocks[0].Locator != tc.locator {
				t.Fatalf("converted block = %+v", got.Blocks[0])
			}
		})
	}
	if got := applyConnectorStructure(source.Document{Blocks: []source.Block{{Kind: "paragraph"}}}, source.ObjectVersion{Provenance: "{"}); got.Blocks[0].Kind != "paragraph" {
		t.Fatalf("non-connector document changed: %+v", got.Blocks[0])
	}
}

// Structured blocks documents already carry their own kinds and per-block
// locators; the connector-item mapping must never overwrite them.
func TestApplyConnectorStructureSkipsBlocksMIME(t *testing.T) {
	block := source.Block{
		ID: "b", Kind: "message", Text: "hello",
		Locator: source.Locator{URL: "https://hooks.slack.com/archives/C1/p2", Author: "ada", Timestamp: "2026-01-02T03:04:05Z", NativeID: "m2"},
	}
	got := applyConnectorStructure(
		source.Document{Blocks: []source.Block{block}},
		source.ObjectVersion{
			MimeType:   source.BlocksMIMEType,
			Provenance: `{"connection_id":"c","kind":"message","locator":{"table":"stamped"}}`,
		},
	)
	if got.Blocks[0] != block {
		t.Fatalf("blocks document mutated: %+v", got.Blocks[0])
	}
}

// Connector documents embed their container so a one-line Slack message
// still carries "#channel"; the upload path must stay untouched.
func TestEmbeddingTextsContainer(t *testing.T) {
	connector := source.Object{ConnectionID: "conn", Kind: "message"}
	ver := source.ObjectVersion{OriginalFilename: "slack:1", Provenance: `{"container":"#borgz","title":"ada in #borgz: deploy"}`}
	embed, search := embeddingTexts(Collection{}, connector, ver, "deploy broken", 1000)
	if !strings.Contains(embed, "#borgz\n") {
		t.Fatalf("container missing from embed text: %q", embed)
	}
	if search != "ada in #borgz: deploy\ndeploy broken" {
		t.Fatalf("search text changed: %q", search)
	}
	upload := source.Object{Kind: "document"} // no connection: upload path
	uploadVer := source.ObjectVersion{OriginalFilename: "notes.txt"}
	embed, search = embeddingTexts(Collection{}, upload, uploadVer, "body text", 1000)
	if embed != "body text" || search != "notes.txt\nbody text" {
		t.Fatalf("upload embed/search changed: %q / %q", embed, search)
	}
	// Empty container adds nothing for connector documents either.
	embed, _ = embeddingTexts(Collection{}, connector, source.ObjectVersion{Provenance: `{}`}, "deploy broken", 1000)
	if embed != "deploy broken" {
		t.Fatalf("empty container added a line: %q", embed)
	}
}
