//go:build integration

package ui_test

import (
	"context"
	"encoding/json"
	"fmt"
	"io"
	"log/slog"
	"net/http"
	"net/http/httptest"
	"strings"
	"sync"
	"testing"

	"github.com/google/uuid"

	"juju/internal/alert"
	"juju/internal/auth"
	"juju/internal/blob"
	"juju/internal/config"
	"juju/internal/live"
	"juju/internal/server"
	"juju/internal/store"
	"juju/internal/worker"
)

// receiver is a program listening for juju's webhook, like the job that
// starts an agent. It checks every signature, as a real one must.
type receiver struct {
	mu     sync.Mutex
	secret string
	got    []alert.WebhookPayload
	bad    int
}

func (rc *receiver) ServeHTTP(w http.ResponseWriter, r *http.Request) {
	body, _ := io.ReadAll(r.Body)
	rc.mu.Lock()
	defer rc.mu.Unlock()
	if alert.Sign(rc.secret, r.Header.Get("X-Juju-Timestamp"), body) != r.Header.Get("X-Juju-Signature") {
		rc.bad++
		w.WriteHeader(http.StatusUnauthorized)
		return
	}
	var p alert.WebhookPayload
	_ = json.Unmarshal(body, &p)
	rc.got = append(rc.got, p)
}

func (rc *receiver) payloads() []alert.WebhookPayload {
	rc.mu.Lock()
	defer rc.mu.Unlock()
	return append([]alert.WebhookPayload(nil), rc.got...)
}

// The whole bring-your-own-agent loop over HTTP, with the real queue,
// worker and notifier: CI records the release's commit, a crash arrives,
// the webhook tells the agent's job (signed, with the commit), the agent
// reads the issue over MCP, links its PR and resolves it, and the open
// dashboard refreshes. When the crash comes back, the webhook says so.
func TestAgentFixLoopEndToEnd(t *testing.T) {
	log := slog.New(slog.NewTextHandler(io.Discard, nil))
	ctx := context.Background()
	st := store.New(pool)
	q := newQueue(t)
	blobs, _ := blob.New(t.TempDir())
	notifier := &alert.Notifier{Settings: st, PublicURL: "http://juju.test", AllowHTTPWebhooks: true, Log: log}
	notifier.Start(1)
	hub := live.NewHub()
	live.MinGap = 0
	go worker.Run(ctx, q, worker.Deps{Store: st, Alerter: notifier, Live: hub, Log: log}, 1)
	t.Cleanup(func() { q.StopDeliveries(); notifier.Stop() })
	srv := httptest.NewServer(server.Routes(server.Deps{Pool: pool, Store: st, Queue: q, Blobs: blobs, MaxUpload: 1 << 20, Auth: &auth.Service{Store: st}, Alerts: notifier, Live: hub, Config: config.Config{PublicURL: "http://juju.test"}, Log: log}))
	defer srv.Close()
	defer hub.Close()

	// A person sets up the project and the webhook in the dashboard.
	hash, _ := auth.HashPassword("correct horse battery")
	if _, err := st.CreateUser(ctx, "grace@agents.test", "Grace", hash, "agents-org", store.JoinOrCreate); err != nil {
		t.Fatal(err)
	}
	grace := newClient(t, srv.URL)
	if code := grace.do("POST", "/api/ui/auth/login", map[string]string{"email": "grace@agents.test", "password": "correct horse battery"}, true, nil); code != 200 {
		t.Fatalf("login: %d", code)
	}
	var me struct{ Organizations []struct{ ID int64 } }
	grace.do("GET", "/api/ui/me", nil, false, &me)
	var proj struct {
		ID        int64  `json:"id"`
		PublicKey string `json:"public_key"`
		SecretKey string `json:"secret_key"`
	}
	grace.do("POST", fmt.Sprintf("/api/ui/orgs/%d/projects", me.Organizations[0].ID), map[string]string{"name": "shop"}, true, &proj)

	rc := &receiver{}
	hook := httptest.NewServer(rc)
	defer hook.Close()
	alertsPath := fmt.Sprintf("/api/ui/projects/%d/alerts", proj.ID)
	var saved struct {
		WebhookURL    string `json:"webhook_url"`
		WebhookSecret string `json:"webhook_secret"`
	}
	settings := map[string]any{"webhook_url": hook.URL, "email_to": []string{}, "on_new_issue": true, "on_regression": true}
	if code := grace.do("PUT", alertsPath, settings, true, &saved); code != 200 || saved.WebhookURL != hook.URL || !strings.HasPrefix(saved.WebhookSecret, "whsec_") {
		t.Fatalf("save webhook: %d %+v", code, saved)
	}
	rc.secret = saved.WebhookSecret
	// The signing secret is shown once: not on a reload, not on a later save.
	var again map[string]any
	grace.do("GET", alertsPath, nil, false, &again)
	if _, ok := again["webhook_secret"]; ok {
		t.Errorf("GET returns the webhook secret: %v", again)
	}
	grace.do("PUT", alertsPath, map[string]any{"email_to": []string{}, "on_new_issue": true, "on_regression": true}, true, &again)
	if _, ok := again["webhook_secret"]; ok || again["webhook_url"] != hook.URL {
		t.Errorf("a save that doesn't touch the webhook changed it or showed the secret: %v", again)
	}

	// Secret-key calls, as CI and the agent make them.
	call := func(method, path, body string, out any) int {
		t.Helper()
		req, _ := http.NewRequest(method, srv.URL+path, strings.NewReader(body))
		req.Header.Set("Authorization", "Bearer "+proj.SecretKey)
		req.Header.Set("Content-Type", "application/json")
		res, err := http.DefaultClient.Do(req)
		if err != nil {
			t.Fatal(err)
		}
		defer res.Body.Close()
		if out != nil {
			_ = json.NewDecoder(res.Body).Decode(out)
		}
		return res.StatusCode
	}
	tool := func(name, args string) string {
		t.Helper()
		var out struct {
			Result struct {
				Content []struct{ Text string }
				IsError bool
			}
		}
		code := call("POST", fmt.Sprintf("/api/v1/projects/%d/mcp", proj.ID), `{"jsonrpc":"2.0","id":1,"method":"tools/call","params":{"name":"`+name+`","arguments":`+args+`}}`, &out)
		if code != 200 || out.Result.IsError || len(out.Result.Content) != 1 {
			t.Fatalf("%s: %d %+v", name, code, out)
		}
		return out.Result.Content[0].Text
	}

	// CI built web@2.0.0 from commit 9c0ffee and recorded it.
	if code := call("POST", fmt.Sprintf("/api/v1/projects/%d/releases", proj.ID), `{"name":"web@2.0.0","commit":"9c0ffee1","repository":"https://github.com/acme/shop"}`, nil); code != 200 {
		t.Fatalf("record release: %d", code)
	}

	refreshes := grace.stream(fmt.Sprintf("/api/ui/projects/%d/live", proj.ID))
	send := func() {
		body := fmt.Sprintf(`{"sdk":{"name":"test"},"events":[{"event_id":%q,"type":"error","platform":"javascript","release":"web@2.0.0","environment":"production","exception":{"type":"TypeError","message":"cart.items is undefined","stacktrace":{"frames":[{"function":"renderCart","filename":"src/cart.js","lineno":42,"colno":17,"in_app":true}]}},"breadcrumbs":[{"type":"http","message":"GET /api/cart 500"}]}]}`, uuid.NewString())
		req, _ := http.NewRequest("POST", fmt.Sprintf("%s/api/v1/projects/%d/events", srv.URL, proj.ID), strings.NewReader(body))
		req.Header.Set("Content-Type", "application/json")
		req.Header.Set("X-Juju-Key", proj.PublicKey)
		res, err := http.DefaultClient.Do(req)
		if err != nil || res.StatusCode != 202 {
			t.Fatalf("ingest: %v %v", err, res)
		}
		res.Body.Close()
	}
	send()
	waitFor(t, "the new-issue webhook", func() bool { return len(rc.payloads()) == 1 })
	expectRefresh(t, refreshes, "after the crash")
	p := rc.payloads()[0]
	if p.Kind != alert.NewIssue || p.Commit != "9c0ffee1" || p.Repository != "https://github.com/acme/shop" || p.Issue.ID == 0 {
		t.Fatalf("webhook payload = %+v", p)
	}
	issueID := p.Issue.ID

	// The agent's job got the issue id: it reads the issue over MCP.
	text := tool("get_issue", fmt.Sprintf(`{"issue_id":%d}`, issueID))
	for _, want := range []string{"TypeError: cart.items is undefined", "web@2.0.0: commit 9c0ffee1 in https://github.com/acme/shop", "* renderCart  src/cart.js:42:17", "\n- [http] GET /api/cart 500"} {
		if !strings.Contains(text, want) {
			t.Errorf("get_issue is missing %q:\n%s", want, text)
		}
	}
	// It opens a PR and links it, and once merged, resolves the issue.
	tool("link_issue", fmt.Sprintf(`{"issue_id":%d,"url":"https://github.com/acme/shop/pull/12","title":"Handle an empty cart"}`, issueID))
	expectRefresh(t, refreshes, "after linking the PR")
	if got := tool("update_issue", fmt.Sprintf(`{"issue_id":%d,"status":"resolved"}`, issueID)); !strings.Contains(got, "now resolved") {
		t.Fatalf("update_issue: %s", got)
	}
	expectRefresh(t, refreshes, "after the agent resolved it")

	// The dashboard shows the PR, the commit and who resolved it (no one:
	// a key did).
	var detail store.IssueDetail
	grace.do("GET", fmt.Sprintf("/api/ui/projects/%d/issues/%d", proj.ID, issueID), nil, false, &detail)
	if detail.Status != "resolved" || len(detail.Links) != 1 || detail.Links[0].Title != "Handle an empty cart" || len(detail.Commits) != 1 || detail.Commits[0].Commit != "9c0ffee1" {
		t.Errorf("dashboard issue = %+v", detail)
	}
	if len(detail.Activity) == 0 || detail.Activity[0].Kind != "resolved" || detail.Activity[0].User != "" {
		t.Errorf("activity = %+v", detail.Activity)
	}

	// The fix didn't work: the crash comes back, and the webhook says so.
	send()
	waitFor(t, "the regression webhook", func() bool { return len(rc.payloads()) == 2 })
	if p := rc.payloads()[1]; p.Kind != alert.Regression || p.Issue.ID != issueID {
		t.Errorf("regression payload = %+v", p)
	}
	if rc.bad != 0 {
		t.Errorf("%d webhook calls had a bad signature", rc.bad)
	}
}
