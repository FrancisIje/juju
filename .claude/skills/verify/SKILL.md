---
name: verify
description: Write tests for and manually verify any change in the juju codebase (Go server, web / React Native / Flutter SDKs, dashboard), then document key changes as a published docs page (artifact) and open a pull request from a new branch to main. Use after implementing a feature or fix, before calling it done, or when the user says "verify", "test this", or "check it works".
---

# Verify a juju change

A change is done only when it passes **two kinds of proof**:

1. **Automated tests.** These stop the change from breaking later (regression protection).
2. **A manual run.** Real data goes through the real running system, so we know it works end to end. Tests only show that code does what we *think* it should. The manual run shows it does what the *user* needs.

The user is learning with this project. In every step, say **why** a test or check exists, not only what it does.

Two rules for every change:

- **Every fix is verified manually, before and after.** Reproduce the bug in the running system and record what goes wrong; fix it; rebuild; run the *same* steps again and record that it's gone (Step 4, "Bug fixes"). A passing test alone doesn't close a fix: a test can pass for the wrong reason, and only the before/after run shows the fix changed what the user sees. This applies to each fix, including the small ones found along the way.
- **Finished work goes out as a pull request** from a new branch to `main` (Step 6), never as a push to `main`. The user reviews and merges.

## Step 1: Work out what changed

- If the repo is under git, run `git status` and `git diff` (also `git diff --staged`). If it isn't, offer to run `git init`, because a diff is the easiest way to see what a change touched.
- Map every changed path to a component and its test tools:

| Path | Component | Test tool |
|---|---|---|
| `server/**` | Go ingest / API / worker | `go test` |
| `sdks/js-core/**`, `sdks/web/**` | Web SDK (TypeScript) | `vitest` |
| `sdks/react-native/**` | React Native SDK | `vitest` (with a fake `react-native` module) |
| `sdks/flutter/**` | Flutter SDK (Dart) | `flutter test` (+ `fake_async`) |
| `dashboard/**` | Dashboard UI | `vitest` + manual check in a browser |
| `server/migrations/**` | Database schema | migrate up, then down, then up again |
| `server/internal/analytics/**` | Product analytics (ClickHouse) | `go test -tags=integration` against a real ClickHouse (`internal/testch` starts one) |
| `server/internal/symbolicate/**` | Source maps, Dart DWARF | `go test` against real fixtures in `testdata/` |

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
- React Native: fake `ErrorUtils` and assert that the previous handler still gets called. Why: we must never swallow the app's own crash behaviour. For fatal errors, also assert it's called only after the queue is saved, and within the timeout if storage hangs.

### Dart (Flutter SDK)
- `flutter test`: cover `FlutterError.onError`, `PlatformDispatcher.instance.onError`, and errors inside `Juju.runZoned` (a `runZonedGuarded`).
- Assert that the original `FlutterError.onError` is still called (same reason as for React Native).
- Test stack trace parsing with real Dart traces saved in `test/fixtures/`, including non-symbolic ones. Generate real ones instead of typing them: `dart run` gives a symbolic trace; `dart compile exe --extra-gen-snapshot-options=--dwarf-stack-traces` gives a non-symbolic one.
- Install hooks on `PlatformDispatcher.instance`, not `binding.platformDispatcher`: flutter_test's `TestPlatformDispatcher` silently ignores writes to `onError`.
- Timers: use `fake_async`, and `clock.now()` (package:clock) instead of `DateTime.now()` so it can be faked.

### Symbolication
- Test against **real** files, never hand-written ones: an esbuild-minified bundle with its map, and Dart debug files (Mach-O from `dart compile aot-snapshot`, ELF from a Flutter Android build). `testdata/README.md` says how each was made.
- For Dart, the answer key is Dart's own decoder: `dart run native_stack_traces:decode translate -d <debug file> -i <trace>`. Our frames must match it exactly (functions, files, lines, columns, inlined frames, hidden artificial frames).
- Normal inputs hide off-by-one bugs (a source map segment spans a whole token; most return addresses sit mid-line). Test the edges on purpose: a map whose adjacent columns map to different lines, and an address where a new line starts.
- Workers share parsed files: run with `-race` and a concurrent test.

### Dashboard (UI)
- Unit tests live in `dashboard/test/` (Vitest + Testing Library + happy-dom). Fake the server with `fakeServer()` from `test/helpers.tsx`, and make it **stateful** when the page reloads after a change (a resolved issue must disappear from the next list fetch, as on the real server), or the test checks the fake instead of the page.
- Test what a person does, by role and label (`getByRole("tab", { name: "Flutter" })`, `getByLabelText("Search issues")`), not CSS classes. Why: it's how people (and screen readers) find things, and it survives restyling.
- Every keyboard shortcut needs a test that it works **and** one that it does not fire while typing in a field ("r" in the search box is a letter, not "resolve").
- Requests that change data must carry the CSRF header: assert it on the recorded call.
- End-to-end: `dashboard/e2e/dashboard.spec.ts` (Playwright, the system Chrome via `channel: "chrome"`) runs first-run setup → project → errors → resolve → regression → Slack, against a **fresh** server. Extend it when you add a flow. `dashboard/e2e/README.md` says how to run it.

### Migrations
- Run up → down → up on a fresh database. Why: it proves the down migration really works, so a bad deploy can be rolled back.
- An index on a big table (`events`) is built with `CREATE INDEX CONCURRENTLY` in its own `-- +goose NO TRANSACTION` file. A plain `CREATE INDEX` blocks writes while it builds, so the deploy would stall ingest.

### Queue and workers
- Anything that goes through the queue is JSON. A field marked `json:"-"` is lost on the way: rebuild it after the queue (`Event.RestoreParsed`) and extend `TestQueueRoundTrip`. The in-memory test queue (`queue.Chan`) passes Go objects and hides this, so run the integration suite too: it uses real JetStream.
- A worker returns an error to retry. It never sleeps or loops by itself: the queue owns retries, backoff and the dead-letter stream.

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

# Dashboard
cd dashboard && npm run check   # tsc (also type-checks the tests) + vitest + vite build
make dashboard                  # embed the build in the server...
cd server && go build ./cmd/juju   # ...which only takes effect after rebuilding the Go binary (go:embed runs at compile time)
cd dashboard && JUJU_E2E_SLACK=<fake slack url> JUJU_E2E_SHOTS=<dir> npm run e2e   # against a fresh server
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
- React Native (`examples/react-native`, see its README):
  - `make example-rn-deps` after any SDK change. The example installs the SDK from `npm pack` tarballs, so it tests what users get.
  - Build **release** builds. iOS: `xcodebuild -workspace ios/JujuExample.xcworkspace -scheme JujuExample -configuration Release -sdk iphonesimulator -derivedDataPath ios/build`, then `xcrun simctl install`/`launch`. Android: `JAVA_HOME="/Applications/Android Studio.app/Contents/jbr/Contents/Home" ANDROID_HOME=~/Library/Android/sdk ./gradlew assembleRelease`, then boot `emulator -avd Pixel_10_Pro -no-window`, `adb reverse tcp:8080 tcp:8080` and `tcp:8098`, `adb install -r`.
  - Gradle doesn't notice SDK changes inside `node_modules`: force the JS step with `./gradlew :app:createBundleReleaseJsAndAssets --rerun :app:assembleRelease`, and check the APK's timestamp. A "successful" build that didn't rebuild proves nothing.
  - Drive scenarios with the control file (`config.json` on port 8098): `nonfatal`, then the **crash test**: stop the juju server, launch with `crash` (the process must die; iOS writes a `.ips` report in `~/Library/Logs/DiagnosticReports`, Android logs `FATAL EXCEPTION`), start the server, relaunch with `none`. The crash must arrive exactly once, with the same `event_id` that was saved.
- Flutter (`examples/flutter`, uses the SDK by path):
  - **Release mode only runs on the Android emulator**: Flutter has no release mode on the iOS simulator. `JAVA_HOME="/Applications/Android Studio.app/Contents/jbr/Contents/Home" flutter build apk --release`, then `adb install -r build/app/outputs/flutter-apk/app-release.apk`. iOS: `flutter build ios --simulator --debug`, then `xcrun simctl install`/`launch`.
  - Same control file as React Native; scenario `all` triggers framework, async (root zone), zone and handled errors. Each event must carry its `tags.mechanism` (`flutter_error`, `platform_dispatcher`, `zone`, `manual`).
  - Release builds need `<uses-permission android:name="android.permission.INTERNET"/>` in `android/app/src/main/AndroidManifest.xml`; the template only adds it to debug and profile, so a release app silently has no network.
  - Offline test: stop the server, launch with `all`, wait for `[juju] send failed` in logcat, `adb shell am force-stop`, start the server, relaunch with `none`. Expect `[juju] restored N event(s)` and N new events, all distinct ids. On iOS, the queue file is readable at `<app data container>/Library/Application Support/juju_queue.json`.
  - iOS lets you read the saved queue directly: `xcrun simctl get_app_container <sim> <bundle id> data`, then `Library/Application Support/<bundle id>/RCTAsyncLocalStorage_V1/manifest.json`, key `@juju/queue`. Android release builds aren't debuggable, so relaunching is the proof there.
- Tap or click the "Throw error" / "Unhandled promise" / "Native crash" buttons.
- Open the dashboard or the API and check that the issue appears with the correct **stack frames** (file, line, function), **release**, **environment**, **device/os context**, and **breadcrumbs** leading up to the error.
- Go offline, throw, then come back online. The event should arrive once, not zero times and not twice.
- For Flutter and React Native, test a **release build** at least once. Debug builds behave differently: no obfuscation, no minification, a different error zone.

### Symbolication changes
- Use one project and send errors **before** uploading anything: frames stay minified and `payload->'symbolication'` names exactly what's missing.
- Upload with the CLI: `juju upload -kind sourcemap -release <the SDK's release> <dir>` (web: `make example-web-build` then `examples/web/dist`; React Native Android: `android/app/build/generated/sourcemaps/react/release/index.android.bundle.map`) or `juju upload -kind dart_symbols build/symbols` (Flutter: `flutter build apk --release --obfuscate --split-debug-info=build/symbols`).
- Wait 30 s (the "not uploaded" answer is cached that long) and send again: status `symbolicated`, real file:line, `raw` holds the original frame, in_app correct (node_modules, Dart SDK and Flutter frames are not app code).
- Grouping must survive a new build: change the code above the bug (lines shift, new minified names or build id), upload the new build's file, and check the error joins the **same** issue.

### Ingest durability, limits and privacy (server/internal/queue, ingest, worker)
- Tests must not be the only proof that "202 means stored". Flood the server, `kill -9` it mid-flood, start it again, and compare every event id that got a 202 with the ids in `events`: none missing, none twice. Do the same with Postgres stopped for longer than the 30 s project cache: ingest must keep answering 202, and the event must be stored once Postgres is back.
- Latency: `make loadtest` (k6, constant arrival rate) against a project with `juju project limits -id N -rate 0`, or the default rate limit turns the test into 429s. Report p99 *and* whether the workers kept up: the queue directory's size after the run, and the stored count against the accepted count. Note the machine's load average: k6, juju and Postgres share one laptop.
- PII: send an event with an email, a `?token=` URL and an `Authorization` header, then check the stored payload **and** the queue's files on disk (`grep -r -a` in `JUJU_DATA_DIR/queue`, while Postgres is stopped so the event stays queued). Scrubbing must happen before the queue.
- Limits: `juju project limits` on a test project, then send bursts: `429` with a `Retry-After` the SDKs can obey, and usage that survives a restart.

### Security-sensitive changes (auth, limits, anything that sends requests or mail)
Sign-up is open, so treat every account as a possible attacker. Attack the change by hand with two accounts in two organizations, through a client that sends `X-Forwarded-For` from `127.0.0.1` the way Caddy does:
- **Authorization**: the other account's ids in every path, and your own project in the path with *their* issue, event or invite id in it. Every one must be 404, or 200 with nothing of theirs in it.
- **Limits** must key on the client's address from `httpx.ClientIP`, never on the email or project alone: anything keyed on a value the attacker can type lets them lock someone else out. Also check that a forged `X-Forwarded-For` from a direct, non-proxy connection is ignored.
- **Outward requests** (webhooks, email): a listener on `127.0.0.1` must see zero connections for private IPs, hostnames resolving to them (`127.0.0.1.nip.io`), decimal and hex IPs, and redirects. Sending must be rate limited.
- **XSS**: send HTML and `javascript:` URLs in every event field, open the pages in real Chrome, and check for dialogs, CSP violations and injected elements.
- `go run golang.org/x/vuln/cmd/govulncheck@v1.1.4 ./...` (CI runs it) and `npm audit --omit=dev`.

### Analytics changes
- Run ClickHouse with `make devch` (or `scripts/devch.sh <dir>`; a pinned, checksum-checked build) and start juju with `JUJU_CLICKHOUSE_URL=http://127.0.0.1:8123`. The log must say `analytics on`.
- Send events the way each SDK does: the web example's `#analytics-selftest`, and the Flutter and React Native examples' `analytics` scenario (control server on 8098, release builds on the emulator). Each sends a known set, so the numbers can be checked exactly.
- Check the rows in ClickHouse itself (`curl 'http://127.0.0.1:8123/?query=SELECT ... FROM <db>.events FORMAT PrettyCompactMonoBlock'`), then the same numbers in the dashboard's Analytics page: a Platform breakdown must equal what the apps sent, and People must merge a device's anonymous and logged-in events into one.
- Queries must work whatever timezone the ClickHouse server is in (the client sets `session_timezone=UTC`). Test on a machine that isn't on UTC, or trends silently shift a day.

### Dashboard changes
- Run `make dashboard`, **rebuild the Go binary**, and confirm the served bundle is the new one: the `index-<hash>.js` name in `curl localhost:8080/` must match `dashboard/dist/assets/`. Otherwise you're testing the old UI.
- Click through the changed flow with real ingested data (send events with the curl above or the e2e spec), not an empty database. Then check the empty, loading and error states on purpose (stop the server to see the error state).
- **Look at the screenshots** (`JUJU_E2E_SHOTS`): every changed page in dark and light, at desktop (1360) and phone (390) width. Read them like a user: clipped text, low contrast, gaps in grids, text cut after a few letters on a phone. Screenshots must be taken with `animations: "disabled"` after layout settles, or a theme switch is captured halfway and looks like a contrast bug.
- With alerts: point the Slack webhook at a local fake (`JUJU_ALLOW_HTTP_WEBHOOKS=true`, a tiny HTTP server that logs POST bodies) and read the payloads. Offer the user a test against their real Slack webhook too.

### UI rules (check every dashboard change against these)
The look follows fal.ai's product UI, with juju's own identity (no fal.ai logo, names or wording):
- **Colors only from `dashboard/src/styles/tokens.css`**: `var(--surface)`, `var(--line)`, `var(--brand)`… never a hex value in a component or in `app.css`. A new color is a new token, defined for dark **and** light. Dark is the default: near-black zinc surfaces, hairline borders (white at 5–10 %), white primary button on dark (dark on light), violet brand for focus and selection, 4–6 px radii, small colored chips. The pixel-art palette is for the login screen and empty states only.
- **Fonts**: Public Sans for text, Chivo Mono for code, ids, counts and file paths. Both are bundled (the CSP blocks font CDNs).
- **Status is a word plus a color, never color alone** (`Unresolved`, `Resolved`, `Ignored`, `Regressed`), so it reads for color-blind people.
- **Easy for newer developers**: plain-language labels ("A resolved issue happens again", not "regression trigger"), a sentence under each setting saying what it does, onboarding with copy-paste code and the DSN filled in, empty states that say what to do next, server errors shown in words with a Retry.
- **Fast for experienced developers**: every common action has a keyboard shortcut shown next to it (`<kbd>`), listed in the `?` dialog (`ShortcutsHelp.tsx`); ⌘K reaches every page; filters live in the URL so views can be shared; raw JSON is one tab away. Shortcuts never fire while typing, and Esc in a dialog closes only the dialog.
- **Honest copy**: say exactly what the server knows. (Example: symbolication status `missing` means "no source maps for this release", not "frames are minified"; the code may never have been minified.)
- **Works at 390 px**: no horizontal page scroll (wide tables and tab rows scroll inside their box), and the important text wraps instead of being cut. Measure it, don't eyeball it: the e2e `shoot()` checks that neither the document nor `.main` (the app's own scroll area, which hides overflow from a document-only check) is wider than the screen. Long single-line content (DSNs, invite links) in a grid needs `min-width: 0` on its container.
- Secrets: the secret key is shown once and kept only in memory (`lib/secrets.ts`), never in browser storage. The webhook URL is never sent back to the browser.

### Bug fixes: before and after
For every fix, including small ones found while testing something else:
1. **Reproduce it first**, against the running system with the bug still in, using the steps a user would take (a Playwright script for the dashboard, curl for the API). Write down what you observed: the wrong output, status code, screenshot, or measurement.
2. **Find the real cause** before fixing. If a first guess doesn't explain what you saw, keep looking: a fix for the wrong cause can still make a test pass.
3. **Fix, then rebuild what's being run** (`make dashboard`, then rebuild the Go binary, since it embeds the UI at compile time) and confirm the new build is the one being served, e.g. the `index-<hash>.js` name in `curl localhost:8080/`.
4. **Run the same steps again** and record that the behaviour is now right. Keep the script: it usually becomes a test.
5. **Check the test guards the fix**: put the bug back (a mutation) and confirm the test fails, then restore it. First make sure the mutant **compiles** (`go vet`, `tsc --noEmit`, `flutter analyze`): a mutant that doesn't compile always "fails", which proves nothing. And when a mutant survives, ask whether the test reproduces the real situation (a retry is a separate request; a phone chart is 350px wide) before deciding the code is fine.
If you can't reproduce a reported bug, say so and say what you tried; don't call it fixed. If it only happens somewhere you can't reach (the user's server, a device you don't have), say exactly that and how the user can confirm.

If you cannot run something (no simulator, Docker not running), say exactly what wasn't verified and why. Never claim it works.

### When another session works on the same repo
Check `ListAgents` and `git status` first: another session may be on another branch in the same folder.
- Work in your **own git worktree** (`git worktree add ../juju-<topic> <branch>`), never in a folder another session uses: one checkout shared by two sessions mixes their files and commits.
- Use **your own ports** for anything you run (Postgres `go run ./cmd/devdb -port 5433`, juju `JUJU_ADDR=:8090`, fake Slack, ClickHouse `CH_HTTP_PORT`), and say which ones you use.
- **Stop processes by port** (`kill $(lsof -t -iTCP:<port> -sTCP:LISTEN)`), never by name: `pkill -f devdb` also kills the other session's database.
- Shared devices (the Android emulator, port 8098, `adb reverse`) are one at a time: ask before taking them.
- Commit only your own changes; leave files another session touched (or a downloaded binary in its folder) out, and move your downloads out of shared folders.
- Integration test ports are fixed per package; if one is busy, wait until it's free and rerun rather than calling the test broken.

## Step 5: Document key changes as an artifact

A **key change** gets a docs page: a finished roadmap phase, a new SDK or platform, a new server
capability (an endpoint, a pipeline stage), or a change to how something core works (event format,
grouping, delivery guarantees). Bug fixes, refactors and small tweaks don't get their own page; if
they change something an existing page says, update that page instead.

### Create or update?
- Pages live in `docs/_src/<name>.html`, one per subject: `roadmap.html` (front page), `web-sdk.html`,
  `react-native-sdk.html`, `flutter-sdk.html`, `symbolication.html`, …
- If the subject already has a page, **update it**: edit the source, then republish. In the session
  that published it, publish the same file path again. In a later session, pass the page's existing
  artifact URL as `url` (after reading it with `action: "read"`), so the link people already have keeps
  working. The URLs are in Claude's memory (`juju-docs-artifacts`); `Artifact` `action: "list"` finds them too.
- Otherwise create a new page, and add its URL to that memory file.
- Always update `roadmap.html` as well: the phase's pill (Done / Next / Planned), the log line at the
  top, and a link from the phase's card to the page (`https://francisije.github.io/juju/<name>.html`).

### What the page contains
Write for someone learning how juju works, using what you just verified, not what you planned:
1. **The problem** in one or two sentences, with a real example (a real minified frame, a real crash).
2. **How it works**: the few decisions that matter, each with its reason. One diagram or one small
   interactive piece where it explains something text can't (a before/after toggle, a picker).
3. **Using it**: the setup code a developer copies.
4. **Verification**: a table of what was checked, on what (platform, build type), with the real
   evidence (event ids, line numbers, counts). List the bugs the devices or real files exposed.
5. **Limits**: what isn't covered yet, plainly.

### How it looks
Reuse the existing pages' system so the docs read as one set: copy the `:root` tokens (light and dark),
fonts (Familjen Grotesk, Atkinson Hyperlegible, JetBrains Mono) and component styles (`.panel`, `.why`,
`.fixes`, tables, `.logline`) from the most recent page. Same header pattern: eyebrow
`juju · Phase N · <subject>`, an h1 that states the idea, a lede, and a log line. Title tag
`juju <Subject>`; link back to the roadmap and the related pages in the footer.

### Publish and build
1. Run the Artifact tool's `quickstart` first for a new page, and follow its page contract (no
   `<html>`/`<head>`/`<body>` tags in the source: the viewer adds them; `make docs` adds them for Pages).
2. Publish the page with a one-word `icon` (new pages only) and a one-sentence `description`.
3. `make docs` to rebuild the GitHub Pages copies in `docs/`, check the generated HTML is well formed,
   and `node --check` any inline script.
4. Commit the source, the generated page and the roadmap together.

## Step 6: Open a pull request

When the checks pass and every fix has its before/after run, ship the work as a pull request. Why: `main` is what deploys and what others build on, so changes reach it only after a review, with CI run on exactly what will merge.

1. **Look at what you're about to commit**: `git status`, then `git diff`. Commit only this change's files: not unrelated edits, generated build output (`server/internal/web/dist/` assets, `dashboard/dist/`, `.build/`), or anything secret (`sk_` keys, `.env` files, webhook URLs). If another session or the user changed files you didn't touch, leave them out and mention them.
2. **Make a new branch** named for the change: `fix/<what>` for fixes, `feat/<what>` for features (e.g. `fix/login-next-redirect`). Start it from up-to-date `main` (`git fetch origin` then `git switch -c <branch> origin/main`). If the commits were already made on local `main`, run `git switch -c <branch>` to take them along, then point local `main` back at `origin/main` with `git branch -f main origin/main`, but only when every commit on local `main` is on the new branch.
3. **Commit** with a message that says what changed and why, in a few lines.
4. **Push the branch**: `git push -u origin <branch>`. Never push to `main`, and never force-push.
5. **Open the PR** to `main`: `gh pr create --base main --head <branch> --title "<short summary>" --body-file <file>`. The body follows Step 7's report: what changed and why, tests added, automated checks, **manual verification (before and after for each fix)**, and what wasn't verified.
6. **Wait for CI** (`gh pr checks <number> --watch`). Remember that main is the production branch: **merging deploys to the live server**, so say in the PR what the merge will ship (including anything already on main that isn't deployed yet) and anything the server needs first (a re-run of `deploy/deploy.sh setup`, a new env var). If a check fails, read the log (`gh run view --log-failed`), fix it on the same branch, push again, and say what failed. Don't merge the PR yourself; the user does that after reviewing.
If there's no git remote, or `gh` isn't logged in (`gh auth status`), stop at the local branch and tell the user what's needed.

## Step 7: Report back

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

### Docs
<the artifact link, created or updated, or "no key change, no page">

### Pull request
<the PR link, its branch, and CI status: passed / failed (what) / still running>
```
