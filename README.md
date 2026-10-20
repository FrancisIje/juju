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
```

In Claude Code, run `/verify` after any change: it writes tests and checks the change by hand.

## Layout

```
server/
  cmd/juju/            the binary: serve, migrate, project create
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
sdks/
  js-core/             @juju/core: events, stack parsing, queue, batching, retry
  web/                 @juju/web: browser error hooks, breadcrumbs, sendBeacon
  react-native/        @juju/react-native: crash handler, saved queue, device context
  flutter/             juju_flutter (Dart): framework, async and zone errors, offline queue
examples/web/          example app that triggers each kind of error
examples/react-native/ React Native app for iOS and Android release-build checks
examples/flutter/      Flutter app for Android release and iOS simulator checks
```
