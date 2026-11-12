package chatbot

import (
	"testing"
	"time"
)

func TestBrokerLateSubscriberGetsAccumulatedText(t *testing.T) {
	var b runBroker
	b.entry("r1")
	b.publish("r1", "delta", "Hello ")
	b.publish("r1", "delta", "world")
	text, ch, cancel := b.subscribeRun("r1")
	defer cancel()
	if text != "Hello world" {
		t.Fatalf("accumulated = %q", text)
	}
	b.publish("r1", "delta", "more")
	select {
	case e := <-ch:
		if e.Kind != "delta" || e.Text != "more" {
			t.Fatalf("live event = %+v", e)
		}
	case <-time.After(time.Second):
		t.Fatal("no live delta")
	}
	b.finish("r1")
	select {
	case e := <-ch:
		if e.Kind != "done" {
			t.Fatalf("expected done, got %+v", e)
		}
	case <-time.After(time.Second):
		t.Fatal("no done event")
	}
	if _, still := <-ch; still {
		t.Fatal("channel must close after done")
	}
	// After finish the entry is gone: new subscribers read done immediately.
	if _, ch2, _ := b.subscribeRun("r1"); ch2 != nil {
		t.Fatal("finished run must have no live channel")
	}
}

func TestBrokerResetClearsAccumulatedText(t *testing.T) {
	var b runBroker
	b.entry("r2")
	b.publish("r2", "delta", "tool attempt text")
	b.publish("r2", "reset", "")
	text, _, cancel := b.subscribeRun("r2")
	defer cancel()
	if text != "" {
		t.Fatalf("accumulated after reset = %q", text)
	}
}

func TestBrokerSlowSubscriberDoesNotBlock(t *testing.T) {
	var b runBroker
	b.entry("r3")
	// Subscribe but never read: the worker's publishes must not block.
	_, _, cancel := b.subscribeRun("r3")
	defer cancel()
	done := make(chan struct{})
	go func() {
		defer close(done)
		// Far more events than any subscriber buffer.
		for range brokerSubscriberBuffer * 10 {
			b.publish("r3", "delta", "x")
		}
	}()
	select {
	case <-done:
	case <-time.After(2 * time.Second):
		t.Fatal("publish blocked on a slow subscriber")
	}
}

func TestSubscribeRunEventsNoEntryIsDone(t *testing.T) {
	s := &Service{}
	events, cancel := s.SubscribeRunEvents("nope")
	defer cancel()
	e := <-events
	if e.Kind != "done" {
		t.Fatalf("expected done, got %+v", e)
	}
	if _, open := <-events; open {
		t.Fatal("channel must close after done")
	}
}
