//go:build integration

package ui_test

import (
	"context"
	"fmt"
	"io"
	"log/slog"
	"math/rand/v2"
	"net/http"
	"net/http/httptest"
	"net/url"
	"strings"
	"testing"
	"time"

	"github.com/google/uuid"

	"juju/internal/alert"
	"juju/internal/analytics"
	"juju/internal/auth"
	"juju/internal/config"
	"juju/internal/queue"
	"juju/internal/server"
	"juju/internal/store"
	"juju/internal/worker"
)

// The whole path, the way an app uses it: SDK envelopes over HTTP → queue →
// worker → ClickHouse writer → the dashboard's analytics endpoints.
func TestAnalyticsEndToEnd(t *testing.T) {
	log := slog.New(slog.NewTextHandler(io.Discard, nil))
	st := store.New(pool)
	ctx := context.Background()
	ch := &analytics.Client{URL: chURL, Database: fmt.Sprintf("ui_test_%d", rand.Int64N(1<<40))}
	if err := ch.Migrate(ctx); err != nil {
		t.Fatal(err)
	}
	t.Cleanup(func() { _ = ch.Exec(context.Background(), "DROP DATABASE IF EXISTS "+ch.Database) })
	q, err := queue.OpenJetStream(ctx, queue.JetStreamConfig{Dir: t.TempDir(), Analytics: true, Log: log})
	if err != nil {
		t.Fatal(err)
	}
	t.Cleanup(q.Close)
	writer := &analytics.Writer{Client: ch, Log: log, FlushEvery: 50 * time.Millisecond}
	writer.Start(q.Analytics())
	go worker.Run(context.Background(), q, worker.Deps{Store: st, Analytics: q, Log: log}, 2)
	t.Cleanup(func() { q.StopDeliveries(); q.StopAnalytics(); writer.Wait() })

	deps := server.Deps{Pool: pool, Store: st, Queue: q, MaxUpload: 1 << 20, Auth: &auth.Service{Store: st}, Alerts: &alert.Notifier{Settings: st, Log: log}, Analytics: ch, Config: config.Config{PublicURL: "http://juju.test"}, Log: log}
	srv := httptest.NewServer(server.Routes(deps))
	defer srv.Close()

	hash, _ := auth.HashPassword("correct horse battery")
	if _, err := st.CreateUser(ctx, "owner@analytics.test", "", hash, "analytics-org", store.JoinOrCreate); err != nil {
		t.Fatal(err)
	}
	owner := newClient(t, srv.URL)
	owner.do("POST", "/api/ui/auth/login", map[string]string{"email": "owner@analytics.test", "password": "correct horse battery"}, true, nil)
	var me struct{ Organizations []struct{ ID int64 } }
	owner.do("GET", "/api/ui/me", nil, false, &me)
	var proj struct {
		ID        int64  `json:"id"`
		PublicKey string `json:"public_key"`
	}
	owner.do("POST", fmt.Sprintf("/api/ui/orgs/%d/projects", me.Organizations[0].ID), map[string]string{"name": "app"}, true, &proj)

	// Three apps send events, as their SDKs would.
	send := func(sdk string, events ...string) {
		t.Helper()
		body := fmt.Sprintf(`{"sdk":{"name":%q,"version":"0.1.0"},"events":[%s]}`, sdk, strings.Join(events, ","))
		req, _ := http.NewRequest("POST", fmt.Sprintf("%s/api/v1/projects/%d/events", srv.URL, proj.ID), strings.NewReader(body))
		req.Header.Set("Content-Type", "application/json")
		req.Header.Set("X-Juju-Key", proj.PublicKey)
		res, err := http.DefaultClient.Do(req)
		if err != nil || res.StatusCode != 202 {
			t.Fatalf("ingest from %s: %v %v", sdk, err, res)
		}
		res.Body.Close()
	}
	ts := time.Now().UTC().Add(-time.Hour).Format(time.RFC3339)
	capture := func(name, who string, props string) string {
		return fmt.Sprintf(`{"event_id":%q,"type":"event","timestamp":%q,"platform":"javascript","name":%q,"distinct_id":%q,"properties":%s}`, uuid.NewString(), ts, name, who, props)
	}
	send("juju.web",
		capture("checkout_started", "anon-w1", `{"plan":"pro"}`),
		capture("checkout_started", "anon-w2", `{"plan":"free"}`),
		capture("checkout_completed", "anon-w1", `{}`),
		fmt.Sprintf(`{"event_id":%q,"type":"identify","timestamp":%q,"platform":"javascript","distinct_id":"user_7","anon_id":"anon-w1"}`, uuid.NewString(), ts),
	)
	send("juju.react-native", capture("checkout_started", "anon-r1", `{"plan":"pro"}`))
	send("juju.flutter", capture("checkout_started", "anon-f1", `{"plan":"pro"}`), capture("checkout_started", "anon-f1", `{"plan":"pro"}`))
	// An error in the same envelope format still becomes an issue, untouched.
	send("juju.web", fmt.Sprintf(`{"event_id":%q,"type":"error","platform":"javascript","exception":{"type":"TypeError","message":"x"}}`, uuid.NewString()))

	type trend struct {
		Series []struct {
			Label string
			Total int64
		}
	}
	byLib := func() map[string]int64 {
		var tr trend
		owner.do("GET", fmt.Sprintf("/api/ui/projects/%d/analytics/trend?event=checkout_started&days=7&breakdown=lib", proj.ID), nil, false, &tr)
		out := map[string]int64{}
		for _, s := range tr.Series {
			out[s.Label] = s.Total
		}
		return out
	}
	waitFor(t, "analytics events in ClickHouse", func() bool { return byLib()["juju.flutter"] == 2 })
	if got := fmt.Sprint(byLib()); got != "map[juju.flutter:2 juju.react-native:1 juju.web:2]" {
		t.Errorf("checkout_started by SDK = %s", got)
	}

	var summary struct {
		Enabled bool
		Events  []struct {
			Name  string
			Count int64
		}
	}
	owner.do("GET", fmt.Sprintf("/api/ui/projects/%d/analytics", proj.ID), nil, false, &summary)
	if !summary.Enabled || len(summary.Events) != 3 || summary.Events[0].Name != "checkout_started" || summary.Events[0].Count != 5 {
		t.Errorf("summary = %+v", summary)
	}

	var funnel struct {
		Steps []struct {
			Event  string
			People int64
		}
	}
	owner.do("GET", fmt.Sprintf("/api/ui/projects/%d/analytics/funnel?step=checkout_started&step=checkout_completed", proj.ID), nil, false, &funnel)
	if len(funnel.Steps) != 2 || funnel.Steps[0].People != 4 || funnel.Steps[1].People != 1 {
		t.Errorf("funnel = %+v", funnel)
	}
	// A comma in an event name is part of the name, not a second step.
	send("juju.web", capture("plan,changed", "anon-w1", `{}`))
	waitFor(t, "the event with a comma", func() bool {
		var f struct {
			Steps []struct {
				Event  string
				People int64
			}
		}
		owner.do("GET", fmt.Sprintf("/api/ui/projects/%d/analytics/funnel?step=%s&step=checkout_started", proj.ID, url.QueryEscape("plan,changed")), nil, false, &f)
		return len(f.Steps) == 2 && f.Steps[0].Event == "plan,changed" && f.Steps[0].People == 1
	})
	var props struct{ Properties []string }
	owner.do("GET", fmt.Sprintf("/api/ui/projects/%d/analytics/properties?event=checkout_started", proj.ID), nil, false, &props)
	if fmt.Sprint(props.Properties) != "[plan]" {
		t.Errorf("properties = %v", props.Properties)
	}
	var issues struct{ Issues []struct{ Title string } }
	owner.do("GET", fmt.Sprintf("/api/ui/projects/%d/issues", proj.ID), nil, false, &issues)
	if len(issues.Issues) != 1 {
		t.Errorf("issues = %+v: only the error is an issue, never an analytics event", issues.Issues)
	}

	// Mistakes get a 400 that says why.
	var bad map[string]string
	if code := owner.do("GET", fmt.Sprintf("/api/ui/projects/%d/analytics/trend?event=x&breakdown=nope", proj.ID), nil, false, &bad); code != 400 || !strings.Contains(bad["error"], "break down") {
		t.Errorf("bad breakdown: %d %v", code, bad)
	}
	if code := owner.do("GET", fmt.Sprintf("/api/ui/projects/%d/analytics/trend?event=x&interval=hour&days=90", proj.ID), nil, false, nil); code != 400 {
		t.Errorf("hourly over 90 days: %d, want 400", code)
	}

	// Someone outside the organization sees nothing, not even that it exists.
	if _, err := st.CreateUser(ctx, "outsider@analytics.test", "", hash, "elsewhere-org", store.JoinOrCreate); err != nil {
		t.Fatal(err)
	}
	outsider := newClient(t, srv.URL)
	outsider.do("POST", "/api/ui/auth/login", map[string]string{"email": "outsider@analytics.test", "password": "correct horse battery"}, true, nil)
	for _, path := range []string{"analytics", "analytics/trend?event=checkout_started", "analytics/funnel?step=a&step=b", "analytics/retention?start=a&return=b", "analytics/properties?event=a"} {
		if code := outsider.do("GET", fmt.Sprintf("/api/ui/projects/%d/%s", proj.ID, path), nil, false, nil); code != 404 {
			t.Errorf("outsider GET %s: %d, want 404", path, code)
		}
	}

	// A server without ClickHouse says analytics is off, and how to turn it on.
	deps.Analytics = nil
	off := httptest.NewServer(server.Routes(deps))
	defer off.Close()
	offOwner := newClient(t, off.URL)
	offOwner.do("POST", "/api/ui/auth/login", map[string]string{"email": "owner@analytics.test", "password": "correct horse battery"}, true, nil)
	var offSummary map[string]any
	offOwner.do("GET", fmt.Sprintf("/api/ui/projects/%d/analytics", proj.ID), nil, false, &offSummary)
	if offSummary["enabled"] != false || !strings.Contains(fmt.Sprint(offSummary["reason"]), "JUJU_CLICKHOUSE_URL") {
		t.Errorf("analytics off: %v", offSummary)
	}
	if code := offOwner.do("GET", fmt.Sprintf("/api/ui/projects/%d/analytics/trend?event=x", proj.ID), nil, false, nil); code != 409 {
		t.Errorf("trend with analytics off: %d, want 409", code)
	}
}
