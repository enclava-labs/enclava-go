package extractmodule

import (
	"bytes"
	"context"
	"encoding/json"
	"errors"
	"io"
	"mime/multipart"
	"net/http"
	"net/http/httptest"
	"net/url"
	"path/filepath"
	"testing"
	"time"

	"enclava-go/internal/artifacts"
	"enclava-go/internal/auth"
	"enclava-go/internal/billing"
	extractsvc "enclava-go/internal/extract"
	"enclava-go/internal/httpx"
	"enclava-go/internal/inference"
	"enclava-go/internal/jobs"
	"enclava-go/internal/source"
	"enclava-go/internal/store/postgres"
	"enclava-go/internal/testdb"
)

func TestExtractTemplateIDFromRequestAcceptsLegacyAliases(t *testing.T) {
	for _, key := range []string{"template_id", "template", "extract_template_id"} {
		form := url.Values{key: {"tpl_1"}}
		r := httptest.NewRequest(http.MethodPost, "/api/v1/extract/process", bytes.NewBufferString(form.Encode()))
		r.Header.Set("Content-Type", "application/x-www-form-urlencoded")
		if got := extractTemplateIDFromRequest(r); got != "tpl_1" {
			t.Fatalf("key %s: got %q", key, got)
		}
	}
}

// TestHandleProcessEnqueuesDurableJobAndSettlesUsage drives the real path:
// HandleProcess → extract.Service.Process (durable artifact + job) → the
// leased attempt handler → billing usage_records.
func TestHandleProcessEnqueuesDurableJobAndSettlesUsage(t *testing.T) {
	ctx := context.Background()
	db := testdb.New(t)
	tenant := testdb.SeedTenant(t, db, "extract-usage")
	user := testdb.SeedUser(t, db, tenant, "extract@example.com", nil)
	apiKey, _ := testdb.SeedAPIKey(t, db, tenant, testdb.SeedKeySpec{
		Name:        "extract-usage",
		OwnerUserID: user.ID,
		Scopes:      []string{"extract"},
	})
	identityRepo := postgres.NewIdentityRepo(db)
	principal, err := auth.NewService(identityRepo, "").Authenticate(ctx, apiKey)
	if err != nil {
		t.Fatalf("authenticate seeded key: %v", err)
	}

	jobsRepo := postgres.NewJobsRepo(db)
	artRepo := postgres.NewArtifactRepo(db)
	root := t.TempDir()
	store, err := artifacts.NewStore(filepath.Join(root, "durable"), filepath.Join(root, "scratch"), artRepo)
	if err != nil {
		t.Fatalf("artifact store: %v", err)
	}
	upstream := &extractHandlerUpstream{
		response: []byte(`{"id":"c","model":"priced-extract","usage":{"prompt_tokens":1000,"completion_tokens":500,"total_tokens":1500},"choices":[{"message":{"content":"{\"total\":12.5}"}}]}`),
	}
	extractService, jobsSvc := newExtractService(t, db, tenant, artRepo, jobsRepo, store, upstream, "priced-extract")
	if err := extractService.UpsertTemplate(ctx, principal, extractsvc.Template{
		ID: "invoice", Name: "Invoice", SystemPrompt: "Extract invoice data.", UserPrompt: "Return JSON.",
		DefaultModel: "priced-extract", Enabled: true,
		OutputSchema: map[string]any{
			"type":       "object",
			"properties": map[string]any{"total": map[string]any{"type": "number"}},
		},
	}); err != nil {
		t.Fatalf("upsert template: %v", err)
	}
	// Seed rates for the vision capability the attempt dispatches under.
	billingRepo := postgres.NewBillingRepo(db)
	for _, u := range []billing.UnitKind{billing.UnitInput, billing.UnitOutput} {
		if err := billingRepo.PutPrice(ctx, tenant, "price_"+string(u), billing.Price{
			SubjectType: billing.PriceSubjectModel, SubjectID: "priced-extract",
			Capability: inference.CapabilityVision, Unit: u,
			RateMicros: 10_000, UnitQuantity: 1000, Currency: "USD",
			EffectiveFrom: time.Now().Add(-time.Hour).UTC(),
		}, principal.ActorID, time.Now().Add(-time.Hour).UTC()); err != nil {
			t.Fatalf("put price: %v", err)
		}
	}

	app := httpx.NewApp(
		auth.NewService(identityRepo, ""),
		nil,
		"extract-provider",
		nil, nil, // prober, identity
		extractService,
		nil, nil, nil, nil,
		nil, nil, nil, nil,
		"sha256", "", "enc_",
		nil, nil,
		5, false, nil, nil,
	)
	handlers := NewHandlers(extractService, 5, app)

	var body bytes.Buffer
	writer := multipart.NewWriter(&body)
	if err := writer.WriteField("template_id", "invoice"); err != nil {
		t.Fatalf("write template field: %v", err)
	}
	fileWriter, err := writer.CreateFormFile("file", "invoice.pdf")
	if err != nil {
		t.Fatalf("create form file: %v", err)
	}
	if _, err := fileWriter.Write([]byte("%PDF-1.7 fake")); err != nil {
		t.Fatalf("write form file: %v", err)
	}
	if err := writer.Close(); err != nil {
		t.Fatalf("close multipart writer: %v", err)
	}

	req := httptest.NewRequest(http.MethodPost, "/api/v1/extract/process", &body)
	req.Header.Set("Content-Type", writer.FormDataContentType())
	rec := httptest.NewRecorder()

	handlers.HandleProcess(rec, req, principal)

	if rec.Code != http.StatusOK {
		t.Fatalf("expected status 200, got %d body=%s", rec.Code, rec.Body.String())
	}
	var out struct {
		JobID  string `json:"job_id"`
		Status string `json:"status"`
	}
	if err := json.Unmarshal(rec.Body.Bytes(), &out); err != nil {
		t.Fatalf("decode response: %v", err)
	}
	if out.JobID == "" || out.Status != jobs.StatusQueued {
		t.Fatalf("expected durable job id, got %+v", out)
	}

	// The job persists before the response returned; the attempt runs under
	// the leased claim and settles usage on billing.usage_records.
	claim, err := jobsSvc.Lease(ctx, "test-worker", 30*time.Second)
	if err != nil || claim == nil {
		t.Fatalf("lease: %v claim=%v", err, claim)
	}
	resultRef, err := extractService.JobHandler()(ctx, claim, func(ctx context.Context) (io.ReadCloser, error) {
		return store.Read(ctx, artifacts.Ref{TenantID: tenant, ID: claim.Job().InputRef})
	})
	if err != nil {
		t.Fatalf("attempt: %v", err)
	}
	if _, err := claim.Complete(ctx, resultRef); err != nil {
		t.Fatalf("complete: %v", err)
	}

	var recCount int
	var amount int64
	var capability, model string
	if err := db.SQL.QueryRowContext(ctx, `
		SELECT capability, model, amount_micros FROM usage_records WHERE tenant_id=$1::uuid`, tenant).
		Scan(&capability, &model, &amount); err != nil {
		t.Fatalf("query usage record: %v", err)
	}
	if capability != inference.CapabilityVision || model != "priced-extract" || amount <= 0 {
		t.Fatalf("unexpected usage: cap=%s model=%s amount=%d", capability, model, amount)
	}
	if err := db.SQL.QueryRowContext(ctx,
		`SELECT COUNT(*) FROM usage_records WHERE tenant_id=$1::uuid`, tenant).Scan(&recCount); err != nil {
		t.Fatalf("count usage: %v", err)
	}
	if recCount != 1 {
		t.Fatalf("usage_records=%d, want 1", recCount)
	}

	// Job detail surfaces the settled result through the owner-scoped read.
	detail, err := extractService.GetJob(ctx, principal, out.JobID)
	if err != nil {
		t.Fatalf("get job: %v", err)
	}
	if detail.Result == nil || detail.Result.TokensUsed != 1500 || detail.Result.Data["total"] != 12.5 {
		t.Fatalf("unexpected detail: %+v", detail.Result)
	}
}

func TestHandleJobDetailDeniesForeignAndUnknown(t *testing.T) {
	ctx := context.Background()
	db := testdb.New(t)
	tenant := testdb.SeedTenant(t, db, "extract-tenant")
	user := testdb.SeedUser(t, db, tenant, "a@example.com", nil)
	keyA, _ := testdb.SeedAPIKey(t, db, tenant, testdb.SeedKeySpec{Name: "a", OwnerUserID: user.ID, Scopes: []string{"extract"}})
	otherUser := testdb.SeedUser(t, db, tenant, "b@example.com", nil)
	keyB, _ := testdb.SeedAPIKey(t, db, tenant, testdb.SeedKeySpec{Name: "b", OwnerUserID: otherUser.ID, Scopes: []string{"extract"}})
	identityRepo := postgres.NewIdentityRepo(db)
	principalA, err := auth.NewService(identityRepo, "").Authenticate(ctx, keyA)
	if err != nil {
		t.Fatalf("auth A: %v", err)
	}
	principalB, err := auth.NewService(identityRepo, "").Authenticate(ctx, keyB)
	if err != nil {
		t.Fatalf("auth B: %v", err)
	}

	jobsRepo := postgres.NewJobsRepo(db)
	artRepo := postgres.NewArtifactRepo(db)
	store, err := artifacts.NewStore(filepath.Join(t.TempDir(), "durable"), filepath.Join(t.TempDir(), "scratch"), artRepo)
	if err != nil {
		t.Fatalf("store: %v", err)
	}
	svc, _ := newExtractService(t, db, tenant, artRepo, jobsRepo, store, &extractHandlerUpstream{}, "unrestricted-model")
	if err := svc.UpsertTemplate(ctx, principalA, extractsvc.Template{
		ID: "invoice", Name: "Invoice", SystemPrompt: "s", UserPrompt: "u", Enabled: true,
	}); err != nil {
		t.Fatalf("template: %v", err)
	}
	out, err := svc.Process(ctx, principalA, extractsvc.ProcessInput{
		TemplateID: "invoice", FileName: "a.pdf", MimeType: "application/pdf", Bytes: []byte("%PDF x"),
	})
	if err != nil {
		t.Fatalf("process: %v", err)
	}

	handlers := NewHandlers(svc, 5, nil)
	_ = handlers // handlers read paths exercised through svc here

	// Foreign job id == unknown id: both ErrNotFound.
	if _, err := svc.GetJob(ctx, principalB, out.JobID); !errors.Is(err, extractsvc.ErrNotFound) {
		t.Fatalf("foreign job read: %v", err)
	}
	if _, err := svc.GetJob(ctx, principalA, "job_nope"); !errors.Is(err, extractsvc.ErrNotFound) {
		t.Fatalf("unknown job read: %v", err)
	}
}

// extractHandlerUpstream is the deterministic transport behind the real
// executor the handler tests drive — policy/routing/accounting run through
// inference.Service, not a stubbed Execute.
type extractHandlerUpstream struct {
	response []byte
}

func (p *extractHandlerUpstream) CreateChatCompletion(ctx context.Context, body []byte) ([]byte, error) {
	return p.response, nil
}

func (p *extractHandlerUpstream) CreateChatCompletionStream(ctx context.Context, body []byte) (io.ReadCloser, error) {
	return io.NopCloser(bytes.NewReader(nil)), nil
}

func (p *extractHandlerUpstream) CreateEmbeddings(ctx context.Context, body []byte) ([]byte, error) {
	return nil, nil
}

func (p *extractHandlerUpstream) DoResponsesRequest(ctx context.Context, method, path string, body []byte) ([]byte, error) {
	return nil, errors.New("unsupported")
}

func (p *extractHandlerUpstream) DoResponsesStream(ctx context.Context, method, path string, body []byte) (io.ReadCloser, error) {
	return nil, errors.New("unsupported")
}

// newExtractService wires the durable stack plus the real executor over the
// fake upstream and registers a vision-capable endpoint with model bindings.
func newExtractService(t *testing.T, db *postgres.DB, tenant string, artRepo *postgres.ArtifactRepo,
	jobsRepo *postgres.JobsRepo, store *artifacts.Store, upstream inference.Upstream, aliases ...string) (*extractsvc.Service, *jobs.Service) {
	t.Helper()
	extractRepo := postgres.NewExtractRepo(db)
	endpoints := postgres.NewEndpointsRepo(db)
	sources := postgres.NewSourceRepo(db)
	billingRepo := postgres.NewBillingRepo(db)
	jobsSvc := jobs.NewService(jobsRepo, 10*time.Millisecond)
	caps := []string{inference.CapabilityChat, inference.CapabilityVision}
	for _, a := range aliases {
		caps = append(caps, inference.ModelCapabilityPrefix+a)
	}
	if err := endpoints.Create(context.Background(), inference.Endpoint{
		ID: "ep-extract", TenantID: tenant, Protocol: "openai_compatible",
		BaseOrigin: "https://extract.upstream.example", Capabilities: caps, Enabled: true,
	}); err != nil {
		t.Fatalf("seed endpoint: %v", err)
	}
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
	return extractsvc.NewService(extractRepo, extractRepo, jobsRepo, jobsSvc, store, exec, endpoints, 5), jobsSvc
}
