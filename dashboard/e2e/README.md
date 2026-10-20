# Dashboard end-to-end tests

They run the whole product: real Chrome → embedded dashboard → Go server →
Postgres → alerts. They need a **fresh** server (no accounts yet), because
the test starts with first-run setup.

```bash
make dashboard                                              # embed the current UI
cd server && go run ./cmd/devdb -dir /tmp/juju-e2e-db       # terminal 1: fresh Postgres
python3 dashboard/e2e/fake-slack.py /tmp/slack.log          # terminal 2: fake Slack on :9911
cd server && JUJU_ALLOW_HTTP_WEBHOOKS=true go run ./cmd/juju serve   # terminal 3
cd dashboard && JUJU_E2E_SLACK=http://127.0.0.1:9911/hook npm run e2e
```

- `JUJU_E2E_SLACK`: a URL that accepts POSTs. Without it, the alert steps are skipped.
  `/tmp/slack.log` then holds the exact Slack messages juju sent.
- `JUJU_E2E_SHOTS=<dir>`: saves screenshots of each page in dark and light, at desktop and phone width.
- To run it again, start over with an empty database directory.
- It uses the Chrome installed on your machine (`channel: "chrome"`), so there's no browser download.
