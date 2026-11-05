// service_test.go exercises the executor's dispatch and accounting lifecycle
// against real PostgreSQL: atomic reservations before dispatch, exact
// once-only settlement under the immutable rate snapshot, retained unknown
// liability, and the verified never-dispatched release (JOB-03/JOB-04/INF-02).
package inference_test

import (
	"bytes"
	"context"
	"encoding/json"
	"errors"
	"fmt"
	"io"
	"net"
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

	// Discovery seams: per-capability probe outcomes and the model list.
	models       []llm.Model
	embedErr     error
	responsesErr error
	visionErr    error
	compactErr   error

	lastBody []byte
	lastPath string
	calls    int32
	// onChat intercepts CreateChatCompletion mid-dispatch — tests use it to
	// cancel the caller's context or probe the dispatch context.
	onChat func(ctx context.Context) error
	// onStream intercepts CreateChatCompletionStream mid-open — tests use
	// it to hold the open and probe the open-phase context.
	onStream func(ctx context.Context) error
}

func (f *fakeUpstream) CreateChatCompletion(ctx context.Context, body []byte) ([]byte, error) {
	atomic.AddInt32(&f.calls, 1)
	f.lastBody = body
	if f.onChat != nil {
		if err := f.onChat(ctx); err != nil {
			return nil, err
		}
	}
	if f.visionErr != nil && bytes.Contains(body, []byte("image_url")) {
		return nil, f.visionErr
	}
	return f.resp, f.err
}

func (f *fakeUpstream) CreateChatCompletionStream(ctx context.Context, body []byte) (io.ReadCloser, error) {
	atomic.AddInt32(&f.calls, 1)
	f.lastBody = body
	if f.onStream != nil {
		if err := f.onStream(ctx); err != nil {
			return nil, err
		}
	}
	if f.streamErr != nil {
		return nil, f.streamErr
	}
	return f.stream, f.err
}

func (f *fakeUpstream) CreateEmbeddings(ctx context.Context, body []byte) ([]byte, error) {
	atomic.AddInt32(&f.calls, 1)
	f.lastBody = body
	if f.embedErr != nil {
		return nil, f.embedErr
	}
	return f.resp, f.err
}

func (f *fakeUpstream) DoResponsesRequest(ctx context.Context, method, path string, body []byte) ([]byte, error) {
	atomic.AddInt32(&f.calls, 1)
	f.lastBody = body
	f.lastPath = path
	if path == "/responses/compact" && f.compactErr != nil {
		return nil, f.compactErr
	}
	if f.responsesErr != nil {
		return nil, f.responsesErr
	}
	return f.resp, f.err
}

func (f *fakeUpstream) ListModels(ctx context.Context) (llm.ModelsResponse, error) {
	return llm.ModelsResponse{Data: f.models}, nil
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
	t                *testing.T
	ctx              context.Context
	db               *postgres.DB
	tenant           string
	endpoint         *postgres.EndpointsRepo
	sources          *postgres.SourceRepo
	artifact         *postgres.ArtifactRepo
	billing          *billing.Service
	brepo            *postgres.BillingRepo
	upstream         *fakeUpstream
	upstreamOverride inference.Upstream
	svc              *inference.Service
	admission        *inference.Admission
	principal        auth.Principal
	factoryErr       error
	conversations    inference.ConversationStore
	// billingOverride swaps the executor's billing seam (tests exercising
	billingOverride inference.Billing
	// transportHook runs inside the transport factory before the fake is
	// returned — tests use it to gate concurrent construction.
	transportHook func()
	// modelSettings enables availability/default/fallback when set before build.
	modelSettings inference.ModelSettingsRepository
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
	billingSeam := inference.Billing(f.billing)
	if f.billingOverride != nil {
		billingSeam = f.billingOverride
	}
	svc, err := inference.NewService(inference.Deps{
		Endpoints:     f.endpoint,
		Sources:       f.sources,
		Authorizer:    source.NewAuthorizer(f.sources),
		Billing:       billingSeam,
		Artifacts:     f.artifact,
		Conversations: f.conversations,
		ModelSettings: f.modelSettings,
	}, inference.WithTransportFactory(
		func(ctx context.Context, e inference.Endpoint, credential string) (inference.Upstream, error) {
			if f.transportHook != nil {
				f.transportHook()
			}
			if f.factoryErr != nil {
				return nil, f.factoryErr
			}
			if f.upstreamOverride != nil {
				return f.upstreamOverride, nil
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

// waitReservationState polls the canonical row until it reaches want —
// stream accounting may still be draining after Body.Close returns.
func (f *fx) waitReservationState(t *testing.T, reservationID, want string) string {
	t.Helper()
	deadline := time.Now().Add(10 * time.Second)
	for time.Now().Before(deadline) {
		var state string
		if err := f.db.SQL.QueryRowContext(f.ctx,
			`SELECT state FROM reservations WHERE tenant_id = $1 AND id = $2`,
			f.tenant, reservationID).Scan(&state); err == nil && state == want {
			return state
		}
		time.Sleep(10 * time.Millisecond)
	}
	return f.reservationState(t, reservationID)
}

// reservationUnits reads the held per-kind unit bound of the canonical row.
func (f *fx) reservationUnits(t *testing.T, reservationID string) map[string]int64 {
	t.Helper()
	var raw []byte
	if err := f.db.SQL.QueryRowContext(f.ctx,
		`SELECT reserved_units FROM reservations WHERE tenant_id = $1 AND id = $2`,
		f.tenant, reservationID).Scan(&raw); err != nil {
		t.Fatalf("reservation units: %v", err)
	}
	var units map[string]int64
	if err := json.Unmarshal(raw, &units); err != nil {
		t.Fatalf("decode units: %v", err)
	}
	return units
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

type failingRefusalAuditBilling struct{ inference.Billing }

func (b *failingRefusalAuditBilling) RecordRequestRefused(context.Context, auth.Principal, string, string, string) error {
	return errors.New("audit store unavailable")
}

func TestRequestRefusalAuditFailureIsSurfaced(t *testing.T) {
	f := newFx(t)
	f.mkEndpoint(t)
	f.billingOverride = &failingRefusalAuditBilling{Billing: f.billing}
	f.build(t)
	_, err := f.svc.Execute(f.ctx, f.principal, f.chatRequest(`{"messages":[{"role":"user","content":"hi"}]}`))
	if err == nil || !strings.Contains(err.Error(), "refusal audit unavailable") || errors.Is(err, billing.ErrPriceMissing) {
		t.Fatalf("audit failure must surface as an internal error, got %v", err)
	}
	if atomic.LoadInt32(&f.upstream.calls) != 0 {
		t.Fatal("refused request reached upstream")
	}
}

func TestRequestRefusalAudit(t *testing.T) {
	for _, tc := range []struct {
		name       string
		setup      string
		wantErr    error
		wantReason string
		key        bool
	}{
		{name: "unpriced model", wantErr: billing.ErrPriceMissing, wantReason: billing.RefusalPriceMissing},
		{name: "unpriced unit", setup: "partial", wantErr: billing.ErrPriceMissing, wantReason: billing.RefusalPriceMissing},
		{name: "budget", setup: "budget", wantErr: billing.ErrBudgetExceeded, wantReason: billing.RefusalBudgetExceeded, key: true},
		{name: "budget currency", setup: "currency", wantErr: billing.ErrCurrencyMismatch, wantReason: billing.RefusalBudgetCurrencyMismatch, key: true},
		{name: "successful model fallback", setup: "model"},
	} {
		t.Run(tc.name, func(t *testing.T) {
			f := newFx(t)
			f.mkEndpoint(t)
			p := f.principal
			if tc.key {
				p = keyPrincipal(f.tenant, "key-1", p.ActorID)
			}
			switch tc.setup {
			case "partial":
				f.price(t, inference.CapabilityChat, billing.UnitInput, 1, 1)
			case "budget", "currency":
				f.flatPrices(t)
				currency := "USD"
				if tc.setup == "currency" {
					currency = "EUR"
				}
				if err := f.brepo.CreateBudget(f.ctx, f.tenant, "budget-refusal", billing.Budget{
					Subject: billing.Subject{Type: billing.BudgetSubjectAPIKey, ID: p.CredentialID},
					Kind:    billing.BudgetKindAmount, Period: billing.PeriodMonthly,
					LimitAmountMicros: 1, Currency: currency, StartsAt: time.Now().UTC().Add(-time.Hour),
				}); err != nil {
					t.Fatal(err)
				}
			case "model":
				for _, unit := range []billing.UnitKind{billing.UnitInput, billing.UnitCachedInput, billing.UnitOutput} {
					if err := f.brepo.PutPrice(f.ctx, f.tenant, "model-"+string(unit), billing.Price{
						SubjectType: billing.PriceSubjectModel, SubjectID: "gpt-4o-mini",
						Capability: inference.CapabilityChat, Unit: unit,
						RateMicros: 1, UnitQuantity: 1, Currency: "USD",
					}, p.ActorID, time.Now().UTC().Add(-time.Minute)); err != nil {
						t.Fatal(err)
					}
				}
			}
			_, err := f.svc.Execute(f.ctx, p, f.chatRequest(`{"messages":[{"role":"user","content":"private prompt"}],"max_tokens":5}`))
			if !errors.Is(err, tc.wantErr) {
				t.Fatalf("execute error = %v, want %v", err, tc.wantErr)
			}
			rows, err := f.brepo.ListBillingAudit(f.ctx, f.tenant, "", "", 100, 0)
			if err != nil {
				t.Fatal(err)
			}
			if tc.wantReason == "" {
				if len(rows) != 0 {
					t.Fatalf("successful request created audit rows: %+v", rows)
				}
				return
			}
			if len(rows) != 1 {
				t.Fatalf("audit rows = %+v, want one refusal", rows)
			}
			row := rows[0]
			subjectType, subjectID := billing.BudgetSubjectUser, p.ActorID
			if tc.key {
				subjectType, subjectID = billing.BudgetSubjectAPIKey, p.CredentialID
			}
			if row.Action != billing.AuditRequestRefused || row.SubjectType != subjectType || row.SubjectID != subjectID ||
				row.ActorType != string(p.ActorKind) || row.ActorID != p.ActorID ||
				len(row.Detail) != 3 || row.Detail["reason"] != tc.wantReason ||
				row.Detail["model"] != "gpt-4o-mini" || row.Detail["capability"] != inference.CapabilityChat {
				t.Fatalf("unexpected refusal audit: %+v", row)
			}
			if atomic.LoadInt32(&f.upstream.calls) != 0 {
				t.Fatal("refused request reached upstream")
			}
		})
	}
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
	// The transport resolves before reserving now: a build failure leaves
	// no reservation at all.
	if n := f.reservationCount(t); n != 0 {
		t.Fatalf("transport build failure left %d reservations", n)
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
	// Caller abandons mid-stream: Close returns promptly while the drain
	// keeps reading; this body ends without a terminal event so liability
	// stays unknown once the drain finishes.
	if err := res.Body.Close(); err != nil {
		t.Fatal(err)
	}
	if got := f.waitReservationState(t, res.ReservationID, billing.ReservationUnknown); got != billing.ReservationUnknown {
		t.Fatalf("cancelled stream must retain liability: %s", got)
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
		Authorizer: source.NewAuthorizer(f.sources),
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
	defer func() { _ = res.Body.Close() }()
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
	// A failed attestation blocks dispatch before any hold exists: the
	// transport is resolved (and attests) before reserving.
	att.verifyErr = errors.New("attestation failed")
	// Rebuild signature so the transport cache misses and re-verifies.
	e2 := got
	e2.CredentialRef = "cred-2"
	if err := f.endpoint.Update(f.ctx, e2); err != nil {
		t.Fatal(err)
	}
	before := f.reservationCount(t)
	if _, err := svc.Execute(f.ctx, f.principal, req); err == nil {
		t.Fatal("failed attestation must block dispatch")
	}
	if n := f.reservationCount(t); n != before {
		t.Fatalf("attestation failure created %d reservations", n-before)
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
		Authorizer: source.NewAuthorizer(f.sources),
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
	defer func() { _ = res.Body.Close() }()
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
		Endpoints: f.endpoint, Sources: f.sources, Authorizer: source.NewAuthorizer(f.sources),
		Billing: f.billing, Artifacts: f.artifact,
	})
	if err != nil {
		t.Fatalf("new SDK inference service: %v", err)
	}
	defer func() { _ = svc.Close() }()
	result, err := svc.Execute(f.ctx, f.principal, inference.Request{
		Capability: inference.CapabilityChat, Model: "gpt-oss-120b",
		Payload:  []byte(`{"messages":[{"role":"user","content":"hi"}]}`),
		Estimate: billing.Usage{InputUnits: 100, OutputUnits: 50},
	})
	if err != nil {
		t.Fatalf("SDK execute: %v", err)
	}
	defer func() { _ = result.Body.Close() }()
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
		Endpoints: f.endpoint, Sources: f.sources, Authorizer: source.NewAuthorizer(f.sources),
		Billing: f.billing, Artifacts: f.artifact,
	})
	if err != nil {
		t.Fatalf("new SDK inference service: %v", err)
	}
	defer func() { _ = svc.Close() }()
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
		Endpoints: f.endpoint, Sources: f.sources, Authorizer: source.NewAuthorizer(f.sources),
		Billing: f.billing, Artifacts: f.artifact,
	})
	if err != nil {
		t.Fatalf("new SDK inference service: %v", err)
	}
	defer func() { _ = svc.Close() }()
	result, err := svc.Execute(f.ctx, f.principal, inference.Request{
		Capability: inference.CapabilityEmbeddings, Model: "gpt-oss-120b",
		Payload:  []byte(`{"input":["alpha","beta"]}`),
		Estimate: billing.Usage{InputUnits: 100},
	})
	if err != nil {
		t.Fatalf("SDK embeddings execute: %v", err)
	}
	defer func() { _ = result.Body.Close() }()
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
		Endpoints: f.endpoint, Sources: f.sources, Authorizer: source.NewAuthorizer(f.sources),
		Billing: f.billing, Artifacts: f.artifact,
	})
	if err != nil {
		t.Fatalf("new SDK inference service: %v", err)
	}
	defer func() { _ = svc.Close() }()
	result, err := svc.Execute(f.ctx, f.principal, inference.Request{
		Capability: inference.CapabilityResponses, Model: "gpt-oss-120b",
		Payload:  []byte(`{"model":"gpt-oss-120b","input":"hello"}`),
		Estimate: billing.Usage{InputUnits: 100, OutputUnits: 50},
	})
	if err != nil {
		t.Fatalf("SDK Responses create: %v", err)
	}
	defer func() { _ = result.Body.Close() }()
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

// --- endpoint runtime observations (plan 08-07 task 3) -----------------------

// runtimeFor returns the runtime row for one endpoint id.
func runtimeFor(t *testing.T, f *fx, endpointID string) inference.EndpointRuntime {
	t.Helper()
	rows, err := f.svc.EndpointRuntime(f.ctx, f.tenant)
	if err != nil {
		t.Fatalf("endpoint runtime: %v", err)
	}
	for _, rt := range rows {
		if rt.EndpointID == endpointID {
			return rt
		}
	}
	t.Fatalf("endpoint %s missing from runtime rows", endpointID)
	return inference.EndpointRuntime{}
}

func TestEndpointRuntimeUnknownUntilDispatched(t *testing.T) {
	f := newFx(t)
	f.mkEndpoint(t)
	f.flatPrices(t)

	rt := runtimeFor(t, f, "ep-1")
	if rt.State != inference.RuntimeStateUnknown {
		t.Fatalf("never-dispatched endpoint state = %q, want unknown", rt.State)
	}
	if rt.LatencyP50Millis != nil || rt.LastSuccessAt != nil || rt.LastError != "" {
		t.Fatalf("unknown endpoint must carry no fabricated evidence: %+v", rt)
	}
}

func TestEndpointRuntimeSuccessReadyWithLatency(t *testing.T) {
	f := newFx(t)
	f.mkEndpoint(t)
	f.flatPrices(t)
	if _, err := f.svc.Execute(f.ctx, f.principal, f.chatRequest(`{"messages":[{"role":"user","content":"hi"}]}`)); err != nil {
		t.Fatalf("execute: %v", err)
	}
	rt := runtimeFor(t, f, "ep-1")
	if rt.State != inference.RuntimeStateReady {
		t.Fatalf("state = %q, want ready", rt.State)
	}
	if rt.Observations != 1 || rt.LastSuccessAt == nil {
		t.Fatalf("observation missing: %+v", rt)
	}
	// A real (non-zero) measured latency sample backs the P50.
	if rt.LatencyP50Millis == nil || *rt.LatencyP50Millis < 0 {
		t.Fatalf("latency p50 missing: %+v", rt)
	}
}

func TestEndpointRuntimeUpstreamErrorDegraded(t *testing.T) {
	f := newFx(t)
	f.mkEndpoint(t)
	f.flatPrices(t)
	f.upstream.err = &llm.UpstreamError{StatusCode: 503, Message: "upstream overloaded"}

	_, err := f.svc.Execute(f.ctx, f.principal, f.chatRequest(`{"messages":[{"role":"user","content":"hi"}]}`))
	if err == nil {
		t.Fatal("expected upstream error")
	}
	rt := runtimeFor(t, f, "ep-1")
	if rt.State != inference.RuntimeStateDegraded {
		t.Fatalf("answered upstream error state = %q, want degraded", rt.State)
	}
	if rt.LastError == "" {
		t.Fatal("degraded endpoint must carry the real error")
	}
}

func TestEndpointRuntimeTransportFailureUnreachable(t *testing.T) {
	f := newFx(t)
	f.mkEndpoint(t)
	f.flatPrices(t)
	f.upstream.err = errors.New("dial tcp 10.0.0.1:443: connection refused")

	_, err := f.svc.Execute(f.ctx, f.principal, f.chatRequest(`{"messages":[{"role":"user","content":"hi"}]}`))
	if err == nil {
		t.Fatal("expected transport failure")
	}
	rt := runtimeFor(t, f, "ep-1")
	if rt.State != inference.RuntimeStateUnreachable {
		t.Fatalf("transport failure state = %q, want unreachable", rt.State)
	}
	if rt.LastError == "" {
		t.Fatal("unreachable endpoint must carry the real error")
	}
}

func TestEndpointRuntimeStreamFailureClasses(t *testing.T) {
	t.Run("mid-stream read failure is unreachable", func(t *testing.T) {
		f := newFx(t)
		f.mkEndpoint(t)
		f.flatPrices(t)
		f.upstream.stream = readErrCloser{err: errors.New("unexpected EOF")}
		req := f.chatRequest(`{"messages":[{"role":"user","content":"hi"}]}`)
		req.Stream = true
		res, err := f.svc.Execute(f.ctx, f.principal, req)
		if err != nil {
			t.Fatalf("stream open: %v", err)
		}
		_, _ = io.ReadAll(res.Body)
		_ = res.Body.Close()
		rt := runtimeFor(t, f, "ep-1")
		if rt.State != inference.RuntimeStateUnreachable {
			t.Fatalf("mid-stream read failure state = %q, want unreachable", rt.State)
		}
	})
	t.Run("terminal failure event is degraded", func(t *testing.T) {
		f := newFx(t)
		f.mkEndpoint(t)
		f.flatPrices(t)
		f.upstream.stream = io.NopCloser(strings.NewReader(
			"data: {\"type\":\"response.failed\"}\n\n"))
		req := f.chatRequest(`{"messages":[{"role":"user","content":"hi"}]}`)
		req.Stream = true
		res, err := f.svc.Execute(f.ctx, f.principal, req)
		if err != nil {
			t.Fatalf("stream open: %v", err)
		}
		_, _ = io.ReadAll(res.Body)
		_ = res.Body.Close()
		rt := runtimeFor(t, f, "ep-1")
		if rt.State != inference.RuntimeStateDegraded {
			t.Fatalf("failed terminal event state = %q, want degraded", rt.State)
		}
	})
	t.Run("client-cancelled stream records nothing", func(t *testing.T) {
		f := newFx(t)
		f.mkEndpoint(t)
		f.flatPrices(t)
		// A stream that would keep producing; the caller abandons it.
		f.upstream.stream = io.NopCloser(strings.NewReader(
			"data: {\"choices\":[{\"delta\":{\"content\":\"hi\"}}]}\n\n"))
		req := f.chatRequest(`{"messages":[{"role":"user","content":"hi"}]}`)
		req.Stream = true
		res, err := f.svc.Execute(f.ctx, f.principal, req)
		if err != nil {
			t.Fatalf("stream open: %v", err)
		}
		if err := res.Body.Close(); err != nil {
			t.Fatal(err)
		}
		rt := runtimeFor(t, f, "ep-1")
		if rt.State != inference.RuntimeStateUnknown || rt.Observations != 0 {
			t.Fatalf("client-cancelled stream must record no observation: %+v", rt)
		}
	})
}

// readErrCloser is a stream body whose reads fail after headers returned.
type readErrCloser struct{ err error }

func (r readErrCloser) Read([]byte) (int, error) { return 0, r.err }
func (r readErrCloser) Close() error             { return nil }

func TestEndpointRuntimeManageObservations(t *testing.T) {
	f := newFx(t)
	f.mkEndpoint(t)
	manageReq := inference.ManageRequest{
		EndpointID: "ep-1", Method: http.MethodGet, Path: "/responses/resp_1",
	}
	f.upstream.resp = []byte(`{"id":"resp_1"}`)
	if _, err := f.svc.Manage(f.ctx, f.principal, manageReq); err != nil {
		t.Fatalf("manage: %v", err)
	}
	if rt := runtimeFor(t, f, "ep-1"); rt.State != inference.RuntimeStateReady {
		t.Fatalf("manage success state = %q, want ready", rt.State)
	}
	f.upstream.err = errors.New("dial tcp: i/o timeout")
	if _, err := f.svc.Manage(f.ctx, f.principal, manageReq); err == nil {
		t.Fatal("expected manage failure")
	}
	rt := runtimeFor(t, f, "ep-1")
	if rt.State != inference.RuntimeStateUnreachable {
		t.Fatalf("manage transport failure state = %q, want unreachable", rt.State)
	}
	if rt.Observations != 2 {
		t.Fatalf("observations = %d, want 2", rt.Observations)
	}
}

// Accounting failures never fabricate endpoint state: the transport outcome
// already recorded the truth, so a settlement error leaves the endpoint ready.
func TestEndpointRuntimeAccountingFailureKeepsReady(t *testing.T) {
	f := newFx(t)
	f.mkEndpoint(t)
	f.flatPrices(t)
	// Wrap the billing seam so settlement of the successful attempt fails.
	f.billingOverride = &failingSettleBilling{Billing: f.billing}
	f.build(t)

	res, err := f.svc.Execute(f.ctx, f.principal, f.chatRequest(`{"messages":[{"role":"user","content":"hi"}]}`))
	if err != nil {
		t.Fatalf("execute: %v", err)
	}
	if res.SettlementError == nil {
		t.Fatal("expected settlement failure evidence")
	}
	rt := runtimeFor(t, f, "ep-1")
	if rt.State != inference.RuntimeStateReady {
		t.Fatalf("accounting failure must not fabricate endpoint state, got %q", rt.State)
	}
}

// failingSettleBilling fails only the settle leg — the observation must not
// depend on accounting success.
type failingSettleBilling struct{ inference.Billing }

func (b *failingSettleBilling) Settle(ctx context.Context, p auth.Principal, reservationID string, actual billing.Usage) (billing.UsageRecord, error) {
	return billing.UsageRecord{}, errors.New("settlement store unavailable")
}

// Two tenants' identically named endpoints never share observations: a
// success for tenant A's ep-1 must leave tenant B's ep-1 unknown.
func TestEndpointRuntimeTwoTenantsIsolated(t *testing.T) {
	f := newFx(t)
	f.mkEndpoint(t)
	f.flatPrices(t)
	if _, err := f.svc.Execute(f.ctx, f.principal, f.chatRequest(`{"messages":[{"role":"user","content":"hi"}]}`)); err != nil {
		t.Fatalf("execute: %v", err)
	}
	// Tenant B gets its own endpoint with the SAME id "ep-1".
	tenantB := testdb.SeedTenant(t, f.db, fmt.Sprintf("inf-b-%d", fixtureSeq.Add(1)))
	if err := f.endpoint.Create(f.ctx, inference.Endpoint{
		ID: "ep-1", TenantID: tenantB, Protocol: "openai_compatible",
		BaseOrigin:   "https://api.upstream.example",
		Capabilities: []string{inference.CapabilityChat, "model:gpt-fast=gpt-4o-mini"},
		Enabled:      true,
	}); err != nil {
		t.Fatalf("create tenant B endpoint: %v", err)
	}
	rows, err := f.svc.EndpointRuntime(f.ctx, tenantB)
	if err != nil {
		t.Fatalf("tenant B runtime: %v", err)
	}
	if len(rows) != 1 || rows[0].EndpointID != "ep-1" {
		t.Fatalf("tenant B rows = %+v", rows)
	}
	if rows[0].State != inference.RuntimeStateUnknown || rows[0].Observations != 0 {
		t.Fatalf("tenant B endpoint borrowed tenant A state: %+v", rows[0])
	}
	if rows[0].LastSuccessAt != nil || rows[0].LatencyP50Millis != nil || rows[0].LastError != "" {
		t.Fatalf("tenant B endpoint leaked A's evidence: %+v", rows[0])
	}
}

// --- priced/routable selector (plan 08-07 task 3) ----------------------------

func TestPricedModelSelectorCoverage(t *testing.T) {
	f := newFx(t)
	// Two endpoints: ep-1 sorts first and carries NO prices; ep-2 prices the
	// alias completely. Both bind the alias and advertise chat.
	f.mkEndpoint(t)
	if err := f.endpoint.Create(f.ctx, inference.Endpoint{
		ID: "ep-2", TenantID: f.tenant, Protocol: "openai_compatible",
		BaseOrigin:   "https://api.upstream.example",
		Capabilities: []string{inference.CapabilityChat, "model:gpt-fast=gpt-4o-mini"},
		Enabled:      true,
	}); err != nil {
		t.Fatalf("create ep-2: %v", err)
	}
	f.priceFor(t, "ep-2", inference.CapabilityChat, billing.UnitInput, 1, 1)
	f.priceFor(t, "ep-2", inference.CapabilityChat, billing.UnitOutput, 2, 1)

	models, err := f.svc.PricedModelSelector(f.ctx, f.principal, "")
	if err != nil {
		t.Fatalf("priced model selector: %v", err)
	}
	if len(models) != 0 {
		t.Fatalf("unpriced first-eligible route must exclude the alias, got %+v", models)
	}

	pinned, err := f.svc.PricedModelSelector(f.ctx, f.principal, "ep-2")
	if err != nil {
		t.Fatalf("pinned selector: %v", err)
	}
	if len(pinned) != 1 || pinned[0].ID != "gpt-fast" {
		t.Fatalf("pinned priced route must include the alias, got %+v", pinned)
	}
}

func TestPricedModelSelectorModelFallbackAndCapabilityScope(t *testing.T) {
	f := newFx(t)
	f.mkEndpoint(t, inference.CapabilityVision)
	// No endpoint prices at all; the model price set is complete for chat.
	modelPrice := func(unit billing.UnitKind, micros int64) {
		t.Helper()
		if err := f.brepo.PutPrice(f.ctx, f.tenant,
			fmt.Sprintf("m-%s-%s-%d", unit, "chat", fixtureSeq.Add(1)), billing.Price{
				SubjectType: billing.PriceSubjectModel, SubjectID: "gpt-4o-mini",
				Capability: inference.CapabilityChat, Unit: unit,
				RateMicros: micros, UnitQuantity: 1, Currency: "USD",
			}, "tester", time.Now().UTC().Add(-time.Minute)); err != nil {
			t.Fatalf("put model price: %v", err)
		}
	}
	modelPrice(billing.UnitInput, 1)
	modelPrice(billing.UnitOutput, 2)
	modelPrice(billing.UnitCachedInput, 1)

	models, err := f.svc.PricedModelSelector(f.ctx, f.principal, "")
	if err != nil {
		t.Fatalf("selector: %v", err)
	}
	if len(models) != 1 || models[0].ID != "gpt-fast" {
		t.Fatalf("complete model price set must make the route selectable, got %+v", models)
	}

	// Capability scoping: the same endpoint+alias is chat-priced but has no
	// vision price set — vision coverage must report missing units.
	missing, err := f.svc.PriceCoverage(f.ctx, f.tenant, "ep-1", "gpt-fast", inference.CapabilityVision, time.Now().UTC())
	if err != nil {
		t.Fatalf("vision coverage: %v", err)
	}
	if len(missing) == 0 {
		t.Fatal("chat prices must not cover the vision capability")
	}
	covered, err := f.svc.PriceCoverage(f.ctx, f.tenant, "ep-1", "gpt-fast", inference.CapabilityChat, time.Now().UTC())
	if err != nil || len(covered) != 0 {
		t.Fatalf("chat coverage = (%v, %v), want (nil, empty)", covered, err)
	}
}

func TestPricedModelSelectorDeniesUngrantedPrincipal(t *testing.T) {
	f := newFx(t)
	f.mkEndpoint(t)
	f.flatPrices(t)
	// A principal whose grants deny the endpoint/model sees nothing.
	denied := f.principal
	denied.AllowedModelsAll = false
	denied.AllowedEndpointsAll = false
	models, err := f.svc.PricedModelSelector(f.ctx, denied, "")
	if err != nil {
		t.Fatalf("selector: %v", err)
	}
	if len(models) != 0 {
		t.Fatalf("denied principal must see no routable models, got %+v", models)
	}
}

// --- key-attributed budgets (plan 08-07 task 3, DB-backed) --------------------

// keyPrincipal is a real api-key-authenticated principal: credential kind and
// id set, billing subject the owning user.
func keyPrincipal(tenant, keyID, ownerUserID string) auth.Principal {
	return auth.Principal{
		TenantID: tenant, ActorID: ownerUserID, ActorKind: auth.ActorKindUser,
		UserID:              ownerUserID,
		CredentialKind:      auth.CredentialKindAPIKey,
		CredentialID:        keyID,
		BillingSubjectKind:  auth.BillingSubjectUser,
		BillingSubjectID:    ownerUserID,
		AllowedModelsAll:    true,
		AllowedEndpointsAll: true,
	}
}

func TestTwoKeysOneOwnerBillIndependently(t *testing.T) {
	f := newFx(t)
	f.mkEndpoint(t)
	f.flatPrices(t)

	owner := testdb.SeedUser(t, f.db, f.tenant, "owner@example.com", nil)
	_, k1 := testdb.SeedAPIKey(t, f.db, f.tenant, testdb.SeedKeySpec{Name: "k1", OwnerUserID: owner.ID})
	_, k2 := testdb.SeedAPIKey(t, f.db, f.tenant, testdb.SeedKeySpec{Name: "k2", OwnerUserID: owner.ID})
	p1, p2 := keyPrincipal(f.tenant, k1.ID, owner.ID), keyPrincipal(f.tenant, k2.ID, owner.ID)
	payload := `{"messages":[{"role":"user","content":"hi"}]}`

	// key1 runs and settles 18µ (12×1 + 3×2).
	res1, err := f.svc.Execute(f.ctx, p1, f.chatRequest(payload))
	if err != nil {
		t.Fatalf("key1 execute: %v", err)
	}
	if _, err := f.billing.Settle(f.ctx, p1, res1.ReservationID, billing.Usage{InputUnits: 12, OutputUnits: 3}); err != nil {
		t.Fatalf("key1 settle: %v", err)
	}
	// Credential attribution persisted on the canonical rows.
	var cred string
	if err := f.db.SQL.QueryRowContext(f.ctx,
		`SELECT credential_id FROM usage_records WHERE tenant_id = $1 AND reservation_id = $2`,
		f.tenant, res1.ReservationID).Scan(&cred); err != nil || cred != k1.ID {
		t.Fatalf("usage credential_id = %q (%v), want %q", cred, err, k1.ID)
	}

	// A budget bound to key1 only, under its settled spend: key1's next
	// request exceeds; key2's identical request sails through.
	if err := f.brepo.CreateBudget(f.ctx, f.tenant, "budget-k1", billing.Budget{
		Name: "key1-only", Subject: billing.Subject{Type: billing.BudgetSubjectAPIKey, ID: k1.ID},
		Kind: billing.BudgetKindAmount, Period: billing.PeriodMonthly,
		LimitAmountMicros: 10, Currency: "USD", StartsAt: time.Now().UTC().Add(-time.Hour),
	}); err != nil {
		t.Fatalf("create budget: %v", err)
	}
	if _, err := f.svc.Execute(f.ctx, p1, f.chatRequest(payload)); !errors.Is(err, billing.ErrBudgetExceeded) {
		t.Fatalf("key1 execute = %v, want ErrBudgetExceeded", err)
	}
	res2, err := f.svc.Execute(f.ctx, p2, f.chatRequest(payload))
	if err != nil {
		t.Fatalf("key2 execute must not touch key1's budget: %v", err)
	}
	if _, err := f.billing.Settle(f.ctx, p2, res2.ReservationID, billing.Usage{InputUnits: 12, OutputUnits: 3}); err != nil {
		t.Fatalf("key2 settle: %v", err)
	}

	// Settled spend is recoverable per key and per owner, never doubled.
	sums, err := f.brepo.UsageBySubject(f.ctx, f.tenant, time.Now().UTC().Add(-time.Hour))
	if err != nil {
		t.Fatalf("usage by subject: %v", err)
	}
	byKey := map[string]int64{} // "type:id" → request count
	for _, s := range sums {
		byKey[s.SubjectType+":"+s.SubjectID] += s.RequestCount
	}
	if byKey["user:"+owner.ID] != 2 {
		t.Fatalf("owner canonical rows = %d requests, want 2 (never double-counted)", byKey["user:"+owner.ID])
	}
	if byKey["api_key:"+k1.ID] != 1 || byKey["api_key:"+k2.ID] != 1 {
		t.Fatalf("key attribution rows wrong: %+v", byKey)
	}
	// Key-scoped settled liability resolves through the credential column.
	rows, err := f.brepo.Liabilities(f.ctx, f.tenant,
		billing.Subject{Type: billing.BudgetSubjectAPIKey, ID: k1.ID},
		time.Now().UTC().Add(-time.Hour))
	if err != nil {
		t.Fatalf("key liabilities: %v", err)
	}
	var k1Settled int64
	for _, row := range rows {
		k1Settled += row.AmountMicros
	}
	if k1Settled != 18 {
		t.Fatalf("key1 settled liability = %d, want 18", k1Settled)
	}
}

// --- accounting hardening: never-sent release, exact disconnect accounting,
// output-bound sizing -----------------------------------------------------

// TestConnectionRefusedReleasesHold drives the real net/http transport at a
// dead port: no request bytes can have left, so the held reservation must
// release — not park as unknown or consume budget.
func TestConnectionRefusedReleasesHold(t *testing.T) {
	f := newFx(t)

	ln, err := net.Listen("tcp", "127.0.0.1:0")
	if err != nil {
		t.Fatal(err)
	}
	dead := "http://" + ln.Addr().String()
	_ = ln.Close() // port is now guaranteed refused

	e := inference.Endpoint{
		ID: "ep-dead", TenantID: f.tenant, Protocol: "openai_compatible",
		BaseOrigin:        dead,
		Capabilities:      []string{inference.CapabilityChat, "model:gpt-fast=gpt-4o-mini"},
		AuthorizedPrivate: true, Enabled: true,
	}
	if err := f.endpoint.Create(f.ctx, e); err != nil {
		t.Fatalf("create endpoint: %v", err)
	}
	f.priceFor(t, "ep-dead", inference.CapabilityChat, billing.UnitInput, 1, 1)
	f.priceFor(t, "ep-dead", inference.CapabilityChat, billing.UnitOutput, 1, 1)

	svc, err := inference.NewService(inference.Deps{
		Endpoints: f.endpoint, Sources: f.sources,
		Authorizer: source.NewAuthorizer(f.sources),
		Billing:    f.billing, Artifacts: f.artifact,
	})
	if err != nil {
		t.Fatal(err)
	}
	defer func() { _ = svc.Close() }()

	req := f.chatRequest(`{"messages":[{"role":"user","content":"hi"}]}`)
	req.EndpointID = "ep-dead"
	if _, err := svc.Execute(f.ctx, f.principal, req); err == nil {
		t.Fatal("connection refused must surface")
	}
	var state string
	if qerr := f.db.SQL.QueryRowContext(f.ctx,
		`SELECT state FROM reservations WHERE tenant_id = $1 LIMIT 1`,
		f.tenant).Scan(&state); qerr != nil {
		t.Fatal(qerr)
	}
	if state != billing.ReservationReleased {
		t.Fatalf("never-sent hold must release, state=%s", state)
	}
}

// A streamed definitive upstream error settles exactly like the non-stream
// branch — the provider answered, so the reservation is settled evidence,
// never unknown liability.
func TestStreamOpenUpstreamErrorSettles(t *testing.T) {
	f := newFx(t)
	f.mkEndpoint(t)
	f.flatPrices(t)
	f.upstream.streamErr = &llm.UpstreamError{StatusCode: 429, Body: []byte(`{"error":{"message":"rate limited"}}`)}

	req := f.chatRequest(`{"messages":[{"role":"user","content":"hi"}]}`)
	req.Stream = true
	_, err := f.svc.Execute(f.ctx, f.principal, req)
	var ue *llm.UpstreamError
	if !errors.As(err, &ue) || ue.StatusCode != 429 {
		t.Fatalf("upstream 429 must surface, got %v", err)
	}
	var state string
	if qerr := f.db.SQL.QueryRowContext(f.ctx,
		`SELECT state FROM reservations WHERE tenant_id = $1 LIMIT 1`,
		f.tenant).Scan(&state); qerr != nil {
		t.Fatal(qerr)
	}
	if state != billing.ReservationSettled {
		t.Fatalf("streamed upstream error must settle, state=%s", state)
	}
	var n int
	if qerr := f.db.SQL.QueryRowContext(f.ctx,
		`SELECT count(*) FROM usage_records WHERE tenant_id = $1`, f.tenant).Scan(&n); qerr != nil || n != 1 {
		t.Fatalf("settlement records = %d (err=%v), want 1", n, qerr)
	}
}

// failingDispatchBilling fails MarkDispatched; the hold must release via
// ReleaseUndispatched — never park as held.
type failingDispatchBilling struct{ inference.Billing }

func (b *failingDispatchBilling) MarkDispatched(ctx context.Context, p auth.Principal, reservationID string) error {
	return errors.New("dispatch store unavailable")
}

func TestMarkDispatchedFailureReleases(t *testing.T) {
	f := newFx(t)
	f.mkEndpoint(t)
	f.flatPrices(t)
	f.billingOverride = &failingDispatchBilling{Billing: f.billing}
	f.build(t)

	_, err := f.svc.Execute(f.ctx, f.principal,
		f.chatRequest(`{"messages":[{"role":"user","content":"hi"}]}`))
	if err == nil {
		t.Fatal("mark-dispatched failure must surface")
	}
	if f.upstream.calls != 0 {
		t.Fatal("failed dispatch mark reached the transport")
	}
	var state string
	if qerr := f.db.SQL.QueryRowContext(f.ctx,
		`SELECT state FROM reservations WHERE tenant_id = $1 LIMIT 1`,
		f.tenant).Scan(&state); qerr != nil {
		t.Fatal(qerr)
	}
	if state != billing.ReservationReleased {
		t.Fatalf("undispatched hold must release, state=%s", state)
	}
}

// Client cancellation mid-dispatch must not cancel the upstream call: the
// provider may still be executing, and the response carries the exact token
// counts the settlement needs.
func TestClientCancelNonStreamStillSettles(t *testing.T) {
	f := newFx(t)
	f.mkEndpoint(t)
	f.flatPrices(t)

	callCtx, cancel := context.WithCancel(f.ctx)
	f.upstream.onChat = func(ctx context.Context) error {
		cancel()
		if err := ctx.Err(); err != nil {
			return err // the dispatch context must outlive the caller's
		}
		return nil
	}

	res, err := f.svc.Execute(callCtx, f.principal,
		f.chatRequest(`{"messages":[{"role":"user","content":"hi"}]}`))
	if err != nil && !errors.Is(err, context.Canceled) {
		t.Fatalf("unexpected dispatch result: %v", err)
	}
	var id string
	if res != nil {
		defer func() { _ = res.Body.Close() }()
		id = res.ReservationID
	} else if qerr := f.db.SQL.QueryRowContext(f.ctx,
		`SELECT id FROM reservations WHERE tenant_id = $1 LIMIT 1`, f.tenant).Scan(&id); qerr != nil {
		t.Fatal(qerr)
	}
	if got := f.waitReservationState(t, id, billing.ReservationSettled); got != billing.ReservationSettled {
		t.Fatalf("cancelled client's attempt state = %s", got)
	}
}

// A client disconnect mid-stream drains the upstream body to its terminal
// usage event and settles the provider's real counts — Close returns
// promptly, the permit releases when the drain ends.
func TestClientDisconnectStreamSettles(t *testing.T) {
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
	buf := make([]byte, 32)
	if _, err := res.Body.Read(buf); err != nil {
		t.Fatalf("read: %v", err)
	}
	if err := res.Body.Close(); err != nil {
		t.Fatal(err)
	}
	if got := f.waitReservationState(t, res.ReservationID, billing.ReservationSettled); got != billing.ReservationSettled {
		t.Fatalf("disconnected stream must settle after drain: %s", got)
	}
	var amount int64
	if qerr := f.db.SQL.QueryRowContext(f.ctx,
		`SELECT amount_micros FROM usage_records WHERE tenant_id = $1 AND reservation_id = $2`,
		f.tenant, res.ReservationID).Scan(&amount); qerr != nil {
		t.Fatal(qerr)
	}
	if amount != 20+2*7 {
		t.Fatalf("drained settlement amount=%d, want 34", amount)
	}
}

// Client cancellation DURING stream open aborts the open — the detached
// lifetime belongs to an opened body, not a wedged one. The hang is modeled
// as a transport whose open blocks on the dispatch context; upstream a
// non-2xx body that never finishes produces the same wedge (adapter-bound).
func TestClientCancelDuringStreamOpenReleases(t *testing.T) {
	f := admissionFx(t, inference.AdmissionConfig{
		Execute: inference.ClassLimit{MaxActive: 1, MaxQueued: 0},
	})

	callCtx, cancel := context.WithCancel(f.ctx)
	defer cancel()
	f.upstream.onStream = func(ctx context.Context) error {
		<-ctx.Done()
		return ctx.Err()
	}

	done := make(chan error, 1)
	go func() {
		_, err := f.svc.Execute(callCtx, f.principal, f.streamRequest())
		done <- err
	}()
	waitActive(t, f.admission, inference.AdmissionExecute, 1)
	cancel()

	select {
	case err := <-done:
		if !errors.Is(err, context.Canceled) {
			t.Fatalf("cancelled stream open error = %v, want context.Canceled", err)
		}
	case <-time.After(5 * time.Second):
		t.Fatal("stream open ignored caller cancellation")
	}
	waitActive(t, f.admission, inference.AdmissionExecute, 0)
}

// flakySettleBilling fails the first n Settle calls; the executor must retry
// within the accounting window instead of parking the reservation unknown.
type flakySettleBilling struct {
	inference.Billing
	calls int32
}

func (b *flakySettleBilling) Settle(ctx context.Context, p auth.Principal, reservationID string, actual billing.Usage) (billing.UsageRecord, error) {
	if atomic.AddInt32(&b.calls, 1) < 3 {
		return billing.UsageRecord{}, errors.New("transient settlement failure")
	}
	return b.Billing.Settle(ctx, p, reservationID, actual)
}

func TestSettleRetryRecoversExactAccounting(t *testing.T) {
	f := newFx(t)
	f.mkEndpoint(t)
	f.flatPrices(t)
	flaky := &flakySettleBilling{Billing: f.billing}
	f.billingOverride = flaky
	f.build(t)

	res, err := f.svc.Execute(f.ctx, f.principal,
		f.chatRequest(`{"messages":[{"role":"user","content":"hi"}]}`))
	if err != nil {
		t.Fatalf("execute: %v", err)
	}
	defer func() { _ = res.Body.Close() }()
	if res.SettlementError != nil {
		t.Fatalf("transient settle failures must retry, got %v", res.SettlementError)
	}
	if atomic.LoadInt32(&flaky.calls) != 3 {
		t.Fatalf("settle calls = %d, want 3", flaky.calls)
	}
	if res.Usage == nil || res.Usage.AmountMicros != 18 {
		t.Fatalf("retried settlement wrong: %+v", res.Usage)
	}
	if f.reservationState(t, res.ReservationID) != billing.ReservationSettled {
		t.Fatal("reservation not settled")
	}
}

// The executor owns the output bound: the payload's declared generation cap
// times n sizes the hold, the configured default is injected upstream when
// absent, and requests above the operation bound refuse 4xx.
func TestOutputBoundDerivedFromPayload(t *testing.T) {
	f := newFx(t)
	f.mkEndpoint(t)
	f.flatPrices(t)

	// Declared cap times n sizes the hold exactly.
	res, err := f.svc.Execute(f.ctx, f.principal,
		f.chatRequest(`{"messages":[{"role":"user","content":"hi"}],"max_tokens":100,"n":3}`))
	if err != nil {
		t.Fatalf("execute: %v", err)
	}
	_ = res.Body.Close()
	units := f.reservationUnits(t, res.ReservationID)
	if units["output"] != 300 {
		t.Fatalf("held output = %v, want 300 (100×3)", units["output"])
	}

	// An absent cap injects the configured default into the upstream body
	// and holds exactly that.
	res2, err := f.svc.Execute(f.ctx, f.principal,
		f.chatRequest(`{"messages":[{"role":"user","content":"hi"}]}`))
	if err != nil {
		t.Fatalf("execute: %v", err)
	}
	_ = res2.Body.Close()
	units = f.reservationUnits(t, res2.ReservationID)
	if units["output"] != 16384 {
		t.Fatalf("held output = %v, want injected default 16384", units["output"])
	}
	var sent map[string]any
	if err := json.Unmarshal(f.upstream.lastBody, &sent); err != nil {
		t.Fatal(err)
	}
	if sent["max_tokens"] != float64(16384) {
		t.Fatalf("upstream payload missing injected cap: %s", f.upstream.lastBody)
	}

	// Above the operation bound refuses — the request can never dispatch.
	calls := atomic.LoadInt32(&f.upstream.calls)
	_, err = f.svc.Execute(f.ctx, f.principal,
		f.chatRequest(`{"messages":[{"role":"user","content":"hi"}],"max_tokens":200000}`))
	if !errors.Is(err, inference.ErrUnbounded) {
		t.Fatalf("over-bound request must refuse, got %v", err)
	}
	if atomic.LoadInt32(&f.upstream.calls) != calls {
		t.Fatal("over-bound request dispatched")
	}

	// A zero configured default injects nothing and holds the operation bound.
	svc, err := inference.NewService(inference.Deps{
		Endpoints: f.endpoint, Sources: f.sources,
		Authorizer: source.NewAuthorizer(f.sources),
		Billing:    f.billing, Artifacts: f.artifact,
	}, inference.WithTransportFactory(
		func(ctx context.Context, e inference.Endpoint, credential string) (inference.Upstream, error) {
			return f.upstream, nil
		}), inference.WithDefaultMaxOutputTokens(0))
	if err != nil {
		t.Fatal(err)
	}
	res3, err := svc.Execute(f.ctx, f.principal,
		f.chatRequest(`{"messages":[{"role":"user","content":"hi"}]}`))
	if err != nil {
		t.Fatalf("execute: %v", err)
	}
	_ = res3.Body.Close()
	units = f.reservationUnits(t, res3.ReservationID)
	if units["output"] != 1<<17 {
		t.Fatalf("zero-default hold = %v, want operation bound %d", units["output"], 1<<17)
	}
	var sent3 map[string]any
	if err := json.Unmarshal(f.upstream.lastBody, &sent3); err != nil {
		t.Fatal(err)
	}
	if _, present := sent3["max_tokens"]; present {
		t.Fatalf("zero default must inject nothing: %s", f.upstream.lastBody)
	}
}

// noStreamUpstream reports itself as incapable of streamed dispatch.
type noStreamUpstream struct{ fakeUpstream }

func (u *noStreamUpstream) SupportsStream(string) bool { return false }

// Unsupported stream/capability combinations refuse before a reservation
// exists — the hold can never strand on a transport that cannot serve it.
func TestUnsupportedStreamRefusedBeforeReserve(t *testing.T) {
	f := newFx(t)
	f.mkEndpoint(t)
	f.flatPrices(t)
	up := &noStreamUpstream{}
	svc, err := inference.NewService(inference.Deps{
		Endpoints: f.endpoint, Sources: f.sources,
		Authorizer: source.NewAuthorizer(f.sources),
		Billing:    f.billing, Artifacts: f.artifact,
	}, inference.WithTransportFactory(
		func(ctx context.Context, e inference.Endpoint, credential string) (inference.Upstream, error) {
			return up, nil
		}))
	if err != nil {
		t.Fatal(err)
	}
	req := f.chatRequest(`{"messages":[{"role":"user","content":"hi"}]}`)
	req.Stream = true
	_, err = svc.Execute(f.ctx, f.principal, req)
	if !errors.Is(err, inference.ErrUnsupportedCapability) {
		t.Fatalf("unsupported stream must refuse 4xx, got %v", err)
	}
	if n := f.reservationCount(t); n != 0 {
		t.Fatalf("unsupported stream left %d reservations", n)
	}
}

func TestDeadlineReturnsBeforeDetachedSettlement(t *testing.T) {
	f := newFx(t)
	f.mkEndpoint(t)
	f.flatPrices(t)
	started := make(chan struct{})
	finish := make(chan struct{})
	f.upstream.onChat = func(ctx context.Context) error {
		close(started)
		<-finish
		return nil
	}
	ctx, cancel := context.WithTimeout(f.ctx, time.Second)
	defer cancel()
	done := make(chan error, 1)
	go func() {
		_, err := f.svc.Execute(ctx, f.principal, f.chatRequest(`{"messages":[{"role":"user","content":"hi"}]}`))
		done <- err
	}()
	<-started
	select {
	case err := <-done:
		if !errors.Is(err, context.DeadlineExceeded) {
			t.Fatalf("got %v", err)
		}
	case <-time.After(3 * time.Second):
		t.Fatal("caller did not return while upstream was blocked")
	}
	var id string
	if err := f.db.SQL.QueryRowContext(f.ctx, `SELECT id FROM reservations WHERE tenant_id = $1`, f.tenant).Scan(&id); err != nil {
		t.Fatal(err)
	}
	close(finish)
	if got := f.waitReservationState(t, id, billing.ReservationSettled); got != billing.ReservationSettled {
		t.Fatalf("reservation state = %s", got)
	}
}

type canceledMarkBilling struct {
	inference.Billing
	started chan struct{}
}

func (b *canceledMarkBilling) MarkDispatched(ctx context.Context, p auth.Principal, id string) error {
	close(b.started)
	<-ctx.Done()
	return errors.New("driver: bad connection")
}

func TestCancelDuringMarkDispatchedReturnsContextError(t *testing.T) {
	f := newFx(t)
	f.mkEndpoint(t)
	f.flatPrices(t)
	b := &canceledMarkBilling{Billing: f.billing, started: make(chan struct{})}
	f.billingOverride = b
	f.build(t)
	ctx, cancel := context.WithCancel(f.ctx)
	done := make(chan error, 1)
	go func() {
		_, err := f.svc.Execute(ctx, f.principal, f.streamRequest())
		done <- err
	}()
	<-b.started
	cancel()
	if err := <-done; !errors.Is(err, context.Canceled) {
		t.Fatalf("got %v, want cancellation", err)
	}
	var state string
	if err := f.db.SQL.QueryRowContext(f.ctx, `SELECT state FROM reservations WHERE tenant_id = $1`, f.tenant).Scan(&state); err != nil || state != billing.ReservationReleased {
		t.Fatalf("reservation state %q, error %v", state, err)
	}
}

func TestNonSSEStreamSettlesReportedUsage(t *testing.T) {
	server := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		w.Header().Set("Content-Type", "application/json")
		_, _ = w.Write([]byte(`{"usage":{"prompt_tokens":12,"completion_tokens":3,"total_tokens":15}}`))
	}))
	defer server.Close()
	f := newFx(t)
	f.mkEndpoint(t)
	f.flatPrices(t)
	f.upstreamOverride = llm.NewOpenAICompatibleAdapter(server.URL, "key", time.Second).WithAllowPrivateEndpoint()
	f.build(t)
	_, err := f.svc.Execute(f.ctx, f.principal, f.streamRequest())
	var protocolErr *llm.StreamProtocolError
	if !errors.As(err, &protocolErr) {
		t.Fatalf("got %v, want stream protocol error", err)
	}
	var state string
	if err := f.db.SQL.QueryRowContext(f.ctx, `SELECT state FROM reservations WHERE tenant_id = $1`, f.tenant).Scan(&state); err != nil || state != billing.ReservationSettled {
		t.Fatalf("reservation state %q, error %v", state, err)
	}
}

type canceledReserveBilling struct {
	inference.Billing
	started chan struct{}
	resume  chan struct{}
	fail    bool
}

func (b *canceledReserveBilling) Reserve(ctx context.Context, p auth.Principal, req billing.ReserveRequest) (billing.Reservation, error) {
	close(b.started)
	<-b.resume
	if b.fail {
		return billing.Reservation{}, errors.New("driver: bad connection")
	}
	return b.Billing.Reserve(ctx, p, req)
}

func TestCancelDuringStreamReservation(t *testing.T) {
	for _, fail := range []bool{false, true} {
		t.Run(fmt.Sprintf("driver_error=%t", fail), func(t *testing.T) {
			f := newFx(t)
			f.mkEndpoint(t)
			f.flatPrices(t)
			b := &canceledReserveBilling{Billing: f.billing, started: make(chan struct{}), resume: make(chan struct{}), fail: fail}
			f.billingOverride = b
			f.build(t)
			ctx, cancel := context.WithCancel(f.ctx)
			done := make(chan error, 1)
			go func() {
				_, err := f.svc.Execute(ctx, f.principal, f.streamRequest())
				done <- err
			}()
			<-b.started
			cancel()
			close(b.resume)
			if err := <-done; !errors.Is(err, context.Canceled) {
				t.Fatalf("got %v, want cancellation", err)
			}
			var held int
			if err := f.db.SQL.QueryRowContext(f.ctx, `SELECT count(*) FROM reservations WHERE tenant_id = $1 AND state = $2`, f.tenant, billing.ReservationHeld).Scan(&held); err != nil || held != 0 {
				t.Fatalf("held reservations %d, error %v", held, err)
			}
			if atomic.LoadInt32(&f.upstream.calls) != 0 {
				t.Fatal("canceled reservation reached upstream")
			}
		})
	}
}
