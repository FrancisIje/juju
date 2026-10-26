package api

import (
	"context"
	"encoding/json"
	"io"
	"log/slog"
	"net/http"
	"net/http/httptest"
	"strings"
	"testing"
	"time"

	"juju/internal/store"
)

// The fakeStore methods below are the fix workflow's. The fake keeps
// state (statuses, links, releases) so a test can change something and
// read it back, as an agent would.

var seen = time.Date(2026, 10, 26, 9, 0, 0, 0, time.UTC)

// issue 7 is in project 1; no other issue exists.
func (f *fakeStore) issue() store.IssueDetail {
	if f.status == "" {
		f.status = "unresolved"
	}
	return store.IssueDetail{
		IssueRow: store.IssueRow{ID: 7, Title: "TypeError: cart.items is undefined", Culprit: "renderCart (src/cart.js)", Status: f.status,
			Platform: "javascript", LastRelease: "web@1.4.0", EventCount: 12, UserCount: 3, FirstSeen: seen, LastSeen: seen},
		Releases: []string{"web@1.4.0", "web@1.3.0"},
		Commits:  f.commits(),
		Links:    append([]store.IssueLink{}, f.links...),
	}
}

func (f *fakeStore) commits() []store.Release {
	out := []store.Release{}
	for _, r := range f.releases {
		out = append(out, r)
	}
	return out
}

func (f *fakeStore) SearchIssues(_ context.Context, projectID int64, flt store.IssueFilter) ([]store.IssueRow, int64, error) {
	f.gotFilter = flt
	d := f.issue()
	if projectID != 1 || (flt.Status != "" && flt.Status != d.Status) {
		return nil, 0, nil
	}
	return []store.IssueRow{d.IssueRow}, 1, nil
}

func (f *fakeStore) IssueDetail(_ context.Context, projectID, issueID int64) (store.IssueDetail, error) {
	if projectID != 1 || issueID != 7 {
		return store.IssueDetail{}, store.ErrNotFound
	}
	return f.issue(), nil
}

const fakeEvent = `{"event_id":"e1","type":"error","timestamp":"2026-10-26T09:00:00Z","platform":"javascript","release":"web@1.4.0","environment":"production",
"exception":{"type":"TypeError","message":"cart.items is undefined","stacktrace":{"frames":[
 {"function":"renderCart","filename":"src/cart.js","lineno":42,"colno":17,"in_app":true},
 {"function":"commitRoot","filename":"node_modules/react-dom/cjs/react-dom.js","lineno":100,"in_app":false}]}},
"breadcrumbs":[{"type":"navigation","message":"/checkout","timestamp":"2026-10-26T08:59:58Z"},{"type":"http","message":"GET /api/cart 500","timestamp":"2026-10-26T08:59:59Z"}],
"tags":{"browser":"Chrome 141"},"symbolication":{"status":"symbolicated"}}`

func (f *fakeStore) IssueEvents(_ context.Context, projectID, issueID int64, _ time.Time, limit int) ([]json.RawMessage, error) {
	if projectID != 1 || issueID != 7 {
		return nil, nil
	}
	return []json.RawMessage{json.RawMessage(fakeEvent)}[:min(1, limit)], nil
}

func (f *fakeStore) SetIssueStatus(_ context.Context, projectID int64, ids []int64, status string, userID int64) (int64, error) {
	f.gotUserID = userID
	if projectID != 1 || len(ids) != 1 || ids[0] != 7 || f.issue().Status == status {
		return 0, nil
	}
	f.status = status
	return 1, nil
}

func (f *fakeStore) AddIssueLink(_ context.Context, projectID, issueID int64, l store.IssueLink) (store.IssueLink, error) {
	if projectID != 1 || issueID != 7 {
		return l, store.ErrNotFound
	}
	l.CreatedAt = seen
	f.links = append(f.links, l)
	return l, nil
}

func (f *fakeStore) SaveRelease(_ context.Context, _ int64, r store.Release) (store.Release, error) {
	if f.releases == nil {
		f.releases = map[string]store.Release{}
	}
	f.releases[r.Name] = r
	return r, nil
}

type countChanges struct{ n int }

func (c *countChanges) Changed(int64) { c.n++ }

type harness struct {
	t       *testing.T
	store   *fakeStore
	changes *countChanges
	mux     *http.ServeMux
}

func newHarness(t *testing.T) *harness {
	h := &harness{t: t, store: &fakeStore{}, changes: &countChanges{}, mux: http.NewServeMux()}
	(&API{Store: h.store, Changes: h.changes, PublicURL: "https://juju.example.com", Log: slog.New(slog.NewTextHandler(io.Discard, nil))}).Register(h.mux)
	return h
}

// do sends a request with the good secret key unless headers say otherwise.
func (h *harness) do(method, path, body string, headers ...string) *httptest.ResponseRecorder {
	req := httptest.NewRequest(method, path, strings.NewReader(body))
	req.Header.Set("Authorization", "Bearer sk_good")
	for i := 0; i+1 < len(headers); i += 2 {
		req.Header.Set(headers[i], headers[i+1])
	}
	rec := httptest.NewRecorder()
	h.mux.ServeHTTP(rec, req)
	return rec
}

func TestFixWorkflowEndpoints(t *testing.T) {
	tests := []struct {
		name, method, path, body string
		wantStatus               int
		wantBody                 string
	}{
		{"issue detail", "GET", "/api/v1/projects/1/issues/7", "", 200, `"title":"TypeError: cart.items is undefined"`},
		{"unknown issue", "GET", "/api/v1/projects/1/issues/8", "", 404, "unknown issue"},
		{"issue id not a number", "GET", "/api/v1/projects/1/issues/abc", "", 404, "unknown issue"},
		{"resolve", "POST", "/api/v1/projects/1/issues/7/status", `{"status":"resolved"}`, 200, `"changed":true`},
		{"bad status", "POST", "/api/v1/projects/1/issues/7/status", `{"status":"fixed"}`, 400, "status must be"},
		{"status of unknown issue", "POST", "/api/v1/projects/1/issues/8/status", `{"status":"resolved"}`, 404, "unknown issue"},
		{"status not JSON", "POST", "/api/v1/projects/1/issues/7/status", `status=resolved`, 400, "must be JSON"},
		{"link a PR", "POST", "/api/v1/projects/1/issues/7/links", `{"url":"https://github.com/acme/web/pull/42","title":"Fix cart"}`, 201, `"url":"https://github.com/acme/web/pull/42"`},
		{"link must be http(s)", "POST", "/api/v1/projects/1/issues/7/links", `{"url":"javascript:alert(1)"}`, 400, "http:// or https://"},
		{"link with a token in it", "POST", "/api/v1/projects/1/issues/7/links", `{"url":"https://x:ghp_secret@github.com/acme/web/pull/1"}`, 400, "user name or password"},
		{"link to unknown issue", "POST", "/api/v1/projects/1/issues/8/links", `{"url":"https://github.com/acme/web/pull/42"}`, 404, "unknown issue"},
		{"record a release", "POST", "/api/v1/projects/1/releases", `{"name":"web@1.4.0","commit":"3F2A9C1D","repository":"https://github.com/acme/web"}`, 200, `"commit":"3f2a9c1d"`},
		{"release needs a name", "POST", "/api/v1/projects/1/releases", `{"commit":"3f2a9c1d"}`, 400, "name must be"},
		{"commit must be hex", "POST", "/api/v1/projects/1/releases", `{"name":"web@1","commit":"main"}`, 400, "git commit id"},
		{"repository with a token", "POST", "/api/v1/projects/1/releases", `{"name":"web@1","repository":"https://x-access-token:ghs_x@github.com/acme/web"}`, 400, "user name or password"},
		{"writes need the secret key", "POST", "/api/v1/projects/1/issues/7/status", `{"status":"resolved"}`, 401, "invalid secret key"},
	}
	for _, tc := range tests {
		t.Run(tc.name, func(t *testing.T) {
			h := newHarness(t)
			var rec *httptest.ResponseRecorder
			if tc.wantStatus == 401 {
				rec = h.do(tc.method, tc.path, tc.body, "Authorization", "Bearer pk_public")
			} else {
				rec = h.do(tc.method, tc.path, tc.body)
			}
			if rec.Code != tc.wantStatus {
				t.Errorf("status = %d, want %d; body %s", rec.Code, tc.wantStatus, rec.Body)
			}
			if !strings.Contains(rec.Body.String(), tc.wantBody) {
				t.Errorf("body %s does not contain %s", rec.Body, tc.wantBody)
			}
		})
	}
}

func TestStatusChangeIsRecordedAndAnnounced(t *testing.T) {
	h := newHarness(t)
	h.do("POST", "/api/v1/projects/1/issues/7/status", `{"status":"resolved"}`)
	if h.store.status != "resolved" || h.store.gotUserID != 0 {
		t.Errorf("status %q by user %d, want resolved by no user (a key)", h.store.status, h.store.gotUserID)
	}
	if h.changes.n != 1 {
		t.Errorf("dashboards told %d times, want 1", h.changes.n)
	}
	// Resolving again changes nothing, so nothing is announced.
	rec := h.do("POST", "/api/v1/projects/1/issues/7/status", `{"status":"resolved"}`)
	if !strings.Contains(rec.Body.String(), `"changed":false`) || h.changes.n != 1 {
		t.Errorf("second resolve: %s, changes %d", rec.Body, h.changes.n)
	}
}

// mcp sends one JSON-RPC request and decodes the reply.
func (h *harness) mcp(body string, headers ...string) (int, map[string]any) {
	h.t.Helper()
	rec := h.do("POST", "/api/v1/projects/1/mcp", body, headers...)
	if rec.Body.Len() == 0 {
		return rec.Code, nil
	}
	var out map[string]any
	if err := json.Unmarshal(rec.Body.Bytes(), &out); err != nil {
		h.t.Fatalf("reply isn't JSON: %s", rec.Body)
	}
	return rec.Code, out
}

// toolText calls a tool and returns its text and isError.
func (h *harness) toolText(name, args string) (string, bool) {
	h.t.Helper()
	_, out := h.mcp(`{"jsonrpc":"2.0","id":1,"method":"tools/call","params":{"name":"` + name + `","arguments":` + args + `}}`)
	res, ok := out["result"].(map[string]any)
	if !ok {
		h.t.Fatalf("no result: %v", out)
	}
	content := res["content"].([]any)[0].(map[string]any)
	return content["text"].(string), res["isError"].(bool)
}

func TestMCPHandshake(t *testing.T) {
	h := newHarness(t)
	code, out := h.mcp(`{"jsonrpc":"2.0","id":1,"method":"initialize","params":{"protocolVersion":"2025-06-18","capabilities":{},"clientInfo":{"name":"test","version":"1"}}}`)
	res := out["result"].(map[string]any)
	if code != 200 || res["protocolVersion"] != "2025-06-18" {
		t.Fatalf("initialize: %d %v", code, out)
	}
	if _, ok := res["capabilities"].(map[string]any)["tools"]; !ok {
		t.Error("server doesn't announce tools")
	}
	// An unknown version gets our newest, and the client decides.
	_, out = h.mcp(`{"jsonrpc":"2.0","id":"a","method":"initialize","params":{"protocolVersion":"1999-01-01"}}`)
	if v := out["result"].(map[string]any)["protocolVersion"]; v != mcpVersions[0] || out["id"] != "a" {
		t.Errorf("version %v, id %v", v, out["id"])
	}
	// Notifications get 202 and no body.
	if code, out := h.mcp(`{"jsonrpc":"2.0","method":"notifications/initialized"}`); code != 202 || out != nil {
		t.Errorf("notification: %d %v", code, out)
	}
	_, out = h.mcp(`{"jsonrpc":"2.0","id":2,"method":"tools/list"}`, "MCP-Protocol-Version", "2025-06-18")
	var names []string
	for _, tool := range out["result"].(map[string]any)["tools"].([]any) {
		tm := tool.(map[string]any)
		names = append(names, tm["name"].(string))
		if tm["inputSchema"].(map[string]any)["type"] != "object" {
			t.Errorf("%s: inputSchema must be an object schema", tm["name"])
		}
	}
	if strings.Join(names, ",") != "list_issues,get_issue,list_issue_events,update_issue,link_issue" {
		t.Errorf("tools = %v", names)
	}
}

func TestMCPProtocolErrors(t *testing.T) {
	tests := []struct {
		name, body string
		headers    []string
		wantStatus int
		wantCode   float64
	}{
		{"not JSON", `{nope`, nil, 200, rpcParseError},
		{"batch", `[{"jsonrpc":"2.0","id":1,"method":"ping"}]`, nil, 200, rpcInvalidRequest},
		{"not JSON-RPC 2.0", `{"id":1,"method":"ping"}`, nil, 200, rpcInvalidRequest},
		{"unknown method", `{"jsonrpc":"2.0","id":1,"method":"resources/list"}`, nil, 200, rpcMethodNotFound},
		{"unknown tool", `{"jsonrpc":"2.0","id":1,"method":"tools/call","params":{"name":"rm_rf","arguments":{}}}`, nil, 200, rpcInvalidParams},
		{"unsupported version header", `{"jsonrpc":"2.0","id":1,"method":"ping"}`, []string{"MCP-Protocol-Version", "2020-01-01"}, 400, 0},
		{"another site's page", `{"jsonrpc":"2.0","id":1,"method":"ping"}`, []string{"Origin", "https://evil.example"}, 403, 0},
	}
	for _, tc := range tests {
		t.Run(tc.name, func(t *testing.T) {
			code, out := newHarness(t).mcp(tc.body, tc.headers...)
			if code != tc.wantStatus {
				t.Fatalf("status %d, want %d: %v", code, tc.wantStatus, out)
			}
			if tc.wantCode == 0 {
				return
			}
			e, ok := out["error"].(map[string]any)
			if !ok || e["code"] != tc.wantCode {
				t.Errorf("error = %v, want code %v", out["error"], tc.wantCode)
			}
		})
	}
	// Our own dashboard's origin is fine.
	if code, _ := newHarness(t).mcp(`{"jsonrpc":"2.0","id":1,"method":"ping"}`, "Origin", "https://juju.example.com"); code != 200 {
		t.Errorf("same origin: %d", code)
	}
	// And the endpoint needs the secret key like the rest of the API.
	if rec := newHarness(t).do("POST", "/api/v1/projects/1/mcp", `{"jsonrpc":"2.0","id":1,"method":"ping"}`, "Authorization", ""); rec.Code != 401 {
		t.Errorf("no key: %d", rec.Code)
	}
}

// The whole loop an agent goes through: find the bug, read it, see which
// commit to check out, link the PR, resolve.
func TestMCPFixLoop(t *testing.T) {
	h := newHarness(t)
	h.do("POST", "/api/v1/projects/1/releases", `{"name":"web@1.4.0","commit":"3f2a9c1d","repository":"https://github.com/acme/web"}`)

	text, isErr := h.toolText("list_issues", `{}`)
	if isErr || !strings.Contains(text, "#7 [unresolved] TypeError: cart.items is undefined") || !strings.Contains(text, "12 events, 3 users") {
		t.Errorf("list_issues:\n%s", text)
	}
	if h.store.gotFilter.Status != "unresolved" || h.store.gotFilter.Limit != 20 {
		t.Errorf("defaults: %+v", h.store.gotFilter)
	}

	text, isErr = h.toolText("get_issue", `{"issue_id":7}`)
	for _, want := range []string{
		"# Issue 7: TypeError: cart.items is undefined",
		"- web@1.4.0: commit 3f2a9c1d in https://github.com/acme/web", // which code to read
		"- web@1.3.0: no commit recorded",
		"* renderCart  src/cart.js:42:17",                           // the user's own frame, marked
		"  commitRoot  node_modules/react-dom/cjs/react-dom.js:100", // a library frame, not marked
		"- 2026-10-26T08:59:59Z [http] GET /api/cart 500",           // what led up to it
		"Dashboard: https://juju.example.com/p/1/issues/7",
		"Symbolication: symbolicated",
	} {
		if !strings.Contains(text, want) {
			t.Errorf("get_issue is missing %q:\n%s", want, text)
		}
	}
	if isErr {
		t.Error("get_issue isError")
	}

	if text, _ := h.toolText("link_issue", `{"issue_id":7,"url":"https://github.com/acme/web/pull/42","title":"Fix empty cart"}`); !strings.Contains(text, "Linked") {
		t.Errorf("link_issue: %s", text)
	}
	if text, _ := h.toolText("get_issue", `{"issue_id":7}`); !strings.Contains(text, "- Fix empty cart: https://github.com/acme/web/pull/42") {
		t.Errorf("link not shown:\n%s", text)
	}
	if text, _ := h.toolText("update_issue", `{"issue_id":7,"status":"resolved"}`); text != "Issue 7 is now resolved." {
		t.Errorf("update_issue: %s", text)
	}
	if text, _ := h.toolText("update_issue", `{"issue_id":7,"status":"resolved"}`); text != "Issue 7 was already resolved." {
		t.Errorf("update_issue again: %s", text)
	}
	if text, _ := h.toolText("list_issues", `{"status":"all"}`); !strings.Contains(text, "[resolved]") {
		t.Errorf("list after resolve: %s", text)
	}
}

// Tool mistakes come back as isError results the model can read and
// correct, not as protocol errors.
func TestMCPToolErrors(t *testing.T) {
	tests := []struct{ tool, args, want string }{
		{"get_issue", `{}`, "issue_id is required"},
		{"get_issue", `{"issue_id":8}`, "no issue with that id in this project"},
		{"get_issue", `{"issue_id":"seven"}`, "invalid arguments"},
		{"list_issue_events", `{"issue_id":8}`, "no issue with that id"},
		{"update_issue", `{"issue_id":7,"status":"done"}`, "status must be"},
		{"link_issue", `{"issue_id":7,"url":"file:///etc/passwd"}`, "http:// or https://"},
		{"list_issues", `{"status":"open"}`, "status must be"},
	}
	for _, tc := range tests {
		t.Run(tc.tool+" "+tc.args, func(t *testing.T) {
			text, isErr := newHarness(t).toolText(tc.tool, tc.args)
			if !isErr || !strings.Contains(text, tc.want) {
				t.Errorf("got %q (isError %v), want an error containing %q", text, isErr, tc.want)
			}
		})
	}
}

func TestListIssueEventsTool(t *testing.T) {
	text, isErr := newHarness(t).toolText("list_issue_events", `{"issue_id":7}`)
	var out struct{ Events []map[string]any }
	if isErr || json.Unmarshal([]byte(text), &out) != nil || len(out.Events) != 1 || out.Events[0]["event_id"] != "e1" {
		t.Errorf("list_issue_events: %s", text)
	}
}

func TestRepoURLIsNotLeakedThroughCleanURL(t *testing.T) {
	for _, in := range []string{"https://user:pass@github.com/a/b", "https://token@github.com/a/b"} {
		if _, err := cleanURL(in); err == nil {
			t.Errorf("%s accepted", in)
		}
	}
}
