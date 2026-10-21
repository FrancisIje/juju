//go:build integration

// The whole dashboard flow over HTTP against a real Postgres, with the real
// worker and alert notifier: sign up, create a project, receive an error,
// resolve it, receive it again, and get a regression alert in (a fake)
// Slack. This is the Phase 6 "done when" as a test.
package ui_test

import (
	"bufio"
	"bytes"
	"context"
	"encoding/json"
	"errors"
	"fmt"
	"io"
	"log/slog"
	"net/http"
	"net/http/cookiejar"
	"net/http/httptest"
	"os"
	"path/filepath"
	"strings"
	"sync"
	"sync/atomic"
	"testing"
	"time"

	embeddedpostgres "github.com/fergusstrange/embedded-postgres"
	"github.com/google/uuid"
	"github.com/jackc/pgx/v5/pgxpool"

	"juju/internal/alert"
	"juju/internal/auth"
	"juju/internal/blob"
	"juju/internal/config"
	"juju/internal/db"
	"juju/internal/live"
	"juju/internal/queue"
	"juju/internal/server"
	"juju/internal/store"
	"juju/internal/testch"
	"juju/internal/worker"
)

var pool *pgxpool.Pool

func TestMain(m *testing.M) { os.Exit(run(m)) }

func run(m *testing.M) int {
	dir, _ := os.MkdirTemp("", "juju-ui-pg-*")
	defer os.RemoveAll(dir)
	// Its own port: go test runs packages in parallel, and the store
	// package's tests use 54329.
	pg := embeddedpostgres.NewDatabase(embeddedpostgres.DefaultConfig().Version(embeddedpostgres.V17).Port(54330).
		RuntimePath(filepath.Join(dir, "rt")).DataPath(filepath.Join(dir, "data")).Logger(nil))
	if err := pg.Start(); err != nil {
		fmt.Fprintln(os.Stderr, err)
		return 1
	}
	defer pg.Stop()
	var err error
	if pool, err = db.Open(context.Background(), "postgres://postgres:postgres@localhost:54330/postgres?sslmode=disable"); err != nil {
		fmt.Fprintln(os.Stderr, err)
		return 1
	}
	defer pool.Close()
	if err := db.Migrate(context.Background(), pool, "up"); err != nil {
		fmt.Fprintln(os.Stderr, err)
		return 1
	}
	// ClickHouse too, for the analytics endpoints; on its own ports (the
	// analytics package's tests use 18123-18126).
	u, stopCH, err := testch.Start(18133)
	if err != nil {
		fmt.Fprintln(os.Stderr, err)
		return 1
	}
	defer stopCH()
	chURL = u
	return m.Run()
}

var chURL string

// slack records messages posted to a fake incoming webhook.
type slack struct {
	mu   sync.Mutex
	msgs []string
}

func (s *slack) ServeHTTP(w http.ResponseWriter, r *http.Request) {
	var body struct{ Text string }
	_ = json.NewDecoder(r.Body).Decode(&body)
	s.mu.Lock()
	s.msgs = append(s.msgs, body.Text)
	s.mu.Unlock()
}

func (s *slack) texts() []string {
	s.mu.Lock()
	defer s.mu.Unlock()
	return append([]string(nil), s.msgs...)
}

type client struct {
	t    *testing.T
	base string
	http *http.Client
}

func newClient(t *testing.T, base string) *client {
	jar, _ := cookiejar.New(nil)
	return &client{t: t, base: base, http: &http.Client{Jar: jar}}
}

func (c *client) do(method, path string, body any, csrf bool, out any) int {
	c.t.Helper()
	var r io.Reader
	if body != nil {
		b, _ := json.Marshal(body)
		r = bytes.NewReader(b)
	}
	req, _ := http.NewRequest(method, c.base+path, r)
	req.Header.Set("Content-Type", "application/json")
	if csrf {
		req.Header.Set(auth.CSRFHeader, "1")
	}
	res, err := c.http.Do(req)
	if err != nil {
		c.t.Fatal(err)
	}
	defer res.Body.Close()
	if out != nil {
		_ = json.NewDecoder(res.Body).Decode(out)
	}
	return res.StatusCode
}

func waitFor(t *testing.T, what string, cond func() bool) {
	t.Helper()
	deadline := time.Now().Add(10 * time.Second)
	for !cond() {
		if time.Now().After(deadline) {
			t.Fatalf("timed out waiting for %s", what)
		}
		time.Sleep(50 * time.Millisecond)
	}
}

// stream opens a server-sent events stream with c's session and returns
// each message's data line.
func (c *client) stream(path string) <-chan string {
	c.t.Helper()
	ctx, cancel := context.WithCancel(context.Background())
	c.t.Cleanup(cancel)
	req, _ := http.NewRequestWithContext(ctx, "GET", c.base+path, nil)
	res, err := c.http.Do(req)
	if err != nil || res.StatusCode != 200 || res.Header.Get("Content-Type") != "text/event-stream" {
		c.t.Fatalf("open stream: %v %v", err, res)
	}
	out := make(chan string, 16)
	go func() {
		defer res.Body.Close()
		sc := bufio.NewScanner(res.Body)
		for sc.Scan() {
			if data, ok := strings.CutPrefix(sc.Text(), "data: "); ok {
				out <- data
			}
		}
	}()
	return out
}

func expectRefresh(t *testing.T, ch <-chan string, when string) {
	t.Helper()
	select {
	case m := <-ch:
		if m != "refresh" {
			t.Fatalf("%s: got %q", when, m)
		}
	case <-time.After(5 * time.Second):
		t.Fatalf("%s: no refresh within 5s", when)
	}
}

func TestDashboardFlowEndToEnd(t *testing.T) {
	log := slog.New(slog.NewTextHandler(io.Discard, nil))
	st := store.New(pool)
	q := queue.NewChan(100)
	blobs, _ := blob.New(t.TempDir())
	notifier := &alert.Notifier{Settings: st, PublicURL: "http://juju.test", AllowHTTPWebhooks: true, Log: log}
	notifier.Start(1)
	hub := live.NewHub()
	live.MinGap = 0
	go worker.Run(context.Background(), q.Events(), worker.Deps{Store: st, Alerter: notifier, Live: hub, Log: log}, 1)
	t.Cleanup(func() { q.Close(); notifier.Stop() })

	cfg := config.Config{PublicURL: "http://juju.test"}
	srv := httptest.NewServer(server.Routes(server.Deps{Pool: pool, Store: st, Queue: q, Blobs: blobs, MaxUpload: 1 << 20, Auth: &auth.Service{Store: st}, Alerts: notifier, Live: hub, Config: cfg, Log: log}))
	defer srv.Close()
	// Runs before srv.Close (defers run last first): Close waits for open
	// connections, and a live stream never ends by itself.
	defer hub.Close()
	hook := &slack{}
	slackSrv := httptest.NewServer(hook)
	defer slackSrv.Close()

	ada := newClient(t, srv.URL)

	// First-run setup needs a server with no accounts. Other tests in this
	// package share the database and create accounts, and test order isn't
	// something to rely on, so start from none.
	if _, err := pool.Exec(context.Background(), "TRUNCATE users CASCADE"); err != nil {
		t.Fatal(err)
	}

	// First run: the server has no users, so sign-up is open.
	var setup map[string]bool
	ada.do("GET", "/api/ui/auth/setup", nil, false, &setup)
	if !setup["needs_setup"] {
		t.Fatalf("setup = %v", setup)
	}
	signup := map[string]string{"email": "Ada@Example.com", "name": "Ada", "password": "correct horse battery", "organization": "acme"}
	if code := ada.do("POST", "/api/ui/auth/signup", signup, false, nil); code != 403 {
		t.Fatalf("sign-up without the CSRF header: %d, want 403", code)
	}
	if code := ada.do("POST", "/api/ui/auth/signup", signup, true, nil); code != 201 {
		t.Fatalf("sign-up: %d", code)
	}

	var me struct {
		Organizations []struct{ ID int64 } `json:"organizations"`
	}
	ada.do("GET", "/api/ui/me", nil, false, &me)
	if len(me.Organizations) != 1 {
		t.Fatalf("me = %+v", me)
	}
	org := me.Organizations[0].ID

	var proj struct {
		ID        int64  `json:"id"`
		PublicKey string `json:"public_key"`
		DSN       string `json:"dsn"`
		SecretKey string `json:"secret_key"`
	}
	if code := ada.do("POST", fmt.Sprintf("/api/ui/orgs/%d/projects", org), map[string]string{"name": "shop-web"}, true, &proj); code != 201 {
		t.Fatalf("create project: %d", code)
	}
	if !strings.HasPrefix(proj.DSN, "http://"+proj.PublicKey+"@juju.test/") || proj.SecretKey == "" {
		t.Fatalf("project = %+v", proj)
	}

	alerts := map[string]any{"slack_webhook_url": slackSrv.URL, "email_to": []string{}, "on_new_issue": true, "on_regression": true}
	var saved map[string]any
	if code := ada.do("PUT", fmt.Sprintf("/api/ui/projects/%d/alerts", proj.ID), alerts, true, &saved); code != 200 {
		t.Fatalf("save alerts: %d %v", code, saved)
	}
	if saved["slack_webhook_set"] != true || strings.Contains(fmt.Sprint(saved), slackSrv.URL) {
		t.Errorf("the webhook URL must be stored but never sent back in full: %v", saved)
	}

	// Ada has the project open: the dashboard listens for refreshes.
	refreshes := ada.stream(fmt.Sprintf("/api/ui/projects/%d/live", proj.ID))

	// An SDK sends an error.
	send := func() {
		body := fmt.Sprintf(`{"sdk":{"name":"test"},"events":[{"event_id":%q,"type":"error","platform":"javascript","release":"web@1.0.0","user":{"id":"u1"},"exception":{"type":"TypeError","message":"x is undefined","stacktrace":{"frames":[{"function":"checkout","filename":"app.js","lineno":4,"in_app":true}]}}}]}`, uuid.NewString())
		req, _ := http.NewRequest("POST", fmt.Sprintf("%s/api/v1/projects/%d/events", srv.URL, proj.ID), strings.NewReader(body))
		req.Header.Set("Content-Type", "application/json")
		req.Header.Set("X-Juju-Key", proj.PublicKey)
		res, err := http.DefaultClient.Do(req)
		if err != nil || res.StatusCode != 202 {
			t.Fatalf("ingest: %v %v", err, res)
		}
		res.Body.Close()
	}
	type issueList struct {
		Issues []struct {
			ID         int64
			Status     string
			EventCount int64 `json:"event_count"`
			UserCount  int64 `json:"user_count"`
		}
	}
	issues := func(status string) issueList {
		var l issueList
		ada.do("GET", fmt.Sprintf("/api/ui/projects/%d/issues?status=%s", proj.ID, status), nil, false, &l)
		return l
	}

	send()
	waitFor(t, "the issue to appear", func() bool { return len(issues("unresolved").Issues) == 1 })
	waitFor(t, "the new-issue alert", func() bool { return len(hook.texts()) == 1 })
	if !strings.HasPrefix(hook.texts()[0], "New issue in shop-web") {
		t.Errorf("first alert = %q", hook.texts()[0])
	}
	issueID := issues("unresolved").Issues[0].ID
	expectRefresh(t, refreshes, "after the first event")

	// Someone fixes the bug and resolves the issue.
	if code := ada.do("POST", fmt.Sprintf("/api/ui/projects/%d/issues/status", proj.ID), map[string]any{"ids": []int64{issueID}, "status": "resolved"}, true, nil); code != 200 {
		t.Fatalf("resolve: %d", code)
	}
	if len(issues("unresolved").Issues) != 0 || len(issues("resolved").Issues) != 1 {
		t.Fatal("the issue should have moved to resolved")
	}
	// Teammates watching the project see a status change too.
	expectRefresh(t, refreshes, "after resolving")

	// The error happens again: a regression.
	send()
	waitFor(t, "the regression alert", func() bool { return len(hook.texts()) == 2 })
	if !strings.HasPrefix(hook.texts()[1], "Regression in shop-web") {
		t.Errorf("second alert = %q", hook.texts()[1])
	}
	l := issues("unresolved")
	if len(l.Issues) != 1 || l.Issues[0].EventCount != 2 || l.Issues[0].UserCount != 1 {
		t.Fatalf("after the regression: %+v", l)
	}

	var detail struct {
		Activity []struct{ Kind, User string }
	}
	ada.do("GET", fmt.Sprintf("/api/ui/projects/%d/issues/%d", proj.ID, issueID), nil, false, &detail)
	if len(detail.Activity) != 3 || detail.Activity[0].Kind != "regressed" || detail.Activity[1].User != "Ada" {
		t.Errorf("activity = %+v: want regressed, resolved by Ada, created", detail.Activity)
	}
	var deliveries struct {
		Deliveries []struct {
			Kind, Channel string
			OK            bool
		}
	}
	ada.do("GET", fmt.Sprintf("/api/ui/projects/%d/alerts", proj.ID), nil, false, &deliveries)
	if len(deliveries.Deliveries) != 2 || !deliveries.Deliveries[0].OK || deliveries.Deliveries[0].Kind != "regression" {
		t.Errorf("deliveries = %+v", deliveries.Deliveries)
	}

	// Sign-up is now closed, and someone else's account can't see the project.
	bob := newClient(t, srv.URL)
	if code := bob.do("POST", "/api/ui/auth/signup", map[string]string{"email": "bob@example.com", "password": "another long password", "organization": "evil"}, true, nil); code != 403 {
		t.Fatalf("second sign-up: %d, want 403 (closed)", code)
	}
	hash, _ := auth.HashPassword("another long password")
	if _, err := st.CreateUser(context.Background(), "bob@example.com", "Bob", hash, "bobs-org", store.JoinOrCreate); err != nil {
		t.Fatal(err)
	}
	if code := bob.do("POST", "/api/ui/auth/login", map[string]string{"email": "bob@example.com", "password": "another long password"}, true, nil); code != 200 {
		t.Fatalf("bob login: %d", code)
	}
	if code := bob.do("GET", fmt.Sprintf("/api/ui/projects/%d/issues", proj.ID), nil, false, nil); code != 404 {
		t.Fatalf("bob reading ada's project: %d, want 404", code)
	}
	if code := bob.do("POST", fmt.Sprintf("/api/ui/projects/%d/issues/status", proj.ID), map[string]any{"ids": []int64{issueID}, "status": "ignored"}, true, nil); code != 404 {
		t.Fatalf("bob changing ada's issue: %d, want 404", code)
	}
	// Not even "something changed" leaks to non-members.
	if code := bob.do("GET", fmt.Sprintf("/api/ui/projects/%d/live", proj.ID), nil, false, nil); code != 404 {
		t.Fatalf("bob watching ada's project: %d, want 404", code)
	}

	// With sign-up open, a stranger can't join Ada's organization by naming
	// it: that would make them an owner of every project in it.
	openCfg := config.Config{PublicURL: "http://juju.test", AllowSignup: true}
	openSrv := httptest.NewServer(server.Routes(server.Deps{Pool: pool, Store: st, Queue: q, Blobs: blobs, MaxUpload: 1 << 20, Auth: &auth.Service{Store: st}, Alerts: notifier, Live: hub, Config: openCfg, Log: log}))
	defer openSrv.Close()
	eve := newClient(t, openSrv.URL)
	var resp map[string]string
	if code := eve.do("POST", "/api/ui/auth/signup", map[string]string{"email": "eve@example.com", "password": "a long enough password", "organization": "acme"}, true, &resp); code != 409 || !strings.Contains(resp["error"], "already exists") {
		t.Fatalf("sign-up into Ada's org: %d %v, want 409", code, resp)
	}
	if code := eve.do("POST", "/api/ui/auth/signup", map[string]string{"email": "eve@example.com", "password": "a long enough password", "organization": "eve-co"}, true, nil); code != 201 {
		t.Fatalf("sign-up with her own org: %d", code)
	}
	if code := eve.do("GET", fmt.Sprintf("/api/ui/projects/%d/issues", proj.ID), nil, false, nil); code != 404 {
		t.Fatalf("eve reading ada's project: %d, want 404", code)
	}

	// Logging out ends the session.
	ada.do("POST", "/api/ui/auth/logout", nil, true, nil)
	if code := ada.do("GET", "/api/ui/me", nil, false, nil); code != 401 {
		t.Fatalf("after logout: %d, want 401", code)
	}
}

func TestInvites(t *testing.T) {
	log := slog.New(slog.NewTextHandler(io.Discard, nil))
	st := store.New(pool)
	ctx := context.Background()
	cfg := config.Config{PublicURL: "http://juju.test"} // sign-up closed
	srv := httptest.NewServer(server.Routes(server.Deps{Pool: pool, Store: st, Queue: queue.NewChan(10), MaxUpload: 1 << 20, Auth: &auth.Service{Store: st}, Alerts: &alert.Notifier{Settings: st, Log: log}, Config: cfg, Log: log}))
	defer srv.Close()

	user := func(email, org string) *client {
		hash, _ := auth.HashPassword("correct horse battery")
		if _, err := st.CreateUser(ctx, email, "", hash, org, store.JoinOrCreate); err != nil {
			t.Fatal(err)
		}
		c := newClient(t, srv.URL)
		if code := c.do("POST", "/api/ui/auth/login", map[string]string{"email": email, "password": "correct horse battery"}, true, nil); code != 200 {
			t.Fatalf("login %s: %d", email, code)
		}
		return c
	}
	orgOf := func(c *client) int64 {
		var me struct{ Organizations []struct{ ID int64 } }
		c.do("GET", "/api/ui/me", nil, false, &me)
		return me.Organizations[0].ID
	}
	ada := user("ada@team.test", "team-org")
	org := orgOf(ada)
	var proj struct{ ID int64 }
	ada.do("POST", fmt.Sprintf("/api/ui/orgs/%d/projects", org), map[string]string{"name": "team-app"}, true, &proj)
	projectPath := fmt.Sprintf("/api/ui/projects/%d/issues", proj.ID)

	invite := func(c *client) (string, int64) {
		t.Helper()
		var r struct {
			URL    string
			Invite struct{ ID int64 }
		}
		if code := c.do("POST", fmt.Sprintf("/api/ui/orgs/%d/invites", org), nil, true, &r); code != 201 {
			t.Fatalf("create invite: %d", code)
		}
		if !strings.HasPrefix(r.URL, "http://juju.test/invite/") {
			t.Fatalf("url = %q", r.URL)
		}
		return strings.TrimPrefix(r.URL, "http://juju.test/invite/"), r.Invite.ID
	}

	// Bob, logged out, opens the link and sees who invited him to what.
	token, _ := invite(ada)
	bob := newClient(t, srv.URL)
	var info map[string]any
	if code := bob.do("GET", "/api/ui/invites/"+token, nil, false, &info); code != 200 || info["organization"] != "team-org" || info["invited_by"] != "ada@team.test" {
		t.Fatalf("invite info: %d %v", code, info)
	}
	// This test server is private (sign-up off), but the invite lets him in, into
	// team-org (no organization of his own), as a member.
	signup := map[string]string{"email": "bob@team.test", "name": "Bob", "password": "another long password", "invite": token}
	if code := bob.do("POST", "/api/ui/auth/signup", signup, true, nil); code != 201 {
		t.Fatalf("sign-up with invite: %d", code)
	}
	var me struct {
		Organizations []struct {
			ID   int64
			Role string
		}
	}
	bob.do("GET", "/api/ui/me", nil, false, &me)
	if len(me.Organizations) != 1 || me.Organizations[0].ID != org || me.Organizations[0].Role != "member" {
		t.Fatalf("bob's orgs = %+v", me.Organizations)
	}
	if code := bob.do("GET", projectPath, nil, false, nil); code != 200 {
		t.Fatalf("bob reading the team's project: %d", code)
	}

	// The link worked once.
	carolNew := newClient(t, srv.URL)
	if code := carolNew.do("POST", "/api/ui/auth/signup", map[string]string{"email": "carol-new@team.test", "password": "another long password", "invite": token}, true, nil); code != 404 {
		t.Fatalf("reusing a used invite: %d, want 404", code)
	}
	if _, _, err := st.UserByEmail(ctx, "carol-new@team.test"); !errors.Is(err, store.ErrNotFound) {
		t.Fatalf("a failed invite sign-up must not create the account: %v", err)
	}
	if code := bob.do("GET", "/api/ui/invites/"+token, nil, false, nil); code != 404 {
		t.Fatalf("used invite still shows: %d", code)
	}

	// Members work on projects but don't manage people.
	if code := bob.do("POST", fmt.Sprintf("/api/ui/orgs/%d/invites", org), nil, true, nil); code != 403 {
		t.Fatalf("member creating an invite: %d, want 403", code)
	}
	var team struct {
		Members []struct {
			UserID int64 `json:"user_id"`
			Role   string
		}
		Invites []struct{ ID int64 }
	}
	bob.do("GET", fmt.Sprintf("/api/ui/orgs/%d/members", org), nil, false, &team)
	if len(team.Members) != 2 || team.Invites != nil {
		t.Fatalf("member's view of the team: %+v (members see people, not invite links)", team)
	}
	adaID, bobID := team.Members[0].UserID, team.Members[1].UserID
	if code := bob.do("DELETE", fmt.Sprintf("/api/ui/orgs/%d/members/%d", org, adaID), nil, true, nil); code != 403 {
		t.Fatalf("member removing the owner: %d, want 403", code)
	}

	// Carol already has an account: she logs in and accepts.
	carol := user("carol@team.test", "carols-own-org")
	token2, _ := invite(ada)
	var acc map[string]any
	if code := carol.do("POST", "/api/ui/invites/"+token2+"/accept", nil, true, &acc); code != 200 || acc["already_member"] != false {
		t.Fatalf("accept: %d %v", code, acc)
	}
	// Accepting again, with a fresh link, while already a member leaves that
	// link unused for whoever it was meant for.
	token3, id3 := invite(ada)
	carol.do("POST", "/api/ui/invites/"+token3+"/accept", nil, true, &acc)
	if acc["already_member"] != true {
		t.Fatalf("second accept: %v", acc)
	}
	if code := bob.do("GET", "/api/ui/invites/"+token3, nil, false, nil); code != 200 {
		t.Fatalf("the unused link should still work: %d", code)
	}

	// Revoking a link stops it.
	if code := ada.do("DELETE", fmt.Sprintf("/api/ui/orgs/%d/invites/%d", org, id3), nil, true, nil); code != 204 {
		t.Fatalf("revoke: %d", code)
	}
	if code := bob.do("GET", "/api/ui/invites/"+token3, nil, false, nil); code != 404 {
		t.Fatalf("revoked invite still works: %d", code)
	}

	// An expired link doesn't work.
	if _, err := st.CreateInvite(ctx, org, adaID, auth.HashToken("expired-token"), time.Now().Add(-time.Minute)); err != nil {
		t.Fatal(err)
	}
	if code := bob.do("GET", "/api/ui/invites/expired-token", nil, false, nil); code != 404 {
		t.Fatalf("expired invite: %d", code)
	}

	// Two people opening one link at the same moment: only one gets in.
	race, _ := invite(ada)
	var wins atomic.Int32
	var wg sync.WaitGroup
	for i := range 2 {
		hash, _ := auth.HashPassword("correct horse battery")
		u, _ := st.CreateUser(ctx, fmt.Sprintf("racer%d@team.test", i), "", hash, fmt.Sprintf("racer-org-%d", i), store.NewOrganization)
		wg.Go(func() {
			if _, _, err := st.AcceptInvite(ctx, auth.HashToken(race), u.ID); err == nil {
				wins.Add(1)
			}
		})
	}
	wg.Wait()
	if wins.Load() != 1 {
		t.Fatalf("%d people joined with one link, want 1", wins.Load())
	}

	// Outsiders can't see the team, or revoke this org's invites through
	// their own organization's URL.
	eve := user("eve@elsewhere.test", "eve-org")
	if code := eve.do("GET", fmt.Sprintf("/api/ui/orgs/%d/members", org), nil, false, nil); code != 404 {
		t.Fatalf("outsider reading members: %d, want 404", code)
	}
	_, id5 := invite(ada)
	if code := eve.do("DELETE", fmt.Sprintf("/api/ui/orgs/%d/invites/%d", orgOf(eve), id5), nil, true, nil); code != 404 {
		t.Fatalf("revoking another org's invite: %d, want 404", code)
	}

	// Removing Bob ends his access at once; the last owner can't be removed.
	if code := ada.do("DELETE", fmt.Sprintf("/api/ui/orgs/%d/members/%d", org, bobID), nil, true, nil); code != 204 {
		t.Fatalf("remove bob: %d", code)
	}
	if code := bob.do("GET", projectPath, nil, false, nil); code != 404 {
		t.Fatalf("removed member reading the project: %d, want 404", code)
	}
	if code := ada.do("DELETE", fmt.Sprintf("/api/ui/orgs/%d/members/%d", org, adaID), nil, true, nil); code != 409 {
		t.Fatalf("removing the last owner: %d, want 409", code)
	}
}
