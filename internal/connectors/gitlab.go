package connectors

import (
	"context"
	"encoding/json"
	"errors"
	"fmt"
	"io"
	"mime"
	"net/http"
	"net/url"
	"path"
	"strconv"
	"strings"
	"time"

	"enclava-go/internal/auth"
	"enclava-go/internal/source"
)

type GitLabAdapter struct {
	project, ref, base, token string
	paths                     []string
	client                    *http.Client
}

type gitLabCursor struct {
	Run      syncRun `json:"run"`
	Phase    string  `json:"phase,omitempty"`
	Page     int     `json:"page,omitempty"`
	Index    int     `json:"index,omitempty"`
	Head     string  `json:"head,omitempty"`
	Previous string  `json:"previous,omitempty"`
	WebURL   string  `json:"web_url,omitempty"`
	Tree     bool    `json:"tree,omitempty"`
}

func NewGitLabAdapter(c source.Connection, secrets CredentialResolver) (Adapter, error) {
	if c.Kind != source.ConnectionGitLab {
		return nil, errors.New("invalid GitLab connection kind")
	}
	project := strings.TrimSpace(stringFromAny(c.Config["project"]))
	if project == "" {
		return nil, errors.New("gitlab config.project is required")
	}
	base := strings.TrimRight(firstNonEmpty(stringFromAny(c.Config["base_url"]), "https://gitlab.com"), "/")
	u, err := requireAPIOrigin(base)
	if err != nil || (u.Path != "" && u.Path != "/") {
		return nil, errors.New("gitlab config.base_url must be a valid API origin")
	}
	var token string
	if ref := strings.TrimSpace(c.CredentialRef); ref != "" {
		if secrets == nil {
			return nil, errors.New("gitlab credential resolver is not configured")
		}
		token, err = secrets.Resolve(c.TenantID, auth.SecretPurposeConnector, u.Scheme+"://"+u.Host, ref)
		if err != nil {
			return nil, err
		}
	}
	return &GitLabAdapter{project: project, ref: strings.TrimSpace(stringFromAny(c.Config["ref"])), base: base + "/api/v4", token: token, paths: stringListFromAny(c.Config["paths"]), client: newConnectorHTTPClient()}, nil
}

func (*GitLabAdapter) Capabilities() AdapterCapabilities { return AdapterCapabilities{Content: true} }

func (g *GitLabAdapter) get(ctx context.Context, endpoint string, raw bool, out any) error {
	for attempt := 0; attempt < 3; attempt++ {
		req, err := http.NewRequestWithContext(ctx, http.MethodGet, endpoint, nil)
		if err != nil {
			return errors.New("invalid gitlab request")
		}
		if g.token != "" {
			req.Header.Set("Authorization", "Bearer "+g.token)
		}
		resp, err := g.client.Do(req)
		if err != nil {
			return fmt.Errorf("gitlab request failed: %w", err)
		}
		if resp.StatusCode >= 200 && resp.StatusCode < 300 {
			if raw {
				var data []byte
				data, err = io.ReadAll(io.LimitReader(resp.Body, (8<<20)+1))
				if err == nil {
					*out.(*[]byte) = data
				}
			} else {
				err = json.NewDecoder(resp.Body).Decode(out)
			}
			_ = resp.Body.Close()
			return err
		}
		status, retry := resp.StatusCode, resp.Header.Get("Retry-After")
		_ = resp.Body.Close()
		if status != http.StatusTooManyRequests || attempt == 2 {
			return fmt.Errorf("gitlab API returned status %d", status)
		}
		delay := time.Second
		if n, e := strconv.Atoi(retry); e == nil && n > 0 {
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
	return errors.New("gitlab rate-limit retry bound exceeded")
}

func (g *GitLabAdapter) projectURL() string { return g.base + "/projects/" + url.PathEscape(g.project) }

func (g *GitLabAdapter) allowed(p string) bool {
	if len(g.paths) == 0 {
		return true
	}
	for _, path := range g.paths {
		path = strings.Trim(strings.TrimSpace(path), "/")
		if p == path || strings.HasPrefix(p, path+"/") {
			return true
		}
	}
	return false
}

func (g *GitLabAdapter) Content(ctx context.Context, c source.Connection, cursor Cursor, limit int) (ContentPage, error) {
	if limit < 1 || limit > 100 {
		limit = 100
	}
	var s gitLabCursor
	if err := decodeJSONCursor(cursor, &s); err != nil {
		return ContentPage{}, err
	}
	if s.Run.Started == "" {
		s.Run = beginRun(c.Config, s.Run, time.Now())
		s.Previous, s.Head, s.Phase, s.Page, s.Index, s.Tree = s.Head, "", "files", 1, 0, s.Run.Full
		var project struct {
			DefaultBranch string `json:"default_branch"`
			WebURL        string `json:"web_url"`
		}
		if err := g.get(ctx, g.projectURL(), false, &project); err != nil {
			return ContentPage{}, err
		}
		ref := firstNonEmpty(g.ref, project.DefaultBranch)
		if ref == "" {
			return ContentPage{}, errors.New("gitlab project has no default branch")
		}
		var branch struct {
			Commit struct {
				ID string `json:"id"`
			} `json:"commit"`
		}
		if err := g.get(ctx, g.projectURL()+"/repository/branches/"+url.PathEscape(ref), false, &branch); err != nil {
			return ContentPage{}, err
		}
		if branch.Commit.ID == "" {
			return ContentPage{}, errors.New("gitlab branch has no commit")
		}
		s.Head, s.WebURL = branch.Commit.ID, strings.TrimRight(project.WebURL, "/")
		if !s.Run.Full && s.Previous == "" {
			s.Tree = true
		}
	}
	if s.Page < 1 {
		s.Page = 1
	}
	switch s.Phase {
	case "files":
		return g.files(ctx, c, s, limit)
	case "issues", "merge_requests":
		return g.discussions(ctx, c, s, limit)
	default:
		return ContentPage{}, errors.New("invalid gitlab cursor phase")
	}
}

func gitLabPage(items []ContentItem, s gitLabCursor) ContentPage {
	return ContentPage{Items: items, NextCursor: encodeJSONCursor(s), Completeness: CompletenessIncomplete, Snapshot: s.Run.Full}
}

type gitLabTreeEntry struct {
	Path string `json:"path"`
	Type string `json:"type"`
}
type gitLabDiff struct {
	NewPath  string `json:"new_path"`
	Deleted  bool   `json:"deleted_file"`
	Overflow bool   `json:"overflow"`
}

func (g *GitLabAdapter) files(ctx context.Context, c source.Connection, s gitLabCursor, limit int) (ContentPage, error) {
	var entries []gitLabTreeEntry
	if !s.Tree && s.Previous != s.Head {
		var compare struct {
			Diffs   []gitLabDiff `json:"diffs"`
			Timeout bool         `json:"compare_timeout"`
		}
		endpoint := g.projectURL() + "/repository/compare?from=" + url.QueryEscape(s.Previous) + "&to=" + url.QueryEscape(s.Head)
		err := g.get(ctx, endpoint, false, &compare)
		if ctx.Err() != nil {
			return ContentPage{}, ctx.Err()
		}
		if err != nil || compare.Timeout {
			s.Tree = true
		} else {
			for _, d := range compare.Diffs {
				if d.Overflow {
					s.Tree = true
					break
				}
				if !d.Deleted {
					entries = append(entries, gitLabTreeEntry{Path: d.NewPath, Type: "blob"})
				}
			}
		}
	}
	if s.Tree {
		entries = nil
		endpoint := fmt.Sprintf("%s/repository/tree?recursive=true&ref=%s&per_page=100&page=%d", g.projectURL(), url.QueryEscape(s.Head), s.Page)
		if err := g.get(ctx, endpoint, false, &entries); err != nil {
			return ContentPage{}, err
		}
	}
	if s.Index < 0 || s.Index > len(entries) {
		return ContentPage{}, errors.New("invalid gitlab file cursor")
	}
	items := make([]ContentItem, 0, limit)
	parent := "gitlab:" + c.ID + ":" + g.project
	for s.Index < len(entries) && len(items) < limit {
		e := entries[s.Index]
		s.Index++
		if e.Type != "blob" || !g.allowed(e.Path) {
			continue
		}
		var data []byte
		endpoint := g.projectURL() + "/repository/files/" + url.PathEscape(e.Path) + "/raw?ref=" + url.QueryEscape(s.Head)
		if err := g.get(ctx, endpoint, true, &data); err != nil {
			return ContentPage{}, err
		}
		if len(data) > 8<<20 {
			continue
		}
		contentType := firstNonEmpty(mime.TypeByExtension(path.Ext(e.Path)), "application/octet-stream")
		items = append(items, ContentItem{NativeID: parent + ":" + e.Path, ParentNativeID: parent, ParentKind: "repository", Kind: "file", NativeRevision: s.Head, Title: e.Path, Filename: e.Path, MIMEType: contentType, Content: data, URL: s.WebURL + "/-/blob/" + url.PathEscape(s.Head) + "/" + escapePathSegments(e.Path)})
	}
	if s.Index < len(entries) {
		return gitLabPage(items, s), nil
	}
	if s.Tree && len(entries) == 100 {
		s.Page++
		s.Index = 0
		return gitLabPage(items, s), nil
	}
	s.Phase, s.Page, s.Index = "issues", 1, 0
	return gitLabPage(items, s), nil
}

type gitLabUser struct {
	Name     string `json:"name"`
	Username string `json:"username"`
}

func (u gitLabUser) display() string { return firstNonEmpty(u.Name, u.Username) }

type gitLabDiscussion struct {
	IID         int          `json:"iid"`
	Title       string       `json:"title"`
	Description string       `json:"description"`
	State       string       `json:"state"`
	WebURL      string       `json:"web_url"`
	CreatedAt   string       `json:"created_at"`
	UpdatedAt   string       `json:"updated_at"`
	Labels      []string     `json:"labels"`
	Assignees   []gitLabUser `json:"assignees"`
	Author      gitLabUser   `json:"author"`
	Milestone   *struct {
		Title string `json:"title"`
	} `json:"milestone"`
	SourceBranch string `json:"source_branch"`
	TargetBranch string `json:"target_branch"`
}
type gitLabNote struct {
	Body      string     `json:"body"`
	System    bool       `json:"system"`
	Author    gitLabUser `json:"author"`
	CreatedAt string     `json:"created_at"`
}

func gitLabTime(raw string) *time.Time {
	t, err := time.Parse(time.RFC3339Nano, raw)
	if err != nil {
		return nil
	}
	t = t.UTC()
	return &t
}
func gitLabTimeText(raw string) string {
	if t := gitLabTime(raw); t != nil {
		return t.Format("2006-01-02 15:04 UTC")
	}
	return raw
}

func (g *GitLabAdapter) discussions(ctx context.Context, c source.Connection, s gitLabCursor, limit int) (ContentPage, error) {
	query := url.Values{"state": {"all"}, "order_by": {"created_at"}, "sort": {"asc"}, "per_page": {strconv.Itoa(limit)}, "page": {strconv.Itoa(s.Page)}}
	if !s.Run.Full {
		query.Set("updated_after", s.Run.Since)
	}
	endpoint := g.projectURL() + "/" + s.Phase
	var rows []gitLabDiscussion
	if err := g.get(ctx, endpoint+"?"+query.Encode(), false, &rows); err != nil {
		return ContentPage{}, err
	}
	items := make([]ContentItem, 0, len(rows))
	parent := "gitlab:" + c.ID + ":" + g.project
	for _, row := range rows {
		identifier, kind, filename := fmt.Sprintf("#%d", row.IID), "issue", fmt.Sprintf("issue-%d.md", row.IID)
		if s.Phase == "merge_requests" {
			identifier, kind, filename = fmt.Sprintf("!%d", row.IID), "merge_request", fmt.Sprintf("mr-%d.md", row.IID)
		}
		title := identifier + " " + row.Title
		assignees := make([]string, 0, len(row.Assignees))
		for _, u := range row.Assignees {
			assignees = append(assignees, u.display())
		}
		milestone := ""
		if row.Milestone != nil {
			milestone = row.Milestone.Title
		}
		var text strings.Builder
		fmt.Fprintf(&text, "# %s\n\nState: %s | Labels: %s | Assignees: %s | Author: %s | Milestone: %s | Project: %s", title, row.State, strings.Join(row.Labels, ", "), strings.Join(assignees, ", "), row.Author.display(), milestone, g.project)
		if kind == "merge_request" {
			fmt.Fprintf(&text, " | Branch: %s → %s", row.SourceBranch, row.TargetBranch)
		}
		fmt.Fprintf(&text, "\nCreated: %s | Updated: %s\n\n%s\n\n## Comments\n", gitLabTimeText(row.CreatedAt), gitLabTimeText(row.UpdatedAt), row.Description)
		for page, count := 1, 0; g.token != "" && count < 1000; page++ {
			var notes []gitLabNote
			if err := g.get(ctx, fmt.Sprintf("%s/%d/notes?sort=asc&order_by=created_at&per_page=100&page=%d", endpoint, row.IID, page), false, &notes); err != nil {
				return ContentPage{}, err
			}
			for _, note := range notes {
				if !note.System && count < 1000 {
					fmt.Fprintf(&text, "\n**%s** — %s\n\n%s\n", note.Author.display(), gitLabTimeText(note.CreatedAt), note.Body)
					count++
				}
			}
			if len(notes) < 100 {
				break
			}
		}
		items = append(items, ContentItem{NativeID: fmt.Sprintf("%s:%s:%d", parent, kind, row.IID), ParentNativeID: parent, ParentKind: "repository", Kind: kind, NativeRevision: row.UpdatedAt, Title: title, Filename: filename, MIMEType: "text/markdown", Text: text.String(), URL: row.WebURL, CreatedAt: gitLabTime(row.CreatedAt), UpdatedAt: gitLabTime(row.UpdatedAt)})
	}
	if len(rows) == limit {
		s.Page++
		return gitLabPage(items, s), nil
	}
	if s.Phase == "issues" {
		s.Phase, s.Page = "merge_requests", 1
		return gitLabPage(items, s), nil
	}
	return ContentPage{Items: items, Completeness: CompletenessComplete, Snapshot: s.Run.Full, Checkpoint: encodeJSONCursor(gitLabCursor{Run: s.Run.finish(), Head: s.Head})}, nil
}
