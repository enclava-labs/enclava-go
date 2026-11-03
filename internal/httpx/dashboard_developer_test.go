// dashboard_developer_test.go — the P-12/UX-M03 developer surface: the
// Account developer card (base URL, auth shape, own keys with settlement
// tokens), the cap-gated /dashboard/api reference and nav entry, the
// one-open-request flow and the admin-only attention row + dismiss. Every
// assertion runs against the real migrated schema with real sessions.
package httpx

import (
	"context"
	"net/http"
	"strings"
	"testing"

	"enclava-go/internal/auth"
	"enclava-go/internal/testdb"
)

// developerPage renders /dashboard/account and returns its <main> content —
// card assertions scope to the page body, never the admin chrome.
func developerPage(t *testing.T, f *identityFixture, sess *http.Cookie) string {
	t.Helper()
	rec := sessionDo(t, f.app, http.MethodGet, "/dashboard/account", "", sess, nil)
	if rec.Code != http.StatusOK {
		t.Fatalf("account page expected 200, got %d body=%s", rec.Code, rec.Body.String())
	}
	body := rec.Body.String()
	mainStart := strings.Index(body, `<main class="main">`)
	if mainStart < 0 {
		t.Fatalf("account page main region missing, got %s", body)
	}
	mainEnd := strings.Index(body[mainStart:], "</main>")
	if mainEnd < 0 {
		t.Fatalf("account page main region unclosed, got %s", body)
	}
	return body[mainStart : mainStart+mainEnd]
}

func TestDashboardDeveloperCardVisibility(t *testing.T) {
	f := newIdentityFixture(t)

	// Member holding an API scope: the full caller half of the card —
	// base URL, auth header shape, endpoint list, reference link — plus the
	// administrator-managed sentence and the request affordance.
	accountLogin(t, f, "dev-member@example.com", "dev-member-pass", []string{"chatbot"})
	memberSess, _ := login(t, f.app, "dev-member@example.com", "dev-member-pass")
	content := developerPage(t, f, memberSess)
	for _, want := range []string{
		`id="account-dev"`, "Authorization: Bearer enc_", "/dashboard/api",
		"/v1/chat/completions", "/v1/embeddings", "/api/v1/rag/search",
		"API keys and spend are managed by an administrator",
		`id="account-api-access-form"`,
	} {
		if !strings.Contains(content, want) {
			t.Errorf("member developer card missing %q\n%s", want, content)
		}
	}
	if strings.Contains(content, "/dashboard/access/api-keys") {
		t.Errorf("member must never see the key-management link\n%s", content)
	}

	// Grantless user: no API relationship → no card at all.
	accountLogin(t, f, "dev-grantless@example.com", "dev-grantless-pass", nil)
	grantlessSess, _ := login(t, f.app, "dev-grantless@example.com", "dev-grantless-pass")
	if content := developerPage(t, f, grantlessSess); strings.Contains(content, `id="account-dev"`) {
		t.Errorf("grantless user must not see the developer card\n%s", content)
	}

	// Administrator: key management is a link, never the member request
	// form; platform.admin alone grants no API scopes so the caller half
	// (Authorization) stays hidden — Access shows instead.
	accountLogin(t, f, "dev-admin@example.com", "dev-admin-pass", []string{auth.ScopePlatformAdmin})
	adminSess, _ := login(t, f.app, "dev-admin@example.com", "dev-admin-pass")
	content = developerPage(t, f, adminSess)
	if !strings.Contains(content, `id="account-dev"`) || !strings.Contains(content, "/dashboard/access/api-keys") {
		t.Errorf("admin developer card missing its manage link\n%s", content)
	}
	if strings.Contains(content, `id="account-api-access-form"`) {
		t.Errorf("administrator must not see the member request form\n%s", content)
	}
}

func TestDashboardAccountOwnKeys(t *testing.T) {
	f := newIdentityFixture(t)
	ctx := context.Background()
	user := accountLogin(t, f, "dev-keys@example.com", "dev-keys-pass", []string{"chatbot"})
	other := accountLogin(t, f, "dev-other@example.com", "dev-other-pass", []string{"chatbot"})
	_, rec := testdb.SeedAPIKey(t, f.db, f.tenant, testdb.SeedKeySpec{
		Name: "ci-bot", OwnerUserID: user.ID, Scopes: []string{"chat.completions"},
	})
	testdb.SeedAPIKey(t, f.db, f.tenant, testdb.SeedKeySpec{
		Name: "other-secret-key", OwnerUserID: other.ID, Scopes: []string{"chat.completions"},
	})
	// Settlement evidence: 100 input (of which 40 cached) + 25 output.
	if _, err := f.db.SQL.ExecContext(ctx,
		`INSERT INTO reservations (tenant_id, id, subject_type, subject_id, price_snapshot,
		 reserved_units, limit_amount_micros, currency, state, settled_at)
		 VALUES ($1::uuid, 'res_own1', 'api_key', $2, '{}', '{}', 0, 'USD', 'settled', now())`,
		f.tenant, rec.ID); err != nil {
		t.Fatalf("seed reservation: %v", err)
	}
	if _, err := f.db.SQL.ExecContext(ctx,
		`INSERT INTO usage_records (tenant_id, id, reservation_id, subject_type, subject_id,
		 capability, model, units, amount_micros, currency, price_snapshot, outcome)
		 VALUES ($1::uuid, 'ur_own1', 'res_own1', 'api_key', $2,
		 'chat', 'm1', '{"input":100,"cached_input":40,"output":25}'::jsonb, 0, 'USD', '{}', 'completed')`,
		f.tenant, rec.ID); err != nil {
		t.Fatalf("seed usage: %v", err)
	}

	sess, _ := login(t, f.app, "dev-keys@example.com", "dev-keys-pass")
	content := developerPage(t, f, sess)
	for _, want := range []string{"ci-bot", rec.KeyPrefix, "165", "100 in", "25 out"} {
		if !strings.Contains(content, want) {
			t.Errorf("own-keys view missing %q\n%s", want, content)
		}
	}
	if strings.Contains(content, "other-secret-key") {
		t.Errorf("own-keys view leaked another user's key\n%s", content)
	}
	if strings.Contains(content, "$") {
		t.Errorf("own-keys view must never carry cost figures\n%s", content)
	}
}

func TestDashboardAPIReferenceGating(t *testing.T) {
	f := newIdentityFixture(t)
	accountLogin(t, f, "dev-ref@example.com", "dev-ref-pass", []string{"rag"})
	memberSess, _ := login(t, f.app, "dev-ref@example.com", "dev-ref-pass")
	rec := sessionDo(t, f.app, http.MethodGet, "/dashboard/api", "", memberSess, nil)
	if rec.Code != http.StatusOK {
		t.Fatalf("api reference expected 200, got %d", rec.Code)
	}
	for _, want := range []string{"API reference", "/v1/chat/completions", "/v1/embeddings", "/api/v1/rag/search", "Authorization: Bearer"} {
		if !strings.Contains(rec.Body.String(), want) {
			t.Errorf("api reference missing %q", want)
		}
	}

	accountLogin(t, f, "dev-ref-none@example.com", "dev-ref-none-pass", nil)
	grantlessSess, _ := login(t, f.app, "dev-ref-none@example.com", "dev-ref-none-pass")
	if rec := sessionDo(t, f.app, http.MethodGet, "/dashboard/api", "", grantlessSess, nil); rec.Code != http.StatusForbidden {
		t.Fatalf("grantless api reference expected 403, got %d", rec.Code)
	}

	// Nav carries the entry only for API callers.
	rec = sessionDo(t, f.app, http.MethodGet, "/dashboard", "", memberSess, nil)
	if !strings.Contains(rec.Body.String(), `href="/dashboard/api"`) || !strings.Contains(rec.Body.String(), "API reference") {
		t.Errorf("member nav missing the API reference entry")
	}
	rec = sessionDo(t, f.app, http.MethodGet, "/dashboard", "", grantlessSess, nil)
	if strings.Contains(rec.Body.String(), "API reference") {
		t.Errorf("grantless nav must not offer the API reference")
	}
}

func TestDashboardAPIAccessRequestFlow(t *testing.T) {
	f := newIdentityFixture(t)
	ctx := context.Background()
	accountLogin(t, f, "dev-req@example.com", "dev-req-pass", []string{"chatbot"})
	memberSess, memberCSRF := login(t, f.app, "dev-req@example.com", "dev-req-pass")

	rec := sessionDo(t, f.app, http.MethodPost, "/dashboard/account/api-access-request", `{"note":"building ci"}`, memberSess, memberCSRF)
	if rec.Code != http.StatusOK {
		t.Fatalf("access request expected 200, got %d body=%s", rec.Code, rec.Body.String())
	}
	if body := decodeJSON(t, rec); body["created"] != true {
		t.Fatalf("first request must create, got %v", body)
	}
	// At most one open request per user — a repeat is idempotent, not a dupe.
	rec = sessionDo(t, f.app, http.MethodPost, "/dashboard/account/api-access-request", `{"note":"again"}`, memberSess, memberCSRF)
	if rec.Code != http.StatusOK {
		t.Fatalf("repeat access request expected 200, got %d", rec.Code)
	}
	if body := decodeJSON(t, rec); body["created"] != false {
		t.Fatalf("repeat request must not create a second open row, got %v", body)
	}
	open, err := f.repo.ListOpenAPIAccessRequests(ctx, f.tenant)
	if err != nil {
		t.Fatalf("list open requests: %v", err)
	}
	if len(open) != 1 || open[0].Note != "building ci" {
		t.Fatalf("expected exactly the first open request, got %#v", open)
	}
	// The requester sees the pending state, not a grant.
	if content := developerPage(t, f, memberSess); !strings.Contains(content, "waiting for an administrator") || strings.Contains(content, `id="account-api-access-form"`) {
		t.Errorf("pending request must render its state, not another form\n%s", content)
	}

	// Members cannot dismiss; the list is admin-only attention.
	memberDismiss := sessionDo(t, f.app, http.MethodPost, "/dashboard/access-requests/"+open[0].ID+"/dismiss", "", memberSess, memberCSRF)
	if memberDismiss.Code != http.StatusForbidden {
		t.Fatalf("member dismiss expected 403, got %d", memberDismiss.Code)
	}
	memberAttention := sessionDo(t, f.app, http.MethodGet, "/dashboard/partials/home/attention", "", memberSess, nil)
	if strings.Contains(memberAttention.Body.String(), "requested API access") {
		t.Errorf("member attention must not list access requests")
	}

	// Admin sees it on the attention feed with the fix link and dismiss.
	accountLogin(t, f, "dev-req-admin@example.com", "dev-req-admin-pass", []string{auth.ScopePlatformAdmin})
	adminSess, adminCSRF := login(t, f.app, "dev-req-admin@example.com", "dev-req-admin-pass")
	rec = sessionDo(t, f.app, http.MethodGet, "/dashboard/partials/home/attention", "", adminSess, nil)
	if rec.Code != http.StatusOK {
		t.Fatalf("admin attention expected 200, got %d", rec.Code)
	}
	body := rec.Body.String()
	for _, want := range []string{
		"dev-req@example.com requested API access",
		"/dashboard/access/api-keys?new=1",
		`data-dismiss-url="/dashboard/access-requests/` + open[0].ID + `/dismiss"`,
		"building ci",
	} {
		if !strings.Contains(body, want) {
			t.Errorf("admin attention missing %q\n%s", want, body)
		}
	}

	rec = sessionDo(t, f.app, http.MethodPost, "/dashboard/access-requests/"+open[0].ID+"/dismiss", "", adminSess, adminCSRF)
	if rec.Code != http.StatusNoContent {
		t.Fatalf("admin dismiss expected 204, got %d body=%s", rec.Code, rec.Body.String())
	}
	if open, err := f.repo.ListOpenAPIAccessRequests(ctx, f.tenant); err != nil || len(open) != 0 {
		t.Fatalf("dismissed request must leave the open list: %#v err=%v", open, err)
	}
	// Unknown/already-closed ids share one 404; nothing was granted.
	rec = sessionDo(t, f.app, http.MethodPost, "/dashboard/access-requests/"+open[0].ID+"/dismiss", "", adminSess, adminCSRF)
	if rec.Code != http.StatusNotFound {
		t.Fatalf("re-dismiss expected 404, got %d", rec.Code)
	}
	keys, err := f.repo.ListAPIKeys(ctx, f.tenant)
	if err != nil {
		t.Fatalf("list keys: %v", err)
	}
	for _, k := range keys {
		if k.OwnerUserID == open[0].UserID {
			t.Fatalf("dismissed request minted a key for the requester: %#v", k)
		}
	}
}
