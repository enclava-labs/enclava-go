package jobs

import (
	"context"
	"errors"
	"fmt"
	"io"
	"os"
	"sync"
	"time"

	"enclava-go/internal/artifacts"
)

// InputFunc resolves the claimed job's immutable input_ref into a reader.
// The runner binds it per claim so handlers never compose storage paths.
type InputFunc func(ctx context.Context) (io.ReadCloser, error)

// HandlerFunc executes one claimed attempt. Call lease.MarkDispatched
// BEFORE any upstream side effect — a worker that dies afterwards resolves
// to unknown instead of silently re-running. Return the result reference
// (opaque artifact id, "" allowed) and the terminal error classification
// via Retryable(err) vs a plain fatal error.
type HandlerFunc func(ctx context.Context, lease *Claim, input InputFunc) (resultRef string, err error)

// InputResolver resolves a job's stored input_ref into a byte stream.
// artifacts.Store satisfies it directly: input_ref stores the artifact ID.
type InputResolver interface {
	Read(ctx context.Context, ref artifacts.Ref) (io.ReadCloser, error)
}

var _ InputResolver = (*artifacts.Store)(nil)

// RunnerConfig tunes the worker loop. Zero values take the defaults
// documented per field.
type RunnerConfig struct {
	WorkerID        string        // process-unique; "worker-<pid>-<rand>" when empty
	Operations      []string      // claimable operations; empty = claim everything
	Concurrency     int           // max simultaneous handlers; default 4
	PollInterval    time.Duration // idle poll delay; default 500ms
	LeaseTTL        time.Duration // lease duration per claim; default 30s
	HeartbeatEvery  time.Duration // heartbeat cadence; default LeaseTTL/3
	CancelPollEvery time.Duration // owner-cancel probe cadence; default HeartbeatEvery
}

// Runner leases jobs and executes registered handlers under the fence
// protocol (JOB-02): heartbeats keep the lease, a lost lease makes every
// subsequent publish fail with ErrStaleFence, and shutdown releases the
// leases it still owns so undispatched work is requeued — dispatched work
// is left to expire into unknown.
type Runner struct {
	svc      *Service
	resolver InputResolver
	handlers map[string]HandlerFunc
	cfg      RunnerConfig
}

func NewRunner(svc *Service, resolver InputResolver, handlers map[string]HandlerFunc, cfg RunnerConfig) *Runner {
	if cfg.Concurrency <= 0 {
		cfg.Concurrency = 4
	}
	if cfg.PollInterval <= 0 {
		cfg.PollInterval = 500 * time.Millisecond
	}
	if cfg.LeaseTTL <= 0 {
		cfg.LeaseTTL = 30 * time.Second
	}
	if cfg.HeartbeatEvery <= 0 {
		cfg.HeartbeatEvery = cfg.LeaseTTL / 3
	}
	if cfg.CancelPollEvery <= 0 {
		cfg.CancelPollEvery = cfg.HeartbeatEvery
	}
	if cfg.WorkerID == "" {
		cfg.WorkerID = fmt.Sprintf("worker-%d-%s", os.Getpid(), newID("w")[2:])
	}
	return &Runner{svc: svc, resolver: resolver, handlers: handlers, cfg: cfg}
}

// Run polls and executes until ctx is cancelled, then drains: in-flight
// handlers observe cancellation, undispatched claims are released back to
// queued, and dispatched ones keep their leases (expiry resolves them to
// unknown). Returns nil on graceful shutdown.
func (r *Runner) Run(ctx context.Context) error {
	sem := make(chan struct{}, r.cfg.Concurrency)
	var wg sync.WaitGroup
	for {
		select {
		case <-ctx.Done():
			wg.Wait()
			return nil
		default:
		}
		claim, err := r.svc.Lease(ctx, r.cfg.WorkerID, r.cfg.LeaseTTL)
		if err != nil {
			if ctx.Err() != nil {
				wg.Wait()
				return nil
			}
			if !sleep(ctx, r.cfg.PollInterval) {
				wg.Wait()
				return nil
			}
			continue
		}
		if claim == nil {
			if !sleep(ctx, r.cfg.PollInterval) {
				wg.Wait()
				return nil
			}
			continue
		}
		select {
		case sem <- struct{}{}:
		case <-ctx.Done():
			// Never started: release so the job is requeued promptly.
			_ = claim.Release(context.Background())
			wg.Wait()
			return nil
		}
		wg.Add(1)
		go func() {
			defer wg.Done()
			defer func() { <-sem }()
			r.execute(ctx, claim)
		}()
	}
}

// execute runs one claim end-to-end: heartbeat/cancel watcher plus the
// handler, then translates the handler outcome into the fenced transition.
func (r *Runner) execute(ctx context.Context, claim *Claim) {
	handler, ok := r.handlers[claim.Job().Operation]
	if !ok {
		_ = claim.Fail(ctx, fmt.Errorf("no handler registered for operation %q", claim.Job().Operation), FailureFatal)
		return
	}
	job := claim.Job()

	jobCtx, cancel := context.WithCancel(ctx)
	defer cancel()

	// Watchdog: heartbeat keeps the lease alive; a heartbeat failure or an
	// owner cancel-request cancels the handler's context.
	stop := make(chan struct{})
	go func() {
		hb := time.NewTicker(r.cfg.HeartbeatEvery)
		defer hb.Stop()
		cb := time.NewTicker(r.cfg.CancelPollEvery)
		defer cb.Stop()
		for {
			select {
			case <-stop:
				return
			case <-hb.C:
				if err := claim.Heartbeat(ctx, r.cfg.LeaseTTL); err != nil {
					cancel()
					return
				}
			case <-cb.C:
				wanted, err := claim.CancelRequested(ctx)
				if err != nil {
					cancel() // fenced read failed — lease is lost
					return
				}
				if wanted {
					cancel()
					return
				}
			}
		}
	}()
	defer close(stop)

	open := func(ctx context.Context) (io.ReadCloser, error) {
		if r.resolver == nil || job.InputRef == "" {
			return nil, fmt.Errorf("jobs: no input resolver or empty input_ref")
		}
		return r.resolver.Read(ctx, artifacts.Ref{TenantID: job.TenantID, ID: job.InputRef})
	}

	resultRef, err := handler(jobCtx, claim, open)
	r.translate(ctx, claim, resultRef, err)
}

// translate maps the handler's return onto the terminal transition. A lost
// fence (ErrStaleFence from the transition itself) means another worker
// already owns the job — no further write is attempted.
func (r *Runner) translate(ctx context.Context, claim *Claim, resultRef string, err error) {
	// Terminal writes must land even during shutdown: detach from the
	// runner's cancellation so release/unknown/fail still reach the repo.
	ctx = context.WithoutCancel(ctx)
	switch {
	case err == nil:
		_, _ = claim.Complete(ctx, resultRef)
	case errors.Is(err, context.Canceled), errors.Is(err, context.DeadlineExceeded):
		if wanted, werr := claim.CancelRequested(ctx); werr == nil && wanted {
			// Owner cancelled: flip to cancelled via the fenced write.
			_, _ = claim.Complete(ctx, resultRef)
			return
		}
		if claim.Dispatched() {
			_ = claim.MarkUnknown(ctx, "worker context cancelled after dispatch")
			return
		}
		_ = claim.Release(ctx)
	default:
		_ = claim.Fail(ctx, err, FailureClassFor(err))
	}
}

func sleep(ctx context.Context, d time.Duration) bool {
	t := time.NewTimer(d)
	defer t.Stop()
	select {
	case <-ctx.Done():
		return false
	case <-t.C:
		return true
	}
}
