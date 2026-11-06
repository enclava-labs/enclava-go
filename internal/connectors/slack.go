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
	"sort"
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
	// Delta runs re-read this much history behind each channel's watermark so
	// late-arriving messages and thread replies near the boundary are not missed.
	slackDeltaOverlap = 6 * time.Hour
)

// slackContentCursor follows Slack's own history and replies cursors. Each
// message is one item and its file attachments queue behind it, so every API
// page is consumed whole and a page never exceeds the requested item bound.
// Channel is the channel in progress when the source syncs every channel. A
// cursor with no Channel is a run checkpoint, not a position: Full runs list
// everything, delta runs read each channel back to Oldest (its watermark)
// minus slackDeltaOverlap and advance Seen to the newest ts read. FullAt is
// the RFC3339 time the last full listing started; Started is this run's.
type slackContentCursor struct {
	Channel     string   `json:"channel,omitempty"`
	History     string   `json:"history,omitempty"`
	HistoryDone bool     `json:"history_done,omitempty"`
	Threads     []string `json:"threads,omitempty"`
	Replies     string   `json:"replies,omitempty"`
	Files       []string `json:"files,omitempty"`

	Full    bool              `json:"full,omitempty"`
	FullAt  string            `json:"full_at,omitempty"`
	Started string            `json:"started,omitempty"`
	Oldest  map[string]string `json:"oldest,omitempty"`
	Seen    map[string]string `json:"seen,omitempty"`
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

// checkpoint returns the stored cursor a completed run starts the next one
// from: no position, the full-listing time advanced on full runs, and each
// channel's watermark. Full runs drop channels that were not listed.
func (c slackContentCursor) checkpoint() slackContentCursor {
	out := slackContentCursor{FullAt: c.FullAt, Oldest: map[string]string{}}
	if c.Full {
		out.FullAt = c.Started
	}
	if !c.Full {
		for channel, ts := range c.Oldest {
			out.Oldest[channel] = ts
		}
	}
	for channel, ts := range c.Seen {
		out.Oldest[channel] = maxSlackTimestamp(out.Oldest[channel], ts)
	}
	return out
}

// resetChannel clears the position of a finished channel without losing the
// run-level fields (Full, FullAt, Started, Oldest, Seen).
func (c *slackContentCursor) resetChannel() {
	c.Channel, c.History, c.HistoryDone, c.Threads, c.Replies, c.Files = "", "", false, nil, "", nil
}

// slackHistoryOldest backs a channel watermark off by the delta overlap for
// conversations.history's oldest parameter; "" lists the channel in full.
func slackHistoryOldest(ts string) string {
	t := slackTimestamp(ts)
	if t == nil {
		return ""
	}
	oldest := t.Add(-slackDeltaOverlap)
	return fmt.Sprintf("%d.%06d", oldest.Unix(), oldest.Nanosecond()/1000)
}

func slackCursorTime(s string) time.Time {
	t, _ := time.Parse(time.RFC3339, s)
	return t
}

type slackAdapter struct {
	client  *http.Client
	baseURL string
	token   string
	// Lookups cached for one sync attempt; an adapter is built per attempt.
	users           map[string]string
	directoryLoaded bool
	titles          map[string]string
	channels        []string // all-channels mode: member channels, sorted
	channelsLoaded  bool
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
	return AdapterCapabilities{Content: true, UnsupportedScopes: []string{"direct_messages", "group_direct_messages", "canvases", "lists", "slack_connect_external_membership_mapping"}}
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
	if state.Channel == "" {
		// No channel in progress: a completed run's checkpoint (or nothing)
		// starts the next run, full when fullSyncDue says so, else a delta
		// that re-reads each channel from its Oldest watermark.
		state.Full = fullSyncDue(conn.Config, slackCursorTime(state.FullAt), time.Now())
		state.Started = time.Now().UTC().Format(time.RFC3339)
		state.Seen = nil
		if state.Full {
			// A full listing ignores watermarks; the next checkpoint's Oldest is rebuilt from Seen.
			state.Oldest = nil
		}
	}
	// A source without channel_id syncs every channel the credential is a
	// member of, one after another in channel-ID order.
	channel := slackChannel(conn)
	if channel == "" {
		if channel = state.Channel; channel == "" {
			if channel, err = a.channelAfter(ctx, conn, ""); err != nil {
				return ContentPage{}, err
			}
		}
	}
	// Absence sweeps run only on explicitly requested reconciliations, and
	// only a full listing can prove an item is gone.
	page := ContentPage{Completeness: CompletenessComplete, Snapshot: state.Full && strings.EqualFold(stringFromAny(conn.Config["reconcile"]), "true")}
	for emptyPages := 0; channel != "" && len(page.Items) < limit; {
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
			if channel, err = a.channelAfter(ctx, conn, channel); err != nil {
				return ContentPage{}, err
			}
			state.resetChannel()
			continue
		}
		budget := limit - len(page.Items)
		var batch []slackMessage
		var next string
		if thread {
			batch, next, err = a.replies(ctx, channel, state.Threads[0], state.Replies, budget)
		} else {
			batch, next, err = a.history(ctx, channel, state.History, slackHistoryOldest(state.Oldest[channel]), budget)
		}
		if err != nil {
			if slackChannel(conn) == "" && slackChannelGone(err) {
				// Left or deleted since the channel list was taken: move on.
				state.resetChannel()
				state.HistoryDone = true
				continue
			}
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
			if state.Seen == nil {
				state.Seen = map[string]string{}
			}
			state.Seen[channel] = maxSlackTimestamp(state.Seen[channel], msg.TS)
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
	if channel != "" && state.HistoryDone && len(state.Threads) == 0 && len(state.Files) == 0 {
		// The page filled exactly as this channel finished.
		if channel, err = a.channelAfter(ctx, conn, channel); err != nil {
			return ContentPage{}, err
		}
		state.resetChannel()
	}
	if channel != "" {
		state.Channel = channel
		page.NextCursor = encodeSlackContentCursor(state)
	} else {
		page.Checkpoint = encodeSlackContentCursor(state.checkpoint())
	}
	return page, nil
}

// history lists up to limit channel messages, newest first, and the cursor
// of the next page ("" at the end). oldest bounds how far back the listing
// goes ("" = the whole channel); it repeats on every page because Slack's
// cursor does not carry the bounds forward.
func (a *slackAdapter) history(ctx context.Context, channel, cursor, oldest string, limit int) ([]slackMessage, string, error) {
	q := url.Values{"channel": {channel}, "limit": {strconv.Itoa(limit)}}
	if cursor != "" {
		q.Set("cursor", cursor)
	}
	if oldest != "" {
		q.Set("oldest", oldest)
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
	if title, ok := a.titles[channel]; ok {
		return title
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
	a.setTitle(channel, title)
	return title
}

func (a *slackAdapter) setTitle(channel, title string) {
	if a.titles == nil {
		a.titles = map[string]string{}
	}
	a.titles[channel] = title
}

// channelAfter returns the channel to sync after current ("" = the first):
// the configured channel_id, or — when the source names none — the next
// channel the credential is a member of, in ID order. "" means done.
func (a *slackAdapter) channelAfter(ctx context.Context, conn source.Connection, current string) (string, error) {
	if configured := slackChannel(conn); configured != "" {
		if current == "" {
			return configured, nil
		}
		return "", nil
	}
	if !a.channelsLoaded {
		if err := a.loadChannels(ctx); err != nil {
			return "", err
		}
	}
	i := sort.SearchStrings(a.channels, current)
	if i < len(a.channels) && a.channels[i] == current {
		i++
	}
	if i < len(a.channels) {
		return a.channels[i], nil
	}
	return "", nil
}

// loadChannels caches the member channels in ID order (the sync order) and
// their names for message headers.
func (a *slackAdapter) loadChannels(ctx context.Context) error {
	channels, err := a.memberChannels(ctx)
	if err != nil {
		return err
	}
	for _, ch := range channels {
		a.channels = append(a.channels, ch.ID)
		a.setTitle(ch.ID, ch.Name)
	}
	sort.Strings(a.channels)
	a.channelsLoaded = true
	return nil
}

// SlackChannel is one channel a Slack credential is a member of.
type SlackChannel struct {
	ID      string `json:"id"`
	Name    string `json:"name"`
	Private bool   `json:"is_private"`
}

// memberChannels lists the public and private channels the credential is a
// member of (users.conversations: bot and user tokens alike see exactly the
// channels they can read), archived channels excluded.
func (a *slackAdapter) memberChannels(ctx context.Context) ([]SlackChannel, error) {
	var out []SlackChannel
	cursor := ""
	for pages := 0; pages < 100; pages++ {
		q := url.Values{"types": {"public_channel,private_channel"}, "exclude_archived": {"true"}, "limit": {"200"}}
		if cursor != "" {
			q.Set("cursor", cursor)
		}
		var data struct {
			Channels []SlackChannel `json:"channels"`
			Metadata struct {
				NextCursor string `json:"next_cursor"`
			} `json:"response_metadata"`
		}
		if err := a.call(ctx, "users.conversations", q, &data); err != nil {
			return nil, err
		}
		for _, ch := range data.Channels {
			if ch.ID != "" {
				out = append(out, ch)
			}
		}
		if cursor = data.Metadata.NextCursor; cursor == "" {
			return out, nil
		}
	}
	return nil, errors.New("slack channel listing exceeded 100 pages")
}

// SlackChannels lists, by name, the channels a Slack credential is a member
// of — the add-source wizard's channel picker. baseURL is the source's
// optional config.base_url; the credential is still released only to its
// bound origin.
func (s *Service) SlackChannels(ctx context.Context, p auth.Principal, credentialRef, baseURL string) ([]SlackChannel, error) {
	if !auth.HasScope(p, "connectors.manage") {
		return nil, source.ErrDenied
	}
	conn := source.Connection{TenantID: p.TenantID, Kind: source.ConnectionSlack, CredentialRef: credentialRef, Config: map[string]any{}}
	if baseURL = strings.TrimSpace(baseURL); baseURL != "" {
		conn.Config["base_url"] = baseURL
	}
	adapter, err := NewSlackAdapter(conn, s.secrets)
	if err != nil {
		return nil, err
	}
	channels, err := adapter.(*slackAdapter).memberChannels(ctx)
	if err != nil {
		return nil, err
	}
	sort.Slice(channels, func(i, j int) bool { return channels[i].Name < channels[j].Name })
	return channels, nil
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
		return "", false, slackAPIError{method: method, code: firstNonEmpty(envelope.Error, "unknown_error")}
	}
	return "", false, json.Unmarshal(payload, out)
}

// slackAPIError is a Web API error envelope ({"ok": false, "error": code}).
type slackAPIError struct{ method, code string }

func (e slackAPIError) Error() string { return "slack " + e.method + " failed: " + e.code }

// slackChannelGone reports a channel the credential can no longer read.
func slackChannelGone(err error) bool {
	var apiErr slackAPIError
	return errors.As(err, &apiErr) && (apiErr.code == "not_in_channel" || apiErr.code == "channel_not_found")
}

// slackChannel is the configured channel; "" syncs every channel the
// credential is a member of.
func slackChannel(conn source.Connection) string {
	return strings.TrimSpace(firstNonEmpty(stringFromAny(conn.Config["channel_id"]), stringFromAny(conn.Config["channel"])))
}
func slackConnection(conn source.Connection) error {
	if conn.Kind != string(source.ConnectionSlack) {
		return errors.New("slack adapter kind does not match connection")
	}
	return nil
}
