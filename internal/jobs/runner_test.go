package jobs_test

import (
	"context"
	"errors"
	"fmt"
	"io"
	"strings"
	"sync/atomic"
	"testing"
	"time"

	"enclava-go/internal/artifacts"
	"enclava-go/internal/auth"
	"enclava-go/internal/jobs"
	"enclava-go/internal/store/postgres"
	"enclava-go/internal/testdb"
)

// stubResolver serves fixed input bytes — it satisfies jobs.InputResolver's
// Read(ctx, artifacts.Ref) contract without a filesystem.
type stubResolver struct{ body string }

func (s stubResolver) Read(_ context.Context, _ artifacts.Ref) (io.ReadCloser, error) {
	return io.NopCloser(strings.NewReader(s.body)), nil
}

func runnerSetup(t *testing.T) (*jobs.Service, *postgres.DB, string) {
	t.Helper()
	db := testdb.New(t)
	tenant := testdb.SeedTenant(t, db, "t-runner")
	return jobs.NewService(postgres.NewJobsRepo(db), 10*time.Millisecond), db, tenant
}

func waitStatus(t *testing.T, svc *jobs.Service, p auth.Principal, id, want string, timeout time.Duration) jobs.Job {
	t.Helper()
	deadline := time.Now().Add(timeout)
	for time.Now().Before(deadline) {
		job, err := svc.Get(context.Background(), p, id)
		if err == nil && job.Status == want {
			return job
		}
		time.Sleep(15 * time.Millisecond)
	}
	job, _ := svc.Get(context.Background(), p, id)
	t.Fatalf("job %s status = %q, want %q", id, job.Status, want)
	return job
}

func TestRunnerExecutesAndCompletes(t *testing.T) {
	svc, db, tenant := runnerSetup(t)
	ctx := context.Background()
	p := principal(tenant, "a", "c")
	job := enqueue(t, svc, p, req(tenant, "k", "op.echo", "h", 1))

	var gotInput string
	r := jobs.NewRunner(svc, stubResolver{body: "payload"}, map[string]jobs.HandlerFunc{
		"op.echo": func(ctx context.Context, lease *jobs.Claim, open jobs.InputFunc) (string, error) {
			rc, err := open(ctx)
			if err != nil {
				return "", err
			}
			defer rc.Close()
			b, _ := io.ReadAll(rc)
			gotInput = string(b)
			if err := lease.MarkDispatched(ctx); err != nil {
				return "", err
			}
			return "result-art-1", nil
		},
	}, jobs.RunnerConfig{PollInterval: 15 * time.Millisecond, LeaseTTL: 2 * time.Second, Concurrency: 2})

	rctx, stop := context.WithCancel(ctx)
	done := make(chan error, 1)
	go func() { done <- r.Run(rctx) }()

	waitStatus(t, svc, p, job.ID, jobs.StatusSucceeded, 3*time.Second)
	stop()
	<-done

	if gotInput != "payload" {
		t.Fatalf("handler input = %q, want payload", gotInput)
	}
	var resultRef string
	if err := db.SQL.QueryRowContext(ctx,
		`SELECT COALESCE(result_ref,'') FROM jobs WHERE tenant_id=$1 AND id=$2`,
		tenant, job.ID).Scan(&resultRef); err != nil || resultRef != "result-art-1" {
		t.Fatalf("result_ref = %q err=%v", resultRef, err)
	}
}

func TestRunnerRequeuesRetryableThenSucceeds(t *testing.T) {
	svc, _, tenant := runnerSetup(t)
	ctx := context.Background()
	p := principal(tenant, "a", "c")
	job := enqueue(t, svc, p, req(tenant, "k", "op.flaky", "h", 1))

	var calls atomic.Int32
	r := jobs.NewRunner(svc, stubResolver{}, map[string]jobs.HandlerFunc{
		"op.flaky": func(ctx context.Context, lease *jobs.Claim, _ jobs.InputFunc) (string, error) {
			if calls.Add(1) == 1 {
				return "", jobs.Retryable(errors.New("transient upstream"))
			}
			_ = lease.MarkDispatched(ctx)
			return "ok", nil
		},
	}, jobs.RunnerConfig{PollInterval: 10 * time.Millisecond, LeaseTTL: 2 * time.Second})

	rctx, stop := context.WithCancel(ctx)
	defer stop()
	go func() { _ = r.Run(rctx) }()

	waitStatus(t, svc, p, job.ID, jobs.StatusSucceeded, 3*time.Second)
	if calls.Load() < 2 {
		t.Fatalf("retryable failure must requeue; calls=%d", calls.Load())
	}
}

func TestRunnerFatalErrorFailsJob(t *testing.T) {
	svc, _, tenant := runnerSetup(t)
	ctx := context.Background()
	p := principal(tenant, "a", "c")
	job := enqueue(t, svc, p, req(tenant, "k", "op.bad", "h", 1))

	r := jobs.NewRunner(svc, stubResolver{}, map[string]jobs.HandlerFunc{
		"op.bad": func(ctx context.Context, lease *jobs.Claim, _ jobs.InputFunc) (string, error) {
			return "", errors.New("permanent")
		},
	}, jobs.RunnerConfig{PollInterval: 10 * time.Millisecond, LeaseTTL: 2 * time.Second})

	rctx, stop := context.WithCancel(ctx)
	defer stop()
	go func() { _ = r.Run(rctx) }()

	fin := waitStatus(t, svc, p, job.ID, jobs.StatusFailed, 3*time.Second)
	if !strings.Contains(fin.Error, "permanent") {
		t.Fatalf("error doc must carry cause, got %q", fin.Error)
	}
}

func TestRunnerReclaimsLostWorkerUnknown(t *testing.T) {
	svc, _, tenant := runnerSetup(t)
	ctx := context.Background()
	p := principal(tenant, "a", "c")

	// dispatched job: worker dies mid-attempt → reclaim marks unknown.
	dispatched := enqueue(t, svc, p, req(tenant, "d", "op.disp", "h", 1))
	dead, err := svc.Lease(ctx, "dead-worker", 60*time.Millisecond)
	if err != nil || dead == nil {
		t.Fatalf("lease: %v", err)
	}
	if err := dead.MarkDispatched(ctx); err != nil {
		t.Fatalf("dispatch: %v", err)
	}

	// undispatched job: worker dies before dispatch → reclaimed+retried.
	retry := enqueue(t, svc, p, req(tenant, "u", "op.retry", "h", 1))
	dead2, err := svc.Lease(ctx, "dead-worker-2", 60*time.Millisecond)
	if err != nil || dead2 == nil {
		t.Fatalf("lease 2: %v", err)
	}
	_ = dead2 // abandoned without dispatch or release

	time.Sleep(80 * time.Millisecond) // both leases expire

	var retried atomic.Int32
	r := jobs.NewRunner(svc, stubResolver{}, map[string]jobs.HandlerFunc{
		"op.retry": func(ctx context.Context, lease *jobs.Claim, _ jobs.InputFunc) (string, error) {
			retried.Add(1)
			return "ok", nil
		},
		"op.disp": func(ctx context.Context, lease *jobs.Claim, _ jobs.InputFunc) (string, error) {
			t.Errorf("dispatched job must never re-run")
			return "", nil
		},
	}, jobs.RunnerConfig{PollInterval: 10 * time.Millisecond, LeaseTTL: 2 * time.Second})

	rctx, stop := context.WithCancel(ctx)
	defer stop()
	go func() { _ = r.Run(rctx) }()

	waitStatus(t, svc, p, dispatched.ID, jobs.StatusUnknown, 3*time.Second)
	waitStatus(t, svc, p, retry.ID, jobs.StatusSucceeded, 3*time.Second)
	if retried.Load() != 1 {
		t.Fatalf("undispatched job must retry once, ran %d", retried.Load())
	}
}

func TestRunnerCancelPropagatesToHandler(t *testing.T) {
	svc, _, tenant := runnerSetup(t)
	ctx := context.Background()
	p := principal(tenant, "a", "c")
	job := enqueue(t, svc, p, req(tenant, "k", "op.slow", "h", 1))

	started := make(chan struct{})
	sawCancel := make(chan struct{})
	r := jobs.NewRunner(svc, stubResolver{}, map[string]jobs.HandlerFunc{
		"op.slow": func(ctx context.Context, lease *jobs.Claim, _ jobs.InputFunc) (string, error) {
			close(started)
			<-ctx.Done() // cancel must reach the running handler
			close(sawCancel)
			return "", ctx.Err()
		},
	}, jobs.RunnerConfig{PollInterval: 10 * time.Millisecond, LeaseTTL: 2 * time.Second,
		CancelPollEvery: 15 * time.Millisecond})

	rctx, stop := context.WithCancel(ctx)
	defer stop()
	go func() { _ = r.Run(rctx) }()

	select {
	case <-started:
	case <-time.After(2 * time.Second):
		t.Fatal("handler never started")
	}
	if err := svc.Cancel(ctx, p, job.ID); err != nil {
		t.Fatalf("cancel: %v", err)
	}
	select {
	case <-sawCancel:
	case <-time.After(2 * time.Second):
		t.Fatal("cancel never reached handler context")
	}
	fin := waitStatus(t, svc, p, job.ID, jobs.StatusCancelled, 3*time.Second)
	_ = fin
}

func TestRunnerConcurrencyBound(t *testing.T) {
	svc, _, tenant := runnerSetup(t)
	ctx := context.Background()
	p := principal(tenant, "a", "c")

	const total, capN = 6, 2
	for i := range total {
		enqueue(t, svc, p, req(tenant, fmt.Sprintf("c%d", i), "op.bound", "h", 1))
	}

	var cur, max atomic.Int32
	release := make(chan struct{})
	r := jobs.NewRunner(svc, stubResolver{}, map[string]jobs.HandlerFunc{
		"op.bound": func(ctx context.Context, lease *jobs.Claim, _ jobs.InputFunc) (string, error) {
			if n := cur.Add(1); n > max.Load() {
				max.Store(n)
			}
			defer cur.Add(-1)
			select {
			case <-release:
			case <-ctx.Done():
				return "", ctx.Err()
			}
			return "ok", nil
		},
	}, jobs.RunnerConfig{PollInterval: 5 * time.Millisecond, LeaseTTL: 2 * time.Second,
		Concurrency: capN})

	rctx, stop := context.WithCancel(ctx)
	defer stop()
	go func() { _ = r.Run(rctx) }()

	// Let handlers park, then release all.
	time.Sleep(150 * time.Millisecond)
	close(release)

	deadline := time.Now().Add(3 * time.Second)
	for time.Now().Before(deadline) {
		list, _ := svc.List(ctx, p, total)
		done := 0
		for _, j := range list {
			if j.Status == jobs.StatusSucceeded {
				done++
			}
		}
		if done == total {
			break
		}
		time.Sleep(15 * time.Millisecond)
	}
	if m := max.Load(); m > int32(capN) {
		t.Fatalf("concurrency bound violated: %d > %d", m, capN)
	}
}

func TestRunnerShutdownReleasesUndispatchedLease(t *testing.T) {
	svc, _, tenant := runnerSetup(t)
	ctx := context.Background()
	p := principal(tenant, "a", "c")
	job := enqueue(t, svc, p, req(tenant, "k", "op.block", "h", 1))

	block := make(chan struct{})
	started := make(chan struct{})
	r := jobs.NewRunner(svc, stubResolver{}, map[string]jobs.HandlerFunc{
		"op.block": func(ctx context.Context, lease *jobs.Claim, _ jobs.InputFunc) (string, error) {
			close(started)
			select {
			case <-block:
				return "ok", nil
			case <-ctx.Done():
				return "", ctx.Err() // undispatched → released on drain
			}
		},
	}, jobs.RunnerConfig{PollInterval: 10 * time.Millisecond, LeaseTTL: 2 * time.Second})

	rctx, stop := context.WithCancel(ctx)
	done := make(chan error, 1)
	go func() { done <- r.Run(rctx) }()
	<-started
	stop()
	select {
	case <-done:
	case <-time.After(2 * time.Second):
		t.Fatal("runner did not drain")
	}

	// Job was released back to queued — a fresh runner can complete it.
	got, err := svc.Get(ctx, p, job.ID)
	if err != nil {
		t.Fatalf("get: %v", err)
	}
	if got.Status != jobs.StatusQueued {
		t.Fatalf("undispatched shutdown must requeue, got %q", got.Status)
	}
	close(block)
}
