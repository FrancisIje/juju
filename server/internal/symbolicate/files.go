package symbolicate

import (
	"context"
	"errors"
	"io"

	"juju/internal/blob"
	"juju/internal/store"
)

// StoredFiles reads uploaded debug files: the index from Postgres, the
// bytes from the blob store.
type StoredFiles struct {
	Store *store.Store
	Blobs *blob.Store
}

func (f StoredFiles) SourceMap(ctx context.Context, projectID int64, release, name string) ([]byte, error) {
	return f.read(ctx, projectID, "sourcemap", release, name)
}

func (f StoredFiles) DartSymbols(ctx context.Context, projectID int64, buildID string) ([]byte, error) {
	return f.read(ctx, projectID, "dart_symbols", "", buildID)
}

func (f StoredFiles) read(ctx context.Context, projectID int64, kind, release, name string) ([]byte, error) {
	sum, err := f.Store.FindDebugFile(ctx, projectID, kind, release, name)
	if errors.Is(err, store.ErrNotFound) {
		return nil, ErrNotFound
	}
	if err != nil {
		return nil, err
	}
	file, err := f.Blobs.Open(sum)
	if err != nil {
		return nil, err
	}
	defer file.Close()
	return io.ReadAll(file)
}

// Validate checks an uploaded file is what it claims to be, and for Dart
// symbols returns the build id to store it under. Rejecting a bad upload
// with a clear message beats discovering it when the next crash arrives.
func Validate(kind string, data []byte) (buildID string, err error) {
	switch kind {
	case "sourcemap":
		_, err := parseSourceMap("upload", data)
		return "", err
	case "dart_symbols":
		syms, err := parseDartSymbols(data)
		if err != nil {
			return "", err
		}
		return syms.buildID, nil
	}
	return "", errors.New(`kind must be "sourcemap" or "dart_symbols"`)
}
