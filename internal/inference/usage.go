// usage.go owns exact per-kind usage accounting for executed attempts
// (JOB-04/INF-05). Provider input/output/cached counts map to disjoint
// billing unit kinds — cached input never settles twice — and unrecognized
// advertised usage fields become billed OtherUnits rather than being treated
// as free. Streaming bodies are wrapped so a cancelled or truncated stream
// retains its reservation liability instead of silently releasing it.
package inference

import (
	"bytes"
	"context"
	"encoding/json"
	"errors"
	"io"
	"math"
	"strings"
	"sync"
	"sync/atomic"
	"time"

	"enclava-go/internal/auth"
	"enclava-go/internal/billing"
	"enclava-go/internal/llm"
)

// usageFromJSON decodes the "usage" object of one provider response or
// terminal stream event into disjoint billing units. Missing usage reports
// false — the caller must treat the outcome as unknown, never zero.
// Cached input is a subset of reported input settled disjointly: InputUnits
// is input-minus-cached so cached tokens are never charged twice.
func usageFromJSON(raw []byte) (billing.Usage, bool) {
	var env map[string]any
	if err := json.Unmarshal(raw, &env); err != nil {
		return billing.Usage{}, false
	}
	usage := usageObject(env)
	if usage == nil {
		// Responses terminal events nest the final object under "response".
		if resp, ok := env["response"].(map[string]any); ok {
			usage = usageObject(resp)
		}
	}
	if usage == nil && env["object"] == "response.input_tokens" {
		// input_tokens responses carry the count at top level rather than
		// under "usage"; synthesize the same shape so the priced operation
		// settles its measured input instead of retaining liability.
		usage = map[string]any{"input_tokens": env["input_tokens"]}
	}
	if usage == nil {
		return billing.Usage{}, false
	}
	return usageFromMap(usage), true
}

func usageObject(env map[string]any) map[string]any {
	if u, ok := env["usage"].(map[string]any); ok {
		return u
	}
	return nil
}

// usageField converts a numeric JSON value to an exact integer unit count.
// Non-integral values round up — never silently free a fractional billed unit.
func usageField(v any) int64 {
	f, ok := usageFloat(v)
	if !ok || f <= 0 {
		return 0
	}
	return int64(math.Ceil(f))
}

func usageFloat(v any) (float64, bool) {
	switch n := v.(type) {
	case float64:
		return n, true
	case json.Number:
		f, err := n.Float64()
		return f, err == nil
	default:
		return 0, false
	}
}

// usageFromMap maps one usage object to billing.Usage: only token counts are
// units. Cached input is a subset of reported input settled disjointly
// (InputUnits is input-minus-cached so cached tokens never bill twice);
// DeepSeek's flat prompt_cache_hit_tokens fills in when no details object
// carries a cached count. Non-token numerics (cost, *_time) and token
// breakdowns already inside a priced kind (total, reasoning, cache misses)
// are ignored; unmapped top-level *_tokens fields become billed OtherUnits
// rather than settling as free.
func usageFromMap(u map[string]any) billing.Usage {
	var out billing.Usage

	input := usageField(u["input_tokens"])
	if input == 0 {
		input = usageField(u["prompt_tokens"])
	}
	output := usageField(u["output_tokens"])
	if output == 0 {
		output = usageField(u["completion_tokens"])
	}

	var cached int64
	for _, key := range []string{"prompt_tokens_details", "input_tokens_details"} {
		if d, ok := u[key].(map[string]any); ok {
			cached += usageField(d["cached_tokens"])
		}
	}
	if cached == 0 {
		cached = usageField(u["prompt_cache_hit_tokens"])
	}
	if cached > input {
		cached = input
	}
	out.InputUnits = input - cached
	out.OutputUnits = output
	out.CachedInputUnits = cached

	consumed := map[string]bool{
		"input_tokens": true, "prompt_tokens": true,
		"output_tokens": true, "completion_tokens": true,
		"total_tokens": true, "prompt_cache_hit_tokens": true,
		// Breakdowns already inside a priced kind — reasoning is part of
		// completion/output, cache misses part of prompt/input. Counting
		// them again would double-charge; pricing them separately would
		// strand settlement.
		"reasoning_tokens": true, "prompt_cache_miss_tokens": true,
	}
	for k, v := range u {
		if consumed[k] || !strings.HasSuffix(k, "_tokens") {
			continue
		}
		if n := usageField(v); n > 0 {
			if out.OtherUnits == nil {
				out.OtherUnits = map[billing.UnitKind]int64{}
			}
			out.OtherUnits[billing.UnitKind(k)] = n
		}
	}
	return out
}

// terminalStreamTypes are Responses stream event types that end the attempt's
// observable output; the final object then carries the usage record.
var terminalStreamTypes = map[string]bool{
	"response.completed":  true,
	"response.incomplete": true,
	"response.failed":     true,
	"response.cancelled":  true,
}

// streamResult classifies how a streamed body ended — the endpoint-health
// evidence the runtime observation ring consumes. Client cancellation is
// never endpoint health; a terminal error event is an answered upstream.
type streamResult struct {
	// ClientCanceled is set when the caller closed the body before the
	// upstream ended it — no endpoint observation may be derived.
	ClientCanceled bool
	// Terminal is set when a terminal event/[DONE] ended the stream.
	Terminal bool
	// UpstreamFailed is set when a terminal event reported failure
	// ("error"/"response.failed"): the endpoint answered, so the outcome is
	// degraded rather than unreachable.
	UpstreamFailed bool
	// Err is the upstream read error when the body failed mid-stream.
	Err error
}

// accountingStream wraps one dispatched streaming body: it observes SSE
// events for the terminal usage record and settles or retains the attempt's
// liability exactly once — on clean termination, caller Close or a failed
// drain. Caller Close before a terminal event returns promptly while a
// background drain keeps reading the upstream body (bounded by the
// transport's own idle/wall limits) until the terminal usage event, so a
// client disconnect still settles the provider's real token counts; only a
// failed drain retains unknown liability. Accounting runs on a detached
// bounded context so client disconnect cannot strand the settlement write.
// All reads and terminal-state mutation serialize under readMu; Close and
// finalized are atomic so prompt Close and slow finalization can race.
type accountingStream struct {
	body   io.ReadCloser
	finish func(usage []byte, terminal bool)

	// outcomeFn is the optional endpoint-health observer; it runs once with
	// the terminal classification, alongside finish.
	outcomeFn func(streamResult)

	readMu         sync.Mutex
	pending        []byte
	usageRaw       []byte
	terminal       bool
	upstreamFailed bool
	eof            bool
	readErr        error
	once           sync.Once
	finalized      atomic.Bool
	closed         atomic.Bool
}

func newAccountingStream(body io.ReadCloser, finish func(usage []byte, terminal bool)) *accountingStream {
	return &accountingStream{body: body, finish: finish}
}

// onOutcome registers the endpoint-health observer for this stream. At most
// one observer; invoked exactly once, in the same once-block as finish.
func (s *accountingStream) onOutcome(fn func(streamResult)) {
	s.outcomeFn = fn
}

func (s *accountingStream) Read(p []byte) (int, error) {
	if s.closed.Load() {
		return 0, io.EOF
	}
	s.readMu.Lock()
	defer s.readMu.Unlock()
	if s.closed.Load() {
		return 0, io.EOF
	}
	n, err := s.body.Read(p)
	if n > 0 {
		s.scan(p[:n])
	}
	switch {
	case errors.Is(err, io.EOF):
		s.eof = true
		s.finalize()
	case err != nil:
		// The upstream body broke mid-stream — the read error is the
		// endpoint-failure evidence; accounting still decides below.
		s.readErr = err
		s.finalize()
	}
	return n, err
}

// scan consumes complete SSE lines, remembering the latest usage object and
// whether a terminal event or [DONE] marker was observed.
func (s *accountingStream) scan(chunk []byte) {
	s.pending = append(s.pending, chunk...)
	for {
		i := bytes.IndexByte(s.pending, '\n')
		if i < 0 {
			break
		}
		line := s.pending[:i]
		s.pending = s.pending[i+1:]
		s.observeLine(line)
	}
	// A partial line tail stays bounded by the response stream cap upstream.
	if len(s.pending) > 1<<20 {
		s.pending = s.pending[len(s.pending)-(1<<20):]
	}
}

func (s *accountingStream) observeLine(line []byte) {
	data, ok := bytes.CutPrefix(bytes.TrimSuffix(line, []byte("\r")), []byte("data:"))
	if !ok {
		return
	}
	data = bytes.TrimLeft(data, " ")
	if string(data) == "[DONE]" {
		s.terminal = true
		return
	}
	var env map[string]any
	if json.Unmarshal(data, &env) != nil {
		return
	}
	if t, _ := env["type"].(string); terminalStreamTypes[t] || t == "error" {
		s.terminal = true
		if t == "error" || t == "response.failed" {
			// The endpoint answered with a reported failure — degraded
			// evidence, not a connectivity failure.
			s.upstreamFailed = true
		}
	}
	if usageObject(env) != nil {
		s.usageRaw = append([]byte(nil), data...)
	} else if resp, ok := env["response"].(map[string]any); ok && usageObject(resp) != nil {
		s.usageRaw = append([]byte(nil), data...)
	}
}

// finalize runs the once-only accounting decision under readMu. A terminal
// event with parseable usage settles under the immutable snapshot; anything
// else — truncated stream, missing usage, failed drain — retains unknown
// liability. The same once-block derives the endpoint-health
// classification: caller Close before any upstream end is client
// cancellation (no observation), a read error carries its error, EOF
// without a terminal event is a transport-class premature end.
func (s *accountingStream) finalize() {
	s.once.Do(func() {
		s.finalized.Store(true)
		if s.outcomeFn != nil {
			res := streamResult{Terminal: s.terminal, UpstreamFailed: s.upstreamFailed, Err: s.readErr}
			if s.closed.Load() && s.readErr == nil && !s.terminal {
				res.ClientCanceled = true
			}
			s.outcomeFn(res)
		}
		s.finish(s.usageRaw, s.terminal)
	})
}

// drain keeps reading the upstream body in the background after caller
// Close, until the terminal usage event or a read failure; the transport's
// own idle/wall bounds cap it. It owns the body's eventual Close.
func (s *accountingStream) drain() {
	buf := make([]byte, 32*1024)
	for {
		s.readMu.Lock()
		switch {
		case s.finalized.Load():
			s.readMu.Unlock()
			_ = s.body.Close()
			return
		case s.terminal && s.usageRaw != nil:
			// The terminal usage event is in hand — settle exactly now,
			// no need to read the body's remaining bytes.
			s.finalize()
			s.readMu.Unlock()
			_ = s.body.Close()
			return
		case s.eof || s.readErr != nil:
			s.finalize()
			s.readMu.Unlock()
			_ = s.body.Close()
			return
		}
		n, err := s.body.Read(buf)
		if n > 0 {
			s.scan(buf[:n])
		}
		switch {
		case errors.Is(err, io.EOF):
			s.eof = true
			s.finalize()
			s.readMu.Unlock()
			_ = s.body.Close()
			return
		case err != nil:
			s.readErr = err
			s.finalize()
			s.readMu.Unlock()
			_ = s.body.Close()
			return
		}
		s.readMu.Unlock()
	}
}

// Close returns promptly: a stream that already ended closes its body at
// once; one still in flight starts the background drain that reads out the
// terminal usage event and settles exactly, closing the body when done.
func (s *accountingStream) Close() error {
	if s.closed.CompareAndSwap(false, true) {
		if s.finalized.Load() {
			return s.body.Close()
		}
		go s.drain()
	}
	return nil
}

// accounting runs fn on a detached, bounded context. Settlement must outlive
// a cancelled client context but never block forever.
func accounting(ctx context.Context, timeout time.Duration, fn func(context.Context) error) error {
	actx, cancel := context.WithTimeout(context.WithoutCancel(ctx), timeout)
	defer cancel()
	return fn(actx)
}

// settleReservation applies the once-only outcome for a dispatched attempt:
// exact usage settles under the immutable snapshot — retried briefly so a
// transient store failure does not strand exact accounting — and anything
// ambiguous keeps unknown liability. An exhausted settle also retains
// liability.
func (s *Service) settleReservation(ctx context.Context, p auth.Principal, res billing.Reservation, usage billing.Usage, usageKnown bool) (*billing.UsageRecord, error) {
	if !usageKnown {
		err := accounting(ctx, accountingTimeout, func(c context.Context) error {
			return s.billing.MarkUnknown(c, p, res.ID)
		})
		return nil, err
	}
	var rec billing.UsageRecord
	err := accounting(ctx, accountingTimeout, func(c context.Context) error {
		var serr error
		for attempt := range 3 {
			if attempt > 0 {
				timer := time.NewTimer(200 * time.Millisecond)
				select {
				case <-c.Done():
					timer.Stop()
					return c.Err()
				case <-timer.C:
				}
			}
			rec, serr = s.billing.Settle(c, p, res.ID, usage)
			if serr == nil {
				return nil
			}
		}
		return serr
	})
	if err != nil {
		// The upstream outcome happened; the reservation stays liable.
		_ = accounting(ctx, accountingTimeout, func(c context.Context) error {
			return s.billing.MarkUnknown(c, p, res.ID)
		})
		return nil, err
	}
	return &rec, nil
}

// isUpstreamError reports whether the provider returned a definitive error
// status — settling its (possibly zero) usage is honest — versus an
// ambiguous transport failure where liability must be retained.
func isUpstreamError(err error) bool {
	var ue *llm.UpstreamError
	return errors.As(err, &ue)
}
