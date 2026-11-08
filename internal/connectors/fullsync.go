package connectors

import (
	"encoding/base64"
	"encoding/json"
	"errors"
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

// incrementalOverlap re-reads this much before the previous run's start so
// items updated while it ran, or under clock skew, are not missed.
const incrementalOverlap = 15 * time.Minute

// syncRun is the run-level state of adapters that list only changed items
// between periodic full listings. A stored checkpoint carries FullAt and
// LastStart; a run in progress also carries Full, Started and Since (all
// RFC3339). Started is empty until beginRun turns a checkpoint into a run.
type syncRun struct {
	Full      bool   `json:"full,omitempty"`
	FullAt    string `json:"full_at,omitempty"`
	LastStart string `json:"last_start,omitempty"`
	Started   string `json:"started,omitempty"`
	Since     string `json:"since,omitempty"`
}

// beginRun starts a run from the previous run's checkpoint: a full listing
// when no run finished yet or fullSyncDue says so, otherwise a delta listing
// of items updated since the previous run started, minus the overlap.
func beginRun(config map[string]any, checkpoint syncRun, now time.Time) syncRun {
	run := syncRun{FullAt: checkpoint.FullAt, Started: now.UTC().Format(time.RFC3339)}
	lastFull, _ := time.Parse(time.RFC3339, checkpoint.FullAt)
	last, err := time.Parse(time.RFC3339, checkpoint.LastStart)
	if err != nil || fullSyncDue(config, lastFull, now) {
		run.Full = true
		return run
	}
	run.Since = last.Add(-incrementalOverlap).UTC().Format(time.RFC3339)
	return run
}

// finish is the checkpoint a completed run hands to the next one.
func (r syncRun) finish() syncRun {
	out := syncRun{FullAt: r.FullAt, LastStart: r.Started}
	if r.Full {
		out.FullAt = r.Started
	}
	return out
}

// encodeJSONCursor and decodeJSONCursor share the "content:" + base64 JSON
// cursor format; an empty cursor decodes to the zero value.
func encodeJSONCursor(v any) Cursor {
	raw, _ := json.Marshal(v)
	return Cursor("content:" + base64.RawURLEncoding.EncodeToString(raw))
}

func decodeJSONCursor(cursor Cursor, v any) error {
	if cursor == "" {
		return nil
	}
	rest, ok := strings.CutPrefix(string(cursor), "content:")
	if !ok {
		return errors.New("invalid content cursor")
	}
	raw, err := base64.RawURLEncoding.DecodeString(rest)
	if err != nil || json.Unmarshal(raw, v) != nil {
		return errors.New("invalid content cursor")
	}
	return nil
}
