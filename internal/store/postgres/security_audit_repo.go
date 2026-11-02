package postgres

import (
	"context"
	"encoding/json"
	"fmt"
	"time"
)

type SecurityAuditRow struct {
	TenantID     string         `json:"tenant_id"`
	ID           string         `json:"id"`
	ActorID      string         `json:"actor_id"`
	ActorKind    string         `json:"actor_kind"`
	CredentialID string         `json:"credential_id"`
	Action       string         `json:"action"`
	TargetKind   string         `json:"target_kind"`
	TargetID     string         `json:"target_id"`
	Outcome      string         `json:"outcome"`
	ClientIP     string         `json:"client_ip"`
	RequestID    string         `json:"request_id"`
	OccurredAt   time.Time      `json:"occurred_at"`
	Details      map[string]any `json:"details"`
}

type SecurityAuditRepo struct {
	db *DB
}

func NewSecurityAuditRepo(db *DB) *SecurityAuditRepo {
	return &SecurityAuditRepo{db: db}
}

func (r *SecurityAuditRepo) Insert(ctx context.Context, row SecurityAuditRow) error {
	if row.TenantID == "" || row.ID == "" || row.Action == "" || row.OccurredAt.IsZero() {
		return fmt.Errorf("insert security audit: missing required fields")
	}
	details := row.Details
	if details == nil {
		details = map[string]any{}
	}
	encoded, err := json.Marshal(details)
	if err != nil {
		return fmt.Errorf("encode security audit details: %w", err)
	}
	_, err = r.db.SQL.ExecContext(ctx, `
		INSERT INTO security_audit
		    (tenant_id, id, actor_id, actor_kind, credential_id, action,
		     target_kind, target_id, outcome, client_ip, request_id, occurred_at, details)
		VALUES ($1,$2,$3,$4,$5,$6,$7,$8,$9,$10,$11,$12,$13)`,
		row.TenantID, row.ID, row.ActorID, row.ActorKind, row.CredentialID, row.Action,
		row.TargetKind, row.TargetID, row.Outcome, row.ClientIP, row.RequestID, row.OccurredAt, encoded)
	if err != nil {
		return fmt.Errorf("insert security audit: %w", err)
	}
	return nil
}

// List returns records before the tenant-local cursor, newest first.
func (r *SecurityAuditRepo) List(ctx context.Context, tenant string, limit int, before string) ([]SecurityAuditRow, error) {
	if tenant == "" || limit <= 0 {
		return nil, fmt.Errorf("list security audit: tenant and positive limit required")
	}
	rows, err := r.db.SQL.QueryContext(ctx, `
		SELECT tenant_id, id, actor_id, actor_kind, credential_id, action,
		       target_kind, target_id, outcome, client_ip, request_id, occurred_at, details
		FROM security_audit
		WHERE tenant_id = $1 AND ($3 = '' OR (occurred_at, id) <
		    (SELECT occurred_at, id FROM security_audit WHERE tenant_id = $1 AND id = $3))
		ORDER BY occurred_at DESC, id DESC
		LIMIT $2`, tenant, limit, before)
	if err != nil {
		return nil, fmt.Errorf("list security audit: %w", err)
	}
	defer func() { _ = rows.Close() }()
	result := make([]SecurityAuditRow, 0)
	for rows.Next() {
		var row SecurityAuditRow
		var details []byte
		if err := rows.Scan(&row.TenantID, &row.ID, &row.ActorID, &row.ActorKind,
			&row.CredentialID, &row.Action, &row.TargetKind, &row.TargetID, &row.Outcome,
			&row.ClientIP, &row.RequestID, &row.OccurredAt, &details); err != nil {
			return nil, fmt.Errorf("scan security audit: %w", err)
		}
		if err := json.Unmarshal(details, &row.Details); err != nil {
			return nil, fmt.Errorf("decode security audit details: %w", err)
		}
		result = append(result, row)
	}
	if err := rows.Err(); err != nil {
		return nil, fmt.Errorf("list security audit: %w", err)
	}
	return result, nil
}
