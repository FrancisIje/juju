//go:build integration

package ui_test

import (
	"context"
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
	"juju/internal/queue"
	"juju/internal/server"
	"juju/internal/store"
	"juju/internal/worker"
)

// The Phase 8 "done when", over HTTP: sessions arrive through the normal
// ingest endpoint, a forced crash lowers the release's crash-free rate, and
// a page load shows up on the performance page with its spans.
func TestReleaseHealthAndPerformanceEndToEnd(t *testing.T) {
	log := slog.New(slog.NewTextHandler(io.Discard, nil))
	st := store.New(pool)
	ctx := context.Background()
	q := queue.NewChan(100)
	go worker.Run(ctx, q.Events(), worker.Deps{Store: st, Log: log}, 1)
	t.Cleanup(q.Close)
	srv := httptest.NewServer(server.Routes(server.Deps{Pool: pool, Store: st, Queue: q, MaxUpload: 1 << 20, Auth: &auth.Service{Store: st}, Alerts: &alert.Notifier{Settings: st, Log: log}, Config: config.Config{PublicURL: "http://juju.test"}, Log: log}))
	defer srv.Close()

	// TestDashboardFlowEndToEnd needs a server with no users (first-run
	// sign-up), and this file's tests run before it. Leave none behind.
	t.Cleanup(func() {
		_, _ = pool.Exec(context.Background(), `DELETE FROM users WHERE email LIKE '%@perf.test' OR email LIKE '%@other.test'`)
	})
	login := func(email, org string) *client {
		hash, _ := auth.HashPassword("correct horse battery")
		if _, err := st.CreateUser(ctx, email, "", hash, org, store.JoinOrCreate); err != nil {
			t.Fatal(err)
		}
		c := newClient(t, srv.URL)
		if code := c.do("POST", "/api/ui/auth/login", map[string]string{"email": email, "password": "correct horse battery"}, true, nil); code != 200 {
			t.Fatalf("login: %d", code)
		}
		return c
	}
	ada := login("ada@perf.test", "perf-org")
	var me struct{ Organizations []struct{ ID int64 } }
	ada.do("GET", "/api/ui/me", nil, false, &me)
	var proj struct {
		ID        int64  `json:"id"`
		PublicKey string `json:"public_key"`
	}
	ada.do("POST", fmt.Sprintf("/api/ui/orgs/%d/projects", me.Organizations[0].ID), map[string]string{"name": "perf-app"}, true, &proj)
	base := fmt.Sprintf("/api/ui/projects/%d", proj.ID)

	ingest := func(events ...string) {
		t.Helper()
		body := `{"sdk":{"name":"test"},"events":[` + strings.Join(events, ",") + `]}`
		req, _ := http.NewRequest("POST", fmt.Sprintf("%s/api/v1/projects/%d/events", srv.URL, proj.ID), strings.NewReader(body))
		req.Header.Set("Content-Type", "application/json")
		req.Header.Set("X-Juju-Key", proj.PublicKey)
		res, err := http.DefaultClient.Do(req)
		if err != nil || res.StatusCode != 202 {
			b, _ := io.ReadAll(res.Body)
			t.Fatalf("ingest: %v %d %s", err, res.StatusCode, b)
		}
		res.Body.Close()
	}
	now := time.Now().UTC()
	ts := func(d time.Duration) string { return now.Add(d).Format(time.RFC3339Nano) }
	session := func(sid string, seq int, status string) string {
		return fmt.Sprintf(`{"event_id":%q,"type":"session","platform":"dart","release":"shop@2.0","timestamp":%q,"session":{"sid":%q,"seq":%d,"status":%q,"started":%q,"errors":0}}`,
			uuid.NewString(), ts(0), sid, seq, status, ts(-time.Minute))
	}

	type health struct {
		Totals   store.SessionStats `json:"totals"`
		Releases []store.ReleaseRow `json:"releases"`
	}
	read := func() health {
		var h health
		if code := ada.do("GET", base+"/releases?period=24h", nil, false, &h); code != 200 {
			t.Fatalf("releases: %d", code)
		}
		return h
	}

	// Two sessions start and end normally: 100 % crash-free.
	a, b := uuid.NewString(), uuid.NewString()
	ingest(session(a, 0, "ok"), session(b, 0, "ok"))
	ingest(session(a, 1, "exited"), session(b, 1, "exited"))
	waitFor(t, "two healthy sessions", func() bool { h := read(); return h.Totals.Sessions == 2 })
	if h := read(); *h.Releases[0].CrashFreeSessions != 1 {
		t.Fatalf("before the crash: %+v", h.Releases[0])
	}

	// A third session crashes: 2 of 3 crash-free.
	c := uuid.NewString()
	ingest(session(c, 0, "ok"))
	ingest(session(c, 1, "crashed"))
	waitFor(t, "the crash", func() bool { return read().Totals.Crashed == 1 })
	h := read()
	if h.Totals.Sessions != 3 || h.Releases[0].Release != "shop@2.0" || fmt.Sprintf("%.4f", *h.Releases[0].CrashFreeSessions) != "0.6667" {
		t.Fatalf("after the crash: %+v / %+v", h.Totals, h.Releases)
	}

	// A page load with one HTTP request.
	txID := uuid.NewString()
	ingest(fmt.Sprintf(`{"event_id":%q,"type":"transaction","platform":"javascript","timestamp":%q,"transaction":{"name":"/cart","op":"pageload","start_timestamp":%q,
		"spans":[{"span_id":"s1","op":"http.client","description":"GET /api/cart","start_timestamp":%q,"timestamp":%q}]}}`,
		txID, ts(0), ts(-800*time.Millisecond), ts(-600*time.Millisecond), ts(-200*time.Millisecond)))
	var list struct {
		Transactions []store.TransactionRow `json:"transactions"`
		Ops          []string               `json:"ops"`
	}
	waitFor(t, "the transaction", func() bool {
		ada.do("GET", base+"/transactions?period=24h", nil, false, &list)
		return len(list.Transactions) == 1
	})
	if tr := list.Transactions[0]; tr.Name != "/cart" || tr.P50 != 800 || len(list.Ops) != 1 {
		t.Fatalf("list = %+v", list)
	}
	var detail store.TransactionDetail
	if code := ada.do("GET", base+"/transactions/summary?period=24h&op=pageload&name="+"%2Fcart", nil, false, &detail); code != 200 || len(detail.Samples) != 1 || detail.Samples[0].EventID != txID {
		t.Fatalf("detail: %d %+v", code, detail)
	}
	var payload struct {
		Transaction struct {
			Spans []struct {
				StartMs    float64 `json:"start_ms"`
				DurationMs float64 `json:"duration_ms"`
			} `json:"spans"`
		} `json:"transaction"`
	}
	if code := ada.do("GET", base+"/transactions/events/"+txID, nil, false, &payload); code != 200 || len(payload.Transaction.Spans) != 1 || payload.Transaction.Spans[0].StartMs != 200 || payload.Transaction.Spans[0].DurationMs != 400 {
		t.Fatalf("sample: %d %+v", code, payload)
	}

	// The unhappy paths.
	for _, tc := range []struct {
		path string
		want int
	}{
		{base + "/releases?period=1y", 400},
		{base + "/transactions/summary?period=24h", 400},              // no name
		{base + "/transactions/summary?period=24h&name=%2Fnope", 404}, // nothing by that name
		{base + "/transactions/events/not-a-uuid", 404},               // a 404, not a database error
		{base + "/transactions/events/" + uuid.NewString(), 404},      // not stored
	} {
		if code := ada.do("GET", tc.path, nil, false, nil); code != tc.want {
			t.Errorf("GET %s: %d, want %d", tc.path, code, tc.want)
		}
	}
	// Someone from another organization can't read any of it.
	eve := login("eve@other.test", "other-org")
	for _, p := range []string{"/releases", "/transactions", "/transactions/events/" + txID} {
		if code := eve.do("GET", base+p, nil, false, nil); code != 404 {
			t.Errorf("outsider GET %s: %d, want 404", p, code)
		}
	}
}
