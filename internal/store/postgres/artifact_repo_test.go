package postgres

import (
	"context"
	"errors"
	"io"
	"os"
	"path/filepath"
	"testing"
	"time"

	"enclava-go/internal/artifacts"
)

// Compile-time proof the concrete repository satisfies the artifact metadata
// boundary (also kept next to the implementation).
var _ artifacts.MetadataStore = (*ArtifactRepo)(nil)

// storePair returns two independent Store instances — the "two processes" of
// the barrier tests — sharing one durable root and one migrated schema on two
// separate connections, plus the DB handle for seeding.
func storePair(t *testing.T) (*artifacts.Store, *artifacts.Store, *DB, string) {
	t.Helper()
	db, scopedDSN := openIsolated(t)
	ctx := context.Background()
	db2, err := Open(ctx, scopedDSN)
	if err != nil {
		t.Fatalf("open second connection: %v", err)
	}
	t.Cleanup(func() { _ = db2.Close() })

	root := t.TempDir()
	durable := filepath.Join(root, "durable")
	scratch := filepath.Join(root, "scratch")

	sa, err := artifacts.NewStore(durable, scratch, NewArtifactRepo(db))
	if err != nil {
		t.Fatalf("NewStore A: %v", err)
	}
	sb, err := artifacts.NewStore(durable, scratch, NewArtifactRepo(db2))
	if err != nil {
		t.Fatalf("NewStore B: %v", err)
	}
	return sa, sb, db, durable
}

func beginOriginal(t *testing.T, s *artifacts.Store, tenant string, ttl time.Duration) (*artifacts.Writer, artifacts.Ref) {
	t.Helper()
	w, ref, err := s.Begin(context.Background(),
		artifacts.Meta{Ref: artifacts.Ref{TenantID: tenant, Kind: artifacts.KindOriginal}},
		ttl, 1<<20)
	if err != nil {
		t.Fatalf("Begin: %v", err)
	}
	return w, ref
}

// TestArtifactRepoRoundTrip: Begin persists the writing row with lease/fence
// before bytes; Finalize commits final with observed hash/size; the row is
// reachable tenant-scoped only.
func TestArtifactRepoRoundTrip(t *testing.T) {
	sa, _, db, durable := storePair(t)
	ctx := context.Background()
	seedTenant(t, ctx, db, idTenantA)
	seedTenant(t, ctx, db, idTenantB)

	w, ref := beginOriginal(t, sa, idTenantA, time.Minute)

	// Writing row committed before any byte exists.
	var state, leaseOwner string
	var fence int64
	if err := db.SQL.QueryRowContext(ctx, `
		SELECT state, lease_owner, fence_token FROM artifacts
		WHERE tenant_id = $1 AND id = $2`, idTenantA, ref.ID).
		Scan(&state, &leaseOwner, &fence); err != nil {
		t.Fatalf("writing row must exist before bytes: %v", err)
	}
	if state != "writing" || leaseOwner == "" || fence == 0 {
		t.Fatalf("writing row missing lease/fence: state=%s owner=%q fence=%d", state, leaseOwner, fence)
	}

	payload := []byte("postgres-backed durable bytes")
	if _, err := w.Write(payload); err != nil {
		t.Fatalf("Write: %v", err)
	}
	if err := w.Finalize(ctx); err != nil {
		t.Fatalf("Finalize: %v", err)
	}

	fref := w.Ref()
	if fref.ByteSize != int64(len(payload)) || len(fref.SHA256) != 64 {
		t.Fatalf("ref missing observed hash/size: %+v", fref)
	}
	var finalState string
	if err := db.SQL.QueryRowContext(ctx,
		`SELECT state FROM artifacts WHERE tenant_id=$1 AND id=$2`, idTenantA, ref.ID).
		Scan(&finalState); err != nil || finalState != "final" {
		t.Fatalf("row must be final: state=%q err=%v", finalState, err)
	}
	got, err := os.ReadFile(filepath.Join(durable, idTenantA, ref.ID))
	if err != nil || string(got) != string(payload) {
		t.Fatalf("durable bytes: %q err=%v", got, err)
	}

	// Cross-tenant: the same artifact ID under tenant B is ErrNotFound.
	repo := NewArtifactRepo(db)
	if _, err := repo.GetArtifact(ctx, idTenantB, ref.ID); !errors.Is(err, artifacts.ErrNotFound) {
		t.Fatalf("cross-tenant GetArtifact got %v, want ErrNotFound", err)
	}
	rc, err := sa.Read(ctx, artifacts.Ref{TenantID: idTenantB, ID: ref.ID})
	if err == nil {
		rc.Close()
		t.Fatal("cross-tenant read must deny")
	}
	rc, err = sa.Read(ctx, artifacts.Ref{TenantID: idTenantA, ID: ref.ID})
	if err != nil {
		t.Fatalf("read own artifact: %v", err)
	}
	back, _ := io.ReadAll(rc)
	rc.Close()
	if string(back) != string(payload) {
		t.Fatalf("read bytes = %q", back)
	}
}

// TestReconcileExpiredWriterFenced: an expired-lease writing row is fenced
// (conditional UPDATE wins) before its .part bytes are deleted; the fenced
// writer's late Finalize fails with ErrLeaseLost.
func TestReconcileExpiredWriterFenced(t *testing.T) {
	sa, sb, db, durable := storePair(t)
	ctx := context.Background()
	seedTenant(t, ctx, db, idTenantA)

	w, ref := beginOriginal(t, sa, idTenantA, 150*time.Millisecond)
	if _, err := w.Write([]byte("orphan-bytes")); err != nil {
		t.Fatalf("Write: %v", err)
	}
	part := filepath.Join(durable, ".tmp", ref.ID+".part")
	if _, err := os.Stat(part); err != nil {
		t.Fatalf(".part must exist mid-write: %v", err)
	}

	time.Sleep(250 * time.Millisecond) // lease expires

	if err := sb.Reconcile(ctx); err != nil {
		t.Fatalf("Reconcile: %v", err)
	}
	var state, reason string
	if err := db.SQL.QueryRowContext(ctx,
		`SELECT state, tombstone_reason FROM artifacts WHERE tenant_id=$1 AND id=$2`,
		idTenantA, ref.ID).Scan(&state, &reason); err != nil {
		t.Fatalf("read row: %v", err)
	}
	if state != "tombstoned" || reason != artifacts.ReasonExpiredWriteLease {
		t.Fatalf("expired writer must be tombstoned(expired_write_lease): %s/%s", state, reason)
	}
	if _, err := os.Stat(part); !os.IsNotExist(err) {
		t.Fatalf("expired writer .part must be deleted: %v", err)
	}
	if err := w.Finalize(ctx); !errors.Is(err, artifacts.ErrLeaseLost) {
		t.Fatalf("fenced writer finalize got %v, want ErrLeaseLost", err)
	}
}

// TestReconcileWriterVsReconcilerBarrier is the two-"process" case: process A
// holds a live-lease writing row mid-stream while process B runs Reconcile.
// A's bytes and row survive and A's Finalize commits.
func TestReconcileWriterVsReconcilerBarrier(t *testing.T) {
	sa, sb, db, durable := storePair(t)
	ctx := context.Background()
	seedTenant(t, ctx, db, idTenantA)

	w, ref := beginOriginal(t, sa, idTenantA, time.Hour)

	written := make(chan struct{})
	reconciled := make(chan error, 1)
	go func() {
		<-written // A mid-stream: .part exists, row live
		reconciled <- sb.Reconcile(ctx)
	}()

	if _, err := w.Write([]byte("live process A bytes")); err != nil {
		t.Fatalf("Write: %v", err)
	}
	close(written)
	if err := <-reconciled; err != nil {
		t.Fatalf("process B Reconcile: %v", err)
	}

	// Row still writing with its lease; .part intact.
	var state string
	if err := db.SQL.QueryRowContext(ctx,
		`SELECT state FROM artifacts WHERE tenant_id=$1 AND id=$2`, idTenantA, ref.ID).
		Scan(&state); err != nil || state != "writing" {
		t.Fatalf("live writer row must survive reconcile: state=%q err=%v", state, err)
	}
	if _, err := os.Stat(filepath.Join(durable, ".tmp", ref.ID+".part")); err != nil {
		t.Fatalf("live writer .part must survive: %v", err)
	}
	// And A can still finalize.
	if err := w.Finalize(ctx); err != nil {
		t.Fatalf("live writer must finalize after reconcile: %v", err)
	}
	if _, err := os.Stat(filepath.Join(durable, idTenantA, ref.ID)); err != nil {
		t.Fatalf("finalized durable file missing: %v", err)
	}
}

// TestReconcileCrashWindow covers the rename-before-commit crash window and
// the rowless-file repair: a durable file whose writing row has a LIVE lease
// is kept; once that row is fenced, the leftover durable bytes go too; a file
// with no row at all is deleted outright.
func TestReconcileCrashWindow(t *testing.T) {
	sa, sb, db, durable := storePair(t)
	ctx := context.Background()
	seedTenant(t, ctx, db, idTenantA)

	w, ref := beginOriginal(t, sa, idTenantA, time.Hour)
	payload := []byte("renamed-but-uncommitted")
	if _, err := w.Write(payload); err != nil {
		t.Fatalf("Write: %v", err)
	}

	// Simulate the crash window: .part renamed into the durable tree while
	// the row is still state=writing with a live lease.
	part := filepath.Join(durable, ".tmp", ref.ID+".part")
	final := filepath.Join(durable, idTenantA, ref.ID)
	if err := os.MkdirAll(filepath.Dir(final), 0o700); err != nil {
		t.Fatal(err)
	}
	if err := os.Rename(part, final); err != nil {
		t.Fatalf("simulate crash-window rename: %v", err)
	}

	// Reconcile while the lease is LIVE: the durable file must survive.
	if err := sb.Reconcile(ctx); err != nil {
		t.Fatalf("Reconcile live window: %v", err)
	}
	if _, err := os.Stat(final); err != nil {
		t.Fatalf("live-lease durable file was wrongly deleted: %v", err)
	}

	// Now expire and fence the row; next reconcile deletes the file.
	repo := NewArtifactRepo(db)
	row, err := repo.GetArtifact(ctx, idTenantA, ref.ID)
	if err != nil {
		t.Fatalf("get row: %v", err)
	}
	fenced, err := repo.InvalidateWriting(ctx, idTenantA, ref.ID,
		row.LeaseOwner, row.FenceToken, artifacts.ReasonExpiredWriteLease)
	if err != nil || !fenced {
		t.Fatalf("fence row: %v fenced=%v", err, fenced)
	}
	if err := sb.Reconcile(ctx); err != nil {
		t.Fatalf("Reconcile fenced: %v", err)
	}
	if _, err := os.Stat(final); !os.IsNotExist(err) {
		t.Fatalf("fenced writer's durable file must be removed: %v", err)
	}
	_ = w.Abort(ctx)

	// Rowless durable file (rename committed, row absent) → removed.
	tenantDir := filepath.Join(durable, idTenantA)
	orphan := filepath.Join(tenantDir, "art_rowless")
	if err := os.WriteFile(orphan, []byte("crash"), 0o600); err != nil {
		t.Fatal(err)
	}
	if err := sb.Reconcile(ctx); err != nil {
		t.Fatalf("Reconcile orphan: %v", err)
	}
	if _, err := os.Stat(orphan); !os.IsNotExist(err) {
		t.Fatalf("rowless durable file must be removed: %v", err)
	}
}

// TestReconcileMissingFinalBytes: a final row whose file is missing becomes
// tombstoned with tombstone_reason=missing_final_bytes; reads deny; a second
// Reconcile changes nothing (idempotent).
func TestReconcileMissingFinalBytes(t *testing.T) {
	sa, sb, db, durable := storePair(t)
	ctx := context.Background()
	seedTenant(t, ctx, db, idTenantA)

	w, ref := beginOriginal(t, sa, idTenantA, time.Minute)
	if _, err := w.Write([]byte("final bytes")); err != nil {
		t.Fatalf("Write: %v", err)
	}
	if err := w.Finalize(ctx); err != nil {
		t.Fatalf("Finalize: %v", err)
	}
	final := filepath.Join(durable, idTenantA, ref.ID)
	if err := os.Remove(final); err != nil {
		t.Fatal(err)
	}

	if err := sb.Reconcile(ctx); err != nil {
		t.Fatalf("Reconcile: %v", err)
	}
	var state, reason string
	var deletedAt *time.Time
	if err := db.SQL.QueryRowContext(ctx, `
		SELECT state, tombstone_reason, deleted_at FROM artifacts
		WHERE tenant_id=$1 AND id=$2`, idTenantA, ref.ID).
		Scan(&state, &reason, &deletedAt); err != nil {
		t.Fatalf("read row: %v", err)
	}
	if state != "tombstoned" || reason != artifacts.ReasonMissingFinalBytes || deletedAt == nil {
		t.Fatalf("missing final must be tombstoned truthfully: %s/%s deleted_at=%v",
			state, reason, deletedAt)
	}
	rc, err := sa.Read(ctx, artifacts.Ref{TenantID: idTenantA, ID: ref.ID})
	if err == nil {
		rc.Close()
		t.Fatal("read of missing final must deny")
	}
	// Idempotent second pass.
	if err := sb.Reconcile(ctx); err != nil {
		t.Fatalf("second Reconcile must be clean: %v", err)
	}
	var state2, reason2 string
	if err := db.SQL.QueryRowContext(ctx,
		`SELECT state, tombstone_reason FROM artifacts WHERE tenant_id=$1 AND id=$2`,
		idTenantA, ref.ID).Scan(&state2, &reason2); err != nil {
		t.Fatal(err)
	}
	if state2 != state || reason2 != reason {
		t.Fatalf("second reconcile changed row: %s/%s -> %s/%s", state, reason, state2, reason2)
	}
}

// TestReconcileTombstonedFileRemoved: tombstoned rows' files are deleted by
// Reconcile (covers a Delete whose file-removal step failed).
func TestReconcileTombstonedFileRemoved(t *testing.T) {
	sa, sb, db, durable := storePair(t)
	ctx := context.Background()
	seedTenant(t, ctx, db, idTenantA)

	w, ref := beginOriginal(t, sa, idTenantA, time.Minute)
	if _, err := w.Write([]byte("delete me")); err != nil {
		t.Fatalf("Write: %v", err)
	}
	if err := w.Finalize(ctx); err != nil {
		t.Fatalf("Finalize: %v", err)
	}
	repo := NewArtifactRepo(db)
	if err := repo.UpdateState(ctx, idTenantA, ref.ID,
		artifacts.StateTombstoned, artifacts.ReasonDeleted); err != nil {
		t.Fatalf("tombstone: %v", err)
	}
	final := filepath.Join(durable, idTenantA, ref.ID)
	if err := sb.Reconcile(ctx); err != nil {
		t.Fatalf("Reconcile: %v", err)
	}
	if _, err := os.Stat(final); !os.IsNotExist(err) {
		t.Fatalf("tombstoned row's file must be removed: %v", err)
	}
}
