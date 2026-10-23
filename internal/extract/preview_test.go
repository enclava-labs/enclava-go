package extract_test

import (
	"context"
	"errors"
	"io"
	"strings"
	"testing"
	"time"

	"enclava-go/internal/artifacts"
	"enclava-go/internal/extract"
	"enclava-go/internal/jobs"
	"enclava-go/internal/source"
	"enclava-go/internal/store/postgres"
)

func TestPreviewJobPublishesCanonicalBlocksAndChunks(t *testing.T) {
	f := newFixture(t)
	ctx := context.Background()
	profiles := postgres.NewChunkProfilesRepo(f.db)
	profile, err := profiles.Create(ctx, f.tenant, source.ChunkProfile{
		ProfileID: "preview-legal", SizeCodePoints: 256, OverlapCodePoints: 16,
		BoundaryStrategy: "clause", IncludeParents: true, TableHandling: "atomic", PreserveClauses: true,
	}, f.principal.ActorID)
	if err != nil {
		t.Fatalf("create profile: %v", err)
	}
	preview := extract.NewPreviewService(profiles, postgres.NewJobsRepo(f.db), f.jobsSvc, f.store, f.sources, 5)
	job, err := preview.Submit(ctx, f.principal, "contract.md", "text/markdown", profile.ID, strings.NewReader("# Clause 1\nText and more text."))
	if err != nil {
		t.Fatalf("submit preview: %v", err)
	}
	claim, err := f.jobsSvc.Lease(ctx, "preview-test", time.Minute)
	if err != nil || claim == nil || claim.JobID != job.ID {
		t.Fatalf("lease preview job: claim=%v err=%v", claim, err)
	}
	resultRef, err := preview.JobHandler()(ctx, claim, func(context.Context) (io.ReadCloser, error) { return nil, nil })
	if err != nil {
		t.Fatalf("run preview: %v", err)
	}
	if _, err := claim.Complete(ctx, resultRef); err != nil {
		t.Fatalf("publish preview: %v", err)
	}
	result, err := preview.Result(ctx, f.principal, job.ID)
	if err != nil {
		t.Fatalf("read preview result: %v", err)
	}
	if result.Document.Ref.TenantID != f.tenant || len(result.Document.Blocks) == 0 || len(result.Chunks) == 0 {
		t.Fatalf("preview did not contain canonical evidence: %+v", result)
	}
	if result.ChunkProfileVersionID != profile.ID || result.Chunks[0].ProfileID != profile.ID {
		t.Fatalf("preview did not retain the immutable profile version: %+v", result)
	}
	for _, span := range result.Chunks[0].Spans {
		if span.ByteStart < 0 || span.ByteEnd <= span.ByteStart {
			t.Fatalf("invalid evidence span: %+v", span)
		}
	}
	if result.OriginalURL != "/api/v1/document-previews/"+job.ID+"/original" {
		t.Fatalf("unexpected original URL: %q", result.OriginalURL)
	}
	other := f.principal
	other.ActorID += "-other"
	if _, err := preview.Result(ctx, other, job.ID); err == nil {
		t.Fatal("another actor read the preview result")
	}
}

func TestCancelledPreviewCannotPublishEvidence(t *testing.T) {
	f := newFixture(t)
	ctx := context.Background()
	profiles := postgres.NewChunkProfilesRepo(f.db)
	profile, err := profiles.Create(ctx, f.tenant, source.ChunkProfile{
		ProfileID: "preview-cancel", SizeCodePoints: 256, BoundaryStrategy: "fixed", TableHandling: "row-wise",
	}, f.principal.ActorID)
	if err != nil {
		t.Fatalf("create profile: %v", err)
	}
	preview := extract.NewPreviewService(profiles, postgres.NewJobsRepo(f.db), f.jobsSvc, f.store, f.sources, 5)
	job, err := preview.Submit(ctx, f.principal, "cancel.txt", "text/plain", profile.ID, strings.NewReader("cancel me"))
	if err != nil {
		t.Fatalf("submit preview: %v", err)
	}
	if err := preview.Cancel(ctx, f.principal, job.ID); err != nil {
		t.Fatalf("cancel preview: %v", err)
	}
	if _, err := preview.Result(ctx, f.principal, job.ID); err == nil {
		t.Fatal("cancelled preview exposed a result")
	}
}

func TestPreviewAdmissionLimitCountsOnlyQueuedAndRunning(t *testing.T) {
	f := newFixture(t)
	ctx := context.Background()
	profiles := postgres.NewChunkProfilesRepo(f.db)
	profile, err := profiles.Create(ctx, f.tenant, source.ChunkProfile{
		ProfileID: "preview-admission", SizeCodePoints: 256, BoundaryStrategy: "fixed", TableHandling: "row-wise",
	}, f.principal.ActorID)
	if err != nil {
		t.Fatal(err)
	}
	preview := extract.NewPreviewService(profiles, postgres.NewJobsRepo(f.db), f.jobsSvc, f.store, f.sources, 5)
	for range 25 {
		if _, err := preview.Submit(ctx, f.principal, "admit.txt", "text/plain", profile.ID, strings.NewReader("admit")); err != nil {
			t.Fatalf("submission rejected before admission limit: %v", err)
		}
	}
	if _, err := preview.Submit(ctx, f.principal, "admit.txt", "text/plain", profile.ID, strings.NewReader("admit")); !errors.Is(err, extract.ErrPreviewAdmissionLimited) {
		t.Fatalf("26th submission err=%v, want admission limit", err)
	}
	claim, err := f.jobsSvc.Lease(ctx, "admission-test", time.Minute)
	if err != nil || claim == nil {
		t.Fatalf("lease: claim=%v err=%v", claim, err)
	}
	if _, err := claim.Complete(ctx, ""); err != nil {
		t.Fatalf("complete one preview: %v", err)
	}
	if _, err := preview.Submit(ctx, f.principal, "admit.txt", "text/plain", profile.ID, strings.NewReader("admit")); err != nil {
		t.Fatalf("terminal preview still counted: %v", err)
	}
}

func TestPreviewRetentionSweepDeletesOldTerminalAndArtifacts(t *testing.T) {
	f := newFixture(t)
	ctx := context.Background()
	repo := postgres.NewJobsRepo(f.db)
	makeArtifact := func(text string) string {
		t.Helper()
		w, _, err := f.store.Begin(ctx, artifacts.Meta{Ref: artifacts.Ref{TenantID: f.tenant, Kind: artifacts.KindOriginal}}, time.Minute, 1024)
		if err != nil {
			t.Fatal(err)
		}
		if _, err := w.Write([]byte(text)); err != nil {
			t.Fatal(err)
		}
		if err := w.Finalize(ctx); err != nil {
			t.Fatal(err)
		}
		return w.Ref().ID
	}
	oldInput, oldResult := makeArtifact("old input"), makeArtifact("old result")
	makeJob := func(key, status, input, result string) string {
		job, _, err := f.jobsSvc.Enqueue(ctx, f.principal, jobs.EnqueueRequest{
			TenantID: f.tenant, Operation: extract.OperationPreview, IdempotencyKey: key,
			InputRef: input, Policy: map[string]any{"upload_owned": true},
		})
		if err != nil {
			t.Fatal(err)
		}
		_, err = f.db.SQL.ExecContext(ctx, `UPDATE jobs SET status=$2, finished_at=now()-interval '8 days', result_ref=$3 WHERE id=$1`, job.ID, status, result)
		if err != nil {
			t.Fatal(err)
		}
		return job.ID
	}
	oldID := makeJob("retention-old", jobs.StatusSucceeded, oldInput, oldResult)
	freshInput := makeArtifact("fresh")
	freshID, _, err := f.jobsSvc.Enqueue(ctx, f.principal, jobs.EnqueueRequest{
		TenantID: f.tenant, Operation: extract.OperationPreview, IdempotencyKey: "retention-running",
		InputRef: freshInput, Policy: map[string]any{"upload_owned": true},
	})
	if err != nil {
		t.Fatal(err)
	}
	if _, err := f.db.SQL.ExecContext(ctx, `UPDATE jobs SET status='running' WHERE id=$1`, freshID.ID); err != nil {
		t.Fatal(err)
	}
	profiles := postgres.NewChunkProfilesRepo(f.db)
	preview := extract.NewPreviewService(profiles, repo, f.jobsSvc, f.store, f.sources, 5)
	preview.RunRetentionSweep(ctx)
	var deletedAt any
	if err := f.db.SQL.QueryRowContext(ctx, `SELECT deleted_at FROM jobs WHERE id=$1`, oldID).Scan(&deletedAt); err != nil || deletedAt == nil {
		t.Fatalf("old job not deleted: deleted_at=%v err=%v", deletedAt, err)
	}
	for _, id := range []string{oldInput, oldResult} {
		if _, err := f.store.Read(ctx, artifacts.Ref{TenantID: f.tenant, ID: id}); err == nil {
			t.Fatalf("artifact %s was retained", id)
		}
	}
	var status string
	if err := f.db.SQL.QueryRowContext(ctx, `SELECT status FROM jobs WHERE id=$1`, freshID.ID).Scan(&status); err != nil || status != jobs.StatusRunning {
		t.Fatalf("running job status=%q err=%v", status, err)
	}
}
