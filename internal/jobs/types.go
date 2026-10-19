// Package jobs holds the canonical durable-job contract types: status,
// enqueue identity, leases/fencing and progress. Pure types only — no store,
// service, llm or rag imports.
package jobs

import (
	"encoding/json"
	"errors"
	"time"
)

// Job statuses. "unknown" means the outcome was never observed; cancellation
// is not proof that upstream side effects never happened.
const (
	StatusQueued    = "queued"
	StatusRunning   = "running"
	StatusSucceeded = "succeeded"
	StatusFailed    = "failed"
	StatusCancelled = "cancelled"
	StatusUnknown   = "unknown"
	StatusDeleted   = "deleted"
)

var (
	// ErrIdempotencyConflict: same idempotency identity replayed with
	// different inputs, policy or revisions.
	ErrIdempotencyConflict = errors.New("jobs: idempotency identity conflict")
	// ErrStaleFence: a worker with an expired/stale fence tried to publish.
	ErrStaleFence = errors.New("jobs: stale fence token")
	// ErrJobNotFound: unknown or deleted job for this tenant.
	ErrJobNotFound = errors.New("jobs: job not found")
)

// Subject identifies the object a job operates on.
type Subject struct {
	ConnectionID     string `json:"connection_id,omitempty"`
	ObjectID         string `json:"object_id,omitempty"`
	ObservedRevision int64  `json:"observed_revision,omitempty"`
}

// EnqueueRequest is the immutable job-creation request. The idempotency
// identity binds tenant, operation and the submitting principal/delegation/
// credential/billing context — stored before a job ID is returned.
type EnqueueRequest struct {
	TenantID       string
	Operation      string
	IdempotencyKey string
	Subject        Subject
	InputRef       string // immutable input reference
	InputHash      string
	Policy         map[string]any
	MaxAttempts    int
}

// Job mirrors the durable schema, including the frozen initiating principal.
type Job struct {
	TenantID          string
	ID                string
	Operation         string
	Status            string
	IdempotencyKey    string
	Subject           Subject
	InputRef          string
	InputHash         string
	Policy            map[string]any
	PrincipalSnapshot json.RawMessage // frozen initiating identity
	FenceToken        int64           // monotonic per lease grant
	LeaseOwner        string
	LeaseExpiresAt    *time.Time
	Attempt           int
	MaxAttempts       int
	Error             string
	CreatedAt         time.Time
	UpdatedAt         time.Time
	CompletedAt       *time.Time
}

// Attempt is one recorded execution attempt of a Job.
type Attempt struct {
	JobID      string
	Number     int
	StartedAt  time.Time
	FinishedAt *time.Time
	Error      string
	Outcome    string
}

// Lease grants one worker exclusive publishing rights under FenceToken.
// An expired/stale worker cannot publish progress or results.
type Lease struct {
	JobID      string
	Owner      string
	FenceToken int64
	ExpiresAt  time.Time
}

// Progress is bounded worker progress reporting.
type Progress struct {
	JobID     string
	Percent   int
	Message   string
	UpdatedAt time.Time
}
