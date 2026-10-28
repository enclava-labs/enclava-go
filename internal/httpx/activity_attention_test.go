package httpx

import (
	"context"
	"fmt"
	"testing"
	"time"

	"enclava-go/internal/chatbot"
	"enclava-go/internal/connectors"
	"enclava-go/internal/extract"
	"enclava-go/internal/jobs"
	"enclava-go/internal/rag"
)

// TestDashboardActivityManagedOperationBoundaries is the B6 gap fill for
// managed-group boundaries: a manager sees tenant jobs ONLY inside their
// managed operation groups — knowledge without sources/extract/assistants
// sees rag ops only, sources sees connector.sync only — and a member's own
// feed narrows to the operations their feature capabilities cover.
func TestDashboardActivityManagedOperationBoundaries(t *testing.T) {
	f := newActivityFixture(t)
	owner := activityPrincipal(t, f, "ops-owner", "rag", "extract", "chatbot", "connectors.manage")
	knowledgeMgr := activityPrincipal(t, f, "kmanager", "rag", "rag.manage")
	sourcesMgr := activityPrincipal(t, f, "srcmanager", "connectors.manage")
	extractOnly := activityPrincipal(t, f, "extract-member", "extract")

	seed := func(op, key string) string {
		t.Helper()
		job, _, err := f.jobsSvc.Enqueue(f.ctx, owner, jobs.EnqueueRequest{
			TenantID: f.tenant, Operation: op, IdempotencyKey: key,
			InputRef: "ref-" + key, InputHash: "h-" + key,
		})
		if err != nil {
			t.Fatalf("enqueue %s: %v", op, err)
		}
		return job.ID
	}
	embedID := seed(rag.OperationEmbed, "op-embed")
	syncID := seed(connectors.OperationSync, "op-sync")
	extractID := seed(extract.OperationProcess, "op-extract")
	runID := seed(chatbot.RunOperation, "op-run")
	memberExtract, _, err := f.jobsSvc.Enqueue(f.ctx, extractOnly, jobs.EnqueueRequest{
		TenantID: f.tenant, Operation: extract.OperationProcess, IdempotencyKey: "op-member-extract",
		InputRef: "ref-me", InputHash: "h-me",
	})
	if err != nil {
		t.Fatalf("enqueue member extract: %v", err)
	}
	memberChat, _, err := f.jobsSvc.Enqueue(f.ctx, extractOnly, jobs.EnqueueRequest{
		TenantID: f.tenant, Operation: chatbot.RunOperation, IdempotencyKey: "op-member-run",
		InputRef: "ref-mr", InputHash: "h-mr",
	})
	if err != nil {
		t.Fatalf("enqueue member run: %v", err)
	}

	ids := func(items []ActivityItem) map[string]bool {
		out := map[string]bool{}
		for _, it := range items {
			out[it.ID] = true
		}
		return out
	}

	// Knowledge manager: rag.embed only — never sync/extract/run.
	km, err := f.app.dashboardActivity(f.ctx, knowledgeMgr, dashboardCapabilities(knowledgeMgr))
	if err != nil {
		t.Fatalf("knowledge manager activity: %v", err)
	}
	got := ids(km)
	if !got[embedID] || got[syncID] || got[extractID] || got[runID] || got[memberExtract.ID] || got[memberChat.ID] {
		t.Fatalf("knowledge manager ops = %#v, want only the rag op", got)
	}

	// Sources manager: connector.sync only.
	sm, err := f.app.dashboardActivity(f.ctx, sourcesMgr, dashboardCapabilities(sourcesMgr))
	if err != nil {
		t.Fatalf("sources manager activity: %v", err)
	}
	got = ids(sm)
	if !got[syncID] || got[embedID] || got[extractID] || got[runID] {
		t.Fatalf("sources manager ops = %#v, want only connector.sync", got)
	}

	// Member with only `extract`: own extract job, never own run job.
	em, err := f.app.dashboardActivity(f.ctx, extractOnly, dashboardCapabilities(extractOnly))
	if err != nil {
		t.Fatalf("member activity: %v", err)
	}
	got = ids(em)
	if !got[memberExtract.ID] || got[memberChat.ID] {
		t.Fatalf("member own ops = %#v, want own extract only", got)
	}
}

// TestDashboardActivityCompletedJobsDoNotStarveOlderWork is the B6
// starvation regression at the tenant-activity level: predicates apply
// BEFORE the window, so an old still-live job is never pushed out of a
// manager's feed by newer completed rows.
func TestDashboardActivityCompletedJobsDoNotStarveOlderWork(t *testing.T) {
	f := newActivityFixture(t)
	other := activityPrincipal(t, f, "starve-owner", "extract")
	manager := activityPrincipal(t, f, "starve-manager", "extract", "extract.manage")

	// The old live job, backdated and lease-delayed so the lease loop never
	// claims it.
	old, _, err := f.jobsSvc.Enqueue(f.ctx, other, jobs.EnqueueRequest{
		TenantID: f.tenant, Operation: extract.OperationProcess, IdempotencyKey: "starve-old",
		InputRef: "ref-old", InputHash: "h-old",
	})
	if err != nil {
		t.Fatalf("enqueue old: %v", err)
	}
	if _, err := f.db.SQL.ExecContext(context.Background(),
		`UPDATE jobs SET submitted_at = submitted_at - interval '2 hours',
		        lease_expires_at = now() + interval '1 hour'
		 WHERE tenant_id = $1::uuid AND id = $2`, f.tenant, old.ID); err != nil {
		t.Fatalf("backdate old job: %v", err)
	}

	// More completed rows than the feed limit, all newer.
	for i := range 60 {
		_, _, err := f.jobsSvc.Enqueue(f.ctx, other, jobs.EnqueueRequest{
			TenantID: f.tenant, Operation: extract.OperationProcess,
			IdempotencyKey: fmt.Sprintf("starve-done-%d", i),
			InputRef:       fmt.Sprintf("ref-done-%d", i), InputHash: fmt.Sprintf("h-done-%d", i),
		})
		if err != nil {
			t.Fatalf("enqueue done %d: %v", i, err)
		}
		claim, err := f.jobsSvc.Lease(f.ctx, "starve-worker", time.Minute)
		if err != nil || claim == nil {
			t.Fatalf("lease done %d: claim=%v err=%v", i, claim, err)
		}
		if _, err := claim.Complete(f.ctx, "{}"); err != nil {
			t.Fatalf("complete done %d: %v", i, err)
		}
	}

	items, err := f.app.dashboardActivity(f.ctx, manager, dashboardCapabilities(manager))
	if err != nil {
		t.Fatalf("manager activity: %v", err)
	}
	found := false
	for _, it := range items {
		if it.ID == old.ID {
			found = true
			if it.Status != jobs.StatusQueued {
				t.Fatalf("starved-row status = %q, want queued", it.Status)
			}
		}
	}
	if !found {
		t.Fatalf("old queued job starved by %d newer completed jobs: %#v", len(items), items)
	}
}
