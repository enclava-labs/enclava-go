package rag

import (
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
			got := applyConnectorStructure(document, tc.metadata)
			// blocks.parent_id references blocks: a connector item's parent object must never become a block parent.
			if got.Blocks[0].ParentID != "" || got.Blocks[0].Kind != tc.kind || got.Blocks[0].Locator != tc.locator {
				t.Fatalf("converted block = %+v", got.Blocks[0])
			}
		})
	}
	if got := applyConnectorStructure(source.Document{Blocks: []source.Block{{Kind: "paragraph"}}}, "{"); got.Blocks[0].Kind != "paragraph" {
		t.Fatalf("non-connector document changed: %+v", got.Blocks[0])
	}
}
