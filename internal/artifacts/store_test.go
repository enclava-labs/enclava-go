package artifacts

import (
	"context"
	"errors"
	"io"
	"os"
	"path/filepath"
	"sync"
	"testing"
	"time"
)

// fakeMeta is an in-memory MetadataStore honoring the same lease/fence
// semantics as the PostgreSQL repo: conditional transitions only.
type fakeMeta struct {
	mu   sync.Mutex
	rows map[string]ArtifactRow
}

func newFakeMeta() *fakeMeta { return &fakeMeta{rows: map[string]ArtifactRow{}} }

func rowKey(tenant, id string) string { return tenant + "\x00" + id }

func (f *fakeMeta) BeginInsert(_ context.Context, row ArtifactRow) error {
	f.mu.Lock()
	defer f.mu.Unlock()
	f.rows[rowKey(row.TenantID, row.ID)] = row
	return nil
}

func (f *fakeMeta) GetArtifact(_ context.Context, tenantID, id string) (ArtifactRow, error) {
	f.mu.Lock()
	defer f.mu.Unlock()
	row, ok := f.rows[rowKey(tenantID, id)]
	if !ok {
		return ArtifactRow{}, ErrNotFound
	}
	return row, nil
}

func (f *fakeMeta) FinalizeCommit(_ context.Context, tenantID, id, leaseOwner string, fenceToken int64, sha256 string, byteSize int64) error {
	f.mu.Lock()
	defer f.mu.Unlock()
	row, ok := f.rows[rowKey(tenantID, id)]
	if !ok || row.State != StateWriting ||
		row.LeaseOwner != leaseOwner || row.FenceToken != fenceToken {
		return ErrLeaseLost
	}
	row.State = StateFinal
	row.SHA256 = sha256
	row.ByteSize = byteSize
	f.rows[rowKey(tenantID, id)] = row
	return nil
}

func (f *fakeMeta) UpdateState(_ context.Context, tenantID, id, state, reason string) error {
	f.mu.Lock()
	defer f.mu.Unlock()
	row, ok := f.rows[rowKey(tenantID, id)]
	if !ok {
		return ErrNotFound
	}
	if row.State == StateTombstoned && state == StateTombstoned {
		return nil // first tombstone reason wins
	}
	row.State = state
	if reason != "" {
		row.TombstoneReason = reason
	}
	f.rows[rowKey(tenantID, id)] = row
	return nil
}

func (f *fakeMeta) DeleteRow(_ context.Context, tenantID, id string) error {
	f.mu.Lock()
	defer f.mu.Unlock()
	k := rowKey(tenantID, id)
	if _, ok := f.rows[k]; !ok {
		return ErrNotFound
	}
	delete(f.rows, k)
	return nil
}

func (f *fakeMeta) ListFinalPaths(_ context.Context) ([]ArtifactRow, error) {
	f.mu.Lock()
	defer f.mu.Unlock()
	var out []ArtifactRow
	for _, row := range f.rows {
		switch row.State {
		case StateFinal, StateDeleting, StateTombstoned:
			out = append(out, row)
		}
	}
	return out, nil
}

func (f *fakeMeta) ListExpiredWriting(_ context.Context, now time.Time) ([]ArtifactRow, error) {
	f.mu.Lock()
	defer f.mu.Unlock()
	var out []ArtifactRow
	for _, row := range f.rows {
		if row.State == StateWriting && !row.LeaseExpiresAt.IsZero() && row.LeaseExpiresAt.Before(now) {
			out = append(out, row)
		}
	}
	return out, nil
}

func (f *fakeMeta) InvalidateWriting(_ context.Context, tenantID, id, leaseOwner string, fenceToken int64, reason string) (bool, error) {
	f.mu.Lock()
	defer f.mu.Unlock()
	k := rowKey(tenantID, id)
	row, ok := f.rows[k]
	if !ok || row.State != StateWriting ||
		row.LeaseOwner != leaseOwner || row.FenceToken != fenceToken {
		return false, nil
	}
	row.State = StateTombstoned
	row.TombstoneReason = reason
	f.rows[k] = row
	return true, nil
}

// --- helpers ---------------------------------------------------------------

const (
	tenantA = "11111111-1111-1111-1111-111111111111"
	tenantB = "22222222-2222-2222-2222-222222222222"
)

func newTestStore(t *testing.T) (*Store, *fakeMeta, string) {
	t.Helper()
	durable := filepath.Join(t.TempDir(), "durable")
	scratch := filepath.Join(t.TempDir(), "scratch")
	meta := newFakeMeta()
	s, err := NewStore(durable, scratch, meta)
	if err != nil {
		t.Fatalf("NewStore: %v", err)
	}
	return s, meta, durable
}

func writeArtifact(t *testing.T, s *Store, tenant string, data []byte) Ref {
	t.Helper()
	ctx := context.Background()
	meta := Meta{Ref: Ref{TenantID: tenant, Kind: KindOriginal}}
	w, _, err := s.Begin(ctx, meta, time.Minute, 1<<20)
	if err != nil {
		t.Fatalf("Begin: %v", err)
	}
	if _, err := w.Write(data); err != nil {
		t.Fatalf("Write: %v", err)
	}
	if err := w.Finalize(ctx); err != nil {
		t.Fatalf("Finalize: %v", err)
	}
	return w.Ref()
}

func durableFile(s *Store, tenant, id string) string {
	return filepath.Join(s.durableRoot, tenant, id)
}

// --- tests -----------------------------------------------------------------

// TestBeginWritingRowBeforeBytes proves the writing row with lease+fence is
// persisted before any byte exists: at the moment Begin returns the .part
// file may exist but the row MUST already be committed.
func TestBeginWritingRowBeforeBytes(t *testing.T) {
	s, meta, durable := newTestStore(t)
	ctx := context.Background()

	w, ref, err := s.Begin(ctx, Meta{Ref: Ref{TenantID: tenantA, Kind: KindOriginal}},
		time.Minute, 1<<20)
	if err != nil {
		t.Fatalf("Begin: %v", err)
	}
	defer w.Abort(ctx)

	row, err := meta.GetArtifact(ctx, tenantA, ref.ID)
	if err != nil {
		t.Fatalf("writing row must exist before bytes: %v", err)
	}
	if row.State != StateWriting || row.LeaseOwner == "" || row.FenceToken == 0 ||
		row.LeaseExpiresAt.IsZero() {
		t.Fatalf("writing row missing lease/fence: %+v", row)
	}
	if _, err := os.Stat(durableFile(s, tenantA, ref.ID)); !os.IsNotExist(err) {
		t.Fatalf("no durable file may exist while writing: %v", err)
	}
	_ = durable
}

// TestWriteFinalizeRoundTrip: bytes stream to .part, finalize renames under
// the tenant dir, commit flips the row to final, Read serves the bytes.
func TestWriteFinalizeRoundTrip(t *testing.T) {
	s, meta, durable := newTestStore(t)
	ctx := context.Background()
	payload := []byte("durable original bytes")

	ref := writeArtifact(t, s, tenantA, payload)

	path := durableFile(s, tenantA, ref.ID)
	if _, err := os.Stat(filepath.Join(durable, ".tmp", ref.ID+".part")); !os.IsNotExist(err) {
		t.Fatalf(".part must be gone after finalize: %v", err)
	}
	got, err := os.ReadFile(path)
	if err != nil {
		t.Fatalf("durable file missing: %v", err)
	}
	if string(got) != string(payload) {
		t.Fatalf("durable bytes = %q, want %q", got, payload)
	}
	row, err := meta.GetArtifact(ctx, tenantA, ref.ID)
	if err != nil || row.State != StateFinal {
		t.Fatalf("row must be final: %+v err=%v", row, err)
	}
	if row.ByteSize != int64(len(payload)) || len(row.SHA256) != 64 {
		t.Fatalf("final row missing observed size/hash: %+v", row)
	}

	rc, err := s.Read(ctx, Ref{TenantID: tenantA, ID: ref.ID})
	if err != nil {
		t.Fatalf("Read: %v", err)
	}
	defer rc.Close()
	readBack, _ := io.ReadAll(rc)
	if string(readBack) != string(payload) {
		t.Fatalf("read bytes = %q, want %q", readBack, payload)
	}
}

// TestOverLimitLeavesNoFile asserts an over-limit Begin+Finalize stream
// leaves no file under the tenant dir and no committed row.
func TestOverLimitLeavesNoFile(t *testing.T) {
	s, meta, durable := newTestStore(t)
	ctx := context.Background()

	w, ref, err := s.Begin(ctx, Meta{Ref: Ref{TenantID: tenantA, Kind: KindResult}},
		time.Minute, 8)
	if err != nil {
		t.Fatalf("Begin: %v", err)
	}
	if _, err := w.Write([]byte("012345678")); !errors.Is(err, ErrOverLimit) {
		t.Fatalf("over-limit write must fail with ErrOverLimit, got %v", err)
	}
	if err := w.Finalize(ctx); !errors.Is(err, ErrWriterClosed) {
		t.Fatalf("finalize after abort must fail, got %v", err)
	}

	entries, err := os.ReadDir(filepath.Join(durable, tenantA))
	if err != nil && !os.IsNotExist(err) {
		t.Fatalf("read tenant dir: %v", err)
	}
	if len(entries) != 0 {
		t.Fatalf("tenant dir must be empty after over-limit abort, found %d entries", len(entries))
	}
	if _, err := os.Stat(filepath.Join(durable, ".tmp", ref.ID+".part")); !os.IsNotExist(err) {
		t.Fatalf("over-limit .part must be removed: %v", err)
	}
	if row, err := meta.GetArtifact(ctx, tenantA, ref.ID); err != nil || row.State != StateTombstoned {
		t.Fatalf("aborted write must be tombstoned, got %+v err=%v", row, err)
	}
}

// TestRefTraversalRejected: a Ref carrying '..', '/', '\\' or empty
// TenantID/ID is an error on Read — never a path.
func TestRefTraversalRejected(t *testing.T) {
	s, _, _ := newTestStore(t)
	ctx := context.Background()
	ref := writeArtifact(t, s, tenantA, []byte("x"))

	bad := []Ref{
		{TenantID: "../escape", ID: ref.ID},
		{TenantID: "a/b", ID: ref.ID},
		{TenantID: `a\b`, ID: ref.ID},
		{TenantID: "", ID: ref.ID},
		{TenantID: ".", ID: ref.ID},
		{TenantID: tenantA, ID: "../" + ref.ID},
		{TenantID: tenantA, ID: ".."},
		{TenantID: tenantA, ID: "x/y"},
		{TenantID: tenantA, ID: ""},
		{TenantID: tenantA, ID: "x\x00y"},
	}
	for i, r := range bad {
		rc, err := s.Read(ctx, r)
		if err == nil {
			rc.Close()
			t.Fatalf("case %d: traversal ref %+v must error", i, r)
		}
		if !errors.Is(err, ErrInvalidRef) {
			t.Fatalf("case %d: ref %+v got %v, want ErrInvalidRef", i, r, err)
		}
	}
	// Same ref, still readable.
	rc, err := s.Read(ctx, Ref{TenantID: tenantA, ID: ref.ID})
	if err != nil {
		t.Fatalf("valid ref must still read: %v", err)
	}
	rc.Close()
}

// TestCrossTenantDenied: tenant A cannot open an artifact addressed by
// tenant B's tenant field — the composed path differs, so the row and the
// bytes are both unreachable.
func TestCrossTenantDenied(t *testing.T) {
	s, _, _ := newTestStore(t)
	ctx := context.Background()
	ref := writeArtifact(t, s, tenantA, []byte("tenant-A-bytes"))

	rc, err := s.Read(ctx, Ref{TenantID: tenantB, ID: ref.ID})
	if err == nil {
		rc.Close()
		t.Fatalf("tenant B must not read tenant A's artifact")
	}
	if !errors.Is(err, ErrNotFound) {
		t.Fatalf("cross-tenant read got %v, want ErrNotFound", err)
	}
	// Guessing the artifact ID under another tenant also denies on Delete.
	if err := s.Delete(ctx, Ref{TenantID: tenantB, ID: ref.ID}); !errors.Is(err, ErrNotFound) {
		t.Fatalf("cross-tenant delete got %v, want ErrNotFound", err)
	}
	// And nothing under tenant B's dir was created.
	if _, err := os.Stat(durableFile(s, tenantB, ref.ID)); !os.IsNotExist(err) {
		t.Fatalf("cross-tenant access must not create/read bytes: %v", err)
	}
}

// TestDeleteTombstonesThenRemoves: Delete marks the row tombstoned and
// removes the bytes; reads deny afterwards.
func TestDeleteTombstonesThenRemoves(t *testing.T) {
	s, meta, _ := newTestStore(t)
	ctx := context.Background()
	ref := writeArtifact(t, s, tenantA, []byte("gone soon"))

	if err := s.Delete(ctx, Ref{TenantID: tenantA, ID: ref.ID}); err != nil {
		t.Fatalf("Delete: %v", err)
	}
	if _, err := os.Stat(durableFile(s, tenantA, ref.ID)); !os.IsNotExist(err) {
		t.Fatalf("deleted file must be gone: %v", err)
	}
	row, err := meta.GetArtifact(ctx, tenantA, ref.ID)
	if err != nil || row.State != StateTombstoned || row.TombstoneReason != ReasonDeleted {
		t.Fatalf("row must be tombstoned(deleted): %+v err=%v", row, err)
	}
	rc, err := s.Read(ctx, Ref{TenantID: tenantA, ID: ref.ID})
	if err == nil {
		rc.Close()
		t.Fatalf("read of tombstoned artifact must fail")
	}
}

// TestFinalizeAfterFence: if the writing row was fenced while the writer
// held it, Finalize returns ErrLeaseLost and leaves no durable bytes.
func TestFinalizeAfterFence(t *testing.T) {
	s, meta, _ := newTestStore(t)
	ctx := context.Background()

	w, ref, err := s.Begin(ctx, Meta{Ref: Ref{TenantID: tenantA, Kind: KindDerived}},
		time.Minute, 1<<20)
	if err != nil {
		t.Fatalf("Begin: %v", err)
	}
	if _, err := w.Write([]byte("partial")); err != nil {
		t.Fatalf("Write: %v", err)
	}
	row, _ := meta.GetArtifact(ctx, tenantA, ref.ID)
	fenced, err := meta.InvalidateWriting(ctx, tenantA, ref.ID, row.LeaseOwner, row.FenceToken, ReasonExpiredWriteLease)
	if err != nil || !fenced {
		t.Fatalf("fence: %v fenced=%v", err, fenced)
	}

	if err := w.Finalize(ctx); !errors.Is(err, ErrLeaseLost) {
		t.Fatalf("fenced writer finalize got %v, want ErrLeaseLost", err)
	}
	if _, err := os.Stat(durableFile(s, tenantA, ref.ID)); !os.IsNotExist(err) {
		t.Fatalf("fenced writer must not leave durable bytes: %v", err)
	}
}

// TestScratchDirLifecycle: scratch dirs are private and cleanup removes them.
func TestScratchDirLifecycle(t *testing.T) {
	s, _, _ := newTestStore(t)
	ctx := context.Background()

	dir, cleanup, err := s.ScratchDir(ctx, tenantA)
	if err != nil {
		t.Fatalf("ScratchDir: %v", err)
	}
	fi, err := os.Stat(dir)
	if err != nil || !fi.IsDir() {
		t.Fatalf("scratch dir missing: %v", err)
	}
	if fi.Mode().Perm() != 0o700 {
		t.Fatalf("scratch dir mode = %o, want 0700", fi.Mode().Perm())
	}
	if err := os.WriteFile(filepath.Join(dir, "work.bin"), []byte("tmp"), 0o600); err != nil {
		t.Fatalf("write scratch file: %v", err)
	}
	cleanup()
	if _, err := os.Stat(dir); !os.IsNotExist(err) {
		t.Fatalf("scratch cleanup must remove dir: %v", err)
	}
	if _, _, err := s.ScratchDir(ctx, "../bad"); !errors.Is(err, ErrInvalidRef) {
		t.Fatalf("scratch tenant traversal got %v, want ErrInvalidRef", err)
	}
}

// TestConcurrentBeginsDistinctIDs: concurrent writers get distinct opaque IDs
// and all finalize without collision.
func TestConcurrentBeginsDistinctIDs(t *testing.T) {
	s, _, _ := newTestStore(t)
	ctx := context.Background()

	const n = 8
	refs := make(chan Ref, n)
	errs := make(chan error, n)
	var wg sync.WaitGroup
	for i := range n {
		wg.Add(1)
		go func(i int) {
			defer wg.Done()
			w, ref, err := s.Begin(ctx, Meta{Ref: Ref{TenantID: tenantA, Kind: KindOriginal}},
				time.Minute, 1<<20)
			if err != nil {
				errs <- err
				return
			}
			if _, err := w.Write([]byte{byte(i)}); err != nil {
				errs <- err
				return
			}
			if err := w.Finalize(ctx); err != nil {
				errs <- err
				return
			}
			refs <- ref
		}(i)
	}
	wg.Wait()
	close(refs)
	close(errs)
	for err := range errs {
		t.Fatalf("concurrent writer: %v", err)
	}
	seen := map[string]bool{}
	for ref := range refs {
		if seen[ref.ID] {
			t.Fatalf("duplicate artifact id %s", ref.ID)
		}
		seen[ref.ID] = true
	}
	if len(seen) != n {
		t.Fatalf("finalized %d of %d artifacts", len(seen), n)
	}
}

// TestReconcilePureFS covers the reconciler paths that need no real DB:
// expired writers fenced, orphan durable files removed, tombstoned files
// removed, missing final bytes tombstoned, idempotency.
func TestReconcilePureFS(t *testing.T) {
	ctx := context.Background()

	t.Run("expired writer fenced and bytes reaped", func(t *testing.T) {
		s, meta, durable := newTestStore(t)
		// Already-expired lease: TTL negative.
		w, ref, err := s.Begin(ctx, Meta{Ref: Ref{TenantID: tenantA, Kind: KindOriginal}},
			-time.Second, 1<<20)
		if err != nil {
			t.Fatalf("Begin: %v", err)
		}
		if _, err := w.Write([]byte("orphan-bytes")); err != nil {
			t.Fatalf("Write: %v", err)
		}
		part := filepath.Join(durable, ".tmp", ref.ID+".part")
		if _, err := os.Stat(part); err != nil {
			t.Fatalf(".part must exist mid-write: %v", err)
		}

		if err := s.Reconcile(ctx); err != nil {
			t.Fatalf("Reconcile: %v", err)
		}
		row, _ := meta.GetArtifact(ctx, tenantA, ref.ID)
		if row.State != StateTombstoned || row.TombstoneReason != ReasonExpiredWriteLease {
			t.Fatalf("expired writer must be tombstoned: %+v", row)
		}
		if _, err := os.Stat(part); !os.IsNotExist(err) {
			t.Fatalf("expired writer .part must be removed: %v", err)
		}
		// The fenced writer can never finalize.
		if err := w.Finalize(ctx); !errors.Is(err, ErrLeaseLost) {
			t.Fatalf("fenced finalize got %v, want ErrLeaseLost", err)
		}
	})

	t.Run("live writer survives reconcile", func(t *testing.T) {
		s, meta, durable := newTestStore(t)
		w, ref, err := s.Begin(ctx, Meta{Ref: Ref{TenantID: tenantA, Kind: KindOriginal}},
			time.Hour, 1<<20)
		if err != nil {
			t.Fatalf("Begin: %v", err)
		}
		if _, err := w.Write([]byte("live")); err != nil {
			t.Fatalf("Write: %v", err)
		}
		if err := s.Reconcile(ctx); err != nil {
			t.Fatalf("Reconcile: %v", err)
		}
		row, _ := meta.GetArtifact(ctx, tenantA, ref.ID)
		if row.State != StateWriting {
			t.Fatalf("live writer row must survive: %+v", row)
		}
		if _, err := os.Stat(filepath.Join(durable, ".tmp", ref.ID+".part")); err != nil {
			t.Fatalf("live .part must survive: %v", err)
		}
		if err := w.Finalize(ctx); err != nil {
			t.Fatalf("live writer must still finalize: %v", err)
		}
	})

	t.Run("orphan durable file removed", func(t *testing.T) {
		s, _, durable := newTestStore(t)
		dir := filepath.Join(durable, tenantA)
		if err := os.MkdirAll(dir, 0o700); err != nil {
			t.Fatal(err)
		}
		orphan := filepath.Join(dir, "art_deadbeef")
		if err := os.WriteFile(orphan, []byte("crashed"), 0o600); err != nil {
			t.Fatal(err)
		}
		if err := s.Reconcile(ctx); err != nil {
			t.Fatalf("Reconcile: %v", err)
		}
		if _, err := os.Stat(orphan); !os.IsNotExist(err) {
			t.Fatalf("rowless durable file must be removed: %v", err)
		}
	})

	t.Run("tombstoned row file removed", func(t *testing.T) {
		s, _, _ := newTestStore(t)
		ref := writeArtifact(t, s, tenantA, []byte("data"))
		if err := s.meta.UpdateState(ctx, tenantA, ref.ID, StateTombstoned, ReasonDeleted); err != nil {
			t.Fatal(err)
		}
		// Simulate file-removal failure on the original Delete: bytes remain.
		if err := s.Reconcile(ctx); err != nil {
			t.Fatalf("Reconcile: %v", err)
		}
		if _, err := os.Stat(durableFile(s, tenantA, ref.ID)); !os.IsNotExist(err) {
			t.Fatalf("tombstoned row's file must be removed: %v", err)
		}
	})

	t.Run("missing final bytes tombstoned and idempotent", func(t *testing.T) {
		s, meta, _ := newTestStore(t)
		ref := writeArtifact(t, s, tenantA, []byte("final"))
		if err := os.Remove(durableFile(s, tenantA, ref.ID)); err != nil {
			t.Fatal(err)
		}
		if err := s.Reconcile(ctx); err != nil {
			t.Fatalf("Reconcile: %v", err)
		}
		row, _ := meta.GetArtifact(ctx, tenantA, ref.ID)
		if row.State != StateTombstoned || row.TombstoneReason != ReasonMissingFinalBytes {
			t.Fatalf("missing final must be tombstoned truthfully: %+v", row)
		}
		rc, err := s.Read(ctx, Ref{TenantID: tenantA, ID: ref.ID})
		if err == nil {
			rc.Close()
			t.Fatal("read of missing final must deny")
		}
		// Idempotent: second run is a clean no-op.
		if err := s.Reconcile(ctx); err != nil {
			t.Fatalf("second Reconcile must be a no-op: %v", err)
		}
		row2, _ := meta.GetArtifact(ctx, tenantA, ref.ID)
		if row2.State != row.State || row2.TombstoneReason != row.TombstoneReason {
			t.Fatalf("second reconcile changed row: %+v -> %+v", row, row2)
		}
	})
}
