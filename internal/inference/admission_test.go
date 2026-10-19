// admission_test.go exercises the executor's bounded admission lifecycle
// (CON-01): max active work, bounded queues, per-tenant sublimits, early
// header return, abandoned bodies, disconnect, nested fan-out and
// management-call progress — all against the real executor over real
// PostgreSQL accounting.
package inference_test

import (
	"context"
	"enclava-go/internal/billing"
	"enclava-go/internal/inference"
	"enclava-go/internal/jobs"
	"enclava-go/internal/store/postgres"
	"errors"
	"io"
	"sync"
	"testing"
	"time"
)

// heldStream blocks reads until release is closed; Close is idempotent and
// unblocks any pending reader so test teardown cannot hang.
type heldStream struct {
	release chan struct{}
	once    sync.Once
}

func newHeldStream() *heldStream { return &heldStream{release: make(chan struct{})} }

func (s *heldStream) Read(p []byte) (int, error) {
	<-s.release
	return 0, io.EOF
}

func (s *heldStream) Close() error {
	s.once.Do(func() { close(s.release) })
	return nil
}

func (s *heldStream) unblock() { _ = s.Close() }

// admissionFx builds a fixture whose executor has an explicit admission gate.
func admissionFx(t *testing.T, cfg inference.AdmissionConfig) *fx {
	t.Helper()
	f := newFx(t)
	f.admission = inference.NewAdmission(cfg)
	f.build(t)
	f.mkEndpoint(t)
	f.flatPrices(t)
	return f
}

func (f *fx) streamRequest() inference.Request {
	req := f.chatRequest(`{"messages":[{"role":"user","content":"hi"}]}`)
	req.Stream = true
	return req
}

// activeStats polls until the pool reaches the expected counts or the
// deadline passes — admission state is synchronous so this is a wait, never
// a sleep-based assertion.
func waitActive(t *testing.T, adm *inference.Admission, cls inference.AdmissionClass, want int) {
	t.Helper()
	deadline := time.Now().Add(5 * time.Second)
	for time.Now().Before(deadline) {
		active, _, _ := adm.Stats(cls)
		if active == want {
			return
		}
		time.Sleep(5 * time.Millisecond)
	}
	active, queued, _ := adm.Stats(cls)
	t.Fatalf("pool never reached active=%d (active=%d queued=%d)", want, active, queued)
}

func TestAdmissionCapsActiveUntilStreamCloses(t *testing.T) {
	f := admissionFx(t, inference.AdmissionConfig{
		Execute: inference.ClassLimit{MaxActive: 1, MaxQueued: 0},
		Manage:  inference.ClassLimit{MaxActive: 1, MaxQueued: 0},
		FanOut:  inference.ClassLimit{MaxActive: 1, MaxQueued: 0},
	})
	held := newHeldStream()
	f.upstream.stream = held

	res, err := f.svc.Execute(f.ctx, f.principal, f.streamRequest())
	if err != nil {
		t.Fatalf("stream execute: %v", err)
	}
	// Headers returned, body open: the slot stays held.
	waitActive(t, f.admission, inference.AdmissionExecute, 1)

	// A second request with no queue depth rejects explicitly.
	_, err = f.svc.Execute(f.ctx, f.principal, f.streamRequest())
	if !errors.Is(err, inference.ErrOverloaded) {
		t.Fatalf("expected ErrOverloaded, got %v", err)
	}
	if n := f.reservationCount(t); n != 1 {
		t.Fatalf("rejected request must not reserve: %d reservations", n)
	}

	if err := res.Body.Close(); err != nil {
		t.Fatal(err)
	}
	waitActive(t, f.admission, inference.AdmissionExecute, 0)
}

func TestAdmissionQueuedCallerProceedsOnRelease(t *testing.T) {
	f := admissionFx(t, inference.AdmissionConfig{
		Execute: inference.ClassLimit{MaxActive: 1, MaxQueued: 4},
		Manage:  inference.ClassLimit{MaxActive: 1, MaxQueued: 0},
		FanOut:  inference.ClassLimit{MaxActive: 1, MaxQueued: 0},
	})
	held := newHeldStream()
	f.upstream.stream = held

	first, err := f.svc.Execute(f.ctx, f.principal, f.streamRequest())
	if err != nil {
		t.Fatalf("first stream: %v", err)
	}
	waitActive(t, f.admission, inference.AdmissionExecute, 1)

	type outcome struct {
		res *inference.Result
		err error
	}
	done := make(chan outcome, 1)
	go func() {
		res, err := f.svc.Execute(f.ctx, f.principal, f.streamRequest())
		done <- outcome{res, err}
	}()

	// The queued caller waits; no overload, no reservation yet beyond the
	// first (queued attempts never hold a budget hold).
	waitDeadline := time.Now().Add(5 * time.Second)
	for {
		_, queued, _ := f.admission.Stats(inference.AdmissionExecute)
		if queued == 1 {
			break
		}
		if time.Now().After(waitDeadline) {
			t.Fatal("second request never queued")
		}
		time.Sleep(5 * time.Millisecond)
	}
	if n := f.reservationCount(t); n != 1 {
		t.Fatalf("queued request must not reserve early: %d reservations", n)
	}

	if err := first.Body.Close(); err != nil {
		t.Fatal(err)
	}
	out := <-done
	if out.err != nil {
		t.Fatalf("queued request: %v", out.err)
	}
	if err := out.res.Body.Close(); err != nil {
		t.Fatal(err)
	}
	waitActive(t, f.admission, inference.AdmissionExecute, 0)
}

func TestAdmissionQueuedCancelLeavesNoLeaks(t *testing.T) {
	f := admissionFx(t, inference.AdmissionConfig{
		Execute: inference.ClassLimit{MaxActive: 1, MaxQueued: 4},
		Manage:  inference.ClassLimit{MaxActive: 1, MaxQueued: 0},
		FanOut:  inference.ClassLimit{MaxActive: 1, MaxQueued: 0},
	})
	held := newHeldStream()
	f.upstream.stream = held

	first, err := f.svc.Execute(f.ctx, f.principal, f.streamRequest())
	if err != nil {
		t.Fatalf("first stream: %v", err)
	}
	waitActive(t, f.admission, inference.AdmissionExecute, 1)

	cctx, cancel := context.WithCancel(f.ctx)
	errCh := make(chan error, 1)
	go func() {
		_, err := f.svc.Execute(cctx, f.principal, f.streamRequest())
		errCh <- err
	}()
	waitDeadline := time.Now().Add(5 * time.Second)
	for {
		_, queued, _ := f.admission.Stats(inference.AdmissionExecute)
		if queued == 1 {
			break
		}
		if time.Now().After(waitDeadline) {
			t.Fatal("queued caller never appeared")
		}
		time.Sleep(5 * time.Millisecond)
	}

	cancel()
	if err := <-errCh; !errors.Is(err, context.Canceled) {
		t.Fatalf("queued cancel should surface context.Canceled, got %v", err)
	}
	waitDeadline = time.Now().Add(5 * time.Second)
	for {
		_, queued, _ := f.admission.Stats(inference.AdmissionExecute)
		if queued == 0 {
			break
		}
		if time.Now().After(waitDeadline) {
			t.Fatal("cancelled waiter never left the queue")
		}
		time.Sleep(5 * time.Millisecond)
	}
	if n := f.reservationCount(t); n != 1 {
		t.Fatalf("cancelled queued request must not reserve: %d", n)
	}

	// Free the first slot, then a fresh request proceeds — no leaked
	// permits, no stuck queue entries.
	if err := first.Body.Close(); err != nil {
		t.Fatal(err)
	}
	second, err := f.svc.Execute(f.ctx, f.principal, f.streamRequest())
	if err != nil {
		t.Fatalf("post-cancel request should proceed, got %v", err)
	}
	if err := second.Body.Close(); err != nil {
		t.Fatal(err)
	}
	waitActive(t, f.admission, inference.AdmissionExecute, 0)
}

func TestAdmissionAbandonedBodyHoldsThenCloseReleases(t *testing.T) {
	f := admissionFx(t, inference.AdmissionConfig{
		Execute: inference.ClassLimit{MaxActive: 1, MaxQueued: 1},
		Manage:  inference.ClassLimit{MaxActive: 1, MaxQueued: 0},
		FanOut:  inference.ClassLimit{MaxActive: 1, MaxQueued: 0},
	})
	held := newHeldStream()
	f.upstream.stream = held

	res, err := f.svc.Execute(f.ctx, f.principal, f.streamRequest())
	if err != nil {
		t.Fatalf("stream execute: %v", err)
	}
	waitActive(t, f.admission, inference.AdmissionExecute, 1)

	// Caller stops reading: the permit is still held. Closing the body
	// (which is what the HTTP layer does on disconnect) frees it.
	if err := res.Body.Close(); err != nil {
		t.Fatal(err)
	}
	waitActive(t, f.admission, inference.AdmissionExecute, 0)
	if n := f.reservationCount(t); n != 1 {
		t.Fatalf("reservation count: %d", n)
	}
	// Early close without terminal usage retains liability, not zero-cost.
	if st := f.reservationState(t, res.ReservationID); st != billing.ReservationUnknown {
		t.Fatalf("abandoned stream must retain liability, state=%s", st)
	}
}

func TestAdmissionManageProgressesWhileExecuteSaturated(t *testing.T) {
	f := admissionFx(t, inference.AdmissionConfig{
		Execute: inference.ClassLimit{MaxActive: 1, MaxQueued: 0},
		Manage:  inference.ClassLimit{MaxActive: 2, MaxQueued: 0},
		FanOut:  inference.ClassLimit{MaxActive: 1, MaxQueued: 0},
	})
	held := newHeldStream()
	f.upstream.stream = held
	f.upstream.resp = []byte(`{"id":"resp_up","usage":{"input_tokens":1,"output_tokens":1}}`)

	res, err := f.svc.Execute(f.ctx, f.principal, f.streamRequest())
	if err != nil {
		t.Fatalf("saturating stream: %v", err)
	}
	defer func() { _ = res.Body.Close() }()
	waitActive(t, f.admission, inference.AdmissionExecute, 1)

	// Control operations draw from their own pool: retrieve/cancel work
	// while the execute pool is saturated and its queue rejects.
	mres, err := f.svc.Manage(f.ctx, f.principal, inference.ManageRequest{
		EndpointID: "ep-1",
		Method:     "GET",
		Path:       "/responses/resp_up",
	})
	if err != nil {
		t.Fatalf("manage must progress under saturation: %v", err)
	}
	raw, err := io.ReadAll(mres.Body)
	if err != nil {
		t.Fatal(err)
	}
	_ = mres.Body.Close()
	if len(raw) == 0 {
		t.Fatal("empty manage body")
	}
	if err := res.Body.Close(); err != nil {
		t.Fatal(err)
	}
	waitActive(t, f.admission, inference.AdmissionExecute, 0)
}

func TestAdmissionFanOutPoolCannotDeadlockExecute(t *testing.T) {
	f := admissionFx(t, inference.AdmissionConfig{
		Execute: inference.ClassLimit{MaxActive: 1, MaxQueued: 0},
		Manage:  inference.ClassLimit{MaxActive: 1, MaxQueued: 0},
		FanOut:  inference.ClassLimit{MaxActive: 2, MaxQueued: 2},
	})
	held := newHeldStream()
	f.upstream.stream = held

	// The JobID must reference a real durable job (reservations.job_id FK).
	job, _, err := jobs.NewService(postgres.NewJobsRepo(f.db), 0).Enqueue(f.ctx, f.principal, jobs.EnqueueRequest{
		Operation:      "test.fanout",
		IdempotencyKey: "fanout-1",
	})
	if err != nil {
		t.Fatalf("enqueue fan-out job: %v", err)
	}

	// A job-originated call occupies the fan-out pool, not the request pool.
	fanReq := f.streamRequest()
	fanReq.JobID = job.ID
	fan, err := f.svc.Execute(f.ctx, f.principal, fanReq)
	if err != nil {
		t.Fatalf("fan-out execute: %v", err)
	}
	waitActive(t, f.admission, inference.AdmissionFanOut, 1)

	// The request pool is untouched by fan-out occupancy — and vice versa.
	out, err := f.svc.Execute(f.ctx, f.principal, f.streamRequest())
	if err != nil {
		t.Fatalf("request execute must not deadlock behind fan-out: %v", err)
	}
	waitActive(t, f.admission, inference.AdmissionExecute, 1)

	if err := fan.Body.Close(); err != nil {
		t.Fatal(err)
	}
	if err := out.Body.Close(); err != nil {
		t.Fatal(err)
	}
	waitActive(t, f.admission, inference.AdmissionFanOut, 0)
	waitActive(t, f.admission, inference.AdmissionExecute, 0)
}

func TestAdmissionPerTenantSublimit(t *testing.T) {
	f := admissionFx(t, inference.AdmissionConfig{
		Execute: inference.ClassLimit{MaxActive: 4, MaxQueued: 4, PerTenant: 1},
		Manage:  inference.ClassLimit{MaxActive: 1, MaxQueued: 0},
		FanOut:  inference.ClassLimit{MaxActive: 1, MaxQueued: 0},
	})
	held := newHeldStream()
	f.upstream.stream = held

	res, err := f.svc.Execute(f.ctx, f.principal, f.streamRequest())
	if err != nil {
		t.Fatalf("first stream: %v", err)
	}
	defer func() { _ = res.Body.Close() }()
	waitActive(t, f.admission, inference.AdmissionExecute, 1)

	// The same tenant cannot queue a second request under PerTenant=1
	// even though the pool has room — one slow actor cannot hog the line.
	_, err = f.svc.Execute(f.ctx, f.principal, f.streamRequest())
	if !errors.Is(err, inference.ErrOverloaded) {
		t.Fatalf("per-tenant sublimit should reject, got %v", err)
	}
}
