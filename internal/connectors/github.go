package connectors

import (
	"context"
	"encoding/base64"
	"encoding/json"
	"errors"
	"fmt"
	"io"
	"net/http"
	"net/url"
	"strconv"
	"strings"
	"time"

	"enclava-go/internal/auth"
	"enclava-go/internal/source"
)

type GitHubAdapter struct {
	owner, repo, ref, token, base string
	paths                         []string
	client                        *http.Client
}
type ghCursor struct {
	Phase  string `json:"phase"`
	Page   int    `json:"page"`
	Index  int    `json:"index"`
	Offset int    `json:"offset,omitempty"`
	SHA    string `json:"tree_sha"`
}
type ghTreeEntry struct {
	Path string `json:"path"`
	Type string `json:"type"`
	SHA  string `json:"sha"`
	Size int64  `json:"size"`
}

func NewGitHubAdapter(c source.Connection, secrets *auth.SecretResolver) (Adapter, error) {
	owner, repo := strings.TrimSpace(stringFromAny(c.Config["owner"])), strings.TrimSpace(stringFromAny(c.Config["repo"]))
	if owner == "" || repo == "" {
		return nil, errors.New("github config.owner and config.repo are required")
	}
	base := strings.TrimRight(firstNonEmpty(stringFromAny(c.Config["api_url"]), "https://api.github.com"), "/")
	u, e := url.Parse(base)
	if e != nil || u.Scheme != "https" || u.Host == "" {
		return nil, errors.New("github API origin must be HTTPS")
	}
	token, e := secrets.Resolve(c.TenantID, auth.SecretPurposeConnector, u.Scheme+"://"+u.Host, c.CredentialRef)
	if e != nil {
		return nil, e
	}
	paths := stringListFromAny(c.Config["paths"])
	if len(paths) == 0 {
		if p := strings.TrimSpace(stringFromAny(c.Config["path"])); p != "" {
			paths = []string{p}
		}
	}
	return &GitHubAdapter{owner: owner, repo: repo, ref: firstNonEmpty(strings.TrimSpace(stringFromAny(c.Config["ref"])), "HEAD"), token: token, base: base, paths: paths, client: newConnectorHTTPClient()}, nil
}
func (g *GitHubAdapter) Capabilities() AdapterCapabilities {
	return AdapterCapabilities{Content: true, Access: true, Membership: true, UnsupportedScopes: []string{"per-file ACL (GitHub permissions apply at repository scope)"}}
}
func (g *GitHubAdapter) request(ctx context.Context, endpoint string, out any) error {
	for attempt := 0; attempt < 3; attempt++ {
		req, e := http.NewRequestWithContext(ctx, http.MethodGet, endpoint, nil)
		if e != nil {
			return e
		}
		req.Header.Set("Accept", "application/vnd.github+json")
		req.Header.Set("X-GitHub-Api-Version", "2022-11-28")
		if g.token != "" {
			req.Header.Set("Authorization", "Bearer "+g.token)
		}
		resp, e := g.client.Do(req)
		if e != nil {
			return e
		}
		if resp.StatusCode >= 200 && resp.StatusCode < 300 {
			e = json.NewDecoder(resp.Body).Decode(out)
			_ = resp.Body.Close()
			return e
		}
		body, _ := io.ReadAll(io.LimitReader(resp.Body, 4096))
		retryAfter := resp.Header.Get("Retry-After")
		status := resp.StatusCode
		_ = resp.Body.Close()
		if attempt == 2 || (status != http.StatusTooManyRequests && status != http.StatusForbidden) || (!strings.Contains(strings.ToLower(string(body)), "secondary rate limit") && status == http.StatusForbidden) {
			return fmt.Errorf("github API returned status %d", status)
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
	return errors.New("github rate-limit retry bound exceeded")
}
func (g *GitHubAdapter) Content(ctx context.Context, c source.Connection, cursor Cursor, limit int) (ContentPage, error) {
	if limit < 1 || limit > 100 {
		limit = 100
	}
	s := ghCursor{Phase: "tree", Page: 1}
	if cursor != "" {
		if e := json.Unmarshal([]byte(cursor), &s); e != nil {
			return ContentPage{}, errors.New("invalid github cursor")
		}
	}
	if s.Phase == "" {
		s.Phase = "tree"
	}
	if s.Page < 1 {
		s.Page = 1
	}
	switch s.Phase {
	case "tree":
		return g.treePage(ctx, c, s, limit)
	case "issues", "pulls":
		return g.discussionPage(ctx, c, s, limit)
	default:
		return ContentPage{}, errors.New("invalid github cursor phase")
	}
}
func (g *GitHubAdapter) treeEntries(ctx context.Context, sha string) ([]ghTreeEntry, error) {
	endpoint := g.base + "/repos/" + url.PathEscape(g.owner) + "/" + url.PathEscape(g.repo) + "/git/trees/" + url.PathEscape(sha) + "?recursive=1"
	var tree struct {
		Tree      []ghTreeEntry `json:"tree"`
		Truncated bool          `json:"truncated"`
	}
	if e := g.request(ctx, endpoint, &tree); e != nil {
		return nil, e
	}
	if !tree.Truncated {
		return tree.Tree, nil
	}
	var root struct {
		Tree []ghTreeEntry `json:"tree"`
	}
	endpoint = g.base + "/repos/" + url.PathEscape(g.owner) + "/" + url.PathEscape(g.repo) + "/git/trees/" + url.PathEscape(sha)
	if e := g.request(ctx, endpoint, &root); e != nil {
		return nil, e
	}
	out := make([]ghTreeEntry, 0, len(root.Tree))
	for _, entry := range root.Tree {
		if entry.Type == "tree" {
			sub, e := g.treeEntries(ctx, entry.SHA)
			if e != nil {
				return nil, e
			}
			for i := range sub {
				sub[i].Path = entry.Path + "/" + sub[i].Path
			}
			out = append(out, sub...)
		} else {
			out = append(out, entry)
		}
	}
	return out, nil
}
func (g *GitHubAdapter) treePage(ctx context.Context, c source.Connection, s ghCursor, limit int) (ContentPage, error) {
	if s.SHA == "" {
		if g.ref == "HEAD" {
			var commit struct {
				SHA string `json:"sha"`
			}
			e := g.request(ctx, g.base+"/repos/"+url.PathEscape(g.owner)+"/"+url.PathEscape(g.repo)+"/commits/HEAD", &commit)
			if e != nil {
				return ContentPage{}, e
			}
			s.SHA = commit.SHA
		} else {
			var ref struct {
				Object struct {
					SHA string `json:"sha"`
				} `json:"object"`
			}
			e := g.request(ctx, g.base+"/repos/"+url.PathEscape(g.owner)+"/"+url.PathEscape(g.repo)+"/git/ref/heads/"+url.PathEscape(g.ref), &ref)
			if e != nil {
				return ContentPage{}, e
			}
			s.SHA = ref.Object.SHA
		}
	}
	tree, e := g.treeEntries(ctx, s.SHA)
	if e != nil {
		return ContentPage{}, e
	}
	items := make([]ContentItem, 0, limit)
	for s.Index < len(tree) && len(items) < limit {
		entry := tree[s.Index]
		s.Index++
		if entry.Type != "blob" || entry.Size > 8<<20 || !g.allowed(entry.Path) {
			continue
		}
		var blob struct {
			Content  string `json:"content"`
			Encoding string `json:"encoding"`
			SHA      string `json:"sha"`
		}
		endpoint := g.base + "/repos/" + url.PathEscape(g.owner) + "/" + url.PathEscape(g.repo) + "/contents/" + escapePathSegments(entry.Path) + "?ref=" + url.QueryEscape(s.SHA)
		if e := g.request(ctx, endpoint, &blob); e != nil {
			return ContentPage{}, e
		}
		data := []byte(blob.Content)
		if blob.Encoding == "base64" {
			data, e = base64.StdEncoding.DecodeString(strings.Join(strings.Fields(blob.Content), ""))
			if e != nil {
				return ContentPage{}, e
			}
		}
		items = append(items, ContentItem{NativeID: "github:" + c.ID + ":" + g.owner + "/" + g.repo + ":" + entry.Path, ParentNativeID: "github:" + c.ID + ":" + g.owner + "/" + g.repo, ParentKind: "repository", Kind: "file", NativeRevision: s.SHA + ":" + firstNonEmpty(blob.SHA, entry.SHA), Title: entry.Path, Filename: entry.Path, Content: data, URL: "https://github.com/" + g.owner + "/" + g.repo + "/blob/" + s.SHA + "/" + entry.Path})
	}
	if s.Index < len(tree) {
		return ghPage(items, s), nil
	}
	return ghPage(items, ghCursor{Phase: "issues", Page: 1, SHA: s.SHA}), nil
}
func (g *GitHubAdapter) discussionPage(ctx context.Context, c source.Connection, s ghCursor, limit int) (ContentPage, error) {
	kind := s.Phase
	endpoint := fmt.Sprintf("%s/repos/%s/%s/%s?state=all&per_page=%d&page=%d", g.base, url.PathEscape(g.owner), url.PathEscape(g.repo), kind, limit, s.Page)
	var rows []struct {
		Number      int            `json:"number"`
		ID          int64          `json:"id"`
		Title       string         `json:"title"`
		Body        string         `json:"body"`
		UpdatedAt   string         `json:"updated_at"`
		HTMLURL     string         `json:"html_url"`
		PullRequest map[string]any `json:"pull_request"`
	}
	if e := g.request(ctx, endpoint, &rows); e != nil {
		return ContentPage{}, e
	}
	items := make([]ContentItem, 0, limit)
	for s.Index < len(rows) {
		r := rows[s.Index]
		if kind == "issues" && r.PullRequest != nil {
			s.Index++
			continue
		}
		id := fmt.Sprintf("github:%s:%s:%s:%d", c.ID, g.owner+"/"+g.repo, kind, r.ID)
		parent := []ContentItem{{NativeID: id, ParentNativeID: "github:" + c.ID + ":" + g.owner + "/" + g.repo, ParentKind: "repository", Kind: kind, NativeRevision: r.UpdatedAt, Title: r.Title, Filename: r.Title, Text: r.Body, URL: r.HTMLURL}}
		comments, e := g.comments(ctx, c, fmt.Sprintf("%s/repos/%s/%s/issues/%d/comments", g.base, url.PathEscape(g.owner), url.PathEscape(g.repo), r.Number), id, kind, r.Title, "comment")
		if e != nil {
			return ContentPage{}, e
		}
		parent = append(parent, comments...)
		if kind == "pulls" {
			comments, e = g.comments(ctx, c, fmt.Sprintf("%s/repos/%s/%s/pulls/%d/comments", g.base, url.PathEscape(g.owner), url.PathEscape(g.repo), r.Number), id, kind, r.Title, "review_comment")
			if e != nil {
				return ContentPage{}, e
			}
			parent = append(parent, comments...)
		}
		if s.Offset < 0 || s.Offset > len(parent) {
			return ContentPage{}, errors.New("invalid github discussion cursor")
		}
		end := min(len(parent), s.Offset+limit-len(items))
		items = append(items, parent[s.Offset:end]...)
		if end < len(parent) {
			s.Offset = end
			return ghPage(items, s), nil
		}
		s.Index++
		s.Offset = 0
		if len(items) == limit && s.Index < len(rows) {
			return ghPage(items, s), nil
		}
	}
	if len(rows) == limit {
		s.Page++
		s.Index = 0
		return ghPage(items, s), nil
	}
	if kind == "issues" {
		return ghPage(items, ghCursor{Phase: "pulls", Page: 1, SHA: s.SHA}), nil
	}
	return ContentPage{Items: items, Completeness: CompletenessComplete, Snapshot: true}, nil
}
func (g *GitHubAdapter) comments(ctx context.Context, c source.Connection, endpoint, parent, parentKind, title, kind string) ([]ContentItem, error) {
	var out []ContentItem
	for page := 1; page <= 100; page++ {
		var rows []struct {
			ID        int64  `json:"id"`
			Body      string `json:"body"`
			UpdatedAt string `json:"updated_at"`
			HTMLURL   string `json:"html_url"`
		}
		if e := g.request(ctx, fmt.Sprintf("%s?per_page=100&page=%d", endpoint, page), &rows); e != nil {
			return nil, e
		}
		for _, r := range rows {
			out = append(out, ContentItem{NativeID: fmt.Sprintf("github:%s:%s:%s:%d", c.ID, g.owner+"/"+g.repo, kind, r.ID), ParentNativeID: parent, ParentKind: parentKind, Kind: kind, NativeRevision: r.UpdatedAt, Title: title, Text: r.Body, URL: r.HTMLURL})
		}
		if len(rows) < 100 {
			return out, nil
		}
	}
	return nil, errors.New("github comment pagination exceeded 100 pages")
}
func ghPage(items []ContentItem, s ghCursor) ContentPage {
	b, _ := json.Marshal(s)
	return ContentPage{Items: items, NextCursor: Cursor(b), Completeness: CompletenessIncomplete}
}
func (g *GitHubAdapter) allowed(p string) bool {
	if len(g.paths) == 0 {
		return true
	}
	for _, x := range g.paths {
		x = strings.Trim(strings.TrimSpace(x), "/")
		if p == x || strings.HasPrefix(p, x+"/") {
			return true
		}
	}
	return false
}
func (g *GitHubAdapter) Access(ctx context.Context, c source.Connection, cursor Cursor, limit int) (AccessPage, error) {
	x, n, e := g.collaborators(ctx, c, cursor, limit)
	complete := n == ""
	cmp := CompletenessIncomplete
	if complete {
		cmp = CompletenessComplete
	}
	return AccessPage{Grants: x, NextCursor: Cursor(n), Completeness: cmp, Snapshot: complete}, e
}
func (g *GitHubAdapter) Membership(ctx context.Context, c source.Connection, cursor Cursor, limit int) (MembershipPage, error) {
	x, n, e := g.collaborators(ctx, c, cursor, limit)
	complete := n == ""
	cmp := CompletenessIncomplete
	if complete {
		cmp = CompletenessComplete
	}
	return MembershipPage{Memberships: x, NextCursor: Cursor(n), Completeness: cmp, Snapshot: complete}, e
}
func (g *GitHubAdapter) collaborators(ctx context.Context, c source.Connection, cursor Cursor, limit int) ([]AccessGrant, string, error) {
	if limit < 1 || limit > 100 {
		limit = 100
	}
	page := 1
	if cursor != "" {
		n, e := strconv.Atoi(string(cursor))
		if e != nil || n < 1 {
			return nil, "", errors.New("invalid github cursor")
		}
		page = n
	}
	var users []struct {
		Login string `json:"login"`
		ID    int64  `json:"id"`
	}
	endpoint := fmt.Sprintf("%s/repos/%s/%s/collaborators?per_page=%d&page=%d", g.base, url.PathEscape(g.owner), url.PathEscape(g.repo), limit, page)
	if e := g.request(ctx, endpoint, &users); e != nil {
		return nil, "", e
	}
	out := make([]AccessGrant, 0, len(users))
	for _, u := range users {
		id := u.Login
		if id == "" {
			id = strconv.FormatInt(u.ID, 10)
		}
		out = append(out, AccessGrant{NativeObjectID: "github:" + c.ID + ":" + g.owner + "/" + g.repo, NativeObjectKind: "repository", NativePrincipalID: id, NativePrincipalName: u.Login, NativeGroupID: g.owner + "/" + g.repo, Scope: "read", Effect: "allow"})
	}
	next := ""
	if len(users) == limit {
		next = strconv.Itoa(page + 1)
	}
	return out, next, nil
}

var _ Adapter = (*GitHubAdapter)(nil)
