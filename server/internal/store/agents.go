package store

import (
	"context"
	"errors"
	"time"

	"github.com/jackc/pgx/v5"
)

// Release ties a release name to the commit it was built from.
type Release struct {
	Name       string    `json:"name"`
	Commit     string    `json:"commit"`
	Repository string    `json:"repository"`
	UpdatedAt  time.Time `json:"updated_at"`
}

// SaveRelease records (or updates) a release's commit. CI calls it on every
// build, so running it twice for the same release just overwrites.
func (s *Store) SaveRelease(ctx context.Context, projectID int64, r Release) (Release, error) {
	err := s.pool.QueryRow(ctx, `
		INSERT INTO releases (project_id, name, commit_sha, repository)
		VALUES ($1, $2, $3, $4)
		ON CONFLICT (project_id, name) DO UPDATE SET
			commit_sha = EXCLUDED.commit_sha, repository = EXCLUDED.repository, updated_at = now()
		RETURNING updated_at`, projectID, r.Name, r.Commit, r.Repository).Scan(&r.UpdatedAt)
	return r, err
}

// ReleaseInfo returns one release's commit, or ErrNotFound if CI never
// recorded it.
func (s *Store) ReleaseInfo(ctx context.Context, projectID int64, name string) (Release, error) {
	r := Release{Name: name}
	err := s.pool.QueryRow(ctx, `SELECT commit_sha, repository, updated_at FROM releases WHERE project_id = $1 AND name = $2`, projectID, name).
		Scan(&r.Commit, &r.Repository, &r.UpdatedAt)
	if errors.Is(err, pgx.ErrNoRows) {
		return r, ErrNotFound
	}
	return r, err
}

// releaseCommits returns the recorded commits for the given release names,
// in no particular order; names without a record are left out.
func (s *Store) releaseCommits(ctx context.Context, projectID int64, names []string) ([]Release, error) {
	rows, err := s.pool.Query(ctx, `
		SELECT name, commit_sha, repository, updated_at FROM releases
		WHERE project_id = $1 AND name = ANY($2)`, projectID, names)
	if err != nil {
		return nil, err
	}
	out, err := pgx.CollectRows(rows, pgx.RowToStructByPos[Release])
	if out == nil {
		out = []Release{}
	}
	return out, err
}

type IssueLink struct {
	URL       string    `json:"url"`
	Title     string    `json:"title"`
	CreatedAt time.Time `json:"created_at"`
}

// AddIssueLink attaches a link (usually a pull request) to an issue in the
// project. Adding the same URL again only updates its title. It returns
// ErrNotFound when the issue isn't in this project: the project_id check is
// what stops one project's key from writing to another project's issues.
func (s *Store) AddIssueLink(ctx context.Context, projectID, issueID int64, l IssueLink) (IssueLink, error) {
	err := s.pool.QueryRow(ctx, `
		INSERT INTO issue_links (issue_id, url, title)
		SELECT id, $3, $4 FROM issues WHERE id = $2 AND project_id = $1
		ON CONFLICT (issue_id, url) DO UPDATE SET title = EXCLUDED.title
		RETURNING created_at`, projectID, issueID, l.URL, l.Title).Scan(&l.CreatedAt)
	if errors.Is(err, pgx.ErrNoRows) {
		return l, ErrNotFound
	}
	return l, err
}

func (s *Store) issueLinks(ctx context.Context, issueID int64) ([]IssueLink, error) {
	rows, err := s.pool.Query(ctx, `SELECT url, title, created_at FROM issue_links WHERE issue_id = $1 ORDER BY created_at, id LIMIT 20`, issueID)
	if err != nil {
		return nil, err
	}
	out, err := pgx.CollectRows(rows, pgx.RowToStructByPos[IssueLink])
	if out == nil {
		out = []IssueLink{}
	}
	return out, err
}

// NewWebhookSecret makes the key juju signs webhook calls with.
func NewWebhookSecret() (string, error) { return randomKey("whsec_") }
