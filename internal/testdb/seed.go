package testdb

import (
	"context"
	"crypto/rand"
	"encoding/hex"
	"encoding/json"
	"fmt"
	"testing"
	"time"

	"enclava-go/internal/auth"
	"enclava-go/internal/store/postgres"
)

// SeedTenant creates a tenant and returns its generated UUID.
func SeedTenant(t *testing.T, db *postgres.DB, slug string) string {
	t.Helper()
	repo := postgres.NewIdentityRepo(db)
	id, err := repo.CreateTenant(context.Background(), slug, slug)
	if err != nil {
		t.Fatalf("seed tenant %q: %v", slug, err)
	}
	return id
}

// SeedUser inserts an active user with a bcrypt password and returns it.
// scopes are granted via a dedicated seeded role assigned to the user.
func SeedUser(t *testing.T, db *postgres.DB, tenantID, email string, scopes []string) auth.User {
	t.Helper()
	ctx := context.Background()
	repo := postgres.NewIdentityRepo(db)
	hash, err := auth.HashPassword("seeded-password-" + randSuffix())
	if err != nil {
		t.Fatalf("hash seed password: %v", err)
	}
	user := auth.User{
		ID:          "user_" + randSuffix(),
		TenantID:    tenantID,
		Email:       email,
		DisplayName: email,
		Status:      "active",
	}
	now := time.Now().UTC()
	user.PasswordHash = hash
	user.PasswordSetAt = &now
	if err := repo.CreateUser(ctx, tenantID, user); err != nil {
		t.Fatalf("seed user %q: %v", email, err)
	}
	if len(scopes) > 0 {
		role := auth.Role{ID: "role_" + randSuffix(), Name: "seeded-" + user.ID, Scopes: scopes}
		if err := repo.CreateRole(ctx, tenantID, role); err != nil {
			t.Fatalf("seed role: %v", err)
		}
		if err := repo.AssignRole(ctx, tenantID, user.ID, role.ID); err != nil {
			t.Fatalf("assign role: %v", err)
		}
	}
	return user
}

// SeedServiceAccount inserts an active service account and returns it.
func SeedServiceAccount(t *testing.T, db *postgres.DB, tenantID, name string, scopes []string) auth.ServiceAccount {
	t.Helper()
	repo := postgres.NewIdentityRepo(db)
	sa := auth.ServiceAccount{
		ID:     "svcacct_" + randSuffix(),
		Name:   name,
		Scopes: scopes,
		Status: "active",
	}
	if err := repo.CreateServiceAccount(context.Background(), tenantID, sa); err != nil {
		t.Fatalf("seed service account %q: %v", name, err)
	}
	return sa
}

// SeedKeySpec describes the key row SeedAPIKey writes.
type SeedKeySpec struct {
	Name             string
	OwnerUserID      string // exactly one of OwnerUserID / ServiceAccountID
	ServiceAccountID string
	Scopes           []string
	ExpiresAt        *time.Time
}

// SeedAPIKey inserts a real stored key row (sha256 of the returned plaintext,
// matching auth.HashForStorage with an empty pepper) and returns the
// plaintext secret plus the stored record.
func SeedAPIKey(t *testing.T, db *postgres.DB, tenantID string, spec SeedKeySpec) (plaintext string, record auth.APIKeyRecord) {
	t.Helper()
	plaintext = "enc_test_" + randSuffix() + randSuffix()
	prefix, hash, algo, err := auth.HashForStorage(plaintext, auth.HashAlgoSHA256, "")
	if err != nil {
		t.Fatalf("hash seed key: %v", err)
	}
	record = auth.APIKeyRecord{
		ID:               "key_" + randSuffix(),
		TenantID:         tenantID,
		OwnerUserID:      spec.OwnerUserID,
		ServiceAccountID: spec.ServiceAccountID,
		Name:             spec.Name,
		KeyPrefix:        prefix,
		KeyHash:          hash,
		KeyHashAlgo:      algo,
		Scopes:           listGrant(spec.Scopes),
		AllowedModels:    allGrant(),
		AllowedTemplates: allGrant(),
		AllowedChatbots:  allGrant(),
		AllowedAgents:    allGrant(),
		AllowedEndpoints: allGrant(),
		AllowedIPs:       allGrant(),
		IsActive:         true,
		ExpiresAt:        spec.ExpiresAt,
	}
	if err := postgres.NewIdentityRepo(db).CreateAPIKey(context.Background(), tenantID, record); err != nil {
		t.Fatalf("seed api key: %v", err)
	}
	return plaintext, record
}

func listGrant(items []string) json.RawMessage {
	raw, err := json.Marshal(struct {
		Mode  string   `json:"mode"`
		Items []string `json:"items"`
	}{Mode: "list", Items: items})
	if err != nil {
		panic(fmt.Sprintf("marshal list grant: %v", err))
	}
	return raw
}

func allGrant() json.RawMessage {
	return json.RawMessage(`{"mode":"all"}`)
}

func randSuffix() string {
	var buf [8]byte
	if _, err := rand.Read(buf[:]); err != nil {
		panic(err)
	}
	return hex.EncodeToString(buf[:])
}
