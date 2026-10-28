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

// TestListActorActivityScopesByActorAndOperation proves the actor key (not
// the submitter binding): a member's NEW session (different credential →
// different submitter key) still sees the job an earlier session submitted,
// and never another actor's job. Operation/status filters narrow the feed.
func TestListActorActivityScopesByActorAndOperation(t *testing.T) {
	svc, db, tenant := setup(t)
	ctx := context.Background()

	aliceOld := principal(tenant, "alice", "cred-1")
	aliceNew := principal(tenant, "alice", "cred-2")
	bob := principal(tenant, "bob", "cred-1")

	oldJob := enqueue(t, svc, aliceOld, req(tenant, "alice-old", "extract.process", "h1", 1))
	bobJob := enqueue(t, svc, bob, req(tenant, "bob-1", "extract.process", "h2", 1))
	newJob := enqueue(t, svc, aliceNew, req(tenant, "alice-new", "chatbot.run", "h3", 1))

	// New session, all statuses/ops: sees BOTH own jobs across the session
	// boundary, never bob's.
	aliceJobs, err := svc.ListActorActivity(ctx, aliceNew, nil, nil, 50)
	if err != nil {
		t.Fatalf("list actor activity: %v", err)
	}
	got := map[string]bool{}
	for _, j := range aliceJobs {
		got[j.ID] = true
	}
	if !got[oldJob.ID] || !got[newJob.ID] {
		t.Fatalf("alice's new session lost an earlier job: %#v", aliceJobs)
	}
	if got[bobJob.ID] {
		t.Fatalf("another actor's job leaked into the actor feed: %#v", aliceJobs)
	}

	// Operation filter narrows to matching ops only.
	runJobs, err := svc.ListActorActivity(ctx, aliceNew, nil, []string{"chatbot.run"}, 50)
	if err != nil || len(runJobs) != 1 || runJobs[0].ID != newJob.ID {
		t.Fatalf("operation filter = %#v err=%v, want only %s", runJobs, err, newJob.ID)
	}

	// Status filter narrows likewise.
	queuedJobs, err := svc.ListActorActivity(ctx, aliceNew, []string{jobs.StatusQueued}, nil, 50)
	if err != nil || len(queuedJobs) != 2 {
		t.Fatalf("status filter = %d jobs err=%v, want both queued", len(queuedJobs), err)
	}

	// Tenant isolation: another tenant's actor sees nothing of alice's.
	otherTenant := testdb.SeedTenant(t, db, "actor-other")
	foreign, err := svc.ListActorActivity(ctx, principal(otherTenant, "alice", "cred-1"), nil, nil, 50)
	if err != nil || len(foreign) != 0 {
		t.Fatalf("cross-tenant actor leak: %#v err=%v", foreign, err)
	}
}

// TestListActorActivityPredicatesBeforeLimit proves the non-starvation rule:
// an old queued job and an old FAILED job stay visible behind a full page of
// newer successes, because status/operation predicates apply in SQL BEFORE
// the LIMIT.
func TestListActorActivityPredicatesBeforeLimit(t *testing.T) {
	svc, db, tenant := setup(t)
	ctx := context.Background()
	p := principal(tenant, "alice", "cred-1")

	// The older failure: real worker path — lease then fenced terminal fail.
	failedJob := enqueue(t, svc, p, req(tenant, "k-failed", "extract.process", "hf", 1))
	claim := leaseJob(t, svc, failedJob.ID)
	if err := claim.Fail(ctx, errors.New("boom"), jobs.FailureFatal); err != nil {
		t.Fatalf("fail job: %v", err)
	}

	// The older queued job, backdated so it is the oldest row, and delayed so
	// the test's lease loop never claims it.
	queuedJob := enqueue(t, svc, p, req(tenant, "k-queued", "extract.process", "hq", 1))
	if _, err := db.SQL.ExecContext(ctx,
		`UPDATE jobs SET submitted_at = submitted_at - interval '2 hours',
		        lease_expires_at = now() + interval '1 hour'
		 WHERE tenant_id = $1::uuid AND id = $2`, tenant, queuedJob.ID); err != nil {
		t.Fatalf("backdate queued job: %v", err)
	}

	// A full page of newer successes on top.
	for i := range 50 {
		done := enqueue(t, svc, p, req(tenant, fmt.Sprintf("k-done-%d", i), "extract.process", fmt.Sprintf("hd%d", i), int64(i+2)))
		claim := leaseJob(t, svc, done.ID)
		if _, err := claim.Complete(ctx, "{}"); err != nil {
			t.Fatalf("complete job: %v", err)
		}
	}

	queued, err := svc.ListActorActivity(ctx, p, []string{jobs.StatusQueued}, nil, 50)
	if err != nil || len(queued) != 1 || queued[0].ID != queuedJob.ID {
		t.Fatalf("old queued job starved behind newer successes: %#v err=%v", queued, err)
	}
	failed, err := svc.ListActorActivity(ctx, p, []string{jobs.StatusFailed}, nil, 50)
	if err != nil || len(failed) != 1 || failed[0].ID != failedJob.ID {
		t.Fatalf("old failed job starved behind newer successes: %#v err=%v", failed, err)
	}
	all, err := svc.ListActorActivity(ctx, p, nil, nil, 50)
	if err != nil || len(all) != 50 {
		t.Fatalf("unfiltered page = %d jobs err=%v, want the newest 50", len(all), err)
	}
	for _, j := range all {
		if j.ID == queuedJob.ID || j.ID == failedJob.ID {
			t.Fatalf("older non-matching job consumed the newest-50 page: %#v", all)
		}
	}
}

// TestListActorActivityCarriesRealProgressOnly proves the progress read
// projection: a job whose worker published real counts carries the stored
// document verbatim; a job without counts carries none.
func TestListActorActivityCarriesRealProgressOnly(t *testing.T) {
	svc, _, tenant := setup(t)
	ctx := context.Background()
	p := principal(tenant, "alice", "cred-1")

	counted := enqueue(t, svc, p, req(tenant, "k-counted", "extract.process", "hc1", 1))
	blank := enqueue(t, svc, p, req(tenant, "k-blank", "extract.process", "hc2", 1))

	claim := leaseJob(t, svc, counted.ID)
	if err := claim.Progress(ctx, 42, "halfway"); err != nil {
		t.Fatalf("publish progress: %v", err)
	}

	rows, err := svc.ListActorActivity(ctx, p, nil, nil, 50)
	if err != nil {
		t.Fatalf("list actor activity: %v", err)
	}
	progressByID := map[string]json.RawMessage{}
	for _, j := range rows {
		progressByID[j.ID] = j.Progress
	}
	if got := progressByID[counted.ID]; got == nil {
		t.Fatal("counted job carries no progress, want the stored document verbatim")
	} else {
		var doc struct {
			Percent int    `json:"percent"`
			Message string `json:"message"`
		}
		if err := json.Unmarshal(got, &doc); err != nil || doc.Percent != 42 || doc.Message != "halfway" {
			t.Fatalf("counted job progress = %q (decoded %+v err=%v), want percent 42 halfway", got, doc, err)
		}
	}
	if got := progressByID[blank.ID]; got != nil {
		t.Fatalf("non-counted job carries progress %q, want none", got)
	}
}

// leaseJob leases one specific queued job, completing anything older that
// gets in the way.
func leaseJob(t *testing.T, svc *jobs.Service, wantID string) *jobs.Claim {
	t.Helper()
	ctx := context.Background()
	for {
		claim, err := svc.Lease(ctx, "test-worker", time.Minute)
		if err != nil {
			t.Fatalf("lease: %v", err)
		}
		if claim == nil {
			t.Fatalf("job %s never became claimable", wantID)
		}
		if claim.Job().ID == wantID {
			return claim
		}
		if _, err := claim.Complete(ctx, "{}"); err != nil {
			t.Fatalf("complete bystander job: %v", err)
		}
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
