package postgres

import (
	"context"
	"database/sql"
	"errors"
	"fmt"
	"strings"

	"enclava-go/internal/source"
)

var ErrChunkProfileNotFound = errors.New("chunk profile not found")

// ChunkProfilesRepo stores immutable profile versions and scopes every read to its tenant.
type ChunkProfilesRepo struct{ db *DB }

func NewChunkProfilesRepo(db *DB) *ChunkProfilesRepo { return &ChunkProfilesRepo{db: db} }

// Create inserts the next immutable version in a profile family.
func (r *ChunkProfilesRepo) Create(ctx context.Context, tenantID string, profile source.ChunkProfile, ownerActorID string) (source.ChunkProfile, error) {
	if tenantID == "" || strings.TrimSpace(profile.ProfileID) == "" || profile.Version != 0 {
		return source.ChunkProfile{}, fmt.Errorf("invalid chunk profile identity")
	}
	tx, err := r.db.SQL.BeginTx(ctx, nil)
	if err != nil {
		return source.ChunkProfile{}, fmt.Errorf("begin chunk profile creation: %w", err)
	}
	defer tx.Rollback()
	if _, err := tx.ExecContext(ctx, `SELECT pg_advisory_xact_lock(hashtextextended($1, 0))`, tenantID+":"+profile.ProfileID); err != nil {
		return source.ChunkProfile{}, fmt.Errorf("lock chunk profile family: %w", err)
	}
	var version int
	if err := tx.QueryRowContext(ctx, `
		SELECT COALESCE(MAX(version), 0) + 1
		FROM chunk_profiles WHERE tenant_id = $1 AND profile_id = $2`,
		tenantID, profile.ProfileID).Scan(&version); err != nil {
		return source.ChunkProfile{}, fmt.Errorf("read chunk profile version: %w", err)
	}
	profile.ID = newPGID("cpv")
	profile.Version = version
	if err := validatePersistedProfile(profile); err != nil {
		return source.ChunkProfile{}, err
	}
	_, err = tx.ExecContext(ctx, `
		INSERT INTO chunk_profiles
			(tenant_id, id, profile_id, version, name, boundary_strategy,
			 max_size_codepoints, overlap_codepoints, include_parent, table_handling,
			 preserve_clauses, preserve_authorship, owner_actor_id)
		VALUES ($1,$2,$3,$4,$3,$5,$6,$7,$8,$9,$10,$11,NULLIF($12,''))`,
		tenantID, profile.ID, profile.ProfileID, profile.Version,
		profile.BoundaryStrategy, profile.SizeCodePoints, profile.OverlapCodePoints,
		profile.IncludeParents, profile.TableHandling, profile.PreserveClauses,
		profile.PreserveAuthorship, ownerActorID)
	if err != nil {
		return source.ChunkProfile{}, fmt.Errorf("insert chunk profile version: %w", err)
	}
	if err := tx.Commit(); err != nil {
		return source.ChunkProfile{}, fmt.Errorf("commit chunk profile version: %w", err)
	}
	return profile, nil
}

// Get returns only profiles owned by tenantID; other tenants are indistinguishable from absent IDs.
func (r *ChunkProfilesRepo) Get(ctx context.Context, tenantID, id string) (source.ChunkProfile, error) {
	p, err := scanChunkProfile(r.db.SQL.QueryRowContext(ctx, `
		SELECT id, profile_id, version, max_size_codepoints, overlap_codepoints,
		       boundary_strategy, include_parent, table_handling, preserve_clauses, preserve_authorship
		FROM chunk_profiles WHERE tenant_id = $1 AND id = $2`, tenantID, id))
	if errors.Is(err, sql.ErrNoRows) {
		return source.ChunkProfile{}, ErrChunkProfileNotFound
	}
	if err != nil {
		return source.ChunkProfile{}, fmt.Errorf("get chunk profile: %w", err)
	}
	return p, nil
}

// List returns only the requested tenant's immutable profile versions.
func (r *ChunkProfilesRepo) List(ctx context.Context, tenantID string) ([]source.ChunkProfile, error) {
	rows, err := r.db.SQL.QueryContext(ctx, `
		SELECT id, profile_id, version, max_size_codepoints, overlap_codepoints,
		       boundary_strategy, include_parent, table_handling, preserve_clauses, preserve_authorship
		FROM chunk_profiles WHERE tenant_id = $1 ORDER BY profile_id, version`, tenantID)
	if err != nil {
		return nil, fmt.Errorf("list chunk profiles: %w", err)
	}
	defer rows.Close()
	profiles := make([]source.ChunkProfile, 0)
	for rows.Next() {
		p, err := scanChunkProfile(rows)
		if err != nil {
			return nil, fmt.Errorf("scan chunk profile: %w", err)
		}
		profiles = append(profiles, p)
	}
	if err := rows.Err(); err != nil {
		return nil, fmt.Errorf("iterate chunk profiles: %w", err)
	}
	return profiles, nil
}

type chunkProfileScanner interface{ Scan(...any) error }

func scanChunkProfile(row chunkProfileScanner) (source.ChunkProfile, error) {
	var p source.ChunkProfile
	var size, overlap int64
	err := row.Scan(&p.ID, &p.ProfileID, &p.Version, &size, &overlap,
		&p.BoundaryStrategy, &p.IncludeParents, &p.TableHandling,
		&p.PreserveClauses, &p.PreserveAuthorship)
	p.SizeCodePoints, p.OverlapCodePoints = int(size), int(overlap)
	return p, err
}

func validatePersistedProfile(p source.ChunkProfile) error {
	if p.SizeCodePoints < 1 || p.OverlapCodePoints < 0 || p.OverlapCodePoints >= p.SizeCodePoints {
		return fmt.Errorf("invalid chunk profile size or overlap")
	}
	switch p.BoundaryStrategy {
	case "fixed", "heading", "clause", "paragraph", "message", "thread", "code_lines":
	default:
		return fmt.Errorf("invalid chunk profile boundary strategy")
	}
	switch p.TableHandling {
	case "atomic", "row-wise", "skip":
	default:
		return fmt.Errorf("invalid chunk profile table handling")
	}
	return nil
}
