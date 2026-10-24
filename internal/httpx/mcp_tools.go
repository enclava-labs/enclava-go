package httpx

import (
	"errors"
	"net/http"
	"strings"

	"enclava-go/internal/auth"
	"enclava-go/internal/mcp"
	"enclava-go/internal/store/postgres"
)

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
		WriteJSON(w, http.StatusOK, map[string]any{"servers": servers})
	case http.MethodPost:
		var server mcp.Server
		if err := DecodeJSON(r, &server); err != nil {
			WriteOpenAIError(w, http.StatusBadRequest, "invalid JSON payload", "invalid_request_error", "invalid_json")
			return
		}
		server.TenantID = p.TenantID
		if server.ID == "" {
			WriteOpenAIError(w, http.StatusBadRequest, "server id is required", "invalid_request_error", "invalid_mcp_server")
			return
		}
		if err := a.mcpRepo.PutServer(r.Context(), server); err != nil {
			WriteOpenAIError(w, http.StatusBadRequest, err.Error(), "invalid_request_error", "invalid_mcp_server")
			return
		}
		WriteJSON(w, http.StatusCreated, server)
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
			WriteJSON(w, http.StatusOK, server)
		case http.MethodPut:
			var server mcp.Server
			if err := DecodeJSON(r, &server); err != nil {
				WriteOpenAIError(w, http.StatusBadRequest, "invalid JSON payload", "invalid_request_error", "invalid_json")
				return
			}
			server.TenantID, server.ID = p.TenantID, serverID
			if err := a.mcpRepo.PutServer(r.Context(), server); err != nil {
				WriteOpenAIError(w, http.StatusBadRequest, err.Error(), "invalid_request_error", "invalid_mcp_server")
				return
			}
			WriteJSON(w, http.StatusOK, server)
		case http.MethodDelete:
			if err := a.mcpRepo.DeleteServer(r.Context(), p.TenantID, serverID); err != nil {
				http.NotFound(w, r)
				return
			}
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
			WriteOpenAIError(w, http.StatusBadGateway, "MCP discovery failed", "server_error", "mcp_discovery_failed")
			return
		}
		// A discovered schema change or removed tool revokes the old revision.
		discovered := make(map[string]mcp.DiscoveredTool, len(tools))
		for _, tool := range tools {
			discovered[tool.Name] = tool
		}
		approved, err := a.mcpRepo.ListApprovals(r.Context(), p.TenantID, serverID)
		if err != nil {
			WriteOpenAIError(w, http.StatusInternalServerError, "internal server error", "server_error", "mcp_approval_list_failed")
			return
		}
		for _, old := range approved {
			tool, ok := discovered[old.ToolID]
			if !ok || old.InputSchemaHash != mcp.SchemaHash(tool.InputSchema) || old.OutputSchemaHash != mcp.SchemaHash(tool.OutputSchema) {
				if err := a.mcpRepo.Revoke(r.Context(), p.TenantID, serverID, old.ToolID); err != nil {
					WriteOpenAIError(w, http.StatusInternalServerError, "internal server error", "server_error", "mcp_revoke_failed")
					return
				}
			}
		}
		WriteJSON(w, http.StatusOK, map[string]any{"tools": tools})
		return
	}
	if len(parts) == 2 && parts[1] == "tools" && r.Method == http.MethodGet {
		approvals, err := a.mcpRepo.ListAllApprovals(r.Context(), p.TenantID, serverID)
		if err != nil {
			WriteOpenAIError(w, http.StatusInternalServerError, "internal server error", "server_error", "mcp_tools_list_failed")
			return
		}
		WriteJSON(w, http.StatusOK, map[string]any{"tools": approvals})
		return
	}
	if len(parts) == 3 && parts[1] == "tools" {
		toolID := parts[2]
		switch r.Method {
		case http.MethodDelete:
			if err := a.mcpRepo.Revoke(r.Context(), p.TenantID, serverID, toolID); err != nil {
				http.NotFound(w, r)
				return
			}
			WriteJSON(w, http.StatusOK, map[string]any{"revoked": true, "tool_id": toolID})
		case http.MethodPut:
			if a.mcpTransport == nil {
				WriteOpenAIError(w, http.StatusServiceUnavailable, "MCP transport is unavailable", "server_error", "mcp_unavailable")
				return
			}
			var input struct {
				ModelName    string   `json:"model_name"`
				CallerScopes []string `json:"caller_scopes"`
			}
			if err := DecodeJSON(r, &input); err != nil || strings.TrimSpace(input.ModelName) == "" || len(input.CallerScopes) == 0 {
				WriteOpenAIError(w, http.StatusBadRequest, "model_name and caller_scopes are required", "invalid_request_error", "invalid_mcp_approval")
				return
			}
			server, err := a.mcpRepo.GetServer(r.Context(), p.TenantID, serverID)
			if err != nil {
				http.NotFound(w, r)
				return
			}
			discovered, err := a.mcpTransport.Discover(r.Context(), server)
			if err != nil {
				WriteOpenAIError(w, http.StatusBadGateway, "MCP discovery failed", "server_error", "mcp_discovery_failed")
				return
			}
			var selected *mcp.DiscoveredTool
			for i := range discovered {
				if discovered[i].Name == toolID {
					selected = &discovered[i]
					break
				}
			}
			if selected == nil {
				http.NotFound(w, r)
				return
			}
			version := int64(1)
			old, oldErr := a.mcpRepo.GetApprovalAny(r.Context(), p.TenantID, serverID, toolID)
			if oldErr == nil {
				version = old.SchemaVersion
				if old.InputSchemaHash != mcp.SchemaHash(selected.InputSchema) || old.OutputSchemaHash != mcp.SchemaHash(selected.OutputSchema) {
					version++
				}
			} else if !errors.Is(oldErr, postgres.ErrMCPApprovalNotFound) {
				WriteOpenAIError(w, http.StatusInternalServerError, "internal server error", "server_error", "mcp_approval_lookup_failed")
				return
			}
			approval := postgres.MCPToolApproval{
				TenantID: p.TenantID, ServerID: serverID, ToolID: toolID,
				ModelName: strings.TrimSpace(input.ModelName), RemoteName: selected.Name,
				InputSchema: selected.InputSchema, OutputSchema: selected.OutputSchema,
				SchemaVersion: version, CallerScopes: input.CallerScopes, Enabled: true,
			}
			if err := a.mcpRepo.Approve(r.Context(), approval); err != nil {
				WriteOpenAIError(w, http.StatusBadRequest, "invalid MCP approval", "invalid_request_error", "invalid_mcp_approval")
				return
			}
			WriteJSON(w, http.StatusOK, map[string]any{"approved": true, "server_id": serverID, "tool_id": toolID, "model_name": approval.ModelName, "schema_version": approval.SchemaVersion})
		default:
			WriteMethodNotAllowed(w)
		}
		return
	}
	http.NotFound(w, r)
}
