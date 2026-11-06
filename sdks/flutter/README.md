# juju_flutter

juju error tracking for Flutter apps.

```dart
import 'package:flutter/material.dart';
import 'package:juju_flutter/juju_flutter.dart';

void main() => Juju.runZoned(() async {
      WidgetsFlutterBinding.ensureInitialized();
      await Juju.init(JujuOptions(
        dsn: 'https://pk_...@juju.example.com/1',
        release: 'shop@1.4.0+12',
        appPackages: {'shop'}, // your pubspec name: marks your frames as app code
      ));
      runApp(MaterialApp(navigatorObservers: [Juju.navigatorObserver], home: const HomePage()));
    });
```

## What it catches

| Error | How | Tag |
|---|---|---|
| Thrown while building, laying out or painting a widget | `FlutterError.onError` | `mechanism: flutter_error` |
| An async error nobody caught (unawaited future, timer) | `PlatformDispatcher.instance.onError` | `mechanism: platform_dispatcher` |
| An async error inside `Juju.runZoned` | the zone's error handler | `mechanism: zone` |
| `Juju.captureException(e, s)` | you call it | `mechanism: manual` |

Every hook keeps the handler that was there before and calls it, so Flutter's own output and your own
handlers keep working. Inside `Juju.runZoned`, async errors go to the zone, not to `PlatformDispatcher`.

## Breadcrumbs

- `Juju.navigatorObserver`: screen changes (route names)
- `Juju.httpClient()`: an `http.Client` that records method, URL path and status (never bodies or query strings)
- `print()` output inside `Juju.runZoned`
- app lifecycle (`resumed`, `paused`, ...): on pause the queue is saved and sent, on resume it's retried

## Offline and killed apps

Unsent events are kept in `juju_queue.json` in the app's support folder (written atomically) and sent on the
next launch. Retries back off 1 s, 2 s, 4 s... up to 60 s, and every retry reuses the event id, so the server
stores each event once.

## If juju breaks, the app carries on

No failure of the server or the SDK reaches your app. You lose reports, never users.

- `Juju.init` never throws and returns within 2 seconds, so it's safe to `await` before `runApp`. A wrong DSN leaves
  juju off and logs why: check for `[juju] not running` when you set it up.
- No other method throws, before init or after. `Juju.navigatorObserver` and `Juju.httpClient()` can't break the
  navigation or the request they record.
- Retries stay 30 to 60 s apart during an outage. `Retry-After` is obeyed for at most an hour.
- Values JSON can't hold (`DateTime`, `NaN`) are sent as text.
- Memory is bounded: 100 errors and 1000 other events, 8 MB of each, 1 MB per event.
- A lone event is saved at once; in a burst, saves are at least 2 s apart. A kill right after a burst can lose the
  newest events.
- Don't `await Juju.flush()` on the way to a screen: a server that hangs holds it for the 30 s request timeout.

## Not yet

- Device model and Android OS version need a native plugin (Android reports a build id, kept as `os.build`).
- Flutter web, and errors in other isolates.
- Non-symbolic traces (`--split-debug-info`) keep their build id and raw trace for symbolication in Phase 5.
