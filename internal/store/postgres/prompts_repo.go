package postgres

import (
	"context"
	"database/sql"
	"encoding/json"
	"errors"
	"fmt"

	"enclava-go/internal/prompts"
)

// PromptsRepo implements prompts.Repository over the tenant-scoped
// prompt_templates domain table. Every statement is pinned to tenant_id;
// missing, malformed-tenant and cross-tenant lookups all read as
// sql.ErrNoRows so unknown and forbidden share one outcome.
type PromptsRepo struct {
	db *DB
}

func NewPromptsRepo(db *DB) *PromptsRepo {
	return &PromptsRepo{db: db}
}

var _ prompts.Repository = (*PromptsRepo)(nil)

const promptTemplateSelect = `
	SELECT tenant_id::text, id, name, description, system_prompt, user_prompt,
	       variables_schema, is_default, is_active, COALESCE(owner_actor_id, ''), created_at
	FROM prompt_templates`

type promptScanner interface {
	Scan(dest ...any) error
}

func scanPromptTemplate(s promptScanner) (prompts.Template, error) {
	var t prompts.Template
	var schema []byte
	err := s.Scan(&t.TenantID, &t.ID, &t.Name, &t.Description, &t.SystemPrompt,
		&t.UserPrompt, &schema, &t.IsDefault, &t.IsActive, &t.OwnerActorID, &t.CreatedAt)
	if err != nil {
		return prompts.Template{}, err
	}
	t.VariablesSchema = map[string]any{}
	_ = json.Unmarshal(schema, &t.VariablesSchema)
	t.UpdatedAt = t.CreatedAt
	return t, nil
}

func (r *PromptsRepo) Create(ctx context.Context, t prompts.Template) error {
	schema, err := json.Marshal(map[string]any(t.VariablesSchema))
	if err != nil {
		return fmt.Errorf("marshal prompt variables_schema: %w", err)
	}
	_, err = r.db.SQL.ExecContext(ctx, `
		INSERT INTO prompt_templates
			(tenant_id, id, name, description, system_prompt, user_prompt,
			 variables_schema, is_default, is_active, owner_actor_id)
		VALUES ($1, $2, $3, $4, $5, $6, $7, $8, $9, $10)`,
		t.TenantID, t.ID, t.Name, t.Description, t.SystemPrompt, t.UserPrompt,
		schema, t.IsDefault, t.IsActive, nullableText(t.OwnerActorID))
	if err != nil {
		return fmt.Errorf("create prompt template: %w", err)
	}
	return nil
}

// EnsureDefaults inserts the built-in templates for a tenant when it has
// none. Idempotent — safe to call on first use.
func (r *PromptsRepo) EnsureDefaults(ctx context.Context, tenantID string, templates []prompts.Template) error {
	var count int
	if err := r.db.SQL.QueryRowContext(ctx,
		`SELECT COUNT(1) FROM prompt_templates WHERE tenant_id = $1`, tenantID).Scan(&count); err != nil {
		return fmt.Errorf("count prompt templates: %w", err)
	}
	if count > 0 {
		return nil
	}
	for _, t := range templates {
		t.TenantID = tenantID
		if err := r.Create(ctx, t); err != nil {
			return err
		}
	}
	return nil
}

func (r *PromptsRepo) List(ctx context.Context, tenantID string) ([]prompts.Template, error) {
	rows, err := r.db.SQL.QueryContext(ctx,
		promptTemplateSelect+` WHERE tenant_id = $1 ORDER BY is_default DESC, name ASC`, tenantID)
	if err != nil {
		return nil, fmt.Errorf("list prompt templates: %w", err)
	}
	defer func() { _ = rows.Close() }()

	out := []prompts.Template{}
	for rows.Next() {
		t, err := scanPromptTemplate(rows)
		if err != nil {
			return nil, err
		}
		out = append(out, t)
	}
	if err := rows.Err(); err != nil {
		return nil, fmt.Errorf("iterate prompt templates: %w", err)
	}
	return out, nil
}

func (r *PromptsRepo) GetByID(ctx context.Context, tenantID, id string) (prompts.Template, error) {
	t, err := scanPromptTemplate(r.db.SQL.QueryRowContext(ctx,
		promptTemplateSelect+` WHERE tenant_id = $1 AND id = $2`, tenantID, id))
	if errors.Is(err, sql.ErrNoRows) || isInvalidUUID(err) {
		return prompts.Template{}, sql.ErrNoRows
	}
	if err != nil {
		return prompts.Template{}, fmt.Errorf("get prompt template: %w", err)
	}
	return t, nil
}

// GetDefault returns the tenant's active default template, newest first.
func (r *PromptsRepo) GetDefault(ctx context.Context, tenantID string) (prompts.Template, error) {
	t, err := scanPromptTemplate(r.db.SQL.QueryRowContext(ctx,
		promptTemplateSelect+` WHERE tenant_id = $1 AND is_default AND is_active
		ORDER BY created_at DESC LIMIT 1`, tenantID))
	if errors.Is(err, sql.ErrNoRows) || isInvalidUUID(err) {
		return prompts.Template{}, sql.ErrNoRows
	}
	if err != nil {
		return prompts.Template{}, fmt.Errorf("get default prompt template: %w", err)
	}
	return t, nil
}

func (r *PromptsRepo) Update(ctx context.Context, t prompts.Template) error {
	schema, err := json.Marshal(map[string]any(t.VariablesSchema))
	if err != nil {
		return fmt.Errorf("marshal prompt variables_schema: %w", err)
	}
	res, err := r.db.SQL.ExecContext(ctx, `
		UPDATE prompt_templates
		SET name = $3, description = $4, system_prompt = $5, user_prompt = $6,
		    variables_schema = $7, is_default = $8, is_active = $9
		WHERE tenant_id = $1 AND id = $2`,
		t.TenantID, t.ID, t.Name, t.Description, t.SystemPrompt, t.UserPrompt,
		schema, t.IsDefault, t.IsActive)
	if err != nil {
		return fmt.Errorf("update prompt template: %w", err)
	}
	if n, _ := res.RowsAffected(); n == 0 {
		return sql.ErrNoRows
	}
	return nil
}

func (r *PromptsRepo) Delete(ctx context.Context, tenantID, id string) error {
	res, err := r.db.SQL.ExecContext(ctx,
		`DELETE FROM prompt_templates WHERE tenant_id = $1 AND id = $2`, tenantID, id)
	if err != nil {
		return fmt.Errorf("delete prompt template: %w", err)
	}
	if n, _ := res.RowsAffected(); n == 0 {
		return sql.ErrNoRows
	}
	return nil
}

