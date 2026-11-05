// dashboard_sources_test.go — Sources page + add-source wizard coverage
// (OA-23, plan 09-02): per-role route gating, real table projections, the
// five-provider drawer markup and the single collection read rule rendered
// in the Permissions column and the wizard's Access step.
//
// Obsolete-file handoff for plan 09-10 — this page replaces the old
// connectors dashboard. Delete then, in the same change:
//   - dashboard_resources.go: handleDashboardPageConnectors,
//     handleDashboardConnectors, handleDashboardConnectorAction and
//     dashboardConnectorsData
//   - dashboard.go registrations: /dashboard/connectors,
//     /dashboard/partials/connectors
//   - dashboard/templates/partial_connectors.tmpl
//   - dashboard/static/connectors.js
package httpx

import (
	"bytes"
	"context"
	"encoding/json"
	"net/http"
	"net/http/httptest"
	"net/url"
	"strings"
	"testing"
	"time"

	"enclava-go/internal/auth"
	"enclava-go/internal/connectors"
	"enclava-go/internal/credentials"
	"enclava-go/internal/jobs"
	"enclava-go/internal/rag"
	"enclava-go/internal/store/postgres"
)

// createSourceFor was folded into the tests below — each seeds sources
// through the real API session.

func TestDashboardSourcesMemberDenied(t *testing.T) {
	f := newIdentityFixture(t)
	sess, csrf := loginUser(t, f, "member@example.com", []string{"chatbot", "rag"}, "member-pass")

	// Page GET: the designed 403 page, never a redirect or a leak.
	rec := sessionDo(t, f.app, http.MethodGet, "/dashboard/sources", "", sess, csrf)
	if rec.Code != http.StatusForbidden {
		t.Fatalf("GET /dashboard/sources: expected 403 for member, got %d", rec.Code)
	}
	if body := rec.Body.String(); !strings.Contains(body, "You don't have access") {
		t.Fatalf("GET /dashboard/sources: expected the designed 403 page, got body %q", body)
	}

	// Partials (path-based): empty-body 403 — no fragment markup for members.
	for _, path := range []string{
		"/dashboard/partials/sources/table",
		"/dashboard/partials/sources/drawer",
	} {
		rec := sessionDo(t, f.app, http.MethodGet, path, "", sess, csrf)
		if rec.Code != http.StatusForbidden {
			t.Fatalf("GET %s: expected 403 for member, got %d", path, rec.Code)
		}
		if body := rec.Body.String(); strings.TrimSpace(body) != "" {
			t.Fatalf("GET %s: expected an empty 403 body for a partial, got %q", path, body)
		}
	}
}

func TestDashboardSourcesTableRows(t *testing.T) {
	f := newIdentityFixture(t)
	ctx := context.Background()
	sess, csrf := loginUser(t, f, "manager@example.com", []string{"connectors.manage", "rag", "rag.manage"}, "manager-pass")

	// A real target collection so the table can resolve the name.
	rec := sessionDo(t, f.app, http.MethodPost, "/api/v1/rag/collections",
		`{"name":"Docs library"}`, sess, csrf)
	if rec.Code != http.StatusCreated {
		t.Fatalf("create collection: expected 201, got %d body=%s", rec.Code, rec.Body.String())
	}
	collection := decodeJSON(t, rec)
	collectionID, _ := collection["id"].(string)
	if collectionID == "" {
		t.Fatalf("create collection: missing id in %v", collection)
	}

	// GitHub needs no credential; Slack's credential_ref must carry the
	// provider prefix.
	made := map[string]string{}
	for name, body := range map[string]string{
		"repo":  `{"type":"github","name":"Docs repo","config":{"owner":"enclava","repo":"docs"},"target_collection_id":"` + collectionID + `"}`,
		"chat":  `{"type":"slack","name":"Help desk","config":{"channel_id":"C123","reconcile":"true"},"credential_ref":"CONNECTOR_SLACK_HELPDESK"}`,
		"muted": `{"type":"github","name":"Archive sync","config":{"owner":"enclava","repo":"archive"}}`,
	} {
		rec := sessionDo(t, f.app, http.MethodPost, "/api/v1/connectors/sources", body, sess, csrf)
		if rec.Code != http.StatusCreated {
			t.Fatalf("create %s source: expected 201, got %d body=%s", name, rec.Code, rec.Body.String())
		}
		created := decodeJSON(t, rec)
		id, _ := created["id"].(string)
		if id == "" {
			t.Fatalf("create %s source: missing id in %v", name, created)
		}
		made[name] = id
	}

	// The third source is paused through the real update API.
	rec = sessionDo(t, f.app, http.MethodPut, "/api/v1/connectors/sources/"+made["muted"],
		`{"is_enabled":false}`, sess, csrf)
	if rec.Code != http.StatusOK {
		t.Fatalf("pause source: expected 200, got %d body=%s", rec.Code, rec.Body.String())
	}

	// A real queued connector.sync job for the repo source → Syncing row.
	jobsRepo := postgres.NewJobsRepo(f.db)
	now := time.Now().UTC()
	if _, _, err := jobsRepo.InsertJob(ctx, jobs.InsertRecord{
		Job: jobs.Job{
			TenantID:       f.tenant,
			ID:             "job_" + made["repo"],
			Operation:      connectors.OperationSync,
			Subject:        jobs.Subject{ConnectionID: made["repo"]},
			IdempotencyKey: "test-sync-" + made["repo"],
			InputHash:      "test-hash",
			CreatedAt:      now,
			UpdatedAt:      now,
		},
		SubmitterKey: "test-submitter",
		Principal:    json.RawMessage(`{}`),
		PolicyJSON:   []byte(`{"connection_id":"` + made["repo"] + `","stream":"content"}`),
	}); err != nil {
		t.Fatalf("seed queued sync job: %v", err)
	}

	// The page renders the seeded rows with real projections only.
	rec = sessionDo(t, f.app, http.MethodGet, "/dashboard/sources", "", sess, csrf)
	if rec.Code != http.StatusOK {
		t.Fatalf("GET /dashboard/sources: expected 200, got %d body=%s", rec.Code, rec.Body.String())
	}
	page := rec.Body.String()
	for _, want := range []string{
		"Docs repo",         // source name
		"Help desk",         // source name
		"Archive sync",      // source name
		"GitHub",            // provider subline
		"Slack",             // provider subline
		"#C123",             // slack scope from config
		"enclava/docs@HEAD", // github scope from config
		"Docs library",      // resolved collection name
		"Ready",             // healthy pill (slack, no jobs)
		"Paused",            // paused pill
		"Syncing",           // queued job row
		"every 3s",          // poll only while a row is syncing
		"Add source",        // header action
		// The single read rule: readers of the destination collection, and
		// only they, can read what the source syncs — named when resolved,
		// generic otherwise.
		"Everyone who can open Docs library can read everything this source syncs.",
		"Everyone who can open the collection can read everything this source syncs.",
	} {
		if !strings.Contains(page, want) {
			t.Fatalf("page output missing %q", want)
		}
	}
	if strings.Contains(page, "User bindings") {
		t.Fatal("sources markup must not carry a bindings action")
	}
	if !strings.Contains(page, `href="/dashboard/sources?status=syncing"`) {
		t.Fatal("page output missing the filter chips")
	}
	// Truthful gaps: no items counts and no invented progress percentages.
	if strings.Contains(page, "progressbar") {
		t.Fatal("progress bar rendered although no stats carry counts")
	}

	// Filter chips narrow server-side.
	rec = sessionDo(t, f.app, http.MethodGet, "/dashboard/sources?status=paused", "", sess, csrf)
	if rec.Code != http.StatusOK || !strings.Contains(rec.Body.String(), "Archive sync") {
		t.Fatalf("paused filter: expected only the paused row, got %d", rec.Code)
	}
	if strings.Contains(rec.Body.String(), "Docs repo") {
		t.Fatal("paused filter leaked non-paused rows")
	}
}

// TestDashboardSourcesTableStateProjection pins the row projection against
// SyncJob fixtures at the template boundary: Syncing-with-counts renders the
// progress bar, counts-less Syncing stays indeterminate, a failed last sync
// renders Needs attention with an actionable credential error.
func TestDashboardSourcesTableStateProjection(t *testing.T) {
	set, err := dashboardTemplateSet("sources")
	if err != nil {
		t.Fatalf("parse sources template set: %v", err)
	}
	last := time.Now().Add(-2 * time.Hour)
	rows := []dashboardSourceRow{
		{Source: connectors.Source{ID: "s1", Name: "Counted", Type: "slack"}, State: "syncing",
			HasProgress: true, ProgressPct: 42, Provider: "Slack", Monogram: "C",
			NextEnabled: true, NextAt: last.Add(30 * time.Minute)},
		{Source: connectors.Source{ID: "s2", Name: "Indeterminate", Type: "github"}, State: "syncing",
			Provider: "GitHub", Monogram: "I"},
		{Source: connectors.Source{ID: "s3", Name: "Broken", Type: "notion"}, State: "warning",
			LastError: "credential refused", Provider: "Notion", Monogram: "B"},
		{Source: connectors.Source{ID: "s4", Name: "Counted items", Type: "google_drive"},
			HasItems: true, Items: 1284, State: "healthy", Provider: "Google Drive", Monogram: "C"},
	}
	view := dashboardView{
		Can:  dashboardCaps{SourcesManage: true, AccessManage: true},
		Data: dashboardSourcesData{Rows: rows, AnySyncing: true},
	}
	var out strings.Builder
	if err := set.ExecuteTemplate(&out, "frag-sources-table", view); err != nil {
		t.Fatalf("render frag-sources-table: %v", err)
	}
	rendered := out.String()
	for _, want := range []string{
		`aria-valuenow="42"`,                // real counts → determinate progress bar
		`style="width:42%"`,                 // width matches the reported percent
		`class="dot pulse"`,                 // counts-less syncing → indeterminate pulse
		"Needs attention",                   // failed last sync label from the vocabulary
		"credential refused",                // the concrete failure is visible beside its next step
		`href="/dashboard/sources?edit=s3"`, // Reconnect deep-links the failing row
		"every 3s",                          // poll rides along with any syncing row
		"1,284",                             // items count when stats report it
	} {
		if !strings.Contains(rendered, want) {
			t.Fatalf("table render missing %q in:\n%s", want, rendered)
		}
	}
	// The determinate row must not also flash the pulse (exactly one of the
	// two syncing presentations per row is decided server-side).
	if strings.Count(rendered, "progressbar") != 1 {
		t.Fatalf("expected exactly one determinate progress bar, got %d", strings.Count(rendered, "progressbar"))
	}

	// Projection helpers: failed → Needs attention, unparseable intervals
	// disable next-sync instead of inventing a time.
	job := connectors.SyncJob{Status: connectors.JobStatusFailed, ErrorMessage: "boom"}
	if got := dashboardSourceState(connectors.Source{IsEnabled: true}, &job); got != "warning" {
		t.Fatalf("failed job state = %q, want warning", got)
	}
	if got := dashboardSourceState(connectors.Source{IsEnabled: false}, nil); got != "paused" {
		t.Fatalf("disabled state = %q, want paused", got)
	}
	if ok, pct := dashboardStatPercent(&connectors.SyncJob{Stats: map[string]any{"processed": 42.0, "total": 100.0}}); !ok || pct != 42 {
		t.Fatalf("count-backed progress = %d, ok=%v; want 42, true", pct, ok)
	}
	if ok, _ := dashboardStatPercent(&connectors.SyncJob{Stats: map[string]any{"percent": 42.0}}); ok {
		t.Fatal("progress without counts must remain indeterminate")
	}
	if ok, _ := dashboardStatItems(&connectors.SyncJob{Stats: map[string]any{"policy": 1}}); ok {
		t.Fatal("items projected although stats carry none")
	}
	cfg := map[string]any{"sync_interval": "30m"}
	if ok, at := dashboardNextSync(cfg, &last, last.Add(-time.Hour), true); !ok || !at.Equal(last.Add(30*time.Minute)) {
		t.Fatalf("next sync = %v ok=%v, want last+30m", at, ok)
	}
	if ok, _ := dashboardNextSync(map[string]any{"sync_interval": "0"}, nil, last, true); ok {
		t.Fatal("disabled interval must not schedule")
	}
	if ok, _ := dashboardNextSync(map[string]any{"sync_interval": "nonsense"}, nil, last, true); ok {
		t.Fatal("unparseable interval must not invent a time")
	}
	if ok, _ := dashboardNextSync(nil, nil, last, false); ok {
		t.Fatal("paused source must not schedule")
	}
}

// TestDashboardSourcesCollectionRule pins the single read rule: every row
// explains that the destination collection's readers are the only readers,
// and no bindings surface (action or partial) remains — even for a tenant
// administrator.
func TestDashboardSourcesCollectionRule(t *testing.T) {
	f := newIdentityFixture(t)
	sess, csrf := loginUser(t, f, "manager@example.com", []string{"connectors.manage", "rag", "rag.manage"}, "manager-pass")

	rec := sessionDo(t, f.app, http.MethodPost, "/api/v1/rag/collections",
		`{"name":"Shared library"}`, sess, csrf)
	if rec.Code != http.StatusCreated {
		t.Fatalf("create collection: expected 201, got %d body=%s", rec.Code, rec.Body.String())
	}
	collectionID, _ := decodeJSON(t, rec)["id"].(string)

	rec = sessionDo(t, f.app, http.MethodPost, "/api/v1/connectors/sources",
		`{"type":"github","name":"Rule repo","config":{"owner":"enclava","repo":"b"},"target_collection_id":"`+collectionID+`"}`, sess, csrf)
	if rec.Code != http.StatusCreated {
		t.Fatalf("create source: expected 201, got %d body=%s", rec.Code, rec.Body.String())
	}
	sourceID, _ := decodeJSON(t, rec)["id"].(string)

	rec = sessionDo(t, f.app, http.MethodGet, "/dashboard/sources", "", sess, csrf)
	if rec.Code != http.StatusOK {
		t.Fatalf("manager page: expected 200, got %d", rec.Code)
	}
	page := rec.Body.String()
	for _, want := range []string{"Sync now", "Pause", "Delete"} {
		if !strings.Contains(page, want) {
			t.Fatalf("manager page missing row action %q", want)
		}
	}
	if !strings.Contains(page, "Everyone who can open Shared library can read everything this source syncs.") {
		t.Fatal("sources page missing the collection read rule")
	}
	for _, gone := range []string{"User bindings", `sources:bindings`, "source accounts have no Enclava binding", "isn't linked to your Enclava user"} {
		if strings.Contains(page, gone) {
			t.Fatalf("sources page still renders bindings/unmapped surface %q", gone)
		}
	}

	// Tenant administrator (platform.admin + connectors.manage): same rule,
	// still no bindings action, and the old bindings partial is simply gone.
	adminSess, adminCSRF := loginUser(t, f, "admin-manager@example.com",
		[]string{auth.ScopePlatformAdmin, "connectors.manage", "rag"}, "admin-pass")
	rec = sessionDo(t, f.app, http.MethodGet, "/dashboard/sources", "", adminSess, adminCSRF)
	if rec.Code != http.StatusOK {
		t.Fatalf("admin page: expected 200, got %d", rec.Code)
	}
	if !strings.Contains(rec.Body.String(), "Everyone who can open Shared library can read everything this source syncs.") {
		t.Fatal("admin page missing the collection read rule")
	}
	if strings.Contains(rec.Body.String(), "User bindings") {
		t.Fatal("admin markup still renders a bindings action")
	}
	rec = sessionDo(t, f.app, http.MethodGet, "/dashboard/partials/sources/bindings?source="+sourceID, "", adminSess, adminCSRF)
	if rec.Code == http.StatusOK {
		t.Fatal("bindings partial still serves — it must be gone")
	}

	// The wizard's Access step states the same rule for every provider.
	rec = sessionDo(t, f.app, http.MethodGet, "/dashboard/partials/sources/drawer", "", sess, csrf)
	if rec.Code != http.StatusOK {
		t.Fatalf("drawer: expected 200, got %d", rec.Code)
	}
	if !strings.Contains(rec.Body.String(), "Everyone who can open the target collection can read everything this source syncs.") {
		t.Fatal("wizard missing the collection read rule")
	}
}

func TestDashboardSourcesDrawerProviders(t *testing.T) {
	f := newIdentityFixture(t)
	sess, csrf := loginUser(t, f, "manager@example.com", []string{"connectors.manage", "rag", "rag.manage"}, "manager-pass")

	rec := sessionDo(t, f.app, http.MethodGet, "/dashboard/partials/sources/drawer", "", sess, csrf)
	if rec.Code != http.StatusOK {
		t.Fatalf("drawer: expected 200, got %d body=%s", rec.Code, rec.Body.String())
	}
	drawer := rec.Body.String()
	for _, provider := range []string{"slack", "teams", "github", "notion", "google_drive"} {
		if !strings.Contains(drawer, `data-provider="`+provider+`"`) ||
			!strings.Contains(drawer, `data-provider-fields="`+provider+`"`) {
			t.Fatalf("drawer missing provider %q tile and field group", provider)
		}
	}
	// Teams: team scope, channel restrictions, chat IDs and Graph base URL.
	for _, want := range []string{"data-team-scope", "data-channels", `data-config="chat_ids"`, `data-config="graph_base_url"`} {
		if !strings.Contains(drawer, want) {
			t.Fatalf("drawer Teams group missing %s", want)
		}
	}
	// The old page's duplicated channel_ids input bug stays fixed by design:
	// exactly ONE Slack channel_id field, and Teams channels ride the
	// restriction textarea instead of a second channel_ids input.
	if got := strings.Count(drawer, `data-config="channel_id"`); got != 1 {
		t.Fatalf("expected exactly one channel_id field, got %d", got)
	}
	if strings.Contains(drawer, `data-config="channel_ids"`) {
		t.Fatal("a raw channel_ids input came back — use the pair textarea")
	}
	for _, want := range []string{`data-config="folder_id"`, "data-notion-pages", `data-config="owner"`, `data-config="repo"`} {
		if !strings.Contains(drawer, want) {
			t.Fatalf("drawer missing %s", want)
		}
	}
	// No credential refs configured on this fixture: every provider renders
	// the truthful empty state and a suggestion input (never secret values).
	for _, provider := range []string{"Slack", "Teams", "Notion", "Google Drive"} {
		if !strings.Contains(drawer, "No "+provider+" credentials configured") {
			t.Fatalf("drawer missing the %s credential empty state", provider)
		}
	}
	// The Access step states the single collection rule per provider and the
	// Notion published-audiences editor is gone.
	for _, provider := range []string{"slack", "teams", "github", "notion", "google_drive"} {
		if !strings.Contains(drawer, `data-provider-access="`+provider+`"`) {
			t.Fatalf("drawer missing the %s access pane", provider)
		}
	}
	if !strings.Contains(drawer, "Everyone who can open the target collection can read everything this source syncs.") {
		t.Fatal("drawer missing the collection read rule")
	}
	for _, gone := range []string{`data-action="sources:audience-add"`, "data-audience-row", "published audiences"} {
		if strings.Contains(drawer, gone) {
			t.Fatalf("drawer still renders removed audience surface %q", gone)
		}
	}

	// With refs bound server-side, only the provider's own ref IDs render.
	set, err := dashboardTemplateSet("sources")
	if err != nil {
		t.Fatalf("parse sources template set: %v", err)
	}
	view := dashboardView{
		Can: dashboardCaps{SourcesManage: true},
		Data: dashboardSourcesDrawerData{
			CredentialRefs: map[string][]auth.SecretRefMeta{
				"slack": {{RefID: "CONNECTOR_SLACK_HELPDESK", Purpose: "connector"}},
			},
			Collections:      []rag.Collection{{ID: "ragcol_preset", Name: "Preset library"}},
			PresetCollection: "ragcol_preset",
		},
	}
	var out strings.Builder
	if err := set.ExecuteTemplate(&out, "frag-sources-drawer", view); err != nil {
		t.Fatalf("render frag-sources-drawer: %v", err)
	}
	rendered := out.String()
	if !strings.Contains(rendered, `value="CONNECTOR_SLACK_HELPDESK"`) {
		t.Fatal("bound slack credential ref not offered")
	}
	if strings.Contains(rendered, "No Slack credentials configured") {
		t.Fatal("slack empty state rendered although a ref is bound")
	}
	if !strings.Contains(rendered, `value="ragcol_preset"`) {
		t.Fatal("preset collection not preselected")
	}
}

// TestDashboardSourcesIndexProgress proves the per-source indexing rollup is
// real: a still-listing sync shows found/indexed counts and keeps the table
// polling, failed items surface the durable job's generic "not indexed"
// line, and a fully indexed source renders counts with no failure line.
func TestDashboardSourcesIndexProgress(t *testing.T) {
	f := newIdentityFixture(t)
	ctx := context.Background()
	sess, csrf := loginUser(t, f, "admin-progress@example.com",
		[]string{auth.ScopePlatformAdmin, "connectors.manage", "rag", "rag.manage"}, "admin-pass")

	rec := sessionDo(t, f.app, http.MethodPost, "/api/v1/rag/collections",
		`{"name":"Progress library"}`, sess, csrf)
	if rec.Code != http.StatusCreated {
		t.Fatalf("create collection: expected 201, got %d body=%s", rec.Code, rec.Body.String())
	}
	collectionID, _ := decodeJSON(t, rec)["id"].(string)
	if collectionID == "" {
		t.Fatal("create collection: missing id")
	}

	made := map[string]string{}
	for name, body := range map[string]string{
		"slack": `{"type":"slack","name":"Progress chat","config":{"channel_id":"C789"},"credential_ref":"CONNECTOR_SLACK_PROGRESS","target_collection_id":"` + collectionID + `"}`,
		"clean": `{"type":"github","name":"Done repo","config":{"owner":"enclava","repo":"done"},"target_collection_id":"` + collectionID + `"}`,
	} {
		rec := sessionDo(t, f.app, http.MethodPost, "/api/v1/connectors/sources", body, sess, csrf)
		if rec.Code != http.StatusCreated {
			t.Fatalf("create %s source: expected 201, got %d body=%s", name, rec.Code, rec.Body.String())
		}
		id, _ := decodeJSON(t, rec)["id"].(string)
		if id == "" {
			t.Fatalf("create %s source: missing id in response", name)
		}
		made[name] = id
	}

	tenant := f.tenant
	for _, seed := range []struct {
		stmt string
		args []any
	}{
		// Slack: one indexed item, one published item whose embed job failed,
		// one still processing.
		{`INSERT INTO source_objects (tenant_id, id, connection_id, kind, native_id, authorization_mode, observed_seq)
		  VALUES ($1, 'prog_obj1', $2, 'message', 'slack:T1', 'collection', 1),
		         ($1, 'prog_obj2', $2, 'message', 'slack:T2', 'collection', 1),
		         ($1, 'prog_obj3', $2, 'message', 'slack:T3', 'collection', 1)`, []any{tenant, made["slack"]}},
		{`INSERT INTO source_object_versions (tenant_id, id, object_id, observed_seq, artifact_ref, content_sha256, byte_size)
		  VALUES ($1, 'prog_ver1', 'prog_obj1', 1, 'art1', 'sha1', 10),
		         ($1, 'prog_ver2', 'prog_obj2', 1, 'art2', 'sha2', 10),
		         ($1, 'prog_ver3', 'prog_obj3', 1, 'art3', 'sha3', 10)`, []any{tenant}},
		{`INSERT INTO collection_items (tenant_id, id, collection_id, object_id)
		  VALUES ($1, 'prog_ci1', $2, 'prog_obj1'), ($1, 'prog_ci2', $2, 'prog_obj2'), ($1, 'prog_ci3', $2, 'prog_obj3')`, []any{tenant, collectionID}},
		{`INSERT INTO index_generations (tenant_id, id, collection_id, object_version_id, generation, state)
		  VALUES ($1, 'prog_gen1', $2, 'prog_ver1', 1, 'published'),
		         ($1, 'prog_gen2', $2, 'prog_ver2', 1, 'published'),
		         ($1, 'prog_gen3', $2, 'prog_ver3', 1, 'staging')`, []any{tenant, collectionID}},
		{`INSERT INTO jobs (tenant_id, id, operation, status, connection_id, idempotency_key, input_hash, input_ref, principal, submitter_key, policy, error)
		  VALUES ($1, 'prog_embed_fail', 'rag.embed', 'failed', $2, 'prog-embed-1', 'h', 'prog_ver2',
		          '{}'::jsonb, 'test-submitter', '{"generation":"1"}'::jsonb,
		          '{"message":"upstream embedding endpoint refused the request"}'::jsonb)`, []any{tenant, made["slack"]}},
		// GitHub: the only item published with no failed job — fully indexed.
		{`INSERT INTO source_objects (tenant_id, id, connection_id, kind, native_id, authorization_mode, observed_seq)
		  VALUES ($1, 'prog_obj9', $2, 'file', 'gh:done/readme.md', 'collection', 1)`, []any{tenant, made["clean"]}},
		{`INSERT INTO source_object_versions (tenant_id, id, object_id, observed_seq, artifact_ref, content_sha256, byte_size)
		  VALUES ($1, 'prog_ver9', 'prog_obj9', 1, 'art9', 'sha9', 10)`, []any{tenant}},
		{`INSERT INTO collection_items (tenant_id, id, collection_id, object_id)
		  VALUES ($1, 'prog_ci9', $2, 'prog_obj9')`, []any{tenant, collectionID}},
		{`INSERT INTO index_generations (tenant_id, id, collection_id, object_version_id, generation, state)
		  VALUES ($1, 'prog_gen9', $2, 'prog_ver9', 1, 'published')`, []any{tenant, collectionID}},
	} {
		if _, err := f.db.SQL.ExecContext(ctx, seed.stmt, seed.args...); err != nil {
			t.Fatalf("seed progress rows: %v", err)
		}
	}

	// A queued content-stream job keeps the slack source in Fetching.
	jobsRepo := postgres.NewJobsRepo(f.db)
	if _, _, err := jobsRepo.InsertJob(ctx, jobs.InsertRecord{
		Job: jobs.Job{
			TenantID:       tenant,
			ID:             "prog_sync",
			Operation:      connectors.OperationSync,
			Subject:        jobs.Subject{ConnectionID: made["slack"]},
			IdempotencyKey: "prog-sync-1",
			InputHash:      "test-hash",
			CreatedAt:      time.Now().UTC(),
			UpdatedAt:      time.Now().UTC(),
		},
		SubmitterKey: "test-submitter",
		Principal:    json.RawMessage(`{}`),
		PolicyJSON:   []byte(`{"connection_id":"` + made["slack"] + `","stream":"content"}`),
	}); err != nil {
		t.Fatalf("seed queued content sync: %v", err)
	}

	rec = sessionDo(t, f.app, http.MethodGet, "/dashboard/sources", "", sess, csrf)
	if rec.Code != http.StatusOK {
		t.Fatalf("GET /dashboard/sources: expected 200, got %d body=%s", rec.Code, rec.Body.String())
	}
	page := rec.Body.String()
	for _, want := range []string{
		"Fetching… 3 found · 1 indexed", // live counts while the stream lists
		`aria-valuenow="66"`,            // (indexed+failed)/items = 2/3
		"every 3s",                      // in-flight work keeps the poll alive
		"1 not indexed: upstream embedding endpoint refused the request",
	} {
		if !strings.Contains(page, want) {
			t.Fatalf("sources page missing %q", want)
		}
	}
	// The fully indexed row carries its item count and nothing else.
	if !strings.Contains(page, `data-label="Items">1`) {
		t.Fatal("fully indexed source missing its item count")
	}
}

// TestDashboardSourcesCredentialsTab covers the access.manage-gated
// Credentials tab and its API over the real store: the disabled notice
// without ENCLAVA_SECRETS_KEY, write-only secrets, name conflicts, the
// wizard offering stored credentials, in-use deletes, the OAuth start and
// callback redirects, escaped return notices and manager denial.
func TestDashboardSourcesCredentialsTab(t *testing.T) {
	f := newIdentityFixture(t)
	repo := postgres.NewCredentialsRepo(f.db)
	disabled, err := credentials.NewService(repo, nil, nil, "")
	if err != nil {
		t.Fatal(err)
	}
	f.app.ConfigureCredentials(disabled)
	sess, csrf := loginUser(t, f, "admin-creds@example.com",
		[]string{auth.ScopePlatformAdmin, "connectors.manage", "rag"}, "admin-pass")
	errorCode := func(rec *httptest.ResponseRecorder) string {
		code, _ := decodeJSON(t, rec)["error"].(map[string]any)["code"].(string)
		return code
	}

	rec := sessionDo(t, f.app, http.MethodGet, "/dashboard/sources/credentials", "", sess, csrf)
	page := rec.Body.String()
	if rec.Code != http.StatusOK || !strings.Contains(page, "ENCLAVA_SECRETS_KEY") || strings.Contains(page, `data-action="creds:add"`) {
		t.Fatalf("disabled store: expected the notice without forms, got %d body=%s", rec.Code, page)
	}
	for _, provider := range []string{"Slack", "Microsoft Teams", "GitHub", "Notion", "Google Drive"} {
		if !strings.Contains(page, "<h2>"+provider+"</h2>") {
			t.Fatalf("credentials page missing the %s card", provider)
		}
	}
	rec = sessionDo(t, f.app, http.MethodPost, "/api/v1/connectors/credentials", `{"provider":"teams","name":"Ops Graph","secret":"s"}`, sess, csrf)
	if rec.Code != http.StatusConflict || errorCode(rec) != "credentials_disabled" {
		t.Fatalf("disabled create: got %d body=%s", rec.Code, rec.Body.String())
	}

	svc, err := credentials.NewService(repo, bytes.Repeat([]byte{9}, 32), nil, "https://enclava.test")
	if err != nil {
		t.Fatal(err)
	}
	f.app.ConfigureCredentials(svc)
	f.app.connectorSvc.SetCredentialResolver(svc)
	// Teams secrets are not probed (app-only client credentials), so this
	// create needs no provider round trip.
	rec = sessionDo(t, f.app, http.MethodPost, "/api/v1/connectors/credentials", `{"provider":"teams","name":"Ops Graph","secret":"client-secret-1"}`, sess, csrf)
	if rec.Code != http.StatusCreated || strings.Contains(rec.Body.String(), "client-secret-1") {
		t.Fatalf("create: got %d body=%s", rec.Code, rec.Body.String())
	}
	const credID = "CONNECTOR_TEAMS_OPS_GRAPH"
	if id, _ := decodeJSON(t, rec)["id"].(string); id != credID {
		t.Fatalf("create: id = %q", id)
	}
	for body, want := range map[string]string{ //nolint:gosec // Synthetic request bodies for validation cases.
		`{"provider":"teams","name":"ops graph","secret":"other"}`: "credential_conflict",
		`{"provider":"jira","name":"X","secret":"s"}`:              "credential_invalid",
		`{"provider":"teams","name":"Empty","secret":"  "}`:        "credential_invalid",
	} {
		rec = sessionDo(t, f.app, http.MethodPost, "/api/v1/connectors/credentials", body, sess, csrf)
		if got := errorCode(rec); got != want {
			t.Fatalf("POST %s: code %q (%d), want %q", body, got, rec.Code, want)
		}
	}
	rec = sessionDo(t, f.app, http.MethodGet, "/api/v1/connectors/credentials", "", sess, csrf)
	if rec.Code != http.StatusOK || !strings.Contains(rec.Body.String(), credID) || strings.Contains(rec.Body.String(), "client-secret-1") ||
		!strings.Contains(rec.Body.String(), `"redirect_uri":"https://enclava.test/dashboard/connectors/oauth/callback/slack"`) {
		t.Fatalf("list: got %d body=%s", rec.Code, rec.Body.String())
	}
	rec = sessionDo(t, f.app, http.MethodGet, "/dashboard/sources/credentials", "", sess, csrf)
	if page := rec.Body.String(); !strings.Contains(page, "Ops Graph") || !strings.Contains(page, `data-action="creds:add"`) || strings.Contains(page, "ENCLAVA_SECRETS_KEY") {
		t.Fatalf("enabled page: body=%s", page)
	}
	rec = sessionDo(t, f.app, http.MethodGet, "/dashboard/partials/sources/credentials", "", sess, csrf)
	if rec.Code != http.StatusOK || !strings.Contains(rec.Body.String(), `id="sources-credentials"`) {
		t.Fatalf("credentials fragment: expected the section, got %d body=%s", rec.Code, rec.Body.String())
	}
	rec = sessionDo(t, f.app, http.MethodGet, "/dashboard/partials/sources/drawer", "", sess, csrf)
	if !strings.Contains(rec.Body.String(), `<option value="`+credID+`">Ops Graph</option>`) {
		t.Fatalf("wizard does not offer the stored credential: %s", rec.Body.String())
	}

	rec = sessionDo(t, f.app, http.MethodPost, "/api/v1/connectors/sources",
		`{"type":"teams","name":"Ops channel","config":{"tenant_id":"t","client_id":"c","team_id":"team"},"credential_ref":"`+credID+`"}`, sess, csrf)
	if rec.Code != http.StatusCreated {
		t.Fatalf("create source: got %d body=%s", rec.Code, rec.Body.String())
	}
	sourceID, _ := decodeJSON(t, rec)["id"].(string)
	rec = sessionDo(t, f.app, http.MethodDelete, "/api/v1/connectors/credentials/"+credID, "", sess, csrf)
	if rec.Code != http.StatusConflict || errorCode(rec) != "credential_in_use" || !strings.Contains(rec.Body.String(), sourceID) {
		t.Fatalf("delete in use: got %d body=%s", rec.Code, rec.Body.String())
	}
	if rec = sessionDo(t, f.app, http.MethodDelete, "/api/v1/connectors/sources/"+sourceID, "", sess, csrf); rec.Code != http.StatusOK {
		t.Fatalf("delete source: got %d body=%s", rec.Code, rec.Body.String())
	}
	if rec = sessionDo(t, f.app, http.MethodDelete, "/api/v1/connectors/credentials/"+credID, "", sess, csrf); rec.Code != http.StatusOK {
		t.Fatalf("delete: got %d body=%s", rec.Code, rec.Body.String())
	}

	// OAuth: no app yet → back to the tab with the reason; with an app → the
	// provider's consent URL; an unknown state never stores anything.
	rec = sessionDo(t, f.app, http.MethodGet, "/dashboard/connectors/oauth/start/slack?name=Labs", "", sess, csrf)
	if loc := rec.Header().Get("Location"); rec.Code != http.StatusSeeOther || !strings.HasPrefix(loc, "/dashboard/sources/credentials?error=") {
		t.Fatalf("start without app: got %d location=%q", rec.Code, loc)
	}
	rec = sessionDo(t, f.app, http.MethodPut, "/api/v1/connectors/oauth-apps/slack", `{"client_id":"123.456","client_secret":"app-secret"}`, sess, csrf)
	if rec.Code != http.StatusOK || strings.Contains(rec.Body.String(), "app-secret") {
		t.Fatalf("save OAuth app: got %d body=%s", rec.Code, rec.Body.String())
	}
	rec = sessionDo(t, f.app, http.MethodGet, "/dashboard/connectors/oauth/start/slack?name=Labs", "", sess, csrf)
	consent, _ := url.Parse(rec.Header().Get("Location"))
	if rec.Code != http.StatusSeeOther || consent == nil || consent.Host != "slack.com" || consent.Query().Get("client_id") != "123.456" || consent.Query().Get("state") == "" {
		t.Fatalf("start: got %d location=%q", rec.Code, rec.Header().Get("Location"))
	}
	rec = sessionDo(t, f.app, http.MethodGet, "/dashboard/connectors/oauth/callback/slack?code=c&state=forged", "", sess, csrf)
	if loc := rec.Header().Get("Location"); rec.Code != http.StatusSeeOther || !strings.Contains(loc, "error=") || strings.Contains(loc, "connected=") {
		t.Fatalf("forged callback: got %d location=%q", rec.Code, loc)
	}

	// OAuth return notices render — the error is escaped text, never markup.
	rec = sessionDo(t, f.app, http.MethodGet,
		"/dashboard/sources/credentials?connected=cred_1&error="+url.QueryEscape("<b>bad</b>"), "", sess, csrf)
	body := rec.Body.String()
	if rec.Code != http.StatusOK || !strings.Contains(body, "Credential connected") || !strings.Contains(body, "&lt;b&gt;bad&lt;/b&gt;") ||
		strings.Contains(body, "<b>bad</b>") {
		t.Fatalf("credentials notices: connected/error not rendered escaped; body=%s", body)
	}

	// A sources manager without platform.admin is denied the tab, its
	// fragment, the API and the OAuth routes; the Sources page hides the tab.
	mgrSess, mgrCSRF := loginUser(t, f, "manager-creds@example.com", []string{"connectors.manage", "rag"}, "manager-pass")
	for _, path := range []string{"/dashboard/sources/credentials", "/dashboard/partials/sources/credentials", "/api/v1/connectors/credentials", "/dashboard/connectors/oauth/start/slack?name=X"} {
		rec := sessionDo(t, f.app, http.MethodGet, path, "", mgrSess, mgrCSRF)
		if rec.Code != http.StatusForbidden {
			t.Fatalf("GET %s: expected 403 for manager, got %d", path, rec.Code)
		}
	}
	rec = sessionDo(t, f.app, http.MethodGet, "/dashboard/sources", "", mgrSess, mgrCSRF)
	if rec.Code != http.StatusOK || strings.Contains(rec.Body.String(), "/dashboard/sources/credentials") {
		t.Fatalf("manager sources page: expected 200 without the Credentials tab, got %d", rec.Code)
	}
}
