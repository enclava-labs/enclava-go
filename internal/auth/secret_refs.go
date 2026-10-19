// Operator-configured, tenant-bound credential references (AUTH-05, INF-01,
// INF-02). A credential_ref on an endpoint, connection or MCP server is an
// opaque identifier; it becomes a secret only through a binding the operator
// declared in trusted runtime configuration (Config.SecretBindings,
// parsed from SECRET_BINDINGS). The resolver never interprets a stored ref
// as an environment-variable name, never reuses another tenant's binding and
// never inherits an unrelated SDK/environment default key. There is no
// second secret backend or credential-provisioning product — the process
// environment remains the only secret source.
package auth

import (
	"errors"
	"fmt"
	"net/url"
	"os"
	"regexp"
	"sort"
	"strings"
)

// ErrSecretDenied is the public outcome for every refused resolution —
// unknown refs, cross-tenant/purpose/origin access and malformed bindings all
// surface the same error so the failure carries no distinguishing detail and
// never embeds a secret value.
var ErrSecretDenied = errors.New("auth: secret resolution denied")

// Integration purposes a binding may be scoped to. The list is deliberately
// small and closed so a typo in operator configuration fails at Load rather
// than silently denying at dispatch.
const (
	SecretPurposeInference  = "inference"
	SecretPurposeConnector  = "connector"
	SecretPurposeMCP        = "mcp"
	SecretPurposeEmbeddings = "embeddings"
)

// SecretBinding is one operator-approved binding: an opaque reference ID that
// may be resolved only for the exact tenant, integration purpose and
// canonical origins listed. EnvName is trusted configuration — it never comes
// from a stored credential_ref or a request.
type SecretBinding struct {
	RefID    string   // opaque identifier referenced by endpoint/connection records
	TenantID string   // exact tenant the binding belongs to
	Purpose  string   // one of the SecretPurpose* constants
	EnvName  string   // operator-owned environment variable supplying the value
	Origins  []string // canonical scheme://host[:port] origins approved for use
}

var envNamePattern = regexp.MustCompile(`^[A-Za-z_][A-Za-z0-9_]*$`)

// CanonicalizeSecretOrigin reduces an origin URL to canonical
// scheme://host[:port] form: only http/https, lowercased scheme and host,
// explicit port only when non-default. Anything else — credentials embedded
// in the URL, path/query/fragment, scheme-relative or host-only input — is
// rejected so a bound origin can never be smuggled into a different scheme,
// port or redirect destination.
func CanonicalizeSecretOrigin(raw string) (string, error) {
	u, err := url.Parse(strings.TrimSpace(raw))
	if err != nil {
		return "", fmt.Errorf("%w: malformed origin", ErrSecretDenied)
	}
	scheme := strings.ToLower(u.Scheme)
	if scheme != "http" && scheme != "https" {
		return "", fmt.Errorf("%w: malformed origin", ErrSecretDenied)
	}
	host := u.Hostname()
	if host == "" || u.User != nil || u.RawQuery != "" || u.Fragment != "" ||
		(u.Path != "" && u.Path != "/") {
		return "", fmt.Errorf("%w: malformed origin", ErrSecretDenied)
	}
	port := u.Port()
	if (scheme == "https" && port == "443") || (scheme == "http" && port == "80") {
		port = ""
	}
	if port != "" {
		return scheme + "://" + strings.ToLower(host) + ":" + port, nil
	}
	return scheme + "://" + strings.ToLower(host), nil
}

// SecretRefMeta is the only information safe to return to configuration UIs:
// the binding's identity, never the env name, origins or any secret value.
type SecretRefMeta struct {
	RefID    string `json:"ref_id"`
	TenantID string `json:"tenant_id"`
	Purpose  string `json:"purpose"`
}

// SecretResolver resolves opaque credential references against the
// operator-approved bindings. All matching happens before any environment
// read, so an unbound ref can never reach os.LookupEnv.
type SecretResolver struct {
	byKey map[string]SecretBinding // tenant + purpose + refID
	order []SecretRefMeta          // deterministic metadata listing
}

// NewSecretResolver validates the bindings and fails closed on duplicates,
// malformed origins, unknown purposes or unusable environment names.
// Validation never reads the environment itself — missing values are reported
// at Resolve time as ErrSecretDenied.
func NewSecretResolver(bindings []SecretBinding) (*SecretResolver, error) {
	r := &SecretResolver{byKey: make(map[string]SecretBinding, len(bindings))}
	for _, b := range bindings {
		b.RefID = strings.TrimSpace(b.RefID)
		b.TenantID = strings.TrimSpace(b.TenantID)
		b.Purpose = strings.TrimSpace(b.Purpose)
		b.EnvName = strings.TrimSpace(b.EnvName)
		if b.RefID == "" || b.TenantID == "" {
			return nil, fmt.Errorf("%w: secret binding requires a reference id and tenant", ErrSecretDenied)
		}
		switch b.Purpose {
		case SecretPurposeInference, SecretPurposeConnector, SecretPurposeMCP, SecretPurposeEmbeddings:
		default:
			return nil, fmt.Errorf("%w: unknown secret binding purpose", ErrSecretDenied)
		}
		if !envNamePattern.MatchString(b.EnvName) {
			return nil, fmt.Errorf("%w: unusable secret binding source", ErrSecretDenied)
		}
		if len(b.Origins) == 0 {
			return nil, fmt.Errorf("%w: secret binding requires at least one approved origin", ErrSecretDenied)
		}
		seen := make(map[string]struct{}, len(b.Origins))
		origins := make([]string, 0, len(b.Origins))
		for _, raw := range b.Origins {
			origin, err := CanonicalizeSecretOrigin(raw)
			if err != nil {
				return nil, err
			}
			if _, dup := seen[origin]; dup {
				return nil, fmt.Errorf("%w: duplicate approved origin", ErrSecretDenied)
			}
			seen[origin] = struct{}{}
			origins = append(origins, origin)
		}
		b.Origins = origins
		key := bindingKey(b.TenantID, b.Purpose, b.RefID)
		if _, dup := r.byKey[key]; dup {
			return nil, fmt.Errorf("%w: duplicate secret binding", ErrSecretDenied)
		}
		r.byKey[key] = b
	}
	for _, b := range r.byKey {
		r.order = append(r.order, SecretRefMeta{RefID: b.RefID, TenantID: b.TenantID, Purpose: b.Purpose})
	}
	sort.Slice(r.order, func(i, j int) bool {
		if r.order[i].TenantID != r.order[j].TenantID {
			return r.order[i].TenantID < r.order[j].TenantID
		}
		if r.order[i].Purpose != r.order[j].Purpose {
			return r.order[i].Purpose < r.order[j].Purpose
		}
		return r.order[i].RefID < r.order[j].RefID
	})
	return r, nil
}

func bindingKey(tenantID, purpose, refID string) string {
	return tenantID + "\x00" + purpose + "\x00" + refID
}

// Resolve returns the credential value for (tenant, purpose, canonical or
// raw origin, ref). Every binding check runs before the environment is read;
// any mismatch — other tenant, other purpose, unapproved or malformed origin,
// unknown ref — is ErrSecretDenied. An endpoint explicitly configured without
// a credential must pass refID == "" and gets an empty value, never a global
// SDK/environment default.
func (r *SecretResolver) Resolve(tenantID, purpose, origin, refID string) (string, error) {
	if refID == "" {
		return "", nil
	}
	if r == nil {
		return "", ErrSecretDenied
	}
	canonical, err := CanonicalizeSecretOrigin(origin)
	if err != nil {
		return "", err
	}
	b, ok := r.byKey[bindingKey(tenantID, purpose, refID)]
	if !ok {
		return "", ErrSecretDenied
	}
	approved := false
	for _, o := range b.Origins {
		if o == canonical {
			approved = true
			break
		}
	}
	if !approved {
		return "", ErrSecretDenied
	}
	value, ok := os.LookupEnv(b.EnvName)
	if !ok || value == "" {
		return "", fmt.Errorf("%w: credential unavailable", ErrSecretDenied)
	}
	return value, nil
}

// Metadata returns safe binding metadata for configuration surfaces — it
// never contains secret values or environment-variable names.
func (r *SecretResolver) Metadata(tenantID string) []SecretRefMeta {
	if r == nil {
		return nil
	}
	var out []SecretRefMeta
	for _, m := range r.order {
		if m.TenantID == tenantID {
			out = append(out, m)
		}
	}
	return out
}
