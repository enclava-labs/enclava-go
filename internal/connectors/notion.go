package connectors

import (
	"context"
	"encoding/json"
	"errors"
	"fmt"
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
	return a.traverseSelections(ctx, a.selections)
}

func (a *NotionAdapter) traverseSelections(ctx context.Context, selections []notionSelection) ([]ContentItem, error) {
	items := make([]ContentItem, 0)
	seen := make(map[string]bool)
	var walk func(string, string, string, string, string, int, bool) error
	walk = func(id, parent, parentKind, title, pageURL string, depth int, isPage bool) error {
		if depth > a.maxDepth {
			return nil
		}
		if isPage {
			var page struct {
				ID         string                     `json:"id"`
				URL        string                     `json:"url"`
				LastEdited string                     `json:"last_edited_time"`
				Properties map[string]json.RawMessage `json:"properties"`
			}
			if err := a.get(ctx, "/pages/"+url.PathEscape(id), &page); err != nil {
				return err
			}
			encoded, err := json.Marshal(page)
			if err != nil {
				return err
			}
			if page.ID != id || !validNotionPageURL(page.URL) {
				return errors.New("notion returned malformed page")
			}
			pageURL = page.URL
			title = ""
			for _, raw := range page.Properties {
				var property struct {
					Type  string `json:"type"`
					Title []struct {
						PlainText string `json:"plain_text"`
					} `json:"title"`
				}
				if err := json.Unmarshal(raw, &property); err != nil {
					return err
				}
				if property.Type == "title" {
					for _, part := range property.Title {
						title += part.PlainText
					}
					break
				}
			}
			if !seen["page:"+id] {
				seen["page:"+id] = true
				items = append(items, ContentItem{NativeID: id, ParentNativeID: parent, ParentKind: parentKind, Kind: "page", NativeRevision: page.LastEdited, Title: title, URL: pageURL, MIMEType: "application/json", Content: encoded})
			}
			if len(items) > a.maxItems {
				return errors.New("notion traversal exceeded configured item bound")
			}
		}
		kind := "block"
		if isPage {
			kind = "page"
		}
		cursor := ""
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
			if err := a.get(ctx, endpoint, &result); err != nil {
				return err
			}
			for _, raw := range result.Results {
				var block struct {
					ID          string `json:"id"`
					Type        string `json:"type"`
					LastEdited  string `json:"last_edited_time"`
					HasChildren bool   `json:"has_children"`
				}
				if err := json.Unmarshal(raw, &block); err != nil || block.ID == "" || block.Type == "" {
					return errors.New("notion returned malformed block")
				}
				// Notion blocks have no own URL; the page anchor jumps to them.
				blockURL := ""
				if pageURL != "" {
					blockURL = pageURL + "#" + strings.ReplaceAll(block.ID, "-", "")
				}
				if !seen["block:"+block.ID] {
					seen["block:"+block.ID] = true
					items = append(items, ContentItem{NativeID: block.ID, ParentNativeID: id, ParentKind: kind, Kind: "block", NativeRevision: block.LastEdited, Title: title, URL: blockURL, MIMEType: "application/json", Content: append([]byte(nil), raw...)})
					if len(items) > a.maxItems {
						return errors.New("notion traversal exceeded configured item bound")
					}
				}
				childPage := block.Type == "child_page"
				if (block.HasChildren || childPage) && depth < a.maxDepth {
					if err := walk(block.ID, id, kind, title, pageURL, depth+1, childPage); err != nil {
						return err
					}
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
	for _, sel := range selections {
		if err := walk(sel.PageID, "", "", "", "", 0, true); err != nil {
			return nil, fmt.Errorf("notion selected page %q: %w", sel.PageID, err)
		}
	}
	return items, nil
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
	return host == "notion.so" || strings.HasSuffix(host, ".notion.so") ||
		host == "notion.site" || strings.HasSuffix(host, ".notion.site")
}
