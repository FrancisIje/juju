package store

import (
	"context"
	"encoding/json"
	"errors"
	"fmt"
	"time"

	"github.com/jackc/pgx/v5"

	"juju/internal/event"
)

// Sessions and transactions (Phase 8). They live in Postgres for now; the
// queries only ever read a time window of one project, so the indexes on
// (project_id, started_at) keep them fast until there's real volume.

// SaveSession merges one session update into the session's row.
//
// The rules make the result the same whatever order updates arrive in, and
// however many times each one is sent:
//   - status comes from the update with the highest seq, except that
//     "crashed" is final;
//   - errors only grow (each update carries the running total);
//   - the start is the earliest seen, the last update the latest;
//   - duration is the length when the session was last sent as ended. A
//     session resumed from the background keeps it until it ends again: if
//     the OS kills the app there, that's the best length there is.
//
// It reports whether the session is new.
func (s *Store) SaveSession(ctx context.Context, e *event.Event) (bool, error) {
	ss := e.Session
	var created bool
	err := s.pool.QueryRow(ctx, `
		INSERT INTO app_sessions AS s (project_id, session_id, release, environment, platform, user_key, status, seq, errors, started_at, updated_at, duration_ms)
		VALUES ($1, $2, $3, $4, $5, $6, $7, $8, $9, $10, $11, $12)
		ON CONFLICT (project_id, session_id) DO UPDATE SET
			status = CASE
				WHEN s.status = 'crashed' OR EXCLUDED.status = 'crashed' THEN 'crashed'
				WHEN EXCLUDED.seq >= s.seq THEN EXCLUDED.status
				ELSE s.status END,
			duration_ms = CASE WHEN EXCLUDED.seq >= s.seq THEN coalesce(EXCLUDED.duration_ms, s.duration_ms) ELSE coalesce(s.duration_ms, EXCLUDED.duration_ms) END,
			seq        = greatest(s.seq, EXCLUDED.seq),
			errors     = greatest(s.errors, EXCLUDED.errors),
			started_at = least(s.started_at, EXCLUDED.started_at),
			updated_at = greatest(s.updated_at, EXCLUDED.updated_at),
			-- setUser often runs after the session started (the user logs in).
			user_key   = CASE WHEN EXCLUDED.user_key <> '' THEN EXCLUDED.user_key ELSE s.user_key END
		RETURNING (xmax = 0)`,
		e.ProjectID, ss.ID, e.Release, e.Environment, e.Platform, userKey(e), ss.Status, ss.Seq, ss.Errors, ss.StartedAt, e.Time, ss.DurationMs,
	).Scan(&created)
	if err != nil {
		return false, fmt.Errorf("upsert session: %w", err)
	}
	return created, nil
}

// SaveTransaction stores one transaction. A retried send has the same
// event_id and is ignored; it reports whether the row was new.
func (s *Store) SaveTransaction(ctx context.Context, e *event.Event) (bool, error) {
	payload, err := json.Marshal(e)
	if err != nil {
		return false, fmt.Errorf("encode payload: %w", err)
	}
	tx := e.Transaction
	tag, err := s.pool.Exec(ctx, `
		INSERT INTO transactions (project_id, event_id, name, op, status, release, environment, platform, started_at, duration_ms, payload)
		VALUES ($1, $2, $3, $4, $5, $6, $7, $8, $9, $10, $11)
		ON CONFLICT (project_id, event_id) DO NOTHING`,
		e.ProjectID, e.EventID, tx.Name, tx.Op, tx.Status, e.Release, e.Environment, e.Platform, tx.Start, tx.DurationMs, payload,
	)
	if err != nil {
		return false, fmt.Errorf("insert transaction: %w", err)
	}
	return tag.RowsAffected() == 1, nil
}

// Window is the time range and grouping a performance page asks for.
type Window struct {
	Since       time.Time
	Bucket      string // "hour" or "day": the width of each point in a chart
	Environment string // "" for all
}

// SessionStats counts sessions. A session is crashed if it ended in a
// crash, errored if it saw errors but didn't crash, and healthy otherwise.
// The crash-free rates are nil when there are no sessions (or no users):
// "no data" must not show as 100 %.
type SessionStats struct {
	Sessions          int64    `json:"sessions"`
	Crashed           int64    `json:"crashed"`
	Errored           int64    `json:"errored"`
	Users             int64    `json:"users"`
	CrashedUsers      int64    `json:"crashed_users"`
	CrashFreeSessions *float64 `json:"crash_free_sessions"`
	CrashFreeUsers    *float64 `json:"crash_free_users"`
}

func (st *SessionStats) rates() {
	st.CrashFreeSessions = crashFree(st.Crashed, st.Sessions)
	st.CrashFreeUsers = crashFree(st.CrashedUsers, st.Users)
}

func crashFree(crashed, total int64) *float64 {
	if total == 0 {
		return nil
	}
	r := 1 - float64(crashed)/float64(total)
	return &r
}

type ReleaseRow struct {
	Release string `json:"release"`
	SessionStats
	// Adoption is this release's share of all sessions in the window: how
	// many people are on it already.
	Adoption  float64   `json:"adoption"`
	FirstSeen time.Time `json:"first_seen"`
	LastSeen  time.Time `json:"last_seen"`
}

type SessionPoint struct {
	Time     time.Time `json:"time"`
	Sessions int64     `json:"sessions"`
	Crashed  int64     `json:"crashed"`
	Errored  int64     `json:"errored"`
}

type ReleaseHealth struct {
	Totals       SessionStats   `json:"totals"`
	Releases     []ReleaseRow   `json:"releases"`
	Series       []SessionPoint `json:"series"`
	Environments []string       `json:"environments"`
}

// The same three counts for every grouping below. Users are counted only
// when the app called setUser; anonymous sessions have no user_key.
const sessionCounts = `
	count(*),
	count(*) FILTER (WHERE status = 'crashed'),
	count(*) FILTER (WHERE status <> 'crashed' AND errors > 0),
	count(DISTINCT user_key) FILTER (WHERE user_key <> ''),
	count(DISTINCT user_key) FILTER (WHERE user_key <> '' AND status = 'crashed')`

// sessionWhere filters a project's sessions to the window. Sessions belong
// to the window they started in.
func sessionWhere(w Window) (string, []any) {
	cond, args := "project_id = $1 AND started_at >= $2", []any{w.Since}
	if w.Environment != "" {
		cond += " AND environment = $3"
		args = append(args, w.Environment)
	}
	return cond, args
}

func (s *Store) ReleaseHealth(ctx context.Context, projectID int64, w Window) (ReleaseHealth, error) {
	var h ReleaseHealth
	cond, extra := sessionWhere(w)
	args := append([]any{projectID}, extra...)

	t := &h.Totals
	if err := s.pool.QueryRow(ctx, `SELECT `+sessionCounts+` FROM app_sessions WHERE `+cond, args...).
		Scan(&t.Sessions, &t.Crashed, &t.Errored, &t.Users, &t.CrashedUsers); err != nil {
		return h, fmt.Errorf("session totals: %w", err)
	}
	t.rates()

	rows, err := s.pool.Query(ctx, `
		SELECT release, `+sessionCounts+`, min(started_at), max(updated_at)
		FROM app_sessions WHERE `+cond+`
		GROUP BY release ORDER BY max(started_at) DESC LIMIT 50`, args...)
	if err != nil {
		return h, fmt.Errorf("sessions by release: %w", err)
	}
	h.Releases, err = pgx.CollectRows(rows, func(r pgx.CollectableRow) (ReleaseRow, error) {
		var x ReleaseRow
		err := r.Scan(&x.Release, &x.Sessions, &x.Crashed, &x.Errored, &x.Users, &x.CrashedUsers, &x.FirstSeen, &x.LastSeen)
		x.rates()
		if t.Sessions > 0 {
			x.Adoption = float64(x.Sessions) / float64(t.Sessions)
		}
		return x, err
	})
	if err != nil {
		return h, err
	}

	rows, err = s.pool.Query(ctx, `
		SELECT date_trunc('`+bucket(w)+`', started_at, 'UTC'), count(*),
		       count(*) FILTER (WHERE status = 'crashed'),
		       count(*) FILTER (WHERE status <> 'crashed' AND errors > 0)
		FROM app_sessions WHERE `+cond+` GROUP BY 1`, args...)
	if err != nil {
		return h, fmt.Errorf("session series: %w", err)
	}
	points, err := pgx.CollectRows(rows, pgx.RowToStructByPos[SessionPoint])
	if err != nil {
		return h, err
	}
	h.Series = fillSeries(w, points, func(t time.Time) SessionPoint { return SessionPoint{Time: t} }, func(p SessionPoint) time.Time { return p.Time })

	h.Environments, err = s.distinct(ctx, `SELECT DISTINCT environment FROM app_sessions WHERE project_id = $1 AND started_at >= $2 ORDER BY 1 LIMIT 20`, projectID, w.Since)
	if h.Releases == nil {
		h.Releases = []ReleaseRow{}
	}
	return h, err
}

// bucket is only ever "hour" or "day"; anything else falls back to day, so
// the value pasted into SQL can't be anything else.
func bucket(w Window) string {
	if w.Bucket == "hour" {
		return "hour"
	}
	return "day"
}

// fillSeries returns one point per bucket from the window's start to now,
// with empty buckets as zeros. Without them a chart would draw a quiet
// night as a straight line from the evening to the morning.
func fillSeries[P any](w Window, have []P, empty func(time.Time) P, at func(P) time.Time) []P {
	step := 24 * time.Hour
	if bucket(w) == "hour" {
		step = time.Hour
	}
	byTime := map[int64]P{}
	for _, p := range have {
		byTime[at(p).Unix()] = p
	}
	out := []P{}
	for t := w.Since.UTC().Truncate(step); !t.After(time.Now()); t = t.Add(step) {
		if p, ok := byTime[t.Unix()]; ok {
			out = append(out, p)
		} else {
			out = append(out, empty(t))
		}
	}
	return out
}

// TransactionRow summarises every transaction with the same name and op.
// Percentiles, not the average: one 30-second outlier drags an average far
// from what most people saw; p50 is the typical experience and p95 the
// slow end that one user in twenty gets.
type TransactionRow struct {
	Name     string    `json:"name"`
	Op       string    `json:"op"`
	Count    int64     `json:"count"`
	P50      float64   `json:"p50"`
	P75      float64   `json:"p75"`
	P95      float64   `json:"p95"`
	Failed   int64     `json:"failed"` // status other than ok
	LastSeen time.Time `json:"last_seen"`
}

const txStats = `
	count(*),
	percentile_cont(0.5)  WITHIN GROUP (ORDER BY duration_ms),
	percentile_cont(0.75) WITHIN GROUP (ORDER BY duration_ms),
	percentile_cont(0.95) WITHIN GROUP (ORDER BY duration_ms),
	count(*) FILTER (WHERE status <> 'ok'),
	max(started_at)`

type TransactionFilter struct {
	Window
	Op   string // "" for all
	Sort string // count (default) | p95 | p50 | name
}

func txWhere(f TransactionFilter) (string, []any) {
	cond, args := "project_id = $1 AND started_at >= $2", []any{f.Since}
	if f.Environment != "" {
		args = append(args, f.Environment)
		cond += fmt.Sprintf(" AND environment = $%d", len(args)+1)
	}
	if f.Op != "" {
		args = append(args, f.Op)
		cond += fmt.Sprintf(" AND op = $%d", len(args)+1)
	}
	return cond, args
}

type TransactionList struct {
	Transactions []TransactionRow `json:"transactions"`
	Ops          []string         `json:"ops"`
	Environments []string         `json:"environments"`
}

func (s *Store) Transactions(ctx context.Context, projectID int64, f TransactionFilter) (TransactionList, error) {
	var out TransactionList
	cond, extra := txWhere(f)
	args := append([]any{projectID}, extra...)
	order := map[string]string{
		"p95":  "percentile_cont(0.95) WITHIN GROUP (ORDER BY duration_ms) DESC",
		"p50":  "percentile_cont(0.5) WITHIN GROUP (ORDER BY duration_ms) DESC",
		"name": "name",
	}[f.Sort]
	if order == "" {
		order = "count(*) DESC"
	}
	rows, err := s.pool.Query(ctx, `
		SELECT name, op, `+txStats+`
		FROM transactions WHERE `+cond+`
		GROUP BY name, op ORDER BY `+order+`, name LIMIT 200`, args...)
	if err != nil {
		return out, fmt.Errorf("list transactions: %w", err)
	}
	if out.Transactions, err = pgx.CollectRows(rows, pgx.RowToStructByPos[TransactionRow]); err != nil {
		return out, err
	}
	if out.Transactions == nil {
		out.Transactions = []TransactionRow{}
	}
	if out.Ops, err = s.distinct(ctx, `SELECT DISTINCT op FROM transactions WHERE project_id = $1 AND started_at >= $2 ORDER BY 1 LIMIT 50`, projectID, f.Since); err != nil {
		return out, err
	}
	out.Environments, err = s.distinct(ctx, `SELECT DISTINCT environment FROM transactions WHERE project_id = $1 AND started_at >= $2 ORDER BY 1 LIMIT 20`, projectID, f.Since)
	return out, err
}

type TransactionSample struct {
	EventID     string    `json:"event_id"`
	StartedAt   time.Time `json:"started_at"`
	DurationMs  float64   `json:"duration_ms"`
	Status      string    `json:"status"`
	Release     string    `json:"release"`
	Environment string    `json:"environment"`
	Platform    string    `json:"platform"`
	Spans       int       `json:"spans"`
}

type ReleaseTiming struct {
	Release string  `json:"release"`
	Count   int64   `json:"count"`
	P50     float64 `json:"p50"`
	P95     float64 `json:"p95"`
}

// SpanOp says where a transaction's time goes: for each kind of span, how
// often it happens and how long it takes.
type SpanOp struct {
	Op    string  `json:"op"`
	Count int64   `json:"count"`
	P50   float64 `json:"p50"`
	P95   float64 `json:"p95"`
	// Total time in this op divided by the number of transactions: what
	// it adds to a typical transaction. Spans can overlap (parallel
	// requests), so these don't have to add up to the duration.
	PerTransaction float64 `json:"per_transaction"`
}

type TimingPoint struct {
	Time  time.Time `json:"time"`
	Count int64     `json:"count"`
	P50   *float64  `json:"p50"`
	P95   *float64  `json:"p95"`
}

type TransactionDetail struct {
	Summary  TransactionRow      `json:"summary"`
	Series   []TimingPoint       `json:"series"`
	Releases []ReleaseTiming     `json:"releases"`
	SpanOps  []SpanOp            `json:"span_ops"`
	Samples  []TransactionSample `json:"samples"`
}

// TransactionDetail is one transaction name: its trend, how releases
// compare, where the time goes, and individual samples to inspect.
// samples is "slowest" or "recent".
func (s *Store) TransactionDetail(ctx context.Context, projectID int64, name, op string, w Window, samples string) (TransactionDetail, error) {
	var d TransactionDetail
	cond, extra := txWhere(TransactionFilter{Window: w, Op: op})
	args := append([]any{projectID}, extra...)
	args = append(args, name)
	cond += fmt.Sprintf(" AND name = $%d", len(args))

	err := s.pool.QueryRow(ctx, `SELECT name, op, `+txStats+` FROM transactions WHERE `+cond+` GROUP BY name, op`, args...).
		Scan(&d.Summary.Name, &d.Summary.Op, &d.Summary.Count, &d.Summary.P50, &d.Summary.P75, &d.Summary.P95, &d.Summary.Failed, &d.Summary.LastSeen)
	if errors.Is(err, pgx.ErrNoRows) {
		return d, ErrNotFound
	}
	if err != nil {
		return d, fmt.Errorf("transaction summary: %w", err)
	}

	rows, err := s.pool.Query(ctx, `
		SELECT date_trunc('`+bucket(w)+`', started_at, 'UTC'), count(*),
		       percentile_cont(0.5)  WITHIN GROUP (ORDER BY duration_ms),
		       percentile_cont(0.95) WITHIN GROUP (ORDER BY duration_ms)
		FROM transactions WHERE `+cond+` GROUP BY 1`, args...)
	if err != nil {
		return d, fmt.Errorf("transaction series: %w", err)
	}
	points, err := pgx.CollectRows(rows, pgx.RowToStructByPos[TimingPoint])
	if err != nil {
		return d, err
	}
	d.Series = fillSeries(w, points, func(t time.Time) TimingPoint { return TimingPoint{Time: t} }, func(p TimingPoint) time.Time { return p.Time })

	rows, err = s.pool.Query(ctx, `
		SELECT release, count(*),
		       percentile_cont(0.5)  WITHIN GROUP (ORDER BY duration_ms),
		       percentile_cont(0.95) WITHIN GROUP (ORDER BY duration_ms)
		FROM transactions WHERE `+cond+`
		GROUP BY release ORDER BY max(started_at) DESC LIMIT 10`, args...)
	if err != nil {
		return d, fmt.Errorf("transaction releases: %w", err)
	}
	if d.Releases, err = pgx.CollectRows(rows, pgx.RowToStructByPos[ReleaseTiming]); err != nil {
		return d, err
	}

	// jsonb_array_elements turns each stored transaction's spans into rows,
	// so spans can be grouped like a table without a table of their own.
	rows, err = s.pool.Query(ctx, `
		SELECT sp->>'op', count(*),
		       percentile_cont(0.5)  WITHIN GROUP (ORDER BY (sp->>'duration_ms')::float8),
		       percentile_cont(0.95) WITHIN GROUP (ORDER BY (sp->>'duration_ms')::float8),
		       sum((sp->>'duration_ms')::float8) / $`+fmt.Sprint(len(args)+1)+`
		FROM transactions, jsonb_array_elements(coalesce(payload->'transaction'->'spans', '[]')) sp
		WHERE `+cond+`
		GROUP BY 1 ORDER BY 5 DESC LIMIT 20`, append(args, d.Summary.Count)...)
	if err != nil {
		return d, fmt.Errorf("span ops: %w", err)
	}
	if d.SpanOps, err = pgx.CollectRows(rows, pgx.RowToStructByPos[SpanOp]); err != nil {
		return d, err
	}

	order := "duration_ms DESC"
	if samples == "recent" {
		order = "started_at DESC"
	}
	rows, err = s.pool.Query(ctx, `
		SELECT event_id::text, started_at, duration_ms, status, release, environment, platform,
		       coalesce(jsonb_array_length(payload->'transaction'->'spans'), 0)
		FROM transactions WHERE `+cond+` ORDER BY `+order+` LIMIT 20`, args...)
	if err != nil {
		return d, fmt.Errorf("transaction samples: %w", err)
	}
	if d.Samples, err = pgx.CollectRows(rows, pgx.RowToStructByPos[TransactionSample]); err != nil {
		return d, err
	}
	if d.Releases == nil {
		d.Releases = []ReleaseTiming{}
	}
	if d.SpanOps == nil {
		d.SpanOps = []SpanOp{}
	}
	if d.Samples == nil {
		d.Samples = []TransactionSample{}
	}
	return d, nil
}

// TransactionEvent returns one stored transaction with its spans.
func (s *Store) TransactionEvent(ctx context.Context, projectID int64, eventID string) (json.RawMessage, error) {
	var payload json.RawMessage
	err := s.pool.QueryRow(ctx, `SELECT payload FROM transactions WHERE project_id = $1 AND event_id = $2`, projectID, eventID).Scan(&payload)
	if errors.Is(err, pgx.ErrNoRows) {
		return nil, ErrNotFound
	}
	return payload, err
}
