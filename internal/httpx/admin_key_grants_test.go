// admin_key_grants_test.go — key issuance and re-arming is bounded by the
// issuer's stored resource authority (D-25): the admin API create/patch/
// regenerate/restore paths and the retained legacy dashboard
// create/regenerate/toggle actions refuse any key whose resource grants
// exceed the caller's own, and a refusal never leaves a half-written row.

package httpx

import (
	"context"
	"net/http"
	"net/url"
	"strings"
	"testing"

	"enclava-go/internal/auth"
	"enclava-go/internal/testdb"
)

func TestAdminAPIKeyGrantsBounded(t *testing.T) {
	f := newIdentityFixture(t)
	ctx := context.Background()

	// A real stored role-grant session caller: platform.admin scope but
	// resource authority limited to models [m1] and assistants [bot1].
	limitedSess, limitedCSRF := loginUserWithGrants(t, f, "limited-admin@example.com",
		[]string{auth.ScopePlatformAdmin},
		auth.ResourceGrants{
			Models:     auth.Allowlist{Items: []string{"m1"}},
			Assistants: auth.Allowlist{Items: []string{"bot1"}},
		}, "limited-pass")

	// Positive anchor: an explicit in-authority list grant mints 201.
	// Omitted grant fields default to mode:all, so a limited caller must send
	// explicit in-authority lists for every kind (empty list = deny).
	rec := sessionDo(t, f.app, http.MethodPost, "/api/v1/admin/api-keys",
		`{"name":"in-authority","scopes":["platform.admin"],"allowed_models":["m1"],"allowed_chatbots":["bot1"],"allowed_agents":["bot1"],"allowed_endpoints":[],"allowed_extract_templates":[]}`, limitedSess, limitedCSRF)
	if rec.Code != http.StatusCreated {
		t.Fatalf("in-authority key create: %d %s", rec.Code, rec.Body.String())
	}
	if secret := decodeJSON(t, rec)["secret_key"].(string); secret == "" {
		t.Fatalf("one-time secret must be returned on create")
	}
	countKeys := func(label string, want int) {
		t.Helper()
		keys, err := f.repo.ListAPIKeys(ctx, f.tenant)
		if err != nil {
			t.Fatalf("list keys: %v", err)
		}
		if len(keys) != want {
			t.Fatalf("%s: key count = %d, want %d", label, len(keys), want)
		}
	}
	countKeys("positive anchor", 2)

	// A limited caller cannot mint out-of-authority, wildcard or omitted
	// (mode:all) resource grants — and nothing persists.
	for _, tc := range []struct{ name, body string }{
		{"unlisted model", `{"name":"m2-key","scopes":[],"allowed_models":["m2"]}`},
		{"wildcard model", `{"name":"star-key","scopes":[],"allowed_models":["*"]}`},
		{"omitted grants (all)", `{"name":"omitted-key","scopes":[]}`},
	} {
		rec := sessionDo(t, f.app, http.MethodPost, "/api/v1/admin/api-keys", tc.body, limitedSess, limitedCSRF)
		if rec.Code != http.StatusForbidden || !strings.Contains(rec.Body.String(), "caller's authority") {
			t.Fatalf("%s create: expected 403 authority refusal, got %d %s", tc.name, rec.Code, rec.Body.String())
		}
	}
	countKeys("denied creates", 2)

	// PUT patch: narrowing stays allowed, widening refuses. The stored key
	// is fully inside the caller's authority so only the patched field can
	// break the bound.
	owner := testdb.SeedUser(t, f.db, f.tenant, "key-owner@example.com", nil)
	deny := []string{}
	_, inKey := testdb.SeedAPIKey(t, f.db, f.tenant, testdb.SeedKeySpec{
		Name:             "patchable",
		OwnerUserID:      owner.ID,
		Scopes:           []string{auth.ScopePlatformAdmin},
		AllowedModels:    &[]string{"m1"},
		AllowedTemplates: &deny,
		AllowedChatbots:  &deny,
		AllowedAgents:    &deny,
		AllowedEndpoints: &deny,
	})
	put := func(body string) int {
		t.Helper()
		return sessionDo(t, f.app, http.MethodPut, "/api/v1/admin/api-keys/"+inKey.ID, body, limitedSess, limitedCSRF).Code
	}
	if code := put(`{"allowed_models":["m1"]}`); code != http.StatusOK {
		t.Fatalf("in-authority patch expected 200, got %d", code)
	}
	for _, body := range []string{`{"allowed_models":["m2"]}`, `{"allowed_models":["*"]}`} {
		if code := put(body); code != http.StatusForbidden {
			t.Fatalf("widening patch %s expected 403, got %d", body, code)
		}
	}

	// Regenerate/restore refuse to re-arm a stored key wider than the
	// caller — the stored secret bytes stay untouched.
	_, wideKey := testdb.SeedAPIKey(t, f.db, f.tenant, testdb.SeedKeySpec{
		Name:          "wide",
		OwnerUserID:   owner.ID,
		Scopes:        []string{auth.ScopePlatformAdmin},
		AllowedModels: &[]string{"m2"},
	})
	for _, action := range []string{"regenerate", "restore"} {
		rec := sessionDo(t, f.app, http.MethodPost, "/api/v1/admin/api-keys/"+wideKey.ID+"/"+action, "", limitedSess, limitedCSRF)
		if rec.Code != http.StatusForbidden || !strings.Contains(rec.Body.String(), "caller's authority") {
			t.Fatalf("%s of wider key: expected 403 authority refusal, got %d %s", action, rec.Code, rec.Body.String())
		}
	}
	fetched, err := f.repo.GetAPIKey(ctx, f.tenant, wideKey.ID)
	if err != nil {
		t.Fatalf("fetch wide key: %v", err)
	}
	if fetched.KeyHash != wideKey.KeyHash || fetched.KeyPrefix != wideKey.KeyPrefix {
		t.Fatalf("refused regenerate/restore must not touch the stored secret")
	}

	// --- Legacy dashboard form paths -------------------------------------

	// Dashboard create omits the grant fields entirely, so the built record
	// requests mode:all on every resource: the limited caller is refused and
	// no key persists.
	rec = sessionForm(t, f.app, http.MethodPost, "/dashboard/partials/keys",
		url.Values{"action": {"create"}, "name": {"dash-omitted"}, "scopes": {auth.ScopePlatformAdmin}}, limitedSess, limitedCSRF)
	if !strings.Contains(rec.Body.String(), "exceed your authority") {
		t.Fatalf("dashboard create expected grants refusal, got %d body=%s", rec.Code, rec.Body.String())
	}
	countKeys("dashboard denied create", 4)

	// Toggle DEACTIVATE on the same wider key stays allowed — disabling
	// never grants anything.
	rec = sessionForm(t, f.app, http.MethodPost, "/dashboard/partials/keys",
		url.Values{"action": {"toggle"}, "key_id": {wideKey.ID}}, limitedSess, limitedCSRF)
	if strings.Contains(rec.Body.String(), "exceed your authority") {
		t.Fatalf("deactivation must never be grants-bound: %s", rec.Body.String())
	}
	fetched, err = f.repo.GetAPIKey(ctx, f.tenant, wideKey.ID)
	if err != nil || fetched.IsActive {
		t.Fatalf("deactivate expected inactive key, got %v %v", fetched, err)
	}

	// Toggle ACTIVATE refuses and leaves the stored row byte-identical.
	before, err := f.repo.GetAPIKey(ctx, f.tenant, wideKey.ID)
	if err != nil {
		t.Fatalf("fetch before toggle: %v", err)
	}
	rec = sessionForm(t, f.app, http.MethodPost, "/dashboard/partials/keys",
		url.Values{"action": {"toggle"}, "key_id": {wideKey.ID}}, limitedSess, limitedCSRF)
	if !strings.Contains(rec.Body.String(), "exceed your authority") {
		t.Fatalf("activate wider key expected refusal, got %d body=%s", rec.Code, rec.Body.String())
	}
	after, err := f.repo.GetAPIKey(ctx, f.tenant, wideKey.ID)
	if err != nil {
		t.Fatalf("fetch after toggle: %v", err)
	}
	if after.IsActive || after.KeyPrefix != before.KeyPrefix || after.KeyHash != before.KeyHash {
		t.Fatalf("refused activate must leave the stored row untouched")
	}

	// Regenerate refuses BEFORE any secret work: no plaintext rendered, no
	// new hash stored, still inactive.
	before, err = f.repo.GetAPIKey(ctx, f.tenant, wideKey.ID)
	if err != nil {
		t.Fatalf("fetch before regenerate: %v", err)
	}
	rec = sessionForm(t, f.app, http.MethodPost, "/dashboard/partials/keys",
		url.Values{"action": {"regenerate"}, "key_id": {wideKey.ID}}, limitedSess, limitedCSRF)
	// The keys table legitimately lists public key prefixes, so the refusal
	// proof is the refusal message plus the byte-identical row below — never
	// the absence of prefix-shaped strings.
	if !strings.Contains(rec.Body.String(), "exceed") {
		t.Fatalf("refused regenerate must render the authority refusal, got %s", rec.Body.String())
	}
	after, err = f.repo.GetAPIKey(ctx, f.tenant, wideKey.ID)
	if err != nil {
		t.Fatalf("fetch after regenerate: %v", err)
	}
	if after.KeyPrefix != before.KeyPrefix || after.KeyHash != before.KeyHash || after.IsActive != before.IsActive {
		t.Fatalf("refused regenerate must be byte-identical: before=%+v after=%+v", before, after)
	}

	// POSITIVE legacy controls: an explicitly all-granted admin keeps the
	// full legacy behavior — regenerate persists a new hash and renders the
	// plaintext once, and toggle activation flips the key.
	all := auth.ResourceGrants{
		Models:           auth.Allowlist{All: true},
		Endpoints:        auth.Allowlist{All: true},
		Assistants:       auth.Allowlist{All: true},
		ExtractTemplates: auth.Allowlist{All: true},
	}
	bossSess, bossCSRF := loginUserWithGrants(t, f, "boss-admin@example.com",
		[]string{auth.ScopePlatformAdmin}, all, "boss-pass")

	rec = sessionForm(t, f.app, http.MethodPost, "/dashboard/partials/keys",
		url.Values{"action": {"regenerate"}, "key_id": {inKey.ID}}, bossSess, bossCSRF)
	if !strings.Contains(rec.Body.String(), "enc_") {
		t.Fatalf("all-granted regenerate expected plaintext render, got %s", rec.Body.String())
	}
	fetched, err = f.repo.GetAPIKey(ctx, f.tenant, inKey.ID)
	if err != nil {
		t.Fatalf("fetch regenerated key: %v", err)
	}
	if fetched.KeyHash == "" || fetched.KeyHash == inKey.KeyHash {
		t.Fatalf("regenerated key must persist a NEW hash")
	}

	rec = sessionForm(t, f.app, http.MethodPost, "/dashboard/partials/keys",
		url.Values{"action": {"toggle"}, "key_id": {inKey.ID}}, bossSess, bossCSRF)
	if strings.Contains(rec.Body.String(), "exceed your authority") {
		t.Fatalf("all-granted toggle must not be refused: %s", rec.Body.String())
	}
	fetched, err = f.repo.GetAPIKey(ctx, f.tenant, inKey.ID)
	if err != nil {
		t.Fatalf("fetch toggled key: %v", err)
	}
	if fetched.IsActive != !inKey.IsActive {
		t.Fatalf("toggle must flip IsActive: got %v", fetched.IsActive)
	}
}
