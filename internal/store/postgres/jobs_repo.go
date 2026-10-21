package postgres

import (
	"context"
	"crypto/rand"
	"database/sql"
	"encoding/base64"
	"encoding/json"
	"errors"
	"fmt"
	"strconv"
	"strings"
	"time"

	"enclava-go/internal/jobs"
)

// JobsRepo is the concrete jobs.JobsRepository over the jobs/job_attempts
// tables (migration 004): every worker-visible write is a conditional update
// on (lease_owner, fence_token), and Claim performs the expired-dispatch
// sweep plus the fenced claim inside one transaction.
type JobsRepo struct {
	db *DB
}

func NewJobsRepo(db *DB) *JobsRepo {
	return &JobsRepo{db: db}
}

var _ jobs.JobsRepository = (*JobsRepo)(nil)

const jobColumns = `tenant_id::text, id, operation, status, connection_id,
	object_id, observed_revision, idempotency_key, input_hash, input_ref,
	principal, submitter_key, policy, attempts, max_attempts, fence_token,
	lease_owner, lease_expires_at, dispatched_attempt_id, result_ref, error,
	submitted_at, started_at, finished_at, deleted_at`

// scanJobRow decodes jobColumns order plus any caller-supplied trailing
func scanJobRow(s rowScanner, extra ...any) (jobs.JobRow, error) {
	var row jobs.JobRow
	var connID, objectID, inputRef, submitterKey, resultRef, dispatched sql.NullString
	var observed sql.NullInt64
	var leaseOwner sql.NullString
	var leaseExpires, startedAt, finishedAt, deletedAt sql.NullTime
	var principal, policy, errDoc []byte
	dests := []any{
		&row.TenantID, &row.ID, &row.Operation, &row.Status, &connID,
		&objectID,
		&observed, &row.IdempotencyKey, &row.InputHash, &inputRef, &principal,
		&submitterKey, &policy, &row.Attempt, &row.MaxAttempts, &row.FenceToken,
		&leaseOwner, &leaseExpires, &dispatched, &resultRef, &errDoc,
		&row.CreatedAt, &startedAt, &finishedAt, &deletedAt}
	dests = append(dests, extra...)
	if err := s.Scan(dests...); err != nil {
		return jobs.JobRow{}, err
	}
	row.Subject.ConnectionID = connID.String
	row.Subject.ObjectID = objectID.String
	row.Subject.ObservedRevision = observed.Int64
	row.InputRef = inputRef.String
	row.SubmitterKey = submitterKey.String
	row.LeaseOwner = leaseOwner.String
	row.DispatchedAttempt = dispatched.String
	row.PrincipalSnapshot = principal
	row.ResultRef = resultRef.String
	if len(policy) > 0 {
		_ = json.Unmarshal(policy, &row.Policy)
	}
	if leaseExpires.Valid {
		t := leaseExpires.Time
		row.LeaseExpiresAt = &t
	}
	if startedAt.Valid {
		t := startedAt.Time
		row.StartedAt = &t
	}
	if finishedAt.Valid {
		t := finishedAt.Time
		row.CompletedAt = &t
	}
	if deletedAt.Valid {
		t := deletedAt.Time
		row.DeletedAt = &t
	}
	if len(errDoc) > 0 {
		var doc struct {
			Message string `json:"message"`
		}
		if json.Unmarshal(errDoc, &doc) == nil {
			row.Error = doc.Message
		}
	}
	return row, nil
}

func nullString(s string) any {
	if s == "" {
		return nil
	}
	return s
}

func nullInt64(v int64) any {
	if v == 0 {
		return nil
	}
	return v
}

// isUniqueViolationPG reports SQLSTATE 23505 — the jobs idempotency
// unique index signalling a same-key insert.
func isUniqueViolationPG(err error) bool {
	return err != nil && strings.Contains(err.Error(), "SQLSTATE 23505")
}

func newAttemptID() string {
	buf := make([]byte, 12)
	_, _ = rand.Read(buf)
	return "att_" + base64.RawURLEncoding.EncodeToString(buf)
}

func mapNoRows(err error) error {
	if errors.Is(err, sql.ErrNoRows) || isInvalidUUID(err) {
		return jobs.ErrJobNotFound
	}
	return err
}

// fenced converts a conditional UPDATE result into ErrStaleFence when the
// (owner, fence_token) guard matched no row.
func fenced(res sql.Result, err error) error {
	if err != nil {
		if isInvalidUUID(err) {
			return jobs.ErrStaleFence
		}
		return err
	}
	n, rerr := res.RowsAffected()
	if rerr != nil {
		return rerr
	}
	if n == 0 {
		return jobs.ErrStaleFence
	}
	return nil
}

// InsertJob inserts the frozen row; on (tenant,operation,idempotency_key)
// conflict it loads the existing row and reports whether every identity
// field matched (sameKey) — policy compared as canonical JSONB server-side.
func (r *JobsRepo) InsertJob(ctx context.Context, rec jobs.InsertRecord) (*jobs.JobRow, bool, error) {
	policy := rec.PolicyJSON
	if len(policy) == 0 {
		policy = []byte("{}")
	}
	_, err := r.db.SQL.ExecContext(ctx, `
		INSERT INTO jobs (tenant_id, id, operation, status, connection_id,
		                  object_id, observed_revision, idempotency_key,
		                  input_hash, input_ref, principal, submitter_key,
		                  policy, max_attempts)
		VALUES ($1,$2,$3,'queued',$4,$5,$6,$7,$8,$9,$10,$11,$12::jsonb,$13)`,
		rec.TenantID, rec.ID, rec.Operation,
		nullString(rec.Subject.ConnectionID), nullString(rec.Subject.ObjectID),
		nullInt64(rec.Subject.ObservedRevision), rec.IdempotencyKey,
		rec.InputHash, nullString(rec.InputRef), rec.Principal,
		rec.SubmitterKey, policy, rec.MaxAttempts)
	switch {
	case err == nil:
		return nil, false, nil
	case !isUniqueViolationPG(err):
		if isInvalidUUID(err) {
			return nil, false, jobs.ErrJobNotFound
		}
		return nil, false, fmt.Errorf("insert job: %w", err)
	}

	var sameKey bool
	row, err := scanJobRow(r.db.SQL.QueryRowContext(ctx, `
		SELECT `+jobColumns+`,
		       (submitter_key = $4 AND input_hash = $5
		        AND input_ref IS NOT DISTINCT FROM $6
		        AND policy = $7::jsonb
		        AND connection_id IS NOT DISTINCT FROM $8
		        AND object_id IS NOT DISTINCT FROM $9
		        AND observed_revision IS NOT DISTINCT FROM $10) AS same_key
		FROM jobs
		WHERE tenant_id = $1 AND operation = $2 AND idempotency_key = $3`,
		rec.TenantID, rec.Operation, rec.IdempotencyKey, rec.SubmitterKey,
		rec.InputHash, nullString(rec.InputRef), policy,
		nullString(rec.Subject.ConnectionID), nullString(rec.Subject.ObjectID),
		nullInt64(rec.Subject.ObservedRevision)), &sameKey)
	if err != nil {
		return nil, false, fmt.Errorf("load idempotent job: %w", mapNoRows(err))
	}
	return &row, sameKey, nil
}

func (r *JobsRepo) GetJob(ctx context.Context, tenantID, id string) (jobs.JobRow, error) {
	row, err := scanJobRow(r.db.SQL.QueryRowContext(ctx, `
		SELECT `+jobColumns+` FROM jobs WHERE tenant_id = $1 AND id = $2`,
		tenantID, id))
	if err != nil {
		return jobs.JobRow{}, fmt.Errorf("get job: %w", mapNoRows(err))
	}
	return row, nil
}

func (r *JobsRepo) GetOwnedJob(ctx context.Context, tenantID, id, submitterKey string) (jobs.JobRow, error) {
	row, err := scanJobRow(r.db.SQL.QueryRowContext(ctx, `
		SELECT `+jobColumns+` FROM jobs
		WHERE tenant_id = $1 AND id = $2 AND submitter_key = $3
		  AND deleted_at IS NULL`,
		tenantID, id, submitterKey))
	if err != nil {
		return jobs.JobRow{}, fmt.Errorf("get owned job: %w", mapNoRows(err))
	}
	return row, nil
}

func (r *JobsRepo) ListOwnedJobs(ctx context.Context, tenantID, submitterKey string, limit int) ([]jobs.JobRow, error) {
	if limit <= 0 {
		limit = 50
	}
	rows, err := r.db.SQL.QueryContext(ctx, `
		SELECT `+jobColumns+` FROM jobs
		WHERE tenant_id = $1 AND submitter_key = $2 AND deleted_at IS NULL
		ORDER BY submitted_at DESC, id DESC
		LIMIT $3`, tenantID, submitterKey, limit)
	if err != nil {
		return nil, fmt.Errorf("list jobs: %w", err)
	}
	defer func() { _ = rows.Close() }()
	out := []jobs.JobRow{}
	for rows.Next() {
		row, err := scanJobRow(rows)
		if err != nil {
			return nil, fmt.Errorf("scan job: %w", err)
		}
		out = append(out, row)
	}
	return out, rows.Err()
}

// ListConnectionJobs lists a connection's non-deleted jobs regardless of
// submitter — scheduled syncs run under the scheduler's principal, and the
// source's managers must still see them. Tenant + connection scoping is the
// authorization boundary.
func (r *JobsRepo) ListConnectionJobs(ctx context.Context, tenantID, connectionID, status string, limit int) ([]jobs.JobRow, error) {
	if limit <= 0 {
		limit = 50
	}
	query := `
		SELECT ` + jobColumns + ` FROM jobs
		WHERE tenant_id = $1 AND connection_id = $2 AND deleted_at IS NULL`
	args := []any{tenantID, connectionID}
	if status != "" {
		query += ` AND status = $3`
		args = append(args, status)
	}
	query += ` ORDER BY submitted_at DESC, id DESC LIMIT ` + strconv.Itoa(limit)
	rows, err := r.db.SQL.QueryContext(ctx, query, args...)
	if err != nil {
		return nil, fmt.Errorf("list connection jobs: %w", err)
	}
	defer func() { _ = rows.Close() }()
	out := []jobs.JobRow{}
	for rows.Next() {
		row, err := scanJobRow(rows)
		if err != nil {
			return nil, fmt.Errorf("scan job: %w", err)
		}
		out = append(out, row)
	}
	return out, rows.Err()
}

// CountOwnedJobs counts the submitter's non-deleted rows — the total the
// owner-scoped list is paged over.
func (r *JobsRepo) CountOwnedJobs(ctx context.Context, tenantID, submitterKey string) (int, error) {
	var total int
	if err := r.db.SQL.QueryRowContext(ctx, `
		SELECT COUNT(*) FROM jobs
		WHERE tenant_id = $1 AND submitter_key = $2 AND deleted_at IS NULL`,
		tenantID, submitterKey).Scan(&total); err != nil {
		return 0, fmt.Errorf("count owned jobs: %w", err)
	}
	return total, nil
}

// CountOwnedJobsSince counts the submitter's non-deleted rows submitted at or
// after `since` — the "recent" window stat the dashboard renders.
func (r *JobsRepo) CountOwnedJobsSince(ctx context.Context, tenantID, submitterKey string, since time.Time) (int, error) {
	var total int
	if err := r.db.SQL.QueryRowContext(ctx, `
		SELECT COUNT(*) FROM jobs
		WHERE tenant_id = $1 AND submitter_key = $2 AND deleted_at IS NULL
		  AND submitted_at >= $3`,
		tenantID, submitterKey, since).Scan(&total); err != nil {
		return 0, fmt.Errorf("count owned jobs since: %w", err)
	}
	return total, nil
}

// Claim is the atomic lease grant (JOB-02). Inside one transaction it:
//  1. sweeps running jobs with an expired lease AND an unfinished dispatched
//     attempt to status=unknown — their upstream outcome was never observed
//     so they are never silently re-leased;
//  2. picks the next runnable row FOR UPDATE SKIP LOCKED (queued past its
//     not-before lease_expires_at, or running with an expired lease and no
//     unfinished dispatched attempt), bumps fence_token, stamps the lease,
//     closes any still-open prior attempts and inserts the new attempt row.
func (r *JobsRepo) Claim(ctx context.Context, workerID string, ttl time.Duration) (*jobs.ClaimedJob, error) {
	var out *jobs.ClaimedJob
	err := WithTx(ctx, r.db.SQL, func(tx *sql.Tx) error {
		// 1. Expired-dispatch sweep → unknown (job + its open attempt).
		if _, err := tx.ExecContext(ctx, `
			WITH swept AS (
				UPDATE jobs j
				SET status = 'unknown', finished_at = now(),
				    error = '{"message":"lease expired after dispatch; outcome unobserved","retryable":false}'::jsonb,
				    lease_owner = NULL, lease_expires_at = NULL
				WHERE j.status = 'running'
				  AND j.lease_expires_at IS NOT NULL AND j.lease_expires_at < now()
				  AND j.dispatched_attempt_id IS NOT NULL
				  AND EXISTS (SELECT 1 FROM job_attempts a
				              WHERE a.tenant_id = j.tenant_id
				                AND a.id = j.dispatched_attempt_id
				                AND a.finished_at IS NULL)
				RETURNING j.tenant_id, j.id, j.dispatched_attempt_id
			)
			UPDATE job_attempts a
			SET status = 'unknown', finished_at = now()
			FROM swept s
			WHERE a.tenant_id = s.tenant_id AND a.id = s.dispatched_attempt_id`); err != nil {
			return fmt.Errorf("sweep expired dispatches: %w", err)
		}

		// 2. Fenced claim of the next runnable job.
		var claimed jobs.JobRow
		claimed, err := scanJobRow(tx.QueryRowContext(ctx, `
			UPDATE jobs j
			SET status = 'running', fence_token = j.fence_token + 1,
			    lease_owner = $1, lease_expires_at = now() + $2::interval,
			    attempts = j.attempts + 1,
			    started_at = COALESCE(j.started_at, now()),
			    dispatched_attempt_id = NULL,
			    error = NULL
			WHERE (j.tenant_id, j.id) = (
				SELECT tenant_id, id FROM jobs c
				WHERE c.cancel_requested_at IS NULL
				  AND c.deleted_at IS NULL
				  AND (
				    (c.status = 'queued'
				     AND (c.lease_expires_at IS NULL OR c.lease_expires_at <= now()))
				    OR
				    (c.status = 'running'
				     AND c.lease_expires_at IS NOT NULL AND c.lease_expires_at < now()
				     AND NOT EXISTS (SELECT 1 FROM job_attempts a
				                     WHERE a.tenant_id = c.tenant_id
				                       AND a.id = c.dispatched_attempt_id
				                       AND a.finished_at IS NULL))
				  )
				ORDER BY c.submitted_at
				LIMIT 1
				FOR UPDATE SKIP LOCKED
			)
			RETURNING `+jobColumns,
			workerID, fmt.Sprintf("%f seconds", ttl.Seconds())))
		if err != nil {
			if errors.Is(err, sql.ErrNoRows) {
				return nil // nothing runnable — not an error
			}
			return fmt.Errorf("claim job: %w", err)
		}

		// Close still-open attempts of a reclaimed job: their outcome was
		// never observed.
		if _, err := tx.ExecContext(ctx, `
			UPDATE job_attempts
			SET status = 'unknown', finished_at = now()
			WHERE tenant_id = $1 AND job_id = $2 AND finished_at IS NULL`,
			claimed.TenantID, claimed.ID); err != nil {
			return fmt.Errorf("close stale attempts: %w", err)
		}

		attemptID := newAttemptID()
		if _, err := tx.ExecContext(ctx, `
			INSERT INTO job_attempts (tenant_id, id, job_id, fence_token,
			                          status, worker_id)
			VALUES ($1, $2, $3, $4, 'running', $5)`,
			claimed.TenantID, attemptID, claimed.ID, claimed.FenceToken, workerID); err != nil {
			return fmt.Errorf("insert attempt: %w", err)
		}

		out = &jobs.ClaimedJob{
			JobRow: claimed,
			Lease: jobs.Lease{
				JobID:      claimed.ID,
				Owner:      workerID,
				FenceToken: claimed.FenceToken,
			},
			AttemptID: attemptID,
		}
		if claimed.LeaseExpiresAt != nil {
			out.Lease.ExpiresAt = *claimed.LeaseExpiresAt
		}
		return nil
	})
	if err != nil {
		return nil, err
	}
	return out, nil
}

func (r *JobsRepo) ExtendLease(ctx context.Context, tenantID, jobID, owner string, fence int64, ttl time.Duration) (time.Time, error) {
	var expires time.Time
	err := r.db.SQL.QueryRowContext(ctx, `
		UPDATE jobs
		SET lease_expires_at = now() + $5::interval
		WHERE tenant_id = $1 AND id = $2 AND status = 'running'
		  AND lease_owner = $3 AND fence_token = $4
		RETURNING lease_expires_at`,
		tenantID, jobID, owner, fence,
		fmt.Sprintf("%f seconds", ttl.Seconds())).Scan(&expires)
	if err != nil {
		return time.Time{}, fmt.Errorf("extend lease: %w",
			mapNoRowsAs(err, jobs.ErrStaleFence))
	}
	return expires, nil
}

func (r *JobsRepo) WriteProgress(ctx context.Context, tenantID, jobID, owner string, fence int64, progress json.RawMessage) error {
	res, err := r.db.SQL.ExecContext(ctx, `
		UPDATE jobs
		SET progress = $5::jsonb
		WHERE tenant_id = $1 AND id = $2 AND status = 'running'
		  AND lease_owner = $3 AND fence_token = $4`,
		tenantID, jobID, owner, fence, progress)
	return fenced(res, err)
}

func (r *JobsRepo) CancelRequested(ctx context.Context, tenantID, jobID, owner string, fence int64) (bool, error) {
	var requested bool
	err := r.db.SQL.QueryRowContext(ctx, `
		SELECT cancel_requested_at IS NOT NULL
		FROM jobs
		WHERE tenant_id = $1 AND id = $2 AND status = 'running'
		  AND lease_owner = $3 AND fence_token = $4`,
		tenantID, jobID, owner, fence).Scan(&requested)
	if err != nil {
		return false, fmt.Errorf("cancel probe: %w",
			mapNoRowsAs(err, jobs.ErrStaleFence))
	}
	return requested, nil
}

func (r *JobsRepo) MarkDispatched(ctx context.Context, tenantID, jobID, owner string, fence int64, attemptID string) error {
	res, err := r.db.SQL.ExecContext(ctx, `
		UPDATE jobs
		SET dispatched_attempt_id = $5
		WHERE tenant_id = $1 AND id = $2 AND status = 'running'
		  AND lease_owner = $3 AND fence_token = $4`,
		tenantID, jobID, owner, fence, attemptID)
	return fenced(res, err)
}

// CompleteJob ends the job succeeded — or cancelled when the owner requested
// cancellation during the run — and finishes the attempt. Returns the
// terminal status written.
func (r *JobsRepo) CompleteJob(ctx context.Context, tenantID, jobID, owner string, fence int64, attemptID, resultRef string) (string, error) {
	var status string
	err := WithTx(ctx, r.db.SQL, func(tx *sql.Tx) error {
		err := tx.QueryRowContext(ctx, `
			UPDATE jobs
			SET status = CASE WHEN cancel_requested_at IS NOT NULL
			                  THEN 'cancelled' ELSE 'succeeded' END,
			    result_ref = $5,
			    finished_at = now(),
			    lease_owner = NULL, lease_expires_at = NULL,
			    dispatched_attempt_id = NULL
			WHERE tenant_id = $1 AND id = $2 AND status = 'running'
			  AND lease_owner = $3 AND fence_token = $4
			RETURNING status`,
			tenantID, jobID, owner, fence, nullString(resultRef)).Scan(&status)
		if err != nil {
			return fmt.Errorf("complete job: %w", mapNoRowsAs(err, jobs.ErrStaleFence))
		}
		res, err := tx.ExecContext(ctx, `
			UPDATE job_attempts
			SET status = 'succeeded', finished_at = now()
			WHERE tenant_id = $1 AND id = $2 AND finished_at IS NULL`,
			tenantID, attemptID)
		if err != nil {
			return fmt.Errorf("finish attempt: %w", err)
		}
		if n, _ := res.RowsAffected(); n == 0 {
			return jobs.ErrStaleFence
		}
		return nil
	})
	return status, err
}

// FailJob records the failed attempt and requeues (retry: status=queued,
// lease_expires_at doubles as the not-before timestamp) or fails the job.
func (r *JobsRepo) FailJob(ctx context.Context, tenantID, jobID, owner string, fence int64, attemptID string, retry bool, notBefore time.Time, errDoc json.RawMessage) error {
	return WithTx(ctx, r.db.SQL, func(tx *sql.Tx) error {
		res, err := tx.ExecContext(ctx, `
			UPDATE jobs
			SET status = CASE WHEN $5 THEN 'queued' ELSE 'failed' END,
			    lease_owner = NULL,
			    lease_expires_at = CASE WHEN $5 THEN $6::timestamptz ELSE NULL END,
			    dispatched_attempt_id = NULL,
			    error = $7::jsonb,
			    finished_at = CASE WHEN $5 THEN NULL ELSE now() END
			WHERE tenant_id = $1 AND id = $2 AND status = 'running'
			  AND lease_owner = $3 AND fence_token = $4`,
			tenantID, jobID, owner, fence, retry, notBefore, errDoc)
		if ferr := fenced(res, err); ferr != nil {
			return fmt.Errorf("fail job: %w", ferr)
		}
		res, err = tx.ExecContext(ctx, `
			UPDATE job_attempts
			SET status = 'failed', finished_at = now(), error = $3::jsonb
			WHERE tenant_id = $1 AND id = $2 AND finished_at IS NULL`,
			tenantID, attemptID, errDoc)
		if err != nil {
			return fmt.Errorf("finish attempt: %w", err)
		}
		if n, _ := res.RowsAffected(); n == 0 {
			return jobs.ErrStaleFence
		}
		return nil
	})
}

// ReleaseJob abandons the lease without consuming dispatch: the job returns
// to queued immediately and the still-open attempt is closed as unknown —
// its outcome was never observed. Used for graceful shutdown.
func (r *JobsRepo) ReleaseJob(ctx context.Context, tenantID, jobID, owner string, fence int64, attemptID string) error {
	return WithTx(ctx, r.db.SQL, func(tx *sql.Tx) error {
		res, err := tx.ExecContext(ctx, `
			UPDATE jobs
			SET status = 'queued',
			    lease_owner = NULL, lease_expires_at = NULL,
			    dispatched_attempt_id = NULL
			WHERE tenant_id = $1 AND id = $2 AND status = 'running'
			  AND lease_owner = $3 AND fence_token = $4`,
			tenantID, jobID, owner, fence)
		if ferr := fenced(res, err); ferr != nil {
			return fmt.Errorf("release job: %w", ferr)
		}
		if _, err := tx.ExecContext(ctx, `
			UPDATE job_attempts
			SET status = 'unknown', finished_at = now()
			WHERE tenant_id = $1 AND id = $2 AND finished_at IS NULL`,
			tenantID, attemptID); err != nil {
			return fmt.Errorf("close attempt: %w", err)
		}
		return nil
	})
}

// MarkUnknownJob transitions running→unknown, finishing the dispatched
// attempt as unknown. The job is retained for audit; never requeued.
func (r *JobsRepo) MarkUnknownJob(ctx context.Context, tenantID, jobID, owner string, fence int64, attemptID string, errDoc json.RawMessage) error {
	return WithTx(ctx, r.db.SQL, func(tx *sql.Tx) error {
		res, err := tx.ExecContext(ctx, `
			UPDATE jobs
			SET status = 'unknown', finished_at = now(),
			    error = $5::jsonb,
			    lease_owner = NULL, lease_expires_at = NULL
			WHERE tenant_id = $1 AND id = $2 AND status = 'running'
			  AND lease_owner = $3 AND fence_token = $4`,
			tenantID, jobID, owner, fence, errDoc)
		if ferr := fenced(res, err); ferr != nil {
			return fmt.Errorf("mark unknown: %w", ferr)
		}
		res, err = tx.ExecContext(ctx, `
			UPDATE job_attempts
			SET status = 'unknown', finished_at = now(), error = $3::jsonb
			WHERE tenant_id = $1 AND id = $2 AND finished_at IS NULL`,
			tenantID, attemptID, errDoc)
		if err != nil {
			return fmt.Errorf("finish attempt: %w", err)
		}
		if n, _ := res.RowsAffected(); n == 0 {
			return jobs.ErrStaleFence
		}
		return nil
	})
}

// CancelJob sets cancel_requested_at and immediately cancels queued work;
// running work flips on the worker's next fenced write (JOB-02).
func (r *JobsRepo) CancelJob(ctx context.Context, tenantID, jobID string) error {
	res, err := r.db.SQL.ExecContext(ctx, `
		UPDATE jobs
		SET cancel_requested_at = COALESCE(cancel_requested_at, now()),
		    status = CASE WHEN status = 'queued' THEN 'cancelled' ELSE status END,
		    finished_at = CASE WHEN status = 'queued' THEN now() ELSE finished_at END
		WHERE tenant_id = $1 AND id = $2
		  AND status IN ('queued','running')`,
		tenantID, jobID)
	if err != nil {
		return err
	}
	n, _ := res.RowsAffected()
	if n == 0 {
		return jobs.ErrJobNotFound
	}
	return nil
}

func (r *JobsRepo) DeleteJob(ctx context.Context, tenantID, jobID string) error {
	res, err := r.db.SQL.ExecContext(ctx, `
		UPDATE jobs
		SET status = 'deleted', deleted_at = now()
		WHERE tenant_id = $1 AND id = $2 AND status <> 'deleted'`,
		tenantID, jobID)
	if err != nil {
		return err
	}
	n, _ := res.RowsAffected()
	if n == 0 {
		return jobs.ErrJobNotFound
	}
	return nil
}

// mapNoRowsAs maps "no row matched" to the caller's sentinel: a fenced
// probe that matches nothing means the lease is stale, not missing.
func mapNoRowsAs(err error, sentinel error) error {
	if errors.Is(err, sql.ErrNoRows) || isInvalidUUID(err) {
		return sentinel
	}
	return err
}
