package extract_test

import (
	"context"
	"encoding/json"
	"errors"
	"io"
	"path/filepath"
	"sync/atomic"
	"testing"
	"time"

	"enclava-go/internal/artifacts"
	"enclava-go/internal/auth"
	"enclava-go/internal/billing"
	"enclava-go/internal/extract"
	"enclava-go/internal/inference"
	"enclava-go/internal/jobs"
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
}

func (f *fakeUpstream) CreateChatCompletion(ctx context.Context, body []byte) ([]byte, error) {
	f.callCount.Add(1)
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

// putPrice seeds one model rate for the vision capability extraction
// dispatches under.
func (f fixture) putPrice(t *testing.T, model string, unit billing.UnitKind, rateMicros int64) {
	t.Helper()
	f.putPriceRow(t, model, unit, rateMicros)
	// Estimates always bound cached input like input, so both rates are needed.
	if unit == billing.UnitInput {
		f.putPriceRow(t, model, billing.UnitCachedInput, rateMicros)
	}
}

func (f fixture) putPriceRow(t *testing.T, model string, unit billing.UnitKind, rateMicros int64) {
	t.Helper()
	err := f.billing.PutPrice(context.Background(), f.tenant, "price_"+model+"_"+string(unit), billing.Price{
		SubjectType:   billing.PriceSubjectModel,
		SubjectID:     model,
		Capability:    inference.CapabilityVision,
		Unit:          unit,
		RateMicros:    rateMicros,
		UnitQuantity:  1000,
		Currency:      "USD",
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
	f.seedTemplate(t, extract.Template{
		ID: "invoice", Name: "Invoice", SystemPrompt: "s", UserPrompt: "u",
		DefaultModel: "priced-extract", Enabled: true,
	})

	out, err := f.svc.Process(ctx, f.principal, extract.ProcessInput{
		TemplateID: "invoice", FileName: "invoice.pdf", MimeType: "application/pdf",
		Bytes: []byte("%PDF-1.7 fake"),
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
		TemplateID: "invoice", FileName: "a.pdf", Bytes: []byte("x"), MimeType: "application/pdf",
	}); !errors.Is(err, extract.ErrForbidden) {
		t.Fatalf("disallowed template: %v, want ErrForbidden", err)
	}

	// A model outside the key's allowlist enqueues durably but the executor
	// denies it at the worker attempt — before any upstream dispatch.
	p := f.principal
	p.AllowedModelsAll = false
	p.AllowedModels = map[string]struct{}{"other-model": {}}
	out, err := f.svc.Process(ctx, p, extract.ProcessInput{
		TemplateID: "invoice", FileName: "a.pdf", Bytes: []byte("x"), MimeType: "application/pdf",
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
		TemplateID: "invoice", FileName: "invoice.pdf", MimeType: "application/pdf",
		Bytes: []byte("%PDF-1.7 fake"),
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
	if detail.Result == nil || detail.Result.TokensUsed != 15 || detail.Result.Data["total"] != float64(1) {
		t.Fatalf("unexpected result: %+v", detail.Result)
	}
	if detail.Job.Status != jobs.StatusSucceeded {
		t.Fatalf("job status=%s", detail.Job.Status)
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
		TemplateID: "invoice", FileName: "invoice.pdf", MimeType: "application/pdf",
		Bytes: []byte("%PDF-1.7 fake"),
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
	f.seedTemplate(t, extract.Template{
		ID: "invoice", Name: "Invoice", SystemPrompt: "s", UserPrompt: "u",
		DefaultModel: "priced-extract", Enabled: true,
	})
	out, err := f.svc.Process(ctx, f.principal, extract.ProcessInput{
		TemplateID: "invoice", FileName: "invoice.pdf", MimeType: "application/pdf",
		Bytes: []byte("%PDF-1.7 fake"),
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
	list, total, err := f.svc.ListJobs(ctx, otherPrincipal, 20, 0)
	if err != nil || len(list) != 0 || total != 0 {
		t.Fatalf("other actor list: %d/%d %v", len(list), total, err)
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
		TemplateID: "invoice", FileName: "invoice.pdf", MimeType: "application/pdf",
		Bytes: []byte("%PDF-1.7 fake"),
	})
	if err != nil {
		t.Fatalf("process: %v", err)
	}
	job, err := f.jobsSvc.Get(ctx, f.principal, out.JobID)
	if err != nil {
		t.Fatalf("get job: %v", err)
	}
	// Deny the vision capability for this input artifact.
	if err := f.sources.SetRestriction(ctx, source.SourceRestriction{
		TenantID:           f.tenant,
		SubjectKind:        source.RestrictionArtifact,
		SubjectID:          job.InputRef,
		DeniedCapabilities: []string{inference.CapabilityVision},
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
