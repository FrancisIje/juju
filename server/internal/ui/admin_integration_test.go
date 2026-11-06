//go:build integration

package ui_test

import (
	"bytes"
	"context"
	"encoding/json"
	"fmt"
	"io"
	"log/slog"
	"net/http"
	"net/http/httptest"
	"strings"
	"testing"
	"time"

	"github.com/google/uuid"

	"juju/internal/alert"
	"juju/internal/auth"
	"juju/internal/config"
	"juju/internal/ingest"
	"juju/internal/server"
	"juju/internal/store"
	"juju/internal/worker"
)

// raw is a GET that returns the status and the whole body, for comparing
// answers byte for byte and searching them for things that must not be there.
func (c *client) raw(path string) (int, string) {
	c.t.Helper()
	res, err := c.http.Get(c.base + path)
	if err != nil {
		c.t.Fatal(err)
	}
	defer res.Body.Close()
	b, _ := io.ReadAll(res.Body)
	return res.StatusCode, string(b)
}

// The super admin view crosses every organization's boundary, so what matters
// most is who can reach it and what it gives away. This runs the real path:
// events through ingest, the queue and the worker, with two organizations.
func TestSuperAdmin(t *testing.T) {
	ctx := context.Background()
	log := slog.New(slog.NewTextHandler(io.Discard, nil))
	st := store.New(pool)
	q := newQueue(t)
	go worker.Run(ctx, q, worker.Deps{Store: st, Log: log}, 1)
	t.Cleanup(q.StopDeliveries)

	// The default in production, so accepted events reach project_usage.
	limits := &ingest.Limits{Usage: st, Now: time.Now, Log: log}
	cfg := config.Config{PublicURL: "http://juju.test", AllowSignup: true}
	srv := httptest.NewServer(server.Routes(server.Deps{Pool: pool, Store: st, Queue: q, MaxUpload: 1 << 20, Auth: &auth.Service{Store: st},
		Alerts: &alert.Notifier{Settings: st, AllowHTTPWebhooks: true, Log: log}, Limits: limits, Config: cfg, Log: log}))
	defer srv.Close()

	id := uuid.NewString()[:8] // this test's own names, so it can run again
	signup := func(name string, extra map[string]any) (*client, string, string) {
		t.Helper()
		c := newClient(t, srv.URL)
		email, org := name+"-"+id+"@example.com", name+"-org-"+id
		body := map[string]any{"email": email, "password": "correct horse battery", "organization": org}
		for k, v := range extra {
			body[k] = v
		}
		if code := c.do("POST", "/api/ui/auth/signup", body, true, nil); code != 201 {
			t.Fatalf("sign-up %s: %d", name, code)
		}
		return c, email, org
	}
	boss, bossEmail, _ := signup("boss", nil)
	ada, adaEmail, adaOrg := signup("ada", nil)

	var me struct {
		Organizations []struct{ ID int64 } `json:"organizations"`
	}
	ada.do("GET", "/api/ui/me", nil, false, &me)
	orgID := me.Organizations[0].ID
	var proj struct {
		ID        int64  `json:"id"`
		PublicKey string `json:"public_key"`
		SecretKey string `json:"secret_key"`
	}
	if code := ada.do("POST", fmt.Sprintf("/api/ui/orgs/%d/projects", orgID), map[string]string{"name": "shop-" + id}, true, &proj); code != 201 {
		t.Fatalf("create project: %d", code)
	}
	if code := ada.do("PUT", fmt.Sprintf("/api/ui/projects/%d/alerts", proj.ID), map[string]any{"slack_webhook_url": "http://hooks.test/SECRET-WEBHOOK", "email_to": []string{}, "on_new_issue": true, "on_regression": true}, true, nil); code != 200 {
		t.Fatalf("save alerts: %d", code)
	}
	var quiet struct {
		ID int64 `json:"id"`
	}
	if code := ada.do("POST", fmt.Sprintf("/api/ui/orgs/%d/projects", orgID), map[string]string{"name": "unused-" + id}, true, &quiet); code != 201 {
		t.Fatalf("create project: %d", code)
	}

	// An error whose text belongs to the customer, and could hold anything.
	const privateText = "PRIVATE-CUSTOMER-TEXT"
	body, _ := json.Marshal(map[string]any{"sdk": map[string]string{"name": "juju.test", "version": "0"}, "events": []map[string]any{{
		"event_id": uuid.NewString(), "type": "error", "platform": "javascript", "timestamp": time.Now().UTC().Format(time.RFC3339), "environment": "prod",
		"exception": map[string]any{"type": "TypeError", "message": privateText, "stacktrace": map[string]any{"frames": []map[string]any{{"function": privateText, "filename": "app.js", "lineno": 1, "in_app": true}}}},
	}}})
	req, _ := http.NewRequest("POST", fmt.Sprintf("%s/api/v1/projects/%d/events", srv.URL, proj.ID), bytes.NewReader(body))
	req.Header.Set("Content-Type", "application/json")
	req.Header.Set("X-Juju-Key", proj.PublicKey)
	res, err := http.DefaultClient.Do(req)
	if err != nil {
		t.Fatal(err)
	}
	res.Body.Close()
	if res.StatusCode != 202 {
		t.Fatalf("send event: %d", res.StatusCode)
	}
	waitFor(t, "the event to be stored", func() bool {
		var n int
		_ = pool.QueryRow(ctx, "SELECT count(*) FROM events WHERE project_id = $1", proj.ID).Scan(&n)
		return n == 1
	})
	if err := limits.Flush(ctx); err != nil {
		t.Fatal(err)
	}

	orgPath := fmt.Sprintf("/api/ui/admin/orgs/%d", orgID)
	const overviewPath = "/api/ui/admin/overview"

	t.Run("logged out: 401", func(t *testing.T) {
		anon := newClient(t, srv.URL)
		for _, p := range []string{overviewPath, orgPath} {
			if code, _ := anon.raw(p); code != 401 {
				t.Errorf("%s: %d", p, code)
			}
		}
	})

	t.Run("an ordinary account can't tell the routes exist", func(t *testing.T) {
		_, unknown := ada.raw("/api/ui/admin/no-such-thing")
		for _, p := range []string{overviewPath, orgPath} {
			code, got := ada.raw(p)
			if code != 404 || got != unknown {
				t.Errorf("%s: %d %q, want the same as an unknown route: %q", p, code, got, unknown)
			}
		}
	})

	t.Run("sign-up can't ask to be a super admin", func(t *testing.T) {
		c, _, _ := signup("sneaky", map[string]any{"is_superadmin": true, "IsSuperAdmin": true})
		if code, _ := c.raw(overviewPath); code != 404 {
			t.Errorf("overview: %d", code)
		}
		var m struct {
			User map[string]any `json:"user"`
		}
		c.do("GET", "/api/ui/me", nil, false, &m)
		if _, ok := m.User["is_superadmin"]; ok {
			t.Errorf("me = %v", m.User)
		}
	})

	if err := st.SetSuperAdmin(ctx, bossEmail, true); err != nil {
		t.Fatal(err)
	}

	t.Run("a grant works on the session already open", func(t *testing.T) {
		var m struct {
			User struct {
				IsSuperAdmin bool `json:"is_superadmin"`
			} `json:"user"`
		}
		boss.do("GET", "/api/ui/me", nil, false, &m)
		if !m.User.IsSuperAdmin {
			t.Error("/me doesn't say super admin")
		}
	})

	var ov store.AdminOverview
	code, overviewBody := boss.raw(overviewPath)
	if code != 200 {
		t.Fatalf("overview as super admin: %d %s", code, overviewBody)
	}
	if err := json.Unmarshal([]byte(overviewBody), &ov); err != nil {
		t.Fatal(err)
	}

	t.Run("numbers match what happened", func(t *testing.T) {
		found := map[int64]store.AdminProject{}
		for _, p := range ov.Projects {
			found[p.ID] = p
		}
		p := found[proj.ID]
		if p.Org != adaOrg || p.Status != store.StatusActive || p.EventsThisMonth != 1 || p.Errors24h != 1 || p.Unresolved != 1 {
			t.Errorf("active project = %+v", p)
		}
		if p.LastEventAt == nil || time.Since(*p.LastEventAt) > time.Minute {
			t.Errorf("last event = %v", p.LastEventAt)
		}
		if u := found[quiet.ID]; u.Status != store.StatusNoEvents || u.EventsThisMonth != 0 || u.LastEventAt != nil {
			t.Errorf("unused project = %+v", u)
		}
		var o *store.AdminOrg
		for i := range ov.Organizations {
			if ov.Organizations[i].ID == orgID {
				o = &ov.Organizations[i]
			}
		}
		if o == nil || o.OwnerEmail != adaEmail || o.Members != 1 || o.Projects != 2 || o.EventsThisMonth != 1 || o.Status != store.StatusActive {
			t.Errorf("org = %+v", o)
		}
		if ov.Totals.Users < 3 || ov.Totals.Organizations < 3 || ov.Totals.ByStatus[store.StatusNoEvents] < 1 {
			t.Errorf("totals = %+v", ov.Totals)
		}
	})

	// Analytics and session events never create an issue, so for these apps
	// the usage row is the only sign of life.
	t.Run("a project with usage but no issues still shows when it last sent", func(t *testing.T) {
		status := func() store.AdminProject {
			t.Helper()
			var got store.AdminOverview
			boss.do("GET", overviewPath, nil, false, &got)
			for _, p := range got.Projects {
				if p.ID == quiet.ID {
					return p
				}
			}
			t.Fatal("project is missing")
			return store.AdminProject{}
		}
		month := store.MonthStart(time.Now())
		if err := st.AddUsage(ctx, month, map[int64]int64{quiet.ID: 3}); err != nil {
			t.Fatal(err)
		}
		if p := status(); p.Status != store.StatusActive || p.EventsThisMonth != 3 || p.LastEventAt == nil {
			t.Fatalf("after its first events: %+v", p)
		}
		if _, err := pool.Exec(ctx, `UPDATE project_usage SET last_accepted_at = now() - interval '30 days' WHERE project_id = $1`, quiet.ID); err != nil {
			t.Fatal(err)
		}
		if p := status(); p.Status != store.StatusQuiet {
			t.Fatalf("30 days later: %+v", p)
		}
		// The row exists now, so this takes the update path.
		if err := st.AddUsage(ctx, month, map[int64]int64{quiet.ID: 1}); err != nil {
			t.Fatal(err)
		}
		if p := status(); p.Status != store.StatusActive || p.EventsThisMonth != 4 {
			t.Fatalf("after sending again: %+v", p)
		}
	})

	t.Run("an SDK with its clock in the future can't claim to be from the future", func(t *testing.T) {
		if _, err := pool.Exec(ctx, `UPDATE issues SET last_seen = now() + interval '1 year' WHERE project_id = $1`, proj.ID); err != nil {
			t.Fatal(err)
		}
		var got store.AdminOverview
		boss.do("GET", overviewPath, nil, false, &got)
		for _, p := range got.Projects {
			if p.ID == proj.ID && (p.LastEventAt == nil || p.LastEventAt.After(time.Now())) {
				t.Errorf("last event = %v", p.LastEventAt)
			}
		}
	})

	t.Run("no customer data or keys in any answer", func(t *testing.T) {
		code, orgBody := boss.raw(orgPath)
		if code != 200 {
			t.Fatalf("org page: %d", code)
		}
		if !strings.Contains(orgBody, adaEmail) || !strings.Contains(orgBody, "shop-"+id) {
			t.Fatalf("org page is missing its members or projects: %s", orgBody)
		}
		for _, secret := range []string{privateText, "SECRET-WEBHOOK", proj.PublicKey, proj.SecretKey, "password", "pk_", "sk_"} {
			if strings.Contains(overviewBody, secret) || strings.Contains(orgBody, secret) {
				t.Errorf("an answer contains %q", secret)
			}
		}
	})

	t.Run("super admin doesn't open other organizations' projects", func(t *testing.T) {
		for _, p := range []string{
			fmt.Sprintf("/api/ui/projects/%d", proj.ID),
			fmt.Sprintf("/api/ui/projects/%d/issues", proj.ID),
			fmt.Sprintf("/api/ui/projects/%d/alerts", proj.ID),
			fmt.Sprintf("/api/ui/orgs/%d/members", orgID),
		} {
			if code, _ := boss.raw(p); code != 404 {
				t.Errorf("%s: %d, want 404", p, code)
			}
		}
	})

	t.Run("bad organization ids", func(t *testing.T) {
		for _, p := range []string{"/api/ui/admin/orgs/999999999", "/api/ui/admin/orgs/abc"} {
			if code, _ := boss.raw(p); code != 404 {
				t.Errorf("%s: %d", p, code)
			}
		}
	})

	t.Run("read-only: nothing here accepts a change", func(t *testing.T) {
		for _, method := range []string{"POST", "PUT", "DELETE"} {
			if code := boss.do(method, overviewPath, map[string]any{}, true, nil); code != 404 {
				t.Errorf("%s overview: %d", method, code)
			}
		}
	})

	t.Run("a revoke takes effect on the very next request", func(t *testing.T) {
		if err := st.SetSuperAdmin(ctx, bossEmail, false); err != nil {
			t.Fatal(err)
		}
		if code, _ := boss.raw(overviewPath); code != 404 {
			t.Errorf("after revoke: %d", code)
		}
	})

	t.Run("granting an unknown email fails instead of waiting for someone to register it", func(t *testing.T) {
		if err := st.SetSuperAdmin(ctx, "nobody-"+id+"@example.com", true); err != store.ErrNotFound {
			t.Errorf("err = %v", err)
		}
		if _, err := st.SuperAdmins(ctx); err != nil {
			t.Error(err)
		}
	})
}
