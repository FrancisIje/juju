// Package store is juju's Postgres access layer. All SQL lives here.
package store

import (
	"context"
	"crypto/rand"
	"crypto/sha256"
	"encoding/hex"
	"encoding/json"
	"errors"
	"fmt"
	"time"

	"github.com/jackc/pgx/v5"
	"github.com/jackc/pgx/v5/pgxpool"

	"juju/internal/event"
	"juju/internal/fingerprint"
)

var ErrNotFound = errors.New("not found")

type Store struct {
	pool *pgxpool.Pool
}

func New(pool *pgxpool.Pool) *Store { return &Store{pool: pool} }

type Project struct {
	ID        int64  `json:"id"`
	Org       string `json:"organization"`
	Name      string `json:"name"`
	PublicKey string `json:"public_key"`
	SecretKey string `json:"secret_key,omitempty"` // only ever set right after creation
}

// CreateProject creates a project (and its organization if needed) with a
// fresh key pair. The secret key is returned once and only its hash is kept.
func (s *Store) CreateProject(ctx context.Context, org, name string) (Project, error) {
	public, err := randomKey("pk_")
	if err != nil {
		return Project{}, err
	}
	secret, err := randomKey("sk_")
	if err != nil {
		return Project{}, err
	}
	p := Project{Org: org, Name: name, PublicKey: public, SecretKey: secret}

	// ON CONFLICT ... DO UPDATE with a no-op SET is a trick so RETURNING
	// gives back the id whether the row was just inserted or already existed.
	err = s.pool.QueryRow(ctx, `
		WITH org AS (
			INSERT INTO organizations (name) VALUES ($1)
			ON CONFLICT (name) DO UPDATE SET name = EXCLUDED.name
			RETURNING id
		)
		INSERT INTO projects (organization_id, name, public_key, secret_key_hash)
		SELECT id, $2, $3, $4 FROM org
		RETURNING id`,
		org, name, public, HashSecret(secret),
	).Scan(&p.ID)
	if err != nil {
		return Project{}, fmt.Errorf("create project: %w", err)
	}
	return p, nil
}

// ProjectPublicKey returns the key that SDKs must send for project id.
func (s *Store) ProjectPublicKey(ctx context.Context, id int64) (string, error) {
	var key string
	err := s.pool.QueryRow(ctx, `SELECT public_key FROM projects WHERE id = $1`, id).Scan(&key)
	if errors.Is(err, pgx.ErrNoRows) {
		return "", ErrNotFound
	}
	return key, err
}

// ProjectSecretHash returns the SHA-256 of the project's secret key.
func (s *Store) ProjectSecretHash(ctx context.Context, id int64) ([]byte, error) {
	var hash []byte
	err := s.pool.QueryRow(ctx, `SELECT secret_key_hash FROM projects WHERE id = $1`, id).Scan(&hash)
	if errors.Is(err, pgx.ErrNoRows) {
		return nil, ErrNotFound
	}
	return hash, err
}

// SaveEvent stores one event and updates its issue, all in one transaction so
// a crash halfway can never leave an issue counting an event that isn't
// stored (or the other way round).
func (s *Store) SaveEvent(ctx context.Context, e *event.Event, fp fingerprint.Result) (bool, error) {
	payload, err := json.Marshal(e)
	if err != nil {
		return false, fmt.Errorf("encode payload: %w", err)
	}

	tx, err := s.pool.Begin(ctx)
	if err != nil {
		return false, err
	}
	// Rollback after a successful Commit is a harmless no-op, so deferring it
	// covers every early return below.
	defer tx.Rollback(ctx)

	// 1. Find or create the issue. Concurrent workers seeing the same new
	//    bug are safe: the UNIQUE (project_id, fingerprint) constraint means
	//    only one INSERT wins and the other takes the DO UPDATE path.
	var issueID int64
	err = tx.QueryRow(ctx, `
		INSERT INTO issues (project_id, fingerprint, title, culprit, first_seen, last_seen)
		VALUES ($1, $2, $3, $4, $5, $5)
		ON CONFLICT (project_id, fingerprint) DO UPDATE SET project_id = EXCLUDED.project_id
		RETURNING id`,
		e.ProjectID, fp.Hash, fp.Title, fp.Culprit, e.Time,
	).Scan(&issueID)
	if err != nil {
		return false, fmt.Errorf("upsert issue: %w", err)
	}

	// 2. Store the event. A retried send has the same event_id, hits the
	//    primary key, and inserts nothing.
	tag, err := tx.Exec(ctx, `
		INSERT INTO events (project_id, event_id, issue_id, timestamp, received_at, platform, release, environment, payload)
		VALUES ($1, $2, $3, $4, $5, $6, $7, $8, $9)
		ON CONFLICT (project_id, event_id) DO NOTHING`,
		e.ProjectID, e.EventID, issueID, e.Time, e.ReceivedAt, e.Platform, e.Release, e.Environment, payload,
	)
	if err != nil {
		return false, fmt.Errorf("insert event: %w", err)
	}
	inserted := tag.RowsAffected() == 1

	// 3. Count it, but only if it was new. Counting before checking would
	//    make every retry inflate the issue's count.
	if inserted {
		_, err = tx.Exec(ctx, `
			UPDATE issues SET
				event_count = event_count + 1,
				first_seen  = LEAST(first_seen, $2),
				last_seen   = GREATEST(last_seen, $2)
			WHERE id = $1`,
			issueID, e.Time,
		)
		if err != nil {
			return false, fmt.Errorf("update issue: %w", err)
		}
	}
	return inserted, tx.Commit(ctx)
}

type Issue struct {
	ID          int64     `json:"id"`
	Fingerprint string    `json:"fingerprint"`
	Title       string    `json:"title"`
	Culprit     string    `json:"culprit"`
	Status      string    `json:"status"`
	EventCount  int64     `json:"event_count"`
	FirstSeen   time.Time `json:"first_seen"`
	LastSeen    time.Time `json:"last_seen"`
}

// ListIssues returns a project's issues, most recently seen first.
func (s *Store) ListIssues(ctx context.Context, projectID int64, limit int) ([]Issue, error) {
	rows, err := s.pool.Query(ctx, `
		SELECT id, fingerprint, title, culprit, status, event_count, first_seen, last_seen
		FROM issues WHERE project_id = $1
		ORDER BY last_seen DESC, id DESC
		LIMIT $2`, projectID, limit)
	if err != nil {
		return nil, err
	}
	// CollectRows closes rows for us, even on error.
	return pgx.CollectRows(rows, pgx.RowToStructByPos[Issue])
}

// ListIssueEvents returns stored payloads for one issue, newest first. The
// project_id filter matters: without it, a valid key for project 1 could read
// project 2's events by guessing an issue id.
func (s *Store) ListIssueEvents(ctx context.Context, projectID, issueID int64, limit int) ([]json.RawMessage, error) {
	rows, err := s.pool.Query(ctx, `
		SELECT payload FROM events
		WHERE project_id = $1 AND issue_id = $2
		ORDER BY timestamp DESC
		LIMIT $3`, projectID, issueID, limit)
	if err != nil {
		return nil, err
	}
	return pgx.CollectRows(rows, pgx.RowTo[json.RawMessage])
}

// HashSecret is how secret keys are stored and compared.
func HashSecret(secret string) []byte {
	sum := sha256.Sum256([]byte(secret))
	return sum[:]
}

// randomKey returns prefix + 32 hex chars from the OS's secure random source.
// math/rand would be predictable; crypto/rand is not.
func randomKey(prefix string) (string, error) {
	b := make([]byte, 16)
	if _, err := rand.Read(b); err != nil {
		return "", err
	}
	return prefix + hex.EncodeToString(b), nil
}
