package ingest

import (
	"context"
	"log/slog"
	"math"
	"sync"
	"time"

	"juju/internal/event"
	"juju/internal/store"
)

// Projects looks up a project's key and limits.
type Projects interface {
	ProjectIngest(ctx context.Context, projectID int64) (store.IngestProject, error)
}

// ProjectCache remembers lookups for TTL, so ingest doesn't query Postgres
// on every request. The cost: a changed limit takes up to TTL to apply.
// "Not found" is cached too, so requests for made-up project ids can't
// turn into a database query each.
//
// When Postgres can't be reached, an expired entry is used anyway (stale
// if error): ingest keeps accepting events for every project it has seen,
// and the durable queue holds them until the database is back. Without
// this, a database outage would stop ingest too, which is exactly what
// the queue between them is meant to prevent.
type ProjectCache struct {
	Src Projects
	TTL time.Duration
	Now func() time.Time

	mu sync.Mutex
	m  map[int64]cached
}

type cached struct {
	p       store.IngestProject
	err     error
	expires time.Time
}

func (c *ProjectCache) ProjectIngest(ctx context.Context, id int64) (store.IngestProject, error) {
	now := c.Now()
	c.mu.Lock()
	e, ok := c.m[id]
	c.mu.Unlock()
	if ok && now.Before(e.expires) {
		return e.p, e.err
	}
	p, err := c.Src.ProjectIngest(ctx, id)
	if err != nil && err != store.ErrNotFound {
		if ok {
			return e.p, e.err // stale, but the best answer we have
		}
		return p, err // a database error is temporary: don't remember it
	}
	c.mu.Lock()
	if c.m == nil {
		c.m = map[int64]cached{}
	}
	c.m[id] = cached{p, err, now.Add(c.TTL)}
	c.mu.Unlock()
	return p, err
}

// Usage is where quota counts are kept (Postgres in production).
type Usage interface {
	MonthUsage(ctx context.Context, projectID int64, month time.Time) (int64, error)
	AddUsage(ctx context.Context, month time.Time, counts map[int64]int64) error
}

// Limits enforces each project's rate limit and monthly quota.
//
// Rate limit: a token bucket per project. Tokens refill at Rate per second
// up to a burst of 5 seconds' worth; each accepted event spends one. The
// burst is there because SDKs send in batches, and an app that was offline
// sends its whole backlog at once when it reconnects.
//
// Both limits are per ingest process: with three ingest replicas, a
// project can get up to three times its rate. Exact global limits would
// need every request to check a shared counter, which is the kind of
// per-request round trip this design avoids. The quota is shared (it's
// counted in Postgres), but each replica only sees the others' counts when
// it flushes, so it can overshoot by a few seconds' worth of events. A
// batch is also let in whole if the project is under its quota when the
// batch arrives, so a quota can be passed by up to one batch (100 events):
// rejecting part of a batch would make SDKs resend the rest forever.
type Limits struct {
	DefaultRate int // events per second for projects with no rate_limit set; 0 = unlimited
	Usage       Usage
	Now         func() time.Time
	Log         *slog.Logger

	mu      sync.Mutex
	buckets map[int64]*bucket
	usage   map[int64]*usage
	carry   map[time.Time]map[int64]int64 // last month's unflushed counts, after a month rolls over
}

type bucket struct {
	tokens float64
	last   time.Time
}

type usage struct {
	month   time.Time
	base    int64 // accepted this month as of the last read from Postgres
	pending int64 // accepted here since the last flush
	loaded  bool  // base has been read at least once
}

// Rejection says why a batch was refused and when to try again.
type Rejection struct {
	Reason     string
	RetryAfter time.Duration
}

// maxQuotaRetry caps Retry-After for a used-up quota. The honest answer is
// "next month", but checking back hourly means a raised quota takes effect
// within the hour, and one rejected request an hour per app costs nothing.
const maxQuotaRetry = time.Hour

// Allow decides whether project p may send n more events now, and if so
// takes n tokens from its bucket. Then call Accepted once the events are
// safely queued, or Refund if queueing failed, so a batch the SDK will
// resend doesn't use up the project's allowance twice.
func (l *Limits) Allow(ctx context.Context, id int64, p store.IngestProject, n int) *Rejection {
	now := l.Now()
	if p.MonthlyQuota != nil {
		u, err := l.monthUsage(ctx, id, now)
		if err != nil {
			// Can't read the count: let events in. Losing a customer's
			// errors is worse than going a little over a quota.
			l.Log.Warn("read quota usage", "project_id", id, "err", err)
		} else if u >= *p.MonthlyQuota {
			wait := min(store.MonthStart(now).AddDate(0, 1, 0).Sub(now), maxQuotaRetry)
			return &Rejection{"monthly event quota reached", wait}
		}
	}

	rate := l.DefaultRate
	if p.RateLimit != nil {
		rate = *p.RateLimit
	}
	if rate <= 0 {
		return nil
	}
	burst := float64(max(5*rate, event.MaxEventsPerEnvelope)) // a full batch must always fit
	l.mu.Lock()
	defer l.mu.Unlock()
	if l.buckets == nil {
		l.buckets = map[int64]*bucket{}
	}
	b, ok := l.buckets[id]
	if !ok {
		b = &bucket{tokens: burst, last: now}
		l.buckets[id] = b
	}
	b.tokens = math.Min(burst, b.tokens+now.Sub(b.last).Seconds()*float64(rate))
	b.last = now
	if b.tokens < float64(n) {
		wait := time.Duration((float64(n) - b.tokens) / float64(rate) * float64(time.Second))
		return &Rejection{"rate limit exceeded", wait}
	}
	b.tokens -= float64(n)
	return nil
}

// Refund gives back the n tokens Allow took for a batch that wasn't queued.
func (l *Limits) Refund(id int64, n int) {
	l.mu.Lock()
	defer l.mu.Unlock()
	if b, ok := l.buckets[id]; ok {
		b.tokens += float64(n) // the next Allow caps it at the burst again
	}
}

// Accepted records n events from project id as accepted: they count
// towards its quota, and were already taken from its bucket by Allow.
func (l *Limits) Accepted(id int64, n int) {
	now := l.Now()
	l.mu.Lock()
	defer l.mu.Unlock()
	l.entry(id, now).pending += int64(n)
}

// entry returns project id's usage for now's month, moving last month's
// unflushed count aside when the month has changed. Call with l.mu held.
func (l *Limits) entry(id int64, now time.Time) *usage {
	month := store.MonthStart(now)
	if l.usage == nil {
		l.usage = map[int64]*usage{}
	}
	u, ok := l.usage[id]
	if !ok {
		u = &usage{month: month}
		l.usage[id] = u
	}
	if !u.month.Equal(month) {
		if u.pending > 0 {
			if l.carry == nil {
				l.carry = map[time.Time]map[int64]int64{}
			}
			if l.carry[u.month] == nil {
				l.carry[u.month] = map[int64]int64{}
			}
			l.carry[u.month][id] += u.pending
		}
		*u = usage{month: month}
	}
	return u
}

// monthUsage returns the project's count for this month, reading Postgres
// the first time. After that, Flush keeps it fresh.
func (l *Limits) monthUsage(ctx context.Context, id int64, now time.Time) (int64, error) {
	l.mu.Lock()
	u := l.entry(id, now)
	if u.loaded {
		n := u.base + u.pending
		l.mu.Unlock()
		return n, nil
	}
	month := u.month
	l.mu.Unlock()

	base, err := l.Usage.MonthUsage(ctx, id, month)
	if err != nil {
		return 0, err
	}
	l.mu.Lock()
	defer l.mu.Unlock()
	u = l.entry(id, now)
	if !u.loaded && u.month.Equal(month) {
		u.base, u.loaded = base, true
	}
	return u.base + u.pending, nil
}

// Flush writes the counts accepted since the last flush to Postgres, then
// rereads the totals of projects with quotas, which picks up what other
// ingest processes counted.
func (l *Limits) Flush(ctx context.Context) error {
	now := l.Now()
	l.mu.Lock()
	batches := l.carry
	l.carry = nil
	if batches == nil {
		batches = map[time.Time]map[int64]int64{}
	}
	var loaded []int64
	for id := range l.usage {
		u := l.entry(id, now)
		if u.pending > 0 {
			if batches[u.month] == nil {
				batches[u.month] = map[int64]int64{}
			}
			batches[u.month][id] += u.pending
			// Moved into base: Postgres will have it after AddUsage, and
			// until then this keeps the in-memory total the same.
			u.base += u.pending
			u.pending = 0
		}
		if u.loaded {
			loaded = append(loaded, id)
		}
	}
	l.mu.Unlock()

	month := store.MonthStart(now)
	for m, counts := range batches {
		if err := l.Usage.AddUsage(ctx, m, counts); err != nil {
			// Put the counts back so the next flush tries again.
			l.mu.Lock()
			for id, n := range counts {
				if m.Equal(month) {
					u := l.entry(id, now)
					u.base -= n
					u.pending += n
				} else {
					if l.carry == nil {
						l.carry = map[time.Time]map[int64]int64{}
					}
					if l.carry[m] == nil {
						l.carry[m] = map[int64]int64{}
					}
					l.carry[m][id] += n
				}
			}
			l.mu.Unlock()
			return err
		}
	}

	for _, id := range loaded {
		base, err := l.Usage.MonthUsage(ctx, id, month)
		if err != nil {
			return err
		}
		l.mu.Lock()
		if u := l.entry(id, now); u.month.Equal(month) {
			u.base = base
		}
		l.mu.Unlock()
	}
	return nil
}

// Run flushes every interval until ctx is done, then once more so a clean
// shutdown doesn't drop the last few seconds of counts.
func (l *Limits) Run(ctx context.Context, interval time.Duration) {
	t := time.NewTicker(interval)
	defer t.Stop()
	for {
		select {
		case <-ctx.Done():
			ctx, cancel := context.WithTimeout(context.Background(), 5*time.Second)
			defer cancel()
			if err := l.Flush(ctx); err != nil {
				l.Log.Error("flush usage at shutdown", "err", err)
			}
			return
		case <-t.C:
			if err := l.Flush(ctx); err != nil {
				l.Log.Warn("flush usage", "err", err)
			}
		}
	}
}
