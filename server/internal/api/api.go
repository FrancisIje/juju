// Package api serves read endpoints for the dashboard and scripts:
//
//	GET /api/v1/projects/{id}/issues
//	GET /api/v1/projects/{id}/issues/{issue_id}/events
//
// These use the project's *secret* key (Authorization: Bearer sk_...). The
// public key cannot be used here: it is embedded in every app build, so
// anyone could extract it and read your errors.
package api

import (
	"context"
	"crypto/subtle"
	"encoding/json"
	"errors"
	"log/slog"
	"net/http"
	"strconv"
	"strings"

	"juju/internal/blob"
	"juju/internal/httpx"
	"juju/internal/store"
)

type Store interface {
	ProjectSecretHash(ctx context.Context, projectID int64) ([]byte, error)
	ListIssues(ctx context.Context, projectID int64, limit int) ([]store.Issue, error)
	ListIssueEvents(ctx context.Context, projectID, issueID int64, limit int) ([]json.RawMessage, error)
	SaveDebugFile(ctx context.Context, projectID int64, f store.DebugFile) (store.DebugFile, error)
	ListDebugFiles(ctx context.Context, projectID int64, release string) ([]store.DebugFile, error)
}

type API struct {
	Store     Store
	Blobs     *blob.Store
	MaxUpload int64 // bytes
	Log       *slog.Logger
}

const (
	defaultLimit = 50
	maxLimit     = 200
)

// Register adds the routes to mux. Go 1.22+ patterns match on method and
// path, so a POST to these paths gets 405 Method Not Allowed automatically.
func (a *API) Register(mux *http.ServeMux) {
	mux.HandleFunc("GET /api/v1/projects/{id}/issues", a.authed(a.listIssues))
	mux.HandleFunc("GET /api/v1/projects/{id}/issues/{issue_id}/events", a.authed(a.listIssueEvents))
	mux.HandleFunc("POST /api/v1/projects/{id}/files", a.authed(a.uploadFile))
	mux.HandleFunc("GET /api/v1/projects/{id}/files", a.authed(a.listFiles))
}

type projectHandler func(w http.ResponseWriter, r *http.Request, projectID int64)

// authed is middleware: it wraps a handler with the secret-key check, so
// each endpoint cannot forget it.
func (a *API) authed(next projectHandler) http.HandlerFunc {
	return func(w http.ResponseWriter, r *http.Request) {
		projectID, err := strconv.ParseInt(r.PathValue("id"), 10, 64)
		if err != nil || projectID <= 0 {
			httpx.WriteError(w, http.StatusNotFound, "unknown project")
			return
		}
		secret, ok := strings.CutPrefix(r.Header.Get("Authorization"), "Bearer ")
		if !ok || secret == "" {
			httpx.WriteError(w, http.StatusUnauthorized, "missing Authorization: Bearer <secret key>")
			return
		}
		want, err := a.Store.ProjectSecretHash(r.Context(), projectID)
		if errors.Is(err, store.ErrNotFound) {
			httpx.WriteError(w, http.StatusUnauthorized, "invalid secret key")
			return
		}
		if err != nil {
			a.Log.Error("look up secret key", "project_id", projectID, "err", err)
			httpx.WriteError(w, http.StatusServiceUnavailable, "temporarily unavailable")
			return
		}
		if subtle.ConstantTimeCompare(store.HashSecret(secret), want) != 1 {
			httpx.WriteError(w, http.StatusUnauthorized, "invalid secret key")
			return
		}
		next(w, r, projectID)
	}
}

func (a *API) listIssues(w http.ResponseWriter, r *http.Request, projectID int64) {
	issues, err := a.Store.ListIssues(r.Context(), projectID, limit(r))
	if err != nil {
		a.fail(w, "list issues", err)
		return
	}
	if issues == nil {
		issues = []store.Issue{} // encode as [] rather than null
	}
	httpx.WriteJSON(w, http.StatusOK, map[string]any{"issues": issues})
}

func (a *API) listIssueEvents(w http.ResponseWriter, r *http.Request, projectID int64) {
	issueID, err := strconv.ParseInt(r.PathValue("issue_id"), 10, 64)
	if err != nil {
		httpx.WriteError(w, http.StatusNotFound, "unknown issue")
		return
	}
	events, err := a.Store.ListIssueEvents(r.Context(), projectID, issueID, limit(r))
	if err != nil {
		a.fail(w, "list issue events", err)
		return
	}
	if events == nil {
		events = []json.RawMessage{}
	}
	httpx.WriteJSON(w, http.StatusOK, map[string]any{"events": events})
}

func (a *API) fail(w http.ResponseWriter, what string, err error) {
	// Log the real error, but don't send database details to the client.
	a.Log.Error(what, "err", err)
	httpx.WriteError(w, http.StatusInternalServerError, "internal error")
}

// limit reads ?limit=N, clamped to [1, maxLimit].
func limit(r *http.Request) int {
	n, err := strconv.Atoi(r.URL.Query().Get("limit"))
	if err != nil || n < 1 {
		return defaultLimit
	}
	return min(n, maxLimit)
}
