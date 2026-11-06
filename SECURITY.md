# Security

## Reporting a vulnerability

Please report security problems privately, not in a public issue: use
**[Report a vulnerability](https://github.com/FrancisIje/juju/security/advisories/new)**
on this repository's Security tab. Include what you found, how to reproduce it, and what an
attacker could do with it. You'll get an answer within a few days, and credit in the fix if you'd
like it.

Please don't test against servers you don't run, including the public demo: run juju locally
(`make devdb` and `make dev`, see the README) and test that.

## How juju protects data

- **Two keys per project.** The public key (`pk_…`) ships inside apps and can only *send* events.
  The secret key (`sk_…`) reads data and uploads source maps; only its SHA-256 is stored.
- **Dashboard logins**: bcrypt passwords; random 32-byte session tokens, stored hashed, in
  `HttpOnly`, `Secure`, `SameSite=Lax` cookies; a CSRF header on every request that changes data.
- **Super admins** see every organization's name, member emails and usage counts, but no issue
  details, event data or keys, and they can't open another organization's projects. Only
  `juju user superadmin`, run on the server, can grant it: no HTTP request can, and it isn't based on
  an email list, because sign-up doesn't verify email addresses. Every other account gets a 404, each
  access is logged (`super admin access`), and a revoke applies to the next request.
- **Rate limits**: failed logins per address and email (10 in 15 minutes) and per address (50);
  sign-ups per address (5 an hour); test alerts per project (5 in 10 minutes); alerts per project
  (30 an hour); events per project (`JUJU_RATE_LIMIT`).
- **Behind a reverse proxy**, juju takes the client's address from `X-Forwarded-For` only when the
  request comes from one of `JUJU_TRUSTED_PROXIES` (default: this machine, `127.0.0.1` and `::1`).
  If your proxy runs elsewhere, set it, or every visitor shares the proxy's limits.
- **Webhooks** go only to public internet addresses. The check runs on the address actually
  dialed, so a hostname that resolves to a private address, or a redirect to one, is refused too.
- **Personal data** that apps send by accident (emails, tokens in URLs, credentials) is removed at
  ingest, before it's written anywhere (`JUJU_SCRUB_PII`).
- **Headers**: a strict Content-Security-Policy on the dashboard, `nosniff` on every response, and
  HSTS when the public URL is https.

## Supported versions

Only the latest commit on `main` gets security fixes.
