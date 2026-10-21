package connectors

import (
	"context"
	"log"
	"strconv"
	"strings"
	"time"

	"enclava-go/internal/auth"
)

// Scheduling defaults and bounds. The per-source override lives in config
// sync_interval (Go duration string like "30m", or seconds as a number);
// "0"/"off" disables scheduled syncs for that source.
const (
	DefaultSyncInterval = time.Hour
	MinSyncInterval     = 5 * time.Minute
)

// parseSyncInterval resolves a connection's configured sync interval with the
// 1h default. Unparseable values disable scheduling rather than guessing.
func parseSyncInterval(config map[string]any) (time.Duration, bool) {
	switch v := config["sync_interval"]; concrete := v.(type) {
	case nil:
		return DefaultSyncInterval, true
	case string:
		s := strings.TrimSpace(strings.ToLower(concrete))
		switch {
		case s == "":
			return DefaultSyncInterval, true
		case s == "0" || s == "off" || s == "disabled":
			return 0, false
		}
		if secs, err := strconv.ParseFloat(s, 64); err == nil {
			return secondsOrDisabled(secs)
		}
		if d, err := time.ParseDuration(s); err == nil {
			return durationOrDisabled(d)
		}
		return 0, false
	case float64:
		return secondsOrDisabled(concrete)
	case int:
		return secondsOrDisabled(float64(concrete))
	default:
		return 0, false
	}
}

func secondsOrDisabled(secs float64) (time.Duration, bool) {
	if secs <= 0 {
		return 0, false
	}
	return max(time.Duration(secs*float64(time.Second)), MinSyncInterval), true
}

func durationOrDisabled(d time.Duration) (time.Duration, bool) {
	if d <= 0 {
		return 0, false
	}
	return max(d, MinSyncInterval), true
}

// syncDue reports whether a connection's interval has elapsed. Any completed
// sync (manual or scheduled) resets the clock via synced_at; a never-synced
// connection is due from creation.
func syncDue(config map[string]any, syncedAt time.Time, now time.Time) bool {
	interval, enabled := parseSyncInterval(config)
	if !enabled {
		return false
	}
	if syncedAt.IsZero() {
		return true
	}
	return !now.Before(syncedAt.Add(interval))
}

// Scheduler re-enqueues connector syncs for due sources. One tick per minute;
// per-source intervals are enforced by syncDue.
type Scheduler struct {
	svc  *Service
	tick time.Duration
}

func NewScheduler(svc *Service) *Scheduler {
	return &Scheduler{svc: svc, tick: time.Minute}
}

func (s *Scheduler) Run(ctx context.Context) {
	t := time.NewTicker(s.tick)
	defer t.Stop()
	for {
		select {
		case <-ctx.Done():
			return
		case <-t.C:
			s.runOnce(ctx)
		}
	}
}

func (s *Scheduler) runOnce(ctx context.Context) {
	conns, err := s.svc.repo.ListSyncCandidates(ctx, 500)
	if err != nil {
		log.Printf("connector scheduler: list candidates: %v", err)
		return
	}
	now := time.Now().UTC()
	for _, c := range conns {
		if ctx.Err() != nil {
			return
		}
		var syncedAt time.Time
		if c.SyncedAt != nil {
			syncedAt = *c.SyncedAt
		}
		if !syncDue(c.Config, syncedAt, now) {
			continue
		}
		// The job runs as the source's creator: authority the creator still
		// holds. If they lost the collection, the sync fails honestly.
		p := auth.Principal{TenantID: c.TenantID, ActorID: c.CreatedBy}
		interval, _ := parseSyncInterval(c.Config)
		if _, err := s.svc.SyncSourceWindowed(ctx, p, c.ID, now.Unix()/int64(interval/time.Second)); err != nil {
			log.Printf("connector scheduler: sync %s: %v", c.ID, err)
		}
	}
}
