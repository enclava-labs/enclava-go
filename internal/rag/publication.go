// Package-level access projection runs beside fenced generation publication.
package rag

import (
	"context"
	"encoding/json"
	"errors"
	"fmt"
	"log"
	"time"

	"enclava-go/internal/auth"
	"enclava-go/internal/jobs"
	"enclava-go/internal/source"
)

// EnqueueAccessBackfills enqueues one job per tenant with an unprojected backlog.
func (s *Service) EnqueueAccessBackfills(ctx context.Context) error {
	if s.jobs == nil {
		return nil
	}
	tenants, err := s.repo.TenantsNeedingAccessBackfill(ctx)
	if err != nil {
		return err
	}
	if len(tenants) == 0 {
		return nil
	}
	for _, tenantID := range tenants {
		p := auth.Principal{TenantID: tenantID, ActorID: "system", ActorKind: auth.ActorKindServiceAccount}
		active, err := s.jobs.ListTenantActivity(ctx, p, []string{OperationAccessBackfill}, []string{jobs.StatusQueued, jobs.StatusRunning}, 1)
		if err != nil {
			return err
		}
		if len(active) > 0 {
			continue
		}
		if _, _, err := s.jobs.Enqueue(ctx, p, jobs.EnqueueRequest{
			TenantID: tenantID, Operation: OperationAccessBackfill,
			IdempotencyKey: OperationAccessBackfill + ":" + tenantID + ":" + newID("fill"),
			InputRef:       tenantID, InputHash: "backfill",
			Policy: map[string]any{"tenant_id": tenantID},
			Lane:   jobs.LaneBackground,
		}); err != nil {
			return err
		}
	}
	return nil
}

func (s *Service) RunAccessBackfillLoop(ctx context.Context, interval time.Duration) {
	ticker := time.NewTicker(interval)
	defer ticker.Stop()
	for {
		if err := s.EnqueueAccessBackfills(ctx); err != nil && ctx.Err() == nil {
			log.Printf("rag access backfill enqueue error: %v", err)
		}
		select {
		case <-ctx.Done():
			return
		case <-ticker.C:
		}
	}
}

func (s *Service) projectGenerationAccess(ctx context.Context, tenantID string, generation IndexGeneration, payload map[string]any, published bool) error {
	return s.repo.ProjectGenerationAccess(ctx, tenantID, generation.CollectionID, generation.ObjectVersionID, generation.Generation, func() error {
		object, err := s.repo.GetObject(ctx, tenantID, generation.ObjectID)
		if err != nil {
			return fmt.Errorf("project access tokens: %w", err)
		}
		tokens, err := s.authorizer.ReadTokens(ctx, object)
		if err != nil {
			return err
		}
		payload["acl"] = tokens
		if published {
			payload["published"] = true
		}
		return s.vectorStore.SetPayload(ctx, generation.QdrantCollection, map[string]any{"must": []map[string]any{
			{"key": "object_version_id", "match": map[string]any{"value": generation.ObjectVersionID}},
			{"key": "generation", "match": map[string]any{"value": generation.Generation}},
		}}, payload)
	})
}

// AccessBackfillJobHandler projects acl + published onto points written
// before the access projection existed. Durable, idempotent and resumable:
// rows flip access_projected only after their set-payload write lands, so
// an interrupted run re-enumerates the same backlog and finishes it.
func (s *Service) AccessBackfillJobHandler() jobs.HandlerFunc {
	return func(ctx context.Context, claim *jobs.Claim, _ jobs.InputFunc) (string, error) {
		job := claim.Job()
		var p auth.Principal
		if err := json.Unmarshal(job.PrincipalSnapshot, &p); err != nil {
			return "", fmt.Errorf("decode frozen principal: %w", err)
		}
		if p.TenantID != job.TenantID || job.InputRef != job.TenantID {
			return "", source.ErrDenied
		}
		if s.vectorStore == nil {
			return "", errors.New("rag access backfill: vector store is not configured")
		}
		if err := claim.MarkDispatched(ctx); err != nil {
			return "", err
		}
		projected := 0
		for {
			generations, err := s.repo.ListUnprojectedGenerations(ctx, job.TenantID, 200)
			if err != nil {
				return "", err
			}
			if len(generations) == 0 {
				return fmt.Sprintf("projected=%d", projected), nil
			}
			for _, generation := range generations {
				if err := s.projectGenerationAccess(ctx, job.TenantID, generation, map[string]any{}, true); err != nil {
					return "", fmt.Errorf("project generation access: %w", err)
				}
				projected++
			}
		}
	}
}

// ErrIndexReconciliationChanged reports that another worker completed or
// re-fenced a pending index reconciliation first.
var ErrIndexReconciliationChanged = errors.New("index reconciliation reservation changed")

func (s *Service) ReconcileIndexPoints(ctx context.Context, tenantID string) error {
	if s.vectorStore == nil {
		return errors.New("rag reconciliation: vector store is not configured")
	}
	for {
		pending, err := s.repo.ListPendingIndexReconciliations(ctx, tenantID, 100)
		if err != nil {
			return err
		}
		if len(pending) == 0 {
			return nil
		}
		completed := 0
		for _, item := range pending {
			published, err := s.repo.ListPublishedGenerations(ctx, tenantID, item.CollectionID)
			if err != nil {
				return err
			}
			for _, current := range published {
				if current.ObjectVersionID == item.ObjectVersionID && current.Generation == item.Generation {
					return fmt.Errorf("rag reconciliation: refusing to delete published generation %s/%d", item.ObjectVersionID, item.Generation)
				}
			}
			if err := deleteReconciledGeneration(ctx, s.vectorStore, item, false); err != nil {
				return err
			}
			// Every embed attempt reconciles the tenant, so concurrent workers race
			// on the same items; losing that race leaves nothing for this one to do.
			if err := s.repo.CompleteIndexReconciliation(ctx, item); errors.Is(err, ErrIndexReconciliationChanged) {
				continue
			} else if err != nil {
				return err
			}
			completed++
		}
		if completed == 0 {
			return nil
		}
	}
}

type generationPointDeleter interface {
	DeleteGenerationPoints(ctx context.Context, collection, versionID string, generation int64) error
}

func deleteReconciledGeneration(ctx context.Context, store generationPointDeleter, item IndexReconciliation, isPublished bool) error {
	if isPublished {
		return fmt.Errorf("rag reconciliation: refusing to delete published generation %s/%d", item.ObjectVersionID, item.Generation)
	}
	if err := store.DeleteGenerationPoints(ctx, item.QdrantCollection, item.ObjectVersionID, item.Generation); err != nil {
		return fmt.Errorf("delete orphan index generation: %w", err)
	}
	return nil
}
