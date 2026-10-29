package extract_test

import (
	"bytes"
	"context"
	"encoding/base64"
	"encoding/json"
	"errors"
	"fmt"
	"io"
	"os"
	"path/filepath"
	"strings"
	"sync/atomic"
	"testing"
	"time"

	"enclava-go/internal/artifacts"
	"enclava-go/internal/auth"
	"enclava-go/internal/billing"
	"enclava-go/internal/extract"
	"enclava-go/internal/inference"
	"enclava-go/internal/jobs"
	"enclava-go/internal/llm"
	"enclava-go/internal/source"
	"enclava-go/internal/store/postgres"
	"enclava-go/internal/testdb"
)

// fakeUpstream is the deterministic transport behind the real executor — the
// tests exercise the actual inference.Service policy/routing/accounting path,
// not a stubbed Execute.
type fakeUpstream struct {
	response  []byte
	callCount atomic.Int32
	// refuseResponseFormat: bodies carrying response_format fail the way
	// engines without structured-output support do — the degrade-retry
	// regression drives this.
	refuseResponseFormat atomic.Bool
	bodies               [][]byte
}

func (f *fakeUpstream) CreateChatCompletion(ctx context.Context, body []byte) ([]byte, error) {
	f.callCount.Add(1)
	bodyCopy := make([]byte, len(body))
	copy(bodyCopy, body)
	f.bodies = append(f.bodies, bodyCopy)
	if f.refuseResponseFormat.Load() && bytes.Contains(body, []byte(`"response_format"`)) {
		return nil, &llm.UpstreamError{StatusCode: 400, Message: "structured output (response_format and the guided_* fields) is not supported by this engine yet"}
	}
	if f.response != nil {
		return f.response, nil
	}
	return []byte(`{"id":"c","usage":{"prompt_tokens":10,"completion_tokens":5,"total_tokens":15},"choices":[{"message":{"content":"{\"total\":1}"}}]}`), nil
}
func (f *fakeUpstream) CreateChatCompletionStream(ctx context.Context, body []byte) (io.ReadCloser, error) {
	f.callCount.Add(1)
	return io.NopCloser(&emptyReader{}), nil
}
func (f *fakeUpstream) CreateEmbeddings(ctx context.Context, body []byte) ([]byte, error) {
	return nil, nil
}
func (f *fakeUpstream) DoResponsesRequest(ctx context.Context, method, path string, body []byte) ([]byte, error) {
	return nil, errors.New("responses not supported in fixture")
}
func (f *fakeUpstream) DoResponsesStream(ctx context.Context, method, path string, body []byte) (io.ReadCloser, error) {
	return nil, errors.New("responses not supported in fixture")
}

type emptyReader struct{}

func (r *emptyReader) Read(p []byte) (int, error) { return 0, io.EOF }
func (r *emptyReader) Close() error               { return nil }

// fixture wires the full durable stack on a real PostgreSQL schema: jobs,
// artifacts, billing and the real executor over a fake upstream transport.
type fixture struct {
	t         *testing.T
	db        *postgres.DB
	svc       *extract.Service
	jobsSvc   *jobs.Service
	store     *artifacts.Store
	upstream  *fakeUpstream
	endpoints *postgres.EndpointsRepo
	sources   *postgres.SourceRepo
	billing   *postgres.BillingRepo
	tenant    string
	principal auth.Principal
}

func newFixture(t *testing.T) fixture {
	t.Helper()
	ctx := context.Background()
	db := testdb.New(t)

	repo := postgres.NewExtractRepo(db)
	jobsRepo := postgres.NewJobsRepo(db)
	artRepo := postgres.NewArtifactRepo(db)
	endpoints := postgres.NewEndpointsRepo(db)
	sources := postgres.NewSourceRepo(db)
	billingRepo := postgres.NewBillingRepo(db)
	root := t.TempDir()
	store, err := artifacts.NewStore(filepath.Join(root, "durable"), filepath.Join(root, "scratch"), artRepo)
	if err != nil {
		t.Fatalf("artifact store: %v", err)
	}
	jobsSvc := jobs.NewService(jobsRepo, 10*time.Millisecond)
	upstream := &fakeUpstream{}
	exec, err := inference.NewService(inference.Deps{
		Endpoints:  endpoints,
		Sources:    sources,
		Authorizer: source.NewAuthorizer(sources, 0),
		Billing:    billing.NewService(billingRepo),
		Artifacts:  artRepo,
	}, inference.WithTransportFactory(
		func(ctx context.Context, e inference.Endpoint, credential string) (inference.Upstream, error) {
			return upstream, nil
		}))
	if err != nil {
		t.Fatalf("executor: %v", err)
	}
	svc := extract.NewService(repo, repo, jobsRepo, jobsSvc, store, exec, endpoints, 5)
	svc.SetSourceRepository(sources)

	tenant := testdb.SeedTenant(t, db, "extract-tenant")
	user := testdb.SeedUser(t, db, tenant, "extract@example.com", nil)
	apiKey, _ := testdb.SeedAPIKey(t, db, tenant, testdb.SeedKeySpec{
		Name:        "extract-key",
		OwnerUserID: user.ID,
		Scopes:      []string{"extract", "extract.manage"},
	})
	principal, err := auth.NewService(postgres.NewIdentityRepo(db), "").Authenticate(ctx, apiKey)
	if err != nil {
		t.Fatalf("authenticate: %v", err)
	}
	return fixture{
		t: t, db: db, svc: svc, jobsSvc: jobsSvc,
		store: store, upstream: upstream, endpoints: endpoints,
		sources: sources, billing: billingRepo,
		tenant: tenant, principal: principal,
	}
}

// seedEndpoint registers the vision-capable endpoint with the given model
// aliases bound as "model:" capabilities.
func (f fixture) seedEndpoint(t *testing.T, aliases ...string) {
	t.Helper()
	caps := []string{inference.CapabilityChat, inference.CapabilityVision}
	for _, a := range aliases {
		caps = append(caps, inference.ModelCapabilityPrefix+a)
	}
	if err := f.endpoints.Create(context.Background(), inference.Endpoint{
		ID: "ep-extract", TenantID: f.tenant, Protocol: "openai_compatible",
		BaseOrigin: "https://extract.upstream.example", Capabilities: caps,
		Enabled: true,
	}); err != nil {
		t.Fatalf("seed endpoint: %v", err)
	}
}

func (f fixture) seedTemplate(t *testing.T, tpl extract.Template) {
	t.Helper()
	if err := f.svc.UpsertTemplate(context.Background(), f.principal, tpl); err != nil {
		t.Fatalf("seed template: %v", err)
	}
}

// putPrice seeds one model rate for the capability used by parsed text tasks.
func (f fixture) putPrice(t *testing.T, model string, unit billing.UnitKind, rateMicros int64) {
	t.Helper()
	f.putPriceRow(t, model, unit, rateMicros)
	if unit == billing.UnitInput {
		f.putPriceRow(t, model, billing.UnitCachedInput, rateMicros)
	}
}

func (f fixture) putPriceRow(t *testing.T, model string, unit billing.UnitKind, rateMicros int64) {
	t.Helper()
	err := f.billing.PutPrice(context.Background(), f.tenant, "price_"+model+"_"+string(unit), billing.Price{
		SubjectType: billing.PriceSubjectModel, SubjectID: model,
		Capability: inference.CapabilityChat, Unit: unit,
		RateMicros: rateMicros, UnitQuantity: 1000, Currency: "USD",
		EffectiveFrom: time.Now().Add(-time.Hour).UTC(),
	}, f.principal.ActorID, time.Now().Add(-time.Hour).UTC())
	if err != nil {
		t.Fatalf("put price: %v", err)
	}
}

// leaseClaim claims the enqueued job for the test worker.
func (f fixture) leaseClaim(t *testing.T) *jobs.Claim {
	t.Helper()
	ctx, cancel := context.WithTimeout(context.Background(), 5*time.Second)
	defer cancel()
	for {
		claim, err := f.jobsSvc.Lease(ctx, "test-worker", 30*time.Second)
		if err != nil {
			t.Fatalf("lease: %v", err)
		}
		if claim != nil {
			return claim
		}
		select {
		case <-ctx.Done():
			t.Fatal("timed out waiting for claim")
		case <-time.After(10 * time.Millisecond):
		}
	}
}

func inputOpener(f fixture, claim *jobs.Claim) jobs.InputFunc {
	return func(ctx context.Context) (io.ReadCloser, error) {
		return f.store.Read(ctx, artifacts.Ref{TenantID: f.tenant, ID: claim.Job().InputRef})
	}
}

func TestTemplateCRUDTenantScoped(t *testing.T) {
	f := newFixture(t)
	ctx := context.Background()
	f.seedTemplate(t, extract.Template{
		ID: "invoice", Name: "Invoice", SystemPrompt: "s", UserPrompt: "u",
		ContextSchema: map[string]any{}, OutputSchema: map[string]any{"type": "object"},
		Enabled: true,
	})

	got, err := f.svc.GetTemplate(ctx, f.principal, "invoice")
	if err != nil {
		t.Fatalf("get template: %v", err)
	}
	if got.Name != "Invoice" || !got.Enabled {
		t.Fatalf("unexpected template: %+v", got)
	}
	list, err := f.svc.ListTemplates(ctx, f.principal)
	if err != nil || len(list) != 1 {
		t.Fatalf("list templates: %v len=%d", err, len(list))
	}

	// A second tenant sees nothing — cross-tenant reads are ErrNotFound.
	other := testdb.SeedTenant(t, f.db, "other-tenant")
	if _, err := f.svc.GetTemplate(ctx, auth.Principal{TenantID: other}, "invoice"); !errors.Is(err, extract.ErrNotFound) {
		t.Fatalf("cross-tenant template read: %v, want ErrNotFound", err)
	}

	if err := f.svc.DeleteTemplate(ctx, f.principal, "invoice"); err != nil {
		t.Fatalf("delete: %v", err)
	}
	if _, err := f.svc.GetTemplate(ctx, f.principal, "invoice"); !errors.Is(err, extract.ErrNotFound) {
		t.Fatalf("deleted template read: %v, want ErrNotFound", err)
	}
}

func TestProcessPersistsInputArtifactAndJobRow(t *testing.T) {
	f := newFixture(t)
	ctx := context.Background()
	f.seedEndpoint(t, "priced-extract")
	f.seedTemplate(t, extract.Template{
		ID: "invoice", Name: "Invoice", SystemPrompt: "s", UserPrompt: "u",
		DefaultModel: "priced-extract", Enabled: true,
	})

	out, err := f.svc.Process(ctx, f.principal, extract.ProcessInput{
		TemplateID: "invoice", FileName: "invoice.txt", MimeType: "text/plain",
		Bytes: []byte("invoice text"),
	})
	if err != nil {
		t.Fatalf("process: %v", err)
	}
	if out.JobID == "" || out.Status != jobs.StatusQueued {
		t.Fatalf("unexpected output: %+v", out)
	}

	// Job row exists BEFORE Process returns — durable acknowledgement.
	job, err := f.jobsSvc.Get(ctx, f.principal, out.JobID)
	if err != nil {
		t.Fatalf("get job: %v", err)
	}
	if job.Operation != extract.OperationProcess || job.InputRef == "" {
		t.Fatalf("unexpected job: %+v", job)
	}
	if job.Policy["endpoint_id"] != "ep-extract" || job.Policy["capability"] != inference.CapabilityChat {
		t.Fatalf("route snapshot was not frozen: %#v", job.Policy)
	}
	// Frozen principal snapshot is on the row.
	var snap auth.Principal
	if err := json.Unmarshal(job.PrincipalSnapshot, &snap); err != nil || snap.TenantID != f.tenant {
		t.Fatalf("frozen principal: %v %+v", err, snap)
	}
	// Input artifact is final and readable.
	rc, err := f.store.Read(ctx, artifacts.Ref{TenantID: f.tenant, ID: job.InputRef})
	if err != nil {
		t.Fatalf("read input artifact: %v", err)
	}
	_ = rc.Close()
}
func TestProcessIdempotencyReplaysAndRejectsChangedInput(t *testing.T) {
	f := newFixture(t)
	ctx := context.Background()
	f.seedEndpoint(t, "priced-extract")
	f.seedTemplate(t, extract.Template{
		ID: "invoice", Name: "Invoice", SystemPrompt: "Extract.", UserPrompt: "Return JSON.",
		DefaultModel: "priced-extract", Enabled: true,
	})
	input := extract.ProcessInput{
		TemplateID: "invoice", FileName: "invoice.txt", MimeType: "text/plain",
		Bytes: []byte("same invoice"), IdempotencyKey: "invoice-upload-1",
	}
	first, err := f.svc.Process(ctx, f.principal, input)
	if err != nil {
		t.Fatalf("first submission: %v", err)
	}
	retry, err := f.svc.Process(ctx, f.principal, input)
	if err != nil || retry.JobID != first.JobID {
		t.Fatalf("exact retry = (%+v, %v), want job %s", retry, err, first.JobID)
	}
	input.Bytes = []byte("changed invoice")
	if _, err := f.svc.Process(ctx, f.principal, input); !errors.Is(err, jobs.ErrIdempotencyConflict) {
		t.Fatalf("changed input error = %v, want idempotency conflict", err)
	}
	var ready int
	if err := f.db.SQL.QueryRowContext(ctx, `
		SELECT COUNT(*) FROM artifacts WHERE tenant_id=$1::uuid AND kind='original' AND state='final'`,
		f.tenant).Scan(&ready); err != nil {
		t.Fatalf("count retained inputs: %v", err)
	}
	if ready != 1 {
		t.Fatalf("final input artifacts=%d, want one; retries must clean newly orphaned bytes", ready)
	}
}

func TestProcessRejectsDisallowedModelAndTemplate(t *testing.T) {
	f := newFixture(t)
	ctx := context.Background()
	f.seedEndpoint(t, "priced-extract")
	f.seedTemplate(t, extract.Template{
		ID: "invoice", Name: "Invoice", SystemPrompt: "s", UserPrompt: "u",
		DefaultModel: "priced-extract", Enabled: true,
	})

	// A template the key isn't granted denies at enqueue.
	p2 := f.principal
	p2.AllowedExtractTemplatesAll = false
	p2.AllowedExtractTemplates = map[string]struct{}{"other": {}}
	if _, err := f.svc.Process(ctx, p2, extract.ProcessInput{
		TemplateID: "invoice", FileName: "a.txt", Bytes: []byte("x"), MimeType: "text/plain",
	}); !errors.Is(err, extract.ErrForbidden) {
		t.Fatalf("disallowed template: %v, want ErrForbidden", err)
	}

	// A model outside the key's allowlist enqueues durably but the executor
	// denies it at the worker attempt — before any upstream dispatch.
	p := f.principal
	p.AllowedModelsAll = false
	p.AllowedModels = map[string]struct{}{"other-model": {}}
	out, err := f.svc.Process(ctx, p, extract.ProcessInput{
		TemplateID: "invoice", FileName: "a.txt", Bytes: []byte("x"), MimeType: "text/plain",
	})
	if err != nil {
		t.Fatalf("process enqueue: %v", err)
	}
	_ = out
	claim := f.leaseClaim(t)
	_, aerr := f.svc.JobHandler()(ctx, claim, inputOpener(f, claim))
	if !errors.Is(aerr, inference.ErrDenied) && !errors.Is(aerr, inference.ErrNoRoute) {
		t.Fatalf("disallowed model attempt: %v, want ErrDenied/ErrNoRoute", aerr)
	}
	if f.upstream.callCount.Load() != 0 {
		t.Fatalf("upstream dispatched for disallowed model")
	}
}

func TestAttemptPricedModelReservesAndSettles(t *testing.T) {
	f := newFixture(t)
	ctx := context.Background()
	f.seedEndpoint(t, "priced-extract", "unpriced-extract")
	f.putPrice(t, "priced-extract", billing.UnitInput, 1000)
	f.putPrice(t, "priced-extract", billing.UnitOutput, 2000)
	f.seedTemplate(t, extract.Template{
		ID: "invoice", Name: "Invoice", SystemPrompt: "s", UserPrompt: "u",
		DefaultModel: "priced-extract", Enabled: true,
	})

	out, err := f.svc.Process(ctx, f.principal, extract.ProcessInput{
		TemplateID: "invoice", FileName: "invoice.txt", MimeType: "text/plain",
		Bytes: []byte("invoice text"),
	})
	if err != nil {
		t.Fatalf("process: %v", err)
	}
	// No provider dispatch without an executor.
	if f.upstream.callCount.Load() != 0 {
		t.Fatalf("provider called during Process")
	}

	claim := f.leaseClaim(t)
	resultRef, err := f.svc.JobHandler()(ctx, claim, inputOpener(f, claim))
	if err != nil {
		t.Fatalf("attempt: %v", err)
	}
	if resultRef == "" {
		t.Fatal("empty result ref")
	}
	if f.upstream.callCount.Load() != 1 {
		t.Fatalf("provider calls=%d, want 1", f.upstream.callCount.Load())
	}

	// Usage record exists (settled) keyed by the reservation.
	var resID string
	if err := f.db.SQL.QueryRowContext(ctx,
		`SELECT id FROM reservations WHERE tenant_id=$1::uuid AND job_id=$2 AND id NOT LIKE '%/%'`,
		f.tenant, out.JobID).Scan(&resID); err != nil {
		t.Fatalf("find reservation: %v", err)
	}
	rec, err := postgres.NewBillingRepo(f.db).GetUsageRecord(ctx, f.tenant, resID)
	if err != nil {
		t.Fatalf("usage record: %v", err)
	}
	if rec.Units[billing.UnitInput] != 10 || rec.Units[billing.UnitOutput] != 5 {
		t.Fatalf("unexpected usage: %+v", rec.Units)
	}
	if rec.AmountMicros <= 0 {
		t.Fatalf("expected positive charge, got %d", rec.AmountMicros)
	}

	// The fenced completion publishes the result ref.
	if _, err := claim.Complete(ctx, resultRef); err != nil {
		t.Fatalf("complete: %v", err)
	}
	detail, err := f.svc.GetJob(ctx, f.principal, out.JobID)
	if err != nil {
		t.Fatalf("get job: %v", err)
	}
	if detail.Result == nil || detail.Result.TokensUsed != 15 || detail.Result.Data["total"] != json.Number("1") {
		t.Fatalf("unexpected result: %+v", detail.Result)
	}
	if detail.Job.Status != jobs.StatusSucceeded {
		t.Fatalf("job status=%s", detail.Job.Status)
	}
}

func TestAttemptDegradesResponseFormatWhenEngineRefuses(t *testing.T) {
	f := newFixture(t)
	f.upstream.refuseResponseFormat.Store(true)
	ctx := context.Background()
	f.seedEndpoint(t, "priced-extract")
	f.putPrice(t, "priced-extract", billing.UnitInput, 1000)
	f.putPrice(t, "priced-extract", billing.UnitOutput, 2000)
	f.seedTemplate(t, extract.Template{
		ID: "invoice", Name: "Invoice", SystemPrompt: "s", UserPrompt: "u",
		DefaultModel: "priced-extract", Enabled: true,
	})

	out, err := f.svc.Process(ctx, f.principal, extract.ProcessInput{
		TemplateID: "invoice", FileName: "invoice.txt", MimeType: "text/plain",
		Bytes: []byte("invoice text"),
	})
	if err != nil {
		t.Fatalf("process: %v", err)
	}
	claim := f.leaseClaim(t)
	resultRef, err := f.svc.JobHandler()(ctx, claim, inputOpener(f, claim))
	if err != nil {
		t.Fatalf("attempt: %v", err)
	}
	if resultRef == "" {
		t.Fatal("empty result ref")
	}
	if got := f.upstream.callCount.Load(); got != 2 {
		t.Fatalf("provider calls=%d, want 2 (refused structured output, then retried without)", got)
	}
	if !bytes.Contains(f.upstream.bodies[0], []byte(`"response_format"`)) {
		t.Fatal("first provider call carried no response_format")
	}
	if bytes.Contains(f.upstream.bodies[1], []byte(`"response_format"`)) {
		t.Fatal("retry still carried response_format")
	}
	if _, err := claim.Complete(ctx, resultRef); err != nil {
		t.Fatalf("complete: %v", err)
	}
	detail, err := f.svc.GetJob(ctx, f.principal, out.JobID)
	if err != nil {
		t.Fatalf("get job: %v", err)
	}
	if detail.Job.Status != jobs.StatusSucceeded || detail.Result == nil || detail.Result.Data["total"] != json.Number("1") {
		t.Fatalf("unexpected result after degrade: %s %+v", detail.Job.Status, detail.Result)
	}
}
func TestVisionJobUsesImagePayloadAndStoresGeneratedResult(t *testing.T) {
	f := newFixture(t)
	ctx := context.Background()
	f.seedEndpoint(t, "priced-vision")
	for _, unit := range []billing.UnitKind{billing.UnitInput, billing.UnitCachedInput, billing.UnitOutput} {
		err := f.billing.PutPrice(ctx, f.tenant, "vision_"+string(unit), billing.Price{
			SubjectType: billing.PriceSubjectModel, SubjectID: "priced-vision",
			Capability: inference.CapabilityVision, Unit: unit,
			RateMicros: 1000, UnitQuantity: 1000, Currency: "USD",
			EffectiveFrom: time.Now().Add(-time.Hour).UTC(),
		}, f.principal.ActorID, time.Now().Add(-time.Hour).UTC())
		if err != nil {
			t.Fatalf("seed vision price: %v", err)
		}
	}
	f.upstream.response = []byte(`{"usage":{"prompt_tokens":8,"completion_tokens":2},"choices":[{"message":{"content":"{\"label\":\"page\"}"}}]}`)
	f.seedTemplate(t, extract.Template{
		ID: "image-labels", Name: "Image labels", SystemPrompt: "Describe visible items.",
		UserPrompt: "Return a label.", DefaultModel: "priced-vision", Enabled: true,
		OutputSchema: map[string]any{
			"type": "object", "properties": map[string]any{"label": map[string]any{"type": "string"}},
			"required": []any{"label"},
		},
	})
	image, err := os.ReadFile("testdata/scanned-page.png")
	if err != nil {
		t.Fatalf("read image fixture: %v", err)
	}
	out, err := f.svc.Process(ctx, f.principal, extract.ProcessInput{
		Operation: extract.OperationVision, TemplateID: "image-labels",
		FileName: "scanned-page.png", MimeType: "image/png", Bytes: image,
	})
	if err != nil {
		t.Fatalf("submit image task: %v", err)
	}
	claim := f.leaseClaim(t)
	resultRef, err := f.svc.JobHandler()(ctx, claim, inputOpener(f, claim))
	if err != nil {
		t.Fatalf("run image task: %v", err)
	}
	if _, err := claim.Complete(ctx, resultRef); err != nil {
		t.Fatalf("complete image task: %v", err)
	}
	detail, err := f.svc.GetJob(ctx, f.principal, out.JobID)
	if err != nil {
		t.Fatalf("read image result: %v", err)
	}
	if detail.Job.Status != jobs.StatusSucceeded || detail.Result == nil ||
		!detail.Result.Generated || detail.Result.Data["label"] != "page" {
		t.Fatalf("unexpected generated image result: %+v", detail)
	}
	if f.upstream.callCount.Load() != 1 {
		t.Fatalf("upstream calls=%d, want one", f.upstream.callCount.Load())
	}
}

func TestAttemptUsesSnapshotSchemaAndExactNumberValidation(t *testing.T) {
	for _, test := range []struct {
		name            string
		content         string
		wantValidation  bool
		wantExactAmount bool
	}{
		{name: "valid numeric edge", content: `{"amount":9007199254740993.0001}`, wantExactAmount: true},
		{name: "nested reference violation", content: `{"amount":"9007199254740993.0001"}`, wantValidation: true},
	} {
		t.Run(test.name, func(t *testing.T) {
			f := newFixture(t)
			ctx := context.Background()
			f.seedEndpoint(t, "priced-extract")
			f.putPrice(t, "priced-extract", billing.UnitInput, 1000)
			f.putPrice(t, "priced-extract", billing.UnitOutput, 2000)
			f.upstream.response, _ = json.Marshal(map[string]any{
				"id":      "c",
				"usage":   map[string]any{"prompt_tokens": 10, "completion_tokens": 5, "total_tokens": 15},
				"choices": []any{map[string]any{"message": map[string]any{"content": test.content}}},
			})
			f.seedTemplate(t, extract.Template{
				ID: "invoice", Name: "Invoice", SystemPrompt: "s", UserPrompt: "u",
				DefaultModel: "priced-extract", Enabled: true,
				OutputSchema: map[string]any{
					"$schema": "https://json-schema.org/draft/2020-12/schema",
					"$defs": map[string]any{
						"amount": map[string]any{"type": "number", "minimum": int64(9007199254740993)},
					},
					"type":       "object",
					"properties": map[string]any{"amount": map[string]any{"$ref": "#/$defs/amount"}},
					"required":   []any{"amount"},
				},
			})
			out, err := f.svc.Process(ctx, f.principal, extract.ProcessInput{
				TemplateID: "invoice", FileName: "invoice.txt", MimeType: "text/plain", Bytes: []byte("invoice text"),
			})
			if err != nil {
				t.Fatalf("process: %v", err)
			}
			claim := f.leaseClaim(t)
			resultRef, err := f.svc.JobHandler()(ctx, claim, inputOpener(f, claim))
			if err != nil {
				t.Fatalf("attempt: %v", err)
			}
			if _, err := claim.Complete(ctx, resultRef); err != nil {
				t.Fatalf("complete: %v", err)
			}
			detail, err := f.svc.GetJob(ctx, f.principal, out.JobID)
			if err != nil || detail.Result == nil {
				t.Fatalf("result readback: %+v, %v", detail, err)
			}
			if detail.Result.SchemaVersion != "https://json-schema.org/draft/2020-12/schema" {
				t.Fatalf("schema version not retained: %q", detail.Result.SchemaVersion)
			}
			amountSchema := detail.Result.OutputSchema["$defs"].(map[string]any)["amount"].(map[string]any)
			if amountSchema["minimum"] != json.Number("9007199254740993") {
				t.Fatalf("persisted schema number was rounded: %#v", amountSchema["minimum"])
			}
			if test.wantValidation {
				if len(detail.Result.ValidationErrors) == 0 || !strings.Contains(detail.Result.ValidationErrors[0], "/amount") {
					t.Fatalf("nested schema violation not reported with instance location: %v", detail.Result.ValidationErrors)
				}
			} else if len(detail.Result.ValidationErrors) != 0 {
				t.Fatalf("valid result rejected: %v", detail.Result.ValidationErrors)
			}
			if test.wantExactAmount && detail.Result.Data["amount"] != json.Number("9007199254740993.0001") {
				t.Fatalf("result number was rounded: %#v", detail.Result.Data["amount"])
			}
		})
	}
}

func TestAttemptUnpricedModelRefusesBeforeProvider(t *testing.T) {
	f := newFixture(t)
	ctx := context.Background()
	f.seedEndpoint(t, "unpriced-extract")
	f.seedTemplate(t, extract.Template{
		ID: "invoice", Name: "Invoice", SystemPrompt: "s", UserPrompt: "u",
		DefaultModel: "unpriced-extract", Enabled: true,
	})

	_, err := f.svc.Process(ctx, f.principal, extract.ProcessInput{
		TemplateID: "invoice", FileName: "invoice.txt", MimeType: "text/plain",
		Bytes: []byte("invoice text"),
	})
	if err != nil {
		t.Fatalf("process: %v", err)
	}

	claim := f.leaseClaim(t)
	_, err = f.svc.JobHandler()(ctx, claim, inputOpener(f, claim))
	if !errors.Is(err, billing.ErrPriceMissing) {
		t.Fatalf("attempt err=%v, want ErrPriceMissing", err)
	}
	if f.upstream.callCount.Load() != 0 {
		t.Fatalf("provider invoked before price admission")
	}
	// No usage row — nothing was charged.
	var count int
	if err := f.db.SQL.QueryRowContext(ctx,
		`SELECT COUNT(*) FROM usage_records WHERE tenant_id=$1::uuid`, f.tenant).Scan(&count); err != nil {
		t.Fatalf("count usage: %v", err)
	}
	if count != 0 {
		t.Fatalf("usage recorded for unpriced dispatch")
	}
}

func TestCrossTenantJobReadsDenyIdentically(t *testing.T) {
	f := newFixture(t)
	ctx := context.Background()
	f.seedEndpoint(t, "priced-extract")
	f.seedTemplate(t, extract.Template{
		ID: "invoice", Name: "Invoice", SystemPrompt: "s", UserPrompt: "u",
		DefaultModel: "priced-extract", Enabled: true,
	})
	out, err := f.svc.Process(ctx, f.principal, extract.ProcessInput{
		TemplateID: "invoice", FileName: "invoice.txt", MimeType: "text/plain",
		Bytes: []byte("invoice text"),
	})
	if err != nil {
		t.Fatalf("process: %v", err)
	}

	// Same tenant, different actor → same outcome as an unknown id.
	otherUser := testdb.SeedUser(t, f.db, f.tenant, "other@example.com", nil)
	otherKey, _ := testdb.SeedAPIKey(t, f.db, f.tenant, testdb.SeedKeySpec{
		Name: "other-key", OwnerUserID: otherUser.ID, Scopes: []string{"extract"},
	})
	otherPrincipal, err := auth.NewService(postgres.NewIdentityRepo(f.db), "").Authenticate(ctx, otherKey)
	if err != nil {
		t.Fatalf("authenticate other: %v", err)
	}
	if _, err := f.svc.GetJob(ctx, otherPrincipal, out.JobID); !errors.Is(err, extract.ErrNotFound) {
		t.Fatalf("cross-actor job read: %v, want ErrNotFound", err)
	}
	if _, err := f.svc.GetJob(ctx, f.principal, "job_missing"); !errors.Is(err, extract.ErrNotFound) {
		t.Fatalf("unknown job read: %v, want ErrNotFound", err)
	}

	// Other actor's job list does not include this job.
	list, total, err := f.svc.ListJobs(ctx, otherPrincipal, extract.OperationProcess, 20, 0)
	if err != nil || len(list) != 0 || total != 0 {
		t.Fatalf("other actor list: %d/%d %v", len(list), total, err)
	}
}
func TestExplicitDeleteFencesAndCleansOnlyUnsharedArtifacts(t *testing.T) {
	f := newFixture(t)
	ctx := context.Background()
	f.seedEndpoint(t, "priced-extract")
	f.putPrice(t, "priced-extract", billing.UnitInput, 1000)
	f.putPrice(t, "priced-extract", billing.UnitOutput, 2000)
	f.seedTemplate(t, extract.Template{
		ID: "invoice", Name: "Invoice", SystemPrompt: "s", UserPrompt: "u",
		DefaultModel: "priced-extract", Enabled: true,
	})
	submit := func(content string) extract.ProcessOutput {
		out, err := f.svc.Process(ctx, f.principal, extract.ProcessInput{
			TemplateID: "invoice", FileName: "invoice.txt", MimeType: "text/plain", Bytes: []byte(content),
		})
		if err != nil {
			t.Fatalf("submit job: %v", err)
		}
		return out
	}
	first := submit("shared evidence")
	firstRow, err := f.jobsSvc.Get(ctx, f.principal, first.JobID)
	if err != nil {
		t.Fatal(err)
	}
	shared, _, err := f.jobsSvc.Enqueue(ctx, f.principal, jobs.EnqueueRequest{
		Operation: extract.OperationProcess, IdempotencyKey: "shared-reference",
		InputRef: firstRow.InputRef, InputHash: firstRow.InputHash,
		Policy: map[string]any{"file_name": "invoice.txt", "mime_type": "text/plain"},
	})
	if err != nil {
		t.Fatalf("enqueue shared reference: %v", err)
	}
	claim := f.leaseClaim(t)
	resultRef, err := f.svc.JobHandler()(ctx, claim, inputOpener(f, claim))
	if err != nil {
		t.Fatalf("run extraction: %v", err)
	}
	if _, err := claim.Complete(ctx, resultRef); err != nil {
		t.Fatalf("complete extraction: %v", err)
	}
	detail, err := f.svc.GetJob(ctx, f.principal, first.JobID)
	if err != nil || detail.Result == nil {
		t.Fatalf("read result: detail=%+v err=%v", detail, err)
	}
	original, filename, contentType, err := f.svc.Original(ctx, f.principal, first.JobID)
	if err != nil {
		t.Fatalf("read original: %v", err)
	}
	body, err := io.ReadAll(original)
	_ = original.Close()
	if err != nil || filename != "invoice.txt" || contentType != "text/plain" || string(body) != "shared evidence" {
		t.Fatalf("original = (%q,%q,%q,%v)", filename, contentType, body, err)
	}
	if err := f.svc.DeleteJob(ctx, f.principal, first.JobID); err != nil {
		t.Fatalf("delete completed job: %v", err)
	}
	if _, err := f.store.Read(ctx, artifacts.Ref{TenantID: f.tenant, ID: firstRow.InputRef}); err != nil {
		t.Fatalf("shared original should survive deletion: %v", err)
	}
	if _, err := f.store.Read(ctx, artifacts.Ref{TenantID: f.tenant, ID: resultRef}); !errors.Is(err, artifacts.ErrNotFound) {
		t.Fatalf("unshared result remains readable, error=%v", err)
	}
	sharedRow, err := f.jobsSvc.Get(ctx, f.principal, shared.ID)
	if err != nil || sharedRow.Status != jobs.StatusQueued {
		t.Fatalf("shared job was changed by other deletion: %+v %v", sharedRow, err)
	}

	second := submit("cancel then delete")
	if err := f.svc.DeleteJob(ctx, f.principal, second.JobID); !errors.Is(err, extract.ErrDeletePending) {
		t.Fatalf("first active delete = %v, want cancellation pending", err)
	}
	if err := f.svc.DeleteJob(ctx, f.principal, second.JobID); err != nil {
		t.Fatalf("delete cancelled job: %v", err)
	}
	if err := f.svc.ReconcileDeletedJobs(ctx); err != nil {
		t.Fatalf("restart cleanup reconciliation: %v", err)
	}
	var status string
	if err := f.db.SQL.QueryRowContext(ctx, `SELECT status FROM jobs WHERE tenant_id=$1::uuid AND id=$2`, f.tenant, second.JobID).Scan(&status); err != nil || status != jobs.StatusDeleted {
		t.Fatalf("audit row status=%q err=%v", status, err)
	}
}

func TestExtractionReadsAndDeletionRecheckCurrentSourceAudience(t *testing.T) {
	f := newFixture(t)
	ctx := context.Background()
	f.seedEndpoint(t, "priced-extract")
	f.putPrice(t, "priced-extract", billing.UnitInput, 1000)
	f.putPrice(t, "priced-extract", billing.UnitOutput, 1000)
	f.seedTemplate(t, extract.Template{
		ID: "invoice", Name: "Invoice", SystemPrompt: "s", UserPrompt: "u",
		DefaultModel: "priced-extract", Enabled: true,
	})
	out, err := f.svc.Process(ctx, f.principal, extract.ProcessInput{
		TemplateID: "invoice", FileName: "invoice.txt", MimeType: "text/plain", Bytes: []byte("audience evidence"),
	})
	if err != nil {
		t.Fatalf("submit: %v", err)
	}
	claim := f.leaseClaim(t)
	resultRef, err := f.svc.JobHandler()(ctx, claim, inputOpener(f, claim))
	if err != nil {
		t.Fatalf("run extraction: %v", err)
	}
	if _, err := claim.Complete(ctx, resultRef); err != nil {
		t.Fatalf("complete extraction: %v", err)
	}
	obj, err := f.sources.UpsertObject(ctx, source.Object{
		TenantID: f.tenant, ID: "extract-audience-object", Kind: "upload", NativeID: "audience-file",
		AuthorizationMode: source.AuthorizationAudience, Audience: []string{f.principal.ActorID}, ObservedSeq: 1,
	})
	if err != nil {
		t.Fatalf("create source object: %v", err)
	}
	if err := f.sources.InsertObjectVersion(ctx, f.tenant, source.ObjectVersion{
		ObjectID: obj.ID, ObservedSeq: 1, ArtifactRef: claim.Job().InputRef,
		OriginalFilename: "invoice.txt", MimeType: "text/plain",
	}); err != nil {
		t.Fatalf("create source version: %v", err)
	}
	objectRef, _ := json.Marshal(source.ObjectRef{TenantID: f.tenant, ObjectID: obj.ID, ObservedSeq: 1})
	if _, err := f.db.SQL.ExecContext(ctx, `UPDATE jobs SET policy=policy || jsonb_build_object('object_ref',$3::jsonb) WHERE tenant_id=$1::uuid AND id=$2`, f.tenant, out.JobID, objectRef); err != nil {
		t.Fatalf("bind source object to job: %v", err)
	}
	if _, err := f.svc.GetJob(ctx, f.principal, out.JobID); err != nil {
		t.Fatalf("authorized source result read: %v", err)
	}
	if _, err := f.db.SQL.ExecContext(ctx, `UPDATE source_objects SET audience='[]'::jsonb WHERE tenant_id=$1::uuid AND id=$2`, f.tenant, obj.ID); err != nil {
		t.Fatalf("revoke source audience: %v", err)
	}
	if _, err := f.svc.GetJob(ctx, f.principal, out.JobID); !errors.Is(err, extract.ErrNotFound) {
		t.Fatalf("revoked result read = %v, want not found", err)
	}
	if _, _, _, err := f.svc.Original(ctx, f.principal, out.JobID); !errors.Is(err, extract.ErrNotFound) {
		t.Fatalf("revoked original read = %v, want not found", err)
	}
	if err := f.svc.DeleteJob(ctx, f.principal, out.JobID); !errors.Is(err, extract.ErrNotFound) {
		t.Fatalf("revoked deletion = %v, want not found", err)
	}
}

// A persisted restriction denying the routed capability on the input artifact
// stops the worker attempt before any model dispatch — destinations can't be
// widened by the caller.
func TestAttemptRestrictedInputDeniesBeforeDispatch(t *testing.T) {
	f := newFixture(t)
	ctx := context.Background()
	f.seedEndpoint(t, "priced-extract")
	f.putPrice(t, "priced-extract", billing.UnitInput, 1000)
	f.putPrice(t, "priced-extract", billing.UnitOutput, 2000)
	f.seedTemplate(t, extract.Template{
		ID: "invoice", Name: "Invoice", SystemPrompt: "s", UserPrompt: "u",
		DefaultModel: "priced-extract", Enabled: true,
	})

	out, err := f.svc.Process(ctx, f.principal, extract.ProcessInput{
		TemplateID: "invoice", FileName: "invoice.txt", MimeType: "text/plain",
		Bytes: []byte("invoice text"),
	})
	if err != nil {
		t.Fatalf("process: %v", err)
	}
	job, err := f.jobsSvc.Get(ctx, f.principal, out.JobID)
	if err != nil {
		t.Fatalf("get job: %v", err)
	}
	// Deny the text capability for this input artifact.
	if err := f.sources.SetRestriction(ctx, source.SourceRestriction{
		TenantID:           f.tenant,
		SubjectKind:        source.RestrictionArtifact,
		SubjectID:          job.InputRef,
		DeniedCapabilities: []string{inference.CapabilityChat},
	}); err != nil {
		t.Fatalf("set restriction: %v", err)
	}

	claim := f.leaseClaim(t)
	if _, err := f.svc.JobHandler()(ctx, claim, inputOpener(f, claim)); !errors.Is(err, inference.ErrDenied) {
		t.Fatalf("restricted attempt: %v, want ErrDenied", err)
	}
	if f.upstream.callCount.Load() != 0 {
		t.Fatalf("upstream dispatched despite denied capability")
	}
}

func TestProcessAdmissionLimitCountsOnlyQueuedAndRunning(t *testing.T) {
	f := newFixture(t)
	ctx := context.Background()
	f.seedEndpoint(t, "priced-extract")
	f.seedTemplate(t, extract.Template{
		ID: "invoice", Name: "Invoice", SystemPrompt: "s", UserPrompt: "u",
		DefaultModel: "priced-extract", Enabled: true,
	})
	for i := range extract.ExtractAdmissionLimit {
		if _, err := f.svc.Process(ctx, f.principal, extract.ProcessInput{
			TemplateID: "invoice", FileName: "admit.txt", MimeType: "text/plain",
			Bytes: []byte(fmt.Sprintf("admission %d", i)),
		}); err != nil {
			t.Fatalf("submission rejected before admission limit: %v", err)
		}
	}
	if _, err := f.svc.Process(ctx, f.principal, extract.ProcessInput{
		TemplateID: "invoice", FileName: "admit.txt", MimeType: "text/plain",
		Bytes: []byte("admission-over"),
	}); !errors.Is(err, extract.ErrExtractionAdmissionLimited) {
		t.Fatalf("submission over limit err=%v, want admission limit", err)
	}
	claim, err := f.jobsSvc.Lease(ctx, "admission-test", time.Minute)
	if err != nil || claim == nil {
		t.Fatalf("lease: claim=%v err=%v", claim, err)
	}
	if _, err := claim.Complete(ctx, ""); err != nil {
		t.Fatalf("complete one job: %v", err)
	}
	if _, err := f.svc.Process(ctx, f.principal, extract.ProcessInput{
		TemplateID: "invoice", FileName: "admit.txt", MimeType: "text/plain",
		Bytes: []byte("admission-after"),
	}); err != nil {
		t.Fatalf("terminal job still counted: %v", err)
	}
}

// TestProcessBoundedByStoredTemplateGrants proves the stored role grant
// document is the extract template gate's single source (R2): a principal
// whose role grants extract_templates [t1] passes the gate for t1 and fails
// for unlisted t2, and a SQL revocation denies t1 on the next request with
// no re-login — the per-request union, not a cached credential.
func TestProcessBoundedByStoredTemplateGrants(t *testing.T) {
	f := newFixture(t)
	ctx := context.Background()
	f.seedEndpoint(t, "priced-extract")
	for _, tpl := range []extract.Template{
		{ID: "t1", Name: "Granted", SystemPrompt: "s", UserPrompt: "u", DefaultModel: "priced-extract", Enabled: true},
		{ID: "t2", Name: "Unlisted", SystemPrompt: "s", UserPrompt: "u", DefaultModel: "priced-extract", Enabled: true},
	} {
		f.seedTemplate(t, tpl)
	}

	user := testdb.SeedUserWithGrants(t, f.db, f.tenant, "granted@example.com",
		[]string{"extract"},
		auth.ResourceGrants{ExtractTemplates: auth.Allowlist{Items: []string{"t1"}}})
	authSvc := auth.NewService(postgres.NewIdentityRepo(f.db), "")
	principal, err := authSvc.PrincipalForActor(ctx, f.tenant, user.ID)
	if err != nil {
		t.Fatalf("principal for granted user: %v", err)
	}

	// Granted template runs past the template gate: the extraction job row
	// exists before Process returns (same seam as
	// TestProcessPersistsInputArtifactAndJobRow).
	out, err := f.svc.Process(ctx, principal, extract.ProcessInput{
		TemplateID: "t1", FileName: "invoice.txt", MimeType: "text/plain",
		Bytes: []byte("invoice text"),
	})
	if err != nil {
		t.Fatalf("granted template process: %v", err)
	}
	job, err := f.jobsSvc.Get(ctx, principal, out.JobID)
	if err != nil || job.Operation != extract.OperationProcess || job.InputRef == "" {
		t.Fatalf("granted template job row missing: job=%+v err=%v", job, err)
	}

	// Unlisted template denies at the gate.
	if _, err := f.svc.Process(ctx, principal, extract.ProcessInput{
		TemplateID: "t2", FileName: "invoice.txt", MimeType: "text/plain",
		Bytes: []byte("invoice text"),
	}); !errors.Is(err, extract.ErrForbidden) {
		t.Fatalf("unlisted template = %v, want ErrForbidden", err)
	}

	// SQL revocation on the stored document denies the SAME template on the
	// next request — the principal union is rebuilt per request, no re-login.
	if _, err := f.db.SQL.ExecContext(ctx,
		`UPDATE roles SET allowed_extract_templates='{"mode":"list","items":[]}'::jsonb
		 WHERE tenant_id = $1::uuid AND id IN (SELECT role_id FROM user_roles WHERE user_id = $2)`,
		f.tenant, user.ID); err != nil {
		t.Fatalf("revoke template grant: %v", err)
	}
	principal, err = authSvc.PrincipalForActor(ctx, f.tenant, user.ID)
	if err != nil {
		t.Fatalf("reload principal: %v", err)
	}
	if _, err := f.svc.Process(ctx, principal, extract.ProcessInput{
		TemplateID: "t1", FileName: "invoice.txt", MimeType: "text/plain",
		Bytes: []byte("invoice text"),
	}); !errors.Is(err, extract.ErrForbidden) {
		t.Fatalf("revoked template = %v, want ErrForbidden", err)
	}
}

// TestListJobsExtractionOperationScope proves the extraction list and stats
// count only extract.process/extract.vision for the owner: a document.preview
// job in the same window never appears in rows or totals, explicit filters
// isolate each member operation, foreign actors stay absent, and non-member
// filter values are rejected (T-08-04-01/03).
func TestListJobsExtractionOperationScope(t *testing.T) {
	f := newFixture(t)
	ctx := context.Background()
	f.seedEndpoint(t, "priced-extract")
	f.seedTemplate(t, extract.Template{
		ID: "invoice", Name: "Invoice", SystemPrompt: "s", UserPrompt: "u",
		DefaultModel: "priced-extract", Enabled: true,
	})
	submit := func(op, name, mime, content string) extract.ProcessOutput {
		out, err := f.svc.Process(ctx, f.principal, extract.ProcessInput{
			Operation: op, TemplateID: "invoice", FileName: name, MimeType: mime,
			Bytes: []byte(content),
		})
		if err != nil {
			t.Fatalf("submit %s job: %v", op, err)
		}
		return out
	}
	processOut := submit(extract.OperationProcess, "invoice.txt", "text/plain", "invoice text")
	pngBytes, err := base64.StdEncoding.DecodeString(
		"iVBORw0KGgoAAAANSUhEUgAAAAEAAAABCAYAAAAfFcSJAAAADUlEQVR42mP8z8BQDwAEhQGAhKmMIQAAAABJRU5ErkJggg==")
	if err != nil {
		t.Fatalf("decode png fixture: %v", err)
	}
	submit(extract.OperationVision, "scan.png", "image/png", string(pngBytes))

	// A shared preview job owned by the same submitter — the contamination
	// case Home's counters suffered before the operation predicate.
	processRow, err := f.jobsSvc.Get(ctx, f.principal, processOut.JobID)
	if err != nil {
		t.Fatal(err)
	}
	if _, _, err := f.jobsSvc.Enqueue(ctx, f.principal, jobs.EnqueueRequest{
		Operation:      extract.OperationPreview,
		IdempotencyKey: "scope-preview-fixture",
		InputRef:       processRow.InputRef, InputHash: processRow.InputHash,
		Policy: map[string]any{},
	}); err != nil {
		t.Fatalf("enqueue preview fixture: %v", err)
	}

	// Default list: the two extraction jobs only — the preview never surfaces.
	list, total, err := f.svc.ListJobs(ctx, f.principal, "", 20, 0)
	if err != nil || total != 2 || len(list) != 2 {
		t.Fatalf("default list: %d/%d %v, want 2/2", len(list), total, err)
	}
	names := map[string]bool{}
	for _, j := range list {
		names[j.FileName] = true
	}
	if !names["invoice.txt"] || !names["scan.png"] || names[""] {
		t.Fatalf("default list files=%v, want the process+vision fixtures only", names)
	}

	// Explicit filters isolate exactly one operation, totals follow the set.
	for op, wantFile := range map[string]string{
		extract.OperationProcess: "invoice.txt", extract.OperationVision: "scan.png",
	} {
		list, total, err = f.svc.ListJobs(ctx, f.principal, op, 20, 0)
		if err != nil || total != 1 || len(list) != 1 {
			t.Fatalf("filter %s: %d/%d %v, want 1/1", op, len(list), total, err)
		}
		if list[0].FileName != wantFile {
			t.Fatalf("filter %s returned %s row", op, list[0].FileName)
		}
	}

	// Closed set: bogus AND shared operation values are rejected.
	for _, bogus := range []string{"bogus", extract.OperationPreview} {
		if _, _, err := f.svc.ListJobs(ctx, f.principal, bogus, 20, 0); !errors.Is(err, extract.ErrInvalidOperation) {
			t.Fatalf("filter %q = %v, want ErrInvalidOperation", bogus, err)
		}
	}

	// Stats are extraction-only: the preview does not move either counter.
	recent, totalStat, err := f.svc.JobStats(ctx, f.principal, time.Now().UTC().Add(-24*time.Hour))
	if err != nil || recent != 2 || totalStat != 2 {
		t.Fatalf("job stats: %d/%d %v, want 2/2", recent, totalStat, err)
	}

	// Another member of the same tenant sees none of these jobs.
	otherUser := testdb.SeedUser(t, f.db, f.tenant, "scope-other@example.com", nil)
	otherKey, _ := testdb.SeedAPIKey(t, f.db, f.tenant, testdb.SeedKeySpec{
		Name: "scope-other-key", OwnerUserID: otherUser.ID, Scopes: []string{"extract"},
	})
	otherPrincipal, err := auth.NewService(postgres.NewIdentityRepo(f.db), "").Authenticate(ctx, otherKey)
	if err != nil {
		t.Fatalf("authenticate other: %v", err)
	}
	if list, total, err = f.svc.ListJobs(ctx, otherPrincipal, "", 20, 0); err != nil || len(list) != 0 || total != 0 {
		t.Fatalf("foreign actor list: %d/%d %v", len(list), total, err)
	}
}

// TestTemplateListingGrantAuthority pins the D-25 authority split: the
// authoring list follows extract.manage, the executable picker follows ONLY
// stored execution grants — manage never implies execution and grants never
// imply administration (T-08-04-02).
func TestTemplateListingGrantAuthority(t *testing.T) {
	f := newFixture(t)
	ctx := context.Background()
	f.seedEndpoint(t, "priced-extract")
	for _, tpl := range []extract.Template{
		{ID: "t1", Name: "Granted", SystemPrompt: "s", UserPrompt: "u", DefaultModel: "priced-extract", Enabled: true},
		{ID: "t2", Name: "Unlisted", SystemPrompt: "s", UserPrompt: "u", DefaultModel: "priced-extract", Enabled: true},
	} {
		f.seedTemplate(t, tpl)
	}
	authSvc := auth.NewService(postgres.NewIdentityRepo(f.db), "")
	principalFor := func(email string, scopes []string, grants auth.ResourceGrants) auth.Principal {
		user := testdb.SeedUserWithGrants(t, f.db, f.tenant, email, scopes, grants)
		p, err := authSvc.PrincipalForActor(ctx, f.tenant, user.ID)
		if err != nil {
			t.Fatalf("principal for %s: %v", email, err)
		}
		return p
	}

	// Zero-template-grant manager: full authoring list, EMPTY picker,
	// execution denied — manage is not an implicit grant.
	manager := principalFor("manager@example.com", []string{"extract", "extract.manage"}, auth.ResourceGrants{})
	templates, err := f.svc.ListTemplates(ctx, manager)
	if err != nil || len(templates) != 2 {
		t.Fatalf("manager authoring list: %d %v, want 2 templates", len(templates), err)
	}
	if picker, err := f.svc.ListExecutableTemplates(ctx, manager); err != nil || len(picker) != 0 {
		t.Fatalf("manager picker: %d %v, want empty", len(picker), err)
	}
	if _, err := f.svc.ProcessReader(ctx, manager, extract.ProcessInput{
		TemplateID: "t1", FileName: "invoice.txt", MimeType: "text/plain", Bytes: []byte("x"),
	}); !errors.Is(err, extract.ErrForbidden) {
		t.Fatalf("manager process = %v, want ErrForbidden", err)
	}

	// Granted member: exactly the granted template in BOTH lists and usable.
	granted := principalFor("granted@example.com", []string{"extract"},
		auth.ResourceGrants{ExtractTemplates: auth.Allowlist{Items: []string{"t1"}}})
	for name, list := range map[string]func() ([]extract.Template, error){
		"authoring": func() ([]extract.Template, error) { return f.svc.ListTemplates(ctx, granted) },
		"picker":    func() ([]extract.Template, error) { return f.svc.ListExecutableTemplates(ctx, granted) },
	} {
		templates, err := list()
		if err != nil || len(templates) != 1 || templates[0].ID != "t1" {
			t.Fatalf("granted member %s list: %+v %v, want [t1]", name, templates, err)
		}
	}
	if _, err := f.svc.ProcessReader(ctx, granted, extract.ProcessInput{
		TemplateID: "t1", FileName: "invoice.txt", MimeType: "text/plain", Bytes: []byte("invoice text"),
	}); err != nil {
		t.Fatalf("granted member process: %v", err)
	}

	// Ungranted member: both lists empty, execution denied.
	ungranted := principalFor("ungranted@example.com", []string{"extract"}, auth.ResourceGrants{})
	for name, list := range map[string]func() ([]extract.Template, error){
		"authoring": func() ([]extract.Template, error) { return f.svc.ListTemplates(ctx, ungranted) },
		"picker":    func() ([]extract.Template, error) { return f.svc.ListExecutableTemplates(ctx, ungranted) },
	} {
		if templates, err := list(); err != nil || len(templates) != 0 {
			t.Fatalf("ungranted member %s list: %+v %v, want empty", name, templates, err)
		}
	}
	if _, err := f.svc.ProcessReader(ctx, ungranted, extract.ProcessInput{
		TemplateID: "t1", FileName: "invoice.txt", MimeType: "text/plain", Bytes: []byte("x"),
	}); !errors.Is(err, extract.ErrForbidden) {
		t.Fatalf("ungranted member process = %v, want ErrForbidden", err)
	}
}
