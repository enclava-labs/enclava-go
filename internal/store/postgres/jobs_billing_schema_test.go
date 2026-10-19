package postgres

import (
	"context"
	"database/sql"
	"encoding/json"
	"strings"
	"testing"
)

// Schema-behavior tests for migrations 004 (artifacts/jobs), 005 (billing)
// and 006 (domain). Plain SQL only — repositories land in later plans.
// RequireTestDB supplies a freshly migrated, schema-isolated database or
// fails the test hard.

func insertJob(t *testing.T, ctx context.Context, db *DB, tenant, id, op, key string) error {
	t.Helper()
	_, err := db.SQL.ExecContext(ctx, `
		INSERT INTO jobs (tenant_id, id, operation, status, idempotency_key,
		                  input_hash, principal, submitter_key)
		VALUES ($1, $2, $3, 'queued', $4, 'hash-' || $5::text, '{"actor_id":"u1"}', 'sub-' || $5::text)`,
		tenant, id, op, key, id)
	return err
}

func isUniqueViolation(err error) bool {
	return err != nil && strings.Contains(err.Error(), "SQLSTATE 23505")
}

// TestJobIdempotencyConflict proves UNIQUE(tenant_id,operation,
// idempotency_key): the same key+operation under one tenant is rejected at
// the database (the service compares inputs and returns
// ErrIdempotencyConflict), while the same operation+key under a different
// tenant coexists.
func TestJobIdempotencyConflict(t *testing.T) {
	db := RequireTestDB(t)
	ctx := context.Background()

	seedTenant(t, ctx, db, idTenantA)
	seedTenant(t, ctx, db, idTenantB)

	if err := insertJob(t, ctx, db, idTenantA, "job_1", "extract.run", "key-1"); err != nil {
		t.Fatalf("insert first job: %v", err)
	}
	// Same tenant, operation and key -> unique violation, regardless of
	// whether the inputs matched.
	if err := insertJob(t, ctx, db, idTenantA, "job_2", "extract.run", "key-1"); !isUniqueViolation(err) {
		t.Fatalf("expected unique violation for replayed idempotency key, got %v", err)
	}
	// Same operation+key under another tenant is a different identity.
	if err := insertJob(t, ctx, db, idTenantB, "job_3", "extract.run", "key-1"); err != nil {
		t.Fatalf("same key under tenant B must coexist: %v", err)
	}
	// Same key, different operation, same tenant also coexists.
	if err := insertJob(t, ctx, db, idTenantA, "job_4", "ingest.sync", "key-1"); err != nil {
		t.Fatalf("same key under different operation must coexist: %v", err)
	}
}

// TestReservationSnapshotRoundTrip proves a reservations row persists an
// arbitrary JSONB price_snapshot verbatim, the reserved per-kind unit
// quantities, and the bound currency — all reconstructible from a SECOND
// connection (cross-process liability reconstruction).
func TestReservationSnapshotRoundTrip(t *testing.T) {
	db, scopedDSN := openIsolated(t)
	ctx := context.Background()

	seedTenant(t, ctx, db, idTenantA)
	if err := insertJob(t, ctx, db, idTenantA, "job_res", "chat.completion", "key-res"); err != nil {
		t.Fatalf("insert job: %v", err)
	}

	snapshot := `{"rates":[{"unit":"input","rate_micros":2500,"unit_quantity":1000},` +
		`{"unit":"output","rate_micros":1,"unit_quantity":5000}],"currency":"usd","rounding_rule":"ceil"}`
	units := `{"input":1000,"output":200}`

	if _, err := db.SQL.ExecContext(ctx, `
		INSERT INTO reservations (tenant_id, id, job_id, subject_type, subject_id,
		                          price_snapshot, reserved_units, limit_amount_micros,
		                          currency, state)
		VALUES ($1, 'res_1', 'job_res', 'api_key', 'key_9', $2::jsonb, $3::jsonb,
		        5000000, 'usd', 'held')`,
		idTenantA, snapshot, units); err != nil {
		t.Fatalf("insert reservation: %v", err)
	}

	// A second connection to the same schema must see the identical data.
	other, err := sql.Open("pgx", scopedDSN)
	if err != nil {
		t.Fatalf("open second connection: %v", err)
	}
	defer other.Close()

	var snapRaw, unitsRaw, currency string
	var limit int64
	if err := other.QueryRowContext(ctx, `
		SELECT price_snapshot::text, reserved_units::text, currency, limit_amount_micros
		FROM reservations WHERE tenant_id = $1 AND id = 'res_1'`, idTenantA).
		Scan(&snapRaw, &unitsRaw, &currency, &limit); err != nil {
		t.Fatalf("read reservation on second connection: %v", err)
	}

	var gotSnap, gotUnits map[string]any
	if err := json.Unmarshal([]byte(snapRaw), &gotSnap); err != nil {
		t.Fatalf("snapshot not JSON: %v", err)
	}
	if err := json.Unmarshal([]byte(unitsRaw), &gotUnits); err != nil {
		t.Fatalf("reserved_units not JSON: %v", err)
	}
	rates, ok := gotSnap["rates"].([]any)
	if !ok || len(rates) != 2 {
		t.Fatalf("price_snapshot must keep both unit rates verbatim, got %s", snapRaw)
	}
	if gotSnap["rounding_rule"] != "ceil" || gotSnap["currency"] != "usd" {
		t.Fatalf("price_snapshot currency/rounding_rule lost: %s", snapRaw)
	}
	if gotUnits["input"] != float64(1000) || gotUnits["output"] != float64(200) {
		t.Fatalf("reserved per-kind units not persisted: %s", unitsRaw)
	}
	if currency != "usd" || limit != 5000000 {
		t.Fatalf("currency/limit round-trip failed: currency=%q limit=%d", currency, limit)
	}
}

// TestUsageRecordSettlesOnce proves usage_records is append-only per
// reservation: a second insert for the same reservation_id conflicts on
// UNIQUE(tenant_id,reservation_id).
func TestUsageRecordSettlesOnce(t *testing.T) {
	db := RequireTestDB(t)
	ctx := context.Background()

	seedTenant(t, ctx, db, idTenantA)
	if _, err := db.SQL.ExecContext(ctx, `
		INSERT INTO reservations (tenant_id, id, subject_type, subject_id,
		                          price_snapshot, reserved_units, limit_amount_micros,
		                          currency, state)
		VALUES ($1, 'res_u', 'api_key', 'key_9', '{}'::jsonb, '{}'::jsonb,
		        100, 'usd', 'held')`, idTenantA); err != nil {
		t.Fatalf("insert reservation: %v", err)
	}

	insertUsage := func(id string) error {
		_, err := db.SQL.ExecContext(ctx, `
			INSERT INTO usage_records (tenant_id, id, reservation_id, subject_type,
			                           subject_id, capability, model, units,
			                           amount_micros, currency, price_snapshot, outcome)
			VALUES ($1, $2, 'res_u', 'api_key', 'key_9', 'chat', 'm1',
			        '{"input":10,"output":5}'::jsonb, 42, 'usd', '{}'::jsonb, 'completed')`,
			idTenantA, id)
		return err
	}

	if err := insertUsage("use_1"); err != nil {
		t.Fatalf("first usage_record: %v", err)
	}
	if err := insertUsage("use_2"); !isUniqueViolation(err) {
		t.Fatalf("second usage_record for one reservation must conflict, got %v", err)
	}
}

// TestPriceChecksAndUnitUniqueness proves exact-rational price integrity:
// negative rate_micros and unit_quantity=0 are rejected by CHECKs, while
// (subject,capability,effective_from) rows coexist across different
// unit_kinds and conflict on the same unit_kind.
func TestPriceChecksAndUnitUniqueness(t *testing.T) {
	db := RequireTestDB(t)
	ctx := context.Background()

	seedTenant(t, ctx, db, idTenantA)

	insertPrice := func(id, unitKind string, rate, qty int64) error {
		_, err := db.SQL.ExecContext(ctx, `
			INSERT INTO billing_prices (tenant_id, id, subject_type, subject_id,
			                            capability, unit_kind, rate_micros,
			                            unit_quantity, currency, effective_from)
			VALUES ($1, $2, 'model', 'm1', 'chat', $3, $4, $5, 'usd',
			        '2026-01-01T00:00:00Z'::timestamptz)`,
			idTenantA, id, unitKind, rate, qty)
		return err
	}

	if err := insertPrice("p_in", "input", 2500, 1000); err != nil {
		t.Fatalf("insert input price: %v", err)
	}
	// Different unit_kind for the same subject/capability/effective_from
	// tuple -> distinct rational rate, must coexist.
	if err := insertPrice("p_out", "output", 7500, 1000); err != nil {
		t.Fatalf("different unit_kind must coexist: %v", err)
	}
	if err := insertPrice("p_in_dup", "input", 2600, 1000); !isUniqueViolation(err) {
		t.Fatalf("duplicate unit_kind at same effective_from must conflict, got %v", err)
	}
	// CHECK: negative rate and zero unit_quantity are rejected.
	if err := insertPrice("p_neg", "cached_input", -1, 1000); err == nil ||
		!strings.Contains(err.Error(), "SQLSTATE 23514") {
		t.Fatalf("negative rate_micros must fail CHECK, got %v", err)
	}
	if err := insertPrice("p_zero", "cached_input", 100, 0); err == nil ||
		!strings.Contains(err.Error(), "SQLSTATE 23514") {
		t.Fatalf("unit_quantity=0 must fail CHECK, got %v", err)
	}
}

// TestBudgetCurrencyBinding proves an amount budget persists its bound
// currency and a reservation persists its own bound currency — the columns
// the admission layer compares to reject a reservation priced in another
// currency (no FX).
func TestBudgetCurrencyBinding(t *testing.T) {
	db := RequireTestDB(t)
	ctx := context.Background()

	seedTenant(t, ctx, db, idTenantA)

	if _, err := db.SQL.ExecContext(ctx, `
		INSERT INTO budgets (tenant_id, id, name, subject_type, subject_id, kind,
		                     limit_value, currency, period, active, starts_at)
		VALUES ($1, 'bud_1', 'key spend', 'api_key', 'key_9', 'amount',
		        100000000, 'usd', 'monthly', true, now())`, idTenantA); err != nil {
		t.Fatalf("insert amount budget: %v", err)
	}
	if _, err := db.SQL.ExecContext(ctx, `
		INSERT INTO reservations (tenant_id, id, subject_type, subject_id,
		                          price_snapshot, reserved_units, limit_amount_micros,
		                          currency, state)
		VALUES ($1, 'res_eur', 'api_key', 'key_9', '{}'::jsonb, '{}'::jsonb,
		        5000, 'eur', 'held')`, idTenantA); err != nil {
		t.Fatalf("insert eur reservation: %v", err)
	}

	var budgetCurrency, resCurrency string
	if err := db.SQL.QueryRowContext(ctx,
		"SELECT currency FROM budgets WHERE tenant_id=$1 AND id='bud_1'", idTenantA).
		Scan(&budgetCurrency); err != nil {
		t.Fatalf("read budget currency: %v", err)
	}
	if err := db.SQL.QueryRowContext(ctx,
		"SELECT currency FROM reservations WHERE tenant_id=$1 AND id='res_eur'", idTenantA).
		Scan(&resCurrency); err != nil {
		t.Fatalf("read reservation currency: %v", err)
	}
	if budgetCurrency != "usd" || resCurrency != "eur" {
		t.Fatalf("bound currencies must persist: budget=%q reservation=%q",
			budgetCurrency, resCurrency)
	}

	// A token budget carries NULL currency; an amount budget may not.
	if _, err := db.SQL.ExecContext(ctx, `
		INSERT INTO budgets (tenant_id, id, name, subject_type, subject_id, kind,
		                     limit_value, currency, period, active, starts_at)
		VALUES ($1, 'bud_tok', 'key tokens', 'api_key', 'key_9', 'tokens',
		        1000000, NULL, 'daily', true, now())`, idTenantA); err != nil {
		t.Fatalf("tokens budget must allow NULL currency: %v", err)
	}
	if _, err := db.SQL.ExecContext(ctx, `
		INSERT INTO budgets (tenant_id, id, name, subject_type, subject_id, kind,
		                     limit_value, currency, period, active, starts_at)
		VALUES ($1, 'bud_bad', 'no currency', 'api_key', 'key_9', 'amount',
		        10, NULL, 'daily', true, now())`, idTenantA); err == nil ||
		!strings.Contains(err.Error(), "SQLSTATE 23514") {
		t.Fatalf("amount budget without currency must fail CHECK, got %v", err)
	}
}

// --- 006 domain schema ---

func seedChunkProfile(t *testing.T, ctx context.Context, db *DB, tenant, id, family string, version int) {
	t.Helper()
	if _, err := db.SQL.ExecContext(ctx, `
		INSERT INTO chunk_profiles (tenant_id, id, profile_id, version, name,
		                            boundary_strategy, max_size_codepoints)
		VALUES ($1, $2, $3, $4, 'profile', 'paragraph', 4000)`,
		tenant, id, family, version); err != nil {
		t.Fatalf("seed chunk_profile %s v%d: %v", id, version, err)
	}
}

func seedSourceVersion(t *testing.T, ctx context.Context, db *DB, tenant, connID, objID, verID string, seq int64) {
	t.Helper()
	seedConnection(t, ctx, db, tenant, connID, "github")
	if _, err := db.SQL.ExecContext(ctx, `
		INSERT INTO source_objects (tenant_id, id, connection_id, kind, native_id,
		                            authorization_mode, observed_seq)
		VALUES ($1, $2, $3, 'repo', 'acme/doc.md', 'source_acl', 1)`,
		tenant, objID, connID); err != nil {
		t.Fatalf("seed source_object %s: %v", objID, err)
	}
	if _, err := db.SQL.ExecContext(ctx, `
		INSERT INTO source_object_versions (tenant_id, id, object_id, observed_seq,
		                                   artifact_ref, content_sha256, byte_size)
		VALUES ($1, $2, $3, $4, 'art-' || $2::text, 'sha', 100)`,
		tenant, verID, objID, seq); err != nil {
		t.Fatalf("seed source_object_version %s: %v", verID, err)
	}
}

// TestChunkProfileVersioning proves multiple immutable versions coexist
// under one profile family, duplicate (tenant_id,profile_id,version)
// conflicts, and references resolve to the exact version row even after a
// newer family version exists.
func TestChunkProfileVersioning(t *testing.T) {
	db := RequireTestDB(t)
	ctx := context.Background()

	seedTenant(t, ctx, db, idTenantA)
	seedTenant(t, ctx, db, idTenantB)
	seedChunkProfile(t, ctx, db, idTenantA, "cp_v1", "fam_1", 1)

	// Version 2 of the same family coexists.
	seedChunkProfile(t, ctx, db, idTenantA, "cp_v2", "fam_1", 2)

	// Duplicate (tenant, family, version) is rejected.
	if _, err := db.SQL.ExecContext(ctx, `
		INSERT INTO chunk_profiles (tenant_id, id, profile_id, version, name,
		                            boundary_strategy, max_size_codepoints)
		VALUES ($1, 'cp_dup', 'fam_1', 2, 'dup', 'sentence', 1000)`, idTenantA); !isUniqueViolation(err) {
		t.Fatalf("duplicate (tenant_id,profile_id,version) must conflict, got %v", err)
	}

	// A collection bound to v1 still resolves to v1 after v2 was inserted.
	if _, err := db.SQL.ExecContext(ctx, `
		INSERT INTO knowledge_collections (tenant_id, id, name, qdrant_collection,
		                                   chunk_profile_version_id)
		VALUES ($1, 'col_1', 'docs', 'qdrant_docs', 'cp_v1')`, idTenantA); err != nil {
		t.Fatalf("insert collection bound to v1: %v", err)
	}
	var boundVersion int
	if err := db.SQL.QueryRowContext(ctx, `
		SELECT cp.version
		FROM knowledge_collections c
		JOIN chunk_profiles cp
		  ON cp.tenant_id = c.tenant_id
		 AND cp.id = c.chunk_profile_version_id
		WHERE c.tenant_id = $1 AND c.id = 'col_1'`, idTenantA).Scan(&boundVersion); err != nil {
		t.Fatalf("resolve bound chunk profile: %v", err)
	}
	if boundVersion != 1 {
		t.Fatalf("collection must stay bound to immutable version 1, got %d", boundVersion)
	}

	// A chunk pins the exact immutable version row the same way.
	seedSourceVersion(t, ctx, db, idTenantA, "conn_c", "obj_c", "ver_c", 1)
	if _, err := db.SQL.ExecContext(ctx, `
		INSERT INTO chunks (tenant_id, id, object_version_id, chunk_profile_version_id,
		                    ordinal, text, byte_start, byte_end)
		VALUES ($1, 'chk_1', 'ver_c', 'cp_v1', 0, 'text', 0, 4)`, idTenantA); err != nil {
		t.Fatalf("insert chunk bound to v1: %v", err)
	}
	var chunkVersion int
	if err := db.SQL.QueryRowContext(ctx, `
		SELECT cp.version
		FROM chunks k
		JOIN chunk_profiles cp
		  ON cp.tenant_id = k.tenant_id
		 AND cp.id = k.chunk_profile_version_id
		WHERE k.tenant_id = $1 AND k.id = 'chk_1'`, idTenantA).Scan(&chunkVersion); err != nil {
		t.Fatalf("resolve chunk profile: %v", err)
	}
	if chunkVersion != 1 {
		t.Fatalf("chunk must resolve to version 1, got %d", chunkVersion)
	}

	// Tenant B can own an identically-named family independently.
	seedChunkProfile(t, ctx, db, idTenantB, "cpb_v1", "fam_1", 1)
}

// TestDomainCrossTenantFKRejected proves index_generations (and by the same
// construction every 006 child reference) cannot point at another tenant's
// knowledge_collection: the composite (tenant_id, ...) FK rejects it.
func TestDomainCrossTenantFKRejected(t *testing.T) {
	db := RequireTestDB(t)
	ctx := context.Background()

	seedTenant(t, ctx, db, idTenantA)
	seedTenant(t, ctx, db, idTenantB)
	seedSourceVersion(t, ctx, db, idTenantA, "conn_x", "obj_x", "ver_x", 1)

	if _, err := db.SQL.ExecContext(ctx, `
		INSERT INTO knowledge_collections (tenant_id, id, name, qdrant_collection)
		VALUES ($1, 'col_a', 'docs', 'qdrant_a')`, idTenantA); err != nil {
		t.Fatalf("insert collection in tenant A: %v", err)
	}

	// Tenant B's generation row referencing tenant A's collection_id must
	// fail the composite FK — a row can never point at another tenant's
	// record.
	if _, err := db.SQL.ExecContext(ctx, `
		INSERT INTO index_generations (tenant_id, id, collection_id,
		                               object_version_id, generation, state)
		VALUES ($1, 'gen_b', 'col_a', 'ver_x', 1, 'staging')`, idTenantB); err == nil ||
		!strings.Contains(err.Error(), "SQLSTATE 23503") {
		t.Fatalf("cross-tenant index_generation must fail composite FK, got %v", err)
	}

	// The same reference inside tenant A is accepted.
	if _, err := db.SQL.ExecContext(ctx, `
		INSERT INTO index_generations (tenant_id, id, collection_id,
		                               object_version_id, generation, state)
		VALUES ($1, 'gen_a', 'col_a', 'ver_x', 1, 'staging')`, idTenantA); err != nil {
		t.Fatalf("same-tenant index_generation must succeed: %v", err)
	}
}
