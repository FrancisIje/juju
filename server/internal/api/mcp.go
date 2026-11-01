package api

import (
	"bytes"
	"context"
	"encoding/json"
	"errors"
	"fmt"
	"io"
	"net/http"
	"net/url"
	"slices"
	"strings"
	"time"

	"juju/internal/event"
	"juju/internal/httpx"
	"juju/internal/store"
)

// MCP (Model Context Protocol) is how AI agents such as Claude Code or
// Cursor call outside tools. This file serves one project's issues as MCP
// tools over "Streamable HTTP": the agent POSTs a JSON-RPC 2.0 message and
// gets the JSON-RPC reply back in the response body.
//
//	POST /api/v1/projects/{id}/mcp   (Authorization: Bearer sk_...)
//
// Serving it from juju itself, rather than a program people install, means
// connecting an agent is one command with a URL and the secret key:
//
//	claude mcp add --transport http juju https://juju.example.com/api/v1/projects/1/mcp \
//	  --header "Authorization: Bearer sk_..."
//
// The server is stateless: no session id, no server-to-client stream. Each
// request carries the key and stands alone, so any API process can answer
// it and a restart loses nothing.

// Protocol versions this server speaks, newest first. On initialize the
// client names the version it wants; we agree to it if we know it, or
// offer our newest and let the client decide.
var mcpVersions = []string{"2025-11-25", "2025-06-18", "2025-03-26"}

const mcpServerName = "juju"

type rpcRequest struct {
	JSONRPC string          `json:"jsonrpc"`
	ID      json.RawMessage `json:"id,omitempty"` // absent for notifications
	Method  string          `json:"method"`
	Params  json.RawMessage `json:"params,omitempty"`
}

type rpcError struct {
	Code    int    `json:"code"`
	Message string `json:"message"`
}

// JSON-RPC error codes.
const (
	rpcParseError     = -32700
	rpcInvalidRequest = -32600
	rpcMethodNotFound = -32601
	rpcInvalidParams  = -32602
	rpcInternal       = -32603
)

func (a *API) mcp(w http.ResponseWriter, r *http.Request, projectID int64) {
	// A web page in someone's browser could otherwise post here using a
	// key it found (DNS rebinding). Agents don't send Origin; a browser
	// always does on a cross-site POST, so any Origin but our own is refused.
	if o := r.Header.Get("Origin"); o != "" && !a.sameOrigin(o) {
		httpx.WriteError(w, http.StatusForbidden, "cross-origin MCP requests are not allowed")
		return
	}
	if v := r.Header.Get("MCP-Protocol-Version"); v != "" && !slices.Contains(mcpVersions, v) {
		httpx.WriteError(w, http.StatusBadRequest, "unsupported MCP-Protocol-Version "+v)
		return
	}
	body, err := io.ReadAll(http.MaxBytesReader(w, r.Body, 64<<10))
	if err != nil {
		writeRPC(w, nil, nil, &rpcError{rpcParseError, "request body too large or unreadable"})
		return
	}
	// JSON-RPC batches (a JSON array) were dropped from MCP in 2025-06-18.
	if b := bytes.TrimSpace(body); len(b) > 0 && b[0] == '[' {
		writeRPC(w, nil, nil, &rpcError{rpcInvalidRequest, "batches are not supported"})
		return
	}
	var req rpcRequest
	if err := json.Unmarshal(body, &req); err != nil {
		writeRPC(w, nil, nil, &rpcError{rpcParseError, "invalid JSON"})
		return
	}
	if req.JSONRPC != "2.0" || req.Method == "" {
		writeRPC(w, req.ID, nil, &rpcError{rpcInvalidRequest, `expected a JSON-RPC 2.0 request with a "method"`})
		return
	}
	// A notification (no id) gets no reply, only 202 Accepted. The one
	// clients send is notifications/initialized; there's nothing to do.
	if len(req.ID) == 0 {
		w.WriteHeader(http.StatusAccepted)
		return
	}
	result, rerr := a.mcpCall(r, projectID, req)
	writeRPC(w, req.ID, result, rerr)
}

func (a *API) mcpCall(r *http.Request, projectID int64, req rpcRequest) (any, *rpcError) {
	switch req.Method {
	case "initialize":
		var p struct {
			ProtocolVersion string `json:"protocolVersion"`
		}
		_ = json.Unmarshal(req.Params, &p)
		version := mcpVersions[0]
		if slices.Contains(mcpVersions, p.ProtocolVersion) {
			version = p.ProtocolVersion
		}
		return map[string]any{
			"protocolVersion": version,
			"capabilities":    map[string]any{"tools": map[string]any{}},
			"serverInfo":      map[string]any{"name": mcpServerName, "title": "juju error tracking", "version": "1"},
			"instructions":    mcpInstructions,
		}, nil
	case "ping":
		return map[string]any{}, nil
	case "tools/list":
		return map[string]any{"tools": mcpTools}, nil
	case "tools/call":
		var p struct {
			Name      string          `json:"name"`
			Arguments json.RawMessage `json:"arguments"`
		}
		if err := json.Unmarshal(req.Params, &p); err != nil {
			return nil, &rpcError{rpcInvalidParams, "params must be {name, arguments}"}
		}
		if len(p.Arguments) == 0 || string(p.Arguments) == "null" {
			p.Arguments = json.RawMessage("{}")
		}
		text, err := a.callTool(r, projectID, p.Name, p.Arguments)
		var unknown unknownTool
		if errors.As(err, &unknown) {
			return nil, &rpcError{rpcInvalidParams, err.Error()}
		}
		// A tool that fails reports it in the result with isError, not as a
		// JSON-RPC error, so the model reads what went wrong and can retry
		// with better arguments.
		if err != nil {
			msg := "juju had an internal error; try again"
			var br badRequest
			if errors.As(err, &br) {
				msg = br.Error()
			} else if errors.Is(err, store.ErrNotFound) {
				msg = "no issue with that id in this project"
			} else {
				a.Log.Error("mcp tool", "tool", p.Name, "project_id", projectID, "err", err)
			}
			return toolResult(msg, true), nil
		}
		return toolResult(text, false), nil
	}
	return nil, &rpcError{rpcMethodNotFound, "unknown method " + req.Method}
}

func toolResult(text string, isError bool) map[string]any {
	return map[string]any{"content": []any{map[string]any{"type": "text", "text": text}}, "isError": isError}
}

func writeRPC(w http.ResponseWriter, id json.RawMessage, result any, rerr *rpcError) {
	if len(id) == 0 {
		id = json.RawMessage("null")
	}
	resp := map[string]any{"jsonrpc": "2.0", "id": id}
	if rerr != nil {
		resp["error"] = rerr
	} else {
		resp["result"] = result
	}
	httpx.WriteJSON(w, http.StatusOK, resp)
}

func (a *API) sameOrigin(origin string) bool {
	u, err := url.Parse(a.PublicURL)
	return err == nil && a.PublicURL != "" && strings.EqualFold(origin, u.Scheme+"://"+u.Host)
}

const mcpInstructions = `juju is an error tracker. These tools read one project's crashes and let you record a fix.
To fix a bug: list_issues to pick one, get_issue for its stack trace, breadcrumbs and the commit its release was built from; read the code at that commit, make the fix, then link_issue with the pull request URL. Resolve the issue (update_issue) only once the fix is merged or deployed: if it happens again after that, juju reopens it as a regression.`

// Each tool's inputSchema is JSON Schema: the agent uses it to build valid
// arguments, and the descriptions are what the model reads to choose a tool.
var mcpTools = []map[string]any{
	{
		"name":        "list_issues",
		"title":       "List issues",
		"description": "List the project's issues (errors grouped by cause), most recent first by default. Shows each issue's id, title, where it happened, how often, and any linked fix.",
		"inputSchema": object(map[string]any{
			"status":      enumProp("Only issues with this status. Default unresolved.", "unresolved", "resolved", "ignored", "all"),
			"query":       strProp("Only issues whose title or culprit contains this text."),
			"environment": strProp("Only issues seen in this environment, e.g. production."),
			"sort":        enumProp("Order. Default last_seen.", "last_seen", "first_seen", "events", "users"),
			"limit":       map[string]any{"type": "integer", "minimum": 1, "maximum": 100, "description": "How many. Default 20."},
		}),
		"annotations": map[string]any{"readOnlyHint": true, "openWorldHint": false},
	},
	{
		"name":        "get_issue",
		"title":       "Get issue",
		"description": "Everything needed to debug one issue: the latest event's stack trace (symbolicated, your own frames marked), breadcrumbs leading up to it, tags and context, the releases it happened in with the git commit each was built from, and links to fixes already under way.",
		"inputSchema": object(map[string]any{"issue_id": idProp()}, "issue_id"),
		"annotations": map[string]any{"readOnlyHint": true, "openWorldHint": false},
	},
	{
		"name":        "list_issue_events",
		"title":       "List issue events",
		"description": "Recent raw events (JSON) of one issue, newest first. Use it to compare occurrences: different users, devices, releases or inputs.",
		"inputSchema": object(map[string]any{
			"issue_id": idProp(),
			"limit":    map[string]any{"type": "integer", "minimum": 1, "maximum": 20, "description": "How many. Default 5."},
		}, "issue_id"),
		"annotations": map[string]any{"readOnlyHint": true, "openWorldHint": false},
	},
	{
		"name":        "update_issue",
		"title":       "Update issue status",
		"description": "Mark an issue resolved, ignored or unresolved. Resolve only once the fix is merged or deployed: a resolved issue that happens again comes back as a regression.",
		"inputSchema": object(map[string]any{
			"issue_id": idProp(),
			"status":   enumProp("The new status.", "resolved", "ignored", "unresolved"),
		}, "issue_id", "status"),
		"annotations": map[string]any{"readOnlyHint": false, "destructiveHint": false, "idempotentHint": true, "openWorldHint": false},
	},
	{
		"name":        "link_issue",
		"title":       "Link a fix to an issue",
		"description": "Attach a link, usually the pull request that fixes the issue, so people and other agents can see a fix is under way. Linking the same URL again updates its title.",
		"inputSchema": object(map[string]any{
			"issue_id": idProp(),
			"url":      map[string]any{"type": "string", "format": "uri", "description": "An http(s) URL, e.g. https://github.com/acme/web/pull/42"},
			"title":    strProp("A short label, e.g. the pull request title."),
		}, "issue_id", "url"),
		"annotations": map[string]any{"readOnlyHint": false, "destructiveHint": false, "idempotentHint": true, "openWorldHint": false},
	},
}

func object(props map[string]any, required ...string) map[string]any {
	o := map[string]any{"type": "object", "properties": props, "additionalProperties": false}
	if len(required) > 0 {
		o["required"] = required
	}
	return o
}

func strProp(desc string) map[string]any {
	return map[string]any{"type": "string", "description": desc}
}

func idProp() map[string]any {
	return map[string]any{"type": "integer", "minimum": 1, "description": "The issue id, as list_issues shows it."}
}

func enumProp(desc string, values ...string) map[string]any {
	return map[string]any{"type": "string", "enum": values, "description": desc}
}

type unknownTool string

func (u unknownTool) Error() string { return "unknown tool " + string(u) }

func (a *API) callTool(r *http.Request, projectID int64, name string, raw json.RawMessage) (string, error) {
	var args struct {
		IssueID     int64  `json:"issue_id"`
		Status      string `json:"status"`
		Query       string `json:"query"`
		Environment string `json:"environment"`
		Sort        string `json:"sort"`
		Limit       int    `json:"limit"`
		URL         string `json:"url"`
		Title       string `json:"title"`
	}
	if err := json.Unmarshal(raw, &args); err != nil {
		return "", badRequest("invalid arguments: " + err.Error())
	}
	needIssue := func() error {
		if args.IssueID <= 0 {
			return badRequest("issue_id is required")
		}
		return nil
	}
	ctx := r.Context()
	switch name {
	case "list_issues":
		return a.toolListIssues(ctx, projectID, args.Status, args.Query, args.Environment, args.Sort, args.Limit)
	case "get_issue":
		if err := needIssue(); err != nil {
			return "", err
		}
		return a.toolGetIssue(ctx, projectID, args.IssueID)
	case "list_issue_events":
		if err := needIssue(); err != nil {
			return "", err
		}
		n := args.Limit
		if n < 1 {
			n = 5
		}
		events, err := a.Store.IssueEvents(ctx, projectID, args.IssueID, time.Time{}, min(n, 20))
		if err != nil {
			return "", err
		}
		if len(events) == 0 {
			// Distinguish "no events" from "no such issue".
			if _, err := a.Store.IssueDetail(ctx, projectID, args.IssueID); err != nil {
				return "", err
			}
		}
		out, _ := json.MarshalIndent(map[string]any{"events": events}, "", "  ")
		return string(out), nil
	case "update_issue":
		if err := needIssue(); err != nil {
			return "", err
		}
		changed, err := a.changeStatus(r, projectID, args.IssueID, args.Status)
		if err != nil {
			return "", err
		}
		if !changed {
			return fmt.Sprintf("Issue %d was already %s.", args.IssueID, args.Status), nil
		}
		return fmt.Sprintf("Issue %d is now %s.", args.IssueID, args.Status), nil
	case "link_issue":
		if err := needIssue(); err != nil {
			return "", err
		}
		l, err := a.linkIssue(r, projectID, args.IssueID, args.URL, args.Title)
		if err != nil {
			return "", err
		}
		return fmt.Sprintf("Linked %s to issue %d.", l.URL, args.IssueID), nil
	}
	return "", unknownTool(name)
}

func (a *API) toolListIssues(ctx context.Context, projectID int64, status, query, env, sort string, limit int) (string, error) {
	switch status {
	case "":
		status = "unresolved"
	case "all":
		status = ""
	case "unresolved", "resolved", "ignored":
	default:
		return "", badRequest("status must be unresolved, resolved, ignored or all")
	}
	if limit < 1 {
		limit = 20
	}
	list, total, err := a.Store.SearchIssues(ctx, projectID, store.IssueFilter{Status: status, Query: query, Environment: env, Sort: sort, Limit: min(limit, 100)})
	if err != nil {
		return "", err
	}
	if len(list) == 0 {
		return "No matching issues.", nil
	}
	var b strings.Builder
	fmt.Fprintf(&b, "%d of %d matching issues:\n", len(list), total)
	for _, x := range list {
		fmt.Fprintf(&b, "\n#%d [%s", x.ID, x.Status)
		if x.Regressed {
			b.WriteString(", regressed")
		}
		fmt.Fprintf(&b, "] %s\n", x.Title)
		if x.Culprit != "" {
			fmt.Fprintf(&b, "   in %s\n", x.Culprit)
		}
		fmt.Fprintf(&b, "   %d events, %d users, last seen %s", x.EventCount, x.UserCount, x.LastSeen.UTC().Format(time.RFC3339))
		if x.LastRelease != "" {
			fmt.Fprintf(&b, ", release %s", x.LastRelease)
		}
		b.WriteString("\n")
	}
	return b.String(), nil
}

func (a *API) toolGetIssue(ctx context.Context, projectID, issueID int64) (string, error) {
	d, err := a.Store.IssueDetail(ctx, projectID, issueID)
	if err != nil {
		return "", err
	}
	events, err := a.Store.IssueEvents(ctx, projectID, issueID, time.Time{}, 1)
	if err != nil {
		return "", err
	}
	var b strings.Builder
	fmt.Fprintf(&b, "# Issue %d: %s\n\n", d.ID, d.Title)
	status := d.Status
	if d.Regressed {
		status += " (regressed: it came back after being resolved)"
	}
	fmt.Fprintf(&b, "Status: %s\n", status)
	fmt.Fprintf(&b, "Events: %d, users affected: %d\n", d.EventCount, d.UserCount)
	fmt.Fprintf(&b, "First seen %s, last seen %s\n", d.FirstSeen.UTC().Format(time.RFC3339), d.LastSeen.UTC().Format(time.RFC3339))
	if d.Culprit != "" {
		fmt.Fprintf(&b, "Culprit: %s\n", d.Culprit)
	}
	if d.Platform != "" {
		fmt.Fprintf(&b, "Platform: %s\n", d.Platform)
	}
	if len(d.Environments) > 0 {
		fmt.Fprintf(&b, "Environments: %s\n", strings.Join(d.Environments, ", "))
	}
	if a.PublicURL != "" {
		fmt.Fprintf(&b, "Dashboard: %s/p/%d/issues/%d\n", strings.TrimRight(a.PublicURL, "/"), projectID, d.ID)
	}

	if len(d.Releases) > 0 {
		b.WriteString("\n## Releases (newest first)\n")
		commits := map[string]store.Release{}
		for _, c := range d.Commits {
			commits[c.Name] = c
		}
		for _, rel := range d.Releases {
			c, ok := commits[rel]
			switch {
			case ok && c.Commit != "":
				fmt.Fprintf(&b, "- %s: commit %s", rel, c.Commit)
				if c.Repository != "" {
					fmt.Fprintf(&b, " in %s", c.Repository)
				}
				b.WriteString("\n")
			default:
				fmt.Fprintf(&b, "- %s: no commit recorded (run `juju release` in CI to record it)\n", rel)
			}
		}
	}
	if len(d.Links) > 0 {
		b.WriteString("\n## Linked fixes\n")
		for _, l := range d.Links {
			if l.Title != "" {
				fmt.Fprintf(&b, "- %s: %s\n", l.Title, l.URL)
			} else {
				fmt.Fprintf(&b, "- %s\n", l.URL)
			}
		}
	}

	if len(events) == 0 {
		b.WriteString("\nNo stored events (they may have passed the retention period).\n")
		return b.String(), nil
	}
	var e event.Event
	if err := json.Unmarshal(events[0], &e); err != nil {
		return "", err
	}
	writeEvent(&b, &e)
	return b.String(), nil
}

// writeEvent renders an event the way a developer reads a crash report:
// the error, the stack with their own frames marked, then what led up to it.
func writeEvent(b *strings.Builder, e *event.Event) {
	fmt.Fprintf(b, "\n## Latest event %s\n", e.EventID)
	fmt.Fprintf(b, "At %s", e.Timestamp)
	if e.Release != "" {
		fmt.Fprintf(b, ", release %s", e.Release)
	}
	if e.Environment != "" {
		fmt.Fprintf(b, ", environment %s", e.Environment)
	}
	b.WriteString("\n")
	if x := e.Exception; x != nil {
		fmt.Fprintf(b, "\n%s: %s\n", x.Type, x.Message)
		if x.Stacktrace != nil && len(x.Stacktrace.Frames) > 0 {
			b.WriteString("\nStack trace, most recent call first (* = your code):\n")
			for i, f := range x.Stacktrace.Frames {
				if i == 50 {
					fmt.Fprintf(b, "  … %d more frames\n", len(x.Stacktrace.Frames)-i)
					break
				}
				mark := " "
				if f.InApp {
					mark = "*"
				}
				fn := f.Function
				if fn == "" {
					fn = "?"
				}
				fmt.Fprintf(b, "%s %s  %s\n", mark, fn, location(f.Filename, f.Lineno, f.Colno))
			}
		}
	}
	if s := e.Symbolication; s != nil {
		fmt.Fprintf(b, "\nSymbolication: %s", s.Status)
		if len(s.Notes) > 0 {
			fmt.Fprintf(b, " (%s)", strings.Join(s.Notes, "; "))
		}
		b.WriteString("\n")
	}
	if n := len(e.Breadcrumbs); n > 0 {
		b.WriteString("\nBreadcrumbs, oldest first (what happened just before):\n")
		for _, c := range e.Breadcrumbs[max(0, n-30):] {
			line := c.Message
			if len(c.Data) > 0 {
				data, _ := json.Marshal(c.Data)
				line += " " + string(data)
			}
			b.WriteString("- ")
			if c.Timestamp != "" {
				b.WriteString(c.Timestamp + " ")
			}
			fmt.Fprintf(b, "[%s] %s\n", c.Type, strings.TrimSpace(line))
		}
	}
	if len(e.Tags) > 0 {
		keys := make([]string, 0, len(e.Tags))
		for k := range e.Tags {
			keys = append(keys, k)
		}
		slices.Sort(keys)
		b.WriteString("\nTags:\n")
		for _, k := range keys {
			fmt.Fprintf(b, "- %s: %s\n", k, e.Tags[k])
		}
	}
	if len(e.Context) > 0 {
		ctx, _ := json.MarshalIndent(e.Context, "", "  ")
		fmt.Fprintf(b, "\nContext:\n%s\n", ctx)
	}
}

func location(file string, line, col int) string {
	if file == "" {
		return "(unknown file)"
	}
	switch {
	case line > 0 && col > 0:
		return fmt.Sprintf("%s:%d:%d", file, line, col)
	case line > 0:
		return fmt.Sprintf("%s:%d", file, line)
	}
	return file
}
