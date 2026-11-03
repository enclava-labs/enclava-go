package httpx

import (
	"sync"
	"time"
)

type rateWindow struct {
	start time.Time
	count int64
	ttl   time.Duration
}

type RateLimiter struct {
	mu sync.Mutex
	// key: apiKeyID + ":" + windowSeconds
	windows map[string]rateWindow
}

func NewRateLimiter() *RateLimiter {
	return &RateLimiter{
		windows: make(map[string]rateWindow),
	}
}

type RateLimitDecision struct {
	Allowed    bool
	Limit      int64
	Remaining  int64
	ResetEpoch int64
	RetryAfter int64
}

func (rl *RateLimiter) AllowKey(key string, limit int64, window time.Duration, now time.Time) RateLimitDecision {
	return rl.allow(key, limit, window, now)
}

// CheckKey reports the admission decision for a request WITHOUT consuming
// budget: callers can gate on every window first and only record the attempt
// (AllowKey) once it is actually consumed. An absent or expired window is
// allowed and reports the reset a window started now would have.
func (rl *RateLimiter) CheckKey(key string, limit int64, window time.Duration, now time.Time) RateLimitDecision {
	if limit <= 0 {
		return RateLimitDecision{Allowed: true}
	}
	if window <= 0 {
		window = time.Minute
	}
	if now.IsZero() {
		now = time.Now().UTC()
	}

	rl.mu.Lock()
	defer rl.mu.Unlock()

	w, ok := rl.windows[key]
	if !ok || now.Sub(w.start) >= window {
		return RateLimitDecision{
			Allowed:    true,
			Limit:      limit,
			Remaining:  limit,
			ResetEpoch: now.Add(window).Unix(),
		}
	}
	if w.count >= limit {
		retryAfter := int64(w.start.Add(window).Sub(now).Seconds())
		if retryAfter < 0 {
			retryAfter = 0
		}
		return RateLimitDecision{
			Allowed:    false,
			Limit:      limit,
			Remaining:  0,
			ResetEpoch: w.start.Add(window).Unix(),
			RetryAfter: retryAfter,
		}
	}
	return RateLimitDecision{
		Allowed:    true,
		Limit:      limit,
		Remaining:  limit - w.count,
		ResetEpoch: w.start.Add(window).Unix(),
	}
}

func (rl *RateLimiter) allow(key string, limit int64, window time.Duration, now time.Time) RateLimitDecision {
	if limit <= 0 {
		return RateLimitDecision{Allowed: true}
	}
	if window <= 0 {
		window = time.Minute
	}
	if now.IsZero() {
		now = time.Now().UTC()
	}

	rl.mu.Lock()
	defer rl.mu.Unlock()

	if len(rl.windows) >= 1024 {
		rl.cleanup(now)
	}

	w, ok := rl.windows[key]
	if !ok || now.Sub(w.start) >= window {
		w = rateWindow{start: now, count: 0, ttl: window}
	}

	if w.count >= limit {
		reset := w.start.Add(window).Unix()
		retryAfter := int64(w.start.Add(window).Sub(now).Seconds())
		if retryAfter < 0 {
			retryAfter = 0
		}
		rl.windows[key] = w
		return RateLimitDecision{
			Allowed:    false,
			Limit:      limit,
			Remaining:  0,
			ResetEpoch: reset,
			RetryAfter: retryAfter,
		}
	}

	w.count++
	rl.windows[key] = w

	remaining := limit - w.count
	if remaining < 0 {
		remaining = 0
	}
	return RateLimitDecision{
		Allowed:    true,
		Limit:      limit,
		Remaining:  remaining,
		ResetEpoch: w.start.Add(window).Unix(),
		RetryAfter: 0,
	}
}

func (rl *RateLimiter) cleanup(now time.Time) {
	for key, window := range rl.windows {
		ttl := window.ttl
		if ttl <= 0 {
			continue
		}
		if now.Sub(window.start) >= ttl {
			delete(rl.windows, key)
		}
	}
}

func itoa(v int64) string {
	// Tiny local itoa for positive numbers.
	if v == 0 {
		return "0"
	}
	neg := v < 0
	if neg {
		v = -v
	}
	buf := [32]byte{}
	i := len(buf)
	for v > 0 {
		i--
		buf[i] = byte('0' + (v % 10))
		v /= 10
	}
	if neg {
		i--
		buf[i] = '-'
	}
	return string(buf[i:])
}
