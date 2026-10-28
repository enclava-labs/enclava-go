// dashboard_extract_test.go — the Extract page (OA-25, plan 09-04) render
// tests: per-role tabs and guards, own-jobs-only listing, result-drawer
// actions gated on search cap + a writable collection, and the labeled
// (never-JSON-textarea) template editor markup.
//
// Cutover record (plan 09-04 executed the extract removal atomically): the
// old `/dashboard/extract` + `/dashboard/partials/extract*` JSON-proxy
// registrations are removed from registerLegacyDashboardRoutes, and
// handleDashboardPageExtract/handleDashboardExtract/dashboardExtractData are
// deleted from dashboard.go; the old dashboard_extract.go JSON handlers and
// extractPrincipal are gone (the five dashboard_resources.go callers now use
// the file-local legacyPrincipal); templates/page_extract.tmpl,
// templates/partial_extract.tmpl and static/extraction.js are deleted.
// Nothing extract-related remains for 09-10.
package httpx

import (
	"context"
	"encoding/json"
	"net/http"
	"net/http/httptest"
	"path/filepath"
	"strings"
	"testing"
	"time"

	"enclava-go/internal/artifacts"
	"enclava-go/internal/auth"
	"enclava-go/internal/extract"
	"enclava-go/internal/jobs"
	"enclava-go/internal/rag"
	"enclava-go/internal/store/postgres"
	"enclava-go/internal/testdb"
)

type extractFixture struct {
	app         *App
	db          *postgres.DB
	repo        *postgres.IdentityRepo
	extractRepo *postgres.ExtractRepo
	jobsRepo    *postgres.JobsRepo
	ragSvc      *rag.Service
	store       *artifacts.Store
	tenant      string
}

func newExtractFixture(t *testing.T) *extractFixture {
	t.Helper()
	db := testdb.New(t)
	repo := postgres.NewIdentityRepo(db)
	tenant := testdb.SeedTenant(t, db, "extract-dash")
	extractRepo := postgres.NewExtractRepo(db)
	jobsRepo := postgres.NewJobsRepo(db)
	endpoints := postgres.NewEndpointsRepo(db)
	artRepo := postgres.NewArtifactRepo(db)
	root := t.TempDir()
	store, err := artifacts.NewStore(filepath.Join(root, "durable"), filepath.Join(root, "scratch"), artRepo)
	if err != nil {
		t.Fatalf("artifact store: %v", err)
	}
	jobsSvc := jobs.NewService(jobsRepo, 0)
	svc := extract.NewService(extractRepo, extractRepo, jobsRepo, jobsSvc, store, nil, endpoints, 5)
	ragSvc := rag.NewService(postgres.NewRAGRepo(db), nil, nil, "", nil, jobsSvc)
	authSvc := auth.NewService(repo, "")
	sessionAuth := NewSessionAuth(authSvc, auth.NewOIDCService(repo, nil), time.Hour, DashboardAuthTemplates(), "extract-dash")
	app := NewApp(authSvc, nil, "", nil, repo, svc, ragSvc, nil, nil, nil, nil, nil, endpoints,
		"sha256", "", "enc_", nil, sessionAuth, 5, false, nil, nil, GateConfig{}, nil, artRepo)
	return &extractFixture{app: app, db: db, repo: repo, extractRepo: extractRepo, jobsRepo: jobsRepo, ragSvc: ragSvc, store: store, tenant: tenant}
}

// extractLogin seeds a scoped user (with real resource grants — never admin
// authority) and returns the session/CSRF cookies plus the user row.
func extractLogin(t *testing.T, f *extractFixture, email string, scopes []string, grants auth.ResourceGrants) (*http.Cookie, *http.Cookie, auth.User) {
	t.Helper()
	user := testdb.SeedUserWithGrants(t, f.db, f.tenant, email, scopes, grants)
	password := "extract-pass-" + email
	setPassword(t, f.repo, f.tenant, user.ID, password)
	sess, csrf := login(t, f.app, email, password)
	return sess, csrf, user
}

// extractSessionPrincipal rebuilds the exact principal AuthenticateSession
// produces for the user's newest session — the submitter key jobs are bound
// to must match what ListJobs/GetJob recompute from the live session.
func extractSessionPrincipal(t *testing.T, f *extractFixture, userID string) auth.Principal {
	t.Helper()
	var sessionID string
	err := f.db.SQL.QueryRowContext(context.Background(),
		`SELECT id FROM sessions WHERE tenant_id = $1::uuid AND user_id = $2 ORDER BY created_at DESC LIMIT 1`,
		f.tenant, userID).Scan(&sessionID)
	if err != nil {
		t.Fatalf("find session for %s: %v", userID, err)
	}
	return auth.Principal{
		TenantID: f.tenant, ActorID: userID, ActorKind: auth.ActorKindUser, UserID: userID,
		CredentialID: sessionID, CredentialKind: auth.CredentialKindSession, SessionID: sessionID,
		BillingSubjectID: userID, BillingSubjectKind: auth.BillingSubjectUser,
	}
}

func seedExtractTemplates(t *testing.T, f *extractFixture) {
	t.Helper()
	for _, tpl := range extract.DefaultTemplates() {
		if err := f.extractRepo.UpsertTemplate(context.Background(), f.tenant, tpl); err != nil {
			t.Fatalf("seed template %s: %v", tpl.ID, err)
		}
	}
}

// seedExtractJob inserts an owned extraction job and forces its status and
// result ref (InsertJob always lands 'queued'; workers publish the rest).
func seedExtractJob(t *testing.T, f *extractFixture, p auth.Principal, id, status, resultRef string, policy map[string]any) {
	t.Helper()
	if policy == nil {
		policy = map[string]any{}
	}
	principalJSON, err := json.Marshal(p)
	if err != nil {
		t.Fatalf("marshal principal: %v", err)
	}
	policyJSON, err := json.Marshal(policy)
	if err != nil {
		t.Fatalf("marshal policy: %v", err)
	}
	_, _, err = f.jobsRepo.InsertJob(context.Background(), jobs.InsertRecord{
		Job: jobs.Job{
			TenantID: p.TenantID, ID: id, Operation: extract.OperationProcess,
			Status: jobs.StatusQueued, IdempotencyKey: id + "-key", Policy: policy,
			PrincipalSnapshot: principalJSON,
		},
		SubmitterKey: jobs.SubmitterKey(p), Principal: principalJSON, PolicyJSON: policyJSON,
	})
	if err != nil {
		t.Fatalf("seed job %s: %v", id, err)
	}
	if status != jobs.StatusQueued || resultRef != "" {
		if _, err := f.db.SQL.ExecContext(context.Background(),
			`UPDATE jobs SET status = $2, result_ref = NULLIF($3, ''), finished_at = CASE WHEN $2 IN ('succeeded','failed','cancelled') THEN now() ELSE finished_at END WHERE id = $1`,
			id, status, resultRef); err != nil {
			t.Fatalf("force job %s status: %v", id, err)
		}
	}
}

// seedExtractResult writes a completed result artifact for the job and
// returns the data it carries.
func seedExtractResult(t *testing.T, f *extractFixture, p auth.Principal, jobID string, data map[string]any) {
	t.Helper()
	doc, err := json.Marshal(extract.ResultView{ID: "res_" + jobID, JobID: jobID, Data: data})
	if err != nil {
		t.Fatalf("marshal result: %v", err)
	}
	w, ref, err := f.store.Begin(context.Background(), artifacts.Meta{
		Ref: artifacts.Ref{TenantID: p.TenantID, Kind: artifacts.KindResult}, MimeType: "application/json",
	}, time.Minute, int64(len(doc))+4096)
	if err != nil {
		t.Fatalf("begin result artifact: %v", err)
	}
	if _, err := w.Write(doc); err != nil {
		t.Fatalf("write result artifact: %v", err)
	}
	if err := w.Finalize(context.Background()); err != nil {
		t.Fatalf("finalize result artifact: %v", err)
	}
	seedExtractJob(t, f, p, jobID, jobs.StatusSucceeded, ref.ID, map[string]any{
		"template_id": "detailed_invoice", "file_name": jobID + "-invoice.pdf",
		"mime_type": "application/pdf", "file_size": 128,
	})
}

func extractGet(t *testing.T, f *extractFixture, path string, sess, csrf *http.Cookie) *httptest.ResponseRecorder {
	t.Helper()
	return sessionForm(t, f.app, http.MethodGet, path, nil, sess, csrf)
}

// TestDashboardExtractTabs: a member sees Process + Jobs only, with the
// picker showing exactly the granted templates; extract.manage adds the
// Templates tab.
func TestDashboardExtractTabs(t *testing.T) {
	f := newExtractFixture(t)
	seedExtractTemplates(t, f)

	memberSess, memberCSRF, _ := extractLogin(t, f, "extract-member@example.com", []string{"extract"},
		auth.ResourceGrants{ExtractTemplates: auth.Allowlist{Items: []string{"detailed_invoice"}}})
	managerSess, managerCSRF, _ := extractLogin(t, f, "extract-manager@example.com", []string{"extract", "extract.manage"},
		auth.ResourceGrants{ExtractTemplates: auth.Allowlist{All: true}})

	rec := extractGet(t, f, "/dashboard/extract", memberSess, memberCSRF)
	if rec.Code != http.StatusOK {
		t.Fatalf("member extract page: expected 200, got %d body=%s", rec.Code, rec.Body.String())
	}
	body := rec.Body.String()
	for _, want := range []string{
		`href="/dashboard/extract?tab=process"`,
		`href="/dashboard/extract?tab=jobs"`,
		`type="file"`, `class="visually-hidden"`, `data-extract-dropzone`,
		`data-action="extract:submit"`,
		`<option value="detailed_invoice">`,
	} {
		if !strings.Contains(body, want) {
			t.Errorf("member process tab missing %q", want)
		}
	}
	if strings.Contains(body, "?tab=templates") {
		t.Errorf("member sees the Templates tab: %s", body)
	}
	if strings.Contains(body, "expense_report") {
		t.Errorf("member picker leaked an ungranted template")
	}

	rec = extractGet(t, f, "/dashboard/extract?tab=jobs", memberSess, memberCSRF)
	if rec.Code != http.StatusOK {
		t.Fatalf("member jobs tab: expected 200, got %d", rec.Code)
	}
	if !strings.Contains(rec.Body.String(), `id="extract-jobs"`) {
		t.Errorf("jobs tab missing the jobs fragment root")
	}

	rec = extractGet(t, f, "/dashboard/extract", managerSess, managerCSRF)
	if rec.Code != http.StatusOK {
		t.Fatalf("manager extract page: expected 200, got %d", rec.Code)
	}
	if !strings.Contains(rec.Body.String(), `href="/dashboard/extract?tab=templates"`) {
		t.Errorf("manager missing the Templates tab link")
	}
	// A manager's execution set still comes from stored grants only — All is
	// set here, so all seeded defaults must be selectable.
	if !strings.Contains(rec.Body.String(), "expense_report") {
		t.Errorf("manager picker missing a granted template")
	}

	rec = extractGet(t, f, "/dashboard/extract?tab=templates", managerSess, managerCSRF)
	if rec.Code != http.StatusOK {
		t.Fatalf("manager templates tab: expected 200, got %d body=%s", rec.Code, rec.Body.String())
	}
	if !strings.Contains(rec.Body.String(), `id="extract-templates"`) {
		t.Errorf("templates tab missing the management fragment root")
	}
}

// TestDashboardExtractMemberDenied: extract.manage gates the templates
// surface, the extract cap gates everything; denied partials stay empty 403s.
func TestDashboardExtractMemberDenied(t *testing.T) {
	f := newExtractFixture(t)
	memberSess, memberCSRF, _ := extractLogin(t, f, "plain-member@example.com", []string{"extract"}, auth.ResourceGrants{})
	outsiderSess, outsiderCSRF, _ := extractLogin(t, f, "chat-only@example.com", []string{"chatbot"}, auth.ResourceGrants{})

	if rec := extractGet(t, f, "/dashboard/partials/extract/templates", memberSess, memberCSRF); rec.Code != http.StatusForbidden {
		t.Fatalf("member templates partial: expected 403, got %d body=%s", rec.Code, rec.Body.String())
	} else if strings.TrimSpace(rec.Body.String()) != "" {
		t.Fatalf("denied partial must render an empty 403, got body %q", rec.Body.String())
	}
	if rec := extractGet(t, f, "/dashboard/extract?tab=templates", memberSess, memberCSRF); rec.Code != http.StatusForbidden {
		t.Fatalf("member templates page: expected 403, got %d", rec.Code)
	}
	for _, path := range []string{
		"/dashboard/extract",
		"/dashboard/extract?tab=jobs",
		"/dashboard/partials/extract/jobs",
		"/dashboard/partials/extract/result?job=somejob",
		"/dashboard/partials/extract/templates",
	} {
		if rec := extractGet(t, f, path, outsiderSess, outsiderCSRF); rec.Code != http.StatusForbidden {
			t.Fatalf("chat-only session %s: expected 403, got %d", path, rec.Code)
		}
	}
}

// TestDashboardExtractJobsOwnOnly: the jobs fragment lists the caller's own
// extraction jobs only — another user's job never leaks, and the live poll
// attribute appears only while a queued/running job exists.
func TestDashboardExtractJobsOwnOnly(t *testing.T) {
	f := newExtractFixture(t)

	aliceSess, aliceCSRF, alice := extractLogin(t, f, "alice@example.com", []string{"extract"}, auth.ResourceGrants{})
	bobSess, bobCSRF, bob := extractLogin(t, f, "bob@example.com", []string{"extract"}, auth.ResourceGrants{})

	aliceP := extractSessionPrincipal(t, f, alice.ID)
	bobP := extractSessionPrincipal(t, f, bob.ID)

	seedExtractJob(t, f, aliceP, "job-alice-queued", jobs.StatusQueued, "", map[string]any{
		"template_id": "detailed_invoice", "file_name": "alice-invoice.pdf", "mime_type": "application/pdf", "file_size": 10,
	})
	seedExtractJob(t, f, aliceP, "job-alice-done", jobs.StatusSucceeded, "", map[string]any{
		"template_id": "detailed_invoice", "file_name": "alice-receipt.pdf", "mime_type": "application/pdf", "file_size": 11,
	})
	seedExtractJob(t, f, bobP, "job-bob-done", jobs.StatusSucceeded, "", map[string]any{
		"template_id": "detailed_invoice", "file_name": "bob-secret.pdf", "mime_type": "application/pdf", "file_size": 12,
	})

	rec := extractGet(t, f, "/dashboard/partials/extract/jobs", aliceSess, aliceCSRF)
	if rec.Code != http.StatusOK {
		t.Fatalf("alice jobs fragment: expected 200, got %d body=%s", rec.Code, rec.Body.String())
	}
	body := rec.Body.String()
	if !strings.Contains(body, "alice-invoice.pdf") || !strings.Contains(body, "alice-receipt.pdf") {
		t.Errorf("alice's own jobs missing from the fragment")
	}
	if strings.Contains(body, "bob-secret.pdf") || strings.Contains(body, "job-bob-done") {
		t.Errorf("bob's job leaked into alice's list")
	}
	if !strings.Contains(body, `data-action="extract:cancel" data-job-id="job-alice-queued"`) {
		t.Errorf("queued job missing its Cancel action")
	}
	if !strings.Contains(body, `every 3s`) {
		t.Errorf("live poll missing while a queued job exists")
	}
	if !strings.Contains(body, `data-confirm="Delete this extraction job?"`) {
		t.Errorf("delete confirmation missing")
	}

	rec = extractGet(t, f, "/dashboard/partials/extract/jobs", bobSess, bobCSRF)
	if rec.Code != http.StatusOK {
		t.Fatalf("bob jobs fragment: expected 200, got %d", rec.Code)
	}
	if strings.Contains(rec.Body.String(), "alice-invoice.pdf") {
		t.Errorf("alice's job leaked into bob's list")
	}
}

// TestDashboardExtractResultActions: the drawer renders fields and validation
// truthfully (locations only where Data carries them), Copy/CSV act on the
// stored result, and Add-to-knowledge appears only with the search cap AND a
// writable collection.
func TestDashboardExtractResultActions(t *testing.T) {
	f := newExtractFixture(t)
	ctx := context.Background()

	ownerSess, ownerCSRF, owner := extractLogin(t, f, "carol@example.com", []string{"extract", "rag"}, auth.ResourceGrants{})
	eveSess, eveCSRF, eve := extractLogin(t, f, "eve@example.com", []string{"extract"}, auth.ResourceGrants{})
	frankSess, frankCSRF, frank := extractLogin(t, f, "frank@example.com", []string{"extract", "rag"}, auth.ResourceGrants{})

	ownerP := extractSessionPrincipal(t, f, owner.ID)
	eveP := extractSessionPrincipal(t, f, eve.ID)
	frankP := extractSessionPrincipal(t, f, frank.ID)

	seedExtractResult(t, f, ownerP, "job-owner", map[string]any{"invoice_number": "INV-1", "total_amount": "9007199254740993.0001"})
	seedExtractResult(t, f, eveP, "job-eve", map[string]any{"invoice_number": "INV-2"})
	seedExtractResult(t, f, frankP, "job-frank", map[string]any{"invoice_number": "INV-3"})

	// Carol owns a writable collection and can merely READ a foreign one.
	if _, err := f.ragSvc.CreateCollection(ctx, ownerP, rag.CreateCollectionInput{Name: "Contracts"}); err != nil {
		t.Fatalf("create owned collection: %v", err)
	}
	foreign := testdb.SeedUser(t, f.db, f.tenant, "dave@example.com", nil)
	foreignP := auth.Principal{TenantID: f.tenant, ActorID: foreign.ID, ActorKind: auth.ActorKindUser, UserID: foreign.ID}
	if _, err := f.ragSvc.CreateCollection(ctx, foreignP, rag.CreateCollectionInput{Name: "Dave private"}); err != nil {
		t.Fatalf("create foreign collection: %v", err)
	}

	rec := extractGet(t, f, "/dashboard/partials/extract/result?job=job-owner", ownerSess, ownerCSRF)
	if rec.Code != http.StatusOK {
		t.Fatalf("owner result drawer: expected 200, got %d body=%s", rec.Code, rec.Body.String())
	}
	body := rec.Body.String()
	for _, want := range []string{
		"invoice_number", "INV-1", "9007199254740993.0001",
		`data-action="extract:copy" data-job-id="job-owner"`,
		`data-action="extract:csv" data-job-id="job-owner"`,
		"Add to knowledge", "Contracts",
		`data-action="extract:import" data-job-id="job-owner"`,
	} {
		if !strings.Contains(body, want) {
			t.Errorf("result drawer missing %q", want)
		}
	}
	// No location producer exists — the cell stays truthful "—", never invented.
	if !strings.Contains(body, "<td data-label=\"Location\">—</td>") {
		t.Errorf("location cell must render the empty marker, got: %s", body)
	}
	// The foreign collection is readable but not writable — never offered.
	if strings.Contains(body, "Dave private") {
		t.Errorf("non-writable collection offered for import")
	}

	// No search cap → no import control.
	rec = extractGet(t, f, "/dashboard/partials/extract/result?job=job-eve", eveSess, eveCSRF)
	if rec.Code != http.StatusOK {
		t.Fatalf("eve result drawer: expected 200, got %d", rec.Code)
	}
	if strings.Contains(rec.Body.String(), "Add to knowledge") {
		t.Errorf("import control rendered without the search cap")
	}

	// Search cap but no writable collection → still hidden.
	rec = extractGet(t, f, "/dashboard/partials/extract/result?job=job-frank", frankSess, frankCSRF)
	if rec.Code != http.StatusOK {
		t.Fatalf("frank result drawer: expected 200, got %d body=%s", rec.Code, rec.Body.String())
	}
	if strings.Contains(rec.Body.String(), "Add to knowledge") {
		t.Errorf("import control rendered without a writable collection")
	}
}

// TestDashboardExtractTemplates: the management tab renders the list, the
// default-model setting, reset-defaults, and an editor whose schema editing
// is labeled rows only — no raw-JSON textarea exists anywhere.
func TestDashboardExtractTemplates(t *testing.T) {
	f := newExtractFixture(t)
	seedExtractTemplates(t, f)

	managerSess, managerCSRF, _ := extractLogin(t, f, "tpl-manager@example.com", []string{"extract", "extract.manage"},
		auth.ResourceGrants{ExtractTemplates: auth.Allowlist{All: true}})

	rec := extractGet(t, f, "/dashboard/partials/extract/templates", managerSess, managerCSRF)
	if rec.Code != http.StatusOK {
		t.Fatalf("manager templates fragment: expected 200, got %d body=%s", rec.Code, rec.Body.String())
	}
	body := rec.Body.String()
	for _, want := range []string{
		`id="extract-templates"`,
		`data-action="extract:edit-template"`,
		`data-template-id="detailed_invoice"`,
		`data-confirm="Delete template Detailed Invoice?"`,
		`data-extract-reset-defaults`,
		`id="extract-default-model"`,
		`data-action="extract:save-settings"`,
		`id="extract-template-editor"`,
		`data-editor-schema="output"`,
		`data-editor-schema="context"`,
		`data-editor-add-row="output"`,
		`data-editor-note`,
		`>Field name<`,
		`>Required<`,
	} {
		if !strings.Contains(body, want) {
			t.Errorf("templates management missing %q", want)
		}
	}
	// The labeled-row rule (OA-34): no textarea carries schema JSON. Prompt
	// textareas are fields, not JSON input.
	for _, banned := range []string{`data-editor-field="output_schema"`, `data-editor-field="context_schema"`} {
		if strings.Contains(body, banned) {
			t.Errorf("raw JSON schema textarea present via %s", banned)
		}
	}
	if n := strings.Count(body, "<textarea"); n != 2 {
		t.Errorf("expected exactly the two prompt textareas, got %d", n)
	}
}
