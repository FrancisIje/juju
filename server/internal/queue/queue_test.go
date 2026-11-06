package queue

import (
	"context"
	"errors"
	"fmt"
	"testing"
	"time"

	"juju/internal/event"
)

func ev(id string) *event.Event {
	return &event.Event{EventID: id, Type: event.TypeError, ProjectID: 7, Timestamp: "2026-10-21T10:00:00Z", Exception: &event.Exception{Type: "E", Message: id}}
}

func TestChanFullAndRetry(t *testing.T) {
	q := NewChan(2)
	if err := q.Publish(context.Background(), []*event.Event{ev("a"), ev("b"), ev("c")}); !errors.Is(err, ErrFull) {
		t.Fatalf("err = %v, want ErrFull", err)
	}
	d := <-q.Deliveries()
	d.Retry(10 * time.Millisecond)
	<-q.Deliveries() // b
	again := <-q.Deliveries()
	if again.Event().EventID != "a" || again.Attempt() != 2 {
		t.Fatalf("retried %s attempt %d", again.Event().EventID, again.Attempt())
	}
}

// openJS starts an embedded JetStream in dir, as juju serve does.
func openJS(t *testing.T, dir string, maxBytes int64) *JetStream {
	t.Helper()
	q, err := OpenJetStream(context.Background(), JetStreamConfig{Dir: dir, MaxBytes: maxBytes, AckWait: time.Second, Fetch: 8})
	if err != nil {
		t.Fatal(err)
	}
	return q
}

func next(t *testing.T, q *JetStream) Delivery {
	t.Helper()
	select {
	case d, ok := <-q.Deliveries():
		if !ok {
			t.Fatal("deliveries closed")
		}
		return d
	case <-time.After(10 * time.Second):
		t.Fatal("no delivery within 10s")
		return nil
	}
}

func none(t *testing.T, q *JetStream, wait time.Duration) {
	t.Helper()
	select {
	case d := <-q.Deliveries():
		t.Fatalf("unexpected delivery of %s (attempt %d)", d.Event().EventID, d.Attempt())
	case <-time.After(wait):
	}
}

func TestJetStreamDeliversOnceAndDropsDuplicates(t *testing.T) {
	q := openJS(t, t.TempDir(), 0)
	defer q.Close()
	ctx := context.Background()
	if err := q.Publish(ctx, []*event.Event{ev("11111111-0000-4000-8000-000000000001")}); err != nil {
		t.Fatal(err)
	}
	// An SDK retrying after a lost response sends the same event again.
	if err := q.Publish(ctx, []*event.Event{ev("11111111-0000-4000-8000-000000000001")}); err != nil {
		t.Fatal(err)
	}
	d := next(t, q)
	if d.Event().ProjectID != 7 || d.Event().Time.IsZero() || d.Event().Exception == nil {
		t.Fatalf("event didn't survive the trip: %+v", d.Event())
	}
	d.Ack()
	none(t, q, 1500*time.Millisecond) // the duplicate was dropped, and the ack holds past AckWait
	if n, _ := q.Pending(ctx); n != 0 {
		t.Errorf("%d events left after the ack, want 0", n)
	}
}

// The heart of Phase 9: an event the worker took but never finished (the
// process was killed) is delivered again after a restart.
func TestJetStreamUnackedEventsSurviveARestart(t *testing.T) {
	dir := t.TempDir()
	q := openJS(t, dir, 0)
	ctx := context.Background()
	var batch []*event.Event
	for i := range 5 {
		batch = append(batch, ev(fmt.Sprintf("22222222-0000-4000-8000-%012d", i)))
	}
	if err := q.Publish(ctx, batch); err != nil {
		t.Fatal(err)
	}
	next(t, q).Ack() // one stored before the "crash"
	next(t, q)       // one taken but never acknowledged
	q.StopDeliveries()
	q.Close()

	q2 := openJS(t, dir, 0)
	defer q2.Close()
	seen := map[string]bool{}
	for range 4 {
		d := next(t, q2)
		seen[d.Event().EventID] = true
		d.Ack()
	}
	if len(seen) != 4 {
		t.Fatalf("after the restart got %d distinct events, want the 4 not yet stored", len(seen))
	}
	none(t, q2, 1500*time.Millisecond)
}

func TestJetStreamRetryAndDeadLetter(t *testing.T) {
	q := openJS(t, t.TempDir(), 0)
	defer q.Close()
	ctx := context.Background()
	_ = q.Publish(ctx, []*event.Event{ev("33333333-0000-4000-8000-000000000001")})
	d := next(t, q)
	d.Retry(200 * time.Millisecond)
	d2 := next(t, q)
	if d2.Attempt() != 2 {
		t.Fatalf("attempt = %d, want 2", d2.Attempt())
	}
	d2.Dead("database down too long")
	none(t, q, 1500*time.Millisecond)
	s, _ := q.js.Stream(ctx, deadStream)
	info, _ := s.Info(ctx)
	if info.State.Msgs != 1 {
		t.Fatalf("dead letters = %d, want 1 (given up on, but kept)", info.State.Msgs)
	}
	m, err := s.GetLastMsgForSubject(ctx, "juju.dead.7")
	if err != nil || m.Header.Get("Juju-Dead-Reason") != "database down too long" {
		t.Fatalf("dead letter: %v %v", m, err)
	}
}

func TestJetStreamFullMeans429(t *testing.T) {
	q := openJS(t, t.TempDir(), 4<<10) // 4 KB: a handful of events
	defer q.Close()
	var err error
	for i := 0; i < 100 && err == nil; i++ {
		err = q.Publish(context.Background(), []*event.Event{ev(fmt.Sprintf("44444444-0000-4000-8000-%012d", i))})
	}
	if !errors.Is(err, ErrFull) {
		t.Fatalf("filling the queue: err = %v, want ErrFull", err)
	}
}

func openAnalytics(t *testing.T, dir string, maxBytes int64) *JetStream {
	t.Helper()
	q, err := OpenJetStream(context.Background(), JetStreamConfig{Dir: dir, MaxBytes: 1 << 20, AckWait: time.Second, Fetch: 8, Analytics: true, AnalyticsMaxBytes: maxBytes, AnalyticsFetch: 8, AnalyticsAckWait: time.Second})
	if err != nil {
		t.Fatal(err)
	}
	return q
}

func analyticsEv(id string) *event.Event {
	return &event.Event{EventID: id, Type: event.TypeEvent, Name: "tap", DistinctID: "u", ProjectID: 7, Timestamp: "2026-10-21T10:00:00Z"}
}

func nextOf(t *testing.T, s Source) Delivery {
	t.Helper()
	select {
	case d := <-s.Deliveries():
		return d
	case <-time.After(10 * time.Second):
		t.Fatal("no delivery within 10s")
		return nil
	}
}

// Clicks and errors never share a stream, so an outage's worth of clicks can't
// take the space errors need.
func TestAnalyticsHasItsOwnStream(t *testing.T) {
	q := openAnalytics(t, t.TempDir(), 1<<20)
	defer q.Close()
	if err := q.Publish(context.Background(), []*event.Event{ev("err"), analyticsEv("click")}); err != nil {
		t.Fatal(err)
	}
	if got := next(t, q).Event().EventID; got != "err" {
		t.Errorf("main stream got %s", got)
	}
	if got := nextOf(t, q.Analytics()).Event().EventID; got != "click" {
		t.Errorf("analytics stream got %s", got)
	}
	none(t, q, 200*time.Millisecond)
}

// The crash case: taken from the queue, never acknowledged, process gone.
func TestUnacknowledgedAnalyticsEventSurvivesARestart(t *testing.T) {
	dir := t.TempDir()
	q := openAnalytics(t, dir, 1<<20)
	if err := q.Publish(context.Background(), []*event.Event{analyticsEv("click")}); err != nil {
		t.Fatal(err)
	}
	nextOf(t, q.Analytics()) // in the writer's hands, not acknowledged
	q.Close()

	q = openAnalytics(t, dir, 1<<20)
	defer q.Close()
	d := nextOf(t, q.Analytics())
	if d.Event().EventID != "click" {
		t.Fatalf("got %s", d.Event().EventID)
	}
	if d.Event().Name != "tap" || d.Event().DistinctID != "u" || d.Event().ProjectID != 7 {
		t.Errorf("event changed on the way: %+v", d.Event())
	}
	d.Ack()
	waitPending(t, q, 0)
}

func waitPending(t *testing.T, q *JetStream, want uint64) {
	t.Helper()
	deadline := time.Now().Add(5 * time.Second)
	for {
		n, err := q.AnalyticsPending(context.Background())
		if err == nil && n == want {
			return
		}
		if time.Now().After(deadline) {
			t.Fatalf("analytics pending = %d (err %v), want %d", n, err, want)
		}
		time.Sleep(20 * time.Millisecond)
	}
}

// Full: the oldest clicks go. Ingest keeps answering, and errors are untouched.
func TestFullAnalyticsStreamDropsOldestAndNeverRefuses(t *testing.T) {
	q := openAnalytics(t, t.TempDir(), 16<<10)
	defer q.Close()
	const sent = 400
	for i := range sent {
		if err := q.Publish(context.Background(), []*event.Event{analyticsEv(fmt.Sprintf("click-%d", i))}); err != nil {
			t.Fatalf("publish %d refused: %v", i, err)
		}
	}
	n, err := q.AnalyticsPending(context.Background())
	if err != nil {
		t.Fatal(err)
	}
	if n == 0 || n >= sent {
		t.Fatalf("kept %d of %d: the limit should have dropped some and kept some", n, sent)
	}
	first := nextOf(t, q.Analytics()).Event().EventID
	if first == "click-0" {
		t.Errorf("the oldest should be gone, got %s first", first)
	}
	if err := q.Publish(context.Background(), []*event.Event{ev("err")}); err != nil {
		t.Fatal(err)
	}
	if got := next(t, q).Event().EventID; got != "err" {
		t.Errorf("error event got lost: %s", got)
	}
}

// A retry from an SDK within the window is dropped, as in the main stream.
func TestAnalyticsDropsADuplicatePublish(t *testing.T) {
	q := openAnalytics(t, t.TempDir(), 1<<20)
	defer q.Close()
	for range 2 {
		if err := q.Publish(context.Background(), []*event.Event{analyticsEv("click")}); err != nil {
			t.Fatal(err)
		}
	}
	waitPending(t, q, 1)
}

// With analytics off (no ClickHouse) nothing piles up unread: they stay in the
// main stream, and turning analytics on later finds them there to move.
func TestAnalyticsOffUsesTheMainStream(t *testing.T) {
	dir := t.TempDir()
	off := openJS(t, dir, 1<<20)
	if err := off.Publish(context.Background(), []*event.Event{analyticsEv("click")}); err != nil {
		t.Fatal(err)
	}
	off.Close()

	on := openAnalytics(t, dir, 1<<20)
	defer on.Close()
	d := next(t, on)
	if d.Event().EventID != "click" {
		t.Fatalf("main stream got %s", d.Event().EventID)
	}
	// What the worker does with it: publish to the analytics stream, then ack.
	if err := on.Publish(context.Background(), []*event.Event{d.Event()}); err != nil {
		t.Fatal(err)
	}
	d.Ack()
	if got := nextOf(t, on.Analytics()).Event().EventID; got != "click" {
		t.Errorf("analytics stream got %s", got)
	}
}

// Set aside for a person to look at, like any event given up on.
func TestAnalyticsDeadLetterIsKept(t *testing.T) {
	q := openAnalytics(t, t.TempDir(), 1<<20)
	defer q.Close()
	if err := q.Publish(context.Background(), []*event.Event{analyticsEv("click")}); err != nil {
		t.Fatal(err)
	}
	nextOf(t, q.Analytics()).Dead("clickhouse refused it")
	waitPending(t, q, 0)
	s, err := q.js.Stream(context.Background(), deadStream)
	if err != nil {
		t.Fatal(err)
	}
	m, err := s.GetLastMsgForSubject(context.Background(), "juju.dead.7")
	if err != nil {
		t.Fatalf("dead letter missing: %v", err)
	}
	if m.Header.Get("Juju-Dead-Reason") != "clickhouse refused it" {
		t.Errorf("reason = %q", m.Header.Get("Juju-Dead-Reason"))
	}
}

// Lowering a limit is what someone short of disk does. It must not stop the
// server from starting, which would take error tracking down with it.
func TestLoweringQueueLimitsStillStarts(t *testing.T) {
	dir := t.TempDir()
	open := func(events, analytics int64) *JetStream {
		t.Helper()
		q, err := OpenJetStream(context.Background(), JetStreamConfig{Dir: dir, MaxBytes: events, Analytics: true, AnalyticsMaxBytes: analytics})
		if err != nil {
			t.Fatalf("open with limits %d, %d: %v", events, analytics, err)
		}
		return q
	}
	open(1<<30, 512<<20).Close()
	q := open(1<<30, 1<<20) // the analytics limit lowered
	q.Close()
	q = open(16<<20, 1<<20) // the main limit lowered too
	defer q.Close()
	s, err := q.js.Stream(context.Background(), analyticsStream)
	if err != nil {
		t.Fatal(err)
	}
	info, err := s.Info(context.Background())
	if err != nil {
		t.Fatal(err)
	}
	if info.Config.MaxBytes != 1<<20 {
		t.Errorf("analytics limit = %d, want the new 1 MB", info.Config.MaxBytes)
	}
}
