// Package alert tells people about new issues and regressions, through a
// Slack incoming webhook, email, and a signed JSON webhook for programs
// (such as a job that starts an AI agent on the new issue).
//
// Sending happens on its own goroutines with its own queue, so a slow or
// down Slack never delays the worker that's storing events.
package alert

import (
	"bytes"
	"context"
	"crypto/hmac"
	"crypto/sha256"
	"encoding/hex"
	"encoding/json"
	"errors"
	"fmt"
	"io"
	"log/slog"
	"net"
	"net/http"
	"net/netip"
	"net/smtp"
	"net/url"
	"strconv"
	"strings"
	"sync"
	"syscall"
	"time"

	"juju/internal/event"
	"juju/internal/fingerprint"
	"juju/internal/ratelimit"
	"juju/internal/store"
)

type Kind string

const (
	NewIssue   Kind = "new_issue"
	Regression Kind = "regression"
	Test       Kind = "test"
)

// Alert is one message about one issue.
type Alert struct {
	Kind        Kind
	ProjectID   int64
	IssueID     int64
	Title       string
	Culprit     string
	Release     string
	Environment string
}

// Settings is the slice of the database the notifier needs.
type Settings interface {
	AlertSettings(ctx context.Context, projectID int64) (store.AlertSettings, error)
	ProjectByID(ctx context.Context, id int64) (store.Project, error)
	RecordDelivery(ctx context.Context, projectID int64, issueID *int64, kind, channel string, err error) error
	ReleaseInfo(ctx context.Context, projectID int64, name string) (store.Release, error)
}

type SMTPConfig struct {
	Addr     string // host:port; empty disables email
	Username string
	Password string
	From     string
}

type Notifier struct {
	Settings  Settings
	SMTP      SMTPConfig
	PublicURL string // where links in alerts point, e.g. https://juju.example.com
	// AllowHTTPWebhooks permits http:// webhook URLs and private network
	// addresses. Only for local testing: see ValidateWebhookURL.
	AllowHTTPWebhooks bool
	HTTP              *http.Client
	Log               *slog.Logger

	queue chan Alert
	wg    sync.WaitGroup
	once  sync.Once

	// At most 30 alerts per project an hour. A bad release creating
	// hundreds of new issues gets announced, not sent hundreds of times.
	// And since anyone can sign up and send errors to their own project,
	// it stops the server being used to mail or post to an address of
	// their choosing without end. warned logs that once an hour, not per
	// dropped alert.
	perProject ratelimit.Window
	warned     ratelimit.Window
}

// MaxAlertsPerHour is how many alerts one project can send in an hour.
const MaxAlertsPerHour = 30

const queueSize = 1000

// Start runs the senders until Stop.
func (n *Notifier) Start(workers int) {
	n.init()
	for range workers {
		n.wg.Go(func() {
			for a := range n.queue {
				n.deliver(context.Background(), a)
			}
		})
	}
}

// Stop finishes sending what's queued, then returns.
func (n *Notifier) Stop() {
	n.init()
	close(n.queue)
	n.wg.Wait()
}

func (n *Notifier) init() {
	n.once.Do(func() {
		n.queue = make(chan Alert, queueSize)
		n.perProject.Max, n.perProject.Per = MaxAlertsPerHour, time.Hour
		n.warned.Max, n.warned.Per = 1, time.Hour
		if n.HTTP == nil {
			dialer := &net.Dialer{Timeout: 5 * time.Second}
			if !n.AllowHTTPWebhooks {
				dialer.Control = publicOnly
			}
			n.HTTP = &http.Client{Timeout: 10 * time.Second, Transport: &http.Transport{DialContext: dialer.DialContext, TLSHandshakeTimeout: 5 * time.Second}}
		}
	})
}

// IssueEvent is called by the worker after an event created or reopened an
// issue. It never blocks: if the queue is full, the alert is dropped and
// logged, because storing events matters more than announcing them.
func (n *Notifier) IssueEvent(e *event.Event, fp fingerprint.Result, res store.SaveResult) {
	kind := NewIssue
	if res.Regressed {
		kind = Regression
	}
	a := Alert{Kind: kind, ProjectID: e.ProjectID, IssueID: res.IssueID, Title: fp.Title, Culprit: fp.Culprit, Release: e.Release, Environment: e.Environment}
	n.init()
	select {
	case n.queue <- a:
	default:
		n.Log.Warn("alert queue full, dropping alert", "project_id", a.ProjectID, "issue_id", a.IssueID, "kind", a.Kind)
	}
}

// SendTest sends a test alert to the given settings right away and returns
// each channel's result, so the settings page can say what worked.
func (n *Notifier) SendTest(ctx context.Context, projectID int64, s store.AlertSettings) map[string]string {
	n.init()
	a := Alert{Kind: Test, ProjectID: projectID, Title: "If you can read this, alerts for this project work."}
	results := map[string]string{}
	for channel, err := range n.send(ctx, a, s) {
		_ = n.Settings.RecordDelivery(ctx, projectID, nil, string(Test), channel, err)
		if err != nil {
			results[channel] = err.Error()
		} else {
			results[channel] = "sent"
		}
	}
	return results
}

func (n *Notifier) deliver(ctx context.Context, a Alert) {
	s, err := n.Settings.AlertSettings(ctx, a.ProjectID)
	if err != nil {
		n.Log.Error("load alert settings", "project_id", a.ProjectID, "err", err)
		return
	}
	if (a.Kind == NewIssue && !s.OnNewIssue) || (a.Kind == Regression && !s.OnRegression) {
		return
	}
	if key := strconv.FormatInt(a.ProjectID, 10); !n.perProject.Allow(key) {
		if n.warned.Allow(key) {
			n.Log.Warn("alert limit reached for this hour; further alerts are not sent", "project_id", a.ProjectID, "limit", MaxAlertsPerHour)
		}
		return
	}
	issueID := a.IssueID
	for channel, err := range n.send(ctx, a, s) {
		if err != nil {
			n.Log.Warn("alert not delivered", "project_id", a.ProjectID, "issue_id", a.IssueID, "channel", channel, "err", err)
		}
		_ = n.Settings.RecordDelivery(ctx, a.ProjectID, &issueID, string(a.Kind), channel, err)
	}
}

// send delivers to every configured channel. Each channel is retried a
// little: a webhook call can fail for a moment without anything being wrong.
func (n *Notifier) send(ctx context.Context, a Alert, s store.AlertSettings) map[string]error {
	project, _ := n.Settings.ProjectByID(ctx, a.ProjectID)
	link := n.issueURL(project, a)
	out := map[string]error{}
	if s.SlackWebhookURL != "" {
		out["slack"] = retry(func() error { return n.postSlack(ctx, s.SlackWebhookURL, a, project, link) })
	}
	if s.WebhookURL != "" {
		out["webhook"] = retry(func() error { return n.postWebhook(ctx, s.WebhookURL, s.WebhookSecret, a, project, link) })
	}
	if len(s.EmailTo) > 0 {
		if n.SMTP.Addr == "" {
			out["email"] = errors.New("email isn't set up on this server (JUJU_SMTP_ADDR)")
		} else {
			out["email"] = retry(func() error { return n.sendEmail(s.EmailTo, a, project, link) })
		}
	}
	return out
}

func retry(f func() error) error {
	var err error
	for i := range 3 {
		if err = f(); err == nil {
			return nil
		}
		time.Sleep(time.Duration(i+1) * 500 * time.Millisecond)
	}
	return err
}

func (n *Notifier) issueURL(p store.Project, a Alert) string {
	base := strings.TrimRight(n.PublicURL, "/")
	if a.IssueID == 0 {
		return fmt.Sprintf("%s/p/%d/issues", base, a.ProjectID)
	}
	return fmt.Sprintf("%s/p/%d/issues/%d", base, a.ProjectID, a.IssueID)
}

func headline(a Alert, project store.Project) string {
	switch a.Kind {
	case NewIssue:
		return "New issue in " + project.Name
	case Regression:
		return "Regression in " + project.Name + ": a resolved issue happened again"
	}
	return "Test alert for " + project.Name
}

// postSlack sends a Slack Block Kit message. "text" is the fallback shown
// in notifications and by clients that don't render blocks.
func (n *Notifier) postSlack(ctx context.Context, webhook string, a Alert, project store.Project, link string) error {
	if err := ValidateWebhookURL(webhook, n.AllowHTTPWebhooks); err != nil {
		return err
	}
	details := []string{}
	if a.Culprit != "" {
		details = append(details, "*Where:* `"+a.Culprit+"`")
	}
	if a.Release != "" {
		details = append(details, "*Release:* "+a.Release)
	}
	if a.Environment != "" {
		details = append(details, "*Environment:* "+a.Environment)
	}
	blocks := []any{map[string]any{"type": "section", "text": map[string]any{"type": "mrkdwn", "text": "*" + headline(a, project) + "*\n" + a.Title}}}
	// Slack rejects a context block with empty text, so it's only added
	// when there's something to put in it.
	if len(details) > 0 {
		blocks = append(blocks, map[string]any{"type": "context", "elements": []any{map[string]any{"type": "mrkdwn", "text": strings.Join(details, "   ")}}})
	}
	blocks = append(blocks, map[string]any{"type": "actions", "elements": []any{map[string]any{
		"type": "button", "text": map[string]any{"type": "plain_text", "text": "Open in juju"}, "url": link,
	}}})
	body := map[string]any{"text": headline(a, project) + ": " + a.Title, "blocks": blocks}
	payload, _ := json.Marshal(body)
	req, err := http.NewRequestWithContext(ctx, "POST", webhook, bytes.NewReader(payload))
	if err != nil {
		return err
	}
	req.Header.Set("Content-Type", "application/json")
	res, err := n.HTTP.Do(req)
	if err != nil {
		return fmt.Errorf("slack: %w", err)
	}
	defer res.Body.Close()
	if res.StatusCode >= 300 {
		b, _ := io.ReadAll(io.LimitReader(res.Body, 200))
		return fmt.Errorf("slack answered %s: %s", res.Status, strings.TrimSpace(string(b)))
	}
	return nil
}

// WebhookPayload is the JSON body of a webhook call. It carries what a
// program needs to act on the issue without another request: the release's
// commit (when CI recorded it) says which code to check out.
type WebhookPayload struct {
	Kind        Kind           `json:"kind"`
	Project     WebhookProject `json:"project"`
	Issue       WebhookIssue   `json:"issue"`
	Release     string         `json:"release,omitempty"`
	Environment string         `json:"environment,omitempty"`
	Commit      string         `json:"commit,omitempty"`
	Repository  string         `json:"repository,omitempty"`
	SentAt      time.Time      `json:"sent_at"`
}

type WebhookProject struct {
	ID   int64  `json:"id"`
	Name string `json:"name"`
}

type WebhookIssue struct {
	ID      int64  `json:"id,omitempty"`
	Title   string `json:"title"`
	Culprit string `json:"culprit,omitempty"`
	URL     string `json:"url"`
}

// postWebhook sends the payload signed with the project's webhook secret:
//
//	X-Juju-Timestamp: 1767225600
//	X-Juju-Signature: sha256=<hex HMAC-SHA256 of "1767225600.<body>">
//
// The receiver recomputes the HMAC with the same secret: a match proves the
// call came from juju and the body wasn't changed. Signing the timestamp too
// lets it refuse old calls, so a recorded request can't be replayed later.
func (n *Notifier) postWebhook(ctx context.Context, webhook, secret string, a Alert, project store.Project, link string) error {
	if err := ValidateWebhookURL(webhook, n.AllowHTTPWebhooks); err != nil {
		return err
	}
	p := WebhookPayload{
		Kind:    a.Kind,
		Project: WebhookProject{ID: a.ProjectID, Name: project.Name},
		Issue:   WebhookIssue{ID: a.IssueID, Title: a.Title, Culprit: a.Culprit, URL: link},
		Release: a.Release, Environment: a.Environment, SentAt: time.Now().UTC(),
	}
	if a.Release != "" {
		if r, err := n.Settings.ReleaseInfo(ctx, a.ProjectID, a.Release); err == nil {
			p.Commit, p.Repository = r.Commit, r.Repository
		}
	}
	body, _ := json.Marshal(p)
	ts := strconv.FormatInt(p.SentAt.Unix(), 10)
	req, err := http.NewRequestWithContext(ctx, "POST", webhook, bytes.NewReader(body))
	if err != nil {
		return err
	}
	req.Header.Set("Content-Type", "application/json")
	req.Header.Set("User-Agent", "juju-webhook/1")
	req.Header.Set("X-Juju-Event", string(a.Kind))
	req.Header.Set("X-Juju-Timestamp", ts)
	req.Header.Set("X-Juju-Signature", Sign(secret, ts, body))
	res, err := n.HTTP.Do(req)
	if err != nil {
		return fmt.Errorf("webhook: %w", err)
	}
	defer res.Body.Close()
	if res.StatusCode >= 300 {
		b, _ := io.ReadAll(io.LimitReader(res.Body, 200))
		return fmt.Errorf("webhook answered %s: %s", res.Status, strings.TrimSpace(string(b)))
	}
	return nil
}

// Sign is the X-Juju-Signature value for a body sent at timestamp ts.
// Receivers can use the same function (or its three lines) to check one.
func Sign(secret, ts string, body []byte) string {
	mac := hmac.New(sha256.New, []byte(secret))
	mac.Write([]byte(ts))
	mac.Write([]byte("."))
	mac.Write(body)
	return "sha256=" + hex.EncodeToString(mac.Sum(nil))
}

func (n *Notifier) sendEmail(to []string, a Alert, project store.Project, link string) error {
	subject := headline(a, project) + ": " + a.Title
	var b strings.Builder
	fmt.Fprintf(&b, "From: %s\r\nTo: %s\r\nSubject: %s\r\nMIME-Version: 1.0\r\nContent-Type: text/plain; charset=utf-8\r\n\r\n", n.SMTP.From, strings.Join(to, ", "), oneLine(subject))
	fmt.Fprintf(&b, "%s\r\n\r\n%s\r\n", headline(a, project), a.Title)
	if a.Culprit != "" {
		fmt.Fprintf(&b, "Where: %s\r\n", a.Culprit)
	}
	if a.Release != "" {
		fmt.Fprintf(&b, "Release: %s\r\n", a.Release)
	}
	fmt.Fprintf(&b, "\r\nOpen in juju: %s\r\n", link)
	var auth smtp.Auth
	if n.SMTP.Username != "" {
		host := strings.Split(n.SMTP.Addr, ":")[0]
		auth = smtp.PlainAuth("", n.SMTP.Username, n.SMTP.Password, host)
	}
	return smtp.SendMail(n.SMTP.Addr, auth, n.SMTP.From, to, []byte(b.String()))
}

// oneLine keeps an error title from injecting extra email headers.
func oneLine(s string) string { return strings.NewReplacer("\r", " ", "\n", " ").Replace(s) }

// ValidateWebhookURL checks a webhook URL before juju's server calls it.
// The server makes this request, to an address a user typed in, so an
// http:// URL could point it at internal services that trust their own
// network (this is called SSRF). Requiring https:// to a real host name
// rules out most of that; plain http is allowed only for local testing.
func ValidateWebhookURL(raw string, allowHTTP bool) error {
	u, err := url.Parse(strings.TrimSpace(raw))
	if err != nil || u.Host == "" {
		return errors.New("the webhook URL isn't a valid URL")
	}
	if u.Scheme != "https" && !(allowHTTP && u.Scheme == "http") {
		return errors.New("the webhook URL must start with https://")
	}
	// Caught early for a clear message. The dialer (publicOnly) is what
	// really enforces it, since a public name can resolve to a private IP.
	if !allowHTTP {
		host := u.Hostname()
		if ip, err := netip.ParseAddr(host); (err == nil && !isPublic(ip)) || host == "localhost" || strings.HasSuffix(host, ".localhost") {
			return errPrivateAddress
		}
	}
	return nil
}

var errPrivateAddress = errors.New("juju only sends webhooks to public internet addresses, not to this server's own network")

// publicOnly runs just before each connection, with the IP actually being
// dialed. Checking here, not when the URL is saved, also covers a hostname
// that resolves to a private IP (or changes to one later) and redirects.
// Without it, anyone who can edit a project's alerts could make the server
// send requests into its own network (SSRF).
func publicOnly(_, address string, _ syscall.RawConn) error {
	host, _, err := net.SplitHostPort(address)
	if err != nil {
		return err
	}
	ip, err := netip.ParseAddr(host)
	if err != nil || !isPublic(ip) {
		return errPrivateAddress
	}
	return nil
}

var sharedAddressSpace = netip.MustParsePrefix("100.64.0.0/10") // carrier-grade NAT, also used inside clouds

func isPublic(ip netip.Addr) bool {
	ip = ip.Unmap()
	return ip.IsGlobalUnicast() && !ip.IsPrivate() && !sharedAddressSpace.Contains(ip)
}
