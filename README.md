# juju

Error tracking and product analytics for Flutter, React Native and web apps, with a Go backend.
Roadmap: https://francisije.github.io/juju/ (source: [docs/_src/roadmap.html](docs/_src/roadmap.html); run `make docs` after editing it).

## Run it locally

```bash
make devdb          # terminal 1: Postgres without Docker (or `make db` if you have Docker)
make dev            # terminal 2: the server on :8080
```

For product analytics, also run `make devch` (ClickHouse without Docker; it downloads a pinned build
once) and start the server with `JUJU_CLICKHOUSE_URL=http://127.0.0.1:8123 make dev`. Without it,
error tracking works as usual and the Analytics page says how to turn it on.

Create a project and send an error:

```bash
cd server && go run ./cmd/juju project create -org acme -name shop-app
# prints id, public_key (for SDKs) and secret_key (for reading; shown once)

curl -X POST localhost:8080/api/v1/projects/1/events \
  -H "Content-Type: application/json" -H "X-Juju-Key: <public_key>" \
  -d '{"sdk":{"name":"curl"},"events":[{"event_id":"'"$(uuidgen)"'","type":"error",
       "platform":"javascript","exception":{"type":"TypeError","message":"x is undefined"}}]}'

curl localhost:8080/api/v1/projects/1/issues -H "Authorization: Bearer <secret_key>"
```

## Dashboard

```bash
make dashboard      # build the UI and embed it in the server binary
make dev            # then open http://localhost:8080
```

Anyone can create an account at `/signup` (linked from the login page) and gets their own
organization. To bring teammates into yours, open **Settings → Team → Create invite link** and send
the link: it works once, for 7 days, and puts them in your organization. Owners invite and remove
people; members do everything else. Sign-up never joins an existing organization by name.

For a private server, set `JUJU_ALLOW_SIGNUP=false`: then invite links are the only way in. Admins can
also add people from the shell: `go run ./cmd/juju user create -email ada@example.com -org Acme`
(on a VPS: `juju-admin user create …`). New projects show their
DSN and setup code for each SDK; the secret key is shown once.

To watch every organization and project on your server, make your account a super admin from the
server's shell, then open **Super admin** in the sidebar (`/admin`):

```bash
juju-admin user superadmin -email you@example.com          # grant (go run ./cmd/juju … locally)
juju-admin user superadmin                                 # list who has it
juju-admin user superadmin -email you@example.com -revoke  # take it away, effective at once
```

It shows counts and each organization's members. It doesn't show issue details or keys, and it
doesn't let you into a project unless you belong to its organization.

- Keyboard first: `⌘K` jumps anywhere, `j`/`k` move, `r`/`i`/`u` resolve, ignore, unresolve, `?` lists the rest.
- Alerts: Settings → Alerts posts new issues and regressions to a Slack incoming webhook or email
  (`JUJU_SMTP_ADDR`, `JUJU_SMTP_USER`, `JUJU_SMTP_PASSWORD`, `JUJU_SMTP_FROM`). Webhooks must be https and on the
  public internet; `JUJU_ALLOW_HTTP_WEBHOOKS=true` lifts both, for a local fake Slack only.
- Set `JUJU_PUBLIC_URL` to the address people use, so DSNs and links in alerts point to it.
- Live: open pages refresh by themselves when errors arrive or a teammate changes an issue
  (server-sent events on `/api/ui/projects/{id}/live`).

For UI work, `make dashboard-dev` serves the dashboard with hot reload on :5174, talking to `make dev` on
:8080. The binary embeds the UI at compile time, so after `make dashboard`, restart `make dev` to see it.

## Deploy to a VPS

One Ubuntu 24.04 server runs everything: Postgres, juju (API, workers and the embedded dashboard)
and Caddy, which gets HTTPS certificates by itself. Point a domain and `demo.` under it at the
server (no domain yet? `<ip>.sslip.io` and `demo.<ip>.sslip.io` work), then from your laptop:

```bash
deploy/deploy.sh setup   root@SERVER_IP juju.example.com   # once: packages, firewall, Postgres, Caddy, systemd
deploy/deploy.sh release root@SERVER_IP                    # build the dashboard + binary, ship, restart
ssh root@SERVER_IP juju-admin user create -email you@example.com -org Acme   # or sign up on the site

# the web example at https://demo.juju.example.com, with its source map uploaded to juju
JUJU_URL=https://juju.example.com JUJU_PROJECT=1 JUJU_SECRET_KEY=sk_... deploy/deploy.sh demo root@SERVER_IP
```

After `deploy/deploy.sh ci-key root@SERVER_IP` (once), **main is the production branch**: every pull
request merged into main runs the full CI on GitHub Actions and then deploys that commit. If a check
fails, nothing is deployed. To redeploy without a new commit, use "Run workflow" on the Deploy action.

The event queue is embedded in juju and lives in `/var/lib/juju/queue`, so there's no extra service to
run. Config lives in `/etc/juju/juju.env` (SMTP for email alerts), logs in `journalctl -u juju`, and
`/etc/cron.daily/juju-backup` dumps the database daily. Run `deploy/deploy.sh release` again for
each update: migrations apply on start.

## Web SDK

```bash
make sdk            # install, type-check, test and build sdks/js-core and sdks/web
make example-web    # serve the example app at http://127.0.0.1:5173/examples/web/
```

Paste a DSN into the example page (`http://<public_key>@localhost:8080/<project id>`) and click the buttons.
In an app:

```js
import * as juju from "@juju/web";
juju.init({ dsn: "https://pk_...@juju.example.com/1", release: "web@2.3.1", environment: "production" });
```

## React Native SDK

```js
import AsyncStorage from "@react-native-async-storage/async-storage";
import * as juju from "@juju/react-native";
juju.init({ dsn: "https://pk_...@juju.example.com/1", release: "shop@1.4.0+12", storage: AsyncStorage });
```

`storage` keeps unsent events on the device, so a crash is reported on the next launch. Example app: [examples/react-native](examples/react-native).

## Flutter SDK

```dart
void main() => Juju.runZoned(() async {
      WidgetsFlutterBinding.ensureInitialized();
      await Juju.init(JujuOptions(dsn: 'https://pk_...@juju.example.com/1', release: 'shop@1.4.0+12', appPackages: {'shop'}));
      runApp(MaterialApp(navigatorObservers: [Juju.navigatorObserver], home: const HomePage()));
    });
```

Catches framework errors (`FlutterError.onError`), async errors (`PlatformDispatcher.onError`) and zone errors,
and keeps unsent events in a file so they survive the app being killed. Example app: [examples/flutter](examples/flutter).

## Product analytics

Count what people do in your apps, with the same SDKs and DSN as error tracking:

```js
juju.capture("checkout_started", { plan: "pro", items: 3 }); // web and React Native
juju.identify(user.id);                                    // after login
juju.reset();                                              // after logout
```
```dart
Juju.capture('checkout_started', {'plan': 'pro', 'items': 3}); // Flutter
Juju.identify(user.id);
Juju.reset();
```

Until `identify`, events count by an anonymous id kept on the device (localStorage, AsyncStorage, a
file). `identify` links that id to the user, so their events before and after logging in count as
one person; `reset` starts a new anonymous id for the next person. The dashboard's **Analytics** page
has trends (by platform, OS, browser, release or any property), funnels and retention.

Events are stored in ClickHouse (`JUJU_CLICKHOUSE_URL`), a column database that keeps "count these
over time" fast at analytics volumes. Postgres keeps everything else. An event stays in its own durable
queue until ClickHouse has it, so a crash or a ClickHouse outage loses nothing that got a `202`.

## Symbolication

Upload debug files after each release build, so stack traces show your original source:

```bash
export JUJU_URL=https://juju.example.com JUJU_PROJECT=1 JUJU_SECRET_KEY=sk_...
juju sourcemaps inject dist/ && juju upload dist/               # web: stamp debug ids, then upload the maps
juju upload index.android.bundle.map                            # React Native, with withJujuConfig in metro.config.js
juju upload -kind dart_symbols build/symbols/                   # Flutter --split-debug-info
```

A debug id stamped into each bundle and its map names the right map, whatever the release is called.
React Native gets one from `withJujuConfig` (`@juju/react-native/metro`) in `metro.config.js`; Dart
symbols are matched by build id. Maps without one need `-release`, matching what the SDK sends. If frames still look
minified, the event's `symbolication.notes` say what's missing.

## Scale, limits and privacy

An event that got `202` is stored, even if the server is killed a moment later: ingest answers only
after the event is in a durable queue (NATS JetStream, embedded in the binary and kept in
`JUJU_DATA_DIR/queue`), and workers acknowledge it only once it's in Postgres. A database outage
doesn't stop ingest either: events wait in the queue until Postgres is back.

| Setting | Default | |
|---|---|---|
| `JUJU_RATE_LIMIT` | `1000` | events/s per project (token bucket, bursts of 5 s); over it, `429` + `Retry-After` |
| `JUJU_RETENTION_DAYS` | `90` | errors, sessions, transactions are deleted after this (issues stay); `0` keeps them |
| `JUJU_ANALYTICS_RETENTION_DAYS` | `365` | ClickHouse table TTL; `0` keeps them |
| `JUJU_SCRUB_PII` | `true` | removes emails, URL tokens, Bearer/Basic credentials, JWTs and values under keys like `password` at ingest, before anything is written; `user.*` and `distinct_id` are kept |
| `JUJU_QUEUE_MAX_MB` | `1024` | queue size on disk; past it, `429` |
| `JUJU_ANALYTICS_QUEUE_MAX_MB` | `512` | analytics events waiting on disk for ClickHouse; past it, the oldest are dropped and ingest keeps answering |
| `JUJU_TRUSTED_PROXIES` | `127.0.0.1,::1` | reverse proxies whose `X-Forwarded-For` is believed; set it if your proxy isn't on the same machine, or every visitor shares the proxy's login and sign-up limits |

Security model and how to report a vulnerability: [SECURITY.md](SECURITY.md).

Per-project limits: `juju project limits -id 1 -rate 500 -quota 5000000` (`-rate default`,
`-quota none` to clear; running servers pick changes up within 30 s). SDKs can send a share of
errors with `sampleRate: 0.25`; sessions still count every error, so crash-free rates stay exact.

To run ingest, workers and the dashboard as separate processes, point them at one NATS JetStream
server (`nats-server -js`) and give each its part:

```bash
JUJU_ROLES=ingest JUJU_NATS_URL=nats://nats:4222 juju serve   # POST /api/v1/.../events only
JUJU_ROLES=worker JUJU_NATS_URL=nats://nats:4222 juju serve   # stores events, alerts, retention
JUJU_ROLES=api    JUJU_NATS_URL=nats://nats:4222 juju serve   # dashboard and read API
```

## Tests

```bash
make sdk-test          # JavaScript SDK tests
make flutter-test      # Flutter SDK: format, analyze, tests
make test              # gofmt, go vet, unit tests with -race
make test-integration  # also runs against a real (embedded) Postgres and ClickHouse
make fuzz              # fuzz the event parser and the PII scrubber, 30s each
make loadtest URL=http://127.0.0.1:8080 PROJECT=1 KEY=pk_...   # k6: p99 under 50 ms at 5,000 events/s
make dashboard-test    # dashboard: type-check and unit tests
```

Dashboard end-to-end tests (real Chrome against a fresh server): see [dashboard/e2e/README.md](dashboard/e2e/README.md).

In Claude Code, run `/verify` after any change: it writes tests and checks the change by hand.

## Layout

```
server/
  cmd/juju/            the binary: serve, migrate, project create/limits, user create/superadmin, upload
  cmd/devdb/           local Postgres without Docker (-port for a second checkout)
  internal/event/      v1 wire format, validation and repair rules
  internal/fingerprint grouping events into issues
  internal/ingest/     POST /api/v1/projects/{id}/events
  internal/queue/      durable queue between ingest and workers (NATS JetStream, embedded or shared)
  internal/worker/     fingerprint + store; retries with backoff, then a dead-letter stream
  internal/store/      all SQL
  internal/api/        read endpoints and debug file uploads (secret key)
  internal/symbolicate source maps and Dart DWARF back to source, before fingerprinting
  internal/blob/       uploaded files on disk, named by their SHA-256
  internal/db/         connection pool + embedded migrations
  internal/auth/       passwords, sessions, CSRF check, login rate limit
  internal/ui/         /api/ui: what the dashboard calls (session cookie)
  internal/alert/      Slack and email alerts for new issues and regressions
  internal/web/        serves the embedded dashboard build
  internal/analytics/  ClickHouse: batched writes, trends, funnels, retention
  internal/live/       server-sent events that refresh open dashboards, across processes via NATS
  internal/testch/     starts a real ClickHouse for integration tests
scripts/devch.sh       pinned ClickHouse for development, tests and CI
loadtest/ingest.js     k6 load test for ingest (make loadtest)
dashboard/             the dashboard: React + Vite, styles in src/styles/tokens.css
sdks/
  js-core/             @juju/core: events, stack parsing, queue, batching, retry
  web/                 @juju/web: browser error hooks, breadcrumbs, sendBeacon
  react-native/        @juju/react-native: crash handler, saved queue, device context
  flutter/             juju_flutter (Dart): framework, async and zone errors, offline queue
examples/web/          example app that triggers each kind of error
examples/react-native/ React Native app for iOS and Android release-build checks
examples/flutter/      Flutter app for Android release and iOS simulator checks
```
