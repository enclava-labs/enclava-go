package postgres

import (
	"context"
	"database/sql"
	"encoding/json"
	"errors"
	"fmt"
	"time"

	"enclava-go/internal/credentials"
)

// CredentialsRepo persists dashboard-managed connector credentials and OAuth
// states (migration 052). Sealed values are opaque here.
type CredentialsRepo struct {
	db *sql.DB
}

// NewCredentialsRepo wraps the shared pool.
func NewCredentialsRepo(db *DB) *CredentialsRepo {
	return &CredentialsRepo{db: db.SQL}
}

var _ credentials.Repository = (*CredentialsRepo)(nil)

const credentialCols = `tenant_id::text, id, provider, kind, name, details::text, COALESCE(created_by, ''), created_at, updated_at` //nolint:gosec // Column list, not a credential.

func scanCredential(row rowScanner) (credentials.Credential, error) {
	var c credentials.Credential
	var details string
	if err := row.Scan(&c.TenantID, &c.ID, &c.Provider, &c.Kind, &c.Name, &details, &c.CreatedBy, &c.CreatedAt, &c.UpdatedAt); err != nil {
		return credentials.Credential{}, err
	}
	if err := json.Unmarshal([]byte(details), &c.Details); err != nil {
		return credentials.Credential{}, fmt.Errorf("decode credential details: %w", err)
	}
	return c, nil
}

func (r *CredentialsRepo) CreateCredential(ctx context.Context, c credentials.Credential, sealed []byte) error {
	details, err := json.Marshal(c.Details)
	if err != nil {
		return err
	}
	_, err = r.db.ExecContext(ctx, `
		INSERT INTO connector_credentials (tenant_id, id, provider, kind, name, sealed, details, created_by)
		VALUES ($1::uuid, $2, $3, $4, $5, $6, $7::jsonb, NULLIF($8, ''))`,
		c.TenantID, c.ID, c.Provider, c.Kind, c.Name, sealed, string(details), c.CreatedBy)
	if isUniqueViolationPG(err) {
		return credentials.ErrConflict
	}
	if err != nil {
		return fmt.Errorf("create credential: %w", err)
	}
	return nil
}

func (r *CredentialsRepo) UpdateCredential(ctx context.Context, tenantID, id string, details map[string]any, sealed []byte) error {
	raw, err := json.Marshal(details)
	if err != nil {
		return err
	}
	res, err := r.db.ExecContext(ctx, `
		UPDATE connector_credentials SET sealed = $3, details = $4::jsonb, updated_at = now()
		WHERE tenant_id = $1::uuid AND id = $2`, tenantID, id, sealed, string(raw))
	if err != nil {
		return fmt.Errorf("update credential: %w", err)
	}
	if n, _ := res.RowsAffected(); n == 0 {
		return credentials.ErrNotFound
	}
	return nil
}

func (r *CredentialsRepo) GetCredential(ctx context.Context, tenantID, id string) (credentials.Credential, []byte, error) {
	var sealed []byte
	row := r.db.QueryRowContext(ctx, `SELECT `+credentialCols+`, sealed FROM connector_credentials WHERE tenant_id = $1::uuid AND id = $2`, tenantID, id)
	var c credentials.Credential
	var details string
	err := row.Scan(&c.TenantID, &c.ID, &c.Provider, &c.Kind, &c.Name, &details, &c.CreatedBy, &c.CreatedAt, &c.UpdatedAt, &sealed)
	if errors.Is(err, sql.ErrNoRows) || isInvalidUUID(err) {
		return credentials.Credential{}, nil, credentials.ErrNotFound
	}
	if err != nil {
		return credentials.Credential{}, nil, fmt.Errorf("get credential: %w", err)
	}
	if err := json.Unmarshal([]byte(details), &c.Details); err != nil {
		return credentials.Credential{}, nil, fmt.Errorf("decode credential details: %w", err)
	}
	return c, sealed, nil
}

func (r *CredentialsRepo) ListCredentials(ctx context.Context, tenantID string) ([]credentials.Credential, error) {
	rows, err := r.db.QueryContext(ctx, `SELECT `+credentialCols+` FROM connector_credentials WHERE tenant_id = $1::uuid ORDER BY provider, lower(name)`, tenantID)
	if err != nil {
		return nil, fmt.Errorf("list credentials: %w", err)
	}
	defer func() { _ = rows.Close() }()
	var out []credentials.Credential
	for rows.Next() {
		c, err := scanCredential(rows)
		if err != nil {
			return nil, err
		}
		out = append(out, c)
	}
	return out, rows.Err()
}

func (r *CredentialsRepo) DeleteCredential(ctx context.Context, tenantID, id string) error {
	res, err := r.db.ExecContext(ctx, `DELETE FROM connector_credentials WHERE tenant_id = $1::uuid AND id = $2`, tenantID, id)
	if err != nil {
		return fmt.Errorf("delete credential: %w", err)
	}
	if n, _ := res.RowsAffected(); n == 0 {
		return credentials.ErrNotFound
	}
	return nil
}

func (r *CredentialsRepo) SourcesUsing(ctx context.Context, tenantID, id string) ([]string, error) {
	rows, err := r.db.QueryContext(ctx, `SELECT id FROM connections WHERE tenant_id = $1::uuid AND credential_ref = $2 ORDER BY id`, tenantID, id)
	if err != nil {
		return nil, fmt.Errorf("sources using credential: %w", err)
	}
	defer func() { _ = rows.Close() }()
	out := []string{}
	for rows.Next() {
		var sourceID string
		if err := rows.Scan(&sourceID); err != nil {
			return nil, err
		}
		out = append(out, sourceID)
	}
	return out, rows.Err()
}

func (r *CredentialsRepo) SaveOAuthState(ctx context.Context, s credentials.OAuthState) error {
	if _, err := r.db.ExecContext(ctx, `DELETE FROM connector_oauth_states WHERE expires_at < now()`); err != nil {
		return fmt.Errorf("prune oauth states: %w", err)
	}
	_, err := r.db.ExecContext(ctx, `
		INSERT INTO connector_oauth_states (state, tenant_id, user_id, provider, name, expires_at)
		VALUES ($1, $2::uuid, $3, $4, $5, $6)`, s.State, s.TenantID, s.UserID, s.Provider, s.Name, s.ExpiresAt)
	if err != nil {
		return fmt.Errorf("save oauth state: %w", err)
	}
	return nil
}

func (r *CredentialsRepo) TakeOAuthState(ctx context.Context, state string, now time.Time) (credentials.OAuthState, error) {
	var s credentials.OAuthState
	err := r.db.QueryRowContext(ctx, `
		DELETE FROM connector_oauth_states WHERE state = $1
		RETURNING state, tenant_id::text, user_id, provider, name, expires_at`, state).
		Scan(&s.State, &s.TenantID, &s.UserID, &s.Provider, &s.Name, &s.ExpiresAt)
	if errors.Is(err, sql.ErrNoRows) {
		return credentials.OAuthState{}, credentials.ErrNotFound
	}
	if err != nil {
		return credentials.OAuthState{}, fmt.Errorf("take oauth state: %w", err)
	}
	if !now.Before(s.ExpiresAt) {
		return credentials.OAuthState{}, credentials.ErrNotFound
	}
	return s, nil
}
