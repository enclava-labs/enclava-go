package httpx

import (
	"context"
	"encoding/json"
	"errors"
	"net/http"
	"strings"
	"time"

	"enclava-go/internal/auth"
	"enclava-go/internal/mcp"
	"enclava-go/internal/store/postgres"
)

type mcpServerInput struct {
	ID                string   `json:"id"`
	Origin            string   `json:"origin"`
	EndpointPath      string   `json:"endpoint_path"`
	CredentialRef     string   `json:"credential_ref"`
	AuthorizedPrivate bool     `json:"authorized_private"`
	AudienceScopes    []string `json:"audience_scopes"`
	Enabled           bool     `json:"enabled"`
}

type mcpServerResponse struct {
	ID                string   `json:"id"`
	Origin            string   `json:"origin"`
	EndpointPath      string   `json:"endpoint_path"`
	CredentialRef     string   `json:"credential_ref,omitempty"`
	AuthorizedPrivate bool     `json:"authorized_private"`
	AudienceScopes    []string `json:"audience_scopes"`
	Enabled           bool     `json:"enabled"`
}

// mcpDiscoveryRow is the frozen §6.9 queue-row contract: durable state,
// schemas, the approved baseline, field-level diffs and untrusted display
// annotations. schema_version is the durable current version the PUT must
// echo to approve.
type mcpDiscoveryRow struct {
	ToolID        string          `json:"tool_id"`
	ModelName     string          `json:"model_name"`
	RemoteName    string          `json:"remote_name"`
	State         string          `json:"state"`
	SchemaVersion int64           `json:"schema_version"`
	InputSchema   json.RawMessage `json:"input_schema"`
	OutputSchema  json.RawMessage `json:"output_schema"`
	PriorSchema   json.RawMessage `json:"prior_schema"`
	DiffFields    json.RawMessage `json:"diff_fields"`
	Annotations   map[string]any  `json:"annotations"`
	DiscoveredAt  time.Time       `json:"discovered_at"`
}

func (in mcpServerInput) server(tenantID, id string) mcp.Server {
	if id == "" {
		id = in.ID
	}
	scopes := normalizeMCPScopeList(in.AudienceScopes)
	return mcp.Server{TenantID: tenantID, ID: id, Origin: in.Origin, EndpointPath: in.EndpointPath, CredentialRef: in.CredentialRef, AuthorizedPrivate: in.AuthorizedPrivate, AudienceScopes: scopes, Enabled: in.Enabled}
}

func normalizeMCPScopeList(scopes []string) []string {
	out := make([]string, 0, len(scopes))
	for _, scope := range scopes {
		out = append(out, strings.TrimSpace(scope))
	}
	return out
}
func mcpBuiltinName(name string) bool {
	switch name {
	case "knowledge.search", "evidence.read", "extract.submit", "extract.status":
		return true
	default:
		return false
	}
}

func mcpServerJSON(server mcp.Server) mcpServerResponse {
	return mcpServerResponse{ID: server.ID, Origin: server.Origin, EndpointPath: server.EndpointPath, CredentialRef: server.CredentialRef, AuthorizedPrivate: server.AuthorizedPrivate, AudienceScopes: server.AudienceScopes, Enabled: server.Enabled}
}

// mcpAnnotationsWire surfaces the stored display hints with the MCP spec's
// defaults made explicit. They are untrusted UI metadata — never authority.
func mcpAnnotationsWire(an mcp.ToolAnnotations) map[string]any {
	wire := map[string]any{
		"read_only":   an.ReadOnlyHint != nil && *an.ReadOnlyHint,
		"writes_data": an.DestructiveHint == nil || *an.DestructiveHint,
		"idempotent":  an.IdempotentHint != nil && *an.IdempotentHint,
		"open_world":  an.OpenWorldHint == nil || *an.OpenWorldHint,
	}
	if an.Title != "" {
		wire["title"] = an.Title
	}
	return wire
}

func mcpDiscoveryJSON(d postgres.MCPToolDiscovery) mcpDiscoveryRow {
	return mcpDiscoveryRow{
		ToolID: d.ToolID, ModelName: d.ModelName, RemoteName: d.RemoteName,
		State: d.State, SchemaVersion: d.SchemaVersion,
		InputSchema: d.InputSchema, OutputSchema: d.OutputSchema,
		PriorSchema: d.PriorInputSchema, DiffFields: d.DiffFields,
		Annotations: mcpAnnotationsWire(d.Annotations), DiscoveredAt: d.LastSeenAt,
	}
}

func mcpDiscoveryRows(rows []postgres.MCPToolDiscovery) []mcpDiscoveryRow {
	out := make([]mcpDiscoveryRow, 0, len(rows))
	for _, row := range rows {
		out = append(out, mcpDiscoveryJSON(row))
	}
	return out
}

func (a *App) ConfigureMCP(repo *postgres.MCPToolsRepo, transport *mcp.StreamableHTTP) {
	a.mcpRepo, a.mcpTransport = repo, transport
	a.mux.HandleFunc("/api/v1/mcp/servers", a.authenticated("platform.admin", a.handleMCPServers))
	a.mux.HandleFunc("/api/v1/mcp/servers/", a.authenticated("platform.admin", a.handleMCPServerByID))
}

func (a *App) handleMCPServers(w http.ResponseWriter, r *http.Request, p auth.Principal) {
	if a.mcpRepo == nil || !auth.HasScope(p, "platform.admin") {
		WriteForbidden(w, "tenant administrator access required")
		return
	}
	switch r.Method {
	case http.MethodGet:
		servers, err := a.mcpRepo.ListServers(r.Context(), p.TenantID)
		if err != nil {
			WriteOpenAIError(w, http.StatusInternalServerError, "internal server error", "server_error", "mcp_server_list_failed")
			return
		}
		response := make([]mcpServerResponse, 0, len(servers))
		for _, server := range servers {
			response = append(response, mcpServerJSON(server))
		}
		WriteJSON(w, http.StatusOK, map[string]any{"servers": response})
	case http.MethodPost:
		var input mcpServerInput
		if err := DecodeJSON(r, &input); err != nil {
			WriteOpenAIError(w, http.StatusBadRequest, "invalid JSON payload", "invalid_request_error", "invalid_json")
			return
		}
		server := input.server(p.TenantID, "")
		if server.ID == "" {
			WriteOpenAIError(w, http.StatusBadRequest, "server id is required", "invalid_request_error", "invalid_mcp_server")
			return
		}
		if err := a.mcpRepo.PutServer(r.Context(), server); err != nil {
			WriteOpenAIError(w, http.StatusBadRequest, err.Error(), "invalid_request_error", "invalid_mcp_server")
			return
		}
		a.recordSecurityAudit(r, p, "mcp.server.created", "mcp.server", server.ID, "success")
		WriteJSON(w, http.StatusCreated, mcpServerJSON(server))
	default:
		WriteMethodNotAllowed(w)
	}
}

func (a *App) handleMCPServerByID(w http.ResponseWriter, r *http.Request, p auth.Principal) {
	if a.mcpRepo == nil || !auth.HasScope(p, "platform.admin") {
		WriteForbidden(w, "tenant administrator access required")
		return
	}
	rest := strings.Trim(strings.TrimPrefix(r.URL.Path, "/api/v1/mcp/servers/"), "/")
	parts := strings.Split(rest, "/")
	if len(parts) == 0 || parts[0] == "" {
		http.NotFound(w, r)
		return
	}
	serverID := parts[0]
	if len(parts) == 1 {
		switch r.Method {
		case http.MethodGet:
			server, err := a.mcpRepo.GetServer(r.Context(), p.TenantID, serverID)
			if err != nil {
				http.NotFound(w, r)
				return
			}
			WriteJSON(w, http.StatusOK, mcpServerJSON(server))
		case http.MethodPut:
			var input mcpServerInput
			if err := DecodeJSON(r, &input); err != nil {
				WriteOpenAIError(w, http.StatusBadRequest, "invalid JSON payload", "invalid_request_error", "invalid_json")
				return
			}
			server := input.server(p.TenantID, serverID)
			if err := a.mcpRepo.PutServer(r.Context(), server); err != nil {
				WriteOpenAIError(w, http.StatusBadRequest, err.Error(), "invalid_request_error", "invalid_mcp_server")
				return
			}
			a.recordSecurityAudit(r, p, "mcp.server.updated", "mcp.server", serverID, "success")
			WriteJSON(w, http.StatusOK, mcpServerJSON(server))
		case http.MethodDelete:
			if err := a.mcpRepo.DeleteServer(r.Context(), p.TenantID, serverID); err != nil {
				http.NotFound(w, r)
				return
			}
			a.recordSecurityAudit(r, p, "mcp.server.deleted", "mcp.server", serverID, "success")
			WriteJSON(w, http.StatusOK, map[string]any{"deleted": true, "id": serverID})
		default:
			WriteMethodNotAllowed(w)
		}
		return
	}
	if len(parts) == 2 && parts[1] == "discovery" && r.Method == http.MethodPost {
		if a.mcpTransport == nil {
			WriteOpenAIError(w, http.StatusServiceUnavailable, "MCP transport is unavailable", "server_error", "mcp_unavailable")
			return
		}
		server, err := a.mcpRepo.GetServer(r.Context(), p.TenantID, serverID)
		if err != nil {
			http.NotFound(w, r)
			return
		}
		tools, err := a.mcpTransport.Discover(r.Context(), server)
		if err != nil {
			writeCtx, cancel := context.WithTimeout(context.WithoutCancel(r.Context()), 5*time.Second)
			writeErr := a.mcpRepo.RecordDiscoveryFailure(writeCtx, p.TenantID, serverID)
			cancel()
			if writeErr != nil {
				WriteOpenAIError(w, http.StatusInternalServerError, "could not record discovery failure", "server_error", "mcp_discovery_persist_failed")
				return
			}
			WriteOpenAIError(w, http.StatusBadGateway, "MCP discovery failed", "server_error", "mcp_discovery_failed")
			return
		}
		// The repo owns every state transition: server identity comes from
		// the authenticated route row, so an empty snapshot still revokes
		// exactly this server's vanished or changed approvals.
		if err := a.mcpRepo.RecordDiscovery(r.Context(), p.TenantID, serverID, tools); err != nil {
			WriteOpenAIError(w, http.StatusInternalServerError, "internal server error", "server_error", "mcp_discovery_persist_failed")
			return
		}
		rows, err := a.mcpRepo.ListServerTools(r.Context(), p.TenantID, serverID)
		if err != nil {
			WriteOpenAIError(w, http.StatusInternalServerError, "internal server error", "server_error", "mcp_tools_list_failed")
			return
		}
		WriteJSON(w, http.StatusOK, map[string]any{"tools": mcpDiscoveryRows(rows)})
		return
	}
	if len(parts) == 2 && parts[1] == "tools" && r.Method == http.MethodGet {
		rows, err := a.mcpRepo.ListServerTools(r.Context(), p.TenantID, serverID)
		if err != nil {
			WriteOpenAIError(w, http.StatusInternalServerError, "internal server error", "server_error", "mcp_tools_list_failed")
			return
		}
		WriteJSON(w, http.StatusOK, map[string]any{"tools": mcpDiscoveryRows(rows)})
		return
	}
	if len(parts) == 3 && parts[1] == "tools" {
		toolID := parts[2]
		switch r.Method {
		case http.MethodDelete:
			// pending/changed -> durable rejected; approved -> revoked;
			// rejected -> idempotent; gone/unknown -> 404.
			if err := a.mcpRepo.RejectDiscovery(r.Context(), p.TenantID, serverID, toolID); err != nil {
				if errors.Is(err, postgres.ErrMCPApprovalNotFound) {
					http.NotFound(w, r)
					return
				}
				WriteOpenAIError(w, http.StatusInternalServerError, "internal server error", "server_error", "mcp_revoke_failed")
				return
			}
			a.recordSecurityAudit(r, p, "mcp.tool.revoked", "mcp.tool", serverID+"/"+toolID, "success")
			WriteJSON(w, http.StatusOK, map[string]any{"revoked": true, "tool_id": toolID})
		case http.MethodPut:
			// Approval never rediscovers: it installs the persisted schemas
			// if and only if the reviewed schema_version still matches the
			// durable current version — compare and install are one tx.
			var input struct {
				ModelName     string   `json:"model_name"`
				CallerScopes  []string `json:"caller_scopes"`
				SchemaVersion *int64   `json:"schema_version"`
			}
			if err := DecodeJSON(r, &input); err != nil {
				WriteOpenAIError(w, http.StatusBadRequest, "invalid JSON payload", "invalid_request_error", "invalid_mcp_approval")
				return
			}
			modelName := strings.TrimSpace(input.ModelName)
			if modelName == "" || mcpBuiltinName(modelName) {
				WriteOpenAIError(w, http.StatusBadRequest, "model_name conflicts with a built-in tool", "invalid_request_error", "invalid_mcp_approval")
				return
			}
			if input.SchemaVersion == nil || *input.SchemaVersion < 1 {
				WriteOpenAIError(w, http.StatusBadRequest, "schema_version of the reviewed discovery row is required", "invalid_request_error", "invalid_mcp_approval")
				return
			}
			approval, err := a.mcpRepo.ApproveDiscovery(r.Context(), p.TenantID, serverID, toolID, *input.SchemaVersion, modelName, normalizeMCPScopeList(input.CallerScopes))
			switch {
			case err == nil:
			case errors.Is(err, postgres.ErrMCPSchemaChanged):
				WriteOpenAIError(w, http.StatusConflict, "schema changed since review; rediscover and review again", "invalid_request_error", "schema_changed")
				return
			case errors.Is(err, postgres.ErrMCPApprovalNotFound):
				http.NotFound(w, r)
				return
			default:
				WriteOpenAIError(w, http.StatusBadRequest, "invalid MCP approval", "invalid_request_error", "invalid_mcp_approval")
				return
			}
			a.recordSecurityAudit(r, p, "mcp.tool.approved", "mcp.tool", serverID+"/"+toolID, "success")
			WriteJSON(w, http.StatusOK, map[string]any{"approved": true, "server_id": serverID, "tool_id": toolID, "model_name": approval.ModelName, "schema_version": approval.SchemaVersion})
		default:
			WriteMethodNotAllowed(w)
		}
		return
	}
	http.NotFound(w, r)
}
