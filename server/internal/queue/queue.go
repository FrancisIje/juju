// Package queue sits between the HTTP ingest handler (fast) and the workers
// that write to Postgres (slow), so SDKs get a reply without waiting on the
// database.
//
// This implementation is an in-memory buffered channel: simple, but events
// still in the buffer are lost if the process crashes. Phase 9 swaps it for a
// durable queue (NATS JetStream / Redis Streams) behind the same interface.
package queue

import "juju/internal/event"

// Queue is the only thing the ingest handler knows about. Keeping it this
// small is what makes the Phase 9 swap a one-file change.
type Queue interface {
	// TryEnqueue adds e without blocking. It returns false when the queue
	// is full, which the handler turns into 429 Too Many Requests.
	TryEnqueue(e *event.Event) bool
}

type Chan struct {
	ch chan *event.Event
}

func NewChan(size int) *Chan {
	return &Chan{ch: make(chan *event.Event, size)}
}

func (c *Chan) TryEnqueue(e *event.Event) bool {
	select {
	case c.ch <- e:
		return true
	default: // buffer full: don't make the SDK wait, tell it to retry later
		return false
	}
}

// Events is the receive side, read by workers. The loop `for e := range
// q.Events()` ends once Close has been called and the buffer is drained.
func (c *Chan) Events() <-chan *event.Event { return c.ch }

// Len reports how many events are waiting. Always near Cap means the workers
// cannot keep up.
func (c *Chan) Len() int { return len(c.ch) }
func (c *Chan) Cap() int { return cap(c.ch) }

// Close must only be called after nothing can call TryEnqueue any more (the
// HTTP server has shut down); sending on a closed channel panics.
func (c *Chan) Close() { close(c.ch) }
