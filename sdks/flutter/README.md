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

## Not yet

- Device model and Android OS version need a native plugin (Android reports a build id, kept as `os.build`).
- Flutter web, and errors in other isolates.
- Non-symbolic traces (`--split-debug-info`) keep their build id and raw trace for symbolication in Phase 5.
