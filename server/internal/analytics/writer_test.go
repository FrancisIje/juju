package analytics

import (
	"fmt"
	"io"
	"log/slog"
	"net/http"
	"net/http/httptest"
	"slices"
	"strings"
	"sync"
	"sync/atomic"
	"testing"
	"time"

	"juju/internal/event"
	"juju/internal/queue"
)

func TestOSAndBrowserFromContext(t *testing.T) {
	tests := []struct {
		name    string
		ctx     map[string]any
		os, brw string
	}{
		{"android app", map[string]any{"os": map[string]any{"name": "android", "version": "15"}}, "Android", ""},
		{"ios app", map[string]any{"os": map[string]any{"name": "iOS"}}, "iOS", ""},
		{"chrome on mac", ua("Mozilla/5.0 (Macintosh; Intel Mac OS X 10_15_7) AppleWebKit/537.36 (KHTML, like Gecko) Chrome/141.0.0.0 Safari/537.36"), "macOS", "Chrome"},
		{"safari on iphone", ua("Mozilla/5.0 (iPhone; CPU iPhone OS 18_0 like Mac OS X) AppleWebKit/605.1.15 (KHTML, like Gecko) Version/18.0 Mobile/15E148 Safari/604.1"), "iOS", "Safari"},
		{"chrome on iphone says Safari and Mac OS X too", ua("Mozilla/5.0 (iPhone; CPU iPhone OS 18_0 like Mac OS X) AppleWebKit/605.1.15 (KHTML, like Gecko) CriOS/141.0 Mobile/15E148 Safari/604.1"), "iOS", "Chrome"},
		{"edge on windows says Chrome too", ua("Mozilla/5.0 (Windows NT 10.0; Win64; x64) AppleWebKit/537.36 (KHTML, like Gecko) Chrome/141.0.0.0 Safari/537.36 Edg/141.0.0.0"), "Windows", "Edge"},
		{"firefox on android", ua("Mozilla/5.0 (Android 15; Mobile; rv:141.0) Gecko/141.0 Firefox/141.0"), "Android", "Firefox"},
		{"nothing", nil, "", ""},
	}
	for _, tc := range tests {
		t.Run(tc.name, func(t *testing.T) {
			os, brw := clientOf(tc.ctx)
			if os != tc.os || brw != tc.brw {
				t.Errorf("got %q %q, want %q %q", os, brw, tc.os, tc.brw)
			}
		})
	}
}

func ua(s string) map[string]any { return map[string]any{"browser": map[string]any{"user_agent": s}} }

// fakeDelivery records what the writer decided, in order.
type fakeDelivery struct {
	e   *event.Event
	log *[]string
	mu  *sync.Mutex
}

func (d *fakeDelivery) note(s string) {
	d.mu.Lock()
	defer d.mu.Unlock()
	*d.log = append(*d.log, s+" "+d.e.Name)
}
func (d *fakeDelivery) Event() *event.Event   { return d.e }
func (d *fakeDelivery) Attempt() int          { return 1 }
func (d *fakeDelivery) Ack()                  { d.note("ack") }
func (d *fakeDelivery) Retry(_ time.Duration) { d.note("retry") }
func (d *fakeDelivery) Dead(_ string)         { d.note("dead") }

type fakeSource struct{ ch chan queue.Delivery }

func (s fakeSource) Deliveries() <-chan queue.Delivery { return s.ch }

// runWriter sends the named events through a Writer against a fake ClickHouse
// and returns what happened, inserts and decisions in one timeline.
func runWriter(t *testing.T, names []string, respond func(body string) int) []string {
	t.Helper()
	var mu sync.Mutex
	var timeline []string
	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		raw, _ := io.ReadAll(r.Body)
		code := respond(string(raw))
		mu.Lock()
		timeline = append(timeline, fmt.Sprintf("insert %d: %d rows", code, strings.Count(string(raw), "\n")))
		mu.Unlock()
		w.WriteHeader(code)
	}))
	defer srv.Close()
	src := fakeSource{ch: make(chan queue.Delivery, len(names))}
	for _, n := range names {
		src.ch <- &fakeDelivery{e: &event.Event{EventID: "e-" + n, Type: event.TypeEvent, Name: n, DistinctID: "a", Time: time.Now(), ReceivedAt: time.Now()}, log: &timeline, mu: &mu}
	}
	close(src.ch)
	w := &Writer{Client: &Client{URL: srv.URL}, Log: slog.New(slog.NewTextHandler(io.Discard, nil)), FlushEvery: time.Hour}
	w.Start(src)
	w.Wait()
	mu.Lock()
	defer mu.Unlock()
	return timeline
}

// The whole point: acknowledging first is how a crash lost accepted events.
func TestWriterAcknowledgesOnlyAfterClickHouseHasTheEvents(t *testing.T) {
	got := runWriter(t, []string{"a", "b"}, func(string) int { return 200 })
	want := []string{"insert 200: 2 rows", "ack a", "ack b"}
	if !slices.Equal(got, want) {
		t.Errorf("timeline = %q, want %q", got, want)
	}
}

// Down or overloaded: nothing is acknowledged, everything goes back.
func TestWriterGivesBackTheBatchWhenClickHouseFails(t *testing.T) {
	for _, code := range []int{500, 503, 404} { // 404: the table isn't created yet
		got := runWriter(t, []string{"a", "b"}, func(string) int { return code })
		want := []string{fmt.Sprintf("insert %d: 2 rows", code), "retry a", "retry b"}
		if !slices.Equal(got, want) {
			t.Errorf("%d: timeline = %q, want %q", code, got, want)
		}
	}
}

// One row ClickHouse refuses must not block the rest, now or after a restart.
func TestWriterSetsAsideOnlyTheRowClickHouseRefuses(t *testing.T) {
	got := runWriter(t, []string{"a", "bad", "c", "d"}, func(body string) int {
		if strings.Contains(body, `"bad"`) {
			return 400
		}
		return 200
	})
	var settled []string
	for _, s := range got {
		if !strings.HasPrefix(s, "insert") {
			settled = append(settled, s)
		}
	}
	slices.Sort(settled)
	if want := []string{"ack a", "ack c", "ack d", "dead bad"}; !slices.Equal(settled, want) {
		t.Errorf("settled = %q, want %q (timeline %q)", settled, want, got)
	}
}

// ClickHouse failing part-way through splitting: bad data set aside, the rest
// retried, never acknowledged unwritten.
func TestWriterSplitDuringAnOutageLosesNothing(t *testing.T) {
	var calls atomic.Int32
	got := runWriter(t, []string{"a", "b"}, func(string) int {
		if calls.Add(1) == 1 {
			return 400
		}
		return 503
	})
	if want := []string{"insert 400: 2 rows", "insert 503: 1 rows", "retry a", "insert 503: 1 rows", "retry b"}; !slices.Equal(got, want) {
		t.Errorf("timeline = %q, want %q", got, want)
	}
}

// Stopping with ClickHouse unreachable must end promptly, handing events back.
func TestWriterStopsWhenClickHouseIsUnreachable(t *testing.T) {
	var mu sync.Mutex
	var log []string
	src := fakeSource{ch: make(chan queue.Delivery, 1)}
	src.ch <- &fakeDelivery{e: &event.Event{EventID: "e", Type: event.TypeEvent, Name: "x", DistinctID: "a", Time: time.Now(), ReceivedAt: time.Now()}, log: &log, mu: &mu}
	close(src.ch)
	w := &Writer{Client: &Client{URL: "http://127.0.0.1:1"}, Log: slog.New(slog.NewTextHandler(io.Discard, nil)), FlushEvery: time.Hour}
	w.Start(src)
	done := make(chan struct{})
	go func() { w.Wait(); close(done) }()
	select {
	case <-done:
	case <-time.After(15 * time.Second):
		t.Fatal("Wait hung with ClickHouse unreachable")
	}
	if !slices.Equal(log, []string{"retry x"}) {
		t.Errorf("decisions = %q, want the event handed back", log)
	}
}
