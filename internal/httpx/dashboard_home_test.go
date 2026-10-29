// dashboard_home_test.go — the /dashboard Home page (OA-31) against real
// services: per-role section composition (§6.2), the six manager attention
// kinds with their fix links, member-scoped own-work attention, the
// zero-filled sparkline asserted on seeded known daily values, per-currency
// spend, the polling running fragment, explicit /dashboard registration
// (no mux 301), the subtree designed-404 contract and the operation-filtered
// extraction total.
package httpx

import (
	"net/http"
	"net/http/httptest"
	"strings"
	"testing"
	"time"

	"enclava-go/internal/auth"
	"enclava-go/internal/billing"
	"enclava-go/internal/chatbot"
	"enclava-go/internal/connectors"
	"enclava-go/internal/extract"
	"enclava-go/internal/jobs"
	"enclava-go/internal/rag"
	"enclava-go/internal/store/postgres"
	"enclava-go/internal/testdb"
)

// homeFixture is an activityFixture (all attention/activity producers wired)
// plus the pieces those producer tests never needed: a session auth handler
// for real browser logins and the extract service for Recent extractions.
type homeFixture struct {
	*activityFixture
}

func newHomeFixture(t *testing.T) *homeFixture {
	t.Helper()
	f := newActivityFixture(t)
	authSvc := auth.NewService(f.identity, "")
	// The session slug must match the seeded tenant slug ("activity-home"),
	// or sign-in resolves no tenant.
	f.app.sessionAuth = NewSessionAuth(authSvc, auth.NewOIDCService(f.identity, nil), time.Hour, DashboardAuthTemplates(), "activity-home")
	extractRepo := postgres.NewExtractRepo(f.db)
	f.app.extractSvc = extract.NewService(extractRepo, extractRepo, postgres.NewJobsRepo(f.db), f.jobsSvc, nil, nil, f.endpoints, 10)
	// newActivityFixture built its App with a nil sessionAuth, so every
	// dashboard route bound a nil-receiver handler. A fresh mux re-registers
	// the same routes bound to the real session handlers.
	f.app.mux = http.NewServeMux()
	f.app.registerBaseRoutes()
	return &homeFixture{activityFixture: f}
}

// homeUser seeds a user carrying explicit grants and logs in as them; the
// returned principal mirrors the SESSION principal — credential/session
// fields included — so seeded rows are owned by the very submitter key the
// browser session resolves to (jobs submitter keys are credential-scoped).
func homeUser(t *testing.T, f *homeFixture, email string, scopes []string, grants auth.ResourceGrants, password string) (auth.Principal, *http.Cookie) {
	t.Helper()
	user := testdb.SeedUserWithGrants(t, f.db, f.tenant, email, scopes, grants)
	setPassword(t, f.identity, f.tenant, user.ID, password)
	sess, _ := login(t, f.app, email, password)
	scopesMap := make(map[string]struct{}, len(scopes))
	for _, s := range scopes {
		scopesMap[s] = struct{}{}
	}
	p := auth.Principal{
		TenantID: f.tenant, ActorID: user.ID, UserID: user.ID,
		ActorKind: auth.ActorKindUser, Scopes: scopesMap,
		BillingSubjectID: user.ID, BillingSubjectKind: auth.BillingSubjectUser,
	}
	var sessionID string
	if err := f.db.SQL.QueryRowContext(f.ctx,
		`SELECT id FROM sessions WHERE tenant_id=$1::uuid AND user_id=$2 AND revoked_at IS NULL
		 ORDER BY created_at DESC LIMIT 1`, f.tenant, user.ID).Scan(&sessionID); err != nil {
		t.Fatalf("seeded session for %s: %v", email, err)
	}
	p.CredentialID, p.CredentialKind, p.SessionID = sessionID, auth.CredentialKindSession, sessionID
	return p, sess
}

func homeGet(t *testing.T, f *homeFixture, path string, sess *http.Cookie) string {
	t.Helper()
	rec := sessionForm(t, f.app, http.MethodGet, path, nil, sess, nil)
	if rec.Code != http.StatusOK {
		t.Fatalf("GET %s: expected 200, got %d body=%s", path, rec.Code, rec.Body.String())
	}
	return rec.Body.String()
}

// enqueue enqueues one job for p and returns it still queued.
func (f *homeFixture) enqueue(t *testing.T, p auth.Principal, operation, key string) jobs.Job {
	t.Helper()
	job, _, err := f.jobsSvc.Enqueue(f.ctx, p, jobs.EnqueueRequest{
		TenantID: f.tenant, Operation: operation, IdempotencyKey: key,
		InputRef: "art-" + key, InputHash: "hash-" + key,
	})
	if err != nil {
		t.Fatalf("enqueue %s: %v", operation, err)
	}
	return job
}

// finishEveryJob drains the queue the way a worker would, settling each job
// succeeded — the terminal-only fragment state.
func (f *homeFixture) finishEveryJob(t *testing.T) {
	t.Helper()
	for {
		claim, err := f.jobsSvc.Lease(f.ctx, "home-worker", time.Minute)
		if err != nil {
			t.Fatalf("lease: %v", err)
		}
		if claim == nil {
			return
		}
		if _, err := claim.Complete(f.ctx, "{}"); err != nil {
			t.Fatalf("complete: %v", err)
		}
	}
}

func TestDashboardHomeMemberSeesOwnSectionsOnly(t *testing.T) {
	f := newHomeFixture(t)
	botGrants := auth.ResourceGrants{Assistants: auth.Allowlist{Items: []string{"bot-granted"}}}
	member, sess := homeUser(t, f, "home-member@example.com",
		[]string{"chatbot", "rag", "extract", "models.list"}, botGrants, "member-pass")

	// A granted assistant (tenant-wide audience ∩ explicit grant) and a
	// second one the member holds no grant item for.
	for _, id := range []string{"bot-granted", "bot-hidden"} {
		if err := f.chatRepo.Create(f.ctx, chatbot.Instance{
			TenantID: f.tenant, ID: id, ExternalID: id + "-ext", Name: "Bot " + id,
			Model: "m", IsEnabled: true, Audience: []string{"*"}, OwnerActorID: "someone-else",
		}); err != nil {
			t.Fatalf("create bot %s: %v", id, err)
		}
	}
	// Own conversation + own readable collection + own failed work.
	if err := f.chatRepo.CreateConversation(f.ctx, chatbot.Conversation{
		TenantID: f.tenant, ID: "conv-home", ChatbotID: "bot-granted", OwnerActorID: member.ActorID,
	}); err != nil {
		t.Fatalf("create conversation: %v", err)
	}
	if _, err := f.ragSvc.CreateCollection(f.ctx, member, rag.CreateCollectionInput{Name: "Team Notes"}); err != nil {
		t.Fatalf("create collection: %v", err)
	}
	f.enqueue(t, member, extract.OperationProcess, "home-extract-1")
	f.failExtraction(t, member, "home-extract-failed")
	f.seedFailedRun(t, member, "run-home", "conv-fail-run")

	body := homeGet(t, f, "/dashboard", sess)

	for _, want := range []string{
		"Needs attention", "Running now", "Confidential inference",
		"Recent conversations", "Start a chat", "Recent extractions", "Your knowledge",
		"Bot bot-granted", "/dashboard/chat/conv-home", "/dashboard/chat?assistant=bot-granted",
		"Team Notes", "Your extraction failed", "Your assistant run failed",
	} {
		if !strings.Contains(body, want) {
			t.Fatalf("member home missing %q\n%s", want, body)
		}
	}
	if strings.Contains(body, "No assistants you can use yet") {
		t.Fatal("granted member must see the granted assistant, not the empty state")
	}
	if strings.Contains(body, "bot-hidden") || strings.Contains(body, "Bot bot-hidden") {
		t.Fatal("assistant without a grant item leaked into Start a chat")
	}
	// Member kinds only: no manager rows, no KPI grid.
	for _, forbidden := range []string{
		"Requests · 30d", "Spend · 30d", "At a glance",
		"failed processing", "sync failed", "unverified", "budget", "schema changed",
	} {
		if strings.Contains(body, forbidden) {
			t.Fatalf("member home must not render manager content %q\n%s", forbidden, body)
		}
	}
}

func TestDashboardHomeGrantlessMemberSeesEmptyState(t *testing.T) {
	f := newHomeFixture(t)
	// Explicit empty mode:list deny grants — no admin or wildcard stand-ins.
	_, sess := homeUser(t, f, "home-grantless@example.com",
		[]string{"chatbot", "rag", "extract"}, auth.ResourceGrants{Assistants: auth.Allowlist{Items: []string{}}}, "pw")
	if err := f.chatRepo.Create(f.ctx, chatbot.Instance{
		TenantID: f.tenant, ID: "bot-any", ExternalID: "bot-any-ext", Name: "Any Bot",
		Model: "m", IsEnabled: true, Audience: []string{"*"}, OwnerActorID: "someone-else",
	}); err != nil {
		t.Fatalf("create bot: %v", err)
	}
	body := homeGet(t, f, "/dashboard", sess)
	if !strings.Contains(body, "No assistants you can use yet — ask an admin to grant access.") {
		t.Fatalf("grantless member must see the grant-needed empty state\n%s", body)
	}
	if strings.Contains(body, "Any Bot") {
		t.Fatal("tenant-wide audience must not bypass the empty stored grant")
	}
}

func TestDashboardHomeAdminKPIsSparkAndSpend(t *testing.T) {
	f := newHomeFixture(t)
	adminScopes := []string{auth.ScopePlatformAdmin, "pricing.manage"}
	admin, sess := homeUser(t, f, "home-admin@example.com", adminScopes, auth.ResourceGrants{}, "admin-pass")

	// Known daily values: 3 requests 2 days ago (USD), 1 today (USD),
	// 1 today (EUR) — spend renders per currency, never summed.
	subject := billingSubjectFor(admin)
	now := time.Now().UTC()
	seedSettledUsage(t, f.budgetFixture, "home-u1", subject, 1, 1_500_000, "USD", now.Add(-48*time.Hour))
	seedSettledUsage(t, f.budgetFixture, "home-u2", subject, 1, 2_500_000, "USD", now.Add(-48*time.Hour))
	seedSettledUsage(t, f.budgetFixture, "home-u3", subject, 1, 3_500_000, "USD", now)
	seedSettledUsage(t, f.budgetFixture, "home-u4", subject, 1, 4_500_000, "EUR", now)

	body := homeGet(t, f, "/dashboard", sess)
	for _, want := range []string{
		"At a glance", "Requests · 30d", "Spend · 30d", "Knowledge documents", "Assistant conversations",
		"$7.50", "€4.50", // two distinct per-currency rows — never one summed figure
	} {
		if !strings.Contains(body, want) {
			t.Fatalf("admin home missing %q\n%s", want, body)
		}
	}
	// The spark polyline must equal spark() over the zero-filled 30-slot
	// series recomputed from the seeded rows — SVG presence proves nothing.
	series, err := f.repo.ListDailyUsage(f.ctx, f.tenant, now.Add(-homeMetricsWindow))
	if err != nil {
		t.Fatalf("list daily usage: %v", err)
	}
	filled := homeSparkSeries(series)
	if len(filled) != homeSparkDays {
		t.Fatalf("zero-filled series length = %d, want %d", len(filled), homeSparkDays)
	}
	// Two seeded records 2 days ago and two today; every other slot is 0.
	if filled[len(filled)-3] != 2 || filled[len(filled)-1] != 2 {
		t.Fatalf("seeded known values missing from series: %v", filled)
	}
	wantSpark := string(spark(filled))
	if !strings.Contains(body, wantSpark) {
		t.Fatalf("rendered spark does not equal spark() of the zero-filled known series\nwant %s", wantSpark)
	}
}

func TestDashboardHomeAttentionKindsAndFixLinks(t *testing.T) {
	f := newHomeFixture(t)

	// Knowledge manager: rag/connectors/extract/chatbot management, no
	// platform.admin, explicit grants — sees its managed kinds only.
	kmg, kmgSess := homeUser(t, f, "home-km@example.com",
		[]string{"rag", "rag.manage", "connectors.manage", "chatbot", "chatbot.manage", "extract", "extract.manage", "models.list"},
		auth.ResourceGrants{Assistants: auth.Allowlist{All: true}}, "km-pass")
	owner, err := f.ragSvc.CreateCollection(f.ctx, kmg, rag.CreateCollectionInput{Name: "KM Docs"})
	if err != nil {
		t.Fatalf("create collection: %v", err)
	}
	f.uploadFailedDocument(t, kmg, "broken-doc.txt", owner.ID, nil)
	f.runNextEmbedFailure(t)
	f.seedSource(t, "src-home", "Home Source")
	f.failSync(t, kmg, "sync-home", "src-home", "authentication failed")

	kmBody := homeGet(t, f, "/dashboard", kmgSess)
	for _, want := range []string{"failed processing", "Open ", "/dashboard/knowledge/", "sync failed", "/dashboard/sources"} {
		if !strings.Contains(kmBody, want) {
			t.Fatalf("KM home missing %q\n%s", want, kmBody)
		}
	}
	for _, forbidden := range []string{"Requests · 30d", "At a glance", "/dashboard/usage", "/dashboard/tools"} {
		if strings.Contains(kmBody, forbidden) {
			t.Fatalf("KM without billing.view/tools must not render %q\n%s", forbidden, kmBody)
		}
	}

	// Admin seeded with every manager kind: each renders with its real
	// ActionHref fix link.
	adminScopes := []string{auth.ScopePlatformAdmin, "pricing.manage", "rag", "connectors.manage", "rag.manage"}
	admin, adminSess := homeUser(t, f, "home-admin2@example.com", adminScopes, auth.ResourceGrants{}, "admin-pass")
	f.uploadFailedDocument(t, admin, "admin-broken.txt", owner.ID, nil)
	f.runNextEmbedFailure(t)
	f.failSync(t, admin, "sync-home-2", "src-home", "authentication failed")
	f.seedConfidentialEndpoint(t) // unverified_endpoint (+ unpriced_units)
	seedSettledUsage(t, f.budgetFixture, "home-budget-use", billingSubjectFor(admin), 1, 90, "USD", time.Now().UTC())
	f.seedTenantBudget(t, 100) // budget: 90/100 µUSD = 90 % ≥ 80 %
	f.seedToolSchemas(t)       // tool_schema_changed
	adminBody := homeGet(t, f, "/dashboard", adminSess)
	for _, want := range []string{
		"failed processing", "/dashboard/knowledge/",
		"sync failed", "/dashboard/sources",
		"not verified", "/dashboard/models",
		"requests that need them are refused", "/dashboard/models/pricing",
		"Budget", "/dashboard/usage/budgets",
		"changed on their server", "/dashboard/tools",
	} {
		if !strings.Contains(adminBody, want) {
			t.Fatalf("admin attention missing %q\n%s", want, adminBody)
		}
	}

	// A member renders only its own failed extraction + failed run.
	member, memberSess := homeUser(t, f, "home-member2@example.com", []string{"chatbot", "extract"}, auth.ResourceGrants{}, "pw")
	f.failExtraction(t, member, "home-extract-failed-2")
	f.seedFailedRun(t, member, "run-home-2", "conv-home-2")
	memberBody := homeGet(t, f, "/dashboard", memberSess)
	if !strings.Contains(memberBody, "Your extraction failed") || !strings.Contains(memberBody, "Your assistant run failed") {
		t.Fatalf("member attention missing own failed work\n%s", memberBody)
	}
	for _, forbidden := range []string{"failed processing", "sync failed", "unverified", "schema changed"} {
		if strings.Contains(memberBody, forbidden) {
			t.Fatalf("member attention leaked manager kind %q\n%s", forbidden, memberBody)
		}
	}
}

func TestDashboardHomeRoutingAndRunningPartial(t *testing.T) {
	f := newHomeFixture(t)
	member, sess := homeUser(t, f, "home-routes@example.com", []string{"extract"}, auth.ResourceGrants{}, "pw")

	// Explicit /dashboard registration: 200 direct, no mux 301.
	rec := sessionForm(t, f.app, http.MethodGet, "/dashboard", nil, sess, nil)
	if rec.Code != http.StatusOK || rec.Header().Get("Location") != "" {
		t.Fatalf("GET /dashboard: got %d Location=%q, want direct 200", rec.Code, rec.Header().Get("Location"))
	}
	if rec := sessionForm(t, f.app, http.MethodGet, "/dashboard/", nil, sess, nil); rec.Code != http.StatusOK {
		t.Fatalf("GET /dashboard/: expected 200, got %d", rec.Code)
	}
	// Unknown subtree paths are the designed 404 page — never Home.
	rec = sessionForm(t, f.app, http.MethodGet, "/dashboard/nonexistent", nil, sess, nil)
	if rec.Code != http.StatusNotFound {
		t.Fatalf("GET /dashboard/nonexistent: expected 404, got %d", rec.Code)
	}
	if !strings.Contains(rec.Body.String(), "Page not found") {
		t.Fatalf("designed 404 page body missing marker: %s", rec.Body.String())
	}
	// Unauthenticated page GET redirects to login preserving return_to.
	req := httptest.NewRequest(http.MethodGet, "/dashboard", nil)
	rec = httptest.NewRecorder()
	f.app.Handler().ServeHTTP(rec, req)
	if rec.Code != http.StatusSeeOther || !strings.Contains(rec.Header().Get("Location"), "/dashboard/login?return_to=%2Fdashboard") {
		t.Fatalf("unauthenticated /dashboard: got %d Location=%q", rec.Code, rec.Header().Get("Location"))
	}

	// Running fragment: the 3s trigger only while non-terminal work exists.
	f.enqueue(t, member, extract.OperationProcess, "routes-queued")
	body := homeGet(t, f, "/dashboard/partials/home/running", sess)
	if !strings.Contains(body, "every 3s") {
		t.Fatalf("running fragment with a queued job must carry the 3s trigger\n%s", body)
	}
	if !strings.Contains(body, `hx-get="/dashboard/partials/home/running"`) {
		t.Fatal("running fragment must be self-replacing via hx-get")
	}
	f.finishEveryJob(t)
	body = homeGet(t, f, "/dashboard/partials/home/running", sess)
	if strings.Contains(body, "every 3s") {
		t.Fatalf("terminal-only fragment must stop polling\n%s", body)
	}
}

func TestDashboardHomeExtractionTotalExcludesOtherOperations(t *testing.T) {
	f := newHomeFixture(t)
	member, sess := homeUser(t, f, "home-totals@example.com", []string{"chatbot", "extract"}, auth.ResourceGrants{}, "pw")
	f.enqueue(t, member, extract.OperationProcess, "totals-extract")
	// Same owner, non-extraction operation: must never inflate the total.
	f.enqueue(t, member, chatbot.RunOperation, "totals-run")
	f.enqueue(t, member, connectors.OperationSync, "totals-sync")

	body := homeGet(t, f, "/dashboard", sess)
	if !strings.Contains(body, "1 total") {
		t.Fatalf("extraction total must count only own extraction jobs\n%s", body)
	}
	if strings.Contains(body, "Extract · ") || strings.Contains(body, "Sync") {
		t.Fatalf("non-extraction job labels leaked into Recent extractions\n%s", body)
	}
}

// billingSubjectFor maps a seeded principal onto its billing subject.
func billingSubjectFor(p auth.Principal) billing.Subject {
	return billing.Subject{Type: auth.BillingSubjectUser, ID: p.UserID}
}
