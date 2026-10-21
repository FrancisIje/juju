package ingest

import (
	"context"
	"errors"
	"fmt"
	"io"
	"log/slog"
	"net/http"
	"net/http/httptest"
	"strings"
	"sync"
	"testing"
	"time"

	"juju/internal/store"
)

// clock is a fake time source: tests move it forward by hand, so "wait a
// second" takes no time and every run gives the same answer.
type clock struct{ t time.Time }

func (c *clock) Now() time.Time           { return c.t }
func (c *clock) Add(d time.Duration)      { c.t = c.t.Add(d) }
func newClock(s string) *clock            { t, _ := time.Parse(time.RFC3339, s); return &clock{t} }
func ptr[T any](v T) *T                   { return &v }
func quiet() *slog.Logger                 { return slog.New(slog.NewTextHandler(io.Discard, nil)) }
func newLimits(c *clock, u Usage) *Limits { return &Limits{Usage: u, Now: c.Now, Log: quiet()} }

// fakeUsage is project_usage in memory, shared between Limits values the
// way Postgres is shared between ingest processes.
type fakeUsage struct {
	mu     sync.Mutex
	rows   map[time.Time]map[int64]int64
	broken bool
}

func (f *fakeUsage) MonthUsage(_ context.Context, id int64, month time.Time) (int64, error) {
	f.mu.Lock()
	defer f.mu.Unlock()
	if f.broken {
		return 0, errors.New("database down")
	}
	return f.rows[month][id], nil
}

func (f *fakeUsage) AddUsage(_ context.Context, month time.Time, counts map[int64]int64) error {
	f.mu.Lock()
	defer f.mu.Unlock()
	if f.broken {
		return errors.New("database down")
	}
	if f.rows == nil {
		f.rows = map[time.Time]map[int64]int64{}
	}
	if f.rows[month] == nil {
		f.rows[month] = map[int64]int64{}
	}
	for id, n := range counts {
		f.rows[month][id] += n
	}
	return nil
}

func (f *fakeUsage) get(month string, id int64) int64 {
	m, _ := time.Parse("2006-01", month)
	f.mu.Lock()
	defer f.mu.Unlock()
	return f.rows[m][id]
}

// send is what the handler does: ask, and on success, record.
func send(l *Limits, p store.IngestProject, n int) *Rejection {
	rej := l.Allow(context.Background(), 1, p, n)
	if rej == nil {
		l.Accepted(1, n)
	}
	return rej
}

func TestRateLimitTokenBucket(t *testing.T) {
	c := newClock("2026-10-21T12:00:00Z")
	l := newLimits(c, &fakeUsage{})
	p := store.IngestProject{RateLimit: ptr(10)} // 10/s; the burst is a full batch (100)

	if rej := send(l, p, 100); rej != nil {
		t.Fatalf("a full batch must fit in the burst: %+v", rej)
	}
	rej := send(l, p, 5)
	if rej == nil || rej.Reason != "rate limit exceeded" {
		t.Fatalf("bucket is empty, want rate limited, got %+v", rej)
	}
	if rej.RetryAfter != 500*time.Millisecond { // 5 events at 10/s
		t.Errorf("RetryAfter = %v, want 500ms", rej.RetryAfter)
	}

	c.Add(time.Second) // refills 10 tokens
	if rej := send(l, p, 10); rej != nil {
		t.Errorf("after 1s, 10 events should fit: %+v", rej)
	}
	if rej := send(l, p, 1); rej == nil {
		t.Error("and the 11th should not")
	}

	c.Add(time.Hour) // refill stops at the burst, it doesn't bank an hour
	if rej := send(l, p, 100); rej != nil {
		t.Errorf("full again after an hour: %+v", rej)
	}
	if rej := send(l, p, 1); rej == nil {
		t.Error("bucket must cap at the burst")
	}
}

func TestRateLimitDefaultAndOverride(t *testing.T) {
	c := newClock("2026-10-21T12:00:00Z")
	l := newLimits(c, &fakeUsage{})
	l.DefaultRate = 20 // burst 100

	if rej := send(l, store.IngestProject{}, 100); rej != nil {
		t.Fatal(rej)
	}
	if send(l, store.IngestProject{}, 1) == nil {
		t.Error("project without its own limit should get the default")
	}
	// rate_limit = 0 means unlimited for this project, whatever the default.
	for i := 0; i < 50; i++ {
		if rej := l.Allow(context.Background(), 2, store.IngestProject{RateLimit: ptr(0)}, 100); rej != nil {
			t.Fatalf("unlimited project was limited: %+v", rej)
		}
	}
}

func TestQuotaCountsAcrossProcessesAndResetsMonthly(t *testing.T) {
	c := newClock("2026-10-31T23:00:00Z")
	db := &fakeUsage{}
	a, b := newLimits(c, db), newLimits(c, db) // two ingest replicas
	p := store.IngestProject{MonthlyQuota: ptr(int64(150))}

	if send(a, p, 100) != nil || send(b, p, 40) != nil {
		t.Fatal("under quota")
	}
	// Each replica only knows its own count until they flush.
	if err := a.Flush(context.Background()); err != nil {
		t.Fatal(err)
	}
	if err := b.Flush(context.Background()); err != nil {
		t.Fatal(err)
	}
	if got := db.get("2026-10", 1); got != 140 {
		t.Fatalf("stored usage = %d, want 140", got)
	}
	if err := a.Flush(context.Background()); err != nil { // a rereads: sees b's 40
		t.Fatal(err)
	}
	if send(a, p, 10) != nil {
		t.Fatal("140 + 10 is exactly the quota, still allowed")
	}
	rej := send(a, p, 1)
	if rej == nil || rej.Reason != "monthly event quota reached" {
		t.Fatalf("want quota rejection, got %+v", rej)
	}
	if rej.RetryAfter != time.Hour {
		t.Errorf("RetryAfter = %v, want capped at 1h", rej.RetryAfter)
	}

	c.Add(30 * time.Minute)
	if rej := send(a, p, 1); rej.RetryAfter != 30*time.Minute {
		t.Errorf("30 min before the month ends, RetryAfter = %v, want 30m", rej.RetryAfter)
	}

	c.Add(time.Hour) // November
	if rej := send(a, p, 5); rej != nil {
		t.Fatalf("new month, fresh quota: %+v", rej)
	}
	if err := a.Flush(context.Background()); err != nil {
		t.Fatal(err)
	}
	if got := db.get("2026-10", 1); got != 150 {
		t.Errorf("October = %d, want 150 (the 10 sent before midnight must land in October)", got)
	}
	if got := db.get("2026-11", 1); got != 5 {
		t.Errorf("November = %d, want 5", got)
	}
}

func TestQuotaFailsOpenAndKeepsCountsWhenDatabaseIsDown(t *testing.T) {
	c := newClock("2026-10-21T12:00:00Z")
	db := &fakeUsage{broken: true}
	l := newLimits(c, db)
	p := store.IngestProject{MonthlyQuota: ptr(int64(10))}

	// Can't read usage: accept rather than drop a customer's errors.
	if rej := send(l, p, 5); rej != nil {
		t.Fatalf("want fail-open, got %+v", rej)
	}
	if err := l.Flush(context.Background()); err == nil {
		t.Fatal("flush should report the broken database")
	}
	db.broken = false
	if err := l.Flush(context.Background()); err != nil {
		t.Fatal(err)
	}
	if got := db.get("2026-10", 1); got != 5 {
		t.Errorf("count from the failed flush was lost: stored %d, want 5", got)
	}
}

func TestProjectCache(t *testing.T) {
	c := newClock("2026-10-21T12:00:00Z")
	src := &countingProjects{}
	pc := &ProjectCache{Src: src, TTL: 30 * time.Second, Now: c.Now}
	ctx := context.Background()

	pc.ProjectIngest(ctx, 1)
	pc.ProjectIngest(ctx, 1)
	if src.calls != 1 {
		t.Errorf("calls = %d, want 1 (second lookup cached)", src.calls)
	}
	c.Add(31 * time.Second)
	pc.ProjectIngest(ctx, 1)
	if src.calls != 2 {
		t.Errorf("calls = %d, want 2 (expired)", src.calls)
	}

	// Unknown ids are cached, so probing them doesn't cost a query each.
	pc.ProjectIngest(ctx, 404)
	if _, err := pc.ProjectIngest(ctx, 404); err != store.ErrNotFound {
		t.Errorf("err = %v, want ErrNotFound", err)
	}
	if src.calls != 3 {
		t.Errorf("calls = %d, want 3", src.calls)
	}

	// A database error is temporary and must not be remembered.
	src.broken = true
	pc.ProjectIngest(ctx, 7)
	src.broken = false
	if _, err := pc.ProjectIngest(ctx, 7); err != nil {
		t.Errorf("error was cached: %v", err)
	}

	// Database down: a project seen before is still served, though its
	// entry has expired, so ingest keeps accepting into the queue.
	c.Add(time.Hour)
	src.broken = true
	if p, err := pc.ProjectIngest(ctx, 7); err != nil || p.PublicKey != key {
		t.Errorf("stale entry not used during an outage: %+v, %v", p, err)
	}
	if _, err := pc.ProjectIngest(ctx, 8); err == nil {
		t.Error("a project never seen can't be checked: want the error")
	}
}

type countingProjects struct {
	calls  int
	broken bool
}

func (p *countingProjects) ProjectIngest(_ context.Context, id int64) (store.IngestProject, error) {
	p.calls++
	switch {
	case p.broken:
		return store.IngestProject{}, errors.New("database down")
	case id == 404:
		return store.IngestProject{}, store.ErrNotFound
	}
	return store.IngestProject{PublicKey: key}, nil
}

type limitedProjects struct{ p store.IngestProject }

func (l limitedProjects) ProjectIngest(context.Context, int64) (store.IngestProject, error) {
	return l.p, nil
}

// Through HTTP: the SDK must get 429 with a Retry-After it can obey, and a
// batch the queue refused must not be counted against the project.
func TestHandlerRateLimitAndQuota(t *testing.T) {
	c := newClock("2026-10-21T12:00:00Z")
	db := &fakeUsage{}
	q := &fakeQueue{room: 1}
	h := &Handler{
		Projects: limitedProjects{store.IngestProject{PublicKey: key, RateLimit: ptr(1), MonthlyQuota: ptr(int64(1000))}},
		Limits:   newLimits(c, db),
		Queue:    q,
		Log:      quiet(),
		Now:      c.Now,
	}
	mux := http.NewServeMux()
	mux.Handle("POST /api/v1/projects/{id}/events", h)
	post := func(n int) *httptest.ResponseRecorder {
		evs := make([]string, n)
		for i := range evs {
			evs[i] = goodEvent(uuid(i + n*1000))
		}
		req := httptest.NewRequest("POST", "/api/v1/projects/1/events", strings.NewReader(envelope(evs...)))
		req.Header.Set("Content-Type", "application/json")
		req.Header.Set("X-Juju-Key", key)
		rec := httptest.NewRecorder()
		mux.ServeHTTP(rec, req)
		return rec
	}

	// Queue has room for one event: a batch of two is refused (429, busy)...
	if rec := post(2); rec.Code != http.StatusTooManyRequests || !strings.Contains(rec.Body.String(), "busy") {
		t.Fatalf("want queue-full 429, got %d %s", rec.Code, rec.Body)
	}
	h.Limits.Flush(context.Background())
	if got := db.get("2026-10", 1); got != 0 {
		t.Errorf("refused batch was counted: usage %d", got)
	}

	q.room = 1000
	// Burst is 100 (a full batch). Spend it, then one more is limited.
	if rec := post(98); rec.Code != http.StatusAccepted {
		t.Fatalf("got %d %s", rec.Code, rec.Body)
	}
	rec := post(3)
	if rec.Code != http.StatusTooManyRequests || !strings.Contains(rec.Body.String(), "rate limit") {
		t.Fatalf("want rate limited, got %d %s", rec.Code, rec.Body)
	}
	if ra := rec.Header().Get("Retry-After"); ra != "1" {
		t.Errorf("Retry-After = %q, want 1 (rounded up, never 0)", ra)
	}
	h.Limits.Flush(context.Background())
	if got := db.get("2026-10", 1); got != 98 {
		t.Errorf("usage = %d, want 98", got)
	}
}

// uuid makes a valid, distinct event id from n.
func uuid(n int) string { return fmt.Sprintf("8f14e45f-ceea-4f6a-9b1e-%012d", n) }
