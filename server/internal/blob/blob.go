// Package blob stores uploaded files on disk, named by their SHA-256.
//
// Naming files by their content ("content-addressed") means the same file
// uploaded twice is stored once, and a name can never point at the wrong
// bytes. Postgres keeps the index (which project, release and name map to
// which hash); only the bytes live here. Swapping this for S3 later only
// changes this package.
package blob

import (
	"crypto/sha256"
	"encoding/hex"
	"errors"
	"fmt"
	"io"
	"os"
	"path/filepath"
	"regexp"
)

var ErrTooLarge = errors.New("file is larger than the upload limit")

type Store struct {
	dir string
}

func New(dir string) (*Store, error) {
	for _, d := range []string{filepath.Join(dir, "tmp"), filepath.Join(dir, "objects")} {
		if err := os.MkdirAll(d, 0o755); err != nil {
			return nil, fmt.Errorf("create blob dir: %w", err)
		}
	}
	return &Store{dir: dir}, nil
}

type Blob struct {
	SHA256 string
	Size   int64
}

// Put copies r into the store, refusing anything over limit bytes. The
// file only appears under its final name once it is complete, so a crash
// or a failed upload never leaves a half-written object behind.
func (s *Store) Put(r io.Reader, limit int64) (Blob, error) {
	tmp, err := os.CreateTemp(filepath.Join(s.dir, "tmp"), "upload-*")
	if err != nil {
		return Blob{}, err
	}
	defer os.Remove(tmp.Name()) // a no-op after the rename below succeeds
	defer tmp.Close()

	h := sha256.New()
	// Read one byte past the limit to tell "exactly the limit" from "too big".
	n, err := io.Copy(io.MultiWriter(tmp, h), io.LimitReader(r, limit+1))
	if err != nil {
		return Blob{}, err
	}
	if n > limit {
		return Blob{}, ErrTooLarge
	}
	if err := tmp.Sync(); err != nil {
		return Blob{}, err
	}
	sum := hex.EncodeToString(h.Sum(nil))
	final := s.path(sum)
	if err := os.MkdirAll(filepath.Dir(final), 0o755); err != nil {
		return Blob{}, err
	}
	if err := os.Rename(tmp.Name(), final); err != nil {
		return Blob{}, err
	}
	return Blob{SHA256: sum, Size: n}, nil
}

var validSum = regexp.MustCompile(`^[0-9a-f]{64}$`)

// Open returns the stored file for a hash.
func (s *Store) Open(sum string) (*os.File, error) {
	// The hash comes from our own database, but checking its shape means a
	// bad value can never become a path like "../../etc/passwd".
	if !validSum.MatchString(sum) {
		return nil, fmt.Errorf("invalid blob hash %q", sum)
	}
	return os.Open(s.path(sum))
}

// objects/ab/abcdef…: two-character folders keep any one directory small.
func (s *Store) path(sum string) string {
	return filepath.Join(s.dir, "objects", sum[:2], sum)
}
