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

// usageFromMap maps one usage object to billing.Usage. Top-level numeric
// fields that are not part of the disjoint input/output accounting are
// preserved in OtherUnits so advertised units never settle as free.
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
	if cached > input {
		cached = input
	}
	out.InputUnits = input - cached
	out.OutputUnits = output
	out.CachedInputUnits = cached

	consumed := map[string]bool{
		"input_tokens": true, "prompt_tokens": true,
		"output_tokens": true, "completion_tokens": true,
		"total_tokens": true, "cached_tokens": true,
	}
	for k, v := range u {
		if consumed[k] || strings.HasSuffix(k, "_details") {
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

// accountingStream wraps one dispatched streaming body: it observes SSE
// events for the terminal usage record and settles or retains the attempt's
// liability exactly once — on clean termination, early caller Close or EOF
// without a terminal event. Cancellation propagates to the upstream body;
// accounting runs on a detached bounded context so client disconnect cannot
// strand the settlement write.
type accountingStream struct {
	body   io.ReadCloser
	finish func(usage []byte, terminal bool)

	pending  []byte
	usageRaw []byte
	terminal bool
	eof      bool
	once     sync.Once
}

func newAccountingStream(body io.ReadCloser, finish func(usage []byte, terminal bool)) *accountingStream {
	return &accountingStream{body: body, finish: finish}
}

func (s *accountingStream) Read(p []byte) (int, error) {
	n, err := s.body.Read(p)
	if n > 0 {
		s.scan(p[:n])
	}
	if errors.Is(err, io.EOF) {
		s.eof = true
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
	}
	if usageObject(env) != nil {
		s.usageRaw = append([]byte(nil), data...)
	} else if resp, ok := env["response"].(map[string]any); ok && usageObject(resp) != nil {
		s.usageRaw = append([]byte(nil), data...)
	}
}

// finalize runs the once-only accounting decision. A terminal event with
// parseable usage settles under the immutable snapshot; anything else —
// truncated stream, missing usage, early Close — retains unknown liability.
func (s *accountingStream) finalize() {
	s.once.Do(func() {
		s.finish(s.usageRaw, s.terminal)
	})
}

// Close finalizes accounting exactly once then closes the upstream body.
func (s *accountingStream) Close() error {
	s.finalize()
	return s.body.Close()
}

// accounting runs fn on a detached, bounded context. Settlement must outlive
// a cancelled client context but never block forever.
func accounting(ctx context.Context, timeout time.Duration, fn func(context.Context) error) error {
	actx, cancel := context.WithTimeout(context.WithoutCancel(ctx), timeout)
	defer cancel()
	return fn(actx)
}

// settleReservation applies the once-only outcome for a dispatched attempt:
// exact usage settles under the immutable snapshot; anything ambiguous keeps
// unknown liability. A settle failure also retains liability.
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
		rec, serr = s.billing.Settle(c, p, res.ID, usage)
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
