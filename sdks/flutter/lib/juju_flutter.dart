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
import 'src/core/tracing.dart';
import 'src/core/transport.dart';
import 'src/flutter/breadcrumbs.dart';
import 'src/flutter/context.dart';
import 'src/flutter/hooks.dart';
import 'src/flutter/performance.dart';
import 'src/flutter/storage.dart';

export 'src/core/client.dart' show JujuClient, ClientOptions, QueueStorage, IdentityStorage, JsonMap, sessionResumeAfter;
export 'src/core/tracing.dart' show JujuTransaction, JujuSpan;
export 'src/flutter/performance.dart' show processStartTime;
export 'src/core/dsn.dart' show Dsn;
export 'src/core/stacktrace.dart' show parseStackTrace, makeIsInApp, JujuFrame, ParsedStack;
export 'src/core/transport.dart' show Transport, SendResult, Delivered, Rejected, RetryLater, HttpTransport;
export 'src/flutter/breadcrumbs.dart' show JujuNavigatorObserver, JujuHttpClient;
export 'src/flutter/storage.dart' show FileQueueStorage, FileIdentityStorage;

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
    this.identityStorage,
    this.transport,
    this.sessions = true,
    this.tracing = true,
    this.tracesSampleRate = 1,
    this.sampleRate = 1,
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

  /// Where the anonymous id is kept between launches. Defaults to a file in
  /// the app's support folder.
  final IdentityStorage? identityStorage;

  /// Replace how events are sent (used by tests).
  final Transport? transport;

  /// Count sessions for the crash-free rate.
  final bool sessions;

  /// Time the app start, screen loads (with [Juju.navigatorObserver]) and
  /// requests made through [Juju.httpClient].
  final bool tracing;

  /// Share of transactions to send, 0 to 1.
  final double tracesSampleRate;

  /// Share of errors to send, 0 to 1. Sessions still count every error.
  final double sampleRate;
}

abstract final class Juju {
  static JujuClient? _client;
  static final List<VoidCallback> _uninstallers = [];

  /// Records screen changes: `MaterialApp(navigatorObservers: [Juju.navigatorObserver])`.
  static final NavigatorObserver navigatorObserver = JujuNavigatorObserver(() => _client, tracing: () => _tracing);
  static bool _tracing = false;

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
      identityStorage: options.identityStorage ?? FileIdentityStorage(),
      isInApp: makeIsInApp(options.appPackages),
      context: deviceContext,
      beforeSend: options.beforeSend,
      flushInterval: options.flushInterval,
      debug: options.debug,
      log: debugPrint,
      random: Random.secure(),
      tracesSampleRate: options.tracesSampleRate,
      sampleRate: options.sampleRate,
    ));
    _client = client;
    _tracing = options.tracing;

    if (options.sessions) client.startSession();
    final lifecycle = JujuLifecycleObserver(client, sessions: options.sessions);
    binding.addObserver(lifecycle);
    _uninstallers
      ..add(installFlutterErrorHook(client))
      // PlatformDispatcher.instance, not binding.platformDispatcher: in an
      // app they're the same object, but flutter_test's wrapper ignores
      // writes to onError, so only the real dispatcher can be hooked there.
      ..add(installPlatformErrorHook(client, PlatformDispatcher.instance))
      ..add(() => binding.removeObserver(lifecycle));

    // The error hooks are in place first. Then read the process start while
    // the saved queue loads, so init doesn't wait for one after the other.
    // Still before runApp, so the app start exists before any request the
    // app makes while starting.
    final processStart = options.tracing ? processStartTime() : Future<DateTime?>.value();
    await client.ready;
    if (options.tracing && identical(_client, client)) _uninstallers.add(startAppStart(client, processStart: await processStart));
  }

  /// Runs [body] in a zone that reports errors no other hook sees (for
  /// example, errors thrown before the Flutter binding exists) and records
  /// `print()` output as breadcrumbs. Call [init] and `runApp` inside it,
  /// so the Flutter binding and the app share one zone.
  static void runZoned(FutureOr<void> Function() body) {
    runZonedGuarded(
      body,
      (error, stack) {
        _client?.captureException(error, stack, mechanism: 'zone', crashed: true);
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

  /// Records something a user did, for product analytics:
  /// `Juju.capture('checkout_started', {'plan': 'pro'})`.
  static void capture(String name, [JsonMap? properties]) => _client?.capture(name, properties);

  /// After login: this install's events so far and from now on belong to [userId].
  static void identify(String userId) => _client?.identify(userId);

  /// After logout: forget the user, so the next person is new.
  static void reset() => _client?.reset();
  static void setTag(String key, String value) => _client?.setTag(key, value);
  static void addBreadcrumb(String type, {String? message, JsonMap? data}) => _client?.addBreadcrumb(type, message: message, data: data);

  /// Times something the app does:
  ///
  /// ```dart
  /// final tx = Juju.startTransaction('checkout');
  /// final span = tx.startChild('db', description: 'save order');
  /// ...; span.finish(); tx.finish();
  /// ```
  ///
  /// Before init it returns a transaction that sends nothing.
  static JujuTransaction startTransaction(String name, {String op = 'custom'}) =>
      _client?.startTransaction(name, op) ?? JujuTransaction(_Inert(), name: name, op: op, sampled: false);

  /// An http.Client that records each request as a breadcrumb, and as a
  /// span of the app start or screen load it happens during.
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
    _tracing = false;
  }

  @visibleForTesting
  static ParsedStack parse(StackTrace stack) => parseStackTrace(stack, makeIsInApp());

  @visibleForTesting
  static SendResult classifyStatus(int status, [String? retryAfter]) => classify(status, retryAfter);
}

class _Inert implements TransactionHost {
  @override
  void finishTransaction(JujuTransaction tx) {}
}
