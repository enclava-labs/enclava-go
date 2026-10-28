// Package mcp owns approved outbound MCP destinations and the policy checks
// that must succeed before a tool call is sent.
package mcp

import (
	"bytes"
	"context"
	"crypto/sha256"
	"encoding/hex"
	"encoding/json"
	"errors"
	"fmt"
	"net"
	"net/url"
	"sort"
	"strconv"
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
	ListServers(context.Context, string) ([]Server, error)
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

// SchemaDigest hashes a JSON schema canonically: object keys sorted, numbers
// value-normalized, whitespace removed — so the same schema after a jsonb
// round-trip (which may reorder keys or reformat literals) always yields the
// same digest. Input that cannot be canonicalized hashes behind a '!' marker
// so it can never collide with a valid schema's digest.
func SchemaDigest(schema json.RawMessage) string {
	canonical, err := canonicalJSON(schema)
	if err != nil {
		canonical = append([]byte{'!'}, schema...)
	}
	sum := sha256.Sum256(canonical)
	return hex.EncodeToString(sum[:])
}

const maxCanonicalNumberDigits = 1024

// canonicalJSON renders raw JSON with sorted object keys, value-normalized
// numbers and no whitespace.
func canonicalJSON(raw json.RawMessage) ([]byte, error) {
	if !json.Valid(raw) {
		return nil, fmt.Errorf("%w: schema is not valid JSON", ErrDenied)
	}
	dec := json.NewDecoder(bytes.NewReader(raw))
	dec.UseNumber()
	var v any
	if err := dec.Decode(&v); err != nil {
		return nil, err
	}
	var buf bytes.Buffer
	if err := writeCanonicalJSON(&buf, v); err != nil {
		return nil, err
	}
	return buf.Bytes(), nil
}

func writeCanonicalJSON(buf *bytes.Buffer, v any) error {
	switch t := v.(type) {
	case map[string]any:
		keys := make([]string, 0, len(t))
		for k := range t {
			keys = append(keys, k)
		}
		sort.Strings(keys)
		buf.WriteByte('{')
		for i, k := range keys {
			if i > 0 {
				buf.WriteByte(',')
			}
			kb, err := json.Marshal(k)
			if err != nil {
				return err
			}
			buf.Write(kb)
			buf.WriteByte(':')
			if err := writeCanonicalJSON(buf, t[k]); err != nil {
				return err
			}
		}
		buf.WriteByte('}')
	case []any:
		buf.WriteByte('[')
		for i, e := range t {
			if i > 0 {
				buf.WriteByte(',')
			}
			if err := writeCanonicalJSON(buf, e); err != nil {
				return err
			}
		}
		buf.WriteByte(']')
	case string:
		b, err := json.Marshal(t)
		if err != nil {
			return err
		}
		buf.Write(b)
	case json.Number:
		n, err := normalizeJSONNumber(t.String())
		if err != nil {
			return err
		}
		buf.WriteString(n)
	case bool:
		buf.WriteString(strconv.FormatBool(t))
	case nil:
		buf.WriteString("null")
	default:
		return fmt.Errorf("mcp: unsupported JSON value %T", v)
	}
	return nil
}

// normalizeJSONNumber encodes a JSON number literal by its numeric value:
// the zero-trimmed significand digits followed by the non-negative decimal
// exponent. This encoding is injective per value (jsonb numeric equality:
// 1.0 == 1 == 1e0), so value-equal literals always hash equal, and every
// JSON number is digits*10^shift exactly, so the shifting never rounds.
func normalizeJSONNumber(lit string) (string, error) {
	neg := false
	s := lit
	if s != "" && s[0] == '-' {
		neg = true
		s = s[1:]
	}
	mant, shift := s, int64(0)
	if i := strings.IndexAny(s, "eE"); i >= 0 {
		mant = s[:i]
		exp, err := strconv.ParseInt(s[i+1:], 10, 32)
		if err != nil {
			return "", fmt.Errorf("mcp: JSON number exponent out of range")
		}
		shift = exp
	}
	intPart, fracPart := mant, ""
	if i := strings.IndexByte(mant, '.'); i >= 0 {
		intPart, fracPart = mant[:i], mant[i+1:]
	}
	digits := strings.TrimLeft(intPart+fracPart, "0")
	shift -= int64(len(fracPart))
	if digits == "" {
		return "0", nil
	}
	for strings.HasSuffix(digits, "0") {
		digits = digits[:len(digits)-1]
		shift++
	}
	if shift < 0 {
		shift = -shift
	}
	if int64(len(digits))+shift > maxCanonicalNumberDigits {
		return "", fmt.Errorf("mcp: JSON number magnitude out of canonical range")
	}
	var b strings.Builder
	if neg {
		b.WriteByte('-')
	}
	b.WriteString(digits)
	for i := int64(0); i < shift; i++ {
		b.WriteByte('0')
	}
	return b.String(), nil
}

// SchemaFieldDiff is one top-level input-schema property change between the
// previously seen (approved) schema and a rediscovered schema.
type SchemaFieldDiff struct {
	Field  string `json:"field"`
	Change string `json:"change"` // added | removed | type_changed
}

// DiffSchemaFields reports top-level property additions, removals and type
// changes between two input schemas in deterministic field order. Deeper
// structure stays implicit in the stored schemas.
func DiffSchemaFields(oldInput, newInput json.RawMessage) []SchemaFieldDiff {
	oldProps, newProps := schemaPropertyTypes(oldInput), schemaPropertyTypes(newInput)
	diffs := make([]SchemaFieldDiff, 0, len(newProps)+len(oldProps))
	for field, newType := range newProps {
		if oldType, ok := oldProps[field]; !ok {
			diffs = append(diffs, SchemaFieldDiff{Field: field, Change: "added"})
		} else if oldType != newType {
			diffs = append(diffs, SchemaFieldDiff{Field: field, Change: "type_changed"})
		}
	}
	for field := range oldProps {
		if _, ok := newProps[field]; !ok {
			diffs = append(diffs, SchemaFieldDiff{Field: field, Change: "removed"})
		}
	}
	sort.Slice(diffs, func(i, j int) bool { return diffs[i].Field < diffs[j].Field })
	return diffs
}

// schemaPropertyTypes maps each top-level property to the canonical encoding
// of its "type" keyword ("" when absent).
func schemaPropertyTypes(schema json.RawMessage) map[string]string {
	var parsed struct {
		Properties map[string]json.RawMessage `json:"properties"`
	}
	if json.Unmarshal(schema, &parsed) != nil {
		return map[string]string{}
	}
	out := make(map[string]string, len(parsed.Properties))
	for field, raw := range parsed.Properties {
		var prop struct {
			Type json.RawMessage `json:"type"`
		}
		if json.Unmarshal(raw, &prop) == nil && len(prop.Type) != 0 {
			if canonical, err := canonicalJSON(prop.Type); err == nil {
				out[field] = string(canonical)
				continue
			}
		}
		out[field] = ""
	}
	return out
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
