package selfmon

import (
	"context"
	"fmt"
	"log/slog"
	"net/http"
	"slices"
	"time"
)

// Handler wraps a log handler so every Error-level line is also reported.
// The server already logs each failure it handles, so this covers all of them,
// present and future, without a reporting call at each place an error occurs.
func (m *Monitor) Handler(next slog.Handler) slog.Handler {
	if m == nil {
		return next
	}
	return &logHandler{next: next, mon: m}
}

type logHandler struct {
	next slog.Handler
	mon  *Monitor
	errs []string // "err" values from WithAttrs
}

func (h *logHandler) Enabled(ctx context.Context, l slog.Level) bool {
	return h.next.Enabled(ctx, l)
}

func (h *logHandler) Handle(ctx context.Context, r slog.Record) error {
	if r.Level >= slog.LevelError {
		// Only the log message and its error are sent. Other attributes can
		// hold customers' identifiers and values, and the stack already says
		// where the failure was.
		msg := ""
		if len(h.errs) > 0 {
			msg = h.errs[0]
		}
		r.Attrs(func(a slog.Attr) bool {
			if a.Key == "err" {
				msg = a.Value.String()
				return false
			}
			return true
		})
		h.mon.CaptureError(r.Message, msg, stack(), nil)
	}
	return h.next.Handle(ctx, r)
}

func (h *logHandler) WithAttrs(attrs []slog.Attr) slog.Handler {
	c := *h
	c.next = h.next.WithAttrs(attrs)
	c.errs = slices.Clone(h.errs)
	for _, a := range attrs {
		if a.Key == "err" {
			c.errs = append(c.errs, a.Value.String())
		}
	}
	return &c
}

func (h *logHandler) WithGroup(name string) slog.Handler {
	c := *h
	c.next = h.next.WithGroup(name)
	return &c
}

// Recover reports a panic in an HTTP handler and lets it carry on. net/http
// already survives handler panics, but it writes them to its own error log,
// so without this they would never reach the monitor.
func (m *Monitor) Recover(next http.Handler) http.Handler {
	if m == nil {
		return next
	}
	return http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		defer func() {
			v := recover()
			if v == nil {
				return
			}
			// ErrAbortHandler is how a handler deliberately drops a connection.
			if v != http.ErrAbortHandler {
				// The route pattern, not the URL: "/api/v1/projects/{id}/mcp"
				// groups well and holds no customer's IDs or query values.
				m.CaptureError("panic", fmt.Sprint(v), stack(), map[string]string{"route": r.Pattern})
			}
			panic(v)
		}()
		next.ServeHTTP(w, r)
	})
}

// Backlog is a number that should stay small, and what to call it when it
// doesn't.
type Backlog struct {
	Type  string // the issue's type, e.g. "platform.queue_backlog"
	Label string // for the message, e.g. "events waiting in the queue"
	Key   string // the property on the heartbeat, e.g. "queue_pending"
	Max   uint64
	Read  func(context.Context) (uint64, error)
}

const (
	sampleEvery    = time.Minute
	heartbeatEvery = 5 * time.Minute
	// While a backlog stays high, repeat it this often. The issue's last-seen
	// time then shows it's still happening, without an event a minute.
	repeatEvery = time.Hour
)

// Watch samples the backlogs until ctx is done. Going over a limit reports an
// error, which becomes an issue on the monitor, so the alerts already set up
// there (new issue, regression) cover it with no new alerting code.
//
// It also sends a heartbeat. A chart of that event's count shows gaps when
// this server stopped working, even if its process stayed up.
func (m *Monitor) Watch(ctx context.Context, backlogs []Backlog) {
	if m == nil {
		return
	}
	w := newWatcher(m, backlogs)
	tick := time.NewTicker(sampleEvery)
	defer tick.Stop()
	w.sample(ctx, time.Now())
	for {
		select {
		case <-ctx.Done():
			return
		case now := <-tick.C:
			w.sample(ctx, now)
		}
	}
}

type watcher struct {
	mon      *Monitor
	backlogs []Backlog
	lastSent map[string]time.Time // when each backlog over its limit last reported
	lastBeat time.Time
}

func newWatcher(m *Monitor, backlogs []Backlog) *watcher {
	return &watcher{mon: m, backlogs: backlogs, lastSent: map[string]time.Time{}}
}

func (w *watcher) sample(ctx context.Context, now time.Time) {
	props := map[string]any{}
	for _, b := range w.backlogs {
		n, err := b.Read(ctx)
		if err != nil {
			w.mon.log.Warn("self-monitoring sample failed", "backlog", b.Type, "err", err)
			continue
		}
		props[b.Key] = float64(n)
		if n <= b.Max {
			delete(w.lastSent, b.Type) // recovered: the next breach reports at once
			continue
		}
		if last, over := w.lastSent[b.Type]; !over || now.Sub(last) >= repeatEvery {
			w.lastSent[b.Type] = now
			w.mon.CaptureError(b.Type, fmt.Sprintf("%d %s, the limit is %d", n, b.Label, b.Max), nil, nil)
		}
	}
	if w.lastBeat.IsZero() || now.Sub(w.lastBeat) >= heartbeatEvery {
		w.lastBeat = now
		w.mon.Capture("platform.heartbeat", props)
	}
}
