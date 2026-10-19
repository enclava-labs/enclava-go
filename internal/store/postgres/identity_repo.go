// identity_repo.go implements auth.IdentityRepository over the migration-002
// identity schema. Every method carries a tenant_id predicate EXCEPT the two
// documented pre-authentication locators — FindKeyCandidates (bounded
// key-prefix scan) and GetSessionByTokenHash (unique token-hash lookup) —
// which must find the row before its tenant is known; the auth service
// verifies secret/expiry/revocation before trusting the row's tenant_id.
package postgres

import (
	"context"
	"database/sql"
	"encoding/json"
	"errors"
	"fmt"
	"time"

	"enclava-go/internal/auth"
)

// IdentityRepo is the postgres-backed auth.IdentityRepository.
type IdentityRepo struct {
	db *sql.DB
}

// NewIdentityRepo wraps the shared pool in an IdentityRepository.
func NewIdentityRepo(db *DB) *IdentityRepo {
	return &IdentityRepo{db: db.SQL}
}

var _ auth.IdentityRepository = (*IdentityRepo)(nil)

// --- small row/arg helpers -------------------------------------------------

type rowScanner interface {
	Scan(dest ...any) error
}

func nullStr(s string) sql.NullString {
	return sql.NullString{String: s, Valid: s != ""}
}

// jsonbArg keeps empty grant documents writable into NOT NULL JSONB columns.
func jsonbArg(raw json.RawMessage) string {
	if len(raw) == 0 {
		return "{}"
	}
	return string(raw)
}

func scanRaw(raw json.RawMessage) json.RawMessage {
	if raw == nil {
		return json.RawMessage{}
	}
	return raw
}

// scopesJSON stores []string scopes as a plain JSON array in the roles and
// service_accounts scopes JSONB columns.
func scopesJSON(scopes []string) (string, error) {
	if scopes == nil {
		return "[]", nil
	}
	b, err := json.Marshal(scopes)
	if err != nil {
		return "", err
	}
	return string(b), nil
}

// scopeSlice decodes the JSON-array scopes column; the '{}' default or a
// malformed value yields no scopes (deny by absence).
func scopeSlice(raw json.RawMessage) []string {
	var scopes []string
	if len(raw) > 0 && raw[0] == '[' {
		_ = json.Unmarshal(raw, &scopes)
	}
	return scopes
}

func nullRateLimit(v *int) sql.NullInt64 {
	if v == nil {
		return sql.NullInt64{}
	}
	return sql.NullInt64{Int64: int64(*v), Valid: true}
}

func rateLimitPtr(n sql.NullInt64) *int {
	if !n.Valid {
		return nil
	}
	v := int(n.Int64)
	return &v
}

// --- tenants ---------------------------------------------------------------

func (r *IdentityRepo) CreateTenant(ctx context.Context, slug, displayName string) (string, error) {
	var id string
	err := r.db.QueryRowContext(ctx,
		`INSERT INTO tenants (id, slug, display_name)
		 VALUES (gen_random_uuid(), $1, $2)
		 RETURNING id::text`,
		slug, displayName).Scan(&id)
	if err != nil {
		return "", fmt.Errorf("create tenant: %w", err)
	}
	return id, nil
}

// --- roles -----------------------------------------------------------------

func (r *IdentityRepo) CreateRole(ctx context.Context, tenantID string, role auth.Role) error {
	scopes, err := scopesJSON(role.Scopes)
	if err != nil {
		return fmt.Errorf("create role: marshal scopes: %w", err)
	}
	_, err = r.db.ExecContext(ctx,
		`INSERT INTO roles (tenant_id, id, name, scopes)
		 VALUES ($1::uuid, $2, $3, $4::jsonb)`,
		tenantID, role.ID, role.Name, scopes)
	if err != nil {
		return fmt.Errorf("create role: %w", err)
	}
	return nil
}

func (r *IdentityRepo) AssignRole(ctx context.Context, tenantID, userID, roleID string) error {
	_, err := r.db.ExecContext(ctx,
		`INSERT INTO user_roles (tenant_id, user_id, role_id)
		 VALUES ($1::uuid, $2, $3)`,
		tenantID, userID, roleID)
	if err != nil {
		return fmt.Errorf("assign role: %w", err)
	}
	return nil
}

func (r *IdentityRepo) RolesForUser(ctx context.Context, tenantID, userID string) ([]auth.Role, error) {
	rows, err := r.db.QueryContext(ctx,
		`SELECT ro.id, ro.name, ro.scopes, ro.created_at
		 FROM roles ro
		 JOIN user_roles ur
		   ON ur.tenant_id = ro.tenant_id AND ur.role_id = ro.id
		 WHERE ur.tenant_id = $1::uuid AND ur.user_id = $2
		 ORDER BY ro.id`,
		tenantID, userID)
	if err != nil {
		return nil, fmt.Errorf("roles for user: %w", err)
	}
	defer rows.Close()

	var out []auth.Role
	for rows.Next() {
		var role auth.Role
		var raw json.RawMessage
		if err := rows.Scan(&role.ID, &role.Name, &raw, &role.CreatedAt); err != nil {
			return nil, fmt.Errorf("roles for user: scan: %w", err)
		}
		role.Scopes = scopeSlice(raw)
		out = append(out, role)
	}
	if err := rows.Err(); err != nil {
		return nil, fmt.Errorf("roles for user: %w", err)
	}
	return out, nil
}

// --- users -----------------------------------------------------------------

const userColumns = `id, tenant_id::text, email, display_name, status,
	password_hash, password_set_at, last_login_at, created_at`

func scanUser(row rowScanner) (auth.User, error) {
	var u auth.User
	var passwordHash sql.NullString
	err := row.Scan(&u.ID, &u.TenantID, &u.Email, &u.DisplayName, &u.Status,
		&passwordHash, &u.PasswordSetAt, &u.LastLoginAt, &u.CreatedAt)
	if err != nil {
		return auth.User{}, err
	}
	u.PasswordHash = passwordHash.String
	return u, nil
}

func (r *IdentityRepo) CreateUser(ctx context.Context, tenantID string, u auth.User) error {
	status := u.Status
	if status == "" {
		status = "active"
	}
	_, err := r.db.ExecContext(ctx,
		`INSERT INTO users
		 (tenant_id, id, email, display_name, status,
		  password_hash, password_set_at, last_login_at)
		 VALUES ($1::uuid, $2, $3, $4, $5, $6, $7, $8)`,
		tenantID, u.ID, u.Email, u.DisplayName, status,
		nullStr(u.PasswordHash), u.PasswordSetAt, u.LastLoginAt)
	if err != nil {
		return fmt.Errorf("create user: %w", err)
	}
	return nil
}

func (r *IdentityRepo) GetUser(ctx context.Context, tenantID, userID string) (auth.User, error) {
	u, err := scanUser(r.db.QueryRowContext(ctx,
		`SELECT `+userColumns+` FROM users
		 WHERE tenant_id = $1::uuid AND id = $2`,
		tenantID, userID))
	if err != nil {
		if errors.Is(err, sql.ErrNoRows) || isInvalidUUID(err) {
			return auth.User{}, auth.ErrNotFound
		}
		return auth.User{}, fmt.Errorf("get user: %w", err)
	}
	return u, nil
}

func (r *IdentityRepo) GetUserByEmail(ctx context.Context, tenantID, email string) (auth.User, error) {
	u, err := scanUser(r.db.QueryRowContext(ctx,
		`SELECT `+userColumns+` FROM users
		 WHERE tenant_id = $1::uuid AND email = $2`,
		tenantID, email))
	if err != nil {
		if errors.Is(err, sql.ErrNoRows) || isInvalidUUID(err) {
			return auth.User{}, auth.ErrNotFound
		}
		return auth.User{}, fmt.Errorf("get user by email: %w", err)
	}
	return u, nil
}

func (r *IdentityRepo) UpdateUserStatus(ctx context.Context, tenantID, userID, status string) error {
	_, err := r.db.ExecContext(ctx,
		`UPDATE users SET status = $3
		 WHERE tenant_id = $1::uuid AND id = $2`,
		tenantID, userID, status)
	if err != nil {
		return fmt.Errorf("update user status: %w", err)
	}
	return nil
}

func (r *IdentityRepo) SetPassword(ctx context.Context, tenantID, userID, hash string, at time.Time) error {
	_, err := r.db.ExecContext(ctx,
		`UPDATE users SET password_hash = $3, password_set_at = $4
		 WHERE tenant_id = $1::uuid AND id = $2`,
		tenantID, userID, hash, at)
	if err != nil {
		return fmt.Errorf("set password: %w", err)
	}
	return nil
}

func (r *IdentityRepo) TouchLastLogin(ctx context.Context, tenantID, userID string, at time.Time) error {
	_, err := r.db.ExecContext(ctx,
		`UPDATE users SET last_login_at = $3
		 WHERE tenant_id = $1::uuid AND id = $2`,
		tenantID, userID, at)
	if err != nil {
		return fmt.Errorf("touch last login: %w", err)
	}
	return nil
}

// --- service accounts ------------------------------------------------------

func (r *IdentityRepo) CreateServiceAccount(ctx context.Context, tenantID string, sa auth.ServiceAccount) error {
	scopes, err := scopesJSON(sa.Scopes)
	if err != nil {
		return fmt.Errorf("create service account: marshal scopes: %w", err)
	}
	status := sa.Status
	if status == "" {
		status = "active"
	}
	_, err = r.db.ExecContext(ctx,
		`INSERT INTO service_accounts (tenant_id, id, name, scopes, status)
		 VALUES ($1::uuid, $2, $3, $4::jsonb, $5)`,
		tenantID, sa.ID, sa.Name, scopes, status)
	if err != nil {
		return fmt.Errorf("create service account: %w", err)
	}
	return nil
}

func (r *IdentityRepo) GetServiceAccount(ctx context.Context, tenantID, id string) (auth.ServiceAccount, error) {
	var sa auth.ServiceAccount
	var raw json.RawMessage
	err := r.db.QueryRowContext(ctx,
		`SELECT id, tenant_id::text, name, scopes, status, created_at
		 FROM service_accounts
		 WHERE tenant_id = $1::uuid AND id = $2`,
		tenantID, id).
		Scan(&sa.ID, &sa.TenantID, &sa.Name, &raw, &sa.Status, &sa.CreatedAt)
	if err != nil {
		if errors.Is(err, sql.ErrNoRows) || isInvalidUUID(err) {
			return auth.ServiceAccount{}, auth.ErrNotFound
		}
		return auth.ServiceAccount{}, fmt.Errorf("get service account: %w", err)
	}
	sa.Scopes = scopeSlice(raw)
	return sa, nil
}

func (r *IdentityRepo) UpdateServiceAccountStatus(ctx context.Context, tenantID, id, status string) error {
	_, err := r.db.ExecContext(ctx,
		`UPDATE service_accounts SET status = $3
		 WHERE tenant_id = $1::uuid AND id = $2`,
		tenantID, id, status)
	if err != nil {
		return fmt.Errorf("update service account status: %w", err)
	}
	return nil
}

// --- API keys --------------------------------------------------------------

const apiKeyColumns = `id, tenant_id::text, owner_user_id, service_account_id,
	name, key_prefix, key_hash, key_hash_algo, scopes, allowed_models,
	allowed_extract_templates, allowed_chatbots, allowed_agents,
	allowed_endpoints, allowed_ips,
	rate_limit_per_minute, rate_limit_per_hour, rate_limit_per_day,
	expires_at, revoked_at, last_used_at, created_at`

// scanAPIKey reads an api_keys row in apiKeyColumns order. IsActive is derived
// from revoked_at — there is no is_active column.
func scanAPIKey(row rowScanner) (auth.APIKeyRecord, error) {
	var k auth.APIKeyRecord
	var owner, sa sql.NullString
	var perMin, perHour, perDay sql.NullInt64
	err := row.Scan(&k.ID, &k.TenantID, &owner, &sa,
		&k.Name, &k.KeyPrefix, &k.KeyHash, &k.KeyHashAlgo, &k.Scopes,
		&k.AllowedModels, &k.AllowedTemplates, &k.AllowedChatbots,
		&k.AllowedAgents, &k.AllowedEndpoints, &k.AllowedIPs,
		&perMin, &perHour, &perDay,
		&k.ExpiresAt, &k.RevokedAt, &k.LastUsedAt, &k.CreatedAt)
	if err != nil {
		return auth.APIKeyRecord{}, err
	}
	k.OwnerUserID = owner.String
	k.ServiceAccountID = sa.String
	k.Scopes = scanRaw(k.Scopes)
	k.AllowedModels = scanRaw(k.AllowedModels)
	k.AllowedTemplates = scanRaw(k.AllowedTemplates)
	k.AllowedChatbots = scanRaw(k.AllowedChatbots)
	k.AllowedAgents = scanRaw(k.AllowedAgents)
	k.AllowedEndpoints = scanRaw(k.AllowedEndpoints)
	k.AllowedIPs = scanRaw(k.AllowedIPs)
	k.RateLimitPerMinute = rateLimitPtr(perMin)
	k.RateLimitPerHour = rateLimitPtr(perHour)
	k.RateLimitPerDay = rateLimitPtr(perDay)
	k.IsActive = k.RevokedAt == nil
	return k, nil
}

func (r *IdentityRepo) CreateAPIKey(ctx context.Context, tenantID string, k auth.APIKeyRecord) error {
	_, err := r.db.ExecContext(ctx,
		`INSERT INTO api_keys
		 (tenant_id, id, owner_user_id, service_account_id, name,
		  key_prefix, key_hash, key_hash_algo, scopes, allowed_models,
		  allowed_extract_templates, allowed_chatbots, allowed_agents,
		  allowed_endpoints, allowed_ips,
		  rate_limit_per_minute, rate_limit_per_hour, rate_limit_per_day,
		  expires_at, revoked_at, last_used_at)
		 VALUES ($1::uuid, $2, $3, $4, $5,
		  $6, $7, $8, $9::jsonb, $10::jsonb,
		  $11::jsonb, $12::jsonb, $13::jsonb,
		  $14::jsonb, $15::jsonb,
		  $16, $17, $18, $19, $20, $21)`,
		tenantID, k.ID, nullStr(k.OwnerUserID), nullStr(k.ServiceAccountID), k.Name,
		k.KeyPrefix, k.KeyHash, k.KeyHashAlgo, jsonbArg(k.Scopes), jsonbArg(k.AllowedModels),
		jsonbArg(k.AllowedTemplates), jsonbArg(k.AllowedChatbots), jsonbArg(k.AllowedAgents),
		jsonbArg(k.AllowedEndpoints), jsonbArg(k.AllowedIPs),
		nullRateLimit(k.RateLimitPerMinute), nullRateLimit(k.RateLimitPerHour),
		nullRateLimit(k.RateLimitPerDay),
		k.ExpiresAt, k.RevokedAt, k.LastUsedAt)
	if err != nil {
		return fmt.Errorf("create api key: %w", err)
	}
	return nil
}

func (r *IdentityRepo) GetAPIKey(ctx context.Context, tenantID, id string) (auth.APIKeyRecord, error) {
	k, err := scanAPIKey(r.db.QueryRowContext(ctx,
		`SELECT `+apiKeyColumns+` FROM api_keys
		 WHERE tenant_id = $1::uuid AND id = $2`,
		tenantID, id))
	if err != nil {
		if errors.Is(err, sql.ErrNoRows) || isInvalidUUID(err) {
			return auth.APIKeyRecord{}, auth.ErrNotFound
		}
		return auth.APIKeyRecord{}, fmt.Errorf("get api key: %w", err)
	}
	return k, nil
}

func (r *IdentityRepo) ListAPIKeys(ctx context.Context, tenantID string) ([]auth.APIKeyRecord, error) {
	rows, err := r.db.QueryContext(ctx,
		`SELECT `+apiKeyColumns+` FROM api_keys
		 WHERE tenant_id = $1::uuid
		 ORDER BY created_at, id`,
		tenantID)
	if err != nil {
		return nil, fmt.Errorf("list api keys: %w", err)
	}
	defer rows.Close()

	var out []auth.APIKeyRecord
	for rows.Next() {
		k, err := scanAPIKey(rows)
		if err != nil {
			return nil, fmt.Errorf("list api keys: scan: %w", err)
		}
		out = append(out, k)
	}
	if err := rows.Err(); err != nil {
		return nil, fmt.Errorf("list api keys: %w", err)
	}
	return out, nil
}

func (r *IdentityRepo) UpdateAPIKey(ctx context.Context, tenantID string, k auth.APIKeyRecord) error {
	// revoked_at IS NULL is the active flag: activating clears it, deactivating
	// stamps it (preserving a caller-supplied revocation time when present).
	var revokedAt *time.Time
	switch {
	case k.IsActive:
		revokedAt = nil
	case k.RevokedAt != nil:
		revokedAt = k.RevokedAt
	default:
		now := time.Now().UTC()
		revokedAt = &now
	}
	_, err := r.db.ExecContext(ctx,
		`UPDATE api_keys SET
		  name = $3, scopes = $4::jsonb, allowed_models = $5::jsonb,
		  allowed_extract_templates = $6::jsonb, allowed_chatbots = $7::jsonb,
		  allowed_agents = $8::jsonb, allowed_endpoints = $9::jsonb,
		  allowed_ips = $10::jsonb,
		  rate_limit_per_minute = $11, rate_limit_per_hour = $12,
		  rate_limit_per_day = $13, expires_at = $14, revoked_at = $15
		 WHERE tenant_id = $1::uuid AND id = $2`,
		tenantID, k.ID,
		k.Name, jsonbArg(k.Scopes), jsonbArg(k.AllowedModels),
		jsonbArg(k.AllowedTemplates), jsonbArg(k.AllowedChatbots),
		jsonbArg(k.AllowedAgents), jsonbArg(k.AllowedEndpoints),
		jsonbArg(k.AllowedIPs),
		nullRateLimit(k.RateLimitPerMinute), nullRateLimit(k.RateLimitPerHour),
		nullRateLimit(k.RateLimitPerDay), k.ExpiresAt, revokedAt)
	if err != nil {
		return fmt.Errorf("update api key: %w", err)
	}
	return nil
}

func (r *IdentityRepo) RevokeAPIKey(ctx context.Context, tenantID, id string, at time.Time) error {
	_, err := r.db.ExecContext(ctx,
		`UPDATE api_keys SET revoked_at = $3
		 WHERE tenant_id = $1::uuid AND id = $2`,
		tenantID, id, at)
	if err != nil {
		return fmt.Errorf("revoke api key: %w", err)
	}
	return nil
}

func (r *IdentityRepo) TouchAPIKeyUsage(ctx context.Context, tenantID, id string, at time.Time) error {
	_, err := r.db.ExecContext(ctx,
		`UPDATE api_keys SET last_used_at = $3
		 WHERE tenant_id = $1::uuid AND id = $2`,
		tenantID, id, at)
	if err != nil {
		return fmt.Errorf("touch api key usage: %w", err)
	}
	return nil
}

// FindKeyCandidates is a pre-authentication locator: it intentionally scans
// key_prefix across ALL tenants, bounded at 50 rows. The service verifies the
// secret, expiry and revocation before trusting the row's tenant_id.
func (r *IdentityRepo) FindKeyCandidates(ctx context.Context, prefix string) ([]auth.APIKeyRecord, error) {
	rows, err := r.db.QueryContext(ctx,
		`SELECT `+apiKeyColumns+` FROM api_keys
		 WHERE key_prefix = $1 AND revoked_at IS NULL
		 ORDER BY created_at
		 LIMIT 50`,
		prefix)
	if err != nil {
		return nil, fmt.Errorf("find key candidates: %w", err)
	}
	defer rows.Close()

	var out []auth.APIKeyRecord
	for rows.Next() {
		k, err := scanAPIKey(rows)
		if err != nil {
			return nil, fmt.Errorf("find key candidates: scan: %w", err)
		}
		out = append(out, k)
	}
	if err := rows.Err(); err != nil {
		return nil, fmt.Errorf("find key candidates: %w", err)
	}
	return out, nil
}

// --- sessions --------------------------------------------------------------

const sessionColumns = `id, tenant_id::text, user_id, token_hash, csrf_token,
	created_at, expires_at, idle_expires_at, revoked_at, revoke_reason`

func scanSession(row rowScanner) (auth.Session, error) {
	var s auth.Session
	var reason sql.NullString
	err := row.Scan(&s.ID, &s.TenantID, &s.UserID, &s.TokenHash, &s.CSRFToken,
		&s.CreatedAt, &s.ExpiresAt, &s.IdleExpiresAt, &s.RevokedAt, &reason)
	if err != nil {
		return auth.Session{}, err
	}
	s.RevokeReason = reason.String
	return s, nil
}

func (r *IdentityRepo) CreateSession(ctx context.Context, tenantID string, s auth.Session) error {
	_, err := r.db.ExecContext(ctx,
		`INSERT INTO sessions
		 (tenant_id, id, user_id, token_hash, csrf_token,
		  expires_at, idle_expires_at, revoked_at, revoke_reason)
		 VALUES ($1::uuid, $2, $3, $4, $5, $6, $7, $8, $9)`,
		tenantID, s.ID, s.UserID, s.TokenHash, s.CSRFToken,
		s.ExpiresAt, s.IdleExpiresAt, s.RevokedAt, nullStr(s.RevokeReason))
	if err != nil {
		return fmt.Errorf("create session: %w", err)
	}
	return nil
}

func (r *IdentityRepo) GetSession(ctx context.Context, tenantID, id string) (auth.Session, error) {
	s, err := scanSession(r.db.QueryRowContext(ctx,
		`SELECT `+sessionColumns+` FROM sessions
		 WHERE tenant_id = $1::uuid AND id = $2`,
		tenantID, id))
	if err != nil {
		if errors.Is(err, sql.ErrNoRows) || isInvalidUUID(err) {
			return auth.Session{}, auth.ErrNotFound
		}
		return auth.Session{}, fmt.Errorf("get session: %w", err)
	}
	return s, nil
}

// GetSessionByTokenHash is a pre-authentication locator: token_hash is UNIQUE,
// so this looks up the row across ALL tenants. The service verifies expiry,
// idle timeout and revocation before trusting the row's tenant_id.
func (r *IdentityRepo) GetSessionByTokenHash(ctx context.Context, tokenHash string) (auth.Session, error) {
	s, err := scanSession(r.db.QueryRowContext(ctx,
		`SELECT `+sessionColumns+` FROM sessions
		 WHERE token_hash = $1`,
		tokenHash))
	if err != nil {
		if errors.Is(err, sql.ErrNoRows) || isInvalidUUID(err) {
			return auth.Session{}, auth.ErrNotFound
		}
		return auth.Session{}, fmt.Errorf("get session by token hash: %w", err)
	}
	return s, nil
}

func (r *IdentityRepo) TouchSession(ctx context.Context, tenantID, id string, idleUntil time.Time) error {
	_, err := r.db.ExecContext(ctx,
		`UPDATE sessions SET idle_expires_at = $3
		 WHERE tenant_id = $1::uuid AND id = $2`,
		tenantID, id, idleUntil)
	if err != nil {
		return fmt.Errorf("touch session: %w", err)
	}
	return nil
}

func (r *IdentityRepo) RevokeSession(ctx context.Context, tenantID, id string, at time.Time, reason string) error {
	_, err := r.db.ExecContext(ctx,
		`UPDATE sessions SET revoked_at = $3, revoke_reason = $4
		 WHERE tenant_id = $1::uuid AND id = $2`,
		tenantID, id, at, nullStr(reason))
	if err != nil {
		return fmt.Errorf("revoke session: %w", err)
	}
	return nil
}

func (r *IdentityRepo) RevokeUserSessions(ctx context.Context, tenantID, userID string, at time.Time, reason string) error {
	_, err := r.db.ExecContext(ctx,
		`UPDATE sessions SET revoked_at = $3, revoke_reason = $4
		 WHERE tenant_id = $1::uuid AND user_id = $2 AND revoked_at IS NULL`,
		tenantID, userID, at, nullStr(reason))
	if err != nil {
		return fmt.Errorf("revoke user sessions: %w", err)
	}
	return nil
}

// --- external identities ---------------------------------------------------

func (r *IdentityRepo) RecordAuthIdentity(ctx context.Context, tenantID string, a auth.AuthIdentity) error {
	_, err := r.db.ExecContext(ctx,
		`INSERT INTO auth_identities
		 (tenant_id, id, user_id, issuer, subject, linked_at, linked_by_user_id)
		 VALUES ($1::uuid, $2, $3, $4, $5, $6, $7)`,
		tenantID, a.ID, a.UserID, a.Issuer, a.Subject,
		a.LinkedAt, nullStr(a.LinkedByUserID))
	if err != nil {
		return fmt.Errorf("record auth identity: %w", err)
	}
	return nil
}

func (r *IdentityRepo) GetAuthIdentity(ctx context.Context, tenantID, issuer, subject string) (auth.AuthIdentity, error) {
	var a auth.AuthIdentity
	var linkedBy sql.NullString
	err := r.db.QueryRowContext(ctx,
		`SELECT id, user_id, issuer, subject, linked_at, linked_by_user_id
		 FROM auth_identities
		 WHERE tenant_id = $1::uuid AND issuer = $2 AND subject = $3`,
		tenantID, issuer, subject).
		Scan(&a.ID, &a.UserID, &a.Issuer, &a.Subject, &a.LinkedAt, &linkedBy)
	if err != nil {
		if errors.Is(err, sql.ErrNoRows) || isInvalidUUID(err) {
			return auth.AuthIdentity{}, auth.ErrNotFound
		}
		if errors.Is(err, sql.ErrNoRows) || isInvalidUUID(err) {
			return auth.AuthIdentity{}, auth.ErrNotFound
		}
		return auth.AuthIdentity{}, fmt.Errorf("get auth identity: %w", err)
	}
	a.LinkedByUserID = linkedBy.String
	return a, nil
}

// --- delegations -----------------------------------------------------------

const delegationColumns = `id, tenant_id::text, delegator_user_id,
	delegate_service_account_id, allowed_scopes, allowed_collections,
	expires_at, revoked_at, created_at`

func scanDelegation(row rowScanner) (auth.Delegation, error) {
	var d auth.Delegation
	err := row.Scan(&d.ID, &d.TenantID, &d.DelegatorUserID,
		&d.DelegateServiceAccountID, &d.AllowedScopes, &d.AllowedCollections,
		&d.ExpiresAt, &d.RevokedAt, &d.CreatedAt)
	if err != nil {
		return auth.Delegation{}, err
	}
	d.AllowedScopes = scanRaw(d.AllowedScopes)
	d.AllowedCollections = scanRaw(d.AllowedCollections)
	return d, nil
}

func (r *IdentityRepo) CreateDelegation(ctx context.Context, tenantID string, d auth.Delegation) error {
	_, err := r.db.ExecContext(ctx,
		`INSERT INTO delegations
		 (tenant_id, id, delegator_user_id, delegate_service_account_id,
		  allowed_scopes, allowed_collections, expires_at, revoked_at)
		 VALUES ($1::uuid, $2, $3, $4, $5::jsonb, $6::jsonb, $7, $8)`,
		tenantID, d.ID, d.DelegatorUserID, d.DelegateServiceAccountID,
		jsonbArg(d.AllowedScopes), jsonbArg(d.AllowedCollections),
		d.ExpiresAt, d.RevokedAt)
	if err != nil {
		return fmt.Errorf("create delegation: %w", err)
	}
	return nil
}

func (r *IdentityRepo) GetDelegation(ctx context.Context, tenantID, id string) (auth.Delegation, error) {
	d, err := scanDelegation(r.db.QueryRowContext(ctx,
		`SELECT `+delegationColumns+` FROM delegations
		 WHERE tenant_id = $1::uuid AND id = $2`,
		tenantID, id))
	if err != nil {
		if errors.Is(err, sql.ErrNoRows) || isInvalidUUID(err) {
			return auth.Delegation{}, auth.ErrNotFound
		}
		return auth.Delegation{}, fmt.Errorf("get delegation: %w", err)
	}
	return d, nil
}

func (r *IdentityRepo) FindDelegation(ctx context.Context, tenantID, delegatorUserID, delegateServiceAccountID string) (auth.Delegation, error) {
	d, err := scanDelegation(r.db.QueryRowContext(ctx,
		`SELECT `+delegationColumns+` FROM delegations
		 WHERE tenant_id = $1::uuid
		   AND delegator_user_id = $2
		   AND delegate_service_account_id = $3
		   AND revoked_at IS NULL
		 ORDER BY created_at DESC
		 LIMIT 1`,
		tenantID, delegatorUserID, delegateServiceAccountID))
	if err != nil {
		if errors.Is(err, sql.ErrNoRows) || isInvalidUUID(err) {
			return auth.Delegation{}, auth.ErrNotFound
		}
		return auth.Delegation{}, fmt.Errorf("find delegation: %w", err)
	}
	return d, nil
}

// --- OIDC providers --------------------------------------------------------

const oidcProviderColumns = `id, name, issuer, client_id, client_secret_ref,
	redirect_path, enabled, created_at`

func scanOIDCProvider(row rowScanner) (auth.OIDCProvider, error) {
	var p auth.OIDCProvider
	err := row.Scan(&p.ID, &p.Name, &p.Issuer, &p.ClientID, &p.ClientSecretRef,
		&p.RedirectPath, &p.Enabled, &p.CreatedAt)
	if err != nil {
		return auth.OIDCProvider{}, err
	}
	return p, nil
}

func (r *IdentityRepo) GetOIDCProvider(ctx context.Context, tenantID, id string) (auth.OIDCProvider, error) {
	p, err := scanOIDCProvider(r.db.QueryRowContext(ctx,
		`SELECT `+oidcProviderColumns+` FROM oidc_providers
		 WHERE tenant_id = $1::uuid AND id = $2`,
		tenantID, id))
	if err != nil {
		if errors.Is(err, sql.ErrNoRows) || isInvalidUUID(err) {
			return auth.OIDCProvider{}, auth.ErrNotFound
		}
		return auth.OIDCProvider{}, fmt.Errorf("get oidc provider: %w", err)
	}
	return p, nil
}

func (r *IdentityRepo) ListOIDCProviders(ctx context.Context, tenantID string) ([]auth.OIDCProvider, error) {
	rows, err := r.db.QueryContext(ctx,
		`SELECT `+oidcProviderColumns+` FROM oidc_providers
		 WHERE tenant_id = $1::uuid
		 ORDER BY created_at, id`,
		tenantID)
	if err != nil {
		return nil, fmt.Errorf("list oidc providers: %w", err)
	}
	defer rows.Close()

	var out []auth.OIDCProvider
	for rows.Next() {
		p, err := scanOIDCProvider(rows)
		if err != nil {
			return nil, fmt.Errorf("list oidc providers: scan: %w", err)
		}
		out = append(out, p)
	}
	if err := rows.Err(); err != nil {
		return nil, fmt.Errorf("list oidc providers: %w", err)
	}
	return out, nil
}

func (r *IdentityRepo) CreateOIDCProvider(ctx context.Context, tenantID string, p auth.OIDCProvider) error {
	_, err := r.db.ExecContext(ctx,
		`INSERT INTO oidc_providers
		 (tenant_id, id, name, issuer, client_id, client_secret_ref,
		  redirect_path, enabled)
		 VALUES ($1::uuid, $2, $3, $4, $5, $6, $7, $8)`,
		tenantID, p.ID, p.Name, p.Issuer, p.ClientID, p.ClientSecretRef,
		p.RedirectPath, p.Enabled)
	if err != nil {
		return fmt.Errorf("create oidc provider: %w", err)
	}
	return nil
}

// --- tenants ---------------------------------------------------------------

func (r *IdentityRepo) GetTenantBySlug(ctx context.Context, slug string) (string, error) {
	var id string
	err := r.db.QueryRowContext(ctx,
		`SELECT id FROM tenants WHERE slug = $1`, slug).Scan(&id)
	if err != nil {
		if errors.Is(err, sql.ErrNoRows) {
			return "", auth.ErrNotFound
		}
		return "", fmt.Errorf("get tenant by slug: %w", err)
	}
	return id, nil
}

// --- OIDC states -----------------------------------------------------------

const oidcStateColumns = `id, state_hash, provider_id, COALESCE(user_id::text,''),
	nonce, pkce_verifier, return_to, subject, email,
	COALESCE(pending_link_user_id,''), expires_at, consumed_at, created_at`

func scanOIDCState(row rowScanner, tenantID string) (auth.OIDCState, error) {
	var s auth.OIDCState
	s.TenantID = tenantID
	err := row.Scan(&s.ID, &s.StateHash, &s.ProviderID, &s.UserID,
		&s.Nonce, &s.PKCEVerifier, &s.ReturnTo, &s.Subject, &s.Email,
		&s.PendingLinkUserID, &s.ExpiresAt, &s.ConsumedAt, &s.CreatedAt)
	if err != nil {
		return auth.OIDCState{}, err
	}
	return s, nil
}

func (r *IdentityRepo) CreateOIDCState(ctx context.Context, tenantID string, s auth.OIDCState) error {
	_, err := r.db.ExecContext(ctx,
		`INSERT INTO oidc_states
		 (tenant_id, id, state_hash, provider_id, user_id, nonce,
		  pkce_verifier, return_to, subject, email, pending_link_user_id,
		  expires_at, consumed_at)
		 VALUES ($1::uuid, $2, $3, $4, NULLIF($5,''), $6, $7, $8, $9, $10,
		         NULLIF($11,''), $12, $13)`,
		tenantID, s.ID, s.StateHash, s.ProviderID, s.UserID, s.Nonce,
		s.PKCEVerifier, s.ReturnTo, s.Subject, s.Email,
		s.PendingLinkUserID, s.ExpiresAt, s.ConsumedAt)
	if err != nil {
		return fmt.Errorf("create oidc state: %w", err)
	}
	return nil
}

func (r *IdentityRepo) GetOIDCState(ctx context.Context, tenantID, stateHash string) (auth.OIDCState, error) {
	s, err := scanOIDCState(r.db.QueryRowContext(ctx,
		`SELECT `+oidcStateColumns+` FROM oidc_states
		 WHERE tenant_id = $1::uuid AND state_hash = $2`,
		tenantID, stateHash), tenantID)
	if err != nil {
		if errors.Is(err, sql.ErrNoRows) || isInvalidUUID(err) {
			return auth.OIDCState{}, auth.ErrNotFound
		}
		if errors.Is(err, sql.ErrNoRows) || isInvalidUUID(err) {
			return auth.OIDCState{}, auth.ErrNotFound
		}
		return auth.OIDCState{}, fmt.Errorf("get oidc state: %w", err)
	}
	return s, nil
}

// ConsumeOIDCState performs the single-use transition: only a row still
// unconsumed flips consumed_at and returns itself; a replay or unknown hash
// reports auth.ErrNotFound.
func (r *IdentityRepo) ConsumeOIDCState(ctx context.Context, tenantID, stateHash string) (auth.OIDCState, error) {
	s, err := scanOIDCState(r.db.QueryRowContext(ctx,
		`UPDATE oidc_states SET consumed_at = now()
		 WHERE tenant_id = $1::uuid AND state_hash = $2 AND consumed_at IS NULL
		 RETURNING `+oidcStateColumns,
		tenantID, stateHash), tenantID)
	if err != nil {
		if errors.Is(err, sql.ErrNoRows) || isInvalidUUID(err) {
			return auth.OIDCState{}, auth.ErrNotFound
		}
		if errors.Is(err, sql.ErrNoRows) || isInvalidUUID(err) {
			return auth.OIDCState{}, auth.ErrNotFound
		}
		return auth.OIDCState{}, fmt.Errorf("consume oidc state: %w", err)
	}
	return s, nil
}
