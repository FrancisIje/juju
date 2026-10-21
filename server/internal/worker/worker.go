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
	"juju/internal/queue"
	"juju/internal/store"
)

// Store is the slice of the database the worker needs. Defining it here, where
// it is used, lets tests pass a fake without a real Postgres.
type Store interface {
	// SaveEvent stores e under the issue for fp, and reports whether that
	// created a new issue or reopened a resolved one.
	SaveEvent(ctx context.Context, e *event.Event, fp fingerprint.Result) (store.SaveResult, error)
	// SaveSession merges a session update; SaveTransaction stores a
	// transaction. Both report whether the row was new.
	SaveSession(ctx context.Context, e *event.Event) (bool, error)
	SaveTransaction(ctx context.Context, e *event.Event) (bool, error)
}

// Alerter is told about new issues and regressions. Optional: nil sends
// no alerts. It must not block: the worker has more events to process.
type Alerter interface {
	IssueEvent(e *event.Event, fp fingerprint.Result, res store.SaveResult)
}

// Live is told when a project's issues changed, so open dashboards
// refresh. Optional, and like Alerter it must not block.
type Live interface {
	Changed(projectID int64)
}

// Analytics takes product analytics events (event, identify). Optional:
// nil means analytics is off and they're dropped. Must not block.
type Analytics interface {
	Add(e *event.Event)
}

// Symbolicator rewrites minified or obfuscated frames into source
// locations. Optional: nil skips symbolication.
type Symbolicator interface {
	Symbolicate(ctx context.Context, e *event.Event)
}

// MaxAttempts is how often a failing event is tried before it's set aside
// as a dead letter. With the delays below that's about 45 minutes, enough
// to ride out a database restart or a short outage.
const MaxAttempts = 50

// retryDelay grows with each attempt: 1s, 5s, 30s, then every minute.
func retryDelay(attempt int) time.Duration {
	switch attempt {
	case 1:
		return time.Second
	case 2:
		return 5 * time.Second
	case 3:
		return 30 * time.Second
	}
	return time.Minute
}

// Deps is what a worker needs. Symbolicator and Alerter are optional.
type Deps struct {
	Store        Store
	Symbolicator Symbolicator
	Alerter      Alerter
	Analytics    Analytics
	Live         Live
	Log          *slog.Logger
}

// Run starts n workers taking deliveries from src, and returns when src
// closes and every worker has finished the event in its hands. Each event
// is acknowledged only after it's stored: if the process dies first, the
// queue delivers it again, so nothing that got a 202 is ever lost.
func Run(ctx context.Context, src queue.Source, d Deps, n int) {
	var wg sync.WaitGroup
	deliveries := src.Deliveries()
	for range n {
		wg.Go(func() {
			for dl := range deliveries {
				handle(ctx, dl, d)
			}
		})
	}
	wg.Wait()
}

func handle(ctx context.Context, dl queue.Delivery, d Deps) {
	e := dl.Event()
	err := process(ctx, e, d)
	if err == nil {
		dl.Ack()
		return
	}
	log := d.Log.With("project_id", e.ProjectID, "event_id", e.EventID, "attempt", dl.Attempt())
	if dl.Attempt() >= MaxAttempts {
		log.Error("giving up on event; kept as a dead letter", "err", err)
		dl.Dead(err.Error())
		return
	}
	delay := retryDelay(dl.Attempt())
	log.Warn("could not store event, will retry", "err", err, "retry_in", delay)
	dl.Retry(delay)
}

// process stores one event. An error means "try again later": the queue
// keeps the event and delivers it again.
func process(ctx context.Context, e *event.Event, d Deps) (err error) {
	log := d.Log.With("project_id", e.ProjectID, "event_id", e.EventID)

	// One malformed event must not kill the worker goroutine. A panic is
	// treated like any other failure: retried, and set aside if it keeps
	// happening. recover only works inside a deferred function.
	defer func() {
		if r := recover(); r != nil {
			log.Error("panic while processing event", "panic", fmt.Sprint(r))
			err = fmt.Errorf("panic: %v", r)
		}
	}()

	switch {
	// Analytics events aren't issues: no symbolication, no fingerprint, no
	// alerts. They go to ClickHouse in batches.
	case e.IsAnalytics():
		if d.Analytics != nil {
			d.Analytics.Add(e)
		} else {
			log.Debug("analytics is off (no JUJU_CLICKHOUSE_URL), dropping event", "name", e.Name)
		}
		return nil
	// Sessions and transactions have their own tables. They need no alerts
	// or live refresh: the pages that show them reload on a timer, and an
	// update per app launch would make every open issue list refetch.
	case e.Type == event.TypeSession:
		_, err := d.Store.SaveSession(ctx, e)
		return err
	case e.Type == event.TypeTransaction:
		_, err := d.Store.SaveTransaction(ctx, e)
		return err
	}

	// Symbolicate before fingerprinting, so issues group by real names and
	// files, which stay the same across builds, instead of minified ones.
	if d.Symbolicator != nil {
		d.Symbolicator.Symbolicate(ctx, e)
		if e.Symbolication != nil && e.Symbolication.Status != "symbolicated" {
			log.Debug("symbolication incomplete", "status", e.Symbolication.Status, "notes", e.Symbolication.Notes)
		}
	}
	fp := fingerprint.Compute(e)
	res, err := d.Store.SaveEvent(ctx, e, fp)
	if err != nil {
		return err
	}
	log.Debug("event stored", "fingerprint", fp.Hash[:12], "duplicate", !res.Inserted, "new_issue", res.NewIssue, "regressed", res.Regressed)
	// A redelivered event the store already has (Inserted false) changed
	// nothing: no second alert, nothing to refresh.
	if d.Alerter != nil && (res.NewIssue || res.Regressed) {
		d.Alerter.IssueEvent(e, fp, res)
	}
	if d.Live != nil && res.Inserted {
		d.Live.Changed(e.ProjectID)
	}
	return nil
}
