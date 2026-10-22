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
//	POST /api/v1/projects/{id}/files?kind=sourcemap                  (a map with a debug id)
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
		// release and name are checked after reading the file: a map with
		// a debug id needs neither.
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
	id, err := symbolicate.Validate(kind, data)
	if err != nil {
		httpx.WriteError(w, http.StatusBadRequest, err.Error())
		return
	}
	save := func(release, name string) (store.DebugFile, bool) {
		f, err := a.Store.SaveDebugFile(r.Context(), projectID, store.DebugFile{Kind: kind, Release: release, Name: name, SHA256: b.SHA256, Size: b.Size})
		if err != nil {
			a.fail(w, "save debug file", err)
			return f, false
		}
		return f, true
	}
	if kind == "dart_symbols" {
		f, ok := save("", id)
		if ok {
			httpx.WriteJSON(w, http.StatusCreated, uploaded{DebugFile: f})
		}
		return
	}

	// Source maps: stored under the debug id inside them, which events
	// name exactly, and also under release and name when those are given,
	// for SDKs from before debug ids.
	legacy := release != "" && name != ""
	if id == "" && !legacy {
		httpx.WriteError(w, http.StatusBadRequest, "this source map has no debug id, so juju can't tell which build it belongs to: run `juju sourcemaps inject` on the build output before uploading, or upload with release (e.g. web@2.3.1) and name (e.g. app.min.js)")
		return
	}
	var f store.DebugFile
	ok := true
	if legacy {
		if f, ok = save(release, name); !ok {
			return
		}
	}
	if id != "" {
		if f, ok = save("", id); !ok {
			return
		}
	}
	httpx.WriteJSON(w, http.StatusCreated, uploaded{DebugFile: f, DebugID: id})
}

// uploaded is what an upload answers: the stored file, and the debug id it
// can be found by, if it has one.
type uploaded struct {
	store.DebugFile
	DebugID string `json:"debug_id,omitempty"`
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
