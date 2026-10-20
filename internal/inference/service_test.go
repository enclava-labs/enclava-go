// service_test.go exercises the executor's dispatch and accounting lifecycle
// against real PostgreSQL: atomic reservations before dispatch, exact
// once-only settlement under the immutable rate snapshot, retained unknown
// liability, and the verified never-dispatched release (JOB-03/JOB-04/INF-02).
package inference_test

import (
	"context"
	"errors"
	"fmt"
	"io"
	"net/http"
	"net/http/httptest"
	"os"
	"strings"
	"sync/atomic"
	"testing"
	"time"

	"enclava-go/internal/artifacts"
	"enclava-go/internal/auth"
	"enclava-go/internal/billing"
	"enclava-go/internal/inference"
	"enclava-go/internal/llm"
	"enclava-go/internal/source"
	"enclava-go/internal/store/postgres"
	"enclava-go/internal/testdb"
)

var fixtureSeq atomic.Int64

// fakeUpstream is a deterministic transport for lifecycle tests — every
// protocol field the executor needs without a real upstream.
type fakeUpstream struct {
	resp      []byte
	err       error
	stream    io.ReadCloser
	streamErr error

	lastBody []byte
	lastPath string
	calls    int32
}

func (f *fakeUpstream) CreateChatCompletion(ctx context.Context, body []byte) ([]byte, error) {
	atomic.AddInt32(&f.calls, 1)
	f.lastBody = body
	return f.resp, f.err
}

func (f *fakeUpstream) CreateChatCompletionStream(ctx context.Context, body []byte) (io.ReadCloser, error) {
	atomic.AddInt32(&f.calls, 1)
	f.lastBody = body
	if f.streamErr != nil {
		return nil, f.streamErr
	}
	return f.stream, f.err
}

func (f *fakeUpstream) CreateEmbeddings(ctx context.Context, body []byte) ([]byte, error) {
	atomic.AddInt32(&f.calls, 1)
	f.lastBody = body
	return f.resp, f.err
}

func (f *fakeUpstream) DoResponsesRequest(ctx context.Context, method, path string, body []byte) ([]byte, error) {
	atomic.AddInt32(&f.calls, 1)
	f.lastBody = body
	f.lastPath = path
	return f.resp, f.err
}

func (f *fakeUpstream) DoResponsesStream(ctx context.Context, method, path string, body []byte) (io.ReadCloser, error) {
	atomic.AddInt32(&f.calls, 1)
	f.lastBody = body
	f.lastPath = path
	if f.streamErr != nil {
		return nil, f.streamErr
	}
	return f.stream, f.err
}

// fakeConversations is a deterministic retained-conversation store.
type fakeConversations map[string]inference.Conversation

func (f fakeConversations) GetConversation(ctx context.Context, tenantID, id string) (inference.Conversation, error) {
	c, ok := f[tenantID+"\x00"+id]
	if !ok {
		return inference.Conversation{}, errors.New("not found")
	}
	return c, nil
}

type fx struct {
	t             *testing.T
	ctx           context.Context
	db            *postgres.DB
	tenant        string
	endpoint      *postgres.EndpointsRepo
	sources       *postgres.SourceRepo
	artifact      *postgres.ArtifactRepo
	billing       *billing.Service
	brepo         *postgres.BillingRepo
	upstream      *fakeUpstream
	svc           *inference.Service
	admission     *inference.Admission
	principal     auth.Principal
	factoryErr    error
	conversations inference.ConversationStore
}

func newFx(t *testing.T) *fx {
	t.Helper()
	db := testdb.New(t)
	ctx := context.Background()
	tenant := testdb.SeedTenant(t, db, fmt.Sprintf("inf-%d", fixtureSeq.Add(1)))
	erepo := postgres.NewEndpointsRepo(db)
	srepo := postgres.NewSourceRepo(db)
	arepo := postgres.NewArtifactRepo(db)
	brepo := postgres.NewBillingRepo(db)
	up := &fakeUpstream{
		resp: []byte(`{"id":"c1","usage":{"prompt_tokens":12,"completion_tokens":3,"total_tokens":15}}`),
	}
	f := &fx{
		t: t, ctx: ctx, db: db, tenant: tenant,
		endpoint: erepo, sources: srepo, artifact: arepo,
		billing: billing.NewService(brepo), brepo: brepo, upstream: up,
	}
	f.principal = auth.Principal{
		TenantID:            tenant,
		ActorID:             "actor-1",
		ActorKind:           auth.ActorKindUser,
		UserID:              "actor-1",
		AllowedModelsAll:    true,
		AllowedEndpointsAll: true,
	}
	f.build(t)
	return f
}

// build (re)creates the executor so tests can flip fixture seams before
// construction.
func (f *fx) build(t *testing.T) {
	t.Helper()
	svc, err := inference.NewService(inference.Deps{
		Endpoints:     f.endpoint,
		Sources:       f.sources,
		Authorizer:    source.NewAuthorizer(f.sources, time.Hour),
		Billing:       f.billing,
		Artifacts:     f.artifact,
		Conversations: f.conversations,
	}, inference.WithTransportFactory(
		func(ctx context.Context, e inference.Endpoint, credential string) (inference.Upstream, error) {
			if f.factoryErr != nil {
				return nil, f.factoryErr
			}
			return f.upstream, nil
		}), inference.WithAdmission(f.admission))
	if err != nil {
		t.Fatalf("new service: %v", err)
	}
	f.svc = svc
}

// endpoint creates the routed endpoint advertising chat plus the alias.
func (f *fx) mkEndpoint(t *testing.T, caps ...string) inference.Endpoint {
	t.Helper()
	e := inference.Endpoint{
		ID: "ep-1", TenantID: f.tenant, Protocol: "openai_compatible",
		BaseOrigin:   "https://api.upstream.example",
		Capabilities: append([]string{inference.CapabilityChat, "model:gpt-fast=gpt-4o-mini"}, caps...),
		Enabled:      true,
	}
	if err := f.endpoint.Create(f.ctx, e); err != nil {
		t.Fatalf("create endpoint: %v", err)
	}
	return e
}

// price installs one endpoint-scoped rate row for the capability on ep-1.
func (f *fx) price(t *testing.T, capability string, unit billing.UnitKind, micros, qty int64) {
	t.Helper()
	f.priceFor(t, "ep-1", capability, unit, micros, qty)
}

// priceFor installs one endpoint-scoped rate row for a given endpoint. An
// input rate also seeds cached input at the same rate: the executor's
// estimates always bound cached input like input, so both rates are required.
func (f *fx) priceFor(t *testing.T, endpointID, capability string, unit billing.UnitKind, micros, qty int64) {
	t.Helper()
	f.putPriceRow(t, endpointID, capability, unit, micros, qty)
	if unit == billing.UnitInput {
		f.putPriceRow(t, endpointID, capability, billing.UnitCachedInput, micros, qty)
	}
}

func (f *fx) putPriceRow(t *testing.T, endpointID, capability string, unit billing.UnitKind, micros, qty int64) {
	t.Helper()
	id := fmt.Sprintf("p-%s-%s-%s-%d", endpointID, capability, unit, fixtureSeq.Add(1))
	if err := f.brepo.PutPrice(f.ctx, f.tenant, id, billing.Price{
		SubjectType: billing.PriceSubjectEndpoint, SubjectID: endpointID,
		Capability: capability, Unit: unit,
		RateMicros: micros, UnitQuantity: qty, Currency: "USD",
	}, "tester", time.Now().UTC().Add(-time.Minute)); err != nil {
		t.Fatalf("put price: %v", err)
	}
}

func (f *fx) flatPrices(t *testing.T) {
	t.Helper()
	f.price(t, inference.CapabilityChat, billing.UnitInput, 1, 1)
	f.price(t, inference.CapabilityChat, billing.UnitOutput, 2, 1)
}

func (f *fx) chatRequest(payload string) inference.Request {
	return inference.Request{
		Capability: inference.CapabilityChat,
		Model:      "gpt-fast",
		Payload:    []byte(payload),
		Estimate:   billing.Usage{InputUnits: 100, OutputUnits: 50},
	}
}

// reservationState reads the canonical reservation row directly.
func (f *fx) reservationState(t *testing.T, reservationID string) string {
	t.Helper()
	var state string
	err := f.db.SQL.QueryRowContext(f.ctx,
		`SELECT state FROM reservations WHERE tenant_id = $1 AND id = $2`,
		f.tenant, reservationID).Scan(&state)
	if err != nil {
		t.Fatalf("reservation %s: %v", reservationID, err)
	}
	return state
}

func (f *fx) reservationCount(t *testing.T) int {
	t.Helper()
	var n int
	err := f.db.SQL.QueryRowContext(f.ctx,
		`SELECT count(DISTINCT id) FROM reservations WHERE tenant_id = $1`,
		f.tenant).Scan(&n)
	if err != nil {
		t.Fatalf("count reservations: %v", err)
	}
	return n
}

func TestExecuteSettlesExactUsage(t *testing.T) {
	f := newFx(t)
	f.mkEndpoint(t)
	f.flatPrices(t)

	res, err := f.svc.Execute(f.ctx, f.principal,
		f.chatRequest(`{"messages":[{"role":"user","content":"hi"}]}`))
	if err != nil {
		t.Fatalf("execute: %v", err)
	}
	if res.Usage == nil {
		t.Fatal("no settlement evidence returned")
	}
	// 12 in × 1µ + 3 out × 2µ = 18µ under the snapshot.
	if res.Usage.AmountMicros != 18 {
		t.Fatalf("settled amount = %d, want 18", res.Usage.AmountMicros)
	}
	if res.Usage.Units[billing.UnitInput] != 12 || res.Usage.Units[billing.UnitOutput] != 3 {
		t.Fatalf("settled units: %+v", res.Usage.Units)
	}
	if f.reservationState(t, res.ReservationID) != billing.ReservationSettled {
		t.Fatal("reservation not settled")
	}
	// The bound upstream model replaced any client model field.
	if !strings.Contains(string(f.upstream.lastBody), `"model":"gpt-4o-mini"`) {
		t.Fatalf("upstream body missing bound model: %s", f.upstream.lastBody)
	}
	if err := res.Body.Close(); err != nil {
		t.Fatal(err)
	}
}

func TestDuplicateSettlementReturnsSameRecord(t *testing.T) {
	f := newFx(t)
	f.mkEndpoint(t)
	f.flatPrices(t)

	res, err := f.svc.Execute(f.ctx, f.principal,
		f.chatRequest(`{"messages":[{"role":"user","content":"hi"}]}`))
	if err != nil {
		t.Fatalf("execute: %v", err)
	}
	first := *res.Usage
	again, err := f.billing.Settle(f.ctx, f.principal, res.ReservationID,
		billing.Usage{InputUnits: 9999})
	if err != nil {
		t.Fatalf("re-settle: %v", err)
	}
	if again.ID != first.ID || again.AmountMicros != first.AmountMicros {
		t.Fatalf("settlement duplicated: %+v", again)
	}
}

func TestMissingPriceBlocksDispatch(t *testing.T) {
	f := newFx(t)
	f.mkEndpoint(t)
	f.price(t, inference.CapabilityChat, billing.UnitInput, 1, 1)
	// Output price deliberately absent.

	_, err := f.svc.Execute(f.ctx, f.principal,
		f.chatRequest(`{"messages":[{"role":"user","content":"hi"}]}`))
	if !errors.Is(err, billing.ErrPriceMissing) {
		t.Fatalf("expected ErrPriceMissing, got %v", err)
	}
	if f.upstream.calls != 0 {
		t.Fatal("missing price must prevent dispatch")
	}
	if n := f.reservationCount(t); n != 0 {
		t.Fatalf("failed reserve left %d reservations", n)
	}
}

// Estimates bound cached input like input, so an endpoint without a
// cached-input rate must refuse dispatch up front: a provider reporting
// cache hits would otherwise strand settlement in unknown liability
// (observed live against Chutes).
func TestMissingCachedInputPriceBlocksDispatch(t *testing.T) {
	f := newFx(t)
	f.mkEndpoint(t)
	f.putPriceRow(t, "ep-1", inference.CapabilityChat, billing.UnitInput, 1, 1)
	f.putPriceRow(t, "ep-1", inference.CapabilityChat, billing.UnitOutput, 2, 1)
	// Cached-input price deliberately absent.

	_, err := f.svc.Execute(f.ctx, f.principal,
		f.chatRequest(`{"messages":[{"role":"user","content":"hi"}]}`))
	if !errors.Is(err, billing.ErrPriceMissing) {
		t.Fatalf("expected ErrPriceMissing, got %v", err)
	}
	if f.upstream.calls != 0 {
		t.Fatal("missing cached-input price must prevent dispatch")
	}
}

func TestFractionalUnitPricing(t *testing.T) {
	f := newFx(t)
	f.mkEndpoint(t)
	// 1.5 USD per 1k tokens: 1500 micros per 1000 units.
	f.price(t, inference.CapabilityChat, billing.UnitInput, 1500, 1000)
	f.price(t, inference.CapabilityChat, billing.UnitOutput, 3000, 1000)

	res, err := f.svc.Execute(f.ctx, f.principal,
		f.chatRequest(`{"messages":[{"role":"user","content":"hi"}]}`))
	if err != nil {
		t.Fatalf("execute: %v", err)
	}
	// 12×1500/1000 = 18; 3×3000/1000 = 9 → 27µ total.
	if res.Usage == nil || res.Usage.AmountMicros != 27 {
		t.Fatalf("fractional charge wrong: %+v", res.Usage)
	}
}

func TestEstimateAboveBoundRejected(t *testing.T) {
	f := newFx(t)
	f.mkEndpoint(t)
	f.flatPrices(t)
	req := f.chatRequest(`{"messages":[{"role":"user","content":"hi"}]}`)
	req.Estimate = billing.Usage{InputUnits: 1<<22 + 1}

	if _, err := f.svc.Execute(f.ctx, f.principal, req); !errors.Is(err, inference.ErrUnbounded) {
		t.Fatalf("expected ErrUnbounded, got %v", err)
	}
	if f.upstream.calls != 0 {
		t.Fatal("unbounded request dispatched")
	}
}

func TestActualOverrunSettlesTruthfully(t *testing.T) {
	f := newFx(t)
	f.mkEndpoint(t)
	f.flatPrices(t)
	f.upstream.resp = []byte(`{"usage":{"prompt_tokens":500,"completion_tokens":100,"total_tokens":600}}`)

	res, err := f.svc.Execute(f.ctx, f.principal,
		f.chatRequest(`{"messages":[{"role":"user","content":"hi"}]}`))
	if err != nil {
		t.Fatalf("execute: %v", err)
	}
	// Overrun above the 100/50 bound records the real amount: 500+200=700µ.
	if res.Usage.AmountMicros != 700 {
		t.Fatalf("overrun clipped: amount=%d", res.Usage.AmountMicros)
	}
}

func TestUpstreamErrorSettlesZero(t *testing.T) {
	f := newFx(t)
	f.mkEndpoint(t)
	f.flatPrices(t)
	f.upstream.err = &llm.UpstreamError{StatusCode: 429, Body: []byte(`{"error":{"message":"rate limited"}}`)}

	_, err := f.svc.Execute(f.ctx, f.principal,
		f.chatRequest(`{"messages":[{"role":"user","content":"hi"}]}`))
	if err == nil {
		t.Fatal("upstream error must surface")
	}
	var count int
	if qerr := f.db.SQL.QueryRowContext(f.ctx,
		`SELECT count(*) FROM usage_records ur JOIN reservations r
		 ON r.id = ur.reservation_id AND r.tenant_id = ur.tenant_id
		 WHERE r.tenant_id = $1`, f.tenant).Scan(&count); qerr != nil {
		t.Fatal(qerr)
	}
	if count != 1 {
		t.Fatalf("definitive upstream error must settle once, got %d records", count)
	}
}

func TestDispatchedFailureRetainsLiability(t *testing.T) {
	f := newFx(t)
	f.mkEndpoint(t)
	f.flatPrices(t)
	f.upstream.err = errors.New("connection reset by peer")

	_, err := f.svc.Execute(f.ctx, f.principal,
		f.chatRequest(`{"messages":[{"role":"user","content":"hi"}]}`))
	if err == nil {
		t.Fatal("transport failure must surface")
	}
	var state string
	if qerr := f.db.SQL.QueryRowContext(f.ctx,
		`SELECT state FROM reservations WHERE tenant_id = $1 LIMIT 1`,
		f.tenant).Scan(&state); qerr != nil {
		t.Fatal(qerr)
	}
	if state != billing.ReservationUnknown {
		t.Fatalf("ambiguous failure must retain liability, state=%s", state)
	}
}

func TestNeverDispatchedFailureReleases(t *testing.T) {
	f := newFx(t)
	f.mkEndpoint(t)
	f.flatPrices(t)
	f.factoryErr = errors.New("transport build failed")

	_, err := f.svc.Execute(f.ctx, f.principal,
		f.chatRequest(`{"messages":[{"role":"user","content":"hi"}]}`))
	if err == nil {
		t.Fatal("transport build failure must surface")
	}
	var state string
	if qerr := f.db.SQL.QueryRowContext(f.ctx,
		`SELECT state FROM reservations WHERE tenant_id = $1 LIMIT 1`,
		f.tenant).Scan(&state); qerr != nil {
		t.Fatal(qerr)
	}
	if state != billing.ReservationReleased {
		t.Fatalf("never-dispatched hold must release, state=%s", state)
	}
}

func TestStreamCompletionSettlesOnce(t *testing.T) {
	f := newFx(t)
	f.mkEndpoint(t)
	f.flatPrices(t)
	f.upstream.stream = io.NopCloser(strings.NewReader(
		"data: {\"choices\":[{\"delta\":{\"content\":\"hi\"}}]}\n\n" +
			"data: {\"usage\":{\"prompt_tokens\":20,\"completion_tokens\":7}}\n\n" +
			"data: [DONE]\n\n"))

	req := f.chatRequest(`{"messages":[{"role":"user","content":"hi"}]}`)
	req.Stream = true
	res, err := f.svc.Execute(f.ctx, f.principal, req)
	if err != nil {
		t.Fatalf("execute: %v", err)
	}
	out, err := io.ReadAll(res.Body)
	if err != nil {
		t.Fatal(err)
	}
	if !strings.Contains(string(out), "[DONE]") {
		t.Fatal("stream body not forwarded")
	}
	if f.reservationState(t, res.ReservationID) != billing.ReservationSettled {
		t.Fatalf("stream not settled: %s", f.reservationState(t, res.ReservationID))
	}
	var amount int64
	if qerr := f.db.SQL.QueryRowContext(f.ctx,
		`SELECT amount_micros FROM usage_records WHERE tenant_id = $1 AND reservation_id = $2`,
		f.tenant, res.ReservationID).Scan(&amount); qerr != nil {
		t.Fatal(qerr)
	}
	if amount != 20+2*7 {
		t.Fatalf("stream settled amount=%d, want 34", amount)
	}
	// Caller-facing body carries include_usage for settlement observability.
	if !strings.Contains(string(f.upstream.lastBody), `"include_usage":true`) {
		t.Fatalf("stream_options not injected: %s", f.upstream.lastBody)
	}
}

func TestStreamEarlyCloseRetainsLiability(t *testing.T) {
	f := newFx(t)
	f.mkEndpoint(t)
	f.flatPrices(t)
	f.upstream.stream = io.NopCloser(strings.NewReader(
		"data: {\"choices\":[{\"delta\":{\"content\":\"hi\"}}]}\n\n"))

	req := f.chatRequest(`{"messages":[{"role":"user","content":"hi"}]}`)
	req.Stream = true
	res, err := f.svc.Execute(f.ctx, f.principal, req)
	if err != nil {
		t.Fatalf("execute: %v", err)
	}
	// Caller abandons mid-stream: Close must retain unknown liability.
	if err := res.Body.Close(); err != nil {
		t.Fatal(err)
	}
	if f.reservationState(t, res.ReservationID) != billing.ReservationUnknown {
		t.Fatalf("cancelled stream must retain liability: %s",
			f.reservationState(t, res.ReservationID))
	}
}

func TestEmbeddingsDispatch(t *testing.T) {
	f := newFx(t)
	f.mkEndpoint(t, inference.CapabilityEmbeddings)
	f.upstream.resp = []byte(`{"data":[{"embedding":[0.1]}],"usage":{"prompt_tokens":9,"total_tokens":9}}`)
	f.price(t, inference.CapabilityEmbeddings, billing.UnitInput, 4, 1)

	res, err := f.svc.Execute(f.ctx, f.principal, inference.Request{
		Capability: inference.CapabilityEmbeddings,
		Model:      "gpt-fast",
		Payload:    []byte(`{"input":"hello"}`),
		Estimate:   billing.Usage{InputUnits: 100},
	})
	if err != nil {
		t.Fatalf("execute: %v", err)
	}
	if res.Usage == nil || res.Usage.AmountMicros != 36 {
		t.Fatalf("embedding usage wrong: %+v", res.Usage)
	}
}

func TestResponsesDispatchAndPathBound(t *testing.T) {
	f := newFx(t)
	f.mkEndpoint(t, inference.CapabilityResponses)
	f.price(t, inference.CapabilityResponses, billing.UnitInput, 1, 1)
	f.price(t, inference.CapabilityResponses, billing.UnitOutput, 1, 1)
	f.upstream.resp = []byte(`{"id":"resp_1","usage":{"input_tokens":5,"output_tokens":2}}`)

	res, err := f.svc.Execute(f.ctx, f.principal, inference.Request{
		Capability: inference.CapabilityResponses,
		Model:      "gpt-fast",
		Payload:    []byte(`{"input":"hi"}`),
		Estimate:   billing.Usage{InputUnits: 100, OutputUnits: 50},
	})
	if err != nil {
		t.Fatalf("execute: %v", err)
	}
	if f.upstream.lastPath != "/responses" {
		t.Fatalf("responses path = %q", f.upstream.lastPath)
	}
	if res.Usage == nil || res.Usage.AmountMicros != 7 {
		t.Fatalf("responses usage wrong: %+v", res.Usage)
	}

	// An escape path is denied outright.
	_, err = f.svc.Execute(f.ctx, f.principal, inference.Request{
		Capability: inference.CapabilityResponses,
		Model:      "gpt-fast",
		Path:       "/responses/../admin",
		Estimate:   billing.Usage{InputUnits: 10, OutputUnits: 10},
	})
	if !errors.Is(err, inference.ErrDenied) {
		t.Fatalf("path escape must deny: %v", err)
	}
}

func TestArtifactInputResolvedFromAuthoritativeStore(t *testing.T) {
	f := newFx(t)
	f.mkEndpoint(t)
	f.flatPrices(t)

	// A finalized artifact row is an admissible input.
	row := artifacts.ArtifactRow{
		TenantID: f.tenant, ID: "art-1", Kind: artifacts.KindOriginal, Version: 1,
		State: "writing", StoragePath: "a/b", LeaseOwner: "w1",
		FenceToken: 7, SHA256: strings.Repeat("a", 64), ByteSize: 12,
	}
	if err := f.artifact.BeginInsert(f.ctx, row); err != nil {
		t.Fatalf("begin artifact: %v", err)
	}
	if err := f.artifact.FinalizeCommit(f.ctx, f.tenant, "art-1", "w1", 7,
		row.SHA256, row.ByteSize); err != nil {
		t.Fatalf("finalize artifact: %v", err)
	}
	req := f.chatRequest(`{"messages":[{"role":"user","content":"hi"}]}`)
	req.Artifacts = []artifacts.Ref{{TenantID: f.tenant, ID: "art-1", Kind: artifacts.KindOriginal, Version: 1}}
	if _, err := f.svc.Execute(f.ctx, f.principal, req); err != nil {
		t.Fatalf("artifact input execute: %v", err)
	}

	// A forged SHA256 relabel denies.
	req.Artifacts = []artifacts.Ref{{TenantID: f.tenant, ID: "art-1",
		SHA256: strings.Repeat("b", 64)}}
	if _, err := f.svc.Execute(f.ctx, f.principal, req); !errors.Is(err, inference.ErrDenied) {
		t.Fatalf("forged artifact ref must deny: %v", err)
	}

	// An unknown artifact id denies identically.
	req.Artifacts = []artifacts.Ref{{TenantID: f.tenant, ID: "nope"}}
	if _, err := f.svc.Execute(f.ctx, f.principal, req); !errors.Is(err, inference.ErrDenied) {
		t.Fatalf("unknown artifact must deny: %v", err)
	}
}

func TestNoReservationWithoutDispatch(t *testing.T) {
	f := newFx(t)
	// No endpoint at all: routing fails before any hold exists.
	_, err := f.svc.Execute(f.ctx, f.principal,
		f.chatRequest(`{"messages":[{"role":"user","content":"hi"}]}`))
	if !errors.Is(err, inference.ErrNoRoute) {
		t.Fatalf("expected ErrNoRoute, got %v", err)
	}
	if n := f.reservationCount(t); n != 0 {
		t.Fatalf("no-route left %d reservations", n)
	}
	if f.upstream.calls != 0 {
		t.Fatal("no-route dispatched upstream")
	}
}

// fakeAttestingUpstream adds an attestation path so verifiable endpoints can
// exercise the verification+stamp lifecycle without a real enclave.
type fakeAttestingUpstream struct {
	fakeUpstream
	verifyErr error
	verified  int32
}

func (f *fakeAttestingUpstream) VerifyAttestation(ctx context.Context) (llm.AttestationStatus, error) {
	atomic.AddInt32(&f.verified, 1)
	if f.verifyErr != nil {
		return llm.AttestationStatus{}, f.verifyErr
	}
	return llm.AttestationStatus{Enabled: true, Verified: true, VerifiedAt: time.Now()}, nil
}

func TestConfidentialEndpointVerifiesBeforeDispatch(t *testing.T) {
	f := newFx(t)
	f.obj(t, "conf-1")
	if err := f.sources.SetRestriction(f.ctx, source.SourceRestriction{
		TenantID: f.tenant, SubjectKind: source.RestrictionObject, SubjectID: "conf-1",
		RequiredAssurance: "high"}); err != nil {
		t.Fatalf("set restriction: %v", err)
	}
	// A tinfoil endpoint with no persisted verification is eligible only
	// because its protocol can prove assurance at dispatch.
	e := inference.Endpoint{
		ID: "ep-enc", TenantID: f.tenant, Protocol: "tinfoil",
		BaseOrigin:   "https://enclave.example",
		Capabilities: []string{inference.CapabilityChat, "model:gpt-fast=gpt-4o-mini"},
		Enabled:      true,
	}
	if err := f.endpoint.Create(f.ctx, e); err != nil {
		t.Fatalf("create endpoint: %v", err)
	}
	f.priceFor(t, "ep-enc", inference.CapabilityChat, billing.UnitInput, 1, 1)
	f.priceFor(t, "ep-enc", inference.CapabilityChat, billing.UnitOutput, 1, 1)

	att := &fakeAttestingUpstream{}
	att.resp = []byte(`{"usage":{"prompt_tokens":5,"completion_tokens":2}}`)
	svc, err := inference.NewService(inference.Deps{
		Endpoints: f.endpoint, Sources: f.sources,
		Authorizer: source.NewAuthorizer(f.sources, time.Hour),
		Billing:    f.billing, Artifacts: f.artifact,
	}, inference.WithTransportFactory(
		func(ctx context.Context, e inference.Endpoint, credential string) (inference.Upstream, error) {
			return att, nil
		}))
	if err != nil {
		t.Fatal(err)
	}

	req := f.chatRequest(`{"messages":[{"role":"user","content":"hi"}]}`)
	req.EndpointID = "ep-enc"
	req.Inputs = []source.ObjectRef{{ObjectID: "conf-1", ObservedSeq: 1}}
	res, err := svc.Execute(f.ctx, f.principal, req)
	if err != nil {
		t.Fatalf("verified-confidential execute: %v", err)
	}
	defer res.Body.Close()
	if atomic.LoadInt32(&att.verified) == 0 {
		t.Fatal("dispatch happened without attestation")
	}
	// Observed verification is stamped on the endpoint record.
	got, err := f.endpoint.Get(f.ctx, f.tenant, "ep-enc")
	if err != nil {
		t.Fatal(err)
	}
	if got.AssuranceVerifiedAt == nil {
		t.Fatal("verification timestamp not persisted")
	}

	// A failed attestation retains the reservation as never-dispatched.
	att.verifyErr = errors.New("attestation failed")
	// Rebuild signature so the transport cache misses and re-verifies.
	e2 := got
	e2.CredentialRef = "cred-2"
	if err := f.endpoint.Update(f.ctx, e2); err != nil {
		t.Fatal(err)
	}
	if _, err := svc.Execute(f.ctx, f.principal, req); err == nil {
		t.Fatal("failed attestation must block dispatch")
	}
	var state string
	if qerr := f.db.SQL.QueryRowContext(f.ctx,
		`SELECT state FROM reservations WHERE tenant_id = $1 ORDER BY created_at DESC LIMIT 1`,
		f.tenant).Scan(&state); qerr != nil {
		t.Fatal(qerr)
	}
	if state != billing.ReservationReleased {
		t.Fatalf("unverified attempt must release, got %s", state)
	}
	if atomic.LoadInt32(&att.calls) != 1 {
		t.Fatalf("attestation failure reached transport calls=%d", att.calls)
	}
}

// TestRealAdapterEndToEnd dispatches through the actual
// OpenAI-compatible transport (httptest server as an authorized-private
// customer-local endpoint) so the HTTP path, guarded dial and usage settle
// run for real.
func TestRealAdapterEndToEnd(t *testing.T) {
	f := newFx(t)

	srv := httptest.NewServer(nil)
	srv.Config.Handler = http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		if r.URL.Path != "/v1/chat/completions" {
			t.Errorf("unexpected upstream path %s", r.URL.Path)
		}
		body, _ := io.ReadAll(r.Body)
		if !strings.Contains(string(body), `"model":"gpt-4o-mini"`) {
			t.Errorf("bound model missing upstream: %s", body)
		}
		w.Header().Set("Content-Type", "application/json")
		_, _ = w.Write([]byte(`{"id":"c1","usage":{"prompt_tokens":8,"completion_tokens":2,"total_tokens":10}}`))
	})
	defer srv.Close()

	e := inference.Endpoint{
		ID: "ep-local", TenantID: f.tenant, Protocol: "openai_compatible",
		BaseOrigin:        srv.URL,
		Capabilities:      []string{inference.CapabilityChat, "model:gpt-fast=gpt-4o-mini"},
		AuthorizedPrivate: true, Enabled: true,
	}
	if err := f.endpoint.Create(f.ctx, e); err != nil {
		t.Fatalf("create endpoint: %v", err)
	}
	f.priceFor(t, "ep-local", inference.CapabilityChat, billing.UnitInput, 1, 1)
	f.priceFor(t, "ep-local", inference.CapabilityChat, billing.UnitOutput, 1, 1)

	svc, err := inference.NewService(inference.Deps{
		Endpoints: f.endpoint, Sources: f.sources,
		Authorizer: source.NewAuthorizer(f.sources, time.Hour),
		Billing:    f.billing, Artifacts: f.artifact,
	})
	if err != nil {
		t.Fatal(err)
	}
	req := f.chatRequest(`{"messages":[{"role":"user","content":"hi"}]}`)
	req.EndpointID = "ep-local"
	res, err := svc.Execute(f.ctx, f.principal, req)
	if err != nil {
		t.Fatalf("real adapter execute: %v", err)
	}
	defer res.Body.Close()
	if res.Usage == nil || res.Usage.AmountMicros != 10 {
		t.Fatalf("real dispatch settlement wrong: %+v", res.Usage)
	}
	if f.reservationState(t, res.ReservationID) != billing.ReservationSettled {
		t.Fatal("reservation not settled after real dispatch")
	}
}

func TestExecuteConfidentialSDKOfflineDemo(t *testing.T) {
	if strings.TrimSpace(os.Getenv("CONFIDENTIAL_INFERENCE_FFI_LIBRARY")) == "" {
		t.Fatal("CONFIDENTIAL_INFERENCE_FFI_LIBRARY is required for this integration test")
	}
	f := newFx(t)
	e := inference.Endpoint{
		ID: "ep-sdk", TenantID: f.tenant, Protocol: "confidential_sdk", SDKProvider: "demo",
		BaseOrigin:   "https://demo.example",
		Capabilities: []string{inference.CapabilityChat, "model:gpt-oss-120b=gpt-oss-120b"},
		Enabled:      true,
	}
	if err := f.endpoint.Create(f.ctx, e); err != nil {
		t.Fatalf("create SDK endpoint: %v", err)
	}
	f.priceFor(t, e.ID, inference.CapabilityChat, billing.UnitInput, 1, 1)
	f.priceFor(t, e.ID, inference.CapabilityChat, billing.UnitOutput, 1, 1)
	svc, err := inference.NewService(inference.Deps{
		Endpoints: f.endpoint, Sources: f.sources, Authorizer: source.NewAuthorizer(f.sources, time.Hour),
		Billing: f.billing, Artifacts: f.artifact,
	})
	if err != nil {
		t.Fatalf("new SDK inference service: %v", err)
	}
	defer svc.Close()
	result, err := svc.Execute(f.ctx, f.principal, inference.Request{
		Capability: inference.CapabilityChat, Model: "gpt-oss-120b",
		Payload:  []byte(`{"messages":[{"role":"user","content":"hi"}]}`),
		Estimate: billing.Usage{InputUnits: 100, OutputUnits: 50},
	})
	if err != nil {
		t.Fatalf("SDK execute: %v", err)
	}
	defer result.Body.Close()
	body, err := io.ReadAll(result.Body)
	if err != nil {
		t.Fatalf("read SDK response: %v", err)
	}
	if !strings.Contains(string(body), `"choices"`) || !strings.Contains(string(body), `"usage"`) {
		t.Fatalf("SDK response is not OpenAI-shaped: %s", body)
	}
	if result.Usage == nil || f.reservationState(t, result.ReservationID) != billing.ReservationSettled {
		t.Fatalf("SDK request did not settle usage: %+v", result.Usage)
	}
	stored, err := f.endpoint.Get(f.ctx, f.tenant, e.ID)
	if err != nil || stored.AssuranceVerifiedAt == nil {
		t.Fatalf("SDK verified assurance was not stamped: endpoint=%+v err=%v", stored, err)
	}
}

func TestExecuteConfidentialSDKStreamFailsClosedForE2EERoute(t *testing.T) {
	if strings.TrimSpace(os.Getenv("CONFIDENTIAL_INFERENCE_FFI_LIBRARY")) == "" {
		t.Fatal("CONFIDENTIAL_INFERENCE_FFI_LIBRARY is required for this integration test")
	}
	f := newFx(t)
	e := inference.Endpoint{
		ID: "ep-sdk-stream", TenantID: f.tenant, Protocol: "confidential_sdk", SDKProvider: "demo",
		BaseOrigin:   "https://demo.example",
		Capabilities: []string{inference.CapabilityChat, "model:gpt-oss-120b=gpt-oss-120b"},
		Enabled:      true,
	}
	if err := f.endpoint.Create(f.ctx, e); err != nil {
		t.Fatalf("create SDK endpoint: %v", err)
	}
	f.priceFor(t, e.ID, inference.CapabilityChat, billing.UnitInput, 1, 1)
	f.priceFor(t, e.ID, inference.CapabilityChat, billing.UnitOutput, 1, 1)
	svc, err := inference.NewService(inference.Deps{
		Endpoints: f.endpoint, Sources: f.sources, Authorizer: source.NewAuthorizer(f.sources, time.Hour),
		Billing: f.billing, Artifacts: f.artifact,
	})
	if err != nil {
		t.Fatalf("new SDK inference service: %v", err)
	}
	defer svc.Close()
	before := f.reservationCount(t)
	_, err = svc.Execute(f.ctx, f.principal, inference.Request{
		Capability: inference.CapabilityChat, Model: "gpt-oss-120b", Stream: true,
		Payload:  []byte(`{"messages":[{"role":"user","content":"hi"}]}`),
		Estimate: billing.Usage{InputUnits: 100, OutputUnits: 50},
	})
	if err == nil || !strings.Contains(err.Error(), "streaming_not_supported") {
		t.Fatalf("E2EE SDK stream must fail closed, got %v", err)
	}
	if got := f.reservationCount(t); got != before+1 {
		t.Fatalf("stream denial reservations = %d, want one attempt", got-before)
	}
	var state string
	if err := f.db.SQL.QueryRowContext(f.ctx,
		`SELECT state FROM reservations WHERE tenant_id = $1 ORDER BY created_at DESC LIMIT 1`, f.tenant).Scan(&state); err != nil {
		t.Fatal(err)
	}
	if state != billing.ReservationReleased && state != billing.ReservationUnknown {
		t.Fatalf("E2EE stream denial state = %s, want released pre-dispatch or unknown after dispatch", state)
	}
	var settlements int
	if err := f.db.SQL.QueryRowContext(f.ctx,
		`SELECT count(*) FROM usage_records WHERE tenant_id = $1`, f.tenant).Scan(&settlements); err != nil {
		t.Fatal(err)
	}
	if settlements != 0 {
		t.Fatalf("fail-closed stream recorded %d settlements, want none", settlements)
	}
}
func TestExecuteConfidentialSDKOfflineDemoEmbeddings(t *testing.T) {
	if strings.TrimSpace(os.Getenv("CONFIDENTIAL_INFERENCE_FFI_LIBRARY")) == "" {
		t.Fatal("CONFIDENTIAL_INFERENCE_FFI_LIBRARY is required for this integration test")
	}
	f := newFx(t)
	e := inference.Endpoint{
		ID: "ep-sdk-embed", TenantID: f.tenant, Protocol: "confidential_sdk", SDKProvider: "demo",
		BaseOrigin:   "https://demo.example",
		Capabilities: []string{inference.CapabilityEmbeddings, "model:gpt-oss-120b=gpt-oss-120b"},
		Enabled:      true,
	}
	if err := f.endpoint.Create(f.ctx, e); err != nil {
		t.Fatalf("create SDK embeddings endpoint: %v", err)
	}
	f.priceFor(t, e.ID, inference.CapabilityEmbeddings, billing.UnitInput, 1, 1)
	svc, err := inference.NewService(inference.Deps{
		Endpoints: f.endpoint, Sources: f.sources, Authorizer: source.NewAuthorizer(f.sources, time.Hour),
		Billing: f.billing, Artifacts: f.artifact,
	})
	if err != nil {
		t.Fatalf("new SDK inference service: %v", err)
	}
	defer svc.Close()
	result, err := svc.Execute(f.ctx, f.principal, inference.Request{
		Capability: inference.CapabilityEmbeddings, Model: "gpt-oss-120b",
		Payload:  []byte(`{"input":["alpha","beta"]}`),
		Estimate: billing.Usage{InputUnits: 100},
	})
	if err != nil {
		t.Fatalf("SDK embeddings execute: %v", err)
	}
	defer result.Body.Close()
	body, err := io.ReadAll(result.Body)
	if err != nil {
		t.Fatalf("read SDK embeddings response: %v", err)
	}
	if !strings.Contains(string(body), `"embedding"`) || !strings.Contains(string(body), `"usage"`) {
		t.Fatalf("SDK embeddings response is not OpenAI-shaped: %s", body)
	}
	if result.Usage == nil || f.reservationState(t, result.ReservationID) != billing.ReservationSettled {
		t.Fatalf("SDK embeddings did not settle usage: %+v", result.Usage)
	}
	stored, err := f.endpoint.Get(f.ctx, f.tenant, e.ID)
	if err != nil || stored.AssuranceVerifiedAt == nil {
		t.Fatalf("SDK embeddings assurance not stamped: endpoint=%+v err=%v", stored, err)
	}
}

func TestExecuteConfidentialSDKOfflineDemoResponsesLifecycle(t *testing.T) {
	if strings.TrimSpace(os.Getenv("CONFIDENTIAL_INFERENCE_FFI_LIBRARY")) == "" {
		t.Fatal("CONFIDENTIAL_INFERENCE_FFI_LIBRARY is required for this integration test")
	}
	f := newFx(t)
	e := inference.Endpoint{
		ID: "ep-sdk-responses", TenantID: f.tenant, Protocol: "confidential_sdk", SDKProvider: "demo",
		BaseOrigin:   "https://demo.example",
		Capabilities: []string{inference.CapabilityResponses, "model:gpt-oss-120b=gpt-oss-120b"},
		Enabled:      true,
	}
	if err := f.endpoint.Create(f.ctx, e); err != nil {
		t.Fatalf("create SDK endpoint: %v", err)
	}
	f.priceFor(t, e.ID, inference.CapabilityResponses, billing.UnitInput, 1, 1)
	f.priceFor(t, e.ID, inference.CapabilityResponses, billing.UnitOutput, 1, 1)
	svc, err := inference.NewService(inference.Deps{
		Endpoints: f.endpoint, Sources: f.sources, Authorizer: source.NewAuthorizer(f.sources, time.Hour),
		Billing: f.billing, Artifacts: f.artifact,
	})
	if err != nil {
		t.Fatalf("new SDK inference service: %v", err)
	}
	defer svc.Close()
	result, err := svc.Execute(f.ctx, f.principal, inference.Request{
		Capability: inference.CapabilityResponses, Model: "gpt-oss-120b",
		Payload:  []byte(`{"model":"gpt-oss-120b","input":"hello"}`),
		Estimate: billing.Usage{InputUnits: 100, OutputUnits: 50},
	})
	if err != nil {
		t.Fatalf("SDK Responses create: %v", err)
	}
	defer result.Body.Close()
	raw, err := io.ReadAll(result.Body)
	if err != nil || !strings.Contains(string(raw), `"object":"response"`) {
		t.Fatalf("SDK Responses create did not return a response object: %s err=%v", raw, err)
	}
	if result.ReservationID == "" {
		t.Fatal("SDK Responses create bypassed the priced executor")
	}
	manage, err := svc.Manage(f.ctx, f.principal, inference.ManageRequest{
		EndpointID: e.ID, Method: "GET", Path: "/responses/resp_demo",
	})
	if manage != nil {
		_ = manage.Body.Close()
	}
	if err == nil || !strings.Contains(err.Error(), "provider_response_manage_failed") {
		t.Fatalf("demo Responses follow-up must fail closed, got %v", err)
	}
}
