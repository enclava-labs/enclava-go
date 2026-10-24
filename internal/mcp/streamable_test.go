package mcp

import (
	"context"
	"encoding/json"
	"io"
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
			_, _ = w.Write([]byte(`{"jsonrpc":"2.0","id":` + stringInt(req.ID) + `,"result":{"tools":[{"name":"remote","inputSchema":` + string(in) + `,"outputSchema":` + string(out) + `,"annotations":{"readOnlyHint":true}}]}}`))
		case "tools/call":
			if req.Params.Name != "remote" {
				t.Errorf("called name=%q", req.Params.Name)
			}
			_, _ = w.Write([]byte(`{"jsonrpc":"2.0","id":` + stringInt(req.ID) + `,"result":{"structuredContent":{"ok":true}}}`))
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
func stringInt(v int64) string { return strconv.FormatInt(v, 10) }
