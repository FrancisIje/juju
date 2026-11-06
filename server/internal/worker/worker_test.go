package worker

import (
	"context"
	"errors"
	"fmt"
	"io"
	"log/slog"
	"strings"
	"sync"
	"testing"
	"time"

	"juju/internal/event"
	"juju/internal/fingerprint"
	"juju/internal/queue"
	"juju/internal/store"
)

// noPerf gives the error-only fakes below the session and transaction
// methods the Store interface needs.
type noPerf struct{}

func (noPerf) SaveSession(context.Context, *event.Event) (bool, error)     { return true, nil }
func (noPerf) SaveTransaction(context.Context, *event.Event) (bool, error) { return true, nil }

type fakeStore struct {
	noPerf
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

func runAll(s Store, ids ...string) *testSource {
	var evs []*event.Event
	for _, id := range ids {
		evs = append(evs, &event.Event{EventID: id, Exception: &event.Exception{Type: "E"}})
	}
	q := src(evs...)
	Run(context.Background(), q, Deps{Store: s, Log: quiet}, 3)
	return q
}

// testSource is a queue that records what the worker did with each event:
// acknowledged, retried (and after what delay) or set aside. Retries come
// back at once, so tests don't wait. It closes when every event is done.
type testSource struct {
	ch     chan queue.Delivery
	mu     sync.Mutex
	open   int
	acked  []string
	dead   []string
	delays []time.Duration
}

func src(events ...*event.Event) *testSource {
	q := &testSource{ch: make(chan queue.Delivery, len(events)+1), open: len(events)}
	for _, e := range events {
		q.ch <- &testDelivery{q: q, e: e, attempt: 1}
	}
	if len(events) == 0 {
		close(q.ch)
	}
	return q
}

func (q *testSource) Deliveries() <-chan queue.Delivery { return q.ch }

func (q *testSource) done() {
	q.open--
	if q.open == 0 {
		close(q.ch)
	}
}

type testDelivery struct {
	q       *testSource
	e       *event.Event
	attempt int
}

func (d *testDelivery) Event() *event.Event { return d.e }
func (d *testDelivery) Attempt() int        { return d.attempt }
func (d *testDelivery) Ack() {
	d.q.mu.Lock()
	defer d.q.mu.Unlock()
	d.q.acked = append(d.q.acked, d.e.EventID)
	d.q.done()
}
func (d *testDelivery) Retry(after time.Duration) {
	d.q.mu.Lock()
	defer d.q.mu.Unlock()
	d.q.delays = append(d.q.delays, after)
	d.q.ch <- &testDelivery{q: d.q, e: d.e, attempt: d.attempt + 1}
}
func (d *testDelivery) Dead(reason string) {
	d.q.mu.Lock()
	defer d.q.mu.Unlock()
	d.q.dead = append(d.q.dead, d.e.EventID+": "+reason)
	d.q.done()
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
	q := runAll(s, "panic", "a", "b")
	if len(s.saved) != 2 {
		t.Fatalf("saved %v; a panic on one event must not lose the others", s.saved)
	}
	// The event that panics every time ends as a dead letter, not lost.
	if len(q.dead) != 1 || !strings.HasPrefix(q.dead[0], "panic: panic: boom") {
		t.Errorf("dead = %v", q.dead)
	}
}

// A failure is retried later through the queue, with growing delays, and
// acknowledged only once it's stored.
func TestRetriesTransientErrors(t *testing.T) {
	s := &fakeStore{failures: 2}
	q := runAll(s, "a")
	if len(s.saved) != 1 || s.calls != 3 {
		t.Fatalf("saved %v after %d calls; want success on the 3rd attempt", s.saved, s.calls)
	}
	if fmt.Sprint(q.delays) != "[1s 5s]" || fmt.Sprint(q.acked) != "[a]" {
		t.Errorf("delays %v, acked %v", q.delays, q.acked)
	}
}

// An event that keeps failing isn't retried forever, and isn't silently
// dropped either: after MaxAttempts it's set aside as a dead letter.
func TestGivesUpAfterMaxAttempts(t *testing.T) {
	s := &fakeStore{failures: MaxAttempts + 10}
	q := runAll(s, "a")
	if len(s.saved) != 0 || s.calls != MaxAttempts {
		t.Fatalf("calls = %d, want exactly %d attempts", s.calls, MaxAttempts)
	}
	if len(q.dead) != 1 || !strings.Contains(q.dead[0], "database is down") || len(q.acked) != 0 {
		t.Fatalf("dead %v, acked %v", q.dead, q.acked)
	}
	if q.delays[len(q.delays)-1] != time.Minute {
		t.Errorf("last retry delay %v, want 1m", q.delays[len(q.delays)-1])
	}
}

type renamer struct{}

// Symbolicate stands in for the real thing: it renames the top frame.
func (renamer) Symbolicate(_ context.Context, e *event.Event) {
	e.Exception.Stacktrace.Frames[0].Function = "lineTotal"
}

type fpStore struct {
	noPerf
	culprit string
}

func (s *fpStore) SaveEvent(_ context.Context, _ *event.Event, fp fingerprint.Result) (store.SaveResult, error) {
	s.culprit = fp.Culprit
	return store.SaveResult{Inserted: true}, nil
}

// Grouping must use the symbolicated frames. If the fingerprint were
// computed first, every deploy (new minified names) would start new issues.
func TestSymbolicatesBeforeFingerprinting(t *testing.T) {
	ch := src(
		&event.Event{Exception: &event.Exception{Type: "TypeError", Stacktrace: &event.Stacktrace{Frames: []event.Frame{{Function: "o", Filename: "app.js", InApp: true}}}}},
	)
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
	var evs []*event.Event
	for _, id := range []string{"new", "repeat", "regressed"} {
		evs = append(evs, &event.Event{EventID: id, Exception: &event.Exception{Type: "E"}})
	}
	ch := src(evs...)
	a := &recordingAlerter{}
	Run(context.Background(), ch, Deps{Store: &fakeStore{}, Alerter: a, Log: quiet}, 1)
	if strings.Join(a.kinds, ",") != "new:new,regressed:regressed" {
		t.Fatalf("alerts = %v", a.kinds)
	}
}

type dupStore struct{ noPerf }

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
	ch := src(
		&event.Event{EventID: "a", ProjectID: 4, Exception: &event.Exception{Type: "E"}},
		&event.Event{EventID: "dup", ProjectID: 4, Exception: &event.Exception{Type: "E"}},
		&event.Event{EventID: "b", ProjectID: 9, Exception: &event.Exception{Type: "E"}},
	)
	l := &recordingLive{}
	Run(context.Background(), ch, Deps{Store: dupStore{}, Live: l, Log: quiet}, 1)
	if len(l.projects) != 2 || l.projects[0] != 4 || l.projects[1] != 9 {
		t.Fatalf("refreshes = %v, want [4 9]", l.projects)
	}
}

// perfStore records which method each event reached.
type perfStore struct {
	mu       sync.Mutex
	calls    []string
	failures int
}

func (p *perfStore) record(kind string) (bool, error) {
	p.mu.Lock()
	defer p.mu.Unlock()
	if p.failures > 0 {
		p.failures--
		return false, errors.New("database is down")
	}
	p.calls = append(p.calls, kind)
	return true, nil
}

func (p *perfStore) SaveEvent(context.Context, *event.Event, fingerprint.Result) (store.SaveResult, error) {
	_, err := p.record("error")
	return store.SaveResult{Inserted: true, NewIssue: true}, err
}
func (p *perfStore) SaveSession(context.Context, *event.Event) (bool, error) {
	return p.record("session")
}
func (p *perfStore) SaveTransaction(context.Context, *event.Event) (bool, error) {
	return p.record("transaction")
}

type panicSymbolicator struct{}

func (panicSymbolicator) Symbolicate(context.Context, *event.Event) {
	panic("sessions have no stack trace")
}

// Sessions and transactions have their own tables. They must never be
// symbolicated, fingerprinted into issues, alerted on or pushed live: a
// session update per app launch would otherwise open an issue per launch.
func TestSessionsAndTransactionsSkipTheIssuePipeline(t *testing.T) {
	ch := src(
		&event.Event{EventID: "s", Type: event.TypeSession, Session: &event.Session{}},
		&event.Event{EventID: "t", Type: event.TypeTransaction, Transaction: &event.Transaction{}},
		&event.Event{EventID: "e", Type: event.TypeError, Exception: &event.Exception{Type: "E"}},
	)
	s, a, l := &perfStore{}, &recordingAlerter{}, &recordingLive{}
	Run(context.Background(), ch, Deps{Store: s, Alerter: a, Live: l, Log: quiet}, 1)
	if strings.Join(s.calls, ",") != "session,transaction,error" {
		t.Fatalf("calls = %v", s.calls)
	}
	if len(l.projects) != 1 {
		t.Errorf("live refreshes = %d, want 1 (the error only)", len(l.projects))
	}
	if strings.Join(a.kinds, ",") != "new:e" {
		t.Errorf("alerts = %v, want only the error's", a.kinds)
	}

	// And the symbolicator is never even called for them.
	ch = src(
		&event.Event{EventID: "s2", Type: event.TypeSession, Session: &event.Session{}},
	)
	s = &perfStore{}
	Run(context.Background(), ch, Deps{Store: s, Symbolicator: panicSymbolicator{}, Log: quiet}, 1)
	if len(s.calls) != 1 {
		t.Fatalf("session not stored: %v", s.calls)
	}
}

func TestSessionWritesAreRetried(t *testing.T) {
	ch := src(
		&event.Event{EventID: "s", Type: event.TypeSession, Session: &event.Session{}},
	)
	s := &perfStore{failures: 2}
	Run(context.Background(), ch, Deps{Store: s, Log: quiet}, 1)
	if len(s.calls) != 1 {
		t.Fatalf("calls = %v, want success on the 3rd attempt", s.calls)
	}
}

type recordingAnalytics struct {
	mu    sync.Mutex
	names []string
	err   error
}

func (a *recordingAnalytics) Publish(_ context.Context, events []*event.Event) error {
	a.mu.Lock()
	defer a.mu.Unlock()
	if a.err != nil {
		return a.err
	}
	for _, e := range events {
		a.names = append(a.names, e.Name)
	}
	return nil
}

// An analytics event left in the main stream (queued before the upgrade) is
// acknowledged only once it's safely in its own stream.
func TestLeftoverAnalyticsEventRetriedIfItCantBeMoved(t *testing.T) {
	ch := queue.NewChan(4)
	if err := ch.Publish(context.Background(), []*event.Event{{EventID: "a", Type: event.TypeEvent, Name: "signed_up", DistinctID: "u"}}); err != nil {
		t.Fatal(err)
	}
	dl := <-ch.Deliveries()
	rec := &recordingAnalytics{err: errors.New("queue unavailable")}
	handle(context.Background(), dl, Deps{Store: &fakeStore{}, Analytics: rec, Log: quiet})
	again := <-ch.Deliveries() // back from Retry, after its delay
	if again.Attempt() != 2 {
		t.Errorf("attempt = %d, want 2", again.Attempt())
	}
}

// Analytics events go to ClickHouse, never to issues: no fingerprint, no
// alert, no row in Postgres. With analytics off they're dropped, still
// without touching issues.
func TestAnalyticsEventsSkipIssues(t *testing.T) {
	for _, on := range []bool{true, false} {
		ch := src(
			&event.Event{EventID: "a", Type: event.TypeEvent, Name: "signed_up", DistinctID: "u"},
			&event.Event{EventID: "b", Type: event.TypeIdentify, Name: "$identify", DistinctID: "u", AnonID: "x"},
			&event.Event{EventID: "err", Type: event.TypeError, Exception: &event.Exception{Type: "E"}},
		)
		s := &fakeStore{}
		alerts := &recordingAlerter{}
		deps := Deps{Store: s, Alerter: alerts, Log: quiet}
		rec := &recordingAnalytics{}
		if on {
			deps.Analytics = rec
		}
		Run(context.Background(), ch, deps, 1)
		if strings.Join(s.saved, ",") != "err" {
			t.Errorf("analytics on=%v: issues got %v, want only the error", on, s.saved)
		}
		if on && strings.Join(rec.names, ",") != "signed_up,$identify" {
			t.Errorf("analytics got %v", rec.names)
		}
	}
}
