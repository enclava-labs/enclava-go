package mcp

import (
	"context"
	"encoding/json"
	"errors"
	"reflect"
	"testing"

	"enclava-go/internal/auth"
)

// TestSchemaDigestCanonical pins the digest contract every MCP site shares:
// key order, whitespace and numeric formatting must not change the digest,
// because schemas live in jsonb and are re-read reformatted.
func TestSchemaDigestCanonical(t *testing.T) {
	base := json.RawMessage(`{"a":1,"b":[2,{"c":"x"}],"d":{"e":true,"f":null}}`)
	reordered := json.RawMessage("{\n  \"b\": [2, {\"c\": \"x\"}],\n  \"d\": {\"f\": null, \"e\": true},\n  \"a\": 1.0\n}")
	if SchemaDigest(base) != SchemaDigest(reordered) {
		t.Fatal("jsonb-equivalent schemas produced different digests")
	}
	if SchemaDigest(base) == SchemaDigest(json.RawMessage(`{"a":2,"b":[2,{"c":"x"}],"d":{"e":true,"f":null}}`)) {
		t.Fatal("semantically different schemas share a digest")
	}
	if SchemaDigest(base) == SchemaDigest(json.RawMessage(`{"a":1,"b":[2,{"c":"y"}],"d":{"e":true,"f":null}}`)) {
		t.Fatal("nested change shares a digest")
	}
	for _, lit := range []string{`1e2`, `100.00`, `1.0e2`, `100`} {
		if SchemaDigest(json.RawMessage(`{"n":`+lit+`}`)) != SchemaDigest(json.RawMessage(`{"n":100}`)) {
			t.Errorf("number literal %s not value-normalized", lit)
		}
	}
	if SchemaDigest(json.RawMessage(`{"n":0.001}`)) != SchemaDigest(json.RawMessage(`{"n":1e-3}`)) {
		t.Error("fractional exponent normalization broken")
	}
	invalid := json.RawMessage(`{"a":`)
	if SchemaDigest(invalid) == SchemaDigest(json.RawMessage(`{"a":1}`)) {
		t.Error("invalid input collided with a valid schema digest")
	}
	if SchemaDigest(invalid) != SchemaDigest(json.RawMessage(`{"a":`)) {
		t.Error("identical invalid inputs must hash deterministically")
	}
	if len(SchemaDigest(base)) != 64 {
		t.Fatalf("digest length %d", len(SchemaDigest(base)))
	}
}

func TestDiffSchemaFieldsReportsTopLevelChanges(t *testing.T) {
	old := json.RawMessage(`{"type":"object","properties":{"keep":{"type":"string"},"typed":{"type":"number"},"gone":{"type":"string"}},"required":["keep"]}`)
	next := json.RawMessage(`{"type":"object","properties":{"keep":{"type":"string"},"typed":{"type":"string"},"added":{"type":"boolean"}}}`)
	got := DiffSchemaFields(old, next)
	want := []SchemaFieldDiff{{Field: "added", Change: "added"}, {Field: "gone", Change: "removed"}, {Field: "typed", Change: "type_changed"}}
	if !reflect.DeepEqual(got, want) {
		t.Fatalf("diff=%v want=%v", got, want)
	}
	if got := DiffSchemaFields(old, old); len(got) != 0 {
		t.Fatalf("identical schemas diffed: %v", got)
	}
}

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
	a.InputSchemaHash = SchemaDigest(a.InputSchema)
	a.OutputSchemaHash = SchemaDigest(a.OutputSchema)
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
