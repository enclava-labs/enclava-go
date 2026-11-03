package connectors

import (
	"context"
	"errors"
	"fmt"
	"net/http"
	"net/http/httptest"
	"strings"
	"testing"

	"enclava-go/internal/artifacts"
	"enclava-go/internal/auth"
	"enclava-go/internal/jobs"
	"enclava-go/internal/source"
	"enclava-go/internal/store/postgres"
	"enclava-go/internal/testdb"
)

func newACLPipelineFixture(t *testing.T, kind, credential string, config map[string]any, adapter Adapter) *connectorFixture {
	t.Helper()
	ctx := context.Background()
	db := testdb.New(t)
	tenant := testdb.SeedTenant(t, db, "connector-acl")
	user := testdb.SeedUser(t, db, tenant, "operator@example.test", nil)
	p := auth.Principal{TenantID: tenant, ActorID: user.ID, ActorKind: auth.ActorKindUser, UserID: user.ID,
		CredentialID: "key_connector", CredentialKind: auth.CredentialKindAPIKey,
		BillingSubjectID: user.ID, BillingSubjectKind: auth.BillingSubjectUser,
		Scopes: map[string]struct{}{"connectors.manage": {}},
	}
	jobRows := postgres.NewJobsRepo(db)
	jobService := jobs.NewService(jobRows, 0)
	repo := postgres.NewSourceRepo(db)
	conn := source.Connection{TenantID: tenant, ID: "connection-test", Kind: kind, Name: kind, CredentialRef: credential,
		Config: config, TargetCollectionID: "collection-test", Status: "active", CreatedBy: user.ID}
	if err := repo.CreateConnection(ctx, conn); err != nil {
		t.Fatal(err)
	}
	store, err := artifacts.NewStore(t.TempDir(), t.TempDir(), postgres.NewArtifactRepo(db))
	if err != nil {
		t.Fatal(err)
	}
	svc := NewService(repo, jobService, jobRows)
	svc.setAllowPrivateBaseURL(true)
	obs := postgres.NewConnectorObservationsRepo(db)
	svc.SetSyncDependencies(AdapterFactories{SourceKind(kind): func(source.Connection) (Adapter, error) { return adapter, nil }}, obs, store, repo)
	svc.SetPrincipalResolver(auth.NewService(postgres.NewIdentityRepo(db), "").PrincipalForActor)
	return &connectorFixture{db: db, svc: svc, jobs: jobService, jobRows: jobRows, sourceRepo: repo, principal: p, tenant: tenant, connection: conn}
}

func TestRealAdaptersACLPipeline(t *testing.T) {
	for _, kind := range []string{SourceTypeSlack, SourceTypeGitHub, SourceTypeGoogleDrive} {
		t.Run(kind, func(t *testing.T) {
			server := httptest.NewTLSServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
				switch r.URL.Path {
				case "/conversations.history":
					_, _ = fmt.Fprint(w, `{"ok":true,"messages":[{"user":"U1","text":"message","ts":"1.000001"}]}`)
				case "/conversations.info":
					_, _ = fmt.Fprint(w, `{"ok":true,"channel":{"name":"general"}}`)
				case "/conversations.members":
					_, _ = fmt.Fprint(w, `{"ok":true,"members":["U1"]}`)
				case "/auth.test":
					_, _ = fmt.Fprint(w, `{"ok":true,"url":"https://example.slack.com/"}`)
				case "/users.list":
					_, _ = fmt.Fprint(w, `{"ok":true,"members":[{"id":"U1","name":"u1"}]}`)
				case "/repos/acme/docs/commits/HEAD":
					_, _ = fmt.Fprint(w, `{"sha":"commit1"}`)
				case "/repos/acme/docs/git/trees/commit1":
					_, _ = fmt.Fprint(w, `{"tree":[{"path":"guide.md","type":"blob","sha":"blob1","size":3}]}`)
				case "/repos/acme/docs/contents/guide.md":
					_, _ = fmt.Fprint(w, `{"content":"ZG9j","encoding":"base64","sha":"blob1"}`)
				case "/repos/acme/docs/issues", "/repos/acme/docs/pulls":
					_, _ = fmt.Fprint(w, `[]`)
				case "/repos/acme/docs/collaborators":
					_, _ = fmt.Fprint(w, `[{"login":"alice","id":1,"permissions":{"pull":true}}]`)
				case "/drive/changes/startPageToken":
					_, _ = fmt.Fprint(w, `{"startPageToken":"start"}`)
				case "/drive/changes":
					_, _ = fmt.Fprint(w, `{"newStartPageToken":"start","changes":[]}`)
				case "/drive/files":
					if strings.Contains(r.URL.Query().Get("fields"), "permissions(") {
						_, _ = fmt.Fprint(w, `{"files":[{"id":"doc","permissions":[{"id":"p1","type":"user","emailAddress":"alice@example.test","role":"reader"}]},{"id":"restricted","copyRequiresWriterPermission":true,"permissions":[{"id":"p1","type":"user","emailAddress":"alice@example.test","role":"reader"}]}]}`)
					} else {
						_, _ = fmt.Fprint(w, `{"files":[{"id":"doc","name":"guide.txt","mimeType":"text/plain","headRevisionId":"rev1"},{"id":"restricted","name":"restricted.txt","mimeType":"text/plain","headRevisionId":"rev1"}]}`)
					}
				case "/drive/files/doc", "/drive/files/restricted":
					_, _ = fmt.Fprint(w, "document")
				default:
					t.Errorf("unexpected endpoint %s", r.URL.Path)
					http.NotFound(w, r)
				}
			}))
			defer server.Close()

			var adapter Adapter
			var envRef, nativeUser, objectKind, nativeObject string
			config := map[string]any{}
			switch kind {
			case SourceTypeSlack:
				envRef, nativeUser, objectKind, nativeObject = "CONNECTOR_SLACK_TEST", "U1", "message", "slack:C1:1.000001"
				config = map[string]any{"base_url": server.URL, "channel_id": "C1"}
				adapter = &slackAdapter{client: server.Client(), baseURL: server.URL, token: "fixture-token", users: map[string]string{}}
			case SourceTypeGitHub:
				envRef, nativeUser, objectKind, nativeObject = "CONNECTOR_GITHUB_TEST", "alice", "file", "github:connection-test:acme/docs:guide.md"
				config = map[string]any{"api_url": server.URL, "owner": "acme", "repo": "docs"}
				adapter = &GitHubAdapter{owner: "acme", repo: "docs", ref: "HEAD", base: server.URL, client: server.Client()}
			case SourceTypeGoogleDrive:
				envRef, nativeUser, objectKind, nativeObject = "CONNECTOR_GOOGLE_DRIVE_TEST", "alice@example.test", SourceTypeGoogleDrive, "doc"
				config = map[string]any{"base_url": server.URL + "/drive", "folder_id": "root"}
				adapter = NewGDriveAdapter(&Service{httpClient: server.Client()})
			}
			t.Setenv(envRef, "fixture-token")
			f := newACLPipelineFixture(t, kind, envRef, config, adapter)
			ctx := context.Background()
			mapped := testdb.SeedUser(t, f.db, f.tenant, "mapped@example.test", nil)
			other := testdb.SeedUser(t, f.db, f.tenant, "other@example.test", nil)
			if err := f.sourceRepo.InsertSourceIdentity(ctx, source.SourceIdentity{TenantID: f.tenant, ConnectionID: f.connection.ID, SourceKind: kind, ID: "identity-mapped", NativeID: nativeUser, MappedUserID: mapped.ID}); err != nil {
				t.Fatal(err)
			}
			f.schedule(t)
			streams := 2
			if adapter.Capabilities().Membership {
				streams++
			}
			for range streams {
				if _, err := f.runNext(t); err != nil {
					t.Fatal(err)
				}
			}
			if _, _, err := f.sourceRepo.LatestCompleteRevision(ctx, f.tenant, f.connection.ID, source.SnapshotGrants); err != nil {
				t.Fatalf("complete grants snapshot: %v", err)
			}
			object, err := f.sourceRepo.ObjectForNativeID(ctx, f.tenant, f.connection.ID, objectKind, nativeObject)
			if err != nil {
				t.Fatalf("synced object: %v", err)
			}
			a := source.NewAuthorizer(f.sourceRepo, 0)
			if err := a.CanRead(ctx, auth.Principal{TenantID: f.tenant, ActorID: mapped.ID, UserID: mapped.ID, ActorKind: auth.ActorKindUser}, object, source.ReadOptions{}); err != nil {
				t.Fatalf("mapped user denied: %v", err)
			}
			if err := a.CanRead(ctx, auth.Principal{TenantID: f.tenant, ActorID: other.ID, UserID: other.ID, ActorKind: auth.ActorKindUser}, object, source.ReadOptions{}); !errors.Is(err, source.ErrDenied) {
				t.Fatalf("unmapped user access: %v", err)
			}
			if kind == SourceTypeGoogleDrive {
				restricted, err := f.sourceRepo.ObjectForNativeID(ctx, f.tenant, f.connection.ID, kind, "restricted")
				if err != nil {
					t.Fatalf("restricted object: %v", err)
				}
				if err := a.CanRead(ctx, auth.Principal{TenantID: f.tenant, ActorID: mapped.ID, UserID: mapped.ID, ActorKind: auth.ActorKindUser}, restricted, source.ReadOptions{}); !errors.Is(err, source.ErrDenied) {
					t.Fatalf("restricted file access: %v", err)
				}
			}
		})
	}
}
