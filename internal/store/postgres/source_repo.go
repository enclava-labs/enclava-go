package postgres

import (
	"context"
	"database/sql"
	"encoding/json"
	"errors"
	"fmt"
	"strings"
	"time"

	"enclava-go/internal/source"
)

// SourceRepo is the concrete source.SourceRepository over the 003_source
// schema. Every statement is scoped by composite (tenant_id, ...) keys;
// Apply*Snapshot owns the only path that can complete an access snapshot and
// retire unseen grant/membership rows.
type SourceRepo struct {
	db *DB
}

func NewSourceRepo(db *DB) *SourceRepo {
	return &SourceRepo{db: db}
}

var _ source.SourceRepository = (*SourceRepo)(nil)

// WithTx runs fn in one transaction — multi-row observations apply through
// it so a partial write can never look complete.
func (r *SourceRepo) WithTx(ctx context.Context, fn func(source.Tx) error) error {
	return WithTx(ctx, r.db.SQL, func(tx *sql.Tx) error { return fn(tx) })
}

func nullableText(s string) any {
	if s == "" {
		return nil
	}
	return s
}

func jsonList(values []string) ([]byte, error) {
	if values == nil {
		values = []string{}
	}
	return json.Marshal(values)
}

func scanJSONList(raw []byte) []string {
	out := []string{}
	if len(raw) == 0 {
		return out
	}
	_ = json.Unmarshal(raw, &out)
	return out
}

// ---------------------------------------------------------------------
// Connections

func (r *SourceRepo) CreateConnection(ctx context.Context, c source.Connection) error {
	config, err := json.Marshal(map[string]any(c.Config))
	if err != nil {
		return fmt.Errorf("marshal connection config: %w", err)
	}
	audience, err := jsonList(c.AudienceDefault)
	if err != nil {
		return fmt.Errorf("marshal connection audience: %w", err)
	}
	status := c.Status
	if status == "" {
		status = "active"
	}
	_, err = r.db.SQL.ExecContext(ctx, `
		INSERT INTO connections (tenant_id, id, kind, name, credential_ref, config,
		                         target_collection_id, audience_default, status, created_by)
		VALUES ($1, $2, $3, $4, $5, $6, $7, $8, $9, $10)`,
		c.TenantID, c.ID, c.Kind, c.Name, c.CredentialRef, config,
		nullableText(c.TargetCollectionID), audience, status, nullableText(c.CreatedBy))
	if err != nil {
		return fmt.Errorf("insert connection: %w", err)
	}
	return nil
}

type connectionScanner interface {
	Scan(dest ...any) error
}

const connectionColumns = `tenant_id::text, id, kind, name, credential_ref, config,
	target_collection_id, audience_default, status, created_by,
	synced_at, sync_status, sync_error, created_at, updated_at`

func scanConnection(row connectionScanner, c *source.Connection) error {
	var config, audience []byte
	var target, createdBy, syncStatus, syncError sql.NullString
	var syncedAt sql.NullTime
	err := row.Scan(&c.TenantID, &c.ID, &c.Kind, &c.Name, &c.CredentialRef,
		&config, &target, &audience, &c.Status, &createdBy,
		&syncedAt, &syncStatus, &syncError, &c.CreatedAt, &c.UpdatedAt)
	if err != nil {
		return err
	}
	_ = json.Unmarshal(config, &c.Config)
	c.TargetCollectionID = target.String
	c.AudienceDefault = scanJSONList(audience)
	c.CreatedBy = createdBy.String
	if syncedAt.Valid {
		t := syncedAt.Time
		c.SyncedAt = &t
	}
	c.SyncStatus = syncStatus.String
	c.SyncError = syncError.String
	return nil
}

func (r *SourceRepo) GetConnection(ctx context.Context, tenantID, id string) (source.Connection, error) {
	var c source.Connection
	err := scanConnection(r.db.SQL.QueryRowContext(ctx,
		`SELECT `+connectionColumns+` FROM connections WHERE tenant_id = $1 AND id = $2`, tenantID, id), &c)
	if errors.Is(err, sql.ErrNoRows) || isInvalidUUID(err) {
		return source.Connection{}, source.ErrDenied
	}
	if err != nil {
		return source.Connection{}, fmt.Errorf("get connection: %w", err)
	}
	return c, nil
}

func (r *SourceRepo) ListConnections(ctx context.Context, tenantID string) ([]source.Connection, error) {
	rows, err := r.db.SQL.QueryContext(ctx,
		`SELECT `+connectionColumns+` FROM connections WHERE tenant_id = $1 ORDER BY id`, tenantID)
	if err != nil {
		return nil, fmt.Errorf("list connections: %w", err)
	}
	defer func() { _ = rows.Close() }()
	var out []source.Connection
	for rows.Next() {
		var c source.Connection
		if err := scanConnection(rows, &c); err != nil {
			return nil, fmt.Errorf("scan connection: %w", err)
		}
		out = append(out, c)
	}
	return out, rows.Err()
}

// ListSyncCandidates returns active connections with a target collection,
// oldest-synced first, across tenants — the scheduler's work list. Interval
// filtering happens in the scheduler (per-source config).
func (r *SourceRepo) ListSyncCandidates(ctx context.Context, limit int) ([]source.Connection, error) {
	if limit <= 0 {
		limit = 500
	}
	rows, err := r.db.SQL.QueryContext(ctx, `
		SELECT `+connectionColumns+`
		FROM connections
		WHERE status = 'active' AND target_collection_id IS NOT NULL
		ORDER BY COALESCE(synced_at, created_at) ASC
		LIMIT $1`, limit)
	if err != nil {
		return nil, fmt.Errorf("list sync candidates: %w", err)
	}
	defer func() { _ = rows.Close() }()
	var out []source.Connection
	for rows.Next() {
		var c source.Connection
		if err := scanConnection(rows, &c); err != nil {
			return nil, fmt.Errorf("scan sync candidate: %w", err)
		}
		out = append(out, c)
	}
	return out, rows.Err()
}

func (r *SourceRepo) UpdateConnectionStatus(ctx context.Context, tenantID, id, status string) error {
	res, err := r.db.SQL.ExecContext(ctx,
		`UPDATE connections SET status = $3, updated_at = now() WHERE tenant_id = $1 AND id = $2`,
		tenantID, id, status)
	if err != nil {
		return fmt.Errorf("update connection status: %w", err)
	}
	if n, _ := res.RowsAffected(); n == 0 {
		return source.ErrDenied
	}
	return nil
}

func (r *SourceRepo) SetSyncState(ctx context.Context, tenantID, id string, syncedAt time.Time, syncStatus, syncError string) error {
	res, err := r.db.SQL.ExecContext(ctx, `
		UPDATE connections SET synced_at = $3, sync_status = $4, sync_error = $5
		WHERE tenant_id = $1 AND id = $2`,
		tenantID, id, syncedAt, nullableText(syncStatus), nullableText(syncError))
	if err != nil {
		return fmt.Errorf("set connection sync state: %w", err)
	}
	if n, _ := res.RowsAffected(); n == 0 {
		return source.ErrDenied
	}
	return nil
}

// ---------------------------------------------------------------------
// Objects

// UpsertObject inserts the object or returns the already-persisted row for
// the same (tenant, connection, kind, native_id) identity. Existing
// identity fields are never mutated. Uploads (empty ConnectionID) dedupe on
// the partial unique index.
func (r *SourceRepo) UpsertObject(ctx context.Context, o source.Object) (source.Object, error) {
	mode := o.AuthorizationMode
	if mode == "" {
		mode = source.AuthorizationSourceACL
	}
	var connID any
	conflict := "(tenant_id, connection_id, kind, native_id) DO NOTHING"
	if o.ConnectionID != "" {
		connID = o.ConnectionID
	} else {
		// Uploads dedupe on the partial unique index, not the nullable
		// UNIQUE constraint.
		conflict = "(tenant_id, kind, native_id) WHERE connection_id IS NULL DO NOTHING"
	}
	var id string
	err := r.db.SQL.QueryRowContext(ctx, `
		INSERT INTO source_objects
		    (tenant_id, id, connection_id, kind, native_id, parent_object_id,
		     authorization_mode, owner_actor_id, audience, upstream_revision, observed_seq)
		VALUES ($1, $2, $3, $4, $5, $6, $7, $8, $9::jsonb, $10, $11)
		ON CONFLICT `+conflict+`
		RETURNING id`,
		o.TenantID, o.ID, connID, o.Kind, o.NativeID, nullableText(o.ParentObjectID),
		mode, nullableText(o.OwnerActorID), mustJSONList(o.Audience),
		nilIfNilStr(o.UpstreamRevision), o.ObservedSeq).Scan(&id)
	switch {
	case errors.Is(err, sql.ErrNoRows):
		// Identity already persisted — return the existing row unchanged.
		existing, gerr := r.objectByNativeID(ctx, o.TenantID, o.ConnectionID, o.Kind, o.NativeID)
		if gerr != nil {
			return source.Object{}, gerr
		}
		return existing, nil
	case err != nil:
		return source.Object{}, fmt.Errorf("upsert object: %w", err)
	}
	return r.GetObject(ctx, o.TenantID, id)
}

// ObserveObject updates metadata only after the source actually observed the object.
func (r *SourceRepo) ObserveObject(ctx context.Context, tenantID, objectID, parentID string, upstream *string) error {
	result, err := r.db.SQL.ExecContext(ctx, `
		UPDATE source_objects SET parent_object_id = $3, upstream_revision = $4,
		       deleted_at = NULL, tombstoned_at = NULL, last_seen_at = now()
		WHERE tenant_id = $1 AND id = $2`,
		tenantID, objectID, nullableText(parentID), nilIfNilStr(upstream))
	if err != nil {
		return fmt.Errorf("observe object: %w", err)
	}
	n, err := result.RowsAffected()
	if err != nil {
		return err
	}
	if n == 0 {
		return source.ErrDenied
	}
	return nil
}

func nilIfNilStr(s *string) any {
	if s == nil {
		return nil
	}
	return *s
}

func mustJSONList(values []string) []byte {
	b, err := jsonList(values)
	if err != nil {
		return []byte("[]")
	}
	return b
}

func (r *SourceRepo) objectByNativeID(ctx context.Context, tenantID, connectionID, kind, nativeID string) (source.Object, error) {
	var id string
	var err error
	if connectionID == "" {
		err = r.db.SQL.QueryRowContext(ctx, `
			SELECT id FROM source_objects
			WHERE tenant_id = $1 AND connection_id IS NULL AND kind = $2 AND native_id = $3`,
			tenantID, kind, nativeID).Scan(&id)
	} else {
		err = r.db.SQL.QueryRowContext(ctx, `
			SELECT id FROM source_objects
			WHERE tenant_id = $1 AND connection_id = $2 AND kind = $3 AND native_id = $4`,
			tenantID, connectionID, kind, nativeID).Scan(&id)
	}
	if err != nil {
		return source.Object{}, fmt.Errorf("lookup object identity: %w", err)
	}
	return r.GetObject(ctx, tenantID, id)
}

func (r *SourceRepo) GetObject(ctx context.Context, tenantID, id string) (source.Object, error) {
	var o source.Object
	var connID, parent, owner sql.NullString
	var audience []byte
	var upstream sql.NullString
	var deleted, tombstoned sql.NullTime
	err := r.db.SQL.QueryRowContext(ctx, `
		SELECT tenant_id::text, id, connection_id, kind, native_id, parent_object_id,
		       authorization_mode, owner_actor_id, audience, upstream_revision,
		       observed_seq, deleted_at, tombstoned_at
		FROM source_objects WHERE tenant_id = $1 AND id = $2`,
		tenantID, id).Scan(
		&o.TenantID, &o.ID, &connID, &o.Kind, &o.NativeID, &parent,
		&o.AuthorizationMode, &owner, &audience, &upstream,
		&o.ObservedSeq, &deleted, &tombstoned)
	if errors.Is(err, sql.ErrNoRows) || isInvalidUUID(err) {
		return source.Object{}, source.ErrDenied
	}
	if err != nil {
		return source.Object{}, fmt.Errorf("get object: %w", err)
	}
	o.ConnectionID = connID.String
	o.ParentObjectID = parent.String
	o.OwnerActorID = owner.String
	o.Audience = scanJSONList(audience)
	if upstream.Valid {
		s := upstream.String
		o.UpstreamRevision = &s
	}
	if deleted.Valid {
		t := deleted.Time
		o.DeletedAt = &t
	}
	if tombstoned.Valid {
		t := tombstoned.Time
		o.TombstonedAt = &t
	}
	return o, nil
}

func (r *SourceRepo) ListObjectsByConnection(ctx context.Context, tenantID, connectionID string) ([]source.Object, error) {
	rows, err := r.db.SQL.QueryContext(ctx, `
		SELECT id FROM source_objects
		WHERE tenant_id = $1 AND connection_id = $2 ORDER BY id`,
		tenantID, connectionID)
	if err != nil {
		return nil, fmt.Errorf("list objects: %w", err)
	}
	defer func() { _ = rows.Close() }()
	var out []source.Object
	for rows.Next() {
		var id string
		if err := rows.Scan(&id); err != nil {
			return nil, fmt.Errorf("scan object id: %w", err)
		}
		o, err := r.GetObject(ctx, tenantID, id)
		if err != nil {
			return nil, err
		}
		out = append(out, o)
	}
	return out, rows.Err()
}

func (r *SourceRepo) MarkObjectDeleted(ctx context.Context, tenantID, id string) error {
	res, err := r.db.SQL.ExecContext(ctx, `
		UPDATE source_objects SET deleted_at = now(), last_seen_at = now()
		WHERE tenant_id = $1 AND id = $2`, tenantID, id)
	if err != nil {
		return fmt.Errorf("mark object deleted: %w", err)
	}
	if n, _ := res.RowsAffected(); n == 0 {
		return source.ErrDenied
	}
	return nil
}

func (r *SourceRepo) TombstoneObject(ctx context.Context, tenantID, id string) error {
	res, err := r.db.SQL.ExecContext(ctx, `
		UPDATE source_objects SET tombstoned_at = now(), deleted_at = COALESCE(deleted_at, now())
		WHERE tenant_id = $1 AND id = $2`, tenantID, id)
	if err != nil {
		return fmt.Errorf("tombstone object: %w", err)
	}
	if n, _ := res.RowsAffected(); n == 0 {
		return source.ErrDenied
	}
	return nil
}

// ---------------------------------------------------------------------
// Versions

// InsertObjectVersion asserts the observed_seq is strictly monotonic within
// the object, then inserts the immutable row. Durable orchestration assigns
// the sequence; the repo only enforces it.
func (r *SourceRepo) InsertObjectVersion(ctx context.Context, tenantID string, ver source.ObjectVersion) error {
	return WithTx(ctx, r.db.SQL, func(tx *sql.Tx) error {
		var maxSeq int64
		if err := tx.QueryRowContext(ctx, `
			SELECT COALESCE(MAX(observed_seq), 0) FROM source_object_versions
			WHERE tenant_id = $1 AND object_id = $2`,
			tenantID, ver.ObjectID).Scan(&maxSeq); err != nil {
			return fmt.Errorf("read max observed_seq: %w", err)
		}
		if ver.ObservedSeq <= maxSeq {
			return fmt.Errorf("observed_seq %d not greater than existing max %d", ver.ObservedSeq, maxSeq)
		}
		restrictions, err := json.Marshal(ver.Restrictions)
		if err != nil {
			return fmt.Errorf("marshal version restrictions: %w", err)
		}
		provenance := ver.Provenance
		if strings.TrimSpace(provenance) == "" {
			provenance = "{}"
		}
		if _, err := tx.ExecContext(ctx, `
			INSERT INTO source_object_versions
			    (tenant_id, id, object_id, observed_seq, artifact_ref, content_sha256,
			     byte_size, mime_type, original_filename, upstream_revision,
			     provenance, restrictions)
			VALUES ($1, $2, $3, $4, $5, $6, $7, $8, $9, $10, $11::jsonb, $12)`,
			tenantID, versionRowID(ver), ver.ObjectID, ver.ObservedSeq, ver.ArtifactRef,
			ver.SHA256, ver.ByteSize, ver.MimeType, ver.OriginalFilename,
			nilIfNilStr(ver.UpstreamRevision), provenance, restrictions); err != nil {
			return fmt.Errorf("insert object version: %w", err)
		}
		if _, err := tx.ExecContext(ctx, `
			UPDATE source_objects SET last_seen_at = now(), observed_seq = GREATEST(observed_seq, $3)
			WHERE tenant_id = $1 AND id = $2`,
			tenantID, ver.ObjectID, ver.ObservedSeq); err != nil {
			return fmt.Errorf("touch object: %w", err)
		}
		return nil
	})
}

func versionRowID(ver source.ObjectVersion) string {
	return fmt.Sprintf("%s#%d", ver.ObjectID, ver.ObservedSeq)
}

func scanVersion(row *sql.Row, ver *source.ObjectVersion) error {
	var mime, filename, provenance, restrictions []byte
	var upstreamStr sql.NullString
	err := row.Scan(&ver.ObjectID, &ver.ObservedSeq, &ver.ArtifactRef, &ver.SHA256,
		&ver.ByteSize, &mime, &filename, &upstreamStr, &provenance, &restrictions)
	if err != nil {
		return err
	}
	ver.MimeType = scanText(mime)
	ver.OriginalFilename = scanText(filename)
	if upstreamStr.Valid {
		s := upstreamStr.String
		ver.UpstreamRevision = &s
	}
	ver.Provenance = string(provenance)
	var list []source.SourceRestriction
	if err := json.Unmarshal(restrictions, &list); err == nil {
		ver.Restrictions = list
	}
	return nil
}

func scanText(b []byte) string { return string(b) }

func (r *SourceRepo) GetObjectVersion(ctx context.Context, tenantID, objectID string, observedSeq int64) (source.ObjectVersion, error) {
	var ver source.ObjectVersion
	err := scanVersion(r.db.SQL.QueryRowContext(ctx, `
		SELECT object_id, observed_seq, artifact_ref, content_sha256, byte_size,
		       mime_type, original_filename, upstream_revision, provenance, restrictions
		FROM source_object_versions
		WHERE tenant_id = $1 AND object_id = $2 AND observed_seq = $3`,
		tenantID, objectID, observedSeq), &ver)
	if errors.Is(err, sql.ErrNoRows) || isInvalidUUID(err) {
		return source.ObjectVersion{}, source.ErrDenied
	}
	if err != nil {
		return source.ObjectVersion{}, fmt.Errorf("get object version: %w", err)
	}
	return ver, nil
}

func (r *SourceRepo) LatestObjectVersion(ctx context.Context, tenantID, objectID string) (source.ObjectVersion, error) {
	var ver source.ObjectVersion
	err := scanVersion(r.db.SQL.QueryRowContext(ctx, `
		SELECT object_id, observed_seq, artifact_ref, content_sha256, byte_size,
		       mime_type, original_filename, upstream_revision, provenance, restrictions
		FROM source_object_versions
		WHERE tenant_id = $1 AND object_id = $2
		ORDER BY observed_seq DESC LIMIT 1`,
		tenantID, objectID), &ver)
	if errors.Is(err, sql.ErrNoRows) || isInvalidUUID(err) {
		return source.ObjectVersion{}, source.ErrDenied
	}
	if err != nil {
		return source.ObjectVersion{}, fmt.Errorf("latest object version: %w", err)
	}
	return ver, nil
}

func (r *SourceRepo) GetObjectVersionByArtifact(ctx context.Context, tenantID, artifactRef string) (source.Object, source.ObjectVersion, error) {
	var objectID string
	var seq int64
	err := r.db.SQL.QueryRowContext(ctx, `
		SELECT object_id, observed_seq FROM source_object_versions
		WHERE tenant_id = $1 AND artifact_ref = $2`,
		tenantID, artifactRef).Scan(&objectID, &seq)
	if errors.Is(err, sql.ErrNoRows) || isInvalidUUID(err) {
		return source.Object{}, source.ObjectVersion{}, source.ErrDenied
	}
	if err != nil {
		return source.Object{}, source.ObjectVersion{}, fmt.Errorf("find object version by artifact: %w", err)
	}
	object, err := r.GetObject(ctx, tenantID, objectID)
	if err != nil {
		return source.Object{}, source.ObjectVersion{}, err
	}
	version, err := r.GetObjectVersion(ctx, tenantID, objectID, seq)
	return object, version, err
}

// IngestHandoffExists proves a connector observation survived the crash
// window between version insert and ingest dispatch: a live jobs row
// (queued/running/succeeded — failed/cancelled/deleted never ran or lost the
// work) covering (connection, object, observed_seq) and targeting this
// collection, or any index generation for that object version in the target
// collection, counts as durable handoff.
func (r *SourceRepo) IngestHandoffExists(ctx context.Context, tenantID, connectionID, collectionID, objectID string, observedSeq int64) (bool, error) {
	var exists bool
	err := r.db.SQL.QueryRowContext(ctx, `
		SELECT EXISTS (
			SELECT 1 FROM jobs
			WHERE tenant_id = $1 AND connection_id = $2 AND object_id = $3
			  AND observed_revision = $4 AND policy->>'collection_id' = $5
			  AND status IN ('queued','running','succeeded')
		) OR EXISTS (
			SELECT 1 FROM index_generations g
			JOIN source_object_versions v
			  ON v.tenant_id = g.tenant_id AND v.id = g.object_version_id
			WHERE g.tenant_id = $1 AND g.collection_id = $5
			  AND v.object_id = $3 AND v.observed_seq = $4
		)`,
		tenantID, connectionID, objectID, observedSeq, collectionID).Scan(&exists)
	if err != nil {
		return false, fmt.Errorf("check ingest handoff: %w", err)
	}
	return exists, nil
}

// ---------------------------------------------------------------------
// Identities, groups, memberships

func (r *SourceRepo) InsertSourceIdentity(ctx context.Context, id source.SourceIdentity) error {
	_, err := r.db.SQL.ExecContext(ctx, `
		INSERT INTO source_identities
		    (tenant_id, id, connection_id, source_user_native_id, display_name, mapped_user_id)
		VALUES ($1, $2, $3, $4, $5, $6)
		ON CONFLICT (tenant_id, connection_id, source_user_native_id)
		DO UPDATE SET mapped_user_id = EXCLUDED.mapped_user_id,
		    display_name = COALESCE(NULLIF(EXCLUDED.display_name, ''), source_identities.display_name)`,
		id.TenantID, id.ID, id.ConnectionID, id.NativeID, id.DisplayName, nullableText(id.MappedUserID))
	if err != nil {
		return fmt.Errorf("upsert source identity: %w", err)
	}
	return nil
}

// ObserveSourceIdentity records a provider-native principal seen during
// access/membership ingestion. It never touches an existing mapping, so a
// sync racing an administrator bind cannot clear it.
func (r *SourceRepo) ObserveSourceIdentity(ctx context.Context, id source.SourceIdentity) error {
	_, err := r.db.SQL.ExecContext(ctx, `
		INSERT INTO source_identities
		    (tenant_id, id, connection_id, source_user_native_id, display_name)
		VALUES ($1, $2, $3, $4, $5)
		ON CONFLICT (tenant_id, connection_id, source_user_native_id)
		DO UPDATE SET display_name = COALESCE(NULLIF(EXCLUDED.display_name, ''), source_identities.display_name)`,
		id.TenantID, id.ID, id.ConnectionID, id.NativeID, id.DisplayName)
	if err != nil {
		return fmt.Errorf("observe source identity: %w", err)
	}
	return nil
}

func (r *SourceRepo) SourceIdentitiesForConnection(ctx context.Context, tenantID, connectionID string) ([]source.SourceIdentity, error) {
	rows, err := r.db.SQL.QueryContext(ctx, `SELECT tenant_id::text,id,connection_id,source_user_native_id,display_name,COALESCE(mapped_user_id,'') FROM source_identities WHERE tenant_id=$1 AND connection_id=$2 ORDER BY source_user_native_id`, tenantID, connectionID)
	if err != nil {
		return nil, err
	}
	defer rows.Close()
	out := []source.SourceIdentity{}
	for rows.Next() {
		var row source.SourceIdentity
		if err := rows.Scan(&row.TenantID, &row.ID, &row.ConnectionID, &row.NativeID, &row.DisplayName, &row.MappedUserID); err != nil {
			return nil, err
		}
		out = append(out, row)
	}
	return out, rows.Err()
}

func (r *SourceRepo) SourceIdentityByID(ctx context.Context, tenantID, id string) (source.SourceIdentity, error) {
	var row source.SourceIdentity
	err := r.db.SQL.QueryRowContext(ctx, `SELECT tenant_id::text,id,connection_id,source_user_native_id,display_name,COALESCE(mapped_user_id,'') FROM source_identities WHERE tenant_id=$1 AND id=$2`, tenantID, id).Scan(&row.TenantID, &row.ID, &row.ConnectionID, &row.NativeID, &row.DisplayName, &row.MappedUserID)
	if errors.Is(err, sql.ErrNoRows) || isInvalidUUID(err) {
		return source.SourceIdentity{}, source.ErrDenied
	}
	return row, err
}

func (r *SourceRepo) UserExists(ctx context.Context, tenantID, userID string) (bool, error) {
	var exists bool
	err := r.db.SQL.QueryRowContext(ctx, `SELECT EXISTS(SELECT 1 FROM users WHERE tenant_id=$1 AND id=$2 AND status='active')`, tenantID, userID).Scan(&exists)
	return exists, err
}

func (r *SourceRepo) InsertSourceGroup(ctx context.Context, g source.SourceGroup) error {
	_, err := r.db.SQL.ExecContext(ctx, `
		INSERT INTO source_groups
		    (tenant_id, id, connection_id, source_group_native_id, name)
		VALUES ($1, $2, $3, $4, $5)
		ON CONFLICT (tenant_id, connection_id, source_group_native_id)
		DO UPDATE SET name = EXCLUDED.name`,
		g.TenantID, g.ID, g.ConnectionID, g.NativeID, g.Name)
	if err != nil {
		return fmt.Errorf("upsert source group: %w", err)
	}
	return nil
}

func (r *SourceRepo) InsertSourceMembership(ctx context.Context, m source.SourceGroupMembership) error {
	rowID := fmt.Sprintf("ms:%s:%s:%d", m.GroupID, m.MemberID, m.AccessRevision)
	_, err := r.db.SQL.ExecContext(ctx, `
		INSERT INTO source_group_memberships
		    (tenant_id, id, group_id, source_identity_id, access_revision)
		VALUES ($1, $2, $3, $4, $5)
		ON CONFLICT (tenant_id, id) DO NOTHING`,
		m.TenantID, rowID, m.GroupID, m.MemberID, m.AccessRevision)
	if err != nil {
		return fmt.Errorf("insert membership: %w", err)
	}
	return nil
}

// SourceIdentityForUser resolves an Enclava user to the connection-scoped
// source identity. Missing mapping denies — unknown mappings never become
// company-wide access.
func (r *SourceRepo) SourceIdentityForUser(ctx context.Context, tenantID, connectionID, userID string) (source.SourceIdentity, error) {
	var id source.SourceIdentity
	var mapped sql.NullString
	err := r.db.SQL.QueryRowContext(ctx, `
		SELECT tenant_id::text, id, connection_id, source_user_native_id, display_name, mapped_user_id
		FROM source_identities
		WHERE tenant_id = $1 AND connection_id = $2 AND mapped_user_id = $3`,
		tenantID, connectionID, userID).Scan(
		&id.TenantID, &id.ID, &id.ConnectionID, &id.NativeID, &id.DisplayName, &mapped)
	if errors.Is(err, sql.ErrNoRows) || isInvalidUUID(err) {
		return source.SourceIdentity{}, source.ErrDenied
	}
	if err != nil {
		return source.SourceIdentity{}, fmt.Errorf("source identity for user: %w", err)
	}
	id.MappedUserID = mapped.String
	return id, nil
}

// GroupIDsForIdentity reads memberships at the revision checked by the authorizer.
func (r *SourceRepo) GroupIDsForIdentity(ctx context.Context, tenantID, connectionID, sourceIdentityID string, revision int64) ([]string, error) {
	rows, err := r.db.SQL.QueryContext(ctx, `
		SELECT m.group_id FROM (
			SELECT revision FROM access_snapshots
			WHERE tenant_id = $1 AND connection_id = $2 AND kind = 'membership' AND status = 'complete'
			ORDER BY revision DESC LIMIT 1
		) s
		LEFT JOIN (
			SELECT m.group_id, m.access_revision FROM source_group_memberships m
			JOIN source_groups g ON g.tenant_id = m.tenant_id AND g.id = m.group_id
			WHERE m.tenant_id = $1 AND g.connection_id = $2 AND m.source_identity_id = $3
		) m ON m.access_revision = s.revision
		WHERE s.revision = $4`, tenantID, connectionID, sourceIdentityID, revision)
	if err != nil {
		return nil, fmt.Errorf("group memberships: %w", err)
	}
	defer func() { _ = rows.Close() }()
	out := []string{}
	found := false
	for rows.Next() {
		found = true
		var g sql.NullString
		if err := rows.Scan(&g); err != nil {
			return nil, fmt.Errorf("scan membership: %w", err)
		}
		if g.Valid {
			out = append(out, g.String)
		}
	}
	if err := rows.Err(); err != nil {
		return nil, err
	}
	if !found {
		return nil, source.ErrDenied
	}
	return out, nil
}

// ---------------------------------------------------------------------
// Grants and snapshots

// AncestorObjectIDs returns the object and its complete, valid parent chain.
func (r *SourceRepo) AncestorObjectIDs(ctx context.Context, tenantID, connectionID, objectID string, maxDepth int) ([]string, error) {
	if maxDepth < 0 {
		return nil, source.ErrDenied
	}
	rows, err := r.db.SQL.QueryContext(ctx, `
		WITH RECURSIVE ancestors AS (
			SELECT $3::text AS id, o.parent_object_id, 0 AS depth,
			       (o.id IS NOT NULL AND o.deleted_at IS NULL AND o.tombstoned_at IS NULL) AS valid
			FROM (SELECT 1) seed
			LEFT JOIN source_objects o ON o.tenant_id = $1 AND o.connection_id = $2 AND o.id = $3
			UNION ALL
			SELECT a.parent_object_id, o.parent_object_id, a.depth + 1,
			       (o.id IS NOT NULL AND o.deleted_at IS NULL AND o.tombstoned_at IS NULL)
			FROM ancestors a
			LEFT JOIN source_objects o ON o.tenant_id = $1 AND o.connection_id = $2 AND o.id = a.parent_object_id
			WHERE a.valid AND a.parent_object_id IS NOT NULL AND a.depth <= $4
		)
		SELECT id, depth, valid FROM ancestors`, tenantID, connectionID, objectID, maxDepth)
	if err != nil {
		return nil, fmt.Errorf("ancestor objects: %w", err)
	}
	defer func() { _ = rows.Close() }()
	ids := []string{}
	seen := map[string]struct{}{}
	for rows.Next() {
		var id string
		var depth int
		var valid bool
		if err := rows.Scan(&id, &depth, &valid); err != nil {
			return nil, fmt.Errorf("scan ancestor: %w", err)
		}
		if _, exists := seen[id]; !valid || depth > maxDepth || exists {
			return nil, source.ErrDenied
		}
		seen[id] = struct{}{}
		ids = append(ids, id)
	}
	if err := rows.Err(); err != nil {
		return nil, err
	}
	return ids, nil
}

// GrantsForObjects reads revision-qualified grants for a connection's objects in one query.
func (r *SourceRepo) GrantsForObjects(ctx context.Context, tenantID, connectionID string, objectIDs []string, accessRevision int64) ([]source.SourceGrant, error) {
	rows, err := r.db.SQL.QueryContext(ctx, `
		SELECT g.object_id, g.subject_type, g.source_identity_id, g.source_group_id,
		       g.effect, g.access_revision
		FROM (
			SELECT revision FROM access_snapshots
			WHERE tenant_id = $1 AND connection_id = $2 AND kind = 'grants' AND status = 'complete'
			ORDER BY revision DESC LIMIT 1
		) s
		LEFT JOIN (
			SELECT g.* FROM source_grants g
			JOIN source_objects o ON o.tenant_id = g.tenant_id AND o.id = g.object_id
			WHERE g.tenant_id = $1 AND o.connection_id = $2 AND g.object_id = ANY($3)
		) g ON g.access_revision = s.revision
		WHERE s.revision = $4`, tenantID, connectionID, objectIDs, accessRevision)
	if err != nil {
		return nil, fmt.Errorf("grants for objects: %w", err)
	}
	defer func() { _ = rows.Close() }()
	out := []source.SourceGrant{}
	found := false
	for rows.Next() {
		found = true
		var objectID, kind, userID, groupID, effect sql.NullString
		var revision sql.NullInt64
		if err := rows.Scan(&objectID, &kind, &userID, &groupID, &effect, &revision); err != nil {
			return nil, fmt.Errorf("scan grant: %w", err)
		}
		if !objectID.Valid {
			continue
		}
		g := source.SourceGrant{TenantID: tenantID, ConnectionID: connectionID,
			ObjectID: objectID.String, SubjectKind: kind.String, Effect: effect.String, AccessRevision: revision.Int64}
		if g.SubjectKind == source.GrantSubjectGroup {
			g.SubjectID = groupID.String
		} else {
			g.SubjectID = userID.String
		}
		out = append(out, g)
	}
	if err := rows.Err(); err != nil {
		return nil, err
	}
	if !found {
		return nil, source.ErrDenied
	}
	return out, nil
}

// BeginSnapshot records an incomplete (never authoritative) snapshot row.
func (r *SourceRepo) BeginSnapshot(ctx context.Context, tenantID, connectionID, kind string, revision int64) error {
	_, err := r.db.SQL.ExecContext(ctx, `
		INSERT INTO access_snapshots (tenant_id, id, connection_id, kind, revision, status)
		VALUES ($1, $2, $3, $4, $5, 'incomplete')
		ON CONFLICT (tenant_id, connection_id, kind, revision) DO NOTHING`,
		tenantID, snapshotRowID(connectionID, kind, revision), connectionID, kind, revision)
	if err != nil {
		return fmt.Errorf("begin snapshot: %w", err)
	}
	return nil
}

func snapshotRowID(connectionID, kind string, revision int64) string {
	return fmt.Sprintf("%s:%s:%d", connectionID, kind, revision)
}

// LatestCompleteRevision returns the newest complete snapshot revision and
// its completion time, or source.ErrDenied when none exists.
func (r *SourceRepo) LatestCompleteRevision(ctx context.Context, tenantID, connectionID, kind string) (int64, time.Time, error) {
	var rev int64
	var completedAt time.Time
	err := r.db.SQL.QueryRowContext(ctx, `
		SELECT revision, completed_at FROM access_snapshots
		WHERE tenant_id = $1 AND connection_id = $2 AND kind = $3 AND status = 'complete'
		ORDER BY revision DESC LIMIT 1`,
		tenantID, connectionID, kind).Scan(&rev, &completedAt)
	if errors.Is(err, sql.ErrNoRows) || isInvalidUUID(err) {
		return 0, time.Time{}, source.ErrDenied
	}
	if err != nil {
		return 0, time.Time{}, fmt.Errorf("latest complete revision: %w", err)
	}
	return rev, completedAt, nil
}

// ApplyAccessSnapshot atomically records a grants observation: insert
// revision-qualified grant rows, mark the snapshot complete, then delete
// grant rows unseen at the now-complete revision. A snapshot that was never
// completed (partial pagination) removes nothing.
func (r *SourceRepo) ApplyAccessSnapshot(ctx context.Context, tx source.Tx, tenantID, connectionID string, revision int64, grants []source.SourceGrant, unmappedSubjects int) error {
	seen := make(map[string]struct{}, len(grants))
	for i, g := range grants {
		var userID, groupID any
		switch g.SubjectKind {
		case source.GrantSubjectGroup:
			groupID = g.SubjectID
		case source.GrantSubjectUser:
			userID = g.SubjectID
		}
		key := fmt.Sprintf("%s|%s|%s|%s", g.ObjectID, g.SubjectKind, g.SubjectID, g.Effect)
		if _, dup := seen[key]; dup {
			continue
		}
		seen[key] = struct{}{}
		rowID := fmt.Sprintf("gr:%s:%d:%d", connectionID, revision, i)
		if _, err := tx.ExecContext(ctx, `
			INSERT INTO source_grants
			    (tenant_id, id, object_id, subject_type, source_identity_id,
			     source_group_id, effect, access_revision)
			VALUES ($1, $2, $3, $4, $5, $6, $7, $8)
			ON CONFLICT (tenant_id, id) DO NOTHING`,
			tenantID, rowID, g.ObjectID, g.SubjectKind, userID, groupID,
			g.Effect, revision); err != nil {
			return fmt.Errorf("apply grant %s: %w", rowID, err)
		}
	}
	if _, err := tx.ExecContext(ctx, `
		UPDATE access_snapshots
		SET status = 'complete', completed_at = now(), unmapped_subjects = $4
		WHERE tenant_id = $1 AND connection_id = $2 AND kind = 'grants'
		  AND revision = $3 AND status = 'incomplete'`,
		tenantID, connectionID, revision, unmappedSubjects); err != nil {
		return fmt.Errorf("complete grants snapshot: %w", err)
	}
	// Retire grant rows on this connection's objects that were not
	// re-observed at this now-complete revision. Only a complete snapshot
	// reaches this statement; an incomplete revision deletes nothing.
	if _, err := tx.ExecContext(ctx, `
		DELETE FROM source_grants g
		USING source_objects o
		WHERE o.tenant_id = g.tenant_id AND o.id = g.object_id
		  AND g.tenant_id = $1 AND o.connection_id = $2
		  AND g.access_revision < $3
		  AND NOT EXISTS (
		      SELECT 1 FROM source_grants n
		      WHERE n.tenant_id = g.tenant_id AND n.object_id = g.object_id
		        AND n.subject_type = g.subject_type
		        AND COALESCE(n.source_identity_id,'') = COALESCE(g.source_identity_id,'')
		        AND COALESCE(n.source_group_id,'') = COALESCE(g.source_group_id,'')
		        AND n.effect = g.effect AND n.access_revision = $3)`,
		tenantID, connectionID, revision); err != nil {
		return fmt.Errorf("prune unseen grants: %w", err)
	}
	return nil
}

// ApplyMembershipSnapshot atomically records a membership observation and
// retires membership rows unseen at the now-complete revision.
func (r *SourceRepo) ApplyMembershipSnapshot(ctx context.Context, tx source.Tx, tenantID, connectionID string, revision int64, memberships []source.SourceGroupMembership, unmappedSubjects int) error {
	type member struct {
		groupID string
		member  string
	}
	seen := make(map[member]struct{}, len(memberships))
	for _, m := range memberships {
		key := member{groupID: m.GroupID, member: m.MemberID}
		if _, dup := seen[key]; dup {
			continue
		}
		seen[key] = struct{}{}
		rowID := fmt.Sprintf("ms:%s:%s:%d", m.GroupID, m.MemberID, revision)
		if _, err := tx.ExecContext(ctx, `
			INSERT INTO source_group_memberships
			    (tenant_id, id, group_id, source_identity_id, access_revision)
			VALUES ($1, $2, $3, $4, $5)
			ON CONFLICT (tenant_id, id) DO NOTHING`,
			tenantID, rowID, m.GroupID, m.MemberID, revision); err != nil {
			return fmt.Errorf("apply membership %s: %w", rowID, err)
		}
	}
	if _, err := tx.ExecContext(ctx, `
		UPDATE access_snapshots
		SET status = 'complete', completed_at = now(), unmapped_subjects = $4
		WHERE tenant_id = $1 AND connection_id = $2 AND kind = 'membership'
		  AND revision = $3 AND status = 'incomplete'`,
		tenantID, connectionID, revision, unmappedSubjects); err != nil {
		return fmt.Errorf("complete membership snapshot: %w", err)
	}
	if _, err := tx.ExecContext(ctx, `
		DELETE FROM source_group_memberships m
		USING source_groups g
		WHERE g.tenant_id = m.tenant_id AND g.id = m.group_id
		  AND m.tenant_id = $1 AND g.connection_id = $2
		  AND m.access_revision < $3
		  AND NOT EXISTS (
		      SELECT 1 FROM source_group_memberships n
		      WHERE n.tenant_id = m.tenant_id AND n.group_id = m.group_id
		        AND n.source_identity_id = m.source_identity_id
		        AND n.access_revision = $3)`,
		tenantID, connectionID, revision); err != nil {
		return fmt.Errorf("prune unseen memberships: %w", err)
	}
	return nil
}

// ---------------------------------------------------------------------
// Restrictions

// SetRestriction upserts a server-owned restriction row. Widening is not
// expressible here — callers may only add denied capabilities or raise the
// required assurance.
func (r *SourceRepo) SetRestriction(ctx context.Context, rs source.SourceRestriction) error {
	denied, err := jsonList(rs.DeniedCapabilities)
	if err != nil {
		return fmt.Errorf("marshal denied capabilities: %w", err)
	}
	labels, err := jsonList(rs.PolicyLabels)
	if err != nil {
		return fmt.Errorf("marshal policy labels: %w", err)
	}
	rowID := fmt.Sprintf("rs:%s:%s", rs.SubjectKind, rs.SubjectID)
	_, err = r.db.SQL.ExecContext(ctx, `
		INSERT INTO source_restrictions
		    (tenant_id, id, subject_type, subject_id, required_assurance,
		     denied_capabilities, policy_labels)
		VALUES ($1, $2, $3, $4, $5, $6, $7)
		ON CONFLICT (tenant_id, id) DO UPDATE SET
		    required_assurance  = EXCLUDED.required_assurance,
		    denied_capabilities = EXCLUDED.denied_capabilities,
		    policy_labels       = EXCLUDED.policy_labels`,
		rs.TenantID, rowID, rs.SubjectKind, rs.SubjectID,
		nullableText(rs.RequiredAssurance), denied, labels)
	if err != nil {
		return fmt.Errorf("set restriction: %w", err)
	}
	return nil
}

func (r *SourceRepo) RestrictionsForSubject(ctx context.Context, tenantID, subjectKind, subjectID string) ([]source.SourceRestriction, error) {
	rows, err := r.db.SQL.QueryContext(ctx, `
		SELECT subject_type, subject_id, COALESCE(required_assurance,''),
		       denied_capabilities, policy_labels
		FROM source_restrictions
		WHERE tenant_id = $1 AND subject_type = $2 AND subject_id = $3`,
		tenantID, subjectKind, subjectID)
	if err != nil {
		return nil, fmt.Errorf("restrictions for subject: %w", err)
	}
	defer func() { _ = rows.Close() }()
	out := []source.SourceRestriction{}
	for rows.Next() {
		var rs source.SourceRestriction
		var denied, labels []byte
		if err := rows.Scan(&rs.SubjectKind, &rs.SubjectID, &rs.RequiredAssurance,
			&denied, &labels); err != nil {
			return nil, fmt.Errorf("scan restriction: %w", err)
		}
		rs.TenantID = tenantID
		rs.DeniedCapabilities = scanJSONList(denied)
		rs.PolicyLabels = scanJSONList(labels)
		out = append(out, rs)
	}
	return out, rows.Err()
}
