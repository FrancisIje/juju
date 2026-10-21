import 'dart:async';
import 'dart:io';
import 'dart:math';

import 'package:clock/clock.dart';
import 'package:fake_async/fake_async.dart';
import 'package:flutter/material.dart';
import 'package:flutter_test/flutter_test.dart';
import 'package:http/http.dart' as http;
import 'package:http/testing.dart';
import 'package:juju_flutter/juju_flutter.dart';
import 'package:juju_flutter/src/flutter/performance.dart' show startAppStart, appStartDeadline;

import 'core_test.dart' show FakeTransport, MemoryStorage, dsn;

final t0 = DateTime.utc(2026, 10, 20, 10);

JujuClient newClient(FakeTransport t, {double rate = 1}) =>
    JujuClient(ClientOptions(dsn: dsn, transport: t, release: 'shop@1.0', tracesSampleRate: rate));

/// Runs [body] with a fake clock and timers starting at [t0].
void fake(void Function(FakeAsync async) body) => fakeAsync(body, initialTime: t0);

Map<String, dynamic> sessionOf(Map<String, dynamic> e) => (e['session'] as Map).cast<String, dynamic>();

void main() {
  group('sessions', () {
    test('a start and an end share the sid, with a higher seq', () {
      fake((async) {
        final t = FakeTransport();
        final c = newClient(t)..startSession();
        c.flush();
        async.elapse(const Duration(seconds: 90));
        c.endSession();
        c.flush();
        async.flushMicrotasks();
        final s = t.ofType('session').map(sessionOf).toList();
        expect(s[0], containsPair('status', 'ok'));
        expect(s[0]['started'], '2026-10-20T10:00:00.000Z');
        expect(
            s[1],
            allOf(containsPair('sid', s[0]['sid']), containsPair('seq', 1), containsPair('status', 'exited'),
                containsPair('duration_ms', 90000)));
      });
    });

    test('errors are counted, and a crash ends the session as crashed', () {
      fake((async) {
        final t = FakeTransport();
        final c = newClient(t)..startSession();
        c.captureException(StateError('handled'), null);
        c.captureException(StateError('fatal'), null, crashed: true);
        c.flush();
        async.flushMicrotasks();
        // Both updates waited in the queue, so only the latest went out.
        final s = t.ofType('session').map(sessionOf).toList();
        expect(s, hasLength(1));
        expect(s.single, allOf(containsPair('status', 'crashed'), containsPair('errors', 2), containsPair('seq', 2)));
        expect(c.sessionId, isNull);
      });
    });

    test('an error reported, then rethrown into a global hook, still crashes the session', () {
      fake((async) {
        final t = FakeTransport();
        final c = newClient(t)..startSession();
        final err = StateError('rethrown');
        c.captureException(err, null);
        expect(c.captureException(err, null, crashed: true), isEmpty);
        c.flush();
        async.flushMicrotasks();
        expect(sessionOf(t.ofType('session').last), allOf(containsPair('status', 'crashed'), containsPair('errors', 1)));
      });
    });

    test('a short trip to the background continues the session; a long one starts a new one', () {
      fake((async) {
        final t = FakeTransport();
        final c = newClient(t)..startSession();
        final first = c.sessionId;
        c.pauseSession();
        async.elapse(sessionResumeAfter - const Duration(seconds: 1));
        c.resumeSession();
        expect(c.sessionId, first);
        c.pauseSession();
        async.elapse(sessionResumeAfter + const Duration(seconds: 1));
        c.resumeSession();
        expect(c.sessionId, isNot(first));
      });
    });

    test('offline, a session keeps a single queued update', () {
      fake((async) {
        final c = JujuClient(ClientOptions(dsn: dsn, transport: FakeTransport([for (var i = 0; i < 99; i++) const RetryLater()])))
          ..startSession();
        for (var i = 0; i < 50; i++) {
          c
            ..pauseSession()
            ..resumeSession();
        }
        expect(c.pending, 1);
      });
    });
  });

  group('transactions', () {
    test('carry spans with microsecond times', () {
      fake((async) {
        final t = FakeTransport();
        final c = newClient(t);
        final tx = c.startTransaction('checkout', 'custom');
        async.elapse(const Duration(microseconds: 250500));
        final span = tx.startChild('http.client', description: 'GET /api/cart');
        async.elapse(const Duration(milliseconds: 500));
        span
          ..setData('status_code', 200)
          ..finish();
        tx.finish();
        c.flush();
        async.flushMicrotasks();
        final e = t.ofType('transaction').single;
        expect(e['release'], 'shop@1.0');
        expect(e['transaction'], allOf(containsPair('name', 'checkout'), containsPair('start_timestamp', '2026-10-20T10:00:00.000Z')));
        final s = (e['transaction']['spans'] as List).single as Map;
        expect(s['start_timestamp'], '2026-10-20T10:00:00.250500Z');
        expect(s['data'], {'status_code': 200});
      });
    });

    test('an idle transaction ends at its last activity, not when its timer fires', () {
      fake((async) {
        final t = FakeTransport();
        final c = newClient(t);
        final tx = c.startTransaction('Cart', 'screen.load', idleTimeout: const Duration(seconds: 1), active: true);
        final release = tx.hold();
        async.elapse(const Duration(milliseconds: 50));
        final req = c.startSpan('http.client');
        async.elapse(const Duration(milliseconds: 50));
        release();
        async.elapse(const Duration(milliseconds: 200));
        req.finish();
        async.elapse(const Duration(milliseconds: 999));
        expect(tx.finished, isFalse);
        async.elapse(const Duration(milliseconds: 1));
        expect(tx.finished, isTrue);
        c.flush();
        async.flushMicrotasks();
        expect(t.ofType('transaction').single['timestamp'], '2026-10-20T10:00:00.300Z');
        expect(c.activeTransaction, isNull);
      });
    });

    test('gives up at the deadline and marks open spans cancelled', () {
      fake((async) {
        final t = FakeTransport();
        final c = newClient(t);
        final tx = c.startTransaction('Feed', 'screen.load', idleTimeout: const Duration(seconds: 1), active: true);
        tx.hold();
        c.startSpan('http.client', description: 'GET /hang');
        async.elapse(const Duration(seconds: 30));
        c.flush();
        async.flushMicrotasks();
        final e = t.ofType('transaction').single['transaction'];
        expect(e['status'], 'deadline_exceeded');
        expect((e['spans'] as List).single['status'], 'cancelled');
      });
    });

    test('a sample rate of 0 sends nothing, and discard() sends nothing', () {
      fake((async) {
        final t = FakeTransport();
        final c = newClient(t, rate: 0);
        c.startTransaction('a', 'custom').finish();
        final c2 = newClient(t);
        c2.startTransaction('b', 'custom').discard();
        c.flush();
        c2.flush();
        async.flushMicrotasks();
        expect(t.ofType('transaction'), isEmpty);
      });
    });
  });

  group('app start', () {
    late Directory dir;
    setUp(() => dir = Directory.systemTemp.createTempSync('juju-proc'));
    tearDown(() => dir.deleteSync(recursive: true));

    // A real /proc/self/stat line, with a program name that has spaces and
    // parentheses in it, which is why fields are counted from the last ')'.
    String statLine(int startTicks) =>
        '4242 (com.example (my) app) S 600 600 0 0 -1 1077952832 9000 0 0 0 40 12 0 0 10 -10 30 0 $startTicks 1234 567 18446744073709551615';

    Future<DateTime?> read(int selfTicks, int nowTicks) {
      final stat = File('${dir.path}/stat')..writeAsStringSync(statLine(selfTicks));
      return withClock(Clock.fixed(t0), () => processStartTime(stat: stat.path, statOfNewProcess: () async => statLine(nowTicks)));
    }

    // The age comes from two start times on the same clock: ours, and a
    // process started just now. (Android forbids /proc/uptime to apps.)
    test('reads the process start from /proc on Android', () async {
      expect(await read(50000, 50250), t0.subtract(const Duration(milliseconds: 2500)));
    });

    test("doesn't treat an old process as a cold start", () async {
      expect(await read(50000, 90000), isNull); // 400 s old: Flutter started in a process that already existed
    });

    test('returns null without /proc (iOS), without starting a process', () async {
      var spawned = false;
      final got = await processStartTime(
          stat: '${dir.path}/missing',
          statOfNewProcess: () async {
            spawned = true;
            return null;
          });
      expect(got, isNull);
      expect(spawned, isFalse);
    });

    test('works for real on Linux, and gives null on macOS', () async {
      final got = await processStartTime();
      if (Platform.isLinux) {
        expect(got, isNotNull);
      } else {
        expect(got, isNull);
      }
    });

    test('runs from the process start until the first frame', () {
      fake((async) {
        final t = FakeTransport();
        final c = newClient(t);
        final firstFrame = Completer<void>();
        startAppStart(c, processStart: t0.subtract(const Duration(milliseconds: 700)), firstFrame: firstFrame.future);
        async.elapse(const Duration(milliseconds: 400));
        firstFrame.complete();
        async.flushMicrotasks();
        c.flush();
        async.flushMicrotasks();
        final e = t.ofType('transaction').single['transaction'];
        expect(e['measurements'], {'from_process_start': 1});
        expect(e['start_timestamp'], '2026-10-20T09:59:59.300Z');
        expect([for (final s in e['spans'] as List) s['description']], ['Process start until juju.init', 'First frame']);
        expect(t.ofType('transaction').single['timestamp'], '2026-10-20T10:00:00.400Z');
      });
    });

    test('is dropped if the first frame never comes', () {
      fake((async) {
        final t = FakeTransport();
        final c = newClient(t);
        startAppStart(c, firstFrame: Completer<void>().future);
        async.elapse(appStartDeadline + const Duration(seconds: 1));
        c.flush();
        async.flushMicrotasks();
        expect(t.ofType('transaction'), isEmpty);
      });
    });
  });

  group('Flutter integration', () {
    tearDown(Juju.close);

    test('an uncaught framework error crashes the session', () async {
      final saved = FlutterError.onError;
      FlutterError.onError = (_) {}; // juju wraps this quiet handler instead of the console printer
      final t = FakeTransport();
      await Juju.init(JujuOptions(dsn: dsn, transport: t, storage: MemoryStorage(), tracing: false));
      FlutterError.reportError(FlutterErrorDetails(exception: StateError('broken build')));
      await Juju.flush();
      expect(sessionOf(t.ofType('session').last), allOf(containsPair('status', 'crashed'), containsPair('errors', 1)));
      Juju.close();
      FlutterError.onError = saved;
    });

    testWidgets('pushing a screen times its load, with its requests as spans', (tester) async {
      final t = FakeTransport();
      await tester.runAsync(() => Juju.init(JujuOptions(dsn: dsn, transport: t, storage: MemoryStorage(), sessions: false)));
      final http.Client api = Juju.httpClient(MockClient((r) async => http.Response('{}', 200)));
      final navKey = GlobalKey<NavigatorState>();
      await tester.pumpWidget(MaterialApp(
        navigatorKey: navKey,
        navigatorObservers: [Juju.navigatorObserver],
        routes: {'/': (_) => const Text('home'), '/cart': (_) => const Text('cart')},
      ));
      navKey.currentState!.pushNamed('/cart');
      await tester.runAsync(() => api.get(Uri.parse('https://shop.example.com/api/cart?session=secret')));
      await tester.pumpAndSettle();
      await tester.pump(const Duration(seconds: 2)); // a second with no requests: the load is done
      await tester.runAsync(Juju.flush);
      final loads = t.ofType('transaction').where((e) => e['transaction']['op'] == 'screen.load').toList();
      expect(loads, hasLength(1));
      expect(loads.single['transaction']['name'], '/cart');
      final span = (loads.single['transaction']['spans'] as List).single as Map;
      expect(span, allOf(containsPair('op', 'http.client'), containsPair('description', 'GET https://shop.example.com/api/cart')));
      Juju.close();
    });
  });

  group('sampleRate (errors)', () {
    test('sends the sampled share of errors, but the session counts them all', () {
      fake((async) {
        final t = FakeTransport();
        // Rolls below 0.25 are kept: two of these four.
        final c =
            JujuClient(ClientOptions(dsn: dsn, transport: t, release: 'shop@1.0', sampleRate: 0.25, random: _Rolls([0.1, 0.5, 0.24, 0.9])))
              ..startSession();
        final ids = [for (var i = 1; i <= 4; i++) c.captureException(StateError('e$i'), StackTrace.current)];
        c.endSession();
        c.flush();
        async.flushMicrotasks();
        expect(ids.map((id) => id.isNotEmpty), [true, false, true, false]);
        expect(t.ofType('error').map((e) => (e['exception'] as Map)['message']), ['Bad state: e1', 'Bad state: e3']);
        // The crash-free rate comes from sessions, so they must not be sampled.
        expect(sessionOf(t.ofType('session').last)['errors'], 4);
      });
    });
  });
}

/// Returns the given rolls from nextDouble, in order; ids stay random.
class _Rolls implements Random {
  _Rolls(this.rolls);
  final List<double> rolls;
  final _ids = Random(1);
  var _i = 0;
  @override
  double nextDouble() => rolls[_i++ % rolls.length];
  @override
  int nextInt(int max) => _ids.nextInt(max);
  @override
  bool nextBool() => _ids.nextBool();
}
