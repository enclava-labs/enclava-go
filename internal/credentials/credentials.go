// Package credentials stores the connector credentials administrators add in
// the dashboard — pasted API tokens and grants obtained through a provider's
// OAuth flow — sealed with AES-256-GCM under ENCLAVA_SECRETS_KEY, and
// resolves them for connector adapters alongside the operator's
// SECRET_BINDINGS. Secret material never leaves the server: views carry only
// non-secret details.
package credentials

import (
	"context"
	"crypto/aes"
	"crypto/cipher"
	"crypto/rand"
	"encoding/base64"
	"encoding/json"
	"errors"
	"fmt"
	"net/http"
	"regexp"
	"strings"
	"time"

	"enclava-go/internal/auth"
)

// Credential kinds.
const (
	KindToken    = "token"     // an API token or key pasted by an administrator
	KindOAuth    = "oauth"     // a grant obtained through the provider's OAuth flow
	KindOAuthApp = "oauth_app" // the tenant's OAuth app (client id and secret) for a provider
)

var (
	ErrDisabled = errors.New("set ENCLAVA_SECRETS_KEY on the server to store connector credentials")
	ErrDenied   = errors.New("tenant administrator access is required")
	ErrInvalid  = errors.New("invalid credential")
	ErrConflict = errors.New("a credential with this name already exists")
	ErrNotFound = errors.New("connector credential not found")
	ErrInUse    = errors.New("the credential is used by sources")
)

// Credential is a stored record without its secret.
type Credential struct {
	TenantID  string
	ID        string
	Provider  string
	Kind      string
	Name      string
	Details   map[string]any
	CreatedBy string
	CreatedAt time.Time
	UpdatedAt time.Time
}

// OAuthState is one pending authorization, single use and bound to its user.
type OAuthState struct {
	State     string
	TenantID  string
	UserID    string
	Provider  string
	Name      string
	ExpiresAt time.Time
}

// Repository persists credentials and OAuth states.
type Repository interface {
	CreateCredential(ctx context.Context, c Credential, sealed []byte) error // ErrConflict on a taken id or name
	UpdateCredential(ctx context.Context, tenantID, id string, details map[string]any, sealed []byte) error
	GetCredential(ctx context.Context, tenantID, id string) (Credential, []byte, error) // ErrNotFound
	ListCredentials(ctx context.Context, tenantID string) ([]Credential, error)
	DeleteCredential(ctx context.Context, tenantID, id string) error
	SourcesUsing(ctx context.Context, tenantID, id string) ([]string, error)
	SaveOAuthState(ctx context.Context, s OAuthState) error
	TakeOAuthState(ctx context.Context, state string, now time.Time) (OAuthState, error) // ErrNotFound when missing or expired
}

// secret is the sealed payload.
type secret struct {
	Token        string    `json:"token,omitempty"`
	RefreshToken string    `json:"refresh_token,omitempty"`
	ExpiresAt    time.Time `json:"expires_at,omitzero"`
	ClientSecret string    `json:"client_secret,omitempty"`
}

// CredentialView is the non-secret projection the API and dashboard show.
type CredentialView struct {
	ID        string         `json:"id"`
	Provider  string         `json:"provider"`
	Kind      string         `json:"kind"`
	Name      string         `json:"name"`
	Details   map[string]any `json:"details"`
	CreatedAt time.Time      `json:"created_at"`
	UpdatedAt time.Time      `json:"updated_at"`
	UsedBy    []string       `json:"used_by"`
}

// EnvRef is a read-only credential bound in server configuration.
type EnvRef struct {
	ID       string `json:"id"`
	Provider string `json:"provider"`
}

// OAuthApp is one provider's OAuth configuration for the tenant.
type OAuthApp struct {
	Provider    string `json:"provider"`
	Supported   bool   `json:"supported"`
	Configured  bool   `json:"configured"`
	ClientID    string `json:"client_id"`
	RedirectURI string `json:"redirect_uri"`
}

// Listing is everything the Credentials tab shows.
type Listing struct {
	Enabled      bool             `json:"enabled"`
	RedirectBase string           `json:"redirect_base"`
	Credentials  []CredentialView `json:"credentials"`
	Environment  []EnvRef         `json:"environment"`
	OAuth        []OAuthApp       `json:"oauth"`
}

// Service manages and resolves connector credentials.
type Service struct {
	repo      Repository
	aead      cipher.AEAD // nil: dashboard-managed credentials are disabled
	env       *auth.SecretResolver
	baseURL   string
	client    *http.Client
	providers map[string]*provider
	now       func() time.Time
}

// NewService builds the store. A nil or empty key disables dashboard-managed
// credentials; publicBaseURL "" disables OAuth.
func NewService(repo Repository, key []byte, env *auth.SecretResolver, publicBaseURL string) (*Service, error) {
	s := &Service{repo: repo, env: env, baseURL: strings.TrimRight(publicBaseURL, "/"),
		client: &http.Client{Timeout: 20 * time.Second}, providers: defaultProviders(), now: time.Now}
	if len(key) > 0 {
		block, err := aes.NewCipher(key)
		if err != nil {
			return nil, fmt.Errorf("credentials key: %w", err)
		}
		if s.aead, err = cipher.NewGCM(block); err != nil {
			return nil, fmt.Errorf("credentials key: %w", err)
		}
	}
	return s, nil
}

// Enabled reports whether administrators can store credentials.
func (s *Service) Enabled() bool { return s.aead != nil }

func (s *Service) seal(tenantID, id string, v secret) ([]byte, error) {
	plain, err := json.Marshal(v) //nolint:gosec // Marshaled only to be sealed; never stored or logged in plaintext.
	if err != nil {
		return nil, err
	}
	nonce := make([]byte, s.aead.NonceSize())
	if _, err := rand.Read(nonce); err != nil {
		return nil, err
	}
	return s.aead.Seal(nonce, nonce, plain, []byte(tenantID+"\x00"+id)), nil
}

func (s *Service) open(tenantID, id string, sealed []byte) (secret, error) {
	var v secret
	n := s.aead.NonceSize()
	if len(sealed) < n {
		return v, errors.New("credentials: sealed value is truncated")
	}
	plain, err := s.aead.Open(nil, sealed[:n], sealed[n:], []byte(tenantID+"\x00"+id))
	if err != nil {
		return v, errors.New("credentials: sealed value does not open under this key")
	}
	return v, json.Unmarshal(plain, &v)
}

func requireAdmin(p auth.Principal) error {
	if p.TenantID == "" || !auth.HasScope(p, auth.ScopePlatformAdmin) || !auth.HasScope(p, "connectors.manage") {
		return ErrDenied
	}
	return nil
}

func (s *Service) provider(id string) (*provider, error) {
	if pv := s.providers[id]; pv != nil {
		return pv, nil
	}
	return nil, fmt.Errorf("%w: provider must be one of github, slack, notion, teams, google_drive, gitlab, jira, linear", ErrInvalid)
}

var slugPattern = regexp.MustCompile(`[^A-Z0-9]+`)

// credentialID derives the credential_ref sources use from the name, e.g.
// CONNECTOR_SLACK_ENCLAVA_LABS.
func credentialID(pv *provider, name string) string {
	slug := strings.Trim(slugPattern.ReplaceAllString(strings.ToUpper(name), "_"), "_")
	if len(slug) > 40 {
		slug = strings.TrimRight(slug[:40], "_")
	}
	if slug == "" {
		slug = "DEFAULT"
	}
	return "CONNECTOR_" + pv.refPrefix + "_" + slug
}

func cleanName(name string) (string, error) {
	name = strings.TrimSpace(name)
	if name == "" || len(name) > 80 {
		return "", fmt.Errorf("%w: name must be 1-80 characters", ErrInvalid)
	}
	return name, nil
}

func (s *Service) redirectURI(providerID string) string {
	if s.baseURL == "" {
		return ""
	}
	return s.baseURL + "/dashboard/connectors/oauth/callback/" + providerID
}

func oauthAppID(pv *provider) string { return "OAUTH_APP_" + pv.refPrefix }

// List returns the tenant's credentials, server-bound refs and OAuth apps.
func (s *Service) List(ctx context.Context, p auth.Principal) (Listing, error) {
	if err := requireAdmin(p); err != nil {
		return Listing{}, err
	}
	out := Listing{Enabled: s.Enabled(), RedirectBase: s.baseURL, Credentials: []CredentialView{}, Environment: []EnvRef{}}
	apps := map[string]Credential{}
	if s.Enabled() {
		rows, err := s.repo.ListCredentials(ctx, p.TenantID)
		if err != nil {
			return Listing{}, err
		}
		for _, c := range rows {
			if c.Kind == KindOAuthApp {
				apps[c.Provider] = c
				continue
			}
			used, err := s.repo.SourcesUsing(ctx, p.TenantID, c.ID)
			if err != nil {
				return Listing{}, err
			}
			out.Credentials = append(out.Credentials, view(c, used))
		}
	}
	for _, ref := range s.env.Metadata(p.TenantID) {
		if ref.Purpose != auth.SecretPurposeConnector {
			continue
		}
		for _, pv := range orderedProviders(s.providers) {
			if strings.HasPrefix(ref.RefID, "CONNECTOR_"+pv.refPrefix+"_") {
				out.Environment = append(out.Environment, EnvRef{ID: ref.RefID, Provider: pv.id})
				break
			}
		}
	}
	for _, pv := range orderedProviders(s.providers) {
		app := OAuthApp{Provider: pv.id, Supported: pv.oauth != nil}
		if app.Supported {
			app.RedirectURI = s.redirectURI(pv.id)
			if c, ok := apps[pv.id]; ok {
				app.Configured = true
				app.ClientID, _ = c.Details["client_id"].(string)
			}
		}
		out.OAuth = append(out.OAuth, app)
	}
	return out, nil
}

func view(c Credential, used []string) CredentialView {
	if used == nil {
		used = []string{}
	}
	details := c.Details
	if details == nil {
		details = map[string]any{}
	}
	return CredentialView{ID: c.ID, Provider: c.Provider, Kind: c.Kind, Name: c.Name, Details: details, CreatedAt: c.CreatedAt, UpdatedAt: c.UpdatedAt, UsedBy: used}
}

// Metadata lists the credential refs a tenant's sources may use: the
// operator's SECRET_BINDINGS plus dashboard-managed credentials (named).
func (s *Service) Metadata(tenantID string) []auth.SecretRefMeta {
	out := s.env.Metadata(tenantID) // nil-safe
	if !s.Enabled() {
		return out
	}
	ctx, cancel := context.WithTimeout(context.Background(), 5*time.Second)
	defer cancel()
	rows, err := s.repo.ListCredentials(ctx, tenantID)
	if err != nil {
		return out
	}
	for _, c := range rows {
		if c.Kind != KindOAuthApp {
			out = append(out, auth.SecretRefMeta{RefID: c.ID, TenantID: tenantID, Purpose: auth.SecretPurposeConnector, Name: c.Name})
		}
	}
	return out
}

// CreateToken stores a pasted API token after the provider accepts it.
// origin optionally overrides the provider's API origin (for example GitHub
// Enterprise); the token is only ever released to that origin.
func (s *Service) CreateToken(ctx context.Context, p auth.Principal, providerID, name, token, origin string) (CredentialView, error) {
	if err := requireAdmin(p); err != nil {
		return CredentialView{}, err
	}
	if !s.Enabled() {
		return CredentialView{}, ErrDisabled
	}
	pv, err := s.provider(providerID)
	if err != nil {
		return CredentialView{}, err
	}
	if name, err = cleanName(name); err != nil {
		return CredentialView{}, err
	}
	token = strings.TrimSpace(token)
	if token == "" || len(token) > 8192 {
		return CredentialView{}, fmt.Errorf("%w: the token or key is required (at most 8192 characters)", ErrInvalid)
	}
	details := map[string]any{}
	if origin = strings.TrimSpace(origin); origin != "" {
		canonical, err := auth.CanonicalizeSecretOrigin(origin)
		if err != nil || !strings.HasPrefix(canonical, "https://") {
			return CredentialView{}, fmt.Errorf("%w: API origin must look like https://api.example.com", ErrInvalid)
		}
		details["origin"] = canonical
	}
	if apiOrigin(pv, details) == "" {
		return CredentialView{}, fmt.Errorf("%w: %s credentials need their site URL as the API origin, e.g. https://acme.atlassian.net", ErrInvalid, pv.label)
	}
	if account, err := pv.probeToken(ctx, s.client, apiOrigin(pv, details), token); err != nil {
		return CredentialView{}, fmt.Errorf("%w: %s rejected the token: %v", ErrInvalid, pv.label, err)
	} else if account != "" {
		details["account"] = account
	}
	c := Credential{TenantID: p.TenantID, ID: credentialID(pv, name), Provider: pv.id, Kind: KindToken, Name: name, Details: details, CreatedBy: p.ActorID}
	if err := s.envCollision(p.TenantID, c.ID); err != nil {
		return CredentialView{}, err
	}
	sealed, err := s.seal(c.TenantID, c.ID, secret{Token: token})
	if err != nil {
		return CredentialView{}, err
	}
	if err := s.repo.CreateCredential(ctx, c, sealed); err != nil {
		return CredentialView{}, err
	}
	return s.viewByID(ctx, p.TenantID, c.ID)
}

func (s *Service) envCollision(tenantID, id string) error {
	for _, ref := range s.env.Metadata(tenantID) {
		if ref.RefID == id {
			return fmt.Errorf("%w: %s is already bound in server configuration", ErrConflict, id)
		}
	}
	return nil
}

func (s *Service) viewByID(ctx context.Context, tenantID, id string) (CredentialView, error) {
	c, _, err := s.repo.GetCredential(ctx, tenantID, id)
	if err != nil {
		return CredentialView{}, err
	}
	used, err := s.repo.SourcesUsing(ctx, tenantID, id)
	if err != nil {
		return CredentialView{}, err
	}
	return view(c, used), nil
}

// Rotate replaces a pasted token after the provider accepts the new one.
func (s *Service) Rotate(ctx context.Context, p auth.Principal, id, token string) (CredentialView, error) {
	if err := requireAdmin(p); err != nil {
		return CredentialView{}, err
	}
	if !s.Enabled() {
		return CredentialView{}, ErrDisabled
	}
	c, _, err := s.repo.GetCredential(ctx, p.TenantID, id)
	if err != nil {
		return CredentialView{}, err
	}
	if c.Kind != KindToken {
		return CredentialView{}, fmt.Errorf("%w: reconnect OAuth credentials instead of rotating them", ErrInvalid)
	}
	pv, err := s.provider(c.Provider)
	if err != nil {
		return CredentialView{}, err
	}
	token = strings.TrimSpace(token)
	if token == "" || len(token) > 8192 {
		return CredentialView{}, fmt.Errorf("%w: the token or key is required (at most 8192 characters)", ErrInvalid)
	}
	details := cloneDetails(c.Details)
	if account, err := pv.probeToken(ctx, s.client, apiOrigin(pv, details), token); err != nil {
		return CredentialView{}, fmt.Errorf("%w: %s rejected the token: %v", ErrInvalid, pv.label, err)
	} else if account != "" {
		details["account"] = account
	}
	sealed, err := s.seal(c.TenantID, c.ID, secret{Token: token})
	if err != nil {
		return CredentialView{}, err
	}
	if err := s.repo.UpdateCredential(ctx, c.TenantID, c.ID, details, sealed); err != nil {
		return CredentialView{}, err
	}
	return s.viewByID(ctx, c.TenantID, c.ID)
}

// Delete removes a credential no source uses.
func (s *Service) Delete(ctx context.Context, p auth.Principal, id string) ([]string, error) {
	if err := requireAdmin(p); err != nil {
		return nil, err
	}
	if !s.Enabled() {
		return nil, ErrDisabled
	}
	c, _, err := s.repo.GetCredential(ctx, p.TenantID, id)
	if err != nil {
		return nil, err
	}
	if c.Kind == KindOAuthApp {
		return nil, ErrNotFound
	}
	used, err := s.repo.SourcesUsing(ctx, p.TenantID, id)
	if err != nil {
		return nil, err
	}
	if len(used) > 0 {
		return used, ErrInUse
	}
	return nil, s.repo.DeleteCredential(ctx, p.TenantID, id)
}

// SetOAuthApp stores the tenant's OAuth app for a provider.
func (s *Service) SetOAuthApp(ctx context.Context, p auth.Principal, providerID, clientID, clientSecret string) (OAuthApp, error) {
	if err := requireAdmin(p); err != nil {
		return OAuthApp{}, err
	}
	if !s.Enabled() {
		return OAuthApp{}, ErrDisabled
	}
	pv, err := s.provider(providerID)
	if err != nil {
		return OAuthApp{}, err
	}
	if pv.oauth == nil {
		return OAuthApp{}, fmt.Errorf("%w: %s connects with an app client secret, not OAuth", ErrInvalid, pv.label)
	}
	clientID, clientSecret = strings.TrimSpace(clientID), strings.TrimSpace(clientSecret)
	if clientID == "" || clientSecret == "" || len(clientID) > 512 || len(clientSecret) > 4096 {
		return OAuthApp{}, fmt.Errorf("%w: client ID and client secret are required", ErrInvalid)
	}
	c := Credential{TenantID: p.TenantID, ID: oauthAppID(pv), Provider: pv.id, Kind: KindOAuthApp, Name: pv.label + " OAuth app",
		Details: map[string]any{"client_id": clientID}, CreatedBy: p.ActorID}
	sealed, err := s.seal(c.TenantID, c.ID, secret{ClientSecret: clientSecret})
	if err != nil {
		return OAuthApp{}, err
	}
	if _, _, err := s.repo.GetCredential(ctx, c.TenantID, c.ID); errors.Is(err, ErrNotFound) {
		err = s.repo.CreateCredential(ctx, c, sealed)
		if err != nil {
			return OAuthApp{}, err
		}
	} else if err != nil {
		return OAuthApp{}, err
	} else if err := s.repo.UpdateCredential(ctx, c.TenantID, c.ID, c.Details, sealed); err != nil {
		return OAuthApp{}, err
	}
	return OAuthApp{Provider: pv.id, Supported: true, Configured: true, ClientID: clientID, RedirectURI: s.redirectURI(pv.id)}, nil
}

// DeleteOAuthApp removes a provider's OAuth app; existing OAuth credentials
// keep working until their tokens need a refresh.
func (s *Service) DeleteOAuthApp(ctx context.Context, p auth.Principal, providerID string) error {
	if err := requireAdmin(p); err != nil {
		return err
	}
	if !s.Enabled() {
		return ErrDisabled
	}
	pv, err := s.provider(providerID)
	if err != nil {
		return err
	}
	return s.repo.DeleteCredential(ctx, p.TenantID, oauthAppID(pv))
}

func (s *Service) oauthApp(ctx context.Context, tenantID string, pv *provider) (clientID, clientSecret string, err error) {
	c, sealed, err := s.repo.GetCredential(ctx, tenantID, oauthAppID(pv))
	if errors.Is(err, ErrNotFound) {
		return "", "", fmt.Errorf("%w: configure the %s OAuth app first", ErrInvalid, pv.label)
	}
	if err != nil {
		return "", "", err
	}
	v, err := s.open(tenantID, c.ID, sealed)
	if err != nil {
		return "", "", err
	}
	clientID, _ = c.Details["client_id"].(string)
	return clientID, v.ClientSecret, nil
}

// StartOAuth records a single-use state and returns the provider's
// authorization URL.
func (s *Service) StartOAuth(ctx context.Context, p auth.Principal, providerID, name string) (string, error) {
	if err := requireAdmin(p); err != nil {
		return "", err
	}
	if !s.Enabled() {
		return "", ErrDisabled
	}
	if s.baseURL == "" {
		return "", fmt.Errorf("%w: set PUBLIC_BASE_URL on the server to use OAuth", ErrInvalid)
	}
	pv, err := s.provider(providerID)
	if err != nil {
		return "", err
	}
	if pv.oauth == nil {
		return "", fmt.Errorf("%w: %s connects with an app client secret, not OAuth", ErrInvalid, pv.label)
	}
	if name, err = cleanName(name); err != nil {
		return "", err
	}
	clientID, _, err := s.oauthApp(ctx, p.TenantID, pv)
	if err != nil {
		return "", err
	}
	raw := make([]byte, 32)
	if _, err := rand.Read(raw); err != nil {
		return "", err
	}
	state := base64.RawURLEncoding.EncodeToString(raw)
	if err := s.repo.SaveOAuthState(ctx, OAuthState{State: state, TenantID: p.TenantID, UserID: p.ActorID, Provider: pv.id, Name: name, ExpiresAt: s.now().Add(10 * time.Minute)}); err != nil {
		return "", err
	}
	return pv.oauth.authorizeURL(clientID, s.redirectURI(pv.id), state), nil
}

// CompleteOAuth consumes the state, exchanges the code and stores the grant.
// Reconnecting under an existing OAuth credential's name replaces its tokens.
func (s *Service) CompleteOAuth(ctx context.Context, p auth.Principal, providerID, state, code string) (CredentialView, error) {
	if err := requireAdmin(p); err != nil {
		return CredentialView{}, err
	}
	if !s.Enabled() {
		return CredentialView{}, ErrDisabled
	}
	pv, err := s.provider(providerID)
	if err != nil || pv.oauth == nil {
		return CredentialView{}, fmt.Errorf("%w: unknown OAuth provider", ErrInvalid)
	}
	pending, err := s.repo.TakeOAuthState(ctx, state, s.now())
	if err != nil || pending.TenantID != p.TenantID || pending.UserID != p.ActorID || pending.Provider != pv.id {
		return CredentialView{}, fmt.Errorf("%w: the authorization expired or was started by someone else; start again", ErrInvalid)
	}
	if strings.TrimSpace(code) == "" {
		return CredentialView{}, fmt.Errorf("%w: %s did not return an authorization code", ErrInvalid, pv.label)
	}
	clientID, clientSecret, err := s.oauthApp(ctx, p.TenantID, pv)
	if err != nil {
		return CredentialView{}, err
	}
	grant, err := pv.oauth.exchange(ctx, s.client, clientID, clientSecret, s.redirectURI(pv.id), code, s.now())
	if err != nil {
		return CredentialView{}, fmt.Errorf("%w: %s refused the authorization: %v", ErrInvalid, pv.label, err)
	}
	details := map[string]any{}
	if grant.account != "" {
		details["account"] = grant.account
	}
	if grant.scope != "" {
		details["scopes"] = grant.scope
	}
	if !grant.secret.ExpiresAt.IsZero() {
		details["expires_at"] = grant.secret.ExpiresAt.UTC().Format(time.RFC3339)
	}
	if details["account"] == nil {
		if account, err := pv.probeToken(ctx, s.client, apiOrigin(pv, details), grant.secret.Token); err == nil && account != "" {
			details["account"] = account
		}
	}
	c := Credential{TenantID: p.TenantID, ID: credentialID(pv, pending.Name), Provider: pv.id, Kind: KindOAuth, Name: pending.Name, Details: details, CreatedBy: p.ActorID}
	if err := s.envCollision(p.TenantID, c.ID); err != nil {
		return CredentialView{}, err
	}
	sealed, err := s.seal(c.TenantID, c.ID, grant.secret)
	if err != nil {
		return CredentialView{}, err
	}
	if existing, _, err := s.repo.GetCredential(ctx, c.TenantID, c.ID); err == nil {
		if existing.Kind != KindOAuth {
			return CredentialView{}, fmt.Errorf("%w: a token credential is already named %q", ErrConflict, pending.Name)
		}
		if err := s.repo.UpdateCredential(ctx, c.TenantID, c.ID, details, sealed); err != nil {
			return CredentialView{}, err
		}
	} else if !errors.Is(err, ErrNotFound) {
		return CredentialView{}, err
	} else if err := s.repo.CreateCredential(ctx, c, sealed); err != nil {
		return CredentialView{}, err
	}
	return s.viewByID(ctx, c.TenantID, c.ID)
}

// Resolve releases a connector credential: a dashboard-managed credential of
// this tenant when refID names one — only to its provider's API origin, and
// refreshed first when its OAuth access token is about to expire — otherwise
// the operator's SECRET_BINDINGS decide.
func (s *Service) Resolve(tenantID, purpose, origin, refID string) (string, error) {
	if s.Enabled() && purpose == auth.SecretPurposeConnector && strings.HasPrefix(refID, "CONNECTOR_") {
		ctx, cancel := context.WithTimeout(context.Background(), 20*time.Second)
		defer cancel()
		c, sealed, err := s.repo.GetCredential(ctx, tenantID, refID)
		if err == nil {
			return s.release(ctx, c, sealed, origin)
		}
		if !errors.Is(err, ErrNotFound) {
			return "", err
		}
	}
	return s.env.Resolve(tenantID, purpose, origin, refID) // nil-safe: denies
}

func (s *Service) release(ctx context.Context, c Credential, sealed []byte, origin string) (string, error) {
	pv := s.providers[c.Provider]
	if pv == nil || c.Kind == KindOAuthApp {
		return "", auth.ErrSecretDenied
	}
	canonical, err := auth.CanonicalizeSecretOrigin(origin)
	if err != nil || !allowedOrigin(pv, c.Details, canonical) {
		return "", auth.ErrSecretDenied
	}
	v, err := s.open(c.TenantID, c.ID, sealed)
	if err != nil {
		return "", auth.ErrSecretDenied
	}
	if c.Kind == KindOAuth && v.RefreshToken != "" && !v.ExpiresAt.IsZero() && s.now().Add(2*time.Minute).After(v.ExpiresAt) {
		if v, err = s.refresh(ctx, c, pv, v); err != nil {
			return "", fmt.Errorf("refresh %s credential %s: %w", pv.label, c.ID, err)
		}
	}
	if v.Token == "" {
		return "", auth.ErrSecretDenied
	}
	return v.Token, nil
}

func (s *Service) refresh(ctx context.Context, c Credential, pv *provider, v secret) (secret, error) {
	if pv.oauth == nil {
		return v, errors.New("provider has no OAuth refresh")
	}
	clientID, clientSecret, err := s.oauthApp(ctx, c.TenantID, pv)
	if err != nil {
		return v, err
	}
	next, err := pv.oauth.refresh(ctx, s.client, clientID, clientSecret, v.RefreshToken, s.now())
	if err != nil {
		return v, err
	}
	if next.RefreshToken == "" {
		next.RefreshToken = v.RefreshToken
	}
	sealed, err := s.seal(c.TenantID, c.ID, next)
	if err != nil {
		return v, err
	}
	details := cloneDetails(c.Details)
	if !next.ExpiresAt.IsZero() {
		details["expires_at"] = next.ExpiresAt.UTC().Format(time.RFC3339)
	}
	if err := s.repo.UpdateCredential(ctx, c.TenantID, c.ID, details, sealed); err != nil {
		return v, err
	}
	return next, nil
}

func cloneDetails(in map[string]any) map[string]any {
	out := make(map[string]any, len(in)+1)
	for k, v := range in {
		out[k] = v
	}
	return out
}
