// identity_dashboard.go — the shell's one tenant-display-name read (07-03).
// New file on purpose: identity_repo.go is phase-8-owned and must not be
// touched by dashboard work.
package postgres

import (
	"context"
	"database/sql"
	"errors"
	"fmt"

	"enclava-go/internal/auth"
)

// GetTenantDisplayName returns tenants.display_name for the id. An absent row
// is auth.ErrNotFound so callers fall back to the tenant slug instead of
// failing the page.
func (r *IdentityRepo) GetTenantDisplayName(ctx context.Context, tenantID string) (string, error) {
	var name string
	err := r.db.QueryRowContext(ctx,
		`SELECT display_name FROM tenants WHERE id = $1::uuid`, tenantID).Scan(&name)
	if err != nil {
		if errors.Is(err, sql.ErrNoRows) || isInvalidUUID(err) {
			return "", auth.ErrNotFound
		}
		return "", fmt.Errorf("get tenant display name: %w", err)
	}
	return name, nil
}
