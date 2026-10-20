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
	Run(context.Background(), ch, store, nil, quiet, 3)
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

type renamer struct{}

// Symbolicate stands in for the real thing: it renames the top frame.
func (renamer) Symbolicate(_ context.Context, e *event.Event) {
	e.Exception.Stacktrace.Frames[0].Function = "lineTotal"
}

type fpStore struct{ culprit string }

func (s *fpStore) SaveEvent(_ context.Context, _ *event.Event, fp fingerprint.Result) (bool, error) {
	s.culprit = fp.Culprit
	return true, nil
}

// Grouping must use the symbolicated frames. If the fingerprint were
// computed first, every deploy (new minified names) would start new issues.
func TestSymbolicatesBeforeFingerprinting(t *testing.T) {
	ch := make(chan *event.Event, 1)
	ch <- &event.Event{Exception: &event.Exception{Type: "TypeError", Stacktrace: &event.Stacktrace{Frames: []event.Frame{{Function: "o", Filename: "app.js", InApp: true}}}}}
	close(ch)
	s := &fpStore{}
	Run(context.Background(), ch, s, renamer{}, quiet, 1)
	if s.culprit != "lineTotal (app.js)" {
		t.Fatalf("culprit = %q: fingerprinted before symbolication", s.culprit)
	}
}
