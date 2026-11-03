package extractmodule

import (
	"bytes"
	"context"
	"encoding/json"
	"errors"
	"fmt"
	"io"
	"mime/multipart"
	"net/http"
	"net/http/httptest"
	"net/url"
	"path/filepath"
	"strings"
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

// TestWriteJobErrorPriceMissing pins the 11-01 envelope contract: the
// persisted-admission sentinel billing.ErrPriceMissing surfaces as 422 +
// code price_missing with the unit token intact — never a generic 500.
func TestWriteJobErrorPriceMissing(t *testing.T) {
	rec := httptest.NewRecorder()
	(&Handlers{}).writeJobError(rec, fmt.Errorf("%w: %s", billing.ErrPriceMissing, "cached_input"), "job_fetch_failed")
	if rec.Code != http.StatusUnprocessableEntity {
		t.Fatalf("status = %d, want 422", rec.Code)
	}
	var env struct {
		Error struct {
			Message string `json:"message"`
			Code    string `json:"code"`
		} `json:"error"`
	}
	if err := json.Unmarshal(rec.Body.Bytes(), &env); err != nil {
		t.Fatal(err)
	}
	if env.Error.Code != "price_missing" || !strings.Contains(env.Error.Message, "cached_input") {
		t.Fatalf("envelope = %+v, want code price_missing with the unit retained", env.Error)
	}

	rec = httptest.NewRecorder()
	(&Handlers{}).writeJobError(rec, extractsvc.ErrNotFound, "job_fetch_failed")
	if rec.Code != http.StatusNotFound {
		t.Fatalf("not-found mapping regressed: %d", rec.Code)
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
	// Structured text extraction uses chat capability after local parsing.
	billingRepo := postgres.NewBillingRepo(db)
	for _, u := range []billing.UnitKind{billing.UnitInput, billing.UnitOutput, billing.UnitCachedInput} {
		if err := billingRepo.PutPrice(ctx, tenant, "price_"+string(u), billing.Price{
			SubjectType: billing.PriceSubjectModel, SubjectID: "priced-extract",
			Capability: inference.CapabilityChat, Unit: u,
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
		nil, nil, nil,
		"sha256", "", "enc_",
		nil, nil,
		5, false, nil, nil, httpx.GateConfig{}, nil, nil,
	)
	handlers := NewHandlers(extractService, 5, app)

	var body bytes.Buffer
	writer := multipart.NewWriter(&body)
	if err := writer.WriteField("template_id", "invoice"); err != nil {
		t.Fatalf("write template field: %v", err)
	}
	fileWriter, err := writer.CreateFormFile("file", "invoice.txt")
	if err != nil {
		t.Fatalf("create form file: %v", err)
	}
	if _, err := fileWriter.Write([]byte("invoice text")); err != nil {
		t.Fatalf("write form file: %v", err)
	}
	if err := writer.Close(); err != nil {
		t.Fatalf("close multipart writer: %v", err)
	}

	req := httptest.NewRequest(http.MethodPost, "/api/v1/extract/jobs", &body)
	req.Header.Set("Content-Type", writer.FormDataContentType())
	rec := httptest.NewRecorder()

	handlers.HandleProcess(rec, req, principal)

	if rec.Code != http.StatusAccepted {
		t.Fatalf("expected status 202, got %d body=%s", rec.Code, rec.Body.String())
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
	if capability != inference.CapabilityChat || model != "priced-extract" || amount <= 0 {
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
	if detail.Result == nil || detail.Result.TokensUsed != 1500 || detail.Result.Data["total"] != json.Number("12.5") {
		t.Fatalf("unexpected detail: %+v", detail.Result)
	}
	originalReq := httptest.NewRequest(http.MethodGet, "/api/v1/extract/jobs/"+out.JobID+"/original", nil)
	originalRec := httptest.NewRecorder()
	handlers.HandleJobDetail(originalRec, originalReq, principal)
	if originalRec.Code != http.StatusOK || originalRec.Header().Get("Content-Type") != "text/plain" ||
		originalRec.Header().Get("X-Content-Type-Options") != "nosniff" || originalRec.Body.String() != "invoice text" {
		t.Fatalf("original response: status=%d headers=%v body=%q", originalRec.Code, originalRec.Header(), originalRec.Body.String())
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
	for _, suffix := range []string{"result", "original"} {
		req := httptest.NewRequest(http.MethodGet, "/api/v1/extract/jobs/"+out.JobID+"/"+suffix, nil)
		rec := httptest.NewRecorder()
		handlers.HandleJobDetail(rec, req, principalB)
		if rec.Code != http.StatusNotFound {
			t.Fatalf("foreign %s route status=%d body=%s", suffix, rec.Code, rec.Body.String())
		}
	}

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
	caps := make([]string, 0, 2+len(aliases))
	caps = append(caps, inference.CapabilityChat, inference.CapabilityVision)
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
	svc := extractsvc.NewService(extractRepo, extractRepo, jobsRepo, jobsSvc, store, exec, endpoints, 5)
	svc.SetSourceRepository(sources)
	return svc, jobsSvc
}

// TestHandleJobsListOperationFilter drives GET /api/v1/extract/jobs?operation=
// over the real repo predicates: the envelope keeps {jobs,total,limit,offset}
// with totals describing the filtered set, a document.preview job never
// surfaces, and non-member values are rejected with 400 invalid_operation.
func TestHandleJobsListOperationFilter(t *testing.T) {
	ctx := context.Background()
	db := testdb.New(t)
	tenant := testdb.SeedTenant(t, db, "extract-ops")
	user := testdb.SeedUser(t, db, tenant, "ops@example.com", nil)
	key, _ := testdb.SeedAPIKey(t, db, tenant, testdb.SeedKeySpec{
		Name: "ops-key", OwnerUserID: user.ID, Scopes: []string{"extract"},
	})
	principal, err := auth.NewService(postgres.NewIdentityRepo(db), "").Authenticate(ctx, key)
	if err != nil {
		t.Fatalf("authenticate: %v", err)
	}
	jobsRepo := postgres.NewJobsRepo(db)
	artRepo := postgres.NewArtifactRepo(db)
	store, err := artifacts.NewStore(filepath.Join(t.TempDir(), "durable"), filepath.Join(t.TempDir(), "scratch"), artRepo)
	if err != nil {
		t.Fatalf("store: %v", err)
	}
	svc, jobsSvc := newExtractService(t, db, tenant, artRepo, jobsRepo, store, &extractHandlerUpstream{}, "priced-extract")
	if err := svc.UpsertTemplate(ctx, principal, extractsvc.Template{
		ID: "invoice", Name: "Invoice", SystemPrompt: "s", UserPrompt: "u",
		DefaultModel: "priced-extract", Enabled: true,
	}); err != nil {
		t.Fatalf("template: %v", err)
	}
	out, err := svc.Process(ctx, principal, extractsvc.ProcessInput{
		TemplateID: "invoice", FileName: "invoice.txt", MimeType: "text/plain", Bytes: []byte("invoice text"),
	})
	if err != nil {
		t.Fatalf("process: %v", err)
	}
	processRow, err := jobsSvc.Get(ctx, principal, out.JobID)
	if err != nil {
		t.Fatal(err)
	}
	if _, _, err := jobsSvc.Enqueue(ctx, principal, jobs.EnqueueRequest{
		Operation:      "document.preview",
		IdempotencyKey: "handler-preview-fixture",
		InputRef:       processRow.InputRef, InputHash: processRow.InputHash,
		Policy: map[string]any{},
	}); err != nil {
		t.Fatalf("enqueue preview fixture: %v", err)
	}
	handlers := NewHandlers(svc, 5, nil)

	list := func(query string) (int, string) {
		req := httptest.NewRequest(http.MethodGet, "/api/v1/extract/jobs"+query, nil)
		rec := httptest.NewRecorder()
		handlers.HandleJobsList(rec, req, principal)
		return rec.Code, rec.Body.String()
	}

	status, body := list("?operation=extract.process")
	if status != http.StatusOK {
		t.Fatalf("process filter status=%d body=%s", status, body)
	}
	var out2 struct {
		Jobs  []map[string]any `json:"jobs"`
		Total int              `json:"total"`
		Limit int              `json:"limit"`
	}
	if err := json.Unmarshal([]byte(body), &out2); err != nil {
		t.Fatalf("decode: %v", err)
	}
	if out2.Total != 1 || len(out2.Jobs) != 1 {
		t.Fatalf("process filter: total=%d jobs=%d body=%s, want exactly the one process job", out2.Total, len(out2.Jobs), body)
	}

	status, body = list("?operation=extract.vision")
	if err := json.Unmarshal([]byte(body), &out2); err != nil {
		t.Fatalf("decode vision: %v", err)
	}
	if status != http.StatusOK || out2.Total != 0 || len(out2.Jobs) != 0 {
		t.Fatalf("vision filter: status=%d total=%d jobs=%d", status, out2.Total, len(out2.Jobs))
	}

	// The shared preview job never moves the unscoped (default) totals either.
	status, body = list("")
	if err := json.Unmarshal([]byte(body), &out2); err != nil {
		t.Fatalf("decode default: %v", err)
	}
	if status != http.StatusOK || out2.Total != 1 {
		t.Fatalf("default list: status=%d total=%d body=%s, want 1", status, out2.Total, body)
	}

	req := httptest.NewRequest(http.MethodGet, "/api/v1/extract/jobs?operation=bogus", nil)
	rec := httptest.NewRecorder()
	handlers.HandleJobsList(rec, req, principal)
	var errBody struct {
		Error struct {
			Code string `json:"code"`
		} `json:"error"`
	}
	if rec.Code != http.StatusBadRequest {
		t.Fatalf("bogus operation status=%d body=%s", rec.Code, rec.Body.String())
	}
	if err := json.Unmarshal(rec.Body.Bytes(), &errBody); err != nil || errBody.Error.Code != "invalid_operation" {
		t.Fatalf("bogus operation code=%q err=%v", errBody.Error.Code, err)
	}
}

// TestHandleTemplateByIDManageVsGrant pins the endpoint authority split: a
// zero-template-grant extract.manage caller lists authoring templates and
// reads/edits by id but holds an EMPTY executable picker (manage≠execute,
// D-25); a grant admits the GET only; no grant denies.
func TestHandleTemplateByIDManageVsGrant(t *testing.T) {
	ctx := context.Background()
	db := testdb.New(t)
	tenant := testdb.SeedTenant(t, db, "extract-split")
	empty := []string{}
	managerKey, _ := testdb.SeedAPIKey(t, db, tenant, testdb.SeedKeySpec{
		Name: "manager-key", OwnerUserID: testdb.SeedUser(t, db, tenant, "mgr@example.com", nil).ID,
		Scopes: []string{"extract", "extract.manage"}, AllowedTemplates: &empty,
	})
	grantedKey, _ := testdb.SeedAPIKey(t, db, tenant, testdb.SeedKeySpec{
		Name: "granted-key", OwnerUserID: testdb.SeedUser(t, db, tenant, "granted@example.com", nil).ID,
		Scopes: []string{"extract"}, AllowedTemplates: &[]string{"invoice"},
	})
	deniedKey, _ := testdb.SeedAPIKey(t, db, tenant, testdb.SeedKeySpec{
		Name: "denied-key", OwnerUserID: testdb.SeedUser(t, db, tenant, "denied@example.com", nil).ID,
		Scopes: []string{"extract"}, AllowedTemplates: &empty,
	})
	identityRepo := postgres.NewIdentityRepo(db)
	authSvc := auth.NewService(identityRepo, "")
	principalFor := func(key string) auth.Principal {
		p, err := authSvc.Authenticate(ctx, key)
		if err != nil {
			t.Fatalf("authenticate: %v", err)
		}
		return p
	}
	manager, granted, denied := principalFor(managerKey), principalFor(grantedKey), principalFor(deniedKey)

	jobsRepo := postgres.NewJobsRepo(db)
	artRepo := postgres.NewArtifactRepo(db)
	store, err := artifacts.NewStore(filepath.Join(t.TempDir(), "durable"), filepath.Join(t.TempDir(), "scratch"), artRepo)
	if err != nil {
		t.Fatalf("store: %v", err)
	}
	svc, _ := newExtractService(t, db, tenant, artRepo, jobsRepo, store, &extractHandlerUpstream{}, "priced-extract")
	// The manager authors the template through the manage-gated service —
	// no template grant involved.
	if err := svc.UpsertTemplate(ctx, manager, extractsvc.Template{
		ID: "invoice", Name: "Invoice", SystemPrompt: "s", UserPrompt: "u", Enabled: true,
	}); err != nil {
		t.Fatalf("manager upsert: %v", err)
	}
	handlers := NewHandlers(svc, 5, nil)

	// Zero-grant manager: authoring list full, picker empty.
	templates, err := svc.ListTemplates(ctx, manager)
	if err != nil || len(templates) != 1 {
		t.Fatalf("manager authoring list: %d %v, want 1", len(templates), err)
	}
	if picker, err := svc.ListExecutableTemplates(ctx, manager); err != nil || len(picker) != 0 {
		t.Fatalf("manager picker: %d %v, want empty", len(picker), err)
	}

	// GET: admitted for the manager (authoring) and the granted member.
	for name, p := range map[string]auth.Principal{"manager": manager, "granted": granted} {
		req := httptest.NewRequest(http.MethodGet, "/api/v1/extract/templates/invoice", nil)
		rec := httptest.NewRecorder()
		handlers.HandleTemplateByID(rec, req, p)
		if rec.Code != http.StatusOK {
			t.Fatalf("%s GET status=%d body=%s", name, rec.Code, rec.Body.String())
		}
	}
	// Ungranted member GET is denied.
	req := httptest.NewRequest(http.MethodGet, "/api/v1/extract/templates/invoice", nil)
	rec := httptest.NewRecorder()
	handlers.HandleTemplateByID(rec, req, denied)
	if rec.Code != http.StatusForbidden {
		t.Fatalf("ungranted GET status=%d body=%s", rec.Code, rec.Body.String())
	}

	// PUT: the manager succeeds with zero grants; the granted member is
	// denied — execution grants never mutate.
	putReq := httptest.NewRequest(http.MethodPut, "/api/v1/extract/templates/invoice",
		strings.NewReader(`{"name":"Renamed"}`))
	putRec := httptest.NewRecorder()
	handlers.HandleTemplateByID(putRec, putReq, manager)
	if putRec.Code != http.StatusOK {
		t.Fatalf("manager PUT status=%d body=%s", putRec.Code, putRec.Body.String())
	}
	putReq = httptest.NewRequest(http.MethodPut, "/api/v1/extract/templates/invoice",
		strings.NewReader(`{"name":"Hijacked"}`))
	putRec = httptest.NewRecorder()
	handlers.HandleTemplateByID(putRec, putReq, granted)
	if putRec.Code != http.StatusForbidden {
		t.Fatalf("granted PUT status=%d body=%s, want 403", putRec.Code, putRec.Body.String())
	}
	after, err := svc.GetTemplate(ctx, manager, "invoice")
	if err != nil || after.Name != "Renamed" {
		t.Fatalf("post-PUT template: %+v %v", after, err)
	}

	// Process still demands the grant: the manager cannot execute (regression
	// against implicit execution from manage).
	if _, err := svc.ProcessReader(ctx, manager, extractsvc.ProcessInput{
		TemplateID: "invoice", FileName: "invoice.txt", MimeType: "text/plain", Bytes: []byte("x"),
	}); !errors.Is(err, extractsvc.ErrForbidden) {
		t.Fatalf("manager process = %v, want ErrForbidden", err)
	}
}
