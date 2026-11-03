package postgres

import (
	"context"
	"database/sql"
	"encoding/json"
	"errors"
	"fmt"
	"strings"
	"time"

	"enclava-go/internal/mcp"
)

var ErrMCPApprovalNotFound = errors.New("postgres: MCP approval not found")

// ErrMCPSchemaChanged reports that an approval was attempted against a stale
// reviewed schema version: the durable discovery row has moved on (HTTP 409).
var ErrMCPSchemaChanged = errors.New("postgres: MCP discovery schema changed since review")

type MCPServer = mcp.Server
type MCPToolApproval = mcp.Approval

// DiscoveredToolSnapshot is one discovered tool as captured by the transport.
type DiscoveredToolSnapshot = mcp.DiscoveredTool

// dbtx is the common transaction surface of *sql.DB and *sql.Tx, so the
// approval/revoke transitions run unchanged inside any transaction.
type dbtx interface {
	ExecContext(context.Context, string, ...any) (sql.Result, error)
	QueryRowContext(context.Context, string, ...any) *sql.Row
}

// MCPToolDiscovery is one durable discovery queue row: the frozen read model
// behind §6.9 (pending/changed/rejected/gone with field-level diffs) and the
// OA-19 attention aggregator's source for schema-changed tools.
type MCPToolDiscovery struct {
	TenantID, ServerID, ToolID    string
	Title, Description, ModelName string
	RemoteName, State             string
	SchemaVersion                 int64
	ApprovedSchemaVersion         sql.NullInt64
	InputSchema, OutputSchema     json.RawMessage
	PriorInputSchema, DiffFields  json.RawMessage
	Annotations                   mcp.ToolAnnotations
	FirstSeenAt, LastSeenAt       time.Time
}

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
	var discoveryAt sql.NullTime
	err := r.db.SQL.QueryRowContext(ctx, `SELECT tenant_id::text,id,base_origin,endpoint_path,credential_ref,authorized_private,audience_scopes,enabled,last_discovery_at,last_discovery_error,created_at,updated_at FROM mcp_servers WHERE tenant_id=$1::uuid AND id=$2`, tenantID, id).Scan(&s.TenantID, &s.ID, &s.Origin, &s.EndpointPath, &s.CredentialRef, &s.AuthorizedPrivate, &scopes, &s.Enabled, &discoveryAt, &s.DiscoveryError, &s.CreatedAt, &s.UpdatedAt)
	if errors.Is(err, sql.ErrNoRows) || isInvalidUUID(err) {
		return MCPServer{}, ErrMCPApprovalNotFound
	}
	if err != nil {
		return MCPServer{}, fmt.Errorf("get MCP server: %w", err)
	}
	if err = json.Unmarshal(scopes, &s.AudienceScopes); err != nil {
		return MCPServer{}, err
	}
	s.DiscoveryAt = discoveryAt.Time
	return s, nil
}

func (r *MCPToolsRepo) Approve(ctx context.Context, a MCPToolApproval) error {
	tx, err := r.db.SQL.BeginTx(ctx, nil)
	if err != nil {
		return err
	}
	defer tx.Rollback()
	if _, err := approveTx(ctx, tx, a); err != nil {
		return err
	}
	return tx.Commit()
}

// approveTx validates and installs one approval (upsert + history) inside the
// caller's transaction, so a version compare and the approval install cannot
// be split by a concurrent discovery.
func approveTx(ctx context.Context, q dbtx, a MCPToolApproval) (MCPToolApproval, error) {
	if a.TenantID == "" || a.ServerID == "" || a.ToolID == "" || a.ModelName == "" || a.RemoteName == "" || a.SchemaVersion < 1 || !json.Valid(a.InputSchema) || !json.Valid(a.OutputSchema) || !validScopes(a.CallerScopes) {
		return a, fmt.Errorf("postgres: invalid MCP approval")
	}
	if _, err := mcp.CompileSchema(a.InputSchema); err != nil {
		return a, fmt.Errorf("postgres: invalid MCP input schema: %w", err)
	}
	if _, err := mcp.CompileSchema(a.OutputSchema); err != nil {
		return a, fmt.Errorf("postgres: invalid MCP output schema: %w", err)
	}
	if len(a.ModelName) > 64 || strings.TrimSpace(a.ModelName) != a.ModelName {
		return a, fmt.Errorf("postgres: invalid MCP model name")
	}
	for _, c := range a.ModelName {
		if !(c >= 'a' && c <= 'z' || c >= 'A' && c <= 'Z' || c >= '0' && c <= '9' || c == '_' || c == '-' || c == '.') {
			return a, fmt.Errorf("postgres: invalid MCP model name")
		}
	}
	ih, oh := mcp.SchemaDigest(a.InputSchema), mcp.SchemaDigest(a.OutputSchema)
	if a.InputSchemaHash != "" && a.InputSchemaHash != ih || a.OutputSchemaHash != "" && a.OutputSchemaHash != oh {
		return a, fmt.Errorf("postgres: MCP schema hash mismatch")
	}
	var revision int64
	err := q.QueryRowContext(ctx, `SELECT revision FROM mcp_tool_approvals WHERE tenant_id=$1::uuid AND server_id=$2 AND tool_id=$3 FOR UPDATE`, a.TenantID, a.ServerID, a.ToolID).Scan(&revision)
	if err != nil && !errors.Is(err, sql.ErrNoRows) {
		return a, fmt.Errorf("read MCP approval revision: %w", err)
	}
	a.Revision = revision + 1
	a.InputSchemaHash = ih
	a.OutputSchemaHash = oh
	scopes, _ := json.Marshal(a.CallerScopes)
	_, err = q.ExecContext(ctx, `INSERT INTO mcp_tool_approvals (tenant_id,server_id,tool_id,model_name,remote_name,input_schema,output_schema,input_schema_hash,output_schema_hash,schema_version,revision,caller_scopes,enabled)
VALUES($1::uuid,$2,$3,$4,$5,$6,$7,$8,$9,$10,$11,$12,$13) ON CONFLICT(tenant_id,server_id,tool_id) DO UPDATE SET model_name=EXCLUDED.model_name,remote_name=EXCLUDED.remote_name,input_schema=EXCLUDED.input_schema,output_schema=EXCLUDED.output_schema,input_schema_hash=EXCLUDED.input_schema_hash,output_schema_hash=EXCLUDED.output_schema_hash,schema_version=EXCLUDED.schema_version,revision=EXCLUDED.revision,caller_scopes=EXCLUDED.caller_scopes,enabled=EXCLUDED.enabled,updated_at=now()`, a.TenantID, a.ServerID, a.ToolID, a.ModelName, a.RemoteName, string(a.InputSchema), string(a.OutputSchema), ih, oh, a.SchemaVersion, a.Revision, string(scopes), a.Enabled)
	if err != nil {
		return a, fmt.Errorf("save MCP approval: %w", err)
	}
	_, err = q.ExecContext(ctx, `INSERT INTO mcp_tool_approval_history(tenant_id,server_id,tool_id,revision,remote_name,input_schema,output_schema,input_schema_hash,output_schema_hash,schema_version,caller_scopes,enabled) VALUES($1::uuid,$2,$3,$4,$5,$6,$7,$8,$9,$10,$11,$12)`, a.TenantID, a.ServerID, a.ToolID, a.Revision, a.RemoteName, string(a.InputSchema), string(a.OutputSchema), ih, oh, a.SchemaVersion, string(scopes), a.Enabled)
	if err != nil {
		return a, fmt.Errorf("record MCP approval history: %w", err)
	}
	return a, nil
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
	revoked, err := revokeTx(ctx, tx, tenantID, serverID, toolID)
	if err != nil {
		return err
	}
	if !revoked {
		return ErrMCPApprovalNotFound
	}
	return tx.Commit()
}

// revokeTx disables an enabled approval and appends its history row inside
// the caller's transaction. It reports whether an enabled approval existed.
func revokeTx(ctx context.Context, q dbtx, tenantID, serverID, toolID string) (bool, error) {
	var a MCPToolApproval
	var scopes []byte
	err := q.QueryRowContext(ctx, `UPDATE mcp_tool_approvals SET enabled=FALSE,revision=revision+1,updated_at=now() WHERE tenant_id=$1::uuid AND server_id=$2 AND tool_id=$3 AND enabled RETURNING tenant_id::text,server_id,tool_id,remote_name,input_schema,output_schema,input_schema_hash,output_schema_hash,schema_version,revision,caller_scopes,enabled`, tenantID, serverID, toolID).Scan(
		&a.TenantID, &a.ServerID, &a.ToolID, &a.RemoteName, &a.InputSchema, &a.OutputSchema, &a.InputSchemaHash, &a.OutputSchemaHash, &a.SchemaVersion, &a.Revision, &scopes, &a.Enabled)
	if errors.Is(err, sql.ErrNoRows) {
		return false, nil
	}
	if err != nil {
		return false, err
	}
	if err := json.Unmarshal(scopes, &a.CallerScopes); err != nil {
		return false, err
	}
	if _, err := q.ExecContext(ctx, `INSERT INTO mcp_tool_approval_history(tenant_id,server_id,tool_id,revision,remote_name,input_schema,output_schema,input_schema_hash,output_schema_hash,schema_version,caller_scopes,enabled) VALUES($1::uuid,$2,$3,$4,$5,$6,$7,$8,$9,$10,$11,$12)`,
		a.TenantID, a.ServerID, a.ToolID, a.Revision, a.RemoteName, string(a.InputSchema), string(a.OutputSchema), a.InputSchemaHash, a.OutputSchemaHash, a.SchemaVersion, string(scopes), a.Enabled); err != nil {
		return false, err
	}
	return true, nil
}
func nullJSON(b json.RawMessage) any {
	if len(b) == 0 {
		return nil
	}
	return string(b)
}

// RecordDiscovery persists one discovery snapshot for an authenticated
// server. Tenant and server identity are explicit arguments — never derived
// from the snapshot — so a legitimately empty tools array still revokes
// exactly this server's vanished approvals. One transaction locks the
// server's discovery rows and approvals, so a concurrent ApproveDiscovery
// either lands entirely before or entirely after this snapshot.
func (r *MCPToolsRepo) RecordDiscovery(ctx context.Context, tenantID, serverID string, tools []DiscoveredToolSnapshot) error {
	if tenantID == "" || serverID == "" {
		return fmt.Errorf("postgres: invalid MCP discovery identity")
	}
	type snapshotTool struct {
		tool                DiscoveredToolSnapshot
		inDigest, outDigest string
	}
	snapshots := make([]snapshotTool, 0, len(tools))
	for _, tool := range tools {
		if tool.Name == "" || !json.Valid(tool.InputSchema) || !json.Valid(tool.OutputSchema) {
			return fmt.Errorf("postgres: invalid MCP discovery snapshot")
		}
		snapshots = append(snapshots, snapshotTool{tool: tool, inDigest: mcp.SchemaDigest(tool.InputSchema), outDigest: mcp.SchemaDigest(tool.OutputSchema)})
	}
	tx, err := r.db.SQL.BeginTx(ctx, nil)
	if err != nil {
		return err
	}
	defer tx.Rollback()
	type knownRow struct {
		inDigest, outDigest, state string
		schemaVersion              int64
		approvedVersion            sql.NullInt64
		inputSchema, outputSchema  json.RawMessage
	}
	existing := make(map[string]knownRow)
	rows, err := tx.QueryContext(ctx, `SELECT tool_id, input_digest, output_digest, state, schema_version, approved_schema_version, input_schema, output_schema FROM mcp_tool_discoveries WHERE tenant_id=$1::uuid AND server_id=$2 FOR UPDATE`, tenantID, serverID)
	if err != nil {
		return fmt.Errorf("lock MCP discoveries: %w", err)
	}
	for rows.Next() {
		var id string
		var row knownRow
		if err = rows.Scan(&id, &row.inDigest, &row.outDigest, &row.state, &row.schemaVersion, &row.approvedVersion, &row.inputSchema, &row.outputSchema); err != nil {
			rows.Close()
			return err
		}
		existing[id] = row
	}
	if err = rows.Err(); err != nil {
		rows.Close()
		return err
	}
	rows.Close()

	enabled := make(map[string]bool)
	approvalVersions := make(map[string]int64)
	approvalSchemas := make(map[string][2]json.RawMessage)
	approvalRows, err := tx.QueryContext(ctx, `SELECT tool_id, enabled, schema_version, input_schema, output_schema FROM mcp_tool_approvals WHERE tenant_id=$1::uuid AND server_id=$2 FOR UPDATE`, tenantID, serverID)
	if err != nil {
		return fmt.Errorf("lock MCP approvals: %w", err)
	}
	for approvalRows.Next() {
		var id string
		var isEnabled bool
		var version int64
		var inSchema, outSchema json.RawMessage
		if err = approvalRows.Scan(&id, &isEnabled, &version, &inSchema, &outSchema); err != nil {
			approvalRows.Close()
			return err
		}
		enabled[id] = isEnabled
		approvalVersions[id] = version
		approvalSchemas[id] = [2]json.RawMessage{inSchema, outSchema}
	}
	if err = approvalRows.Err(); err != nil {
		approvalRows.Close()
		return err
	}
	approvalRows.Close()

	insertDiscovery := func(tool DiscoveredToolSnapshot, annotations []byte, state string, version int64, approvedVersion sql.NullInt64, priorIn, priorOut, diff json.RawMessage) error {
		_, err := tx.ExecContext(ctx, `INSERT INTO mcp_tool_discoveries (tenant_id,server_id,tool_id,title,description,annotations,remote_name,input_schema,output_schema,input_digest,output_digest,schema_version,state,approved_schema_version,prior_input_schema,prior_output_schema,diff_fields)
VALUES($1::uuid,$2,$3,$4,$5,$6,$7,$8,$9,$10,$11,$12,$13,$14,$15,$16,$17)`,
			tenantID, serverID, tool.Name, tool.Title, tool.Description, string(annotations), tool.Name, string(tool.InputSchema), string(tool.OutputSchema), mcp.SchemaDigest(tool.InputSchema), mcp.SchemaDigest(tool.OutputSchema), version, state, approvedVersion, nullJSON(priorIn), nullJSON(priorOut), string(diff))
		if err != nil {
			return fmt.Errorf("record MCP discovery: %w", err)
		}
		return nil
	}
	updateDiscovery := func(tool DiscoveredToolSnapshot, annotations []byte, version int64, state string, priorIn, priorOut, diff json.RawMessage) error {
		_, err := tx.ExecContext(ctx, `UPDATE mcp_tool_discoveries SET title=$4, description=$5, annotations=$6, remote_name=$7, input_schema=$8, output_schema=$9, input_digest=$10, output_digest=$11, schema_version=$12, state=$13, prior_input_schema=$14, prior_output_schema=$15, diff_fields=$16, last_seen_at=now()
WHERE tenant_id=$1::uuid AND server_id=$2 AND tool_id=$3`,
			tenantID, serverID, tool.Name, tool.Title, tool.Description, string(annotations), tool.Name, string(tool.InputSchema), string(tool.OutputSchema), mcp.SchemaDigest(tool.InputSchema), mcp.SchemaDigest(tool.OutputSchema), version, state, nullJSON(priorIn), nullJSON(priorOut), string(diff))
		if err != nil {
			return fmt.Errorf("update MCP discovery: %w", err)
		}
		return nil
	}

	snapshotSeen := make(map[string]struct{}, len(snapshots))
	for _, snap := range snapshots {
		tool := snap.tool
		annotations, _ := json.Marshal(tool.Annotations)
		prev, hasRow := existing[tool.Name]
		delete(existing, tool.Name)
		snapshotSeen[tool.Name] = struct{}{}
		legacy := false
		if !hasRow {
			if schemas, hasApproval := approvalSchemas[tool.Name]; hasApproval {
				// Approval installed before durable discovery: adopt it as the
				// baseline so a live approval is never silently downgraded.
				legacy = true
				prev = knownRow{
					inDigest: mcp.SchemaDigest(schemas[0]), outDigest: mcp.SchemaDigest(schemas[1]),
					state: "approved", schemaVersion: approvalVersions[tool.Name],
					approvedVersion: sql.NullInt64{Int64: approvalVersions[tool.Name], Valid: true},
					inputSchema:     schemas[0], outputSchema: schemas[1],
				}
			} else {
				if err := insertDiscovery(tool, annotations, "pending", 1, sql.NullInt64{}, nil, nil, json.RawMessage("[]")); err != nil {
					return err
				}
				continue
			}
		}
		if prev.inDigest == snap.inDigest && prev.outDigest == snap.outDigest {
			// Nothing changed: identical rediscovery keeps state and version.
			if legacy {
				if err := insertDiscovery(tool, annotations, "approved", prev.schemaVersion, prev.approvedVersion, nil, nil, json.RawMessage("[]")); err != nil {
					return err
				}
				continue
			}
			_, err := tx.ExecContext(ctx, `UPDATE mcp_tool_discoveries SET title=$4, description=$5, annotations=$6, remote_name=$7, last_seen_at=now() WHERE tenant_id=$1::uuid AND server_id=$2 AND tool_id=$3`,
				tenantID, serverID, tool.Name, tool.Title, tool.Description, string(annotations), tool.Name)
			if err != nil {
				return fmt.Errorf("refresh MCP discovery: %w", err)
			}
			continue
		}
		state, priorIn, priorOut, diff := "pending", json.RawMessage(nil), json.RawMessage(nil), json.RawMessage("[]")
		switch {
		case prev.approvedVersion.Valid:
			// An approved (or previously approved) schema moved on: keep the
			// approved baseline for the diff view and revoke the stale approval.
			state = "changed"
			priorIn, priorOut = prev.inputSchema, prev.outputSchema
			if d, err := json.Marshal(mcp.DiffSchemaFields(prev.inputSchema, tool.InputSchema)); err == nil {
				diff = d
			}
		case prev.state == "rejected":
			// A durable rejection survives rediscovery; the version still
			// moves so a later approval reviews the new schema.
			state = "rejected"
		}
		if enabled[tool.Name] {
			if _, err := revokeTx(ctx, tx, tenantID, serverID, tool.Name); err != nil {
				return err
			}
		}
		if legacy {
			if err := insertDiscovery(tool, annotations, state, prev.schemaVersion+1, prev.approvedVersion, priorIn, priorOut, diff); err != nil {
				return err
			}
			continue
		}
		if err := updateDiscovery(tool, annotations, prev.schemaVersion+1, state, priorIn, priorOut, diff); err != nil {
			return err
		}
	}
	// Legacy approvals whose tool vanished with no discovery row yet.
	for toolID := range approvalSchemas {
		if _, seen := snapshotSeen[toolID]; seen {
			continue
		}
		if _, hasRow := existing[toolID]; hasRow {
			continue
		}
		if _, err := revokeTx(ctx, tx, tenantID, serverID, toolID); err != nil {
			return err
		}
	}
	// Tools absent from THIS snapshot are gone for exactly this server.
	for toolID, prev := range existing {
		if _, err := revokeTx(ctx, tx, tenantID, serverID, toolID); err != nil {
			return err
		}
		priorIn, priorOut := json.RawMessage(nil), json.RawMessage(nil)
		if prev.approvedVersion.Valid {
			priorIn, priorOut = prev.inputSchema, prev.outputSchema
		}
		if _, err := tx.ExecContext(ctx, `UPDATE mcp_tool_discoveries SET state='gone', prior_input_schema=$4, prior_output_schema=$5, diff_fields='[]'::jsonb WHERE tenant_id=$1::uuid AND server_id=$2 AND tool_id=$3`,
			tenantID, serverID, toolID, nullJSON(priorIn), nullJSON(priorOut)); err != nil {
			return fmt.Errorf("mark MCP discovery gone: %w", err)
		}
	}
	// The server's health outcome rides the same transaction so a committed
	// snapshot and a "Connected" pill can never disagree.
	if _, err := tx.ExecContext(ctx, `UPDATE mcp_servers SET last_discovery_at=now(), last_discovery_error='' WHERE tenant_id=$1::uuid AND id=$2`, tenantID, serverID); err != nil {
		return fmt.Errorf("record MCP discovery outcome: %w", err)
	}
	return tx.Commit()
}

func (r *MCPToolsRepo) RecordDiscoveryFailure(ctx context.Context, tenantID, serverID string) error {
	res, err := r.db.SQL.ExecContext(ctx, `UPDATE mcp_servers SET last_discovery_at=now(), last_discovery_error='Discovery failed' WHERE tenant_id=$1::uuid AND id=$2`, tenantID, serverID)
	if err != nil {
		return fmt.Errorf("record MCP discovery failure: %w", err)
	}
	if n, _ := res.RowsAffected(); n == 0 {
		return ErrMCPApprovalNotFound
	}
	return nil
}

// DiscoveryState reports one tool's durable discovery state ("pending",
// "changed", "approved", "rejected", "gone"); unknown tools report an empty
// string. It is the narrow read run-time tool classification uses to explain
// WHY a configured tool is unavailable.
func (r *MCPToolsRepo) DiscoveryState(ctx context.Context, tenantID, serverID, toolID string) (string, error) {
	var state string
	err := r.db.SQL.QueryRowContext(ctx, `SELECT state FROM mcp_tool_discoveries WHERE tenant_id=$1::uuid AND server_id=$2 AND tool_id=$3`, tenantID, serverID, toolID).Scan(&state)
	if errors.Is(err, sql.ErrNoRows) {
		return "", nil
	}
	if err != nil {
		return "", fmt.Errorf("read MCP discovery state: %w", err)
	}
	return state, nil
}

// ApproveDiscovery is the one atomic review transition: lock the durable
// discovery row, compare the reviewed schema_version, install the approval
// from the persisted schemas and flip the row to approved — all in one
// transaction, so a concurrent discovery cannot slip between compare and
// install. A stale wantVersion fails with ErrMCPSchemaChanged.
func (r *MCPToolsRepo) ApproveDiscovery(ctx context.Context, tenantID, serverID, toolID string, wantVersion int64, modelName string, callerScopes []string) (MCPToolApproval, error) {
	tx, err := r.db.SQL.BeginTx(ctx, nil)
	if err != nil {
		return MCPToolApproval{}, err
	}
	defer tx.Rollback()
	var remoteName, state string
	var version int64
	var inputSchema, outputSchema json.RawMessage
	err = tx.QueryRowContext(ctx, `SELECT remote_name, state, schema_version, input_schema, output_schema FROM mcp_tool_discoveries WHERE tenant_id=$1::uuid AND server_id=$2 AND tool_id=$3 FOR UPDATE`, tenantID, serverID, toolID).Scan(&remoteName, &state, &version, &inputSchema, &outputSchema)
	if errors.Is(err, sql.ErrNoRows) || isInvalidUUID(err) {
		return MCPToolApproval{}, ErrMCPApprovalNotFound
	}
	if err != nil {
		return MCPToolApproval{}, fmt.Errorf("read MCP discovery: %w", err)
	}
	if state != "pending" && state != "changed" && state != "approved" {
		return MCPToolApproval{}, ErrMCPApprovalNotFound
	}
	if wantVersion != version {
		return MCPToolApproval{}, ErrMCPSchemaChanged
	}
	approval, err := approveTx(ctx, tx, MCPToolApproval{
		TenantID: tenantID, ServerID: serverID, ToolID: toolID,
		ModelName: strings.TrimSpace(modelName), RemoteName: remoteName,
		InputSchema: inputSchema, OutputSchema: outputSchema,
		SchemaVersion: version, CallerScopes: callerScopes, Enabled: true,
	})
	if err != nil {
		return MCPToolApproval{}, err
	}
	if _, err := tx.ExecContext(ctx, `UPDATE mcp_tool_discoveries SET state='approved', approved_schema_version=schema_version, prior_input_schema=NULL, prior_output_schema=NULL, diff_fields='[]'::jsonb, last_seen_at=now() WHERE tenant_id=$1::uuid AND server_id=$2 AND tool_id=$3`, tenantID, serverID, toolID); err != nil {
		return MCPToolApproval{}, fmt.Errorf("approve MCP discovery: %w", err)
	}
	if err := tx.Commit(); err != nil {
		return MCPToolApproval{}, err
	}
	return approval, nil
}

// RejectDiscovery durably rejects a never-approved (or changed) tool, and
// revokes then rejects an approved one. Rejected rows render in the queue;
// rejecting an already-rejected tool is an idempotent no-op; gone and
// unknown tools are not rejectable.
func (r *MCPToolsRepo) RejectDiscovery(ctx context.Context, tenantID, serverID, toolID string) error {
	tx, err := r.db.SQL.BeginTx(ctx, nil)
	if err != nil {
		return err
	}
	defer tx.Rollback()
	var state string
	err = tx.QueryRowContext(ctx, `SELECT state FROM mcp_tool_discoveries WHERE tenant_id=$1::uuid AND server_id=$2 AND tool_id=$3 FOR UPDATE`, tenantID, serverID, toolID).Scan(&state)
	if errors.Is(err, sql.ErrNoRows) || isInvalidUUID(err) {
		return ErrMCPApprovalNotFound
	}
	if err != nil {
		return fmt.Errorf("read MCP discovery: %w", err)
	}
	if state == "gone" {
		return ErrMCPApprovalNotFound
	}
	if state != "rejected" {
		if _, err := revokeTx(ctx, tx, tenantID, serverID, toolID); err != nil {
			return err
		}
		if _, err := tx.ExecContext(ctx, `UPDATE mcp_tool_discoveries SET state='rejected' WHERE tenant_id=$1::uuid AND server_id=$2 AND tool_id=$3`, tenantID, serverID, toolID); err != nil {
			return fmt.Errorf("reject MCP discovery: %w", err)
		}
	}
	return tx.Commit()
}

// mcpDiscoverySelect reads one discovery row per tool; prior_input_schema is
// NULL on rows without a change baseline, and listDiscoveries scans it into
// a nullable buffer so the absent prior stays nil instead of an empty doc.
const mcpDiscoverySelect = `SELECT d.tenant_id::text, d.server_id, d.tool_id, d.title, d.description, d.remote_name, d.state, d.schema_version, d.approved_schema_version, d.input_schema, d.output_schema, d.prior_input_schema, d.diff_fields, d.annotations, d.first_seen_at, d.last_seen_at, COALESCE(a.model_name, '')
FROM mcp_tool_discoveries d
LEFT JOIN mcp_tool_approvals a ON a.tenant_id=d.tenant_id AND a.server_id=d.server_id AND a.tool_id=d.tool_id`

// ListServerTools is the §6.9 queue read for one server: every durable
// discovery row with its approval-derived model name.
func (r *MCPToolsRepo) ListServerTools(ctx context.Context, tenantID, serverID string) ([]MCPToolDiscovery, error) {
	return r.listDiscoveries(ctx, mcpDiscoverySelect+` WHERE d.tenant_id=$1::uuid AND d.server_id=$2 ORDER BY d.tool_id`, tenantID, serverID)
}

// ListToolsByState reads queue rows across the tenant's servers, filtered to
// the given states (all states when none are given).
func (r *MCPToolsRepo) ListToolsByState(ctx context.Context, tenantID string, states ...string) ([]MCPToolDiscovery, error) {
	if len(states) == 0 {
		return r.listDiscoveries(ctx, mcpDiscoverySelect+` WHERE d.tenant_id=$1::uuid ORDER BY d.server_id, d.tool_id`, tenantID)
	}
	valid := map[string]bool{"pending": true, "changed": true, "approved": true, "rejected": true, "gone": true}
	placeholders := make([]string, 0, len(states))
	args := []any{tenantID}
	for _, state := range states {
		if !valid[state] {
			return nil, fmt.Errorf("postgres: invalid MCP discovery state %q", state)
		}
		args = append(args, state)
		placeholders = append(placeholders, fmt.Sprintf("$%d", len(args)))
	}
	return r.listDiscoveries(ctx, mcpDiscoverySelect+` WHERE d.tenant_id=$1::uuid AND d.state IN (`+strings.Join(placeholders, ",")+`) ORDER BY d.server_id, d.tool_id`, args...)
}

func (r *MCPToolsRepo) listDiscoveries(ctx context.Context, query string, args ...any) ([]MCPToolDiscovery, error) {
	rows, err := r.db.SQL.QueryContext(ctx, query, args...)
	if err != nil {
		return nil, err
	}
	defer rows.Close()
	out := []MCPToolDiscovery{}
	for rows.Next() {
		var d MCPToolDiscovery
		var annotations, priorInput []byte
		if err := rows.Scan(&d.TenantID, &d.ServerID, &d.ToolID, &d.Title, &d.Description, &d.RemoteName, &d.State, &d.SchemaVersion, &d.ApprovedSchemaVersion, &d.InputSchema, &d.OutputSchema, &priorInput, &d.DiffFields, &annotations, &d.FirstSeenAt, &d.LastSeenAt, &d.ModelName); err != nil {
			return nil, err
		}
		d.PriorInputSchema = priorInput // NULL column: no change baseline
		if err := json.Unmarshal(annotations, &d.Annotations); err != nil {
			return nil, err
		}
		out = append(out, d)
	}
	return out, rows.Err()
}

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
