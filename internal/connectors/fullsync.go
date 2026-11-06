package connectors

import (
	"strconv"
	"strings"
	"time"
)

// DefaultFullSyncInterval bounds how long incremental syncs run between full
// listings; only a full listing sees deletions, edits of older items and
// replies to older threads.
const DefaultFullSyncInterval = 24 * time.Hour

// fullSyncDue reports whether a source's next sync must list everything: it
// was never fully listed, or its full_sync_interval (Go duration string or
// seconds; default 24h) has passed since the last full listing.
func fullSyncDue(config map[string]any, lastFull, now time.Time) bool {
	if lastFull.IsZero() {
		return true
	}
	return !now.Before(lastFull.Add(fullSyncInterval(config)))
}

func fullSyncInterval(config map[string]any) time.Duration {
	switch v := config["full_sync_interval"].(type) {
	case string:
		s := strings.TrimSpace(v)
		if secs, err := strconv.ParseFloat(s, 64); err == nil && secs > 0 {
			return time.Duration(secs * float64(time.Second))
		}
		if d, err := time.ParseDuration(s); err == nil && d > 0 {
			return d
		}
	case float64:
		if v > 0 {
			return time.Duration(v * float64(time.Second))
		}
	}
	return DefaultFullSyncInterval
}
