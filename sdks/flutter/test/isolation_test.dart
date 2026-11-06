// The promise this file protects: whatever happens to juju's server or to the
// SDK itself, the app that hosts it carries on. A monitoring tool that can
// take the app down is worse than no monitoring tool.
//
// Faults are injected on purpose: a clock and a random source that throw stand
// in for "a bug somewhere in the SDK", since they sit under nearly every code path.

import 'dart:async';
import 'dart:convert';
import 'dart:math';

import 'package:clock/clock.dart';
import 'package:fake_async/fake_async.dart';
import 'package:flutter/material.dart';
import 'package:flutter_test/flutter_test.dart';
import 'package:http/http.dart' as http;
import 'package:http/testing.dart';
import 'package:juju_flutter/juju_flutter.dart';
import 'package:juju_flutter/src/core/transport.dart' show parseRetryAfter;
import 'package:juju_flutter/src/flutter/breadcrumbs.dart' show JujuLifecycleObserver;

const dsn = 'https://pk_abc@juju.example.com/1';

class _Scripted implements Transport {
  _Scripted(this.answer);
  final SendResult Function() answer;
  final sent = <Map<String, dynamic>>[];
  final at = <DateTime>[];
  void Function()? onSend;

  @override
  Future<SendResult> send(Map<String, dynamic> envelope) async {
    at.add(clock.now());
    sent.add(jsonDecode(jsonEncode(envelope)) as Map<String, dynamic>);
    onSend?.call();
    return answer();
  }
}

class _Memory implements QueueStorage {
  _Memory([List<JsonMap>? initial]) : stored = initial ?? [];
  List<JsonMap> stored;
  @override
  Future<List<JsonMap>> load() async => stored;
  @override
  Future<void> save(List<JsonMap> events) async => stored = jsonDecode(jsonEncode(events)).cast<JsonMap>();
}

class _HangingStorage implements QueueStorage {
  @override
  Future<List<JsonMap>> load() => Completer<List<JsonMap>>().future;
  @override
  Future<void> save(List<JsonMap> events) => Completer<void>().future;
}

class _ThrowingStorage implements QueueStorage {
  @override
  Future<List<JsonMap>> load() async => throw const FormatException('disk broke');
  @override
  Future<void> save(List<JsonMap> events) async => throw StateError('disk full');
}

class _ThrowingIdentity implements IdentityStorage {
  @override
  Future<Map<String, String>> load() async => throw const FormatException('disk broke');
  @override
  Future<void> save(Map<String, String> ids) async => throw StateError('disk full');
}

class _HangingIdentity implements IdentityStorage {
  @override
  Future<Map<String, String>> load() => Completer<Map<String, String>>().future;
  @override
  Future<void> save(Map<String, String> ids) => Completer<void>().future;
}

class _CountingStorage implements QueueStorage {
  _CountingStorage(this.onSave);
  final void Function() onSave;
  @override
  Future<List<JsonMap>> load() async => [];
  @override
  Future<void> save(List<JsonMap> events) async => onSave();
}

class _BrokenRandom implements Random {
  @override
  double nextDouble() => throw StateError('random broke');
  @override
  int nextInt(int max) => throw StateError('random broke');
  @override
  bool nextBool() => throw StateError('random broke');
}

class _ExplodingToString {
  @override
  String toString() => throw StateError('toString broke');
}

final _brokenClock = Clock(() => throw StateError('clock broke'));

JujuClient _client(Transport t, {QueueStorage? storage, Random? random}) => JujuClient(ClientOptions(
      dsn: dsn,
      transport: t,
      storage: storage,
      random: random ?? Random(1),
    ));

/// Every public call an app can make on a client, in the order an app might.
void _useEverything(JujuClient c) {
  c.startSession();
  c.addBreadcrumb('app', message: 'hello');
  c.captureException(StateError('boom'), StackTrace.current);
  c.capture('checkout');
  c.identify('u1');
  c.setUser({'id': 'u1'});
  c.setTag('a', 'b');
  c.reset();
  c.pauseSession();
  c.resumeSession();
  c.startSpan('http').finish();
  final tx = c.startTransaction('checkout', 'custom');
  tx.startChild('db').finish();
  tx.finish();
  c.retryNow();
  c.endSession();
  c.persistNow();
  c.flush();
  c.close();
}

void main() {
  TestWidgetsFlutterBinding.ensureInitialized();

  tearDown(Juju.close);

  group('a long server outage', () {
    // The delay was 1 s * 2^(failures-1), and Dart ints wrap at 64 bits. After
    // about 45 failures (30 minutes of outage) it wrapped negative, then to 0.
    test('retries stay spaced out however long it lasts', () {
      fakeAsync((async) {
        final t = _Scripted(() => const RetryLater());
        final c = _client(t);
        // Without this cap a broken backoff retries at zero delay, and fake
        // time never advances: the test would spin forever instead of failing.
        t.onSend = () {
          if (t.at.length > 2000) c.close();
        };
        c.captureException(StateError('boom'), StackTrace.current);
        async.elapse(const Duration(hours: 6));

        // 60 s cap, times jitter of at least 0.5: at most one try per 30 s.
        expect(t.at.length, lessThan(6 * 60 * 2 + 10), reason: 'six hours of outage must be a few hundred tries');
        for (var i = 8; i < t.at.length; i++) {
          final gap = t.at[i].difference(t.at[i - 1]);
          expect(gap, greaterThanOrEqualTo(const Duration(seconds: 29)), reason: 'try $i came after only $gap');
          expect(gap, lessThanOrEqualTo(const Duration(seconds: 61)), reason: 'try $i came after $gap');
        }
        c.close();
      });
    });

    test('a nonsense Retry-After can neither throw nor stop sending for good', () {
      for (final header in ['9000000000000', '9223372036854775807', '86400000']) {
        fakeAsync((async) {
          var calls = 0;
          final t = _Scripted(() => calls++ == 0 ? RetryLater(status: 429, retryAfter: parseRetryAfter(header)) : const Delivered());
          final errors = <Object>[];
          runZonedGuarded(() {
            final c = _client(t);
            c.captureException(StateError('boom'), StackTrace.current);
            async.elapse(const Duration(hours: 2));
            c.close();
          }, (e, _) => errors.add(e));
          async.flushMicrotasks();
          expect(errors, isEmpty, reason: 'Retry-After: $header');
          expect(t.sent.length, 2, reason: 'Retry-After: $header must still retry within the hour');
        });
      }
    });

    test('server answers 500 forever: the queue stays bounded', () {
      fakeAsync((async) {
        final t = _Scripted(() => const RetryLater(status: 500));
        final c = _client(t);
        for (var i = 0; i < 5000; i++) {
          c.captureException(StateError('e$i'), StackTrace.current);
          c.capture('tap');
        }
        async.elapse(const Duration(minutes: 5));
        expect(c.pending, lessThanOrEqualTo(1100), reason: '100 errors + 1000 others');
        c.close();
      });
    });
  });

  group('data the app passes in', () {
    final circular = <String, dynamic>{};
    circular['self'] = circular;

    Future<(JujuClient, List<Map<String, dynamic>>)> real() async {
      final bodies = <Map<String, dynamic>>[];
      final mock = MockClient((req) async {
        bodies.add(jsonDecode(req.body) as Map<String, dynamic>);
        return http.Response('', 202);
      });
      final c = JujuClient(ClientOptions(dsn: dsn, transport: HttpTransport(Dsn.parse(dsn), client: mock, compress: false)));
      await c.ready;
      return (c, bodies);
    }

    final bad = <String, Object Function()>{
      'a DateTime': () => DateTime.utc(2026),
      'an object': Object.new,
      'NaN': () => double.nan,
      'infinity': () => double.infinity,
      'a map with int keys': () => {1: 'x'},
      'an object whose toString throws': _ExplodingToString.new,
      'a circular map': () => circular,
    };

    // jsonEncode throws on these, at send time. The failure looked like a
    // network error, so the batch was retried forever and blocked the queue.
    bad.forEach((name, value) {
      test('capture() with $name does not block the events behind it', () async {
        final (c, bodies) = await real();
        c.capture('bad', {'v': value()});
        c.capture('good');
        await c.distinctId;
        await c.flush();
        final names = [
          for (final b in bodies)
            for (final e in b['events'] as List) e['name']
        ];
        expect(names, contains('good'));
        expect(c.pending, 0);
        c.close();
      });
    });

    test('setUser and breadcrumbs with bad values do not block error reports', () async {
      final (c, bodies) = await real();
      c.setUser({'joined': DateTime.utc(2026)});
      c.addBreadcrumb('x', data: {'v': double.nan});
      c.captureException(StateError('boom'), StackTrace.current);
      await c.flush();
      expect(bodies.single['events'], hasLength(1));
      expect(c.pending, 0);
      c.close();
    });

    test('a bad value does not stop the queue being saved for the next launch', () async {
      final store = _Memory();
      final c = _client(_Scripted(() => const RetryLater()), storage: store);
      await c.ready;
      c.capture('bad', {'v': DateTime.utc(2026)});
      await c.distinctId;
      c.captureException(StateError('saved'), StackTrace.current);
      await c.persistNow();
      expect(store.stored.where((e) => e['type'] == 'error'), hasLength(1));
      c.close();
    });
  });

  group('resources the SDK uses', () {
    test('a burst of events costs a few saves, not one each', () {
      fakeAsync((async) {
        var saves = 0;
        final store = _CountingStorage(() => saves++);
        final c = _client(_Scripted(() => const RetryLater()), storage: store);
        async.flushMicrotasks();
        for (var i = 0; i < 200; i++) {
          c.captureException(StateError('e$i'), StackTrace.current);
          async.elapse(const Duration(milliseconds: 10));
          c.capture('tap');
        }
        async.elapse(const Duration(seconds: 3));
        // 2 s of burst, at most one save per 2 s, plus the trailing one.
        expect(saves, lessThanOrEqualTo(4));
        expect(saves, greaterThanOrEqualTo(1));
        c.close();
      });
    });

    test('a lone error is saved at once, and the last events of a burst get saved', () {
      fakeAsync((async) {
        final store = _Memory();
        final c = _client(_Scripted(() => const RetryLater()), storage: store);
        async.flushMicrotasks();
        c.captureException(StateError('first'), StackTrace.current);
        async.flushMicrotasks();
        expect(store.stored, hasLength(1), reason: 'a crash right after must not lose it');
        c.captureException(StateError('second'), StackTrace.current);
        async.flushMicrotasks();
        expect(store.stored, hasLength(1), reason: 'too soon: coalesced into the next save');
        async.elapse(const Duration(seconds: 2));
        expect(store.stored, hasLength(2));
        c.close();
      });
    });

    test('close saves a save that was still waiting', () {
      fakeAsync((async) {
        final store = _Memory();
        final c = _client(_Scripted(() => const RetryLater()), storage: store);
        async.flushMicrotasks();
        c.captureException(StateError('first'), StackTrace.current);
        c.captureException(StateError('second'), StackTrace.current);
        c.close();
        async.flushMicrotasks();
        expect(store.stored, hasLength(2));
      });
    });

    test('a huge exception message is cut before it is queued', () async {
      final t = _Scripted(() => const Delivered());
      final c = _client(t);
      c.captureException(Exception('x' * 5000000), null);
      await c.flush();
      final message = t.sent.single['events'][0]['exception']['message'] as String;
      expect(message.length, 8192);
      c.close();
    });

    test('an event over the server limit is dropped, and the next one is kept', () async {
      final t = _Scripted(() => const Delivered());
      final c = _client(t);
      c.capture('huge', {'v': 'x' * (2 << 20)});
      c.capture('small');
      await c.distinctId;
      await c.flush();
      final names = [for (final e in t.sent.single['events'] as List) e['name']];
      expect(names, ['small']);
      c.close();
    });

    test('big events cannot use more than the memory limit, and errors keep their own', () async {
      final c = _client(_Scripted(() => const RetryLater()));
      for (var i = 0; i < 40; i++) {
        c.capture('big', {'v': 'x' * (500 << 10)});
      }
      await c.distinctId;
      expect(c.pending, lessThanOrEqualTo(17), reason: '8 MB of 500 KB events');
      expect(c.captureException(StateError('still room for errors'), null), isNotEmpty);
      c.close();
    });

    test('a full error queue skips parsing the stack', () {
      var contextCalls = 0;
      final c = JujuClient(ClientOptions(
        dsn: dsn,
        transport: _Scripted(() => const RetryLater()),
        maxQueueSize: 2,
        context: () {
          contextCalls++;
          return {};
        },
      ));
      for (var i = 0; i < 10; i++) {
        c.captureException(StateError('e$i'), StackTrace.current);
      }
      expect(contextCalls, 2);
      c.close();
    });

    test('identity storage that never answers cannot hold events in memory', () async {
      final t = _Scripted(() => const Delivered());
      final c = JujuClient(ClientOptions(dsn: dsn, transport: t, identityStorage: _HangingIdentity()));
      c.capture('tap');
      await c.distinctId; // completes in spite of the storage
      await c.flush();
      expect(t.sent.expand((e) => e['events'] as List).map((e) => e['name']), contains('tap'));
      c.close();
    }, timeout: const Timeout(Duration(seconds: 15)));
  });

  group('bad data is kept as far as it can be', () {
    test('a value JSON cannot hold arrives as text, not lost', () async {
      final t = _Scripted(() => const Delivered());
      final c = _client(t);
      c.capture('bad', {'when': DateTime.utc(2026), 'ratio': double.nan});
      await c.distinctId;
      await c.flush();
      final props = t.sent.single['events'][0]['properties'] as Map;
      expect(props['when'], contains('2026'));
      expect(props['ratio'], 'NaN');
      c.close();
    });

    test('a circular user or breadcrumb is left out, and errors still get reported', () async {
      final circular = <String, dynamic>{};
      circular['self'] = circular;
      final t = _Scripted(() => const Delivered());
      final c = _client(t);
      c.setUser(circular);
      c.addBreadcrumb('x', data: circular);
      expect(c.captureException(StateError('boom'), null), isNotEmpty);
      await c.flush();
      expect(t.sent.single['events'], hasLength(1));
      c.close();
    });
  });

  group('faults inside the SDK', () {
    test('no client call throws when the random source breaks', () {
      _useEverything(_client(_Scripted(() => const Delivered()), random: _BrokenRandom()));
    });

    test('no client call throws when the clock breaks', () async {
      await withClock(_brokenClock, () async {
        _useEverything(_client(_Scripted(() => const Delivered())));
        await Future<void>.delayed(Duration.zero);
      });
    });

    test('a failed flush never reaches the app as an error', () async {
      final errors = <Object>[];
      await runZonedGuarded(() async {
        await withClock(_brokenClock, () async {
          final c = _client(_Scripted(() => const RetryLater()));
          await c.flush();
          c.captureException(StateError('boom'), StackTrace.current);
          await c.flush();
          c.close();
        });
      }, (e, _) => errors.add(e));
      await Future<void>.delayed(Duration.zero);
      expect(errors, isEmpty);
    });

    test('a flush that fails midway completes instead of rejecting', () async {
      // Timers call flush with nobody listening: a rejection would reach the
      // app's global error handler, looking like a bug in the app.
      final c = _client(_Scripted(() => const RetryLater()));
      c.captureException(StateError('boom'), null);
      await withClock(_brokenClock, () => expectLater(c.flush(), completes));
      c.close();
    });

    test('sessions keep working next to corrupt entries from a queue file', () async {
      final t = _Scripted(() => const RetryLater());
      final store = _Memory([
        {'event_id': 'old', 'type': 'session'},
      ]);
      final c = _client(t, storage: store);
      await c.ready;
      c.startSession();
      await c.flush();
      final sessions = t.sent.expand((e) => e['events'] as List).where((e) => e['session'] != null);
      expect(sessions, isNotEmpty, reason: 'the new session must still be queued');
      c.close();
    });

    test('a corrupt queue file does not break lifecycle changes', () async {
      // Our files are written atomically, but a backup restore, an older SDK or
      // a bad disk could bring back anything. A session entry with no session
      // once threw a TypeError out of the app's own lifecycle callback.
      final store = _Memory([
        {'event_id': 'old', 'type': 'session'},
        {'event_id': 'old2', 'type': 'session', 'session': 'not a map'},
      ]);
      final c = _client(_Scripted(() => const RetryLater()), storage: store);
      await c.ready;
      c.startSession();
      final observer = JujuLifecycleObserver(c, sessions: true);
      observer.didChangeAppLifecycleState(AppLifecycleState.paused);
      observer.didChangeAppLifecycleState(AppLifecycleState.resumed);
      c.close();
    });

    test('a bad beforeSend or error.toString only loses that one event', () async {
      final t = _Scripted(() => const Delivered());
      final c = JujuClient(ClientOptions(dsn: dsn, transport: t, beforeSend: (e) => throw StateError('hook broke')));
      expect(c.captureException(StateError('x'), null), '');
      c.close();
      final c2 = _client(t);
      expect(c2.captureException(_ExplodingToString(), null), '');
      expect(c2.captureException(StateError('still works'), null), isNotEmpty);
      c2.close();
    });

    test('a transaction handed to the app keeps working when the SDK breaks', () {
      final c = _client(_Scripted(() => const Delivered()), random: _BrokenRandom());
      final tx = c.startTransaction('checkout', 'custom');
      final span = tx.startChild('db');
      span.setData('rows', 1);
      span.finish();
      tx.setMeasurement('n', 1);
      tx.finish();
      c.close();
    });
  });

  group('Juju.init must never stop the app from starting', () {
    // A typical main(): anything thrown or hung in init means runApp never
    // runs, and the user stares at a splash screen.
    Future<bool> reachedRunApp(JujuOptions options) async {
      await Juju.init(options).timeout(const Duration(seconds: 8));
      return true;
    }

    for (final bad in ['', 'not-a-dsn', 'ftp://pk@host/1', 'https://host/1', 'https://pk@host/abc', 'https://pk@[bad/1']) {
      test('a bad DSN "$bad" leaves juju off, without throwing', () async {
        expect(await reachedRunApp(JujuOptions(dsn: bad)), isTrue);
        expect(Juju.client, isNull);
        // And the rest of the API is a harmless no-op.
        expect(Juju.captureException(StateError('x')), '');
        Juju.capture('x');
        await Juju.flush();
      });
    }

    test('queue storage that never answers delays start by a bounded time', () async {
      final watch = Stopwatch()..start();
      expect(await reachedRunApp(JujuOptions(dsn: dsn, storage: _HangingStorage(), identityStorage: _ThrowingIdentity(), tracing: false)),
          isTrue);
      expect(watch.elapsed, lessThan(const Duration(seconds: 3)));
    });

    test('with hanging storage, juju still reports errors', () async {
      final t = _Scripted(() => const Delivered());
      await Juju.init(
          JujuOptions(dsn: dsn, transport: t, storage: _HangingStorage(), identityStorage: _ThrowingIdentity(), tracing: false));
      Juju.captureException(StateError('boom'));
      await Juju.flush();
      expect(t.sent, isNotEmpty);
    });

    test('storage that throws is ignored', () async {
      expect(await reachedRunApp(JujuOptions(dsn: dsn, storage: _ThrowingStorage(), identityStorage: _ThrowingIdentity(), tracing: false)),
          isTrue);
    });

    test('the default storage without its plugin is ignored', () async {
      // path_provider has no implementation in a test, like a missing plugin in an app.
      expect(await reachedRunApp(JujuOptions(dsn: dsn, transport: _Scripted(() => const Delivered()), tracing: false)), isTrue);
    });

    test('an internal fault during init leaves juju off and the hooks undone', () async {
      final before = FlutterError.onError;
      final ok = await withClock(
          _brokenClock, () => reachedRunApp(JujuOptions(dsn: dsn, transport: _Scripted(() => const Delivered()), storage: _Memory())));
      expect(ok, isTrue);
      expect(FlutterError.onError, same(before));
    });
  });

  group('the app\'s own errors still reach the app', () {
    test('FlutterError.onError calls the previous handler, even if juju is broken', () async {
      final seen = <Object>[];
      final previous = FlutterError.onError;
      FlutterError.onError = (d) => seen.add(d.exception);
      addTearDown(() => FlutterError.onError = previous);
      await Juju.init(JujuOptions(
          dsn: dsn,
          transport: _Scripted(() => const Delivered()),
          storage: _Memory(),
          tracing: false,
          beforeSend: (_) => throw StateError('hook broke')));
      withClock(_brokenClock, () => FlutterError.onError!(FlutterErrorDetails(exception: StateError('app error'))));
      expect(seen, hasLength(1));
    });
  });

  group('instrumentation in the app\'s path', () {
    // These wrap the app's real work. If recording fails, the work must not.
    JujuClient? broken() => throw StateError('juju broke');

    test('a request succeeds when recording it fails', () async {
      final api = JujuHttpClient(broken, MockClient((_) async => http.Response('ok', 200)));
      final res = await api.get(Uri.parse('https://api.example.com/cart?token=secret'));
      expect(res.body, 'ok');
    });

    test('a failed request gives the app its own error, not juju\'s', () async {
      final api = JujuHttpClient(broken, MockClient((_) async => throw http.ClientException('no network')));
      await expectLater(api.get(Uri.parse('https://api.example.com/cart')), throwsA(isA<http.ClientException>()));
    });

    testWidgets('navigation works when recording it fails', (tester) async {
      final key = GlobalKey<NavigatorState>();
      await tester.pumpWidget(MaterialApp(
        navigatorKey: key,
        navigatorObservers: [JujuNavigatorObserver(broken, tracing: () => true)],
        home: const Text('home'),
        routes: {'/next': (_) => const Text('next')},
      ));
      key.currentState!.pushNamed('/next');
      await tester.pumpAndSettle();
      expect(find.text('next'), findsOneWidget);
      key.currentState!.pop();
      await tester.pumpAndSettle();
      expect(find.text('home'), findsOneWidget);
      expect(tester.takeException(), isNull);
    });

    testWidgets('other lifecycle observers still run when juju breaks', (tester) async {
      final c = _client(_Scripted(() => const Delivered()), random: _BrokenRandom());
      final observer = JujuLifecycleObserver(c, sessions: true);
      withClock(_brokenClock, () {
        observer.didChangeAppLifecycleState(AppLifecycleState.paused);
        observer.didChangeAppLifecycleState(AppLifecycleState.resumed);
      });
      c.close();
    });
  });

  group('Juju.runZoned', () {
    test('print inside the zone works when recording it fails', () async {
      final errors = <Object>[];
      await Juju.init(JujuOptions(dsn: dsn, transport: _Scripted(() => const Delivered()), storage: _Memory(), tracing: false));
      final done = Completer<void>();
      runZonedGuarded(() {
        Juju.runZoned(() {
          withClock(_brokenClock, () => print('app log line')); // ignore: avoid_print
          done.complete();
        });
      }, (e, _) => errors.add(e));
      await done.future;
      expect(errors, isEmpty);
    });
  });
}
