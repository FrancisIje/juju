# juju

Error tracking and product analytics for Flutter, React Native and web apps, with a Go backend.
Roadmap: https://francisije.github.io/juju/ (source: [docs/_src/roadmap.html](docs/_src/roadmap.html); run `make docs` after editing it).

## Run it locally

```bash
make devdb          # terminal 1: Postgres without Docker (or `make db` if you have Docker)
make dev            # terminal 2: the server on :8080
```

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

The first visit creates the owner account and an organization. After that, sign-up is closed and
the login page tells people to ask for an account. Add them to your organization from the shell with
`go run ./cmd/juju user create -email ada@example.com -org Acme` (on a VPS: `juju-admin user create …`),
or set `JUJU_ALLOW_SIGNUP=true` to open `/signup` to everyone. Self sign-up always creates a new
organization; it can never join an existing one by name. New projects show their
DSN and setup code for each SDK; the secret key is shown once.

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

Config lives in `/etc/juju/juju.env` (SMTP for email alerts), logs in `journalctl -u juju`, and
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

## Symbolication

Upload debug files after each release build, so stack traces show your original source:

```bash
export JUJU_URL=https://juju.example.com JUJU_PROJECT=1 JUJU_SECRET_KEY=sk_...
juju upload -kind sourcemap -release web@2.3.1 dist/            # web: *.map next to your bundles
juju upload -kind sourcemap -release shop@1.4.0+12 index.android.bundle.map   # React Native
juju upload -kind dart_symbols build/symbols/                   # Flutter --split-debug-info
```

The release must match what the SDK sends. Dart symbols are matched by build id. If frames still look
minified, the event's `symbolication.notes` say what's missing.

## Tests

```bash
make sdk-test          # JavaScript SDK tests
make flutter-test      # Flutter SDK: format, analyze, tests
make test              # gofmt, go vet, unit tests with -race
make test-integration  # also runs against a real (embedded) Postgres
make fuzz              # fuzz the event parser for 30s
make dashboard-test    # dashboard: type-check and unit tests
```

Dashboard end-to-end tests (real Chrome against a fresh server): see [dashboard/e2e/README.md](dashboard/e2e/README.md).

In Claude Code, run `/verify` after any change: it writes tests and checks the change by hand.

## Layout

```
server/
  cmd/juju/            the binary: serve, migrate, project create, user create, upload
  cmd/devdb/           local Postgres without Docker
  internal/event/      v1 wire format, validation and repair rules
  internal/fingerprint grouping events into issues
  internal/ingest/     POST /api/v1/projects/{id}/events
  internal/queue/      buffered channel between ingest and workers
  internal/worker/     fingerprint + store, with retries
  internal/store/      all SQL
  internal/api/        read endpoints and debug file uploads (secret key)
  internal/symbolicate source maps and Dart DWARF back to source, before fingerprinting
  internal/blob/       uploaded files on disk, named by their SHA-256
  internal/db/         connection pool + embedded migrations
  internal/auth/       passwords, sessions, CSRF check, login rate limit
  internal/ui/         /api/ui: what the dashboard calls (session cookie)
  internal/alert/      Slack and email alerts for new issues and regressions
  internal/web/        serves the embedded dashboard build
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
