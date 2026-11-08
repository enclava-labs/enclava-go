package connectors

import (
	"bytes"
	"context"
	"encoding/json"
	"errors"
	"fmt"
	"io"
	"net/http"
	"sort"
	"strconv"
	"strings"
	"time"

	"enclava-go/internal/auth"
	"enclava-go/internal/source"
)

const (
	linearAPIOrigin        = "https://api.linear.app"
	linearCommentPageSize  = 100
	linearMaxComments      = 1000
	linearDeltaPhaseIssues = ""
	linearDeltaPhaseByID   = "changed"
)

// LinearAdapter syncs Linear issues (with comments) as one markdown document
// per issue. Delta runs list issues updated since the previous run and then
// re-fetch issues whose comments changed.
type LinearAdapter struct {
	base   string
	apiKey string
	connID string
	teams  []string
	client *http.Client
}

type linearCursor struct {
	Run      syncRun  `json:"run"`
	Phase    string   `json:"phase,omitempty"`
	After    string   `json:"after,omitempty"`
	IssueIDs []string `json:"issue_ids,omitempty"`
}

type linearIssue struct {
	ID, Identifier, Title, Description, URL, PriorityLabel string
	CreatedAt, UpdatedAt                                   string
	State                                                  *struct {
		Name string `json:"name"`
	} `json:"state"`
	Team *struct {
		Key  string `json:"key"`
		Name string `json:"name"`
	} `json:"team"`
	Assignee *struct {
		Name string `json:"name"`
	} `json:"assignee"`
	Creator *struct {
		Name string `json:"name"`
	} `json:"creator"`
	Labels *struct {
		Nodes []struct {
			Name string `json:"name"`
		} `json:"nodes"`
	} `json:"labels"`
	Project *struct {
		Name string `json:"name"`
	} `json:"project"`
	Comments struct {
		Nodes    []linearComment `json:"nodes"`
		PageInfo linearPageInfo  `json:"pageInfo"`
	} `json:"comments"`
}

type linearComment struct {
	ID        string `json:"id"`
	Body      string `json:"body"`
	CreatedAt string `json:"createdAt"`
	User      *struct {
		Name string `json:"name"`
	} `json:"user"`
	Issue *struct {
		ID string `json:"id"`
	} `json:"issue"`
}

type linearPageInfo struct {
	HasNextPage bool   `json:"hasNextPage"`
	EndCursor   string `json:"endCursor"`
}

type linearIssueConn struct {
	Nodes    []linearIssue  `json:"nodes"`
	PageInfo linearPageInfo `json:"pageInfo"`
}

type linearCommentConn struct {
	Nodes    []linearComment `json:"nodes"`
	PageInfo linearPageInfo  `json:"pageInfo"`
}

type linearResp struct {
	Data struct {
		Issues   *linearIssueConn   `json:"issues"`
		Issue    *linearIssue       `json:"issue"`
		Comments *linearCommentConn `json:"comments"`
	} `json:"data"`
	Errors []struct {
		Message    string `json:"message"`
		Extensions struct {
			Code string `json:"code"`
		} `json:"extensions"`
	} `json:"errors"`
}

const linearIssuesQuery = `query LinearIssues($first: Int!, $after: String, $filter: IssueFilter) {
  issues(first: $first, after: $after, orderBy: createdAt, filter: $filter) {
    nodes {
      id identifier title description url priorityLabel createdAt updatedAt
      state { name } team { key name } assignee { name } creator { name }
      labels { nodes { name } } project { name }
      comments(first: 100) {
        nodes { id body createdAt user { name } }
        pageInfo { hasNextPage endCursor }
      }
    }
    pageInfo { hasNextPage endCursor }
  }
}`

const linearIssueCommentsQuery = `query LinearIssueComments($id: String!, $after: String) {
  issue(id: $id) {
    comments(first: 100, after: $after) {
      nodes { id body createdAt user { name } }
      pageInfo { hasNextPage endCursor }
    }
  }
}`

const linearCommentsQuery = `query LinearComments($after: String, $filter: CommentFilter) {
  comments(first: 100, after: $after, filter: $filter) {
    nodes { id issue { id } }
    pageInfo { hasNextPage endCursor }
  }
}`

func NewLinearAdapter(c source.Connection, secrets CredentialResolver) (Adapter, error) {
	if c.Kind != source.ConnectionLinear {
		return nil, errors.New("linear adapter kind does not match connection")
	}
	ref := strings.TrimSpace(c.CredentialRef)
	if ref == "" {
		return nil, errors.New("linear credential_ref is required")
	}
	if secrets == nil {
		return nil, errors.New("linear credential resolver is not configured")
	}
	if v, ok := c.Config["base_url"]; ok && v != nil {
		if _, ok := v.(string); !ok {
			return nil, errors.New("linear config.base_url must be a string")
		}
	}
	switch keys := c.Config["team_keys"].(type) {
	case nil, string, []string:
	case []any:
		for _, key := range keys {
			if _, ok := key.(string); !ok {
				return nil, errors.New("linear config.team_keys must contain strings")
			}
		}
	default:
		return nil, errors.New("linear config.team_keys must be a list or comma-separated string")
	}
	base := strings.TrimRight(firstNonEmpty(stringFromAny(c.Config["base_url"]), linearAPIOrigin), "/")
	parsed, err := requireAPIOrigin(base)
	if err != nil {
		return nil, errors.New("linear config.base_url: " + err.Error())
	}
	apiKey, err := secrets.Resolve(c.TenantID, auth.SecretPurposeConnector, parsed.Scheme+"://"+parsed.Host, ref)
	if err != nil {
		return nil, fmt.Errorf("resolve linear credential: %w", err)
	}
	if apiKey == "" {
		return nil, errors.New("linear credential resolved to an empty key")
	}
	var teams []string
	for _, key := range stringListFromAny(c.Config["team_keys"]) {
		if key = strings.TrimSpace(key); key != "" {
			teams = append(teams, key)
		}
	}
	return &LinearAdapter{base: base, apiKey: apiKey, connID: c.ID, teams: teams, client: newConnectorHTTPClient()}, nil
}

func (l *LinearAdapter) Capabilities() AdapterCapabilities {
	return AdapterCapabilities{Content: true}
}

// graphql posts one query and retries on HTTP 429 or a RATELIMITED GraphQL
// error, mirroring the GitHub adapter's bounded backoff.
func (l *LinearAdapter) graphql(ctx context.Context, query string, variables map[string]any, out *linearResp) error {
	body, err := json.Marshal(map[string]any{"query": query, "variables": variables})
	if err != nil {
		return err
	}
	for attempt := range 3 {
		req, err := http.NewRequestWithContext(ctx, http.MethodPost, l.base+"/graphql", bytes.NewReader(body))
		if err != nil {
			return err
		}
		req.Header.Set("Content-Type", "application/json")
		req.Header.Set("Authorization", l.apiKey)
		resp, err := l.client.Do(req)
		if err != nil {
			return err
		}
		raw, readErr := io.ReadAll(resp.Body)
		retryAfter := resp.Header.Get("Retry-After")
		status := resp.StatusCode
		_ = resp.Body.Close()
		if readErr != nil {
			return errors.New("linear API response could not be read")
		}

		var decoded linearResp
		rateLimited := status == http.StatusTooManyRequests
		if status >= 200 && status < 300 {
			if err := json.Unmarshal(raw, &decoded); err != nil {
				return errors.New("linear API returned an invalid response")
			}
			if len(decoded.Errors) == 0 {
				*out = decoded
				return nil
			}
			for _, e := range decoded.Errors {
				if strings.EqualFold(e.Extensions.Code, "RATELIMITED") {
					rateLimited = true
				}
			}
		}
		if attempt == 2 || !rateLimited {
			if len(decoded.Errors) > 0 {
				return errors.New("linear API returned a GraphQL error")
			}
			return fmt.Errorf("linear API returned status %d", status)
		}
		delay := time.Second
		if n, err := strconv.Atoi(retryAfter); err == nil && n > 0 {
			delay = time.Duration(n) * time.Second
		}
		if delay > 10*time.Second {
			delay = 10 * time.Second
		}
		select {
		case <-ctx.Done():
			return ctx.Err()
		case <-time.After(delay):
		}
	}
	return errors.New("linear rate-limit retry bound exceeded")
}

func (l *LinearAdapter) teamFilter() map[string]any {
	if len(l.teams) == 0 {
		return nil
	}
	keys := make([]any, len(l.teams))
	for i, key := range l.teams {
		keys[i] = key
	}
	return map[string]any{"team": map[string]any{"key": map[string]any{"in": keys}}}
}

func (l *LinearAdapter) Content(ctx context.Context, c source.Connection, cursor Cursor, limit int) (ContentPage, error) {
	if limit < 1 {
		limit = 1
	} else if limit > 100 {
		limit = 100
	}
	var state linearCursor
	if err := decodeJSONCursor(cursor, &state); err != nil {
		return ContentPage{}, err
	}
	if state.Run.Started == "" {
		state.Run = beginRun(c.Config, state.Run, time.Now())
	}
	if state.Phase == linearDeltaPhaseByID {
		return l.changedIssuesPage(ctx, state, limit)
	}
	if state.Phase != linearDeltaPhaseIssues {
		return ContentPage{}, errors.New("invalid linear cursor phase")
	}
	return l.issuesPage(ctx, state, limit)
}

// issuesPage runs phase 1: issues ordered by createdAt, filtered to updatedAt
// > Since on delta runs. When the listing ends on a delta run it collects the
// issues whose comments changed and hands them to phase 2.
func (l *LinearAdapter) issuesPage(ctx context.Context, s linearCursor, limit int) (ContentPage, error) {
	filter := l.teamFilter()
	if !s.Run.Full {
		filter = map[string]any{"updatedAt": map[string]any{"gt": s.Run.Since}}
		if team := l.teamFilter(); team != nil {
			filter["team"] = team["team"]
		}
	}
	variables := map[string]any{"first": limit, "filter": filter}
	if s.After != "" {
		variables["after"] = s.After
	}
	var resp linearResp
	if err := l.graphql(ctx, linearIssuesQuery, variables, &resp); err != nil {
		return ContentPage{}, err
	}
	if resp.Data.Issues == nil {
		return ContentPage{}, errors.New("linear API response has no issues")
	}
	conn := resp.Data.Issues
	if len(conn.Nodes) > limit {
		return ContentPage{}, errors.New("linear API returned too many issues")
	}
	items, err := l.issueItems(ctx, conn.Nodes)
	if err != nil {
		return ContentPage{}, err
	}
	page := ContentPage{Items: items, Completeness: CompletenessComplete, Snapshot: s.Run.Full}
	if conn.PageInfo.HasNextPage {
		if conn.PageInfo.EndCursor == "" || conn.PageInfo.EndCursor == s.After {
			return ContentPage{}, errors.New("linear API returned invalid issues cursor")
		}
		next := s
		next.After = conn.PageInfo.EndCursor
		page.NextCursor = encodeJSONCursor(next)
		return page, nil
	}
	if s.Run.Full {
		page.Checkpoint = encodeJSONCursor(linearCursor{Run: s.Run.finish()})
		return page, nil
	}
	// Delta listing exhausted: queue issues whose comments changed.
	ids, err := l.changedCommentIssueIDs(ctx, s.Run.Since)
	if err != nil {
		return ContentPage{}, err
	}
	if len(ids) == 0 {
		page.Checkpoint = encodeJSONCursor(linearCursor{Run: s.Run.finish()})
		return page, nil
	}
	page.NextCursor = encodeJSONCursor(linearCursor{Run: s.Run, Phase: linearDeltaPhaseByID, IssueIDs: ids})
	return page, nil
}

// changedIssuesPage runs phase 2 of a delta run: re-fetch issues by id in
// chunks of at most limit.
func (l *LinearAdapter) changedIssuesPage(ctx context.Context, s linearCursor, limit int) (ContentPage, error) {
	if len(s.IssueIDs) == 0 {
		return ContentPage{}, errors.New("invalid linear cursor issue ids")
	}
	chunk := s.IssueIDs
	if len(chunk) > limit {
		chunk = chunk[:limit]
	}
	ids := make([]any, len(chunk))
	for i, id := range chunk {
		ids[i] = id
	}
	var resp linearResp
	variables := map[string]any{"first": len(chunk), "filter": map[string]any{"id": map[string]any{"in": ids}}}
	if s.After != "" {
		variables["after"] = s.After
	}
	err := l.graphql(ctx, linearIssuesQuery, variables, &resp)
	if err != nil {
		return ContentPage{}, err
	}
	if resp.Data.Issues == nil {
		return ContentPage{}, errors.New("linear API response has no issues")
	}
	if len(resp.Data.Issues.Nodes) > len(chunk) {
		return ContentPage{}, errors.New("linear API returned too many issues")
	}
	items, err := l.issueItems(ctx, resp.Data.Issues.Nodes)
	if err != nil {
		return ContentPage{}, err
	}
	page := ContentPage{Items: items, Completeness: CompletenessComplete}
	if resp.Data.Issues.PageInfo.HasNextPage {
		next := resp.Data.Issues.PageInfo.EndCursor
		if next == "" || next == s.After {
			return ContentPage{}, errors.New("linear API returned invalid issues cursor")
		}
		s.After = next
		page.NextCursor = encodeJSONCursor(s)
		return page, nil
	}
	if rest := s.IssueIDs[len(chunk):]; len(rest) > 0 {
		page.NextCursor = encodeJSONCursor(linearCursor{Run: s.Run, Phase: linearDeltaPhaseByID, IssueIDs: rest})
		return page, nil
	}
	page.Checkpoint = encodeJSONCursor(linearCursor{Run: s.Run.finish()})
	return page, nil
}

// changedCommentIssueIDs lists distinct issue ids with comments updated since
// the run's Since, scoped to the configured teams when set.
func (l *LinearAdapter) changedCommentIssueIDs(ctx context.Context, since string) ([]string, error) {
	filter := map[string]any{"updatedAt": map[string]any{"gt": since}}
	if team := l.teamFilter(); team != nil {
		filter["issue"] = team
	}
	seen := map[string]bool{}
	var ids []string
	after := ""
	for {
		variables := map[string]any{"filter": filter}
		if after != "" {
			variables["after"] = after
		}
		var resp linearResp
		if err := l.graphql(ctx, linearCommentsQuery, variables, &resp); err != nil {
			return nil, err
		}
		if resp.Data.Comments == nil {
			return nil, errors.New("linear API response has no comments")
		}
		for _, node := range resp.Data.Comments.Nodes {
			if node.Issue != nil && node.Issue.ID != "" && !seen[node.Issue.ID] {
				seen[node.Issue.ID] = true
				ids = append(ids, node.Issue.ID)
			}
		}
		if !resp.Data.Comments.PageInfo.HasNextPage {
			return ids, nil
		}
		if resp.Data.Comments.PageInfo.EndCursor == "" || resp.Data.Comments.PageInfo.EndCursor == after {
			return nil, errors.New("linear API returned invalid comments cursor")
		}
		after = resp.Data.Comments.PageInfo.EndCursor
	}
}

// issueItems fetches comment overflow and renders one markdown item per issue.
func (l *LinearAdapter) issueItems(ctx context.Context, issues []linearIssue) ([]ContentItem, error) {
	items := make([]ContentItem, 0, len(issues))
	for i := range issues {
		issue := issues[i]
		comments := issue.Comments.Nodes
		after := issue.Comments.PageInfo.EndCursor
		for issue.Comments.PageInfo.HasNextPage && len(comments) < linearMaxComments {
			if after == "" {
				return nil, errors.New("linear API returned invalid issue comments cursor")
			}
			var resp linearResp
			err := l.graphql(ctx, linearIssueCommentsQuery, map[string]any{"id": issue.ID, "after": after}, &resp)
			if err != nil {
				return nil, err
			}
			if resp.Data.Issue == nil {
				return nil, errors.New("linear API response has no issue comments")
			}
			comments = append(comments, resp.Data.Issue.Comments.Nodes...)
			if !resp.Data.Issue.Comments.PageInfo.HasNextPage {
				break
			}
			next := resp.Data.Issue.Comments.PageInfo.EndCursor
			if next == "" || next == after {
				return nil, errors.New("linear API returned invalid issue comments cursor")
			}
			after = next
		}
		if len(comments) > linearMaxComments {
			comments = comments[:linearMaxComments]
		}
		sort.SliceStable(comments, func(i, j int) bool {
			a, _ := time.Parse(time.RFC3339, comments[i].CreatedAt)
			b, _ := time.Parse(time.RFC3339, comments[j].CreatedAt)
			return a.Before(b)
		})
		items = append(items, l.issueItem(issue, comments))
	}
	return items, nil
}

func (l *LinearAdapter) issueItem(issue linearIssue, comments []linearComment) ContentItem {
	created, _ := time.Parse(time.RFC3339, issue.CreatedAt)
	updated, _ := time.Parse(time.RFC3339, issue.UpdatedAt)
	var meta []string
	if issue.State != nil && issue.State.Name != "" {
		meta = append(meta, "**Status:** "+issue.State.Name)
	}
	if issue.PriorityLabel != "" {
		meta = append(meta, "**Priority:** "+issue.PriorityLabel)
	}
	if issue.Assignee != nil && issue.Assignee.Name != "" {
		meta = append(meta, "**Assignee:** "+issue.Assignee.Name)
	}
	if issue.Creator != nil && issue.Creator.Name != "" {
		meta = append(meta, "**Creator:** "+issue.Creator.Name)
	}
	if issue.Labels != nil {
		var names []string
		for _, label := range issue.Labels.Nodes {
			if label.Name != "" {
				names = append(names, label.Name)
			}
		}
		if len(names) > 0 {
			meta = append(meta, "**Labels:** "+strings.Join(names, ", "))
		}
	}
	if issue.Project != nil && issue.Project.Name != "" {
		meta = append(meta, "**Project:** "+issue.Project.Name)
	}
	if issue.Team != nil && issue.Team.Name != "" {
		meta = append(meta, "**Team:** "+issue.Team.Name)
	}
	var doc strings.Builder
	doc.WriteString("# " + issue.Identifier + " " + issue.Title + "\n\n")
	doc.WriteString(strings.Join(meta, " · ") + "\n\n")
	doc.WriteString("Created: " + created.UTC().Format("2006-01-02 15:04 UTC") + " · Updated: " + updated.UTC().Format("2006-01-02 15:04 UTC") + "\n\n")
	if body := strings.TrimSpace(issue.Description); body != "" {
		doc.WriteString(body + "\n\n")
	}
	doc.WriteString("## Comments\n\n")
	for _, comment := range comments {
		author := "Unknown"
		if comment.User != nil && comment.User.Name != "" {
			author = comment.User.Name
		}
		at, _ := time.Parse(time.RFC3339, comment.CreatedAt)
		doc.WriteString("**" + author + "** — " + at.UTC().Format("2006-01-02 15:04 UTC") + "\n\n")
		doc.WriteString(strings.TrimSpace(comment.Body) + "\n\n")
	}
	parentID, parentKind := "", ""
	if issue.Team != nil && issue.Team.Key != "" {
		parentID = "linear:" + l.connID + ":team:" + issue.Team.Key
		parentKind = "team"
	}
	return ContentItem{
		NativeID:       "linear:" + l.connID + ":" + issue.ID,
		ParentNativeID: parentID,
		ParentKind:     parentKind,
		Kind:           "issue",
		NativeRevision: issue.UpdatedAt,
		Title:          issue.Identifier + " " + issue.Title,
		Filename:       issue.Identifier + ".md",
		MIMEType:       "text/markdown",
		Text:           doc.String(),
		URL:            issue.URL,
		CreatedAt:      &created,
		UpdatedAt:      &updated,
	}
}

var _ Adapter = (*LinearAdapter)(nil)
