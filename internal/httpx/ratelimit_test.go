// ratelimit_test.go — IDOP-I01 regression coverage: every rate-limit window
// owns a distinct bucket so one request is counted once per window, windows
// keep independent reset timestamps, and a request one window rejects does
// not consume budget in the others.
package httpx

import (
	"net/http"
	"net/http/httptest"
	"testing"
	"time"
)

func TestEnforceRateLimitsWindowIsolation(t *testing.T) {
	app := &App{rl: NewRateLimiter()}
	now := time.Now()
	checks := []rateLimitCheck{
		{limit: 2, window: time.Minute},
		{limit: 500, window: time.Hour},
		{limit: 5000, window: 24 * time.Hour},
	}

	for i := 1; i <= 2; i++ {
		if !app.enforceRateLimits(httptest.NewRecorder(), "cred:k", now, checks) {
			t.Fatalf("request %d must be admitted", i)
		}
	}
	// Each window counted each request exactly once, in its own bucket.
	for _, tc := range []struct {
		window time.Duration
		count  int64
	}{
		{time.Minute, 2}, {time.Hour, 2}, {24 * time.Hour, 2},
	} {
		w, ok := app.rl.windows["cred:k:"+itoa(int64(tc.window/time.Second))]
		if !ok {
			t.Fatalf("missing %s bucket", tc.window)
		}
		if w.count != tc.count {
			t.Fatalf("%s bucket count = %d, want %d", tc.window, w.count, tc.count)
		}
	}
	if len(app.rl.windows) != 3 {
		t.Fatalf("expected one bucket per window, got %d: %v", len(app.rl.windows), app.rl.windows)
	}

	// Third request: the minute window denies, and the rejection must not
	// consume hour/day budget.
	rec := httptest.NewRecorder()
	if app.enforceRateLimits(rec, "cred:k", now, checks) {
		t.Fatal("third request inside the minute must be rejected")
	}
	if rec.Code != http.StatusTooManyRequests {
		t.Fatalf("expected 429, got %d", rec.Code)
	}
	for _, tc := range []struct {
		window time.Duration
		count  int64
	}{
		{time.Hour, 2}, {24 * time.Hour, 2},
	} {
		if w := app.rl.windows["cred:k:"+itoa(int64(tc.window/time.Second))]; w.count != tc.count {
			t.Fatalf("rejected request consumed %s budget: count = %d, want %d", tc.window, w.count, tc.count)
		}
	}

	// After the minute window rolls over the request is admitted again.
	if !app.enforceRateLimits(httptest.NewRecorder(), "cred:k", now.Add(time.Minute+time.Second), checks) {
		t.Fatal("request after the minute window must be admitted")
	}
	if w := app.rl.windows["cred:k:"+itoa(int64((time.Hour)/time.Second))]; w.count != 3 {
		t.Fatalf("hour bucket after rollover = %d, want 3", w.count)
	}
}

func TestCheckKeyDoesNotConsume(t *testing.T) {
	rl := NewRateLimiter()
	now := time.Now()
	for range 3 {
		if dec := rl.CheckKey("k", 2, time.Minute, now); !dec.Allowed {
			t.Fatal("a non-incrementing check can never exhaust the limit")
		}
	}
	rl.AllowKey("k", 2, time.Minute, now)
	rl.AllowKey("k", 2, time.Minute, now)
	if dec := rl.CheckKey("k", 2, time.Minute, now); dec.Allowed {
		t.Fatal("check must observe the recorded count")
	}
	if dec := rl.CheckKey("k", 2, time.Minute, now.Add(2*time.Minute)); !dec.Allowed {
		t.Fatal("expired window must check allowed")
	}
}
