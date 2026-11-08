package connectors

import (
	"context"
	"encoding/base64"
	"encoding/json"
	"errors"
	"fmt"
	"net/http"
	"net/url"
	"strconv"
	"strings"
	"time"
	"unicode"

	"enclava-go/internal/auth"
	"enclava-go/internal/source"
)

type JiraAdapter struct {
	base, project, authorization, tenantID, connectionID string
	client                                               *http.Client
}

type jiraCursor struct {
	Run           syncRun `json:"run"`
	NextPageToken string  `json:"next_page_token,omitempty"`
	Minutes       int64   `json:"minutes,omitempty"`
}

type jiraNamed struct {
	Name        string `json:"name"`
	DisplayName string `json:"displayName"`
}

type jiraIssue struct {
	ID     string `json:"id"`
	Key    string `json:"key"`
	Fields struct {
		Summary     string      `json:"summary"`
		Description string      `json:"description"`
		Status      jiraNamed   `json:"status"`
		IssueType   jiraNamed   `json:"issuetype"`
		Priority    jiraNamed   `json:"priority"`
		Assignee    jiraNamed   `json:"assignee"`
		Reporter    jiraNamed   `json:"reporter"`
		Labels      []string    `json:"labels"`
		Components  []jiraNamed `json:"components"`
		Created     string      `json:"created"`
		Updated     string      `json:"updated"`
	} `json:"fields"`
}

type jiraComment struct {
	Author  jiraNamed `json:"author"`
	Body    string    `json:"body"`
	Created string    `json:"created"`
}

func NewJiraAdapter(c source.Connection, secrets CredentialResolver) (Adapter, error) {
	if c.Kind != source.ConnectionJira {
		return nil, errors.New("jira adapter kind does not match connection")
	}
	base := strings.TrimSpace(stringFromAny(c.Config["base_url"]))
	if base == "" {
		return nil, errors.New("jira config.base_url is required")
	}
	u, err := requireAPIOrigin(base)
	if err != nil {
		return nil, err
	}
	if u.Path != "" && u.Path != "/" {
		return nil, errors.New("jira config.base_url must be a site origin")
	}
	project := strings.TrimSpace(stringFromAny(c.Config["project"]))
	if project == "" {
		return nil, errors.New("jira config.project is required")
	}
	if strings.IndexFunc(project, unicode.IsControl) >= 0 {
		return nil, errors.New("jira config.project contains control characters")
	}
	ref := strings.TrimSpace(c.CredentialRef)
	if ref == "" {
		return nil, errors.New("jira credential_ref is required")
	}
	if secrets == nil {
		return nil, errors.New("jira credential resolver is not configured")
	}
	origin := u.Scheme + "://" + u.Host
	secret, err := secrets.Resolve(c.TenantID, auth.SecretPurposeConnector, origin, ref)
	if err != nil {
		return nil, err
	}
	if !strings.Contains(secret, ":") {
		return nil, errors.New("jira credential must be email:api-token")
	}
	return &JiraAdapter{base: origin, project: project, authorization: "Basic " + base64.StdEncoding.EncodeToString([]byte(secret)), tenantID: c.TenantID, connectionID: c.ID, client: newConnectorHTTPClient()}, nil
}

func (a *JiraAdapter) Capabilities() AdapterCapabilities { return AdapterCapabilities{Content: true} }

func (a *JiraAdapter) get(ctx context.Context, endpoint string, target any) error {
	for attempt := range 3 {
		req, err := http.NewRequestWithContext(ctx, http.MethodGet, endpoint, nil)
		if err != nil {
			return err
		}
		req.Header.Set("Accept", "application/json")
		req.Header.Set("Authorization", a.authorization)
		resp, err := a.client.Do(req)
		if err != nil {
			return err
		}
		if resp.StatusCode >= 200 && resp.StatusCode < 300 {
			err = json.NewDecoder(resp.Body).Decode(target)
			_ = resp.Body.Close()
			return err
		}
		status, retryAfter := resp.StatusCode, resp.Header.Get("Retry-After")
		_ = resp.Body.Close()
		if status != http.StatusTooManyRequests || attempt == 2 {
			return fmt.Errorf("jira API returned status %d", status)
		}
		delay := time.Second
		if n, err := strconv.Atoi(retryAfter); err == nil && n > 0 {
			if n > 10 {
				n = 10
			}
			delay = time.Duration(n) * time.Second
		}
		select {
		case <-ctx.Done():
			return ctx.Err()
		case <-time.After(delay):
		}
	}
	return errors.New("jira rate-limit retry bound exceeded")
}

func (a *JiraAdapter) Content(ctx context.Context, c source.Connection, cursor Cursor, limit int) (ContentPage, error) {
	if c.Kind != source.ConnectionJira || c.TenantID != a.tenantID || c.ID != a.connectionID {
		return ContentPage{}, errors.New("jira adapter connection does not match")
	}
	if limit < 1 {
		return ContentPage{}, errors.New("jira content limit must be positive")
	}
	if limit > 100 {
		limit = 100
	}
	var state jiraCursor
	if err := decodeJSONCursor(cursor, &state); err != nil {
		return ContentPage{}, err
	}
	if state.Run.Started == "" {
		now := time.Now()
		state.Run = beginRun(c.Config, state.Run, now)
		state.NextPageToken = ""
		if !state.Run.Full {
			since, err := time.Parse(time.RFC3339, state.Run.Since)
			if err != nil {
				return ContentPage{}, errors.New("invalid jira sync cursor")
			}
			d := now.Sub(since)
			state.Minutes = int64(d / time.Minute)
			if d%time.Minute > 0 {
				state.Minutes++
			}
			if state.Minutes < 1 {
				state.Minutes = 1
			}
		}
	}
	project := strings.NewReplacer("\\", "\\\\", "\"", "\\\"").Replace(a.project)
	jql := "project = \"" + project + "\""
	if !state.Run.Full {
		jql += fmt.Sprintf(" AND updated >= \"-%dm\"", state.Minutes)
	}
	jql += " ORDER BY created ASC"
	q := url.Values{"jql": {jql}, "fields": {"summary,description,status,issuetype,priority,assignee,reporter,labels,components,created,updated"}, "maxResults": {strconv.Itoa(limit)}}
	if state.NextPageToken != "" {
		q.Set("nextPageToken", state.NextPageToken)
	}
	var result struct {
		Issues        []jiraIssue `json:"issues"`
		NextPageToken string      `json:"nextPageToken"`
		IsLast        bool        `json:"isLast"`
	}
	if err := a.get(ctx, a.base+"/rest/api/2/search/jql?"+q.Encode(), &result); err != nil {
		return ContentPage{}, err
	}
	if len(result.Issues) > limit {
		return ContentPage{}, errors.New("jira search exceeded content page bound")
	}
	page := ContentPage{Snapshot: state.Run.Full, Completeness: CompletenessComplete}
	for _, issue := range result.Issues {
		if issue.ID == "" || issue.Key == "" {
			return ContentPage{}, errors.New("jira issue is missing id or key")
		}
		item, err := a.issue(ctx, c, issue)
		if err != nil {
			return ContentPage{}, err
		}
		page.Items = append(page.Items, item)
	}
	if !result.IsLast && result.NextPageToken != "" {
		if result.NextPageToken == state.NextPageToken {
			return ContentPage{}, errors.New("jira search repeated page token")
		}
		state.NextPageToken = result.NextPageToken
		page.NextCursor = encodeJSONCursor(state)
	} else {
		page.Checkpoint = encodeJSONCursor(jiraCursor{Run: state.Run.finish()})
	}
	return page, nil
}

func jiraTime(value string) (*time.Time, error) {
	if value == "" {
		return nil, nil
	}
	t, err := time.Parse("2006-01-02T15:04:05.000-0700", value)
	if err != nil {
		return nil, errors.New("invalid jira timestamp")
	}
	t = t.UTC()
	return &t, nil
}

func (a *JiraAdapter) issue(ctx context.Context, c source.Connection, issue jiraIssue) (ContentItem, error) {
	created, err := jiraTime(issue.Fields.Created)
	if err != nil {
		return ContentItem{}, err
	}
	updated, err := jiraTime(issue.Fields.Updated)
	if err != nil {
		return ContentItem{}, err
	}
	comments := make([]jiraComment, 0)
	for len(comments) < 1000 {
		q := url.Values{"startAt": {strconv.Itoa(len(comments))}, "maxResults": {"100"}, "orderBy": {"created"}}
		var result struct {
			Comments []jiraComment `json:"comments"`
			Total    int           `json:"total"`
		}
		if err := a.get(ctx, a.base+"/rest/api/2/issue/"+url.PathEscape(issue.Key)+"/comment?"+q.Encode(), &result); err != nil {
			return ContentItem{}, err
		}
		if len(result.Comments) == 0 && len(comments) < result.Total {
			return ContentItem{}, errors.New("jira comments pagination did not advance")
		}
		remaining := 1000 - len(comments)
		if len(result.Comments) > remaining {
			result.Comments = result.Comments[:remaining]
		}
		comments = append(comments, result.Comments...)
		if len(comments) >= result.Total || len(result.Comments) == 0 {
			break
		}
	}
	components := make([]string, 0, len(issue.Fields.Components))
	for _, component := range issue.Fields.Components {
		components = append(components, component.Name)
	}
	title := issue.Key + " " + issue.Fields.Summary
	var text strings.Builder
	fmt.Fprintf(&text, "# %s\n\nType: %s | Status: %s | Priority: %s | Assignee: %s | Reporter: %s | Labels: %s | Components: %s | Project: %s\n", title, issue.Fields.IssueType.Name, issue.Fields.Status.Name, issue.Fields.Priority.Name, issue.Fields.Assignee.DisplayName, issue.Fields.Reporter.DisplayName, strings.Join(issue.Fields.Labels, ", "), strings.Join(components, ", "), a.project)
	if created != nil {
		fmt.Fprintf(&text, "Created: %s\n", created.Format("2006-01-02 15:04 UTC"))
	}
	if updated != nil {
		fmt.Fprintf(&text, "Updated: %s\n", updated.Format("2006-01-02 15:04 UTC"))
	}
	fmt.Fprintf(&text, "\n%s\n\n## Comments\n", issue.Fields.Description)
	for _, comment := range comments {
		at, err := jiraTime(comment.Created)
		if err != nil {
			return ContentItem{}, err
		}
		stamp := ""
		if at != nil {
			stamp = at.Format("2006-01-02 15:04 UTC")
		}
		fmt.Fprintf(&text, "\n**%s** — %s\n\n%s\n", comment.Author.DisplayName, stamp, comment.Body)
	}
	return ContentItem{NativeID: "jira:" + c.ID + ":" + issue.ID, ParentNativeID: "jira:" + c.ID + ":project:" + a.project, Kind: "issue", ParentKind: "project", NativeRevision: issue.Fields.Updated, Title: title, Filename: issue.Key + ".md", MIMEType: "text/markdown", Text: text.String(), URL: a.base + "/browse/" + url.PathEscape(issue.Key), CreatedAt: created, UpdatedAt: updated}, nil
}
