package postgres

import (
	"context"
	"crypto/rand"
	"database/sql"
	"encoding/base64"
	"encoding/json"
	"errors"
	"fmt"
	"time"

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
const documentSelect = `
	WITH latest_version AS (
		SELECT DISTINCT ON (object_id) id, object_id, observed_seq, artifact_ref,
		       content_sha256, byte_size, mime_type, original_filename, provenance
		FROM source_object_versions
		WHERE tenant_id = $1
		ORDER BY object_id, observed_seq DESC
	),
	latest_gen AS (
		SELECT DISTINCT ON (object_version_id) object_version_id, state
		FROM index_generations
		WHERE tenant_id = $1
		ORDER BY object_version_id, generation DESC
	)
	SELECT o.id, ci.collection_id, o.kind, o.native_id, o.observed_seq,
	       lv.id, lv.observed_seq, lv.artifact_ref, lv.content_sha256, lv.byte_size,
	       lv.mime_type, lv.original_filename, lv.provenance,
	       COALESCE(lg.state, 'staging'), COALESCE(o.deleted_at, o.tombstoned_at) IS NOT NULL,
	       o.first_seen_at, o.last_seen_at,
	       (SELECT COUNT(*) FROM chunks k
	        WHERE k.tenant_id = $1 AND k.object_version_id = lv.id)
	FROM source_objects o
	JOIN collection_items ci ON ci.tenant_id = o.tenant_id AND ci.object_id = o.id
	LEFT JOIN latest_version lv ON lv.object_id = o.id
	LEFT JOIN latest_gen lg ON lg.object_version_id = lv.id`

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
	err := scanner.Scan(&d.ID, &d.CollectionID, &d.SourceType, &nativeID, &seq,
		&verID, &verSeq, &artifactRef, &sha, &byteSize,
		&mime, &filename, &provenance,
		&state, &gone, &firstSeen, &lastSeen, &chunkCount)
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
		// Caller treats deleted objects as absent.
		return rag.Document{}, source.ErrDenied
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
	switch state {
	case "published":
		d.Status = rag.DocumentStatusReady
		now := d.UpdatedAt
		d.ProcessedAt = &now
	case "failed":
		d.Status = rag.DocumentStatusFailed
	default:
		d.Status = rag.DocumentStatusProcessing
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
	d.TenantID = tenantID
	return d, nil
}

func (r *RAGRepo) ListDocuments(ctx context.Context, tenantID, collectionID string, limit, offset int) ([]rag.Document, error) {
	if limit <= 0 {
		limit = 50
	}
	rows, err := r.db.SQL.QueryContext(ctx,
		documentSelect+` WHERE o.tenant_id = $1 AND ci.collection_id = $2
		 ORDER BY o.first_seen_at DESC LIMIT $3 OFFSET $4`,
		tenantID, collectionID, limit, offset)
	if err != nil {
		return nil, fmt.Errorf("list documents: %w", err)
	}
	defer func() { _ = rows.Close() }()
	var out []rag.Document
	for rows.Next() {
		d, err := scanDocument(rows)
		if err != nil {
			if errors.Is(err, source.ErrDenied) {
				continue
			}
			return nil, err
		}
		d.TenantID = tenantID
		out = append(out, d)
	}
	return out, rows.Err()
}

// DeleteObjectRows removes the durable object row and its dependents —
// items, generations, chunks and versions — after the caller already
// tombstoned the object and dropped its index points.
func (r *RAGRepo) DeleteObjectRows(ctx context.Context, tenantID, objectID string) error {
	return WithTx(ctx, r.db.SQL, func(tx *sql.Tx) error {
		stmts := []struct {
			q    string
			args []any
		}{
			{`DELETE FROM collection_items WHERE tenant_id = $1 AND object_id = $2`, []any{tenantID, objectID}},
			{`DELETE FROM index_generations WHERE tenant_id = $1 AND object_version_id IN
			  (SELECT id FROM source_object_versions WHERE tenant_id = $1 AND object_id = $2)`, []any{tenantID, objectID}},
			{`DELETE FROM chunks WHERE tenant_id = $1 AND object_version_id IN
			  (SELECT id FROM source_object_versions WHERE tenant_id = $1 AND object_id = $2)`, []any{tenantID, objectID}},
			{`DELETE FROM source_object_versions WHERE tenant_id = $1 AND object_id = $2`, []any{tenantID, objectID}},
			{`DELETE FROM source_objects WHERE tenant_id = $1 AND id = $2`, []any{tenantID, objectID}},
		}
		for _, st := range stmts {
			if _, err := tx.ExecContext(ctx, st.q, st.args...); err != nil {
				return fmt.Errorf("delete object rows: %w", err)
			}
		}
		return nil
	})
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

// ReplaceVersionChunksAndStage is the single transactional publish boundary:
// prior chunk rows for the version are replaced and the generation row is
// upserted in 'staging' state, atomically. profileVersionID may be empty at
// enqueue time (bound later by the attempt); chunk rows are skipped then.
func (r *RAGRepo) ReplaceVersionChunksAndStage(ctx context.Context, tenantID, collectionID, objectID, versionID string, seq, generation int64, profileVersionID string, chunks []rag.DocumentChunk) error {
	return WithTx(ctx, r.db.SQL, func(tx *sql.Tx) error {
		if profileVersionID != "" || len(chunks) > 0 {
			if _, err := tx.ExecContext(ctx, `
				DELETE FROM chunks WHERE tenant_id = $1 AND object_version_id = $2`,
				tenantID, versionID); err != nil {
				return fmt.Errorf("clear version chunks: %w", err)
			}
			for _, ch := range chunks {
				spanRefs, err := json.Marshal(ch.SpanRefs)
				if err != nil {
					return fmt.Errorf("marshal chunk spans: %w", err)
				}
				start, end := ch.ByteStart, ch.ByteEnd
				if len(ch.SpanRefs) == 0 && end == 0 {
					end = len(ch.Content)
				}
				if _, err := tx.ExecContext(ctx, `
					INSERT INTO chunks
					    (tenant_id, id, object_version_id, chunk_profile_version_id,
					     ordinal, text, byte_start, byte_end, span_refs, owner_actor_id)
					VALUES ($1, $2, $3, $4, $5, $6, $7, $8, $9::jsonb, NULL)`,
					tenantID, ch.ID, versionID, profileVersionID, ch.ChunkIndex,
					ch.Content, start, end, spanRefs); err != nil {
					return fmt.Errorf("insert chunk: %w", err)
				}
			}
		}
		if _, err := tx.ExecContext(ctx, `
			INSERT INTO index_generations
			    (tenant_id, id, collection_id, object_version_id, generation, state)
			VALUES ($1, $2, $3, $4, $5, 'staging')
			ON CONFLICT (tenant_id, collection_id, object_version_id, generation)
			DO UPDATE SET state = EXCLUDED.state`,
			tenantID, newPGID("idxgen"), collectionID, versionID, generation); err != nil {
			return fmt.Errorf("stage generation: %w", err)
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
		SELECT k.id, $2, ci.collection_id, k.ordinal, k.text, k.created_at,
		       k.byte_start, k.byte_end, k.span_refs
		FROM chunks k
		JOIN source_object_versions v ON v.tenant_id = k.tenant_id AND v.id = k.object_version_id
		JOIN collection_items ci ON ci.tenant_id = k.tenant_id AND ci.object_id = v.object_id
		WHERE k.tenant_id = $1 AND k.object_version_id = $3
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
			&ch.ByteStart, &ch.ByteEnd, &spans); err != nil {
			return nil, err
		}
		if err := json.Unmarshal(spans, &ch.SpanRefs); err != nil {
			return nil, fmt.Errorf("decode chunk spans: %w", err)
		}
		ch.CollectionID = collectionID.String
		ch.QdrantPointID = ch.ID
		ch.TokenCount = len(ch.Content) / 4
		out = append(out, ch)
	}
	return out, rows.Err()
}
