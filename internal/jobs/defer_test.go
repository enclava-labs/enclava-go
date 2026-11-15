package jobs_test

import (
	"context"
	"errors"
	"testing"
	"time"

	"enclava-go/internal/auth"
	"enclava-go/internal/jobs"
)

// deferCause is the handler-side "dependency unavailable" error.
var deferCause = errors.New("extract: extraction worker unavailable")

// rowOf reads the job through the owner-scoped service projection.
func rowOf(t *testing.T, svc *jobs.Service, p auth.Principal, id string) jobs.Job {
	t.Helper()
	job, err := svc.Get(context.Background(), p, id)
	if err != nil {
		t.Fatalf("get job: %v", err)
	}
	return job
}

// TestDeferRequeuesWithoutConsumingAttempt pins the FailureDeferred
// contract: back to queued with the delay honoured, the claim's attempt
// increment undone, counters persisted — after N deferrals attempts is
// still 0 and only the next real claim consumes one.
func TestDeferRequeuesWithoutConsumingAttempt(t *testing.T) {
	svc, _, tenant := setup(t)
	ctx := context.Background()
	p := principal(tenant, "a", "c")
	job := enqueue(t, svc, p, req(tenant, "k-defer", "op.embed", "h", 1))

	claim := leaseJob(t, svc, job.ID)
	if err := claim.Fail(ctx, jobs.Deferred(deferCause, 500*time.Millisecond), jobs.FailureDeferred); err != nil {
		t.Fatalf("defer: %v", err)
	}
	got := rowOf(t, svc, p, job.ID)
	if got.Status != jobs.StatusQueued {
		t.Fatalf("status = %q, want queued", got.Status)
	}
	if got.Attempt != 0 {
		t.Fatalf("attempts = %d, want 0 (deferral must not consume)", got.Attempt)
	}
	if got.Deferrals != 1 || got.DropDeferrals != 0 {
		t.Fatalf("deferrals = %d/%d, want 1/0", got.Deferrals, got.DropDeferrals)
	}
	if got.FirstDeferredAt == nil {
		t.Fatal("first_deferred_at must be recorded on the first deferral")
	}
	if got.Error != deferCause.Error() {
		t.Fatalf("error = %q, want %q", got.Error, deferCause.Error())
	}

	// The delay gates the next claim: still pending immediately after.
	if c, err := svc.Lease(ctx, "w2", time.Minute); err != nil || c != nil {
		t.Fatalf("lease before run-after returned %v, %v; want nothing runnable", c, err)
	}
	time.Sleep(600 * time.Millisecond)

	// Second claim consumes one attempt; the handler defers again.
	claim = leaseJob(t, svc, job.ID)
	if claim.Job().Deferrals != 1 {
		t.Fatalf("claim.Job().Deferrals = %d, want 1 (counters must reach the handler)", claim.Job().Deferrals)
	}
	if err := claim.Fail(ctx, jobs.Deferred(deferCause, time.Second), jobs.FailureDeferred); err != nil {
		t.Fatalf("second defer: %v", err)
	}
	got = rowOf(t, svc, p, job.ID)
	if got.Attempt != 0 || got.Deferrals != 2 {
		t.Fatalf("after two deferrals attempts/deferrals = %d/%d, want 0/2", got.Attempt, got.Deferrals)
	}
	if got.Status != jobs.StatusQueued {
		t.Fatalf("status = %q, want queued", got.Status)
	}
}

// TestDeferDropCounterSeparatesMidRequestDrops pins that DeferredDrop bumps
// only drop_deferrals — the counter ingest caps at 3.
func TestDeferDropCounterSeparatesMidRequestDrops(t *testing.T) {
	svc, _, tenant := setup(t)
	ctx := context.Background()
	p := principal(tenant, "a", "c")
	job := enqueue(t, svc, p, req(tenant, "k-drop", "op.embed", "h", 1))

	claim := leaseJob(t, svc, job.ID)
	if err := claim.Fail(ctx, jobs.DeferredDrop(errors.New("EOF mid-response"), time.Second), jobs.FailureDeferred); err != nil {
		t.Fatalf("defer drop: %v", err)
	}
	got := rowOf(t, svc, p, job.ID)
	if got.Deferrals != 1 || got.DropDeferrals != 1 {
		t.Fatalf("deferrals = %d/%d, want 1/1", got.Deferrals, got.DropDeferrals)
	}
}

// TestDeferStaleFenceRejected pins fencing on the deferral write: a worker
// that lost its lease cannot defer the job.
func TestDeferStaleFenceRejected(t *testing.T) {
	svc, _, tenant := setup(t)
	ctx := context.Background()
	p := principal(tenant, "a", "c")
	job := enqueue(t, svc, p, req(tenant, "k-defer-fence", "op.embed", "h", 1))

	stale, err := svc.Lease(ctx, "worker-stale", 60*time.Millisecond)
	if err != nil || stale == nil {
		t.Fatalf("lease: %v", err)
	}
	time.Sleep(80 * time.Millisecond) // lease expires
	fresh, err := svc.Lease(ctx, "worker-fresh", testTTL)
	if err != nil || fresh == nil || fresh.Job().ID != job.ID {
		t.Fatalf("reclaim: %v (%v)", fresh, err)
	}
	if err := stale.Fail(ctx, jobs.Deferred(deferCause, time.Second), jobs.FailureDeferred); !jobs.IsStaleFence(err) {
		t.Fatalf("stale defer err = %v, want ErrStaleFence", err)
	}
	got := rowOf(t, svc, p, job.ID)
	if got.Deferrals != 0 || got.Status != jobs.StatusRunning {
		t.Fatalf("stale deferral mutated the row: deferrals=%d status=%s", got.Deferrals, got.Status)
	}
}

// TestRunnerDefersThenSucceeds pins the end-to-end runner translation: a
// handler returning jobs.Deferred requeues without consuming an attempt and
// the next claim runs to success with exactly one consumed attempt.
func TestRunnerDefersThenSucceeds(t *testing.T) {
	svc, _, tenant := runnerSetup(t)
	p := principal(tenant, "a", "c")
	job := enqueue(t, svc, p, req(tenant, "k-runner-defer", "op.echo", "h", 1))

	var calls int
	r := jobs.NewRunner(svc, stubResolver{body: "payload"}, map[string]jobs.HandlerFunc{
		"op.echo": func(ctx context.Context, lease *jobs.Claim, open jobs.InputFunc) (string, error) {
			calls++
			if calls == 1 {
				return "", jobs.Deferred(deferCause, 20*time.Millisecond)
			}
			if err := lease.MarkDispatched(ctx); err != nil {
				return "", err
			}
			return "", nil
		},
	}, jobs.RunnerConfig{PollInterval: 15 * time.Millisecond, LeaseTTL: time.Second, HeartbeatEvery: 300 * time.Millisecond})

	runCtx, stop := context.WithCancel(context.Background())
	done := make(chan error, 1)
	go func() { done <- r.Run(runCtx) }()
	defer func() {
		stop()
		<-done
	}()
	final := waitStatus(t, svc, p, job.ID, jobs.StatusSucceeded, 5*time.Second)
	if final.Attempt != 1 || final.Deferrals != 1 {
		t.Fatalf("attempts/deferrals = %d/%d, want 1/1", final.Attempt, final.Deferrals)
	}
	if calls != 2 {
		t.Fatalf("handler calls = %d, want 2", calls)
	}
}
