package chatbot

import (
	"context"
	"encoding/json"
	"regexp"

	"enclava-go/internal/auth"
	"enclava-go/internal/mcp"
	"enclava-go/internal/source"
)

var mcpModelName = regexp.MustCompile(`^[A-Za-z0-9_.-]{1,64}$`)

type MCPRunTools struct {
	Executor   *mcp.Executor
	Authorizer MCPInputAuthorizer
}

type MCPCaller struct{}

func (MCPCaller) Authorize(_ context.Context, p auth.Principal, server mcp.Server, approval mcp.Approval, args json.RawMessage) error {
	if p.TenantID == "" || p.TenantID != server.TenantID || server.ID != approval.ServerID ||
		len(args) == 0 || len(args) > mcp.MaxToolInputBytes ||
		!mcp.HasScopes(p, server.AudienceScopes, approval.CallerScopes) {
		return mcp.ErrDenied
	}
	return nil
}

func (d MCPRunTools) Definitions(ctx context.Context, p, owner auth.Principal, bot Instance) ([]map[string]any, error) {
	if d.Executor == nil || d.Executor.Repository == nil {
		return nil, nil
	}
	configured := configuredToolIDs(bot)
	servers, err := d.Executor.Repository.ListServers(ctx, p.TenantID)
	if err != nil {
		return nil, err
	}
	var tools []map[string]any
	for _, server := range servers {
		if !server.Enabled {
			continue
		}
		approvals, err := d.Executor.Repository.ListApprovals(ctx, p.TenantID, server.ID)
		if err != nil {
			return nil, err
		}
		for _, approval := range approvals {
			if !contains(configured, approval.ModelName) || !mcpModelName.MatchString(approval.ModelName) ||
				(!mcp.HasScopes(p, server.AudienceScopes, approval.CallerScopes) ||
					!mcp.HasScopes(owner, server.AudienceScopes, approval.CallerScopes)) {
				continue
			}
			var schema map[string]any
			if err := json.Unmarshal(approval.InputSchema, &schema); err != nil {
				return nil, mcp.ErrDenied
			}
			tools = append(tools, map[string]any{"type": "function", "function": map[string]any{
				"name": approval.ModelName, "description": "Invoke an explicitly approved MCP tool.", "parameters": schema,
			}})
		}
	}
	return tools, nil
}

func (d MCPRunTools) Invoke(ctx context.Context, p, owner auth.Principal, bot Instance, modelName string, args json.RawMessage, inputs []source.ObjectRef) (BuiltinResult, error) {
	if d.Executor == nil || d.Executor.Repository == nil || d.Authorizer == nil || !toolEnabled(bot, modelName) || !mcpModelName.MatchString(modelName) {
		return BuiltinResult{}, mcp.ErrDenied
	}
	if err := d.Authorizer.AuthorizeMCPInput(ctx, p, inputs); err != nil {
		return BuiltinResult{}, err
	}
	servers, err := d.Executor.Repository.ListServers(ctx, p.TenantID)
	if err != nil {
		return BuiltinResult{}, err
	}
	for _, server := range servers {
		if !server.Enabled {
			continue
		}
		approvals, err := d.Executor.Repository.ListApprovals(ctx, p.TenantID, server.ID)
		if err != nil {
			return BuiltinResult{}, err
		}
		for _, approval := range approvals {
			if approval.ModelName != modelName || !mcp.HasScopes(p, server.AudienceScopes, approval.CallerScopes) ||
				!mcp.HasScopes(owner, server.AudienceScopes, approval.CallerScopes) {
				continue
			}
			value, err := d.Executor.Invoke(ctx, p, server.ID, approval.ToolID, args)
			if err != nil {
				return BuiltinResult{}, err
			}
			return BuiltinResult{Value: value}, nil
		}
	}
	return BuiltinResult{}, mcp.ErrDenied
}
