package analytics

import (
	"context"
	"errors"
	"log/slog"
	"net/http"
	"regexp"
	"strings"
	"time"

	"juju/internal/event"
	"juju/internal/queue"
)

// Writer batches analytics events from the queue into ClickHouse. ClickHouse
// wants few, large inserts: each insert creates a new part on disk that it
// later merges, so thousands of one-row inserts a second would swamp it.
//
// An event is acknowledged only after ClickHouse has it, so a crash at any
// moment loses nothing: whatever wasn't acknowledged is delivered again. That
// can write a row twice, which is safe because the table merges rows with the
// same event_id and counts use uniqExact(event_id).
//
// When ClickHouse is down the batch goes back to the queue with a delay. The
// queue owns retries; this holds nothing it can't lose.
type Writer struct {
	Client     *Client
	Log        *slog.Logger
	FlushEvery time.Duration // default 1s
	BatchSize  int           // flush early at this many rows; default 5000

	done chan struct{}
}

// Start runs the batching loop until src's deliveries close.
func (w *Writer) Start(src queue.Source) {
	if w.FlushEvery == 0 {
		w.FlushEvery = time.Second
	}
	if w.BatchSize == 0 {
		w.BatchSize = 5000
	}
	w.done = make(chan struct{})
	go w.loop(src.Deliveries())
}

// Wait returns once the deliveries have closed and the last batch is handled.
func (w *Writer) Wait() { <-w.done }

func (w *Writer) loop(deliveries <-chan queue.Delivery) {
	defer close(w.done)
	var batch []queue.Delivery
	tick := time.NewTicker(w.FlushEvery)
	defer tick.Stop()
	failures := 0
	var retryAt time.Time

	flush := func(ctx context.Context) {
		if len(batch) == 0 {
			return
		}
		delay := min(30*time.Second, time.Second<<min(failures, 5))
		err := w.write(ctx, batch, delay)
		pending := len(batch)
		batch = nil
		if err != nil {
			failures++
			retryAt = time.Now().Add(delay)
			w.Log.Warn("analytics: write failed, the queue will deliver these again", "err", err, "batch", pending, "retry_in", delay)
			return
		}
		if failures > 0 {
			w.Log.Info("analytics: writing again", "after_failures", failures)
		}
		failures, retryAt = 0, time.Time{}
	}

	for {
		select {
		case d, ok := <-deliveries:
			if !ok {
				ctx, cancel := context.WithTimeout(context.Background(), 10*time.Second)
				flush(ctx)
				cancel()
				return
			}
			// ClickHouse just failed: don't try it again yet. Anything that
			// arrives meanwhile waits in the queue, not in memory.
			if wait := time.Until(retryAt); wait > 0 {
				d.Retry(wait)
				continue
			}
			batch = append(batch, d)
			if len(batch) >= w.BatchSize {
				flush(context.Background())
			}
		case <-tick.C:
			flush(context.Background())
		}
	}
}

// write settles every delivery in batch. It returns an error only when
// ClickHouse couldn't take them right now; those are retried after delay.
func (w *Writer) write(ctx context.Context, batch []queue.Delivery, delay time.Duration) error {
	err := w.insert(ctx, batch)
	if err == nil {
		for _, d := range batch {
			d.Ack()
		}
		return nil
	}
	if !isBadData(err) {
		for _, d := range batch {
			d.Retry(delay)
		}
		return err
	}
	// ClickHouse understood the request and refuses this data. Retrying won't
	// change its mind, and with durable delivery one such row would otherwise
	// block every event behind it, forever. Halve until it's alone, keep the
	// good rows, and set the bad one aside.
	if len(batch) == 1 {
		w.Log.Error("analytics: ClickHouse refuses this event; kept as a dead letter", "event_id", batch[0].Event().EventID, "err", err)
		batch[0].Dead(err.Error())
		return nil
	}
	half := len(batch) / 2
	return errors.Join(w.write(ctx, batch[:half], delay), w.write(ctx, batch[half:], delay))
}

// insert writes the identity links first: a person's first event and its
// identify can arrive in one batch, and either order gives the same answer at
// query time. If the events then fail the links are written again on retry,
// which changes nothing: queries take the earliest link.
func (w *Writer) insert(ctx context.Context, batch []queue.Delivery) error {
	var events, aliases []any
	for _, d := range batch {
		e := d.Event()
		events = append(events, Row(e))
		if e.Type == event.TypeIdentify && e.AnonID != "" && e.AnonID != e.DistinctID {
			aliases = append(aliases, aliasRow{ProjectID: e.ProjectID, DistinctID: e.AnonID, PersonID: e.DistinctID, CreatedAt: e.Time.Format(time.RFC3339Nano)})
		}
	}
	if err := w.Client.Insert(ctx, "person_aliases", aliases); err != nil {
		return err
	}
	return w.Client.Insert(ctx, "events", events)
}

// isBadData: ClickHouse answers 400 for data it can't parse. Everything else
// (no answer, out of memory, too many parts, a table not created yet) can pass.
func isBadData(err error) bool {
	var re *ResponseError
	return errors.As(err, &re) && re.Code == http.StatusBadRequest
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
