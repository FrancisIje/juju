package api

import (
	"errors"
	"io"
	"net/http"
	"strings"
	"time"

	"juju/internal/blob"
	"juju/internal/httpx"
	"juju/internal/store"
	"juju/internal/symbolicate"
)

// Upload a debug file (secret key required: these files contain your source code):
//
//	POST /api/v1/projects/{id}/files?kind=sourcemap&release=web@2.3.1&name=app.min.js
//	POST /api/v1/projects/{id}/files?kind=dart_symbols
//
// The body is the file itself. Sending raw bytes (not a multipart form)
// keeps it simple to stream: `curl --data-binary @app.min.js.map` works.
func (a *API) uploadFile(w http.ResponseWriter, r *http.Request, projectID int64) {
	// The server's 30 s timeouts suit event posts, but a large symbols file
	// from a CI machine can take minutes. Extend them for this request only.
	rc := http.NewResponseController(w)
	_ = rc.SetReadDeadline(time.Now().Add(15 * time.Minute))
	_ = rc.SetWriteDeadline(time.Now().Add(15 * time.Minute))

	q := r.URL.Query()
	kind, release, name := q.Get("kind"), strings.TrimSpace(q.Get("release")), strings.TrimSpace(q.Get("name"))
	switch kind {
	case "sourcemap":
		if release == "" || name == "" {
			httpx.WriteError(w, http.StatusBadRequest, "a source map needs release (the app's release, e.g. web@2.3.1) and name (the bundle it maps, e.g. app.min.js)")
			return
		}
	case "dart_symbols":
		release, name = "", "" // found by build id, which is read from the file
	default:
		httpx.WriteError(w, http.StatusBadRequest, `kind must be "sourcemap" or "dart_symbols"`)
		return
	}

	b, err := a.Blobs.Put(r.Body, a.MaxUpload)
	if errors.Is(err, blob.ErrTooLarge) {
		httpx.WriteError(w, http.StatusRequestEntityTooLarge, "file is larger than the upload limit (JUJU_MAX_UPLOAD_MB)")
		return
	}
	if err != nil {
		a.fail(w, "store upload", err)
		return
	}
	data, err := a.readBlob(b.SHA256)
	if err != nil {
		a.fail(w, "read upload", err)
		return
	}
	buildID, err := symbolicate.Validate(kind, data)
	if err != nil {
		httpx.WriteError(w, http.StatusBadRequest, err.Error())
		return
	}
	if kind == "dart_symbols" {
		name = buildID
	}
	f, err := a.Store.SaveDebugFile(r.Context(), projectID, store.DebugFile{Kind: kind, Release: release, Name: name, SHA256: b.SHA256, Size: b.Size})
	if err != nil {
		a.fail(w, "save debug file", err)
		return
	}
	httpx.WriteJSON(w, http.StatusCreated, f)
}

func (a *API) listFiles(w http.ResponseWriter, r *http.Request, projectID int64) {
	files, err := a.Store.ListDebugFiles(r.Context(), projectID, r.URL.Query().Get("release"))
	if err != nil {
		a.fail(w, "list debug files", err)
		return
	}
	if files == nil {
		files = []store.DebugFile{}
	}
	httpx.WriteJSON(w, http.StatusOK, map[string]any{"files": files})
}

func (a *API) readBlob(sum string) ([]byte, error) {
	f, err := a.Blobs.Open(sum)
	if err != nil {
		return nil, err
	}
	defer f.Close()
	return io.ReadAll(f)
}
