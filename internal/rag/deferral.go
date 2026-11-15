// Resilient ingest (plan §3.7): a document never ends up "failed" because
// the extraction worker was down or restarting. The embed handler maps
// worker unavailability onto jobs.Deferred (requeue, attempt not consumed);
// only a worker that keeps dying mid-parse on the same document, or one
// that stays down past EXTRACT_WORKER_MAX_WAIT, fails it terminally.
package rag

import (
	"context"
	"errors"
	"fmt"
	"time"

	"enclava-go/internal/auth"
	"enclava-go/internal/extract"
	"enclava-go/internal/jobs"
	"enclava-go/internal/source"
)

const (
	// Deferral backoff: 15 s doubling per consecutive deferral, 5 m cap.
	deferBaseDelay = 15 * time.Second
	deferMaxDelay  = 5 * time.Minute
	// A worker that drops the connection mid-parse this many times on one
	// job fails it: a document that crashes the worker must not loop.
	midParseDropLimit = 3
	// defaultWorkerMaxWait mirrors config's EXTRACT_WORKER_MAX_WAIT default;
	// SetExtractWorkerMaxWait overrides it.
	defaultWorkerMaxWait = 24 * time.Hour

	// waitingMessage is the neutral document-side state while a job is
	// deferred for the extraction worker (never an error).
	waitingMessage = StatusWaitingForWorker
)

// SetExtractWorkerMaxWait bounds how long one job may stay continuously
// deferred for the extraction worker before it fails terminally (0 restores
// the 24 h default). Wired from EXTRACT_WORKER_MAX_WAIT in main.
func (s *Service) SetExtractWorkerMaxWait(d time.Duration) {
	if d <= 0 {
		d = defaultWorkerMaxWait
	}
	s.workerMaxWait = d
}

func (s *Service) maxWorkerWait() time.Duration {
	if s.workerMaxWait <= 0 {
		return defaultWorkerMaxWait
	}
	return s.workerMaxWait
}

// deferDelay doubles from 15 s per already-recorded deferral, capped at 5 m.
func deferDelay(deferrals int) time.Duration {
	d := deferBaseDelay
	for range deferrals {
		d *= 2
		if d >= deferMaxDelay {
			return deferMaxDelay
		}
	}
	return d
}

// classifyParseError maps an embed attempt's parse failure onto the durable
// outcome. Worker unavailability defers (never fails) the job; a third
// mid-parse drop or an exceeded max-wait fails it with the specific
// message. Every other error keeps its previous classification.
func (s *Service) classifyParseError(ctx context.Context, claim *jobs.Claim, err error) error {
	if !errors.Is(err, extract.ErrWorkerUnavailable) {
		return err
	}
	job := claim.Job()
	dropped := errors.Is(err, extract.ErrWorkerDroppedMidParse)
	if dropped && job.DropDeferrals+1 >= midParseDropLimit {
		return fmt.Errorf("the extraction worker stopped while processing this document (%d mid-parse drops)", midParseDropLimit)
	}
	if job.FirstDeferredAt != nil && time.Since(*job.FirstDeferredAt) >= s.maxWorkerWait() {
		return fmt.Errorf("the extraction worker was unavailable for over %s; the document failed so it can be retried once the worker is back", s.maxWorkerWait().Round(time.Minute))
	}
	s.markWaiting(ctx, job)
	if dropped {
		return jobs.DeferredDrop(err, deferDelay(job.Deferrals))
	}
	return jobs.Deferred(err, deferDelay(job.Deferrals))
}

// markWaiting records the neutral wait state on the document's staged
// generation: the document stays processing and the message clears itself
// when a later attempt succeeds and rewrites the generation's warnings.
func (s *Service) markWaiting(ctx context.Context, job jobs.Job) {
	collectionID, _ := job.Policy["collection_id"].(string)
	generation := int64FromAny(job.Policy["generation"])
	if collectionID == "" || generation == 0 || job.InputRef == "" {
		return
	}
	_ = s.repo.SetGenerationWarnings(ctx, job.TenantID, collectionID, job.InputRef, generation, []string{waitingMessage})
}

// ReprocessFailedDocuments requeues every failed document in the collection
// through the same code path as ReprocessDocument (same authz shape:
// collection managers only, per-document read ACLs still apply and skip
// unreadable rows). Returns the number of documents requeued.
func (s *Service) ReprocessFailedDocuments(ctx context.Context, p auth.Principal, collectionID string) (int, error) {
	c, err := s.repo.GetCollection(ctx, p.TenantID, collectionID)
	if err != nil {
		return 0, err
	}
	if !collectionManageable(c, p) {
		return 0, source.ErrDenied
	}
	// Collect the failed set BEFORE reprocessing: a reprocess stages a new
	// generation and would shift a live "failed" filter mid-pagination.
	var failed []Document
	const page = 200
	for offset := 0; ; offset += page {
		docs, err := s.repo.ListDocuments(ctx, p.TenantID, c.ID, DocumentStatusFailed, page, offset)
		if err != nil {
			return 0, err
		}
		failed = append(failed, docs...)
		if len(docs) < page {
			break
		}
	}
	requeued := 0
	for _, doc := range failed {
		// ReprocessDocument performs the per-document authorization; a
		// document the caller cannot read is skipped, not fatal — the bulk
		// endpoint requeues exactly what its caller may touch.
		if _, err := s.ReprocessDocument(ctx, p, doc.ID); err != nil {
			if errors.Is(err, source.ErrDenied) {
				continue
			}
			return requeued, err
		}
		requeued++
	}
	return requeued, nil
}
