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

	"enclava-go/internal/extract"
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

func (r *JobsRepo) CountByOperationStatus(ctx context.Context) ([]jobs.JobCount, error) {
	rows, err := r.db.SQL.QueryContext(ctx, `SELECT operation, status, COUNT(*) FROM jobs WHERE deleted_at IS NULL GROUP BY operation, status ORDER BY operation, status`)
	if err != nil {
		return nil, fmt.Errorf("count jobs by operation and status: %w", err)
	}
	defer func() { _ = rows.Close() }()
	var counts []jobs.JobCount
	for rows.Next() {
		var count jobs.JobCount
		if err := rows.Scan(&count.Operation, &count.Status, &count.Count); err != nil {
			return nil, fmt.Errorf("scan job count: %w", err)
		}
		counts = append(counts, count)
	}
	if err := rows.Err(); err != nil {
		return nil, fmt.Errorf("read job counts: %w", err)
	}
	return counts, nil
}

const jobColumns = `tenant_id::text, id, operation, status, connection_id,
	object_id, observed_revision, idempotency_key, input_hash, input_ref,
	principal, submitter_key, policy, attempts, max_attempts, fence_token,
	lease_owner, lease_expires_at, dispatched_attempt_id, result_ref, progress, error,
	submitted_at, updated_at, started_at, finished_at, deleted_at`

// scanJobRow decodes jobColumns order plus any caller-supplied trailing
func scanJobRow(s rowScanner, extra ...any) (jobs.JobRow, error) {
	var row jobs.JobRow
	var connID, objectID, inputRef, submitterKey, resultRef, dispatched sql.NullString
	var observed sql.NullInt64
	var leaseOwner sql.NullString
	var leaseExpires, startedAt, finishedAt, deletedAt sql.NullTime
	var principal, policy, progress, errDoc []byte
	dests := make([]any, 0, 27+len(extra))
	dests = append(dests,
		&row.TenantID, &row.ID, &row.Operation, &row.Status, &connID,
		&objectID,
		&observed, &row.IdempotencyKey, &row.InputHash, &inputRef, &principal,
		&submitterKey, &policy, &row.Attempt, &row.MaxAttempts, &row.FenceToken,
		&leaseOwner, &leaseExpires, &dispatched, &resultRef, &progress, &errDoc,
		&row.CreatedAt, &row.UpdatedAt, &startedAt, &finishedAt, &deletedAt)
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
	// Real counts only: an empty progress doc (or percent 0) never carries,
	// so consumers can render Progress verbatim without inventing a percent.
	if len(progress) > 0 {
		var counts struct {
			Percent int `json:"percent"`
		}
		if json.Unmarshal(progress, &counts) == nil && counts.Percent > 0 {
			row.Progress = progress
		}
	}
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
			Message       string `json:"message"`
			Reason        string `json:"reason"`
			ReservationID string `json:"reservation_id"`
		}
		if json.Unmarshal(errDoc, &doc) == nil {
			row.Error = doc.Message
			row.OutcomeReason = doc.Reason
			row.OutcomeReservationID = doc.ReservationID
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
// unique index signaling a same-key insert.
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
		                  policy, max_attempts, lane)
		VALUES ($1,$2,$3,'queued',$4,$5,$6,$7,$8,$9,$10,$11,$12::jsonb,$13,COALESCE(NULLIF($14, ''), 'interactive'))`,
		rec.TenantID, rec.ID, rec.Operation,
		nullString(rec.Subject.ConnectionID), nullString(rec.Subject.ObjectID),
		nullInt64(rec.Subject.ObservedRevision), rec.IdempotencyKey,
		rec.InputHash, nullString(rec.InputRef), rec.Principal,
		rec.SubmitterKey, policy, rec.MaxAttempts, string(rec.Lane))
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
func (r *JobsRepo) GetIdempotency(ctx context.Context, tenantID, operation, key string) (jobs.JobRow, error) {
	row, err := scanJobRow(r.db.SQL.QueryRowContext(ctx, `
		SELECT `+jobColumns+` FROM jobs
		WHERE tenant_id = $1 AND operation = $2 AND idempotency_key = $3`,
		tenantID, operation, key))
	if err != nil {
		return jobs.JobRow{}, fmt.Errorf("get idempotent job: %w", mapNoRows(err))
	}
	return row, nil
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

// appendJobPredicate appends an `column IN (...)` predicate with positional
// placeholders; predicates apply before ORDER BY/LIMIT so the filtered count
// IS the total. Empty slices append nothing.
func appendJobPredicate(query string, args []any, column string, values []string) (string, []any) {
	if len(values) == 0 {
		return query, args
	}
	placeholders := make([]string, len(values))
	for i, v := range values {
		args = append(args, v)
		placeholders[i] = "$" + strconv.Itoa(len(args))
	}
	return query + ` AND ` + column + ` IN (` + strings.Join(placeholders, ",") + `)`, args
}

// ListOwnedJobsFiltered lists the submitter's non-deleted rows restricted to
// the given operations and statuses — predicates apply in SQL before LIMIT.
// Nil slices skip the predicate.
func (r *JobsRepo) ListOwnedJobsFiltered(ctx context.Context, tenantID, submitterKey string, ops, statuses []string, limit int) ([]jobs.JobRow, error) {
	if limit <= 0 {
		limit = 50
	}
	query := `
		SELECT ` + jobColumns + ` FROM jobs
		WHERE tenant_id = $1 AND submitter_key = $2 AND deleted_at IS NULL`
	args := []any{tenantID, submitterKey}
	query, args = appendJobPredicate(query, args, "operation", ops)
	query, args = appendJobPredicate(query, args, "status", statuses)
	//nolint:gosec // limit is an integer, not SQL input
	query += `
		ORDER BY submitted_at DESC, id DESC
		LIMIT ` + strconv.Itoa(limit)
	rows, err := r.db.SQL.QueryContext(ctx, query, args...)
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

// ListOwnedJobs keeps the operation-unfiltered owner listing for callers that
// page across every operation (jobs.Service.List, connectors).
func (r *JobsRepo) ListOwnedJobs(ctx context.Context, tenantID, submitterKey string, limit int) ([]jobs.JobRow, error) {
	return r.ListOwnedJobsFiltered(ctx, tenantID, submitterKey, nil, nil, limit)
}

// listActivityRows is the shared activity query: every predicate (tenant,
// optional actor, operations, statuses) applies in SQL BEFORE the LIMIT —
// the non-starvation rule, so an old queued job behind a page of completed
// rows and an old failure behind newer successes still surface. Newest first.
func (r *JobsRepo) listActivityRows(ctx context.Context, tenantID, actorPredicate, actorID string, ops, statuses []string, limit int) ([]jobs.JobRow, error) {
	if limit <= 0 {
		limit = 50
	}
	query := `
		SELECT ` + jobColumns + ` FROM jobs
		WHERE tenant_id = $1 AND deleted_at IS NULL`
	args := []any{tenantID}
	if actorPredicate != "" {
		query += ` AND ` + actorPredicate
		args = append(args, actorID)
	}
	query, args = appendJobPredicate(query, args, "operation", ops)
	query, args = appendJobPredicate(query, args, "status", statuses)
	//nolint:gosec // limit is an integer, not SQL input
	query += `
		ORDER BY submitted_at DESC, id DESC
		LIMIT ` + strconv.Itoa(limit)
	rows, err := r.db.SQL.QueryContext(ctx, query, args...)
	if err != nil {
		return nil, fmt.Errorf("list activity jobs: %w", err)
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

// ListActorJobs lists one actor's jobs across every submitter binding — the
// member activity/attention read keyed on the frozen principal's ActorID.
// Read-only: Get/Cancel/replay stay submitter-key bound.
func (r *JobsRepo) ListActorJobs(ctx context.Context, tenantID, actorID string, statuses, ops []string, limit int) ([]jobs.JobRow, error) {
	return r.listActivityRows(ctx, tenantID, `principal->>'ActorID' = $2`, actorID, ops, statuses, limit)
}

// ListManagedJobs lists the tenant's jobs restricted to the given
// operations/statuses — the capability-gated manager view; no actor filter.
func (r *JobsRepo) ListManagedJobs(ctx context.Context, tenantID string, ops, statuses []string, limit int) ([]jobs.JobRow, error) {
	return r.listActivityRows(ctx, tenantID, "", "", ops, statuses, limit)
}

// ListConnectionJobs lists a connection's non-deleted jobs regardless of
// submitter — scheduled syncs run under the scheduler's principal, and the
// source's managers must still see them. Tenant + connection scoping is the
// authorization boundary. operation/status "" match any; filtering happens
// before the limit, so a sync's thousands of ingest jobs never push its
// connector.sync rows out of the window.
func (r *JobsRepo) ListConnectionJobs(ctx context.Context, tenantID, connectionID, operation, status string, limit int) ([]jobs.JobRow, error) {
	if limit <= 0 {
		limit = 50
	}
	query := `
		SELECT ` + jobColumns + ` FROM jobs
		WHERE tenant_id = $1 AND connection_id = $2 AND deleted_at IS NULL
		  AND ($3 = '' OR operation = $3) AND ($4 = '' OR status = $4)`
	args := []any{tenantID, connectionID, operation, status}
	query += ` ORDER BY submitted_at DESC, id DESC LIMIT ` + strconv.Itoa(limit) //nolint:gosec // limit is an int, cannot inject SQL
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

// CountOwnedJobsFiltered counts the submitter's non-deleted rows restricted
// to the given operations — the total the filtered list pages over.
func (r *JobsRepo) CountOwnedJobsFiltered(ctx context.Context, tenantID, submitterKey string, ops []string) (int, error) {
	query := `
		SELECT COUNT(*) FROM jobs
		WHERE tenant_id = $1 AND submitter_key = $2 AND deleted_at IS NULL`
	args := []any{tenantID, submitterKey}
	query, args = appendJobPredicate(query, args, "operation", ops)
	var total int
	if err := r.db.SQL.QueryRowContext(ctx, query, args...).Scan(&total); err != nil {
		return 0, fmt.Errorf("count owned jobs: %w", err)
	}
	return total, nil
}

// CountOwnedJobs counts the submitter's non-deleted rows — the total the
// operation-unfiltered owner-scoped list is paged over.
func (r *JobsRepo) CountOwnedJobs(ctx context.Context, tenantID, submitterKey string) (int, error) {
	return r.CountOwnedJobsFiltered(ctx, tenantID, submitterKey, nil)
}

// CountOwnedJobsSinceFiltered counts the submitter's non-deleted rows
// restricted to the given operations submitted at or after `since` — the
// operation-scoped "recent" window stat.
func (r *JobsRepo) CountOwnedJobsSinceFiltered(ctx context.Context, tenantID, submitterKey string, ops []string, since time.Time) (int, error) {
	query := `
		SELECT COUNT(*) FROM jobs
		WHERE tenant_id = $1 AND submitter_key = $2 AND deleted_at IS NULL
		  AND submitted_at >= $3`
	args := []any{tenantID, submitterKey, since}
	query, args = appendJobPredicate(query, args, "operation", ops)
	var total int
	if err := r.db.SQL.QueryRowContext(ctx, query, args...).Scan(&total); err != nil {
		return 0, fmt.Errorf("count owned jobs since: %w", err)
	}
	return total, nil
}

// CountOwnedJobsSince counts the submitter's non-deleted rows submitted at or
// after `since` — the unfiltered "recent" window stat.
func (r *JobsRepo) CountOwnedJobsSince(ctx context.Context, tenantID, submitterKey string, since time.Time) (int, error) {
	return r.CountOwnedJobsSinceFiltered(ctx, tenantID, submitterKey, nil, since)
}

// CountLiveArtifactReferences protects a shared artifact from one job's
// explicit deletion; deleted audit rows do not retain ownership.
func (r *JobsRepo) CountLiveArtifactReferences(ctx context.Context, tenantID, artifactRef, excludingJobID string) (int, error) {
	var count int
	err := r.db.SQL.QueryRowContext(ctx, `
		SELECT COUNT(*) FROM jobs
		WHERE tenant_id=$1::uuid AND id<>$3 AND deleted_at IS NULL
		  AND status<>'deleted' AND (input_ref=$2 OR result_ref=$2)`,
		tenantID, artifactRef, excludingJobID).Scan(&count)
	return count, err
}

// ListDeletedExtractJobs resumes artifact cleanup initiated by an explicit
// extraction delete after an application restart.
func (r *JobsRepo) ListDeletedExtractJobs(ctx context.Context) ([]jobs.JobRow, error) {
	rows, err := r.db.SQL.QueryContext(ctx, `
		SELECT `+jobColumns+` FROM jobs
		WHERE operation IN ($1,$2) AND status='deleted'`,
		extract.OperationProcess, extract.OperationVision)
	if err != nil {
		return nil, err
	}
	defer func() { _ = rows.Close() }()
	out := []jobs.JobRow{}
	for rows.Next() {
		row, err := scanJobRow(rows)
		if err != nil {
			return nil, err
		}
		out = append(out, row)
	}
	return out, rows.Err()
}

// Claim is the atomic lease grant (JOB-02). Inside one transaction it:
//  1. sweeps running jobs with an expired lease AND an unfinished dispatched
//     attempt to status=unknown — their upstream outcome was never observed
//     so they are never silently re-leased;
//  2. picks the next runnable row FOR UPDATE SKIP LOCKED (queued past its
//     not-before lease_expires_at, or running with an expired lease and no
//     unfinished dispatched attempt), bumps fence_token, stamps the lease,
//     closes any still-open prior attempts and inserts the new attempt row.
func (r *JobsRepo) Claim(ctx context.Context, workerID string, ttl time.Duration, lane jobs.Lane) (*jobs.ClaimedJob, error) {
	var out *jobs.ClaimedJob
	err := WithTx(ctx, r.db.SQL, func(tx *sql.Tx) error {
		// 1. Expired-dispatch sweep → unknown (job + its open attempt).
		if _, err := tx.ExecContext(ctx, `
			WITH swept AS (
				UPDATE jobs j
				SET status = 'unknown', finished_at = now(), updated_at = now(),
				    error = jsonb_build_object(
				        'message', 'interrupted after the request was sent; usage may have been billed',
				        'retryable', false, 'reason', 'interrupted',
				        'reservation_id', (SELECT r.id FROM reservations r
				            WHERE r.tenant_id = j.tenant_id AND r.job_id = j.id
				              AND r.attempt_id = j.dispatched_attempt_id ORDER BY r.created_at DESC, r.id DESC LIMIT 1)),
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
			    updated_at = now(),
			    attempts = j.attempts + 1,
			    started_at = COALESCE(j.started_at, now()),
			    dispatched_attempt_id = NULL,
			    error = NULL
			WHERE (j.tenant_id, j.id) = (
				SELECT tenant_id, id FROM jobs c
				WHERE c.cancel_requested_at IS NULL
				  AND c.deleted_at IS NULL
				  AND ($3 = '' OR c.lane = $3)
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
			workerID, fmt.Sprintf("%f seconds", ttl.Seconds()), string(lane)))
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
		SET lease_expires_at = now() + $5::interval, updated_at = now()
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
		SET progress = $5::jsonb, updated_at = now()
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
		SET dispatched_attempt_id = $5, updated_at = now()
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
			    finished_at = now(), updated_at = now(),
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
			    error = $7::jsonb, updated_at = now(),
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
			    dispatched_attempt_id = NULL, updated_at = now()
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
			UPDATE jobs j
			SET status = 'unknown', finished_at = now(), updated_at = now(),
			    error = $5::jsonb || jsonb_build_object(
			        'reservation_id', (SELECT r.id FROM reservations r
			            WHERE r.tenant_id = j.tenant_id AND r.job_id = j.id
			              AND r.attempt_id = $6 ORDER BY r.created_at DESC, r.id DESC LIMIT 1)),
			    lease_owner = NULL, lease_expires_at = NULL
			WHERE j.tenant_id = $1 AND j.id = $2 AND j.status = 'running'
			  AND j.lease_owner = $3 AND j.fence_token = $4`,
			tenantID, jobID, owner, fence, errDoc, attemptID)
		if ferr := fenced(res, err); ferr != nil {
			return fmt.Errorf("mark unknown: %w", ferr)
		}
		res, err = tx.ExecContext(ctx, `
			UPDATE job_attempts a
			SET status = 'unknown', finished_at = now(),
			    error = $3::jsonb || jsonb_build_object(
			        'reservation_id', (SELECT r.id FROM reservations r
			            WHERE r.tenant_id = a.tenant_id AND r.job_id = a.job_id
			              AND r.attempt_id = a.id ORDER BY r.created_at DESC, r.id DESC LIMIT 1))
			WHERE a.tenant_id = $1 AND a.id = $2 AND a.finished_at IS NULL`,
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
		    updated_at = now(),
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
		SET status = 'deleted', deleted_at = now(), updated_at = now()
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

func (r *JobsRepo) CountActivePreviews(ctx context.Context, tenantID string) (int, error) {
	var count int
	err := r.db.SQL.QueryRowContext(ctx, `
		SELECT count(*) FROM jobs
		WHERE tenant_id = $1 AND operation = $2 AND deleted_at IS NULL
		  AND status IN ('queued','running')`, tenantID, extract.OperationPreview).Scan(&count)
	return count, err
}

func (r *JobsRepo) CountActiveExtractions(ctx context.Context, tenantID string) (int, error) {
	var count int
	err := r.db.SQL.QueryRowContext(ctx, `
		SELECT count(*) FROM jobs
		WHERE tenant_id = $1 AND operation IN ($2,$3) AND deleted_at IS NULL
		  AND status IN ('queued','running')`,
		tenantID, extract.OperationProcess, extract.OperationVision).Scan(&count)
	return count, err
}
func (r *JobsRepo) ExpiredPreviewJobs(ctx context.Context, before time.Time, limit int) ([]extract.ExpiredPreview, error) {
	rows, err := r.db.SQL.QueryContext(ctx, `
		SELECT tenant_id::text, id, COALESCE(input_ref,''), COALESCE(result_ref,''),
		       COALESCE((policy->>'upload_owned')::boolean, false)
		FROM jobs
		WHERE operation = $1 AND deleted_at IS NULL
		  AND status IN ('succeeded','failed','cancelled','unknown')
		  AND finished_at < $2
		ORDER BY finished_at LIMIT $3`, extract.OperationPreview, before, limit)
	if err != nil {
		return nil, err
	}
	defer func() { _ = rows.Close() }()
	var expired []extract.ExpiredPreview
	for rows.Next() {
		var row extract.ExpiredPreview
		if err := rows.Scan(&row.TenantID, &row.ID, &row.InputRef, &row.ResultRef, &row.UploadOwned); err != nil {
			return nil, err
		}
		expired = append(expired, row)
	}
	return expired, rows.Err()
}

func (r *JobsRepo) DeleteExpiredPreviewJobs(ctx context.Context, ids []string) error {
	for _, id := range ids {
		if _, err := r.db.SQL.ExecContext(ctx, `
			UPDATE jobs SET status = 'deleted', deleted_at = now(), updated_at = now()
			WHERE id = $1 AND operation = $2 AND deleted_at IS NULL
			  AND status IN ('succeeded','failed','cancelled','unknown')`,
			id, extract.OperationPreview); err != nil {
			return err
		}
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
