//go:build integration

package store_test

import (
	"context"
	"encoding/json"
	"errors"
	"fmt"
	"math"
	"testing"
	"time"

	"github.com/google/uuid"

	"juju/internal/event"
	"juju/internal/store"
)

type sessionUpdate struct {
	sid      string
	seq      int64
	status   string
	errors   int
	release  string
	env      string
	user     string
	duration int64 // 0: not sent
}

func newSession(t *testing.T, projectID int64, u sessionUpdate) *event.Event {
	t.Helper()
	s := map[string]any{"sid": u.sid, "seq": u.seq, "status": u.status, "errors": u.errors, "started": time.Now().Add(-time.Minute).UTC().Format(time.RFC3339Nano)}
	if u.duration > 0 {
		s["duration_ms"] = u.duration
	}
	m := map[string]any{
		"event_id": uuid.NewString(), "type": "session", "platform": "javascript",
		"timestamp": time.Now().UTC().Format(time.RFC3339Nano), "release": u.release, "environment": u.env, "session": s,
	}
	if u.user != "" {
		m["user"] = map[string]any{"id": u.user}
	}
	raw, _ := json.Marshal(m)
	e, err := event.Normalize(raw, time.Now())
	if err != nil {
		t.Fatal(err)
	}
	e.ProjectID = projectID
	return e
}

func saveSessions(t *testing.T, s *store.Store, projectID int64, updates ...sessionUpdate) {
	t.Helper()
	for _, u := range updates {
		if _, err := s.SaveSession(context.Background(), newSession(t, projectID, u)); err != nil {
			t.Fatal(err)
		}
	}
}

func health(t *testing.T, s *store.Store, projectID int64, env string) store.ReleaseHealth {
	t.Helper()
	h, err := s.ReleaseHealth(context.Background(), projectID, store.Window{Since: time.Now().Add(-24 * time.Hour), Bucket: "hour", Environment: env})
	if err != nil {
		t.Fatal(err)
	}
	return h
}

// The same updates in every order must give the same row. Offline queues
// and retries deliver them late, twice, or shuffled.
func TestSessionUpdatesMergeInAnyOrder(t *testing.T) {
	start := sessionUpdate{seq: 0, status: "ok", release: "r1", env: "production"}
	errored := sessionUpdate{seq: 1, status: "ok", errors: 2, release: "r1", env: "production", user: "u1"}
	end := sessionUpdate{seq: 2, status: "exited", errors: 2, release: "r1", env: "production", duration: 60_000}
	orders := [][]sessionUpdate{
		{start, errored, end},
		{end, errored, start},
		{errored, start, end, end, start}, // with repeats
		{end, start, errored},
	}
	for i, order := range orders {
		t.Run(fmt.Sprint(i), func(t *testing.T) {
			s, p := newProject(t)
			sid := uuid.NewString()
			for j := range order {
				order[j].sid = sid
			}
			saveSessions(t, s, p.ID, order...)
			h := health(t, s, p.ID, "")
			if h.Totals.Sessions != 1 {
				t.Fatalf("order %d: %d sessions, want 1: updates of one session must merge", i, h.Totals.Sessions)
			}
			var status string
			var errs int
			var dur *int64
			var user string
			if err := pool.QueryRow(context.Background(), `SELECT status, errors, duration_ms, user_key FROM app_sessions WHERE project_id = $1`, p.ID).Scan(&status, &errs, &dur, &user); err != nil {
				t.Fatal(err)
			}
			// The status comes from seq 2 even when seq 0 arrived after it;
			// the user set mid-session survives updates without one.
			if status != "exited" || errs != 2 || dur == nil || *dur != 60_000 || user != "id:u1" {
				t.Errorf("order %d: status %s, errors %d, duration %v, user %q", i, status, errs, dur, user)
			}
			if h.Totals.Errored != 1 || h.Totals.Crashed != 0 {
				t.Errorf("order %d: totals %+v", i, h.Totals)
			}
		})
	}
}

// Nothing after a crash makes a session healthy again, even an update
// with a higher seq (an SDK that restored an old "exited" from disk).
func TestCrashIsFinal(t *testing.T) {
	s, p := newProject(t)
	sid := uuid.NewString()
	saveSessions(t, s, p.ID,
		sessionUpdate{sid: sid, seq: 1, status: "crashed", errors: 1, release: "r1"},
		sessionUpdate{sid: sid, seq: 5, status: "exited", errors: 1, release: "r1"},
		sessionUpdate{sid: sid, seq: 6, status: "ok", errors: 1, release: "r1"},
	)
	if h := health(t, s, p.ID, ""); h.Totals.Crashed != 1 {
		t.Fatalf("totals %+v: a crashed session became healthy", h.Totals)
	}
}

func TestReleaseHealth(t *testing.T) {
	s, p := newProject(t)
	add := func(release, env, status string, errors int, user string) {
		saveSessions(t, s, p.ID, sessionUpdate{sid: uuid.NewString(), seq: 1, status: status, errors: errors, release: release, env: env, user: user})
	}
	// shop@1.0: 4 sessions, 1 crashed, 1 errored. Users a, b, c; only c crashed.
	add("shop@1.0", "production", "exited", 0, "a")
	add("shop@1.0", "production", "exited", 3, "b")
	add("shop@1.0", "production", "crashed", 1, "c")
	add("shop@1.0", "production", "ok", 0, "")
	// shop@1.1: 4 healthy sessions, one of them in staging.
	for range 3 {
		add("shop@1.1", "production", "exited", 0, "a")
	}
	add("shop@1.1", "staging", "exited", 0, "")

	h := health(t, s, p.ID, "")
	if h.Totals.Sessions != 8 || h.Totals.Crashed != 1 || h.Totals.Errored != 1 {
		t.Fatalf("totals %+v", h.Totals)
	}
	byRelease := map[string]store.ReleaseRow{}
	for _, r := range h.Releases {
		byRelease[r.Release] = r
	}
	old, cur := byRelease["shop@1.0"], byRelease["shop@1.1"]
	// 3 of 4 sessions crash-free; 2 of 3 users crash-free.
	if !near(old.CrashFreeSessions, 0.75) || !near(old.CrashFreeUsers, 2.0/3) || old.Adoption != 0.5 {
		t.Errorf("shop@1.0 = %+v (crash-free %v / users %v)", old, deref(old.CrashFreeSessions), deref(old.CrashFreeUsers))
	}
	if !near(cur.CrashFreeSessions, 1) || cur.Users != 1 {
		t.Errorf("shop@1.1 = %+v", cur)
	}

	staging := health(t, s, p.ID, "staging")
	if staging.Totals.Sessions != 1 || len(staging.Releases) != 1 {
		t.Errorf("staging = %+v", staging)
	}
	if len(h.Environments) != 2 {
		t.Errorf("environments = %v", h.Environments)
	}
	// One point per hour for 24 hours (25 when the window starts mid-hour),
	// with the empty hours present as zeros.
	if n := len(h.Series); n < 24 || n > 25 {
		t.Errorf("%d series points", n)
	}
	var sum int64
	for _, pt := range h.Series {
		sum += pt.Sessions
	}
	if sum != 8 {
		t.Errorf("series adds up to %d sessions, want 8", sum)
	}
}

// With no sessions the rate is unknown, not 100 %.
func TestReleaseHealthWithoutSessions(t *testing.T) {
	s, p := newProject(t)
	h := health(t, s, p.ID, "")
	if h.Totals.CrashFreeSessions != nil || h.Releases == nil || len(h.Releases) != 0 {
		t.Fatalf("health = %+v", h)
	}
}

func near(got *float64, want float64) bool { return got != nil && math.Abs(*got-want) < 1e-9 }
func deref(f *float64) any {
	if f == nil {
		return nil
	}
	return *f
}

func newTransaction(t *testing.T, projectID int64, name string, durMs int, spans ...map[string]any) *event.Event {
	t.Helper()
	end := time.Now().UTC()
	start := end.Add(-time.Duration(durMs) * time.Millisecond)
	raw, _ := json.Marshal(map[string]any{
		"event_id": uuid.NewString(), "type": "transaction", "platform": "javascript", "release": "web@1",
		"timestamp":   end.Format(time.RFC3339Nano),
		"transaction": map[string]any{"name": name, "op": "pageload", "start_timestamp": start.Format(time.RFC3339Nano), "spans": spans},
	})
	e, err := event.Normalize(raw, time.Now())
	if err != nil {
		t.Fatal(err)
	}
	e.ProjectID = projectID
	return e
}

func span(op string, startMs, durMs int, txStart time.Time) map[string]any {
	st := txStart.Add(time.Duration(startMs) * time.Millisecond)
	return map[string]any{"span_id": uuid.NewString()[:16], "op": op, "start_timestamp": st.Format(time.RFC3339Nano), "timestamp": st.Add(time.Duration(durMs) * time.Millisecond).Format(time.RFC3339Nano)}
}

func TestTransactions(t *testing.T) {
	ctx := context.Background()
	s, p := newProject(t)
	var slowest *event.Event
	for _, d := range []int{100, 200, 1000} {
		e := newTransaction(t, p.ID, "/checkout", d)
		// Give each one an HTTP span of half its length.
		e.Transaction.Spans = []event.Span{{SpanID: "a", Op: "http.client", DurationMs: float64(d) / 2}}
		if ok, err := s.SaveTransaction(ctx, e); err != nil || !ok {
			t.Fatalf("save: %v %v", ok, err)
		}
		slowest = e
	}
	// A retried send is stored once.
	if ok, _ := s.SaveTransaction(ctx, slowest); ok {
		t.Error("a duplicate event_id was stored again")
	}
	if _, err := s.SaveTransaction(ctx, newTransaction(t, p.ID, "/", 50)); err != nil {
		t.Fatal(err)
	}

	w := store.Window{Since: time.Now().Add(-time.Hour), Bucket: "hour"}
	list, err := s.Transactions(ctx, p.ID, store.TransactionFilter{Window: w, Sort: "p95"})
	if err != nil {
		t.Fatal(err)
	}
	if len(list.Transactions) != 2 || list.Transactions[0].Name != "/checkout" {
		t.Fatalf("list = %+v", list.Transactions)
	}
	c := list.Transactions[0]
	// percentile_cont interpolates: p95 of 100, 200, 1000 is 200 + 0.9 × 800.
	if c.Count != 3 || c.P50 != 200 || math.Abs(c.P95-920) > 1e-9 {
		t.Errorf("/checkout = %+v", c)
	}

	d, err := s.TransactionDetail(ctx, p.ID, "/checkout", "pageload", w, "slowest")
	if err != nil {
		t.Fatal(err)
	}
	if d.Samples[0].EventID != slowest.EventID || d.Samples[0].DurationMs != 1000 || d.Samples[0].Spans != 1 {
		t.Errorf("slowest sample = %+v", d.Samples[0])
	}
	// 650 ms of HTTP over 3 transactions: about 217 ms per page load.
	if len(d.SpanOps) != 1 || d.SpanOps[0].Op != "http.client" || math.Abs(d.SpanOps[0].PerTransaction-650.0/3) > 1e-9 {
		t.Errorf("span ops = %+v", d.SpanOps)
	}
	if len(d.Releases) != 1 || d.Releases[0].Count != 3 {
		t.Errorf("releases = %+v", d.Releases)
	}

	if _, err := s.TransactionDetail(ctx, p.ID, "/nope", "", w, ""); !errors.Is(err, store.ErrNotFound) {
		t.Errorf("unknown name: %v, want ErrNotFound", err)
	}

	// Another project's transaction is invisible, even by its exact id.
	other, _ := newProject2(t)
	if _, err := s.TransactionEvent(ctx, other.ID, slowest.EventID); !errors.Is(err, store.ErrNotFound) {
		t.Errorf("cross-project read: %v", err)
	}
	if raw, err := s.TransactionEvent(ctx, p.ID, slowest.EventID); err != nil || len(raw) == 0 {
		t.Errorf("own read: %v", err)
	}
}

// newProject2 makes a second project in the same test.
func newProject2(t *testing.T) (store.Project, error) {
	p, err := store.New(pool).CreateProject(context.Background(), "org2-"+t.Name(), "proj")
	if err != nil {
		t.Fatal(err)
	}
	return p, nil
}
