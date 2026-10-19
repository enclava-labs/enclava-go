package auth_test

import (
	"context"
	"crypto"
	"crypto/rand"
	"crypto/rsa"
	"crypto/sha256"
	"encoding/base64"
	"encoding/json"
	"math/big"
	"net/http"
	"net/http/httptest"
	"net/url"
	"strings"
	"testing"
	"time"

	"enclava-go/internal/auth"
	"enclava-go/internal/store/postgres"
	"enclava-go/internal/testdb"
)

// stubOIDCProvider is a discovery+token+JWKS provider on httptest that signs
// ID tokens with an in-memory RSA key.
type stubOIDCProvider struct {
	srv   *httptest.Server
	key   *rsa.PrivateKey
	kid   string
	nonce string // filled from the consumed oidc_states row per token request
	db    *postgres.DB
}

func newStubOIDCProvider(t *testing.T, db *postgres.DB) *stubOIDCProvider {
	t.Helper()
	key, err := rsa.GenerateKey(rand.Reader, 2048)
	if err != nil {
		t.Fatalf("generate RSA key: %v", err)
	}
	p := &stubOIDCProvider{key: key, kid: "stub-key-1", db: db}
	mux := http.NewServeMux()
	mux.HandleFunc("/.well-known/openid-configuration", func(w http.ResponseWriter, r *http.Request) {
		w.Header().Set("Content-Type", "application/json")
		_ = json.NewEncoder(w).Encode(map[string]any{
			"issuer":                                p.srv.URL,
			"authorization_endpoint":                p.srv.URL + "/authorize",
			"token_endpoint":                        p.srv.URL + "/token",
			"jwks_uri":                              p.srv.URL + "/keys",
			"id_token_signing_alg_values_supported": []string{"RS256"},
		})
	})
	mux.HandleFunc("/keys", func(w http.ResponseWriter, r *http.Request) {
		n := base64.RawURLEncoding.EncodeToString(p.key.PublicKey.N.Bytes())
		e := base64.RawURLEncoding.EncodeToString(big.NewInt(int64(p.key.PublicKey.E)).Bytes())
		w.Header().Set("Content-Type", "application/json")
		_ = json.NewEncoder(w).Encode(map[string]any{
			"keys": []map[string]any{{
				"kty": "RSA", "kid": p.kid, "use": "sig", "alg": "RS256",
				"n": n, "e": e,
			}},
		})
	})
	mux.HandleFunc("/authorize", func(w http.ResponseWriter, r *http.Request) {
		w.WriteHeader(http.StatusOK)
	})
	mux.HandleFunc("/token", func(w http.ResponseWriter, r *http.Request) {
		if err := r.ParseForm(); err != nil {
			w.WriteHeader(http.StatusBadRequest)
			return
		}
		// The consumed state row carries the nonce the flow stored.
		var nonce, subject string
		row := db.SQL.QueryRowContext(r.Context(),
			`SELECT nonce, COALESCE(subject,'') FROM oidc_states
			 ORDER BY created_at DESC LIMIT 1`)
		if err := row.Scan(&nonce, &subject); err != nil {
			w.WriteHeader(http.StatusInternalServerError)
			return
		}
		code := r.FormValue("code")
		// A code of the form FORGED:<nonce>:<sub> mints a token whose nonce
		// does not match the stored one — the replay/MITM probe.
		if strings.HasPrefix(code, "FORGED:") {
			parts := strings.SplitN(code, ":", 3)
			nonce, code = parts[1], parts[2]
		}
		idToken := p.signIDToken(code, nonce, stubEmail, stubEmailVerified)
		w.Header().Set("Content-Type", "application/json")
		_ = json.NewEncoder(w).Encode(map[string]any{
			"access_token": "stub-access",
			"token_type":   "Bearer",
			"expires_in":   3600,
			"id_token":     idToken,
		})
	})
	p.srv = httptest.NewServer(mux)
	t.Cleanup(p.srv.Close)
	return p
}

var stubEmail = "stub-user@example.com"
var stubEmailVerified = true

func (p *stubOIDCProvider) signIDToken(subject, nonce, email string, emailVerified bool) string {
	header, _ := json.Marshal(map[string]any{"alg": "RS256", "kid": p.kid, "typ": "JWT"})
	claims, _ := json.Marshal(map[string]any{
		"iss":            p.srv.URL,
		"sub":            subject,
		"aud":            "client-stub",
		"exp":            time.Now().Add(5 * time.Minute).Unix(),
		"iat":            time.Now().Unix(),
		"nonce":          nonce,
		"email":          email,
		"email_verified": emailVerified,
	})
	part := base64.RawURLEncoding.EncodeToString(header) + "." +
		base64.RawURLEncoding.EncodeToString(claims)
	sum := sha256.Sum256([]byte(part))
	sig, err := rsa.SignPKCS1v15(rand.Reader, p.key, crypto.SHA256, sum[:])
	if err != nil {
		panic(err)
	}
	return part + "." + base64.RawURLEncoding.EncodeToString(sig)
}

func seedOIDCProvider(t *testing.T, repo *postgres.IdentityRepo, tenantID, issuer string, enabled bool) {
	t.Helper()
	err := repo.CreateOIDCProvider(context.Background(), tenantID, auth.OIDCProvider{
		ID:              "prov_stub",
		Name:            "Stub IdP",
		Issuer:          issuer,
		ClientID:        "client-stub",
		ClientSecretRef: "env:ENCLAVA_TEST_STUB_SECRET",
		RedirectPath:    "/auth/oidc/prov_stub/callback",
		Enabled:         enabled,
	})
	if err != nil {
		t.Fatalf("seed oidc provider: %v", err)
	}
}

func oidcFixture(t *testing.T) (*postgres.DB, *postgres.IdentityRepo, *auth.OIDCService, string, *stubOIDCProvider) {
	t.Helper()
	db := testdb.New(t)
	repo := postgres.NewIdentityRepo(db)
	t.Setenv("ENCLAVA_TEST_STUB_SECRET", "stub-secret")
	svc := auth.NewOIDCService(repo, auth.EnvSecretResolver)
	stub := newStubOIDCProvider(t, db)
	tenant := testdb.SeedTenant(t, db, "tenant-"+randName())
	seedOIDCProvider(t, repo, tenant, stub.srv.URL, true)
	return db, repo, svc, tenant, stub
}

func seedPasswordUser(t *testing.T, repo *postgres.IdentityRepo, tenantID, email, password string) auth.User {
	t.Helper()
	hash, err := auth.HashPassword(password)
	if err != nil {
		t.Fatalf("hash password: %v", err)
	}
	now := time.Now().UTC()
	user := auth.User{
		ID:            "user_" + randName(),
		TenantID:      tenantID,
		Email:         email,
		DisplayName:   email,
		Status:        "active",
		PasswordHash:  hash,
		PasswordSetAt: &now,
	}
	if err := repo.CreateUser(context.Background(), tenantID, user); err != nil {
		t.Fatalf("seed user: %v", err)
	}
	return user
}

func oidcParams(t *testing.T, authURL string) url.Values {
	t.Helper()
	u, err := url.Parse(authURL)
	if err != nil {
		t.Fatalf("parse auth URL: %v", err)
	}
	return u.Query()
}

func TestOIDCRoundTripLinkedIdentity(t *testing.T) {
	_, repo, svc, tenant, stub := oidcFixture(t)
	ctx := context.Background()
	user := seedPasswordUser(t, repo, tenant, stubEmail, "correct-horse")
	if err := repo.RecordAuthIdentity(ctx, tenant, auth.AuthIdentity{
		ID: "aid_1", UserID: user.ID, Issuer: stub.srv.URL, Subject: "sub-42",
		LinkedAt: time.Now().UTC(), LinkedByUserID: user.ID,
	}); err != nil {
		t.Fatalf("record identity: %v", err)
	}

	flow, err := svc.BeginFlow(ctx, tenant, "prov_stub", "/dashboard/keys", "https://app.example.com")
	if err != nil {
		t.Fatalf("BeginFlow: %v", err)
	}
	q := oidcParams(t, flow.AuthURL)
	if q.Get("client_id") != "client-stub" || q.Get("state") != flow.State ||
		q.Get("nonce") == "" || q.Get("code_challenge") == "" {
		t.Fatalf("authorize URL missing bound params: %s", flow.AuthURL)
	}
	res, err := svc.CompleteFlow(ctx, tenant, flow.State, "sub-42", "https://app.example.com")
	if err != nil {
		t.Fatalf("CompleteFlow: %v", err)
	}
	if res.UserID != user.ID || res.ReturnTo != "/dashboard/keys" || res.LinkRequired {
		t.Fatalf("unexpected result %+v", res)
	}
}

func TestOIDCStateReplayDenied(t *testing.T) {
	_, _, svc, tenant, _ := oidcFixture(t)
	ctx := context.Background()
	flow, err := svc.BeginFlow(ctx, tenant, "prov_stub", "", "https://app.example.com")
	if err != nil {
		t.Fatalf("BeginFlow: %v", err)
	}
	if _, err := svc.CompleteFlow(ctx, tenant, flow.State, "sub-1", "https://app.example.com"); err != nil {
		t.Fatalf("first CompleteFlow: %v", err)
	}
	if _, err := svc.CompleteFlow(ctx, tenant, flow.State, "sub-1", "https://app.example.com"); err == nil {
		t.Fatal("replayed state must be denied")
	}
}

func TestOIDCExpiredStateDenied(t *testing.T) {
	db, _, svc, tenant, _ := oidcFixture(t)
	ctx := context.Background()
	flow, err := svc.BeginFlow(ctx, tenant, "prov_stub", "", "https://app.example.com")
	if err != nil {
		t.Fatalf("BeginFlow: %v", err)
	}
	if _, err := db.SQL.ExecContext(ctx,
		`UPDATE oidc_states SET expires_at = now() - interval '1 minute'`); err != nil {
		t.Fatalf("expire state: %v", err)
	}
	if _, err := svc.CompleteFlow(ctx, tenant, flow.State, "sub-1", "https://app.example.com"); err == nil {
		t.Fatal("expired state must be denied")
	}
}

func TestOIDCExternalReturnToRejected(t *testing.T) {
	_, _, svc, tenant, _ := oidcFixture(t)
	ctx := context.Background()
	for _, bad := range []string{"https://evil.example.com", "//evil.example.com/x", "javascript:alert(1)", "no-slash"} {
		if _, err := svc.BeginFlow(ctx, tenant, "prov_stub", bad, "https://app.example.com"); err == nil {
			t.Fatalf("return_to %q must be rejected", bad)
		}
	}
}

func TestOIDCUnlinkedIdentityRequiresLinkStep(t *testing.T) {
	_, repo, svc, tenant, _ := oidcFixture(t)
	ctx := context.Background()
	user := seedPasswordUser(t, repo, tenant, stubEmail, "correct-horse")

	flow, err := svc.BeginFlow(ctx, tenant, "prov_stub", "/dashboard", "https://app.example.com")
	if err != nil {
		t.Fatalf("BeginFlow: %v", err)
	}
	res, err := svc.CompleteFlow(ctx, tenant, flow.State, "sub-77", "https://app.example.com")
	if err != nil {
		t.Fatalf("CompleteFlow: %v", err)
	}
	if !res.LinkRequired || res.UserID != "" || res.LinkToken == "" || res.Email != stubEmail {
		t.Fatalf("expected link-required result, got %+v", res)
	}
	// Wrong password does not link and does not consume the challenge.
	if _, err := svc.ConfirmLink(ctx, tenant, res.LinkToken, stubEmail, "wrong-password"); err == nil {
		t.Fatal("wrong password must be denied")
	}
	// A different account's credentials can never redirect the link to it.
	other := seedPasswordUser(t, repo, tenant, "other@example.com", "other-password")
	if _, err := svc.ConfirmLink(ctx, tenant, res.LinkToken, other.Email, "other-password"); err == nil {
		t.Fatal("credentials for a non-candidate account must be denied")
	}
	linked, err := svc.ConfirmLink(ctx, tenant, res.LinkToken, stubEmail, "correct-horse")
	if err != nil {
		t.Fatalf("ConfirmLink: %v", err)
	}
	if linked.UserID != user.ID || linked.ReturnTo != "/dashboard" {
		t.Fatalf("unexpected link result %+v", linked)
	}
	if _, err := svc.ConfirmLink(ctx, tenant, res.LinkToken, stubEmail, "correct-horse"); err == nil {
		t.Fatal("link challenge replay must be denied")
	}
	// Now a fresh flow resolves the identity directly.
	flow2, err := svc.BeginFlow(ctx, tenant, "prov_stub", "", "https://app.example.com")
	if err != nil {
		t.Fatalf("BeginFlow: %v", err)
	}
	res2, err := svc.CompleteFlow(ctx, tenant, flow2.State, "sub-77", "https://app.example.com")
	if err != nil {
		t.Fatalf("CompleteFlow: %v", err)
	}
	if res2.UserID != user.ID || res2.LinkRequired {
		t.Fatalf("linked identity must resolve the user, got %+v", res2)
	}
}

func TestOIDCDisabledUserDenied(t *testing.T) {
	_, repo, svc, tenant, stub := oidcFixture(t)
	ctx := context.Background()
	user := seedPasswordUser(t, repo, tenant, stubEmail, "correct-horse")
	if err := repo.RecordAuthIdentity(ctx, tenant, auth.AuthIdentity{
		ID: "aid_2", UserID: user.ID, Issuer: stub.srv.URL, Subject: "sub-99",
		LinkedAt: time.Now().UTC(), LinkedByUserID: user.ID,
	}); err != nil {
		t.Fatalf("record identity: %v", err)
	}
	if err := repo.UpdateUserStatus(ctx, tenant, user.ID, "disabled"); err != nil {
		t.Fatalf("disable user: %v", err)
	}
	flow, err := svc.BeginFlow(ctx, tenant, "prov_stub", "", "https://app.example.com")
	if err != nil {
		t.Fatalf("BeginFlow: %v", err)
	}
	if _, err := svc.CompleteFlow(ctx, tenant, flow.State, "sub-99", "https://app.example.com"); err == nil {
		t.Fatal("disabled user must be denied")
	}
}

func TestOIDCDisabledProviderDenied(t *testing.T) {
	db := testdb.New(t)
	repo := postgres.NewIdentityRepo(db)
	svc := auth.NewOIDCService(repo, auth.EnvSecretResolver)
	stub := newStubOIDCProvider(t, db)
	tenant := testdb.SeedTenant(t, db, "tenant-"+randName())
	seedOIDCProvider(t, repo, tenant, stub.srv.URL, false)
	if _, err := svc.BeginFlow(context.Background(), tenant, "prov_stub", "", "https://app.example.com"); err == nil {
		t.Fatal("disabled provider must be denied")
	}
}

func TestOIDCNonceMismatchDenied(t *testing.T) {
	_, _, svc, tenant, _ := oidcFixture(t)
	ctx := context.Background()
	flow, err := svc.BeginFlow(ctx, tenant, "prov_stub", "", "https://app.example.com")
	if err != nil {
		t.Fatalf("BeginFlow: %v", err)
	}
	if _, err := svc.CompleteFlow(ctx, tenant, flow.State,
		"FORGED:attacker-nonce:sub-5", "https://app.example.com"); err == nil ||
		!strings.Contains(err.Error(), "nonce") {
		t.Fatalf("nonce mismatch must be denied, got %v", err)
	}
}
