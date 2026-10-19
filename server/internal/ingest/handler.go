// Package ingest is the HTTP endpoint SDKs send events to:
//
//	POST /api/v1/projects/{id}/events
//
// It does only fast work (auth, size limits, validation) and hands accepted
// events to the queue, so the SDK gets its reply in microseconds and a slow
// database never slows down the apps we monitor.
//
// Status codes are instructions to the SDK:
//
//	202 stored or queued: delete from your queue
//	400 / 401 / 413 / 415 broken request: don't retry, it will never work
//	429 / 5xx temporary: keep the events and retry with backoff
package ingest

import (
	"bytes"
	"compress/gzip"
	"context"
	"crypto/subtle"
	"errors"
	"fmt"
	"io"
	"log/slog"
	"mime"
	"net/http"
	"strconv"
	"time"

	"juju/internal/event"
	"juju/internal/httpx"
	"juju/internal/queue"
	"juju/internal/store"
)

// Keys looks up a project's public key. In production this hits Postgres on
// every request; a small in-memory cache is an easy later optimization.
type Keys interface {
	ProjectPublicKey(ctx context.Context, projectID int64) (string, error)
}

type Handler struct {
	Keys  Keys
	Queue queue.Queue
	Log   *slog.Logger
	Now   func() time.Time // injectable clock so tests are deterministic
}

type DroppedEvent struct {
	Index  int    `json:"index"`
	Reason string `json:"reason"`
}

type Response struct {
	Accepted int            `json:"accepted"`
	Dropped  []DroppedEvent `json:"dropped"`
}

func (h *Handler) ServeHTTP(w http.ResponseWriter, r *http.Request) {
	allowCORS(w)

	// Layer 1: the HTTP request itself. Cheapest checks first, so bad
	// requests cost us almost nothing.
	projectID, err := strconv.ParseInt(r.PathValue("id"), 10, 64)
	if err != nil || projectID <= 0 {
		httpx.WriteError(w, http.StatusNotFound, "unknown project")
		return
	}
	if status, msg := h.authenticate(r, projectID); status != 0 {
		httpx.WriteError(w, status, msg)
		return
	}
	// text/plain is accepted for sendBeacon: a beacon with a JSON content
	// type needs a CORS preflight, which beacons can't do, so browsers refuse
	// to send it. The body must still be valid JSON either way.
	if mt, _, _ := mime.ParseMediaType(r.Header.Get("Content-Type")); mt != "application/json" && mt != "text/plain" {
		httpx.WriteError(w, http.StatusUnsupportedMediaType, "Content-Type must be application/json")
		return
	}
	body, status, err := readBody(w, r)
	if err != nil {
		httpx.WriteError(w, status, err.Error())
		return
	}
	// Debug level only: one line per request would be noisy in production,
	// but it's how you check what an SDK actually sends.
	h.Log.Debug("envelope received",
		"project_id", projectID,
		"content_type", r.Header.Get("Content-Type"),
		"content_encoding", r.Header.Get("Content-Encoding"),
		"wire_bytes", r.ContentLength,
		"json_bytes", len(body),
	)

	// Layers 2 and 3: valid JSON, and a sane envelope.
	env, err := event.ParseEnvelope(body)
	if err != nil {
		httpx.WriteError(w, http.StatusBadRequest, err.Error())
		return
	}

	// Layer 4: each event on its own. Bad ones are dropped, good ones kept.
	now := h.Now()
	resp := Response{Dropped: []DroppedEvent{}}
	var accepted []*event.Event
	for i, raw := range env.Events {
		e, err := event.Normalize(raw, now)
		if err != nil {
			resp.Dropped = append(resp.Dropped, DroppedEvent{Index: i, Reason: err.Error()})
			continue
		}
		e.ProjectID = projectID
		e.SDK = env.SDK
		accepted = append(accepted, e)
	}
	if len(accepted) == 0 {
		// Nothing usable: 400 tells the SDK not to retry this batch.
		httpx.WriteJSON(w, http.StatusBadRequest, resp)
		return
	}

	for _, e := range accepted {
		if !h.Queue.TryEnqueue(e) {
			// Workers are behind. Some events of this batch may already be
			// queued; that is fine, because the SDK retries the whole batch
			// and dedupe on event_id drops the copies.
			h.Log.Warn("queue full, rejecting batch", "project_id", projectID)
			w.Header().Set("Retry-After", "5")
			httpx.WriteError(w, http.StatusTooManyRequests, "server is busy, retry later")
			return
		}
	}
	resp.Accepted = len(accepted)
	httpx.WriteJSON(w, http.StatusAccepted, resp)
}

// authenticate returns a non-zero status when the request may not send
// events to this project.
func (h *Handler) authenticate(r *http.Request, projectID int64) (int, string) {
	got := r.Header.Get("X-Juju-Key")
	if got == "" {
		// sendBeacon can't set headers, so browsers send the key in the URL.
		// That's fine for the public key: it is already visible in every app.
		got = r.URL.Query().Get("juju_key")
	}
	if got == "" {
		return http.StatusUnauthorized, "missing X-Juju-Key header"
	}
	want, err := h.Keys.ProjectPublicKey(r.Context(), projectID)
	if errors.Is(err, store.ErrNotFound) {
		// Same answer as a wrong key, so nobody can probe which project
		// ids exist.
		return http.StatusUnauthorized, "invalid project key"
	}
	if err != nil {
		h.Log.Error("look up project key", "project_id", projectID, "err", err)
		return http.StatusServiceUnavailable, "temporarily unavailable"
	}
	// ConstantTimeCompare takes the same time whether the first or the last
	// byte differs. A normal == stops at the first difference, and that
	// timing difference can leak the key one character at a time.
	if subtle.ConstantTimeCompare([]byte(got), []byte(want)) != 1 {
		return http.StatusUnauthorized, "invalid project key"
	}
	return 0, ""
}

// readBody reads the request body, decompressing gzip, and enforces
// MaxBodyBytes both on the bytes received and on the decompressed size.
func readBody(w http.ResponseWriter, r *http.Request) ([]byte, int, error) {
	tooBig := fmt.Errorf("body is larger than %d bytes", event.MaxBodyBytes)

	// MaxBytesReader limits what we read off the network.
	var src io.Reader = http.MaxBytesReader(w, r.Body, event.MaxBodyBytes)

	switch enc := r.Header.Get("Content-Encoding"); enc {
	case "", "identity":
	case "gzip":
		gz, err := gzip.NewReader(src)
		if err != nil {
			return nil, http.StatusBadRequest, fmt.Errorf("invalid gzip body")
		}
		defer gz.Close()
		// A gzip bomb is a few KB that inflates to gigabytes. Limiting the
		// *decompressed* stream too means we never hold more than 1 MB.
		// We read one byte past the limit to detect "too big".
		src = io.LimitReader(gz, event.MaxBodyBytes+1)
	default:
		return nil, http.StatusUnsupportedMediaType, fmt.Errorf("unsupported Content-Encoding %q (use gzip)", enc)
	}

	var buf bytes.Buffer
	if _, err := buf.ReadFrom(src); err != nil {
		var maxErr *http.MaxBytesError
		if errors.As(err, &maxErr) {
			return nil, http.StatusRequestEntityTooLarge, tooBig
		}
		return nil, http.StatusBadRequest, fmt.Errorf("could not read body")
	}
	if buf.Len() > event.MaxBodyBytes {
		return nil, http.StatusRequestEntityTooLarge, tooBig
	}
	return buf.Bytes(), 0, nil
}
