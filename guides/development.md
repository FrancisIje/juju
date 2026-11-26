# Working on juju

Tests and a map of the code. Back to the [README](../README.md).

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

Dashboard end-to-end tests (real Chrome against a fresh server): see [dashboard/e2e/README.md](../dashboard/e2e/README.md).

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
scripts/record-demo.cjs records the README demo video
guides/                the longer docs the README links to
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

## The README demo video

`docs/img/demo/demo.gif` (played inline on GitHub) and `demo.mp4` come from
[scripts/record-demo.cjs](../scripts/record-demo.cjs). It drives Chrome through the web example and the dashboard,
then stitches the two side by side with ffmpeg. Re-record it when the dashboard or the example
changes visibly. It needs a fresh server, and the steps are at the top of the script.
