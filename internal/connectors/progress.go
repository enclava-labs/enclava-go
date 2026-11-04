// progress.go — per-source indexing progress: one aggregate query over the
// target collection plus one jobs scan for the still-listing content stream.
package connectors

import (
	"context"
	"strings"

	"enclava-go/internal/auth"
	"enclava-go/internal/source"
)

// ProgressRepository aggregates one connection's indexing state. Optional —
// *postgres.SourceRepo implements it; stores that do not leave progress
// reporting off rather than blocking reads.
type ProgressRepository interface {
	SourceIndexProgress(ctx context.Context, tenantID, connectionID, collectionID string) (source.IndexProgress, error)
}

// SourceProgress is the service-level projection: the aggregate counts plus
// whether the content stream is still listing items.
type SourceProgress struct {
	source.IndexProgress
	Fetching bool
}

// indexProgressJobScan bounds the sync-jobs scan for an in-flight content
// stream: a submission enqueues one job per stream, newest first.
const indexProgressJobScan = 10

// IndexProgress reports what a source's sync produced and what remains.
// Authorization mirrors GetStreamState: connectors.manage + tenant.
func (s *Service) IndexProgress(ctx context.Context, p auth.Principal, sourceID string) (SourceProgress, error) {
	if !auth.HasScope(p, "connectors.manage") {
		return SourceProgress{}, source.ErrDenied
	}
	if err := requireTenant(p); err != nil {
		return SourceProgress{}, err
	}
	conn, err := s.repo.GetConnection(ctx, p.TenantID, strings.TrimSpace(sourceID))
	if err != nil {
		return SourceProgress{}, err
	}
	out := SourceProgress{}
	if repo, ok := s.repo.(ProgressRepository); ok && strings.TrimSpace(conn.TargetCollectionID) != "" {
		out.IndexProgress, err = repo.SourceIndexProgress(ctx, p.TenantID, conn.ID, conn.TargetCollectionID)
		if err != nil {
			return SourceProgress{}, err
		}
	}
	if s.jobRows == nil {
		return out, nil
	}
	rows, err := s.jobRows.ListConnectionJobs(ctx, p.TenantID, conn.ID, OperationSync, "", indexProgressJobScan)
	if err != nil {
		return SourceProgress{}, err
	}
	for _, row := range rows {
		if row.Policy["stream"] != string(StreamContent) {
			continue
		}
		if row.Status == JobStatusQueued || row.Status == JobStatusRunning {
			out.Fetching = true
			break
		}
	}
	return out, nil
}
