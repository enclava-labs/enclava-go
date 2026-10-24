package chatbot

import (
	"context"
	"enclava-go/internal/source"
	"encoding/json"
	"errors"
	"testing"

	"enclava-go/internal/auth"
	"enclava-go/internal/mcp"
)

type runMCPRepo struct {
	server    mcp.Server
	approvals []mcp.Approval
}
type runMCPInputAuth struct{ err error }

func (a runMCPInputAuth) AuthorizeMCPInput(context.Context, auth.Principal, []source.ObjectRef) error {
	return a.err
}

func (r *runMCPRepo) GetServer(_ context.Context, tenant, id string) (mcp.Server, error) {
	if tenant != r.server.TenantID || id != r.server.ID || !r.server.Enabled {
		return mcp.Server{}, mcp.ErrDenied
	}
	return r.server, nil
}
func (r *runMCPRepo) ListServers(_ context.Context, tenant string) ([]mcp.Server, error) {
	if tenant != r.server.TenantID {
		return nil, nil
	}
	return []mcp.Server{r.server}, nil
}
func (r *runMCPRepo) GetApproval(_ context.Context, tenant, server, tool string) (mcp.Approval, error) {
	for _, a := range r.approvals {
		if a.TenantID == tenant && a.ServerID == server && a.ToolID == tool && a.Enabled {
			return a, nil
		}
	}
	return mcp.Approval{}, mcp.ErrDenied
}
func (r *runMCPRepo) ListApprovals(_ context.Context, tenant, server string) ([]mcp.Approval, error) {
	var out []mcp.Approval
	for _, a := range r.approvals {
		if a.TenantID == tenant && a.ServerID == server && a.Enabled {
			out = append(out, a)
		}
	}
	return out, nil
}

type runMCPTransport struct {
	calls int
	bound int64
}

func (t *runMCPTransport) Call(_ context.Context, _ mcp.Server, _ mcp.Approval, _ string, _ json.RawMessage, bound int64) (json.RawMessage, error) {
	t.calls++
	t.bound = bound
	return json.RawMessage(`{"ok":true}`), nil
}

func TestApprovedMCPToolRunsUnderConfiguredAssistantAndCurrentApproval(t *testing.T) {
	input := json.RawMessage(`{"type":"object","additionalProperties":false}`)
	output := json.RawMessage(`{"type":"object","properties":{"ok":{"type":"boolean"}},"required":["ok"],"additionalProperties":false}`)
	repo := &runMCPRepo{
		server:    mcp.Server{TenantID: "tenant", ID: "server", Origin: "https://mcp.example", EndpointPath: "/mcp", Enabled: true, AudienceScopes: []string{"assistant.use"}},
		approvals: []mcp.Approval{{TenantID: "tenant", ServerID: "server", ToolID: "remote-tool", ModelName: "approved.lookup", RemoteName: "remote-tool", InputSchema: input, OutputSchema: output, InputSchemaHash: mcp.SchemaHash(input), OutputSchemaHash: mcp.SchemaHash(output), CallerScopes: []string{"mcp.use"}, Enabled: true}},
	}
	transport := &runMCPTransport{}
	dispatcher := MCPRunTools{Executor: &mcp.Executor{Repository: repo, Caller: MCPCaller{}, Transport: transport}, Authorizer: runMCPInputAuth{}}
	p := auth.Principal{TenantID: "tenant", Scopes: map[string]struct{}{"assistant.use": {}, "mcp.use": {}}}
	bot := Instance{Settings: map[string]any{"tools": []any{"approved.lookup"}}}
	defs, err := dispatcher.Definitions(context.Background(), p, p, bot)
	if err != nil || len(defs) != 1 {
		t.Fatalf("tool definitions=%v err=%v", defs, err)
	}
	got, err := dispatcher.Invoke(context.Background(), p, p, bot, "approved.lookup", json.RawMessage(`{}`), nil)
	if err != nil || string(got.Value) != `{"ok":true}` {
		t.Fatalf("tool result=%s err=%v", got.Value, err)
	}
	ownerWithoutToolScope := auth.Principal{TenantID: "tenant", Scopes: map[string]struct{}{"assistant.use": {}}}
	if _, err := dispatcher.Invoke(context.Background(), p, ownerWithoutToolScope, bot, "approved.lookup", json.RawMessage(`{}`), nil); !errors.Is(err, mcp.ErrDenied) {
		t.Fatalf("owner without tool scope was allowed: %v", err)
	}
	if defs, err := dispatcher.Definitions(context.Background(), p, ownerWithoutToolScope, bot); err != nil || len(defs) != 0 {
		t.Fatalf("owner without tool scope exposed definitions: defs=%v err=%v", defs, err)
	}
	if transport.calls != 1 || transport.bound != mcp.MaxToolOutputBytes {
		t.Fatalf("calls=%d bound=%d", transport.calls, transport.bound)
	}
	if _, err := dispatcher.Invoke(context.Background(), p, p, Instance{}, "approved.lookup", json.RawMessage(`{}`), nil); !errors.Is(err, mcp.ErrDenied) {
		t.Fatalf("assistant-unconfigured tool error=%v", err)
	}
	dispatcher.Authorizer = runMCPInputAuth{err: source.ErrDenied}
	if _, err := dispatcher.Invoke(context.Background(), p, p, bot, "approved.lookup", json.RawMessage(`{}`), []source.ObjectRef{{ObjectID: "doc"}}); !errors.Is(err, source.ErrDenied) {
		t.Fatalf("restricted MCP input error=%v", err)
	}
	if transport.calls != 1 {
		t.Fatalf("restricted MCP input dispatched; calls=%d", transport.calls)
	}
	dispatcher.Authorizer = runMCPInputAuth{}
	if transport.calls != 1 {
		t.Fatalf("unconfigured tool dispatched; calls=%d", transport.calls)
	}
	repo.approvals[0].Enabled = false
	if _, err := dispatcher.Invoke(context.Background(), p, p, bot, "approved.lookup", json.RawMessage(`{}`), nil); !errors.Is(err, mcp.ErrDenied) {
		t.Fatalf("revoked tool error=%v", err)
	}
	if transport.calls != 1 {
		t.Fatalf("revoked tool dispatched; calls=%d", transport.calls)
	}
}

func TestChangedMCPApprovalSchemaCannotReachTransport(t *testing.T) {
	input := json.RawMessage(`{"type":"object"}`)
	output := json.RawMessage(`{"type":"object"}`)
	repo := &runMCPRepo{
		server:    mcp.Server{TenantID: "tenant", ID: "server", Origin: "https://mcp.example", Enabled: true, AudienceScopes: []string{"assistant.use"}},
		approvals: []mcp.Approval{{TenantID: "tenant", ServerID: "server", ToolID: "remote-tool", ModelName: "approved.lookup", RemoteName: "remote-tool", InputSchema: json.RawMessage(`{"type":"string"}`), OutputSchema: output, InputSchemaHash: mcp.SchemaHash(input), OutputSchemaHash: mcp.SchemaHash(output), CallerScopes: []string{"mcp.use"}, Enabled: true}},
	}
	transport := &runMCPTransport{}
	dispatcher := MCPRunTools{Executor: &mcp.Executor{Repository: repo, Caller: MCPCaller{}, Transport: transport}, Authorizer: runMCPInputAuth{}}
	p := auth.Principal{TenantID: "tenant", Scopes: map[string]struct{}{"assistant.use": {}, "mcp.use": {}}}
	bot := Instance{Settings: map[string]any{"tools": []any{"approved.lookup"}}}
	if _, err := dispatcher.Invoke(context.Background(), p, p, bot, "approved.lookup", json.RawMessage(`{}`), nil); !errors.Is(err, mcp.ErrDenied) {
		t.Fatalf("changed schema error=%v", err)
	}
	if transport.calls != 0 {
		t.Fatalf("schema-changed tool dispatched; calls=%d", transport.calls)
	}
}

func TestMCPDefinitionsSuppressDuplicateAndBuiltinNames(t *testing.T) {
	input := json.RawMessage(`{"type":"object"}`)
	output := json.RawMessage(`{"type":"object"}`)
	repo := &runMCPRepo{
		server: mcp.Server{TenantID: "tenant", ID: "server", Enabled: true, AudienceScopes: []string{"assistant.use"}},
		approvals: []mcp.Approval{
			{TenantID: "tenant", ServerID: "server", ToolID: "one", ModelName: "duplicate", InputSchema: input, Enabled: true, CallerScopes: []string{"mcp.use"}},
			{TenantID: "tenant", ServerID: "server", ToolID: "two", ModelName: "duplicate", InputSchema: input, Enabled: true, CallerScopes: []string{"mcp.use"}},
			{TenantID: "tenant", ServerID: "server", ToolID: "builtin", ModelName: "knowledge.search", InputSchema: input, OutputSchema: output, Enabled: true, CallerScopes: []string{"mcp.use"}},
		},
	}
	dispatcher := MCPRunTools{Executor: &mcp.Executor{Repository: repo}}
	p := auth.Principal{TenantID: "tenant", Scopes: map[string]struct{}{"assistant.use": {}, "mcp.use": {}}}
	bot := Instance{Settings: map[string]any{"tools": []any{"duplicate", "knowledge.search"}}}
	defs, err := dispatcher.Definitions(context.Background(), p, p, bot)
	if err != nil || len(defs) != 1 {
		t.Fatalf("duplicate/builtin model names advertised: defs=%v err=%v", defs, err)
	}
}
