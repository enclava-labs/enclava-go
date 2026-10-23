// Package-level access projection runs beside fenced generation publication.
package rag

import (
	"context"
	"encoding/json"
	"errors"
	"fmt"

	"enclava-go/internal/jobs"
	"enclava-go/internal/source"
)

// AccessJobHandler projects completed source-access revisions onto vectors.
// Authorization still consults the canonical source repository at read time.
func (s *Service) AccessJobHandler() jobs.HandlerFunc {
	return func(ctx context.Context, claim *jobs.Claim, _ jobs.InputFunc) (string, error) {
		job := claim.Job()
		connectionID, _ := job.Policy["connection_id"].(string)
		kind, _ := job.Policy["snapshot_kind"].(string)
		revision, ok := integerPolicy(job.Policy["revision"])
		if connectionID == "" || connectionID != job.Subject.ConnectionID || revision <= 0 || !ok || job.InputRef != connectionID || (kind != source.SnapshotGrants && kind != source.SnapshotMembership) {
			return "", errors.New("rag access: invalid snapshot reference")
		}
		if job.TenantID == "" || job.Subject.ObservedRevision != revision {
			return "", source.ErrDenied
		}
		if s.vectorStore == nil {
			return "", errors.New("rag access: vector store is not configured")
		}
		if err := claim.MarkDispatched(ctx); err != nil {
			return "", err
		}
		latest, _, err := s.repo.LatestCompleteRevision(ctx, job.TenantID, connectionID, kind)
		if err != nil {
			return "", err
		}
		if latest != revision {
			return "stale access revision", nil
		}
		connection, err := s.repo.GetConnection(ctx, job.TenantID, connectionID)
		if err != nil {
			return "", err
		}
		generations, err := s.repo.ListGenerationsForConnection(ctx, job.TenantID, connectionID)
		if err != nil {
			return "", err
		}
		for _, generation := range generations {
			if generation.Tombstoned || connection.Status != "active" || generation.State == "superseded" {
				if err := s.vectorStore.DeleteGenerationPoints(ctx, generation.QdrantCollection, generation.ObjectVersionID, generation.Generation); err != nil {
					return "", fmt.Errorf("clean inaccessible generation: %w", err)
				}
				continue
			}
			filter := map[string]any{"must": []map[string]any{
				{"key": "object_version_id", "match": map[string]any{"value": generation.ObjectVersionID}},
				{"key": "generation", "match": map[string]any{"value": generation.Generation}},
			}}
			revisionField := "grants_revision"
			if kind == source.SnapshotMembership {
				revisionField = "membership_revision"
			}
			if err := s.vectorStore.SetPayload(ctx, generation.QdrantCollection, filter, map[string]any{revisionField: revision, "access_revision": revision, "access_enabled": true}); err != nil {
				return "", fmt.Errorf("update access revision: %w", err)
			}
		}
		return fmt.Sprintf("%s:%d", connectionID, revision), nil
	}
}

func integerPolicy(value any) (int64, bool) {
	switch n := value.(type) {
	case int64:
		return n, true
	case int:
		return int64(n), true
	case float64:
		return int64(n), n == float64(int64(n))
	case json.Number:
		v, err := n.Int64()
		return v, err == nil
	default:
		return 0, false
	}
}
