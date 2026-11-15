package connectors

import (
	"bytes"
	"context"
	"encoding/json"
	"errors"
	"fmt"
	"net/http"
	"net/http/httptest"
	"sort"
	"strconv"
	"strings"
	"sync"
	"sync/atomic"
	"testing"
	"time"

	"enclava-go/internal/auth"
	"enclava-go/internal/source"
)

// fakeSlack serves conversations.history/replies with Slack's cursor paging
// over fixed messages, plus the lookups the adapter makes.
type fakeSlack struct {
	t         *testing.T
	history   []map[string]any            // channel messages, any order
	byChannel map[string][]map[string]any // per-channel history; falls back to history
	channels  []map[string]any            // member channels for users.conversations
	replies   map[string][]map[string]any // thread ts -> replies, any order
	mu        sync.Mutex
	calls     map[string]int
	oldest    map[string]string // channel -> oldest param of its last history call
	override  func(w http.ResponseWriter, r *http.Request) bool
}

func slackTS(m map[string]any) float64 {
	v, _ := strconv.ParseFloat(m["ts"].(string), 64)
	return v
}

func (f *fakeSlack) count(path string) int {
	f.mu.Lock()
	defer f.mu.Unlock()
	return f.calls[path]
}

// lastOldest returns the oldest param of channel's last history call.
func (f *fakeSlack) lastOldest(channel string) string {
	f.mu.Lock()
	defer f.mu.Unlock()
	return f.oldest[channel]
}

// slackCursorPage pages msgs with opaque "o:<offset>" cursors, as Slack does.
func slackCursorPage(msgs []map[string]any, lead map[string]any, cursor string, limit int) map[string]any {
	offset, _ := strconv.Atoi(strings.TrimPrefix(cursor, "o:"))
	msgs = msgs[min(offset, len(msgs)):]
	page := map[string]any{"ok": true, "has_more": len(msgs) > limit}
	if len(msgs) > limit {
		msgs = msgs[:limit]
		page["response_metadata"] = map[string]any{"next_cursor": "o:" + strconv.Itoa(offset+limit)}
	}
	if lead != nil {
		msgs = append([]map[string]any{lead}, msgs...)
	}
	page["messages"] = msgs
	return page
}

func (f *fakeSlack) ServeHTTP(w http.ResponseWriter, r *http.Request) {
	f.mu.Lock()
	if f.calls == nil {
		f.calls = map[string]int{}
	}
	f.calls[r.URL.Path]++
	f.mu.Unlock()
	if f.override != nil && f.override(w, r) {
		return
	}
	if r.Header.Get("Authorization") != "Bearer test-token" {
		f.t.Errorf("authorization = %q", r.Header.Get("Authorization"))
	}
	q := r.URL.Query()
	limit, _ := strconv.Atoi(q.Get("limit"))
	write := func(v any) {
		if err := json.NewEncoder(w).Encode(v); err != nil {
			f.t.Error(err)
		}
	}
	switch r.URL.Path {
	case "/auth.test":
		write(map[string]any{"ok": true, "url": "https://workspace.slack.com/"})
	case "/conversations.info":
		write(map[string]any{"ok": true, "channel": map[string]any{"name": "engineering"}})
	case "/users.list":
		// Only U1 is in the directory; U2 (say, a Slack Connect guest) needs users.info.
		write(map[string]any{"ok": true, "members": []map[string]any{{"id": "U1", "name": "alice", "profile": map[string]any{"display_name": "Alice"}}}})
	case "/users.info":
		if q.Get("user") != "U2" {
			write(map[string]any{"ok": false, "error": "user_not_found"})
			return
		}
		write(map[string]any{"ok": true, "user": map[string]any{"id": "U2", "name": "bob", "real_name": "Bob"}})
	case "/conversations.history":
		msgs := f.history
		if f.byChannel != nil {
			msgs = f.byChannel[q.Get("channel")]
		}
		msgs = append([]map[string]any(nil), msgs...)
		sort.Slice(msgs, func(i, j int) bool { return slackTS(msgs[i]) > slackTS(msgs[j]) })
		f.mu.Lock()
		if f.oldest == nil {
			f.oldest = map[string]string{}
		}
		f.oldest[q.Get("channel")] = q.Get("oldest")
		f.mu.Unlock()
		write(slackCursorPage(msgs, nil, q.Get("cursor"), limit))
	case "/users.conversations":
		write(map[string]any{"ok": true, "channels": f.channels})
	case "/conversations.replies":
		parent := q.Get("ts")
		msgs := append([]map[string]any(nil), f.replies[parent]...)
		sort.Slice(msgs, func(i, j int) bool { return slackTS(msgs[i]) < slackTS(msgs[j]) })
		// Like Slack, every replies page leads with the parent on top of limit replies.
		write(slackCursorPage(msgs, map[string]any{"ts": parent, "text": "root"}, q.Get("cursor"), limit))
	case "/files.info":
		write(map[string]any{"ok": true, "file": map[string]any{"id": q.Get("file"), "name": "report.pdf", "title": "Report", "mimetype": "application/pdf"}})
	default:
		f.t.Errorf("unexpected endpoint %s", r.URL.Path)
		http.NotFound(w, r)
	}
}

func newSlackTestAdapter(t *testing.T, handler http.Handler) (*slackAdapter, source.Connection) {
	t.Helper()
	server := httptest.NewServer(handler)
	t.Cleanup(server.Close)
	t.Setenv("SLACK_FIXTURE_TOKEN", "test-token")
	resolver, err := auth.NewSecretResolver([]auth.SecretBinding{{RefID: "CONNECTOR_SLACK_TEST", TenantID: "tenant", Purpose: auth.SecretPurposeConnector, Origins: []string{server.URL}, EnvName: "SLACK_FIXTURE_TOKEN"}})
	if err != nil {
		t.Fatal(err)
	}
	conn := source.Connection{TenantID: "tenant", Kind: source.ConnectionSlack, CredentialRef: "CONNECTOR_SLACK_TEST", Config: map[string]any{"channel_id": "C1", "base_url": server.URL}}
	adapter, err := NewSlackAdapter(conn, resolver)
	if err != nil {
		t.Fatal(err)
	}
	a := adapter.(*slackAdapter)
	a.client = server.Client()
	return a, conn
}

func TestSlackContentPagesStayWithinBoundAcrossThreads(t *testing.T) {
	fake := &fakeSlack{t: t,
		history: []map[string]any{
			{"user": "U1", "text": "root", "ts": "1.000001", "thread_ts": "1.000001", "reply_count": 3, "files": []map[string]any{{"id": "F1"}}},
			{"user": "U2", "subtype": "channel_join", "text": "<@U2> has joined the channel", "ts": "2.000000"},
			{"user": "U1", "text": "also sent to channel", "ts": "1.000004", "thread_ts": "1.000001", "subtype": "thread_broadcast"},
			{"user": "U2", "text": "later <@U1> see <#C9|ops>, <https://example.com|docs> &amp; <!here>", "ts": "3.000000"},
		},
		replies: map[string][]map[string]any{"1.000001": {
			{"user": "U2", "text": "first reply", "ts": "1.000002", "thread_ts": "1.000001"},
			{"user": "U1", "text": "second reply", "ts": "1.000003", "thread_ts": "1.000001"},
			{"user": "U1", "text": "also sent to channel", "ts": "1.000004", "thread_ts": "1.000001", "subtype": "thread_broadcast"},
		}},
	}
	a, conn := newSlackTestAdapter(t, fake)
	var items []ContentItem
	var cursor Cursor
	for pages := 0; ; pages++ {
		if pages > 10 {
			t.Fatal("content listing did not terminate")
		}
		page, err := a.Content(context.Background(), conn, cursor, 2)
		if err != nil {
			t.Fatal(err)
		}
		if len(page.Items) > 2 || page.Completeness != CompletenessComplete {
			t.Fatalf("page exceeded bound or was incomplete: %+v", page)
		}
		items = append(items, page.Items...)
		if cursor = page.NextCursor; cursor == "" {
			break
		}
	}
	ids := make([]string, 0, len(items))
	for _, item := range items {
		ids = append(ids, item.NativeID)
	}
	want := "slack:C1:thread:3.000000 slack:C1:thread:1.000001 slack:file:F1"
	if strings.Join(ids, " ") != want {
		t.Fatalf("items = %v, want %s", ids, want)
	}
	latest, root, file := items[0], items[1], items[2]
	if latest.Kind != "thread" || latest.Title != "Bob in #engineering: later @Alice see #ops, docs (https://example.com)" ||
		latest.ParentNativeID != "slack:C1" || latest.ParentKind != "channel" ||
		latest.Container != "#engineering" || strings.Join(latest.Authors, ",") != "Bob" ||
		latest.NativeRevision != "3.000000:0" || latest.URL != "https://workspace.slack.com/archives/C1/p3000000" {
		t.Fatalf("standalone thread item = %+v", latest)
	}
	if len(latest.Blocks) != 1 {
		t.Fatalf("standalone thread blocks = %+v", latest.Blocks)
	}
	block := latest.Blocks[0]
	if block.Kind != source.StructuredMessage || block.Text != "later @Alice see #ops, docs (https://example.com) & @here" ||
		block.Author != "Bob" || block.NativeID != "3.000000" || block.ParentNativeID != "" ||
		block.CreatedAt != "1970-01-01T00:00:03Z" || block.UpdatedAt != "" ||
		block.URL != "https://workspace.slack.com/archives/C1/p3000000" {
		t.Fatalf("standalone block = %+v", block)
	}
	if root.Kind != "thread" || root.NativeID != "slack:C1:thread:1.000001" || root.Title != "Alice in #engineering: root" ||
		root.NativeRevision != "1.000004:3" || root.URL != "https://workspace.slack.com/archives/C1/p1000001" ||
		strings.Join(root.Authors, ",") != "Alice,Bob" {
		t.Fatalf("thread item = %+v", root)
	}
	if len(root.Blocks) != 4 {
		t.Fatalf("thread blocks = %+v", root.Blocks)
	}
	// The broadcast reply appears in channel history and in
	// conversations.replies; the thread document must contain it once.
	broadcasts := 0
	for i, b := range root.Blocks {
		if b.Text == "also sent to channel" {
			broadcasts++
		}
		if b.NativeID != []string{"1.000001", "1.000002", "1.000003", "1.000004"}[i] {
			t.Fatalf("block %d out of time order: %+v", i, b)
		}
		if i == 0 {
			if b.ParentNativeID != "" || b.Author != "Alice" || b.Text != "root" ||
				b.URL != "https://workspace.slack.com/archives/C1/p1000001" {
				t.Fatalf("root block = %+v", b)
			}
		} else if b.ParentNativeID != "1.000001" {
			t.Fatalf("reply block missing thread parent: %+v", b)
		}
	}
	if broadcasts != 1 {
		t.Fatalf("broadcast reply duplicated: %d blocks", broadcasts)
	}
	if reply := root.Blocks[1]; reply.Author != "Bob" || reply.Text != "first reply" ||
		reply.URL != "https://workspace.slack.com/archives/C1/p1000002?cid=C1&thread_ts=1.000001" {
		t.Fatalf("reply block = %+v", reply)
	}
	if file.ParentNativeID != "slack:C1:thread:1.000001" || file.ParentKind != "thread" || file.Kind != "file" {
		t.Fatalf("file item = %+v", file)
	}
	if fake.count("/auth.test") != 1 || fake.count("/conversations.info") != 1 || fake.count("/users.list") != 1 || fake.count("/users.info") != 1 || fake.count("/conversations.replies") != 1 {
		t.Fatalf("lookups were not cached per sync: auth.test=%d conversations.info=%d users.list=%d users.info=%d conversations.replies=%d", fake.count("/auth.test"), fake.count("/conversations.info"), fake.count("/users.list"), fake.count("/users.info"), fake.count("/conversations.replies"))
	}
}

func TestSlackChannelTitleUnavailable(t *testing.T) {
	fake := &fakeSlack{t: t, history: []map[string]any{{"user": "U1", "text": "a", "ts": "1"}, {"user": "U1", "text": "b", "ts": "2"}}}
	fake.override = func(w http.ResponseWriter, r *http.Request) bool {
		if r.URL.Path != "/conversations.info" {
			return false
		}
		_, _ = fmt.Fprint(w, `{"ok":false,"error":"missing_scope"}`)
		return true
	}
	a, conn := newSlackTestAdapter(t, fake)
	page, err := a.Content(context.Background(), conn, "", 10)
	if err != nil || len(page.Items) != 2 || fake.count("/conversations.info") != 1 {
		t.Fatalf("page=%+v conversations.info=%d err=%v", page, fake.count("/conversations.info"), err)
	}
	for _, item := range page.Items {
		if item.Container != "#C1" || len(item.Blocks) != 1 || item.Blocks[0].Text != map[string]string{"slack:C1:thread:1": "a", "slack:C1:thread:2": "b"}[item.NativeID] {
			t.Fatalf("unexpected item without channel title: %+v", item)
		}
	}
}

func TestSlackRateLimitWaitsThenGivesUp(t *testing.T) {
	limitedFake := func(limited int32) *fakeSlack {
		var remaining atomic.Int32
		remaining.Store(limited)
		fake := &fakeSlack{t: t, history: []map[string]any{{"user": "U1", "text": "hello", "ts": "1"}}}
		fake.override = func(w http.ResponseWriter, r *http.Request) bool {
			if remaining.Add(-1) < 0 {
				return false
			}
			w.Header().Set("Retry-After", "0")
			w.WriteHeader(http.StatusTooManyRequests)
			return true
		}
		return fake
	}
	a, conn := newSlackTestAdapter(t, limitedFake(2))
	if page, err := a.Content(context.Background(), conn, "", 10); err != nil || len(page.Items) != 1 {
		t.Fatalf("rate-limited listing did not recover: page=%+v err=%v", page, err)
	}

	fake := limitedFake(1 << 30)
	a, conn = newSlackTestAdapter(t, fake)
	_, err := a.Content(context.Background(), conn, "", 10)
	if err == nil || err.Error() != "slack conversations.history rate limited (Retry-After 0)" || fake.count("/conversations.history") != slackRateLimitRetries+1 {
		t.Fatalf("persistent rate limit: err=%v conversations.history=%d", err, fake.count("/conversations.history"))
	}
}

// TestSlackWithoutChannelSyncsEveryMemberChannel: a source without
// channel_id lists every channel the credential is a member of and skips
// one it has since left instead of failing the sync.
func TestSlackWithoutChannelSyncsEveryMemberChannel(t *testing.T) {
	history := map[string]string{
		"C1": `{"ok":true,"messages":[{"user":"U1","text":"second","ts":"2.000000"},{"user":"U1","text":"first","ts":"1.000000"}],"has_more":false}`,
		"C2": `{"ok":true,"messages":[{"user":"U2","text":"elsewhere","ts":"3.000000"}],"has_more":false}`,
	}
	members := map[string]string{"C1": `["U1"]`, "C2": `["U1","U2"]`}
	a, conn := newSlackTestAdapter(t, http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		q := r.URL.Query()
		switch r.URL.Path {
		case "/users.conversations":
			if q.Get("types") != "public_channel,private_channel" || q.Get("exclude_archived") != "true" {
				t.Errorf("users.conversations query = %v", q)
			}
			if q.Get("cursor") == "" {
				_, _ = fmt.Fprint(w, `{"ok":true,"channels":[{"id":"C3","name":"left"},{"id":"C2","name":"random"}],"response_metadata":{"next_cursor":"p2"}}`)
				return
			}
			_, _ = fmt.Fprint(w, `{"ok":true,"channels":[{"id":"C1","name":"general"}],"response_metadata":{"next_cursor":""}}`)
		case "/conversations.history":
			if body, ok := history[q.Get("channel")]; ok {
				_, _ = fmt.Fprint(w, body)
				return
			}
			_, _ = fmt.Fprint(w, `{"ok":false,"error":"not_in_channel"}`)
		case "/conversations.members":
			if list, ok := members[q.Get("channel")]; ok {
				_, _ = fmt.Fprintf(w, `{"ok":true,"members":%s}`, list)
				return
			}
			_, _ = fmt.Fprint(w, `{"ok":false,"error":"channel_not_found"}`)
		case "/auth.test":
			_, _ = fmt.Fprint(w, `{"ok":true,"url":"https://workspace.slack.com/"}`)
		case "/users.list":
			_, _ = fmt.Fprint(w, `{"ok":true,"members":[{"id":"U1","name":"alice"},{"id":"U2","name":"bob"}]}`)
		default:
			t.Errorf("unexpected endpoint %s", r.URL.Path)
			http.NotFound(w, r)
		}
	}))
	delete(conn.Config, "channel_id")
	ctx := context.Background()

	var items []string
	var cursor Cursor
	for pages := 0; ; pages++ {
		page, err := a.Content(ctx, conn, cursor, 2)
		if err != nil || pages > 10 || len(page.Items) > 2 {
			t.Fatalf("content page %d = %+v, %v", pages, page, err)
		}
		for _, item := range page.Items {
			items = append(items, item.NativeID+"#"+item.Title)
		}
		if cursor = page.NextCursor; cursor == "" {
			break
		}
	}
	if got := strings.Join(items, " "); got != "slack:C1:thread:2.000000#alice in #general: second slack:C1:thread:1.000000#alice in #general: first slack:C2:thread:3.000000#bob in #random: elsewhere" {
		t.Fatalf("content = %s", got)
	}
}

// TestSlackChannelsPickerListsMemberChannelsByName: the wizard's picker gets
// the credential's channels sorted by name with their privacy, and only a
// principal that may manage connectors can list them.
func TestSlackChannelsPickerListsMemberChannelsByName(t *testing.T) {
	server := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		if r.URL.Path != "/users.conversations" || r.Header.Get("Authorization") != "Bearer test-token" {
			t.Errorf("unexpected request %s", r.URL)
		}
		_, _ = fmt.Fprint(w, `{"ok":true,"channels":[{"id":"C2","name":"random","is_private":true},{"id":"C1","name":"general"}]}`)
	}))
	defer server.Close()
	svc := &Service{secrets: fixedCredential("test-token")}
	manager := auth.Principal{TenantID: "tenant", Scopes: map[string]struct{}{"connectors.manage": {}}}
	got, err := svc.SlackChannels(context.Background(), manager, "CONNECTOR_SLACK_TEST", server.URL)
	if err != nil || len(got) != 2 || got[0] != (SlackChannel{ID: "C1", Name: "general"}) || got[1] != (SlackChannel{ID: "C2", Name: "random", Private: true}) {
		t.Fatalf("channels = %+v, %v", got, err)
	}
	if _, err := svc.SlackChannels(context.Background(), auth.Principal{TenantID: "tenant"}, "CONNECTOR_SLACK_TEST", server.URL); !errors.Is(err, source.ErrDenied) {
		t.Fatalf("principal without connectors.manage: %v", err)
	}
}

func TestNewSlackAdapterReleasesTokenOnlyForBoundTenantAndOrigin(t *testing.T) {
	t.Setenv("SLACK_FIXTURE_TOKEN", "test-token")
	t.Setenv("CONNECTOR_SLACK_UNBOUND", "leaked")
	resolver, err := auth.NewSecretResolver([]auth.SecretBinding{{RefID: "CONNECTOR_SLACK_TEST", TenantID: "tenant", Purpose: auth.SecretPurposeConnector, Origins: []string{"https://slack.com"}, EnvName: "SLACK_FIXTURE_TOKEN"}})
	if err != nil {
		t.Fatal(err)
	}
	conn := func(tenant, ref, baseURL string) source.Connection {
		config := map[string]any{"channel_id": "C1"}
		if baseURL != "" {
			config["base_url"] = baseURL
		}
		return source.Connection{TenantID: tenant, Kind: source.ConnectionSlack, CredentialRef: ref, Config: config}
	}
	adapter, err := NewSlackAdapter(conn("tenant", "CONNECTOR_SLACK_TEST", ""), resolver)
	if err != nil || adapter.(*slackAdapter).token != "test-token" || adapter.(*slackAdapter).baseURL != "https://slack.com/api" {
		t.Fatalf("bound credential: adapter=%+v err=%v", adapter, err)
	}
	for name, c := range map[string]source.Connection{
		"other tenant":           conn("other", "CONNECTOR_SLACK_TEST", ""),
		"unbound origin":         conn("tenant", "CONNECTOR_SLACK_TEST", "https://collector.example/api"),
		"ref read as env var":    conn("tenant", "CONNECTOR_SLACK_UNBOUND", ""),
		"missing credential_ref": conn("tenant", "", ""),
	} {
		if _, err := NewSlackAdapter(c, resolver); err == nil {
			t.Errorf("%s: adapter built without a bound credential", name)
		} else if name != "missing credential_ref" && !errors.Is(err, auth.ErrSecretDenied) {
			t.Errorf("%s: err = %v", name, err)
		}
	}
}

func TestSlackPlainText(t *testing.T) {
	names := map[string]string{"U1": "Alice"}
	lookup := func(id string) string {
		if name, ok := names[id]; ok {
			return name
		}
		return id
	}
	for in, want := range map[string]string{
		"hi <@U1>":                              "hi @Alice",
		"hi <@U9|old-name>":                     "hi @old-name",
		"hi <@U9>":                              "hi @U9",
		"in <#C1|general> and <#C2>":            "in #general and #C2",
		"<!here> <!channel> <!subteam^S1|@eng>": "@here @channel @eng",
		"<!date^1392734382^{date}|Feb 18>":      "Feb 18",
		"see <https://a.example/x|the doc>":     "see the doc (https://a.example/x)",
		"see <https://a.example/x>":             "see https://a.example/x",
		"mail <mailto:a@b.example|a@b.example>": "mail a@b.example",
		"a &lt;b&gt; &amp;&amp; c":              "a <b> && c",
	} {
		if got := slackPlainText(in, lookup); got != want {
			t.Errorf("slackPlainText(%q) = %q, want %q", in, got, want)
		}
	}
}

// TestSlackIncrementalSyncs: the first run lists every channel fully and
// checkpoints per-channel watermarks. A run from that checkpoint reads only
// the last few hours of known channels (conversations.history oldest) but
// lists a newly joined channel in full, and a run whose full listing has
// aged past full_sync_interval lists everything again.
func TestSlackIncrementalSyncs(t *testing.T) {
	fake := &fakeSlack{t: t,
		byChannel: map[string][]map[string]any{
			"C1": {{"user": "U1", "text": "first", "ts": "1728000000.000000"}},
		},
		channels: []map[string]any{{"id": "C1", "name": "general"}},
	}
	run := func(conn source.Connection, cursor Cursor, limit int) ContentPage {
		t.Helper()
		a, _ := newSlackTestAdapter(t, fake)
		var page ContentPage
		for pages := 0; ; pages++ {
			var err error
			if page, err = a.Content(context.Background(), conn, cursor, limit); err != nil || pages > 20 {
				t.Fatalf("page %d = %+v, %v", pages, page, err)
			}
			if cursor = page.NextCursor; cursor == "" {
				return page
			}
		}
	}
	_, conn := newSlackTestAdapter(t, fake)
	delete(conn.Config, "channel_id")
	conn.Config["reconcile"] = "true"

	full := run(conn, "", 100)
	if !full.Snapshot || full.Checkpoint == "" || full.NextCursor != "" {
		t.Fatalf("first run must list a full snapshot and checkpoint: %+v", full)
	}
	if fake.lastOldest("C1") != "" {
		t.Fatalf("full run sent oldest = %q", fake.lastOldest("C1"))
	}
	cp, err := decodeSlackContentCursor(full.Checkpoint)
	if err != nil {
		t.Fatal(err)
	}
	if cp.Channel != "" || cp.Full || cp.Oldest["C1"] != "1728000000.000000" || cp.FullAt == "" {
		t.Fatalf("checkpoint = %+v", cp)
	}

	// Within the interval the next run is a delta: oldest bounds known
	// channels and a newly joined channel is still read in full.
	fake.byChannel["C1"] = append(fake.byChannel["C1"], map[string]any{"user": "U2", "text": "second", "ts": "1728003600.000000"})
	fake.byChannel["C2"] = []map[string]any{{"user": "U1", "text": "new channel", "ts": "1728005000.000000"}}
	fake.channels = append(fake.channels, map[string]any{"id": "C2", "name": "random"})
	// Limit 1 forces C1's two messages over two pages, so lastOldest records
	// the second (cursor-carrying) history call's bound.
	delta := run(conn, full.Checkpoint, 1)
	if delta.Snapshot || delta.Checkpoint == "" {
		t.Fatalf("delta run must not snapshot: %+v", delta)
	}
	if fake.lastOldest("C1") != "1727978400.000000" {
		t.Fatalf("delta oldest for C1 = %q, want 1727978400.000000 (watermark - 6h)", fake.lastOldest("C1"))
	}
	if fake.lastOldest("C2") != "" {
		t.Fatalf("new channel should list fully, oldest = %q", fake.lastOldest("C2"))
	}
	deltaCP, err := decodeSlackContentCursor(delta.Checkpoint)
	if err != nil {
		t.Fatal(err)
	}
	if deltaCP.Oldest["C1"] != "1728003600.000000" || deltaCP.Oldest["C2"] != "1728005000.000000" || deltaCP.FullAt != cp.FullAt {
		t.Fatalf("delta checkpoint = %+v", deltaCP)
	}

	// A checkpoint older than full_sync_interval forces the full listing again.
	stale := deltaCP
	stale.FullAt = time.Now().UTC().Add(-25 * time.Hour).Format(time.RFC3339)
	fullAgain := run(conn, encodeSlackContentCursor(stale), 100)
	if !fullAgain.Snapshot || fake.lastOldest("C1") != "" || fake.lastOldest("C2") != "" {
		t.Fatalf("stale FullAt must relist fully: page=%+v oldest C1=%q C2=%q", fullAgain, fake.lastOldest("C1"), fake.lastOldest("C2"))
	}
}

// TestSlackLegacyCursorRunsFull: cursors stored by earlier releases (raw
// Slack cursors without the content: prefix) decode empty and start a full
// listing rather than failing the sync.
func TestSlackLegacyCursorRunsFull(t *testing.T) {
	fake := &fakeSlack{t: t, history: []map[string]any{{"user": "U1", "text": "hello", "ts": "1728000000.000000"}}}
	a, conn := newSlackTestAdapter(t, fake)
	conn.Config["reconcile"] = "true"
	page, err := a.Content(context.Background(), conn, Cursor("raw-legacy-cursor"), 10)
	if err != nil || !page.Snapshot || len(page.Items) != 1 || page.Checkpoint == "" {
		t.Fatalf("legacy cursor page = %+v, %v", page, err)
	}
}

// slackThreadFixture returns a fake workspace with one thread (root plus the
// given replies) and one standalone message.
func slackThreadFixture(t *testing.T, replies []map[string]any) *fakeSlack {
	t.Helper()
	return &fakeSlack{t: t,
		history: []map[string]any{
			{"user": "U1", "text": "root message", "ts": "1.000000", "thread_ts": "1.000000", "reply_count": len(replies)},
			{"user": "U2", "text": "standalone", "ts": "2.000000"},
		},
		replies: map[string][]map[string]any{"1.000000": replies},
	}
}

func slackContentItems(t *testing.T, fake *fakeSlack) []ContentItem {
	t.Helper()
	a, conn := newSlackTestAdapter(t, fake)
	var items []ContentItem
	var cursor Cursor
	for pages := 0; ; pages++ {
		if pages > 10 {
			t.Fatal("content listing did not terminate")
		}
		page, err := a.Content(context.Background(), conn, cursor, 10)
		if err != nil {
			t.Fatal(err)
		}
		items = append(items, page.Items...)
		if cursor = page.NextCursor; cursor == "" {
			return items
		}
	}
}

func threadItemByID(t *testing.T, items []ContentItem, nativeID string) ContentItem {
	t.Helper()
	for _, item := range items {
		if item.NativeID == nativeID {
			return item
		}
	}
	t.Fatalf("item %s not found in %+v", nativeID, items)
	return ContentItem{}
}

// TestSlackThreadRepliesPaginate: a thread larger than one replies page is
// fetched to the end and lands in a single document.
func TestSlackThreadRepliesPaginate(t *testing.T) {
	replies := make([]map[string]any, 0, 205)
	for i := 1; i <= 205; i++ {
		replies = append(replies, map[string]any{"user": "U1", "text": fmt.Sprintf("reply %03d", i), "ts": fmt.Sprintf("1.%06d", i), "thread_ts": "1.000000"})
	}
	fake := slackThreadFixture(t, replies)
	items := slackContentItems(t, fake)
	thread := threadItemByID(t, items, "slack:C1:thread:1.000000")
	if len(thread.Blocks) != 206 || thread.NativeRevision != "1.000205:205" {
		t.Fatalf("thread = %d blocks revision %q", len(thread.Blocks), thread.NativeRevision)
	}
	if thread.Blocks[1].NativeID != "1.000001" || thread.Blocks[205].NativeID != "1.000205" {
		t.Fatalf("block order = %q .. %q", thread.Blocks[1].NativeID, thread.Blocks[205].NativeID)
	}
	if got := fake.count("/conversations.replies"); got != 2 {
		t.Fatalf("conversations.replies calls = %d, want 2 pages", got)
	}
}

// TestSlackStandaloneMessageIsThreadOfOne: a message with no replies is a
// thread document of one block under the same id scheme, and its files parent
// to that document.
func TestSlackStandaloneMessageIsThreadOfOne(t *testing.T) {
	fake := &fakeSlack{t: t, history: []map[string]any{
		{"user": "U1", "text": "hello <@U2>", "ts": "5.000000", "files": []map[string]any{{"id": "F9"}}},
	}}
	items := slackContentItems(t, fake)
	if len(items) != 2 {
		t.Fatalf("items = %+v", items)
	}
	thread := items[0]
	if thread.NativeID != "slack:C1:thread:5.000000" || thread.Kind != "thread" || thread.NativeRevision != "5.000000:0" ||
		thread.Title != "Alice in #engineering: hello @Bob" || thread.URL != "https://workspace.slack.com/archives/C1/p5000000" ||
		len(thread.Blocks) != 1 {
		t.Fatalf("standalone item = %+v", thread)
	}
	if thread.Blocks[0].Text != "hello @Bob" || thread.Blocks[0].ParentNativeID != "" {
		t.Fatalf("standalone block = %+v", thread.Blocks[0])
	}
	file := items[1]
	if file.NativeID != "slack:file:F9" || file.ParentNativeID != "slack:C1:thread:5.000000" || file.ParentKind != "thread" {
		t.Fatalf("file item = %+v", file)
	}
}

// TestSlackEditedMessageChangesRevision: an edit moves the thread revision
// and updated_at even though no reply was added.
func TestSlackEditedMessageChangesRevision(t *testing.T) {
	fake := slackThreadFixture(t, []map[string]any{
		{"user": "U2", "text": "first", "ts": "1.000002", "thread_ts": "1.000000"},
		{"user": "U1", "text": "second", "ts": "1.000003", "thread_ts": "1.000000"},
	})
	before := threadItemByID(t, slackContentItems(t, fake), "slack:C1:thread:1.000000")
	if before.NativeRevision != "1.000003:2" {
		t.Fatalf("revision before edit = %q", before.NativeRevision)
	}
	fake.replies["1.000000"][1]["edited"] = map[string]any{"user": "U1", "ts": "5.000000"}
	fake.replies["1.000000"][1]["text"] = "second, edited"
	after := threadItemByID(t, slackContentItems(t, fake), "slack:C1:thread:1.000000")
	if after.NativeRevision != "5.000000:2" {
		t.Fatalf("revision after edit = %q, want edit ts to lead", after.NativeRevision)
	}
	if after.UpdatedAt == nil || after.UpdatedAt.Unix() != 5 {
		t.Fatalf("updated_at = %v", after.UpdatedAt)
	}
	edited := after.Blocks[2]
	if edited.NativeID != "1.000003" || edited.Text != "second, edited" || edited.UpdatedAt != "1970-01-01T00:00:05Z" {
		t.Fatalf("edited block = %+v", edited)
	}
}

// TestSlackDeletedReplyChangesThread: a reply that disappears upstream leaves
// the thread document on the next listing.
func TestSlackDeletedReplyChangesThread(t *testing.T) {
	fake := slackThreadFixture(t, []map[string]any{
		{"user": "U2", "text": "kept", "ts": "1.000002", "thread_ts": "1.000000"},
		{"user": "U1", "text": "deleted later", "ts": "1.000003", "thread_ts": "1.000000"},
	})
	before := threadItemByID(t, slackContentItems(t, fake), "slack:C1:thread:1.000000")
	if len(before.Blocks) != 3 || before.NativeRevision != "1.000003:2" {
		t.Fatalf("before = %d blocks %q", len(before.Blocks), before.NativeRevision)
	}
	fake.replies["1.000000"] = fake.replies["1.000000"][:1]
	after := threadItemByID(t, slackContentItems(t, fake), "slack:C1:thread:1.000000")
	if len(after.Blocks) != 2 || after.NativeRevision != "1.000002:1" {
		t.Fatalf("after = %d blocks %q", len(after.Blocks), after.NativeRevision)
	}
	if after.Blocks[1].Text != "kept" {
		t.Fatalf("remaining block = %+v", after.Blocks[1])
	}
}

// TestSlackUnchangedThreadKeepsRevision: listing the same thread twice yields
// the same revision and byte-identical blocks, so the sync dedupes it.
func TestSlackUnchangedThreadKeepsRevision(t *testing.T) {
	fake := slackThreadFixture(t, []map[string]any{
		{"user": "U2", "text": "a reply", "ts": "1.000002", "thread_ts": "1.000000"},
	})
	first := threadItemByID(t, slackContentItems(t, fake), "slack:C1:thread:1.000000")
	second := threadItemByID(t, slackContentItems(t, fake), "slack:C1:thread:1.000000")
	if first.NativeRevision != second.NativeRevision {
		t.Fatalf("revisions differ: %q vs %q", first.NativeRevision, second.NativeRevision)
	}
	firstBody, err := source.EncodeBlocks(first.Blocks)
	if err != nil {
		t.Fatal(err)
	}
	secondBody, err := source.EncodeBlocks(second.Blocks)
	if err != nil {
		t.Fatal(err)
	}
	if !bytes.Equal(firstBody, secondBody) {
		t.Fatal("unchanged thread encoded different blocks")
	}
}

// TestSlackTombstonedRootKeepsReplies: a deleted root message still heads a
// thread document built from its replies; a tombstone with no replies emits
// nothing.
func TestSlackTombstonedRootKeepsReplies(t *testing.T) {
	fake := &fakeSlack{t: t,
		history: []map[string]any{
			{"subtype": "tombstone", "text": "<@U1> deleted a message", "ts": "1.000000", "thread_ts": "1.000000", "reply_count": 2},
			{"subtype": "tombstone", "text": "<@U1> deleted a message", "ts": "3.000000", "thread_ts": "3.000000"},
		},
		replies: map[string][]map[string]any{"1.000000": {
			{"user": "U2", "text": "still here", "ts": "1.000001", "thread_ts": "1.000000"},
			{"user": "U1", "text": "and this", "ts": "1.000002", "thread_ts": "1.000000"},
		}},
	}
	items := slackContentItems(t, fake)
	if len(items) != 1 {
		t.Fatalf("items = %+v", items)
	}
	thread := items[0]
	if thread.NativeID != "slack:C1:thread:1.000000" || thread.Kind != "thread" || thread.NativeRevision != "1.000002:2" {
		t.Fatalf("thread item = %+v", thread)
	}
	if len(thread.Blocks) != 2 {
		t.Fatalf("blocks = %+v", thread.Blocks)
	}
	for i, want := range []struct{ ts, author, text string }{
		{"1.000001", "Bob", "still here"},
		{"1.000002", "Alice", "and this"},
	} {
		block := thread.Blocks[i]
		if block.NativeID != want.ts || block.ParentNativeID != "1.000000" || block.Author != want.author || block.Text != want.text {
			t.Fatalf("block %d = %+v", i, block)
		}
	}
	// The document links to the root permalink and is titled from the first
	// surviving message.
	if thread.URL != "https://workspace.slack.com/archives/C1/p1000000" || thread.Title != "Bob in #engineering: still here" {
		t.Fatalf("thread link/title = %q %q", thread.URL, thread.Title)
	}
	if strings.Join(thread.Authors, ",") != "Bob,Alice" {
		t.Fatalf("authors = %v", thread.Authors)
	}
}

// seedLegacySlackMessages runs one sync whose adapter still emits the
// pre-upgrade per-message items, leaving their objects and versions behind.
func seedLegacySlackMessages(t *testing.T) *connectorFixture {
	t.Helper()
	adapter := &testAdapter{capabilities: AdapterCapabilities{Content: true}}
	adapter.content = func(context.Context, source.Connection, Cursor, int) (ContentPage, error) {
		return ContentPage{Items: []ContentItem{
			{NativeID: "slack:C1:1.000000", Kind: "message", ParentNativeID: "slack:C1", ParentKind: "channel", Text: "root message"},
			{NativeID: "slack:C1:1.000002", Kind: "message", ParentNativeID: "slack:C1:1.000000", ParentKind: "message", Text: "a reply"},
			{NativeID: "slack:C1:2.000000", Kind: "message", ParentNativeID: "slack:C1", ParentKind: "channel", Text: "standalone"},
		}, Completeness: CompletenessComplete, Snapshot: true}, nil
	}
	f := newConnectorFixture(t, adapter)
	f.schedule(t)
	if _, err := f.runNext(t); err != nil {
		t.Fatal(err)
	}
	return f
}

// useRealSlackAdapter points the fixture's Slack factory at the fake Slack
// server and marks the source for reconciled full listings.
func useRealSlackAdapter(t *testing.T, f *connectorFixture, fake *fakeSlack) {
	t.Helper()
	useSlackAdapterWithConfig(t, f, fake, map[string]any{"reconcile": "true"})
}

func useSlackAdapterWithConfig(t *testing.T, f *connectorFixture, fake *fakeSlack, extra map[string]any) {
	t.Helper()
	server := httptest.NewServer(fake)
	t.Cleanup(server.Close)
	f.svc.setAllowPrivateBaseURL(true)
	f.svc.SetSyncDependencies(AdapterFactories{SourceKindSlack: func(c source.Connection) (Adapter, error) {
		return NewSlackAdapter(c, fixedCredential("test-token"))
	}}, f.observations, f.artifacts, f.sourceRepo)
	ctx := context.Background()
	conn, err := f.sourceRepo.GetConnection(ctx, f.tenant, f.connection.ID)
	if err != nil {
		t.Fatal(err)
	}
	conn.Config = map[string]any{"channel_id": "C1", "base_url": server.URL}
	for k, v := range extra {
		conn.Config[k] = v
	}
	if err := f.sourceRepo.UpdateConnection(ctx, conn); err != nil {
		t.Fatal(err)
	}
}

func legacyMessageDeleted(t *testing.T, f *connectorFixture, nativeID string) bool {
	t.Helper()
	ctx := context.Background()
	object, err := f.sourceRepo.ObjectForNativeID(ctx, f.tenant, f.connection.ID, "message", nativeID)
	if err != nil {
		t.Fatalf("legacy object %s: %v", nativeID, err)
	}
	stored, err := f.sourceRepo.GetObject(ctx, f.tenant, object.ID)
	if err != nil {
		t.Fatal(err)
	}
	return stored.DeletedAt != nil
}

// TestSlackCompleteSyncTombstonesPerMessageItems: the first complete
// reconciled listing after the upgrade observes only thread documents, so
// the sweep tombstones the old per-message items.
func TestSlackCompleteSyncTombstonesPerMessageItems(t *testing.T) {
	f := seedLegacySlackMessages(t)
	fake := slackThreadFixture(t, []map[string]any{
		{"user": "U2", "text": "a reply", "ts": "1.000002", "thread_ts": "1.000000"},
	})
	// The seeded messages used ts 1.000000/1.000002/2.000000; align the fake
	// thread root with the seeded standalone message's ts is unnecessary —
	// what matters is that no slack:C1:<ts> item is emitted any more.
	useRealSlackAdapter(t, f, fake)
	f.schedule(t)
	if _, err := f.runNext(t); err != nil {
		t.Fatal(err)
	}
	for _, nativeID := range []string{"slack:C1:1.000000", "slack:C1:1.000002", "slack:C1:2.000000"} {
		if !legacyMessageDeleted(t, f, nativeID) {
			t.Fatalf("old per-message item %s survived the complete sync", nativeID)
		}
	}
	ctx := context.Background()
	for _, nativeID := range []string{"slack:C1:thread:1.000000", "slack:C1:thread:2.000000"} {
		if _, err := f.sourceRepo.ObjectForNativeID(ctx, f.tenant, f.connection.ID, "thread", nativeID); err != nil {
			t.Fatalf("thread document %s missing after sync: %v", nativeID, err)
		}
	}
}

// TestSlackIncompleteSyncKeepsPerMessageItems: a listing that fails before
// its terminal page deletes nothing, even under reconcile.
func TestSlackIncompleteSyncKeepsPerMessageItems(t *testing.T) {
	f := seedLegacySlackMessages(t)
	fake := &fakeSlack{t: t}
	fake.override = func(w http.ResponseWriter, r *http.Request) bool {
		if r.URL.Path != "/conversations.history" {
			return false
		}
		if r.URL.Query().Get("cursor") == "" {
			_, _ = fmt.Fprint(w, `{"ok":true,"messages":[{"user":"U1","text":"root","ts":"1.000000","thread_ts":"1.000000","reply_count":1}],"has_more":true,"response_metadata":{"next_cursor":"p2"}}`)
			return true
		}
		_, _ = fmt.Fprint(w, `{"ok":false,"error":"internal_error"}`)
		return true
	}
	useRealSlackAdapter(t, f, fake)
	f.schedule(t)
	if _, err := f.runNext(t); err == nil {
		t.Fatal("interrupted listing unexpectedly succeeded")
	}
	// The adapter fails within its first page, so nothing was observed and
	// nothing may be deleted.
	for _, nativeID := range []string{"slack:C1:1.000000", "slack:C1:1.000002", "slack:C1:2.000000"} {
		if legacyMessageDeleted(t, f, nativeID) {
			t.Fatalf("incomplete sync deleted %s", nativeID)
		}
	}
}

// TestSlackDeltaSyncRetiresSupersededMessages: without reconcile no listing
// is a snapshot, yet a thread document still retires the per-message items
// it replaces (the upgrade path for sources that never reconcile).
func TestSlackDeltaSyncRetiresSupersededMessages(t *testing.T) {
	f := seedLegacySlackMessages(t)
	fake := slackThreadFixture(t, []map[string]any{
		{"user": "U2", "text": "a reply", "ts": "1.000002", "thread_ts": "1.000000"},
	})
	useSlackAdapterWithConfig(t, f, fake, nil)
	f.schedule(t)
	if _, err := f.runNext(t); err != nil {
		t.Fatal(err)
	}
	for _, nativeID := range []string{"slack:C1:1.000000", "slack:C1:1.000002", "slack:C1:2.000000"} {
		if !legacyMessageDeleted(t, f, nativeID) {
			t.Fatalf("old per-message item %s survived a non-reconciled sync", nativeID)
		}
	}
}
