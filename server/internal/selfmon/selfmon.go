// Package selfmon lets a juju server report on itself to a second juju
// instance, the same way an app reports to juju: events over HTTP, to a DSN.
//
// The rule for everything here is that monitoring must never hurt the thing
// it monitors. Capturing never blocks, a full buffer drops events, a failed
// send is written to the local log and forgotten, and a bad DSN turns
// reporting off instead of stopping the server.
package selfmon

import (
	"bytes"
	"context"
	"encoding/json"
	"errors"
	"fmt"
	"log/slog"
	"net/http"
	"net/url"
	"regexp"
	"runtime"
	"runtime/debug"
	"strings"
	"sync"
	"sync/atomic"
	"time"

	"github.com/google/uuid"

	"juju/internal/event"
)

const (
	bufferSize = 1024
	// One batch per interval caps what an error storm can send at 10 events
	// a second. The two instances may share a server, so a storm on one must
	// not become a load problem for both.
	batchSize     = 50
	flushInterval = 5 * time.Second
	sendTimeout   = 5 * time.Second
	maxFrames     = 30
)

// Options configures a Monitor.
type Options struct {
	DSN       string // where to report, https://pk_...@host/1
	PublicURL string // this server's own URL, to refuse reporting to itself
	Log       *slog.Logger
	Client    *http.Client // nil: a default with a short timeout
	// FlushInterval overrides how often batches are sent, for tests.
	FlushInterval time.Duration
}

// Monitor sends events to the monitoring instance. A nil *Monitor is valid
// and does nothing, so callers don't need to check whether it's turned on.
type Monitor struct {
	endpoint string
	key      string
	release  string
	client   *http.Client
	log      *slog.Logger // never the wrapped logger: see New
	interval time.Duration
	ch       chan event.Event
	stop     chan struct{}
	done     chan struct{}
	once     sync.Once
	dropped  atomic.Int64
}

var dsnPattern = regexp.MustCompile(`^(https?)://([^@/\s]+)@([^/\s?#@]+)/(\d+)/?$`)

// New starts a Monitor. Opts.Log must be the plain logger, not one wrapped by
// Handler: a failed send is logged, and if that log line were itself captured
// and sent, an unreachable monitor would make the server report on its own
// reporting forever.
func New(o Options) (*Monitor, error) {
	m := dsnPattern.FindStringSubmatch(strings.TrimSpace(o.DSN))
	if m == nil {
		// The DSN holds a key, so the message doesn't repeat it.
		return nil, errors.New("JUJU_MONITOR_DSN is not a valid DSN, it should look like https://pk_...@host/1")
	}
	scheme, key, host, project := m[1], m[2], m[3], m[4]
	// A server reporting to itself turns each error into another event it
	// has to process, which is the wrong thing to do during an incident, and
	// an outage would go unreported anyway.
	if u, err := url.Parse(o.PublicURL); err == nil && strings.EqualFold(u.Host, host) {
		return nil, fmt.Errorf("JUJU_MONITOR_DSN points at this server (%s), it must be a different instance", host)
	}
	client := o.Client
	if client == nil {
		client = &http.Client{Timeout: sendTimeout}
	}
	interval := o.FlushInterval
	if interval == 0 {
		interval = flushInterval
	}
	mon := &Monitor{
		endpoint: fmt.Sprintf("%s://%s/api/v1/projects/%s/events", scheme, host, project),
		key:      key,
		release:  buildRelease(),
		client:   client,
		log:      o.Log,
		interval: interval,
		ch:       make(chan event.Event, bufferSize),
		stop:     make(chan struct{}),
		done:     make(chan struct{}),
	}
	go mon.run()
	return mon, nil
}

// buildRelease names this build by its git commit, which the Go toolchain
// stamps into the binary. It's what lets the monitor tell a regression from
// an old bug, and link an error to the deploy that caused it.
func buildRelease() string {
	info, ok := debug.ReadBuildInfo()
	if !ok {
		return ""
	}
	for _, s := range info.Settings {
		if s.Key == "vcs.revision" && len(s.Value) >= 12 {
			return "juju@" + s.Value[:12]
		}
	}
	return ""
}

// Close sends what's buffered and stops. Safe to call more than once.
func (m *Monitor) Close() {
	if m == nil {
		return
	}
	m.once.Do(func() { close(m.stop) })
	<-m.done
}

func (m *Monitor) enqueue(e event.Event) {
	if m == nil {
		return
	}
	e.EventID = uuid.NewString()
	e.Timestamp = time.Now().UTC().Format(time.RFC3339Nano)
	e.Platform = "other"
	e.Environment = "production"
	e.Release = m.release
	select {
	case m.ch <- e:
	default:
		m.dropped.Add(1)
	}
}

// CaptureError reports an error, which the monitor groups into an issue by
// its type and stack (or, with no stack, its type and message).
func (m *Monitor) CaptureError(typ, message string, frames []event.Frame, tags map[string]string) {
	e := event.Event{Type: event.TypeError, Tags: tags, Exception: &event.Exception{Type: typ, Message: message}}
	if len(frames) > 0 {
		e.Exception.Stacktrace = &event.Stacktrace{Frames: frames}
	}
	m.enqueue(e)
}

// Capture reports an analytics event, which only ever counts toward charts.
func (m *Monitor) Capture(name string, properties map[string]any) {
	m.enqueue(event.Event{Type: event.TypeEvent, Name: name, DistinctID: "juju-server", Properties: properties})
}

func (m *Monitor) run() {
	defer close(m.done)
	tick := time.NewTicker(m.interval)
	defer tick.Stop()
	for {
		select {
		case <-tick.C:
			m.flush(m.take())
		case <-m.stop:
			// One last batch, so an error that led to the shutdown still goes
			// out. Whatever doesn't fit is lost: shutdown must not wait on it.
			m.flush(m.take())
			return
		}
	}
}

// take removes up to one batch from the buffer without waiting.
func (m *Monitor) take() []event.Event {
	batch := make([]event.Event, 0, batchSize)
	for len(batch) < batchSize {
		select {
		case e := <-m.ch:
			batch = append(batch, e)
		default:
			return batch
		}
	}
	return batch
}

func (m *Monitor) flush(batch []event.Event) {
	if n := m.dropped.Swap(0); n > 0 {
		m.log.Warn("self-monitoring buffer full, events dropped", "dropped", n)
	}
	if len(batch) == 0 {
		return
	}
	// A failed batch isn't retried. Keeping it would mean holding memory for
	// as long as the monitor is down, and the outage itself is what the
	// external uptime check is there to catch.
	if err := m.send(batch); err != nil {
		m.log.Warn("self-monitoring send failed", "err", err, "events", len(batch))
	}
}

func (m *Monitor) send(batch []event.Event) error {
	env := event.Envelope{SDK: event.SDK{Name: "juju-server", Version: "1"}}
	for _, e := range batch {
		b, err := json.Marshal(e)
		if err != nil {
			return err
		}
		env.Events = append(env.Events, b)
	}
	body, err := json.Marshal(env)
	if err != nil {
		return err
	}
	ctx, cancel := context.WithTimeout(context.Background(), sendTimeout)
	defer cancel()
	req, err := http.NewRequestWithContext(ctx, http.MethodPost, m.endpoint, bytes.NewReader(body))
	if err != nil {
		return err
	}
	req.Header.Set("Content-Type", "application/json")
	req.Header.Set("X-Juju-Key", m.key)
	res, err := m.client.Do(req)
	if err != nil {
		return err
	}
	defer res.Body.Close()
	if res.StatusCode >= 300 {
		return fmt.Errorf("monitor answered %s", res.Status)
	}
	return nil
}

// stack returns the current goroutine's frames, most recent first, leaving
// out the logging and panic machinery above the code that actually failed.
func stack() []event.Frame {
	pcs := make([]uintptr, 64)
	n := runtime.Callers(2, pcs)
	it := runtime.CallersFrames(pcs[:n])
	var out []event.Frame
	for len(out) < maxFrames {
		f, more := it.Next()
		if f.Function != "" && !skipFrame(f.Function) {
			out = append(out, event.Frame{
				Function: f.Function,
				Filename: f.File,
				Lineno:   f.Line,
				InApp:    strings.HasPrefix(f.Function, "juju/"),
			})
		}
		if !more {
			break
		}
	}
	return out
}

func skipFrame(fn string) bool {
	return strings.HasPrefix(fn, "juju/internal/selfmon.(*logHandler).") ||
		strings.HasPrefix(fn, "juju/internal/selfmon.(*Monitor).") ||
		strings.HasPrefix(fn, "log/slog.") ||
		fn == "runtime.gopanic" || fn == "runtime.panicmem" || fn == "runtime.sigpanic" || fn == "runtime.goPanicIndex"
}
