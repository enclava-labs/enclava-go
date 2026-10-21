package connectors

import (
	"context"
	"testing"
	"time"
)

func TestParseSyncInterval(t *testing.T) {
	for _, tc := range []struct {
		name    string
		config  map[string]any
		want    time.Duration
		enabled bool
	}{
		{"default when absent", nil, DefaultSyncInterval, true},
		{"default when empty", map[string]any{"sync_interval": ""}, DefaultSyncInterval, true},
		{"duration string", map[string]any{"sync_interval": "30m"}, 30 * time.Minute, true},
		{"seconds string", map[string]any{"sync_interval": "1800"}, 30 * time.Minute, true},
		{"seconds number", map[string]any{"sync_interval": float64(1800)}, 30 * time.Minute, true},
		{"int seconds", map[string]any{"sync_interval": 1800}, 30 * time.Minute, true},
		{"floored to minimum", map[string]any{"sync_interval": "10s"}, MinSyncInterval, true},
		{"off string", map[string]any{"sync_interval": "off"}, 0, false},
		{"disabled word", map[string]any{"sync_interval": "disabled"}, 0, false},
		{"zero", map[string]any{"sync_interval": "0"}, 0, false},
		{"zero number", map[string]any{"sync_interval": float64(0)}, 0, false},
		{"negative", map[string]any{"sync_interval": "-5m"}, 0, false},
		{"garbage disables", map[string]any{"sync_interval": "banana"}, 0, false},
		{"wrong type disables", map[string]any{"sync_interval": []any{"1h"}}, 0, false},
	} {
		got, enabled := parseSyncInterval(tc.config)
		if got != tc.want || enabled != tc.enabled {
			t.Fatalf("%s: parseSyncInterval() = %v, %v; want %v, %v", tc.name, got, enabled, tc.want, tc.enabled)
		}
	}
}

func TestSyncDue(t *testing.T) {
	now := time.Date(2026, 10, 21, 12, 0, 0, 0, time.UTC)
	hourAgo := now.Add(-DefaultSyncInterval)
	if !syncDue(map[string]any{}, hourAgo, now) {
		t.Fatal("default interval elapsed exactly should be due")
	}
	if syncDue(map[string]any{}, hourAgo.Add(time.Second), now) {
		t.Fatal("interval not yet elapsed must not be due")
	}
	if !syncDue(map[string]any{}, time.Time{}, now) {
		t.Fatal("never synced should be due")
	}
	if syncDue(map[string]any{"sync_interval": "off"}, time.Time{}, now) {
		t.Fatal("disabled source must never be due")
	}
}

// The scheduler enqueues one windowed job per due source, skips fresh and
// disabled ones, and the windowed key lets the next interval produce a new
// job instead of deduping into the succeeded one.
func TestSchedulerRunOnceEnqueuesDueSources(t *testing.T) {
	f := newConnectorFixture(t)
	ctx := context.Background()
	old := time.Now().UTC().Add(-2 * DefaultSyncInterval)
	due, err := f.svc.CreateSource(ctx, f.principal, CreateSourceInput{
		Type: SourceTypeGitHub, Name: "due", TargetCollectionID: "ragcol_test",
		Config: map[string]any{"owner": "acme", "repo": "docs", "paths": []any{"README.md"}},
	})
	if err != nil {
		t.Fatalf("create due source: %v", err)
	}
	fresh, err := f.svc.CreateSource(ctx, f.principal, CreateSourceInput{
		Type: SourceTypeGitHub, Name: "fresh", TargetCollectionID: "ragcol_test",
		Config: map[string]any{"owner": "acme", "repo": "docs", "paths": []any{"README.md"}},
	})
	if err != nil {
		t.Fatalf("create fresh source: %v", err)
	}
	off, err := f.svc.CreateSource(ctx, f.principal, CreateSourceInput{
		Type: SourceTypeGitHub, Name: "off", TargetCollectionID: "ragcol_test",
		Config: map[string]any{"owner": "acme", "repo": "docs", "paths": []any{"README.md"}, "sync_interval": "off"},
	})
	if err != nil {
		t.Fatalf("create off source: %v", err)
	}
	for _, id := range []string{due.ID, off.ID} {
		if err := f.repo.SetSyncState(ctx, f.tenant, id, old, "succeeded", ""); err != nil {
			t.Fatalf("set sync state %s: %v", id, err)
		}
	}
	if err := f.repo.SetSyncState(ctx, f.tenant, fresh.ID, time.Now().UTC(), "succeeded", ""); err != nil {
		t.Fatal(err)
	}

	s := NewScheduler(f.svc)
	s.runOnce(ctx)

	for _, id := range []string{due.ID, fresh.ID, off.ID} {
		jobs := f.queuedJobsFor(t, id)
		want := 0
		if id == due.ID {
			want = 1
		}
		if len(jobs) != want {
			t.Fatalf("source %s: expected %d queued job(s), got %v", id, want, jobs)
		}
	}
}

// queuedJobsFor lists queued sync jobs for one source — the manager's view,
// which includes scheduler-submitted jobs.
func (f *fixture) queuedJobsFor(t *testing.T, sourceID string) []string {
	t.Helper()
	list, err := f.svc.ListSyncJobs(context.Background(), f.principal, sourceID, "queued", 50, 0)
	if err != nil {
		t.Fatalf("list sync jobs for %s: %v", sourceID, err)
	}
	out := []string{}
	for _, j := range list {
		out = append(out, j.ID)
	}
	return out
}
