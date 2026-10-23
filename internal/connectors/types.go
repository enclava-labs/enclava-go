package connectors

import (
	"context"
	"time"

	"enclava-go/internal/jobs"
	"enclava-go/internal/source"
)

type SourceKind string

const (
	SourceKindGitHub      SourceKind = SourceKind(source.ConnectionGitHub)
	SourceKindSlack       SourceKind = SourceKind(source.ConnectionSlack)
	SourceKindTeams       SourceKind = SourceKind(source.ConnectionTeams)
	SourceKindNotion      SourceKind = SourceKind(source.ConnectionNotion)
	SourceKindGoogleDrive SourceKind = SourceKind(source.ConnectionGoogleDrive)
)

// Cursor is opaque provider state scoped to one connection and stream.
type Cursor string

type Completeness string

const (
	CompletenessComplete   Completeness = "complete"
	CompletenessIncomplete Completeness = "incomplete"
)

type StreamKind string

const (
	StreamContent    StreamKind = "content"
	StreamAccess     StreamKind = "access"
	StreamMembership StreamKind = "membership"
)

// ContentItem carries provider identity and material, never a local source ref.
type ContentItem struct {
	NativeID, ParentNativeID string
	Kind, NativeRevision     string
	Filename, MIMEType       string
	Content                  []byte
	Text                     string
	URL                      string
	CreatedAt, UpdatedAt     *time.Time
	Locator                  source.Locator
}

// AccessGrant identifies provider-native principals until a binding resolves.
type AccessGrant struct {
	NativeObjectID, NativeObjectKind, NativePrincipalID, NativeGroupID string
	Scope, Effect                                                      string
	Inherited                                                          bool
}

type ContentPage struct {
	Items        []ContentItem
	NextCursor   Cursor
	Completeness Completeness
	Snapshot     bool
}

type AccessPage struct {
	Grants       []AccessGrant
	NextCursor   Cursor
	Completeness Completeness
	Snapshot     bool
}

type MembershipPage struct {
	Memberships  []AccessGrant
	NextCursor   Cursor
	Completeness Completeness
	Snapshot     bool
}

// Adapter pages are bounded; incomplete pages cannot authorize deletion.
type Adapter interface {
	Content(context.Context, source.Connection, Cursor, int) (ContentPage, error)
	Access(context.Context, source.Connection, Cursor, int) (AccessPage, error)
	Membership(context.Context, source.Connection, Cursor, int) (MembershipPage, error)
	Capabilities() AdapterCapabilities
}

type AdapterCapabilities struct {
	Content, Access, Membership bool
	UnsupportedScopes           []string
}

type AdapterFactories map[SourceKind]func(source.Connection) (Adapter, error)

type StreamState = jobs.ConnectorStreamCheckpoint

type Observation = jobs.ConnectorObservation
type ObservationRepository interface {
	StartRun(context.Context, string, string, string, string) (StreamState, error)
	GetStream(context.Context, string, string, string) (StreamState, error)
	PersistPage(context.Context, StreamState, []Observation, string, bool, bool, string) (StreamState, error)
	ListObservations(context.Context, StreamState) ([]Observation, error)
	WithFence(context.Context, StreamState, func(source.Tx) error) error
}

// Canonical source kinds (schema CHECK constraint); linear was a legacy
// alias and is rejected — there is no linear adapter in the canonical set.
const (
	SourceTypeGitHub      = "github"
	SourceTypeSlack       = "slack"
	SourceTypeTeams       = "teams"
	SourceTypeNotion      = "notion"
	SourceTypeGoogleDrive = "google_drive"
)

// Durable job operation enqueued by SyncSource. The job persists queued
// until the shared executor registers the handler.
const (
	OperationSync   = "connector.sync"
	OperationIngest = "rag.ingest"
	OperationAccess = "rag.access"
)

const (
	JobStatusQueued    = "queued"
	JobStatusRunning   = "running"
	JobStatusSucceeded = "succeeded"
	JobStatusFailed    = "failed"
	JobStatusUnknown   = "unknown"
	JobStatusCancelled = "cancelled"
)

// Source is the wire-stable connector source view over a source.Connection
// row. Checkpoint lives inside Config under the "checkpoint" key.
type Source struct {
	ID                 string         `json:"id"`
	Type               string         `json:"type"`
	Name               string         `json:"name"`
	TargetCollectionID string         `json:"target_collection_id,omitempty"`
	Config             map[string]any `json:"config,omitempty"`
	CredentialRef      string         `json:"credential_ref,omitempty"`
	Checkpoint         map[string]any `json:"checkpoint,omitempty"`
	IsEnabled          bool           `json:"is_enabled"`
	CreatedAt          time.Time      `json:"created_at"`
	UpdatedAt          time.Time      `json:"updated_at"`
	LastSyncAt         *time.Time     `json:"last_sync_at,omitempty"`
}

// SyncJob is the wire-stable view of a durable connector.sync jobs row.
type SyncJob struct {
	ID                string         `json:"id"`
	ConnectorSourceID string         `json:"connector_source_id"`
	Status            string         `json:"status"`
	StartedAt         *time.Time     `json:"started_at,omitempty"`
	CompletedAt       *time.Time     `json:"completed_at,omitempty"`
	ErrorMessage      string         `json:"error_message,omitempty"`
	Stats             map[string]any `json:"stats,omitempty"`
	CreatedAt         time.Time      `json:"created_at"`
}

type CreateSourceInput struct {
	Type               string         `json:"type"`
	Name               string         `json:"name"`
	TargetCollectionID string         `json:"target_collection_id,omitempty"`
	Config             map[string]any `json:"config,omitempty"`
	CredentialRef      string         `json:"credential_ref,omitempty"`
	IsEnabled          *bool          `json:"is_enabled,omitempty"`
}

type UpdateSourceInput struct {
	Type               *string        `json:"type,omitempty"`
	Name               *string        `json:"name,omitempty"`
	TargetCollectionID *string        `json:"target_collection_id,omitempty"`
	Config             map[string]any `json:"config,omitempty"`
	CredentialRef      *string        `json:"credential_ref,omitempty"`
	Checkpoint         map[string]any `json:"checkpoint,omitempty"`
	IsEnabled          *bool          `json:"is_enabled,omitempty"`
}
