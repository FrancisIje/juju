# Fixing juju issues with an AI agent

juju doesn't run an agent itself. It gives yours what it needs: the crash, what led up to it, the commit
the release was built from, and a place to record the fix. Your agent runs where your code already is: your
laptop or your CI.

## 1. Record each release's commit (CI)

```bash
export JUJU_URL=https://myjuju.dev JUJU_PROJECT=1 JUJU_SECRET_KEY=sk_...
juju release -name web@$VERSION   # commit and repository come from git
```

The name must be the `release` your SDK sends. Without this, an agent only knows "web@2.3.1" and may read
today's code instead of the code that crashed.

## 2. Connect your agent (MCP)

juju serves MCP at `https://<juju>/api/v1/projects/<id>/mcp`, authenticated with the project's secret key.

```bash
# Claude Code
claude mcp add --transport http juju https://myjuju.dev/api/v1/projects/1/mcp \
  --header "Authorization: Bearer $JUJU_SECRET_KEY"
```

Then ask: *"Fix the juju issue affecting the most users."* Tools it gets:

| Tool | Does |
|---|---|
| `list_issues` | Issues, filtered and sorted (default: unresolved, most recent) |
| `get_issue` | Stack trace (your frames marked), breadcrumbs, tags, context, releases with commits, linked fixes |
| `list_issue_events` | Recent raw events, to compare occurrences |
| `link_issue` | Attach the pull request to the issue |
| `update_issue` | Resolve, ignore or reopen |

The same things over plain HTTP, for scripts:

```
GET  /api/v1/projects/{id}/issues/{issue_id}
POST /api/v1/projects/{id}/issues/{issue_id}/status   {"status": "resolved"}
POST /api/v1/projects/{id}/issues/{issue_id}/links    {"url": "...", "title": "..."}
POST /api/v1/projects/{id}/releases                   {"name": "...", "commit": "...", "repository": "..."}
```

## 3. Run it unattended (GitHub Actions)

[`fix-juju-issue.yml`](fix-juju-issue.yml) runs Claude Code on a schedule, by hand, or on a
`repository_dispatch` event. It opens a pull request and links it to the issue; a person reviews, merges
and resolves.

## 4. Start it from juju's webhook

In the dashboard, *Settings → Alerts → Webhook for programs*. juju posts this JSON for each new issue and
regression:

```json
{"kind": "new_issue", "project": {"id": 1, "name": "web"},
 "issue": {"id": 42, "title": "TypeError: cart.items is undefined", "culprit": "renderCart (src/cart.js)", "url": "https://myjuju.dev/p/1/issues/42"},
 "release": "web@2.3.1", "environment": "production", "commit": "9c0ffee…", "repository": "https://github.com/acme/web",
 "sent_at": "2026-10-26T09:00:00Z"}
```

Check every call before acting on it: anyone who finds your URL can post to it.

```js
import crypto from "node:crypto";

function fromJuju(req, rawBody, secret) {
  const ts = req.headers["x-juju-timestamp"];
  if (Math.abs(Date.now() / 1000 - Number(ts)) > 300) return false; // refuse replays of old calls
  const want = "sha256=" + crypto.createHmac("sha256", secret).update(`${ts}.${rawBody}`).digest("hex");
  const got = String(req.headers["x-juju-signature"] ?? "");
  return got.length === want.length && crypto.timingSafeEqual(Buffer.from(got), Buffer.from(want));
}
```

GitHub's `repository_dispatch` needs a GitHub token in the request, which juju doesn't hold. So a small relay
(a serverless function) checks the signature above, then calls
`POST https://api.github.com/repos/<owner>/<repo>/dispatches` with
`{"event_type": "juju-issue", "client_payload": <juju's body>}`. A relay cuts down the alerts too: one agent run per
issue, only in production, only between 9 and 5.

## Limits

- The agent changes code only where it runs. juju never holds your repository or a GitHub token.
- The secret key can read and change all of a project's issues: keep it in a secret store, never in a file
  you commit.
- Webhook alerts share the 30-per-hour limit per project with Slack and email.
