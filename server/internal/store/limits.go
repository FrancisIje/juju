package store

import (
	"context"
	"errors"
	"time"

	"github.com/jackc/pgx/v5"
)

// IngestProject is what ingest needs to know about a project: its key and
// its limits. Nil limits mean "not set": the server's default rate, and no
// quota.
type IngestProject struct {
	PublicKey    string
	RateLimit    *int   // events per second; 0 = unlimited
	MonthlyQuota *int64 // events per calendar month (UTC)
}

// ProjectIngest returns the key and limits for project id.
func (s *Store) ProjectIngest(ctx context.Context, id int64) (IngestProject, error) {
	var p IngestProject
	err := s.pool.QueryRow(ctx,
		`SELECT public_key, rate_limit, monthly_quota FROM projects WHERE id = $1`, id,
	).Scan(&p.PublicKey, &p.RateLimit, &p.MonthlyQuota)
	if errors.Is(err, pgx.ErrNoRows) {
		return IngestProject{}, ErrNotFound
	}
	return p, err
}

// SetProjectLimits sets (or, with nil, clears) a project's limits.
func (s *Store) SetProjectLimits(ctx context.Context, id int64, rate *int, quota *int64) error {
	tag, err := s.pool.Exec(ctx,
		`UPDATE projects SET rate_limit = $2, monthly_quota = $3 WHERE id = $1`, id, rate, quota)
	if err != nil {
		return err
	}
	if tag.RowsAffected() == 0 {
		return ErrNotFound
	}
	return nil
}

// MonthStart is the first instant of t's calendar month in UTC: the key
// quotas are counted under.
func MonthStart(t time.Time) time.Time {
	t = t.UTC()
	return time.Date(t.Year(), t.Month(), 1, 0, 0, 0, 0, time.UTC)
}

// MonthUsage returns how many events project id has had accepted in the
// month starting at month.
func (s *Store) MonthUsage(ctx context.Context, id int64, month time.Time) (int64, error) {
	var n int64
	err := s.pool.QueryRow(ctx,
		`SELECT accepted FROM project_usage WHERE project_id = $1 AND month = $2`, id, month,
	).Scan(&n)
	if errors.Is(err, pgx.ErrNoRows) {
		return 0, nil
	}
	return n, err
}

// AddUsage adds counts (project id → events) to the month's usage, in one
// statement however many projects there are. Adding (rather than setting)
// is what lets several ingest processes count into the same row.
func (s *Store) AddUsage(ctx context.Context, month time.Time, counts map[int64]int64) error {
	if len(counts) == 0 {
		return nil
	}
	ids := make([]int64, 0, len(counts))
	ns := make([]int64, 0, len(counts))
	for id, n := range counts {
		ids = append(ids, id)
		ns = append(ns, n)
	}
	// A project deleted since the events arrived would break the foreign
	// key, so join against projects and skip it.
	_, err := s.pool.Exec(ctx, `
		INSERT INTO project_usage (project_id, month, accepted)
		SELECT u.id, $1, u.n
		FROM unnest($2::bigint[], $3::bigint[]) AS u(id, n)
		JOIN projects p ON p.id = u.id
		ON CONFLICT (project_id, month) DO UPDATE
		SET accepted = project_usage.accepted + EXCLUDED.accepted`,
		month, ids, ns)
	return err
}
