package api

import (
	"encoding/json"
	"errors"
	"net/http"
	"net/url"
	"regexp"
	"strconv"
	"strings"

	"juju/internal/httpx"
	"juju/internal/store"
)

// The endpoints in this file let a program (CI, a script, an AI agent) do
// what a person does in the dashboard to fix a bug: read an issue in full,
// find the commit that crashed, mark the issue resolved and link the fix.
//
//	GET  /api/v1/projects/{id}/issues/{issue_id}
//	POST /api/v1/projects/{id}/issues/{issue_id}/status   {"status": "resolved"}
//	POST /api/v1/projects/{id}/issues/{issue_id}/links    {"url": "...", "title": "..."}
//	POST /api/v1/projects/{id}/releases                   {"name": "web@1.2.0", "commit": "...", "repository": "..."}

const (
	maxReleaseName = 200 // same as event.maxRelease, so every release an SDK sends fits
	maxURL         = 2000
	maxTitle       = 200
)

// A git commit id: SHA-1 (40 hex) or SHA-256 (64), or an abbreviation of
// at least 7, as `git rev-parse --short` prints.
var commitRE = regexp.MustCompile(`^[0-9a-f]{7,64}$`)

func (a *API) issueDetail(w http.ResponseWriter, r *http.Request, projectID int64) {
	issueID, ok := issueParam(w, r)
	if !ok {
		return
	}
	d, err := a.Store.IssueDetail(r.Context(), projectID, issueID)
	if errors.Is(err, store.ErrNotFound) {
		httpx.WriteError(w, http.StatusNotFound, "unknown issue")
		return
	}
	if err != nil {
		a.fail(w, "load issue", err)
		return
	}
	httpx.WriteJSON(w, http.StatusOK, d)
}

func (a *API) setStatus(w http.ResponseWriter, r *http.Request, projectID int64) {
	issueID, ok := issueParam(w, r)
	if !ok {
		return
	}
	var req struct {
		Status string `json:"status"`
	}
	if !decode(w, r, &req) {
		return
	}
	changed, err := a.changeStatus(r, projectID, issueID, req.Status)
	if err != nil {
		a.writeErr(w, err)
		return
	}
	httpx.WriteJSON(w, http.StatusOK, map[string]any{"changed": changed})
}

// changeStatus is shared by the REST endpoint and the MCP tool.
func (a *API) changeStatus(r *http.Request, projectID, issueID int64, status string) (bool, error) {
	if status != "resolved" && status != "ignored" && status != "unresolved" {
		return false, badRequest("status must be resolved, ignored or unresolved")
	}
	// Check the issue exists first, so an unknown id is a 404 rather than
	// "nothing changed".
	if _, err := a.Store.IssueDetail(r.Context(), projectID, issueID); err != nil {
		return false, err
	}
	// userID 0: changed by a key, not a person; the activity log shows no name.
	n, err := a.Store.SetIssueStatus(r.Context(), projectID, []int64{issueID}, status, 0)
	if err != nil {
		return false, err
	}
	if n > 0 && a.Changes != nil {
		a.Changes.Changed(projectID) // open dashboards refresh
	}
	return n > 0, nil
}

func (a *API) addLink(w http.ResponseWriter, r *http.Request, projectID int64) {
	issueID, ok := issueParam(w, r)
	if !ok {
		return
	}
	var req store.IssueLink
	if !decode(w, r, &req) {
		return
	}
	l, err := a.linkIssue(r, projectID, issueID, req.URL, req.Title)
	if err != nil {
		a.writeErr(w, err)
		return
	}
	httpx.WriteJSON(w, http.StatusCreated, l)
}

func (a *API) linkIssue(r *http.Request, projectID, issueID int64, rawURL, title string) (store.IssueLink, error) {
	u, err := cleanURL(rawURL)
	if err != nil {
		return store.IssueLink{}, err
	}
	title = strings.TrimSpace(title)
	if len(title) > maxTitle {
		title = title[:maxTitle]
	}
	l, err := a.Store.AddIssueLink(r.Context(), projectID, issueID, store.IssueLink{URL: u, Title: title})
	if err == nil && a.Changes != nil {
		a.Changes.Changed(projectID)
	}
	return l, err
}

func (a *API) saveRelease(w http.ResponseWriter, r *http.Request, projectID int64) {
	var req store.Release
	if !decode(w, r, &req) {
		return
	}
	req.Name = strings.TrimSpace(req.Name)
	req.Commit = strings.ToLower(strings.TrimSpace(req.Commit))
	switch {
	case req.Name == "" || len(req.Name) > maxReleaseName:
		httpx.WriteError(w, http.StatusBadRequest, "name must be the release your SDK sends, 1 to 200 characters")
		return
	case req.Commit != "" && !commitRE.MatchString(req.Commit):
		httpx.WriteError(w, http.StatusBadRequest, "commit must be a git commit id (7 to 64 hex characters)")
		return
	}
	if req.Repository != "" {
		repo, err := cleanURL(req.Repository)
		if err != nil {
			httpx.WriteError(w, http.StatusBadRequest, "repository: "+err.Error())
			return
		}
		req.Repository = repo
	}
	saved, err := a.Store.SaveRelease(r.Context(), projectID, req)
	if err != nil {
		a.fail(w, "save release", err)
		return
	}
	httpx.WriteJSON(w, http.StatusOK, saved)
}

// cleanURL accepts only http(s) URLs without a user name or password. CI
// remotes often look like https://x-access-token:ghs_...@github.com/o/r,
// and that token must not end up stored and shown to everyone on the team.
func cleanURL(raw string) (string, error) {
	raw = strings.TrimSpace(raw)
	u, err := url.Parse(raw)
	if err != nil || u.Host == "" || (u.Scheme != "https" && u.Scheme != "http") {
		return "", badRequest("url must be an http:// or https:// URL")
	}
	if u.User != nil {
		return "", badRequest("url must not contain a user name or password")
	}
	if len(raw) > maxURL {
		return "", badRequest("url is too long")
	}
	return u.String(), nil
}

// badRequest is an error whose message is safe to show the caller.
type badRequest string

func (e badRequest) Error() string { return string(e) }

// writeErr turns an error from a shared helper into a response: the
// caller's mistake → 400, unknown issue → 404, anything else is ours.
func (a *API) writeErr(w http.ResponseWriter, err error) {
	var br badRequest
	switch {
	case errors.As(err, &br):
		httpx.WriteError(w, http.StatusBadRequest, br.Error())
	case errors.Is(err, store.ErrNotFound):
		httpx.WriteError(w, http.StatusNotFound, "unknown issue")
	default:
		a.fail(w, "issue update", err)
	}
}

func issueParam(w http.ResponseWriter, r *http.Request) (int64, bool) {
	id, err := strconv.ParseInt(r.PathValue("issue_id"), 10, 64)
	if err != nil || id <= 0 {
		httpx.WriteError(w, http.StatusNotFound, "unknown issue")
		return 0, false
	}
	return id, true
}

func decode(w http.ResponseWriter, r *http.Request, v any) bool {
	if err := json.NewDecoder(http.MaxBytesReader(w, r.Body, 64<<10)).Decode(v); err != nil {
		httpx.WriteError(w, http.StatusBadRequest, "the request body must be JSON")
		return false
	}
	return true
}
