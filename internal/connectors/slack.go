package connectors

import (
	"context"
	"encoding/base64"
	"encoding/json"
	"errors"
	"fmt"
	"net/http"
	"net/url"
	"regexp"
	"strconv"
	"strings"
	"time"

	"enclava-go/internal/auth"
	"enclava-go/internal/source"
)

const (
	slackPageLimit = 200
	slackAPIBase   = "https://slack.com/api"
	// Slack answers bursts with 429 + Retry-After; waiting keeps a large channel import alive.
	slackRateLimitRetries = 5
	slackMaxRetryWait     = time.Minute
)

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

// slackContentCursor follows Slack's own history and replies cursors. Each
// message is one item and its file attachments queue behind it, so every API
// page is consumed whole and a page never exceeds the requested item bound.
type slackContentCursor struct {
	History     string   `json:"history,omitempty"`
	HistoryDone bool     `json:"history_done,omitempty"`
	Threads     []string `json:"threads,omitempty"`
	Replies     string   `json:"replies,omitempty"`
	Files       []string `json:"files,omitempty"`
}

func encodeSlackContentCursor(cursor slackContentCursor) Cursor {
	raw, _ := json.Marshal(cursor)
	return Cursor("content:" + base64.RawURLEncoding.EncodeToString(raw))
}

func decodeSlackContentCursor(cursor Cursor) (slackContentCursor, error) {
	var state slackContentCursor
	rest, ok := strings.CutPrefix(string(cursor), "content:")
	if !ok {
		// Earlier releases stored raw Slack cursors; relisting is safe because sync dedupes on content hash.
		return state, nil
	}
	raw, err := base64.RawURLEncoding.DecodeString(rest)
	if err != nil {
		return state, errors.New("invalid slack content cursor")
	}
	if err := json.Unmarshal(raw, &state); err != nil {
		return state, errors.New("invalid slack content cursor")
	}
	return state, nil
}

type slackAdapter struct {
	client  *http.Client
	baseURL string
	token   string
	// Lookups cached for one sync attempt; an adapter is built per attempt.
	users           map[string]string
	directoryLoaded bool
	title           *string
	workspaceURL    string
}

// NewSlackAdapter returns the native Slack Web API adapter. The bot token is
// resolved through SECRET_BINDINGS for the connection's tenant and the Slack
// API origin, so a stored credential_ref is never read as an environment name
// and the token is never sent to an unbound origin.
func NewSlackAdapter(conn source.Connection, resolver CredentialResolver) (Adapter, error) {
	if err := slackConnection(conn); err != nil {
		return nil, err
	}
	ref := strings.TrimSpace(conn.CredentialRef)
	if ref == "" || resolver == nil {
		return nil, errors.New("slack credential_ref is required")
	}
	base := strings.TrimRight(strings.TrimSpace(stringFromAny(conn.Config["base_url"])), "/")
	if base == "" {
		base = slackAPIBase
	}
	parsed, err := requireAPIOrigin(base)
	if err != nil {
		return nil, errors.New("slack config.base_url: " + err.Error())
	}
	token, err := resolver.Resolve(conn.TenantID, auth.SecretPurposeConnector, parsed.Scheme+"://"+parsed.Host, ref)
	if err != nil {
		return nil, fmt.Errorf("resolve slack credential: %w", err)
	}
	if token == "" {
		return nil, errors.New("slack credential_ref is required")
	}
	return &slackAdapter{client: newConnectorHTTPClient(), baseURL: base, token: token, users: map[string]string{}}, nil
}

func (*slackAdapter) Capabilities() AdapterCapabilities {
	return AdapterCapabilities{Content: true, Access: true, Membership: true, UnsupportedScopes: []string{"direct_messages", "group_direct_messages", "canvases", "lists", "slack_connect_external_membership_mapping"}}
}

func (a *slackAdapter) Content(ctx context.Context, conn source.Connection, cursor Cursor, limit int) (ContentPage, error) {
	if err := slackConnection(conn); err != nil {
		return ContentPage{}, err
	}
	if limit < 1 || limit > slackPageLimit {
		return ContentPage{}, errors.New("slack page limit must be between 1 and 200")
	}
	state, err := decodeSlackContentCursor(cursor)
	if err != nil {
		return ContentPage{}, err
	}
	channel := slackChannel(conn)
	// Only explicitly requested whole-channel reconciliations can authorize absence sweeps.
	page := ContentPage{Completeness: CompletenessComplete, Snapshot: strings.EqualFold(stringFromAny(conn.Config["reconcile"]), "true")}
	for emptyPages := 0; len(page.Items) < limit; {
		if len(state.Files) > 0 {
			file, err := a.fileItem(ctx, state.Files[0])
			if err != nil {
				return ContentPage{}, err
			}
			if file != nil {
				page.Items = append(page.Items, *file)
			}
			state.Files = state.Files[1:]
			continue
		}
		thread := len(state.Threads) > 0
		if !thread && state.HistoryDone {
			break
		}
		budget := limit - len(page.Items)
		var batch []slackMessage
		var next string
		if thread {
			batch, next, err = a.replies(ctx, channel, state.Threads[0], state.Replies, budget)
		} else {
			batch, next, err = a.history(ctx, channel, state.History, budget)
		}
		if err != nil {
			return ContentPage{}, err
		}
		if len(batch) > budget {
			return ContentPage{}, errors.New("slack returned more messages than requested")
		}
		if len(batch) == 0 && next != "" {
			if emptyPages++; emptyPages > 100 {
				return ContentPage{}, errors.New("slack returned too many empty pages")
			}
		}
		for _, msg := range batch {
			if msg.TS == "" {
				return ContentPage{}, errors.New("slack message omitted ts")
			}
			// Thread replies also sent to the channel are listed with their thread.
			if !thread && msg.ThreadTS != "" && msg.ThreadTS != msg.TS {
				continue
			}
			if !thread && msg.ReplyCount > 0 {
				state.Threads = append(state.Threads, msg.TS)
			}
			if slackNoticeSubtypes[msg.Subtype] {
				continue
			}
			item, err := a.messageItem(ctx, channel, msg)
			if err != nil {
				return ContentPage{}, err
			}
			page.Items = append(page.Items, item)
			for _, file := range msg.Files {
				state.Files = append(state.Files, file.ID)
			}
		}
		switch {
		case thread && next == "":
			state.Threads, state.Replies = state.Threads[1:], ""
		case thread:
			state.Replies = next
		case next == "":
			state.HistoryDone = true
		default:
			state.History = next
		}
	}
	if !state.HistoryDone || len(state.Threads) > 0 || len(state.Files) > 0 {
		page.NextCursor = encodeSlackContentCursor(state)
	}
	return page, nil
}

// history lists up to limit channel messages, newest first, and the cursor
// of the next page ("" at the end).
func (a *slackAdapter) history(ctx context.Context, channel, cursor string, limit int) ([]slackMessage, string, error) {
	q := url.Values{"channel": {channel}, "limit": {strconv.Itoa(limit)}}
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
	if err := a.call(ctx, "conversations.history", q, &data); err != nil {
		return nil, "", err
	}
	if !data.HasMore {
		return data.Messages, "", nil
	}
	if data.Metadata.NextCursor == "" {
		return nil, "", errors.New("slack history has_more without next_cursor")
	}
	return data.Messages, data.Metadata.NextCursor, nil
}

// replies lists up to limit replies of parent, oldest first, and the cursor
// of the next page ("" at the end). Slack repeats the parent on every page;
// it is dropped here.
func (a *slackAdapter) replies(ctx context.Context, channel, parent, cursor string, limit int) ([]slackMessage, string, error) {
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
	if err := a.call(ctx, "conversations.replies", q, &data); err != nil {
		return nil, "", err
	}
	out := make([]slackMessage, 0, len(data.Messages))
	for _, msg := range data.Messages {
		if msg.TS == parent {
			continue
		}
		msg.ThreadTS = parent
		out = append(out, msg)
	}
	if !data.HasMore {
		return out, "", nil
	}
	if data.Metadata.NextCursor == "" {
		return nil, "", errors.New("slack replies has_more without next_cursor")
	}
	return out, data.Metadata.NextCursor, nil
}

func (a *slackAdapter) threadMessages(ctx context.Context, channel, parent string, limit int) ([]slackMessage, error) {
	var messages []slackMessage
	cursor := ""
	for pages := 0; pages < 100; pages++ {
		batch, next, err := a.replies(ctx, channel, parent, cursor, limit)
		if err != nil {
			return nil, err
		}
		messages = append(messages, batch...)
		if next == "" {
			return messages, nil
		}
		cursor = next
	}
	return nil, errors.New("slack thread exceeded page bound")
}

type slackMessage struct {
	User       string `json:"user"`
	Username   string `json:"username"`
	Subtype    string `json:"subtype"`
	Text       string `json:"text"`
	TS         string `json:"ts"`
	ThreadTS   string `json:"thread_ts"`
	ReplyCount int    `json:"reply_count"`
	BotProfile struct {
		Name string `json:"name"`
	} `json:"bot_profile"`
	Files []struct {
		ID string `json:"id"`
	} `json:"files"`
}

// slackNoticeSubtypes are channel notices with no conversation content.
var slackNoticeSubtypes = map[string]bool{
	"channel_join": true, "channel_leave": true, "channel_topic": true, "channel_purpose": true,
	"channel_name": true, "channel_archive": true, "channel_unarchive": true, "channel_convert_to_private": true,
	"group_join": true, "group_leave": true, "group_topic": true, "group_purpose": true,
	"group_name": true, "group_archive": true, "group_unarchive": true, "tombstone": true,
}

func (a *slackAdapter) messageItem(ctx context.Context, channel string, msg slackMessage) (ContentItem, error) {
	link, err := a.permalink(ctx, channel, msg)
	if err != nil {
		return ContentItem{}, err
	}
	title := a.channelTitle(ctx, channel)
	parent, parentKind := "slack:"+channel, "channel"
	if msg.ThreadTS != "" && msg.ThreadTS != msg.TS {
		parent, parentKind = "slack:"+channel+":"+msg.ThreadTS, "message"
	}
	return ContentItem{NativeID: "slack:" + channel + ":" + msg.TS, ParentNativeID: parent, ParentKind: parentKind,
		Kind: "message", NativeRevision: msg.TS, Title: title, Filename: "slack-" + channel + "-" + msg.TS + ".txt", MIMEType: "text/plain",
		Text: a.messageText(ctx, title, msg), URL: link, CreatedAt: slackTimestamp(msg.TS)}, nil
}

// messageText renders a message as a header line (channel, author, time)
// followed by its text with Slack markup resolved to readable names.
func (a *slackAdapter) messageText(ctx context.Context, title string, msg slackMessage) string {
	var header []string
	if title != "" {
		header = append(header, "#"+title)
	}
	if msg.ThreadTS != "" && msg.ThreadTS != msg.TS {
		header = append(header, "thread reply")
	}
	author := firstNonEmpty(msg.Username, msg.BotProfile.Name, "unknown")
	if msg.User != "" {
		author = a.userName(ctx, msg.User)
	}
	header = append(header, author)
	if created := slackTimestamp(msg.TS); created != nil {
		header = append(header, created.Format("2006-01-02 15:04 UTC"))
	}
	return strings.Join(header, " · ") + "\n" + slackPlainText(msg.Text, func(id string) string { return a.userName(ctx, id) })
}

var slackMarkupPattern = regexp.MustCompile(`<([^<>]*)>`)

// slackPlainText resolves Slack message markup (mentions, channel and link
// references, special mentions) and the three HTML entities Slack escapes.
func slackPlainText(text string, userName func(string) string) string {
	out := slackMarkupPattern.ReplaceAllStringFunc(text, func(match string) string {
		target, label, _ := strings.Cut(match[1:len(match)-1], "|")
		switch {
		case strings.HasPrefix(target, "@"):
			id := target[1:]
			if name := userName(id); name != id || label == "" {
				return "@" + name
			}
			return "@" + label
		case strings.HasPrefix(target, "#"):
			if label != "" {
				return "#" + label
			}
			return target
		case strings.HasPrefix(target, "!"):
			if label != "" {
				return label
			}
			return "@" + target[1:]
		case label != "" && label != strings.TrimPrefix(target, "mailto:"):
			return label + " (" + target + ")"
		default:
			return strings.TrimPrefix(target, "mailto:")
		}
	})
	return strings.NewReplacer("&lt;", "<", "&gt;", ">", "&amp;", "&").Replace(out)
}

type slackUser struct {
	ID       string `json:"id"`
	Name     string `json:"name"`
	RealName string `json:"real_name"`
	Profile  struct {
		DisplayName string `json:"display_name"`
		RealName    string `json:"real_name"`
	} `json:"profile"`
}

func (u slackUser) displayName() string {
	return firstNonEmpty(strings.TrimSpace(u.Profile.DisplayName), strings.TrimSpace(u.Profile.RealName), strings.TrimSpace(u.RealName), strings.TrimSpace(u.Name))
}

// userName returns a member's display name. The workspace directory is read
// once per sync (users.list, 200 per call); users.info covers anyone missing
// from it, and the member ID is the fallback (for example without users:read).
func (a *slackAdapter) userName(ctx context.Context, id string) string {
	if name, ok := a.users[id]; ok {
		return name
	}
	if !a.directoryLoaded {
		a.directoryLoaded = true
		a.loadDirectory(ctx)
		if name, ok := a.users[id]; ok {
			return name
		}
	}
	var data struct {
		User slackUser `json:"user"`
	}
	name := id
	if err := a.call(ctx, "users.info", url.Values{"user": {id}}, &data); err == nil {
		name = firstNonEmpty(data.User.displayName(), id)
	}
	a.users[id] = name
	return name
}

// loadDirectory caches workspace member names; on failure the remaining
// lookups fall back to users.info.
func (a *slackAdapter) loadDirectory(ctx context.Context) {
	cursor := ""
	for pages := 0; pages < 100; pages++ {
		q := url.Values{"limit": {"200"}}
		if cursor != "" {
			q.Set("cursor", cursor)
		}
		var data struct {
			Members  []slackUser `json:"members"`
			Metadata struct {
				NextCursor string `json:"next_cursor"`
			} `json:"response_metadata"`
		}
		if err := a.call(ctx, "users.list", q, &data); err != nil {
			return
		}
		for _, u := range data.Members {
			if name := u.displayName(); u.ID != "" && name != "" {
				a.users[u.ID] = name
			}
		}
		if cursor = data.Metadata.NextCursor; cursor == "" {
			return
		}
	}
}

func (a *slackAdapter) channelTitle(ctx context.Context, channel string) string {
	if a.title != nil {
		return *a.title
	}
	var info struct {
		Channel struct {
			Name string `json:"name"`
		} `json:"channel"`
	}
	title := ""
	if err := a.call(ctx, "conversations.info", url.Values{"channel": {channel}}, &info); err == nil {
		title = info.Channel.Name
	}
	a.title = &title
	return title
}

// permalink builds Slack's canonical message link from the workspace URL,
// one auth.test call per sync instead of chat.getPermalink per message.
func (a *slackAdapter) permalink(ctx context.Context, channel string, msg slackMessage) (string, error) {
	if a.workspaceURL == "" {
		var data struct {
			URL string `json:"url"`
		}
		if err := a.call(ctx, "auth.test", nil, &data); err != nil {
			return "", err
		}
		u, err := url.Parse(data.URL)
		if err != nil || u.Scheme != "https" || u.Host == "" {
			return "", errors.New("slack auth.test returned no workspace url")
		}
		a.workspaceURL = "https://" + u.Host
	}
	link := a.workspaceURL + "/archives/" + url.PathEscape(channel) + "/p" + strings.Replace(msg.TS, ".", "", 1)
	if msg.ThreadTS != "" && msg.ThreadTS != msg.TS {
		link += "?" + url.Values{"thread_ts": {msg.ThreadTS}, "cid": {channel}}.Encode()
	}
	return link, nil
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

func (a *slackAdapter) fileItem(ctx context.Context, id string) (*ContentItem, error) {
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
	if err := a.call(ctx, "files.info", url.Values{"file": {id}}, &data); err != nil {
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

func (a *slackAdapter) Access(ctx context.Context, conn source.Connection, cursor Cursor, limit int) (AccessPage, error) {
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
		if err := a.call(ctx, "conversations.members", q, &data); err != nil {
			page.Completeness = CompletenessIncomplete
			return page, err
		}
		for _, uid := range data.Members {
			page.Grants = append(page.Grants, AccessGrant{NativeObjectID: "slack:" + channel, NativeObjectKind: "channel", NativePrincipalID: uid, NativePrincipalName: a.userName(ctx, uid), NativeGroupID: channel, Scope: "read", Effect: "allow"})
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
		if err := a.call(ctx, "conversations.history", q, &data); err != nil {
			page.Completeness = CompletenessIncomplete
			return page, err
		}
		for _, msg := range data.Messages {
			for _, file := range msg.Files {
				grants, err := a.fileShareGrants(ctx, file.ID)
				if err != nil {
					page.Completeness = CompletenessIncomplete
					return page, err
				}
				page.Grants = append(page.Grants, grants...)
			}
			if msg.ReplyCount > 0 {
				replies, err := a.threadMessages(ctx, channel, msg.TS, limit)
				if err != nil {
					page.Completeness = CompletenessIncomplete
					return page, err
				}
				for _, reply := range replies {
					for _, file := range reply.Files {
						grants, err := a.fileShareGrants(ctx, file.ID)
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

func (a *slackAdapter) fileShareGrants(ctx context.Context, id string) ([]AccessGrant, error) {
	var data struct {
		File struct {
			ID       string                                `json:"id"`
			External bool                                  `json:"is_external"`
			Shares   map[string]map[string]json.RawMessage `json:"shares"`
		} `json:"file"`
	}
	if err := a.call(ctx, "files.info", url.Values{"file": {id}}, &data); err != nil {
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

func (a *slackAdapter) Membership(ctx context.Context, conn source.Connection, cursor Cursor, limit int) (MembershipPage, error) {
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
	if err := a.call(ctx, "conversations.members", q, &data); err != nil {
		page.Completeness = CompletenessIncomplete
		return page, err
	}
	for _, uid := range data.Members {
		page.Memberships = append(page.Memberships, AccessGrant{NativeGroupID: slackChannel(conn), NativePrincipalID: uid, NativePrincipalName: a.userName(ctx, uid), Scope: "member", Effect: "allow"})
	}
	page.NextCursor = Cursor(data.Metadata.NextCursor)
	return page, nil
}

// call performs one Slack Web API GET, waiting out rate limits a bounded
// number of times before failing with the advertised Retry-After.
func (a *slackAdapter) call(ctx context.Context, method string, query url.Values, out any) error {
	endpoint := a.baseURL + "/" + method
	if len(query) != 0 {
		endpoint += "?" + query.Encode()
	}
	for attempt := 0; ; attempt++ {
		retryAfter, limited, err := a.attempt(ctx, method, endpoint, out)
		if !limited {
			return err
		}
		seconds, parseErr := strconv.Atoi(strings.TrimSpace(retryAfter))
		wait := time.Duration(seconds) * time.Second
		if parseErr != nil || seconds < 0 || wait > slackMaxRetryWait || attempt >= slackRateLimitRetries {
			return fmt.Errorf("slack %s rate limited (Retry-After %s)", method, retryAfter)
		}
		timer := time.NewTimer(wait)
		select {
		case <-ctx.Done():
			timer.Stop()
			return ctx.Err()
		case <-timer.C:
		}
	}
}

// attempt reports the Retry-After header and true when Slack rate limited the request.
func (a *slackAdapter) attempt(ctx context.Context, method, endpoint string, out any) (string, bool, error) {
	req, err := http.NewRequestWithContext(ctx, http.MethodGet, endpoint, nil)
	if err != nil {
		return "", false, err
	}
	req.Header.Set("Authorization", "Bearer "+a.token)
	req.Header.Set("Accept", "application/json")
	resp, err := a.client.Do(req)
	if err != nil {
		return "", false, err
	}
	defer func() { _ = resp.Body.Close() }()
	if resp.StatusCode == http.StatusTooManyRequests {
		return resp.Header.Get("Retry-After"), true, nil
	}
	if resp.StatusCode < 200 || resp.StatusCode >= 300 {
		return "", false, fmt.Errorf("slack %s returned HTTP %d", method, resp.StatusCode)
	}
	var payload json.RawMessage
	if err := json.NewDecoder(resp.Body).Decode(&payload); err != nil {
		return "", false, err
	}
	var envelope struct {
		OK    bool   `json:"ok"`
		Error string `json:"error"`
	}
	if err := json.Unmarshal(payload, &envelope); err != nil {
		return "", false, err
	}
	if !envelope.OK {
		return "", false, fmt.Errorf("slack %s failed: %s", method, firstNonEmpty(envelope.Error, "unknown_error"))
	}
	return "", false, json.Unmarshal(payload, out)
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
