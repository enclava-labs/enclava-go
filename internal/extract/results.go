package extract

import (
	"context"
	"errors"
	"fmt"

	"enclava-go/internal/artifacts"
	"enclava-go/internal/auth"
	"enclava-go/internal/jobs"
)

var ErrDeletePending = errors.New("extract deletion pending cancellation")

// CancelJob requests worker cancellation without requiring inference admission.
func (s *Service) CancelJob(ctx context.Context, p auth.Principal, id string) error {
	if _, err := s.jobRows.GetOwnedJob(ctx, p.TenantID, id, jobs.SubmitterKey(p)); err != nil {
		return mapNotFound(err)
	}
	if err := s.jobs.Cancel(ctx, p, id); err != nil {
		return mapNotFound(err)
	}
	return nil
}

// DeleteJob fences the worker before removing application-owned artifacts.
// Non-terminal jobs remain intact until a later DELETE observes cancellation.
func (s *Service) DeleteJob(ctx context.Context, p auth.Principal, id string) error {
	row, err := s.jobRows.GetOwnedJob(ctx, p.TenantID, id, jobs.SubmitterKey(p))
	if err != nil {
		return mapNotFound(err)
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
	if s.artifacts == nil {
		return errors.New("extract artifact store unavailable")
	}
	for _, ref := range []string{row.InputRef, row.ResultRef} {
		if ref == "" || ref == row.InputRef && ref == row.ResultRef {
			continue
		}
		if err := s.artifacts.Delete(ctx, artifacts.Ref{TenantID: p.TenantID, ID: ref}); err != nil {
			return fmt.Errorf("delete extract artifact: %w", err)
		}
	}
	return nil
}
