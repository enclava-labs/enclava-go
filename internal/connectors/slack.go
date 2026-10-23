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
	"time"

	"mini-enclava/internal/source"
)

const slackPageLimit = 200

type slackAdapter struct {
	client *http.Client
}

// NewSlackAdapter returns the native Slack Web API adapter.
func NewSlackAdapter(source.Connection) (Adapter, error) {
	return slackAdapter{client: &http.Client{Timeout: 30 * time.Second, CheckRedirect: refuseConnectorRedirect}}, nil
}

func (slackAdapter) Capabilities() AdapterCapabilities {
	return AdapterCapabilities{Content: true, Access: true, Membership: true, UnsupportedScopes: []string{"direct_messages", "group_direct_messages", "canvases", "lists", "slack_connect_external_membership_mapping"}}
}

func (a slackAdapter) Content(ctx context.Context, conn source.Connection, cursor Cursor, limit int) (ContentPage, error) {
	if err := slackConnection(conn); err != nil { return ContentPage{}, err }
	if limit < 1 || limit > slackPageLimit { return ContentPage{}, errors.New("slack page limit must be between 1 and 200") }
	channel := slackChannel(conn)
	q := url.Values{"channel": {channel}, "limit": {strconv.Itoa(limit)}}
	if cursor != "" { q.Set("cursor", string(cursor)) }
	var data struct {
		Messages []slackMessage `json:"messages"`
		HasMore bool `json:"has_more"`
		Metadata struct { NextCursor string `json:"next_cursor"` } `json:"response_metadata"`
	}
	if err := a.call(ctx, conn, "conversations.history", q, &data); err != nil { return ContentPage{}, err }
	page := ContentPage{Completeness: CompletenessComplete}
	// Only explicitly requested whole-channel reconciliations can authorize absence sweeps.
	page.Snapshot = strings.EqualFold(stringFromAny(conn.Config["reconcile"]), "true")
	for _, msg := range data.Messages {
		item, err := a.messageItem(ctx, conn, channel, msg)
		if err != nil { return ContentPage{}, err }
		page.Items = append(page.Items, item)
		if msg.ReplyCount > 0 {
			replies, err := a.threadMessages(ctx, conn, channel, msg.TS, limit)
			if err != nil { return ContentPage{}, err }
			for _, reply := range replies {
				item, err := a.messageItem(ctx, conn, channel, reply)
				if err != nil { return ContentPage{}, err }
				page.Items = append(page.Items, item)
				for _, file := range reply.Files {
					attachment, err := a.fileItem(ctx, conn, file.ID)
					if err != nil { return ContentPage{}, err }
					if attachment != nil { page.Items = append(page.Items, *attachment) }
				}
			}
		}
		for _, file := range msg.Files {
			attachment, err := a.fileItem(ctx, conn, file.ID)
			if err != nil { return ContentPage{}, err }
			if attachment != nil { page.Items = append(page.Items, *attachment) }
		}
	}
	page.NextCursor = Cursor(data.Metadata.NextCursor)
	if data.HasMore && page.NextCursor == "" { return ContentPage{}, errors.New("slack history has_more without next_cursor") }
	if !data.HasMore { page.NextCursor = "" }
	return page, nil
}

func (a slackAdapter) threadMessages(ctx context.Context, conn source.Connection, channel, parent string, limit int) ([]slackMessage, error) {
	var messages []slackMessage
	cursor := ""
	for pages := 0; pages < 100; pages++ {
		q := url.Values{"channel": {channel}, "ts": {parent}, "limit": {strconv.Itoa(limit)}}
		if cursor != "" { q.Set("cursor", cursor) }
		var data struct {
			Messages []slackMessage `json:"messages"`
			HasMore bool `json:"has_more"`
			Metadata struct { NextCursor string `json:"next_cursor"` } `json:"response_metadata"`
		}
		if err := a.call(ctx, conn, "conversations.replies", q, &data); err != nil { return nil, err }
		for _, message := range data.Messages {
			if message.TS != parent { messages = append(messages, message) }
		}
		if !data.HasMore { return messages, nil }
		if data.Metadata.NextCursor == "" { return nil, errors.New("slack replies has_more without next_cursor") }
		cursor = data.Metadata.NextCursor
	}
	return nil, errors.New("slack thread exceeded page bound")
}

type slackMessage struct {
	User string `json:"user"`
	Text string `json:"text"`
	TS string `json:"ts"`
	ThreadTS string `json:"thread_ts"`
	ReplyCount int `json:"reply_count"`
	Files []struct { ID string `json:"id"` } `json:"files"`
}

func (a slackAdapter) messageItem(ctx context.Context, conn source.Connection, channel string, msg slackMessage) (ContentItem, error) {
	if msg.TS == "" { return ContentItem{}, errors.New("slack message omitted ts") }
	q := url.Values{"channel": {channel}, "message_ts": {msg.TS}}
	var permalink struct { Permalink string `json:"permalink"` }
	if err := a.call(ctx, conn, "chat.getPermalink", q, &permalink); err != nil { return ContentItem{}, err }
	created := slackTimestamp(msg.TS)
	parent := msg.ThreadTS
	if parent != "" { parent = "slack:" + channel + ":" + parent }
	return ContentItem{NativeID: "slack:" + channel + ":" + msg.TS, ParentNativeID: parent,
		Kind: "message", NativeRevision: msg.TS, Filename: "slack-" + channel + "-" + msg.TS + ".txt", MIMEType: "text/plain",
		Text: "[" + msg.TS + " " + msg.User + "] " + msg.Text, URL: permalink.Permalink, CreatedAt: optionalTime(created)}, nil
}

func slackTimestamp(ts string) *time.Time {
	seconds, fraction, _ := strings.Cut(ts, ".")
	whole, err := strconv.ParseInt(seconds, 10, 64)
	if err != nil { return nil }
	nanos := int64(0)
	if fraction != "" {
		if len(fraction) > 9 { fraction = fraction[:9] }
		fraction = fraction + strings.Repeat("0", 9-len(fraction))
		nanos, err = strconv.ParseInt(fraction, 10, 64)
		if err != nil { return nil }
	}
	value := time.Unix(whole, nanos).UTC()
	return &value
}

func (a slackAdapter) fileItem(ctx context.Context, conn source.Connection, id string) (*ContentItem, error) {
	if id == "" { return nil, errors.New("slack message file omitted id") }
	var data struct { File struct {
		ID string `json:"id"`; Name string `json:"name"`; Title string `json:"title"`; Mimetype string `json:"mimetype"`; Permalink string `json:"permalink"`; External bool `json:"is_external"`; ExternalURL string `json:"external_url"`; Shares map[string]map[string][]struct { ChannelID string `json:"channel_id"` } `json:"shares"`
	} `json:"file"` }
	if err := a.call(ctx, conn, "files.info", url.Values{"file": {id}}, &data); err != nil { return nil, err }
	f := data.File
	if f.External { return nil, nil } // External targets require their own verified connector scope.
	if f.ID == "" { return nil, errors.New("slack files.info omitted file id") }
	name := firstNonEmpty(f.Name, f.Title, f.ID)
	return &ContentItem{NativeID: "slack:file:" + f.ID, Kind: "file", NativeRevision: f.ID, Filename: name, MIMEType: f.Mimetype, URL: f.Permalink}, nil
}

func (a slackAdapter) Access(ctx context.Context, conn source.Connection, cursor Cursor, limit int) (AccessPage, error) {
	if err := slackConnection(conn); err != nil { return AccessPage{}, err }
	if limit < 1 || limit > slackPageLimit { return AccessPage{}, errors.New("slack page limit must be between 1 and 200") }
	channel := slackChannel(conn)
	q := url.Values{"channel": {channel}}
	if cursor != "" { q.Set("cursor", string(cursor)) }
	var data struct { Members []string `json:"members"`; Metadata struct { NextCursor string `json:"next_cursor"` } `json:"response_metadata"` }
	err := a.call(ctx, conn, "conversations.members", q, &data)
	page := AccessPage{Completeness: CompletenessComplete}
	if err != nil { page.Completeness = CompletenessIncomplete; return page, err }
	for _, uid := range data.Members { page.Grants = append(page.Grants, AccessGrant{NativeObjectID: "slack:"+channel, NativeObjectKind: "channel", NativePrincipalID: uid, NativeGroupID: channel, Scope: "read", Effect: "allow", Inherited: true}) }
	page.NextCursor = Cursor(data.Metadata.NextCursor)
	page.Snapshot = true
	return page, nil
}

func (a slackAdapter) Membership(ctx context.Context, conn source.Connection, cursor Cursor, limit int) (MembershipPage, error) {
	page := MembershipPage{Completeness: CompletenessComplete, Snapshot: true}
	access, err := a.Access(ctx, conn, cursor, limit)
	if err != nil { page.Completeness = CompletenessIncomplete; return page, err }
	page.Memberships, page.NextCursor = access.Grants, access.NextCursor
	return page, nil
}

func (a slackAdapter) call(ctx context.Context, conn source.Connection, method string, query url.Values, out any) error {
	view := connectionView(conn)
	token, err := connectorToken(view)
	if err != nil { return err }
	base := strings.TrimRight(connectorBaseURL(view, "https://slack.com/api"), "/")
	endpoint := base + "/" + method
	if len(query) != 0 { endpoint += "?" + query.Encode() }
	req, err := http.NewRequestWithContext(ctx, http.MethodGet, endpoint, nil)
	if err != nil { return err }
	req.Header.Set("Authorization", "Bearer "+token)
	req.Header.Set("Accept", "application/json")
	resp, err := a.client.Do(req)
	if err != nil { return err }
	defer resp.Body.Close()
	if resp.StatusCode == http.StatusTooManyRequests {
		return fmt.Errorf("slack %s rate limited (Retry-After %s)", method, resp.Header.Get("Retry-After"))
	}
	if resp.StatusCode < 200 || resp.StatusCode >= 300 { return fmt.Errorf("slack %s returned HTTP %d", method, resp.StatusCode) }
	var payload json.RawMessage
	if err := json.NewDecoder(resp.Body).Decode(&payload); err != nil { return err }
	var envelope struct { OK bool `json:"ok"`; Error string `json:"error"` }
	if err := json.Unmarshal(payload, &envelope); err != nil { return err }
	if !envelope.OK { return fmt.Errorf("slack %s failed: %s", method, firstNonEmpty(envelope.Error, "unknown_error")) }
	return json.Unmarshal(payload, out)
}

func slackChannel(conn source.Connection) string { return strings.TrimSpace(firstNonEmpty(stringFromAny(conn.Config["channel_id"]), stringFromAny(conn.Config["channel"]))) }
func slackConnection(conn source.Connection) error {
	if conn.Kind != string(source.ConnectionSlack) { return errors.New("slack adapter kind does not match connection") }
	if slackChannel(conn) == "" { return errors.New("slack config.channel_id is required") }
	return nil
}
