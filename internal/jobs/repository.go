package jobs

import (
	"context"
	"encoding/json"
	"errors"
	"time"
)

// JobRow is the full durable jobs row: the public Job projection plus the
// fields only the service reads (submitter binding, dispatch/tombstone
// bookkeeping).
type JobRow struct {
	Job
	SubmitterKey      string
	DispatchedAttempt string
	ResultRef         string
	StartedAt         *time.Time
	DeletedAt         *time.Time
}

// InsertRecord carries everything Enqueue persists BEFORE the job ID is
// returned: the frozen principal snapshot and its canonical submitter
// binding alongside the request fields.
type InsertRecord struct {
	Job
	SubmitterKey string
	Principal    json.RawMessage
	PolicyJSON   []byte
}

// ClaimedJob is the atomic claim outcome: the job row after the lease grant
// plus the freshly inserted attempt row. The repository increments the
// fence, stamps the lease and inserts the attempt in one transaction.
type ClaimedJob struct {
	JobRow
	Lease     Lease
	AttemptID string
}

// JobsRepository is the durable boundary for jobs.Service (JOB-01/JOB-02).
// Every transition that publishes worker output is fenced by
// (owner, fence_token): a stale worker's writes are no-ops surfaced as
// ErrStaleFence, never silent mutations.
type JobsRepository interface {
	// InsertJob inserts the row; sameKey reports whether an idempotent
	// replay matched (all supplied fields equal) when a row already existed.
	// sameKey=false on existing row means a real conflict.
	InsertJob(ctx context.Context, rec InsertRecord) (existing *JobRow, sameKey bool, err error)
	// GetJob returns the row for (tenant, id) regardless of status, or
	// ErrJobNotFound (deleted rows are still visible to the service so it
	// can shape owner-facing errors).
	GetJob(ctx context.Context, tenantID, id string) (JobRow, error)
	// GetOwnedJob is GetJob plus owner binding: row must carry submitterKey
	// and must not be deleted.
	GetOwnedJob(ctx context.Context, tenantID, id, submitterKey string) (JobRow, error)
	// ListOwnedJobs returns the newest non-deleted rows for one submitter.
	ListOwnedJobs(ctx context.Context, tenantID, submitterKey string, limit int) ([]JobRow, error)

	// Claim atomically: (1) sweeps every running job whose lease expired
	// with an unfinished DISPATCHED attempt to status=unknown (the outcome
	// was never observed — never silently re-leased); (2) re-leases the
	// next runnable row (queued and past its not-before timestamp, or
	// running with an expired lease and no unfinished dispatched attempt),
	// bumping fence_token, stamping lease_owner/lease_expires_at and
	// inserting the job_attempts row. Returns (nil,nil) when nothing is
	// runnable — callers poll again.
	Claim(ctx context.Context, workerID string, ttl time.Duration) (*ClaimedJob, error)

	// ExtendLease advances lease_expires_at only while owner+fence hold it.
	ExtendLease(ctx context.Context, tenantID, jobID, owner string, fence int64, ttl time.Duration) (time.Time, error)
	// WriteProgress stores the progress document under the same fence.
	WriteProgress(ctx context.Context, tenantID, jobID, owner string, fence int64, progress json.RawMessage) error
	// CancelRequested reports whether cancellation was requested on the
	// leased job (fenced read: ErrStaleFence when the lease was lost).
	CancelRequested(ctx context.Context, tenantID, jobID, owner string, fence int64) (bool, error)
	// MarkDispatched records attemptID as the dispatched attempt — called
	// BEFORE the handler performs its upstream call so a reclaim never
	// silently re-dispatches (JOB-02 unknown-outcome contract).
	MarkDispatched(ctx context.Context, tenantID, jobID, owner string, fence int64, attemptID string) error
	// CompleteJob transitions running→succeeded — or cancelled when
	// cancel was requested — and finishes the attempt; returns the status
	// actually written.
	CompleteJob(ctx context.Context, tenantID, jobID, owner string, fence int64, attemptID, resultRef string) (string, error)
	// FailJob records a failed attempt and either requeues (retry=true,
	// status=queued, lease_expires_at doubles as the not-before timestamp
	// until the next claim) or transitions to terminal failed.
	FailJob(ctx context.Context, tenantID, jobID, owner string, fence int64, attemptID string, retry bool, notBefore time.Time, errDoc json.RawMessage) error
	// ReleaseJob abandons the lease without consuming the attempt's
	// dispatch: job back to queued, unfinished attempt closed as unknown
	// (its outcome was never observed).
	ReleaseJob(ctx context.Context, tenantID, jobID, owner string, fence int64, attemptID string) error
	// MarkUnknownJob transitions running→unknown, finishing the dispatched
	// attempt as unknown. Never requeues.
	MarkUnknownJob(ctx context.Context, tenantID, jobID, owner string, fence int64, attemptID string, errDoc json.RawMessage) error
	// CancelJob sets cancel_requested_at; queued rows transition to
	// cancelled immediately (claimable set excludes them).
	CancelJob(ctx context.Context, tenantID, jobID string) error
	// DeleteJob tombstones the row (status=deleted, deleted_at).
	DeleteJob(ctx context.Context, tenantID, jobID string) error
}

// IsStaleFence reports whether err is a fencing violation.
func IsStaleFence(err error) bool { return errors.Is(err, ErrStaleFence) }
