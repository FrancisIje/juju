package alert

import (
	"bufio"
	"context"
	"encoding/json"
	"io"
	"log/slog"
	"net"
	"net/http"
	"net/http/httptest"
	"strconv"
	"strings"
	"sync"
	"sync/atomic"
	"testing"
	"time"

	"juju/internal/event"
	"juju/internal/fingerprint"
	"juju/internal/store"
)

type fakeSettings struct {
	mu         sync.Mutex
	settings   store.AlertSettings
	deliveries []string
}

func (f *fakeSettings) AlertSettings(context.Context, int64) (store.AlertSettings, error) {
	return f.settings, nil
}

func (f *fakeSettings) ProjectByID(context.Context, int64) (store.Project, error) {
	return store.Project{ID: 7, Name: "shop-web"}, nil
}

func (f *fakeSettings) RecordDelivery(_ context.Context, _ int64, _ *int64, kind, channel string, err error) error {
	f.mu.Lock()
	defer f.mu.Unlock()
	status := "ok"
	if err != nil {
		status = "failed: " + err.Error()
	}
	f.deliveries = append(f.deliveries, kind+"/"+channel+"/"+status)
	return nil
}

func (f *fakeSettings) ReleaseInfo(_ context.Context, _ int64, name string) (store.Release, error) {
	if name != "web@2.3.1" {
		return store.Release{}, store.ErrNotFound
	}
	return store.Release{Name: name, Commit: "3f2a9c1d", Repository: "https://github.com/acme/web"}, nil
}

func (f *fakeSettings) recorded() []string {
	f.mu.Lock()
	defer f.mu.Unlock()
	return append([]string(nil), f.deliveries...)
}

// slackServer records what a Slack incoming webhook would receive.
func slackServer(t *testing.T, status int) (*httptest.Server, func() []map[string]any) {
	var mu sync.Mutex
	var got []map[string]any
	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		var body map[string]any
		_ = json.NewDecoder(r.Body).Decode(&body)
		mu.Lock()
		got = append(got, body)
		mu.Unlock()
		w.WriteHeader(status)
		_, _ = w.Write([]byte("ok"))
	}))
	t.Cleanup(srv.Close)
	return srv, func() []map[string]any {
		mu.Lock()
		defer mu.Unlock()
		return append([]map[string]any(nil), got...)
	}
}

func notifier(s *fakeSettings) *Notifier {
	return &Notifier{Settings: s, PublicURL: "https://juju.example.com", AllowHTTPWebhooks: true, Log: slog.New(slog.NewTextHandler(io.Discard, nil))}
}

func regression() (*event.Event, fingerprint.Result, store.SaveResult) {
	return &event.Event{ProjectID: 7, Release: "web@2.3.1", Environment: "production"},
		fingerprint.Result{Title: "TypeError: x is undefined", Culprit: "checkout (app.js)"},
		store.SaveResult{Inserted: true, IssueID: 42, Regressed: true}
}

func waitFor(t *testing.T, cond func() bool) {
	t.Helper()
	deadline := time.Now().Add(5 * time.Second)
	for !cond() {
		if time.Now().After(deadline) {
			t.Fatal("timed out")
		}
		time.Sleep(10 * time.Millisecond)
	}
}

func TestRegressionAlertReachesSlack(t *testing.T) {
	srv, received := slackServer(t, 200)
	s := &fakeSettings{settings: store.AlertSettings{SlackWebhookURL: srv.URL, OnNewIssue: true, OnRegression: true}}
	n := notifier(s)
	n.Start(1)
	n.IssueEvent(regression())
	n.Stop() // waits for the queue to drain

	msgs := received()
	if len(msgs) != 1 {
		t.Fatalf("slack got %d messages, want 1", len(msgs))
	}
	text := msgs[0]["text"].(string)
	if !strings.Contains(text, "Regression in shop-web") || !strings.Contains(text, "TypeError: x is undefined") {
		t.Errorf("text = %q", text)
	}
	raw, _ := json.Marshal(msgs[0]["blocks"])
	if !strings.Contains(string(raw), "https://juju.example.com/p/7/issues/42") || !strings.Contains(string(raw), "web@2.3.1") {
		t.Errorf("blocks don't link to the issue or name the release: %s", raw)
	}
	if got := s.recorded(); len(got) != 1 || got[0] != "regression/slack/ok" {
		t.Errorf("deliveries = %v", got)
	}
}

func TestRuleTogglesAreRespected(t *testing.T) {
	srv, received := slackServer(t, 200)
	s := &fakeSettings{settings: store.AlertSettings{SlackWebhookURL: srv.URL, OnNewIssue: true, OnRegression: false}}
	n := notifier(s)
	n.Start(1)
	n.IssueEvent(regression()) // regressions turned off
	e, fp, res := regression()
	res.Regressed, res.NewIssue = false, true
	n.IssueEvent(e, fp, res) // new issues still on
	n.Stop()
	msgs := received()
	if len(msgs) != 1 || !strings.Contains(msgs[0]["text"].(string), "New issue") {
		t.Fatalf("messages = %v", msgs)
	}
}

func TestSlackErrorIsRecordedNotLost(t *testing.T) {
	srv, received := slackServer(t, 404) // e.g. the webhook was deleted in Slack
	s := &fakeSettings{settings: store.AlertSettings{SlackWebhookURL: srv.URL, OnRegression: true}}
	n := notifier(s)
	n.Start(1)
	n.IssueEvent(regression())
	n.Stop()
	if len(received()) != 3 {
		t.Errorf("tried %d times, want 3 (retries)", len(received()))
	}
	got := s.recorded()
	if len(got) != 1 || !strings.Contains(got[0], "failed: slack answered 404") {
		t.Fatalf("deliveries = %v", got)
	}
}

func TestEmailAlert(t *testing.T) {
	smtpAddr, mails := fakeSMTP(t)
	s := &fakeSettings{settings: store.AlertSettings{EmailTo: []string{"oncall@example.com"}, OnRegression: true}}
	n := notifier(s)
	n.SMTP = SMTPConfig{Addr: smtpAddr, From: "juju <alerts@example.com>"}
	n.Start(1)
	e, fp, res := regression()
	fp.Title = "Bad\r\nBcc: attacker@example.com" // a title must not inject email headers
	n.IssueEvent(e, fp, res)
	n.Stop()
	waitFor(t, func() bool { return len(mails()) == 1 })
	m := mails()[0]
	if !strings.Contains(m, "To: oncall@example.com") || !strings.Contains(m, "Subject: Regression in shop-web") {
		t.Errorf("mail:\n%s", m)
	}
	headers := strings.SplitN(m, "\r\n\r\n", 2)[0]
	if strings.Contains(headers, "\r\nBcc:") {
		t.Errorf("header injection: %q", headers)
	}
	if !strings.Contains(m, "https://juju.example.com/p/7/issues/42") {
		t.Errorf("no link in mail")
	}
}

func TestEmailWithoutSMTPSaysSo(t *testing.T) {
	s := &fakeSettings{settings: store.AlertSettings{EmailTo: []string{"a@example.com"}}}
	results := notifier(s).SendTest(context.Background(), 7, s.settings)
	if !strings.Contains(results["email"], "JUJU_SMTP_ADDR") {
		t.Fatalf("results = %v", results)
	}
}

func TestSendTestReportsEachChannel(t *testing.T) {
	srv, received := slackServer(t, 200)
	s := &fakeSettings{settings: store.AlertSettings{SlackWebhookURL: srv.URL}}
	results := notifier(s).SendTest(context.Background(), 7, s.settings)
	if results["slack"] != "sent" || len(received()) != 1 {
		t.Fatalf("results = %v", results)
	}
	if !strings.Contains(received()[0]["text"].(string), "Test alert") {
		t.Error("not marked as a test")
	}
	// A test alert has no location, release or environment: Slack rejects a
	// context block with empty text, so there must be none.
	for _, b := range received()[0]["blocks"].([]any) {
		if b.(map[string]any)["type"] == "context" {
			t.Errorf("empty context block sent: %v", b)
		}
	}
}

func TestFullQueueDropsInsteadOfBlocking(t *testing.T) {
	n := notifier(&fakeSettings{})
	// Not started: nothing drains the queue.
	done := make(chan struct{})
	go func() {
		for range queueSize + 10 {
			n.IssueEvent(regression())
		}
		close(done)
	}()
	select {
	case <-done:
	case <-time.After(2 * time.Second):
		t.Fatal("IssueEvent blocked: the worker would stall on a slow Slack")
	}
}

func TestValidateWebhookURL(t *testing.T) {
	for _, tc := range []struct {
		url       string
		allowHTTP bool
		ok        bool
	}{
		{"https://hooks.slack.com/services/T/B/x", false, true},
		{"http://localhost:9999/hook", false, false},
		{"http://localhost:9999/hook", true, true},
		{"file:///etc/passwd", true, false},
		{"not a url", false, false},
		// The server's own network is off limits outside local testing.
		{"https://localhost/hook", false, false},
		{"https://127.0.0.1/hook", false, false},
		{"https://10.0.0.5/hook", false, false},
		{"https://192.168.1.1/hook", false, false},
		{"https://169.254.169.254/latest/meta-data", false, false}, // cloud metadata
		{"https://[::1]/hook", false, false},
		{"https://[::ffff:127.0.0.1]/hook", false, false},
		{"https://100.100.100.200/hook", false, false},
		{"https://[::ffff:100.100.100.200]/hook", false, false}, // IPv4 inside IPv6 must not slip past the range check
		{"https://203.0.113.9/hook", false, true},               // a public address
	} {
		if err := ValidateWebhookURL(tc.url, tc.allowHTTP); (err == nil) != tc.ok {
			t.Errorf("%s (allowHTTP=%v): err = %v", tc.url, tc.allowHTTP, err)
		}
	}
}

// A hostname that passes the URL check can still resolve to a private IP,
// so the dialer checks the address it actually connects to.
func TestDialerRefusesPrivateAddresses(t *testing.T) {
	var hits atomic.Int32
	srv := httptest.NewTLSServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) { hits.Add(1) }))
	defer srv.Close()
	n := &Notifier{Settings: &fakeSettings{}, Log: slog.New(slog.NewTextHandler(io.Discard, nil))}
	n.init()
	// "localhost" resolves to 127.0.0.1, like a rebinding DNS name would.
	_, err := n.HTTP.Get(strings.Replace(srv.URL, "127.0.0.1", "localhost", 1))
	if err == nil || !strings.Contains(err.Error(), "public internet addresses") {
		t.Fatalf("err = %v, want the private address refusal", err)
	}
	if hits.Load() != 0 {
		t.Fatal("the request reached the private address")
	}

	// Local testing mode may reach a fake Slack on this machine.
	local := &Notifier{Settings: &fakeSettings{}, AllowHTTPWebhooks: true, Log: n.Log}
	local.init()
	local.HTTP.Transport.(*http.Transport).TLSClientConfig = srv.Client().Transport.(*http.Transport).TLSClientConfig
	if _, err := local.HTTP.Get(srv.URL); err != nil || hits.Load() != 1 {
		t.Fatalf("local testing mode: err = %v, hits = %d", err, hits.Load())
	}
}

// fakeSMTP is the smallest SMTP server net/smtp will talk to: enough to
// capture the message it sends.
func fakeSMTP(t *testing.T) (string, func() []string) {
	ln, err := net.Listen("tcp", "127.0.0.1:0")
	if err != nil {
		t.Fatal(err)
	}
	t.Cleanup(func() { ln.Close() })
	var mu sync.Mutex
	var mails []string
	go func() {
		for {
			c, err := ln.Accept()
			if err != nil {
				return
			}
			go func(c net.Conn) {
				defer c.Close()
				r := bufio.NewReader(c)
				say := func(s string) { _, _ = c.Write([]byte(s + "\r\n")) }
				say("220 fake ESMTP")
				for {
					line, err := r.ReadString('\n')
					if err != nil {
						return
					}
					cmd := strings.ToUpper(strings.TrimSpace(line))
					switch {
					case strings.HasPrefix(cmd, "EHLO"), strings.HasPrefix(cmd, "HELO"):
						say("250 fake")
					case strings.HasPrefix(cmd, "DATA"):
						say("354 go ahead")
						var b strings.Builder
						for {
							l, err := r.ReadString('\n')
							if err != nil || l == ".\r\n" {
								break
							}
							b.WriteString(l)
						}
						mu.Lock()
						mails = append(mails, b.String())
						mu.Unlock()
						say("250 queued")
					case strings.HasPrefix(cmd, "QUIT"):
						say("221 bye")
						return
					default:
						say("250 ok")
					}
				}
			}(c)
		}
	}()
	return ln.Addr().String(), func() []string {
		mu.Lock()
		defer mu.Unlock()
		return append([]string(nil), mails...)
	}
}

// Anyone can sign up and send errors to their own project, so a project
// can't make the server send without end: 30 alerts an hour, then nothing
// until the hour has passed. Another project has its own allowance.
func TestAlertsPerProjectAreCapped(t *testing.T) {
	srv, received := slackServer(t, 200)
	s := &fakeSettings{settings: store.AlertSettings{SlackWebhookURL: srv.URL, OnNewIssue: true, OnRegression: true}}
	n := notifier(s)
	n.Start(1)
	for i := 0; i < MaxAlertsPerHour+20; i++ {
		n.IssueEvent(regression()) // project 7
	}
	e, fp, res := regression()
	e.ProjectID = 8
	n.IssueEvent(e, fp, res)
	n.Stop()
	if got := len(received()); got != MaxAlertsPerHour+1 {
		t.Errorf("slack got %d messages, want %d for project 7 and 1 for project 8", got, MaxAlertsPerHour+1)
	}
}

// The generic webhook is for programs: it must carry the commit (so an agent
// can check out the code that crashed) and a signature the receiver can check.
func TestWebhookIsSignedAndCarriesTheCommit(t *testing.T) {
	type call struct {
		body             []byte
		ts, sig, eventHd string
	}
	got := make(chan call, 1)
	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		b, _ := io.ReadAll(r.Body)
		got <- call{b, r.Header.Get("X-Juju-Timestamp"), r.Header.Get("X-Juju-Signature"), r.Header.Get("X-Juju-Event")}
	}))
	t.Cleanup(srv.Close)
	s := &fakeSettings{settings: store.AlertSettings{WebhookURL: srv.URL, WebhookSecret: "whsec_test", OnRegression: true}}
	n := notifier(s)
	n.Start(1)
	n.IssueEvent(regression())
	n.Stop()

	c := <-got
	if want := Sign("whsec_test", c.ts, c.body); c.sig != want {
		t.Errorf("signature %q, want %q", c.sig, want)
	}
	if Sign("whsec_other", c.ts, c.body) == c.sig {
		t.Error("a different secret gives the same signature")
	}
	if c.eventHd != "regression" {
		t.Errorf("X-Juju-Event = %q", c.eventHd)
	}
	var p WebhookPayload
	if err := json.Unmarshal(c.body, &p); err != nil {
		t.Fatal(err)
	}
	if p.Kind != Regression || p.Issue.ID != 42 || p.Issue.URL != "https://juju.example.com/p/7/issues/42" || p.Project.Name != "shop-web" {
		t.Errorf("payload = %+v", p)
	}
	if p.Commit != "3f2a9c1d" || p.Repository != "https://github.com/acme/web" || p.Release != "web@2.3.1" {
		t.Errorf("payload has no commit: %+v", p)
	}
	if ts, _ := strconv.ParseInt(c.ts, 10, 64); ts != p.SentAt.Unix() {
		t.Errorf("timestamp header %s doesn't match sent_at %v", c.ts, p.SentAt)
	}
	if got := s.recorded(); len(got) != 1 || got[0] != "regression/webhook/ok" {
		t.Errorf("deliveries = %v", got)
	}
}

// A release CI never recorded still alerts, just without a commit.
func TestWebhookWithoutRecordedRelease(t *testing.T) {
	got := make(chan []byte, 1)
	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		b, _ := io.ReadAll(r.Body)
		got <- b
	}))
	t.Cleanup(srv.Close)
	s := &fakeSettings{settings: store.AlertSettings{WebhookURL: srv.URL, WebhookSecret: "whsec_test", OnNewIssue: true}}
	n := notifier(s)
	n.Start(1)
	e, fp, res := regression()
	e.Release, res.Regressed = "web@9.9.9", false
	n.IssueEvent(e, fp, res)
	n.Stop()
	b := <-got
	if strings.Contains(string(b), `"commit"`) {
		t.Errorf("payload has a commit for an unrecorded release: %s", b)
	}
}
