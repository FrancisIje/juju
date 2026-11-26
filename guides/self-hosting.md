# Running and hosting juju

Run juju on your laptop, use the dashboard, and deploy it to one server. Back to the [README](../README.md).

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

