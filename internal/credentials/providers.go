package credentials

import (
	"bytes"
	"context"
	"encoding/base64"
	"encoding/json"
	"errors"
	"fmt"
	"io"
	"net/http"
	"net/url"
	"slices"
	"strings"
	"time"
)

// provider describes one connector service: the API origin its credentials
// may be released to, how a pasted token is validated, and its OAuth flow
// (nil when the service authenticates with an app secret instead).
type provider struct {
	id, label, refPrefix string
	defaultOrigin        string
	extraOrigins         []string // sibling APIs the default credential also serves
	probe                func(ctx context.Context, client *http.Client, origin, token string) (string, error)
	oauth                *oauthSpec
}

func defaultProviders() map[string]*provider {
	out := map[string]*provider{}
	for _, pv := range []*provider{
		{id: "github", label: "GitHub", refPrefix: "GITHUB", defaultOrigin: "https://api.github.com", probe: probeGitHub,
			oauth: &oauthSpec{authEndpoint: "https://github.com/login/oauth/authorize", exchangeEndpoint: "https://github.com/login/oauth/access_token",
				scopes: []string{"repo", "read:org"}, scopeSep: " "}},
		{id: "slack", label: "Slack", refPrefix: "SLACK", defaultOrigin: "https://slack.com", probe: probeSlack,
			oauth: &oauthSpec{authEndpoint: "https://slack.com/oauth/v2/authorize", exchangeEndpoint: "https://slack.com/api/oauth.v2.access",
				scopes: []string{"channels:history", "channels:read", "groups:history", "groups:read", "users:read", "files:read"}, scopeSep: ","}},
		{id: "notion", label: "Notion", refPrefix: "NOTION", defaultOrigin: "https://api.notion.com", probe: probeNotion,
			oauth: &oauthSpec{authEndpoint: "https://api.notion.com/v1/oauth/authorize", exchangeEndpoint: "https://api.notion.com/v1/oauth/token",
				extra: url.Values{"response_type": {"code"}, "owner": {"user"}}, basicAuth: true, jsonBody: true}},
		{id: "teams", label: "Microsoft Teams", refPrefix: "TEAMS", defaultOrigin: "https://graph.microsoft.com"},
		{id: "google_drive", label: "Google Drive", refPrefix: "GOOGLE_DRIVE", defaultOrigin: "https://www.googleapis.com", extraOrigins: []string{"https://sheets.googleapis.com"}, probe: probeGoogleDrive,
			oauth: &oauthSpec{authEndpoint: "https://accounts.google.com/o/oauth2/v2/auth", exchangeEndpoint: "https://oauth2.googleapis.com/token",
				scopes: []string{"https://www.googleapis.com/auth/drive.readonly"}, scopeSep: " ",
				extra: url.Values{"response_type": {"code"}, "access_type": {"offline"}, "prompt": {"consent"}, "include_granted_scopes": {"true"}}}},
		{id: "gitlab", label: "GitLab", refPrefix: "GITLAB", defaultOrigin: "https://gitlab.com", probe: probeGitLab},
		// Jira sites have no shared API origin: credentials name their site.
		{id: "jira", label: "Jira", refPrefix: "JIRA", probe: probeJira},
		{id: "linear", label: "Linear", refPrefix: "LINEAR", defaultOrigin: "https://api.linear.app", probe: probeLinear},
	} {
		out[pv.id] = pv
	}
	return out
}

var providerOrder = []string{"github", "slack", "notion", "teams", "google_drive", "gitlab", "jira", "linear"}

func orderedProviders(m map[string]*provider) []*provider {
	out := make([]*provider, 0, len(providerOrder))
	for _, id := range providerOrder {
		if pv := m[id]; pv != nil {
			out = append(out, pv)
		}
	}
	return out
}

// apiOrigin is the only origin a credential is released to: its custom
// origin (for example GitHub Enterprise) or the provider's API.
func apiOrigin(pv *provider, details map[string]any) string {
	if origin, _ := details["origin"].(string); origin != "" {
		return origin
	}
	return pv.defaultOrigin
}

// allowedOrigin reports whether a credential may be sent to origin: its API
// origin, or (without a custom origin) the provider's sibling APIs such as
// Google Sheets for Drive exports.
func allowedOrigin(pv *provider, details map[string]any, origin string) bool {
	if origin == apiOrigin(pv, details) {
		return true
	}
	if custom, _ := details["origin"].(string); custom != "" {
		return false
	}
	return slices.Contains(pv.extraOrigins, origin)
}

func (pv *provider) probeToken(ctx context.Context, client *http.Client, origin, token string) (string, error) {
	if pv.probe == nil {
		return "", nil
	}
	return pv.probe(ctx, client, origin, token)
}

// getJSON performs one authenticated GET and decodes a 2xx JSON body; the
// error never echoes the token or the response body.
func getJSON(ctx context.Context, client *http.Client, endpoint string, header http.Header, out any) error {
	req, err := http.NewRequestWithContext(ctx, http.MethodGet, endpoint, nil)
	if err != nil {
		return err
	}
	req.Header = header
	return doJSON(client, req, out)
}

func doJSON(client *http.Client, req *http.Request, out any) error {
	req.Header.Set("Accept", "application/json")
	resp, err := client.Do(req)
	if err != nil {
		return fmt.Errorf("request failed: %w", err)
	}
	defer func() { _ = resp.Body.Close() }()
	raw, err := io.ReadAll(io.LimitReader(resp.Body, 1<<20))
	if err != nil {
		return err
	}
	if resp.StatusCode < 200 || resp.StatusCode >= 300 {
		return fmt.Errorf("HTTP %d", resp.StatusCode)
	}
	return json.Unmarshal(raw, out)
}

func bearer(token string) http.Header {
	return http.Header{"Authorization": {"Bearer " + token}}
}

func probeGitHub(ctx context.Context, client *http.Client, origin, token string) (string, error) {
	endpoint := origin + "/user"
	if origin != "https://api.github.com" {
		endpoint = origin + "/api/v3/user" // GitHub Enterprise Server
	}
	header := bearer(token)
	header.Set("X-GitHub-Api-Version", "2022-11-28")
	var user struct {
		Login string `json:"login"`
	}
	if err := getJSON(ctx, client, endpoint, header, &user); err != nil {
		return "", err
	}
	return user.Login, nil
}

func probeSlack(ctx context.Context, client *http.Client, origin, token string) (string, error) {
	var out struct {
		OK    bool   `json:"ok"`
		Error string `json:"error"`
		Team  string `json:"team"`
	}
	if err := getJSON(ctx, client, origin+"/api/auth.test", bearer(token), &out); err != nil {
		return "", err
	}
	if !out.OK {
		return "", errors.New(firstNonEmpty(out.Error, "auth.test failed"))
	}
	return out.Team, nil
}

func probeNotion(ctx context.Context, client *http.Client, origin, token string) (string, error) {
	header := bearer(token)
	header.Set("Notion-Version", "2022-06-28")
	var me struct {
		Name string `json:"name"`
		Bot  struct {
			WorkspaceName string `json:"workspace_name"`
		} `json:"bot"`
	}
	if err := getJSON(ctx, client, origin+"/v1/users/me", header, &me); err != nil {
		return "", err
	}
	return firstNonEmpty(me.Bot.WorkspaceName, me.Name), nil
}

func probeGoogleDrive(ctx context.Context, client *http.Client, origin, token string) (string, error) {
	var about struct {
		User struct {
			EmailAddress string `json:"emailAddress"`
		} `json:"user"`
	}
	if err := getJSON(ctx, client, origin+"/drive/v3/about?fields=user", bearer(token), &about); err != nil {
		return "", err
	}
	return about.User.EmailAddress, nil
}

func probeGitLab(ctx context.Context, client *http.Client, origin, token string) (string, error) {
	var user struct {
		Username string `json:"username"`
	}
	if err := getJSON(ctx, client, origin+"/api/v4/user", bearer(token), &user); err != nil {
		return "", err
	}
	return user.Username, nil
}

// probeJira checks an Atlassian account email and API token pasted as
// email:token against the credential's site.
func probeJira(ctx context.Context, client *http.Client, origin, token string) (string, error) {
	if !strings.Contains(token, ":") {
		return "", errors.New("paste the account email and API token as email:token")
	}
	header := http.Header{"Authorization": {"Basic " + base64.StdEncoding.EncodeToString([]byte(token))}}
	var me struct {
		EmailAddress string `json:"emailAddress"`
		DisplayName  string `json:"displayName"`
	}
	if err := getJSON(ctx, client, origin+"/rest/api/2/myself", header, &me); err != nil {
		return "", err
	}
	return firstNonEmpty(me.EmailAddress, me.DisplayName), nil
}

// probeLinear checks a personal API key; Linear takes it without "Bearer".
func probeLinear(ctx context.Context, client *http.Client, origin, token string) (string, error) {
	req, err := http.NewRequestWithContext(ctx, http.MethodPost, origin+"/graphql", strings.NewReader(`{"query":"{ viewer { email } organization { name } }"}`))
	if err != nil {
		return "", err
	}
	req.Header.Set("Authorization", token)
	req.Header.Set("Content-Type", "application/json")
	var out struct {
		Data struct {
			Viewer       struct{ Email string }
			Organization struct{ Name string }
		}
		Errors []json.RawMessage
	}
	if err := doJSON(client, req, &out); err != nil {
		return "", err
	}
	if len(out.Errors) > 0 {
		return "", errors.New("linear rejected the key")
	}
	return firstNonEmpty(out.Data.Organization.Name, out.Data.Viewer.Email), nil
}

// oauthSpec is one provider's OAuth 2.0 authorization-code flow.
type oauthSpec struct {
	authEndpoint     string
	exchangeEndpoint string // code and refresh-token grants
	scopes           []string
	scopeSep         string
	extra            url.Values
	basicAuth        bool // client credentials as HTTP Basic (Notion)
	jsonBody         bool // JSON token request body (Notion)
}

func (o *oauthSpec) authorizeURL(clientID, redirectURI, state string) string {
	q := url.Values{"client_id": {clientID}, "redirect_uri": {redirectURI}, "state": {state}}
	if len(o.scopes) > 0 {
		q.Set("scope", strings.Join(o.scopes, o.scopeSep))
	}
	for k, vs := range o.extra {
		q[k] = vs
	}
	return o.authEndpoint + "?" + q.Encode()
}

type grant struct {
	secret  secret
	account string
	scope   string
}

func (o *oauthSpec) exchange(ctx context.Context, client *http.Client, clientID, clientSecret, redirectURI, code string, now time.Time) (grant, error) {
	return o.tokenRequest(ctx, client, clientID, clientSecret, map[string]string{"grant_type": "authorization_code", "code": code, "redirect_uri": redirectURI}, now)
}

func (o *oauthSpec) refresh(ctx context.Context, client *http.Client, clientID, clientSecret, refreshToken string, now time.Time) (secret, error) {
	g, err := o.tokenRequest(ctx, client, clientID, clientSecret, map[string]string{"grant_type": "refresh_token", "refresh_token": refreshToken}, now)
	return g.secret, err
}

func (o *oauthSpec) tokenRequest(ctx context.Context, client *http.Client, clientID, clientSecret string, fields map[string]string, now time.Time) (grant, error) {
	var body []byte
	contentType := "application/x-www-form-urlencoded"
	if o.jsonBody {
		contentType = "application/json"
		raw, err := json.Marshal(fields)
		if err != nil {
			return grant{}, err
		}
		body = raw
	} else {
		form := url.Values{}
		for k, v := range fields {
			form.Set(k, v)
		}
		if !o.basicAuth {
			form.Set("client_id", clientID)
			form.Set("client_secret", clientSecret)
		}
		body = []byte(form.Encode())
	}
	req, err := http.NewRequestWithContext(ctx, http.MethodPost, o.exchangeEndpoint, bytes.NewReader(body))
	if err != nil {
		return grant{}, err
	}
	req.Header.Set("Content-Type", contentType)
	req.Header.Set("Accept", "application/json")
	if o.basicAuth {
		req.SetBasicAuth(clientID, clientSecret)
	}
	resp, err := client.Do(req)
	if err != nil {
		return grant{}, fmt.Errorf("token request failed: %w", err)
	}
	defer func() { _ = resp.Body.Close() }()
	raw, err := io.ReadAll(io.LimitReader(resp.Body, 1<<20))
	if err != nil {
		return grant{}, err
	}
	var payload struct {
		OK               *bool  `json:"ok"` // Slack reports errors in-band
		Error            string `json:"error"`
		ErrorDescription string `json:"error_description"`
		AccessToken      string `json:"access_token"`
		RefreshToken     string `json:"refresh_token"`
		ExpiresIn        int64  `json:"expires_in"`
		Scope            string `json:"scope"`
		Team             struct {
			Name string `json:"name"`
		} `json:"team"`
		WorkspaceName string `json:"workspace_name"`
	}
	_ = json.Unmarshal(raw, &payload)
	if resp.StatusCode < 200 || resp.StatusCode >= 300 || payload.Error != "" || (payload.OK != nil && !*payload.OK) || payload.AccessToken == "" {
		return grant{}, errors.New(firstNonEmpty(payload.ErrorDescription, payload.Error, fmt.Sprintf("HTTP %d", resp.StatusCode)))
	}
	g := grant{secret: secret{Token: payload.AccessToken, RefreshToken: payload.RefreshToken}, scope: payload.Scope, account: firstNonEmpty(payload.Team.Name, payload.WorkspaceName)}
	if payload.ExpiresIn > 0 {
		g.secret.ExpiresAt = now.Add(time.Duration(payload.ExpiresIn) * time.Second)
	}
	return g, nil
}

func firstNonEmpty(values ...string) string {
	for _, v := range values {
		if strings.TrimSpace(v) != "" {
			return v
		}
	}
	return ""
}
