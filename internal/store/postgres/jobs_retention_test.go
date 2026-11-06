package postgres

import (
	"context"
	"testing"
	"time"
)

// seedFinishedJob inserts one job plus one closed attempt, stamped at
// `finished`. Distinct from insertJob (queued, no attempt) because the purge
// predicate keys on status and COALESCE(finished_at, updated_at).
func seedFinishedJob(t *testing.T, ctx context.Context, db *DB, tenant, id, op, status, inputRef, generation string, finished time.Time) {
	t.Helper()
	policy := "{}"
	if generation != "" {
		policy = `{"generation":"` + generation + `"}`
	}
	if _, err := db.SQL.ExecContext(ctx, `
		INSERT INTO jobs (tenant_id, id, operation, status, idempotency_key,
		                  input_hash, input_ref, principal, submitter_key, policy,
		                  submitted_at, updated_at, finished_at)
		VALUES ($1, $2, $3, $4, $5, 'hash-' || $6::text, $7, '{}'::jsonb,
		        'sub-' || $6::text, $8::jsonb, $9, $9, $9)`,
		tenant, id, op, status, "key-"+id, id, nullString(inputRef), policy, finished); err != nil {
		t.Fatalf("seed job %s: %v", id, err)
	}
	if _, err := db.SQL.ExecContext(ctx, `
		INSERT INTO job_attempts (tenant_id, id, job_id, fence_token, status,
		                          worker_id, dispatched_at, finished_at)
		VALUES ($1, 'att-' || $2::text, $2, 1, 'succeeded', 'w1', $3, $3)`,
		tenant, id, finished); err != nil {
		t.Fatalf("seed attempt for %s: %v", id, err)
	}
}

func jobExists(t *testing.T, ctx context.Context, db *DB, tenant, id string) bool {
	t.Helper()
	var n int
	if err := db.SQL.QueryRowContext(ctx,
		`SELECT count(*) FROM jobs WHERE tenant_id = $1 AND id = $2`, tenant, id).Scan(&n); err != nil {
		t.Fatalf("count job %s: %v", id, err)
	}
	return n == 1
}

// TestPurgeFinishedJobs proves the retention sweep deletes only terminal jobs
// of the listed operations older than the cutoff: billing evidence (a
// reservations row, even one bound to the attempt alone), recent jobs,
// unlisted operations, non-terminal jobs and the failed/unknown job that
// still marks an unpublished generation as failed all survive.
func TestPurgeFinishedJobs(t *testing.T) {
	db := RequireTestDB(t)
	ctx := context.Background()
	seedTenant(t, ctx, db, idTenantA)
	repo := NewJobsRepo(db)

	before := time.Now().Add(-24 * time.Hour)
	old := time.Now().Add(-30 * 24 * time.Hour)
	recent := time.Now().Add(-time.Hour)

	// Deletable: old terminal jobs of the listed operations.
	seedFinishedJob(t, ctx, db, idTenantA, "j_ingest", "rag.ingest", "succeeded", "", "", old)
	seedFinishedJob(t, ctx, db, idTenantA, "j_embed", "rag.embed", "failed", "", "", old)

	// Kept: recent, unlisted operation, non-terminal.
	seedFinishedJob(t, ctx, db, idTenantA, "j_recent", "rag.ingest", "succeeded", "", "", recent)
	seedFinishedJob(t, ctx, db, idTenantA, "j_other", "extract.process", "succeeded", "", "", old)
	seedFinishedJob(t, ctx, db, idTenantA, "j_queued", "rag.ingest", "queued", "", "", old)
	seedFinishedJob(t, ctx, db, idTenantA, "j_running", "rag.ingest", "running", "", "", old)

	// Kept: a reservation bound only to the attempt still counts as billing
	// evidence — the job and its attempt stay.
	seedFinishedJob(t, ctx, db, idTenantA, "j_billed", "rag.embed", "succeeded", "", "", old)
	if _, err := db.SQL.ExecContext(ctx, `
		INSERT INTO reservations (tenant_id, id, attempt_id, subject_type, subject_id,
		                          price_snapshot, reserved_units, limit_amount_micros,
		                          currency, state)
		VALUES ($1, 'res_billed', 'att-j_billed', 'endpoint', 'ep_1',
		        '{}'::jsonb, '{}'::jsonb, 100, 'usd', 'settled')`, idTenantA); err != nil {
		t.Fatalf("insert reservation: %v", err)
	}

	// Kept: a failed rag.embed whose generation never published is what shows
	// the document as failed; a published twin is deletable.
	seedSourceVersion(t, ctx, db, idTenantA, "conn_stg", "obj_stg", "ver_stg", 1)
	seedSourceVersion(t, ctx, db, idTenantA, "conn_pub", "obj_pub", "ver_pub", 1)
	if _, err := db.SQL.ExecContext(ctx, `
		INSERT INTO knowledge_collections (tenant_id, id, name, qdrant_collection)
		VALUES ($1, 'col_ret', 'docs', 'qc_ret')`, idTenantA); err != nil {
		t.Fatalf("insert collection: %v", err)
	}
	for _, row := range []struct{ gen, ver, state string }{
		{"gen_stg", "ver_stg", "staging"},
		{"gen_pub", "ver_pub", "published"},
	} {
		if _, err := db.SQL.ExecContext(ctx, `
			INSERT INTO index_generations (tenant_id, id, collection_id,
			                               object_version_id, generation, state)
			VALUES ($1, $2, 'col_ret', $3, 1, $4)`,
			idTenantA, row.gen, row.ver, row.state); err != nil {
			t.Fatalf("insert generation %s: %v", row.gen, err)
		}
	}
	seedFinishedJob(t, ctx, db, idTenantA, "j_staging", "rag.embed", "failed", "ver_stg", "1", old)
	seedFinishedJob(t, ctx, db, idTenantA, "j_pub", "rag.embed", "failed", "ver_pub", "1", old)

	deleted, err := repo.PurgeFinishedJobs(ctx, before, []string{"rag.ingest", "rag.embed"}, 100)
	if err != nil {
		t.Fatalf("PurgeFinishedJobs: %v", err)
	}
	if deleted != 3 {
		t.Fatalf("purged %d jobs, want 3 (j_ingest, j_embed, j_pub)", deleted)
	}

	for id, want := range map[string]bool{
		"j_ingest":  false,
		"j_embed":   false,
		"j_pub":     false,
		"j_recent":  true,
		"j_other":   true,
		"j_queued":  true,
		"j_running": true,
		"j_billed":  true,
		"j_staging": true,
	} {
		if got := jobExists(t, ctx, db, idTenantA, id); got != want {
			t.Fatalf("job %s present=%v, want %v", id, got, want)
		}
	}

	// Attempts die in the same statement as their jobs; the billed attempt
	// survives with its job.
	var attempts int
	if err := db.SQL.QueryRowContext(ctx, `
		SELECT count(*) FROM job_attempts
		WHERE tenant_id = $1 AND job_id IN ('j_ingest', 'j_embed', 'j_pub')`,
		idTenantA).Scan(&attempts); err != nil {
		t.Fatalf("count deleted jobs' attempts: %v", err)
	}
	if attempts != 0 {
		t.Fatalf("%d attempts survived their jobs", attempts)
	}
	if err := db.SQL.QueryRowContext(ctx, `
		SELECT count(*) FROM job_attempts
		WHERE tenant_id = $1 AND job_id = 'j_billed'`,
		idTenantA).Scan(&attempts); err != nil {
		t.Fatalf("count billed attempt: %v", err)
	}
	if attempts != 1 {
		t.Fatalf("billed job's attempt missing")
	}
}

// TestPurgeFinishedJobsLimit proves one call deletes at most `limit` jobs so
// the sweeper's next batch handles the rest.
func TestPurgeFinishedJobsLimit(t *testing.T) {
	db := RequireTestDB(t)
	ctx := context.Background()
	seedTenant(t, ctx, db, idTenantA)
	repo := NewJobsRepo(db)

	before := time.Now().Add(-24 * time.Hour)
	old := time.Now().Add(-30 * 24 * time.Hour)
	for _, id := range []string{"j_l1", "j_l2", "j_l3"} {
		seedFinishedJob(t, ctx, db, idTenantA, id, "rag.ingest", "succeeded", "", "", old)
	}

	deleted, err := repo.PurgeFinishedJobs(ctx, before, []string{"rag.ingest"}, 2)
	if err != nil {
		t.Fatalf("PurgeFinishedJobs: %v", err)
	}
	if deleted != 2 {
		t.Fatalf("first batch deleted %d, want 2", deleted)
	}
	deleted, err = repo.PurgeFinishedJobs(ctx, before, []string{"rag.ingest"}, 2)
	if err != nil {
		t.Fatalf("PurgeFinishedJobs second batch: %v", err)
	}
	if deleted != 1 {
		t.Fatalf("second batch deleted %d, want 1", deleted)
	}
	if jobExists(t, ctx, db, idTenantA, "j_l1") || jobExists(t, ctx, db, idTenantA, "j_l2") || jobExists(t, ctx, db, idTenantA, "j_l3") {
		t.Fatalf("limited purge left jobs behind")
	}
}
