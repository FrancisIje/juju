package analytics

import (
	"context"
	"log/slog"
	"regexp"
	"strings"
	"sync"
	"time"

	"juju/internal/event"
)

// Writer batches analytics events into ClickHouse. ClickHouse wants few,
// large inserts: each insert creates a new part on disk that it later
// merges, so thousands of one-row inserts a second would swamp it.
//
// It never blocks the worker. If ClickHouse is down, it keeps retrying with
// backoff while holding up to MaxPending rows; past that the oldest are
// dropped and counted, because storing errors matters more than counting
// clicks.
type Writer struct {
	Client     *Client
	Log        *slog.Logger
	FlushEvery time.Duration // default 1s
	BatchSize  int           // flush early at this many rows; default 5000
	MaxPending int           // default 100000

	in      chan *event.Event
	stop    chan struct{}
	done    chan struct{}
	once    sync.Once
	mu      sync.Mutex
	dropped int64
}

func (w *Writer) init() {
	w.once.Do(func() {
		if w.FlushEvery == 0 {
			w.FlushEvery = time.Second
		}
		if w.BatchSize == 0 {
			w.BatchSize = 5000
		}
		if w.MaxPending == 0 {
			w.MaxPending = 100_000
		}
		w.in = make(chan *event.Event, 10_000)
		w.stop = make(chan struct{})
		w.done = make(chan struct{})
	})
}

// Add hands an event to the writer without waiting.
func (w *Writer) Add(e *event.Event) {
	w.init()
	select {
	case w.in <- e:
	default:
		w.countDrop(1)
	}
}

// Dropped is how many events were thrown away because ClickHouse couldn't
// keep up or was down for too long.
func (w *Writer) Dropped() int64 {
	w.mu.Lock()
	defer w.mu.Unlock()
	return w.dropped
}

func (w *Writer) countDrop(n int) {
	w.mu.Lock()
	w.dropped += int64(n)
	w.mu.Unlock()
}

// Start runs the batching loop until Stop.
func (w *Writer) Start() {
	w.init()
	go w.loop()
}

// Stop flushes what's pending (trying for up to 10s) and returns.
func (w *Writer) Stop() {
	w.init()
	close(w.stop)
	<-w.done
}

func (w *Writer) loop() {
	defer close(w.done)
	var events, aliases []any
	tick := time.NewTicker(w.FlushEvery)
	defer tick.Stop()
	failures := 0
	var retryAt time.Time

	flush := func(ctx context.Context) {
		if len(events) == 0 && len(aliases) == 0 {
			return
		}
		if !retryAt.IsZero() && time.Now().Before(retryAt) {
			return
		}
		// Aliases first: a person's first event and its identify can arrive
		// in one batch, and either order gives the same answer at query time.
		err := w.Client.Insert(ctx, "person_aliases", aliases)
		if err == nil {
			aliases = nil
			err = w.Client.Insert(ctx, "events", events)
		}
		if err != nil {
			failures++
			delay := min(30*time.Second, time.Second<<min(failures-1, 5))
			retryAt = time.Now().Add(delay)
			w.Log.Warn("analytics: write failed, will retry", "err", err, "pending", len(events), "retry_in", delay)
			return
		}
		if failures > 0 {
			w.Log.Info("analytics: writing again", "after_failures", failures)
		}
		failures, retryAt = 0, time.Time{}
		events = nil
	}

	add := func(e *event.Event) {
		events = append(events, Row(e))
		if e.Type == event.TypeIdentify && e.AnonID != "" && e.AnonID != e.DistinctID {
			aliases = append(aliases, aliasRow{ProjectID: e.ProjectID, DistinctID: e.AnonID, PersonID: e.DistinctID, CreatedAt: e.Time.Format(time.RFC3339Nano)})
		}
		if over := len(events) - w.MaxPending; over > 0 {
			events = events[over:]
			w.countDrop(over)
			w.Log.Warn("analytics: too many events waiting, dropped the oldest", "dropped", over)
		}
	}

	for {
		select {
		case e := <-w.in:
			add(e)
			if len(events) >= w.BatchSize {
				flush(context.Background())
			}
		case <-tick.C:
			flush(context.Background())
		case <-w.stop:
			// Take what's still in the channel. A plain break inside select
			// would only leave the select, so the loop is labeled.
		drain:
			for {
				select {
				case e := <-w.in:
					add(e)
				default:
					break drain
				}
			}
			ctx, cancel := context.WithTimeout(context.Background(), 10*time.Second)
			retryAt = time.Time{}
			flush(ctx)
			cancel()
			if len(events) > 0 {
				w.countDrop(len(events))
				w.Log.Error("analytics: lost events at shutdown, ClickHouse unreachable", "count", len(events))
			}
			return
		}
	}
}

type eventRow struct {
	ProjectID   int64             `json:"project_id"`
	EventID     string            `json:"event_id"`
	Name        string            `json:"name"`
	DistinctID  string            `json:"distinct_id"`
	Timestamp   string            `json:"timestamp"`
	ReceivedAt  string            `json:"received_at"`
	Platform    string            `json:"platform"`
	Lib         string            `json:"lib"`
	Release     string            `json:"release"`
	Environment string            `json:"environment"`
	OS          string            `json:"os"`
	Browser     string            `json:"browser"`
	Properties  map[string]string `json:"properties"`
}

type aliasRow struct {
	ProjectID  int64  `json:"project_id"`
	DistinctID string `json:"distinct_id"`
	PersonID   string `json:"person_id"`
	CreatedAt  string `json:"created_at"`
}

// Row is what's stored for an event. OS and browser come from the context
// the SDKs already send (mobile: os.name; web: the user agent).
func Row(e *event.Event) eventRow {
	osName, browser := clientOf(e.Context)
	return eventRow{
		ProjectID:   e.ProjectID,
		EventID:     e.EventID,
		Name:        e.Name,
		DistinctID:  e.DistinctID,
		Timestamp:   e.Time.Format(time.RFC3339Nano),
		ReceivedAt:  e.ReceivedAt.Format(time.RFC3339Nano),
		Platform:    e.Platform,
		Lib:         e.SDK.Name,
		Release:     e.Release,
		Environment: e.Environment,
		OS:          osName,
		Browser:     browser,
		Properties:  e.StringProperties(),
	}
}

func clientOf(ctx map[string]any) (osName, browser string) {
	if os, ok := ctx["os"].(map[string]any); ok {
		if n, ok := os["name"].(string); ok {
			osName = n
		}
	}
	if b, ok := ctx["browser"].(map[string]any); ok {
		if ua, ok := b["user_agent"].(string); ok {
			uaOS, uaBrowser := parseUserAgent(ua)
			if osName == "" {
				osName = uaOS
			}
			browser = uaBrowser
		}
	}
	return normalizeOS(osName), browser
}

// parseUserAgent recognises the common cases, enough for a breakdown.
// Order matters: Edge and Chrome on Android also say "Safari", Chrome on
// iOS says "Mac OS X".
func parseUserAgent(ua string) (osName, browser string) {
	switch {
	case strings.Contains(ua, "iPhone"), strings.Contains(ua, "iPad"):
		osName = "iOS"
	case strings.Contains(ua, "Android"):
		osName = "Android"
	case strings.Contains(ua, "Windows"):
		osName = "Windows"
	case strings.Contains(ua, "Mac OS X"), strings.Contains(ua, "Macintosh"):
		osName = "macOS"
	case strings.Contains(ua, "CrOS"):
		osName = "ChromeOS"
	case strings.Contains(ua, "Linux"):
		osName = "Linux"
	}
	switch {
	case strings.Contains(ua, "Edg/"):
		browser = "Edge"
	case strings.Contains(ua, "Firefox/"), strings.Contains(ua, "FxiOS/"):
		browser = "Firefox"
	case strings.Contains(ua, "Chrome/"), strings.Contains(ua, "CriOS/"):
		browser = "Chrome"
	case strings.Contains(ua, "Safari/"):
		browser = "Safari"
	}
	return osName, browser
}

var osNames = map[string]string{"ios": "iOS", "android": "Android", "macos": "macOS", "windows": "Windows", "linux": "Linux", "fuchsia": "Fuchsia"}
var spaces = regexp.MustCompile(`\s+`)

// normalizeOS makes "android", "Android" and " android " one value, so a
// breakdown doesn't split one OS in three.
func normalizeOS(s string) string {
	s = spaces.ReplaceAllString(strings.TrimSpace(s), " ")
	if n, ok := osNames[strings.ToLower(s)]; ok {
		return n
	}
	return s
}
