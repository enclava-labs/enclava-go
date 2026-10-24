// Package mcp owns approved outbound MCP destinations and the policy checks
// that must succeed before a tool call is sent.
package mcp

import (
	"context"
	"crypto/sha256"
	"encoding/hex"
	"encoding/json"
	"errors"
	"fmt"
	"net"
	"net/url"
	"strings"
	"time"

	"enclava-go/internal/auth"
)

var ErrDenied = errors.New("mcp: invocation denied")

type Server struct {
	TenantID, ID, Origin, EndpointPath, CredentialRef string
	AuthorizedPrivate, Enabled                        bool
	AudienceScopes                                    []string
	CreatedAt, UpdatedAt                              time.Time
}
type Approval struct {
	TenantID, ServerID, ToolID, ModelName, RemoteName string
	InputSchema, OutputSchema                         json.RawMessage
	InputSchemaHash, OutputSchemaHash                 string
	SchemaVersion, Revision                           int64
	CallerScopes                                      []string
	Enabled                                           bool
	CreatedAt, UpdatedAt                              time.Time
}
type Repository interface {
	GetServer(context.Context, string, string) (Server, error)
	GetApproval(context.Context, string, string, string) (Approval, error)
	ListApprovals(context.Context, string, string) ([]Approval, error)
}
type CredentialResolver interface {
	Resolve(string, string, string, string) (string, error)
}
type RequestAuthorizer func(context.Context, auth.Principal, json.RawMessage) error

// HasScopes applies explicit caller and server audiences; empty lists deny.
func HasScopes(p auth.Principal, serverScopes, callerScopes []string) bool {
	if len(serverScopes) == 0 || len(callerScopes) == 0 {
		return false
	}
	for _, scope := range serverScopes {
		if _, ok := p.Scopes[scope]; !ok {
			return false
		}
	}
	for _, scope := range callerScopes {
		if _, ok := p.Scopes[scope]; !ok {
			return false
		}
	}
	return true
}

func ValidateOrigin(raw string, authorizedPrivate bool) (string, error) {
	u, err := url.Parse(strings.TrimSpace(raw))
	if err != nil || u.Hostname() == "" || u.User != nil || u.RawQuery != "" || u.Fragment != "" || (u.Path != "" && u.Path != "/") {
		return "", fmt.Errorf("%w: invalid origin", ErrDenied)
	}
	scheme, host, port := strings.ToLower(u.Scheme), strings.ToLower(u.Hostname()), u.Port()
	if scheme == "https" && port == "443" || scheme == "http" && port == "80" {
		port = ""
	}
	switch scheme {
	case "https":
	case "http":
		if !authorizedPrivate || (host != "localhost" && host != "127.0.0.1" && host != "::1") {
			return "", fmt.Errorf("%w: insecure origin", ErrDenied)
		}
	default:
		return "", fmt.Errorf("%w: origin scheme", ErrDenied)
	}
	if port != "" {
		host = net.JoinHostPort(host, port)
	} else if strings.Contains(host, ":") {
		host = "[" + host + "]"
	}
	return scheme + "://" + host, nil
}

func ValidateEndpointPath(raw string) (string, error) {
	if raw == "" {
		return "/", nil
	}
	u, err := url.Parse(raw)
	if err != nil || !strings.HasPrefix(u.Path, "/") || u.IsAbs() || u.Host != "" || u.User != nil || u.RawQuery != "" || u.Fragment != "" {
		return "", fmt.Errorf("%w: invalid MCP endpoint path", ErrDenied)
	}
	for _, segment := range strings.Split(u.Path, "/") {
		if segment == ".." {
			return "", fmt.Errorf("%w: invalid MCP endpoint path", ErrDenied)
		}
	}
	return u.EscapedPath(), nil
}

func SchemaHash(schema json.RawMessage) string {
	sum := sha256.Sum256(schema)
	return hex.EncodeToString(sum[:])
}

// ValidateJSON checks a value against its approved, locally compiled schema.
func ValidateJSON(validate func(json.RawMessage) error, value json.RawMessage) error {
	if len(value) == 0 || !json.Valid(value) || validate == nil {
		return fmt.Errorf("%w: malformed JSON", ErrDenied)
	}
	if err := validate(value); err != nil {
		return fmt.Errorf("%w: schema mismatch", ErrDenied)
	}
	return nil
}
