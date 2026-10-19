package worker

import (
	"context"
	"errors"
	"io"
	"log/slog"
	"sync"
	"testing"

	"juju/internal/event"
	"juju/internal/fingerprint"
)

type fakeStore struct {
	mu       sync.Mutex
	saved    []string
	failures int // fail this many calls before succeeding
	calls    int
}

func (f *fakeStore) SaveEvent(_ context.Context, e *event.Event, _ fingerprint.Result) (bool, error) {
	f.mu.Lock()
	defer f.mu.Unlock()
	f.calls++
	if e.EventID == "panic" {
		panic("boom")
	}
	if f.failures > 0 {
		f.failures--
		return false, errors.New("database is down")
	}
	f.saved = append(f.saved, e.EventID)
	return true, nil
}

var quiet = slog.New(slog.NewTextHandler(io.Discard, nil))

func runAll(store Store, ids ...string) {
	ch := make(chan *event.Event, len(ids))
	for _, id := range ids {
		ch <- &event.Event{EventID: id, Exception: &event.Exception{Type: "E"}}
	}
	close(ch)
	Run(context.Background(), ch, store, quiet, 3)
}

func TestRunDrainsEverything(t *testing.T) {
	s := &fakeStore{}
	runAll(s, "a", "b", "c", "d", "e")
	if len(s.saved) != 5 {
		t.Fatalf("saved %d events, want 5: Run must not return before the channel is drained", len(s.saved))
	}
}

func TestPanicDoesNotStopWorkers(t *testing.T) {
	s := &fakeStore{}
	runAll(s, "panic", "a", "b")
	if len(s.saved) != 2 {
		t.Fatalf("saved %v; a panic on one event must not lose the others", s.saved)
	}
}

func TestRetriesTransientErrors(t *testing.T) {
	s := &fakeStore{failures: 2}
	runAll(s, "a")
	if len(s.saved) != 1 || s.calls != 3 {
		t.Fatalf("saved %v after %d calls; want success on the 3rd attempt", s.saved, s.calls)
	}
}

func TestGivesUpAfterRetries(t *testing.T) {
	s := &fakeStore{failures: 10}
	runAll(s, "a")
	if len(s.saved) != 0 || s.calls != attempts {
		t.Fatalf("calls = %d, want exactly %d attempts", s.calls, attempts)
	}
}
