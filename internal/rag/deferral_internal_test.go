package rag

import (
	"testing"
	"time"
)

// TestDeferDelayShape pins the deferral backoff: 15 s doubling per
// consecutive deferral, capped at 5 m.
func TestDeferDelayShape(t *testing.T) {
	for _, tc := range []struct {
		deferrals int
		want      time.Duration
	}{
		{0, 15 * time.Second},
		{1, 30 * time.Second},
		{2, time.Minute},
		{3, 2 * time.Minute},
		{4, 4 * time.Minute},
		{5, 5 * time.Minute},
		{50, 5 * time.Minute},
	} {
		if got := deferDelay(tc.deferrals); got != tc.want {
			t.Fatalf("deferDelay(%d) = %v, want %v", tc.deferrals, got, tc.want)
		}
	}
}

// TestMaxWorkerWaitDefault pins that an unset/zero override restores the
// 24 h default (EXTRACT_WORKER_MAX_WAIT is optional config).
func TestMaxWorkerWaitDefault(t *testing.T) {
	s := &Service{}
	if got := s.maxWorkerWait(); got != defaultWorkerMaxWait {
		t.Fatalf("unset max wait = %v, want %v", got, defaultWorkerMaxWait)
	}
	s.SetExtractWorkerMaxWait(0)
	if got := s.maxWorkerWait(); got != defaultWorkerMaxWait {
		t.Fatalf("zero override = %v, want default", got)
	}
	s.SetExtractWorkerMaxWait(2 * time.Hour)
	if got := s.maxWorkerWait(); got != 2*time.Hour {
		t.Fatalf("override = %v, want 2h", got)
	}
}
