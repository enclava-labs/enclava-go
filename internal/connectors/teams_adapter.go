package connectors

import (
	"context"
	"enclava-go/internal/auth"
	"enclava-go/internal/source"
	"encoding/base64"
	"encoding/json"
	"errors"
	"fmt"
	"net/http"
	"net/url"
	"strconv"
	"strings"
	"sync"
	"time"
)

const teamsGraph = "https://graph.microsoft.com/v1.0"

// TeamsAdapter reads only configured Teams and chats using app-only Graph access.
// Channel roots sync through messages/delta and chats through a
// lastModifiedDateTime watermark; a periodic full listing reconciles deletions
// and older replies.
type TeamsAdapter struct {
	connection  source.Connection
	resolver    CredentialResolver
	client      *http.Client
	baseURL     string
	tokenMu     sync.Mutex
	accessToken string
	tokenExpiry time.Time
}

func NewTeamsAdapter(connection source.Connection, resolver CredentialResolver) (Adapter, error) {
	if connection.Kind != source.ConnectionTeams {
		return nil, errors.New("teams adapter requires a teams connection")
	}
	if strings.TrimSpace(connection.TenantID) == "" || strings.TrimSpace(connection.CredentialRef) == "" {
		return nil, errors.New("teams tenant and credential_ref are required")
	}
	base := strings.TrimRight(stringFromAny(connection.Config["graph_base_url"]), "/")
	if base == "" {
		base = teamsGraph
	}
	if _, err := requireAPIOrigin(base); err != nil {
		return nil, errors.New("teams Graph base URL: " + err.Error())
	}
	if resolver == nil {
		return nil, errors.New("teams secret resolver is required")
	}
	return &TeamsAdapter{connection: connection, resolver: resolver, client: newConnectorHTTPClient(), baseURL: base}, nil
}

func (a *TeamsAdapter) Capabilities() AdapterCapabilities {
	return AdapterCapabilities{
		Content: true,
		UnsupportedScopes: []string{
			"app-only access does not impersonate users",
			"chat messages and deletions plus replies to older channel messages are picked up by the periodic full sync",
			"meeting chats",
			"shared-channel guest identity beyond Graph allMembers",
			"SharePoint attachment contents",
		},
	}
}
func (a *TeamsAdapter) token(ctx context.Context) (string, error) {
	a.tokenMu.Lock()
	defer a.tokenMu.Unlock()
	if a.accessToken != "" && time.Now().Add(time.Minute).Before(a.tokenExpiry) {
		return a.accessToken, nil
	}
	secret, err := a.resolver.Resolve(a.connection.TenantID, auth.SecretPurposeConnector, originOf(a.baseURL), a.connection.CredentialRef)
	if err != nil {
		return "", err
	}
	tenant := strings.TrimSpace(stringFromAny(a.connection.Config["tenant_id"]))
	clientID := strings.TrimSpace(stringFromAny(a.connection.Config["client_id"]))
	if tenant == "" || clientID == "" || secret == "" {
		return "", errors.New("teams tenant_id, client_id and connector credential are required")
	}
	endpoint := "https://login.microsoftonline.com/" + url.PathEscape(tenant) + "/oauth2/v2.0/token"
	form := url.Values{"client_id": {clientID}, "client_secret": {secret}, "scope": {"https://graph.microsoft.com/.default"}, "grant_type": {"client_credentials"}}
	req, err := http.NewRequestWithContext(ctx, http.MethodPost, endpoint, strings.NewReader(form.Encode()))
	if err != nil {
		return "", err
	}
	req.Header.Set("Content-Type", "application/x-www-form-urlencoded")
	var response struct {
		AccessToken string `json:"access_token"`
		ExpiresIn   int    `json:"expires_in"`
	}
	if err := a.doJSON(req, &response); err != nil {
		return "", err
	}
	if response.AccessToken == "" {
		return "", errors.New("teams token response omitted access_token")
	}
	if response.ExpiresIn <= 0 {
		response.ExpiresIn = 3600
	}
	a.accessToken, a.tokenExpiry = response.AccessToken, time.Now().Add(time.Duration(response.ExpiresIn)*time.Second)
	return a.accessToken, nil
}

func originOf(raw string) string {
	u, _ := url.Parse(raw)
	return u.Scheme + "://" + u.Host
}

func (a *TeamsAdapter) doJSON(req *http.Request, target any) error {
	for attempt := range 3 {
		if attempt > 0 && req.Body != nil {
			if req.GetBody == nil {
				return errors.New("graph request cannot be retried")
			}
			body, err := req.GetBody()
			if err != nil {
				return err
			}
			req.Body = body
		}
		resp, err := a.client.Do(req)
		if err != nil {
			return err
		}
		if resp.StatusCode != http.StatusTooManyRequests || attempt == 2 {
			body, err := readConnectorBody(resp)
			if err != nil {
				return err
			}
			if err := json.Unmarshal(body, target); err != nil {
				return fmt.Errorf("decode Teams Graph response: %w", err)
			}
			return nil
		}
		delay, _ := strconv.Atoi(resp.Header.Get("Retry-After"))
		_ = resp.Body.Close()
		if delay > 30 {
			delay = 30
		}
		if delay > 0 {
			timer := time.NewTimer(time.Duration(delay) * time.Second)
			select {
			case <-req.Context().Done():
				timer.Stop()
				return req.Context().Err()
			case <-timer.C:
			}
		}
	}
	return errors.New("graph throttling retry limit exceeded")
}

func (a *TeamsAdapter) get(ctx context.Context, token, endpoint string, target any) error {
	if !connectorSameOrigin(a.baseURL, endpoint) {
		return errors.New("teams pagination URL changed origin")
	}
	req, err := http.NewRequestWithContext(ctx, http.MethodGet, endpoint, nil)
	if err != nil {
		return err
	}
	req.Header.Set("Authorization", "Bearer "+token)
	req.Header.Set("Accept", "application/json")
	return a.doJSON(req, target)
}

// teamsContentCursor pages one run's listing by offset and carries the durable
// per-run state: the completed full listing's time plus the delta links and chat
// watermarks a completed run passes to the next.
type teamsContentCursor struct {
	Offset     int               `json:"offset,omitempty"`
	Full       bool              `json:"full,omitempty"`
	FullAt     string            `json:"full_at,omitempty"`
	Started    string            `json:"started,omitempty"`
	DeltaLinks map[string]string `json:"delta_links,omitempty"`
	ChatMarks  map[string]string `json:"chat_marks,omitempty"`
}

func encodeTeamsContentCursor(state teamsContentCursor) Cursor {
	raw, _ := json.Marshal(state)
	return Cursor("content:" + base64.RawURLEncoding.EncodeToString(raw))
}

func decodeTeamsContentCursor(cursor Cursor) (teamsContentCursor, error) {
	var state teamsContentCursor
	raw := strings.TrimSpace(string(cursor))
	if raw == "" {
		return state, nil
	}
	if rest, ok := strings.CutPrefix(raw, "content:"); ok {
		decoded, err := base64.RawURLEncoding.DecodeString(rest)
		if err != nil || json.Unmarshal(decoded, &state) != nil {
			return state, errors.New("invalid teams cursor")
		}
	} else {
		// Legacy cursors were integer offsets of full runs.
		n, err := strconv.Atoi(raw)
		if err != nil {
			return state, errors.New("invalid teams cursor")
		}
		state.Offset, state.Full = n, true
	}
	if state.Offset < 0 {
		return state, errors.New("invalid teams cursor")
	}
	return state, nil
}

type teamsPage struct {
	Value []json.RawMessage `json:"value"`
	Next  string            `json:"@odata.nextLink"`
	Delta string            `json:"@odata.deltaLink"`
}

func (a *TeamsAdapter) list(ctx context.Context, token, endpoint string, max int) ([]json.RawMessage, error) {
	items := make([]json.RawMessage, 0)
	for pages := 0; endpoint != ""; pages++ {
		if pages >= 100 {
			return nil, errors.New("teams pagination exceeded 100 pages")
		}
		var page teamsPage
		if err := a.get(ctx, token, endpoint, &page); err != nil {
			return nil, err
		}
		items = append(items, page.Value...)
		if len(items) > max {
			return nil, errors.New("teams listing exceeded configured bound")
		}
		endpoint = page.Next
	}
	return items, nil
}

func (a *TeamsAdapter) selectedTeams() []string {
	if raw, ok := a.connection.Config["team_ids"].([]any); ok {
		ids := make([]string, 0, len(raw))
		for _, v := range raw {
			if id := strings.TrimSpace(stringFromAny(v)); id != "" {
				ids = append(ids, id)
			}
		}
		return ids
	}
	if id := strings.TrimSpace(stringFromAny(a.connection.Config["team_id"])); id != "" {
		return []string{id}
	}
	return nil
}
func (a *TeamsAdapter) selectedChats() []string {
	raw, _ := a.connection.Config["chat_ids"].([]any)
	ids := make([]string, 0, len(raw))
	for _, v := range raw {
		if id := strings.TrimSpace(stringFromAny(v)); id != "" {
			ids = append(ids, id)
		}
	}
	return ids
}
func teamsID(s string) string { return url.PathEscape(s) }
func parseTeamsTime(raw string) *time.Time {
	t, err := time.Parse(time.RFC3339Nano, raw)
	if err != nil {
		return nil
	}
	return &t
}

type graphMessage struct {
	ID   string `json:"id"`
	Body struct {
		Content string `json:"content"`
	} `json:"body"`
	Created  string `json:"createdDateTime"`
	Modified string `json:"lastModifiedDateTime"`
	WebURL   string `json:"webUrl"`
	Deleted  string `json:"deletedDateTime"`
	From     struct {
		User struct{ ID, DisplayName string } `json:"user"`
	} `json:"from"`
	Attachments []struct{ ID, Name, ContentType, ContentURL string } `json:"attachments"`
}

func (a *TeamsAdapter) Content(ctx context.Context, c source.Connection, cursor Cursor, limit int) (ContentPage, error) {
	if c.ID != a.connection.ID || c.Kind != source.ConnectionTeams {
		return ContentPage{}, errors.New("teams adapter connection mismatch")
	}
	if limit < 1 || limit > 100 {
		return ContentPage{}, errors.New("teams page limit must be between 1 and 100")
	}
	state, err := decodeTeamsContentCursor(cursor)
	if err != nil {
		return ContentPage{}, err
	}
	started := time.Now()
	if state.Offset == 0 {
		// Run start: a completed listing is due again after full_sync_interval.
		var lastFull time.Time
		if state.FullAt != "" {
			if parsed, err := time.Parse(time.RFC3339, state.FullAt); err == nil {
				lastFull = parsed
			}
		}
		state = teamsContentCursor{Full: fullSyncDue(a.connection.Config, lastFull, started), FullAt: state.FullAt, Started: started.Format(time.RFC3339), DeltaLinks: state.DeltaLinks, ChatMarks: state.ChatMarks}
		if state.Full {
			state.DeltaLinks, state.ChatMarks = nil, nil
		}
	}
	offset := state.Offset
	token, err := a.token(ctx)
	if err != nil {
		return ContentPage{}, err
	}
	items := make([]ContentItem, 0)
	appendItem := func(item ContentItem) error {
		if len(items) == teamsMaxItems {
			return errors.New("teams content listing exceeded configured bound")
		}
		items = append(items, item)
		return nil
	}
	// Starting links and watermarks stay constant for the run's whole listing;
	// fresh values collected here only enter the final page's checkpoint.
	nextLinks := map[string]string{}
	nextMarks := map[string]string{}
	if !state.Full {
		for k, v := range state.DeltaLinks {
			nextLinks[k] = v
		}
		for k, v := range state.ChatMarks {
			nextMarks[k] = v
		}
	}
	for _, team := range a.selectedTeams() {
		channels, err := a.list(ctx, token, a.baseURL+"/teams/"+teamsID(team)+"/channels?$top=100", teamsMaxItems)
		if err != nil {
			return ContentPage{Completeness: CompletenessIncomplete}, fmt.Errorf("list channels for team %s: %w", team, err)
		}
		for _, raw := range channels {
			var channel struct {
				ID             string `json:"id"`
				MembershipType string `json:"membershipType"`
			}
			if err := json.Unmarshal(raw, &channel); err != nil || channel.ID == "" {
				return ContentPage{}, errors.New("teams channel payload is malformed")
			}
			if !allowedTeamChannel(a.connection.Config, team, channel.ID) {
				continue
			}
			key := team + "\x00" + channel.ID
			var messages []json.RawMessage
			if state.Full {
				// Full runs list every root directly: messages/delta only covers
				// recent history, which would make the snapshot sweep drop older ones.
				messages, err = a.list(ctx, token, a.baseURL+"/teams/"+teamsID(team)+"/channels/"+teamsID(channel.ID)+"/messages?$top=50", teamsMaxItems)
				if err == nil {
					_, link, derr := a.listDelta(ctx, token, a.baseURL+"/teams/"+teamsID(team)+"/channels/"+teamsID(channel.ID)+"/messages/delta", teamsMaxItems)
					if derr == nil && link != "" {
						nextLinks[key] = link
					}
				}
			} else {
				endpoint := state.DeltaLinks[key]
				if endpoint == "" {
					endpoint = a.baseURL + "/teams/" + teamsID(team) + "/channels/" + teamsID(channel.ID) + "/messages/delta"
				}
				var link string
				messages, link, err = a.listDelta(ctx, token, endpoint, teamsMaxItems)
				if link != "" {
					nextLinks[key] = link
				}
			}
			if err != nil {
				return ContentPage{Completeness: CompletenessIncomplete}, fmt.Errorf("list %s channel %s messages: %w", channel.MembershipType, channel.ID, err)
			}
			messages = filterDeletedTeamsMessages(messages)
			for _, messageRaw := range messages {
				var m graphMessage
				if err := json.Unmarshal(messageRaw, &m); err != nil || m.ID == "" {
					return ContentPage{}, errors.New("teams message payload is malformed")
				}
				if err := appendItem(teamsMessageItem(team, channel.ID, channel.MembershipType, m, "")); err != nil {
					return ContentPage{Completeness: CompletenessIncomplete}, err
				}
				for _, at := range m.Attachments {
					id := at.ID
					if id == "" {
						id = at.Name
					}
					if id != "" {
						item := ContentItem{NativeID: "teams:" + team + ":" + channel.ID + ":" + m.ID + ":attachment:" + id, ParentNativeID: channel.ID, ParentKind: "teams_channel", Kind: "teams_attachment", Title: at.Name, Filename: at.Name, MIMEType: at.ContentType, URL: at.ContentURL}
						if err := appendItem(item); err != nil {
							return ContentPage{Completeness: CompletenessIncomplete}, err
						}
					}
				}
				replies, err := a.list(ctx, token, a.baseURL+"/teams/"+teamsID(team)+"/channels/"+teamsID(channel.ID)+"/messages/"+teamsID(m.ID)+"/replies?$top=50", teamsMaxItems)
				if err != nil {
					return ContentPage{Completeness: CompletenessIncomplete}, fmt.Errorf("list replies for channel %s message %s: %w", channel.ID, m.ID, err)
				}
				for _, replyRaw := range replies {
					var reply graphMessage
					if err := json.Unmarshal(replyRaw, &reply); err != nil || reply.ID == "" {
						return ContentPage{}, errors.New("teams reply payload is malformed")
					}
					if err := appendItem(teamsMessageItem(team, channel.ID, channel.MembershipType, reply, m.ID)); err != nil {
						return ContentPage{Completeness: CompletenessIncomplete}, err
					}
				}
			}
		}
	}
	for _, chat := range a.selectedChats() {
		endpoint := a.baseURL + "/chats/" + teamsID(chat) + "/messages?$top=50"
		watermark := nextMarks[chat]
		if !state.Full && watermark != "" {
			if mark, err := time.Parse(time.RFC3339, watermark); err == nil {
				since := mark.Add(-5 * time.Minute).UTC().Format(time.RFC3339)
				endpoint += "&$orderby=" + url.QueryEscape("lastModifiedDateTime desc") + "&$filter=" + url.QueryEscape("lastModifiedDateTime gt "+since)
			}
		}
		messages, err := a.list(ctx, token, endpoint, teamsMaxItems)
		if err != nil {
			return ContentPage{Completeness: CompletenessIncomplete}, fmt.Errorf("list configured chat %s messages (Chat.Read.All/ChatMessage.Read.All required): %w", chat, err)
		}
		for _, raw := range messages {
			var m graphMessage
			if err := json.Unmarshal(raw, &m); err != nil || m.ID == "" {
				return ContentPage{}, errors.New("teams chat message payload is malformed")
			}
			if parsed := parseTeamsTime(m.Modified); parsed != nil {
				if current := parseTeamsTime(nextMarks[chat]); current == nil || parsed.After(*current) {
					nextMarks[chat] = parsed.UTC().Format(time.RFC3339)
				}
			}
			item := teamsMessageItem("", chat, "chat", m, "")
			item.NativeID = "teams:chat:" + chat + ":" + m.ID
			item.Kind = "teams_chat_message"
			item.ParentNativeID, item.ParentKind = chat, "teams_chat"
			if err := appendItem(item); err != nil {
				return ContentPage{Completeness: CompletenessIncomplete}, err
			}
		}
	}
	if offset > len(items) {
		return ContentPage{}, errors.New("teams cursor exceeds listing")
	}
	end := offset + limit
	if end > len(items) {
		end = len(items)
	}
	page := ContentPage{Items: items[offset:end], Completeness: CompletenessComplete}
	state.Offset = end
	if end < len(items) {
		page.NextCursor = encodeTeamsContentCursor(state)
	} else {
		// Deletions and replies to older roots only surface in a full listing.
		page.Snapshot = state.Full
		if state.Full {
			state.FullAt = state.Started
		}
		state.DeltaLinks, state.ChatMarks = nextLinks, nextMarks
		state.Offset = 0
		page.Checkpoint = encodeTeamsContentCursor(state)
	}
	return page, nil
}

// listDelta follows a messages/delta chain and returns the final deltaLink.
func (a *TeamsAdapter) listDelta(ctx context.Context, token, endpoint string, max int) ([]json.RawMessage, string, error) {
	items := make([]json.RawMessage, 0)
	delta := ""
	for pages := 0; endpoint != ""; pages++ {
		if pages >= 100 {
			return nil, "", errors.New("teams pagination exceeded 100 pages")
		}
		var page teamsPage
		if err := a.get(ctx, token, endpoint, &page); err != nil {
			return nil, "", err
		}
		items = append(items, page.Value...)
		if len(items) > max {
			return nil, "", errors.New("teams listing exceeded configured bound")
		}
		if page.Delta != "" {
			delta = page.Delta
		}
		endpoint = page.Next
	}
	return items, delta, nil
}

// filterDeletedTeamsMessages drops tombstones a delta round reports; full runs
// apply them through the snapshot sweep instead.
func filterDeletedTeamsMessages(raws []json.RawMessage) []json.RawMessage {
	kept := raws[:0]
	for _, raw := range raws {
		var probe struct {
			Deleted string `json:"deletedDateTime"`
		}
		if json.Unmarshal(raw, &probe) == nil && probe.Deleted != "" {
			continue
		}
		kept = append(kept, raw)
	}
	return kept
}
func teamsMessageItem(team, channel, membership string, m graphMessage, parent string) ContentItem {
	id := "teams:" + team + ":" + channel + ":" + m.ID
	kind := "teams_channel_message"
	if parent != "" {
		kind = "teams_channel_reply"
	}
	parentID, parentKind := channel, "teams_channel"
	return ContentItem{NativeID: id, ParentNativeID: parentID, ParentKind: parentKind, Kind: kind + ":" + membership, NativeRevision: m.Modified, Text: m.Body.Content, URL: m.WebURL, CreatedAt: parseTeamsTime(m.Created), UpdatedAt: parseTeamsTime(m.Modified)}
}
func allowedTeamChannel(config map[string]any, team, channel string) bool {
	allowed, ok := config["channel_ids"].(map[string]any)
	if !ok {
		return true
	}
	raw, ok := allowed[team].([]any)
	if !ok {
		return false
	}
	for _, v := range raw {
		if stringFromAny(v) == channel {
			return true
		}
	}
	return false
}
