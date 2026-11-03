package chatbot

import (
	"context"
	"encoding/json"
	"fmt"

	"enclava-go/internal/auth"
	"enclava-go/internal/mcp"
	"enclava-go/internal/source"
)

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

// toolUnavailableError explains WHY a configured MCP tool cannot be invoked —
// the run-step error and the model's tool result both carry this text. It
// wraps mcp.ErrDenied so an unavailable tool still aborts the run under the
// same classification every other MCP denial takes.
type toolUnavailableError struct{ reason string }

func (e *toolUnavailableError) Error() string { return e.reason }
func (e *toolUnavailableError) Unwrap() error { return mcp.ErrDenied }

// mcpApprovalLister is the optional repository surface that includes REVOKED
// approvals — the enabled-only ListApprovals cannot distinguish pending,
// rejected or scope-denied tools.
type mcpApprovalLister interface {
	ListAllApprovals(context.Context, string, string) ([]mcp.Approval, error)
}

// mcpDiscoveryStater is the optional repository surface that reports one
// tool's durable discovery state (rejected tools keep their approval row
// disabled, so the state distinguishes "rejected" from "pending approval").
type mcpDiscoveryStater interface {
	DiscoveryState(context.Context, string, string, string) (string, error)
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
	seen := make(map[string]struct{})
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
			_, builtinCollision := builtinSchemas[approval.ModelName]
			_, duplicate := seen[approval.ModelName]
			if !contains(configured, approval.ModelName) || builtinCollision || duplicate ||
				!mcp.HasScopes(p, server.AudienceScopes, approval.CallerScopes) ||
				!mcp.HasScopes(owner, server.AudienceScopes, approval.CallerScopes) {
				continue
			}
			var schema map[string]any
			if err := json.Unmarshal(approval.InputSchema, &schema); err != nil {
				return nil, mcp.ErrDenied
			}
			seen[approval.ModelName] = struct{}{}
			tools = append(tools, map[string]any{"type": "function", "function": map[string]any{
				"name": approval.ModelName, "description": "Invoke an explicitly approved MCP tool.", "parameters": schema,
			}})
		}
	}
	return tools, nil
}

func (d MCPRunTools) Invoke(ctx context.Context, p, owner auth.Principal, bot Instance, modelName string, args json.RawMessage, inputs []source.ObjectRef) (BuiltinResult, error) {
	if d.Executor == nil || d.Executor.Repository == nil || d.Authorizer == nil || !toolEnabled(bot, modelName) {
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
	if reason := d.unavailableReason(ctx, p, owner, modelName, servers); reason != nil {
		return BuiltinResult{}, reason
	}
	return BuiltinResult{}, mcp.ErrDenied
}

// UnavailableReason is the run-start gate's classifier: nil when the named
// tool is usable (or the reason cannot be determined), otherwise the
// distinct unavailability reason Invoke would report.
func (d MCPRunTools) UnavailableReason(ctx context.Context, p, owner auth.Principal, _ Instance, modelName string) error {
	if d.Executor == nil || d.Executor.Repository == nil {
		return nil
	}
	servers, err := d.Executor.Repository.ListServers(ctx, p.TenantID)
	if err != nil {
		return nil
	}
	return d.unavailableReason(ctx, p, owner, modelName, servers)
}

// unavailableReason classifies why the named tool had no usable enabled
// approval: server disabled, awaiting approval, rejected, or caller scopes
// missing. It returns nil when no repository evidence names a reason — the
// caller falls back to "unknown tool".
func (d MCPRunTools) unavailableReason(ctx context.Context, p, owner auth.Principal, modelName string, servers []mcp.Server) error {
	lister, _ := d.Executor.Repository.(mcpApprovalLister)
	stater, _ := d.Executor.Repository.(mcpDiscoveryStater)
	if lister == nil {
		return nil
	}
	for _, server := range servers {
		approvals, err := lister.ListAllApprovals(ctx, p.TenantID, server.ID)
		if err != nil {
			continue
		}
		for _, approval := range approvals {
			if approval.ModelName != modelName {
				continue
			}
			if !server.Enabled {
				return &toolUnavailableError{reason: fmt.Sprintf("tool %q is on a disabled server", modelName)}
			}
			if !approval.Enabled {
				state := ""
				if stater != nil {
					state, _ = stater.DiscoveryState(ctx, p.TenantID, server.ID, approval.ToolID)
				}
				if state == "rejected" {
					return &toolUnavailableError{reason: fmt.Sprintf("tool %q was rejected", modelName)}
				}
				return &toolUnavailableError{reason: fmt.Sprintf("tool %q is pending approval", modelName)}
			}
			if !mcp.HasScopes(p, server.AudienceScopes, approval.CallerScopes) || !mcp.HasScopes(owner, server.AudienceScopes, approval.CallerScopes) {
				return &toolUnavailableError{reason: fmt.Sprintf("tool %q requires caller scopes this caller does not hold", modelName)}
			}
		}
		if stater != nil {
			state, err := stater.DiscoveryState(ctx, p.TenantID, server.ID, modelName)
			if err == nil && (state == "pending" || state == "changed" || state == "rejected") {
				if !server.Enabled {
					return &toolUnavailableError{reason: fmt.Sprintf("tool %q is on a disabled server", modelName)}
				}
				if state == "rejected" {
					return &toolUnavailableError{reason: fmt.Sprintf("tool %q was rejected", modelName)}
				}
				return &toolUnavailableError{reason: fmt.Sprintf("tool %q is pending approval", modelName)}
			}
		}
	}
	return nil
}
