package source

// Document is the canonical structured representation of one parsed
// ObjectVersion: immutable ordered Blocks plus the owning version identity.
type Document struct {
	Ref           ObjectRef
	Blocks        []Block
	ParseWarnings []string
}

// Block is one ordered structural unit of extracted text. ByteStart/ByteEnd
// are UTF-8 byte offsets into extracted text — never offsets inside a
// compressed PDF/DOCX original.
type Block struct {
	ID        string
	Ordinal   int
	Kind      string // paragraph|heading|table|message|code|…
	ParentID  string // document hierarchy
	Text      string
	ByteStart int
	ByteEnd   int
	Locator   Locator // page/box/table/clause metadata where available
}

// Chunk is a bounded derived unit produced under an immutable ChunkProfile.
// It carries the source spans it covers; generated content is never relabeled
// as original evidence.
type Chunk struct {
	ID            string
	ObjectRef     ObjectRef
	ProfileID     string // immutable chunk_profile_version_id
	Ordinal       int
	Text          string
	Spans         []Span
	ParentChunkID string
	Warnings      []string
	// Links maps code-point offsets inside Text to the deep link of the
	// block that starts there (ordered by Offset; consecutive spans with
	// the same URL collapse to one entry).
	Links []ChunkLink
}

// ChunkLink is one deep-link entry of a chunk: Offset is the code-point
// offset of the block's text inside the chunk text.
type ChunkLink struct {
	Offset    int    `json:"offset"`
	URL       string `json:"url,omitempty"`
	Author    string `json:"author,omitempty"`
	Timestamp string `json:"timestamp,omitempty"`
	NativeID  string `json:"native_id,omitempty"`
}

// ChunkProfile is one immutable, versioned processing profile. Sizes and
// overlap are measured in Unicode code points and labeled as such — never
// presented as token counts. Changing a profile creates a new version row;
// published evidence is never silently mutated.
type ChunkProfile struct {
	ID                 string // this immutable row (tenant-scoped PK)
	ProfileID          string // version family
	Version            int    // monotonic within ProfileID
	SizeCodePoints     int
	OverlapCodePoints  int
	BoundaryStrategy   string // auto|fixed|heading|clause|paragraph|message|thread|code_lines
	IncludeParents     bool
	TableHandling      string // e.g. atomic|row-wise|skip
	PreserveClauses    bool   // legal profiles keep clause/subsection context
	PreserveAuthorship bool   // chat profiles keep message/thread authorship
}
