package connectors

import (
	"context"
	"encoding/json"
	"errors"
	"fmt"
	"log"
	"net/http"
	"net/url"
	"strconv"
	"strings"
	"sync"
	"time"

	"enclava-go/internal/auth"
	"enclava-go/internal/source"
)

const (
	notionMaxDepth  = 8
	notionMaxItems  = 500
	notionAPIOrigin = "https://api.notion.com"
)

type notionSelection struct {
	PageID string
}

type NotionAdapter struct {
	baseURL              string
	token                string
	tenantID             string
	connectionID         string
	selections           []notionSelection
	maxDepth             int
	maxItems             int
	client               *http.Client
	requestMu            sync.Mutex
	nextRequest          time.Time
	snapshotMu           sync.Mutex
	contentSnapshot      []ContentItem
	contentSnapshotReady bool
}

// NewNotionAdapter builds a publisher-selected Notion adapter. Traversal is
// bounded to 500 items and depth 8 by default; config may lower either bound.
func NewNotionAdapter(conn source.Connection, resolver CredentialResolver) (Adapter, error) {
	if conn.Kind != source.ConnectionNotion {
		return nil, errors.New("notion adapter kind does not match connection")
	}
	ref := strings.TrimSpace(conn.CredentialRef)
	if ref == "" {
		return nil, errors.New("notion credential_ref is required")
	}
	base := strings.TrimRight(strings.TrimSpace(stringFromAny(conn.Config["base_url"])), "/")
	if base == "" {
		base = notionAPIOrigin + "/v1"
	}
	parsed, err := requireAPIOrigin(base)
	if err != nil {
		return nil, errors.New("notion config.base_url: " + err.Error())
	}
	origin := parsed.Scheme + "://" + parsed.Host
	token, err := resolver.Resolve(conn.TenantID, auth.SecretPurposeConnector, origin, ref)
	if err != nil {
		return nil, fmt.Errorf("resolve notion credential: %w", err)
	}
	if token == "" {
		return nil, errors.New("notion credential_ref is required")
	}
	selections, err := notionSelections(conn.Config["selected_pages"])
	if err != nil {
		return nil, err
	}
	if len(selections) == 0 {
		return nil, errors.New("notion config.selected_pages is required")
	}
	depth := intFromAny(conn.Config["max_depth"], notionMaxDepth)
	items := intFromAny(conn.Config["max_pages"], notionMaxItems)
	if depth < 1 || depth > notionMaxDepth || items < 1 || items > notionMaxItems {
		return nil, errors.New("notion traversal bounds must be between 1 and 8 depth and 1 and 500 items")
	}
	return &NotionAdapter{
		baseURL: base, token: token, tenantID: conn.TenantID, connectionID: conn.ID,
		selections: selections, maxDepth: depth, maxItems: items,
		client: newConnectorHTTPClient(),
	}, nil
}

func notionSelections(raw any) ([]notionSelection, error) {
	values, ok := raw.([]any)
	if !ok || len(values) == 0 {
		return nil, errors.New("notion config.selected_pages must be a non-empty array")
	}
	out := make([]notionSelection, 0, len(values))
	seen := map[string]bool{}
	for _, rawValue := range values {
		value, ok := rawValue.(map[string]any)
		if !ok {
			return nil, errors.New("notion selected_pages entries must be objects")
		}
		id := strings.TrimSpace(stringFromAny(value["page_id"]))
		if id == "" || seen[id] {
			return nil, errors.New("notion selected page IDs must be non-empty and unique")
		}
		seen[id] = true
		out = append(out, notionSelection{PageID: id})
	}
	return out, nil
}

func (a *NotionAdapter) Capabilities() AdapterCapabilities {
	return AdapterCapabilities{Content: true}
}

func (a *NotionAdapter) validateConnection(conn source.Connection) error {
	if conn.Kind != source.ConnectionNotion || conn.TenantID != a.tenantID || conn.ID != a.connectionID {
		return errors.New("notion adapter connection does not match")
	}
	return nil
}
func (a *NotionAdapter) Content(ctx context.Context, conn source.Connection, cursor Cursor, limit int) (ContentPage, error) {
	if err := a.validateConnection(conn); err != nil {
		return ContentPage{}, err
	}
	if limit < 1 || limit > 100 {
		return ContentPage{}, errors.New("notion page limit must be between 1 and 100")
	}
	offset := 0
	if cursor != "" {
		parsed, err := strconv.Atoi(string(cursor))
		if err != nil || parsed < 0 {
			return ContentPage{}, errors.New("invalid notion content cursor")
		}
		offset = parsed
	}
	a.snapshotMu.Lock()
	defer a.snapshotMu.Unlock()
	if cursor != "" && !a.contentSnapshotReady {
		offset = 0
	}
	if cursor == "" || !a.contentSnapshotReady {
		a.contentSnapshotReady = false
		items, err := a.traverse(ctx)
		if err != nil {
			return ContentPage{Completeness: CompletenessIncomplete}, err
		}
		a.contentSnapshot = items
		a.contentSnapshotReady = true
	}
	items := a.contentSnapshot
	if offset > len(items) {
		return ContentPage{}, errors.New("notion content cursor exceeds snapshot")
	}
	end := offset + limit
	if end > len(items) {
		end = len(items)
	}
	page := ContentPage{Items: items[offset:end], Completeness: CompletenessComplete, Snapshot: true}
	if end < len(items) {
		page.NextCursor = Cursor(strconv.Itoa(end))
	}
	return page, nil
}

func (a *NotionAdapter) traverse(ctx context.Context) ([]ContentItem, error) {
	items, stats, err := a.traverseSelections(ctx, a.selections)
	// Connectors have no warning channel; skipped content is reported on the
	// service log so a sync that silently drops blocks is still visible.
	if stats.unsupportedBlocks > 0 {
		log.Printf("notion connection %s: skipped %d unsupported blocks", a.connectionID, stats.unsupportedBlocks)
	}
	if stats.truncated {
		log.Printf("notion connection %s: block tree truncated at depth %d", a.connectionID, a.maxDepth)
	}
	return items, err
}

// notionSyncStats counts content the traversal could not represent.
type notionSyncStats struct {
	unsupportedBlocks int
	truncated         bool
}

// notionChildPage is a child_page boundary discovered while collecting one
// page's blocks; the child becomes its own document.
type notionChildPage struct {
	id         string
	parent     string
	parentKind string
	ancestors  []string
	depth      int
}

// notionPageContext carries the per-page fields every block link needs.
type notionPageContext struct {
	title     string
	pageURL   string
	ancestors []string
}

type notionTraversal struct {
	adapter   *NotionAdapter
	items     []ContentItem
	seen      map[string]bool
	userNames map[string]string
	nodes     int
	stats     notionSyncStats
}

func (a *NotionAdapter) traverseSelections(ctx context.Context, selections []notionSelection) ([]ContentItem, notionSyncStats, error) {
	t := &notionTraversal{
		adapter:   a,
		seen:      make(map[string]bool),
		userNames: make(map[string]string),
	}
	for _, sel := range selections {
		if err := t.walkPage(ctx, sel.PageID, "", "", nil, 0); err != nil {
			return nil, t.stats, fmt.Errorf("notion selected page %q: %w", sel.PageID, err)
		}
	}
	return t.items, t.stats, nil
}

// walkPage emits one ContentItem for a page whose Blocks hold the page's
// whole block tree, then walks each child page discovered inside it.
func (t *notionTraversal) walkPage(ctx context.Context, id, parent, parentKind string, ancestors []string, depth int) error {
	if depth > t.adapter.maxDepth {
		t.stats.truncated = true
		return nil
	}
	if t.seen["page:"+id] {
		return nil
	}
	t.seen["page:"+id] = true
	var page struct {
		ID           string `json:"id"`
		URL          string `json:"url"`
		CreatedTime  string `json:"created_time"`
		LastEdited   string `json:"last_edited_time"`
		LastEditedBy struct {
			ID string `json:"id"`
		} `json:"last_edited_by"`
		Properties map[string]json.RawMessage `json:"properties"`
	}
	if err := t.adapter.get(ctx, "/pages/"+url.PathEscape(id), &page); err != nil {
		return err
	}
	if page.ID != id || !validNotionPageURL(page.URL) {
		return errors.New("notion returned malformed page")
	}
	title, err := notionPageTitle(page.Properties)
	if err != nil {
		return err
	}
	item := ContentItem{
		NativeID: id, ParentNativeID: parent, ParentKind: parentKind,
		Kind: "page", NativeRevision: page.LastEdited, Title: title, URL: page.URL,
		CreatedAt: notionTime(page.CreatedTime), UpdatedAt: notionTime(page.LastEdited),
		Container: strings.Join(ancestors, " / "),
	}
	if name := t.userName(ctx, page.LastEditedBy.ID); name != "" {
		item.Authors = []string{name}
	}
	pc := &notionPageContext{title: title, pageURL: page.URL, ancestors: ancestors}
	var childPages []notionChildPage
	if err := t.collectBlocks(ctx, id, "page", false, pc, depth, &item.Blocks, &childPages); err != nil {
		return err
	}
	if len(item.Blocks) == 0 {
		// A page with no representable blocks still syncs as an empty
		// structured document so the item keeps its identity.
		encoded, err := source.EncodeBlocks(nil)
		if err != nil {
			return err
		}
		item.Content = encoded
		item.MIMEType = source.BlocksMIMEType
		item.Filename = "document.blocks.json"
	}
	t.items = append(t.items, item)
	if err := t.countNode(); err != nil {
		return err
	}
	for _, child := range childPages {
		if err := t.walkPage(ctx, child.id, child.parent, child.parentKind, child.ancestors, child.depth); err != nil {
			return err
		}
	}
	return nil
}

// collectBlocks appends the structured blocks found under id (a page or block
// id) to out in depth-first document order. childPages collects child_page
// boundaries, which become documents of their own. parentKind is the kind of
// the container whose children are listed ("page" or "block");
// tableHasColumnHeader marks a table parent whose first row is a header.
func (t *notionTraversal) collectBlocks(ctx context.Context, id, parentKind string, tableHasColumnHeader bool, pc *notionPageContext, depth int, out *[]source.StructuredBlock, childPages *[]notionChildPage) error {
	cursor := ""
	tableRows := 0
	for pages := 0; ; pages++ {
		if pages >= 100 {
			return errors.New("notion children pagination exceeded 100 pages")
		}
		endpoint := "/blocks/" + url.PathEscape(id) + "/children?page_size=100"
		if cursor != "" {
			endpoint += "&start_cursor=" + url.QueryEscape(cursor)
		}
		var result struct {
			Results []json.RawMessage `json:"results"`
			HasMore bool              `json:"has_more"`
			Next    string            `json:"next_cursor"`
		}
		if err := t.adapter.get(ctx, endpoint, &result); err != nil {
			return err
		}
		for _, raw := range result.Results {
			block, err := parseNotionBlock(raw)
			if err != nil {
				return err
			}
			emit := !t.seen["block:"+block.ID]
			if emit {
				t.seen["block:"+block.ID] = true
			}
			header := false
			if block.Type == "table_row" {
				header = tableHasColumnHeader && tableRows == 0
				tableRows++
			}
			if emit {
				if sb, ok := t.structuredBlock(ctx, block, pc, header); ok {
					*out = append(*out, sb)
					if err := t.countNode(); err != nil {
						return err
					}
				}
			}
			childPage := block.Type == "child_page"
			if (block.HasChildren || childPage) && depth < t.adapter.maxDepth {
				if childPage {
					*childPages = append(*childPages, notionChildPage{
						id: block.ID, parent: id, parentKind: parentKind,
						ancestors: append(append([]string{}, pc.ancestors...), pc.title),
						depth:     depth + 1,
					})
				} else {
					nestedHeader := block.Type == "table" && notionTableHasColumnHeader(block.raw)
					if err := t.collectBlocks(ctx, block.ID, "block", nestedHeader, pc, depth+1, out, childPages); err != nil {
						return err
					}
				}
			} else if block.HasChildren || childPage {
				t.stats.truncated = true
			}
		}
		if !result.HasMore {
			break
		}
		if result.Next == "" || result.Next == cursor {
			return errors.New("notion returned invalid pagination cursor")
		}
		cursor = result.Next
	}
	return nil
}

func (t *notionTraversal) countNode() error {
	t.nodes++
	if t.nodes > t.adapter.maxItems {
		return errors.New("notion traversal exceeded configured item bound")
	}
	return nil
}

// userName resolves a Notion user id to a display name. Lookups are cached
// per traversal and best-effort: an unresolvable editor leaves the author
// empty rather than failing the sync.
func (t *notionTraversal) userName(ctx context.Context, id string) string {
	if id == "" {
		return ""
	}
	if name, ok := t.userNames[id]; ok {
		return name
	}
	name := ""
	var user struct {
		Name string `json:"name"`
	}
	if err := t.adapter.get(ctx, "/users/"+url.PathEscape(id), &user); err == nil {
		name = strings.TrimSpace(user.Name)
	}
	t.userNames[id] = name
	return name
}

// notionBlock is the common envelope of a Notion block; raw keeps the full
// payload so each type's own object can be decoded on demand.
type notionBlock struct {
	ID           string `json:"id"`
	Type         string `json:"type"`
	CreatedTime  string `json:"created_time"`
	LastEdited   string `json:"last_edited_time"`
	LastEditedBy struct {
		ID string `json:"id"`
	} `json:"last_edited_by"`
	HasChildren bool            `json:"has_children"`
	raw         json.RawMessage `json:"-"`
}

func parseNotionBlock(raw json.RawMessage) (notionBlock, error) {
	var block notionBlock
	if err := json.Unmarshal(raw, &block); err != nil || block.ID == "" || block.Type == "" {
		return notionBlock{}, errors.New("notion returned malformed block")
	}
	block.raw = raw
	return block, nil
}

// structuredBlock maps one Notion block to a StructuredBlock. It returns
// false for containers and boundaries that emit nothing (table, columns,
// child pages/databases) and for empty text; unsupported types are counted.
func (t *notionTraversal) structuredBlock(ctx context.Context, block notionBlock, pc *notionPageContext, tableHeader bool) (source.StructuredBlock, bool) {
	payload := notionBlockPayload(block.raw, block.Type)
	var fields struct {
		RichText []notionRichText   `json:"rich_text"`
		Cells    [][]notionRichText `json:"cells"`
		Checked  bool               `json:"checked"`
		Language string             `json:"language"`
	}
	_ = json.Unmarshal(payload, &fields)
	text := notionRichTextString(fields.RichText)
	var kind, language string
	var level int
	switch block.Type {
	case "heading_1", "heading_2", "heading_3":
		kind = source.StructuredHeading
		level, _ = strconv.Atoi(block.Type[len(block.Type)-1:])
	case "paragraph", "quote", "callout", "toggle":
		kind = source.StructuredParagraph
	case "bulleted_list_item", "numbered_list_item":
		kind = source.StructuredListItem
	case "to_do":
		kind = source.StructuredListItem
		if strings.TrimSpace(text) != "" {
			if fields.Checked {
				text = "[x] " + text
			} else {
				text = "[ ] " + text
			}
		}
	case "code":
		kind = source.StructuredCode
		language = fields.Language
	case "table_row":
		kind = source.StructuredTableRow
		if tableHeader {
			kind = source.StructuredTableHeader
		}
		text = notionTableRowText(fields.Cells)
	case "table", "column_list", "column", "child_page", "child_database":
		return source.StructuredBlock{}, false
	default:
		t.stats.unsupportedBlocks++
		return source.StructuredBlock{}, false
	}
	if strings.TrimSpace(text) == "" {
		return source.StructuredBlock{}, false
	}
	blockURL := ""
	if pc.pageURL != "" {
		// Notion blocks have no own URL; the page anchor jumps to them.
		blockURL = pc.pageURL + "#" + strings.ReplaceAll(block.ID, "-", "")
	}
	return source.StructuredBlock{
		Kind: kind, Text: text, Level: level, Language: language,
		URL: blockURL, Author: t.userName(ctx, block.LastEditedBy.ID),
		CreatedAt: block.CreatedTime, UpdatedAt: block.LastEdited,
		NativeID: block.ID,
	}, true
}

type notionRichText struct {
	PlainText string `json:"plain_text"`
}

func notionRichTextString(parts []notionRichText) string {
	var b strings.Builder
	for _, part := range parts {
		b.WriteString(part.PlainText)
	}
	return b.String()
}

func notionTableRowText(cells [][]notionRichText) string {
	parts := make([]string, 0, len(cells))
	for _, cell := range cells {
		parts = append(parts, notionRichTextString(cell))
	}
	return strings.Join(parts, " | ")
}

// notionBlockPayload returns the type-specific object of a block, e.g. the
// "paragraph" member of a paragraph block.
func notionBlockPayload(raw json.RawMessage, blockType string) json.RawMessage {
	var all map[string]json.RawMessage
	if err := json.Unmarshal(raw, &all); err != nil {
		return nil
	}
	return all[blockType]
}

func notionTableHasColumnHeader(raw json.RawMessage) bool {
	var table struct {
		HasColumnHeader bool `json:"has_column_header"`
	}
	if err := json.Unmarshal(notionBlockPayload(raw, "table"), &table); err != nil {
		return false
	}
	return table.HasColumnHeader
}

func notionPageTitle(properties map[string]json.RawMessage) (string, error) {
	for _, raw := range properties {
		var property struct {
			Type  string `json:"type"`
			Title []struct {
				PlainText string `json:"plain_text"`
			} `json:"title"`
		}
		if err := json.Unmarshal(raw, &property); err != nil {
			return "", err
		}
		if property.Type == "title" {
			var title strings.Builder
			for _, part := range property.Title {
				title.WriteString(part.PlainText)
			}
			return title.String(), nil
		}
	}
	return "", nil
}

func notionTime(raw string) *time.Time {
	if raw == "" {
		return nil
	}
	parsed, err := time.Parse(time.RFC3339, raw)
	if err != nil {
		return nil
	}
	return &parsed
}

var errNotionAccessLost = errors.New("notion publisher access lost")

func (a *NotionAdapter) waitRateLimit(ctx context.Context) error {
	a.requestMu.Lock()
	defer a.requestMu.Unlock()
	if delay := time.Until(a.nextRequest); delay > 0 {
		timer := time.NewTimer(delay)
		select {
		case <-ctx.Done():
			timer.Stop()
			return ctx.Err()
		case <-timer.C:
		}
	}
	a.nextRequest = time.Now().Add(time.Second / 3)
	return nil
}
func (a *NotionAdapter) get(ctx context.Context, path string, target any) error {
	for attempt := range 3 {
		if err := a.waitRateLimit(ctx); err != nil {
			return err
		}
		req, err := http.NewRequestWithContext(ctx, http.MethodGet, a.baseURL+path, nil)
		if err != nil {
			return err
		}
		req.Header.Set("Authorization", "Bearer "+a.token)
		req.Header.Set("Notion-Version", "2022-06-28")
		req.Header.Set("Accept", "application/json")
		resp, err := a.client.Do(req)
		if err != nil {
			return fmt.Errorf("notion request failed: %w", err)
		}
		defer func() { _ = resp.Body.Close() }()
		status := resp.StatusCode
		retryAfter := resp.Header.Get("Retry-After")
		body, readErr := readConnectorBody(resp)
		if status == http.StatusTooManyRequests && attempt < 2 {
			delay := time.Second
			if seconds, parseErr := strconv.Atoi(retryAfter); parseErr == nil && seconds > 0 {
				delay = time.Duration(seconds) * time.Second
			}
			if delay > 2*time.Second {
				delay = 2 * time.Second
			}
			timer := time.NewTimer(delay)
			select {
			case <-ctx.Done():
				timer.Stop()
				return ctx.Err()
			case <-timer.C:
			}
			continue
		}
		if readErr != nil {
			if status == http.StatusNotFound || status == http.StatusUnauthorized || status == http.StatusForbidden {
				return fmt.Errorf("%w: status %d", errNotionAccessLost, status)
			}
			return readErr
		}
		if err := json.Unmarshal(body, target); err != nil {
			return fmt.Errorf("decode notion response: %w", err)
		}
		return nil
	}
	return errors.New("notion rate limit retry bound exceeded")
}
func validNotionPageURL(raw string) bool {
	u, err := url.Parse(raw)
	if err != nil || u.Scheme != "https" || u.User != nil {
		return false
	}
	host := strings.ToLower(strings.TrimSuffix(u.Hostname(), "."))
	// The API now returns app.notion.com links; older pages still use notion.so.
	return host == "notion.so" || strings.HasSuffix(host, ".notion.so") ||
		host == "notion.com" || strings.HasSuffix(host, ".notion.com") ||
		host == "notion.site" || strings.HasSuffix(host, ".notion.site")
}
