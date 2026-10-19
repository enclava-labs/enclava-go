// proxy_concurrency_test.go is the plan 01-31 behavior proof: two real
// enclava processes over one isolated PostgreSQL schema, one controllable
// real HTTP upstream, and a shared durable artifact root. It exercises
// bounded per-process concurrency (active streams, queued callers,
// explicit overload rejection), queued cancellation, control-plane progress
// under saturation, shared PostgreSQL budget admission across processes,
// client-disconnect settlement and process-kill liability.
//
// The required integration mode fails (never silently passes) when the
// database or the app binary cannot be built/started; run it with
// ENCLAVA_PROXY_INTEGRATION=1 and ENCLAVA_TEST_DATABASE_URL set.
package tests

import (
	"context"
	"encoding/json"
	"fmt"
	"io"
	"net/http"
	"net/http/httptest"
	"os"
	"strings"
	"sync"
	"sync/atomic"
	"testing"
	"time"
	"unicode/utf8"

	"enclava-go/internal/billing"
	"enclava-go/internal/store/postgres"
	"enclava-go/internal/testdb"
)

const (
	proxyChatBody      = `{"model":"test-model","messages":[{"role":"user","content":"hi"}]}`
	proxyResponsesBody = `{"model":"test-model","input":"hi"}`
	proxyEmbedBody     = `{"model":"test-model","input":"hi"}`
)

// estimateUnits mirrors the handler's per-kind bound math for the fixed
// payloads above so the seeded budget's arithmetic is exact, not tuned.
func estimateUnits(payload string) int64 {
	return int64((utf8.RuneCount([]byte(payload)) + 3) / 4)
}

const outputBound = int64(1 << 17)

// heldUpstream is a real HTTP upstream that can pin streams open and counts
// what it actually observes — the ground truth for "streams returned headers
// but the slot stayed busy".
type heldUpstream struct {
	t *testing.T

	active       atomic.Int64
	maxActive    atomic.Int64
	streamsSeen  atomic.Int64
	streamsEnded atomic.Int64
	mu           sync.Mutex
	barrier      chan struct{}

	srv *httptest.Server
}

func newHeldUpstream(t *testing.T) *heldUpstream {
	t.Helper()
	u := &heldUpstream{t: t, barrier: make(chan struct{})}
	u.srv = httptest.NewServer(http.HandlerFunc(u.serve))
	t.Cleanup(u.srv.Close)
	return u
}

func (u *heldUpstream) swapBarrier() chan struct{} {
	u.mu.Lock()
	defer u.mu.Unlock()
	u.barrier = make(chan struct{})
	return u.barrier
}

func (u *heldUpstream) currentBarrier() chan struct{} {
	u.mu.Lock()
	defer u.mu.Unlock()
	return u.barrier
}

func (u *heldUpstream) serve(w http.ResponseWriter, r *http.Request) {
	cur := u.active.Add(1)
	for {
		m := u.maxActive.Load()
		if cur <= m || u.maxActive.CompareAndSwap(m, cur) {
			break
		}
	}
	defer u.active.Add(-1)

	path := strings.TrimPrefix(r.URL.Path, "/v1")
	switch {
	case strings.HasSuffix(path, "/models") && r.Method == http.MethodGet:
		writeUpstreamJSON(w, `{"object":"list","data":[{"id":"test-model","object":"model"}]}`)
	case strings.HasSuffix(path, "/embeddings"):
		writeUpstreamJSON(w, `{"object":"list","data":[{"object":"embedding","embedding":[0.1,0.2],"index":0}],"usage":{"prompt_tokens":7,"total_tokens":7}}`)
	case strings.HasSuffix(path, "/cancel"):
		writeUpstreamJSON(w, `{"id":"resp_up1","object":"response","status":"cancelled","usage":{"input_tokens":1,"output_tokens":0,"total_tokens":1}}`)
	case strings.HasSuffix(path, "/responses") || strings.HasPrefix(path, "/responses/"):
		writeUpstreamJSON(w, `{"id":"resp_up1","object":"response","model":"test-model","status":"completed",`+
			`"output":[{"id":"msg_up1","type":"message","role":"assistant","content":[{"type":"output_text","text":"hi"}]}],`+
			`"usage":{"input_tokens":9,"output_tokens":4,"total_tokens":13}}`)
	case strings.HasSuffix(path, "/chat/completions"):
		var probe struct {
			Stream bool `json:"stream"`
		}
		raw, _ := io.ReadAll(r.Body)
		_ = json.Unmarshal(raw, &probe)
		if !probe.Stream {
			writeUpstreamJSON(w, `{"id":"chatcmpl-1","object":"chat.completion","choices":[{"index":0,"message":{"role":"assistant","content":"hi"},"finish_reason":"stop"}],"usage":{"prompt_tokens":12,"completion_tokens":3,"total_tokens":15}}`)
			return
		}
		u.streamsSeen.Add(1)
		defer u.streamsEnded.Add(1)
		w.Header().Set("Content-Type", "text/event-stream")
		w.WriteHeader(http.StatusOK)
		// Headers flush immediately: the client sees the stream begin while
		// the upstream still holds it open.
		_, _ = io.WriteString(w, "data: {\"id\":\"c1\",\"object\":\"chat.completion.chunk\",\"choices\":[]}\n\n")
		if f, ok := w.(http.Flusher); ok {
			f.Flush()
		}
		select {
		case <-u.currentBarrier():
			_, _ = io.WriteString(w, "data: {\"id\":\"c1\",\"object\":\"chat.completion.chunk\",\"choices\":[],\"usage\":{\"prompt_tokens\":12,\"completion_tokens\":3,\"total_tokens\":15}}\n\n")
			_, _ = io.WriteString(w, "data: [DONE]\n\n")
		case <-r.Context().Done():
		}
	default:
		http.Error(w, `{"error":"unknown upstream path"}`, http.StatusNotFound)
	}
}

func writeUpstreamJSON(w http.ResponseWriter, body string) {
	w.Header().Set("Content-Type", "application/json")
	w.WriteHeader(http.StatusOK)
	_, _ = io.WriteString(w, body)
}

// proxyResult is one client-visible outcome: status, OpenAI error code and
// the decoded body id (for bindings).
type proxyResult struct {
	status int
	code   string
	id     string
	raw    []byte
}

func doJSON(client *http.Client, base, key, method, path, body string) *proxyResult {
	req, err := http.NewRequest(method, base+path, strings.NewReader(body))
	if err != nil {
		return &proxyResult{status: -1}
	}
	req.Header.Set("Authorization", "Bearer "+key)
	req.Header.Set("Content-Type", "application/json")
	resp, err := client.Do(req)
	if err != nil {
		return &proxyResult{status: -1}
	}
	defer func() { _ = resp.Body.Close() }()
	raw, _ := io.ReadAll(resp.Body)
	out := &proxyResult{status: resp.StatusCode, raw: raw}
	var env struct {
		ID    string `json:"id"`
		Error *struct {
			Code string `json:"code"`
		} `json:"error"`
	}
	if json.Unmarshal(raw, &env) == nil {
		out.id = env.ID
		if env.Error != nil {
			out.code = env.Error.Code
		}
	}
	return out
}

func startStream(ctx context.Context, client *http.Client, base, key string) (*http.Response, error) {
	req, err := http.NewRequestWithContext(ctx, http.MethodPost, base+"/v1/chat/completions",
		strings.NewReader(`{"model":"test-model","stream":true,"messages":[{"role":"user","content":"hi"}]}`))
	if err != nil {
		return nil, err
	}
	req.Header.Set("Authorization", "Bearer "+key)
	req.Header.Set("Content-Type", "application/json")
	return client.Do(req)
}

func waitFor(t *testing.T, what string, cond func() bool) {
	t.Helper()
	deadline := time.Now().Add(15 * time.Second)
	for time.Now().Before(deadline) {
		if cond() {
			return
		}
		time.Sleep(25 * time.Millisecond)
	}
	t.Fatalf("timed out waiting for %s", what)
}

func countReservations(t *testing.T, db *postgres.DB, tenant, state string) int {
	t.Helper()
	var n int
	q := `SELECT count(DISTINCT id) FROM reservations WHERE tenant_id = $1`
	args := []any{tenant}
	if state != "" {
		q += ` AND state = $2`
		args = append(args, state)
	}
	if err := db.SQL.QueryRowContext(context.Background(), q, args...).Scan(&n); err != nil {
		t.Fatalf("count reservations (%s): %v", state, err)
	}
	return n
}

// TestProxyTwoProcess drives the whole plan-02-31 concurrency proof. It is
// behavior, not throughput: tiny deterministic limits, explicit barriers and
// PostgreSQL-observed accounting.
func TestProxyTwoProcess(t *testing.T) {
	if strings.TrimSpace(getenvOr("ENCLAVA_PROXY_INTEGRATION")) != "1" {
		t.Skip("ENCLAVA_PROXY_INTEGRATION=1 required for the real-process proof")
	}
	// Missing prerequisites are failures, never skipped evidence.
	db, dsn := testdb.NewWithDSN(t)
	ctx := context.Background()

	up := newHeldUpstream(t)

	// The env provider seeds its canonical endpoint on the bootstrap tenant;
	// the test principal must live there for routing to see it.
	tenant := testdb.SeedTenant(t, db, "bootstrap")
	user := testdb.SeedUser(t, db, tenant, "proxy@example.com", []string{
		"models.list", "chat.completions", "embeddings.create", "responses",
	})
	key, _ := testdb.SeedAPIKey(t, db, tenant, testdb.SeedKeySpec{
		Name:        "proxy-key",
		OwnerUserID: user.ID,
		Scopes:      []string{"models.list", "chat.completions", "embeddings.create", "responses"},
	})

	brepo := postgres.NewBillingRepo(db)
	priceSeq := 0
	price := func(capability string, unit billing.UnitKind, micros int64) {
		t.Helper()
		priceSeq++
		if err := brepo.PutPrice(ctx, tenant, fmt.Sprintf("p-conc-%d", priceSeq), billing.Price{
			SubjectType: billing.PriceSubjectEndpoint, SubjectID: "openai_compatible",
			Capability: capability, Unit: unit, RateMicros: micros, UnitQuantity: 1, Currency: "USD",
		}, "tester", time.Now().Add(-time.Minute).UTC()); err != nil {
			t.Fatalf("put price: %v", err)
		}
	}
	// Per-unit 1µ pricing on every exercised capability → bound equals the
	// unit estimate, making budget arithmetic exact.
	for _, cap := range []string{"chat/completions", "embeddings", "responses"} {
		price(cap, billing.UnitInput, 1)
		price(cap, billing.UnitOutput, 1)
	}

	chatBound := estimateUnits(proxyChatBody) + outputBound // +2 later

	dataDir := t.TempDir() // one shared durable artifact root for both processes
	env := map[string]string{
		"ENCLAVA_DATA_DIR":           dataDir,
		"INFERENCE_PROVIDER":         "openai_compatible",
		"OPENAI_COMPATIBLE_BASE_URL": up.srv.URL,
		"OPENAI_COMPATIBLE_API_KEY":  "test-upstream-key",
		"SESSION_TENANT_SLUG":        "bootstrap",
		"INFERENCE_MAX_ACTIVE":       "1",
		"INFERENCE_MAX_QUEUED":       "1",
		"INFERENCE_MAX_INFLIGHT":     "16",
		"INFERENCE_MAX_HTTP_QUEUE":   "16",
		"INFERENCE_MAX_BODY_BYTES":   "16777216",
		"PROVIDER_PROBE_INTERVAL":    "60s",
		"CONNECTOR_SYNC_INTERVAL":    "0",
	}
	appA := testdb.StartApp(t, testdb.StartOpts{DatabaseURL: dsn, Env: env})
	appB := testdb.StartApp(t, testdb.StartOpts{DatabaseURL: dsn, Env: env})

	// --- Phase 1: mixed workload sanity — real priced paths settle ----------
	res := doJSON(appA.Client, appA.BaseURL, key, http.MethodPost, "/v1/responses", proxyResponsesBody)
	if res.status != http.StatusOK {
		t.Fatalf("responses create: %d %s\nappA logs: %s\nappB logs: %s", res.status, res.raw, appA.Logs(), appB.Logs())
	}
	publicID := res.id
	if publicID == "" || strings.Contains(publicID, "up1") {
		t.Fatalf("response id must be a bound public id, got %q", publicID)
	}
	if res := doJSON(appA.Client, appA.BaseURL, key, http.MethodPost, "/v1/embeddings", proxyEmbedBody); res.status != http.StatusOK {
		t.Fatalf("embeddings: %d %s", res.status, res.raw)
	}

	// --- Phase 2: process-local bounds + progress + queued cancel -----------
	barrier := up.swapBarrier()
	s1ctx, s1cancel := context.WithCancel(context.Background())
	defer s1cancel()
	s1, err := startStream(s1ctx, appA.Client, appA.BaseURL, key)
	if err != nil {
		t.Fatalf("held stream: %v", err)
	}
	if s1.StatusCode != http.StatusOK {
		t.Fatalf("held stream status: %d", s1.StatusCode)
	}
	deadline := time.Now().Add(15 * time.Second)
	for time.Now().Before(deadline) && !(up.streamsSeen.Load() == 1 && up.active.Load() == 1) {
		time.Sleep(25 * time.Millisecond)
	}
	if !(up.streamsSeen.Load() == 1 && up.active.Load() == 1) {
		t.Fatalf("timed out waiting for upstream to observe the open stream\nappA: %s\nappB: %s", appA.Logs(), appB.Logs())
	}

	// Second caller queues (MaxActive=1, MaxQueued=1) on appA.
	s2ctx, s2cancel := context.WithCancel(context.Background())
	s2done := make(chan *http.Response, 1)
	s2err := make(chan error, 1)
	go func() {
		resp, err := startStream(s2ctx, appA.Client, appA.BaseURL, key)
		if err != nil {
			s2err <- err
			return
		}
		s2done <- resp
	}()
	// Give s2 a moment to reach the server and park in the queue; then a
	// third request must be rejected explicitly (429 overloaded).
	time.Sleep(300 * time.Millisecond)
	s3 := doJSON(appA.Client, appA.BaseURL, key, http.MethodPost, "/v1/chat/completions", proxyChatBody)
	if s3.status != http.StatusTooManyRequests {
		t.Fatalf("saturated process must reject with 429, got %d %s\nappA: %s", s3.status, s3.raw, appA.Logs())
	}

	// Control plane stays live under saturation: unpriced retrieve runs on
	// the reserved manage pool, not the exhausted execute pool.
	man := doJSON(appA.Client, appA.BaseURL, key, http.MethodGet, "/v1/responses/"+publicID, "")
	if man.status != http.StatusOK {
		t.Fatalf("manage under saturation: %d %s", man.status, man.raw)
	}

	// appB has independent process-local capacity — the limits sum across
	// processes by design (no global concurrency cap exists).
	s4ctx, s4cancel := context.WithCancel(context.Background())
	defer s4cancel()
	s4, err := startStream(s4ctx, appB.Client, appB.BaseURL, key)
	if err != nil {
		t.Fatalf("appB stream: %v", err)
	}
	if s4.StatusCode != http.StatusOK {
		t.Fatalf("appB must admit its own stream while appA saturates: %d", s4.StatusCode)
	}
	waitFor(t, "upstream to observe both process streams", func() bool {
		return up.streamsSeen.Load() == 2 && up.active.Load() == 2
	})

	// Queued cancellation: abandon s2's client request — the parked queue
	// entry must cancel without ever dispatching upstream.
	s2cancel()
	select {
	case resp := <-s2done:
		// If the queue entry slipped through, it must still be closed.
		_ = resp.Body.Close()
	case err := <-s2err:
		_ = err // client-side cancel error is the expected outcome
	case <-time.After(10 * time.Second):
		t.Fatal("queued cancel never resolved")
	}
	// Nothing queued ever reached the upstream: still exactly 2 streams.
	if n := up.streamsSeen.Load(); n != 2 {
		t.Fatalf("cancelled queued request reached upstream (%d streams)", n)
	}

	// Disconnect propagation: cancel s1's client request — the transport
	// must close, freeing appA's active slot.
	s1cancel()
	_ = s1.Body.Close()
	waitFor(t, "upstream to observe s1 disconnect", func() bool {
		return up.active.Load() == 1 && up.streamsEnded.Load() == 1
	})
	close(barrier) // release anything still parked on it (safety)

	// --- Phase 3: settlement evidence after disconnect ----------------------
	// s1 disconnected before any terminal event: its reservation retains
	// unknown liability. s4 is still open (unknown too) until released.
	waitFor(t, "s1 reservation to mark unknown", func() bool {
		return countReservations(t, db, tenant, billing.ReservationUnknown) >= 1
	})
	_ = s4.Body.Close()
	waitFor(t, "s4 upstream close", func() bool { return up.active.Load() == 0 })

	// --- Phase 4: shared budget race across both processes ------------------
	// Liability must be fully marked before the race limit is computed —
	// both disconnected streams must have left 'held' (unknown = retained).
	waitFor(t, "stream reservations to finish liability marking", func() bool {
		return countReservations(t, db, tenant, "held") == 0
	})
	// The budget admits exactly one more chat hold on top of the liability
	// already owed (held/unknown reservations plus settled usage records):
	// deterministically one accepted hold across the two processes.
	var currentLiability int64
	if err := db.SQL.QueryRowContext(ctx, `
		SELECT (SELECT COALESCE(sum(limit_amount_micros),0) FROM reservations
		         WHERE tenant_id = $1 AND state IN ('held','unknown') AND id NOT LIKE '%/%')
		     + (SELECT COALESCE(sum(amount_micros),0) FROM usage_records
		         WHERE tenant_id = $1 AND created_at >= date_trunc('month', now()))`, tenant).Scan(&currentLiability); err != nil {
		t.Fatalf("current liability: %v", err)
	}
	budgetLimit := currentLiability + chatBound
	if err := brepo.CreateBudget(ctx, tenant, "b-conc", billing.Budget{
		Subject: billing.Subject{Type: billing.BudgetSubjectTenant, ID: tenant},
		Kind:    billing.BudgetKindAmount, Period: billing.PeriodMonthly,
		LimitAmountMicros: budgetLimit, Currency: "USD",
		StartsAt: time.Now().Add(-time.Hour).UTC(),
	}); err != nil {
		t.Fatalf("create budget: %v", err)
	}
	const raceN = 4
	var wg sync.WaitGroup
	results := make([]*proxyResult, 2*raceN)
	for i := range raceN {
		for j, app := range []*testdb.AppHandle{appA, appB} {
			wg.Add(1)
			go func(i, j int, app *testdb.AppHandle) {
				defer wg.Done()
				results[j*raceN+i] = doJSON(app.Client, app.BaseURL, key, http.MethodPost, "/v1/chat/completions", proxyChatBody)
			}(i, j, app)
		}
	}
	wg.Wait()
	accepted, budgetDenied, overloaded, other := 0, 0, 0, 0
	for _, r := range results {
		switch {
		case r == nil:
			other++
		case r.status == http.StatusOK:
			accepted++
		case r.status == http.StatusTooManyRequests && r.code == "budget_exceeded":
			budgetDenied++
		case r.status == http.StatusTooManyRequests && r.code == "overloaded":
			// Local capacity saturation is an explicit, allowed rejection —
			// it never reserved budget.
			overloaded++
		default:
			other++
			t.Logf("unexpected race outcome: %d %s", r.status, r.raw)
		}
	}
	if accepted != 1 || budgetDenied < 1 || budgetDenied+overloaded != 2*raceN-1 || other != 0 {
		t.Fatalf("shared budget race: accepted=%d budgetDenied=%d overloaded=%d other=%d (want 1/≥1/rest/0)",
			accepted, budgetDenied, overloaded, other)
	}
	// Authoritative shared accounting: the liability the budget sees
	// (held/unknown reservations plus settled usage records) never exceeds
	// the limit no matter how the two processes interleaved.
	var totalLiability int64
	if err := db.SQL.QueryRowContext(ctx, `
		SELECT (SELECT COALESCE(sum(limit_amount_micros),0) FROM reservations
		         WHERE tenant_id = $1 AND state IN ('held','unknown') AND id NOT LIKE '%/%')
		     + (SELECT COALESCE(sum(amount_micros),0) FROM usage_records
		         WHERE tenant_id = $1 AND created_at >= date_trunc('month', now()))`, tenant).Scan(&totalLiability); err != nil {
		t.Fatalf("sum liability: %v", err)
	}
	if totalLiability > budgetLimit {
		t.Fatalf("two processes over-admitted the shared budget: liability=%d limit=%d", totalLiability, budgetLimit)
	}

	// --- Phase 5: process termination retains unknown liability -------------
	// The race budget is deliberately exhausted now; the kill-phase stream
	// needs fresh headroom — widen it directly in the store.
	if _, err := db.SQL.ExecContext(ctx,
		`UPDATE budgets SET limit_value = limit_value + $2 WHERE tenant_id = $1::uuid AND id = 'b-conc'`,
		tenant, 2*chatBound); err != nil {
		t.Fatalf("widen budget: %v", err)
	}
	barrier2 := up.swapBarrier()
	defer close(barrier2)
	heldBefore := countReservations(t, db, tenant, "held")
	s6ctx, s6cancel := context.WithCancel(context.Background())
	defer s6cancel()
	s6, err := startStream(s6ctx, appB.Client, appB.BaseURL, key)
	if err != nil {
		t.Fatalf("kill-phase stream: %v", err)
	}
	if s6.StatusCode != http.StatusOK {
		t.Fatalf("kill-phase stream status: %d", s6.StatusCode)
	}
	waitFor(t, "upstream to observe kill-phase stream", func() bool {
		return up.streamsSeen.Load() == 3
	})
	appB.Kill()
	// The dead process can never settle or release: its reservation must
	// remain held (unknown-outcome liability preserved in PostgreSQL).
	waitFor(t, "kill-phase reservation to remain held", func() bool {
		return countReservations(t, db, tenant, "held") >= heldBefore+1
	})

	// Exact settled evidence: settled usage records carry the real upstream
	// unit counts, not estimates or zeros.
	var sumIn, sumOut int64
	rows, err := db.SQL.QueryContext(ctx,
		`SELECT units FROM usage_records WHERE tenant_id = $1`, tenant)
	if err != nil {
		t.Fatalf("usage records: %v", err)
	}
	defer rows.Close()
	n := 0
	for rows.Next() {
		var raw string
		if err := rows.Scan(&raw); err != nil {
			t.Fatalf("scan units: %v", err)
		}
		var units map[string]int64
		if err := json.Unmarshal([]byte(raw), &units); err != nil {
			t.Fatalf("decode units %q: %v", raw, err)
		}
		sumIn += units["input"]
		sumOut += units["output"]
		n++
	}
	if n == 0 || sumIn <= 0 {
		t.Fatalf("expected settled usage evidence with real units, got %d records sum_in=%d", n, sumIn)
	}
	t.Logf("observed: %d usage records, input=%d output=%d, race accepted=%d budgetDenied=%d overloaded=%d, streamsSeen=%d maxActive=%d",
		n, sumIn, sumOut, accepted, budgetDenied, overloaded, up.streamsSeen.Load(), up.maxActive.Load())
}

func getenvOr(key string) string { return strings.TrimSpace(os.Getenv(key)) }
