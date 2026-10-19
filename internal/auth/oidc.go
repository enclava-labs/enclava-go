// oidc.go implements the OIDC authorization-code flow (AUTH-02) on top of
// the stored oidc_providers and oidc_states rows: BeginFlow binds
// state+nonce+PKCE to a tenant/provider and returns the authorize URL;
// CompleteFlow consumes the state exactly once, verifies the exchanged ID
// token (signature, issuer, audience, expiry, nonce) via the provider's
// discovery document and resolves the verified issuer+subject through the
// identity-linking rules in linking.go. No tokens are persisted — sessions
// are the only retained credential.
package auth

import (
	"context"
	"crypto/rand"
	"crypto/sha256"
	"encoding/base64"
	"encoding/hex"
	"errors"
	"fmt"
	"net/http"
	"net/url"
	"os"
	"strings"
	"time"

	"github.com/coreos/go-oidc/v3/oidc"
	"golang.org/x/oauth2"
)

// oidcStateTTL bounds a begin-flow or pending-link state row.
const oidcStateTTL = 10 * time.Minute

// ClientSecretResolver turns a stored client_secret_ref into the secret
// value. The only supported form is "env:<NAME>" — an operator-owned
// environment-backed binding; every other reference fails closed. Distinct
// from SecretResolver (secret_refs.go), the tenant/purpose/origin-bound
// resolver used by inference, connectors and MCP.
type ClientSecretResolver func(ref string) (string, error)

// EnvSecretResolver resolves "env:<NAME>" references from the process
// environment. Refs without the env: prefix are rejected so a stored value
// can never name an arbitrary lookup.
func EnvSecretResolver(ref string) (string, error) {
	if !strings.HasPrefix(ref, "env:") {
		return "", fmt.Errorf("unsupported client_secret_ref %q: want env:<NAME>", ref)
	}
	name := strings.TrimSpace(strings.TrimPrefix(ref, "env:"))
	if name == "" {
		return "", fmt.Errorf("unsupported client_secret_ref %q: want env:<NAME>", ref)
	}
	secret, ok := os.LookupEnv(name)
	if !ok || secret == "" {
		return "", fmt.Errorf("client secret env %s is not set", name)
	}
	return secret, nil
}

// OIDCService owns the provider login flows. It is separate from Service so
// the HTTP layer can mount local-login without OIDC when no provider rows
// are enabled.
type OIDCService struct {
	repo      IdentityRepository
	resolve   ClientSecretResolver
	transport *http.Client
}

// NewOIDCService wires the flow service; EnvSecretResolver is the default
// secret binding when resolve is nil.
func NewOIDCService(repo IdentityRepository, resolve ClientSecretResolver) *OIDCService {
	if resolve == nil {
		resolve = EnvSecretResolver
	}
	return &OIDCService{repo: repo, resolve: resolve, transport: http.DefaultClient}
}

// SetHTTPClient overrides the client used for discovery/JWKS/token calls —
// tests point it at an httptest server; production keeps the default.
func (o *OIDCService) SetHTTPClient(c *http.Client) {
	if c != nil {
		o.transport = c
	}
}

// Flow is the begin-flow result: the provider authorize URL the browser is
// redirected to and the plaintext state bound to it.
type Flow struct {
	AuthURL string
	State   string
}

// BeginFlow validates the return_to allowlist (relative paths only), stores
// a hashed state row with nonce + PKCE verifier and returns the provider's
// authorize URL. redirectBase is the deployment's external origin (scheme +
// host); the full callback URI is redirectBase + provider.RedirectPath.
func (o *OIDCService) BeginFlow(ctx context.Context, tenantID, providerID, returnTo, redirectBase string) (Flow, error) {
	if !safeReturnPath(returnTo) {
		return Flow{}, errors.New("return_to must be a local path")
	}
	base, err := url.Parse(strings.TrimSpace(redirectBase))
	if err != nil || base.Scheme == "" || base.Host == "" {
		return Flow{}, errors.New("redirect base must be an absolute origin")
	}
	p, conf, err := o.flowClient(ctx, tenantID, providerID, base.String())
	if err != nil {
		return Flow{}, err
	}

	state, err := randomToken()
	if err != nil {
		return Flow{}, err
	}
	nonce, err := randomToken()
	if err != nil {
		return Flow{}, err
	}
	verifier := oauth2.GenerateVerifier()
	row := OIDCState{
		ID:           "ostate_" + randomHex(12),
		StateHash:    oidcStateHash(state),
		ProviderID:   p.ID,
		Nonce:        nonce,
		PKCEVerifier: verifier,
		ReturnTo:     returnTo,
		ExpiresAt:    time.Now().UTC().Add(oidcStateTTL),
	}
	if err := o.repo.CreateOIDCState(ctx, tenantID, row); err != nil {
		return Flow{}, err
	}
	return Flow{
		AuthURL: conf.AuthCodeURL(state,
			oidc.Nonce(nonce),
			oauth2.S256ChallengeOption(verifier)),
		State: state,
	}, nil
}

// FlowResult is the completed-callback outcome. A zero UserID means the
// verified external identity is not yet bound to a user: LinkRequired is
// set and LinkToken is a one-shot challenge token redeemed by ConfirmLink
// after the browser proves the local account.
type FlowResult struct {
	UserID       string
	ReturnTo     string
	LinkRequired bool
	LinkToken    string
	ProviderName string
	Email        string
}

// CompleteFlow consumes the state row exactly once (replay is denied),
// exchanges the code with PKCE and verifies the ID token's signature and
// claims — including the stored nonce — against the provider's discovery
// document, then resolves issuer+subject through the linking rules.
func (o *OIDCService) CompleteFlow(ctx context.Context, tenantID, state, code, redirectBase string) (FlowResult, error) {
	row, err := o.repo.ConsumeOIDCState(ctx, tenantID, oidcStateHash(state))
	if err != nil {
		if errors.Is(err, ErrNotFound) {
			return FlowResult{}, errors.New("invalid or replayed OIDC state")
		}
		return FlowResult{}, err
	}
	if row.PendingLinkUserID != "" || row.PKCEVerifier == "" {
		// A link-challenge row is not a flow state; only ConfirmLink consumes it.
		return FlowResult{}, errors.New("invalid OIDC state kind")
	}
	if time.Now().UTC().After(row.ExpiresAt) {
		return FlowResult{}, errors.New("OIDC state expired")
	}
	base, err := url.Parse(strings.TrimSpace(redirectBase))
	if err != nil || base.Scheme == "" || base.Host == "" {
		return FlowResult{}, errors.New("redirect base must be an absolute origin")
	}
	p, conf, verifier, err := o.flowClientFull(ctx, tenantID, row.ProviderID, base.String())
	if err != nil {
		return FlowResult{}, err
	}
	token, err := conf.Exchange(oidcHTTPContext(ctx, o.transport), code,
		oauth2.VerifierOption(row.PKCEVerifier))
	if err != nil {
		return FlowResult{}, fmt.Errorf("code exchange: %w", err)
	}
	rawID, ok := token.Extra("id_token").(string)
	if !ok || rawID == "" {
		return FlowResult{}, errors.New("provider did not return an id_token")
	}
	idToken, err := verifier.Verify(oidcHTTPContext(ctx, o.transport), rawID)
	if err != nil {
		return FlowResult{}, fmt.Errorf("id_token verification: %w", err)
	}
	if idToken.Nonce != row.Nonce {
		return FlowResult{}, errors.New("OIDC nonce mismatch")
	}
	var claims struct {
		Email         string `json:"email"`
		EmailVerified bool   `json:"email_verified"`
	}
	if err := idToken.Claims(&claims); err != nil {
		return FlowResult{}, fmt.Errorf("id_token claims: %w", err)
	}
	return o.resolveIdentity(ctx, tenantID, p, idToken.Subject,
		strings.ToLower(strings.TrimSpace(claims.Email)), claims.EmailVerified, row.ReturnTo)
}

// EnabledProviders lists the tenant's enabled provider rows for the login
// page — disabled providers never render a link.
func (o *OIDCService) EnabledProviders(ctx context.Context, tenantID string) ([]OIDCProvider, error) {
	providers, err := o.repo.ListOIDCProviders(ctx, tenantID)
	if err != nil {
		return nil, err
	}
	out := make([]OIDCProvider, 0, len(providers))
	for _, p := range providers {
		if p.Enabled {
			out = append(out, p)
		}
	}
	return out, nil
}

// flowClient loads the stored provider row (disabled denies), runs
// discovery and resolves the client secret. BeginFlow needs only the
// authorize endpoint; CompleteFlow also needs the verifier.
func (o *OIDCService) flowClient(ctx context.Context, tenantID, providerID, redirectBase string) (OIDCProvider, oauth2.Config, error) {
	p, conf, _, err := o.flowClientFull(ctx, tenantID, providerID, redirectBase)
	return p, conf, err
}

func (o *OIDCService) flowClientFull(ctx context.Context, tenantID, providerID, redirectBase string) (OIDCProvider, oauth2.Config, *oidc.IDTokenVerifier, error) {
	p, err := o.repo.GetOIDCProvider(ctx, tenantID, providerID)
	if err != nil {
		return OIDCProvider{}, oauth2.Config{}, nil, fmt.Errorf("resolve OIDC provider: %w", err)
	}
	if !p.Enabled {
		return OIDCProvider{}, oauth2.Config{}, nil, errors.New("OIDC provider is disabled")
	}
	discovered, err := oidc.NewProvider(oidcHTTPContext(ctx, o.transport), p.Issuer)
	if err != nil {
		return OIDCProvider{}, oauth2.Config{}, nil, fmt.Errorf("provider discovery: %w", err)
	}
	secret, err := o.resolve(p.ClientSecretRef)
	if err != nil {
		return OIDCProvider{}, oauth2.Config{}, nil, err
	}
	conf := oauth2.Config{
		ClientID:     p.ClientID,
		ClientSecret: secret,
		Endpoint:     discovered.Endpoint(),
		RedirectURL:  redirectBase + p.RedirectPath,
		Scopes:       []string{oidc.ScopeOpenID, "email"},
	}
	return p, conf, discovered.Verifier(&oidc.Config{ClientID: p.ClientID}), nil
}

func oidcHTTPContext(ctx context.Context, c *http.Client) context.Context {
	return context.WithValue(ctx, oauth2.HTTPClient, c)
}

// safeReturnPath allowlists local redirect targets: an absolute path with no
// scheme, host or protocol-relative prefix — anything else denies.
func safeReturnPath(returnTo string) bool {
	if returnTo == "" {
		return true
	}
	if strings.HasPrefix(returnTo, "//") || strings.ContainsAny(returnTo, "\r\n") {
		return false
	}
	u, err := url.Parse(returnTo)
	if err != nil {
		return false
	}
	return !u.IsAbs() && u.Host == "" && strings.HasPrefix(returnTo, "/")
}

func randomToken() (string, error) {
	raw := make([]byte, 32)
	if _, err := rand.Read(raw); err != nil {
		return "", err
	}
	return base64.RawURLEncoding.EncodeToString(raw), nil
}

// oidcStateHash is sha256 of the plaintext state — the DB never stores the
// token an attacker could replay from a leaked row.
func oidcStateHash(state string) string {
	sum := sha256.Sum256([]byte(state))
	return hex.EncodeToString(sum[:])
}
