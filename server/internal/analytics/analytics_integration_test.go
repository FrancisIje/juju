//go:build integration

package analytics

import (
	"context"
	"fmt"
	"io"
	"log/slog"
	"math/rand/v2"
	"net/http"
	"net/http/httptest"
	"net/http/httputil"
	"net/url"
	"os"
	"sync/atomic"
	"testing"
	"time"

	"juju/internal/event"
	"juju/internal/testch"
)

// The tests run against a real ClickHouse, like the store tests run against
// a real Postgres: query syntax, merges, timezones and parameters are exactly
// what a fake would get wrong.
var chURL string

func TestMain(m *testing.M) { os.Exit(run(m)) }

func run(m *testing.M) int {
	u, stop, err := testch.Start(18123)
	if err != nil {
		fmt.Fprintln(os.Stderr, err)
		return 1
	}
	defer stop()
	chURL = u
	return m.Run()
}

// newClient makes a client with a database of its own, so tests never see
// each other's rows.
func newClient(t *testing.T) *Client {
	t.Helper()
	c := &Client{URL: chURL, Database: fmt.Sprintf("test_%d", rand.Int64N(1<<40))}
	ctx := context.Background()
	if err := c.Migrate(ctx); err != nil {
		t.Fatal(err)
	}
	if err := c.Migrate(ctx); err != nil { // runs on every start: must be safe to repeat
		t.Fatalf("second Migrate: %v", err)
	}
	t.Cleanup(func() { _ = c.Exec(context.Background(), "DROP DATABASE IF EXISTS "+c.Database) })
	return c
}

var quiet = slog.New(slog.NewTextHandler(io.Discard, nil))

var day0 = time.Now().UTC().Truncate(24*time.Hour).AddDate(0, 0, -6) // 6 days ago, midnight

type ev struct {
	day      int // days after day0
	hour     int
	name     string
	who      string
	lib      string
	props    map[string]any
	id       string // event_id; random if empty
	identify string // for identify events: the anon id being linked
}

// write stores events through the Writer, the same path production uses.
func write(t *testing.T, c *Client, project int64, evs []ev) {
	t.Helper()
	w := &Writer{Client: c, Log: quiet, FlushEvery: 50 * time.Millisecond}
	w.Start()
	for _, e := range evs {
		id := e.id
		if id == "" {
			id = fmt.Sprintf("%08x-0000-4000-8000-%012x", rand.Uint32(), rand.Uint64()&0xffffffffffff)
		}
		x := &event.Event{EventID: id, Type: event.TypeEvent, Name: e.name, DistinctID: e.who, Properties: e.props,
			Platform: "javascript", Environment: "production", ProjectID: project,
			Time: day0.AddDate(0, 0, e.day).Add(time.Duration(e.hour) * time.Hour), ReceivedAt: time.Now()}
		x.SDK.Name = e.lib
		if e.identify != "" {
			x.Type, x.Name, x.AnonID = event.TypeIdentify, "$identify", e.identify
		}
		w.Add(x)
	}
	w.Stop()
	if w.Dropped() != 0 {
		t.Fatalf("writer dropped %d events", w.Dropped())
	}
}

func week() Range { return Range{From: day0, To: day0.AddDate(0, 0, 7)} }

func TestTrendCountsByDayAndBreakdown(t *testing.T) {
	c := newClient(t)
	dup := "11111111-1111-4111-8111-111111111111"
	write(t, c, 1, []ev{
		{day: 0, name: "checkout_started", who: "a", lib: "juju.web", props: map[string]any{"plan": "pro"}},
		{day: 0, name: "checkout_started", who: "b", lib: "juju.flutter", props: map[string]any{"plan": "free"}},
		{day: 2, name: "checkout_started", who: "a", lib: "juju.web", id: dup},
		{day: 2, name: "checkout_started", who: "c", lib: "juju.react-native"},
		{day: 3, name: "signed_up", who: "a", lib: "juju.web"}, // another event: not counted
	})
	// An SDK retry: the same event again, in a separate insert. (Copies in
	// one insert are merged by ClickHouse right away; across inserts they
	// stay until a background merge, so counts must not rely on it.)
	write(t, c, 1, []ev{{day: 2, name: "checkout_started", who: "a", lib: "juju.web", id: dup}})
	write(t, c, 2, []ev{{day: 0, name: "checkout_started", who: "z", lib: "juju.web"}}) // another project
	ctx := context.Background()

	res, err := c.Trend(ctx, 1, TrendQuery{Event: "checkout_started", Range: week(), Interval: Day})
	if err != nil {
		t.Fatal(err)
	}
	if len(res.Buckets) != 7 || len(res.Series) != 1 {
		t.Fatalf("buckets %d, series %d", len(res.Buckets), len(res.Series))
	}
	if got := res.Series[0].Values; fmt.Sprint(got) != "[2 0 2 0 0 0 0]" {
		t.Errorf("per day = %v, want [2 0 2 0 0 0 0] (the retried event counted once, other project excluded)", got)
	}

	res, err = c.Trend(ctx, 1, TrendQuery{Event: "checkout_started", Range: week(), Interval: Day, Breakdown: "lib"})
	if err != nil {
		t.Fatal(err)
	}
	got := map[string]int64{}
	for _, s := range res.Series {
		got[s.Label] = s.Total
	}
	if fmt.Sprint(got) != "map[juju.flutter:1 juju.react-native:1 juju.web:2]" {
		t.Errorf("by SDK = %v", got)
	}
	if res.Series[0].Label != "juju.web" {
		t.Errorf("biggest first: %v", res.Series[0].Label)
	}

	res, err = c.Trend(ctx, 1, TrendQuery{Event: "checkout_started", Range: week(), Interval: Day, Breakdown: "property:plan"})
	if err != nil {
		t.Fatal(err)
	}
	got = map[string]int64{}
	for _, s := range res.Series {
		got[s.Label] = s.Total
	}
	if got["pro"] != 1 || got["free"] != 1 || got[""] != 2 {
		t.Errorf("by plan = %v (no plan sent: label \"\")", got)
	}

	res, _ = c.Trend(ctx, 1, TrendQuery{Event: "checkout_started", Range: week(), Interval: Day, People: true})
	if res.Series[0].Total != 4 || res.Series[0].Values[2] != 2 { // a, b on day 0; a, c on day 2
		t.Errorf("people per day = %v", res.Series[0].Values)
	}
}

func TestIdentifyMergesAnonymousAndKnownUser(t *testing.T) {
	c := newClient(t)
	write(t, c, 1, []ev{
		{day: 0, name: "viewed_pricing", who: "anon-1"},
		{day: 0, hour: 1, who: "user_42", identify: "anon-1"},
		{day: 0, hour: 2, name: "viewed_pricing", who: "user_42"},
		// A second identify of the same device to another account doesn't
		// move anon-1's history: the first link wins.
		{day: 0, hour: 3, who: "user_99", identify: "anon-1"},
		{day: 0, hour: 4, name: "viewed_pricing", who: "anon-2"},
	})
	res, err := c.Trend(context.Background(), 1, TrendQuery{Event: "viewed_pricing", Range: week(), Interval: Day, People: true})
	if err != nil {
		t.Fatal(err)
	}
	if res.Series[0].Total != 2 { // user_42 (anon-1 + user_42), and anon-2
		t.Errorf("people = %d, want 2", res.Series[0].Total)
	}
	// Still right after ClickHouse merges parts (the reason aliases aren't a ReplacingMergeTree).
	if err := c.Exec(context.Background(), "OPTIMIZE TABLE person_aliases FINAL"); err != nil {
		t.Fatal(err)
	}
	res, _ = c.Trend(context.Background(), 1, TrendQuery{Event: "viewed_pricing", Range: week(), Interval: Day, People: true})
	if res.Series[0].Total != 2 {
		t.Errorf("after merge: people = %d, want 2", res.Series[0].Total)
	}
}

func TestFunnelOrderAndWindow(t *testing.T) {
	c := newClient(t)
	write(t, c, 1, []ev{
		// a: all three in order, within an hour
		{day: 0, hour: 1, name: "viewed", who: "a"}, {day: 0, hour: 1, name: "added", who: "a"}, {day: 0, hour: 2, name: "paid", who: "a"},
		// b: viewed then added
		{day: 0, hour: 1, name: "viewed", who: "b"}, {day: 0, hour: 3, name: "added", who: "b"},
		// c: added before viewed: only step 1 counts
		{day: 1, hour: 1, name: "added", who: "c"}, {day: 1, hour: 2, name: "viewed", who: "c"},
		// d: paid 3 days later: outside a 24h window
		{day: 1, name: "viewed", who: "d"}, {day: 1, hour: 1, name: "added", who: "d"}, {day: 4, name: "paid", who: "d"},
	})
	steps, err := c.Funnel(context.Background(), 1, FunnelQuery{Steps: []string{"viewed", "added", "paid"}, Range: week(), Window: 24 * time.Hour})
	if err != nil {
		t.Fatal(err)
	}
	got := fmt.Sprint(steps[0].People, steps[1].People, steps[2].People)
	if got != "4 3 1" {
		t.Errorf("funnel = %s, want 4 3 1", got)
	}
}

func TestRetentionCohorts(t *testing.T) {
	c := newClient(t)
	write(t, c, 1, []ev{
		{day: 0, name: "signed_up", who: "a"}, {day: 1, name: "opened", who: "a"}, {day: 2, name: "opened", who: "a"},
		{day: 0, name: "signed_up", who: "b"}, {day: 2, name: "opened", who: "b"},
		{day: 0, name: "opened", who: "b"}, // the same day as signing up: not a return
		{day: 1, name: "signed_up", who: "c"}, {day: 2, name: "opened", who: "c"},
	})
	cohorts, err := c.Retention(context.Background(), 1, RetentionQuery{Start: "signed_up", Return: "opened", Range: Range{From: day0, To: day0.AddDate(0, 0, 2)}, Periods: 3})
	if err != nil {
		t.Fatal(err)
	}
	if len(cohorts) != 2 {
		t.Fatalf("%d cohorts", len(cohorts))
	}
	if got := fmt.Sprint(cohorts[0].People, cohorts[0].Returned); got != "2 [1 2 0]" {
		t.Errorf("day 0 cohort = %s, want 2 [1 2 0]", got)
	}
	if got := fmt.Sprint(cohorts[1].People, cohorts[1].Returned); got != "1 [1 0 0]" {
		t.Errorf("day 1 cohort = %s, want 1 [1 0 0]", got)
	}
}

func TestEventNamesPropertiesAndInjection(t *testing.T) {
	c := newClient(t)
	write(t, c, 1, []ev{
		{day: 0, name: "signed_up", who: "a", props: map[string]any{"plan": "pro", "ref": "ad"}},
		{day: 1, name: "signed_up", who: "b", props: map[string]any{"plan": "free"}},
		{day: 1, name: "opened", who: "b"},
	})
	ctx := context.Background()
	names, err := c.EventNames(ctx, 1, week())
	if err != nil || len(names) != 2 || names[0].Name != "signed_up" || names[0].Count != 2 {
		t.Fatalf("names = %+v, %v", names, err)
	}
	keys, err := c.PropertyKeys(ctx, 1, "signed_up", week())
	if err != nil || fmt.Sprint(keys) != "[plan ref]" {
		t.Fatalf("keys = %v, %v", keys, err)
	}
	// Values are parameters, never SQL: a hostile event name is just a name
	// that matches nothing.
	for _, evil := range []string{"x' OR 1=1 --", "signed_up') OR ('1'='1", "\\'; DROP TABLE events; --"} {
		res, err := c.Trend(ctx, 1, TrendQuery{Event: evil, Range: week(), Interval: Day, Breakdown: "property:" + evil})
		if err != nil {
			t.Fatalf("%q: %v", evil, err)
		}
		if res.Series[0].Total != 0 {
			t.Errorf("%q matched %d events", evil, res.Series[0].Total)
		}
	}
	if names, _ := c.EventNames(ctx, 1, week()); len(names) != 2 {
		t.Error("the events table should still be there")
	}
	// A breakdown that isn't on the list is refused before any SQL is built.
	if _, err := c.Trend(ctx, 1, TrendQuery{Event: "opened", Range: week(), Interval: Day, Breakdown: "name; DROP TABLE events"}); !IsBadQuery(err) {
		t.Errorf("unknown breakdown: err = %v", err)
	}
}

// ClickHouse briefly failing (a restart, a deploy) must not lose events:
// the writer keeps the batch and retries.
func TestWriterRetriesWhenClickHouseFails(t *testing.T) {
	c := newClient(t)
	target, _ := url.Parse(chURL)
	proxy := httputil.NewSingleHostReverseProxy(target)
	var calls atomic.Int32
	flaky := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		if calls.Add(1) <= 2 {
			http.Error(w, "down for a moment", http.StatusServiceUnavailable)
			return
		}
		proxy.ServeHTTP(w, r)
	}))
	defer flaky.Close()

	w := &Writer{Client: &Client{URL: flaky.URL, Database: c.Database}, Log: quiet, FlushEvery: 50 * time.Millisecond}
	w.Start()
	for i := range 3 {
		x := &event.Event{EventID: fmt.Sprintf("22222222-2222-4222-8222-%012d", i), Type: event.TypeEvent, Name: "tap", DistinctID: "a", ProjectID: 1, Time: day0, ReceivedAt: day0}
		w.Add(x)
	}
	deadline := time.Now().Add(15 * time.Second)
	for {
		res, err := c.Trend(context.Background(), 1, TrendQuery{Event: "tap", Range: week(), Interval: Day})
		if err == nil && res.Series[0].Total == 3 {
			break
		}
		if time.Now().After(deadline) {
			t.Fatalf("events never arrived after ClickHouse recovered (calls: %d)", calls.Load())
		}
		time.Sleep(100 * time.Millisecond)
	}
	w.Stop()
	if calls.Load() < 3 {
		t.Errorf("expected retries, got %d calls", calls.Load())
	}
}
