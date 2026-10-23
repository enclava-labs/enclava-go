package rag

import (
	"encoding/json"
	"testing"

	"enclava-go/internal/source"
)

func TestApplyConnectorStructure(t *testing.T) {
	cases := []struct {
		name, metadata, parent, kind string
		locator                      source.Locator
	}{
		{name: "Slack thread message", metadata: `{"connection_id":"c","kind":"message"}`, parent: "thread-object", kind: "message"},
		{name: "nested Notion block", metadata: `{"connection_id":"c","kind":"block"}`, parent: "page-object", kind: "heading"},
		{name: "Drive sheet tab", metadata: `{"connection_id":"c","kind":"sheet","locator":{"table":"Budget 2026"}}`, kind: "table", locator: source.Locator{Table: "Budget 2026"}},
	}
	for _, tc := range cases {
		t.Run(tc.name, func(t *testing.T) {
			document := source.Document{Blocks: []source.Block{{ID: "b", Kind: "paragraph", Text: "content"}}}
			got := applyConnectorStructure(document, source.Object{ParentObjectID: tc.parent}, tc.metadata)
			if got.Blocks[0].ParentID != tc.parent || got.Blocks[0].Kind != tc.kind || got.Blocks[0].Locator != tc.locator {
				t.Fatalf("converted block = %+v", got.Blocks[0])
			}
		})
	}
	var invalid json.RawMessage = []byte("{")
	if got := applyConnectorStructure(source.Document{Blocks: []source.Block{{Kind: "paragraph"}}}, source.Object{}, string(invalid)); got.Blocks[0].Kind != "paragraph" {
		t.Fatalf("non-connector document changed: %+v", got.Blocks[0])
	}
}
