package connectors

import (
	"context"
	"encoding/base64"
	"encoding/json"
	"errors"
	"net/http"
	"net/http/httptest"
	"strings"
	"testing"
	"time"

	"enclava-go/internal/auth"
	"enclava-go/internal/jobs"
	"enclava-go/internal/rag"
	"enclava-go/internal/source"
	"enclava-go/internal/store/postgres"
	"enclava-go/internal/testdb"
)

type fakeRAGUploader struct {
	uploads []rag.UploadDocumentInput
	// live simulates the collection's present documents; nil means "track
	// uploads automatically". deleted removes an id from the live set.
	live    map[string]struct{}
	deleted map[string]struct{}
}

func (f *fakeRAGUploader) LiveSourceIDs(ctx context.Context, p auth.Principal, collectionID string) (map[string]struct{}, error) {
	if f.live == nil {
		f.live = map[string]struct{}{}
		for _, u := range f.uploads {
			f.live[u.SourceID] = struct{}{}
		}
	}
	out := make(map[string]struct{}, len(f.live))
	for id := range f.live {
		if _, gone := f.deleted[id]; !gone {
			out[id] = struct{}{}
		}
	}
	return out, nil
}

func (f *fakeRAGUploader) UploadDocument(ctx context.Context, p auth.Principal, collectionID string, input rag.UploadDocumentInput) (rag.UploadDocumentOutput, error) {
	f.uploads = append(f.uploads, input)
	return rag.UploadDocumentOutput{
		Document: rag.Document{
			ID:           "ragdoc_test",
			CollectionID: collectionID,
			Status:       rag.DocumentStatusReady,
			FileName:     input.FileName,
			MimeType:     input.MimeType,
		},
	}, nil
}

type fixture struct {
	db        *postgres.DB
	svc       *Service
	jobs      *jobs.Service
	repo      *postgres.SourceRepo
	uploader  *fakeRAGUploader
	tenant    string
	principal auth.Principal
}

func newConnectorFixture(t *testing.T) *fixture {
	t.Helper()
	db := testdb.New(t)
	tenant := testdb.SeedTenant(t, db, "conn")
	user := testdb.SeedUser(t, db, tenant, "conn-owner@example.test", nil)
	jobsRepo := postgres.NewJobsRepo(db)
	jobsSvc := jobs.NewService(jobsRepo, 0)
	repo := postgres.NewSourceRepo(db)
	uploader := &fakeRAGUploader{}
	svc := NewService(repo, jobsSvc, jobsRepo, uploader)
	p := auth.Principal{
		TenantID:       tenant,
		ActorID:        user.ID,
		ActorKind:      auth.ActorKindUser,
		UserID:         user.ID,
		CredentialID:   "key_test",
		CredentialKind: auth.CredentialKindAPIKey,
	}
	return &fixture{db: db, svc: svc, jobs: jobsSvc, repo: repo, uploader: uploader, tenant: tenant, principal: p}
}

// runNextJob leases the next queued connector.sync job and runs the
// handler — the real fenced worker path.
func (f *fixture) runNextJob(t *testing.T) error {
	t.Helper()
	ctx := context.Background()
	claim, err := f.jobs.Lease(ctx, "test-worker", time.Minute)
	if err != nil {
		t.Fatalf("lease: %v", err)
	}
	if claim == nil {
		return errors.New("no job to lease")
	}
	resultRef, runErr := f.svc.JobHandler()(ctx, claim, nil)
	if runErr != nil {
		_ = claim.Fail(ctx, runErr, jobs.FailureFatal)
		return runErr
	}
	if _, err := claim.Complete(ctx, resultRef); err != nil {
		t.Fatalf("complete: %v", err)
	}
	return nil
}

func TestSyncSourceEnqueuesDurableJobAndSyncs(t *testing.T) {
	f := newConnectorFixture(t)
	ctx := context.Background()
	src, err := f.svc.CreateSource(ctx, f.principal, CreateSourceInput{
		Type:               SourceTypeSlack,
		Name:               "Slack Fixture",
		TargetCollectionID: "ragcol_test",
		Config: map[string]any{
			"documents": []any{
				map[string]any{
					"file_name":  "thread.txt",
					"mime_type":  "text/plain",
					"content":    "connector fixture phrase",
					"source_id":  "thread-1",
					"source_uri": "slack://thread-1",
				},
			},
		},
	})
	if err != nil {
		t.Fatalf("create source: %v", err)
	}

	job, err := f.svc.SyncSource(ctx, f.principal, src.ID)
	if err != nil {
		t.Fatalf("sync source: %v", err)
	}
	if job.Status != JobStatusQueued {
		t.Fatalf("expected queued job before dispatch, got %#v", job)
	}
	if job.ConnectorSourceID != src.ID {
		t.Fatalf("job not bound to connection: %#v", job)
	}
	if len(f.uploader.uploads) != 0 {
		t.Fatal("sync must not run synchronously — the job is queued only")
	}
	if err := f.runNextJob(t); err != nil {
		t.Fatalf("run sync job: %v", err)
	}
	if len(f.uploader.uploads) != 1 {
		t.Fatalf("expected one upload, got %d", len(f.uploader.uploads))
	}
	if f.uploader.uploads[0].SourceType != SourceTypeSlack || f.uploader.uploads[0].SourceID != "thread-1" {
		t.Fatalf("unexpected upload metadata: %#v", f.uploader.uploads[0])
	}
	stored, err := f.svc.GetSource(ctx, f.principal, src.ID)
	if err != nil {
		t.Fatalf("get source: %v", err)
	}
	if stored.LastSyncAt == nil {
		t.Fatal("expected last_sync_at recorded")
	}
}

func TestSyncSourceIdempotentEnqueue(t *testing.T) {
	f := newConnectorFixture(t)
	ctx := context.Background()
	src, err := f.svc.CreateSource(ctx, f.principal, CreateSourceInput{
		Type:               SourceTypeGitHub,
		Name:               "Idempotent",
		TargetCollectionID: "ragcol_test",
		Config: map[string]any{
			"documents": []any{
				map[string]any{"file_name": "a.txt", "content": "same work"},
			},
		},
	})
	if err != nil {
		t.Fatalf("create source: %v", err)
	}
	j1, err := f.svc.SyncSource(ctx, f.principal, src.ID)
	if err != nil {
		t.Fatalf("first sync: %v", err)
	}
	j2, err := f.svc.SyncSource(ctx, f.principal, src.ID)
	if err != nil {
		t.Fatalf("second sync: %v", err)
	}
	if j1.ID != j2.ID {
		t.Fatalf("expected idempotent replay to return same job, got %s vs %s", j1.ID, j2.ID)
	}
	// One queued job only — running it twice is impossible: a second Lease
	// returns nothing runnable once it completes.
	if err := f.runNextJob(t); err != nil {
		t.Fatalf("run job: %v", err)
	}
	claim, err := f.jobs.Lease(ctx, "test-worker", time.Minute)
	if err != nil {
		t.Fatalf("lease: %v", err)
	}
	if claim != nil {
		t.Fatal("expected no second job after completion")
	}
}

func TestCrossTenantConnectionDeny(t *testing.T) {
	f := newConnectorFixture(t)
	ctx := context.Background()
	src, err := f.svc.CreateSource(ctx, f.principal, CreateSourceInput{
		Type:   SourceTypeGitHub,
		Name:   "Tenant A",
		Config: map[string]any{"documents": []any{map[string]any{"content": "x"}}},
	})
	if err != nil {
		t.Fatalf("create source: %v", err)
	}
	otherTenant := testdb.SeedTenant(t, f.db, "other")
	otherUser := testdb.SeedUser(t, f.db, otherTenant, "other@example.test", nil)
	intruder := auth.Principal{TenantID: otherTenant, ActorID: otherUser.ID, ActorKind: auth.ActorKindUser, UserID: otherUser.ID}
	if _, err := f.svc.GetSource(ctx, intruder, src.ID); !errors.Is(err, source.ErrDenied) {
		t.Fatalf("expected cross-tenant deny, got %v", err)
	}
	if _, err := f.svc.SyncSource(ctx, intruder, src.ID); !errors.Is(err, source.ErrDenied) {
		t.Fatalf("expected cross-tenant sync deny, got %v", err)
	}
	sources, err := f.svc.ListSources(ctx, intruder, "", 20, 0)
	if err != nil {
		t.Fatalf("list: %v", err)
	}
	if len(sources) != 0 {
		t.Fatalf("cross-tenant list leaked %#v", sources)
	}
}

func TestCreateSourceRejectsUnsafeCredentialRefs(t *testing.T) {
	f := newConnectorFixture(t)
	ctx := context.Background()
	for _, tc := range []struct{ name, ref string }{
		{"admin password", "ADMIN_PASSWORD"},
		{"privatemode key", "PRIVATEMODE_API_KEY"},
		{"bootstrap key", "BOOTSTRAP_API_KEY"},
		{"cross type", "CONNECTOR_GITHUB_TOKEN"},
		{"lowercase", "connector_slack_token"},
	} {
		t.Run(tc.name, func(t *testing.T) {
			_, err := f.svc.CreateSource(ctx, f.principal, CreateSourceInput{
				Type:          SourceTypeSlack,
				Name:          "Unsafe credential",
				CredentialRef: tc.ref,
			})
			if err == nil {
				t.Fatalf("expected credential_ref %q to be rejected", tc.ref)
			}
		})
	}
	if _, err := f.svc.CreateSource(ctx, f.principal, CreateSourceInput{
		Type: SourceTypeSlack,
		Name: "Missing credential",
	}); err == nil {
		t.Fatal("expected empty slack credential_ref to be rejected")
	}
	if _, err := f.svc.CreateSource(ctx, f.principal, CreateSourceInput{
		Type: "linear", Name: "legacy linear",
	}); err == nil {
		t.Fatal("expected legacy linear type to be rejected — not canonical")
	}
}

func TestCreateSourceAcceptsMatchingCredentialRef(t *testing.T) {
	f := newConnectorFixture(t)
	ctx := context.Background()
	src, err := f.svc.CreateSource(ctx, f.principal, CreateSourceInput{
		Type:          SourceTypeSlack,
		Name:          "Slack token",
		CredentialRef: "CONNECTOR_SLACK_TOKEN",
		Config:        map[string]any{"documents": []any{map[string]any{"content": "x"}}},
	})
	if err != nil {
		t.Fatalf("create source: %v", err)
	}
	if src.CredentialRef != "CONNECTOR_SLACK_TOKEN" {
		t.Fatalf("unexpected credential_ref: %q", src.CredentialRef)
	}
	replacement := "ADMIN_PASSWORD"
	if _, err := f.svc.UpdateSource(ctx, f.principal, src.ID, UpdateSourceInput{CredentialRef: &replacement}); err == nil {
		t.Fatal("expected update to reject ADMIN_PASSWORD")
	}
	stored, err := f.svc.GetSource(ctx, f.principal, src.ID)
	if err != nil {
		t.Fatalf("get source: %v", err)
	}
	if stored.CredentialRef != "CONNECTOR_SLACK_TOKEN" {
		t.Fatalf("credential_ref changed after rejected update: %q", stored.CredentialRef)
	}
}

func TestCreateSourceRejectsPrivateBaseURL(t *testing.T) {
	f := newConnectorFixture(t)
	ctx := context.Background()
	for _, rawURL := range []string{
		"http://169.254.169.254/",
		"http://10.1.2.3/",
		"http://127.0.0.1/",
		"http://192.168.1.20/",
		"http://[::1]/",
	} {
		t.Run(rawURL, func(t *testing.T) {
			_, err := f.svc.CreateSource(ctx, f.principal, CreateSourceInput{
				Type: SourceTypeSlack,
				Name: "Private origin",
				Config: map[string]any{
					"base_url":              rawURL,
					"allow_unauthenticated": "true",
					"channel_id":            "C123",
				},
			})
			if err == nil {
				t.Fatalf("expected base_url %s to be rejected", rawURL)
			}
		})
	}
	resolveCtx, cancel := context.WithTimeout(ctx, 5*time.Second)
	defer cancel()
	if _, err := f.svc.CreateSource(resolveCtx, f.principal, CreateSourceInput{
		Type: SourceTypeGitHub,
		Name: "Unresolved origin",
		Config: map[string]any{
			"base_url": "http://connector-origin.invalid/v1",
			"owner":    "acme",
			"repo":     "docs",
			"paths":    []any{"README.md"},
		},
	}); err == nil {
		t.Fatal("expected unresolved base_url to be rejected")
	}
}

func TestSyncGitHubSourceFetchesFiles(t *testing.T) {
	f := newConnectorFixture(t)
	ctx := context.Background()
	f.svc.setAllowPrivateBaseURL(true)
	server := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		if r.URL.Path != "/repos/acme/docs/contents/README.md" {
			t.Fatalf("unexpected path: %s", r.URL.Path)
		}
		writeTestJSON(t, w, map[string]any{
			"name":     "README.md",
			"path":     "README.md",
			"type":     "file",
			"encoding": "base64",
			"content":  base64.StdEncoding.EncodeToString([]byte("github connector maple")),
			"html_url": "https://github.example/acme/docs/README.md",
		})
	}))
	defer server.Close()

	src, err := f.svc.CreateSource(ctx, f.principal, CreateSourceInput{
		Type:               SourceTypeGitHub,
		Name:               "GitHub Docs",
		TargetCollectionID: "ragcol_test",
		Config: map[string]any{
			"base_url": server.URL,
			"owner":    "acme",
			"repo":     "docs",
			"paths":    []any{"README.md"},
		},
	})
	if err != nil {
		t.Fatalf("create source: %v", err)
	}
	if _, err := f.svc.SyncSource(ctx, f.principal, src.ID); err != nil {
		t.Fatalf("sync source: %v", err)
	}
	if err := f.runNextJob(t); err != nil {
		t.Fatalf("run sync: %v", err)
	}
	if len(f.uploader.uploads) != 1 || !strings.Contains(string(f.uploader.uploads[0].Bytes), "github connector maple") {
		t.Fatalf("unexpected github upload: %#v", f.uploader.uploads)
	}
	if f.uploader.uploads[0].SourceType != SourceTypeGitHub || f.uploader.uploads[0].SourceID != "github:acme/docs:README.md" {
		t.Fatalf("unexpected github source metadata: %#v", f.uploader.uploads[0])
	}
}

func TestSyncReingestsDeletedDocument(t *testing.T) {
	f := newConnectorFixture(t)
	ctx := context.Background()
	f.svc.setAllowPrivateBaseURL(true)
	requests := 0
	server := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		requests++
		writeTestJSON(t, w, map[string]any{
			"ok": true,
			"messages": []any{
				map[string]any{"user": "U1", "text": "deleted doc returns", "ts": "2000.000001"},
			},
		})
	}))
	defer server.Close()

	src, err := f.svc.CreateSource(ctx, f.principal, CreateSourceInput{
		Type:               SourceTypeSlack,
		Name:               "Deleted Doc",
		TargetCollectionID: "ragcol_test",
		CredentialRef:      "CONNECTOR_SLACK_TOKEN",
		Config:             map[string]any{"base_url": server.URL, "channel_id": "C777"},
	})
	if err != nil {
		t.Fatalf("create source: %v", err)
	}
	bump := func(v string) {
		t.Helper()
		if _, err := f.svc.UpdateSource(ctx, f.principal, src.ID, UpdateSourceInput{
			Config: map[string]any{"base_url": server.URL, "channel_id": "C777", "bump": v},
		}); err != nil {
			t.Fatalf("bump %s: %v", v, err)
		}
	}
	sync := func() {
		t.Helper()
		if _, err := f.svc.SyncSource(ctx, f.principal, src.ID); err != nil {
			t.Fatalf("sync: %v", err)
		}
		if err := f.runNextJob(t); err != nil {
			t.Fatalf("run job: %v", err)
		}
	}
	t.Setenv("CONNECTOR_SLACK_TOKEN", "slack-token")

	sync()
	if len(f.uploader.uploads) != 1 {
		t.Fatalf("first sync should upload once, got %d", len(f.uploader.uploads))
	}
	bump("v2")
	sync()
	if len(f.uploader.uploads) != 1 {
		t.Fatalf("unchanged existing document must skip, got %d uploads", len(f.uploader.uploads))
	}
	// Operator deletes the document; identical upstream content must now be
	// re-ingested by the next sync instead of skipped forever.
	if f.uploader.deleted == nil {
		f.uploader.deleted = map[string]struct{}{}
	}
	f.uploader.deleted["slack:C777:2000.000001"] = struct{}{}
	bump("v3")
	sync()
	if len(f.uploader.uploads) != 2 {
		t.Fatalf("deleted document must be re-ingested, got %d uploads", len(f.uploader.uploads))
	}
	_ = requests
}

func TestSyncSlackSourceFetchesChannelHistory(t *testing.T) {
	f := newConnectorFixture(t)
	ctx := context.Background()
	f.svc.setAllowPrivateBaseURL(true)
	t.Setenv("CONNECTOR_SLACK_TOKEN", "slack-token")
	server := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		if r.Header.Get("Authorization") != "Bearer slack-token" {
			t.Fatalf("missing auth header")
		}
		if r.URL.Query().Get("channel") != "C123" {
			t.Fatalf("unexpected channel: %s", r.URL.RawQuery)
		}
		writeTestJSON(t, w, map[string]any{
			"ok": true,
			"messages": []any{
				map[string]any{"user": "U1", "text": "slack connector cedar", "ts": "1.0"},
			},
		})
	}))
	defer server.Close()

	src, err := f.svc.CreateSource(ctx, f.principal, CreateSourceInput{
		Type:               SourceTypeSlack,
		Name:               "Slack Channel",
		TargetCollectionID: "ragcol_test",
		CredentialRef:      "CONNECTOR_SLACK_TOKEN",
		Config: map[string]any{
			"base_url":   server.URL,
			"channel_id": "C123",
		},
	})
	if err != nil {
		t.Fatalf("create source: %v", err)
	}
	if _, err := f.svc.SyncSource(ctx, f.principal, src.ID); err != nil {
		t.Fatalf("sync source: %v", err)
	}
	if err := f.runNextJob(t); err != nil {
		t.Fatalf("run sync: %v", err)
	}
	if len(f.uploader.uploads) != 1 || !strings.Contains(string(f.uploader.uploads[0].Bytes), "slack connector cedar") {
		t.Fatalf("unexpected slack upload: %#v", f.uploader.uploads)
	}
}

func TestSyncSlackSourceUsesTimestampCheckpoint(t *testing.T) {
	f := newConnectorFixture(t)
	ctx := context.Background()
	f.svc.setAllowPrivateBaseURL(true)
	t.Setenv("CONNECTOR_SLACK_TOKEN", "slack-token")
	requests := 0
	server := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		requests++
		if requests == 1 {
			if r.URL.Query().Get("oldest") != "" {
				t.Fatalf("first request should not include oldest: %s", r.URL.RawQuery)
			}
			writeTestJSON(t, w, map[string]any{
				"ok": true,
				"messages": []any{
					map[string]any{"user": "U1", "text": "slack checkpoint first", "ts": "1000.000001"},
					map[string]any{"user": "U2", "text": "slack checkpoint second", "ts": "1000.000003"},
				},
			})
			return
		}
		if r.URL.Query().Get("oldest") != "1000.000003" || r.URL.Query().Get("inclusive") != "false" {
			t.Fatalf("second request missing checkpoint cursor: %s", r.URL.RawQuery)
		}
		writeTestJSON(t, w, map[string]any{
			"ok":       true,
			"messages": []any{},
		})
	}))
	defer server.Close()

	src, err := f.svc.CreateSource(ctx, f.principal, CreateSourceInput{
		Type:               SourceTypeSlack,
		Name:               "Slack Checkpoint",
		TargetCollectionID: "ragcol_test",
		CredentialRef:      "CONNECTOR_SLACK_TOKEN",
		Config: map[string]any{
			"base_url":   server.URL,
			"channel_id": "C123",
		},
	})
	if err != nil {
		t.Fatalf("create source: %v", err)
	}
	// Change config between syncs so the idempotency key differs — same
	// config would replay the same durable job.
	if _, err := f.svc.SyncSource(ctx, f.principal, src.ID); err != nil {
		t.Fatalf("first sync: %v", err)
	}
	if err := f.runNextJob(t); err != nil {
		t.Fatalf("run first: %v", err)
	}
	if _, err := f.svc.UpdateSource(ctx, f.principal, src.ID, UpdateSourceInput{
		Config: map[string]any{"base_url": server.URL, "channel_id": "C123", "bump": "v2"},
	}); err != nil {
		t.Fatalf("bump config: %v", err)
	}
	if _, err := f.svc.SyncSource(ctx, f.principal, src.ID); err != nil {
		t.Fatalf("second sync: %v", err)
	}
	if err := f.runNextJob(t); err != nil {
		t.Fatalf("run second: %v", err)
	}
	if len(f.uploader.uploads) != 2 {
		t.Fatalf("expected one upload per message and no new ones, got %d", len(f.uploader.uploads))
	}
	updated, err := f.svc.GetSource(ctx, f.principal, src.ID)
	if err != nil {
		t.Fatalf("get updated source: %v", err)
	}
	if updated.Checkpoint["slack_latest_ts"] != "1000.000003" {
		t.Fatalf("expected slack timestamp checkpoint, got %#v", updated.Checkpoint)
	}
}

func TestConnectorRedirectDoesNotForwardAuthorization(t *testing.T) {
	f := newConnectorFixture(t)
	ctx := context.Background()
	f.svc.setAllowPrivateBaseURL(true)
	t.Setenv("CONNECTOR_SLACK_TOKEN", "super-secret")

	secondHits := 0
	secondAuth := ""
	second := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		secondHits++
		secondAuth = r.Header.Get("Authorization")
		writeTestJSON(t, w, map[string]any{"ok": true, "messages": []any{}})
	}))
	defer second.Close()

	firstHits := 0
	first := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		firstHits++
		if got := r.Header.Get("Authorization"); got != "Bearer super-secret" {
			t.Errorf("first hop authorization = %q", got)
		}
		http.Redirect(w, r, second.URL+"/stolen", http.StatusFound)
	}))
	defer first.Close()

	src, err := f.svc.CreateSource(ctx, f.principal, CreateSourceInput{
		Type:               SourceTypeSlack,
		Name:               "Redirect Slack",
		TargetCollectionID: "ragcol_test",
		CredentialRef:      "CONNECTOR_SLACK_TOKEN",
		Config: map[string]any{
			"base_url":   first.URL,
			"channel_id": "C123",
		},
	})
	if err != nil {
		t.Fatalf("create source: %v", err)
	}
	if _, err := f.svc.SyncSource(ctx, f.principal, src.ID); err != nil {
		t.Fatalf("sync source: %v", err)
	}
	if err := f.runNextJob(t); err == nil || !strings.Contains(err.Error(), "redirect") {
		t.Fatalf("expected redirect to fail the sync job, got %v", err)
	}
	if firstHits != 1 {
		t.Fatalf("expected one request to the redirecting server, got %d", firstHits)
	}
	if secondHits != 0 || secondAuth != "" {
		t.Fatalf("redirect was followed, hits=%d authorization=%q", secondHits, secondAuth)
	}
	if len(f.uploader.uploads) != 0 {
		t.Fatalf("expected no uploads after a refused redirect, got %d", len(f.uploader.uploads))
	}
}

func writeTestJSON(t *testing.T, w http.ResponseWriter, payload any) {
	t.Helper()
	w.Header().Set("Content-Type", "application/json")
	if err := json.NewEncoder(w).Encode(payload); err != nil {
		t.Fatalf("write json: %v", err)
	}
}
