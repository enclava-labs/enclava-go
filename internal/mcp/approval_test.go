package mcp

import (
	"context"
	"encoding/json"
	"errors"
	"testing"

	"enclava-go/internal/auth"
)

func TestOriginBindingAndSchemaValidation(t *testing.T) {
	for _, tc := range []struct {
		origin  string
		private bool
		want    string
		bad     bool
	}{
		{"https://Example.com:443", false, "https://example.com", false},
		{"http://127.0.0.1:8080", true, "http://127.0.0.1:8080", false},
		{"http://[::1]", true, "http://[::1]", false},
		{"http://10.0.0.1", true, "", true}, {"https://user@host", false, "", true},
		{"https://host/path", false, "", true}, {"file:///tmp", true, "", true},
	} {
		got, err := ValidateOrigin(tc.origin, tc.private)
		if (err != nil) != tc.bad || got != tc.want {
			t.Errorf("ValidateOrigin(%q)=(%q,%v)", tc.origin, got, err)
		}
	}
	validate, err := CompileSchema(json.RawMessage(`{"$schema":"https://json-schema.org/draft/2020-12/schema","type":"object","properties":{"query":{"type":"string"}},"required":["query"],"additionalProperties":false}`))
	if err != nil {
		t.Fatal(err)
	}
	if err = ValidateJSON(validate, json.RawMessage(`{"query":"x"}`)); err != nil {
		t.Fatal(err)
	}
	if err = ValidateJSON(validate, json.RawMessage(`{"query":1}`)); err == nil {
		t.Fatal("invalid input schema accepted")
	}
	if _, err = CompileSchema(json.RawMessage(`{"$ref":"https://evil.example/schema"}`)); err == nil {
		t.Fatal("remote schema reference accepted")
	}
}

type testRepo struct {
	s Server
	a Approval
}

func (r testRepo) GetServer(_ context.Context, tenant, id string) (Server, error) {
	if tenant != r.s.TenantID || id != r.s.ID || !r.s.Enabled {
		return Server{}, errors.New("not found")
	}
	return r.s, nil
}
func (r testRepo) ListServers(context.Context, string) ([]Server, error) { return []Server{r.s}, nil }
func (r testRepo) GetApproval(_ context.Context, tenant, server, tool string) (Approval, error) {
	if tenant != r.a.TenantID || server != r.a.ServerID || tool != r.a.ToolID || !r.a.Enabled {
		return Approval{}, errors.New("not found")
	}
	return r.a, nil
}
func (r testRepo) ListApprovals(context.Context, string, string) ([]Approval, error) { return nil, nil }

type testCaller struct{ called bool }

func (c *testCaller) Authorize(context.Context, auth.Principal, Server, Approval, json.RawMessage) error {
	c.called = true
	return nil
}

type testTransport struct{ calls int }

func (tr *testTransport) Call(context.Context, Server, Approval, string, json.RawMessage, int64) (json.RawMessage, error) {
	tr.calls++
	return json.RawMessage(`{"ok":true}`), nil
}

func TestInvokeRequiresCurrentApprovalAndCallerScope(t *testing.T) {
	a := Approval{TenantID: "t", ServerID: "s", ToolID: "x", RemoteName: "remote-hostile-name", InputSchema: json.RawMessage(`{"type":"object"}`), OutputSchema: json.RawMessage(`{"type":"object"}`), Enabled: true, CallerScopes: []string{"mcp.use"}}
	a.InputSchemaHash = SchemaHash(a.InputSchema)
	a.OutputSchemaHash = SchemaHash(a.OutputSchema)
	repo := testRepo{Server{TenantID: "t", ID: "s", Origin: "https://mcp.test", Enabled: true, AudienceScopes: []string{"assistant.use"}}, a}
	caller := &testCaller{}
	transport := &testTransport{}
	exec := Executor{Repository: repo, Caller: caller, Transport: transport}
	p := auth.Principal{TenantID: "t", Scopes: map[string]struct{}{"assistant.use": {}, "mcp.use": {}}}
	if _, err := exec.Invoke(context.Background(), p, "s", "x", json.RawMessage(`{}`)); err != nil {
		t.Fatal(err)
	}
	if transport.calls != 1 || !caller.called {
		t.Fatalf("dispatch=%d caller=%v", transport.calls, caller.called)
	}
	p.TenantID = "foreign"
	if _, err := exec.Invoke(context.Background(), p, "s", "x", json.RawMessage(`{}`)); !errors.Is(err, ErrDenied) {
		t.Fatalf("cross-tenant invocation = %v", err)
	}
	if transport.calls != 1 {
		t.Fatal("foreign call reached transport")
	}
	p.TenantID = "t"
	repo.a.InputSchema = json.RawMessage(`{"type":"string"}`)
	exec.Repository = repo
	if _, err := exec.Invoke(context.Background(), p, "s", "x", json.RawMessage(`{}`)); !errors.Is(err, ErrDenied) {
		t.Fatalf("changed schema approval = %v", err)
	}
	if transport.calls != 1 {
		t.Fatal("changed schema reached transport")
	}
}
