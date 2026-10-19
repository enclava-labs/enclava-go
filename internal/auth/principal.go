package auth

import (
	"context"
	"encoding/json"
)

// ActorKind identifies what kind of tenant actor a Principal authenticates as.
type ActorKind string

const (
	ActorKindUser           ActorKind = "user"
	ActorKindServiceAccount ActorKind = "service_account"
)

// CredentialKind identifies the credential a Principal was authenticated with.
type CredentialKind string

const (
	CredentialKindSession CredentialKind = "session"
	CredentialKindAPIKey  CredentialKind = "api_key"
)

// Billing subject kinds for Principal.BillingSubjectKind.
const (
	BillingSubjectUser           = "user"
	BillingSubjectServiceAccount = "service_account"
	BillingSubjectAPIKey         = "api_key"
)

// Principal is the canonical validated identity carried to every service
// operation (AUTH-04). There is no IsAdmin and no ID-zero authority path;
// platform scope comes from Scopes ("platform.admin").
//
// Allowlist decode contract (documented here, enforced by the repository
// decoder in 01-08): each allowlist column stores {"mode":"all"} for an
// explicit server-set unrestricted grant (→ the matching *All bool is true,
// map empty) or {"mode":"list","items":[...]} (→ map populated, *All false).
// Absent or malformed JSON decodes as deny-everything (*All false + empty
// map) — never silently unrestricted. Helpers consult the All flag; an empty
// map alone never means unrestricted.
type Principal struct {
	TenantID       string
	ActorID        string
	ActorKind      ActorKind
	UserID         string
	CredentialID   string
	CredentialKind CredentialKind
	SessionID      string

	Scopes                  map[string]struct{}
	AllowedModels           map[string]struct{}
	AllowedExtractTemplates map[string]struct{}
	AllowedChatbots         map[string]struct{}
	AllowedAgents           map[string]struct{}
	AllowedEndpoints        map[string]struct{} // inference endpoint IDs, never URL paths
	AllowedIPs              map[string]struct{}

	// mode:"all" grants — set only from a stored {"mode":"all"} row.
	AllowedModelsAll           bool
	AllowedExtractTemplatesAll bool
	AllowedChatbotsAll         bool
	AllowedAgentsAll           bool
	AllowedEndpointsAll        bool
	AllowedIPsAll              bool

	// Keep *int through every retained producer; 01-08 widens atomically.
	RateLimitPerMinute *int
	RateLimitPerHour   *int
	RateLimitPerDay    *int

	DelegatedByActorID string
	BillingSubjectID   string
	BillingSubjectKind string

	// Legacy fields retained until 01-35 so existing callers keep compiling;
	// populated from stored rows. Deleted in one sweep — do not add new uses.
	APIKeyID          int64
	Name              string
	IsUnlimited       bool
	BudgetLimitTokens *int64
	BudgetLimitCents  *int64
	BudgetPeriod      string
	RawAPIKeyPrefix   string
}

type contextKey string

const principalContextKey contextKey = "principal"

func WithPrincipal(ctx context.Context, principal Principal) context.Context {
	return context.WithValue(ctx, principalContextKey, principal)
}

func PrincipalFromContext(ctx context.Context) (Principal, bool) {
	p, ok := ctx.Value(principalContextKey).(Principal)
	return p, ok
}

// DecodeAllowlist decodes a stored allowlist grant into its All flag and item
// map. {"mode":"all"} → all=true, empty map. {"mode":"list","items":[...]} →
// all=false, populated map. Absent or malformed input → all=false, empty
// map: deny-everything, never silently unrestricted.
func DecodeAllowlist(raw json.RawMessage) (items map[string]struct{}, all bool) {
	items = map[string]struct{}{}
	if len(raw) == 0 {
		return items, false
	}
	var grant struct {
		Mode  string   `json:"mode"`
		Items []string `json:"items"`
	}
	if err := json.Unmarshal(raw, &grant); err != nil {
		return items, false
	}
	switch grant.Mode {
	case "all":
		return items, true
	case "list":
		for _, item := range grant.Items {
			items[item] = struct{}{}
		}
	}
	return items, false
}

// allowlistSet maps a legacy stored string list into the canonical all/list
// shape, preserving the old storage contract (no column → unrestricted;
// a "*" item → unrestricted) until 01-08 switches storage to mode rows.
func allowlistSet(values []string) (map[string]struct{}, bool) {
	if len(values) == 0 {
		return map[string]struct{}{}, true
	}
	set := make(map[string]struct{}, len(values))
	for _, v := range values {
		set[v] = struct{}{}
	}
	if _, ok := set["*"]; ok {
		return map[string]struct{}{}, true
	}
	return set, false
}

func allowlistAllowed(all bool, items map[string]struct{}, ids ...string) bool {
	if all {
		return true
	}
	// "*" inside a list still means unrestricted — legacy stored rows carry
	// it until 01-08's mode-row cutover. A plain empty map denies.
	if _, ok := items["*"]; ok {
		return true
	}
	for _, id := range ids {
		if _, ok := items[id]; ok {
			return true
		}
	}
	return false
}

func HasScope(p Principal, scope string) bool {
	if _, ok := p.Scopes["*"]; ok {
		return true
	}
	_, ok := p.Scopes[scope]
	return ok
}

func ModelAllowed(p Principal, model string) bool {
	return allowlistAllowed(p.AllowedModelsAll, p.AllowedModels, model)
}

func ExtractTemplateAllowed(p Principal, templateID string) bool {
	return allowlistAllowed(p.AllowedExtractTemplatesAll, p.AllowedExtractTemplates, templateID)
}

func ChatbotAllowed(p Principal, chatbotIDs ...string) bool {
	return allowlistAllowed(p.AllowedChatbotsAll, p.AllowedChatbots, chatbotIDs...)
}

func AgentAllowed(p Principal, agentIDs ...string) bool {
	return allowlistAllowed(p.AllowedAgentsAll, p.AllowedAgents, agentIDs...)
}

// EndpointAllowed checks whether an inference endpoint ID is allowed.
// endpointID is an inference endpoint identifier enforced by the executor —
// never a URL path pattern; HTTP route permission uses Scopes only.
func EndpointAllowed(p Principal, endpointID string) bool {
	return allowlistAllowed(p.AllowedEndpointsAll, p.AllowedEndpoints, endpointID)
}

// IPAllowed checks whether a client source IP is allowed by the stored IP
// allowlist. Plain entries match literally; CIDR-aware matching stays with
// the httpx caller until 01-16.
func IPAllowed(p Principal, ip string) bool {
	return allowlistAllowed(p.AllowedIPsAll, p.AllowedIPs, ip)
}
