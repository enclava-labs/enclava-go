package source

import "encoding/json"

// BlocksMIMEType is the structured connector document format: connectors
// send already-structured blocks instead of a file to parse. It is produced
// only by connectors and must be rejected on upload endpoints.
const BlocksMIMEType = "application/vnd.enclava.blocks+json"

// Structured block kinds accepted in a BlocksDocument.
const (
	StructuredHeading     = "heading"
	StructuredParagraph   = "paragraph"
	StructuredListItem    = "list_item"
	StructuredCode        = "code"
	StructuredMessage     = "message"
	StructuredTableRow    = "table_row"
	StructuredTableHeader = "table_header"
)

// StructuredBlock is one block of a connector document. ParentNativeID links
// a reply to its thread root so message chunking keeps threads together.
// CreatedAt/UpdatedAt are RFC 3339 strings.
type StructuredBlock struct {
	Kind           string `json:"kind"`
	Text           string `json:"text"`
	Level          int    `json:"level,omitempty"`
	Language       string `json:"language,omitempty"`
	URL            string `json:"url,omitempty"`
	Author         string `json:"author,omitempty"`
	CreatedAt      string `json:"created_at,omitempty"`
	UpdatedAt      string `json:"updated_at,omitempty"`
	NativeID       string `json:"native_id,omitempty"`
	ParentNativeID string `json:"parent_native_id,omitempty"`
}

// BlocksDocument is the JSON body of a BlocksMIMEType document.
type BlocksDocument struct {
	Blocks []StructuredBlock `json:"blocks"`
}

// EncodeBlocks renders blocks as a BlocksMIMEType body. Output is
// deterministic for identical input, so unchanged content keeps its hash.
func EncodeBlocks(blocks []StructuredBlock) ([]byte, error) {
	if blocks == nil {
		blocks = []StructuredBlock{}
	}
	return json.Marshal(BlocksDocument{Blocks: blocks})
}
