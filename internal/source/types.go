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
	// AuthorizationOwner: explicit owner/audience grants (uploads, generated).
	AuthorizationOwner = "owner"
	// AuthorizationAudience: object shared to a named audience.
	AuthorizationAudience = "audience"
	// AuthorizationSourceACL: effective grants come from source-native ACLs.
	AuthorizationSourceACL = "source_acl"
	// AuthorizationPublishedAudience: Notion publisher model per D-07 — a
	// publisher with verified access explicitly assigns Enclava audiences.
	AuthorizationPublishedAudience = "published_audience"
)

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
	Audience          []string
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

// SourceIdentity binds a source-native principal to a tenant identity through
// an explicit verified tenant-and-connection-scoped mapping. ID is the
// tenant-scoped row identity grants/memberships reference; NativeID is the
// source-native user ID.
type SourceIdentity struct {
	TenantID     string
	ConnectionID string
	SourceKind   string
	ID           string
	NativeID     string
	MappedUserID string
}

// SourceGroup is a source-native group within a connection. ID is the
// tenant-scoped row identity; NativeID the source-native group ID.
type SourceGroup struct {
	TenantID     string
	ConnectionID string
	SourceKind   string
	ID           string
	NativeID     string
	Name         string
}

// SourceGroupMembership records source-native group membership. MemberID is
// the source_identities row ID (tenant, connection-scoped), not a native ID.
type SourceGroupMembership struct {
	TenantID       string
	ConnectionID   string
	GroupID        string
	MemberID       string // source_identities.id
	AccessRevision int64  // snapshot revision under which this was observed
}

// Grant subjects and effects.
const (
	GrantSubjectUser  = "user"
	GrantSubjectGroup = "group"

	GrantEffectAllow = "allow"
	GrantEffectDeny  = "deny"
)

// SourceGrant is a revision-qualified source-native effective grant.
// Deny overrides allow within the same authoritative snapshot. SubjectID is
// a source_identities.id for GrantSubjectUser, source_groups.id for
// GrantSubjectGroup.
type SourceGrant struct {
	TenantID       string
	ConnectionID   string // snapshot scope the grant was observed under
	ObjectID       string
	SubjectKind    string // GrantSubject* const set
	SubjectID      string // source_identities.id or source_groups.id
	Effect         string // GrantEffect* const set
	AccessRevision int64
}

// AccessSnapshot kinds.
const (
	SnapshotContent    = "content"
	SnapshotGrants     = "grants"
	SnapshotMembership = "membership"
)

// AccessSnapshot marks a completed, authoritative observation of access data.
// An incomplete snapshot is never authoritative and cannot remove unseen
// objects or grants.
type AccessSnapshot struct {
	TenantID     string
	ConnectionID string
	Kind         string // Snapshot* const set
	Revision     int64
	CompletedAt  *time.Time // nil while incomplete
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
	Page    *int   `json:"page,omitempty"`
	Box     string `json:"box,omitempty"`
	Table   string `json:"table,omitempty"`
	Clause  string `json:"clause,omitempty"`
	Section string `json:"section,omitempty"`
}
