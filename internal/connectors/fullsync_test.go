package connectors

import (
	"testing"
	"time"
)

func TestBeginRunChoosesFullOrDeltaListing(t *testing.T) {
	now := time.Date(2026, 11, 8, 12, 0, 0, 0, time.UTC)
	at := func(d time.Duration) string { return now.Add(d).Format(time.RFC3339) }
	for name, tc := range map[string]struct {
		config     map[string]any
		checkpoint syncRun
		full       bool
		since      string
	}{
		"first run":            {checkpoint: syncRun{}, full: true},
		"legacy cursor":        {checkpoint: syncRun{FullAt: at(-time.Hour)}, full: true},
		"within interval":      {checkpoint: syncRun{FullAt: at(-time.Hour), LastStart: at(-30 * time.Minute)}, since: at(-45 * time.Minute)},
		"interval elapsed":     {checkpoint: syncRun{FullAt: at(-25 * time.Hour), LastStart: at(-30 * time.Minute)}, full: true},
		"configured interval":  {config: map[string]any{"full_sync_interval": "30m"}, checkpoint: syncRun{FullAt: at(-time.Hour), LastStart: at(-10 * time.Minute)}, full: true},
		"unparseable last run": {checkpoint: syncRun{FullAt: at(-time.Hour), LastStart: "yesterday"}, full: true},
	} {
		run := beginRun(tc.config, tc.checkpoint, now)
		if run.Full != tc.full || run.Since != tc.since || run.Started != now.Format(time.RFC3339) || run.FullAt != tc.checkpoint.FullAt {
			t.Errorf("%s: run = %+v", name, run)
		}
	}

	full := beginRun(nil, syncRun{}, now).finish()
	if full != (syncRun{FullAt: now.Format(time.RFC3339), LastStart: now.Format(time.RFC3339)}) {
		t.Fatalf("full run checkpoint = %+v", full)
	}
	later := now.Add(time.Hour)
	delta := beginRun(nil, full, later).finish()
	if delta != (syncRun{FullAt: full.FullAt, LastStart: later.Format(time.RFC3339)}) {
		t.Fatalf("delta run checkpoint = %+v", delta)
	}
}
