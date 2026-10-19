package connectors

import "time"

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
const OperationSync = "connector.sync"

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
