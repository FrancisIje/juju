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
	"juju/internal/store"
)

// Store is the slice of the database the worker needs. Defining it here, where
// it is used, lets tests pass a fake without a real Postgres.
type Store interface {
	// SaveEvent stores e under the issue for fp, and reports whether that
	// created a new issue or reopened a resolved one.
	SaveEvent(ctx context.Context, e *event.Event, fp fingerprint.Result) (store.SaveResult, error)
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

const (
	attempts  = 3
	baseDelay = 100 * time.Millisecond
)

// Run starts n workers reading from events and blocks until the channel is
// closed and every buffered event has been processed. Draining before
// returning is what makes a clean shutdown (Ctrl-C, deploy) lose nothing.
// Deps is what a worker needs. Symbolicator and Alerter are optional.
type Deps struct {
	Store        Store
	Symbolicator Symbolicator
	Alerter      Alerter
	Analytics    Analytics
	Live         Live
	Log          *slog.Logger
}

func Run(ctx context.Context, events <-chan *event.Event, d Deps, n int) {
	var wg sync.WaitGroup
	for range n {
		wg.Go(func() {
			for e := range events {
				process(ctx, e, d)
			}
		})
	}
	wg.Wait()
}

func process(ctx context.Context, e *event.Event, d Deps) {
	log := d.Log.With("project_id", e.ProjectID, "event_id", e.EventID)

	// One malformed event must not kill the worker goroutine, or the queue
	// would stop draining. recover only works inside a deferred function.
	defer func() {
		if r := recover(); r != nil {
			log.Error("panic while processing event", "panic", fmt.Sprint(r))
		}
	}()

	// Analytics events aren't issues: no symbolication, no fingerprint, no
	// alerts. They go to ClickHouse in batches.
	if e.IsAnalytics() {
		if d.Analytics != nil {
			d.Analytics.Add(e)
		} else {
			log.Debug("analytics is off (no JUJU_CLICKHOUSE_URL), dropping event", "name", e.Name)
		}
		return
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
	var err error
	for i := range attempts {
		var res store.SaveResult
		res, err = d.Store.SaveEvent(ctx, e, fp)
		if err == nil {
			log.Debug("event stored", "fingerprint", fp.Hash[:12], "duplicate", !res.Inserted, "new_issue", res.NewIssue, "regressed", res.Regressed)
			if d.Alerter != nil && (res.NewIssue || res.Regressed) {
				d.Alerter.IssueEvent(e, fp, res)
			}
			// A retried duplicate changed nothing, so nothing to refresh.
			if d.Live != nil && res.Inserted {
				d.Live.Changed(e.ProjectID)
			}
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
