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

## Tests

```bash
make sdk-test          # JavaScript SDK tests
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
  internal/api/        read endpoints (secret key)
  internal/db/         connection pool + embedded migrations
sdks/
  js-core/             @juju/core: events, stack parsing, queue, batching, retry
  web/                 @juju/web: browser error hooks, breadcrumbs, sendBeacon
  react-native/        @juju/react-native: crash handler, saved queue, device context
examples/web/          example app that triggers each kind of error
examples/react-native/ React Native app for iOS and Android release-build checks
```
