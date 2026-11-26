# Scale, limits and privacy

What juju promises about your data, the settings that bound it, and how to split it across machines. Back to the [README](../README.md).

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

Security model and how to report a vulnerability: [SECURITY.md](../SECURITY.md).

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

