package sqlite

import (
	"context"
	"database/sql"
	"encoding/json"
	"fmt"
	"time"

	"enclava-go/internal/auth"
)

type APIKeysRepo struct {
	db *sql.DB
}

func NewAPIKeysRepo(db *sql.DB) *APIKeysRepo {
	return &APIKeysRepo{db: db}
}

func (r *APIKeysRepo) FindCandidatesByPrefix(ctx context.Context, prefix string) ([]auth.APIKeyRecord, error) {
	rows, err := r.db.QueryContext(ctx, `
		SELECT
			id, name, key_prefix, key_hash, key_hash_algo,
			is_active, expires_at,
			scopes, allowed_models, allowed_extract_templates, allowed_chatbots, allowed_agents, allowed_endpoints,
			allowed_ips,
			rate_limit_per_minute, rate_limit_per_hour, rate_limit_per_day,
			is_unlimited, budget_limit_tokens, budget_limit_cents, budget_period
		FROM api_keys
		WHERE key_prefix = ?
		  AND is_deleted = 0
	`, prefix)
	if err != nil {
		return nil, fmt.Errorf("query api key candidates: %w", err)
	}
	defer func() {
		_ = rows.Close()
	}()

	out := []auth.APIKeyRecord{}
	for rows.Next() {
		var rec auth.APIKeyRecord
		var isActive int
		var isUnlimited int
		var expires sql.NullString
		var scopesRaw, modelsRaw, templatesRaw, chatbotsRaw, agentsRaw, endpointsRaw string
		var allowedIPsRaw string
		var budgetPeriod sql.NullString
		var budgetTokens sql.NullInt64
		var budgetCents sql.NullInt64
		var rlMin sql.NullInt64
		var rlHour sql.NullInt64
		var rlDay sql.NullInt64

		var legacyID int64
		if err := rows.Scan(
			&legacyID,
			&rec.Name,
			&rec.KeyPrefix,
			&rec.KeyHash,
			&rec.KeyHashAlgo,
			&isActive,
			&expires,
			&scopesRaw,
			&modelsRaw,
			&templatesRaw,
			&chatbotsRaw,
			&agentsRaw,
			&endpointsRaw,
			&allowedIPsRaw,
			&rlMin,
			&rlHour,
			&rlDay,
			&isUnlimited,
			&budgetTokens,
			&budgetCents,
			&budgetPeriod,
		); err != nil {
			return nil, fmt.Errorf("scan api key candidate: %w", err)
		}

		rec.ID = fmt.Sprintf("%d", legacyID)
		rec.IsActive = isActive == 1
		// Legacy columns store string lists; map onto the canonical
		// mode/list grant documents (empty or "*" list → mode:all, matching
		// the old unrestricted storage contract until this store is ported).
		rec.Scopes = legacyGrant(unmarshalStringList(scopesRaw))
		rec.AllowedModels = legacyGrant(unmarshalStringList(modelsRaw))
		rec.AllowedTemplates = legacyGrant(unmarshalStringList(templatesRaw))
		rec.AllowedChatbots = legacyGrant(unmarshalStringList(chatbotsRaw))
		rec.AllowedAgents = legacyGrant(unmarshalStringList(agentsRaw))
		rec.AllowedEndpoints = legacyGrant(unmarshalStringList(endpointsRaw))
		rec.AllowedIPs = legacyGrant(unmarshalStringList(allowedIPsRaw))
		if rlMin.Valid {
			v := int(rlMin.Int64)
			rec.RateLimitPerMinute = &v
		}
		if rlHour.Valid {
			v := int(rlHour.Int64)
			rec.RateLimitPerHour = &v
		}
		if rlDay.Valid {
			v := int(rlDay.Int64)
			rec.RateLimitPerDay = &v
		}
		rec.IsUnlimited = isUnlimited == 1
		if budgetTokens.Valid {
			v := budgetTokens.Int64
			rec.BudgetLimitTokens = &v
		}
		if budgetCents.Valid {
			v := budgetCents.Int64
			rec.BudgetLimitCents = &v
		}
		if budgetPeriod.Valid {
			rec.BudgetPeriod = budgetPeriod.String
		}
		if expires.Valid && expires.String != "" {
			t, err := parseDBTime(expires.String)
			if err == nil {
				rec.ExpiresAt = &t
			}
		}
		out = append(out, rec)
	}

	return out, nil
}

// legacyGrant maps a legacy stored string list to a canonical grant
// document: empty list or a "*" item is the old unrestricted contract →
// {"mode":"all"}; otherwise an explicit {"mode":"list"} row.
func legacyGrant(values []string) json.RawMessage {
	for _, v := range values {
		if v == "*" {
			return json.RawMessage(`{"mode":"all"}`)
		}
	}
	if len(values) == 0 {
		return json.RawMessage(`{"mode":"all"}`)
	}
	raw, err := json.Marshal(struct {
		Mode  string   `json:"mode"`
		Items []string `json:"items"`
	}{Mode: "list", Items: values})
	if err != nil {
		return json.RawMessage(`{"mode":"list","items":[]}`)
	}
	return raw
}

func (r *APIKeysRepo) TouchUsage(ctx context.Context, id int64) error {
	_, err := r.db.ExecContext(ctx, `
		UPDATE api_keys
		SET total_requests = total_requests + 1,
		    last_used_at = datetime('now'),
		    updated_at = datetime('now')
		WHERE id = ?
		  AND is_deleted = 0
	`, id)
	if err != nil {
		return fmt.Errorf("touch API key usage: %w", err)
	}
	return nil
}

func (r *APIKeysRepo) EnsureBootstrapKey(ctx context.Context, plaintext, algo, pepper string, minLength int) error {
	if plaintext == "" {
		return nil
	}
	if minLength <= 0 {
		minLength = 48
	}
	if len(plaintext) < minLength {
		return fmt.Errorf("bootstrap key length must be at least %d", minLength)
	}
	prefix, hash, usedAlgo, err := auth.HashForStorage(plaintext, algo, pepper)
	if err != nil {
		return fmt.Errorf("hash bootstrap key: %w", err)
	}
	var count int
	if err := r.db.QueryRowContext(ctx, `
		SELECT COUNT(1)
		FROM api_keys
		WHERE key_hash = ?
		   OR (name = 'bootstrap' AND key_prefix = ?)
	`, hash, prefix).Scan(&count); err != nil {
		return fmt.Errorf("check bootstrap key: %w", err)
	}
	if count > 0 {
		return nil
	}

	// Bootstrap key is intended for operators. It can be used to provision other keys and inspect usage.
	scopes := []string{
		"platform.admin",
		"api_keys.manage",
		"analytics.read",
		"budgets.manage",
		"models.list",
		"chat.completions",
		"embeddings.create",
		"responses",
		"openai.proxy",
		"extract",
		"extract.manage",
		"rag",
		"rag.manage",
		"chatbot",
		"chatbot.manage",
		"connectors.manage",
		"pricing.manage",
	}
	scopesJSON, err := marshalJSON(scopes)
	if err != nil {
		return fmt.Errorf("marshal bootstrap scopes: %w", err)
	}
	_, err = r.db.ExecContext(ctx, `
		INSERT INTO api_keys(name, key_prefix, key_hash, key_hash_algo, is_active, expires_at, scopes, allowed_models, allowed_endpoints, allowed_extract_templates, allowed_chatbots, allowed_agents)
		VALUES (?, ?, ?, ?, 1, NULL, ?, '[]', '[]', '[]', '[]', '[]')
	`, "bootstrap", prefix, hash, usedAlgo, scopesJSON)
	if err != nil {
		return fmt.Errorf("insert bootstrap key: %w", err)
	}
	return nil
}

func (r *APIKeysRepo) CreateAPIKey(ctx context.Context, name, plaintext, algo, pepper string, scopes []string) (int64, error) {
	prefix, hash, usedAlgo, err := auth.HashForStorage(plaintext, algo, pepper)
	if err != nil {
		return 0, err
	}
	scopesJSON, err := marshalJSON(scopes)
	if err != nil {
		return 0, fmt.Errorf("marshal scopes: %w", err)
	}
	res, err := r.db.ExecContext(ctx, `
		INSERT INTO api_keys(name, key_prefix, key_hash, key_hash_algo, is_active, expires_at, scopes, allowed_models, allowed_endpoints, allowed_extract_templates, allowed_chatbots, allowed_agents)
		VALUES (?, ?, ?, ?, 0, NULL, ?, '[]', '[]', '[]', '[]', '[]')
	`, name, prefix, hash, usedAlgo, scopesJSON)
	if err != nil {
		return 0, fmt.Errorf("create api key: %w", err)
	}
	id, _ := res.LastInsertId()
	return id, nil
}

func (r *APIKeysRepo) List(ctx context.Context, includeDeleted bool) ([]APIKey, error) {
	where := "1=1"
	if !includeDeleted {
		where = "is_deleted = 0"
	}
	rows, err := r.db.QueryContext(ctx, fmt.Sprintf(`
		SELECT
			id, name, description,
			key_prefix, key_hash, key_hash_algo,
			is_active, is_deleted, deleted_at, expires_at,
			scopes, allowed_models, allowed_endpoints, allowed_extract_templates, allowed_chatbots, allowed_agents, allowed_ips,
			rate_limit_per_minute, rate_limit_per_hour, rate_limit_per_day,
			is_unlimited, budget_limit_tokens, budget_limit_cents, budget_period,
			tags,
			total_requests, total_tokens, total_cost_cents, total_cost_microcents, last_used_at,
			created_at, updated_at
		FROM api_keys
		WHERE %s
		ORDER BY created_at DESC
	`, where))
	if err != nil {
		return nil, fmt.Errorf("list api keys: %w", err)
	}
	defer func() {
		_ = rows.Close()
	}()

	out := []APIKey{}
	for rows.Next() {
		k, err := scanAPIKey(rows)
		if err != nil {
			return nil, err
		}
		out = append(out, k)
	}
	return out, nil
}

func (r *APIKeysRepo) GetByID(ctx context.Context, id int64) (APIKey, error) {
	row := r.db.QueryRowContext(ctx, `
		SELECT
			id, name, description,
			key_prefix, key_hash, key_hash_algo,
			is_active, is_deleted, deleted_at, expires_at,
			scopes, allowed_models, allowed_endpoints, allowed_extract_templates, allowed_chatbots, allowed_agents, allowed_ips,
			rate_limit_per_minute, rate_limit_per_hour, rate_limit_per_day,
			is_unlimited, budget_limit_tokens, budget_limit_cents, budget_period,
			tags,
			total_requests, total_tokens, total_cost_cents, total_cost_microcents, last_used_at,
			created_at, updated_at
		FROM api_keys
		WHERE id = ?
	`, id)
	return scanAPIKey(row)
}

func (r *APIKeysRepo) Update(ctx context.Context, k APIKey) error {
	scopesJSON, err := marshalJSON(k.Scopes)
	if err != nil {
		return fmt.Errorf("marshal scopes: %w", err)
	}
	modelsJSON, err := marshalJSON(k.AllowedModels)
	if err != nil {
		return fmt.Errorf("marshal allowed_models: %w", err)
	}
	endpointsJSON, err := marshalJSON(k.AllowedEndpoints)
	if err != nil {
		return fmt.Errorf("marshal allowed_endpoints: %w", err)
	}
	templatesJSON, err := marshalJSON(k.AllowedExtractTemplates)
	if err != nil {
		return fmt.Errorf("marshal allowed_extract_templates: %w", err)
	}
	chatbotsJSON, err := marshalJSON(k.AllowedChatbots)
	if err != nil {
		return fmt.Errorf("marshal allowed_chatbots: %w", err)
	}
	agentsJSON, err := marshalJSON(k.AllowedAgents)
	if err != nil {
		return fmt.Errorf("marshal allowed_agents: %w", err)
	}
	ipsJSON, err := marshalJSON(k.AllowedIPs)
	if err != nil {
		return fmt.Errorf("marshal allowed_ips: %w", err)
	}
	tagsJSON, err := marshalJSON(k.Tags)
	if err != nil {
		return fmt.Errorf("marshal tags: %w", err)
	}
	_, err = r.db.ExecContext(ctx, `
		UPDATE api_keys
		SET name = ?,
		    description = ?,
		    is_active = ?,
		    expires_at = ?,
		    scopes = ?,
		    allowed_models = ?,
		    allowed_endpoints = ?,
		    allowed_extract_templates = ?,
		    allowed_chatbots = ?,
		    allowed_agents = ?,
		    allowed_ips = ?,
		    rate_limit_per_minute = ?,
		    rate_limit_per_hour = ?,
		    rate_limit_per_day = ?,
		    is_unlimited = ?,
		    budget_limit_tokens = ?,
		    budget_limit_cents = ?,
		    budget_period = ?,
		    tags = ?,
		    updated_at = datetime('now')
		WHERE id = ?
	`, k.Name,
		nullableString(k.Description),
		boolToInt(k.IsActive),
		nullableTime(k.ExpiresAt),
		scopesJSON,
		modelsJSON,
		endpointsJSON,
		templatesJSON,
		chatbotsJSON,
		agentsJSON,
		ipsJSON,
		nullableInt(k.RateLimitPerMinute),
		nullableInt(k.RateLimitPerHour),
		nullableInt(k.RateLimitPerDay),
		boolToInt(k.IsUnlimited),
		nullableInt64(k.BudgetLimitTokens),
		nullableInt64(k.BudgetLimitCents),
		nullableString(k.BudgetPeriod),
		tagsJSON,
		k.ID,
	)
	if err != nil {
		return fmt.Errorf("update api key: %w", err)
	}
	return nil
}

func (r *APIKeysRepo) SetSecret(ctx context.Context, id int64, prefix, hash, algo string) error {
	_, err := r.db.ExecContext(ctx, `
		UPDATE api_keys
		SET key_prefix = ?,
		    key_hash = ?,
		    key_hash_algo = ?,
		    updated_at = datetime('now')
		WHERE id = ?
		  AND is_deleted = 0
	`, prefix, hash, algo, id)
	if err != nil {
		return fmt.Errorf("update api key secret: %w", err)
	}
	return nil
}

func (r *APIKeysRepo) SoftDelete(ctx context.Context, id int64) error {
	_, err := r.db.ExecContext(ctx, `
		UPDATE api_keys
		SET is_deleted = 1,
		    is_active = 0,
		    deleted_at = datetime('now'),
		    updated_at = datetime('now')
		WHERE id = ?
	`, id)
	if err != nil {
		return fmt.Errorf("soft delete api key: %w", err)
	}
	return nil
}

func (r *APIKeysRepo) Restore(ctx context.Context, id int64) error {
	_, err := r.db.ExecContext(ctx, `
		UPDATE api_keys
		SET is_deleted = 0,
		    is_active = 1,
		    deleted_at = NULL,
		    updated_at = datetime('now')
		WHERE id = ?
	`, id)
	if err != nil {
		return fmt.Errorf("restore api key: %w", err)
	}
	return nil
}

func (r *APIKeysRepo) AddUsage(ctx context.Context, apiKeyID int64, promptTokens, completionTokens, totalTokens, costCents int64) error {
	_, err := r.db.ExecContext(ctx, `
		UPDATE api_keys
		SET total_tokens = total_tokens + ?,
		    total_cost_cents = total_cost_cents + ?,
		    total_cost_microcents = total_cost_microcents + ?,
		    updated_at = datetime('now')
		WHERE id = ?
		  AND is_deleted = 0
	`, totalTokens, costCents, costCents*microcentsPerCent, apiKeyID)
	if err != nil {
		return fmt.Errorf("add api key usage: %w", err)
	}
	return nil
}

func (r *APIKeysRepo) ActivateKey(ctx context.Context, id int64) error {
	_, err := r.db.ExecContext(ctx, `
		UPDATE api_keys
		SET is_active = 1,
		    updated_at = datetime('now')
		WHERE id = ?
		  AND is_deleted = 0
	`, id)
	if err != nil {
		return fmt.Errorf("activate api key: %w", err)
	}
	return nil
}

type apiKeyScanner interface {
	Scan(dest ...any) error
}

func scanAPIKey(s apiKeyScanner) (APIKey, error) {
	var k APIKey
	var (
		desc        sql.NullString
		expires     sql.NullString
		deletedAt   sql.NullString
		scopesRaw   string
		modelsRaw   string
		endptsRaw   string
		tplsRaw     string
		chatbotsRaw string
		agentsRaw   string
		ipsRaw      string
		tagsRaw     string
		isActive    int
		isDeleted   int
		isUnlimited int
		rlMin       sql.NullInt64
		rlHour      sql.NullInt64
		rlDay       sql.NullInt64
		bTok        sql.NullInt64
		bCents      sql.NullInt64
		bPeriod     sql.NullString
		lastUsedAt  sql.NullString
		createdRaw  string
		updatedRaw  string
	)

	if err := s.Scan(
		&k.ID,
		&k.Name,
		&desc,
		&k.KeyPrefix,
		&k.KeyHash,
		&k.KeyHashAlgo,
		&isActive,
		&isDeleted,
		&deletedAt,
		&expires,
		&scopesRaw,
		&modelsRaw,
		&endptsRaw,
		&tplsRaw,
		&chatbotsRaw,
		&agentsRaw,
		&ipsRaw,
		&rlMin,
		&rlHour,
		&rlDay,
		&isUnlimited,
		&bTok,
		&bCents,
		&bPeriod,
		&tagsRaw,
		&k.TotalRequests,
		&k.TotalTokens,
		&k.TotalCostCents,
		&k.TotalCostMicrocents,
		&lastUsedAt,
		&createdRaw,
		&updatedRaw,
	); err != nil {
		return APIKey{}, fmt.Errorf("scan api key: %w", err)
	}

	k.IsActive = isActive == 1
	k.IsDeleted = isDeleted == 1
	k.IsUnlimited = isUnlimited == 1
	if desc.Valid {
		k.Description = desc.String
	}
	k.Scopes = unmarshalStringList(scopesRaw)
	k.AllowedModels = unmarshalStringList(modelsRaw)
	k.AllowedEndpoints = unmarshalStringList(endptsRaw)
	k.AllowedExtractTemplates = unmarshalStringList(tplsRaw)
	k.AllowedChatbots = unmarshalStringList(chatbotsRaw)
	k.AllowedAgents = unmarshalStringList(agentsRaw)
	k.AllowedIPs = unmarshalStringList(ipsRaw)
	k.Tags = unmarshalStringList(tagsRaw)
	if rlMin.Valid {
		v := int(rlMin.Int64)
		k.RateLimitPerMinute = &v
	}
	if rlHour.Valid {
		v := int(rlHour.Int64)
		k.RateLimitPerHour = &v
	}
	if rlDay.Valid {
		v := int(rlDay.Int64)
		k.RateLimitPerDay = &v
	}
	if bTok.Valid {
		v := bTok.Int64
		k.BudgetLimitTokens = &v
	}
	if bCents.Valid {
		v := bCents.Int64
		k.BudgetLimitCents = &v
	}
	if bPeriod.Valid {
		k.BudgetPeriod = bPeriod.String
	}

	if expires.Valid && expires.String != "" {
		t, err := parseDBTime(expires.String)
		if err == nil {
			k.ExpiresAt = &t
		}
	}
	if deletedAt.Valid && deletedAt.String != "" {
		t, err := parseDBTime(deletedAt.String)
		if err == nil {
			k.DeletedAt = &t
		}
	}
	if lastUsedAt.Valid && lastUsedAt.String != "" {
		t, err := parseDBTime(lastUsedAt.String)
		if err == nil {
			k.LastUsedAt = &t
		}
	}
	createdAt, err := parseDBTime(createdRaw)
	if err != nil {
		return APIKey{}, fmt.Errorf("parse api key created_at: %w", err)
	}
	updatedAt, err := parseDBTime(updatedRaw)
	if err != nil {
		return APIKey{}, fmt.Errorf("parse api key updated_at: %w", err)
	}
	k.CreatedAt = createdAt
	k.UpdatedAt = updatedAt
	return k, nil
}

func nullableTime(t *time.Time) any {
	if t == nil || t.IsZero() {
		return nil
	}
	return t.UTC().Format(time.RFC3339)
}

func nullableInt(v *int) any {
	if v == nil {
		return nil
	}
	return *v
}

func nullableInt64(v *int64) any {
	if v == nil {
		return nil
	}
	return *v
}
