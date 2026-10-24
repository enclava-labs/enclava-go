package mcp

import (
	"context"
	"encoding/json"
	"io"
	"net"
	"net/http"
	"net/http/httptest"
	"strconv"
	"strings"
	"testing"
)

func TestStreamableHTTPCallChecksApprovedSchemaAndUsesSession(t *testing.T) {
	in := json.RawMessage(`{"type":"object","properties":{"q":{"type":"string"}},"required":["q"],"additionalProperties":false}`)
	out := json.RawMessage(`{"type":"object","properties":{"ok":{"type":"boolean"}},"required":["ok"],"additionalProperties":false}`)
	var methods []string
	contentOnly := false
	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		if r.URL.Path != "/mcp" {
			t.Errorf("endpoint path = %q", r.URL.Path)
		}
		if r.Method == http.MethodDelete {
			w.WriteHeader(http.StatusAccepted)
			return
		}
		if r.Header.Get("Mcp-Session-Id") != "" && r.Header.Get("Mcp-Session-Id") != "sid" {
			t.Errorf("unexpected session header")
		}
		b, _ := io.ReadAll(r.Body)
		var req struct {
			ID     int64  `json:"id"`
			Method string `json:"method"`
			Params struct {
				Name string `json:"name"`
			} `json:"params"`
		}
		_ = json.Unmarshal(b, &req)
		methods = append(methods, req.Method)
		if req.Method == "initialize" {
			w.Header().Set("Mcp-Session-Id", "sid")
			w.Header().Set("Content-Type", "application/json")
			_, _ = w.Write([]byte(`{"jsonrpc":"2.0","id":` + json.Number(stringInt(req.ID)).String() + `,"result":{"protocolVersion":"2025-06-18"}}`))
			return
		}
		if req.Method == "notifications/initialized" {
			w.WriteHeader(http.StatusAccepted)
			return
		}
		w.Header().Set("Content-Type", "application/json")
		switch req.Method {
		case "tools/list":
			outputSchema := ""
			if len(out) > 0 {
				outputSchema = `,"outputSchema":` + string(out)
			}
			_, _ = w.Write([]byte(`{"jsonrpc":"2.0","id":` + stringInt(req.ID) + `,"result":{"tools":[{"name":"remote","inputSchema":` + string(in) + outputSchema + `,"annotations":{"readOnlyHint":true}}]}}`))
		case "tools/call":
			if req.Params.Name != "remote" {
				t.Errorf("called name=%q", req.Params.Name)
			}
			if contentOnly {
				_, _ = w.Write([]byte(`{"jsonrpc":"2.0","id":` + stringInt(req.ID) + `,"result":{"content":[{"type":"text","text":"ok"}]}}`))
			} else {
				_, _ = w.Write([]byte(`{"jsonrpc":"2.0","id":` + stringInt(req.ID) + `,"result":{"structuredContent":{"ok":true}}}`))
			}
		case http.MethodDelete:
			w.WriteHeader(http.StatusAccepted)
		default:
			t.Errorf("unexpected MCP method %q", req.Method)
		}
	}))
	defer srv.Close()
	a := Approval{RemoteName: "remote", InputSchema: in, OutputSchema: out, InputSchemaHash: SchemaHash(in), OutputSchemaHash: SchemaHash(out)}
	got, err := (&StreamableHTTP{}).Call(context.Background(), Server{TenantID: "t", Origin: srv.URL, EndpointPath: "/mcp", AuthorizedPrivate: true}, a, "remote", json.RawMessage(`{"q":"x"}`), 1024)
	if err != nil {
		t.Fatal(err)
	}
	if string(got) != `{"ok":true}` {
		t.Fatalf("output=%s", got)
	}
	if strings.Join(methods, ",") != "initialize,notifications/initialized,tools/list,tools/call" {
		t.Fatalf("methods=%v", methods)
	}
	methods = nil
	discovered, err := (&StreamableHTTP{}).Discover(context.Background(), Server{TenantID: "t", Origin: srv.URL, EndpointPath: "/mcp", AuthorizedPrivate: true, Enabled: true})
	if err != nil || len(discovered) != 1 || discovered[0].Name != "remote" {
		t.Fatalf("discovery=%v err=%v", discovered, err)
	}
	if strings.Join(methods, ",") != "initialize,notifications/initialized,tools/list" {
		t.Fatalf("discovery methods=%v", methods)
	}
	out = json.RawMessage(`{}`)
	contentOnly = true
	a.OutputSchema, a.OutputSchemaHash = out, SchemaHash(out)
	got, err = (&StreamableHTTP{}).Call(context.Background(), Server{TenantID: "t", Origin: srv.URL, EndpointPath: "/mcp", AuthorizedPrivate: true}, a, "remote", json.RawMessage(`{"q":"x"}`), 1024)
	if err != nil || !strings.HasPrefix(string(got), `[`) {
		t.Fatalf("content-only result with omitted schema = %s err=%v", got, err)
	}
}
func TestStreamableHTTPDoesNotFollowRedirect(t *testing.T) {
	var targetCalls int
	target := httptest.NewServer(http.HandlerFunc(func(http.ResponseWriter, *http.Request) { targetCalls++ }))
	defer target.Close()
	source := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		http.Redirect(w, r, target.URL, http.StatusTemporaryRedirect)
	}))
	defer source.Close()
	_, err := (&StreamableHTTP{}).Call(context.Background(), Server{TenantID: "t", Origin: source.URL, AuthorizedPrivate: true}, Approval{RemoteName: "x"}, "x", json.RawMessage(`{}`), 32)
	if err == nil {
		t.Fatal("redirect was followed")
	}
	if targetCalls != 0 {
		t.Fatalf("redirect target received %d requests", targetCalls)
	}
}

func TestInitializedNotificationOmitsID(t *testing.T) {
	body, err := json.Marshal(rpcRequest{JSONRPC: "2.0", Method: "notifications/initialized"})
	if err != nil {
		t.Fatal(err)
	}
	if strings.Contains(string(body), `"id"`) {
		t.Fatalf("notification contains request ID: %s", body)
	}
}

func TestDialFilterRequiresPrivateOptIn(t *testing.T) {
	for _, raw := range []string{"10.0.0.1", "172.16.0.1", "192.168.1.1", "fc00::1", "127.0.0.1", "::1"} {
		if allowedDialIP(net.ParseIP(raw), false) {
			t.Errorf("private address %s passed without opt-in", raw)
		}
		if !allowedDialIP(net.ParseIP(raw), true) {
			t.Errorf("private address %s failed with opt-in", raw)
		}
	}
}
func stringInt(v int64) string { return strconv.FormatInt(v, 10) }
