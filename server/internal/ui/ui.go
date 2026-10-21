// Package ui is the JSON API behind the dashboard, under /api/ui/. It uses
// login sessions (cookies), unlike /api/v1, which uses project keys for
// SDKs and scripts.
package ui

import (
	"context"
	"encoding/json"
	"errors"
	"log/slog"
	"net"
	"net/http"
	"net/mail"
	"strconv"
	"strings"
	"time"

	"juju/internal/alert"
	"juju/internal/analytics"
	"juju/internal/auth"
	"juju/internal/httpx"
	"juju/internal/live"
	"juju/internal/store"
)

type API struct {
	Store       *store.Store
	Auth        *auth.Service
	Alerts      *alert.Notifier
	Live        *live.Hub         // optional: without it, /live answers 404 and the dashboard polls
	Analytics   *analytics.Client // optional: nil when ClickHouse isn't configured
	PublicURL   string            // used to build DSNs, e.g. https://juju.example.com
	AllowSignup bool              // can anyone create an account and organization? (the first account always can)
	Log         *slog.Logger
}

func (a *API) Register(mux *http.ServeMux) {
	// Before login.
	mux.HandleFunc("GET /api/ui/auth/setup", a.setup)
	mux.Handle("POST /api/ui/auth/signup", auth.RequireCSRF(http.HandlerFunc(a.signup)))
	mux.Handle("POST /api/ui/auth/login", auth.RequireCSRF(http.HandlerFunc(a.login)))

	// Logged in.
	in := func(h http.HandlerFunc) http.Handler { return a.Auth.Middleware(h) }
	mux.Handle("POST /api/ui/auth/logout", in(a.logout))
	mux.Handle("GET /api/ui/me", in(a.me))
	mux.Handle("POST /api/ui/orgs/{org}/projects", in(a.createProject))

	// Logged in and a member of the project's organization.
	p := func(h projectHandler) http.Handler { return a.Auth.Middleware(a.member(h)) }
	mux.Handle("GET /api/ui/projects/{project}", p(a.project))
	mux.Handle("POST /api/ui/projects/{project}/rotate-secret", p(a.rotateSecret))
	mux.Handle("GET /api/ui/projects/{project}/issues", p(a.issues))
	mux.Handle("POST /api/ui/projects/{project}/issues/status", p(a.setStatus))
	mux.Handle("GET /api/ui/projects/{project}/issues/{issue}", p(a.issue))
	mux.Handle("GET /api/ui/projects/{project}/issues/{issue}/events", p(a.issueEvents))
	mux.Handle("GET /api/ui/projects/{project}/alerts", p(a.getAlerts))
	mux.Handle("PUT /api/ui/projects/{project}/alerts", p(a.putAlerts))
	mux.Handle("POST /api/ui/projects/{project}/alerts/test", p(a.testAlerts))
	mux.Handle("GET /api/ui/projects/{project}/files", p(a.files))
	mux.Handle("GET /api/ui/projects/{project}/live", p(a.live))

	a.registerTeam(mux)
	a.registerAnalytics(mux)
}

type projectHandler func(w http.ResponseWriter, r *http.Request, projectID int64, user store.User)

// member checks the user belongs to the project's organization. Not being a
// member looks exactly like the project not existing, so nobody can probe
// which project ids exist.
func (a *API) member(next projectHandler) http.HandlerFunc {
	return func(w http.ResponseWriter, r *http.Request) {
		u, _ := auth.UserFrom(r.Context())
		pid, err := strconv.ParseInt(r.PathValue("project"), 10, 64)
		if err != nil {
			httpx.WriteError(w, http.StatusNotFound, "project not found")
			return
		}
		if _, err := a.Store.MemberRole(r.Context(), u.ID, pid); err != nil {
			if errors.Is(err, store.ErrNotFound) {
				httpx.WriteError(w, http.StatusNotFound, "project not found")
				return
			}
			a.fail(w, "check membership", err)
			return
		}
		next(w, r, pid, u)
	}
}

func (a *API) setup(w http.ResponseWriter, r *http.Request) {
	n, err := a.Store.CountUsers(r.Context())
	if err != nil {
		a.fail(w, "count users", err)
		return
	}
	httpx.WriteJSON(w, http.StatusOK, map[string]bool{"needs_setup": n == 0, "signup_open": n == 0 || a.AllowSignup})
}

type signupRequest struct {
	Email        string `json:"email"`
	Name         string `json:"name"`
	Password     string `json:"password"`
	Organization string `json:"organization"`
	// Invite is the token from an invite link. With one, sign-up works even
	// when it's closed, and joins the inviting organization instead of
	// creating one.
	Invite string `json:"invite"`
}

func (a *API) signup(w http.ResponseWriter, r *http.Request) {
	var req signupRequest
	if !decode(w, r, &req) {
		return
	}
	n, err := a.Store.CountUsers(r.Context())
	if err != nil {
		a.fail(w, "count users", err)
		return
	}
	if n > 0 && !a.AllowSignup && req.Invite == "" {
		httpx.WriteError(w, http.StatusForbidden, "sign-up is turned off on this server; to join a team, open the invite link they sent you")
		return
	}
	email := auth.NormalizeEmail(req.Email)
	if _, err := mail.ParseAddress(email); err != nil || !strings.Contains(email, "@") {
		httpx.WriteError(w, http.StatusBadRequest, "enter a valid email address")
		return
	}
	if err := auth.ValidatePassword(req.Password); err != nil {
		httpx.WriteError(w, http.StatusBadRequest, err.Error())
		return
	}
	org := strings.TrimSpace(req.Organization)
	if org == "" && req.Invite == "" {
		httpx.WriteError(w, http.StatusBadRequest, "name your organization, for example your company or team")
		return
	}
	hash, err := auth.HashPassword(req.Password)
	if err != nil {
		a.fail(w, "hash password", err)
		return
	}
	var u store.User
	if req.Invite != "" {
		u, _, err = a.Store.CreateUserWithInvite(r.Context(), email, strings.TrimSpace(req.Name), hash, auth.HashToken(req.Invite))
	} else {
		u, err = a.Store.CreateUser(r.Context(), email, strings.TrimSpace(req.Name), hash, org, store.NewOrganization)
	}
	if errors.Is(err, store.ErrInviteInvalid) {
		httpx.WriteError(w, http.StatusNotFound, inviteGone)
		return
	}
	if errors.Is(err, store.ErrConflict) {
		httpx.WriteError(w, http.StatusConflict, "an account with this email already exists; log in instead")
		return
	}
	if errors.Is(err, store.ErrOrgTaken) {
		httpx.WriteError(w, http.StatusConflict, "an organization with this name already exists; choose another name, or ask its owner to add you")
		return
	}
	if err != nil {
		a.fail(w, "create user", err)
		return
	}
	if err := a.Auth.StartSession(r.Context(), w, u.ID); err != nil {
		a.fail(w, "start session", err)
		return
	}
	httpx.WriteJSON(w, http.StatusCreated, u)
}

func (a *API) login(w http.ResponseWriter, r *http.Request) {
	var req struct{ Email, Password string }
	if !decode(w, r, &req) {
		return
	}
	u, err := a.Auth.Login(r.Context(), w, req.Email, req.Password, clientIP(r))
	switch {
	case errors.Is(err, auth.ErrBadLogin):
		httpx.WriteError(w, http.StatusUnauthorized, err.Error())
	case errors.Is(err, auth.ErrTooManyAttempts):
		httpx.WriteError(w, http.StatusTooManyRequests, err.Error())
	case err != nil:
		a.fail(w, "login", err)
	default:
		httpx.WriteJSON(w, http.StatusOK, u)
	}
}

func (a *API) logout(w http.ResponseWriter, r *http.Request) {
	if err := a.Auth.Logout(r.Context(), w, r); err != nil {
		a.fail(w, "logout", err)
		return
	}
	w.WriteHeader(http.StatusNoContent)
}

func (a *API) me(w http.ResponseWriter, r *http.Request) {
	u, _ := auth.UserFrom(r.Context())
	orgs, err := a.Store.UserOrgs(r.Context(), u.ID)
	if err != nil {
		a.fail(w, "list orgs", err)
		return
	}
	if orgs == nil {
		orgs = []store.OrgProjects{}
	}
	httpx.WriteJSON(w, http.StatusOK, map[string]any{"user": u, "organizations": orgs})
}

// projectInfo is what the setup and settings pages need. secret_key is only
// ever set right after creation or rotation.
type projectInfo struct {
	ID        int64  `json:"id"`
	Name      string `json:"name"`
	Org       string `json:"organization"`
	PublicKey string `json:"public_key"`
	DSN       string `json:"dsn"`
	SecretKey string `json:"secret_key,omitempty"`
}

func (a *API) info(p store.Project) projectInfo {
	return projectInfo{ID: p.ID, Name: p.Name, Org: p.Org, PublicKey: p.PublicKey, DSN: dsn(a.PublicURL, p.PublicKey, p.ID), SecretKey: p.SecretKey}
}

// dsn builds https://pk_…@host/123 from the server's public URL.
func dsn(publicURL, key string, projectID int64) string {
	scheme, rest, ok := strings.Cut(strings.TrimRight(publicURL, "/"), "://")
	if !ok {
		scheme, rest = "http", publicURL
	}
	return scheme + "://" + key + "@" + rest + "/" + strconv.FormatInt(projectID, 10)
}

func (a *API) createProject(w http.ResponseWriter, r *http.Request) {
	u, _ := auth.UserFrom(r.Context())
	orgID, err := strconv.ParseInt(r.PathValue("org"), 10, 64)
	if err != nil {
		httpx.WriteError(w, http.StatusNotFound, "organization not found")
		return
	}
	if _, err := a.Store.OrgRole(r.Context(), u.ID, orgID); err != nil {
		httpx.WriteError(w, http.StatusNotFound, "organization not found")
		return
	}
	var req struct{ Name string }
	if !decode(w, r, &req) {
		return
	}
	name := strings.TrimSpace(req.Name)
	if name == "" || len(name) > 80 {
		httpx.WriteError(w, http.StatusBadRequest, "give the project a name (up to 80 characters), like shop-web or shop-ios")
		return
	}
	p, err := a.Store.CreateProjectInOrg(r.Context(), orgID, name)
	if errors.Is(err, store.ErrConflict) {
		httpx.WriteError(w, http.StatusConflict, "this organization already has a project with that name")
		return
	}
	if err != nil {
		a.fail(w, "create project", err)
		return
	}
	httpx.WriteJSON(w, http.StatusCreated, a.info(p))
}

func (a *API) project(w http.ResponseWriter, r *http.Request, pid int64, _ store.User) {
	p, err := a.Store.ProjectByID(r.Context(), pid)
	if err != nil {
		a.fail(w, "load project", err)
		return
	}
	counts, err := a.Store.CountIssues(r.Context(), pid)
	if err != nil {
		a.fail(w, "count issues", err)
		return
	}
	envs, err := a.Store.Environments(r.Context(), pid)
	if err != nil {
		a.fail(w, "list environments", err)
		return
	}
	httpx.WriteJSON(w, http.StatusOK, map[string]any{"project": a.info(p), "counts": counts, "environments": envs})
}

func (a *API) rotateSecret(w http.ResponseWriter, r *http.Request, pid int64, _ store.User) {
	secret, err := a.Store.RotateSecret(r.Context(), pid)
	if err != nil {
		a.fail(w, "rotate secret", err)
		return
	}
	httpx.WriteJSON(w, http.StatusOK, map[string]string{"secret_key": secret})
}

func (a *API) issues(w http.ResponseWriter, r *http.Request, pid int64, _ store.User) {
	q := r.URL.Query()
	status := q.Get("status")
	if status == "all" {
		status = ""
	} else if status == "" {
		status = "unresolved"
	}
	if status != "" && status != "unresolved" && status != "resolved" && status != "ignored" {
		httpx.WriteError(w, http.StatusBadRequest, "status must be unresolved, resolved, ignored or all")
		return
	}
	limit := clamp(atoi(q.Get("limit"), 50), 1, 100)
	f := store.IssueFilter{Status: status, Query: q.Get("q"), Environment: q.Get("environment"), Sort: q.Get("sort"), Limit: limit, Offset: max(0, atoi(q.Get("offset"), 0))}
	list, total, err := a.Store.SearchIssues(r.Context(), pid, f)
	if err != nil {
		a.fail(w, "search issues", err)
		return
	}
	if list == nil {
		list = []store.IssueRow{}
	}
	counts, err := a.Store.CountIssues(r.Context(), pid)
	if err != nil {
		a.fail(w, "count issues", err)
		return
	}
	httpx.WriteJSON(w, http.StatusOK, map[string]any{"issues": list, "total": total, "counts": counts})
}

func (a *API) issue(w http.ResponseWriter, r *http.Request, pid int64, _ store.User) {
	id, err := strconv.ParseInt(r.PathValue("issue"), 10, 64)
	if err != nil {
		httpx.WriteError(w, http.StatusNotFound, "issue not found")
		return
	}
	d, err := a.Store.IssueDetail(r.Context(), pid, id)
	if errors.Is(err, store.ErrNotFound) {
		httpx.WriteError(w, http.StatusNotFound, "issue not found")
		return
	}
	if err != nil {
		a.fail(w, "load issue", err)
		return
	}
	if d.Activity == nil {
		d.Activity = []store.Activity{}
	}
	httpx.WriteJSON(w, http.StatusOK, d)
}

func (a *API) issueEvents(w http.ResponseWriter, r *http.Request, pid int64, _ store.User) {
	id, err := strconv.ParseInt(r.PathValue("issue"), 10, 64)
	if err != nil {
		httpx.WriteError(w, http.StatusNotFound, "issue not found")
		return
	}
	var before time.Time
	if b := r.URL.Query().Get("before"); b != "" {
		if before, err = time.Parse(time.RFC3339Nano, b); err != nil {
			httpx.WriteError(w, http.StatusBadRequest, "before must be an RFC 3339 time")
			return
		}
	}
	events, err := a.Store.IssueEvents(r.Context(), pid, id, before, clamp(atoi(r.URL.Query().Get("limit"), 20), 1, 100))
	if err != nil {
		a.fail(w, "list events", err)
		return
	}
	if events == nil {
		events = []json.RawMessage{}
	}
	httpx.WriteJSON(w, http.StatusOK, map[string]any{"events": events})
}

func (a *API) setStatus(w http.ResponseWriter, r *http.Request, pid int64, u store.User) {
	var req struct {
		IDs    []int64 `json:"ids"`
		Status string  `json:"status"`
	}
	if !decode(w, r, &req) {
		return
	}
	if len(req.IDs) == 0 || len(req.IDs) > 100 {
		httpx.WriteError(w, http.StatusBadRequest, "choose between 1 and 100 issues")
		return
	}
	switch req.Status {
	case "resolved", "ignored", "unresolved":
	default:
		httpx.WriteError(w, http.StatusBadRequest, "status must be resolved, ignored or unresolved")
		return
	}
	n, err := a.Store.SetIssueStatus(r.Context(), pid, req.IDs, req.Status, u.ID)
	if err != nil {
		a.fail(w, "set status", err)
		return
	}
	// Teammates with the project open see the change too.
	if n > 0 && a.Live != nil {
		a.Live.Changed(pid)
	}
	httpx.WriteJSON(w, http.StatusOK, map[string]int64{"changed": n})
}

// alertsView hides most of the webhook URL: it works like a password, since
// anyone who has it can post into the channel.
type alertsView struct {
	SlackWebhookSet  bool             `json:"slack_webhook_set"`
	SlackWebhookHint string           `json:"slack_webhook_hint"`
	EmailTo          []string         `json:"email_to"`
	OnNewIssue       bool             `json:"on_new_issue"`
	OnRegression     bool             `json:"on_regression"`
	EmailAvailable   bool             `json:"email_available"`
	Deliveries       []store.Delivery `json:"deliveries"`
}

func (a *API) alertsView(ctx context.Context, pid int64) (alertsView, error) {
	s, err := a.Store.AlertSettings(ctx, pid)
	if err != nil {
		return alertsView{}, err
	}
	d, err := a.Store.RecentDeliveries(ctx, pid, 10)
	if err != nil {
		return alertsView{}, err
	}
	if d == nil {
		d = []store.Delivery{}
	}
	v := alertsView{SlackWebhookSet: s.SlackWebhookURL != "", EmailTo: s.EmailTo, OnNewIssue: s.OnNewIssue, OnRegression: s.OnRegression, EmailAvailable: a.Alerts.SMTP.Addr != "", Deliveries: d}
	if v.SlackWebhookSet {
		v.SlackWebhookHint = "…" + s.SlackWebhookURL[max(0, len(s.SlackWebhookURL)-4):]
	}
	return v, nil
}

func (a *API) getAlerts(w http.ResponseWriter, r *http.Request, pid int64, _ store.User) {
	v, err := a.alertsView(r.Context(), pid)
	if err != nil {
		a.fail(w, "load alerts", err)
		return
	}
	httpx.WriteJSON(w, http.StatusOK, v)
}

type alertsUpdate struct {
	// nil keeps the saved webhook; "" removes it.
	SlackWebhookURL *string  `json:"slack_webhook_url"`
	EmailTo         []string `json:"email_to"`
	OnNewIssue      bool     `json:"on_new_issue"`
	OnRegression    bool     `json:"on_regression"`
}

func (a *API) putAlerts(w http.ResponseWriter, r *http.Request, pid int64, _ store.User) {
	var req alertsUpdate
	if !decode(w, r, &req) {
		return
	}
	cur, err := a.Store.AlertSettings(r.Context(), pid)
	if err != nil {
		a.fail(w, "load alerts", err)
		return
	}
	next := store.AlertSettings{SlackWebhookURL: cur.SlackWebhookURL, OnNewIssue: req.OnNewIssue, OnRegression: req.OnRegression}
	if req.SlackWebhookURL != nil {
		next.SlackWebhookURL = strings.TrimSpace(*req.SlackWebhookURL)
		if next.SlackWebhookURL != "" {
			if err := alert.ValidateWebhookURL(next.SlackWebhookURL, a.Alerts.AllowHTTPWebhooks); err != nil {
				httpx.WriteError(w, http.StatusBadRequest, err.Error())
				return
			}
		}
	}
	for _, e := range req.EmailTo {
		e = auth.NormalizeEmail(e)
		if e == "" {
			continue
		}
		if _, err := mail.ParseAddress(e); err != nil {
			httpx.WriteError(w, http.StatusBadRequest, e+" isn't a valid email address")
			return
		}
		next.EmailTo = append(next.EmailTo, e)
	}
	if len(next.EmailTo) > 20 {
		httpx.WriteError(w, http.StatusBadRequest, "add at most 20 email addresses")
		return
	}
	if err := a.Store.SaveAlertSettings(r.Context(), pid, next); err != nil {
		a.fail(w, "save alerts", err)
		return
	}
	a.getAlerts(w, r, pid, store.User{})
}

func (a *API) testAlerts(w http.ResponseWriter, r *http.Request, pid int64, _ store.User) {
	s, err := a.Store.AlertSettings(r.Context(), pid)
	if err != nil {
		a.fail(w, "load alerts", err)
		return
	}
	if s.SlackWebhookURL == "" && len(s.EmailTo) == 0 {
		httpx.WriteError(w, http.StatusBadRequest, "add a Slack webhook or an email address first")
		return
	}
	httpx.WriteJSON(w, http.StatusOK, map[string]any{"results": a.Alerts.SendTest(r.Context(), pid, s)})
}

func (a *API) files(w http.ResponseWriter, r *http.Request, pid int64, _ store.User) {
	files, err := a.Store.ListDebugFiles(r.Context(), pid, r.URL.Query().Get("release"))
	if err != nil {
		a.fail(w, "list files", err)
		return
	}
	if files == nil {
		files = []store.DebugFile{}
	}
	httpx.WriteJSON(w, http.StatusOK, map[string]any{"files": files})
}

// live streams "refresh" signals (server-sent events) while the project
// is open in a dashboard. See package live.
func (a *API) live(w http.ResponseWriter, r *http.Request, pid int64, _ store.User) {
	if a.Live == nil {
		httpx.WriteError(w, http.StatusNotFound, "live updates are off")
		return
	}
	a.Live.Serve(w, r, pid)
}

func (a *API) fail(w http.ResponseWriter, what string, err error) {
	a.Log.Error(what, "err", err)
	httpx.WriteError(w, http.StatusInternalServerError, "something went wrong on the server; try again")
}

// decode reads a JSON body (at most 64 KB) and answers 400 if it isn't one.
func decode(w http.ResponseWriter, r *http.Request, v any) bool {
	dec := json.NewDecoder(http.MaxBytesReader(w, r.Body, 64<<10))
	if err := dec.Decode(v); err != nil {
		httpx.WriteError(w, http.StatusBadRequest, "the request body must be JSON")
		return false
	}
	return true
}

func clientIP(r *http.Request) string {
	host, _, err := net.SplitHostPort(r.RemoteAddr)
	if err != nil {
		return r.RemoteAddr
	}
	return host
}

func atoi(s string, fallback int) int {
	n, err := strconv.Atoi(s)
	if err != nil {
		return fallback
	}
	return n
}

func clamp(n, lo, hi int) int { return min(max(n, lo), hi) }
