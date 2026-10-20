package worker

import (
	"context"
	"errors"
	"io"
	"log/slog"
	"strings"
	"sync"
	"testing"

	"juju/internal/event"
	"juju/internal/fingerprint"
	"juju/internal/store"
)

type fakeStore struct {
	mu       sync.Mutex
	saved    []string
	failures int // fail this many calls before succeeding
	calls    int
}

func (f *fakeStore) SaveEvent(_ context.Context, e *event.Event, _ fingerprint.Result) (store.SaveResult, error) {
	f.mu.Lock()
	defer f.mu.Unlock()
	f.calls++
	if e.EventID == "panic" {
		panic("boom")
	}
	if f.failures > 0 {
		f.failures--
		return store.SaveResult{}, errors.New("database is down")
	}
	f.saved = append(f.saved, e.EventID)
	return store.SaveResult{Inserted: true, IssueID: 1, NewIssue: e.EventID == "new", Regressed: e.EventID == "regressed"}, nil
}

var quiet = slog.New(slog.NewTextHandler(io.Discard, nil))

func runAll(s Store, ids ...string) {
	ch := make(chan *event.Event, len(ids))
	for _, id := range ids {
		ch <- &event.Event{EventID: id, Exception: &event.Exception{Type: "E"}}
	}
	close(ch)
	Run(context.Background(), ch, Deps{Store: s, Log: quiet}, 3)
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

func (s *fpStore) SaveEvent(_ context.Context, _ *event.Event, fp fingerprint.Result) (store.SaveResult, error) {
	s.culprit = fp.Culprit
	return store.SaveResult{Inserted: true}, nil
}

// Grouping must use the symbolicated frames. If the fingerprint were
// computed first, every deploy (new minified names) would start new issues.
func TestSymbolicatesBeforeFingerprinting(t *testing.T) {
	ch := make(chan *event.Event, 1)
	ch <- &event.Event{Exception: &event.Exception{Type: "TypeError", Stacktrace: &event.Stacktrace{Frames: []event.Frame{{Function: "o", Filename: "app.js", InApp: true}}}}}
	close(ch)
	s := &fpStore{}
	Run(context.Background(), ch, Deps{Store: s, Symbolicator: renamer{}, Log: quiet}, 1)
	if s.culprit != "lineTotal (app.js)" {
		t.Fatalf("culprit = %q: fingerprinted before symbolication", s.culprit)
	}
}

type recordingAlerter struct {
	mu    sync.Mutex
	kinds []string
}

func (a *recordingAlerter) IssueEvent(e *event.Event, _ fingerprint.Result, res store.SaveResult) {
	a.mu.Lock()
	defer a.mu.Unlock()
	switch {
	case res.Regressed:
		a.kinds = append(a.kinds, "regressed:"+e.EventID)
	case res.NewIssue:
		a.kinds = append(a.kinds, "new:"+e.EventID)
	}
}

// Only new issues and regressions are announced; an ordinary repeat of a
// known issue must not alert anyone.
func TestAlertsOnlyForNewIssuesAndRegressions(t *testing.T) {
	ch := make(chan *event.Event, 3)
	for _, id := range []string{"new", "repeat", "regressed"} {
		ch <- &event.Event{EventID: id, Exception: &event.Exception{Type: "E"}}
	}
	close(ch)
	a := &recordingAlerter{}
	Run(context.Background(), ch, Deps{Store: &fakeStore{}, Alerter: a, Log: quiet}, 1)
	if strings.Join(a.kinds, ",") != "new:new,regressed:regressed" {
		t.Fatalf("alerts = %v", a.kinds)
	}
}

type dupStore struct{}

// "dup" is a retried send of an event already stored.
func (dupStore) SaveEvent(_ context.Context, e *event.Event, _ fingerprint.Result) (store.SaveResult, error) {
	return store.SaveResult{Inserted: e.EventID != "dup", IssueID: 1}, nil
}

type recordingLive struct {
	mu       sync.Mutex
	projects []int64
}

func (l *recordingLive) Changed(id int64) {
	l.mu.Lock()
	defer l.mu.Unlock()
	l.projects = append(l.projects, id)
}

// Every stored event refreshes open dashboards for its project; a retried
// duplicate changed nothing, so it must not.
func TestLiveRefreshOnlyForStoredEvents(t *testing.T) {
	ch := make(chan *event.Event, 3)
	ch <- &event.Event{EventID: "a", ProjectID: 4, Exception: &event.Exception{Type: "E"}}
	ch <- &event.Event{EventID: "dup", ProjectID: 4, Exception: &event.Exception{Type: "E"}}
	ch <- &event.Event{EventID: "b", ProjectID: 9, Exception: &event.Exception{Type: "E"}}
	close(ch)
	l := &recordingLive{}
	Run(context.Background(), ch, Deps{Store: dupStore{}, Live: l, Log: quiet}, 1)
	if len(l.projects) != 2 || l.projects[0] != 4 || l.projects[1] != 9 {
		t.Fatalf("refreshes = %v, want [4 9]", l.projects)
	}
}
