package postgres

import (
	"context"
	"encoding/json"
	"errors"
	"fmt"
	"sync"
	"testing"
	"time"

	"enclava-go/internal/jobs"
)

func insertJobRow(t *testing.T, ctx context.Context, r *JobsRepo, tenant, key, op string) jobs.Job {
	t.Helper()
	rec := jobs.InsertRecord{
		Job: jobs.Job{
			TenantID:       tenant,
			ID:             "job_" + key + "_" + op,
			Operation:      op,
			Status:         jobs.StatusQueued,
			IdempotencyKey: key,
			Subject:        jobs.Subject{ConnectionID: "c", ObjectID: "o", ObservedRevision: 1},
			InputHash:      "h",
			Policy:         map[string]any{"m": "x"},
			MaxAttempts:    3,
		},
		SubmitterKey: "sub-1",
		Principal:    json.RawMessage(`{"actor":"a"}`),
		PolicyJSON:   []byte(`{"m":"x"}`),
	}
	existing, _, err := r.InsertJob(ctx, rec)
	if err != nil {
		t.Fatalf("insert job: %v", err)
	}
	if existing != nil {
		t.Fatalf("fresh insert returned existing row")
	}
	return rec.Job
}

// TestJobsRepoInsertIdempotentMatch proves the repo's replay path: identical
// resubmission matches (sameKey=true), changed identity matches nothing.
func TestJobsRepoInsertIdempotentMatch(t *testing.T) {
	db := RequireTestDB(t)
	ctx := context.Background()
	tenant := idTenantA
	seedTenant(t, ctx, db, tenant)
	r := NewJobsRepo(db)
	job := insertJobRow(t, ctx, r, tenant, "k1", "op")

	rec := jobs.InsertRecord{
		Job:          job,
		SubmitterKey: "sub-1",
		Principal:    json.RawMessage(`{"actor":"a"}`),
		PolicyJSON:   []byte(`{"m":"x"}`),
	}
	existing, same, err := r.InsertJob(ctx, rec)
	if err != nil {
		t.Fatalf("replay: %v", err)
	}
	if existing == nil || !same || existing.ID != job.ID {
		t.Fatalf("identical replay must match: %v %v", existing, same)
	}

	rec.SubmitterKey = "sub-2" // different owner
	_, same, err = r.InsertJob(ctx, rec)
	if err != nil || same {
		t.Fatalf("different owner must not match: same=%v err=%v", same, err)
	}
	rec.SubmitterKey = "sub-1"
	rec.InputHash = "other" // different input
	_, same, err = r.InsertJob(ctx, rec)
	if err != nil || same {
		t.Fatalf("different input must not match: same=%v err=%v", same, err)
	}
	rec.InputHash = "h"
	rec.PolicyJSON = []byte(`{"m":"y"}`) // different policy
	_, same, err = r.InsertJob(ctx, rec)
	if err != nil || same {
		t.Fatalf("different policy must not match: same=%v err=%v", same, err)
	}
}

// TestJobsRepoClaimRaces proves the fenced claim hands one queued job to
// exactly one concurrent claimant.
func TestJobsRepoClaimRaces(t *testing.T) {
	db := RequireTestDB(t)
	ctx := context.Background()
	tenant := idTenantA
	seedTenant(t, ctx, db, tenant)
	r := NewJobsRepo(db)
	insertJobRow(t, ctx, r, tenant, "k1", "op")

	out := make(chan *jobs.ClaimedJob, 4)
	var wg sync.WaitGroup
	for i := range 4 {
		wg.Add(1)
		go func(i int) {
			defer wg.Done()
			cj, err := r.Claim(ctx, string(rune('a'+i)), time.Second)
			if err != nil {
				t.Errorf("claim: %v", err)
				return
			}
			out <- cj
		}(i)
	}
	wg.Wait()
	close(out)

	var winners []*jobs.ClaimedJob
	for cj := range out {
		if cj != nil {
			winners = append(winners, cj)
		}
	}
	if len(winners) != 1 {
		t.Fatalf("claim must hand job to exactly one worker, got %d", len(winners))
	}
	if winners[0].JobRow.Status != jobs.StatusRunning || winners[0].Lease.FenceToken != 1 {
		t.Fatalf("claimed row: status=%q fence=%d", winners[0].JobRow.Status, winners[0].Lease.FenceToken)
	}
	if winners[0].AttemptID == "" {
		t.Fatalf("claim must insert the attempt row")
	}
	var status string
	if err := db.SQL.QueryRowContext(ctx,
		`SELECT status FROM job_attempts WHERE tenant_id = $1 AND id = $2`,
		tenant, winners[0].AttemptID).Scan(&status); err != nil || status != "running" {
		t.Fatalf("attempt row: status=%q err=%v", status, err)
	}
}

// TestJobsRepoExpiredDispatchSweep proves Claim's first phase: an expired
// running job whose dispatched attempt never finished is swept to unknown —
// never re-leased.
func TestJobsRepoExpiredDispatchSweep(t *testing.T) {
	db := RequireTestDB(t)
	ctx := context.Background()
	tenant := idTenantA
	seedTenant(t, ctx, db, tenant)
	r := NewJobsRepo(db)
	job := insertJobRow(t, ctx, r, tenant, "k1", "op")

	cj, err := r.Claim(ctx, "w1", 60*time.Millisecond)
	if err != nil || cj == nil {
		t.Fatalf("claim: %v", err)
	}
	if err := r.MarkDispatched(ctx, tenant, job.ID, "w1", cj.Lease.FenceToken, cj.AttemptID); err != nil {
		t.Fatalf("mark dispatched: %v", err)
	}
	time.Sleep(80 * time.Millisecond)

	cj2, err := r.Claim(ctx, "w2", time.Second)
	if err != nil {
		t.Fatalf("second claim: %v", err)
	}
	if cj2 != nil {
		t.Fatalf("dispatched expired lease must sweep to unknown, not re-lease")
	}
	row, err := r.GetJob(ctx, tenant, job.ID)
	if err != nil {
		t.Fatalf("get: %v", err)
	}
	if row.Status != jobs.StatusUnknown {
		t.Fatalf("status=%q, want unknown", row.Status)
	}
	var attStatus string
	var finished bool
	if err := db.SQL.QueryRowContext(ctx,
		`SELECT status, finished_at IS NOT NULL FROM job_attempts
		 WHERE tenant_id = $1 AND id = $2`, tenant, cj.AttemptID).
		Scan(&attStatus, &finished); err != nil {
		t.Fatalf("attempt: %v", err)
	}
	if attStatus != "unknown" || !finished {
		t.Fatalf("dispatched attempt must finish unknown, got %q finished=%v", attStatus, finished)
	}
}

// TestJobsRepoUndispatchedReclaimReleases proves the complementary path:
// expired lease with no dispatch → re-leased, open attempt closed.
func TestJobsRepoUndispatchedReclaimReleases(t *testing.T) {
	db := RequireTestDB(t)
	ctx := context.Background()
	tenant := idTenantA
	seedTenant(t, ctx, db, tenant)
	r := NewJobsRepo(db)
	insertJobRow(t, ctx, r, tenant, "k1", "op")

	cj, err := r.Claim(ctx, "w1", 60*time.Millisecond)
	if err != nil || cj == nil {
		t.Fatalf("claim: %v", err)
	}
	time.Sleep(80 * time.Millisecond)
	cj2, err := r.Claim(ctx, "w2", time.Second)
	if err != nil || cj2 == nil {
		t.Fatalf("reclaim: %v cj2=%v", err, cj2)
	}
	if cj2.Lease.FenceToken != cj.Lease.FenceToken+1 {
		t.Fatalf("fence must bump: %d -> %d", cj.Lease.FenceToken, cj2.Lease.FenceToken)
	}
	// Old fenced write now fails.
	if err := r.WriteProgress(ctx, tenant, cj2.JobRow.ID, "w1", cj.Lease.FenceToken,
		json.RawMessage(`{"percent":10}`)); !errors.Is(err, jobs.ErrStaleFence) {
		t.Fatalf("stale fence write must fail, got %v", err)
	}
	var attStatus string
	if err := db.SQL.QueryRowContext(ctx,
		`SELECT status FROM job_attempts WHERE tenant_id = $1 AND id = $2`,
		tenant, cj.AttemptID).Scan(&attStatus); err != nil {
		t.Fatalf("old attempt: %v", err)
	}
	if attStatus != "unknown" {
		t.Fatalf("abandoned attempt must close unknown, got %q", attStatus)
	}
}

// TestJobsRepoRetryableRequeueBackoff proves FailJob retry=true requeues
// with lease_expires_at as the not-before timestamp: the job is not
// claimable before it and claimable after.
func TestJobsRepoRetryableRequeueBackoff(t *testing.T) {
	db := RequireTestDB(t)
	ctx := context.Background()
	tenant := idTenantA
	seedTenant(t, ctx, db, tenant)
	r := NewJobsRepo(db)
	job := insertJobRow(t, ctx, r, tenant, "k1", "op")

	cj, err := r.Claim(ctx, "w1", time.Second)
	if err != nil || cj == nil {
		t.Fatalf("claim: %v", err)
	}
	notBefore := time.Now().Add(120 * time.Millisecond)
	if err := r.FailJob(ctx, tenant, job.ID, "w1", cj.Lease.FenceToken,
		cj.AttemptID, true, notBefore, json.RawMessage(`{"message":"x","retryable":true}`)); err != nil {
		t.Fatalf("fail retryable: %v", err)
	}
	row, _ := r.GetJob(ctx, tenant, job.ID)
	if row.Status != jobs.StatusQueued {
		t.Fatalf("retryable fail must requeue, got %q", row.Status)
	}
	if c, err := r.Claim(ctx, "w2", time.Second); err != nil || c != nil {
		t.Fatalf("job inside backoff must not be claimable")
	}
	time.Sleep(140 * time.Millisecond)
	c2, err := r.Claim(ctx, "w2", time.Second)
	if err != nil || c2 == nil {
		t.Fatalf("job past backoff must be claimable: %v", err)
	}
	if c2.Attempt != 2 {
		t.Fatalf("attempt counter must advance to 2, got %d", c2.Attempt)
	}
}

// insertActorJob inserts one job row carrying the actor key in the frozen
// principal with an explicit status — the row shape the activity queries key
// on (jobs.principal->>'ActorID').
func insertActorJob(t *testing.T, ctx context.Context, r *JobsRepo, tenant, id, op, status, actor string) jobs.Job {
	t.Helper()
	rec := jobs.InsertRecord{
		Job: jobs.Job{
			TenantID: tenant, ID: id, Operation: op, Status: status,
			IdempotencyKey: id,
			Subject:        jobs.Subject{ConnectionID: "c", ObjectID: "o", ObservedRevision: 1},
			InputHash:      "h", Policy: map[string]any{}, MaxAttempts: 3,
		},
		SubmitterKey: "submitter-" + id,
		Principal:    json.RawMessage(`{"ActorID":"` + actor + `"}`),
		PolicyJSON:   []byte(`{}`),
	}
	if _, _, err := r.InsertJob(ctx, rec); err != nil {
		t.Fatalf("insert actor job %s: %v", id, err)
	}
	// InsertJob freezes status='queued' (real transitions go through the
	// fenced claim path); a terminal fixture row flips it explicitly, the
	// same way the run-repo tests seed bound-job states.
	if status != jobs.StatusQueued {
		if _, err := r.db.SQL.ExecContext(ctx,
			`UPDATE jobs SET status = $3 WHERE tenant_id = $1 AND id = $2`,
			tenant, id, status); err != nil {
			t.Fatalf("status %s for %s: %v", status, id, err)
		}
	}
	return rec.Job
}

// TestJobsRepoListActorJobsActorScoped proves the actor key scopes the feed:
// one actor's rows only, within the tenant, filtered by operation/status.
func TestJobsRepoListActorJobsActorScoped(t *testing.T) {
	db := RequireTestDB(t)
	ctx := context.Background()
	tenant := idTenantA
	seedTenant(t, ctx, db, tenant)
	other := idTenantB
	seedTenant(t, ctx, db, other)
	r := NewJobsRepo(db)

	mine := insertActorJob(t, ctx, r, tenant, "job_actor1", "extract.process", jobs.StatusQueued, "actor-1")
	insertActorJob(t, ctx, r, tenant, "job_actor2", "extract.process", jobs.StatusQueued, "actor-2")
	insertActorJob(t, ctx, r, other, "job_other_tenant", "extract.process", jobs.StatusQueued, "actor-1")

	rows, err := r.ListActorJobs(ctx, tenant, "actor-1", nil, nil, 50)
	if err != nil || len(rows) != 1 || rows[0].ID != mine.ID {
		t.Fatalf("actor feed = %#v err=%v, want exactly %s", rows, err, mine.ID)
	}
	// Deleted rows never surface.
	if err := r.DeleteJob(ctx, tenant, mine.ID); err != nil {
		t.Fatalf("delete job: %v", err)
	}
	rows, err = r.ListActorJobs(ctx, tenant, "actor-1", nil, nil, 50)
	if err != nil || len(rows) != 0 {
		t.Fatalf("tombstoned job leaked into the actor feed: %#v err=%v", rows, err)
	}

	// Operation + status predicates narrow the query server-side.
	insertActorJob(t, ctx, r, tenant, "job_run", "chatbot.run", jobs.StatusRunning, "actor-1")
	rows, err = r.ListActorJobs(ctx, tenant, "actor-1", []string{jobs.StatusRunning}, []string{"chatbot.run"}, 50)
	if err != nil || len(rows) != 1 || rows[0].Operation != "chatbot.run" {
		t.Fatalf("filtered actor feed = %#v err=%v, want the chatbot.run row", rows, err)
	}
}

// TestJobsRepoListManagedJobsPredicatesBeforeLimit proves the tenant-wide
// managed query filters in SQL before LIMIT: the older FAILED row stays
// reachable behind a full page of newer successes.
func TestJobsRepoListManagedJobsPredicatesBeforeLimit(t *testing.T) {
	db := RequireTestDB(t)
	ctx := context.Background()
	tenant := idTenantA
	seedTenant(t, ctx, db, tenant)
	r := NewJobsRepo(db)

	failed := insertActorJob(t, ctx, r, tenant, "job_old_failed", "extract.process", jobs.StatusFailed, "actor-1")
	if _, err := db.SQL.ExecContext(ctx,
		`UPDATE jobs SET submitted_at = submitted_at - interval '2 hours' WHERE tenant_id = $1 AND id = $2`,
		tenant, failed.ID); err != nil {
		t.Fatalf("backdate failed job: %v", err)
	}
	for i := range 50 {
		insertActorJob(t, ctx, r, tenant, fmt.Sprintf("job_done_%d", i), "extract.process", jobs.StatusSucceeded, "actor-1")
	}

	// The failed row is not in the newest-50 page, but its own status query
	// still surfaces it.
	page, err := r.ListManagedJobs(ctx, tenant, nil, nil, 50)
	if err != nil || len(page) != 50 {
		t.Fatalf("unfiltered page = %d rows err=%v, want 50", len(page), err)
	}
	for _, row := range page {
		if row.ID == failed.ID {
			t.Fatalf("failed row must not consume the newest-50 page: %#v", page)
		}
	}
	failedRows, err := r.ListManagedJobs(ctx, tenant, nil, []string{jobs.StatusFailed}, 50)
	if err != nil || len(failedRows) != 1 || failedRows[0].ID != failed.ID {
		t.Fatalf("status-filtered managed feed = %#v err=%v, want the old failed row", failedRows, err)
	}
	// Operation filters narrow the tenant-wide read too.
	syncRows, err := r.ListManagedJobs(ctx, tenant, []string{"connector.sync"}, nil, 50)
	if err != nil || len(syncRows) != 0 {
		t.Fatalf("operation-filtered managed feed = %#v err=%v, want none", syncRows, err)
	}
}

// TestJobsRepoActivityProgressRoundTrip proves the activity projection reads
// jobs.progress: a job whose worker published real counts carries the stored
// document; a non-counted row carries none.
func TestJobsRepoActivityProgressRoundTrip(t *testing.T) {
	db := RequireTestDB(t)
	ctx := context.Background()
	tenant := idTenantA
	seedTenant(t, ctx, db, tenant)
	r := NewJobsRepo(db)

	counted := insertActorJob(t, ctx, r, tenant, "job_progress", "extract.process", jobs.StatusQueued, "actor-1")
	blank := insertActorJob(t, ctx, r, tenant, "job_blank", "extract.process", jobs.StatusQueued, "actor-1")

	cj, err := r.Claim(ctx, "w1", time.Second)
	if err != nil || cj == nil || cj.JobRow.ID != counted.ID {
		t.Fatalf("claim counted job: cj=%v err=%v", cj, err)
	}
	if err := r.WriteProgress(ctx, tenant, counted.ID, "w1", cj.Lease.FenceToken,
		json.RawMessage(`{"percent":42,"message":"halfway"}`)); err != nil {
		t.Fatalf("write progress: %v", err)
	}

	rows, err := r.ListActorJobs(ctx, tenant, "actor-1", nil, nil, 50)
	if err != nil {
		t.Fatalf("list actor jobs: %v", err)
	}
	progressByID := map[string]json.RawMessage{}
	for _, row := range rows {
		progressByID[row.ID] = row.Progress
	}
	if got := progressByID[counted.ID]; got == nil {
		t.Fatal("counted row carries no progress")
	} else {
		var doc struct {
			Percent int `json:"percent"`
		}
		if err := json.Unmarshal(got, &doc); err != nil || doc.Percent != 42 {
			t.Fatalf("counted row progress = %q (decoded %+v err=%v), want percent 42", got, doc, err)
		}
	}
	if got := progressByID[blank.ID]; got != nil {
		t.Fatalf("non-counted row carries progress %q, want none", got)
	}
}
