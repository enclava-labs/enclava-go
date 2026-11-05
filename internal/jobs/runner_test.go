package jobs_test

import (
	"context"
	"errors"
	"fmt"
	"io"
	"strings"
	"sync"
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

type deleteTrackingResolver struct {
	deleted chan artifacts.Ref
}

func (r deleteTrackingResolver) Read(context.Context, artifacts.Ref) (io.ReadCloser, error) {
	return io.NopCloser(strings.NewReader("")), nil
}

func (r deleteTrackingResolver) Delete(_ context.Context, ref artifacts.Ref) error {
	r.deleted <- ref
	return nil
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
			defer func() { _ = rc.Close() }()
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
	if err := <-done; err != nil {
		t.Fatalf("runner: %v", err)
	}

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
func TestRunnerDeletesResultAfterStaleCompletion(t *testing.T) {
	svc, _, tenant := runnerSetup(t)
	ctx := context.Background()
	p := principal(tenant, "a", "c")
	job := enqueue(t, svc, p, req(tenant, "stale-output", "op.stale", "h", 1))
	resolver := deleteTrackingResolver{deleted: make(chan artifacts.Ref, 1)}
	r := jobs.NewRunner(svc, resolver, map[string]jobs.HandlerFunc{
		"op.stale": func(ctx context.Context, _ *jobs.Claim, _ jobs.InputFunc) (string, error) {
			if err := svc.Delete(ctx, p, job.ID); err != nil {
				return "", err
			}
			return "orphan-result", nil
		},
	}, jobs.RunnerConfig{PollInterval: 10 * time.Millisecond, LeaseTTL: 2 * time.Second})
	rctx, stop := context.WithCancel(ctx)
	done := make(chan error, 1)
	go func() { done <- r.Run(rctx) }()
	select {
	case ref := <-resolver.deleted:
		if ref.TenantID != tenant || ref.ID != "orphan-result" {
			t.Fatalf("deleted result ref = %+v", ref)
		}
	case <-time.After(3 * time.Second):
		t.Fatal("stale completion result was not cleaned up")
	}
	stop()
	if err := <-done; err != nil {
		t.Fatalf("runner: %v", err)
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

func TestRunnerJobTimeoutFreesSlot(t *testing.T) {
	for _, dispatched := range []bool{false, true} {
		t.Run(fmt.Sprintf("dispatched=%t", dispatched), func(t *testing.T) {
			svc, _, tenant := runnerSetup(t)
			p := principal(tenant, "a", "c")
			// The undispatched deadline exercises the retry path: attempt 1
			// requeues (attempt+1 < maxAttempts), attempt 2 fails terminally.
			first := enqueue(t, svc, p, req(tenant, "timeout", "op.timeout", "h", 1))
			var calls atomic.Int32
			var once sync.Once
			started := make(chan struct{})
			canceled := make(chan error, 4)
			r := jobs.NewRunner(svc, stubResolver{}, map[string]jobs.HandlerFunc{
				"op.timeout": func(ctx context.Context, claim *jobs.Claim, _ jobs.InputFunc) (string, error) {
					calls.Add(1)
					if dispatched {
						if err := claim.MarkDispatched(ctx); err != nil {
							return "", err
						}
					}
					once.Do(func() { close(started) })
					<-ctx.Done()
					canceled <- ctx.Err()
					return "", ctx.Err()
				},
				"op.next": func(context.Context, *jobs.Claim, jobs.InputFunc) (string, error) {
					return "ok", nil
				},
			}, jobs.RunnerConfig{Concurrency: 1, PollInterval: 5 * time.Millisecond, LeaseTTL: 2 * time.Second, JobTimeout: 100 * time.Millisecond})
			ctx, stop := context.WithCancel(context.Background())
			done := make(chan error, 1)
			go func() { done <- r.Run(ctx) }()
			defer func() {
				stop()
				if err := <-done; err != nil {
					t.Errorf("runner returned %v", err)
				}
			}()
			select {
			case <-started:
			case <-time.After(3 * time.Second):
				t.Fatal("handler did not start")
			}
			second := enqueue(t, svc, p, req(tenant, "next", "op.next", "h", 1))
			select {
			case err := <-canceled:
				if !errors.Is(err, context.DeadlineExceeded) {
					t.Fatalf("handler cancellation = %v", err)
				}
			case <-time.After(3 * time.Second):
				t.Fatal("handler was not canceled")
			}
			want := jobs.StatusFailed
			wantCalls := int32(2) // retry, then terminal failure
			if dispatched {
				want = jobs.StatusUnknown
				wantCalls = 1
			}
			waitStatus(t, svc, p, first.ID, want, 3*time.Second)
			if got := calls.Load(); got != wantCalls {
				t.Fatalf("handler calls = %d, want %d", got, wantCalls)
			}
			// The freed slot let a Concurrency=1 runner claim the next job.
			waitStatus(t, svc, p, second.ID, jobs.StatusSucceeded, 3*time.Second)
		})
	}
}

func TestJobCountsByOperationStatus(t *testing.T) {
	svc, db, tenant := runnerSetup(t)
	p := principal(tenant, "a", "c")
	enqueue(t, svc, p, req(tenant, "one", "op.count", "h", 1))
	enqueue(t, svc, p, req(tenant, "two", "op.count", "h", 1))
	counts, err := postgres.NewJobsRepo(db).CountByOperationStatus(context.Background())
	if err != nil {
		t.Fatal(err)
	}
	if len(counts) != 1 || counts[0].Operation != "op.count" || counts[0].Status != jobs.StatusQueued || counts[0].Count != 2 {
		t.Fatalf("unexpected job counts: %+v", counts)
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
	}, jobs.RunnerConfig{PollInterval: 10 * time.Millisecond, LeaseTTL: 2 * time.Second,
		DrainTimeout: 50 * time.Millisecond})

	rctx, stop := context.WithCancel(ctx)
	done := make(chan error, 1)
	go func() { done <- r.Run(rctx) }()
	<-started
	stop()
	select {
	case err := <-done:
		if err != nil {
			t.Fatalf("runner: %v", err)
		}
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

func TestRunnerShutdownInterruptedJobRecordsReasonAndReservation(t *testing.T) {
	svc, db, tenant := runnerSetup(t)
	ctx := context.Background()
	p := principal(tenant, "a", "c")
	job := enqueue(t, svc, p, req(tenant, "k", "op.slow", "h", 1))

	attemptID := make(chan string, 1)
	r := jobs.NewRunner(svc, stubResolver{}, map[string]jobs.HandlerFunc{
		"op.slow": func(ctx context.Context, lease *jobs.Claim, _ jobs.InputFunc) (string, error) {
			if err := lease.MarkDispatched(ctx); err != nil {
				return "", err
			}
			attemptID <- lease.AttemptID()
			<-ctx.Done() // shutdown cancels the in-flight handler
			return "", ctx.Err()
		},
	}, jobs.RunnerConfig{PollInterval: 10 * time.Millisecond, LeaseTTL: 2 * time.Second,
		DrainTimeout: 50 * time.Millisecond})

	rctx, stop := context.WithCancel(ctx)
	done := make(chan error, 1)
	go func() { done <- r.Run(rctx) }()

	var att string
	select {
	case att = <-attemptID:
	case <-time.After(2 * time.Second):
		t.Fatal("handler never dispatched")
	}
	// The attempt's billing hold existed when the worker was interrupted —
	// the job row must carry it for reconciliation and retry warnings.
	if _, err := db.SQL.ExecContext(ctx, `
		INSERT INTO reservations (tenant_id, id, job_id, attempt_id, subject_type,
		                          subject_id, price_snapshot, reserved_units,
		                          limit_amount_micros, currency, state)
		VALUES ($1, 'res_interrupted', $2, $3, 'model', 'm1',
		        '{}'::jsonb, '{}'::jsonb, 0, 'USD', 'held')`,
		tenant, job.ID, att); err != nil {
		t.Fatalf("seed reservation: %v", err)
	}
	stop()
	select {
	case err := <-done:
		if err != nil {
			t.Fatalf("runner: %v", err)
		}
	case <-time.After(2 * time.Second):
		t.Fatal("runner did not drain")
	}

	fin := waitStatus(t, svc, p, job.ID, jobs.StatusUnknown, 3*time.Second)
	if fin.OutcomeReason != jobs.ReasonInterrupted {
		t.Fatalf("outcome reason = %q, want %q", fin.OutcomeReason, jobs.ReasonInterrupted)
	}
	if fin.OutcomeReservationID != "res_interrupted" {
		t.Fatalf("outcome reservation = %q, want res_interrupted", fin.OutcomeReservationID)
	}
	if !strings.Contains(fin.Error, "usage may have been billed") {
		t.Fatalf("interrupted error message = %q", fin.Error)
	}
	if fin.UpdatedAt.Before(fin.CreatedAt) {
		t.Fatalf("updated_at %v precedes created_at %v", fin.UpdatedAt, fin.CreatedAt)
	}
}

func TestRunnerShutdownDrainsInFlightJobs(t *testing.T) {
	svc, _, tenant := runnerSetup(t)
	ctx := context.Background()
	p := principal(tenant, "a", "c")
	job := enqueue(t, svc, p, req(tenant, "k", "op.drain", "h", 1))

	release := make(chan struct{})
	started := make(chan struct{})
	handlerErr := make(chan error, 1)
	r := jobs.NewRunner(svc, stubResolver{}, map[string]jobs.HandlerFunc{
		"op.drain": func(ctx context.Context, lease *jobs.Claim, _ jobs.InputFunc) (string, error) {
			if err := lease.MarkDispatched(ctx); err != nil {
				return "", err
			}
			close(started)
			<-release // park until the test releases the drain window
			handlerErr <- ctx.Err()
			return "drained-result", nil
		},
	}, jobs.RunnerConfig{PollInterval: 10 * time.Millisecond, LeaseTTL: 2 * time.Second,
		DrainTimeout: 2 * time.Second})

	rctx, stop := context.WithCancel(ctx)
	done := make(chan error, 1)
	go func() { done <- r.Run(rctx) }()

	select {
	case <-started:
	case <-time.After(2 * time.Second):
		t.Fatal("handler never dispatched")
	}
	stop()
	select {
	case err := <-done:
		t.Fatalf("runner returned before in-flight work finished (err=%v)", err)
	case <-time.After(50 * time.Millisecond):
	}
	close(release)
	select {
	case err := <-done:
		if err != nil {
			t.Fatalf("runner: %v", err)
		}
	case <-time.After(2 * time.Second):
		t.Fatal("runner did not drain")
	}
	if err := <-handlerErr; err != nil {
		t.Fatalf("shutdown cancelled the in-flight handler: %v", err)
	}

	// The dispatched job completed inside the drain window: succeeded, not
	// unknown (interrupted) or released back to queued.
	got, err := svc.Get(ctx, p, job.ID)
	if err != nil {
		t.Fatalf("get: %v", err)
	}
	if got.Status != jobs.StatusSucceeded {
		t.Fatalf("drained job status = %q, want %q", got.Status, jobs.StatusSucceeded)
	}
}

func TestRunnerPreservesHandlerTimeoutOutcome(t *testing.T) {
	svc, _, tenant := runnerSetup(t)
	p := principal(tenant, "a", "c")
	job := enqueue(t, svc, p, req(tenant, "timeout-outcome", "op.timeout-outcome", "h", 1))
	r := jobs.NewRunner(svc, stubResolver{}, map[string]jobs.HandlerFunc{
		"op.timeout-outcome": func(ctx context.Context, claim *jobs.Claim, _ jobs.InputFunc) (string, error) {
			if err := claim.MarkDispatched(ctx); err != nil {
				return "", err
			}
			<-ctx.Done()
			return "", errors.New("model call timed out")
		},
	}, jobs.RunnerConfig{PollInterval: 10 * time.Millisecond, LeaseTTL: time.Second, JobTimeout: 50 * time.Millisecond})
	ctx, stop := context.WithCancel(context.Background())
	done := make(chan error, 1)
	go func() { done <- r.Run(ctx) }()
	fin := waitStatus(t, svc, p, job.ID, jobs.StatusFailed, 3*time.Second)
	stop()
	if err := <-done; err != nil {
		t.Fatalf("runner: %v", err)
	}
	if fin.Error != "model call timed out" {
		t.Fatalf("error = %q, want handler timeout outcome", fin.Error)
	}
}
