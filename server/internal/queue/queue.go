// Package queue sits between the HTTP ingest handler (fast) and the workers
// that store events (slower), so SDKs get a reply without waiting on the
// database.
//
// Production uses JetStream (jetstream.go): events are written to disk
// before ingest answers 202, and a worker acknowledges each one only after
// storing it, so a crash or restart at any moment loses nothing: whatever
// wasn't acknowledged is delivered again. Chan is an in-memory version with
// the same interfaces, for tests.
package queue

import (
	"context"
	"errors"
	"sync"
	"time"

	"juju/internal/event"
)

// ErrFull: the queue holds as much as it's allowed to. Ingest answers 429,
// and the SDK retries later.
var ErrFull = errors.New("queue full")

// Publisher is what ingest needs: hand over a batch, durably.
type Publisher interface {
	// Publish returns nil once every event is safely queued, ErrFull when
	// the queue is at its limit, or another error when it can't be reached.
	// On an error some events may already be queued; that's fine, because
	// the SDK resends the batch and the queue drops duplicates by event_id.
	Publish(ctx context.Context, events []*event.Event) error
}

// Delivery is one event handed to a worker. Exactly one of Ack, Retry or
// Dead must be called.
type Delivery interface {
	Event() *event.Event
	// Attempt is 1 the first time this event is delivered, 2 after one retry...
	Attempt() int
	// Ack: stored, forget it.
	Ack()
	// Retry: couldn't store it now; deliver it again after the delay.
	Retry(after time.Duration)
	// Dead: give up after too many attempts. It's kept aside (the dead-letter
	// stream) for inspection, not silently dropped.
	Dead(reason string)
}

// Source is what workers need: the stream of deliveries.
type Source interface {
	Deliveries() <-chan Delivery
}

// Chan is an in-memory queue for tests: a buffered channel. Retries are
// put back after their delay; Dead records the reason.
type Chan struct {
	ch     chan Delivery
	mu     sync.Mutex
	closed bool
	wg     sync.WaitGroup // retries waiting to be put back
	dead   []string
}

func NewChan(size int) *Chan { return &Chan{ch: make(chan Delivery, size)} }

func (c *Chan) Publish(_ context.Context, events []*event.Event) error {
	c.mu.Lock()
	defer c.mu.Unlock()
	if c.closed {
		return errors.New("queue closed")
	}
	for _, e := range events {
		select {
		case c.ch <- &chanDelivery{c: c, e: e, attempt: 1}:
		default:
			return ErrFull
		}
	}
	return nil
}

func (c *Chan) Deliveries() <-chan Delivery { return c.ch }

// Close ends Deliveries once everything buffered has been taken. Call it
// only after nothing publishes any more.
func (c *Chan) Close() {
	c.wg.Wait()
	c.mu.Lock()
	c.closed = true
	c.mu.Unlock()
	close(c.ch)
}

func (c *Chan) Len() int { return len(c.ch) }

// DeadLetters lists the reasons events were given up on.
func (c *Chan) DeadLetters() []string {
	c.mu.Lock()
	defer c.mu.Unlock()
	return append([]string(nil), c.dead...)
}

type chanDelivery struct {
	c       *Chan
	e       *event.Event
	attempt int
}

func (d *chanDelivery) Event() *event.Event { return d.e }
func (d *chanDelivery) Attempt() int        { return d.attempt }
func (d *chanDelivery) Ack()                {}
func (d *chanDelivery) Retry(after time.Duration) {
	d.c.wg.Add(1)
	go func() {
		defer d.c.wg.Done()
		time.Sleep(after)
		d.c.ch <- &chanDelivery{c: d.c, e: d.e, attempt: d.attempt + 1}
	}()
}
func (d *chanDelivery) Dead(reason string) {
	d.c.mu.Lock()
	d.c.dead = append(d.c.dead, d.e.EventID+": "+reason)
	d.c.mu.Unlock()
}
