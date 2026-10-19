---
name: verify
description: Write tests for and manually verify any change in the juju codebase (Go server, web / React Native / Flutter SDKs, dashboard). Use after implementing a feature or fix, before calling it done, or when the user says "verify", "test this", or "check it works".
---

# Verify a juju change

A change is done only when it passes **two kinds of proof**:

1. **Automated tests.** These stop the change from breaking later (regression protection).
2. **A manual run.** Real data goes through the real running system, so we know it works end to end. Tests only show that code does what we *think* it should. The manual run shows it does what the *user* needs.

The user is learning with this project. In every step, say **why** a test or check exists, not only what it does.

## Step 1: Work out what changed

- If the repo is under git, run `git status` and `git diff` (also `git diff --staged`). If it isn't, offer to run `git init`, because a diff is the easiest way to see what a change touched.
- Map every changed path to a component and its test tools:

| Path | Component | Test tool |
|---|---|---|
| `server/**` | Go ingest / API / worker | `go test` |
| `sdks/js-core/**`, `sdks/web/**` | Web SDK (TypeScript) | `vitest` |
| `sdks/react-native/**` | React Native SDK | `jest` |
| `sdks/flutter/**` | Flutter SDK (Dart) | `flutter test` |
| `dashboard/**` | Dashboard UI | `vitest` + manual check in a browser |
| `server/migrations/**` | Database schema | migrate up, then down, then up again |

- Write down the **behaviour** that changed in one sentence, for example: "Errors with the same normalized stack trace now group into one issue." You will test that sentence, not the lines of code.

## Step 2: Write or update tests

Write the test that would have caught the bug, or that proves the feature works. For a bug fix, run the test **before** the fix if you can and show it failing. A test that never failed proves nothing.

### Go (server)
- Put tests next to the code as `foo_test.go`, in the same package. Use **table-driven tests** (`[]struct{name string; in X; want Y}` with `t.Run(tc.name, ...)`). Why: adding a new edge case is then a single line.
- HTTP handlers: use `net/http/httptest` (`httptest.NewRecorder` or `httptest.NewServer`). Assert the status code **and** the body. Always cover these failure cases for ingest:
  - missing or wrong project key → `401`
  - malformed JSON → `400`
  - body over the size limit → `413`
  - unknown `type` → `400` (or accepted and dropped, whichever the spec says)
- Parsers (envelope decoding, stack trace normalization, source maps): add a **fuzz test** (`func FuzzX(f *testing.F)`). Why: SDKs in the wild send garbage, and the parser must never panic.
- Grouping / fingerprinting: use **golden files** in `testdata/`. Each one holds an input event and its expected fingerprint. Why: a grouping change silently splits or merges issues for every customer, so any change to the output must be deliberate and visible in the diff.
- Anything that touches Postgres or ClickHouse: write an integration test marked with `//go:build integration`. Run it against the docker compose stack. Each test creates its own project so tests never share rows.
- Don't mock what you own. Use real structs and a real database in integration tests. Mock only things outside our control (the email or Slack sender, the clock). Inject a clock through an interface so timestamps are deterministic.

### TypeScript SDKs (web, React Native)
- Test the transport with a fake `fetch`: batching, retry with backoff, dropping after N attempts, `sendBeacon` on page hide.
- Test the error hooks: throw inside a `setTimeout` and reject a promise, then assert that exactly one event was queued with the right `exception.type` and frames.
- React Native: mock `ErrorUtils.setGlobalHandler` and assert that the previous handler still gets called. Why: we must never swallow the app's own crash behaviour.

### Dart (Flutter SDK)
- `flutter test`: cover `FlutterError.onError`, `PlatformDispatcher.instance.onError`, and errors inside `runZonedGuarded`.
- Assert that the original `FlutterError.onError` is still called (same reason as for React Native).
- Test stack trace parsing with real Dart traces saved in `test/fixtures/`, including obfuscated ones.

### Migrations
- Run up → down → up on a fresh database. Why: it proves the down migration really works, so a bad deploy can be rolled back.

## Step 3: Run the automated checks

Run only what applies to the changed components, and show the real output. If something fails, say so and fix it. Never skip a failing test to get to green.

```bash
# Go
cd server
gofmt -l .                      # must print nothing
go vet ./...
go test -race -count=1 ./...    # -race catches data races in concurrent ingest code; -count=1 skips cached results
go test -race -tags=integration ./...   # only when the docker compose stack is up
command -v golangci-lint >/dev/null && golangci-lint run

# JavaScript SDKs (npm workspace in sdks/: js-core, web, later react-native)
cd sdks && npm install && npm run check   # typecheck + vitest + build
# vitest runs without type checking, so `npm run check` (which runs tsc) is required, not just `npm test`

# Flutter
cd sdks/flutter && dart format --set-exit-if-changed . && flutter analyze && flutter test
```

For the package you changed, also check coverage with `go test -cover ./internal/<pkg>/...`. Don't chase a percentage. Use the number to find branches you forgot.

## Step 4: Verify manually against the running system

Start the stack. Use whatever exists: `docker compose up -d` plus `go run ./cmd/juju`, or `make dev`. Then exercise the change the way a real SDK or user would.

### Ingest / server changes
Send a real event and confirm it landed:

```bash
KEY=<project public key>; PID=<project id>
curl -i -X POST "http://localhost:8080/api/v1/projects/$PID/events" \
  -H "Content-Type: application/json" -H "X-Juju-Key: $KEY" \
  -d '{"sdk":{"name":"juju.manual","version":"0.0.0"},"events":[{
        "event_id":"'"$(uuidgen | tr A-Z a-z)"'","type":"error","platform":"javascript",
        "timestamp":"'"$(date -u +%FT%TZ)"'","release":"web@1.0.0","environment":"dev",
        "exception":{"type":"TypeError","message":"x is undefined",
          "stacktrace":{"frames":[{"function":"render","filename":"app.js","lineno":42,"colno":7,"in_app":true}]}}}]}'
```

Then confirm the event was stored. Check through the API (`curl localhost:8080/api/v1/projects/$PID/issues`) **and** directly in the database (`psql` / `clickhouse-client`). Why both: the API could be reading from a cache or hiding a bug that the database exposes.

Always try the unhappy paths by hand too: wrong key, the same event twice (does it dedupe by `event_id`?), and the same error with a different line number (does it still group the way we intend?).

### SDK changes
- Run the example app (`examples/web`, `examples/react_native`, `examples/flutter`) pointed at the local server.
- Web: `make sdk` builds the bundle, `make example-web` serves it at http://127.0.0.1:5173/examples/web/. The page runs on a different origin from the server (port 5173 vs 8080), so CORS is exercised for real.
- Web without clicking: open `index.html?dsn=<url-encoded DSN>#selftest` in headless Chrome (`"/Applications/Google Chrome.app/Contents/MacOS/Google Chrome" --headless=new --remote-debugging-port=9333 --enable-logging=stderr --user-data-dir=<scratch dir> <url>`). It triggers every scenario on load, and `[juju]` debug lines show up in Chrome's stderr. Use `--virtual-time-budget=5000 --screenshot=<file>` instead to capture the page's own log.
- Offline → online: start the page while the juju server is **stopped** (the debug log shows growing backoff delays), then start the server. Each error must arrive exactly once: check `event_count` per issue and the row count in `events`.
- Simulated-browser (happy-dom) tests can't prove everything. For example, it doesn't call `window.onerror` for dispatched events, so check that case in real Chrome.
- Tap or click the "Throw error" / "Unhandled promise" / "Native crash" buttons.
- Open the dashboard or the API and check that the issue appears with the correct **stack frames** (file, line, function), **release**, **environment**, **device/os context**, and **breadcrumbs** leading up to the error.
- Go offline, throw, then come back online. The event should arrive once, not zero times and not twice.
- For Flutter and React Native, test a **release build** at least once. Debug builds behave differently: no obfuscation, no minification, a different error zone.

### Dashboard changes
- Open the page and click through the changed flow with real ingested data, not an empty database. Check both light and dark mode, and a narrow window.

If you cannot run something (no simulator, Docker not running), say exactly what wasn't verified and why. Never claim it works.

## Step 5: Report back

End with a short report in this shape:

```
### What changed and why
<one or two sentences in plain language: the behaviour, and the reason for the approach>

### Tests added
- <test name>: <what it protects against>

### Automated checks
<commands run + pass/fail, with the failing output if any>

### Manual verification
- <step> → <what you observed (status code, row in DB, issue in dashboard)>

### Not verified
<anything skipped, and how the user can check it>

### Concept to learn
<one idea from this change worth understanding, e.g. "why we fingerprint on in-app frames only">
```
