## Unreleased

- `Juju.init` never throws and returns within 2 s. **Changed:** a wrong DSN no longer throws; juju stays off and logs why.
- Fixed retries turning non-stop after about 24 to 45 minutes of outage (the backoff overflowed).
- Fixed a huge `Retry-After` reaching the app as a `RangeError`; it's now obeyed for at most an hour.
- Fixed one value JSON can't hold (`DateTime`, `NaN`) blocking all reporting; such values are now sent as text.
- Queue saves are at least 2 s apart in a burst, instead of re-encoding the queue for every event.
- Memory limits in bytes: 8 KB per error message, 1 MB per event, 8 MB of errors and 8 MB of everything else.
- Recording failures can't break the app's navigation, requests or lifecycle callbacks.

## 0.1.0

- First version: framework, async and zone errors; breadcrumbs (navigation, http, print, lifecycle);
  batching, gzip, backoff with Retry-After; an offline queue that survives the app being killed.
