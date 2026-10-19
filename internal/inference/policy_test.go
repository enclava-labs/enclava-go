// policy_test.go covers the shared input/destination authorization pass
// against real PostgreSQL (AUTH-04/AUTH-06/INF-06): restriction intersections,
// stale/forged references, actor allowlists, cross-tenant endpoints,
// capability mismatch, purported assurance, nested destinations and the
// reusable AuthorizeDestination path.
package inference_test

import (
	"errors"
	"strings"
	"testing"
	"time"

	"enclava-go/internal/billing"
	"enclava-go/internal/inference"
	"enclava-go/internal/source"
)

// obj seeds an owner-authorized object with one version.
func (f *fx) obj(t *testing.T, id string) source.Object {
	t.Helper()
	o, err := f.sources.UpsertObject(f.ctx, source.Object{
		TenantID: f.tenant, ID: id, Kind: "file", NativeID: "n-" + id,
		AuthorizationMode: source.AuthorizationOwner, OwnerActorID: "actor-1",
		ObservedSeq: 1})
	if err != nil {
		t.Fatalf("upsert object: %v", err)
	}
	if err := f.sources.InsertObjectVersion(f.ctx, f.tenant, source.ObjectVersion{
		ObjectID: id, ObservedSeq: 1, ArtifactRef: "art-" + id,
		SHA256: "aa", ByteSize: 10, MimeType: "text/plain"}); err != nil {
		t.Fatalf("insert version: %v", err)
	}
	return o
}

func TestInputRestrictionDeniesCapability(t *testing.T) {
	f := newFx(t)
	f.mkEndpoint(t)
	f.flatPrices(t)
	f.obj(t, "doc-1")
	// The object forbids chat processing entirely.
	if err := f.sources.SetRestriction(f.ctx, source.SourceRestriction{
		TenantID: f.tenant, SubjectKind: source.RestrictionObject, SubjectID: "doc-1",
		DeniedCapabilities: []string{inference.CapabilityChat}}); err != nil {
		t.Fatalf("set restriction: %v", err)
	}
	req := f.chatRequest(`{"messages":[{"role":"user","content":"hi"}]}`)
	req.Inputs = []source.ObjectRef{{ObjectID: "doc-1", ObservedSeq: 1}}
	if _, err := f.svc.Execute(f.ctx, f.principal, req); !errors.Is(err, inference.ErrDenied) {
		t.Fatalf("restricted input must deny, got %v", err)
	}
	if f.upstream.calls != 0 {
		t.Fatal("denied input reached the transport")
	}
}

func TestNarrowingCanTightenNeverWiden(t *testing.T) {
	f := newFx(t)
	f.mkEndpoint(t)
	f.flatPrices(t)
	f.obj(t, "doc-1")
	if err := f.sources.SetRestriction(f.ctx, source.SourceRestriction{
		TenantID: f.tenant, SubjectKind: source.RestrictionObject, SubjectID: "doc-1",
		DeniedCapabilities: []string{inference.CapabilityEmbeddings}}); err != nil {
		t.Fatalf("set restriction: %v", err)
	}

	// A request that drops the persisted deny widens policy — denied inside
	// CanRead before any routing.
	req := f.chatRequest(`{"messages":[{"role":"user","content":"hi"}]}`)
	req.Inputs = []source.ObjectRef{{ObjectID: "doc-1", ObservedSeq: 1}}
	req.Restriction = &source.SourceRestriction{RequiredAssurance: "standard"}
	if _, err := f.svc.Execute(f.ctx, f.principal, req); !errors.Is(err, inference.ErrDenied) {
		t.Fatalf("widening request must deny, got %v", err)
	}

	// A request retaining the persisted deny and adding one more tightens —
	// and the added denial then blocks the routed capability.
	req.Restriction = &source.SourceRestriction{
		DeniedCapabilities: []string{inference.CapabilityEmbeddings, inference.CapabilityChat}}
	if _, err := f.svc.Execute(f.ctx, f.principal, req); !errors.Is(err, inference.ErrDenied) {
		t.Fatalf("narrowed-out capability must deny, got %v", err)
	}
}

func TestUnknownAndStaleReferencesDeny(t *testing.T) {
	f := newFx(t)
	f.mkEndpoint(t)
	f.flatPrices(t)

	// Unknown object id.
	req := f.chatRequest(`{"messages":[{"role":"user","content":"hi"}]}`)
	req.Inputs = []source.ObjectRef{{ObjectID: "ghost", ObservedSeq: 1}}
	if _, err := f.svc.Execute(f.ctx, f.principal, req); !errors.Is(err, inference.ErrDenied) {
		t.Fatalf("unknown object must deny, got %v", err)
	}

	// Stale/deleted object denies identically.
	f.obj(t, "doc-gone")
	if err := f.sources.MarkObjectDeleted(f.ctx, f.tenant, "doc-gone"); err != nil {
		t.Fatalf("delete: %v", err)
	}
	req.Inputs = []source.ObjectRef{{ObjectID: "doc-gone", ObservedSeq: 1}}
	if _, err := f.svc.Execute(f.ctx, f.principal, req); !errors.Is(err, inference.ErrDenied) {
		t.Fatalf("deleted object must deny, got %v", err)
	}

	// A version that never existed denies.
	f.obj(t, "doc-2")
	req.Inputs = []source.ObjectRef{{ObjectID: "doc-2", ObservedSeq: 99}}
	if _, err := f.svc.Execute(f.ctx, f.principal, req); !errors.Is(err, inference.ErrDenied) {
		t.Fatalf("nonexistent version must deny, got %v", err)
	}

	// Cross-tenant reference id denies.
	req.Inputs = []source.ObjectRef{{TenantID: "00000000-0000-0000-0000-0000000000aa", ObjectID: "doc-2", ObservedSeq: 1}}
	if _, err := f.svc.Execute(f.ctx, f.principal, req); !errors.Is(err, inference.ErrDenied) {
		t.Fatalf("cross-tenant ref must deny, got %v", err)
	}
}

func TestOtherActorObjectDenied(t *testing.T) {
	f := newFx(t)
	f.mkEndpoint(t)
	f.flatPrices(t)
	f.obj(t, "doc-1")
	other := f.principal
	other.ActorID = "someone-else"
	other.UserID = "someone-else"

	req := f.chatRequest(`{"messages":[{"role":"user","content":"hi"}]}`)
	req.Inputs = []source.ObjectRef{{ObjectID: "doc-1", ObservedSeq: 1}}
	if _, err := f.svc.Execute(f.ctx, other, req); !errors.Is(err, inference.ErrDenied) {
		t.Fatalf("foreign-owned input must deny, got %v", err)
	}
}

func TestForgedPayloadFieldsDeny(t *testing.T) {
	f := newFx(t)
	f.mkEndpoint(t)
	f.flatPrices(t)

	for _, payload := range []string{
		`{"tenant_id":"00000000-0000-0000-0000-0000000000aa","messages":[]}`,
		`{"api_key":"sk-leak","messages":[]}`,
		`{"base_url":"https://evil.example","messages":[]}`,
		`{"credential_ref":"other-ref","messages":[]}`,
		`{"store":true,"messages":[]}`,
		`{"previous_response_id":"resp_foreign","messages":[]}`,
		`{"file_id":"file-abc","messages":[]}`,
	} {
		_, err := f.svc.Execute(f.ctx, f.principal, f.chatRequest(payload))
		if !errors.Is(err, inference.ErrDenied) {
			t.Fatalf("forged payload %q must deny, got %v", payload, err)
		}
	}
	if f.upstream.calls != 0 {
		t.Fatal("forged payload reached the transport")
	}
	if n := f.reservationCount(t); n != 0 {
		t.Fatalf("denied payloads left %d reservations", n)
	}
}

func TestNestedDestinationsDeny(t *testing.T) {
	f := newFx(t)
	f.mkEndpoint(t)
	f.flatPrices(t)

	for _, tc := range []struct {
		name    string
		payload string
		want    error
	}{
		{"hosted web_search tool", `{"tools":[{"type":"web_search"}],"messages":[]}`, inference.ErrUnsupportedCapability},
		{"remote image url", `{"messages":[{"role":"user","content":[{"type":"input_image","image_url":{"url":"https://evil.example/x.png"}}]}]}`, inference.ErrDenied},
		{"nested remote file", `{"input":[{"type":"input_file","file_url":"https://evil.example/f.pdf"}]}`, inference.ErrDenied},
	} {
		_, err := f.svc.Execute(f.ctx, f.principal, f.chatRequest(tc.payload))
		if !errors.Is(err, tc.want) {
			t.Fatalf("%s: want %v, got %v", tc.name, tc.want, err)
		}
	}
	if f.upstream.calls != 0 {
		t.Fatal("nested unauthorizable destination reached transport")
	}
}

func TestCapabilityMismatchNoRoute(t *testing.T) {
	f := newFx(t)
	// Endpoint advertises embeddings only (plus the model alias).
	e := f.mkEndpoint(t)
	e.Capabilities = []string{inference.CapabilityEmbeddings, "model:gpt-fast=gpt-4o-mini"}
	if err := f.endpoint.Update(f.ctx, e); err != nil {
		t.Fatalf("update endpoint: %v", err)
	}
	f.flatPrices(t)
	if _, err := f.svc.Execute(f.ctx, f.principal,
		f.chatRequest(`{"messages":[{"role":"user","content":"hi"}]}`)); !errors.Is(err, inference.ErrNoRoute) {
		t.Fatalf("capability mismatch must yield no route, got %v", err)
	}
}

func TestVisionUpgradeRoutesAsVision(t *testing.T) {
	f := newFx(t)
	// The endpoint never advertises vision: an image part upgrades chat to
	// vision and finds no eligible destination.
	f.mkEndpoint(t)
	f.flatPrices(t)
	f.price(t, inference.CapabilityVision, billing.UnitInput, 1, 1)
	f.price(t, inference.CapabilityVision, billing.UnitOutput, 1, 1)
	payload := `{"messages":[{"role":"user","content":[{"type":"input_text","text":"what"},{"type":"input_image","image_url":{"url":"data:image/png;base64,AAAA"}}]}]}`
	if _, err := f.svc.Execute(f.ctx, f.principal, f.chatRequest(payload)); !errors.Is(err, inference.ErrNoRoute) {
		t.Fatalf("vision payload without vision endpoint must not route as chat, got %v", err)
	}
	// With the capability advertised, the same payload routes under the
	// vision price set.
	e, err := f.endpoint.Get(f.ctx, f.tenant, "ep-1")
	if err != nil {
		t.Fatalf("get endpoint: %v", err)
	}
	e.Capabilities = append(e.Capabilities, inference.CapabilityVision)
	if err := f.endpoint.Update(f.ctx, e); err != nil {
		t.Fatalf("update endpoint: %v", err)
	}
	f.upstream.resp = []byte(`{"usage":{"prompt_tokens":10,"completion_tokens":2}}`)
	res, err := f.svc.Execute(f.ctx, f.principal, f.chatRequest(payload))
	if err != nil {
		t.Fatalf("vision execute: %v", err)
	}
	if res.Capability != inference.CapabilityVision {
		t.Fatalf("routed capability = %s", res.Capability)
	}
}

func TestCrossTenantAndDisallowedEndpoints(t *testing.T) {
	f := newFx(t)
	f.mkEndpoint(t)
	f.flatPrices(t)

	// An endpoint id belonging to another tenant is not a route.
	req := f.chatRequest(`{"messages":[{"role":"user","content":"hi"}]}`)
	req.EndpointID = "foreign-ep"
	if _, err := f.svc.Execute(f.ctx, f.principal, req); !errors.Is(err, inference.ErrNoRoute) {
		t.Fatalf("foreign endpoint must not route, got %v", err)
	}

	// Actor endpoint allowlist excludes the only endpoint.
	p := f.principal
	p.AllowedEndpointsAll = false
	p.AllowedEndpoints = map[string]struct{}{}
	if _, err := f.svc.Execute(f.ctx, p, f.chatRequest(`{"messages":[{"role":"user","content":"hi"}]}`)); !errors.Is(err, inference.ErrNoRoute) {
		t.Fatalf("unallowed endpoint must not route, got %v", err)
	}

	// Actor model allowlist excludes the alias.
	p = f.principal
	p.AllowedModelsAll = false
	p.AllowedModels = map[string]struct{}{}
	if _, err := f.svc.Execute(f.ctx, p, f.chatRequest(`{"messages":[{"role":"user","content":"hi"}]}`)); !errors.Is(err, inference.ErrNoRoute) {
		t.Fatalf("unallowed model must not route, got %v", err)
	}

	// Unknown alias: the endpoint binds only gpt-fast.
	req = f.chatRequest(`{"messages":[{"role":"user","content":"hi"}]}`)
	req.Model = "gpt-admin"
	if _, err := f.svc.Execute(f.ctx, f.principal, req); !errors.Is(err, inference.ErrNoRoute) {
		t.Fatalf("unbound alias must not route, got %v", err)
	}
}

func TestPurportedConfidentialFlagDenied(t *testing.T) {
	f := newFx(t)
	e := f.mkEndpoint(t)
	f.flatPrices(t)
	f.obj(t, "doc-1")

	// The endpoint claims assurance_mode but has no observed verification.
	e.AssuranceMode = "attested"
	if err := f.endpoint.Update(f.ctx, e); err != nil {
		t.Fatalf("update: %v", err)
	}
	// Required high assurance with an unverified endpoint: no route.
	if err := f.sources.SetRestriction(f.ctx, source.SourceRestriction{
		TenantID: f.tenant, SubjectKind: source.RestrictionObject, SubjectID: "doc-1",
		RequiredAssurance: "high"}); err != nil {
		t.Fatalf("set restriction: %v", err)
	}
	req := f.chatRequest(`{"messages":[{"role":"user","content":"hi"}]}`)
	req.Inputs = []source.ObjectRef{{ObjectID: "doc-1", ObservedSeq: 1}}
	if _, err := f.svc.Execute(f.ctx, f.principal, req); !errors.Is(err, inference.ErrNoRoute) {
		t.Fatalf("unverified endpoint must not serve confidential input, got %v", err)
	}

	// Recorded verification admits it.
	verified := time.Now().UTC().Add(-time.Minute)
	e.AssuranceVerifiedAt = &verified
	if err := f.endpoint.Update(f.ctx, e); err != nil {
		t.Fatalf("update: %v", err)
	}
	if _, err := f.svc.Execute(f.ctx, f.principal, req); err != nil {
		t.Fatalf("verified endpoint denied: %v", err)
	}
}

func TestConversationRefOwnership(t *testing.T) {
	f := newFx(t)
	f.mkEndpoint(t)
	f.flatPrices(t)
	f.conversations = fakeConversations{
		f.tenant + "\x00conv-1": {TenantID: f.tenant, ID: "conv-1", OwnerActorID: "actor-1"},
	}
	f.build(t) // rebind the executor to the conversation store

	req := f.chatRequest(`{"messages":[{"role":"user","content":"hi"}]}`)
	req.Conversations = []string{"conv-1"}
	if _, err := f.svc.Execute(f.ctx, f.principal, req); err != nil {
		t.Fatalf("owned conversation denied: %v", err)
	}

	other := f.principal
	other.ActorID = "actor-2"
	if _, err := f.svc.Execute(f.ctx, other, req); !errors.Is(err, inference.ErrDenied) {
		t.Fatalf("foreign conversation must deny, got %v", err)
	}
}

func TestAuthorizeDestinationSharedPolicy(t *testing.T) {
	f := newFx(t)
	e := f.mkEndpoint(t)
	f.obj(t, "doc-1")

	// Happy path: destination evaluation returns the effective decision.
	dec, err := f.svc.AuthorizeDestination(f.ctx, f.principal,
		f.chatRequest(`{"messages":[{"role":"user","content":"hi"}]}`), e)
	if err != nil {
		t.Fatalf("authorize destination: %v", err)
	}
	if dec.Capability != inference.CapabilityChat || dec.UpstreamModel != "gpt-4o-mini" {
		t.Fatalf("decision wrong: %+v", dec)
	}

	// A cross-tenant endpoint record never authorizes.
	foreign := e
	foreign.TenantID = "00000000-0000-0000-0000-0000000000aa"
	if _, err := f.svc.AuthorizeDestination(f.ctx, f.principal,
		f.chatRequest(`{"messages":[]}`), foreign); !errors.Is(err, inference.ErrDenied) {
		t.Fatalf("foreign destination must deny, got %v", err)
	}

	// A restricted input denies the destination evaluation equally.
	if err := f.sources.SetRestriction(f.ctx, source.SourceRestriction{
		TenantID: f.tenant, SubjectKind: source.RestrictionObject, SubjectID: "doc-1",
		DeniedCapabilities: []string{inference.CapabilityChat}}); err != nil {
		t.Fatalf("set restriction: %v", err)
	}
	req := f.chatRequest(`{"messages":[{"role":"user","content":"hi"}]}`)
	req.Inputs = []source.ObjectRef{{ObjectID: "doc-1", ObservedSeq: 1}}
	if _, err := f.svc.AuthorizeDestination(f.ctx, f.principal, req, e); !errors.Is(err, inference.ErrDenied) {
		t.Fatalf("restricted destination must deny, got %v", err)
	}
}

func TestServiceConstructionRequiresSeams(t *testing.T) {
	if _, err := inference.NewService(inference.Deps{}); err == nil {
		t.Fatal("missing seams must fail construction")
	}
}

func TestFunctionToolWithSchemaParamsAllowed(t *testing.T) {
	f := newFx(t)
	f.mkEndpoint(t)
	f.flatPrices(t)
	// A function tool whose parameters schema declares "type":"object" must
	// pass — schema "type" is data, not a tool discriminator.
	payload := `{"messages":[{"role":"user","content":"hi"}],"tools":[{"type":"function","function":{"name":"lookup","parameters":{"type":"object","properties":{"q":{"type":"string"}}}}}]}`
	if _, err := f.svc.Execute(f.ctx, f.principal, f.chatRequest(payload)); err != nil {
		t.Fatalf("function tool denied: %v", err)
	}
}

func TestModelFieldReplacedNotRespected(t *testing.T) {
	f := newFx(t)
	f.mkEndpoint(t)
	f.flatPrices(t)
	// A client-supplied model field is replaced by the endpoint binding.
	if _, err := f.svc.Execute(f.ctx, f.principal,
		f.chatRequest(`{"model":"gpt-admin-secret","messages":[]}`)); err != nil {
		t.Fatalf("execute: %v", err)
	}
	if !strings.Contains(string(f.upstream.lastBody), `"model":"gpt-4o-mini"`) ||
		strings.Contains(string(f.upstream.lastBody), "gpt-admin-secret") {
		t.Fatalf("client model leaked upstream: %s", f.upstream.lastBody)
	}
}
