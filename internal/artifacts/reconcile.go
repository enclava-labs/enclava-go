// Startup crash/traversal reconciliation for the artifact store (D-10,
// CORE-03). Reconcile is cross-process safe: it only reaps a writer after the
// row's lease has expired AND the row has been atomically fenced, so a live
// writer in another process — including the rename-before-final-commit
// window — is never orphaned. Safe to run at every startup while another
// process writes; idempotent.
package artifacts

import (
	"context"
	"errors"
	"fmt"
	"io/fs"
	"os"
	"path/filepath"
	"strings"
	"time"
)

// Reconcile repairs the durable root and the metadata rows after a crash or
// alongside live writers:
//
//	(a) every state=writing row whose lease expired is FIRST atomically
//	    fenced (writing→tombstoned conditional on lease_owner/fence_token,
//	    which revokes that writer's FinalizeCommit) and only then its bytes
//	    are deleted; a writing row with a live lease is left untouched;
//	(b) durable files on disk are deleted only when no live writing row owns
//	    them: absent row, tombstoned/deleting row, or an expired-lease
//	    writing row (fenced in (a)); a live-lease writing row's file is kept;
//	(c) tombstoned/deleting rows get their files removed, and final rows
//	    whose file is missing are tombstoned with
//	    tombstone_reason=missing_final_bytes.
//
// Filesystem failures other than non-existence are collected and reported —
// a permission error is never treated as proof that bytes are missing.
func (s *Store) Reconcile(ctx context.Context) error {
	var errs []error
	now := time.Now()

	// (a) Fence expired writers, then reap their bytes. Only the process that
	// wins the conditional update may delete — losers leave the bytes for the
	// winner or a later pass.
	expired, err := s.meta.ListExpiredWriting(ctx, now)
	if err != nil {
		return fmt.Errorf("artifacts: list expired writes: %w", err)
	}
	for _, row := range expired {
		fenced, err := s.meta.InvalidateWriting(ctx, row.TenantID, row.ID,
			row.LeaseOwner, row.FenceToken, ReasonExpiredWriteLease)
		if err != nil {
			errs = append(errs, fmt.Errorf("fence writing %s/%s: %w", row.TenantID, row.ID, err))
			continue
		}
		if !fenced {
			continue // another reconciler owns this row now
		}
		s.removeOwned(&errs, row.TenantID, row.ID)
	}

	// (b) Traverse the durable tree. Every regular file at <tenant>/<id> is
	// checked against its row; anything not owned by a live write or a final
	// row is removed.
	err = filepath.WalkDir(s.durableRoot, func(path string, d fs.DirEntry, err error) error {
		if err != nil {
			errs = append(errs, fmt.Errorf("walk %s: %w", path, err))
			if d != nil && d.IsDir() {
				return fs.SkipDir
			}
			return nil
		}
		if path == s.durableRoot {
			return nil
		}
		rel, rerr := filepath.Rel(s.durableRoot, path)
		if rerr != nil {
			errs = append(errs, fmt.Errorf("walk rel %s: %w", path, rerr))
			return nil
		}
		if d.IsDir() {
			// The temp staging area is row-owned and handled via the rows;
			// scratch lives under a different root entirely.
			if rel == ".tmp" {
				return fs.SkipDir
			}
			return nil
		}
		parts := strings.Split(rel, string(filepath.Separator))
		if len(parts) != 2 {
			// Not an application-composed <tenant>/<id> file.
			if rerr := os.Remove(path); rerr != nil && !os.IsNotExist(rerr) {
				errs = append(errs, fmt.Errorf("remove stray %s: %w", path, rerr))
			}
			return nil
		}
		tenantID, id := parts[0], parts[1]
		row, rerr := s.rowForFile(ctx, tenantID, id)
		if rerr != nil {
			errs = append(errs, rerr)
			return nil
		}
		switch {
		case row == nil:
			// No row — an incomplete crash artifact or foreign file.
			removeFile(&errs, path)
		case row.State == StateFinal:
			// Durable and committed; leave it.
		case row.State == StateWriting && !row.LeaseExpiresAt.IsZero() && row.LeaseExpiresAt.After(now):
			// Live writer mid-flight (possibly rename-before-commit): keep.
		default:
			// Tombstoned/deleting rows and expired writers (fenced in (a)).
			removeFile(&errs, path)
		}
		return nil
	})
	if err != nil {
		errs = append(errs, fmt.Errorf("walk durable root: %w", err))
	}

	// (c) Row-driven repair for everything that ever carried durable bytes.
	rows, err := s.meta.ListFinalPaths(ctx)
	if err != nil {
		errs = append(errs, fmt.Errorf("list final paths: %w", err))
	} else {
		for _, row := range rows {
			path, perr := s.durablePath(row.TenantID, row.ID)
			if perr != nil {
				errs = append(errs, fmt.Errorf("row %s/%s path: %w", row.TenantID, row.ID, perr))
				continue
			}
			_, statErr := os.Stat(path)
			switch {
			case row.State == StateTombstoned || row.State == StateDeleting:
				if statErr == nil || (statErr != nil && !os.IsNotExist(statErr)) {
					removeFile(&errs, path)
				}
			case row.State == StateFinal:
				if statErr == nil {
					continue
				}
				if !os.IsNotExist(statErr) {
					// Unreadable/permissioned file is not proof it is missing.
					errs = append(errs, fmt.Errorf("stat final %s: %w", path, statErr))
					continue
				}
				if uerr := s.meta.UpdateState(ctx, row.TenantID, row.ID,
					StateTombstoned, ReasonMissingFinalBytes); uerr != nil {
					errs = append(errs, fmt.Errorf("tombstone missing final %s/%s: %w", row.TenantID, row.ID, uerr))
				}
			}
		}
	}

	return errors.Join(errs...)
}

// rowForFile resolves the row owning a traversed file. A nil row (and nil
// error) means no row exists — the file is safe to delete. Validation and
// lookup errors never mean "no row": they are returned so the file is left
// for the next pass instead of wrongly deleted.
func (s *Store) rowForFile(ctx context.Context, tenantID, id string) (*ArtifactRow, error) {
	if err := validateComponent(tenantID); err != nil {
		return nil, nil // cannot name a row — foreign file
	}
	if err := validateComponent(id); err != nil {
		return nil, nil
	}
	row, err := s.meta.GetArtifact(ctx, tenantID, id)
	if errors.Is(err, ErrNotFound) {
		return nil, nil
	}
	if err != nil {
		return nil, fmt.Errorf("lookup %s/%s: %w", tenantID, id, err)
	}
	return &row, nil
}

// removeOwned deletes both byte locations for a row this caller just fenced
// or finished: the durable file and the .part temp file.
func (s *Store) removeOwned(errs *[]error, tenantID, id string) {
	if path, err := s.durablePath(tenantID, id); err == nil {
		removeFile(errs, path)
	} else {
		*errs = append(*errs, err)
	}
	removeFile(errs, s.tempPath(id))
}

func removeFile(errs *[]error, path string) {
	if err := os.Remove(path); err != nil && !os.IsNotExist(err) {
		*errs = append(*errs, fmt.Errorf("remove %s: %w", path, err))
	}
}
