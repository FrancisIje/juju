# juju

Error tracking and product analytics for Flutter, React Native and web apps, with a Go backend.
See the roadmap in [docs/roadmap.html](docs/roadmap.html).

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

## Tests

```bash
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
```
