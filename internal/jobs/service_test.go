package jobs_test

import (
	"context"
	"encoding/json"
	"errors"
	"fmt"
	"sync"
	"testing"
	"time"

	"enclava-go/internal/auth"
	"enclava-go/internal/jobs"
	"enclava-go/internal/store/postgres"
	"enclava-go/internal/testdb"
)

const testTTL = 250 * time.Millisecond

func setup(t *testing.T) (*jobs.Service, *postgres.DB, string) {
	t.Helper()
	db := testdb.New(t)
	tenant := testdb.SeedTenant(t, db, "t-"+fmt.Sprint(time.Now().UnixNano()))
	return jobs.NewService(postgres.NewJobsRepo(db), 10*time.Millisecond), db, tenant
}

func principal(tenant, actor, cred string) auth.Principal {
	return auth.Principal{
		TenantID:            tenant,
		ActorID:             actor,
		ActorKind:           auth.ActorKindUser,
		CredentialID:        cred,
		CredentialKind:      auth.CredentialKindAPIKey,
		BillingSubjectID:    "bill-1",
		BillingSubjectKind:  auth.BillingSubjectAPIKey,
		AllowedModelsAll:    true,
		AllowedEndpointsAll: true,
	}
}

func req(tenant, key, op, hash string, rev int64) jobs.EnqueueRequest {
	return jobs.EnqueueRequest{
		TenantID:       tenant,
		Operation:      op,
		IdempotencyKey: key,
		Subject:        jobs.Subject{ConnectionID: "conn-1", ObjectID: "obj-1", ObservedRevision: rev},
		InputRef:       "art-" + hash,
		InputHash:      hash,
		Policy:         map[string]any{"model": "m1"},
	}
}

func enqueue(t *testing.T, svc *jobs.Service, p auth.Principal, r jobs.EnqueueRequest) jobs.Job {
	t.Helper()
	job, created, err := svc.Enqueue(context.Background(), p, r)
	if err != nil {
		t.Fatalf("enqueue: %v", err)
	}
	if !created {
		t.Fatalf("expected new job, got replay of %s", job.ID)
	}
	return job
}

// --- Idempotency (JOB-01) -------------------------------------------------

func TestEnqueueExactReplayReturnsSameJob(t *testing.T) {
	svc, _, tenant := setup(t)
	ctx := context.Background()
	p := principal(tenant, "actor-1", "cred-1")

	job := enqueue(t, svc, p, req(tenant, "k1", "extract.process", "h1", 1))

	again, created, err := svc.Enqueue(ctx, p, req(tenant, "k1", "extract.process", "h1", 1))
	if err != nil {
		t.Fatalf("replay: %v", err)
	}
	if created || again.ID != job.ID {
		t.Fatalf("replay must return the same job %s, got %s (created=%v)", job.ID, again.ID, created)
	}
	// Key-order permuted policy is the same canonical policy.
	other := req(tenant, "k1", "extract.process", "h1", 1)
	other.Policy = map[string]any{"model": "m1"}
	if _, created, err := svc.Enqueue(ctx, p, other); err != nil || created {
		t.Fatalf("canonical-policy replay: created=%v err=%v", created, err)
	}
}

func TestEnqueueDifferentOwnerSameKeyConflicts(t *testing.T) {
	svc, _, tenant := setup(t)
	ctx := context.Background()

	enqueue(t, svc, principal(tenant, "actor-1", "cred-1"), req(tenant, "k1", "op", "h1", 1))

	// Same tenant, same key+input, DIFFERENT owner: conflict — the stored
	// job's authority is never lent to another actor.
	_, _, err := svc.Enqueue(ctx, principal(tenant, "actor-2", "cred-2"), req(tenant, "k1", "op", "h1", 1))
	if !errors.Is(err, jobs.ErrIdempotencyConflict) {
		t.Fatalf("expected ErrIdempotencyConflict, got %v", err)
	}
}

func TestEnqueueNarrowedPolicyConflicts(t *testing.T) {
	svc, _, tenant := setup(t)
	ctx := context.Background()
	p := principal(tenant, "actor-1", "cred-1")

	enqueue(t, svc, p, req(tenant, "k1", "op", "h1", 1))

	r := req(tenant, "k1", "op", "h1", 1)
	r.Policy = map[string]any{"model": "m1", "max_cost": 10} // different policy
	if _, _, err := svc.Enqueue(ctx, p, r); !errors.Is(err, jobs.ErrIdempotencyConflict) {
		t.Fatalf("policy change must conflict, got %v", err)
	}
	r2 := req(tenant, "k1", "op", "h1", 1)
	r2.InputHash = "h2"
	if _, _, err := svc.Enqueue(ctx, p, r2); !errors.Is(err, jobs.ErrIdempotencyConflict) {
		t.Fatalf("input change must conflict, got %v", err)
	}
}

func TestABAObservedRevisionsAreDistinctJobs(t *testing.T) {
	svc, _, tenant := setup(t)
	p := principal(tenant, "a", "c")

	j1 := enqueue(t, svc, p, req(tenant, "rev-1", "sync", "h", 1))
	j2 := enqueue(t, svc, p, req(tenant, "rev-2", "sync", "h", 2))
	j3 := enqueue(t, svc, p, req(tenant, "rev-3", "sync", "h", 1)) // A again: new work
	if j1.ID == j2.ID || j2.ID == j3.ID || j1.ID == j3.ID {
		t.Fatalf("A→B→A must create three distinct jobs: %s %s %s", j1.ID, j2.ID, j3.ID)
	}
	// And the same revision under the SAME key still replays.
	again, created, err := svc.Enqueue(context.Background(), p, req(tenant, "rev-1", "sync", "h", 1))
	if err != nil || created || again.ID != j1.ID {
		t.Fatalf("same-key replay: id=%s created=%v err=%v", again.ID, created, err)
	}
}

func TestEnqueueFreezesPrincipalBeforeAck(t *testing.T) {
	svc, db, tenant := setup(t)
	p := principal(tenant, "actor-9", "cred-9")
	job := enqueue(t, svc, p, req(tenant, "k", "op", "h", 1))

	var snap []byte
	if err := db.SQL.QueryRowContext(context.Background(),
		`SELECT principal FROM jobs WHERE tenant_id = $1 AND id = $2`,
		tenant, job.ID).Scan(&snap); err != nil {
		t.Fatalf("read frozen principal: %v", err)
	}
	var frozen auth.Principal
	if err := json.Unmarshal(snap, &frozen); err != nil {
		t.Fatalf("frozen principal not JSON: %v", err)
	}
	if frozen.ActorID != "actor-9" || frozen.CredentialID != "cred-9" ||
		frozen.BillingSubjectID != "bill-1" || frozen.TenantID != tenant {
		t.Fatalf("frozen principal lost fields: %+v", frozen)
	}
}

// --- Lease + fencing (JOB-02) --------------------------------------------

func TestRacingLeaseHandsJobToExactlyOneWorker(t *testing.T) {
	svc, _, tenant := setup(t)
	ctx := context.Background()
	p := principal(tenant, "a", "c")
	enqueue(t, svc, p, req(tenant, "k", "op", "h", 1))

	res := make(chan *jobs.Claim, 2)
	var wg sync.WaitGroup
	for _, w := range []string{"worker-a", "worker-b"} {
		wg.Add(1)
		go func(w string) {
			defer wg.Done()
			c, err := svc.Lease(ctx, w, testTTL)
			if err != nil {
				t.Errorf("lease %s: %v", w, err)
				return
			}
			res <- c
		}(w)
	}
	wg.Wait()
	close(res)

	var got []*jobs.Claim
	for c := range res {
		if c != nil {
			got = append(got, c)
		}
	}
	if len(got) != 1 {
		t.Fatalf("racing lease must hand the job to exactly one worker, got %d", len(got))
	}
}

func TestExpiredLeaseReclaimedOldFenceRejected(t *testing.T) {
	svc, _, tenant := setup(t)
	ctx := context.Background()
	p := principal(tenant, "a", "c")
	enqueue(t, svc, p, req(tenant, "k", "op", "h", 1))

	c1, err := svc.Lease(ctx, "worker-1", 60*time.Millisecond)
	if err != nil || c1 == nil {
		t.Fatalf("lease: %v", err)
	}
	time.Sleep(80 * time.Millisecond) // lease expires

	c2, err := svc.Lease(ctx, "worker-2", testTTL)
	if err != nil || c2 == nil {
		t.Fatalf("reclaim expired lease: %v c2=%v", err, c2)
	}
	if c2.FenceToken <= c1.FenceToken {
		t.Fatalf("fence must increase: %d -> %d", c1.FenceToken, c2.FenceToken)
	}
	if err := c1.Progress(ctx, 50, "stale"); !errors.Is(err, jobs.ErrStaleFence) {
		t.Fatalf("stale progress must fail fenced, got %v", err)
	}
	if _, err := c1.Complete(ctx, "r"); !errors.Is(err, jobs.ErrStaleFence) {
		t.Fatalf("stale complete must fail fenced, got %v", err)
	}
	if _, err := c2.Complete(ctx, "r-ok"); err != nil {
		t.Fatalf("live claim completes: %v", err)
	}
}

func TestExpiredLeaseWithDispatchedAttemptBecomesUnknown(t *testing.T) {
	svc, _, tenant := setup(t)
	ctx := context.Background()
	p := principal(tenant, "a", "c")
	job := enqueue(t, svc, p, req(tenant, "k", "op", "h", 1))

	c, err := svc.Lease(ctx, "worker-1", 60*time.Millisecond)
	if err != nil || c == nil {
		t.Fatalf("lease: %v", err)
	}
	if err := c.MarkDispatched(ctx); err != nil {
		t.Fatalf("mark dispatched: %v", err)
	}
	time.Sleep(80 * time.Millisecond) // lease expires with dispatched attempt open

	// Reclaim must NOT re-lease: the outcome was never observed.
	c2, err := svc.Lease(ctx, "worker-2", testTTL)
	if err != nil {
		t.Fatalf("re-lease: %v", err)
	}
	if c2 != nil {
		t.Fatalf("dispatched expired attempt must sweep to unknown, not re-lease")
	}
	got, err := svc.Get(ctx, p, job.ID)
	if err != nil {
		t.Fatalf("get: %v", err)
	}
	if got.Status != jobs.StatusUnknown {
		t.Fatalf("status = %q, want unknown", got.Status)
	}
	// Never silently replayed even by later claims.
	if c3, _ := svc.Lease(ctx, "worker-3", testTTL); c3 != nil {
		t.Fatalf("unknown job must never be re-leased")
	}
}

func TestUndispatchedExpiredLeaseReleases(t *testing.T) {
	svc, _, tenant := setup(t)
	ctx := context.Background()
	p := principal(tenant, "a", "c")
	enqueue(t, svc, p, req(tenant, "k", "op", "h", 1))

	c, err := svc.Lease(ctx, "worker-1", 60*time.Millisecond)
	if err != nil || c == nil {
		t.Fatalf("lease: %v", err)
	}
	_ = c
	time.Sleep(80 * time.Millisecond)
	c2, err := svc.Lease(ctx, "worker-2", testTTL)
	if err != nil || c2 == nil {
		t.Fatalf("undispatched expired lease must re-lease: %v", err)
	}
}

// --- Cancel + tombstone ---------------------------------------------------

func TestCancelBeforeDispatchPreventsLease(t *testing.T) {
	svc, _, tenant := setup(t)
	ctx := context.Background()
	p := principal(tenant, "a", "c")
	job := enqueue(t, svc, p, req(tenant, "k", "op", "h", 1))

	if err := svc.Cancel(ctx, p, job.ID); err != nil {
		t.Fatalf("cancel queued: %v", err)
	}
	got, _ := svc.Get(ctx, p, job.ID)
	if got.Status != jobs.StatusCancelled {
		t.Fatalf("queued cancel must transition, got %q", got.Status)
	}
	if c, _ := svc.Lease(ctx, "w", testTTL); c != nil {
		t.Fatalf("cancelled job must never lease")
	}
	if err := svc.Cancel(ctx, p, job.ID); err != nil {
		t.Fatalf("cancel cancelled is idempotent: %v", err)
	}
}

func TestCancelDuringRunFlipsOnFencedComplete(t *testing.T) {
	svc, _, tenant := setup(t)
	ctx := context.Background()
	p := principal(tenant, "a", "c")
	job := enqueue(t, svc, p, req(tenant, "k", "op", "h", 1))

	c, err := svc.Lease(ctx, "w", testTTL)
	if err != nil || c == nil {
		t.Fatalf("lease: %v", err)
	}
	if err := svc.Cancel(ctx, p, job.ID); err != nil {
		t.Fatalf("cancel running: %v", err)
	}
	if wanted, _ := c.CancelRequested(ctx); !wanted {
		t.Fatalf("worker must observe cancel request")
	}
	status, err := c.Complete(ctx, "r")
	if err != nil {
		t.Fatalf("fenced complete after cancel: %v", err)
	}
	if status != jobs.StatusCancelled {
		t.Fatalf("running cancel must flip complete to cancelled, got %q", status)
	}
	got, _ := svc.Get(ctx, p, job.ID)
	if got.Status != jobs.StatusCancelled {
		t.Fatalf("job status = %q", got.Status)
	}
}

func TestTombstonedJobCannotBeCompletedByStaleWorker(t *testing.T) {
	svc, _, tenant := setup(t)
	ctx := context.Background()
	p := principal(tenant, "a", "c")
	job := enqueue(t, svc, p, req(tenant, "k", "op", "h", 1))

	c, err := svc.Lease(ctx, "w", testTTL)
	if err != nil || c == nil {
		t.Fatalf("lease: %v", err)
	}
	if err := svc.Delete(ctx, p, job.ID); err != nil {
		t.Fatalf("delete: %v", err)
	}
	if _, err := c.Complete(ctx, "r"); !errors.Is(err, jobs.ErrStaleFence) {
		t.Fatalf("completing a tombstoned job must fail fenced, got %v", err)
	}
	if _, err := svc.Get(ctx, p, job.ID); !errors.Is(err, jobs.ErrJobNotFound) {
		t.Fatalf("deleted job must read as not-found, got %v", err)
	}
	if err := svc.Delete(ctx, p, job.ID); !errors.Is(err, jobs.ErrJobNotFound) {
		t.Fatalf("re-delete: %v", err)
	}
}

func TestCancelTerminalDeniedAndOwnershipScoping(t *testing.T) {
	svc, _, tenant := setup(t)
	ctx := context.Background()
	p := principal(tenant, "a", "c")
	other := principal(tenant, "b", "d")
	job := enqueue(t, svc, p, req(tenant, "k", "op", "h", 1))

	c, _ := svc.Lease(ctx, "w", testTTL)
	if _, err := c.Complete(ctx, "r"); err != nil {
		t.Fatalf("complete: %v", err)
	}
	if err := svc.Cancel(ctx, p, job.ID); !errors.Is(err, jobs.ErrJobTerminal) {
		t.Fatalf("cancel succeeded job must be denied, got %v", err)
	}
	// Owner scoping: another actor's principal sees nothing.
	if _, err := svc.Get(ctx, other, job.ID); !errors.Is(err, jobs.ErrJobNotFound) {
		t.Fatalf("other owner get: %v", err)
	}
	if err := svc.Cancel(ctx, other, job.ID); !errors.Is(err, jobs.ErrJobNotFound) {
		t.Fatalf("other owner cancel: %v", err)
	}
	list, err := svc.List(ctx, other, 10)
	if err != nil || len(list) != 0 {
		t.Fatalf("other owner list must be empty: %v %d", err, len(list))
	}
}
