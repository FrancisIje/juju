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
