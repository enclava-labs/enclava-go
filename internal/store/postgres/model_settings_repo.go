package postgres

import (
	"context"
	"database/sql"
	"errors"
	"fmt"

	"enclava-go/internal/inference"
)

// ModelSettingsRepo persists the tenant's per-model availability, default
// and fallback (migration 051).
type ModelSettingsRepo struct {
	db *sql.DB
}

// NewModelSettingsRepo wraps the shared pool.
func NewModelSettingsRepo(db *DB) *ModelSettingsRepo {
	return &ModelSettingsRepo{db: db.SQL}
}

var _ inference.ModelSettingsRepository = (*ModelSettingsRepo)(nil)

// ListModelSettings returns the tenant's stored settings ordered by model.
func (r *ModelSettingsRepo) ListModelSettings(ctx context.Context, tenantID string) ([]inference.ModelSetting, error) {
	rows, err := r.db.QueryContext(ctx, `
		SELECT model, available, is_default, COALESCE(fallback_model, '')
		FROM model_settings WHERE tenant_id = $1::uuid ORDER BY model`, tenantID)
	if err != nil {
		return nil, fmt.Errorf("list model settings: %w", err)
	}
	defer func() { _ = rows.Close() }()
	var out []inference.ModelSetting
	for rows.Next() {
		var s inference.ModelSetting
		if err := rows.Scan(&s.Model, &s.Available, &s.Default, &s.FallbackModel); err != nil {
			return nil, fmt.Errorf("scan model setting: %w", err)
		}
		out = append(out, s)
	}
	return out, rows.Err()
}

// GetModelSetting returns one stored setting; ok is false when none exists.
func (r *ModelSettingsRepo) GetModelSetting(ctx context.Context, tenantID, model string) (inference.ModelSetting, bool, error) {
	s := inference.ModelSetting{Model: model}
	err := r.db.QueryRowContext(ctx, `
		SELECT available, is_default, COALESCE(fallback_model, '')
		FROM model_settings WHERE tenant_id = $1::uuid AND model = $2`, tenantID, model).
		Scan(&s.Available, &s.Default, &s.FallbackModel)
	if errors.Is(err, sql.ErrNoRows) {
		return inference.ModelSetting{}, false, nil
	}
	if err != nil {
		return inference.ModelSetting{}, false, fmt.Errorf("get model setting: %w", err)
	}
	return s, true, nil
}

// PutModelSetting upserts one setting; a new default clears the previous one
// in the same transaction.
func (r *ModelSettingsRepo) PutModelSetting(ctx context.Context, tenantID string, s inference.ModelSetting) error {
	tx, err := r.db.BeginTx(ctx, nil)
	if err != nil {
		return fmt.Errorf("begin model setting: %w", err)
	}
	defer func() { _ = tx.Rollback() }()
	if s.Default {
		if _, err := tx.ExecContext(ctx, `
			UPDATE model_settings SET is_default = false, updated_at = now()
			WHERE tenant_id = $1::uuid AND model <> $2 AND is_default`, tenantID, s.Model); err != nil {
			return fmt.Errorf("clear default model: %w", err)
		}
	}
	if _, err := tx.ExecContext(ctx, `
		INSERT INTO model_settings (tenant_id, model, available, is_default, fallback_model)
		VALUES ($1::uuid, $2, $3, $4, NULLIF($5, ''))
		ON CONFLICT (tenant_id, model) DO UPDATE SET
			available = EXCLUDED.available, is_default = EXCLUDED.is_default,
			fallback_model = EXCLUDED.fallback_model, updated_at = now()`,
		tenantID, s.Model, s.Available, s.Default, s.FallbackModel); err != nil {
		return fmt.Errorf("put model setting: %w", err)
	}
	if err := tx.Commit(); err != nil {
		return fmt.Errorf("commit model setting: %w", err)
	}
	return nil
}
