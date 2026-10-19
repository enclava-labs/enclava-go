// Durable filesystem artifact store (D-10/CORE-03): streaming writes with
// byte limits, atomic finalization before metadata commit, tenant-isolated
// server-composed paths and crash reconciliation. Begin persists a 'writing'
// row with lease/fence BEFORE any bytes exist; Finalize verifies that same
// row/fence; a crash anywhere is repaired by Reconcile without data loss or
// exposure. No caller string ever becomes a filesystem path.
package artifacts

import (
	"context"
	"crypto/rand"
	"crypto/sha256"
	"encoding/binary"
	"encoding/hex"
	"errors"
	"fmt"
	"hash"
	"io"
	"os"
	"path/filepath"
	"strings"
	"time"
)

// Artifact lifecycle states — mirror the artifacts.state enum in migration 004.
const (
	StateWriting    = "writing"
	StateFinal      = "final"
	StateDeleting   = "deleting"
	StateTombstoned = "tombstoned"
)

// Tombstone reasons (tombstone_reason is required once state=tombstoned).
const (
	ReasonDeleted           = "deleted"
	ReasonWriteAborted      = "write_aborted"
	ReasonExpiredWriteLease = "expired_write_lease"
	ReasonMissingFinalBytes = "missing_final_bytes"
)

var (
	// ErrInvalidRef is returned when a Ref field cannot safely name an
	// artifact (empty, or contains '/', '\\', '..' or NUL).
	ErrInvalidRef = errors.New("artifacts: invalid reference")
	// ErrNotFound is returned when no usable artifact row exists — absent,
	// still writing, deleting or tombstoned rows all deny identically.
	ErrNotFound = errors.New("artifacts: not found")
	// ErrLeaseLost is returned when the writer's row was reclaimed (fenced or
	// otherwise transitioned) before Finalize could commit it.
	ErrLeaseLost = errors.New("artifacts: write lease lost")
	// ErrOverLimit is returned when a stream exceeds the Begin byte limit;
	// the partial bytes are already gone when it is returned.
	ErrOverLimit = errors.New("artifacts: byte limit exceeded")
	// ErrWriterClosed is returned by Write/Finalize after the writer closed.
	ErrWriterClosed = errors.New("artifacts: writer already closed")
	// ErrChecksum is returned by Finalize when caller-declared SHA-256 does
	// not match the streamed bytes.
	ErrChecksum = errors.New("artifacts: sha256 mismatch")
)

// ArtifactRow is the metadata row behind every artifact. The 'writing' row —
// including lease_owner, lease_expires_at and fence_token — is committed
// before the first byte is created, so an active writer is never an orphan.
type ArtifactRow struct {
	TenantID        string
	ID              string
	Kind            string
	Version         int
	SHA256          string
	ByteSize        int64
	MimeType        string
	State           string
	StoragePath     string // server-composed durable path, relative to root
	LeaseOwner      string
	LeaseExpiresAt  time.Time
	FenceToken      int64
	TombstoneReason string
}

// MetadataStore is the narrow persistence boundary for artifact rows. The
// PostgreSQL implementation is internal/store/postgres.ArtifactRepo; tests
// may substitute an in-memory fake for pure-filesystem cases.
type MetadataStore interface {
	// BeginInsert commits a new state=writing row carrying the lease and
	// fence token before any byte exists.
	BeginInsert(ctx context.Context, row ArtifactRow) error
	// GetArtifact returns the row for (tenantID, id), or ErrNotFound.
	GetArtifact(ctx context.Context, tenantID, id string) (ArtifactRow, error)
	// FinalizeCommit atomically flips state writing→final, recording the
	// observed hash/size, only while the row is still writing AND still
	// owned by (leaseOwner, fenceToken). Returns ErrLeaseLost otherwise.
	FinalizeCommit(ctx context.Context, tenantID, id, leaseOwner string, fenceToken int64, sha256 string, byteSize int64) error
	// UpdateState transitions the row to state with the given tombstone
	// reason; an already-tombstoned row keeps its first reason.
	UpdateState(ctx context.Context, tenantID, id, state, tombstoneReason string) error
	// DeleteRow removes the row entirely.
	DeleteRow(ctx context.Context, tenantID, id string) error
	// ListFinalPaths returns every row that ever carried durable bytes:
	// state final, deleting or tombstoned.
	ListFinalPaths(ctx context.Context) ([]ArtifactRow, error)
	// ListExpiredWriting returns state=writing rows whose lease expired
	// before now.
	ListExpiredWriting(ctx context.Context, now time.Time) ([]ArtifactRow, error)
	// InvalidateWriting atomically fences one writing row
	// (writing→tombstoned) only while (leaseOwner, fenceToken) still own it,
	// revoking the writer's FinalizeCommit before its bytes are reaped.
	// Returns false when the row changed underneath the caller.
	InvalidateWriting(ctx context.Context, tenantID, id, leaseOwner string, fenceToken int64, tombstoneReason string) (bool, error)
}

// Store is the durable artifact byte store. durableRoot holds tenant-scoped
// finalized files plus a private .tmp staging area; scratchRoot holds
// private per-tenant scratch directories. The two roots MUST be disjoint.
type Store struct {
	durableRoot string
	scratchRoot string
	tmpDir      string
	meta        MetadataStore
	owner       string // this process's lease-owner identity
}

// NewStore validates the roots (disjoint, created with private permissions)
// and returns a ready Store.
func NewStore(durableRoot, scratchRoot string, meta MetadataStore) (*Store, error) {
	if durableRoot == "" || scratchRoot == "" {
		return nil, fmt.Errorf("artifacts: durable and scratch roots are required")
	}
	if meta == nil {
		return nil, fmt.Errorf("artifacts: metadata store is required")
	}
	var err error
	if durableRoot, err = filepath.Abs(durableRoot); err != nil {
		return nil, fmt.Errorf("artifacts: durable root: %w", err)
	}
	if scratchRoot, err = filepath.Abs(scratchRoot); err != nil {
		return nil, fmt.Errorf("artifacts: scratch root: %w", err)
	}
	if durableRoot == scratchRoot ||
		strings.HasPrefix(scratchRoot, durableRoot+string(filepath.Separator)) ||
		strings.HasPrefix(durableRoot, scratchRoot+string(filepath.Separator)) {
		return nil, fmt.Errorf("artifacts: durable and scratch roots must be disjoint")
	}
	tmp := filepath.Join(durableRoot, ".tmp")
	for _, dir := range []string{durableRoot, tmp, scratchRoot} {
		if err := os.MkdirAll(dir, 0o700); err != nil {
			return nil, fmt.Errorf("artifacts: create %s: %w", dir, err)
		}
	}
	host, _ := os.Hostname()
	return &Store{
		durableRoot: durableRoot,
		scratchRoot: scratchRoot,
		tmpDir:      tmp,
		meta:        meta,
		owner:       fmt.Sprintf("%s-%d-%s", host, os.Getpid(), randHex(4)),
	}, nil
}

// Begin commits the state=writing metadata row (lease + fence) FIRST and only
// then opens a private .part temp file. No caller-supplied string becomes a
// path: the opaque ID is generated here and the tenant is validated before it
// joins the composed durable path. maxBytes bounds the stream; a leaseTTL
// zero-or-negative means an already-expired lease.
func (s *Store) Begin(ctx context.Context, meta Meta, leaseTTL time.Duration, maxBytes int64) (*Writer, Ref, error) {
	ref := meta.Ref
	if err := validateComponent(ref.TenantID); err != nil {
		return nil, Ref{}, fmt.Errorf("tenant: %w", err)
	}
	switch ref.Kind {
	case KindOriginal, KindResult, KindDerived:
	default:
		return nil, Ref{}, fmt.Errorf("kind %q: %w", ref.Kind, ErrInvalidRef)
	}
	if maxBytes <= 0 {
		return nil, Ref{}, fmt.Errorf("artifacts: byte limit must be positive")
	}

	id := "art_" + randHex(16)
	fence := newFenceToken()
	ref.ID = id

	row := ArtifactRow{
		TenantID:       ref.TenantID,
		ID:             id,
		Kind:           ref.Kind,
		Version:        ref.Version,
		SHA256:         ref.SHA256, // declared hash, if any; Finalize verifies
		ByteSize:       ref.ByteSize,
		MimeType:       meta.MimeType,
		State:          StateWriting,
		StoragePath:    storageRel(ref.TenantID, id),
		LeaseOwner:     s.owner,
		LeaseExpiresAt: time.Now().Add(leaseTTL),
		FenceToken:     fence,
	}
	if row.Version < 1 {
		row.Version = 1
	}
	// The writing row is committed before the first byte exists.
	if err := s.meta.BeginInsert(ctx, row); err != nil {
		return nil, Ref{}, fmt.Errorf("artifacts: begin insert: %w", err)
	}

	tmpPath := filepath.Join(s.tmpDir, id+".part")
	f, err := os.OpenFile(tmpPath, os.O_WRONLY|os.O_CREATE|os.O_EXCL, 0o600)
	if err != nil {
		return nil, Ref{}, fmt.Errorf("artifacts: create temp file: %w", err)
	}
	return &Writer{
		s:        s,
		ref:      ref,
		row:      row,
		f:        f,
		tmpPath:  tmpPath,
		durable:  filepath.Join(s.durableRoot, row.StoragePath),
		hasher:   sha256.New(),
		maxBytes: maxBytes,
	}, ref, nil
}

// Writer streams bytes to the private temp file, enforcing the byte limit
// mid-stream. Finalize atomically moves bytes into the durable tree and
// commits the row; Abort discards them and tombstones the writing row.
type Writer struct {
	s        *Store
	ref      Ref
	row      ArtifactRow
	f        *os.File
	tmpPath  string
	durable  string
	hasher   hash.Hash
	written  int64
	maxBytes int64
	done     bool
}

// Write implements io.Writer. Crossing the limit aborts the write: the temp
// file is removed and the row tombstoned before ErrOverLimit is returned.
func (w *Writer) Write(p []byte) (int, error) {
	if w.done {
		return 0, ErrWriterClosed
	}
	if w.written+int64(len(p)) > w.maxBytes {
		w.abort(context.Background())
		return 0, ErrOverLimit
	}
	n, err := w.f.Write(p)
	w.written += int64(n)
	w.hasher.Write(p[:n])
	if err != nil {
		w.abort(context.Background())
		return n, fmt.Errorf("artifacts: stream write: %w", err)
	}
	return n, nil
}

// Finalize verifies this lease/fence still owns the writing row, fsyncs the
// temp file, atomically renames it into the tenant's durable directory,
// fsyncs that directory, then commits state=final through the same fence. If
// the row was reclaimed meanwhile the just-renamed bytes are removed and
// ErrLeaseLost returned — a fenced writer can never complete.
func (w *Writer) Finalize(ctx context.Context) error {
	if w.done {
		return ErrWriterClosed
	}
	sum := hex.EncodeToString(w.hasher.Sum(nil))
	if want := w.row.SHA256; want != "" && !strings.EqualFold(want, sum) {
		w.abort(ctx)
		return ErrChecksum
	}

	// Ownership is checked before rename/fsync/commit: a reclaimed writer
	// cannot finalize.
	row, err := w.s.meta.GetArtifact(ctx, w.row.TenantID, w.row.ID)
	if err != nil {
		w.abort(ctx)
		return fmt.Errorf("artifacts: finalize verify: %w", err)
	}
	if row.State != StateWriting || row.LeaseOwner != w.row.LeaseOwner || row.FenceToken != w.row.FenceToken {
		w.abort(ctx)
		return ErrLeaseLost
	}

	if err := w.f.Sync(); err != nil {
		w.abort(ctx)
		return fmt.Errorf("artifacts: fsync temp: %w", err)
	}
	if err := w.f.Close(); err != nil {
		w.abort(ctx)
		return fmt.Errorf("artifacts: close temp: %w", err)
	}

	dir := filepath.Dir(w.durable)
	if err := os.MkdirAll(dir, 0o700); err != nil {
		w.abort(ctx)
		return fmt.Errorf("artifacts: tenant dir: %w", err)
	}
	if err := os.Rename(w.tmpPath, w.durable); err != nil {
		w.abort(ctx)
		return fmt.Errorf("artifacts: finalize rename: %w", err)
	}
	if err := fsyncDir(dir); err != nil {
		// The rename must be durable before the metadata commit records the
		// artifact as finalized; otherwise a crash could leave finalized
		// metadata pointing at bytes the filesystem never persisted.
		w.abort(ctx)
		return fmt.Errorf("artifacts: fsync tenant dir: %w", err)
	}

	if err := w.s.meta.FinalizeCommit(ctx, w.row.TenantID, w.row.ID,
		w.row.LeaseOwner, w.row.FenceToken, sum, w.written); err != nil {
		// The row was reclaimed between the ownership check and the commit:
		// remove the just-renamed bytes so nothing durable escapes the fence.
		_ = os.Remove(w.durable)
		w.abort(ctx)
		if errors.Is(err, ErrLeaseLost) {
			return ErrLeaseLost
		}
		return fmt.Errorf("artifacts: finalize commit: %w", err)
	}

	w.done = true
	w.ref.SHA256 = sum
	w.ref.ByteSize = w.written
	return nil
}

// Ref returns the artifact reference, including the observed SHA-256 and byte
// size once Finalize has committed.
func (w *Writer) Ref() Ref { return w.ref }

// Abort discards the in-flight bytes and tombstones the writing row so it
// cannot be finalized later. Safe to call after Finalize (no-op).
func (w *Writer) Abort(ctx context.Context) error {
	if w.done {
		return ErrWriterClosed
	}
	w.abort(ctx)
	return nil
}

// Close aborts the writer — an artifact writer that closes without Finalize
// never leaves durable bytes.
func (w *Writer) Close() error { return w.Abort(context.Background()) }

func (w *Writer) abort(ctx context.Context) {
	if w.done {
		return
	}
	w.done = true
	_ = w.f.Close()
	_ = os.Remove(w.tmpPath)
	_ = os.Remove(w.durable)
	_ = w.s.meta.UpdateState(ctx, w.row.TenantID, w.row.ID, StateTombstoned, ReasonWriteAborted)
}

// Read opens a finalized artifact for reading. The path is composed ONLY from
// validated Ref fields under the durable root — a Ref is never itself a path
// and caller permission checks stay with the caller. Non-final rows deny
// identically to absent rows.
func (s *Store) Read(ctx context.Context, ref Ref) (io.ReadCloser, error) {
	path, err := s.durablePath(ref.TenantID, ref.ID)
	if err != nil {
		return nil, err
	}
	row, err := s.meta.GetArtifact(ctx, ref.TenantID, ref.ID)
	if err != nil {
		return nil, err
	}
	if row.State != StateFinal {
		return nil, ErrNotFound
	}
	f, err := os.Open(path)
	if err != nil {
		if os.IsNotExist(err) {
			return nil, ErrNotFound
		}
		return nil, fmt.Errorf("artifacts: open: %w", err)
	}
	return f, nil
}

// Delete marks the row tombstoned first, then removes the durable file (and
// any leftover temp bytes). A file-removal failure leaves the tombstone for
// Reconcile to finish the cleanup.
func (s *Store) Delete(ctx context.Context, ref Ref) error {
	path, err := s.durablePath(ref.TenantID, ref.ID)
	if err != nil {
		return err
	}
	if _, err := s.meta.GetArtifact(ctx, ref.TenantID, ref.ID); err != nil {
		return err
	}
	if err := s.meta.UpdateState(ctx, ref.TenantID, ref.ID, StateTombstoned, ReasonDeleted); err != nil {
		return fmt.Errorf("artifacts: tombstone: %w", err)
	}
	_ = os.Remove(path)
	_ = os.Remove(s.tempPath(ref.ID))
	return nil
}

// ScratchDir creates a private 0700 directory under the tenant's scratch
// area. The returned cleanup removes it — callers use it after
// cancellation/failure so scratch stays bounded.
func (s *Store) ScratchDir(ctx context.Context, tenantID string) (string, func(), error) {
	if err := validateComponent(tenantID); err != nil {
		return "", nil, fmt.Errorf("tenant: %w", err)
	}
	base := filepath.Join(s.scratchRoot, tenantID)
	if err := os.MkdirAll(base, 0o700); err != nil {
		return "", nil, fmt.Errorf("artifacts: scratch root: %w", err)
	}
	dir, err := os.MkdirTemp(base, "scratch-")
	if err != nil {
		return "", nil, fmt.Errorf("artifacts: scratch dir: %w", err)
	}
	if err := os.Chmod(dir, 0o700); err != nil {
		_ = os.RemoveAll(dir)
		return "", nil, fmt.Errorf("artifacts: scratch permissions: %w", err)
	}
	return dir, func() { _ = os.RemoveAll(dir) }, nil
}

// durablePath composes the on-disk path from validated fields only and proves
// the result stays inside the durable root.
func (s *Store) durablePath(tenantID, id string) (string, error) {
	if err := validateComponent(tenantID); err != nil {
		return "", fmt.Errorf("tenant: %w", err)
	}
	if err := validateComponent(id); err != nil {
		return "", fmt.Errorf("id: %w", err)
	}
	resolved := filepath.Join(s.durableRoot, storageRel(tenantID, id))
	root := s.durableRoot + string(filepath.Separator)
	if !strings.HasPrefix(resolved+string(filepath.Separator), root) && resolved != s.durableRoot {
		return "", fmt.Errorf("artifacts: path escapes durable root: %w", ErrInvalidRef)
	}
	return resolved, nil
}

func (s *Store) tempPath(id string) string {
	return filepath.Join(s.tmpDir, id+".part")
}

// storageRel is the server-composed durable path relative to the root.
func storageRel(tenantID, id string) string {
	return tenantID + string(filepath.Separator) + id
}

// validateComponent rejects anything that could alter the composed path:
// empty strings, '.', '..', separators and NUL.
func validateComponent(s string) error {
	if s == "" || s == "." || strings.Contains(s, "..") ||
		strings.ContainsRune(s, '/') || strings.ContainsRune(s, '\\') ||
		strings.ContainsRune(s, 0) {
		return ErrInvalidRef
	}
	return nil
}

func randHex(n int) string {
	buf := make([]byte, n)
	_, _ = rand.Read(buf)
	return hex.EncodeToString(buf)
}

func newFenceToken() int64 {
	var buf [8]byte
	_, _ = rand.Read(buf[:])
	return int64(binary.LittleEndian.Uint64(buf[:]) & (1<<63 - 1))
}

func fsyncDir(dir string) error {
	d, err := os.Open(dir)
	if err != nil {
		return err
	}
	defer d.Close()
	return d.Sync()
}
