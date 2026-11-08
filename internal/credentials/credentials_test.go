package credentials

import (
	"bytes"
	"context"
	"encoding/base64"
	"encoding/json"
	"errors"
	"io"
	"net/http"
	"net/http/httptest"
	"net/url"
	"strconv"
	"strings"
	"sync"
	"testing"
	"time"

	"enclava-go/internal/auth"
)

type memRepo struct {
	mu     sync.Mutex
	creds  map[string]Credential
	sealed map[string][]byte
	states map[string]OAuthState
	using  map[string][]string
}

func newMemRepo() *memRepo {
	return &memRepo{creds: map[string]Credential{}, sealed: map[string][]byte{}, states: map[string]OAuthState{}, using: map[string][]string{}}
}

func (m *memRepo) CreateCredential(_ context.Context, c Credential, sealed []byte) error {
	m.mu.Lock()
	defer m.mu.Unlock()
	if _, ok := m.creds[c.TenantID+"/"+c.ID]; ok {
		return ErrConflict
	}
	for _, other := range m.creds {
		if other.TenantID == c.TenantID && other.Provider == c.Provider && other.Kind != KindOAuthApp && c.Kind != KindOAuthApp && strings.EqualFold(other.Name, c.Name) {
			return ErrConflict
		}
	}
	m.creds[c.TenantID+"/"+c.ID], m.sealed[c.TenantID+"/"+c.ID] = c, sealed
	return nil
}

func (m *memRepo) UpdateCredential(_ context.Context, tenantID, id string, details map[string]any, sealed []byte) error {
	m.mu.Lock()
	defer m.mu.Unlock()
	c, ok := m.creds[tenantID+"/"+id]
	if !ok {
		return ErrNotFound
	}
	c.Details = details
	m.creds[tenantID+"/"+id], m.sealed[tenantID+"/"+id] = c, sealed
	return nil
}

func (m *memRepo) GetCredential(_ context.Context, tenantID, id string) (Credential, []byte, error) {
	m.mu.Lock()
	defer m.mu.Unlock()
	c, ok := m.creds[tenantID+"/"+id]
	if !ok {
		return Credential{}, nil, ErrNotFound
	}
	return c, m.sealed[tenantID+"/"+id], nil
}

func (m *memRepo) ListCredentials(_ context.Context, tenantID string) ([]Credential, error) {
	m.mu.Lock()
	defer m.mu.Unlock()
	var out []Credential
	for _, c := range m.creds {
		if c.TenantID == tenantID {
			out = append(out, c)
		}
	}
	return out, nil
}

func (m *memRepo) DeleteCredential(_ context.Context, tenantID, id string) error {
	m.mu.Lock()
	defer m.mu.Unlock()
	if _, ok := m.creds[tenantID+"/"+id]; !ok {
		return ErrNotFound
	}
	delete(m.creds, tenantID+"/"+id)
	return nil
}

func (m *memRepo) SourcesUsing(_ context.Context, _ string, id string) ([]string, error) {
	return m.using[id], nil
}

func (m *memRepo) SaveOAuthState(_ context.Context, s OAuthState) error {
	m.mu.Lock()
	defer m.mu.Unlock()
	m.states[s.State] = s
	return nil
}

func (m *memRepo) TakeOAuthState(_ context.Context, state string, now time.Time) (OAuthState, error) {
	m.mu.Lock()
	defer m.mu.Unlock()
	s, ok := m.states[state]
	delete(m.states, state)
	if !ok || !now.Before(s.ExpiresAt) {
		return OAuthState{}, ErrNotFound
	}
	return s, nil
}

const tenant = "11111111-1111-1111-1111-111111111111"

func admin(actor string) auth.Principal {
	return auth.Principal{TenantID: tenant, ActorID: actor, Scopes: map[string]struct{}{auth.ScopePlatformAdmin: {}, "connectors.manage": {}}}
}

func newTestService(t *testing.T, repo Repository, env *auth.SecretResolver, client *http.Client) *Service {
	t.Helper()
	svc, err := NewService(repo, bytes.Repeat([]byte{7}, 32), env, "https://enclava.test/")
	if err != nil {
		t.Fatal(err)
	}
	svc.client = client
	return svc
}

func TestTokenCredentialSealedAndOriginBound(t *testing.T) {
	slack := httptest.NewTLSServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		if r.URL.Path != "/api/auth.test" {
			http.NotFound(w, r)
			return
		}
		if r.Header.Get("Authorization") == "Bearer xoxb-good" {
			_, _ = io.WriteString(w, `{"ok":true,"team":"Enclava Labs"}`)
			return
		}
		_, _ = io.WriteString(w, `{"ok":false,"error":"invalid_auth"}`)
	}))
	defer slack.Close()
	t.Setenv("ENV_SLACK_TOKEN", "xoxb-env")
	env, err := auth.NewSecretResolver([]auth.SecretBinding{{RefID: "CONNECTOR_SLACK_ENV", TenantID: tenant, Purpose: auth.SecretPurposeConnector, EnvName: "ENV_SLACK_TOKEN", Origins: []string{"https://slack.com"}}})
	if err != nil {
		t.Fatal(err)
	}
	repo := newMemRepo()
	svc := newTestService(t, repo, env, slack.Client())
	ctx := context.Background()

	if _, err := svc.CreateToken(ctx, admin("u1"), "slack", "Enclava Labs", "xoxb-bad", slack.URL); !errors.Is(err, ErrInvalid) || !strings.Contains(err.Error(), "invalid_auth") {
		t.Fatalf("rejected token: %v", err)
	}
	if _, err := svc.CreateToken(ctx, admin("u1"), "slack", "Insecure", "xoxb-good", "http://slack.internal"); !errors.Is(err, ErrInvalid) {
		t.Fatalf("plain-http origin accepted: %v", err)
	}
	nonAdmin := admin("u2")
	delete(nonAdmin.Scopes, auth.ScopePlatformAdmin)
	if _, err := svc.CreateToken(ctx, nonAdmin, "slack", "X", "xoxb-good", slack.URL); !errors.Is(err, ErrDenied) {
		t.Fatalf("non-admin create: %v", err)
	}
	view, err := svc.CreateToken(ctx, admin("u1"), "slack", "Enclava Labs", "xoxb-good", slack.URL)
	if err != nil {
		t.Fatal(err)
	}
	if view.ID != "CONNECTOR_SLACK_ENCLAVA_LABS" || view.Details["account"] != "Enclava Labs" || view.Kind != KindToken {
		t.Fatalf("view = %+v", view)
	}
	if bytes.Contains(repo.sealed[tenant+"/"+view.ID], []byte("xoxb-good")) {
		t.Fatal("token stored in plaintext")
	}
	if _, err := svc.CreateToken(ctx, admin("u1"), "slack", "enclava labs", "xoxb-good", slack.URL); !errors.Is(err, ErrConflict) {
		t.Fatalf("duplicate name: %v", err)
	}
	if _, err := svc.CreateToken(ctx, admin("u1"), "slack", "env", "xoxb-good", slack.URL); !errors.Is(err, ErrConflict) {
		t.Fatalf("shadowing a server binding: %v", err)
	}

	if got, err := svc.Resolve(tenant, auth.SecretPurposeConnector, slack.URL, view.ID); err != nil || got != "xoxb-good" {
		t.Fatalf("resolve = %q, %v", got, err)
	}
	for name, call := range map[string][4]string{
		"provider default origin": {tenant, auth.SecretPurposeConnector, "https://slack.com", view.ID},
		"other tenant":            {"22222222-2222-2222-2222-222222222222", auth.SecretPurposeConnector, slack.URL, view.ID},
		"other purpose":           {tenant, auth.SecretPurposeMCP, slack.URL, view.ID},
	} {
		if got, err := svc.Resolve(call[0], call[1], call[2], call[3]); !errors.Is(err, auth.ErrSecretDenied) || got != "" {
			t.Fatalf("%s: resolve = %q, %v", name, got, err)
		}
	}
	if got, err := svc.Resolve(tenant, auth.SecretPurposeConnector, "https://slack.com", "CONNECTOR_SLACK_ENV"); err != nil || got != "xoxb-env" {
		t.Fatalf("server binding fallback = %q, %v", got, err)
	}
	refs := svc.Metadata(tenant)
	if len(refs) != 2 || refs[0].RefID != "CONNECTOR_SLACK_ENV" || refs[1].Name != "Enclava Labs" {
		t.Fatalf("metadata = %+v", refs)
	}

	// The seal is bound to tenant and id: a value copied onto another
	// credential does not open.
	other := Credential{TenantID: tenant, ID: "CONNECTOR_SLACK_COPY", Provider: "slack", Kind: KindToken, Name: "Copy", Details: view.Details}
	if err := repo.CreateCredential(ctx, other, repo.sealed[tenant+"/"+view.ID]); err != nil {
		t.Fatal(err)
	}
	if _, err := svc.Resolve(tenant, auth.SecretPurposeConnector, slack.URL, other.ID); !errors.Is(err, auth.ErrSecretDenied) {
		t.Fatalf("transplanted seal opened: %v", err)
	}

	repo.using[view.ID] = []string{"src-1"}
	if used, err := svc.Delete(ctx, admin("u1"), view.ID); !errors.Is(err, ErrInUse) || len(used) != 1 {
		t.Fatalf("delete in use = %v, %v", used, err)
	}
	delete(repo.using, view.ID)
	if _, err := svc.Delete(ctx, admin("u1"), view.ID); err != nil {
		t.Fatal(err)
	}

	disabled, err := NewService(repo, nil, env, "")
	if err != nil {
		t.Fatal(err)
	}
	if _, err := disabled.CreateToken(ctx, admin("u1"), "slack", "Later", "xoxb-good", slack.URL); !errors.Is(err, ErrDisabled) {
		t.Fatalf("disabled create: %v", err)
	}
	if got, err := disabled.Resolve(tenant, auth.SecretPurposeConnector, "https://slack.com", "CONNECTOR_SLACK_ENV"); err != nil || got != "xoxb-env" {
		t.Fatalf("disabled store must keep server bindings working: %q, %v", got, err)
	}
}

func TestGoogleOAuthConnectAndRefresh(t *testing.T) {
	var mu sync.Mutex
	var refreshes []string
	google := httptest.NewTLSServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		switch r.URL.Path {
		case "/token":
			_ = r.ParseForm()
			if r.PostForm.Get("client_id") != "cid" || r.PostForm.Get("client_secret") != "csecret" {
				w.WriteHeader(http.StatusUnauthorized)
				_, _ = io.WriteString(w, `{"error":"invalid_client"}`)
				return
			}
			switch r.PostForm.Get("grant_type") {
			case "authorization_code":
				if r.PostForm.Get("code") != "good-code" || r.PostForm.Get("redirect_uri") != "https://enclava.test/dashboard/connectors/oauth/callback/google_drive" {
					w.WriteHeader(http.StatusBadRequest)
					_, _ = io.WriteString(w, `{"error":"invalid_grant","error_description":"Bad Request"}`)
					return
				}
				_, _ = io.WriteString(w, `{"access_token":"at-1","refresh_token":"rt-1","expires_in":3600,"scope":"https://www.googleapis.com/auth/drive.readonly"}`)
			case "refresh_token":
				mu.Lock()
				refreshes = append(refreshes, r.PostForm.Get("refresh_token"))
				n := len(refreshes)
				mu.Unlock()
				_, _ = io.WriteString(w, `{"access_token":"at-`+strconv.Itoa(n+1)+`","expires_in":3600}`)
			}
		case "/drive/v3/about":
			if !strings.HasPrefix(r.Header.Get("Authorization"), "Bearer at-") {
				w.WriteHeader(http.StatusUnauthorized)
				return
			}
			_, _ = io.WriteString(w, `{"user":{"emailAddress":"admin@example.com"}}`)
		default:
			http.NotFound(w, r)
		}
	}))
	defer google.Close()
	repo := newMemRepo()
	svc := newTestService(t, repo, nil, google.Client())
	pv := svc.providers["google_drive"]
	pv.defaultOrigin, pv.extraOrigins = google.URL, nil
	pv.oauth.exchangeEndpoint = google.URL + "/token"
	now := time.Date(2026, 1, 2, 3, 4, 5, 0, time.UTC)
	svc.now = func() time.Time { return now }
	ctx := context.Background()

	if _, err := svc.StartOAuth(ctx, admin("u1"), "google_drive", "Team Drive"); !errors.Is(err, ErrInvalid) {
		t.Fatalf("start without an OAuth app: %v", err)
	}
	if _, err := svc.SetOAuthApp(ctx, admin("u1"), "teams", "cid", "csecret"); !errors.Is(err, ErrInvalid) {
		t.Fatalf("teams OAuth app accepted: %v", err)
	}
	if _, err := svc.SetOAuthApp(ctx, admin("u1"), "google_drive", "cid", "csecret"); err != nil {
		t.Fatal(err)
	}
	authorize, err := svc.StartOAuth(ctx, admin("u1"), "google_drive", "Team Drive")
	if err != nil {
		t.Fatal(err)
	}
	u, err := url.Parse(authorize)
	if err != nil {
		t.Fatal(err)
	}
	q := u.Query()
	if u.Host != "accounts.google.com" || q.Get("client_id") != "cid" || q.Get("access_type") != "offline" ||
		q.Get("redirect_uri") != "https://enclava.test/dashboard/connectors/oauth/callback/google_drive" || len(q.Get("state")) < 40 {
		t.Fatalf("authorize URL = %s", authorize)
	}
	state := q.Get("state")

	if _, err := svc.CompleteOAuth(ctx, admin("u1"), "google_drive", "forged", "good-code"); !errors.Is(err, ErrInvalid) {
		t.Fatalf("unknown state: %v", err)
	}
	view, err := svc.CompleteOAuth(ctx, admin("u1"), "google_drive", state, "good-code")
	if err != nil {
		t.Fatal(err)
	}
	if view.ID != "CONNECTOR_GOOGLE_DRIVE_TEAM_DRIVE" || view.Kind != KindOAuth || view.Details["account"] != "admin@example.com" || view.Details["expires_at"] != "2026-01-02T04:04:05Z" {
		t.Fatalf("view = %+v", view)
	}
	if _, err := svc.CompleteOAuth(ctx, admin("u1"), "google_drive", state, "good-code"); !errors.Is(err, ErrInvalid) {
		t.Fatalf("replayed state: %v", err)
	}
	other, _ := svc.StartOAuth(ctx, admin("u1"), "google_drive", "Other")
	otherURL, _ := url.Parse(other)
	if _, err := svc.CompleteOAuth(ctx, admin("u2"), "google_drive", otherURL.Query().Get("state"), "good-code"); !errors.Is(err, ErrInvalid) {
		t.Fatalf("state completed by another admin: %v", err)
	}

	if got, err := svc.Resolve(tenant, auth.SecretPurposeConnector, google.URL, view.ID); err != nil || got != "at-1" {
		t.Fatalf("fresh token = %q, %v", got, err)
	}
	now = now.Add(59 * time.Minute)
	if got, err := svc.Resolve(tenant, auth.SecretPurposeConnector, google.URL, view.ID); err != nil || got != "at-2" {
		t.Fatalf("refreshed token = %q, %v", got, err)
	}
	now = now.Add(59 * time.Minute)
	if got, err := svc.Resolve(tenant, auth.SecretPurposeConnector, google.URL, view.ID); err != nil || got != "at-3" {
		t.Fatalf("second refresh = %q, %v", got, err)
	}
	if strings.Join(refreshes, ",") != "rt-1,rt-1" {
		t.Fatalf("refresh grants = %v (the refresh token must survive responses that omit it)", refreshes)
	}

	expired, _ := svc.StartOAuth(ctx, admin("u1"), "google_drive", "Late")
	expiredURL, _ := url.Parse(expired)
	now = now.Add(11 * time.Minute)
	if _, err := svc.CompleteOAuth(ctx, admin("u1"), "google_drive", expiredURL.Query().Get("state"), "good-code"); !errors.Is(err, ErrInvalid) {
		t.Fatalf("expired state: %v", err)
	}
}

func TestTokenRequestWireFormats(t *testing.T) {
	type seen struct {
		contentType, basicUser, body string
	}
	var got seen
	stub := httptest.NewTLSServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		user, _, _ := r.BasicAuth()
		raw, _ := io.ReadAll(r.Body)
		got = seen{r.Header.Get("Content-Type"), user, string(raw)}
		if strings.Contains(string(raw), "bad") {
			_, _ = io.WriteString(w, `{"ok":false,"error":"invalid_code"}`)
			return
		}
		_, _ = io.WriteString(w, `{"ok":true,"access_token":"tok","team":{"name":"Labs"},"workspace_name":"WS"}`)
	}))
	defer stub.Close()
	providers := defaultProviders()
	now := time.Now()

	notion := *providers["notion"].oauth
	notion.exchangeEndpoint = stub.URL
	g, err := notion.exchange(context.Background(), stub.Client(), "cid", "csecret", "https://enclava.test/cb", "code-1", now)
	if err != nil || g.secret.Token != "tok" {
		t.Fatalf("notion exchange = %+v, %v", g, err)
	}
	var body map[string]string
	if got.contentType != "application/json" || got.basicUser != "cid" || json.Unmarshal([]byte(got.body), &body) != nil || body["code"] != "code-1" || body["client_secret"] != "" {
		t.Fatalf("notion request = %+v", got)
	}

	slack := *providers["slack"].oauth
	slack.exchangeEndpoint = stub.URL
	g, err = slack.exchange(context.Background(), stub.Client(), "cid", "csecret", "https://enclava.test/cb", "code-2", now)
	form, _ := url.ParseQuery(got.body)
	if err != nil || g.account != "Labs" || got.contentType != "application/x-www-form-urlencoded" || form.Get("client_secret") != "csecret" || got.basicUser != "" {
		t.Fatalf("slack exchange = %+v, %v, request %+v", g, err, got)
	}
	if _, err := slack.exchange(context.Background(), stub.Client(), "cid", "csecret", "https://enclava.test/cb", "bad", now); err == nil || err.Error() != "invalid_code" {
		t.Fatalf("slack in-band error = %v", err)
	}
	if !strings.Contains(providers["slack"].oauth.authorizeURL("cid", "https://enclava.test/cb", "st"), "scope="+url.QueryEscape("channels:history,channels:read")) {
		t.Fatal("slack scopes must be comma separated")
	}
}

func TestJiraAndLinearTokensAreProbedAndSiteBound(t *testing.T) {
	api := httptest.NewTLSServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		switch r.URL.Path {
		case "/rest/api/2/myself":
			if r.Header.Get("Authorization") != "Basic "+base64.StdEncoding.EncodeToString([]byte("ann@acme.test:tok")) {
				w.WriteHeader(http.StatusUnauthorized)
				return
			}
			_, _ = io.WriteString(w, `{"emailAddress":"ann@acme.test"}`)
		case "/graphql":
			if r.Method != http.MethodPost || r.Header.Get("Authorization") != "lin_api_good" {
				_, _ = io.WriteString(w, `{"errors":[{"message":"Authentication required"}]}`)
				return
			}
			_, _ = io.WriteString(w, `{"data":{"viewer":{"email":"v@acme.test"},"organization":{"name":"Acme"}}}`)
		default:
			http.NotFound(w, r)
		}
	}))
	defer api.Close()
	svc := newTestService(t, newMemRepo(), nil, api.Client())
	ctx := context.Background()

	if _, err := svc.CreateToken(ctx, admin("u1"), "jira", "Acme", "ann@acme.test:tok", ""); !errors.Is(err, ErrInvalid) || !strings.Contains(err.Error(), "site URL") {
		t.Fatalf("jira credential without a site: %v", err)
	}
	if _, err := svc.CreateToken(ctx, admin("u1"), "jira", "Acme", "tok", api.URL); !errors.Is(err, ErrInvalid) || !strings.Contains(err.Error(), "email:token") {
		t.Fatalf("jira token without email: %v", err)
	}
	view, err := svc.CreateToken(ctx, admin("u1"), "jira", "Acme", "ann@acme.test:tok", api.URL)
	if err != nil || view.ID != "CONNECTOR_JIRA_ACME" || view.Details["account"] != "ann@acme.test" {
		t.Fatalf("jira credential = %+v, %v", view, err)
	}
	if got, err := svc.Resolve(tenant, auth.SecretPurposeConnector, api.URL, view.ID); err != nil || got != "ann@acme.test:tok" {
		t.Fatalf("jira release to its site = %q, %v", got, err)
	}
	if _, err := svc.Resolve(tenant, auth.SecretPurposeConnector, "https://other.atlassian.net", view.ID); err == nil {
		t.Fatal("jira credential released to another site")
	}

	if _, err := svc.CreateToken(ctx, admin("u1"), "linear", "Bad", "lin_api_bad", api.URL); !errors.Is(err, ErrInvalid) {
		t.Fatalf("rejected linear key: %v", err)
	}
	if view, err := svc.CreateToken(ctx, admin("u1"), "linear", "Acme", "lin_api_good", api.URL); err != nil || view.Details["account"] != "Acme" {
		t.Fatalf("linear credential = %+v, %v", view, err)
	}
}
