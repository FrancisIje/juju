package selfmon

import (
	"bytes"
	"context"
	"errors"
	"io"
	"log/slog"
	"net/http"
	"net/http/httptest"
	"strings"
	"sync"
	"sync/atomic"
	"testing"
	"time"

	"juju/internal/event"
	"juju/internal/fingerprint"
)

func quiet() *slog.Logger { return slog.New(slog.NewTextHandler(io.Discard, nil)) }

// sink stands in for the monitoring instance. It parses bodies with the real
// ingest functions, so a test fails if the reporter stops speaking the format
// the server accepts.
type sink struct {
	t      *testing.T
	srv    *httptest.Server
	mu     sync.Mutex
	events []*event.Event
	bodies []string
	keys   []string
	paths  []string
	calls  atomic.Int64
	status int
}

func newSink(t *testing.T) *sink {
	s := &sink{t: t, status: http.StatusAccepted}
	s.srv = httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		s.calls.Add(1)
		body, _ := io.ReadAll(r.Body)
		s.mu.Lock()
		defer s.mu.Unlock()
		s.bodies = append(s.bodies, string(body))
		s.keys = append(s.keys, r.Header.Get("X-Juju-Key"))
		s.paths = append(s.paths, r.URL.Path)
		env, err := event.ParseEnvelope(body)
		if err != nil {
			t.Errorf("the server would reject this envelope: %v", err)
			return
		}
		for _, raw := range env.Events {
			e, err := event.Normalize(raw, time.Now())
			if err != nil {
				t.Errorf("the server would reject this event: %v\n%s", err, raw)
				continue
			}
			s.events = append(s.events, e)
		}
		w.WriteHeader(s.status)
	}))
	t.Cleanup(s.srv.Close)
	return s
}

func (s *sink) dsn() string {
	return strings.Replace(s.srv.URL, "http://", "http://pk_test@", 1) + "/7"
}

func (s *sink) got() []*event.Event {
	s.mu.Lock()
	defer s.mu.Unlock()
	return append([]*event.Event(nil), s.events...)
}

func newMonitor(t *testing.T, s *sink, log *slog.Logger) *Monitor {
	t.Helper()
	m, err := New(Options{DSN: s.dsn(), PublicURL: "https://myjuju.dev", Log: log, FlushInterval: 5 * time.Millisecond})
	if err != nil {
		t.Fatal(err)
	}
	t.Cleanup(m.Close)
	return m
}

func TestBadDSNIsRefusedWithoutEchoingTheKey(t *testing.T) {
	_, err := New(Options{DSN: "https://pk_secret@host", Log: quiet()})
	if err == nil {
		t.Fatal("want an error")
	}
	if strings.Contains(err.Error(), "pk_secret") {
		t.Errorf("the error repeats the key: %v", err)
	}
}

// A server reporting to itself would add load during the incident it reports.
func TestRefusesToReportToItself(t *testing.T) {
	_, err := New(Options{DSN: "https://pk_x@MyJuju.dev/1", PublicURL: "https://myjuju.dev", Log: quiet()})
	if err == nil {
		t.Fatal("want an error")
	}
	m, err := New(Options{DSN: "https://pk_x@monitor.myjuju.dev/1", PublicURL: "https://myjuju.dev", Log: quiet()})
	if err != nil {
		t.Fatalf("a different host is fine: %v", err)
	}
	m.Close()
}

func TestErrorLogBecomesAnIssueEvent(t *testing.T) {
	s := newSink(t)
	m := newMonitor(t, s, quiet())
	log := slog.New(m.Handler(slog.NewTextHandler(io.Discard, nil)))

	log.Error("list issues", "err", errors.New("connection refused"), "email", "customer@example.com")
	log.Info("not an error", "err", errors.New("ignored"))
	log.Warn("not an error either")
	m.Close()

	got := s.got()
	if len(got) != 1 {
		t.Fatalf("want only the Error line reported, got %d events", len(got))
	}
	e := got[0]
	if e.Type != event.TypeError || e.Exception.Type != "list issues" || e.Exception.Message != "connection refused" {
		t.Errorf("unexpected event: %+v %+v", e, e.Exception)
	}
	if strings.Contains(s.bodies[0], "customer@example.com") {
		t.Error("a log attribute other than err reached the monitor")
	}
	if s.keys[0] != "pk_test" || s.paths[0] != "/api/v1/projects/7/events" {
		t.Errorf("sent to %s with key %q", s.paths[0], s.keys[0])
	}
	f := e.Exception.Stacktrace.Frames[0]
	if !strings.Contains(f.Function, "TestErrorLogBecomesAnIssueEvent") {
		t.Errorf("the top frame should be the code that logged, got %q", f.Function)
	}
}

func TestAttributesFromWithReachTheMessage(t *testing.T) {
	s := newSink(t)
	m := newMonitor(t, s, quiet())
	log := slog.New(m.Handler(slog.NewTextHandler(io.Discard, nil))).With("err", "from With")
	log.Error("boom")
	m.Close()
	if got := s.got(); len(got) != 1 || got[0].Exception.Message != "from With" {
		t.Fatalf("got %+v", got)
	}
}

// The same line of code failing with different details is one bug, and must
// be one issue, or an outage would raise an alert per request.
func TestSameCallSiteGroupsIntoOneIssue(t *testing.T) {
	s := newSink(t)
	m := newMonitor(t, s, quiet())
	log := slog.New(m.Handler(slog.NewTextHandler(io.Discard, nil)))
	for _, detail := range []string{"timeout after 3s on row 812", "broken pipe"} {
		log.Error("store event", "err", detail)
	}
	m.Close()
	got := s.got()
	if len(got) != 2 {
		t.Fatalf("got %d events", len(got))
	}
	if a, b := fingerprint.Compute(got[0]).Hash, fingerprint.Compute(got[1]).Hash; a != b {
		t.Error("the two failures got different fingerprints")
	}
}

// If reporting a failed send were itself reported, an unreachable monitor
// would keep the server sending forever.
func TestFailedSendIsNotReportedAgain(t *testing.T) {
	s := newSink(t)
	s.status = http.StatusInternalServerError
	var local bytes.Buffer
	var mu sync.Mutex
	base := slog.New(slog.NewTextHandler(lockedWriter{&mu, &local}, nil))
	m := newMonitor(t, s, base)
	log := slog.New(m.Handler(base.Handler()))

	log.Error("first failure", "err", "x")
	time.Sleep(60 * time.Millisecond) // many flush intervals
	m.Close()

	if n := s.calls.Load(); n != 1 {
		t.Errorf("want 1 request, got %d: the reporter is reporting on itself", n)
	}
	mu.Lock()
	defer mu.Unlock()
	if !strings.Contains(local.String(), "self-monitoring send failed") {
		t.Errorf("the failure should be in the local log:\n%s", local.String())
	}
}

type lockedWriter struct {
	mu *sync.Mutex
	w  io.Writer
}

func (l lockedWriter) Write(p []byte) (int, error) {
	l.mu.Lock()
	defer l.mu.Unlock()
	return l.w.Write(p)
}

// Capturing happens on request paths, so it must never wait for the network.
func TestCaptureNeverBlocksWhenTheBufferIsFull(t *testing.T) {
	s := newSink(t)
	m, err := New(Options{DSN: s.dsn(), PublicURL: "https://myjuju.dev", Log: quiet(), FlushInterval: time.Hour})
	if err != nil {
		t.Fatal(err)
	}
	defer m.Close()

	done := make(chan struct{})
	go func() {
		for range bufferSize + 25 {
			m.CaptureError("t", "m", nil, nil)
		}
		close(done)
	}()
	select {
	case <-done:
	case <-time.After(5 * time.Second):
		t.Fatal("capture blocked")
	}
	if n := m.dropped.Load(); n != 25 {
		t.Errorf("want 25 dropped, got %d", n)
	}
}

func TestRecoverReportsThePanicAndPassesItOn(t *testing.T) {
	s := newSink(t)
	m := newMonitor(t, s, quiet())
	mux := http.NewServeMux()
	mux.HandleFunc("GET /things/{id}", func(http.ResponseWriter, *http.Request) { panic("kaboom") })
	h := m.Recover(mux)

	func() {
		defer func() {
			if v := recover(); v != "kaboom" {
				t.Errorf("the panic should continue to net/http, got %v", v)
			}
		}()
		h.ServeHTTP(httptest.NewRecorder(), httptest.NewRequest("GET", "/things/secret-id", nil))
	}()
	m.Close()

	got := s.got()
	if len(got) != 1 {
		t.Fatalf("got %d events", len(got))
	}
	if got[0].Exception.Type != "panic" || got[0].Exception.Message != "kaboom" {
		t.Errorf("unexpected exception: %+v", got[0].Exception)
	}
	if got[0].Tags["route"] != "GET /things/{id}" {
		t.Errorf("route tag = %q", got[0].Tags["route"])
	}
	if strings.Contains(s.bodies[0], "secret-id") {
		t.Error("the raw URL reached the monitor")
	}
}

func TestRecoverIgnoresDeliberateAborts(t *testing.T) {
	s := newSink(t)
	m := newMonitor(t, s, quiet())
	h := m.Recover(http.HandlerFunc(func(http.ResponseWriter, *http.Request) { panic(http.ErrAbortHandler) }))
	func() {
		defer func() { _ = recover() }()
		h.ServeHTTP(httptest.NewRecorder(), httptest.NewRequest("GET", "/", nil))
	}()
	m.Close()
	if got := s.got(); len(got) != 0 {
		t.Errorf("an abort is not a bug, got %d events", len(got))
	}
}

func TestBacklogReportsOnceThenHourlyThenAgainAfterRecovery(t *testing.T) {
	s := newSink(t)
	m := newMonitor(t, s, quiet())
	var n atomic.Uint64
	w := newWatcher(m, []Backlog{{
		Type: "platform.queue_backlog", Label: "events waiting", Key: "queue_pending", Max: 100,
		Read: func(context.Context) (uint64, error) { return n.Load(), nil },
	}})
	ctx := context.Background()
	start := time.Date(2026, 11, 6, 12, 0, 0, 0, time.UTC)
	at := func(d time.Duration) time.Time { return start.Add(d) }

	n.Store(50)
	w.sample(ctx, at(0)) // under the limit
	n.Store(500)
	w.sample(ctx, at(time.Minute))    // reports
	w.sample(ctx, at(2*time.Minute))  // still high: quiet
	w.sample(ctx, at(61*time.Minute)) // an hour on: reports again
	n.Store(50)
	w.sample(ctx, at(62*time.Minute)) // recovered
	n.Store(500)
	w.sample(ctx, at(63*time.Minute)) // a new breach reports at once
	m.Close()

	errs := 0
	for _, e := range s.got() {
		if e.Type == event.TypeError {
			errs++
		}
	}
	if errs != 3 {
		t.Errorf("want 3 reports, got %d", errs)
	}
}

// With no stack, grouping uses the message with numbers masked, so a backlog
// of 500 and one of 900 are the same issue.
func TestBacklogMessagesGroupIntoOneIssue(t *testing.T) {
	a := event.Event{Exception: &event.Exception{Type: "platform.queue_backlog", Message: "500 events waiting, the limit is 100"}}
	b := event.Event{Exception: &event.Exception{Type: "platform.queue_backlog", Message: "900 events waiting, the limit is 100"}}
	if fingerprint.Compute(&a).Hash != fingerprint.Compute(&b).Hash {
		t.Error("different backlog sizes made different issues")
	}
}

func TestHeartbeatIsPeriodicAndSurvivesAFailedRead(t *testing.T) {
	s := newSink(t)
	m := newMonitor(t, s, quiet())
	w := newWatcher(m, []Backlog{
		{Type: "ok", Key: "ok_pending", Max: 10, Read: func(context.Context) (uint64, error) { return 3, nil }},
		{Type: "bad", Key: "bad_pending", Max: 10, Read: func(context.Context) (uint64, error) { return 0, errors.New("no") }},
	})
	start := time.Date(2026, 11, 6, 12, 0, 0, 0, time.UTC)
	for i := range 11 { // 12:00 to 12:10, a sample a minute
		w.sample(context.Background(), start.Add(time.Duration(i)*time.Minute))
	}
	m.Close()

	got := s.got()
	if len(got) != 3 { // 12:00, 12:05, 12:10
		t.Fatalf("want 3 heartbeats, got %d", len(got))
	}
	if got[0].Name != "platform.heartbeat" || got[0].Properties["ok_pending"] != "3" {
		t.Errorf("unexpected heartbeat: %+v", got[0])
	}
	if _, ok := got[0].Properties["bad_pending"]; ok {
		t.Error("a failed read shouldn't report a value")
	}
}

// Everything is called unconditionally by the server, reporting on or off.
func TestNilMonitorDoesNothing(t *testing.T) {
	var m *Monitor
	m.CaptureError("t", "m", nil, nil)
	m.Capture("e", nil)
	m.Close()
	m.Watch(context.Background(), nil)
	inner := slog.NewTextHandler(io.Discard, nil)
	if m.Handler(inner) != slog.Handler(inner) {
		t.Error("Handler should return the handler it was given")
	}
	h := http.NotFoundHandler()
	rec := httptest.NewRecorder()
	m.Recover(h).ServeHTTP(rec, httptest.NewRequest("GET", "/", nil))
	if rec.Code != http.StatusNotFound {
		t.Errorf("code = %d", rec.Code)
	}
}
