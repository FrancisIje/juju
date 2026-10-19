package queue

import (
	"testing"

	"juju/internal/event"
)

func TestChanRejectsWhenFull(t *testing.T) {
	q := NewChan(2)
	for i := range 2 {
		if !q.TryEnqueue(&event.Event{}) {
			t.Fatalf("enqueue %d failed with room left", i)
		}
	}
	if q.TryEnqueue(&event.Event{}) {
		t.Fatal("enqueue succeeded on a full queue; it must return false, not block")
	}
	<-q.Events()
	if !q.TryEnqueue(&event.Event{}) {
		t.Fatal("enqueue failed after a slot was freed")
	}
}

func TestChanCloseDrains(t *testing.T) {
	q := NewChan(3)
	q.TryEnqueue(&event.Event{EventID: "a"})
	q.TryEnqueue(&event.Event{EventID: "b"})
	q.Close()

	var got []string
	for e := range q.Events() { // ends after the buffered events, because the channel is closed
		got = append(got, e.EventID)
	}
	if len(got) != 2 || got[0] != "a" || got[1] != "b" {
		t.Fatalf("drained %v, want [a b] in order", got)
	}
}
