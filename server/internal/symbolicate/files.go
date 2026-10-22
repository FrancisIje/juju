package symbolicate

import (
	"context"
	"errors"
	"io"

	"juju/internal/blob"
	"juju/internal/debugid"
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

// SourceMapByDebugID finds a source map by the debug id inside it, which
// the upload stored it under (see Validate).
func (f StoredFiles) SourceMapByDebugID(ctx context.Context, projectID int64, debugID string) ([]byte, error) {
	return f.read(ctx, projectID, "sourcemap", "", debugID)
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

// Validate checks an uploaded file is what it claims to be, and returns the
// id to store it under: a Dart file's build id, or a source map's debug id
// ("" if it has none). Rejecting a bad upload with a clear message beats
// discovering it when the next crash arrives.
func Validate(kind string, data []byte) (id string, err error) {
	switch kind {
	case "sourcemap":
		if _, err := parseSourceMap("upload", data); err != nil {
			return "", err
		}
		return debugid.FromSourceMap(data), nil
	case "dart_symbols":
		syms, err := parseDartSymbols(data)
		if err != nil {
			return "", err
		}
		return syms.buildID, nil
	}
	return "", errors.New(`kind must be "sourcemap" or "dart_symbols"`)
}
