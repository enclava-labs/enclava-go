package rag_test

import (
	"context"
	"errors"
	"net"
	"net/http"
	"path/filepath"
	"strings"
	"testing"
	"time"

	"enclava-go/internal/auth"
	"enclava-go/internal/extract"
	"enclava-go/internal/jobs"
	"enclava-go/internal/rag"
)

// runEmbedAttempt leases the next embed job and settles it the way the
// runner would: the handler's error class decides the fenced transition.
// Returns the claimed job row and the handler error.
func runEmbedAttempt(t *testing.T, f *fixture) (jobs.Job, error) {
	t.Helper()
	ctx := context.Background()
	claim, err := f.jobs.Lease(ctx, "deferral-worker", time.Minute)
	if err != nil {
		t.Fatalf("lease: %v", err)
	}
	if claim == nil {
		t.Fatal("no queued job to run")
	}
	_, herr := f.svc.JobHandler()(ctx, claim, nil)
	if herr == nil {
		if _, err := claim.Complete(ctx, ""); err != nil {
			t.Fatalf("complete: %v", err)
		}
		return claim.Job(), nil
	}
	if err := claim.Fail(ctx, herr, jobs.FailureClassFor(herr)); err != nil {
		t.Fatalf("settle %v: %v", herr, err)
	}
	return claim.Job(), herr
}

// waitForJobStatus polls the owner view until the job reaches status.
func waitForJobStatus(t *testing.T, f *fixture, p auth.Principal, id, want string) jobs.Job {
	t.Helper()
	deadline := time.Now().Add(3 * time.Second)
	for time.Now().Before(deadline) {
		job, err := f.jobs.Get(context.Background(), p, id)
		if err == nil && job.Status == want {
			return job
		}
		time.Sleep(10 * time.Millisecond)
	}
	job, _ := f.jobs.Get(context.Background(), p, id)
	t.Fatalf("job %s status = %q, want %q", id, job.Status, want)
	return job
}

// expireRunAfter fast-forwards the deferral delay: the 15s..5m backoff is
// production timing, not what the test is proving.
func expireRunAfter(t *testing.T, f *fixture, jobID string) {
	t.Helper()
	if _, err := f.db.SQL.Exec(`UPDATE jobs SET lease_expires_at = now() WHERE id = $1`, jobID); err != nil {
		t.Fatalf("expire run-after: %v", err)
	}
}

// uploadWorkerDocument uploads a PDF (the worker path) into a fresh
// collection; with the worker socket pointed at a dead path the embed
// attempt hits ErrWorkerUnavailable at dial time.
func uploadWorkerDocument(t *testing.T, f *fixture, name string) rag.Document {
	t.Helper()
	c := f.collection(t)
	out, err := f.svc.UploadDocument(context.Background(), f.principal, c.ID, rag.UploadDocumentInput{
		FileName: name, MimeType: "application/pdf", Bytes: []byte("%PDF-1.4\n%%EOF\n"),
	})
	if err != nil {
		t.Fatalf("upload %s: %v", name, err)
	}
	return out.Document
}

func withDeadWorker(t *testing.T) {
	t.Helper()
	extract.SetNativeWorker(filepath.Join(t.TempDir(), "absent.sock"))
	t.Cleanup(func() { extract.SetNativeWorker("") })
}

// TestEmbedDefersWhenWorkerDown pins acceptance criterion 1: with the worker
// gone, the embed attempt DEFERS — the job returns to queued without
// consuming an attempt, the document stays processing with the waiting
// message, nothing is failed.
func TestEmbedDefersWhenWorkerDown(t *testing.T) {
	f := newFixture(t)
	withDeadWorker(t)
	doc := uploadWorkerDocument(t, f, "worker-down.pdf")

	job, err := runEmbedAttempt(t, f)
	if err == nil {
		t.Fatal("handler should report worker unavailability")
	}
	var de jobs.DeferredError
	if !errors.As(err, &de) {
		t.Fatalf("err = %v, want a jobs.Deferred error", err)
	}
	if !errors.Is(err, extract.ErrWorkerUnavailable) {
		t.Fatalf("err = %v, want ErrWorkerUnavailable inside", err)
	}
	if de.Delay != 15*time.Second {
		t.Fatalf("first deferral delay = %v, want 15s", de.Delay)
	}

	row := waitForJobStatus(t, f, f.principal, job.ID, jobs.StatusQueued)
	if row.Attempt != 0 {
		t.Fatalf("attempts = %d, want 0 after deferral", row.Attempt)
	}
	if row.Deferrals != 1 || row.DropDeferrals != 0 || row.FirstDeferredAt == nil {
		t.Fatalf("counters = %d/%d first=%v, want 1/0 non-nil", row.Deferrals, row.DropDeferrals, row.FirstDeferredAt)
	}

	d := f.docStatus(t, doc.CollectionID, doc.ID)
	if d.Status != rag.DocumentStatusProcessing {
		t.Fatalf("document status = %q, want processing", d.Status)
	}
	if d.StatusMessage != rag.StatusWaitingForWorker || len(d.Warnings) != 0 {
		t.Fatalf("document status message = %q warnings = %v, want the waiting message as status only", d.StatusMessage, d.Warnings)
	}
	if d.ErrorMessage != "" {
		t.Fatalf("document error = %q, want empty (not failed)", d.ErrorMessage)
	}

	// Backoff doubles on the next deferral; attempts stay unconsumed.
	expireRunAfter(t, f, job.ID)
	job, err = runEmbedAttempt(t, f)
	if err == nil {
		t.Fatal("second attempt should defer too")
	}
	if !errors.As(err, &de) || de.Delay != 30*time.Second {
		t.Fatalf("second deferral delay = %v, want 30s", err)
	}
	row = waitForJobStatus(t, f, f.principal, job.ID, jobs.StatusQueued)
	if row.Attempt != 0 || row.Deferrals != 2 {
		t.Fatalf("attempts/deferrals = %d/%d, want 0/2", row.Attempt, row.Deferrals)
	}
}

// serveDropWorker binds a worker socket whose /v1/parse accepts the request
// and then drops the connection mid-response — the mid-parse drop shape.
func serveDropWorker(t *testing.T) string {
	t.Helper()
	l, err := net.Listen("unix", filepath.Join(t.TempDir(), "drop.sock"))
	if err != nil {
		t.Fatal(err)
	}
	server := &http.Server{Handler: http.HandlerFunc(func(w http.ResponseWriter, _ *http.Request) {
		hj, ok := w.(http.Hijacker)
		if !ok {
			t.Fatal("cannot hijack")
		}
		conn, _, err := hj.Hijack()
		if err != nil {
			t.Fatal(err)
		}
		_, _ = conn.Write([]byte("HTTP/1.1 200 OK\r\nContent-Type: application/json\r\nContent-Length: 500\r\n\r\n{\"blocks\":["))
		_ = conn.Close()
	}), ReadHeaderTimeout: 10 * time.Second}
	go func() { _ = server.Serve(l) }()
	t.Cleanup(func() { _ = server.Close(); _ = l.Close() })
	return l.Addr().String()
}

// TestEmbedFailsAfterThreeMidParseDrops pins acceptance criterion 4: two
// mid-parse drops defer; the third fails the document with the specific
// message, and the drop counter drove the decision.
func TestEmbedFailsAfterThreeMidParseDrops(t *testing.T) {
	f := newFixture(t)
	extract.SetNativeWorker(serveDropWorker(t))
	t.Cleanup(func() { extract.SetNativeWorker("") })
	doc := uploadWorkerDocument(t, f, "crasher.pdf")

	var job jobs.Job
	var lastErr error
	var de jobs.DeferredError
	for range 2 {
		job, lastErr = runEmbedAttempt(t, f)
		if lastErr == nil || !errors.As(lastErr, &de) || !de.Drop {
			t.Fatalf("drop attempt err = %v, want a drop deferral", lastErr)
		}
		waitForJobStatus(t, f, f.principal, job.ID, jobs.StatusQueued)
		expireRunAfter(t, f, job.ID)
	}

	job, lastErr = runEmbedAttempt(t, f)
	if lastErr == nil || errors.As(lastErr, &de) {
		t.Fatalf("third drop err = %v, want terminal failure", lastErr)
	}
	if !strings.Contains(lastErr.Error(), "the extraction worker stopped while processing this document") {
		t.Fatalf("err = %v, want the mid-parse drop message", lastErr)
	}
	row := waitForJobStatus(t, f, f.principal, job.ID, jobs.StatusFailed)
	if row.DropDeferrals != 2 {
		t.Fatalf("drop_deferrals = %d, want 2 (third drop failed, not deferred)", row.DropDeferrals)
	}
	d := f.docStatus(t, doc.CollectionID, doc.ID)
	if d.Status != rag.DocumentStatusFailed || !strings.Contains(d.ErrorMessage, "stopped while processing this document") {
		t.Fatalf("document = %s/%q, want failed with the drop message", d.Status, d.ErrorMessage)
	}
}

// TestEmbedFailsAfterMaxWait pins acceptance criterion 3: once the
// continuous deferral span passes EXTRACT_WORKER_MAX_WAIT the next attempt
// fails terminally with a clear message instead of waiting forever.
func TestEmbedFailsAfterMaxWait(t *testing.T) {
	f := newFixture(t)
	f.svc.SetExtractWorkerMaxWait(50 * time.Millisecond)
	withDeadWorker(t)
	doc := uploadWorkerDocument(t, f, "patient.pdf")

	job, err := runEmbedAttempt(t, f)
	if err == nil {
		t.Fatal("first attempt should defer")
	}
	waitForJobStatus(t, f, f.principal, job.ID, jobs.StatusQueued)
	time.Sleep(60 * time.Millisecond) // pass the max wait
	expireRunAfter(t, f, job.ID)

	var de jobs.DeferredError
	job, err = runEmbedAttempt(t, f)
	if err == nil || errors.As(err, &de) {
		t.Fatalf("err = %v, want terminal failure past max wait", err)
	}
	if !strings.Contains(err.Error(), "unavailable for over") {
		t.Fatalf("err = %v, want the max-wait message", err)
	}
	waitForJobStatus(t, f, f.principal, job.ID, jobs.StatusFailed)
	d := f.docStatus(t, doc.CollectionID, doc.ID)
	if d.Status != rag.DocumentStatusFailed {
		t.Fatalf("document status = %q, want failed", d.Status)
	}
}

// TestEmbedNonWorkerErrorsStayFatal pins that the classification only
// changes worker availability: a whitespace-only upload (no extractable
// text) still fails terminally, exactly as before.
func TestEmbedNonWorkerErrorsStayFatal(t *testing.T) {
	f := newFixture(t)
	c := f.collection(t)
	var body strings.Builder
	for _, b := range []byte("blank.txt") {
		body.WriteString(strings.Repeat(" ", int(b)+1))
		body.WriteByte('\t')
	}
	out, err := f.svc.UploadDocument(context.Background(), f.principal, c.ID, rag.UploadDocumentInput{
		FileName: "blank.txt", MimeType: "text/plain", Bytes: []byte(body.String()),
	})
	if err != nil {
		t.Fatal(err)
	}
	_, herr := runEmbedAttempt(t, f)
	if herr == nil || errors.As(herr, new(jobs.DeferredError)) {
		t.Fatalf("err = %v, want a plain fatal error", herr)
	}
	if errors.Is(herr, extract.ErrWorkerUnavailable) {
		t.Fatalf("err = %v, must not classify as worker unavailability", herr)
	}
	d := f.docStatus(t, out.Document.CollectionID, out.Document.ID)
	if d.Status != rag.DocumentStatusFailed {
		t.Fatalf("document status = %q, want failed", d.Status)
	}
	if len(d.Warnings) != 0 {
		t.Fatalf("warnings = %v, want none for a fatal parse failure", d.Warnings)
	}
}
