package httpx

import (
	"bytes"
	"context"
	"encoding/json"
	"net/http"
	"net/http/httptest"
	"testing"

	"enclava-go/internal/auth"
	"enclava-go/internal/connectors"
	"enclava-go/internal/jobs"
	"enclava-go/internal/source"
	"enclava-go/internal/store/postgres"
	"enclava-go/internal/testdb"
)

func TestConnectorBindingHTTPBindRevokeControlsSourceReads(t *testing.T) {
	db := testdb.New(t)
	ctx := context.Background()
	identityRepo := postgres.NewIdentityRepo(db)
	tenant := testdb.SeedTenant(t, db, "connector-binding-http")
	admin := testdb.SeedUser(t, db, tenant, "binding-admin@example.test", nil)
	mapped := testdb.SeedUser(t, db, tenant, "binding-user@example.test", nil)
	key, _ := testdb.SeedAPIKey(t, db, tenant, testdb.SeedKeySpec{Name: "binding-admin", OwnerUserID: admin.ID, Scopes: []string{"connectors.manage", auth.ScopePlatformAdmin}})
	sourceRepo := postgres.NewSourceRepo(db)
	connection := source.Connection{TenantID: tenant, ID: "connection-binding", Kind: source.ConnectionSlack, Name: "Slack", Config: map[string]any{}, Status: "active", CreatedBy: admin.ID}
	if err := sourceRepo.CreateConnection(ctx, connection); err != nil {
		t.Fatal(err)
	}
	if err := sourceRepo.InsertSourceIdentity(ctx, source.SourceIdentity{TenantID: tenant, ConnectionID: connection.ID, ID: "observed-user", NativeID: "slack-user-9"}); err != nil {
		t.Fatal(err)
	}
	if _, err := sourceRepo.SourceIdentityForUser(ctx, tenant, connection.ID, mapped.ID); err == nil {
		t.Fatal("unbound user unexpectedly resolved")
	}
	jobsRepo := postgres.NewJobsRepo(db)
	connectorSvc := connectors.NewService(sourceRepo, jobs.NewService(jobsRepo, 0), jobsRepo)
	connectorSvc.SetSyncDependencies(connectors.AdapterFactories{}, nil, nil, sourceRepo)
	app := NewApp(auth.NewService(identityRepo, ""), nil, "", nil, identityRepo, nil, nil, nil, nil, nil, nil, nil, nil, "sha256", "", "enc_", nil, nil, 1, false, nil, nil, GateConfig{}, sourceRepo, nil)
	app.connectorSvc = connectorSvc
	request := func(method, path string, payload any) *httptest.ResponseRecorder {
		var body bytes.Buffer
		if payload != nil {
			_ = json.NewEncoder(&body).Encode(payload)
		}
		req := httptest.NewRequest(method, path, &body)
		req.Header.Set("Authorization", "Bearer "+key)
		rec := httptest.NewRecorder()
		app.Handler().ServeHTTP(rec, req)
		return rec
	}
	bound := request(http.MethodPost, "/api/v1/connectors/sources/"+connection.ID+"/bindings", map[string]string{
		"source_user_native_id": "slack-user-9", "mapped_user_id": mapped.ID,
		"verification_evidence": "verified against the provider user record",
	})
	if bound.Code != http.StatusCreated {
		t.Fatalf("bind status=%d body=%s", bound.Code, bound.Body.String())
	}
	var binding source.SourceIdentity
	if err := json.Unmarshal(bound.Body.Bytes(), &binding); err != nil {
		t.Fatal(err)
	}
	if _, err := sourceRepo.SourceIdentityForUser(ctx, tenant, connection.ID, mapped.ID); err != nil {
		t.Fatalf("binding did not enable source read: %v", err)
	}
	listed := request(http.MethodGet, "/api/v1/connectors/sources/"+connection.ID+"/bindings", nil)
	if listed.Code != http.StatusOK || !bytes.Contains(listed.Body.Bytes(), []byte("slack-user-9")) {
		t.Fatalf("binding list status=%d body=%s", listed.Code, listed.Body.String())
	}
	revoked := request(http.MethodDelete, "/api/v1/connectors/sources/"+connection.ID+"/bindings/"+binding.ID, nil)
	if revoked.Code != http.StatusOK {
		t.Fatalf("revoke status=%d body=%s", revoked.Code, revoked.Body.String())
	}
	if _, err := sourceRepo.SourceIdentityForUser(ctx, tenant, connection.ID, mapped.ID); err == nil {
		t.Fatal("revoked binding still resolved for source reads")
	}
}
