package extract_test

import (
	"context"
	"io"
	"strings"
	"testing"
	"time"

	"enclava-go/internal/extract"
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
