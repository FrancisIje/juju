/// juju error tracking for Flutter.
///
/// ```dart
/// void main() => Juju.runZoned(() async {
///       WidgetsFlutterBinding.ensureInitialized();
///       await Juju.init(JujuOptions(dsn: 'https://pk_...@juju.example.com/1', release: 'shop@1.4.0+12'));
///       runApp(const MyApp());
///     });
/// ```
///
/// Every method except [Juju.init] is safe to call before init (it does
/// nothing) and never throws. init throws a FormatException on an invalid
/// DSN, so a configuration mistake is noticed at startup.
library;

import 'dart:async';
import 'dart:math';

import 'package:flutter/foundation.dart';
import 'package:flutter/widgets.dart';
import 'package:http/http.dart' as http;

import 'src/core/client.dart';
import 'src/core/stacktrace.dart';
import 'src/core/transport.dart';
import 'src/flutter/breadcrumbs.dart';
import 'src/flutter/context.dart';
import 'src/flutter/hooks.dart';
import 'src/flutter/storage.dart';

export 'src/core/client.dart' show JujuClient, ClientOptions, QueueStorage, JsonMap;
export 'src/core/dsn.dart' show Dsn;
export 'src/core/stacktrace.dart' show parseStackTrace, makeIsInApp, JujuFrame, ParsedStack;
export 'src/core/transport.dart' show Transport, SendResult, Delivered, Rejected, RetryLater, HttpTransport;
export 'src/flutter/breadcrumbs.dart' show JujuNavigatorObserver, JujuHttpClient;
export 'src/flutter/storage.dart' show FileQueueStorage;

class JujuOptions {
  const JujuOptions({
    required this.dsn,
    this.release,
    this.environment,
    this.appPackages = const {},
    this.beforeSend,
    this.debug = false,
    this.flushInterval = const Duration(seconds: 5),
    this.storage,
    this.transport,
  });

  final String dsn;

  /// e.g. "shop@1.4.0+12": the app version and build number.
  final String? release;

  /// Defaults to "production" in release builds and "development" otherwise.
  final String? environment;

  /// Your app's own package names (from pubspec.yaml), so their frames are
  /// marked as app code. Without it, every package except Flutter's own
  /// and a few well-known ones counts as app code.
  final Set<String> appPackages;
  final JsonMap? Function(JsonMap event)? beforeSend;
  final bool debug;
  final Duration flushInterval;

  /// Where unsent events are kept between launches. Defaults to a file in
  /// the app's support folder.
  final QueueStorage? storage;

  /// Replace how events are sent (used by tests).
  final Transport? transport;
}

abstract final class Juju {
  static JujuClient? _client;
  static final List<VoidCallback> _uninstallers = [];

  /// Records screen changes: `MaterialApp(navigatorObservers: [Juju.navigatorObserver])`.
  static final NavigatorObserver navigatorObserver = JujuNavigatorObserver(() => _client);

  static JujuClient? get client => _client;

  static Future<void> init(JujuOptions options) async {
    close(); // calling init twice replaces the old setup instead of doubling every hook
    final binding = WidgetsFlutterBinding.ensureInitialized();
    final client = JujuClient(ClientOptions(
      dsn: options.dsn,
      release: options.release,
      environment: options.environment ?? (kReleaseMode ? 'production' : 'development'),
      platform: 'dart',
      sdkName: 'juju.flutter',
      transport: options.transport,
      storage: options.storage ?? FileQueueStorage(),
      isInApp: makeIsInApp(options.appPackages),
      context: deviceContext,
      beforeSend: options.beforeSend,
      flushInterval: options.flushInterval,
      debug: options.debug,
      log: debugPrint,
      random: Random.secure(),
    ));
    _client = client;

    final lifecycle = JujuLifecycleObserver(client);
    binding.addObserver(lifecycle);
    _uninstallers
      ..add(installFlutterErrorHook(client))
      // PlatformDispatcher.instance, not binding.platformDispatcher: in an
      // app they're the same object, but flutter_test's wrapper ignores
      // writes to onError, so only the real dispatcher can be hooked there.
      ..add(installPlatformErrorHook(client, PlatformDispatcher.instance))
      ..add(() => binding.removeObserver(lifecycle));
    await client.ready;
  }

  /// Runs [body] in a zone that reports errors no other hook sees (for
  /// example, errors thrown before the Flutter binding exists) and records
  /// `print()` output as breadcrumbs. Call [init] and `runApp` inside it,
  /// so the Flutter binding and the app share one zone.
  static void runZoned(FutureOr<void> Function() body) {
    runZonedGuarded(
      body,
      (error, stack) {
        _client?.captureException(error, stack, mechanism: 'zone');
        // Still print it, as Flutter would without a guarded zone.
        Zone.root.print('Uncaught error: $error\n$stack');
      },
      zoneSpecification: ZoneSpecification(print: (self, parent, zone, line) {
        // debugPrint and our own [juju] logs go through here too; skip those.
        if (!line.startsWith('[juju]')) _client?.addBreadcrumb('print', message: line);
        parent.print(zone, line);
      }),
    );
  }

  /// Report an error the app caught. Returns the event_id, or "" if not sent.
  static String captureException(Object error, [StackTrace? stack, Map<String, String>? tags]) =>
      _client?.captureException(error, stack ?? StackTrace.current, tags: tags, mechanism: 'manual') ?? '';

  static void setUser(JsonMap? user) => _client?.setUser(user);
  static void setTag(String key, String value) => _client?.setTag(key, value);
  static void addBreadcrumb(String type, {String? message, JsonMap? data}) => _client?.addBreadcrumb(type, message: message, data: data);

  /// An http.Client that records each request as a breadcrumb.
  static http.Client httpClient([http.Client? inner]) => JujuHttpClient(() => _client, inner);

  /// Send queued events now.
  static Future<void> flush() => _client?.flush() ?? Future.value();

  /// Remove every hook and stop sending.
  static void close() {
    for (final uninstall in _uninstallers.reversed) {
      try {
        uninstall();
      } catch (_) {}
    }
    _uninstallers.clear();
    _client?.close();
    _client = null;
  }

  @visibleForTesting
  static ParsedStack parse(StackTrace stack) => parseStackTrace(stack, makeIsInApp());

  @visibleForTesting
  static SendResult classifyStatus(int status, [String? retryAfter]) => classify(status, retryAfter);
}
