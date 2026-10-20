package store

import (
	"context"
	"errors"
	"fmt"
	"time"

	"github.com/jackc/pgx/v5"
)

type DebugFile struct {
	ID        int64     `json:"id"`
	Kind      string    `json:"kind"`
	Release   string    `json:"release,omitempty"`
	Name      string    `json:"name"`
	SHA256    string    `json:"sha256"`
	Size      int64     `json:"size"`
	CreatedAt time.Time `json:"created_at"`
}

// SaveDebugFile records an uploaded file. Uploading the same name again for
// a release replaces it: rebuilding a release with a fixed map should win.
func (s *Store) SaveDebugFile(ctx context.Context, projectID int64, f DebugFile) (DebugFile, error) {
	err := s.pool.QueryRow(ctx, `
		INSERT INTO debug_files (project_id, kind, release, name, sha256, size)
		VALUES ($1, $2, $3, $4, $5, $6)
		ON CONFLICT (project_id, kind, release, name)
		DO UPDATE SET sha256 = EXCLUDED.sha256, size = EXCLUDED.size, created_at = now()
		RETURNING id, created_at`,
		projectID, f.Kind, f.Release, f.Name, f.SHA256, f.Size,
	).Scan(&f.ID, &f.CreatedAt)
	if err != nil {
		return DebugFile{}, fmt.Errorf("save debug file: %w", err)
	}
	return f, nil
}

// FindDebugFile returns the hash of the file stored for a kind, release and
// name. Dart symbols are stored with an empty release and their build id
// as the name.
func (s *Store) FindDebugFile(ctx context.Context, projectID int64, kind, release, name string) (string, error) {
	var sum string
	err := s.pool.QueryRow(ctx, `
		SELECT sha256 FROM debug_files
		WHERE project_id = $1 AND kind = $2 AND release = $3 AND name = $4`,
		projectID, kind, release, name).Scan(&sum)
	if errors.Is(err, pgx.ErrNoRows) {
		return "", ErrNotFound
	}
	return sum, err
}

// ListDebugFiles returns a project's uploaded files, newest first, for one
// release or (with release "") all of them.
func (s *Store) ListDebugFiles(ctx context.Context, projectID int64, release string) ([]DebugFile, error) {
	rows, err := s.pool.Query(ctx, `
		SELECT id, kind, release, name, sha256, size, created_at FROM debug_files
		WHERE project_id = $1 AND ($2 = '' OR release = $2)
		ORDER BY created_at DESC, id DESC
		LIMIT 500`, projectID, release)
	if err != nil {
		return nil, err
	}
	return pgx.CollectRows(rows, pgx.RowToStructByPos[DebugFile])
}
