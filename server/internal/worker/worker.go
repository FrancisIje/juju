// Package worker drains the queue: fingerprint each event, then store it.
package worker

import (
	"context"
	"fmt"
	"log/slog"
	"sync"
	"time"

	"juju/internal/event"
	"juju/internal/fingerprint"
)

// Store is the slice of the database the worker needs. Defining it here, where
// it is used, lets tests pass a fake without a real Postgres.
type Store interface {
	// SaveEvent stores e under the issue for fp. inserted is false when the
	// event_id was already stored (a retried send).
	SaveEvent(ctx context.Context, e *event.Event, fp fingerprint.Result) (inserted bool, err error)
}

const (
	attempts  = 3
	baseDelay = 100 * time.Millisecond
)

// Run starts n workers reading from events and blocks until the channel is
// closed and every buffered event has been processed. Draining before
// returning is what makes a clean shutdown (Ctrl-C, deploy) lose nothing.
func Run(ctx context.Context, events <-chan *event.Event, store Store, log *slog.Logger, n int) {
	var wg sync.WaitGroup
	for range n {
		wg.Go(func() {
			for e := range events {
				process(ctx, e, store, log)
			}
		})
	}
	wg.Wait()
}

func process(ctx context.Context, e *event.Event, store Store, log *slog.Logger) {
	log = log.With("project_id", e.ProjectID, "event_id", e.EventID)

	// One malformed event must not kill the worker goroutine, or the queue
	// would stop draining. recover only works inside a deferred function.
	defer func() {
		if r := recover(); r != nil {
			log.Error("panic while processing event", "panic", fmt.Sprint(r))
		}
	}()

	fp := fingerprint.Compute(e)
	var err error
	for i := range attempts {
		var inserted bool
		inserted, err = store.SaveEvent(ctx, e, fp)
		if err == nil {
			log.Debug("event stored", "fingerprint", fp.Hash[:12], "duplicate", !inserted)
			return
		}
		if ctx.Err() != nil || i == attempts-1 {
			break
		}
		time.Sleep(baseDelay << i) // 100ms, then 200ms: give a flaky DB time to recover
	}
	// The SDK already got 202 and deleted the event, so it is gone. A
	// durable queue (Phase 9) fixes this by retrying from disk.
	log.Error("dropping event after retries", "err", err)
}
