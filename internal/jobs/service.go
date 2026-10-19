// Durable job service (JOB-01/JOB-02): Enqueue freezes the input identity
// and the full submitting principal BEFORE the job ID is returned; claims
// carry a monotonic fence so a stale worker can never publish; cancellation,
// tombstones and the unknown-outcome path are durable, not in-memory.
package jobs

import (
	"context"
	"crypto/rand"
	"crypto/sha256"
	"encoding/base64"
	"encoding/hex"
	"encoding/json"
	"errors"
	"fmt"
	"sync/atomic"
	"time"

	"enclava-go/internal/auth"
)

var (
	// ErrJobTerminal: cancel/delete targeted a job already in a terminal
	// state (succeeded, failed, unknown). Cancelled is idempotent.
	ErrJobTerminal = errors.New("jobs: job already terminal")
	// ErrInvalidRequest: required enqueue fields missing or inconsistent.
	ErrInvalidRequest = errors.New("jobs: invalid enqueue request")
)

// FailureClass drives Fail's retry decision.
type FailureClass int

const (
	FailureFatal FailureClass = iota
	FailureRetryable
)

// RetryableError marks a handler error as safe to retry: Fail requeues the
// job with backoff until max_attempts; anything else is terminal.
type RetryableError struct{ Err error }

func (e RetryableError) Error() string { return e.Err.Error() }
func (e RetryableError) Unwrap() error { return e.Err }

// Retryable wraps err so the runner classifies it retryable.
func Retryable(err error) error {
	if err == nil {
		return nil
	}
	return RetryableError{Err: err}
}

// FailureClassFor maps a handler error to its class. Context cancellation
// is NOT a failure — the runner routes it through release/unknown itself.
func FailureClassFor(err error) FailureClass {
	var re RetryableError
	if errors.As(err, &re) {
		return FailureRetryable
	}
	return FailureFatal
}

// Service owns the job state machine; all persistence flows through
// JobsRepository so every publish is fence-checked atomically.
type Service struct {
	repo      JobsRepository
	retryBase time.Duration // requeue delay after a retryable failure
}

// NewService builds the service. retryBase≤0 defaults to 100ms; the nth
// attempt's requeue delay is retryBase*2^(n-1) clamped to 30s.
func NewService(repo JobsRepository, retryBase time.Duration) *Service {
	if retryBase <= 0 {
		retryBase = 100 * time.Millisecond
	}
	return &Service{repo: repo, retryBase: retryBase}
}

func newID(prefix string) string {
	buf := make([]byte, 12)
	_, _ = rand.Read(buf)
	return prefix + "_" + base64.RawURLEncoding.EncodeToString(buf)
}

// submitterKey is the canonical owner binding: one idempotency key can be
// replayed only by the exact actor+credential+delegation+billing context
// that created the job — never lent to another actor in the same tenant.
func submitterKey(p auth.Principal) string {
	parts := []string{
		p.TenantID, p.ActorID, string(p.ActorKind),
		p.CredentialID, string(p.CredentialKind), p.SessionID,
		p.DelegatedByActorID, p.BillingSubjectID, string(p.BillingSubjectKind),
	}
	sum := sha256.Sum256([]byte(fmt.Sprintf("%q", parts)))
	return hex.EncodeToString(sum[:])
}

// SubmitterKey exposes the canonical owner binding for callers that read job
// rows through an owner-scoped repository view (e.g. extract job reads).
func SubmitterKey(p auth.Principal) string { return submitterKey(p) }

func canonicalPolicy(p map[string]any) []byte {
	if p == nil {
		return []byte("{}")
	}
	b, err := json.Marshal(p) // map keys sorted → canonical for equality
	if err != nil {
		return []byte("{}")
	}
	return b
}

// Enqueue persists the job — frozen principal snapshot, submitter binding,
// policy, input identity — before the ID exists for the caller. A replay of
// (tenant,operation,idempotency_key) returns the stored job ONLY when the
// submitter binding, input ref/hash, canonical policy and subject+observed
// revision all match; any difference is ErrIdempotencyConflict. A different
// observed_revision under a different key is distinct work (A→B→A).
func (s *Service) Enqueue(ctx context.Context, p auth.Principal, req EnqueueRequest) (Job, bool, error) {
	if p.TenantID == "" {
		return Job{}, false, fmt.Errorf("%w: principal tenant required", ErrInvalidRequest)
	}
	if req.TenantID != "" && req.TenantID != p.TenantID {
		return Job{}, false, fmt.Errorf("%w: request tenant does not match principal", ErrInvalidRequest)
	}
	if req.Operation == "" || req.IdempotencyKey == "" {
		return Job{}, false, fmt.Errorf("%w: operation and idempotency key are required", ErrInvalidRequest)
	}
	maxAttempts := req.MaxAttempts
	if maxAttempts <= 0 {
		maxAttempts = 3
	}

	principalJSON, err := json.Marshal(p)
	if err != nil {
		return Job{}, false, fmt.Errorf("freeze principal: %w", err)
	}
	policyJSON := canonicalPolicy(req.Policy)

	rec := InsertRecord{
		Job: Job{
			TenantID:       p.TenantID,
			ID:             newID("job"),
			Operation:      req.Operation,
			Status:         StatusQueued,
			IdempotencyKey: req.IdempotencyKey,
			Subject:        req.Subject,
			InputRef:       req.InputRef,
			InputHash:      req.InputHash,
			Policy:         req.Policy,
			MaxAttempts:    maxAttempts,
		},
		SubmitterKey: submitterKey(p),
		Principal:    principalJSON,
		PolicyJSON:   policyJSON,
	}

	existing, same, err := s.repo.InsertJob(ctx, rec)
	if err != nil {
		return Job{}, false, err
	}
	if existing == nil {
		return rec.Job, true, nil
	}
	// (tenant,operation,key) already existed: replay only on FULL identity
	// match — a different owner's key, changed input/policy or a different
	// observed revision is a conflict that never leaks the stored job.
	if same &&
		existing.SubmitterKey == rec.SubmitterKey &&
		existing.Subject == req.Subject &&
		existing.InputRef == req.InputRef &&
		existing.InputHash == req.InputHash {
		return existing.Job, false, nil
	}
	return Job{}, false, ErrIdempotencyConflict
}

// Claim is a granted lease plus the job row: the worker's exclusive
// publishing right for one attempt.
type Claim struct {
	Lease
	svc        *Service
	job        JobRow
	attemptID  string
	dispatched atomic.Bool // set by MarkDispatched — current attempt only
}

// Job returns the claimed row (tenant, operation, input ref/hash, subject,
// attempt counters) so handlers never re-query.
func (c *Claim) Job() Job { return c.job.Job }

// TenantID exposes the composite-key scope for repo lookups (resolver).
func (c *Claim) TenantID() string { return c.job.TenantID }

// AttemptID is this claim's job_attempts row id — pass it to MarkDispatched
// before any upstream side effect.
func (c *Claim) AttemptID() string { return c.attemptID }

// Lease claims one runnable job for workerID: a queued job past its
// not-before time, or a running job whose lease expired undispatched.
// Expired running jobs with an unfinished dispatched attempt are swept to
// unknown instead. Returns (nil,nil) when nothing is runnable.
func (s *Service) Lease(ctx context.Context, workerID string, ttl time.Duration) (*Claim, error) {
	cj, err := s.repo.Claim(ctx, workerID, ttl)
	if err != nil || cj == nil {
		return nil, err
	}
	return &Claim{
		Lease:     cj.Lease,
		svc:       s,
		job:       cj.JobRow,
		attemptID: cj.AttemptID,
	}, nil
}

// Heartbeat extends the claim's lease; ErrStaleFence when already lost.
func (c *Claim) Heartbeat(ctx context.Context, ttl time.Duration) error {
	expires, err := c.svc.repo.ExtendLease(ctx, c.job.TenantID, c.JobID, c.Owner, c.FenceToken, ttl)
	if err != nil {
		return err
	}
	c.ExpiresAt = expires
	return nil
}

// Progress publishes bounded progress under the claim's fence.
func (c *Claim) Progress(ctx context.Context, percent int, message string) error {
	if percent < 0 {
		percent = 0
	}
	if percent > 100 {
		percent = 100
	}
	doc, err := json.Marshal(map[string]any{
		"percent": percent,
		"message": message,
		"at":      time.Now().UTC().Format(time.RFC3339Nano),
	})
	if err != nil {
		return err
	}
	return c.svc.repo.WriteProgress(ctx, c.job.TenantID, c.JobID, c.Owner, c.FenceToken, doc)
}

// CancelRequested reports whether the job's owner requested cancellation;
// the handler should abort and return ctx.Err() so the runner transitions
// the job to cancelled on the fenced write.
func (c *Claim) CancelRequested(ctx context.Context) (bool, error) {
	return c.svc.repo.CancelRequested(ctx, c.job.TenantID, c.JobID, c.Owner, c.FenceToken)
}

// MarkDispatched must be called BEFORE the handler performs an upstream
// side effect. After this call, a lost worker's attempt resolves to
// unknown on reclaim — never a silent re-dispatch.
func (c *Claim) MarkDispatched(ctx context.Context) error {
	if err := c.svc.repo.MarkDispatched(ctx, c.job.TenantID, c.JobID, c.Owner, c.FenceToken, c.attemptID); err != nil {
		return err
	}
	c.dispatched.Store(true)
	return nil
}

// Dispatched reports whether the current attempt recorded its dispatch
// point — the runner's release-vs-unknown branch.
func (c *Claim) Dispatched() bool { return c.dispatched.Load() }

// Complete publishes the result and ends the job (succeeded — or cancelled
// when the owner cancelled mid-run). resultRef is an opaque artifact ref.
func (c *Claim) Complete(ctx context.Context, resultRef string) (string, error) {
	return c.svc.repo.CompleteJob(ctx, c.job.TenantID, c.JobID, c.Owner, c.FenceToken, c.attemptID, resultRef)
}

// Fail records the attempt error; FailureRetryable requeues with backoff
// while attempts remain, otherwise the job is terminal failed.
func (c *Claim) Fail(ctx context.Context, cause error, class FailureClass) error {
	retry := class == FailureRetryable && c.job.Attempt+1 < c.job.MaxAttempts
	delay := retryDelay(c.svc.retryBase, c.job.Attempt)
	doc := errorDoc(cause, class == FailureRetryable)
	return c.svc.repo.FailJob(ctx, c.job.TenantID, c.JobID, c.Owner, c.FenceToken, c.attemptID, retry, time.Now().Add(delay), doc)
}

// MarkUnknown ends the job as unknown: the attempt was dispatched and its
// outcome was never observed. Retained, never auto-requeued.
func (c *Claim) MarkUnknown(ctx context.Context, reason string) error {
	doc := errorDoc(errors.New(reason), false)
	return c.svc.repo.MarkUnknownJob(ctx, c.job.TenantID, c.JobID, c.Owner, c.FenceToken, c.attemptID, doc)
}

// Release abandons the lease without consuming dispatch: the job returns to
// queued and the unfinished attempt is closed (used on graceful shutdown).
func (c *Claim) Release(ctx context.Context) error {
	return c.svc.repo.ReleaseJob(ctx, c.job.TenantID, c.JobID, c.Owner, c.FenceToken, c.attemptID)
}

func retryDelay(base time.Duration, attempt int) time.Duration {
	const cap = 30 * time.Second
	delay := base
	for range attempt {
		delay *= 2
		if delay >= cap {
			return cap
		}
	}
	return delay
}

func errorDoc(err error, retryable bool) json.RawMessage {
	msg := "unknown error"
	if err != nil {
		msg = err.Error()
	}
	b, merr := json.Marshal(map[string]any{"message": msg, "retryable": retryable})
	if merr != nil {
		return json.RawMessage(fmt.Sprintf(`{"message":%q,"retryable":false}`, msg))
	}
	return b
}

// Get returns the job only when p is the bound submitter — another tenant
// member's job is indistinguishable from absent.
func (s *Service) Get(ctx context.Context, p auth.Principal, id string) (Job, error) {
	row, err := s.repo.GetOwnedJob(ctx, p.TenantID, id, submitterKey(p))
	if err != nil {
		return Job{}, err
	}
	return row.Job, nil
}

// List returns the caller's newest jobs, newest first.
func (s *Service) List(ctx context.Context, p auth.Principal, limit int) ([]Job, error) {
	rows, err := s.repo.ListOwnedJobs(ctx, p.TenantID, submitterKey(p), limit)
	if err != nil {
		return nil, err
	}
	out := make([]Job, len(rows))
	for i := range rows {
		out[i] = rows[i].Job
	}
	return out, nil
}

// Cancel requests cancellation: a queued job transitions to cancelled
// immediately; a running job flips when its worker's next fenced write
// observes cancel_requested. Terminal (non-cancelled) states are denied.
func (s *Service) Cancel(ctx context.Context, p auth.Principal, id string) error {
	row, err := s.repo.GetOwnedJob(ctx, p.TenantID, id, submitterKey(p))
	if err != nil {
		return err
	}
	switch row.Status {
	case StatusCancelled:
		return nil // idempotent
	case StatusSucceeded, StatusFailed, StatusUnknown:
		return ErrJobTerminal
	}
	return s.repo.CancelJob(ctx, p.TenantID, id)
}

// Delete tombstones the job: reads stop returning it and a stale worker's
// fenced writes can no longer complete it.
func (s *Service) Delete(ctx context.Context, p auth.Principal, id string) error {
	row, err := s.repo.GetOwnedJob(ctx, p.TenantID, id, submitterKey(p))
	if err != nil {
		return err
	}
	if row.Status == StatusDeleted {
		return ErrJobNotFound
	}
	return s.repo.DeleteJob(ctx, p.TenantID, id)
}
