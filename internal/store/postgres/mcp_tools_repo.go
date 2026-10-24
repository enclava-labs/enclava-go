package postgres

import (
	"context"
	"crypto/sha256"
	"database/sql"
	"encoding/hex"
	"encoding/json"
	"errors"
	"fmt"
	"strings"

	"enclava-go/internal/mcp"
)

var ErrMCPApprovalNotFound = errors.New("postgres: MCP approval not found")

type MCPServer = mcp.Server
type MCPToolApproval = mcp.Approval

type MCPToolsRepo struct{ db *DB }

func NewMCPToolsRepo(db *DB) *MCPToolsRepo { return &MCPToolsRepo{db: db} }

func (r *MCPToolsRepo) PutServer(ctx context.Context, s MCPServer) error {
	if s.TenantID == "" || s.ID == "" || !validScopes(s.AudienceScopes) {
		return fmt.Errorf("postgres: invalid MCP server")
	}
	origin, err := mcp.ValidateOrigin(s.Origin, s.AuthorizedPrivate)
	if err != nil {
		return err
	}
	s.Origin = origin
	path, err := mcp.ValidateEndpointPath(s.EndpointPath)
	if err != nil {
		return err
	}
	s.EndpointPath = path
	b, _ := json.Marshal(s.AudienceScopes)
	res, err := r.db.SQL.ExecContext(ctx, `INSERT INTO mcp_servers (tenant_id,id,base_origin,endpoint_path,credential_ref,authorized_private,audience_scopes,enabled)
		VALUES ($1::uuid,$2,$3,$4,$5,$6,$7,$8) ON CONFLICT (tenant_id,id) DO UPDATE SET authorized_private=EXCLUDED.authorized_private, audience_scopes=EXCLUDED.audience_scopes, enabled=EXCLUDED.enabled, updated_at=now()
		WHERE mcp_servers.base_origin=EXCLUDED.base_origin AND mcp_servers.endpoint_path=EXCLUDED.endpoint_path AND mcp_servers.credential_ref=EXCLUDED.credential_ref`,
		s.TenantID, s.ID, s.Origin, s.EndpointPath, s.CredentialRef, s.AuthorizedPrivate, string(b), s.Enabled)
	if err != nil {
		return fmt.Errorf("put MCP server: %w", err)
	}
	n, err := res.RowsAffected()
	if err != nil || n == 0 {
		return fmt.Errorf("postgres: MCP server origin and credential binding are immutable")
	}
	return nil
}

func (r *MCPToolsRepo) GetServer(ctx context.Context, tenantID, id string) (MCPServer, error) {
	var s MCPServer
	var scopes []byte
	err := r.db.SQL.QueryRowContext(ctx, `SELECT tenant_id::text,id,base_origin,endpoint_path,credential_ref,authorized_private,audience_scopes,enabled,created_at,updated_at FROM mcp_servers WHERE tenant_id=$1::uuid AND id=$2`, tenantID, id).Scan(&s.TenantID, &s.ID, &s.Origin, &s.EndpointPath, &s.CredentialRef, &s.AuthorizedPrivate, &scopes, &s.Enabled, &s.CreatedAt, &s.UpdatedAt)
	if errors.Is(err, sql.ErrNoRows) || isInvalidUUID(err) {
		return MCPServer{}, ErrMCPApprovalNotFound
	}
	if err != nil {
		return MCPServer{}, fmt.Errorf("get MCP server: %w", err)
	}
	if err = json.Unmarshal(scopes, &s.AudienceScopes); err != nil {
		return MCPServer{}, err
	}
	return s, nil
}

func (r *MCPToolsRepo) Approve(ctx context.Context, a MCPToolApproval) error {
	if a.TenantID == "" || a.ServerID == "" || a.ToolID == "" || a.ModelName == "" || a.RemoteName == "" || a.SchemaVersion < 1 || !json.Valid(a.InputSchema) || !json.Valid(a.OutputSchema) || !validScopes(a.CallerScopes) {
		return fmt.Errorf("postgres: invalid MCP approval")
	}
	if _, err := mcp.CompileSchema(a.InputSchema); err != nil {
		return fmt.Errorf("postgres: invalid MCP input schema: %w", err)
	}
	if _, err := mcp.CompileSchema(a.OutputSchema); err != nil {
		return fmt.Errorf("postgres: invalid MCP output schema: %w", err)
	}
	if len(a.ModelName) > 64 || strings.TrimSpace(a.ModelName) != a.ModelName {
		return fmt.Errorf("postgres: invalid MCP model name")
	}
	for _, c := range a.ModelName {
		if !(c >= 'a' && c <= 'z' || c >= 'A' && c <= 'Z' || c >= '0' && c <= '9' || c == '_' || c == '-' || c == '.') {
			return fmt.Errorf("postgres: invalid MCP model name")
		}
	}
	ih, oh := schemaDigest(a.InputSchema), schemaDigest(a.OutputSchema)
	if a.InputSchemaHash != "" && a.InputSchemaHash != ih || a.OutputSchemaHash != "" && a.OutputSchemaHash != oh {
		return fmt.Errorf("postgres: MCP schema hash mismatch")
	}
	ctxTx, err := r.db.SQL.BeginTx(ctx, nil)
	if err != nil {
		return err
	}
	defer ctxTx.Rollback()
	var revision int64
	err = ctxTx.QueryRowContext(ctx, `SELECT revision FROM mcp_tool_approvals WHERE tenant_id=$1::uuid AND server_id=$2 AND tool_id=$3 FOR UPDATE`, a.TenantID, a.ServerID, a.ToolID).Scan(&revision)
	if err != nil && !errors.Is(err, sql.ErrNoRows) {
		return fmt.Errorf("read MCP approval revision: %w", err)
	}
	a.Revision = revision + 1
	a.InputSchemaHash = ih
	a.OutputSchemaHash = oh
	scopes, _ := json.Marshal(a.CallerScopes)
	_, err = ctxTx.ExecContext(ctx, `INSERT INTO mcp_tool_approvals (tenant_id,server_id,tool_id,model_name,remote_name,input_schema,output_schema,input_schema_hash,output_schema_hash,schema_version,revision,caller_scopes,enabled)
	VALUES($1::uuid,$2,$3,$4,$5,$6,$7,$8,$9,$10,$11,$12,$13) ON CONFLICT(tenant_id,server_id,tool_id) DO UPDATE SET model_name=EXCLUDED.model_name,remote_name=EXCLUDED.remote_name,input_schema=EXCLUDED.input_schema,output_schema=EXCLUDED.output_schema,input_schema_hash=EXCLUDED.input_schema_hash,output_schema_hash=EXCLUDED.output_schema_hash,schema_version=EXCLUDED.schema_version,revision=EXCLUDED.revision,caller_scopes=EXCLUDED.caller_scopes,enabled=EXCLUDED.enabled,updated_at=now()`, a.TenantID, a.ServerID, a.ToolID, a.ModelName, a.RemoteName, string(a.InputSchema), string(a.OutputSchema), ih, oh, a.SchemaVersion, a.Revision, string(scopes), a.Enabled)
	if err != nil {
		return fmt.Errorf("save MCP approval: %w", err)
	}
	_, err = ctxTx.ExecContext(ctx, `INSERT INTO mcp_tool_approval_history(tenant_id,server_id,tool_id,revision,remote_name,input_schema,output_schema,input_schema_hash,output_schema_hash,schema_version,caller_scopes,enabled) VALUES($1::uuid,$2,$3,$4,$5,$6,$7,$8,$9,$10,$11,$12)`, a.TenantID, a.ServerID, a.ToolID, a.Revision, a.RemoteName, string(a.InputSchema), string(a.OutputSchema), ih, oh, a.SchemaVersion, string(scopes), a.Enabled)
	if err != nil {
		return fmt.Errorf("record MCP approval history: %w", err)
	}
	return ctxTx.Commit()
}

func (r *MCPToolsRepo) GetApproval(ctx context.Context, tenantID, serverID, toolID string) (MCPToolApproval, error) {
	return r.getApproval(ctx, tenantID, serverID, toolID, true)
}
func (r *MCPToolsRepo) getApproval(ctx context.Context, tenantID, serverID, toolID string, enabled bool) (MCPToolApproval, error) {
	var a MCPToolApproval
	var scopes []byte
	q := `SELECT tenant_id::text,server_id,tool_id,model_name,remote_name,input_schema,output_schema,input_schema_hash,output_schema_hash,schema_version,revision,caller_scopes,enabled,created_at,updated_at FROM mcp_tool_approvals WHERE tenant_id=$1::uuid AND server_id=$2 AND tool_id=$3`
	if enabled {
		q += ` AND enabled AND EXISTS(SELECT 1 FROM mcp_servers s WHERE s.tenant_id=mcp_tool_approvals.tenant_id AND s.id=mcp_tool_approvals.server_id AND s.enabled)`
	}
	err := r.db.SQL.QueryRowContext(ctx, q, tenantID, serverID, toolID).Scan(&a.TenantID, &a.ServerID, &a.ToolID, &a.ModelName, &a.RemoteName, &a.InputSchema, &a.OutputSchema, &a.InputSchemaHash, &a.OutputSchemaHash, &a.SchemaVersion, &a.Revision, &scopes, &a.Enabled, &a.CreatedAt, &a.UpdatedAt)
	if errors.Is(err, sql.ErrNoRows) || isInvalidUUID(err) {
		return MCPToolApproval{}, ErrMCPApprovalNotFound
	}
	if err != nil {
		return MCPToolApproval{}, fmt.Errorf("get MCP approval: %w", err)
	}
	if err = json.Unmarshal(scopes, &a.CallerScopes); err != nil {
		return MCPToolApproval{}, err
	}
	return a, nil
}

func (r *MCPToolsRepo) ListApprovals(ctx context.Context, tenantID, serverID string) ([]MCPToolApproval, error) {
	rows, err := r.db.SQL.QueryContext(ctx, `SELECT tool_id FROM mcp_tool_approvals WHERE tenant_id=$1::uuid AND server_id=$2 AND enabled ORDER BY tool_id`, tenantID, serverID)
	if err != nil {
		return nil, err
	}
	defer rows.Close()
	out := []MCPToolApproval{}
	for rows.Next() {
		var id string
		if err = rows.Scan(&id); err != nil {
			return nil, err
		}
		a, e := r.GetApproval(ctx, tenantID, serverID, id)
		if e != nil {
			return nil, e
		}
		out = append(out, a)
	}
	return out, rows.Err()
}

func (r *MCPToolsRepo) ListServers(ctx context.Context, tenantID string) ([]MCPServer, error) {
	rows, err := r.db.SQL.QueryContext(ctx, `SELECT id FROM mcp_servers WHERE tenant_id=$1::uuid ORDER BY id`, tenantID)
	if err != nil {
		return nil, err
	}
	defer rows.Close()
	var out []MCPServer
	for rows.Next() {
		var id string
		if err := rows.Scan(&id); err != nil {
			return nil, err
		}
		server, err := r.GetServer(ctx, tenantID, id)
		if err != nil {
			return nil, err
		}
		out = append(out, server)
	}
	return out, rows.Err()
}

func (r *MCPToolsRepo) ListAllApprovals(ctx context.Context, tenantID, serverID string) ([]MCPToolApproval, error) {
	rows, err := r.db.SQL.QueryContext(ctx, `SELECT tool_id FROM mcp_tool_approvals WHERE tenant_id=$1::uuid AND server_id=$2 ORDER BY tool_id`, tenantID, serverID)
	if err != nil {
		return nil, err
	}
	defer rows.Close()
	var out []MCPToolApproval
	for rows.Next() {
		var id string
		if err := rows.Scan(&id); err != nil {
			return nil, err
		}
		a, err := r.getApproval(ctx, tenantID, serverID, id, false)
		if err != nil {
			return nil, err
		}
		out = append(out, a)
	}
	return out, rows.Err()
}

func (r *MCPToolsRepo) DeleteServer(ctx context.Context, tenantID, id string) error {
	res, err := r.db.SQL.ExecContext(ctx, `DELETE FROM mcp_servers WHERE tenant_id=$1::uuid AND id=$2`, tenantID, id)
	if err != nil {
		return err
	}
	n, err := res.RowsAffected()
	if err != nil {
		return err
	}
	if n == 0 {
		return ErrMCPApprovalNotFound
	}
	return nil
}

func (r *MCPToolsRepo) SetServerEnabled(ctx context.Context, tenantID, id string, enabled bool) error {
	res, err := r.db.SQL.ExecContext(ctx, `UPDATE mcp_servers SET enabled=$3,updated_at=now() WHERE tenant_id=$1::uuid AND id=$2`, tenantID, id, enabled)
	if err != nil {
		return err
	}
	n, err := res.RowsAffected()
	if err != nil {
		return err
	}
	if n == 0 {
		return ErrMCPApprovalNotFound
	}
	return nil
}

func (r *MCPToolsRepo) GetApprovalAny(ctx context.Context, tenantID, serverID, toolID string) (MCPToolApproval, error) {
	return r.getApproval(ctx, tenantID, serverID, toolID, false)
}
func (r *MCPToolsRepo) Revoke(ctx context.Context, tenantID, serverID, toolID string) error {
	tx, err := r.db.SQL.BeginTx(ctx, nil)
	if err != nil {
		return err
	}
	defer tx.Rollback()
	var a MCPToolApproval
	var scopes []byte
	err = tx.QueryRowContext(ctx, `UPDATE mcp_tool_approvals SET enabled=FALSE,revision=revision+1,updated_at=now() WHERE tenant_id=$1::uuid AND server_id=$2 AND tool_id=$3 AND enabled RETURNING tenant_id::text,server_id,tool_id,remote_name,input_schema,output_schema,input_schema_hash,output_schema_hash,schema_version,revision,caller_scopes,enabled`, tenantID, serverID, toolID).Scan(
		&a.TenantID, &a.ServerID, &a.ToolID, &a.RemoteName, &a.InputSchema, &a.OutputSchema, &a.InputSchemaHash, &a.OutputSchemaHash, &a.SchemaVersion, &a.Revision, &scopes, &a.Enabled)
	if errors.Is(err, sql.ErrNoRows) {
		return ErrMCPApprovalNotFound
	}
	if err != nil {
		return err
	}
	if err := json.Unmarshal(scopes, &a.CallerScopes); err != nil {
		return err
	}
	if _, err := tx.ExecContext(ctx, `INSERT INTO mcp_tool_approval_history(tenant_id,server_id,tool_id,revision,remote_name,input_schema,output_schema,input_schema_hash,output_schema_hash,schema_version,caller_scopes,enabled) VALUES($1::uuid,$2,$3,$4,$5,$6,$7,$8,$9,$10,$11,$12)`,
		a.TenantID, a.ServerID, a.ToolID, a.Revision, a.RemoteName, string(a.InputSchema), string(a.OutputSchema), a.InputSchemaHash, a.OutputSchemaHash, a.SchemaVersion, string(scopes), a.Enabled); err != nil {
		return err
	}
	return tx.Commit()
}
func schemaDigest(b []byte) string { sum := sha256.Sum256(b); return hex.EncodeToString(sum[:]) }
func validScopes(v []string) bool {
	if len(v) == 0 {
		return false
	}
	seen := make(map[string]struct{}, len(v))
	for _, scope := range v {
		scope = strings.TrimSpace(scope)
		if scope == "" {
			return false
		}
		if _, exists := seen[scope]; exists {
			return false
		}
		seen[scope] = struct{}{}
	}
	return true
}
