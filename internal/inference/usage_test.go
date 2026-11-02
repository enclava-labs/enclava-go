// usage_test.go covers per-kind usage parsing and stream accounting
// semantics: disjoint cached input, advertised nonstandard units never
// free, once-only finalize, and retained liability on truncated streams.
package inference

import (
	"io"
	"strings"
	"sync/atomic"
	"testing"
	"time"

	"enclava-go/internal/billing"
)

func TestUsageFromJSONChatShape(t *testing.T) {
	body := `{"id":"c1","model":"m","usage":{"prompt_tokens":100,"completion_tokens":40,"total_tokens":140,"prompt_tokens_details":{"cached_tokens":30}}}`
	u, ok := usageFromJSON([]byte(body))
	if !ok {
		t.Fatal("usage not parsed")
	}
	if u.InputUnits != 70 || u.OutputUnits != 40 || u.CachedInputUnits != 30 {
		t.Fatalf("disjoint units wrong: %+v", u)
	}
	if len(u.OtherUnits) != 0 {
		t.Fatalf("unexpected other units: %+v", u.OtherUnits)
	}
}

func TestUsageFromJSONResponsesShape(t *testing.T) {
	body := `{"type":"response.completed","response":{"id":"r1","usage":{"input_tokens":50,"output_tokens":12,"input_tokens_details":{"cached_tokens":50}}}}`
	u, ok := usageFromJSON([]byte(body))
	if !ok {
		t.Fatal("usage not parsed")
	}
	if u.InputUnits != 0 || u.OutputUnits != 12 || u.CachedInputUnits != 50 {
		t.Fatalf("cached input double-counted: %+v", u)
	}
}

func TestUsageFromJSONUnrecognizedUnitsAreNotFree(t *testing.T) {
	body := `{"usage":{"prompt_tokens":10,"completion_tokens":2,"audio_tokens":7}}`
	u, ok := usageFromJSON([]byte(body))
	if !ok {
		t.Fatal("usage not parsed")
	}
	if u.OtherUnits[billing.UnitKind("audio_tokens")] != 7 {
		t.Fatalf("advertised unit dropped: %+v", u.OtherUnits)
	}
}

// Chutes/vLLM report reasoning_tokens top level as a subset of
// completion_tokens (observed live: completion 6 incl. reasoning 1). It must
// bill once as output — neither a second unit nor an unpriced one.
func TestUsageFromJSONTopLevelReasoningTokensAreOutputBreakdown(t *testing.T) {
	body := `{"usage":{"prompt_tokens":18,"completion_tokens":6,"reasoning_tokens":1,"total_tokens":24,"prompt_tokens_details":{"cached_tokens":3}}}`
	u, ok := usageFromJSON([]byte(body))
	if !ok {
		t.Fatal("usage not parsed")
	}
	if u.InputUnits != 15 || u.CachedInputUnits != 3 || u.OutputUnits != 6 {
		t.Fatalf("units wrong: %+v", u)
	}
	if len(u.OtherUnits) != 0 {
		t.Fatalf("reasoning tokens billed as a separate unit: %+v", u.OtherUnits)
	}
}

func TestUsageFromJSONMissingUsage(t *testing.T) {
	if _, ok := usageFromJSON([]byte(`{"id":"c1"}`)); ok {
		t.Fatal("missing usage must report unknown, not zero")
	}
	if _, ok := usageFromJSON([]byte(`not json`)); ok {
		t.Fatal("malformed body must report unknown")
	}
}

func TestAccountingStreamSettlesTerminalUsage(t *testing.T) {
	var got struct {
		calls    int32
		terminal bool
		usage    []byte
	}
	finish := func(usage []byte, terminal bool) {
		atomic.AddInt32(&got.calls, 1)
		got.usage = usage
		got.terminal = terminal
	}
	sse := "data: {\"id\":\"c1\",\"choices\":[{\"delta\":{\"content\":\"hi\"}}]}\n\n" +
		"data: {\"id\":\"c1\",\"usage\":{\"prompt_tokens\":11,\"completion_tokens\":4}}\n\n" +
		"data: [DONE]\n\n"
	s := newAccountingStream(io.NopCloser(strings.NewReader(sse)), finish)
	if _, err := io.ReadAll(s); err != nil {
		t.Fatal(err)
	}
	_ = s.Close()
	if atomic.LoadInt32(&got.calls) != 1 {
		t.Fatalf("finish ran %d times", got.calls)
	}
	if !got.terminal || len(got.usage) == 0 {
		t.Fatalf("terminal=%v usage=%s", got.terminal, got.usage)
	}
	u, ok := usageFromJSON(got.usage)
	if !ok || u.InputUnits != 11 || u.OutputUnits != 4 {
		t.Fatalf("settled usage wrong: %+v", u)
	}
}

func TestAccountingStreamResponsesTerminal(t *testing.T) {
	var calls int32
	var terminal bool
	finish := func(usage []byte, term bool) {
		atomic.AddInt32(&calls, 1)
		terminal = term
	}
	sse := "data: {\"type\":\"response.completed\",\"response\":{\"usage\":{\"input_tokens\":3,\"output_tokens\":1}}}\n\n"
	s := newAccountingStream(io.NopCloser(strings.NewReader(sse)), finish)
	if _, err := io.ReadAll(s); err != nil {
		t.Fatal(err)
	}
	if atomic.LoadInt32(&calls) != 1 || !terminal {
		t.Fatalf("terminal=%v calls=%d", terminal, calls)
	}
}

// waitStreamFinalized waits until the stream's once-only accounting ran to
// completion — Close returns promptly now that terminal usage drains in the
// background. finalized flips before the finish callback inside the once-
// block, so readMu (which serializes finalize) is the completion signal.
func waitStreamFinalized(t *testing.T, s *accountingStream) {
	t.Helper()
	deadline := time.Now().Add(5 * time.Second)
	for !s.finalized.Load() {
		if !time.Now().Before(deadline) {
			t.Fatal("stream accounting never finalized")
		}
		time.Sleep(time.Millisecond)
	}
	s.readMu.Lock()
	s.readMu.Unlock()
}

func TestAccountingStreamEarlyCloseRetainsLiability(t *testing.T) {
	var calls int32
	var terminal bool
	finish := func(usage []byte, term bool) {
		atomic.AddInt32(&calls, 1)
		terminal = term
	}
	// A stream that never terminates.
	s := newAccountingStream(io.NopCloser(strings.NewReader("data: {\"choices\":[{\"delta\":{}}]}\n")), finish)
	buf := make([]byte, 64)
	_, _ = s.Read(buf)
	if err := s.Close(); err != nil {
		t.Fatal(err)
	}
	waitStreamFinalized(t, s)
	if atomic.LoadInt32(&calls) != 1 || terminal {
		t.Fatalf("early close must be non-terminal: calls=%d terminal=%v", calls, terminal)
	}
	// Double close finalizes once.
	_ = s.Close()
	if atomic.LoadInt32(&calls) != 1 {
		t.Fatalf("double close ran finish twice")
	}
}

// A client-abandoned stream keeps reading the upstream body and settles the
// provider's real terminal usage — never unknown while the drain succeeds.
func TestAccountingStreamEarlyCloseDrainsTerminalUsage(t *testing.T) {
	var calls int32
	var terminal bool
	var usage []byte
	finish := func(raw []byte, term bool) {
		atomic.AddInt32(&calls, 1)
		terminal = term
		usage = raw
	}
	sse := "data: {\"choices\":[{\"delta\":{\"content\":\"hi\"}}]}\n\n" +
		"data: {\"usage\":{\"prompt_tokens\":9,\"completion_tokens\":2}}\n\n" +
		"data: [DONE]\n\n"
	s := newAccountingStream(io.NopCloser(strings.NewReader(sse)), finish)
	buf := make([]byte, 32)
	if _, err := s.Read(buf); err != nil {
		t.Fatal(err)
	}
	// Caller abandons mid-stream: Close returns promptly, the drain reads
	// out the terminal usage event and finalizes exactly.
	if err := s.Close(); err != nil {
		t.Fatal(err)
	}
	waitStreamFinalized(t, s)
	if atomic.LoadInt32(&calls) != 1 || !terminal {
		t.Fatalf("drained stream must be terminal: calls=%d terminal=%v", calls, terminal)
	}
	u, ok := usageFromJSON(usage)
	if !ok || u.InputUnits != 9 || u.OutputUnits != 2 {
		t.Fatalf("drained usage wrong: %+v", u)
	}
}

// usageFromMap ignores non-token numerics and breakdown fields already
// inside a priced kind; DeepSeek's flat cache-hit count fills in when the
// details object reports no cached input.
func TestUsageFromMapDeepSeekAndIgnoredNumerics(t *testing.T) {
	u := usageFromMap(map[string]any{
		"prompt_tokens":            float64(100),
		"completion_tokens":        float64(40),
		"total_tokens":             float64(140),
		"prompt_cache_hit_tokens":  float64(30),
		"prompt_cache_miss_tokens": float64(70),
		"cost":                     float64(0.02),
		"latency_time":             float64(1.5),
		"audio_tokens":             float64(7),
	})
	if u.InputUnits != 70 || u.OutputUnits != 40 || u.CachedInputUnits != 30 {
		t.Fatalf("deepseek units wrong: %+v", u)
	}
	if len(u.OtherUnits) != 1 || u.OtherUnits[billing.UnitKind("audio_tokens")] != 7 {
		t.Fatalf("other units wrong: %+v", u.OtherUnits)
	}

	// A details-object cached count wins over the flat field, and cached
	// never exceeds input.
	u = usageFromMap(map[string]any{
		"prompt_tokens":           float64(10),
		"prompt_cache_hit_tokens": float64(99),
		"prompt_tokens_details":   map[string]any{"cached_tokens": float64(4)},
	})
	if u.CachedInputUnits != 4 || u.InputUnits != 6 {
		t.Fatalf("details cached must win: %+v", u)
	}
	u = usageFromMap(map[string]any{
		"prompt_tokens":           float64(10),
		"prompt_cache_hit_tokens": float64(99),
	})
	if u.CachedInputUnits != 10 || u.InputUnits != 0 {
		t.Fatalf("cached must clamp to input: %+v", u)
	}
}

func TestAccountingStreamEOFWithoutTerminalRetainsLiability(t *testing.T) {
	var terminal bool
	finish := func(usage []byte, term bool) { terminal = term }
	s := newAccountingStream(io.NopCloser(strings.NewReader(
		"data: {\"choices\":[{\"delta\":{}}]}\n")), finish)
	if _, err := io.ReadAll(s); err != nil {
		t.Fatal(err)
	}
	if terminal {
		t.Fatal("truncated stream must not settle")
	}
}
