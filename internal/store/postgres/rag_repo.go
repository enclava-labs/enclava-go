package postgres

import (
	"context"
	"crypto/rand"
	"database/sql"
	"encoding/base64"
	"encoding/json"
	"errors"
	"fmt"
	"strings"
	"time"

	"enclava-go/internal/jobs"
	"enclava-go/internal/rag"
	"enclava-go/internal/source"
)

func newPGID(prefix string) string {
	buf := make([]byte, 12)
	_, _ = rand.Read(buf)
	return prefix + "_" + base64.RawURLEncoding.EncodeToString(buf)
}

// RAGRepo implements rag.Repository over the canonical source/object/version
// tables plus the domain knowledge_collections, collection_items,
// chunk_profiles, chunks and index_generations schema. Tenant scoping is on
// every statement; authorization lives in the service layer via
// source.Authorizer and Collection audience/owner checks.
type RAGRepo struct {
	*SourceRepo
	db *DB
}

func NewRAGRepo(db *DB) *RAGRepo {
	return &RAGRepo{SourceRepo: NewSourceRepo(db), db: db}
}

var _ rag.Repository = (*RAGRepo)(nil)

// ---------------------------------------------------------------------
// Connections — full CRUD for the connectors service (UpdateConnection and
// DeleteConnection extend the narrow authorizer seam).

func (r *SourceRepo) UpdateConnection(ctx context.Context, c source.Connection) error {
	config, err := json.Marshal(map[string]any(c.Config))
	if err != nil {
		return fmt.Errorf("marshal connection config: %w", err)
	}
	audience, err := jsonList(c.AudienceDefault)
	if err != nil {
		return fmt.Errorf("marshal connection audience: %w", err)
	}
	res, err := r.db.SQL.ExecContext(ctx, `
		UPDATE connections SET name = $3, credential_ref = $4, config = $5::jsonb,
		       target_collection_id = $6, audience_default = $7::jsonb, status = $8
		WHERE tenant_id = $1 AND id = $2`,
		c.TenantID, c.ID, c.Name, c.CredentialRef, config,
		nullableText(c.TargetCollectionID), audience, c.Status)
	if err != nil {
		return fmt.Errorf("update connection: %w", err)
	}
	if n, _ := res.RowsAffected(); n == 0 {
		return source.ErrDenied
	}
	return nil
}

func (r *SourceRepo) DeleteConnection(ctx context.Context, tenantID, id string) error {
	res, err := r.db.SQL.ExecContext(ctx,
		`DELETE FROM connections WHERE tenant_id = $1 AND id = $2`, tenantID, id)
	if err != nil {
		return fmt.Errorf("delete connection: %w", err)
	}
	if n, _ := res.RowsAffected(); n == 0 {
		return source.ErrDenied
	}
	return nil
}

// ---------------------------------------------------------------------
// Collections

func (r *RAGRepo) InsertCollection(ctx context.Context, c rag.Collection) error {
	meta := map[string]any{}
	for k, v := range c.Metadata {
		meta[k] = v
	}
	meta["_acl"] = c.ACL
	meta["_chunk_size"] = c.ChunkSize
	meta["_chunk_overlap"] = c.ChunkOverlap
	metaBytes, err := json.Marshal(meta)
	if err != nil {
		return fmt.Errorf("marshal collection metadata: %w", err)
	}
	aud, err := jsonList(c.Audience)
	if err != nil {
		return fmt.Errorf("marshal collection audience: %w", err)
	}
	profileVersionID, err := r.EnsureChunkProfile(ctx, c.TenantID, "rag-"+c.ID, c.ChunkSize, c.ChunkOverlap)
	if err != nil {
		return err
	}
	_, err = r.db.SQL.ExecContext(ctx, `
		INSERT INTO knowledge_collections
		    (tenant_id, id, name, description, embedding_model, qdrant_collection,
		     chunk_profile_version_id, audience, metadata, owner_actor_id, is_active)
		VALUES ($1, $2, $3, $4, $5, $6, $7, $8::jsonb, $9::jsonb, $10, $11)`,
		c.TenantID, c.ID, c.Name, c.Description, c.EmbeddingModel, c.QdrantCollection,
		profileVersionID, aud, metaBytes, nullableText(c.OwnerActorID), c.IsActive)
	if err != nil {
		return fmt.Errorf("insert collection: %w", err)
	}
	return nil
}

func scanCollection(scanner interface {
	Scan(dest ...any) error
}) (rag.Collection, error) {
	var c rag.Collection
	var desc, qdrant, owner, profile sql.NullString
	var audience, meta []byte
	var createdAt time.Time
	err := scanner.Scan(&c.TenantID, &c.ID, &c.Name, &desc, &c.EmbeddingModel, &qdrant,
		&audience, &meta, &owner, &c.IsActive, &createdAt, &profile)
	if err != nil {
		return rag.Collection{}, err
	}
	c.Description = desc.String
	c.QdrantCollection = qdrant.String
	c.ChunkProfileVersionID = profile.String
	c.OwnerActorID = owner.String
	c.Audience = scanJSONList(audience)
	c.CreatedAt = createdAt
	c.UpdatedAt = createdAt
	var metaMap map[string]any
	if err := json.Unmarshal(meta, &metaMap); err == nil && metaMap != nil {
		if acl, ok := metaMap["_acl"].(map[string]any); ok {
			c.ACL = acl
		}
		if n, ok := metaMap["_chunk_size"].(float64); ok {
			c.ChunkSize = int(n)
		}
		if n, ok := metaMap["_chunk_overlap"].(float64); ok {
			c.ChunkOverlap = int(n)
		}
		delete(metaMap, "_acl")
		delete(metaMap, "_chunk_size")
		delete(metaMap, "_chunk_overlap")
	}
	c.Metadata = metaMap
	if c.Metadata == nil {
		c.Metadata = map[string]any{}
	}
	if c.ACL == nil {
		c.ACL = map[string]any{"public": false}
	}
	return c, nil
}

const collectionSelect = `
	SELECT tenant_id::text, id, name, description, embedding_model,
	       qdrant_collection, audience, metadata, owner_actor_id, is_active, created_at,
	       COALESCE(chunk_profile_version_id, '')
	FROM knowledge_collections`

func (r *RAGRepo) GetCollection(ctx context.Context, tenantID, id string) (rag.Collection, error) {
	c, err := scanCollection(r.db.SQL.QueryRowContext(ctx,
		collectionSelect+` WHERE tenant_id = $1 AND id = $2`, tenantID, id))
	if errors.Is(err, sql.ErrNoRows) || isInvalidUUID(err) {
		return rag.Collection{}, source.ErrDenied
	}
	if err != nil {
		return rag.Collection{}, fmt.Errorf("get collection: %w", err)
	}
	return c, nil
}

func (r *RAGRepo) ListCollections(ctx context.Context, tenantID string) ([]rag.Collection, error) {
	rows, err := r.db.SQL.QueryContext(ctx,
		collectionSelect+` WHERE tenant_id = $1 ORDER BY id`, tenantID)
	if err != nil {
		return nil, fmt.Errorf("list collections: %w", err)
	}
	defer func() { _ = rows.Close() }()
	var out []rag.Collection
	for rows.Next() {
		c, err := scanCollection(rows)
		if err != nil {
			return nil, err
		}
		out = append(out, c)
	}
	return out, rows.Err()
}

func (r *RAGRepo) UpdateCollection(ctx context.Context, c rag.Collection) error {
	meta := map[string]any{}
	for k, v := range c.Metadata {
		meta[k] = v
	}
	meta["_acl"] = c.ACL
	meta["_chunk_size"] = c.ChunkSize
	meta["_chunk_overlap"] = c.ChunkOverlap
	metaBytes, err := json.Marshal(meta)
	if err != nil {
		return fmt.Errorf("marshal collection metadata: %w", err)
	}
	aud, err := jsonList(c.Audience)
	if err != nil {
		return fmt.Errorf("marshal collection audience: %w", err)
	}
	res, err := r.db.SQL.ExecContext(ctx, `
		UPDATE knowledge_collections SET name = $3, description = $4, embedding_model = $5,
		       audience = $6::jsonb, metadata = $7::jsonb, is_active = $8,
		       chunk_profile_version_id = $9
		WHERE tenant_id = $1 AND id = $2`,
		c.TenantID, c.ID, c.Name, c.Description, c.EmbeddingModel, aud, metaBytes,
		c.IsActive, nullableText(c.ChunkProfileVersionID))
	if err != nil {
		return fmt.Errorf("update collection: %w", err)
	}
	if n, _ := res.RowsAffected(); n == 0 {
		return source.ErrDenied
	}
	return nil
}

func (r *RAGRepo) DeleteCollection(ctx context.Context, tenantID, id string) error {
	res, err := r.db.SQL.ExecContext(ctx,
		`DELETE FROM knowledge_collections WHERE tenant_id = $1 AND id = $2`, tenantID, id)
	if err != nil {
		return fmt.Errorf("delete collection: %w", err)
	}
	if n, _ := res.RowsAffected(); n == 0 {
		return source.ErrDenied
	}
	return nil
}

// EnsureChunkProfile returns the immutable chunk_profiles row id for the
// (profileID, size, overlap) triple, inserting a new version when the
// parameters changed. Changing a profile creates a new processing version —
// published evidence is never silently mutated.
func (r *RAGRepo) EnsureChunkProfile(ctx context.Context, tenantID, profileID string, size, overlap int) (string, error) {
	var id string
	var version int
	err := r.db.SQL.QueryRowContext(ctx, `
		SELECT id, version FROM chunk_profiles
		WHERE tenant_id = $1 AND profile_id = $2
		ORDER BY version DESC LIMIT 1`, tenantID, profileID).Scan(&id, &version)
	switch {
	case errors.Is(err, sql.ErrNoRows):
		id = newPGID("cpv")
		if _, err := r.db.SQL.ExecContext(ctx, `
			INSERT INTO chunk_profiles
			    (tenant_id, id, profile_id, version, name, boundary_strategy,
			     max_size_codepoints, overlap_codepoints, table_handling)
			VALUES ($1, $2, $3, 1, $4, 'fixed', $5, $6, 'row-wise')`,
			tenantID, id, profileID, profileID, size, overlap); err != nil {
			return "", fmt.Errorf("insert chunk profile: %w", err)
		}
		return id, nil
	case err != nil:
		return "", fmt.Errorf("read chunk profile: %w", err)
	}
	var haveSize, haveOverlap int64
	if err := r.db.SQL.QueryRowContext(ctx, `
		SELECT max_size_codepoints, overlap_codepoints FROM chunk_profiles
		WHERE tenant_id = $1 AND id = $2`, tenantID, id).Scan(&haveSize, &haveOverlap); err != nil {
		return "", err
	}
	if int(haveSize) == size && int(haveOverlap) == overlap {
		return id, nil
	}
	id = newPGID("cpv")
	if _, err := r.db.SQL.ExecContext(ctx, `
		INSERT INTO chunk_profiles
		    (tenant_id, id, profile_id, version, name, boundary_strategy,
		     max_size_codepoints, overlap_codepoints, table_handling)
		VALUES ($1, $2, $3, $4, $5, 'fixed', $6, $7, 'row-wise')`,
		tenantID, id, profileID, version+1, profileID, size, overlap); err != nil {
		return "", fmt.Errorf("insert chunk profile version: %w", err)
	}
	return id, nil
}

// ---------------------------------------------------------------------
// Collection items + document projection

func (r *RAGRepo) AddCollectionItem(ctx context.Context, tenantID, collectionID, objectID, addedBy string) error {
	_, err := r.db.SQL.ExecContext(ctx, `
		INSERT INTO collection_items (tenant_id, id, collection_id, object_id, added_by)
		VALUES ($1, $2, $3, $4, $5)
		ON CONFLICT (tenant_id, collection_id, object_id) DO NOTHING`,
		tenantID, newPGID("citem"), collectionID, objectID, nullableText(addedBy))
	if err != nil {
		return fmt.Errorf("add collection item: %w", err)
	}
	return nil
}

func (r *RAGRepo) RemoveCollectionItem(ctx context.Context, tenantID, collectionID, objectID string) error {
	res, err := r.db.SQL.ExecContext(ctx, `
		DELETE FROM collection_items
		WHERE tenant_id = $1 AND collection_id = $2 AND object_id = $3`,
		tenantID, collectionID, objectID)
	if err != nil {
		return fmt.Errorf("remove collection item: %w", err)
	}
	if n, _ := res.RowsAffected(); n == 0 {
		return source.ErrDenied
	}
	return nil
}

func (r *RAGRepo) ListCollectionItems(ctx context.Context, tenantID, collectionID string) ([]string, error) {
	rows, err := r.db.SQL.QueryContext(ctx, `
		SELECT object_id FROM collection_items
		WHERE tenant_id = $1 AND collection_id = $2`, tenantID, collectionID)
	if err != nil {
		return nil, fmt.Errorf("list collection items: %w", err)
	}
	defer func() { _ = rows.Close() }()
	var out []string
	for rows.Next() {
		var id string
		if err := rows.Scan(&id); err != nil {
			return nil, err
		}
		out = append(out, id)
	}
	return out, rows.Err()
}

// documentSelect projects the legacy-stable rag.Document JSON shape: object
// row + latest version + latest index generation state + chunk count.
// latest_gen is scoped per (collection, object_version) so a generation in
// one collection cannot mask another, the chunk count spans only that latest
// generation of the listing collection, and a durable failed embed/ingest
// job projects its real error text.
const documentSelect = `
	WITH latest_version AS (
		SELECT DISTINCT ON (object_id) id, object_id, observed_seq, artifact_ref,
		       content_sha256, byte_size, mime_type, original_filename, provenance
		FROM source_object_versions
		WHERE tenant_id = $1
		ORDER BY object_id, observed_seq DESC
	),
	latest_gen AS (
		SELECT DISTINCT ON (collection_id, object_version_id)
		       collection_id, object_version_id, generation, state
		FROM index_generations
		WHERE tenant_id = $1
		ORDER BY collection_id, object_version_id, generation DESC
	)
	SELECT o.id, ci.collection_id, o.kind, o.native_id, o.observed_seq,
	       lv.id, lv.observed_seq, lv.artifact_ref, lv.content_sha256, lv.byte_size,
	       lv.mime_type, lv.original_filename, lv.provenance,
	       COALESCE(lg.state, 'staging'),
	       COALESCE(ci.removed_at, o.deleted_at, o.tombstoned_at) IS NOT NULL,
	       o.first_seen_at, o.last_seen_at,
	       (SELECT COUNT(*) FROM chunks k
	        WHERE k.tenant_id = $1 AND k.object_version_id = lv.id
	          AND k.collection_id = ci.collection_id AND k.generation = lg.generation),
	       j.error->>'message'
	FROM source_objects o
	JOIN collection_items ci ON ci.tenant_id = o.tenant_id AND ci.object_id = o.id
	LEFT JOIN latest_version lv ON lv.object_id = o.id
	LEFT JOIN latest_gen lg ON lg.object_version_id = lv.id AND lg.collection_id = ci.collection_id
	LEFT JOIN jobs j ON j.tenant_id = $1
	     AND j.operation IN ('` + rag.OperationEmbed + `', '` + rag.OperationIngest + `')
	     AND j.input_ref = lg.object_version_id
	     AND j.policy->>'generation' = lg.generation::text
	     AND j.status = 'failed'`

// documentStatusPredicate mirrors scanDocument's Go-side status derivation so
// the repo filter and the service projection can never disagree.
func documentStatusPredicate(status string) string {
	const (
		removed = `(COALESCE(ci.removed_at, o.deleted_at, o.tombstoned_at) IS NOT NULL)`
		failed  = `(j.id IS NOT NULL OR lg.state = 'failed')`
	)
	switch status {
	case rag.DocumentStatusRemoved:
		return removed
	case rag.DocumentStatusFailed:
		return `NOT ` + removed + ` AND ` + failed
	case rag.DocumentStatusReady:
		return `NOT ` + removed + ` AND NOT ` + failed + ` AND lg.state = 'published'`
	case rag.DocumentStatusProcessing:
		return `NOT ` + removed + ` AND NOT ` + failed + ` AND COALESCE(lg.state, 'staging') NOT IN ('published', 'failed')`
	default:
		return `NOT ` + removed
	}
}

func scanDocument(scanner interface {
	Scan(dest ...any) error
}) (rag.Document, error) {
	var d rag.Document
	var nativeID, artifactRef, sha, mime, filename, provenance sql.NullString
	var seq, verSeq sql.NullInt64
	var verID sql.NullString
	var byteSize sql.NullInt64
	var state string
	var gone bool
	var firstSeen, lastSeen time.Time
	var chunkCount int64
	var errorMessage sql.NullString
	err := scanner.Scan(&d.ID, &d.CollectionID, &d.SourceType, &nativeID, &seq,
		&verID, &verSeq, &artifactRef, &sha, &byteSize,
		&mime, &filename, &provenance,
		&state, &gone, &firstSeen, &lastSeen, &chunkCount, &errorMessage)
	if err != nil {
		return rag.Document{}, err
	}
	d.SourceID = nativeID.String
	d.ObservedSeq = verSeq.Int64
	d.ObjectVersionID = verID.String
	d.ArtifactRef = artifactRef.String
	d.TextHash = sha.String
	d.MimeType = mime.String
	d.FileName = filename.String
	d.CreatedAt = firstSeen
	d.UpdatedAt = lastSeen
	d.QdrantPointCount = int(chunkCount)
	if gone {
		// Management-visible tombstone metadata only: never re-expose
		// content fields for a removed document.
		d.Status = rag.DocumentStatusRemoved
		d.TextHash = ""
		d.Metadata = map[string]any{}
		d.QdrantPointCount = 0
		return d, nil
	}
	if errorMessage.Valid && errorMessage.String != "" {
		// The generation's durable embed/ingest job failed: carry the job's
		// real error text, never an invented string.
		d.Status = rag.DocumentStatusFailed
		d.ErrorMessage = errorMessage.String
	} else {
		switch state {
		case "failed":
			d.Status = rag.DocumentStatusFailed
		case "published":
			d.Status = rag.DocumentStatusReady
			now := d.UpdatedAt
			d.ProcessedAt = &now
		default:
			d.Status = rag.DocumentStatusProcessing
		}
	}
	var prov map[string]any
	if provenance.Valid {
		_ = json.Unmarshal([]byte(provenance.String), &prov)
	}
	if prov != nil {
		if v, ok := prov["source_uri"].(string); ok {
			d.SourceURI = v
		}
		if m, ok := prov["metadata"].(map[string]any); ok {
			d.Metadata = m
		}
	}
	if d.Metadata == nil {
		d.Metadata = map[string]any{}
	}
	return d, nil
}

func (r *RAGRepo) GetDocument(ctx context.Context, tenantID, objectID string) (rag.Document, error) {
	d, err := scanDocument(r.db.SQL.QueryRowContext(ctx,
		documentSelect+` WHERE o.tenant_id = $1 AND o.id = $2`, tenantID, objectID))
	if errors.Is(err, sql.ErrNoRows) || isInvalidUUID(err) {
		return rag.Document{}, source.ErrDenied
	}
	if err != nil {
		return rag.Document{}, fmt.Errorf("get document: %w", err)
	}
	if d.Status == rag.DocumentStatusRemoved {
		// Single-document reads treat removed objects as absent; the
		// management projection is list-only.
		return rag.Document{}, source.ErrDenied
	}
	d.TenantID = tenantID
	return d, nil
}

func (r *RAGRepo) ListDocuments(ctx context.Context, tenantID, collectionID, status string, limit, offset int) ([]rag.Document, error) {
	if limit <= 0 {
		limit = 50
	}
	rows, err := r.db.SQL.QueryContext(ctx,
		documentSelect+` WHERE o.tenant_id = $1 AND ci.collection_id = $2 AND `+documentStatusPredicate(status)+`
		 ORDER BY o.first_seen_at DESC, o.id, ci.id LIMIT $3 OFFSET $4`,
		tenantID, collectionID, limit, offset)
	if err != nil {
		return nil, fmt.Errorf("list documents: %w", err)
	}
	defer func() { _ = rows.Close() }()
	return collectDocuments(rows, tenantID)
}

// ListFailedDocuments is the raw tenant-wide failed-document projection — an
// internal building block for Service.ListFailedDocuments, which authorizes
// per principal BEFORE its limit. Never serve these rows to a caller
// directly: source ACLs are not applied here.
func (r *RAGRepo) ListFailedDocuments(ctx context.Context, tenantID string, limit, offset int) ([]rag.Document, error) {
	if limit <= 0 {
		limit = 50
	}
	rows, err := r.db.SQL.QueryContext(ctx,
		documentSelect+` WHERE o.tenant_id = $1 AND j.id IS NOT NULL
		 AND COALESCE(ci.removed_at, o.deleted_at, o.tombstoned_at) IS NULL
		 ORDER BY o.first_seen_at DESC, o.id, ci.id LIMIT $2 OFFSET $3`,
		tenantID, limit, offset)
	if err != nil {
		return nil, fmt.Errorf("list failed documents: %w", err)
	}
	defer func() { _ = rows.Close() }()
	return collectDocuments(rows, tenantID)
}

// ListTenantDocuments is the raw tenant-wide non-removed document projection —
// an internal building block for Service.CountDocuments, which authorizes per
// principal. Never serve these rows to a caller directly.
func (r *RAGRepo) ListTenantDocuments(ctx context.Context, tenantID string, limit, offset int) ([]rag.Document, error) {
	if limit <= 0 {
		limit = 50
	}
	rows, err := r.db.SQL.QueryContext(ctx,
		documentSelect+` WHERE o.tenant_id = $1
		 AND COALESCE(ci.removed_at, o.deleted_at, o.tombstoned_at) IS NULL
		 ORDER BY o.first_seen_at DESC, o.id, ci.id LIMIT $2 OFFSET $3`,
		tenantID, limit, offset)
	if err != nil {
		return nil, fmt.Errorf("list tenant documents: %w", err)
	}
	defer func() { _ = rows.Close() }()
	return collectDocuments(rows, tenantID)
}

// CountDocuments counts tenant documents; excludeRemoved drops the removed
// tombstones. Raw tenant-wide building block — never served raw to a caller
// (no management exception: knowledge.manage derives from rag.manage and
// unlocks nothing here).
func (r *RAGRepo) CountDocuments(ctx context.Context, tenantID string, excludeRemoved bool) (int64, error) {
	q := `SELECT COUNT(DISTINCT o.id) FROM source_objects o
		JOIN collection_items ci ON ci.tenant_id = o.tenant_id AND ci.object_id = o.id
		WHERE o.tenant_id = $1`
	if excludeRemoved {
		q += ` AND COALESCE(ci.removed_at, o.deleted_at, o.tombstoned_at) IS NULL`
	}
	var n int64
	if err := r.db.SQL.QueryRowContext(ctx, q, tenantID).Scan(&n); err != nil {
		return 0, fmt.Errorf("count documents: %w", err)
	}
	return n, nil
}

// MarkCollectionItemRemoved writes the soft tombstone: the collection item
// row stays as management metadata while every member read-path excludes it.
func (r *RAGRepo) MarkCollectionItemRemoved(ctx context.Context, tenantID, collectionID, objectID string) error {
	res, err := r.db.SQL.ExecContext(ctx, `
		UPDATE collection_items SET removed_at = now()
		WHERE tenant_id = $1 AND collection_id = $2 AND object_id = $3 AND removed_at IS NULL`,
		tenantID, collectionID, objectID)
	if err != nil {
		return fmt.Errorf("mark collection item removed: %w", err)
	}
	if n, _ := res.RowsAffected(); n == 0 {
		return source.ErrDenied
	}
	return nil
}

// ListBlockLocators returns the retained locators for parsed blocks — the
// single resolution path for citation locations. Only persisted fields are
// ever surfaced; missing blocks resolve to nothing.
func (r *RAGRepo) ListBlockLocators(ctx context.Context, tenantID string, blockIDs []string) (map[string]source.Locator, error) {
	out := make(map[string]source.Locator, len(blockIDs))
	if len(blockIDs) == 0 {
		return out, nil
	}
	placeholders := make([]string, len(blockIDs))
	args := make([]any, 0, len(blockIDs)+1)
	args = append(args, tenantID)
	for i, id := range blockIDs {
		placeholders[i] = fmt.Sprintf("$%d", i+2)
		args = append(args, id)
	}
	rows, err := r.db.SQL.QueryContext(ctx,
		`SELECT id, locator FROM blocks WHERE tenant_id = $1 AND id IN (`+strings.Join(placeholders, ",")+`)`,
		args...)
	if err != nil {
		return nil, fmt.Errorf("list block locators: %w", err)
	}
	defer func() { _ = rows.Close() }()
	for rows.Next() {
		var id string
		var raw []byte
		if err := rows.Scan(&id, &raw); err != nil {
			return nil, err
		}
		var locator source.Locator
		if len(raw) > 0 {
			_ = json.Unmarshal(raw, &locator)
		}
		out[id] = locator
	}
	return out, rows.Err()
}

func collectDocuments(rows *sql.Rows, tenantID string) ([]rag.Document, error) {
	var out []rag.Document
	for rows.Next() {
		d, err := scanDocument(rows)
		if err != nil {
			return nil, err
		}
		d.TenantID = tenantID
		out = append(out, d)
	}
	return out, rows.Err()
}

// LiveCollectionSourceIDs returns the native source ids currently present in
// a collection (not deleted or tombstoned). Connector syncs use it so an
// unchanged-content skip never fires for a document the operator deleted —
// the next sync must recreate it.
func (r *RAGRepo) LiveCollectionSourceIDs(ctx context.Context, tenantID, collectionID string) (map[string]struct{}, error) {
	rows, err := r.db.SQL.QueryContext(ctx, `
		SELECT o.native_id FROM source_objects o
		JOIN collection_items ci ON ci.tenant_id = o.tenant_id AND ci.object_id = o.id
		WHERE o.tenant_id = $1 AND ci.collection_id = $2
		  AND o.deleted_at IS NULL AND o.tombstoned_at IS NULL`,
		tenantID, collectionID)
	if err != nil {
		return nil, fmt.Errorf("live collection source ids: %w", err)
	}
	defer func() { _ = rows.Close() }()
	out := make(map[string]struct{})
	for rows.Next() {
		var nativeID string
		if err := rows.Scan(&nativeID); err != nil {
			return nil, fmt.Errorf("live collection source ids: %w", err)
		}
		out[nativeID] = struct{}{}
	}
	return out, rows.Err()
}

// ---------------------------------------------------------------------
// Generations + chunks

// NextGeneration returns the next index generation number for (collection,
// object_version): max(existing)+1, so a reprocess is a new generation and a
// stale worker's generation can never be the live one.
func (r *RAGRepo) NextGeneration(ctx context.Context, tenantID, collectionID, versionID string) (int64, error) {
	var gen int64
	err := r.db.SQL.QueryRowContext(ctx, `
		SELECT COALESCE(MAX(generation), 0) + 1 FROM index_generations
		WHERE tenant_id = $1 AND collection_id = $2 AND object_version_id = $3`,
		tenantID, collectionID, versionID).Scan(&gen)
	if err != nil {
		return 0, fmt.Errorf("next generation: %w", err)
	}
	return gen, nil
}

func (r *RAGRepo) SaveParsedDocument(ctx context.Context, parserVersion string, document source.Document) error {
	return WithTx(ctx, r.db.SQL, func(tx *sql.Tx) error {
		versionID := fmt.Sprintf("%s#%d", document.Ref.ObjectID, document.Ref.ObservedSeq)
		var existing string
		err := tx.QueryRowContext(ctx, `
			SELECT id FROM parsed_documents
			WHERE tenant_id = $1 AND object_version_id = $2 AND parser = 'rag-structured' AND parser_version = $3
			ORDER BY created_at DESC LIMIT 1`,
			document.Ref.TenantID, versionID, parserVersion).Scan(&existing)
		if err == nil {
			return nil
		}
		if err != sql.ErrNoRows {
			return fmt.Errorf("find parsed document: %w", err)
		}
		id := newPGID("parsed")
		if _, err := tx.ExecContext(ctx, `
			INSERT INTO parsed_documents (tenant_id, id, object_version_id, parser, parser_version, block_count)
			VALUES ($1, $2, $3, 'rag-structured', $4, $5)`,
			document.Ref.TenantID, id, versionID, parserVersion, len(document.Blocks)); err != nil {
			return fmt.Errorf("insert parsed document: %w", err)
		}
		for _, block := range document.Blocks {
			locator, err := json.Marshal(block.Locator)
			if err != nil {
				return fmt.Errorf("marshal block locator: %w", err)
			}
			var parent any
			if block.ParentID != "" {
				parent = block.ParentID
			}
			if _, err := tx.ExecContext(ctx, `
				INSERT INTO blocks (tenant_id, id, parsed_document_id, ordinal, kind, parent_id, text, byte_start, byte_end, locator)
				VALUES ($1, $2, $3, $4, $5, $6, $7, $8, $9, $10::jsonb)`,
				document.Ref.TenantID, block.ID, id, block.Ordinal, block.Kind, parent,
				block.Text, block.ByteStart, block.ByteEnd, locator); err != nil {
				return fmt.Errorf("insert parsed block: %w", err)
			}
		}
		return nil
	})
}

// ReplaceVersionChunksAndStage inserts generation-keyed chunk rows and stages
// the generation atomically. Only its own still-staged rows may be retried.
func (r *RAGRepo) ReplaceVersionChunksAndStage(ctx context.Context, tenantID, collectionID, objectID, versionID string, seq, generation int64, profileVersionID string, chunks []rag.DocumentChunk) error {
	return WithTx(ctx, r.db.SQL, func(tx *sql.Tx) error {
		_, err := tx.ExecContext(ctx, `
			INSERT INTO index_generations
			    (tenant_id, id, collection_id, object_version_id, generation, state,
			     chunk_profile_version_id, artifact_ref)
			VALUES ($1, $2, $3, $4, $5, 'staging', NULLIF($6, ''), COALESCE(
			    (SELECT artifact_ref FROM source_object_versions
			     WHERE tenant_id=$1 AND id=$4), ''))
			ON CONFLICT (tenant_id, collection_id, object_version_id, generation) DO NOTHING`,
			tenantID, newPGID("idxgen"), collectionID, versionID, generation, profileVersionID)
		if err != nil {
			return fmt.Errorf("stage generation: %w", err)
		}
		var state string
		if err := tx.QueryRowContext(ctx, `
			SELECT state FROM index_generations
			WHERE tenant_id=$1 AND collection_id=$2 AND object_version_id=$3 AND generation=$4
			FOR UPDATE`, tenantID, collectionID, versionID, generation).Scan(&state); err != nil {
			return fmt.Errorf("read staged generation: %w", err)
		}
		if state != "staging" {
			return fmt.Errorf("generation %d is immutable in state %q", generation, state)
		}
		if _, err := tx.ExecContext(ctx, `
			DELETE FROM chunks WHERE tenant_id=$1 AND collection_id=$2
			  AND object_version_id=$3 AND generation=$4`,
			tenantID, collectionID, versionID, generation); err != nil {
			return fmt.Errorf("replace staged chunks: %w", err)
		}
		for _, ch := range chunks {
			spanRefs, err := json.Marshal(ch.SpanRefs)
			if err != nil {
				return fmt.Errorf("marshal chunk spans: %w", err)
			}
			locator, err := json.Marshal(ch.Metadata)
			if err != nil {
				return fmt.Errorf("marshal chunk locator: %w", err)
			}
			start, end := ch.ByteStart, ch.ByteEnd
			if len(ch.SpanRefs) == 0 && end == 0 {
				end = len(ch.Content)
			}
			if _, err := tx.ExecContext(ctx, `
				INSERT INTO chunks
				    (tenant_id, id, object_version_id, chunk_profile_version_id,
				     ordinal, text, byte_start, byte_end, span_refs, owner_actor_id,
				     collection_id, generation, block_path, locator, qdrant_point_id)
				VALUES ($1,$2,$3,$4,$5,$6,$7,$8,$9::jsonb,NULL,$10,$11,$12,$13::jsonb,$14)`,
				tenantID, ch.ID, versionID, profileVersionID, ch.ChunkIndex,
				ch.Content, start, end, spanRefs, collectionID, generation, ch.HeaderPath, locator, ch.QdrantPointID); err != nil {
				return fmt.Errorf("insert generation chunk: %w", err)
			}
		}
		return nil
	})
}

func (r *RAGRepo) PublishGenerationFenced(ctx context.Context, tenantID, collectionID, objectID, versionID string, generation int64, jobID, leaseOwner, attemptID string, fenceToken int64) error {
	return WithTx(ctx, r.db.SQL, func(tx *sql.Tx) error {
		var state string
		err := tx.QueryRowContext(ctx, `
			SELECT g.state
			FROM index_generations g
			JOIN source_object_versions v ON v.tenant_id=g.tenant_id AND v.id=g.object_version_id
			JOIN source_objects o ON o.tenant_id=v.tenant_id AND o.id=v.object_id
			JOIN jobs j ON j.tenant_id=g.tenant_id
			WHERE g.tenant_id=$1 AND g.collection_id=$2 AND g.object_version_id=$3
			  AND g.generation=$4 AND o.id=$5 AND o.deleted_at IS NULL AND o.tombstoned_at IS NULL
			  AND o.observed_seq=v.observed_seq
			  AND j.id=$6 AND j.status='running' AND j.lease_owner=$7
			  AND j.fence_token=$8 AND j.dispatched_attempt_id=$9
			  AND j.lease_expires_at > now()
			FOR UPDATE OF g`, tenantID, collectionID, versionID, generation, objectID, jobID, leaseOwner, fenceToken, attemptID).Scan(&state)
		if err != nil {
			return fmt.Errorf("generation publication fence: %w", err)
		}
		if state != "staging" {
			return fmt.Errorf("generation is not staged: %s", state)
		}
		if _, err := tx.ExecContext(ctx, `
			INSERT INTO index_reconciliation
			    (tenant_id, collection_id, object_version_id, generation, fence_token, state)
			SELECT tenant_id, collection_id, object_version_id, generation, lease_fence, 'pending'
			FROM index_generations
			WHERE tenant_id=$1 AND collection_id=$2 AND state='published'
			  AND object_version_id IN (
			    SELECT id FROM source_object_versions WHERE tenant_id=$1 AND object_id=$3
			  )
			ON CONFLICT (tenant_id, collection_id, object_version_id, generation)
			DO UPDATE SET state='pending', fence_token=EXCLUDED.fence_token`,
			tenantID, collectionID, objectID); err != nil {
			return fmt.Errorf("queue superseded generation cleanup: %w", err)
		}
		if _, err := tx.ExecContext(ctx, `
			UPDATE index_generations old SET state='superseded'
			FROM source_object_versions ov
			WHERE old.tenant_id=$1 AND old.collection_id=$2 AND old.state='published'
			  AND ov.tenant_id=old.tenant_id AND ov.id=old.object_version_id
			  AND ov.object_id=$3`, tenantID, collectionID, objectID); err != nil {
			return fmt.Errorf("supersede previous generation: %w", err)
		}
		res, err := tx.ExecContext(ctx, `
			UPDATE index_generations SET state='published', published_at=now(), lease_fence=$6
			WHERE tenant_id=$1 AND collection_id=$2 AND object_version_id=$3 AND generation=$4
			  AND state='staging' AND EXISTS (
			    SELECT 1 FROM jobs WHERE tenant_id=$1 AND id=$5 AND status='running'
			      AND lease_owner=$7 AND fence_token=$6 AND dispatched_attempt_id=$8
			      AND lease_expires_at > now())`,
			tenantID, collectionID, versionID, generation, jobID, fenceToken, leaseOwner, attemptID)
		if err != nil {
			return fmt.Errorf("publish generation: %w", err)
		}
		if n, _ := res.RowsAffected(); n != 1 {
			return jobs.ErrStaleFence
		}
		return nil
	})
}

func (r *RAGRepo) MarkGenerationState(ctx context.Context, tenantID, collectionID, versionID string, generation int64, state string) error {
	res, err := r.db.SQL.ExecContext(ctx, `
		UPDATE index_generations SET state = $5,
		       published_at = CASE WHEN $5 = 'published' THEN now() ELSE published_at END
		WHERE tenant_id = $1 AND collection_id = $2 AND object_version_id = $3 AND generation = $4`,
		tenantID, collectionID, versionID, generation, state)
	if err != nil {
		return fmt.Errorf("mark generation state: %w", err)
	}
	if n, _ := res.RowsAffected(); n == 0 {
		return fmt.Errorf("generation not found")
	}
	return nil
}

func (r *RAGRepo) ChunkCountForVersion(ctx context.Context, tenantID, versionID string) (int, error) {
	var n int
	err := r.db.SQL.QueryRowContext(ctx, `
		SELECT COUNT(*) FROM chunks WHERE tenant_id = $1 AND object_version_id = $2`,
		tenantID, versionID).Scan(&n)
	if err != nil {
		return 0, fmt.Errorf("count chunks: %w", err)
	}
	return n, nil
}

func (r *RAGRepo) ListChunks(ctx context.Context, tenantID, objectID, versionID string) ([]rag.DocumentChunk, error) {
	rows, err := r.db.SQL.QueryContext(ctx, `
		SELECT k.id, v.object_id, ci.collection_id, k.ordinal, k.text, k.created_at,
		       k.byte_start, k.byte_end, k.span_refs, k.generation, k.qdrant_point_id
		FROM chunks k
		JOIN source_object_versions v ON v.tenant_id = k.tenant_id AND v.id = k.object_version_id
		JOIN collection_items ci ON ci.tenant_id = k.tenant_id AND ci.object_id = v.object_id
		WHERE k.tenant_id = $1 AND k.object_version_id = $3 AND v.object_id=$2
		ORDER BY k.ordinal`, tenantID, objectID, versionID)
	if err != nil {
		return nil, fmt.Errorf("list chunks: %w", err)
	}
	defer func() { _ = rows.Close() }()
	var out []rag.DocumentChunk
	for rows.Next() {
		var ch rag.DocumentChunk
		var collectionID sql.NullString
		var spans []byte
		if err := rows.Scan(&ch.ID, &ch.DocumentID, &collectionID, &ch.ChunkIndex, &ch.Content, &ch.CreatedAt,
			&ch.ByteStart, &ch.ByteEnd, &spans, &ch.Generation, &ch.QdrantPointID); err != nil {
			return nil, err
		}
		if err := json.Unmarshal(spans, &ch.SpanRefs); err != nil {
			return nil, fmt.Errorf("decode chunk spans: %w", err)
		}
		ch.CollectionID = collectionID.String
		ch.TokenCount = len(ch.Content) / 4
		out = append(out, ch)
	}
	return out, rows.Err()
}

func (r *RAGRepo) ListGenerationsForConnection(ctx context.Context, tenantID, connectionID string) ([]rag.IndexGeneration, error) {
	rows, err := r.db.SQL.QueryContext(ctx, `
		SELECT g.collection_id, c.qdrant_collection, o.id, v.id, g.generation,
		       g.state, (o.deleted_at IS NOT NULL OR o.tombstoned_at IS NOT NULL)
		FROM index_generations g
		JOIN source_object_versions v ON v.tenant_id=g.tenant_id AND v.id=g.object_version_id
		JOIN source_objects o ON o.tenant_id=v.tenant_id AND o.id=v.object_id
		JOIN knowledge_collections c ON c.tenant_id=g.tenant_id AND c.id=g.collection_id
		WHERE g.tenant_id=$1 AND o.connection_id=$2`, tenantID, connectionID)
	if err != nil {
		return nil, fmt.Errorf("list connection generations: %w", err)
	}
	defer rows.Close()
	var result []rag.IndexGeneration
	for rows.Next() {
		var g rag.IndexGeneration
		if err := rows.Scan(&g.CollectionID, &g.QdrantCollection, &g.ObjectID, &g.ObjectVersionID, &g.Generation, &g.State, &g.Tombstoned); err != nil {
			return nil, fmt.Errorf("scan connection generation: %w", err)
		}
		result = append(result, g)
	}
	if err := rows.Err(); err != nil {
		return nil, fmt.Errorf("iterate connection generations: %w", err)
	}
	return result, nil
}

func (r *RAGRepo) FindArtifactObjectVersion(ctx context.Context, tenantID, artifactID string) (source.Object, source.ObjectVersion, error) {
	var objectID string
	var seq int64
	err := r.db.SQL.QueryRowContext(ctx, `
		SELECT object_id, observed_seq FROM source_object_versions
		WHERE tenant_id=$1 AND artifact_ref=$2 ORDER BY observed_seq DESC LIMIT 1`,
		tenantID, artifactID).Scan(&objectID, &seq)
	if errors.Is(err, sql.ErrNoRows) {
		return source.Object{}, source.ObjectVersion{}, source.ErrDenied
	}
	if err != nil {
		return source.Object{}, source.ObjectVersion{}, fmt.Errorf("find artifact source: %w", err)
	}
	object, err := r.GetObject(ctx, tenantID, objectID)
	if err != nil {
		return source.Object{}, source.ObjectVersion{}, err
	}
	version, err := r.GetObjectVersion(ctx, tenantID, objectID, seq)
	return object, version, err
}

func (r *RAGRepo) GetStoredResult(ctx context.Context, tenantID, jobID string) (rag.StoredResult, error) {
	var result rag.StoredResult
	err := r.db.SQL.QueryRowContext(ctx, `
		SELECT operation, status, COALESCE(input_ref,''), COALESCE(result_ref,'')
		FROM jobs WHERE tenant_id=$1 AND id=$2 AND deleted_at IS NULL`,
		tenantID, jobID).Scan(&result.Operation, &result.Status, &result.InputRef, &result.ResultRef)
	if err != nil {
		return rag.StoredResult{}, fmt.Errorf("read stored result: %w", err)
	}
	return result, nil
}

func (r *RAGRepo) ReserveKnowledgeImport(ctx context.Context, tenantID, collectionID, importRef, objectID, kind string) (string, bool, error) {
	_, err := r.db.SQL.ExecContext(ctx, `
		INSERT INTO knowledge_imports (tenant_id, collection_id, import_ref, object_id, provenance_kind, state)
		VALUES ($1,$2,$3,$4,$5,'reserved')
		ON CONFLICT (tenant_id, collection_id, import_ref) DO NOTHING`,
		tenantID, collectionID, importRef, objectID, kind)
	if err != nil {
		return "", false, fmt.Errorf("reserve knowledge import: %w", err)
	}
	var reservedID, state string
	err = r.db.SQL.QueryRowContext(ctx, `
		SELECT object_id, state FROM knowledge_imports
		WHERE tenant_id=$1 AND collection_id=$2 AND import_ref=$3`,
		tenantID, collectionID, importRef).Scan(&reservedID, &state)
	if err != nil {
		return "", false, fmt.Errorf("read knowledge import reservation: %w", err)
	}
	return reservedID, state == "complete", nil
}

func (r *RAGRepo) RecordKnowledgeImport(ctx context.Context, tenantID, collectionID, importRef, objectID, kind string) error {
	res, err := r.db.SQL.ExecContext(ctx, `
		UPDATE knowledge_imports SET state='complete'
		WHERE tenant_id=$1 AND collection_id=$2 AND import_ref=$3 AND object_id=$4 AND provenance_kind=$5`,
		tenantID, collectionID, importRef, objectID, kind)
	if err != nil {
		return fmt.Errorf("record knowledge import: %w", err)
	}
	if n, _ := res.RowsAffected(); n != 1 {
		return errors.New("record knowledge import: reservation missing or mismatched")
	}
	return nil
}
func (r *RAGRepo) ListPendingIndexReconciliations(ctx context.Context, tenantID string, limit int) ([]rag.IndexReconciliation, error) {
	if limit <= 0 {
		limit = 100
	}
	rows, err := r.db.SQL.QueryContext(ctx, `
		SELECT ir.tenant_id::text, ir.collection_id, c.qdrant_collection,
		       ir.object_version_id, ir.generation, ir.fence_token
		FROM index_reconciliation ir
		JOIN index_generations g ON g.tenant_id=ir.tenant_id
		  AND g.collection_id=ir.collection_id AND g.object_version_id=ir.object_version_id
		  AND g.generation=ir.generation
		JOIN knowledge_collections c ON c.tenant_id=ir.tenant_id AND c.id=ir.collection_id
		WHERE ir.tenant_id=$1 AND ir.state='pending' AND g.state='superseded'
		ORDER BY ir.created_at LIMIT $2`, tenantID, limit)
	if err != nil {
		return nil, fmt.Errorf("list pending index reconciliations: %w", err)
	}
	defer rows.Close()
	var pending []rag.IndexReconciliation
	for rows.Next() {
		var item rag.IndexReconciliation
		if err := rows.Scan(&item.TenantID, &item.CollectionID, &item.QdrantCollection,
			&item.ObjectVersionID, &item.Generation, &item.FenceToken); err != nil {
			return nil, fmt.Errorf("scan pending index reconciliation: %w", err)
		}
		pending = append(pending, item)
	}
	if err := rows.Err(); err != nil {
		return nil, fmt.Errorf("iterate pending index reconciliations: %w", err)
	}
	return pending, nil
}

func (r *RAGRepo) CompleteIndexReconciliation(ctx context.Context, item rag.IndexReconciliation) error {
	res, err := r.db.SQL.ExecContext(ctx, `
		UPDATE index_reconciliation SET state='complete'
		WHERE tenant_id=$1 AND collection_id=$2 AND object_version_id=$3
		  AND generation=$4 AND fence_token=$5 AND state='pending'`,
		item.TenantID, item.CollectionID, item.ObjectVersionID, item.Generation, item.FenceToken)
	if err != nil {
		return fmt.Errorf("complete index reconciliation: %w", err)
	}
	if n, _ := res.RowsAffected(); n != 1 {
		return fmt.Errorf("index reconciliation reservation changed")
	}
	return nil
}

func (r *RAGRepo) ListPublishedGenerations(ctx context.Context, tenantID, collectionID string) ([]rag.IndexGeneration, error) {
	rows, err := r.db.SQL.QueryContext(ctx, `
		SELECT g.collection_id, c.qdrant_collection, o.id, g.object_version_id,
		       g.generation, g.state, false
		FROM index_generations g
		JOIN source_object_versions v ON v.tenant_id=g.tenant_id AND v.id=g.object_version_id
		JOIN source_objects o ON o.tenant_id=v.tenant_id AND o.id=v.object_id
		JOIN knowledge_collections c ON c.tenant_id=g.tenant_id AND c.id=g.collection_id
		WHERE g.tenant_id=$1 AND g.collection_id=$2 AND g.state='published'
		  AND o.deleted_at IS NULL AND o.tombstoned_at IS NULL AND o.observed_seq=v.observed_seq`,
		tenantID, collectionID)
	if err != nil {
		return nil, fmt.Errorf("list published generations: %w", err)
	}
	defer rows.Close()
	var result []rag.IndexGeneration
	for rows.Next() {
		var generation rag.IndexGeneration
		if err := rows.Scan(&generation.CollectionID, &generation.QdrantCollection, &generation.ObjectID, &generation.ObjectVersionID, &generation.Generation, &generation.State, &generation.Tombstoned); err != nil {
			return nil, fmt.Errorf("scan published generation: %w", err)
		}
		result = append(result, generation)
	}
	if err := rows.Err(); err != nil {
		return nil, fmt.Errorf("iterate published generations: %w", err)
	}
	return result, nil
}

func (r *RAGRepo) ListGenerationChunks(ctx context.Context, tenantID, collectionID, versionID string, generation int64) ([]rag.DocumentChunk, error) {
	rows, err := r.db.SQL.QueryContext(ctx, `
		SELECT k.id, v.object_id, k.collection_id, k.generation, k.ordinal, k.text,
		       k.byte_start, k.byte_end, k.span_refs, k.block_path, k.locator, k.qdrant_point_id, k.created_at
		FROM chunks k
		JOIN source_object_versions v ON v.tenant_id = k.tenant_id AND v.id = k.object_version_id
		WHERE k.tenant_id=$1 AND k.collection_id=$2 AND k.object_version_id=$3 AND k.generation=$4
		ORDER BY k.ordinal`, tenantID, collectionID, versionID, generation)
	if err != nil {
		return nil, fmt.Errorf("list generation chunks: %w", err)
	}
	defer rows.Close()
	var result []rag.DocumentChunk
	for rows.Next() {
		var chunk rag.DocumentChunk
		var spans, metadata []byte
		if err := rows.Scan(&chunk.ID, &chunk.DocumentID, &chunk.CollectionID, &chunk.Generation, &chunk.ChunkIndex, &chunk.Content, &chunk.ByteStart, &chunk.ByteEnd, &spans, &chunk.HeaderPath, &metadata, &chunk.QdrantPointID, &chunk.CreatedAt); err != nil {
			return nil, fmt.Errorf("scan generation chunk: %w", err)
		}
		if err := json.Unmarshal(spans, &chunk.SpanRefs); err != nil {
			return nil, fmt.Errorf("decode chunk spans: %w", err)
		}
		if err := json.Unmarshal(metadata, &chunk.Metadata); err != nil {
			return nil, fmt.Errorf("decode chunk metadata: %w", err)
		}
		result = append(result, chunk)
	}
	if err := rows.Err(); err != nil {
		return nil, fmt.Errorf("iterate generation chunks: %w", err)
	}
	return result, nil
}

func (r *RAGRepo) SaveEvidence(ctx context.Context, record rag.EvidenceRecord) error {
	spans, err := json.Marshal(record.SpanRefs)
	if err != nil {
		return fmt.Errorf("marshal evidence spans: %w", err)
	}
	_, err = r.db.SQL.ExecContext(ctx, `
		INSERT INTO rag_evidence (tenant_id,id,collection_id,object_id,object_version_id,observed_seq,generation,chunk_id,span_refs)
		VALUES ($1,$2,$3,$4,$5,$6,$7,$8,$9::jsonb)`,
		record.TenantID, record.ID, record.CollectionID, record.ObjectID, record.ObjectVersionID, record.ObservedSeq, record.Generation, record.ChunkID, spans)
	if err != nil {
		return fmt.Errorf("save evidence record: %w", err)
	}
	return nil
}

func (r *RAGRepo) GetEvidence(ctx context.Context, tenantID, id string) (rag.EvidenceRecord, error) {
	var record rag.EvidenceRecord
	var spans []byte
	err := r.db.SQL.QueryRowContext(ctx, `
		SELECT id,collection_id,object_id,object_version_id,observed_seq,generation,chunk_id,span_refs
		FROM rag_evidence WHERE tenant_id=$1 AND id=$2`, tenantID, id).
		Scan(&record.ID, &record.CollectionID, &record.ObjectID, &record.ObjectVersionID, &record.ObservedSeq, &record.Generation, &record.ChunkID, &spans)
	if errors.Is(err, sql.ErrNoRows) {
		return rag.EvidenceRecord{}, source.ErrDenied
	}
	if err != nil {
		return rag.EvidenceRecord{}, fmt.Errorf("get evidence record: %w", err)
	}
	record.TenantID = tenantID
	if err := json.Unmarshal(spans, &record.SpanRefs); err != nil {
		return rag.EvidenceRecord{}, fmt.Errorf("decode evidence spans: %w", err)
	}
	return record, nil
}
