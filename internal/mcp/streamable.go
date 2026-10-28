package mcp

import (
	"bytes"
	"context"
	"encoding/json"
	"errors"
	"fmt"
	"io"
	"net"
	"net/http"
	"net/url"
	"strings"
	"sync"
	"sync/atomic"
	"time"
)

const maxDiscoveryTools = 128
const maxProtocolBody = 2 << 20
const maxCallDuration = 20 * time.Second
const maxConcurrentCalls = 16

var ErrOverloaded = errors.New("mcp: call capacity exceeded")

type StreamableHTTP struct {
	Credential                CredentialResolver
	ClientName, ClientVersion string
	next                      atomic.Int64
	once                      sync.Once
	slots                     chan struct{}
}
type rpcRequest struct {
	JSONRPC string `json:"jsonrpc"`
	ID      int64  `json:"id,omitempty"`
	Method  string `json:"method"`
	Params  any    `json:"params,omitempty"`
}
type rpcError struct {
	Code    int    `json:"code"`
	Message string `json:"message"`
}
type rpcResponse struct {
	JSONRPC string          `json:"jsonrpc"`
	ID      int64           `json:"id"`
	Result  json.RawMessage `json:"result"`
	Error   *rpcError       `json:"error"`
}

func validRPC(body []byte, id int64) error {
	var response rpcResponse
	if json.Unmarshal(body, &response) != nil || response.JSONRPC != "2.0" || response.ID != id || response.Error != nil || len(response.Result) == 0 {
		return ErrDenied
	}
	return nil
}

func (t *StreamableHTTP) Call(ctx context.Context, s Server, a Approval, remote string, args json.RawMessage, maxBytes int64) (json.RawMessage, error) {
	if t == nil || maxBytes < 1 || maxBytes > MaxToolOutputBytes || remote == "" || remote != a.RemoteName {
		return nil, ErrDenied
	}
	origin, err := ValidateOrigin(s.Origin, s.AuthorizedPrivate)
	if err != nil {
		return nil, err
	}
	path, err := ValidateEndpointPath(s.EndpointPath)
	if err != nil {
		return nil, err
	}
	u, err := url.Parse(origin + path)
	if err != nil {
		return nil, ErrDenied
	}
	t.once.Do(func() { t.slots = make(chan struct{}, maxConcurrentCalls) })
	select {
	case t.slots <- struct{}{}:
		defer func() { <-t.slots }()
	default:
		return nil, ErrOverloaded
	}
	callCtx, cancel := context.WithTimeout(ctx, maxCallDuration)
	defer cancel()
	client := t.httpClient(s.AuthorizedPrivate)
	cred := ""
	if s.CredentialRef != "" {
		if t.Credential == nil {
			return nil, ErrDenied
		}
		cred, err = t.Credential.Resolve(s.TenantID, "mcp", origin, s.CredentialRef)
		if err != nil {
			return nil, ErrDenied
		}
	}
	clientName, clientVersion := t.ClientName, t.ClientVersion
	if clientName == "" {
		clientName = "enclava"
	}
	if clientVersion == "" {
		clientVersion = "1"
	}
	session := ""
	defer func() {
		if session == "" {
			return
		}
		cleanupCtx, cleanupCancel := context.WithTimeout(context.Background(), 2*time.Second)
		defer cleanupCancel()
		req, err := http.NewRequestWithContext(cleanupCtx, http.MethodDelete, u.String(), nil)
		if err == nil {
			req.Header.Set("Mcp-Session-Id", session)
			if cred != "" {
				req.Header.Set("Authorization", "Bearer "+cred)
			}
			if resp, err := client.Do(req); err == nil {
				_ = resp.Body.Close()
			}
		}
	}()
	id := t.next.Add(1)
	init := rpcRequest{JSONRPC: "2.0", ID: id, Method: "initialize", Params: map[string]any{"protocolVersion": "2025-06-18", "capabilities": map[string]any{}, "clientInfo": map[string]string{"name": clientName, "version": clientVersion}}}
	resp, err := t.post(callCtx, client, u, cred, session, init, maxProtocolBody)
	if err != nil {
		return nil, err
	}
	session = resp.Header.Get("Mcp-Session-Id")
	initBytes, readErr := readBounded(resp.Body, maxProtocolBody)
	_ = resp.Body.Close()
	if readErr != nil {
		return nil, readErr
	}
	var initResult struct {
		Result struct {
			ProtocolVersion string `json:"protocolVersion"`
		} `json:"result"`
		Error *rpcError `json:"error"`
	}
	if err = validRPC(initBytes, id); err != nil || json.Unmarshal(initBytes, &initResult) != nil || initResult.Error != nil || initResult.Result.ProtocolVersion == "" {
		return nil, ErrDenied
	}
	if err = t.postNotify(callCtx, client, u, cred, session, "notifications/initialized", nil); err != nil {
		return nil, err
	}
	listID := t.next.Add(1)
	list, err := t.post(callCtx, client, u, cred, session, rpcRequest{JSONRPC: "2.0", ID: listID, Method: "tools/list", Params: map[string]any{"limit": maxDiscoveryTools}}, maxProtocolBody)
	if err != nil {
		return nil, err
	}
	listBytes, err := readBounded(list.Body, maxProtocolBody)
	_ = list.Body.Close()
	if err != nil {
		return nil, err
	}
	if h := list.Header.Get("Mcp-Session-Id"); session != "" && h != "" && h != session {
		return nil, ErrDenied
	}
	if session == "" {
		session = list.Header.Get("Mcp-Session-Id")
	}
	var lr struct {
		Result struct {
			Tools []struct {
				Name         string          `json:"name"`
				InputSchema  json.RawMessage `json:"inputSchema"`
				OutputSchema json.RawMessage `json:"outputSchema"`
				Annotations  json.RawMessage `json:"annotations"`
			} `json:"tools"`
			NextCursor string `json:"nextCursor"`
		} `json:"result"`
		Error *rpcError `json:"error"`
	}
	if err = validRPC(listBytes, listID); err != nil || json.Unmarshal(listBytes, &lr) != nil || lr.Error != nil || lr.Result.NextCursor != "" || len(lr.Result.Tools) > maxDiscoveryTools {
		return nil, ErrDenied
	}
	found := false
	for _, tool := range lr.Result.Tools {
		outputSchema := tool.OutputSchema
		if len(outputSchema) == 0 {
			outputSchema = json.RawMessage(`{}`)
		}
		if tool.Name == remote && SchemaDigest(tool.InputSchema) == a.InputSchemaHash && SchemaDigest(outputSchema) == a.OutputSchemaHash {
			found = true
			break
		}
	}
	if !found {
		return nil, ErrDenied
	}
	callID := t.next.Add(1)
	call := rpcRequest{JSONRPC: "2.0", ID: callID, Method: "tools/call", Params: map[string]any{"name": remote, "arguments": json.RawMessage(args)}}
	out, err := t.post(callCtx, client, u, cred, session, call, int(maxBytes)+4096)
	if err != nil {
		return nil, err
	}
	body, readErr := readBounded(out.Body, int(maxBytes)+4096)
	_ = out.Body.Close()
	if readErr != nil {
		return nil, readErr
	}
	var result struct {
		Result struct {
			Content    []json.RawMessage `json:"content"`
			Structured json.RawMessage   `json:"structuredContent"`
			IsError    bool              `json:"isError"`
		} `json:"result"`
		Error *rpcError `json:"error"`
	}
	if validRPC(body, callID) != nil || json.Unmarshal(body, &result) != nil || result.Error != nil || result.Result.IsError {
		return nil, ErrDenied
	}
	var payload []byte
	if len(result.Result.Structured) != 0 {
		payload = result.Result.Structured
	} else {
		payload, err = json.Marshal(result.Result.Content)
	}
	if err != nil || len(payload) > int(maxBytes) {
		return nil, ErrDenied
	}
	// An omitted schema approves any JSON shape, including the content array.
	if len(a.OutputSchema) != 0 && string(a.OutputSchema) != `{}` {
		compiled, compileErr := CompileSchema(a.OutputSchema)
		if compileErr != nil || ValidateJSON(compiled, payload) != nil {
			return nil, ErrDenied
		}
	}
	return payload, nil
}

// ToolAnnotations carries only the MCP spec's display hints. They are
// untrusted UI metadata surfaced verbatim — never authorization evidence.
type ToolAnnotations struct {
	Title           string `json:"title,omitempty"`
	ReadOnlyHint    *bool  `json:"readOnlyHint,omitempty"`
	DestructiveHint *bool  `json:"destructiveHint,omitempty"`
	IdempotentHint  *bool  `json:"idempotentHint,omitempty"`
	OpenWorldHint   *bool  `json:"openWorldHint,omitempty"`
}

type DiscoveredTool struct {
	Name         string          `json:"name"`
	Title        string          `json:"title,omitempty"`
	Description  string          `json:"description,omitempty"`
	InputSchema  json.RawMessage `json:"inputSchema"`
	OutputSchema json.RawMessage `json:"outputSchema"`
	Annotations  ToolAnnotations `json:"annotations"`
}

// Discover lists a bounded snapshot without approving or enabling any tool.
func (t *StreamableHTTP) Discover(ctx context.Context, s Server) ([]DiscoveredTool, error) {
	if t == nil || !s.Enabled {
		return nil, ErrDenied
	}
	origin, err := ValidateOrigin(s.Origin, s.AuthorizedPrivate)
	if err != nil {
		return nil, err
	}
	path, err := ValidateEndpointPath(s.EndpointPath)
	if err != nil {
		return nil, err
	}
	u, err := url.Parse(origin + path)
	if err != nil {
		return nil, ErrDenied
	}
	t.once.Do(func() { t.slots = make(chan struct{}, maxConcurrentCalls) })
	select {
	case t.slots <- struct{}{}:
		defer func() { <-t.slots }()
	default:
		return nil, ErrOverloaded
	}
	client := t.httpClient(s.AuthorizedPrivate)
	cred := ""
	if s.CredentialRef != "" {
		if t.Credential == nil {
			return nil, ErrDenied
		}
		cred, err = t.Credential.Resolve(s.TenantID, "mcp", origin, s.CredentialRef)
		if err != nil {
			return nil, ErrDenied
		}
	}
	callCtx, cancel := context.WithTimeout(ctx, maxCallDuration)
	defer cancel()
	id := t.next.Add(1)
	init, err := t.post(callCtx, client, u, cred, "", rpcRequest{
		JSONRPC: "2.0", ID: id, Method: "initialize",
		Params: map[string]any{"protocolVersion": "2025-06-18", "capabilities": map[string]any{}, "clientInfo": map[string]string{"name": "enclava", "version": "1"}},
	}, maxProtocolBody)
	if err != nil {
		return nil, err
	}
	session := init.Header.Get("Mcp-Session-Id")
	defer func() {
		if session == "" {
			return
		}
		cleanupCtx, cleanupCancel := context.WithTimeout(context.Background(), 2*time.Second)
		defer cleanupCancel()
		req, err := http.NewRequestWithContext(cleanupCtx, http.MethodDelete, u.String(), nil)
		if err == nil {
			req.Header.Set("Mcp-Session-Id", session)
			if cred != "" {
				req.Header.Set("Authorization", "Bearer "+cred)
			}
			if resp, err := client.Do(req); err == nil {
				_ = resp.Body.Close()
			}
		}
	}()
	initBody, readErr := readBounded(init.Body, maxProtocolBody)
	_ = init.Body.Close()
	if readErr != nil || validRPC(initBody, id) != nil {
		return nil, ErrDenied
	}
	var initialized struct {
		Result struct {
			ProtocolVersion string `json:"protocolVersion"`
		} `json:"result"`
	}
	if json.Unmarshal(initBody, &initialized) != nil || initialized.Result.ProtocolVersion == "" {
		return nil, ErrDenied
	}
	if err = t.postNotify(callCtx, client, u, cred, session, "notifications/initialized", nil); err != nil {
		return nil, err
	}
	listID := t.next.Add(1)
	list, err := t.post(callCtx, client, u, cred, session, rpcRequest{JSONRPC: "2.0", ID: listID, Method: "tools/list", Params: map[string]any{"limit": maxDiscoveryTools}}, maxProtocolBody)
	if err != nil {
		return nil, err
	}
	defer list.Body.Close()
	if session != "" && list.Header.Get("Mcp-Session-Id") != "" && list.Header.Get("Mcp-Session-Id") != session {
		return nil, ErrDenied
	}
	body, err := readBounded(list.Body, maxProtocolBody)
	if err != nil || validRPC(body, listID) != nil {
		return nil, ErrDenied
	}
	var response struct {
		Result struct {
			Tools      []DiscoveredTool `json:"tools"`
			NextCursor string           `json:"nextCursor"`
		} `json:"result"`
	}
	if json.Unmarshal(body, &response) != nil || response.Result.NextCursor != "" || len(response.Result.Tools) > maxDiscoveryTools {
		return nil, ErrDenied
	}
	seen := make(map[string]struct{}, len(response.Result.Tools))
	for i := range response.Result.Tools {
		tool := &response.Result.Tools[i]
		if tool.Name == "" || !json.Valid(tool.InputSchema) {
			return nil, ErrDenied
		}
		// Descriptive metadata is bounded display text, not free storage.
		if len(tool.Title) > 1024 || len(tool.Description) > 4096 || len(tool.Annotations.Title) > 1024 {
			return nil, ErrDenied
		}
		if len(tool.OutputSchema) == 0 {
			tool.OutputSchema = json.RawMessage(`{}`)
		}
		if !json.Valid(tool.OutputSchema) {
			return nil, ErrDenied
		}
		if _, ok := seen[tool.Name]; ok {
			return nil, ErrDenied
		}
		seen[tool.Name] = struct{}{}
		if _, err := CompileSchema(tool.InputSchema); err != nil {
			return nil, ErrDenied
		}
		if _, err := CompileSchema(tool.OutputSchema); err != nil {
			return nil, ErrDenied
		}
	}
	return response.Result.Tools, nil
}

func (t *StreamableHTTP) postNotify(ctx context.Context, c *http.Client, u *url.URL, cred, session, method string, params any) error {
	req := rpcRequest{JSONRPC: "2.0", Method: method, Params: params}
	_, err := t.post(ctx, c, u, cred, session, req, maxProtocolBody)
	return err
}
func (t *StreamableHTTP) post(ctx context.Context, c *http.Client, u *url.URL, cred, session string, rpc rpcRequest, max int) (*http.Response, error) {
	body, err := json.Marshal(rpc)
	if err != nil {
		return nil, ErrDenied
	}
	req, err := http.NewRequestWithContext(ctx, http.MethodPost, u.String(), bytes.NewReader(body))
	if err != nil {
		return nil, ErrDenied
	}
	req.Header.Set("Content-Type", "application/json")
	req.Header.Set("Accept", "application/json, text/event-stream")
	if cred != "" {
		req.Header.Set("Authorization", "Bearer "+cred)
	}
	if session != "" {
		req.Header.Set("Mcp-Session-Id", session)
	}
	resp, err := c.Do(req)
	if err != nil {
		return nil, fmt.Errorf("mcp: remote outcome unknown: %w", err)
	}
	if resp.StatusCode < 200 || resp.StatusCode >= 300 {
		_ = resp.Body.Close()
		return nil, fmt.Errorf("mcp: HTTP status %s", resp.Status)
	}
	if resp.ContentLength > int64(max) {
		_ = resp.Body.Close()
		return nil, ErrDenied
	}
	contentType := strings.ToLower(resp.Header.Get("Content-Type"))
	isSSE := strings.HasPrefix(contentType, "text/event-stream")
	data, err := readBounded(resp.Body, max)
	_ = resp.Body.Close()
	if err != nil {
		return nil, err
	}
	if isSSE && rpc.ID != 0 {
		data, err = sseResponse(data, rpc.ID)
		if err != nil {
			return nil, err
		}
	}
	resp.Body = io.NopCloser(bytes.NewReader(data))
	return resp, nil
}
func sseResponse(body []byte, id int64) ([]byte, error) {
	lines := strings.Split(string(body), "\n")
	var data []string
	events := 0
	for _, line := range lines {
		line = strings.TrimSuffix(line, "\r")
		if line == "" {
			if len(data) != 0 {
				events++
				if events > 256 {
					return nil, ErrDenied
				}
				candidate := []byte(strings.Join(data, "\n"))
				if validRPC(candidate, id) == nil {
					return candidate, nil
				}
				data = nil
			}
			continue
		}
		if strings.HasPrefix(line, "data:") {
			data = append(data, strings.TrimPrefix(strings.TrimPrefix(line, "data:"), " "))
		}
	}
	if len(data) != 0 {
		candidate := []byte(strings.Join(data, "\n"))
		if validRPC(candidate, id) == nil {
			return candidate, nil
		}
	}
	return nil, ErrDenied
}
func readBounded(r io.Reader, max int) ([]byte, error) {
	b, err := io.ReadAll(io.LimitReader(r, int64(max)+1))
	if err != nil {
		return nil, err
	}
	if len(b) > max {
		return nil, ErrDenied
	}
	return b, nil
}
func (t *StreamableHTTP) httpClient(allowPrivate bool) *http.Client {
	return &http.Client{Timeout: maxCallDuration, CheckRedirect: func(*http.Request, []*http.Request) error { return http.ErrUseLastResponse }, Transport: &http.Transport{Proxy: nil, DisableKeepAlives: true, DialContext: func(ctx context.Context, network, address string) (net.Conn, error) {
		host, port, err := net.SplitHostPort(address)
		if err != nil {
			return nil, err
		}
		ips, err := net.DefaultResolver.LookupIPAddr(ctx, host)
		if err != nil {
			return nil, err
		}
		for _, ip := range ips {
			if !allowedDialIP(ip.IP, allowPrivate) {
				continue
			}
			d := net.Dialer{Timeout: 5 * time.Second}
			return d.DialContext(ctx, network, net.JoinHostPort(ip.IP.String(), port))
		}
		return nil, errors.New("mcp: destination address denied")
	}}}
}
func allowedDialIP(ip net.IP, allowPrivate bool) bool {
	return allowPrivate && (ip.IsPrivate() || ip.IsLoopback()) || ip.IsGlobalUnicast() && !ip.IsPrivate() && !ip.IsLoopback() && !ip.IsLinkLocalUnicast() && !ip.IsLinkLocalMulticast()
}
