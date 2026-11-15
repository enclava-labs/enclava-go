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
	"regexp"
	"sort"
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
	Phase string `json:"phase"`
	Page  int    `json:"page"`
	Index int    `json:"index"`
	SHA   string `json:"tree_sha"`
}
type ghTreeEntry struct {
	Path string `json:"path"`
	Type string `json:"type"`
	SHA  string `json:"sha"`
	Size int64  `json:"size"`
}

func NewGitHubAdapter(c source.Connection, secrets CredentialResolver) (Adapter, error) {
	owner, repo := strings.TrimSpace(stringFromAny(c.Config["owner"])), strings.TrimSpace(stringFromAny(c.Config["repo"]))
	if owner == "" || repo == "" {
		return nil, errors.New("github config.owner and config.repo are required")
	}
	base := strings.TrimRight(firstNonEmpty(stringFromAny(c.Config["api_url"]), "https://api.github.com"), "/")
	u, e := url.Parse(base)
	if e != nil || u.Scheme != "https" || u.Host == "" {
		return nil, errors.New("github API origin must be HTTPS")
	}
	var token string
	if ref := strings.TrimSpace(c.CredentialRef); ref != "" {
		if secrets == nil {
			return nil, errors.New("github credential resolver is not configured")
		}
		if token, e = secrets.Resolve(c.TenantID, auth.SecretPurposeConnector, u.Scheme+"://"+u.Host, ref); e != nil {
			return nil, e
		}
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
	return AdapterCapabilities{Content: true}
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
		items = append(items, ContentItem{NativeID: "github:" + c.ID + ":" + g.owner + "/" + g.repo + ":" + entry.Path, ParentNativeID: "github:" + c.ID + ":" + g.owner + "/" + g.repo, ParentKind: "repository", Kind: "file", NativeRevision: s.SHA + ":" + firstNonEmpty(blob.SHA, entry.SHA), Title: entry.Path, Filename: entry.Path, MIMEType: repositoryFileMIME(entry.Path, data), Content: data, URL: "https://github.com/" + g.owner + "/" + g.repo + "/blob/" + s.SHA + "/" + entry.Path})
	}
	if s.Index < len(tree) {
		return ghPage(items, s), nil
	}
	return ghPage(items, ghCursor{Phase: "issues", Page: 1, SHA: s.SHA}), nil
}

// ghDiscussion is one row of the issues or pulls list endpoints; pull
// requests appear in both (issues rows carry pull_request).
type ghDiscussion struct {
	Number      int            `json:"number"`
	ID          int64          `json:"id"`
	Title       string         `json:"title"`
	Body        string         `json:"body"`
	CreatedAt   string         `json:"created_at"`
	UpdatedAt   string         `json:"updated_at"`
	HTMLURL     string         `json:"html_url"`
	PullRequest map[string]any `json:"pull_request"`
	User        struct {
		Login string `json:"login"`
	} `json:"user"`
}

// ghComment is one row of the issue comments, review comments or reviews
// endpoints. Reviews carry submitted_at instead of created_at.
type ghComment struct {
	ID          int64  `json:"id"`
	Body        string `json:"body"`
	HTMLURL     string `json:"html_url"`
	CreatedAt   string `json:"created_at"`
	SubmittedAt string `json:"submitted_at"`
	User        struct {
		Login string `json:"login"`
	} `json:"user"`
}

func (g *GitHubAdapter) discussionPage(ctx context.Context, c source.Connection, s ghCursor, limit int) (ContentPage, error) {
	kind := s.Phase
	endpoint := fmt.Sprintf("%s/repos/%s/%s/%s?state=all&per_page=%d&page=%d", g.base, url.PathEscape(g.owner), url.PathEscape(g.repo), kind, limit, s.Page)
	var rows []ghDiscussion
	if e := g.request(ctx, endpoint, &rows); e != nil {
		return ContentPage{}, e
	}
	if s.Index < 0 {
		return ContentPage{}, errors.New("invalid github discussion cursor")
	}
	if s.Index > len(rows) {
		// The upstream listing shrank since the cursor was taken; every row
		// it pointed at was already emitted.
		s.Index = len(rows)
	}
	items := make([]ContentItem, 0, limit)
	for s.Index < len(rows) && len(items) < limit {
		r := rows[s.Index]
		s.Index++
		if kind == "issues" && r.PullRequest != nil {
			continue
		}
		item, e := g.discussionItem(ctx, c, kind, r)
		if e != nil {
			return ContentPage{Completeness: CompletenessIncomplete}, e
		}
		items = append(items, item)
	}
	if s.Index < len(rows) {
		return ghPage(items, s), nil
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

// discussionItem builds the single document for one issue or pull request:
// a title heading, the body as markdown structure blocks and one message
// block per comment, review comment and review with a body.
func (g *GitHubAdapter) discussionItem(ctx context.Context, c source.Connection, kind string, r ghDiscussion) (ContentItem, error) {
	singular := "issue"
	if kind == "pulls" {
		singular = "pull"
	}
	title := fmt.Sprintf("#%d: %s", r.Number, r.Title)
	author := r.User.Login
	blocks := make([]source.StructuredBlock, 0, 16)
	blocks = append(blocks, source.StructuredBlock{
		Kind: source.StructuredHeading, Level: 1, Text: title,
		URL: r.HTMLURL, Author: author, CreatedAt: r.CreatedAt,
		NativeID: "title",
	})
	blocks = append(blocks, markdownStructureBlocks(r.Body, r.HTMLURL, author, r.CreatedAt)...)

	type message struct {
		created string
		block   source.StructuredBlock
	}
	var messages []message
	// Before v1.2 issue and review comments were their own items; the
	// discussion document retires them.
	var superseded []string
	collect := func(comments []ghComment, nativeKind string) {
		for _, cm := range comments {
			if nativeKind != "review" {
				superseded = append(superseded, fmt.Sprintf("github:%s:%s:%s:%d", c.ID, g.owner+"/"+g.repo, nativeKind, cm.ID))
			}
			if strings.TrimSpace(cm.Body) == "" {
				continue
			}
			created := firstNonEmpty(cm.CreatedAt, cm.SubmittedAt)
			messages = append(messages, message{created: created, block: source.StructuredBlock{
				Kind:      source.StructuredMessage,
				Text:      cm.Body,
				URL:       cm.HTMLURL,
				Author:    cm.User.Login,
				CreatedAt: created,
				NativeID:  nativeKind + ":" + strconv.FormatInt(cm.ID, 10),
				// Comments hang off the title heading so mixed-mode chunking
				// packs them as one discussion under the issue title.
				ParentNativeID: "title",
			}})
		}
	}
	base := fmt.Sprintf("%s/repos/%s/%s", g.base, url.PathEscape(g.owner), url.PathEscape(g.repo))
	comments, e := g.discussionComments(ctx, fmt.Sprintf("%s/issues/%d/comments", base, r.Number))
	if e != nil {
		return ContentItem{}, e
	}
	collect(comments, "comment")
	if kind == "pulls" {
		reviewComments, e := g.discussionComments(ctx, fmt.Sprintf("%s/pulls/%d/comments", base, r.Number))
		if e != nil {
			return ContentItem{}, e
		}
		collect(reviewComments, "review_comment")
		reviews, e := g.discussionComments(ctx, fmt.Sprintf("%s/pulls/%d/reviews", base, r.Number))
		if e != nil {
			return ContentItem{}, e
		}
		collect(reviews, "review")
	}
	sort.SliceStable(messages, func(i, j int) bool {
		ti, tj := gitLabTime(messages[i].created), gitLabTime(messages[j].created)
		if ti != nil && tj != nil {
			return ti.Before(*tj)
		}
		return messages[i].created < messages[j].created
	})
	authors := []string{}
	seenAuthor := map[string]bool{}
	addAuthor := func(login string) {
		if login == "" || seenAuthor[login] {
			return
		}
		seenAuthor[login] = true
		authors = append(authors, login)
	}
	addAuthor(author)
	for _, m := range messages {
		blocks = append(blocks, m.block)
		addAuthor(m.block.Author)
	}
	return ContentItem{
		NativeID:       fmt.Sprintf("github:%s:%s:%s:%d", c.ID, g.owner+"/"+g.repo, kind, r.ID),
		ParentNativeID: "github:" + c.ID + ":" + g.owner + "/" + g.repo,
		ParentKind:     "repository",
		Kind:           kind,
		NativeRevision: r.UpdatedAt,
		Title:          title,
		Filename:       singular + "-" + strconv.Itoa(r.Number) + ".blocks.json",
		URL:            r.HTMLURL,
		CreatedAt:      gitLabTime(r.CreatedAt),
		UpdatedAt:      gitLabTime(r.UpdatedAt),
		Blocks:         blocks,
		Container:      g.owner + "/" + g.repo,
		Authors:        authors,
		Supersedes:     superseded,
	}, nil
}

// ghCommentPageLimit bounds comment pagination per endpoint; hitting it
// means the discussion document would be silently incomplete, so the page
// fails instead (an incomplete page never authorizes deletion).
const ghCommentPageLimit = 100

func (g *GitHubAdapter) discussionComments(ctx context.Context, endpoint string) ([]ghComment, error) {
	var out []ghComment
	for page := 1; page <= ghCommentPageLimit; page++ {
		var rows []ghComment
		if e := g.request(ctx, fmt.Sprintf("%s?per_page=100&page=%d", endpoint, page), &rows); e != nil {
			return nil, e
		}
		out = append(out, rows...)
		if len(rows) < 100 {
			return out, nil
		}
	}
	return nil, fmt.Errorf("github comment pagination exceeded %d pages for %s", ghCommentPageLimit, endpoint)
}

var (
	ghHeading      = regexp.MustCompile(`^ {0,3}(#{1,6})[ \t]+(.*)$`)
	ghHeadingClose = regexp.MustCompile(`[ \t]+#+[ \t]*$`)
	ghListItem     = regexp.MustCompile(`^ {0,3}(?:[-*+]|\d{1,9}[.)])[ \t]+(.*)$`)
)

// markdownStructureBlocks splits an issue or pull request body into heading,
// paragraph, list_item and code blocks. It is deliberately line-based and
// forgiving: anything unrecognized stays paragraph text and an unclosed
// fence still yields its code block.
func markdownStructureBlocks(body, link, author, createdAt string) []source.StructuredBlock {
	var blocks []source.StructuredBlock
	emit := func(b source.StructuredBlock) {
		b.URL, b.Author, b.CreatedAt = link, author, createdAt
		blocks = append(blocks, b)
	}
	var paragraph []string
	flushParagraph := func() {
		if text := strings.TrimSpace(strings.Join(paragraph, "\n")); text != "" {
			emit(source.StructuredBlock{Kind: source.StructuredParagraph, Text: text})
		}
		paragraph = nil
	}
	var code []string
	codeLanguage := ""
	fenceChar, fenceLen := byte(0), 0
	inFence := false
	body = strings.ReplaceAll(body, "\r\n", "\n")
	for _, line := range strings.Split(body, "\n") {
		if inFence {
			if ch, n, rest, ok := fenceRun(line); ok && ch == fenceChar && n >= fenceLen && strings.TrimSpace(rest) == "" {
				inFence = false
				emit(source.StructuredBlock{Kind: source.StructuredCode, Text: strings.Join(code, "\n"), Language: codeLanguage})
				code, codeLanguage = nil, ""
			} else {
				code = append(code, line)
			}
			continue
		}
		if ch, n, rest, ok := fenceRun(line); ok {
			flushParagraph()
			inFence, fenceChar, fenceLen = true, ch, n
			if fields := strings.Fields(rest); len(fields) > 0 {
				codeLanguage = fields[0]
			}
			continue
		}
		if strings.TrimSpace(line) == "" {
			flushParagraph()
			continue
		}
		if m := ghHeading.FindStringSubmatch(line); m != nil {
			flushParagraph()
			text := strings.TrimSpace(ghHeadingClose.ReplaceAllString(strings.TrimSpace(m[2]), ""))
			if text != "" {
				// Body headings sit below the issue title (level 1).
				emit(source.StructuredBlock{Kind: source.StructuredHeading, Level: min(len(m[1])+1, 6), Text: text})
			}
			continue
		}
		if m := ghListItem.FindStringSubmatch(line); m != nil {
			flushParagraph()
			if text := strings.TrimSpace(m[1]); text != "" {
				emit(source.StructuredBlock{Kind: source.StructuredListItem, Text: text})
			}
			continue
		}
		paragraph = append(paragraph, strings.TrimRight(line, " \t"))
	}
	flushParagraph()
	if inFence {
		emit(source.StructuredBlock{Kind: source.StructuredCode, Text: strings.Join(code, "\n"), Language: codeLanguage})
	}
	return blocks
}

// fenceRun reports a fenced-code marker: up to three leading spaces, then at
// least three backticks or tildes; rest is everything after the run.
func fenceRun(line string) (ch byte, n int, rest string, ok bool) {
	i := 0
	for i < len(line) && line[i] == ' ' {
		i++
	}
	if i > 3 || i >= len(line) {
		return 0, 0, "", false
	}
	if line[i] != '`' && line[i] != '~' {
		return 0, 0, "", false
	}
	j := i
	for j < len(line) && line[j] == line[i] {
		j++
	}
	if j-i < 3 {
		return 0, 0, "", false
	}
	return line[i], j - i, line[j:], true
}

func ghPage(items []ContentItem, s ghCursor) ContentPage {
	b, _ := json.Marshal(s)
	// Every GitHub listing is a full snapshot of the configured scope, so
	// every page reports Snapshot: a completed listing may tombstone objects
	// the snapshot no longer contains (for example legacy per-comment items).
	return ContentPage{Items: items, NextCursor: Cursor(b), Completeness: CompletenessIncomplete, Snapshot: true}
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

var _ Adapter = (*GitHubAdapter)(nil)
