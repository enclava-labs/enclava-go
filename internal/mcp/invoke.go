package mcp

import (
	"context"
	"encoding/json"
	"fmt"

	"enclava-go/internal/auth"
)

const MaxToolInputBytes = 64 << 10
const MaxToolOutputBytes = 1 << 20

type Caller interface {
	Authorize(context.Context, auth.Principal, Server, Approval, json.RawMessage) error
}
type Transport interface {
	Call(context.Context, Server, Approval, string, json.RawMessage, int64) (json.RawMessage, error)
}
type Executor struct {
	Repository Repository
	Caller     Caller
	Transport  Transport
}

// Invoke re-loads current server and approval state on every call. The server's
// advertised annotations never participate in authorization or replay policy.
func (e *Executor) Invoke(ctx context.Context, p auth.Principal, serverID, toolID string, args json.RawMessage) (json.RawMessage, error) {
	if e == nil || e.Repository == nil || e.Caller == nil || e.Transport == nil || p.TenantID == "" || len(args) == 0 || len(args) > MaxToolInputBytes || !json.Valid(args) {
		return nil, ErrDenied
	}
	s, err := e.Repository.GetServer(ctx, p.TenantID, serverID)
	if err != nil || s.TenantID != p.TenantID || s.ID != serverID || !s.Enabled || !hasAll(p, s.AudienceScopes) {
		return nil, ErrDenied
	}
	a, err := e.Repository.GetApproval(ctx, p.TenantID, serverID, toolID)
	if err != nil || a.TenantID != p.TenantID || a.ServerID != serverID || a.ToolID != toolID || !a.Enabled || a.InputSchemaHash != SchemaDigest(a.InputSchema) || a.OutputSchemaHash != SchemaDigest(a.OutputSchema) {
		return nil, ErrDenied
	}
	if !hasAll(p, a.CallerScopes) {
		return nil, ErrDenied
	}
	inputSchema, err := CompileSchema(a.InputSchema)
	if err != nil || ValidateJSON(inputSchema, args) != nil {
		return nil, ErrDenied
	}
	outputSchema, err := CompileSchema(a.OutputSchema)
	if err != nil {
		return nil, ErrDenied
	}
	if err = e.Caller.Authorize(ctx, p, s, a, args); err != nil {
		return nil, fmt.Errorf("%w: caller policy", ErrDenied)
	}
	result, err := e.Transport.Call(ctx, s, a, a.RemoteName, args, MaxToolOutputBytes)
	if err != nil {
		return nil, err
	}
	if len(result) > MaxToolOutputBytes || ValidateJSON(outputSchema, result) != nil {
		return nil, ErrDenied
	}
	return result, nil
}

func hasAll(p auth.Principal, scopes []string) bool {
	if len(scopes) == 0 {
		return false
	}
	for _, scope := range scopes {
		if _, ok := p.Scopes[scope]; !ok {
			return false
		}
	}
	return true
}
