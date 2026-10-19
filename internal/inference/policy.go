// policy.go is the shared authoritative input/destination authorization pass
// (AUTH-04/AUTH-06/INF-06). Every execution — and every later approved
// outbound destination via AuthorizeDestination — resolves immutable input
// references from tenant-owned storage under current source authorization,
// intersects actor/tenant/persisted processing restrictions with endpoint
// capability and assurance, and refuses forged, stale or cross-tenant
// references with the same indistinguishable denial.
package inference

import (
	"context"
	"encoding/json"
	"fmt"
	"math"
	"net/http"
	"strings"

	"enclava-go/internal/artifacts"
	"enclava-go/internal/auth"
	"enclava-go/internal/source"
)

// SourceStore is the narrow persistence seam the policy pass needs from
// source storage. *postgres.SourceRepo satisfies it.
type SourceStore interface {
	GetObject(ctx context.Context, tenantID, id string) (source.Object, error)
	GetObjectVersion(ctx context.Context, tenantID, objectID string, observedSeq int64) (source.ObjectVersion, error)
	LatestObjectVersion(ctx context.Context, tenantID, objectID string) (source.ObjectVersion, error)
	RestrictionsForSubject(ctx context.Context, tenantID, subjectKind, subjectID string) ([]source.SourceRestriction, error)
}

// ObjectAuthorizer is the single fail-closed source read decision point;
// *source.Authorizer satisfies it.
type ObjectAuthorizer interface {
	CanRead(ctx context.Context, p auth.Principal, obj source.Object, opts source.ReadOptions) error
	EffectiveRestrictions(ctx context.Context, obj source.Object, ver *source.ObjectVersion, artifactID string) (source.SourceRestriction, error)
}

// ArtifactStore resolves opaque artifact references against durable tenant
// metadata. *postgres.ArtifactRepo satisfies it.
type ArtifactStore interface {
	GetArtifact(ctx context.Context, tenantID, id string) (artifacts.ArtifactRow, error)
}

// ConversationStore resolves retained conversation identity. The postgres
// adapter lands with the conversational executor consumers (01-28+); an
// unset store denies conversation references outright.
type ConversationStore interface {
	GetConversation(ctx context.Context, tenantID, id string) (Conversation, error)
}

// Conversation is the executor-visible retained-resource identity for a
// stored conversation.
type Conversation struct {
	TenantID     string
	ID           string
	OwnerActorID string
}

// restrictionSubjectTenant scopes a SourceRestriction row to tenant-wide
// processing policy; subjectID is the tenant id.
const restrictionSubjectTenant = "tenant"

// restrictionSubjectConversation scopes restrictions attached to a retained
// conversation; subjectID is the conversation id.
const restrictionSubjectConversation = "conversation"

// forbiddenPayloadKeys are request fields that must never reach an upstream
// or the dispatch path: identity, credentials, arbitrary origins and
// upstream-retention/resource ids the server cannot authorize or price.
// Presence denies the request — matching is on the exact JSON key at any
// depth.
var forbiddenPayloadKeys = map[string]bool{
	"tenant_id": true, "owner": true, "owner_actor_id": true,
	"user_id": true, "actor_id": true, "billing_subject": true,
	"api_key": true, "api_base": true, "base_url": true, "endpoint": true,
	"credential_ref": true, "credential": true, "credentials": true,
	"authorization": true, "auth_token": true, "access_token": true,
	"secret": true, "client_secret": true,
	"previous_response_id": true, "conversation": true,
	"file_id": true, "container_id": true,
	"user": true,
}

// forbiddenPayloadTruths are boolean keys that may only be false/absent: a
// request cannot enable upstream retention or background execution the
// server cannot observe and account.
var forbiddenPayloadTruths = map[string]bool{
	"store": true, "background": true,
}

// deniedNestedTypes are "type" discriminators with no safe or enforceable
// authorization/pricing rule — hosted tools and remote resources reject
// explicitly rather than pass through.
var deniedNestedTypes = map[string]bool{
	"web_search": true, "web_search_preview": true, "file_search": true,
	"code_interpreter": true, "computer_use_preview": true, "shell": true,
	"custom": true, "hosted_tool": true, "remote_mcp": true, "mcp": true,
}

// allowedNestedTypes are the only "type" discriminator values a payload may
// carry — text parts, inline image/file/audio parts, function tools and the
// standard message/content shapes.
var allowedNestedTypes = map[string]bool{
	"function": true,
	"text":     true, "input_text": true, "output_text": true, "refusal": true,
	"input_image": true, "image_url": true, "input_file": true, "file": true,
	"input_audio": true, "message": true, "tool_call": true,
	"tool_calls": true, "json_object": true, "json_schema": true,
}

// urlPayloadKeys are keys whose string value must be an inline data URI —
// any absolute URL is an unauthorizable remote destination.
var urlPayloadKeys = map[string]bool{
	"url": true, "image_url": true, "file_url": true, "audio_url": true,
	"href": true, "src": true, "uri": true,
}

// payloadInfo is what analyzePayload learned from one request body.
type payloadInfo struct {
	// needsVision marks payloads containing image/file input parts, which
	// upgrade a chat request to the vision capability for pricing/routing.
	needsVision bool
	// root is the decoded payload object — the executor injects the bound
	// upstream model into it before dispatch.
	root map[string]any
}

// resolveInputs authorizes every referenced input and returns the merged
// effective restriction set (persisted ∪ request narrowing). Any failure is
// ErrDenied — unknown, forbidden, stale, forged and cross-tenant inputs are
// indistinguishable.
func (s *Service) resolveInputs(ctx context.Context, p auth.Principal, req Request) (source.SourceRestriction, error) {
	effective := source.SourceRestriction{TenantID: p.TenantID}

	// Tenant-wide persisted processing policy (subject kind "tenant").
	rows, err := s.sources.RestrictionsForSubject(ctx, p.TenantID, restrictionSubjectTenant, p.TenantID)
	if err != nil {
		return effective, fmt.Errorf("%w", ErrDenied)
	}
	for _, r := range rows {
		mergeInto(&effective, r)
	}

	for _, ref := range req.Inputs {
		if ref.TenantID != "" && ref.TenantID != p.TenantID {
			return effective, fmt.Errorf("%w", ErrDenied)
		}
		obj, err := s.sources.GetObject(ctx, p.TenantID, ref.ObjectID)
		if err != nil || obj.TenantID != p.TenantID {
			return effective, fmt.Errorf("%w", ErrDenied)
		}
		var ver source.ObjectVersion
		if ref.ObservedSeq > 0 {
			ver, err = s.sources.GetObjectVersion(ctx, p.TenantID, obj.ID, ref.ObservedSeq)
		} else {
			ver, err = s.sources.LatestObjectVersion(ctx, p.TenantID, obj.ID)
		}
		if err != nil || ver.ObjectID != obj.ID {
			return effective, fmt.Errorf("%w", ErrDenied)
		}
		// Current source authorization under request narrowing — the request
		// can tighten, never widen (checkNarrowing runs inside CanRead).
		if err := s.authorizer.CanRead(ctx, p, obj, source.ReadOptions{RequestedRestriction: req.Restriction}); err != nil {
			return effective, fmt.Errorf("%w", ErrDenied)
		}
		restr, err := s.authorizer.EffectiveRestrictions(ctx, obj, &ver, "")
		if err != nil {
			return effective, fmt.Errorf("%w", ErrDenied)
		}
		mergeInto(&effective, restr)
	}

	for _, ref := range req.Artifacts {
		if s.artifacts == nil || ref.ID == "" ||
			(ref.TenantID != "" && ref.TenantID != p.TenantID) {
			return effective, fmt.Errorf("%w", ErrDenied)
		}
		row, err := s.artifacts.GetArtifact(ctx, p.TenantID, ref.ID)
		if err != nil || row.TenantID != p.TenantID {
			return effective, fmt.Errorf("%w", ErrDenied)
		}
		// Only finalized rows are usable inputs, and every caller-asserted
		// ref field must match the durable row — a forged relabel denies.
		if row.State != "final" || row.TombstoneReason != "" ||
			(ref.Kind != "" && row.Kind != ref.Kind) ||
			(ref.Version != 0 && row.Version != ref.Version) ||
			(ref.SHA256 != "" && !strings.EqualFold(row.SHA256, ref.SHA256)) ||
			(ref.ByteSize != 0 && row.ByteSize != ref.ByteSize) {
			return effective, fmt.Errorf("%w", ErrDenied)
		}
		rows, err := s.sources.RestrictionsForSubject(ctx, p.TenantID, source.RestrictionArtifact, ref.ID)
		if err != nil {
			return effective, fmt.Errorf("%w", ErrDenied)
		}
		for _, r := range rows {
			mergeInto(&effective, r)
		}
	}

	for _, convID := range req.Conversations {
		if convID == "" || s.conversations == nil {
			return effective, fmt.Errorf("%w", ErrDenied)
		}
		conv, err := s.conversations.GetConversation(ctx, p.TenantID, convID)
		if err != nil || conv.TenantID != p.TenantID {
			return effective, fmt.Errorf("%w", ErrDenied)
		}
		if conv.OwnerActorID != p.ActorID &&
			(p.DelegatedByActorID == "" || conv.OwnerActorID != p.DelegatedByActorID) {
			return effective, fmt.Errorf("%w", ErrDenied)
		}
		rows, err := s.sources.RestrictionsForSubject(ctx, p.TenantID, restrictionSubjectConversation, convID)
		if err != nil {
			return effective, fmt.Errorf("%w", ErrDenied)
		}
		for _, r := range rows {
			mergeInto(&effective, r)
		}
	}

	// Request narrowing may only add denied capabilities/labels and raise
	// assurance — union it (per-object widening was already rejected inside
	// CanRead via checkNarrowing).
	if req.Restriction != nil {
		mergeInto(&effective, *req.Restriction)
	}
	return effective, nil
}

// mergeInto unions restriction r into dst: highest assurance wins, denied
// capabilities and labels union (restrictions only tighten).
func mergeInto(dst *source.SourceRestriction, r source.SourceRestriction) {
	if assuranceRankOf(r.RequiredAssurance) > assuranceRankOf(dst.RequiredAssurance) {
		dst.RequiredAssurance = r.RequiredAssurance
	}
	dst.DeniedCapabilities = unionOf(dst.DeniedCapabilities, r.DeniedCapabilities)
	dst.PolicyLabels = unionOf(dst.PolicyLabels, r.PolicyLabels)
}

// assuranceRankOf orders required assurance; unknown levels rank highest so
// an unrecognized persisted level can never be weakened — the same ordering
// the source package enforces internally.
func assuranceRankOf(level string) int {
	switch level {
	case "":
		return 0
	case "standard":
		return 1
	case "high":
		return 2
	default:
		return 3
	}
}

func unionOf(a, b []string) []string {
	seen := make(map[string]struct{}, len(a)+len(b))
	out := append([]string{}, a...)
	for _, v := range a {
		seen[v] = struct{}{}
	}
	for _, v := range b {
		if _, ok := seen[v]; !ok {
			seen[v] = struct{}{}
			out = append(out, v)
		}
	}
	return out
}

// restrictionDenies reports whether the effective restriction set denies the
// routed capability.
func restrictionDenies(r source.SourceRestriction, capability string) bool {
	for _, c := range r.DeniedCapabilities {
		if c == capability {
			return true
		}
	}
	return false
}

// analyzePayload bounds, decodes and validates the provider body. It runs
// before routing so image/file parts can upgrade the routed capability and
// unauthorizable shapes never reach a transport.
func (s *Service) analyzePayload(req Request) (payloadInfo, error) {
	var info payloadInfo
	if s.payloadLimit > 0 && int64(len(req.Payload)) > s.payloadLimit {
		return info, fmt.Errorf("%w: payload exceeds bound", ErrDenied)
	}
	root := map[string]any{}
	if len(req.Payload) > 0 {
		dec := json.NewDecoder(strings.NewReader(string(req.Payload)))
		dec.UseNumber()
		if err := dec.Decode(&root); err != nil {
			return info, fmt.Errorf("%w: payload must be a JSON object", ErrDenied)
		}
		if dec.More() {
			return info, fmt.Errorf("%w: trailing payload data", ErrDenied)
		}
	}
	if err := walkPayload(root, &info, ""); err != nil {
		return info, err
	}
	if err := validateResponsesTarget(req); err != nil {
		return info, err
	}
	info.root = root
	return info, nil
}

// effectiveCapability resolves the routed capability: a chat request whose
// payload carries image/file parts needs the vision rule.
func effectiveCapability(capability string, info payloadInfo) (string, error) {
	if capability == CapabilityChat && info.needsVision {
		return CapabilityVision, nil
	}
	return capability, nil
}

// marshalPayload produces the dispatched body: the endpoint-bound upstream
// model replaces any client-supplied value, the stream flag reflects the
// request and streaming chat bodies carry stream_options.include_usage so
// terminal usage is observable for settlement.
func marshalPayload(root map[string]any, upstreamModel string, req Request) ([]byte, error) {
	delete(root, "model")
	if upstreamModel != "" {
		root["model"] = upstreamModel
	}
	root["stream"] = req.Stream
	if req.Stream && (req.Capability == CapabilityChat || req.Capability == CapabilityVision) {
		// Chat streams only emit usage when the server asks for it.
		opts, _ := root["stream_options"].(map[string]any)
		if opts == nil {
			opts = map[string]any{}
		}
		opts["include_usage"] = true
		root["stream_options"] = opts
	}
	body, err := json.Marshal(root)
	if err != nil {
		return nil, fmt.Errorf("%w: unmarshalable payload", ErrDenied)
	}
	return body, nil
}

// validateResponsesTarget restricts Responses dispatches to the bounded
// /responses operation surface — no caller-supplied absolute URL or path
// escape.
func validateResponsesTarget(req Request) error {
	if req.Capability != CapabilityResponses && req.Capability != CapabilityCompact {
		return nil
	}
	switch strings.ToUpper(req.Method) {
	case "", http.MethodGet, http.MethodPost, http.MethodDelete:
	default:
		return fmt.Errorf("%w: responses method", ErrDenied)
	}
	p := req.Path
	if p == "" {
		return nil // dispatch maps the capability default
	}
	if !strings.HasPrefix(p, "/responses") || strings.Contains(p, "..") ||
		strings.Contains(p, "//") || strings.ContainsAny(p, "?#") ||
		!validPathTail(strings.TrimPrefix(p, "/responses")) {
		return fmt.Errorf("%w: responses path", ErrDenied)
	}
	return nil
}

func validPathTail(tail string) bool {
	if tail == "" {
		return true
	}
	if !strings.HasPrefix(tail, "/") {
		return false
	}
	for _, seg := range strings.Split(tail[1:], "/") {
		if seg == "" {
			return false
		}
		for _, r := range seg {
			if !(r >= 'a' && r <= 'z') && !(r >= 'A' && r <= 'Z') &&
				!(r >= '0' && r <= '9') && r != '-' && r != '_' && r != '.' {
				return false
			}
		}
	}
	return true
}

// discriminatorContexts are the payload keys whose array elements use "type"
// as a resource/tool discriminator. "type" elsewhere — JSON Schema
// parameters/properties/items, arbitrary response_format shapes — is data,
// not a destination claim, and stays uninterpreted.
var discriminatorContexts = map[string]bool{
	"tools": true, "content": true, "input": true,
	"tool_calls": true, "items": false, // items is schema-shaped
}

// walkPayload recursively enforces the bounded provider contract. parentKey
// is the enclosing object's key ("" at root); elements of a "tools" array
// must be function tools.
func walkPayload(node any, info *payloadInfo, parentKey string) error {
	switch v := node.(type) {
	case map[string]any:
		inTools := parentKey == "tools"
		isDiscriminator := discriminatorContexts[parentKey]
		var typeVal string
		if t, ok := v["type"]; ok && isDiscriminator {
			s, ok := t.(string)
			if !ok {
				return fmt.Errorf("%w: non-string type discriminator", ErrDenied)
			}
			typeVal = s
			if inTools && typeVal != "function" {
				return fmt.Errorf("%w: tool type %q", ErrUnsupportedCapability, typeVal)
			}
			if deniedNestedTypes[typeVal] {
				return fmt.Errorf("%w: type %q", ErrUnsupportedCapability, typeVal)
			}
			if !allowedNestedTypes[typeVal] {
				return fmt.Errorf("%w: type %q", ErrDenied, typeVal)
			}
			if typeVal == "input_image" || typeVal == "image_url" ||
				typeVal == "input_file" || typeVal == "file" {
				info.needsVision = true
			}
		}
		for k, val := range v {
			kl := strings.ToLower(k)
			if forbiddenPayloadKeys[kl] {
				return fmt.Errorf("%w: field %q is not permitted", ErrDenied, k)
			}
			if forbiddenPayloadTruths[kl] {
				if b, ok := val.(bool); ok && b {
					return fmt.Errorf("%w: %s cannot be enabled", ErrDenied, k)
				}
			}
			if urlPayloadKeys[kl] {
				// URL-bearing fields accept inline data URIs or nested
				// objects (recursed below); anything else — http(s), file,
				// opaque handles — is an unauthorizable remote destination.
				if s, ok := val.(string); ok {
					if s != "" && !strings.HasPrefix(s, "data:") {
						return fmt.Errorf("%w: %s remote destination", ErrDenied, k)
					}
				} else if _, isMap := val.(map[string]any); !isMap && val != nil {
					return fmt.Errorf("%w: %s must be a data URI or object", ErrDenied, k)
				}
			}
			if err := walkPayload(val, info, kl); err != nil {
				return err
			}
		}
	case []any:
		for _, item := range v {
			if err := walkPayload(item, info, parentKey); err != nil {
				return err
			}
		}
	case json.Number:
		f, err := v.Float64()
		if err != nil || math.IsNaN(f) || math.IsInf(f, 0) {
			return fmt.Errorf("%w: numeric payload value out of range", ErrDenied)
		}
	}
	return nil
}

// AuthorizeDestination resolves request inputs and evaluates one
// server-loaded destination endpoint under the same policy Execute uses —
// the reusable authorization path for later approved outbound destinations.
// dest must come from the tenant's endpoint store, never from caller data.
func (s *Service) AuthorizeDestination(ctx context.Context, p auth.Principal, req Request, dest Endpoint) (Decision, error) {
	if p.TenantID == "" || dest.TenantID != p.TenantID {
		return Decision{}, fmt.Errorf("%w", ErrDenied)
	}
	info, err := s.analyzePayload(req)
	if err != nil {
		return Decision{}, err
	}
	capability, err := effectiveCapability(req.Capability, info)
	if err != nil {
		return Decision{}, err
	}
	return s.evaluate(ctx, p, req, dest, capability)
}

// evaluate checks one candidate endpoint for one request after payload
// analysis: enablement, actor endpoint/model allowlists, the approved model
// alias binding, advertised capability, the merged input/tenant/request
// restriction set and required assurance.
func (s *Service) evaluate(ctx context.Context, p auth.Principal, req Request, dest Endpoint, capability string) (Decision, error) {
	if !dest.Enabled || !auth.EndpointAllowed(p, dest.ID) {
		return Decision{}, fmt.Errorf("%w", ErrDenied)
	}
	if req.Model != "" && !auth.ModelAllowed(p, req.Model) {
		return Decision{}, fmt.Errorf("%w", ErrDenied)
	}
	upstream, bound := upstreamModelFor(dest, req.Model)
	if !bound {
		// No approved alias binding — including an empty alias — never
		// resolves to an upstream model.
		return Decision{}, fmt.Errorf("%w", ErrDenied)
	}
	if !capabilityAdvertised(dest, capability) {
		return Decision{}, fmt.Errorf("%w", ErrNoRoute)
	}
	effective, err := s.resolveInputs(ctx, p, req)
	if err != nil {
		return Decision{}, err
	}
	if restrictionDenies(effective, capability) {
		return Decision{}, fmt.Errorf("%w", ErrDenied)
	}
	if !assuranceSatisfied(dest, effective.RequiredAssurance) && !verifiable(dest) {
		return Decision{}, fmt.Errorf("%w", ErrNoRoute)
	}
	return Decision{
		Capability:    capability,
		UpstreamModel: upstream,
		Effective:     effective,
	}, nil
}

// verifiable reports whether the endpoint's protocol supports real-time
// attestation so a stale/missing assurance timestamp can be re-verified at
// dispatch. A purported confidential claim on a non-verifiable endpoint is
// never trusted.
func verifiable(e Endpoint) bool {
	return e.Protocol == "tinfoil"
}

// route picks the deterministic eligible endpoint for the resolved
// capability: enabled, actor-allowlisted, capability-advertising and bound
// to the requested model alias. Selection order is the repository's
// deterministic id order — no load-based failover, which could replay a
// possibly accepted request on another endpoint.
func (s *Service) route(ctx context.Context, p auth.Principal, req Request, capability string) (Endpoint, error) {
	var candidates []Endpoint
	if req.EndpointID != "" {
		e, err := s.endpoints.Get(ctx, p.TenantID, req.EndpointID)
		if err != nil {
			return Endpoint{}, fmt.Errorf("%w", ErrNoRoute)
		}
		candidates = []Endpoint{e}
	} else {
		list, err := s.endpoints.ListCapabilities(ctx, p.TenantID, capability)
		if err != nil {
			return Endpoint{}, fmt.Errorf("list endpoints: %w", err)
		}
		candidates = list // repo already returns id-ordered, enabled rows
	}
	for _, e := range candidates {
		if !e.Enabled || !capabilityAdvertised(e, capability) {
			continue
		}
		if !auth.EndpointAllowed(p, e.ID) {
			continue
		}
		if !auth.ModelAllowed(p, req.Model) {
			continue
		}
		if _, bound := upstreamModelFor(e, req.Model); !bound {
			continue
		}
		return e, nil
	}
	return Endpoint{}, fmt.Errorf("%w", ErrNoRoute)
}
