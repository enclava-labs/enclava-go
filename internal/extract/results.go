package extract

import (
	"context"
	"enclava-go/internal/artifacts"
	"enclava-go/internal/auth"
	"enclava-go/internal/jobs"
	"enclava-go/internal/source"
	"encoding/json"
	"errors"
	"fmt"
	"io"
	"mime"
	"path/filepath"
)

var ErrDeletePending = errors.New("extract deletion pending cancellation")

func (s *Service) CancelJob(ctx context.Context, p auth.Principal, id string) error {
	row, err := s.jobRows.GetOwnedJob(ctx, p.TenantID, id, jobs.SubmitterKey(p))
	if err != nil || (row.Operation != OperationProcess && row.Operation != OperationVision) {
		return ErrNotFound
	}
	if err := s.authorizeSource(ctx, p, row); err != nil {
		return ErrNotFound
	}
	if err := s.jobs.Cancel(ctx, p, id); err != nil {
		return mapNotFound(err)
	}
	return nil
}

func (s *Service) Original(ctx context.Context, p auth.Principal, id string) (io.ReadCloser, string, string, error) {
	row, err := s.jobRows.GetOwnedJob(ctx, p.TenantID, id, jobs.SubmitterKey(p))
	if err != nil || (row.Operation != OperationProcess && row.Operation != OperationVision) {
		return nil, "", "", ErrNotFound
	}
	if err := s.authorizeSource(ctx, p, row); err != nil {
		return nil, "", "", ErrNotFound
	}
	if s.artifacts == nil {
		return nil, "", "", errors.New("extract artifact store unavailable")
	}
	filename := filepath.Base(stringValue(row.Policy["file_name"]))
	mimeType := stringValue(row.Policy["mime_type"])
	if _, _, err := mime.ParseMediaType(mimeType); err != nil {
		return nil, "", "", errors.New("extract original MIME metadata invalid")
	}
	if filename == "." || filename == "" || mimeType == "" {
		return nil, "", "", errors.New("extract original metadata unavailable")
	}
	reader, err := s.artifacts.Read(ctx, artifacts.Ref{TenantID: p.TenantID, ID: row.InputRef, Kind: artifacts.KindOriginal, Version: intFromPolicy(row.Policy["input_version"])})
	if err != nil {
		return nil, "", "", fmt.Errorf("read extract original artifact: %w", err)
	}
	return reader, filename, mimeType, nil
}

func (s *Service) authorizeSource(ctx context.Context, p auth.Principal, row jobs.JobRow) error {
	raw, ok := row.Policy["object_ref"]
	if !ok {
		return nil
	}
	if s.sources == nil || s.authorizer == nil {
		return source.ErrDenied
	}
	encoded, err := json.Marshal(raw)
	if err != nil {
		return source.ErrDenied
	}
	var ref source.ObjectRef
	if err := json.Unmarshal(encoded, &ref); err != nil || ref.TenantID != p.TenantID || ref.ObjectID == "" || ref.ObservedSeq < 1 {
		return source.ErrDenied
	}
	object, err := s.sources.GetObject(ctx, p.TenantID, ref.ObjectID)
	if err != nil {
		return source.ErrDenied
	}
	version, err := s.sources.GetObjectVersion(ctx, p.TenantID, ref.ObjectID, ref.ObservedSeq)
	if err != nil || version.ArtifactRef != row.InputRef {
		return source.ErrDenied
	}
	return s.authorizer.CanReadVersion(ctx, p, object, version, source.ReadOptions{})
}

// DeleteJob fences the worker before removing application-owned artifacts.
// Non-terminal jobs remain intact until a later DELETE observes cancellation.
func (s *Service) DeleteJob(ctx context.Context, p auth.Principal, id string) error {
	row, err := s.jobRows.GetOwnedJob(ctx, p.TenantID, id, jobs.SubmitterKey(p))
	if err != nil {
		return mapNotFound(err)
	}
	if row.Operation != OperationProcess && row.Operation != OperationVision {
		return ErrNotFound
	}
	if err := s.authorizeSource(ctx, p, row); err != nil {
		return ErrNotFound
	}
	switch row.Status {
	case jobs.StatusQueued, jobs.StatusRunning:
		if err := s.jobs.Cancel(ctx, p, id); err != nil {
			return mapNotFound(err)
		}
		return ErrDeletePending
	case jobs.StatusDeleted:
		return nil
	}
	if err := s.jobs.Delete(ctx, p, id); err != nil {
		return mapNotFound(err)
	}
	return s.cleanupArtifacts(ctx, row)
}

// ReconcileDeletedJobs resumes only artifact cleanup already authorized by
// an explicit DELETE; it does not apply a retention policy.
func (s *Service) ReconcileDeletedJobs(ctx context.Context) error {
	rows, err := s.jobRows.ListDeletedExtractJobs(ctx)
	if err != nil {
		return err
	}
	for _, row := range rows {
		if err := s.cleanupArtifacts(ctx, row); err != nil {
			return err
		}
	}
	return nil
}

func (s *Service) cleanupArtifacts(ctx context.Context, row jobs.JobRow) error {
	if s.artifacts == nil {
		return errors.New("extract artifact store unavailable")
	}
	seen := map[string]struct{}{}
	for _, ref := range []string{row.InputRef, row.ResultRef} {
		if ref == "" {
			continue
		}
		if _, duplicate := seen[ref]; duplicate {
			continue
		}
		seen[ref] = struct{}{}
		count, err := s.jobRows.CountLiveArtifactReferences(ctx, row.TenantID, ref, row.ID)
		if err != nil {
			return fmt.Errorf("count extract artifact references: %w", err)
		}
		if count != 0 {
			continue
		}
		if err := s.artifacts.Delete(ctx, artifacts.Ref{TenantID: row.TenantID, ID: ref}); err != nil {
			return fmt.Errorf("delete extract artifact: %w", err)
		}
	}
	return nil
}
