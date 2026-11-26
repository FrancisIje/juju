# SDKs

Error tracking, product analytics and readable stack traces for web, React Native and Flutter apps. Back to the [README](../README.md).

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

`storage` keeps unsent events on the device, so a crash is reported on the next launch. Example app: [examples/react-native](../examples/react-native).

## Flutter SDK

```dart
void main() => Juju.runZoned(() async {
      WidgetsFlutterBinding.ensureInitialized();
      await Juju.init(JujuOptions(dsn: 'https://pk_...@juju.example.com/1', release: 'shop@1.4.0+12', appPackages: {'shop'}));
      runApp(MaterialApp(navigatorObservers: [Juju.navigatorObserver], home: const HomePage()));
    });
```

Catches framework errors (`FlutterError.onError`), async errors (`PlatformDispatcher.onError`) and zone errors,
and keeps unsent events in a file so they survive the app being killed. Example app: [examples/flutter](../examples/flutter).

## Product analytics

Count what people do in your apps, with the same SDKs and DSN as error tracking:

```js
juju.capture("checkout_started", { plan: "pro", items: 3 }); // web and React Native
juju.identify(user.id);                                    // after login
juju.reset();                                              // after logout
```
```dart
Juju.capture('checkout_started', {'plan': 'pro', 'items': 3}); // Flutter
Juju.identify(user.id);
Juju.reset();
```

Until `identify`, events count by an anonymous id kept on the device (localStorage, AsyncStorage, a
file). `identify` links that id to the user, so their events before and after logging in count as
one person; `reset` starts a new anonymous id for the next person. The dashboard's **Analytics** page
has trends (by platform, OS, browser, release or any property), funnels and retention.

Events are stored in ClickHouse (`JUJU_CLICKHOUSE_URL`), a column database that keeps "count these
over time" fast at analytics volumes. Postgres keeps everything else. An event stays in its own durable
queue until ClickHouse has it, so a crash or a ClickHouse outage loses nothing that got a `202`.

## Symbolication

Upload debug files after each release build, so stack traces show your original source:

```bash
export JUJU_URL=https://juju.example.com JUJU_PROJECT=1 JUJU_SECRET_KEY=sk_...
juju sourcemaps inject dist/ && juju upload dist/               # web: stamp debug ids, then upload the maps
juju upload index.android.bundle.map                            # React Native, with withJujuConfig in metro.config.js
juju upload -kind dart_symbols build/symbols/                   # Flutter --split-debug-info
```

A debug id stamped into each bundle and its map names the right map, whatever the release is called.
React Native gets one from `withJujuConfig` (`@juju/react-native/metro`) in `metro.config.js`; Dart
symbols are matched by build id. Maps without one need `-release`, matching what the SDK sends. If frames still look
minified, the event's `symbolication.notes` say what's missing.

