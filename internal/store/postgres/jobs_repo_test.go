package postgres

import (
	"context"
	"encoding/json"
	"errors"
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
