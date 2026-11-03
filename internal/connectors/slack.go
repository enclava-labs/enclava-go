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

	"enclava-go/internal/source"
)

const slackPageLimit = 200

type slackAccessCursor struct {
	Members, History         string
	MembersDone, HistoryDone bool
}

func encodeSlackAccessCursor(cursor slackAccessCursor) Cursor {
	raw, _ := json.Marshal(cursor)
	return Cursor("access:" + base64.RawURLEncoding.EncodeToString(raw))
}

func decodeSlackAccessCursor(cursor Cursor) (slackAccessCursor, error) {
	if cursor == "" {
		return slackAccessCursor{}, nil
	}
	if !strings.HasPrefix(string(cursor), "access:") {
		return slackAccessCursor{}, errors.New("invalid slack access cursor")
	}
	raw, err := base64.RawURLEncoding.DecodeString(strings.TrimPrefix(string(cursor), "access:"))
	if err != nil {
		return slackAccessCursor{}, err
	}
	var state slackAccessCursor
	if err := json.Unmarshal(raw, &state); err != nil {
		return slackAccessCursor{}, err
	}
	return state, nil
}

type slackAdapter struct {
	client *http.Client
}

// NewSlackAdapter returns the native Slack Web API adapter.
func NewSlackAdapter(source.Connection) (Adapter, error) {
	return slackAdapter{client: newConnectorHTTPClient()}, nil
}

func (slackAdapter) Capabilities() AdapterCapabilities {
	return AdapterCapabilities{Content: true, Access: true, Membership: true, UnsupportedScopes: []string{"direct_messages", "group_direct_messages", "canvases", "lists", "slack_connect_external_membership_mapping"}}
}

func (a slackAdapter) Content(ctx context.Context, conn source.Connection, cursor Cursor, limit int) (ContentPage, error) {
	if err := slackConnection(conn); err != nil {
		return ContentPage{}, err
	}
	if limit < 1 || limit > slackPageLimit {
		return ContentPage{}, errors.New("slack page limit must be between 1 and 200")
	}
	channel := slackChannel(conn)
	q := url.Values{"channel": {channel}, "limit": {strconv.Itoa(limit)}}
	if cursor != "" {
		q.Set("cursor", string(cursor))
	}
	var data struct {
		Messages []slackMessage `json:"messages"`
		HasMore  bool           `json:"has_more"`
		Metadata struct {
			NextCursor string `json:"next_cursor"`
		} `json:"response_metadata"`
	}
	if err := a.call(ctx, conn, "conversations.history", q, &data); err != nil {
		return ContentPage{}, err
	}
	var info struct {
		Channel struct {
			Name string `json:"name"`
		} `json:"channel"`
	}
	if len(data.Messages) > 0 {
		if err := a.call(ctx, conn, "conversations.info", url.Values{"channel": {channel}}, &info); err != nil {
			if ctx.Err() != nil {
				return ContentPage{}, ctx.Err()
			}
			info.Channel.Name = ""
		}
	}
	page := ContentPage{Completeness: CompletenessComplete}
	// Only explicitly requested whole-channel reconciliations can authorize absence sweeps.
	page.Snapshot = strings.EqualFold(stringFromAny(conn.Config["reconcile"]), "true")
	for _, msg := range data.Messages {
		item, err := a.messageItem(ctx, conn, channel, info.Channel.Name, msg)
		if err != nil {
			return ContentPage{}, err
		}
		page.Items = append(page.Items, item)
		if msg.ReplyCount > 0 {
			replies, err := a.threadMessages(ctx, conn, channel, msg.TS, limit)
			if err != nil {
				return ContentPage{}, err
			}
			for _, reply := range replies {
				reply.ThreadTS = msg.TS
				item, err := a.messageItem(ctx, conn, channel, info.Channel.Name, reply)
				if err != nil {
					return ContentPage{}, err
				}
				page.Items = append(page.Items, item)
				for _, file := range reply.Files {
					attachment, err := a.fileItem(ctx, conn, file.ID)
					if err != nil {
						return ContentPage{}, err
					}
					if attachment != nil {
						page.Items = append(page.Items, *attachment)
					}
				}
			}
		}
		for _, file := range msg.Files {
			attachment, err := a.fileItem(ctx, conn, file.ID)
			if err != nil {
				return ContentPage{}, err
			}
			if attachment != nil {
				page.Items = append(page.Items, *attachment)
			}
		}
	}
	page.NextCursor = Cursor(data.Metadata.NextCursor)
	if data.HasMore && page.NextCursor == "" {
		return ContentPage{}, errors.New("slack history has_more without next_cursor")
	}
	if !data.HasMore {
		page.NextCursor = ""
	}
	return page, nil
}

func (a slackAdapter) threadMessages(ctx context.Context, conn source.Connection, channel, parent string, limit int) ([]slackMessage, error) {
	var messages []slackMessage
	cursor := ""
	for pages := 0; pages < 100; pages++ {
		q := url.Values{"channel": {channel}, "ts": {parent}, "limit": {strconv.Itoa(limit)}}
		if cursor != "" {
			q.Set("cursor", cursor)
		}
		var data struct {
			Messages []slackMessage `json:"messages"`
			HasMore  bool           `json:"has_more"`
			Metadata struct {
				NextCursor string `json:"next_cursor"`
			} `json:"response_metadata"`
		}
		if err := a.call(ctx, conn, "conversations.replies", q, &data); err != nil {
			return nil, err
		}
		for _, message := range data.Messages {
			if message.TS != parent {
				messages = append(messages, message)
			}
		}
		if !data.HasMore {
			return messages, nil
		}
		if data.Metadata.NextCursor == "" {
			return nil, errors.New("slack replies has_more without next_cursor")
		}
		cursor = data.Metadata.NextCursor
	}
	return nil, errors.New("slack thread exceeded page bound")
}

type slackMessage struct {
	User       string `json:"user"`
	Text       string `json:"text"`
	TS         string `json:"ts"`
	ThreadTS   string `json:"thread_ts"`
	ReplyCount int    `json:"reply_count"`
	Files      []struct {
		ID string `json:"id"`
	} `json:"files"`
}

func (a slackAdapter) messageItem(ctx context.Context, conn source.Connection, channel, title string, msg slackMessage) (ContentItem, error) {
	if msg.TS == "" {
		return ContentItem{}, errors.New("slack message omitted ts")
	}
	q := url.Values{"channel": {channel}, "message_ts": {msg.TS}}
	var permalink struct {
		Permalink string `json:"permalink"`
	}
	if err := a.call(ctx, conn, "chat.getPermalink", q, &permalink); err != nil {
		return ContentItem{}, err
	}
	created := slackTimestamp(msg.TS)
	parent, parentKind := "slack:"+channel, "channel"
	if msg.ThreadTS != "" && msg.ThreadTS != msg.TS {
		parent, parentKind = "slack:"+channel+":"+msg.ThreadTS, "message"
	}
	return ContentItem{NativeID: "slack:" + channel + ":" + msg.TS, ParentNativeID: parent, ParentKind: parentKind,
		Kind: "message", NativeRevision: msg.TS, Title: title, Filename: "slack-" + channel + "-" + msg.TS + ".txt", MIMEType: "text/plain",
		Text: "[" + msg.TS + " " + msg.User + "] " + msg.Text, URL: permalink.Permalink, CreatedAt: created}, nil
}

func slackTimestamp(ts string) *time.Time {
	seconds, fraction, _ := strings.Cut(ts, ".")
	whole, err := strconv.ParseInt(seconds, 10, 64)
	if err != nil {
		return nil
	}
	nanos := int64(0)
	if fraction != "" {
		if len(fraction) > 9 {
			fraction = fraction[:9]
		}
		fraction = fraction + strings.Repeat("0", 9-len(fraction))
		nanos, err = strconv.ParseInt(fraction, 10, 64)
		if err != nil {
			return nil
		}
	}
	value := time.Unix(whole, nanos).UTC()
	return &value
}

func (a slackAdapter) fileItem(ctx context.Context, conn source.Connection, id string) (*ContentItem, error) {
	if id == "" {
		return nil, errors.New("slack message file omitted id")
	}
	var data struct {
		File struct {
			ID          string `json:"id"`
			Name        string `json:"name"`
			Title       string `json:"title"`
			Mimetype    string `json:"mimetype"`
			Permalink   string `json:"permalink"`
			External    bool   `json:"is_external"`
			ExternalURL string `json:"external_url"`
			Shares      map[string]map[string][]struct {
				ChannelID string `json:"channel_id"`
			} `json:"shares"`
		} `json:"file"`
	}
	if err := a.call(ctx, conn, "files.info", url.Values{"file": {id}}, &data); err != nil {
		return nil, err
	}
	f := data.File
	if f.External {
		return nil, nil
	} // External targets require their own verified connector scope.
	if f.ID == "" {
		return nil, errors.New("slack files.info omitted file id")
	}
	name := firstNonEmpty(f.Name, f.Title, f.ID)
	return &ContentItem{NativeID: "slack:file:" + f.ID, Kind: "file", NativeRevision: f.ID, Title: f.Name, Filename: name, MIMEType: f.Mimetype, URL: f.Permalink}, nil
}

func (a slackAdapter) Access(ctx context.Context, conn source.Connection, cursor Cursor, limit int) (AccessPage, error) {
	if err := slackConnection(conn); err != nil {
		return AccessPage{}, err
	}
	if limit < 1 || limit > slackPageLimit {
		return AccessPage{}, errors.New("slack page limit must be between 1 and 200")
	}
	state, err := decodeSlackAccessCursor(cursor)
	if err != nil {
		return AccessPage{Completeness: CompletenessIncomplete}, err
	}
	channel := slackChannel(conn)
	page := AccessPage{Completeness: CompletenessComplete, Snapshot: true}
	if !state.MembersDone {
		q := url.Values{"channel": {channel}, "limit": {strconv.Itoa(limit)}}
		if state.Members != "" {
			q.Set("cursor", state.Members)
		}
		var data struct {
			Members  []string `json:"members"`
			Metadata struct {
				NextCursor string `json:"next_cursor"`
			} `json:"response_metadata"`
		}
		if err := a.call(ctx, conn, "conversations.members", q, &data); err != nil {
			page.Completeness = CompletenessIncomplete
			return page, err
		}
		for _, uid := range data.Members {
			page.Grants = append(page.Grants, AccessGrant{NativeObjectID: "slack:" + channel, NativeObjectKind: "channel", NativePrincipalID: uid, NativeGroupID: channel, Scope: "read", Effect: "allow"})
		}
		state.Members = data.Metadata.NextCursor
		state.MembersDone = state.Members == ""
	}
	if !state.HistoryDone {
		q := url.Values{"channel": {channel}, "limit": {strconv.Itoa(limit)}}
		if state.History != "" {
			q.Set("cursor", state.History)
		}
		var data struct {
			Messages []slackMessage `json:"messages"`
			HasMore  bool           `json:"has_more"`
			Metadata struct {
				NextCursor string `json:"next_cursor"`
			} `json:"response_metadata"`
		}
		if err := a.call(ctx, conn, "conversations.history", q, &data); err != nil {
			page.Completeness = CompletenessIncomplete
			return page, err
		}
		for _, msg := range data.Messages {
			for _, file := range msg.Files {
				grants, err := a.fileShareGrants(ctx, conn, file.ID)
				if err != nil {
					page.Completeness = CompletenessIncomplete
					return page, err
				}
				page.Grants = append(page.Grants, grants...)
			}
			if msg.ReplyCount > 0 {
				replies, err := a.threadMessages(ctx, conn, channel, msg.TS, limit)
				if err != nil {
					page.Completeness = CompletenessIncomplete
					return page, err
				}
				for _, reply := range replies {
					for _, file := range reply.Files {
						grants, err := a.fileShareGrants(ctx, conn, file.ID)
						if err != nil {
							page.Completeness = CompletenessIncomplete
							return page, err
						}
						page.Grants = append(page.Grants, grants...)
					}
				}
			}
		}
		if data.HasMore && data.Metadata.NextCursor == "" {
			page.Completeness = CompletenessIncomplete
			return page, errors.New("slack history has_more without next_cursor")
		}
		state.History = data.Metadata.NextCursor
		state.HistoryDone = !data.HasMore
	}
	if len(page.Grants) > limit {
		page.Completeness = CompletenessIncomplete
		return page, errors.New("slack access page exceeds requested bound")
	}
	if !state.MembersDone || !state.HistoryDone {
		page.NextCursor = encodeSlackAccessCursor(state)
	}
	return page, nil
}

func (a slackAdapter) fileShareGrants(ctx context.Context, conn source.Connection, id string) ([]AccessGrant, error) {
	var data struct {
		File struct {
			ID       string                                `json:"id"`
			External bool                                  `json:"is_external"`
			Shares   map[string]map[string]json.RawMessage `json:"shares"`
		} `json:"file"`
	}
	if err := a.call(ctx, conn, "files.info", url.Values{"file": {id}}, &data); err != nil {
		return nil, err
	}
	if data.File.External {
		return nil, nil
	}
	if data.File.ID == "" {
		return nil, errors.New("slack files.info omitted file id")
	}
	var grants []AccessGrant
	for _, scope := range []string{"public", "private"} {
		for channel := range data.File.Shares[scope] {
			grants = append(grants, AccessGrant{NativeObjectID: "slack:file:" + data.File.ID, NativeObjectKind: "file", NativeGroupID: channel, Scope: "read", Effect: "allow"})
		}
	}
	return grants, nil
}

func (a slackAdapter) Membership(ctx context.Context, conn source.Connection, cursor Cursor, limit int) (MembershipPage, error) {
	page := MembershipPage{Completeness: CompletenessComplete, Snapshot: true}
	if err := slackConnection(conn); err != nil {
		return MembershipPage{}, err
	}
	if limit < 1 || limit > slackPageLimit {
		return MembershipPage{}, errors.New("slack page limit must be between 1 and 200")
	}
	q := url.Values{"channel": {slackChannel(conn)}, "limit": {strconv.Itoa(limit)}}
	if cursor != "" {
		q.Set("cursor", string(cursor))
	}
	var data struct {
		Members  []string `json:"members"`
		Metadata struct {
			NextCursor string `json:"next_cursor"`
		} `json:"response_metadata"`
	}
	if err := a.call(ctx, conn, "conversations.members", q, &data); err != nil {
		page.Completeness = CompletenessIncomplete
		return page, err
	}
	for _, uid := range data.Members {
		page.Memberships = append(page.Memberships, AccessGrant{NativeGroupID: slackChannel(conn), NativePrincipalID: uid, Scope: "member", Effect: "allow"})
	}
	page.NextCursor = Cursor(data.Metadata.NextCursor)
	return page, nil
}

func (a slackAdapter) call(ctx context.Context, conn source.Connection, method string, query url.Values, out any) error {
	view := connectionView(conn)
	token, err := connectorToken(view)
	if err != nil {
		return err
	}
	base := strings.TrimRight(connectorBaseURL(view, "https://slack.com/api"), "/")
	endpoint := base + "/" + method
	if len(query) != 0 {
		endpoint += "?" + query.Encode()
	}
	req, err := http.NewRequestWithContext(ctx, http.MethodGet, endpoint, nil)
	if err != nil {
		return err
	}
	req.Header.Set("Authorization", "Bearer "+token)
	req.Header.Set("Accept", "application/json")
	resp, err := a.client.Do(req)
	if err != nil {
		return err
	}
	defer func() { _ = resp.Body.Close() }()
	if resp.StatusCode == http.StatusTooManyRequests {
		return fmt.Errorf("slack %s rate limited (Retry-After %s)", method, resp.Header.Get("Retry-After"))
	}
	if resp.StatusCode < 200 || resp.StatusCode >= 300 {
		return fmt.Errorf("slack %s returned HTTP %d", method, resp.StatusCode)
	}
	var payload json.RawMessage
	if err := json.NewDecoder(resp.Body).Decode(&payload); err != nil {
		return err
	}
	var envelope struct {
		OK    bool   `json:"ok"`
		Error string `json:"error"`
	}
	if err := json.Unmarshal(payload, &envelope); err != nil {
		return err
	}
	if !envelope.OK {
		return fmt.Errorf("slack %s failed: %s", method, firstNonEmpty(envelope.Error, "unknown_error"))
	}
	return json.Unmarshal(payload, out)
}

func slackChannel(conn source.Connection) string {
	return strings.TrimSpace(firstNonEmpty(stringFromAny(conn.Config["channel_id"]), stringFromAny(conn.Config["channel"])))
}
func slackConnection(conn source.Connection) error {
	if conn.Kind != string(source.ConnectionSlack) {
		return errors.New("slack adapter kind does not match connection")
	}
	if slackChannel(conn) == "" {
		return errors.New("slack config.channel_id is required")
	}
	return nil
}
