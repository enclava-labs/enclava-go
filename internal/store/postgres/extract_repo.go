package postgres

import (
	"context"
	"database/sql"
	"encoding/json"
	"errors"
	"fmt"

	"enclava-go/internal/extract"
)

// ExtractRepo is the concrete tenant-scoped extract_templates /
// extract_settings store over migration 006. Every statement is keyed by
// (tenant_id, id); unknown and cross-tenant ids both return
// extract.ErrNotFound.
type ExtractRepo struct {
	db *DB
}

func NewExtractRepo(db *DB) *ExtractRepo {
	return &ExtractRepo{db: db}
}

var _ extract.TemplatesRepo = (*ExtractRepo)(nil)
var _ extract.SettingsRepo = (*ExtractRepo)(nil)

const extractTemplateCols = `id, name, description, system_prompt, user_prompt,
	variables_schema, output_schema, default_model, enabled, owner_actor_id,
	is_default, created_at`

func (r *ExtractRepo) ListTemplates(ctx context.Context, tenantID string) ([]extract.Template, error) {
	rows, err := r.db.SQL.QueryContext(ctx, `
		SELECT `+extractTemplateCols+`
		FROM extract_templates WHERE tenant_id = $1::uuid
		ORDER BY name, id`, tenantID)
	if err != nil {
		return nil, fmt.Errorf("list extract templates: %w", err)
	}
	defer func() { _ = rows.Close() }()
	out := []extract.Template{}
	for rows.Next() {
		t, err := scanExtractTemplate(rows)
		if err != nil {
			return nil, fmt.Errorf("scan extract template: %w", err)
		}
		out = append(out, t)
	}
	return out, rows.Err()
}

func (r *ExtractRepo) GetTemplate(ctx context.Context, tenantID, id string) (extract.Template, error) {
	t, err := scanExtractTemplate(r.db.SQL.QueryRowContext(ctx, `
		SELECT `+extractTemplateCols+`
		FROM extract_templates WHERE tenant_id = $1::uuid AND id = $2`, tenantID, id))
	if err != nil {
		if errors.Is(err, sql.ErrNoRows) || isInvalidUUID(err) {
			return extract.Template{}, extract.ErrNotFound
		}
		return extract.Template{}, fmt.Errorf("get extract template: %w", err)
	}
	return t, nil
}

func (r *ExtractRepo) UpsertTemplate(ctx context.Context, tenantID string, t extract.Template) error {
	ctxSchema, err := json.Marshal(t.ContextSchema)
	if err != nil {
		return fmt.Errorf("marshal context schema: %w", err)
	}
	outSchema, err := json.Marshal(t.OutputSchema)
	if err != nil {
		return fmt.Errorf("marshal output schema: %w", err)
	}
	_, err = r.db.SQL.ExecContext(ctx, `
		INSERT INTO extract_templates
			(tenant_id, id, name, description, system_prompt, user_prompt,
			 variables_schema, output_schema, default_model, enabled,
			 owner_actor_id, is_default)
		VALUES ($1::uuid, $2, $3, $4, $5, $6, $7, $8, $9, $10, $11, $12)
		ON CONFLICT (tenant_id, id) DO UPDATE SET
			name             = excluded.name,
			description      = excluded.description,
			system_prompt    = excluded.system_prompt,
			user_prompt      = excluded.user_prompt,
			variables_schema = excluded.variables_schema,
			output_schema    = excluded.output_schema,
			default_model    = excluded.default_model,
			enabled          = excluded.enabled,
			owner_actor_id   = excluded.owner_actor_id,
			is_default       = excluded.is_default`,
		tenantID, t.ID, t.Name, t.Description, t.SystemPrompt, t.UserPrompt,
		ctxSchema, outSchema, t.DefaultModel, t.Enabled,
		nullString(t.OwnerActorID), t.IsDefault)
	if err != nil {
		return fmt.Errorf("upsert extract template: %w", err)
	}
	return nil
}

func (r *ExtractRepo) DeleteTemplate(ctx context.Context, tenantID, id string) error {
	res, err := r.db.SQL.ExecContext(ctx, `
		DELETE FROM extract_templates WHERE tenant_id = $1::uuid AND id = $2`,
		tenantID, id)
	if err != nil {
		return fmt.Errorf("delete extract template: %w", err)
	}
	if n, _ := res.RowsAffected(); n == 0 {
		return extract.ErrNotFound
	}
	return nil
}

// ReplaceDefaultTemplates swaps the tenant's is_default set inside one
// transaction — tenant-authored templates are untouched.
func (r *ExtractRepo) ReplaceDefaultTemplates(ctx context.Context, tenantID string, templates []extract.Template) error {
	return WithTx(ctx, r.db.SQL, func(tx *sql.Tx) error {
		if _, err := tx.ExecContext(ctx, `
			DELETE FROM extract_templates
			WHERE tenant_id = $1::uuid AND is_default`, tenantID); err != nil {
			return fmt.Errorf("delete default templates: %w", err)
		}
		for _, t := range templates {
			ctxSchema, err := json.Marshal(t.ContextSchema)
			if err != nil {
				return fmt.Errorf("marshal context schema for %s: %w", t.ID, err)
			}
			outSchema, err := json.Marshal(t.OutputSchema)
			if err != nil {
				return fmt.Errorf("marshal output schema for %s: %w", t.ID, err)
			}
			if _, err := tx.ExecContext(ctx, `
				INSERT INTO extract_templates
					(tenant_id, id, name, description, system_prompt, user_prompt,
					 variables_schema, output_schema, default_model, enabled,
					 owner_actor_id, is_default)
				VALUES ($1::uuid, $2, $3, $4, $5, $6, $7, $8, $9, $10, NULL, $11)
				ON CONFLICT (tenant_id, id) DO UPDATE SET
					name             = excluded.name,
					description      = excluded.description,
					system_prompt    = excluded.system_prompt,
					user_prompt      = excluded.user_prompt,
					variables_schema = excluded.variables_schema,
					output_schema    = excluded.output_schema,
					default_model    = excluded.default_model,
					enabled          = excluded.enabled,
					is_default       = excluded.is_default`,
				tenantID, t.ID, t.Name, t.Description, t.SystemPrompt, t.UserPrompt,
				ctxSchema, outSchema, t.DefaultModel, t.Enabled, t.IsDefault); err != nil {
				return fmt.Errorf("insert default template %s: %w", t.ID, err)
			}
		}
		return nil
	})
}

func (r *ExtractRepo) GetSettings(ctx context.Context, tenantID string) (extract.Settings, error) {
	var s extract.Settings
	err := r.db.SQL.QueryRowContext(ctx, `
		SELECT id, default_model, max_file_size_mb, created_at
		FROM extract_settings
		WHERE tenant_id = $1::uuid AND id = 'default'`, tenantID).
		Scan(&s.ID, &s.DefaultModel, &s.MaxFileSizeMB, &s.CreatedAt)
	if err != nil {
		if errors.Is(err, sql.ErrNoRows) || isInvalidUUID(err) {
			return extract.Settings{}, extract.ErrNotFound
		}
		return extract.Settings{}, fmt.Errorf("get extract settings: %w", err)
	}
	return s, nil
}

func (r *ExtractRepo) UpsertSettings(ctx context.Context, tenantID string, s extract.Settings) (extract.Settings, error) {
	if s.ID == "" {
		s.ID = "default"
	}
	err := r.db.SQL.QueryRowContext(ctx, `
		INSERT INTO extract_settings
			(tenant_id, id, default_model, max_file_size_mb, owner_actor_id)
		VALUES ($1::uuid, 'default', $2, $3, NULL)
		ON CONFLICT (tenant_id, id) DO UPDATE SET
			default_model    = excluded.default_model,
			max_file_size_mb = excluded.max_file_size_mb
		RETURNING id, default_model, max_file_size_mb, created_at`,
		tenantID, s.DefaultModel, s.MaxFileSizeMB).
		Scan(&s.ID, &s.DefaultModel, &s.MaxFileSizeMB, &s.CreatedAt)
	if err != nil {
		return extract.Settings{}, fmt.Errorf("upsert extract settings: %w", err)
	}
	return s, nil
}

func scanExtractTemplate(row interface{ Scan(dest ...any) error }) (extract.Template, error) {
	var t extract.Template
	var owner sql.NullString
	var ctxRaw, outRaw []byte
	if err := row.Scan(
		&t.ID, &t.Name, &t.Description, &t.SystemPrompt, &t.UserPrompt,
		&ctxRaw, &outRaw, &t.DefaultModel, &t.Enabled, &owner,
		&t.IsDefault, &t.CreatedAt,
	); err != nil {
		return extract.Template{}, err
	}
	if owner.Valid {
		t.OwnerActorID = owner.String
	}
	t.ContextSchema = map[string]any{}
	t.OutputSchema = map[string]any{}
	if len(ctxRaw) > 0 {
		if err := json.Unmarshal(ctxRaw, &t.ContextSchema); err != nil {
			return extract.Template{}, fmt.Errorf("decode context schema: %w", err)
		}
	}
	if len(outRaw) > 0 {
		if err := json.Unmarshal(outRaw, &t.OutputSchema); err != nil {
			return extract.Template{}, fmt.Errorf("decode output schema: %w", err)
		}
	}
	return t, nil
}
