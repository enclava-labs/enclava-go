package chatbot

import "sync"

// RunStreamEvent is one Chat-page stream event: Kind is "delta" (Text set),
// "reset" or "done". Deltas exist only in memory — the persisted thread stays
// the single source of truth.
type RunStreamEvent struct {
	Kind string
	Text string
}

const (
	// maxBrokerText caps the accumulated per-attempt text a late subscriber
	// can replay; live deltas keep flowing past the cap.
	maxBrokerText = 256 << 10
	// brokerSubscriberBuffer is the per-subscriber queue: a subscriber that
	// falls this far behind is dropped, never blocking the worker.
	brokerSubscriberBuffer = 16
)

type brokerSubscriber struct {
	ch   chan RunStreamEvent
	done bool
}

type brokerEntry struct {
	text   []byte
	subs   map[*brokerSubscriber]struct{}
	closed bool
}

// runBroker is the in-process fan-out of the CURRENT model attempt's content
// deltas, keyed by run ID. No persistence: an entry lives from worker start
// to worker end in the same process; anything else reads "done".
type runBroker struct {
	mu      sync.Mutex
	entries map[string]*brokerEntry
}

func (b *runBroker) entry(runID string) *brokerEntry {
	b.mu.Lock()
	defer b.mu.Unlock()
	if b.entries == nil {
		b.entries = map[string]*brokerEntry{}
	}
	e := b.entries[runID]
	if e == nil {
		e = &brokerEntry{subs: map[*brokerSubscriber]struct{}{}}
		b.entries[runID] = e
	}
	return e
}

// publish appends to the accumulated text (bounded) and fans out an event,
// dropping any subscriber whose buffer is full.
func (b *runBroker) publish(runID, kind, text string) {
	b.mu.Lock()
	defer b.mu.Unlock()
	e := b.entries[runID]
	if e == nil {
		return
	}
	if kind == "reset" {
		e.text = e.text[:0]
	}
	if kind == "delta" && len(e.text) < maxBrokerText {
		e.text = append(e.text, text...)
	}
	for sub := range e.subs {
		if sub.done {
			continue
		}
		select {
		case sub.ch <- RunStreamEvent{Kind: kind, Text: text}:
		default:
			sub.done = true
			close(sub.ch)
			delete(e.subs, sub)
		}
	}
}

// finish ends the entry: subscribers see done and channel close.
func (b *runBroker) finish(runID string) {
	b.mu.Lock()
	defer b.mu.Unlock()
	e := b.entries[runID]
	if e == nil {
		return
	}
	e.closed = true
	for sub := range e.subs {
		if !sub.done {
			sub.done = true
			select {
			case sub.ch <- RunStreamEvent{Kind: "done"}:
			default:
			}
			close(sub.ch)
		}
		delete(e.subs, sub)
	}
	delete(b.entries, runID)
}

// subscribeRun returns the current attempt's accumulated text plus a live
// event channel (nil when the run already ended). cancel unsubscribes.
func (b *runBroker) subscribeRun(runID string) (string, <-chan RunStreamEvent, func()) {
	b.mu.Lock()
	defer b.mu.Unlock()
	e := b.entries[runID]
	if e == nil {
		return "", nil, func() {}
	}
	sub := &brokerSubscriber{ch: make(chan RunStreamEvent, brokerSubscriberBuffer)}
	e.subs[sub] = struct{}{}
	text := string(e.text)
	cancel := func() {
		b.mu.Lock()
		defer b.mu.Unlock()
		if !sub.done {
			sub.done = true
			close(sub.ch)
		}
		delete(e.subs, sub)
	}
	// Replays race a concurrent finish/cancel: an entry that ended between
	// the map read and now still delivers its buffered events then closes.
	if e.closed {
		cancel()
		return text, nil, func() {}
	}
	return text, sub.ch, cancel
}

// SubscribeRunEvents is the HTTP-facing subscription: a late subscriber
// first receives reset + the accumulated delta, then live events, then done
// (channel close). The returned cancel unsubscribes; callers MUST call it.
func (s *Service) SubscribeRunEvents(runID string) (<-chan RunStreamEvent, func()) {
	out := make(chan RunStreamEvent, brokerSubscriberBuffer+2)
	text, live, cancel := s.broker.subscribeRun(runID)
	if text != "" {
		out <- RunStreamEvent{Kind: "reset"}
		out <- RunStreamEvent{Kind: "delta", Text: text}
	}
	if live == nil {
		out <- RunStreamEvent{Kind: "done"}
		close(out)
		return out, func() {}
	}
	go func() {
		defer close(out)
		defer cancel()
		for event := range live {
			select {
			case out <- event:
			default:
				// Reader gone or slow: stop relaying (polling still covers
				// the client); keep draining live so the goroutine exits.
				for range live {
				}
				return
			}
		}
	}()
	return out, cancel
}
