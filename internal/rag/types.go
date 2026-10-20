package rag

import "time"

const (
	DocumentStatusProcessing = "processing"
	DocumentStatusReady      = "ready"
	DocumentStatusFailed     = "failed"

	DefaultChunkSize    = 1000
	DefaultChunkOverlap = 150
)

// Collection is a tenant-scoped knowledge collection. It persists over
// knowledge_collections + chunk_profiles; Audience carries actor IDs (or "*"
// for tenant-wide read) decoded to/from the stored audience JSON list.
type Collection struct {
	TenantID              string         `json:"-"`
	ChunkProfileVersionID string         `json:"-"`
	ID                    string         `json:"id"`
	Name                  string         `json:"name"`
	Description           string         `json:"description,omitempty"`
	OwnerActorID          string         `json:"owner_actor_id,omitempty"`
	QdrantCollection      string         `json:"qdrant_collection"`
	EmbeddingProvider     string         `json:"embedding_provider,omitempty"`
	EmbeddingModel        string         `json:"embedding_model"`
	ChunkSize             int            `json:"chunk_size"`
	ChunkOverlap          int            `json:"chunk_overlap"`
	ACL                   map[string]any `json:"acl,omitempty"`
	Audience              []string       `json:"audience,omitempty"`
	Metadata              map[string]any `json:"metadata,omitempty"`
	IsActive              bool           `json:"is_active"`
	CreatedAt             time.Time      `json:"created_at"`
	UpdatedAt             time.Time      `json:"updated_at"`
}

// Document is the legacy-stable JSON view over a source.Object row joined to
// its latest source_object_versions row and the published index generation.
type Document struct {
	TenantID         string         `json:"-"`
	ObjectVersionID  string         `json:"-"`
	ObservedSeq      int64          `json:"-"`
	ArtifactRef      string         `json:"-"`
	ID               string         `json:"id"`
	CollectionID     string         `json:"collection_id"`
	SourceType       string         `json:"source_type"`
	SourceID         string         `json:"source_id,omitempty"`
	SourceURI        string         `json:"source_uri,omitempty"`
	FileName         string         `json:"file_name"`
	MimeType         string         `json:"mime_type"`
	Status           string         `json:"status"`
	TextHash         string         `json:"text_hash,omitempty"`
	Metadata         map[string]any `json:"metadata,omitempty"`
	QdrantPointCount int            `json:"qdrant_point_count"`
	ErrorMessage     string         `json:"error_message,omitempty"`
	CreatedAt        time.Time      `json:"created_at"`
	UpdatedAt        time.Time      `json:"updated_at"`
	ProcessedAt      *time.Time     `json:"processed_at,omitempty"`
}

// DocumentChunk is one derived chunk row pinned to an immutable
// chunk_profile_version_id; QdrantPointID is the row ID (Qdrant upserts are
// keyed by it so stale generations cannot clobber a newer version's index).
type DocumentChunk struct {
	ID            string         `json:"id"`
	DocumentID    string         `json:"document_id"`
	CollectionID  string         `json:"collection_id"`
	ChunkIndex    int            `json:"chunk_index"`
	Content       string         `json:"content"`
	TokenCount    int            `json:"token_count"`
	QdrantPointID string         `json:"qdrant_point_id,omitempty"`
	Metadata      map[string]any `json:"metadata,omitempty"`
	CreatedAt     time.Time      `json:"created_at"`
}

type CreateCollectionInput struct {
	Name           string         `json:"name"`
	Description    string         `json:"description,omitempty"`
	EmbeddingModel string         `json:"embedding_model,omitempty"`
	ChunkSize      int            `json:"chunk_size,omitempty"`
	ChunkOverlap   int            `json:"chunk_overlap,omitempty"`
	ACL            map[string]any `json:"acl,omitempty"`
	Metadata       map[string]any `json:"metadata,omitempty"`
}

type UpdateCollectionInput struct {
	Name           *string        `json:"name,omitempty"`
	Description    *string        `json:"description,omitempty"`
	EmbeddingModel *string        `json:"embedding_model,omitempty"`
	ChunkSize      *int           `json:"chunk_size,omitempty"`
	ChunkOverlap   *int           `json:"chunk_overlap,omitempty"`
	ACL            map[string]any `json:"acl,omitempty"`
	Metadata       map[string]any `json:"metadata,omitempty"`
	IsActive       *bool          `json:"is_active,omitempty"`
}

type UploadDocumentInput struct {
	FileName   string         `json:"file_name"`
	MimeType   string         `json:"mime_type"`
	Bytes      []byte         `json:"-"`
	SourceType string         `json:"source_type,omitempty"`
	SourceID   string         `json:"source_id,omitempty"`
	SourceURI  string         `json:"source_uri,omitempty"`
	Metadata   map[string]any `json:"metadata,omitempty"`
}

type SearchInput struct {
	CollectionID string         `json:"collection_id"`
	Query        string         `json:"query"`
	Limit        int            `json:"limit,omitempty"`
	Filter       map[string]any `json:"filter,omitempty"`
}

type SearchResult struct {
	DocumentID string         `json:"document_id"`
	ChunkID    string         `json:"chunk_id"`
	ChunkStart int            `json:"chunk_start"`
	ChunkEnd   int            `json:"chunk_end"`
	Score      float64        `json:"score"`
	Content    string         `json:"content"`
	SourceURI  string         `json:"source_uri,omitempty"`
	FileName   string         `json:"file_name,omitempty"`
	Metadata   map[string]any `json:"metadata,omitempty"`
}

type UploadDocumentOutput struct {
	Document Document        `json:"document"`
	Chunks   []DocumentChunk `json:"chunks"`
}
