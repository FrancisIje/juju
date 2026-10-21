package event

import (
	"fmt"
	"slices"
	"strings"
	"time"

	"github.com/google/uuid"
)

// Session is one update to an app session: a single run of the app in the
// foreground (mobile) or one page load (web). The SDK sends an update when
// the session starts, when it ends and when it crashes; the server keeps one
// row per session and merges the updates into it.
//
// Updates can arrive late, twice or out of order (offline queues, retries),
// so each carries Seq, a counter the SDK increases with every update. The
// server keeps the status of the highest Seq, except that "crashed" is
// final: nothing after a crash can make the session healthy again.
type Session struct {
	ID         string `json:"sid"`
	Seq        int64  `json:"seq"`
	Status     string `json:"status"`                // ok (running), exited (ended normally), crashed
	Started    string `json:"started"`               // when the session began, RFC 3339
	Errors     int    `json:"errors"`                // errors reported during the session so far
	DurationMs *int64 `json:"duration_ms,omitempty"` // set when the session ends

	StartedAt time.Time `json:"-"` // parsed Started
}

// Transaction is one timed operation: a page load, an app start, a screen
// load, or anything the app measures itself. Spans are the steps inside it
// (an HTTP request, a database query), so a slow transaction shows where
// its time went.
type Transaction struct {
	Name           string             `json:"name"`            // what was measured: "/checkout", "app.start", "CartScreen"
	Op             string             `json:"op"`              // the kind: pageload, navigation, app.start, screen.load, custom ...
	Status         string             `json:"status"`          // ok, error, cancelled, deadline_exceeded
	StartTimestamp string             `json:"start_timestamp"` // the event's own Timestamp is the end
	Measurements   map[string]float64 `json:"measurements,omitempty"`
	Spans          []Span             `json:"spans,omitempty"`

	// Set by the server, so the dashboard and SQL needn't parse timestamps.
	DurationMs float64   `json:"duration_ms"`
	Start      time.Time `json:"-"`
}

type Span struct {
	SpanID         string         `json:"span_id"`
	ParentSpanID   string         `json:"parent_span_id,omitempty"`
	Op             string         `json:"op"`
	Description    string         `json:"description,omitempty"`
	Status         string         `json:"status,omitempty"`
	StartTimestamp string         `json:"start_timestamp"`
	Timestamp      string         `json:"timestamp"`
	Data           map[string]any `json:"data,omitempty"`

	// Set by the server: milliseconds from the transaction's start, and the
	// span's length. This is what a waterfall chart draws.
	StartMs    float64 `json:"start_ms"`
	DurationMs float64 `json:"duration_ms"`
}

const (
	maxTransactionName = 200
	maxOp              = 64
	maxSpans           = 500
	maxSpanDescription = 500
	maxMeasurements    = 20
	// A transaction longer than this is almost always one the app forgot to
	// finish. Its duration would wreck every percentile, so it's dropped.
	maxTransactionLength = time.Hour
)

var sessionStatuses = []string{"ok", "exited", "crashed"}

func validateSession(e *Event) ValidationError {
	var errs ValidationError
	s := e.Session
	if s == nil {
		return ValidationError{{"session", "is required for type \"session\""}}
	}
	if u, err := uuid.Parse(s.ID); err != nil || u == uuid.Nil {
		errs = append(errs, FieldError{"session.sid", "must be a UUID"})
	} else {
		s.ID = u.String()
	}
	if !slices.Contains(sessionStatuses, s.Status) {
		errs = append(errs, FieldError{"session.status", fmt.Sprintf("%q is not one of ok, exited, crashed", s.Status)})
	}
	if s.Seq < 0 {
		errs = append(errs, FieldError{"session.seq", "must not be negative"})
	}
	return errs
}

func validateTransaction(e *Event) ValidationError {
	var errs ValidationError
	tx := e.Transaction
	if tx == nil {
		return ValidationError{{"transaction", "is required for type \"transaction\""}}
	}
	if strings.TrimSpace(tx.Name) == "" {
		errs = append(errs, FieldError{"transaction.name", "is required"})
	}
	start, err := time.Parse(time.RFC3339Nano, tx.StartTimestamp)
	if err != nil {
		errs = append(errs, FieldError{"transaction.start_timestamp", "must be an RFC 3339 time"})
	}
	// The end is the event's timestamp. Unlike an error's, it can't be
	// replaced by the receive time: the duration is the whole point.
	end, err := time.Parse(time.RFC3339Nano, e.Timestamp)
	switch {
	case err != nil:
		errs = append(errs, FieldError{"timestamp", "must be an RFC 3339 time (the transaction's end)"})
	case !start.IsZero() && end.Before(start):
		errs = append(errs, FieldError{"timestamp", "is before transaction.start_timestamp"})
	case !start.IsZero() && end.Sub(start) > maxTransactionLength:
		errs = append(errs, FieldError{"transaction", "is longer than 1 hour; was it finished?"})
	}
	tx.Start = start
	return errs
}

// repairSession runs after the event's timestamp has been repaired.
func (e *Event) repairSession() {
	s := e.Session
	t, err := time.Parse(time.RFC3339Nano, s.Started)
	switch {
	case s.Started == "" || err != nil:
		e.repair("session.started: missing or not RFC 3339, used the update's timestamp")
		t = e.Time
	case t.After(e.Time):
		e.repair("session.started: after the update's timestamp, used the timestamp")
		t = e.Time
	}
	s.StartedAt = t.UTC()
	s.Started = s.StartedAt.Format(time.RFC3339Nano)
	if s.Errors < 0 {
		s.Errors = 0
	}
	if s.DurationMs != nil && *s.DurationMs < 0 {
		s.DurationMs = nil
		e.repair("session.duration_ms: negative, dropped")
	}
	// A session update needs no breadcrumbs; don't store them.
	e.Breadcrumbs = nil
}

// repairTransaction runs after the event's timestamp has been repaired.
// clientEnd is the end time the SDK sent, before that repair.
func (e *Event) repairTransaction(clientEnd time.Time) {
	tx := e.Transaction
	// A phone with a wrong clock gets its end time replaced by the receive
	// time. Shift the start and every span by the same amount, so the
	// durations, which don't depend on the clock being right, survive.
	shift := e.Time.Sub(clientEnd)
	tx.Start = tx.Start.Add(shift).UTC()
	tx.StartTimestamp = tx.Start.Format(time.RFC3339Nano)
	tx.DurationMs = ms(e.Time.Sub(tx.Start))

	tx.Name = e.truncate("transaction.name", strings.TrimSpace(tx.Name), maxTransactionName)
	if tx.Op = strings.TrimSpace(tx.Op); tx.Op == "" {
		tx.Op = "custom"
	}
	tx.Op = e.truncate("transaction.op", tx.Op, maxOp)
	if tx.Status == "" {
		tx.Status = "ok"
	}
	tx.Status, _ = truncate(tx.Status, maxOp)

	if len(tx.Measurements) > maxMeasurements {
		e.repair(fmt.Sprintf("transaction.measurements: %d sent, dropped them all (max %d)", len(tx.Measurements), maxMeasurements))
		tx.Measurements = nil
	}

	if n := len(tx.Spans); n > maxSpans {
		// The earliest spans usually explain a slow start; keep those.
		tx.Spans = tx.Spans[:maxSpans]
		e.repair(fmt.Sprintf("transaction.spans: %d sent, kept the first %d", n, maxSpans))
	}
	kept := tx.Spans[:0]
	dropped := 0
	for _, sp := range tx.Spans {
		start, err1 := time.Parse(time.RFC3339Nano, sp.StartTimestamp)
		end, err2 := time.Parse(time.RFC3339Nano, sp.Timestamp)
		if err1 != nil || err2 != nil || end.Before(start) || strings.TrimSpace(sp.Op) == "" {
			dropped++
			continue
		}
		start, end = start.Add(shift).UTC(), end.Add(shift).UTC()
		sp.StartTimestamp, sp.Timestamp = start.Format(time.RFC3339Nano), end.Format(time.RFC3339Nano)
		sp.StartMs = ms(start.Sub(tx.Start))
		sp.DurationMs = ms(end.Sub(start))
		sp.Op, _ = truncate(sp.Op, maxOp)
		sp.Description, _ = truncate(sp.Description, maxSpanDescription)
		sp.Status, _ = truncate(sp.Status, maxOp)
		kept = append(kept, sp)
	}
	tx.Spans = kept
	if len(tx.Spans) == 0 {
		tx.Spans = nil
	}
	if dropped > 0 {
		e.repair(fmt.Sprintf("transaction.spans: dropped %d without an op or with bad times", dropped))
	}
}

// ms converts a duration to milliseconds with microsecond precision: an
// HTTP span on a fast network can be under a millisecond.
func ms(d time.Duration) float64 { return float64(d.Microseconds()) / 1000 }
