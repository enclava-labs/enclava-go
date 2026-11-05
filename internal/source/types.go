// Package source holds the canonical source/object/version/access contract
// types shared by connectors, document processing, search and agents.
// It is a pure-type package: it imports no service, store, llm or rag package.
package source

import "time"

// Connection kinds. Use canonical values; google_drive is canonical for Drive.
const (
	ConnectionGitHub      = "github"
	ConnectionSlack       = "slack"
	ConnectionTeams       = "teams"
	ConnectionNotion      = "notion"
	ConnectionGoogleDrive = "google_drive"
)

// Object authorization modes.
const (
	// AuthorizationOwner: readable only by the object's owner actor.
	AuthorizationOwner = "owner"
	// AuthorizationCollection: readable by the principals that can read a
	// collection containing the object.
	AuthorizationCollection = "collection"
)

// CollectionAccess is the read-relevant state of a knowledge collection,
// consumed by the shared collection read rule.
type CollectionAccess struct {
	ID           string
	OwnerActorID string
	Audience     []string
	IsActive     bool
}

// Connection is a tenant-scoped configured source connection.
type Connection struct {
	TenantID           string
	ID                 string
	Kind               string // Connection* const set
	Name               string
	CredentialRef      string // server-approved ref bound to tenant+kind+origin
	Config             map[string]any
	TargetCollectionID string
	AudienceDefault    []string
	Status             string
	CreatedBy          string
	SyncedAt           *time.Time
	SyncStatus         string
	SyncError          string
	CreatedAt          time.Time
	UpdatedAt          time.Time
}

// Object is a canonical source object. Source identity is
// (TenantID, ConnectionID, kind, NativeID) — never a bare filename or URL.
// ConnectionID == "" means a tenant-owned upload, not a connector object.
//
// ObservedSeq is the local monotonic observation revision (always present,
// assigned by durable orchestration). UpstreamRevision is the source-native
// revision when the source supplies one; it may be absent. Identical bytes
// observed after A→B→A must not collapse a new transition into an old job.
type Object struct {
	TenantID          string
	ConnectionID      string
	ID                string
	Kind              string
	NativeID          string
	ParentObjectID    string
	AuthorizationMode string // Authorization* const set
	OwnerActorID      string
	UpstreamRevision  *string
	ObservedSeq       int64
	DeletedAt         *time.Time
	TombstonedAt      *time.Time
}

// ObjectVersion is one immutable content observation of an Object.
type ObjectVersion struct {
	ObjectID         string
	ObservedSeq      int64
	ArtifactRef      string // opaque artifacts.Ref ID — never a path
	SHA256           string
	ByteSize         int64
	MimeType         string
	OriginalFilename string
	UpstreamRevision *string
	Provenance       string // source-native provenance, when available
	Restrictions     []SourceRestriction
}

// Restriction subjects.
const (
	RestrictionTenant     = "tenant"
	RestrictionConnection = "connection"
	RestrictionObject     = "object"
	RestrictionArtifact   = "artifact"
)

// SourceRestriction is a server-owned processing restriction attached to a
// tenant, connection, object or artifact. Restrictions narrow — never widen —
// the destinations and capabilities an execution may use.
type SourceRestriction struct {
	TenantID           string
	SubjectKind        string // Restriction* const set
	SubjectID          string
	RequiredAssurance  string
	DeniedCapabilities []string
	PolicyLabels       []string
}

// ObjectRef is THE canonical evidence locator: every evidence, span and
// citation reference uses exactly this shape. Do not invent a second ref.
type ObjectRef struct {
	TenantID    string
	ObjectID    string
	ObservedSeq int64
}

// Span identifies a block plus a UTF-8 byte range inside extracted block
// text — never offsets inside a compressed original.
type Span struct {
	Ref       ObjectRef
	BlockID   string
	ByteStart int
	ByteEnd   int
}

// Locator retains real location metadata a source format supplies. Never
// fabricate fields a source format cannot supply.
type Locator struct {
	Page         *int   `json:"page,omitempty"`
	Box          string `json:"box,omitempty"`
	Table        string `json:"table,omitempty"`
	Clause       string `json:"clause,omitempty"`
	Section      string `json:"section,omitempty"`
	HeadingLevel int    `json:"heading_level,omitempty"`
	Language     string `json:"language,omitempty"`
}
