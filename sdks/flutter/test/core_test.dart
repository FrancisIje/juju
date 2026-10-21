import 'dart:convert';
import 'dart:io';
import 'dart:async';
import 'dart:math';

import 'package:clock/clock.dart';
import 'package:fake_async/fake_async.dart';
import 'package:flutter_test/flutter_test.dart';
import 'package:http/http.dart' as http;
import 'package:http/testing.dart';
import 'package:juju_flutter/juju_flutter.dart';
import 'package:juju_flutter/src/core/transport.dart' show classify, parseRetryAfter;

const dsn = 'https://pk_abc@juju.example.com/1';

StackTrace fixture(String name) => StackTrace.fromString(File('test/fixtures/$name').readAsStringSync());

/// Records every envelope and answers from a script.
class FakeTransport implements Transport {
  FakeTransport([List<SendResult>? answers]) : _answers = answers ?? [];
  final List<SendResult> _answers;
  final sent = <Map<String, dynamic>>[];

  @override
  Future<SendResult> send(Map<String, dynamic> envelope) async {
    sent.add(jsonDecode(jsonEncode(envelope)) as Map<String, dynamic>); // a deep copy
    return _answers.isEmpty ? const Delivered() : _answers.removeAt(0);
  }

  List<Map<String, dynamic>> get events => [for (final env in sent) ...(env['events'] as List).cast<Map<String, dynamic>>()];

  /// The error events among events of every type (sessions share the queue).
  List<Map<String, dynamic>> get errors => [
        for (final e in events)
          if (e['type'] == 'error') e
      ];
  List<Map<String, dynamic>> ofType(String type) => [
        for (final e in events)
          if (e['type'] == type) e
      ];
}

class MemoryStorage implements QueueStorage {
  MemoryStorage([List<JsonMap>? initial]) : stored = initial ?? [];
  List<JsonMap> stored;
  int saves = 0;

  @override
  Future<List<JsonMap>> load() async => [for (final e in stored) Map.of(e)];

  @override
  Future<void> save(List<JsonMap> events) async {
    saves++;
    stored = [for (final e in events) Map.of(e)];
  }
}

// random() = 0.999… removes jitter, so backoff delays are exact in tests.
class _NoJitter implements Random {
  @override
  double nextDouble() => 0.9999999;
  @override
  int nextInt(int max) => 7 % max;
  @override
  bool nextBool() => true;
}

JujuClient client(Transport t,
        {QueueStorage? storage, int maxBatchSize = 20, int maxQueueSize = 100, JsonMap? Function(JsonMap)? beforeSend}) =>
    JujuClient(ClientOptions(
        dsn: dsn,
        transport: t,
        storage: storage,
        maxBatchSize: maxBatchSize,
        maxQueueSize: maxQueueSize,
        beforeSend: beforeSend,
        random: _NoJitter()));

void main() {
  group('Dsn', () {
    test('splits a DSN into what the transport needs', () {
      final d = Dsn.parse('http://pk_abc@localhost:8080/42/');
      expect(d.publicKey, 'pk_abc');
      expect(d.endpoint.toString(), 'http://localhost:8080/api/v1/projects/42/events');
    });

    for (final bad in ['pk_abc', 'https://juju.example.com/1', 'https://pk_abc@juju.example.com/', 'ftp://pk_abc@host/1']) {
      test('rejects "$bad"', () => expect(() => Dsn.parse(bad), throwsFormatException));
    }
  });

  group('parseStackTrace', () {
    test('parses a real Dart VM trace, skipping async gaps', () {
      final p = parseStackTrace(fixture('dart_vm_symbolic.txt'), makeIsInApp());
      expect(p.symbolic, isTrue);
      expect(p.frames.map((f) => f.function), ['CartPage.total', 'CartPage.build', 'checkout', 'main']);
      expect(p.frames.first.toJson(),
          {'function': 'CartPage.total', 'filename': 'file:///Users/dev/shop/bin/shop.dart', 'lineno': 5, 'colno': 76, 'in_app': true});
    });

    test("marks the app's packages as app code, and Flutter and dart: as not", () {
      final p = parseStackTrace(fixture('flutter_framework_error.txt'), makeIsInApp());
      expect([for (final f in p.frames) f.inApp], [true, false, false, false, false, false, true]);
      expect(p.frames.last.function, 'main.<anonymous closure>');
    });

    test('appPackages limits app code to the named packages', () {
      final p = parseStackTrace(
          StackTrace.fromString('#0 a (package:shop/a.dart:1:1)\n#1 b (package:some_lib/b.dart:2:2)'), makeIsInApp({'shop'}));
      expect([for (final f in p.frames) f.inApp], [true, false]);
    });

    test('keeps the build id of a real non-symbolic (--split-debug-info) trace', () {
      final p = parseStackTrace(fixture('dart_aot_non_symbolic.txt'), makeIsInApp());
      expect(p.symbolic, isFalse);
      expect(p.buildId, 'b4485c2e0287804e97e466d035d1c91c');
      expect(p.frames, hasLength(3));
      expect(p.frames.first.function, '_kDartIsolateSnapshotInstructions+0x34c67');
    });

    test('handles a missing stack', () => expect(parseStackTrace(null, makeIsInApp()).frames, isEmpty));
  });

  group('the status code contract', () {
    test('classifies responses', () {
      expect(classify(202, null), isA<Delivered>());
      expect(classify(400, null), isA<Rejected>());
      expect(classify(401, null), isA<Rejected>());
      expect(classify(503, null), isA<RetryLater>());
      expect((classify(429, '5') as RetryLater).retryAfter, const Duration(seconds: 5));
    });

    test('reads Retry-After as seconds or an HTTP date', () {
      withClock(Clock.fixed(DateTime.utc(2026, 10, 19, 12)), () {
        expect(parseRetryAfter('30'), const Duration(seconds: 30));
        expect(parseRetryAfter('Mon, 19 Oct 2026 12:00:10 GMT'), const Duration(seconds: 10));
        expect(parseRetryAfter('soon'), isNull);
      });
    });
  });

  group('HttpTransport', () {
    final d = Dsn.parse(dsn);

    test('posts JSON with the key header', () async {
      late http.Request seen;
      final t = HttpTransport(d, client: MockClient((r) async => (seen = r, http.Response('{}', 202)).$2));
      expect(await t.send({'sdk': {}, 'events': []}), isA<Delivered>());
      expect(seen.url.toString(), 'https://juju.example.com/api/v1/projects/1/events');
      expect(seen.headers['X-Juju-Key'], 'pk_abc');
      expect(seen.headers.containsKey('Content-Encoding'), isFalse); // small bodies aren't compressed
    });

    test('gzips bodies over 1 KB, and they decompress to the same JSON', () async {
      late http.Request seen;
      final t = HttpTransport(d, client: MockClient((r) async => (seen = r, http.Response('{}', 202)).$2));
      final env = {
        'events': List.generate(
            20,
            (i) => {
                  'event_id': '$i',
                  'exception': {'type': 'TypeError', 'message': 'Null check operator used on a null value'}
                })
      };
      await t.send(env);
      expect(seen.headers['Content-Encoding'], 'gzip');
      expect(jsonDecode(utf8.decode(gzip.decode(seen.bodyBytes))), env);
      expect(seen.bodyBytes.length, lessThan(utf8.encode(jsonEncode(env)).length ~/ 3));
    });

    test('treats a network failure as retryable', () async {
      final t = HttpTransport(d, client: MockClient((r) async => throw const SocketException('connection refused')));
      expect(await t.send({}), isA<RetryLater>());
    });
  });

  group('batching and retries', () {
    test('collects events for 5 s, then sends one request', () {
      fakeAsync((async) {
        final t = FakeTransport();
        final c = client(t);
        c.captureException(StateError('a'), null);
        c.captureException(StateError('b'), null);
        async.elapse(const Duration(milliseconds: 4999));
        expect(t.sent, isEmpty);
        async.elapse(const Duration(milliseconds: 1));
        expect(t.sent, hasLength(1));
        expect(t.events.map((e) => e['exception']['message']), ['Bad state: a', 'Bad state: b']);
      });
    });

    test('backs off 1 s, 2 s, 4 s, resending the same event_id', () {
      fakeAsync((async) {
        const fail = RetryLater(status: 503);
        final t = FakeTransport([fail, fail, fail]);
        final c = client(t);
        final id = c.captureException(StateError('x'), null);
        c.flush();
        async.flushMicrotasks();
        async.elapse(const Duration(milliseconds: 999));
        expect(t.sent, hasLength(1));
        async.elapse(const Duration(milliseconds: 1));
        expect(t.sent, hasLength(2));
        async.elapse(const Duration(seconds: 2));
        expect(t.sent, hasLength(3));
        async.elapse(const Duration(seconds: 4));
        expect(t.sent, hasLength(4));
        expect(c.pending, 0);
        expect(t.events.map((e) => e['event_id']).toSet(), {id}); // one id on every attempt: the server dedupes
      });
    });

    test('waits at least Retry-After, even past an earlier batch timer', () {
      fakeAsync((async) {
        final t = FakeTransport([const RetryLater(status: 429, retryAfter: Duration(seconds: 10))]);
        final c = client(t);
        c.captureException(StateError('x'), null); // sets the 5 s batch timer
        c.flush();
        async.flushMicrotasks();
        async.elapse(const Duration(milliseconds: 9999));
        expect(t.sent, hasLength(1));
        async.elapse(const Duration(milliseconds: 1));
        expect(t.sent, hasLength(2));
      });
    });

    test('drops events the server rejects, instead of retrying forever', () {
      fakeAsync((async) {
        final t = FakeTransport([const Rejected(400)]);
        final c = client(t);
        c.captureException(StateError('x'), null);
        c.flush();
        async.elapse(const Duration(minutes: 5));
        expect(t.sent, hasLength(1));
        expect(c.pending, 0);
      });
    });

    test('retryNow skips the remaining backoff', () {
      fakeAsync((async) {
        final t = FakeTransport([const RetryLater(), const RetryLater()]);
        final c = client(t);
        c.captureException(StateError('x'), null);
        c.flush();
        async.elapse(const Duration(seconds: 1)); // second failure; next try in 2 s
        c.retryNow();
        async.elapse(Duration.zero);
        expect(t.sent, hasLength(3));
      });
    });
  });

  group('events', () {
    test('carry type, message, frames, tags and breadcrumbs', () async {
      final t = FakeTransport();
      final c = JujuClient(ClientOptions(
          dsn: dsn,
          transport: t,
          release: 'shop@1.4.0+12',
          environment: 'staging',
          context: () => {
                'os': {'name': 'android'}
              }));
      c.setUser({'id': 'u_1'});
      c.addBreadcrumb('navigation', message: '/ → /cart');
      c.captureException(StateError('no element'), fixture('flutter_framework_error.txt'), mechanism: 'flutter_error');
      await c.flush();
      final e = t.events.single;
      expect(e['platform'], 'dart');
      expect(e['release'], 'shop@1.4.0+12');
      expect(e['exception']['type'], 'StateError');
      expect(e['exception']['message'], 'Bad state: no element');
      expect(e['exception']['stacktrace']['frames'][0]['function'], 'CartPage.build');
      expect(e['tags'], {'mechanism': 'flutter_error'});
      expect(e['user'], {'id': 'u_1'});
      expect(e['breadcrumbs'], hasLength(1));
      expect(e['event_id'], matches(RegExp(r'^[0-9a-f]{8}-[0-9a-f]{4}-4[0-9a-f]{3}-[89ab][0-9a-f]{3}-[0-9a-f]{12}$')));
      expect(t.sent.single['sdk'], {'name': 'juju.dart', 'version': '0.1.0'});
    });

    test("strip Dart's 'Exception: ' prefix and private underscores", () async {
      final t = FakeTransport();
      final c = client(t);
      c.captureException(Exception('payment failed'), null);
      c.captureException(TypeError(), null);
      await c.flush();
      expect(t.events[0]['exception']['message'], 'payment failed'); // not "Exception: payment failed"
      expect(t.events[1]['exception']['type'], isNot(startsWith('_')));
    });

    test("don't repeat the type in the message", () async {
      final t = FakeTransport();
      final c = client(t);
      try {
        jsonDecode('{not json');
      } catch (e, s) {
        c.captureException(e, s);
      }
      await c.flush();
      expect(t.events.single['exception']['type'], 'FormatException');
      expect(t.events.single['exception']['message'], startsWith('Unexpected character'));
    });

    test("cap deep stacks the way the server would, so phones don't upload frames it drops", () async {
      final t = FakeTransport();
      final c = client(t);
      final deep = StackTrace.fromString([for (var i = 0; i < 335; i++) '#$i      f$i (package:shop/a.dart:$i:1)'].join('\n'));
      c.captureException(StateError('deep'), deep);
      await c.flush();
      final frames = t.events.single['exception']['stacktrace']['frames'] as List;
      expect(frames, hasLength(250));
      expect(frames[49]['function'], 'f49');
      expect(frames[50]['function'], 'f135');
      expect(frames.last['function'], 'f334');
    });

    test('keep the raw trace and build id when the trace is non-symbolic', () async {
      final t = FakeTransport();
      final c = client(t);
      c.captureException(StateError('x'), fixture('dart_aot_non_symbolic.txt'));
      await c.flush();
      expect(t.events.single['context']['dart_stack']['build_id'], 'b4485c2e0287804e97e466d035d1c91c');
    });

    test('the same error object is reported once', () {
      final c = client(FakeTransport());
      final err = StateError('caught, reported, rethrown');
      expect(c.captureException(err, null), isNotEmpty);
      expect(c.captureException(err, null), isEmpty);
      expect(c.captureException('a string', null), isNotEmpty);
      expect(c.captureException('a string', null), isNotEmpty); // values have no identity: never deduplicated
    });

    test('beforeSend can drop events, and a throwing one never escapes', () {
      final c = client(FakeTransport(), beforeSend: (e) => e['exception']['message'].contains('ignore') ? null : e);
      expect(c.captureException(StateError('ignore me'), null), isEmpty);
      expect(c.captureException(StateError('keep'), null), isNotEmpty);
      final bad = client(FakeTransport(), beforeSend: (_) => throw StateError('bug in app code'));
      expect(() => bad.captureException(StateError('x'), null), returnsNormally);
    });

    test('the offline queue drops new events once full', () {
      final c = client(FakeTransport(), maxQueueSize: 3);
      for (var i = 0; i < 5; i++) {
        c.captureException(StateError('$i'), null);
      }
      expect(c.pending, 3);
    });

    test('keeps only the last 100 breadcrumbs', () {
      final c = client(FakeTransport());
      for (var i = 0; i < 150; i++) {
        c.addBreadcrumb('x', message: '$i');
      }
      expect(c.breadcrumbs, hasLength(100));
      expect(c.breadcrumbs.first['message'], '50');
    });
  });

  group('saved queue', () {
    test('a later launch sends what an earlier one saved', () async {
      final disk = MemoryStorage();
      final first = client(FakeTransport([const RetryLater()]), storage: disk);
      await first.ready;
      final id = first.captureException(StateError('killed while offline'), null);
      await first.persistNow();
      first.close();

      final t = FakeTransport();
      final second = client(t, storage: MemoryStorage(disk.stored));
      await second.ready;
      await second.flush();
      expect(t.events.map((e) => e['event_id']), [id]);
    });

    test("doesn't wipe saved events when an error happens before loading finishes", () async {
      final old = {
        'event_id': '11111111-1111-4111-8111-111111111111',
        'type': 'error',
        'exception': {'type': 'E', 'message': 'from last launch'}
      };
      final disk = _SlowStorage([old]);
      final c = client(FakeTransport([const RetryLater()]), storage: disk);
      c.captureException(StateError('right at launch'), null); // before load() returns
      await Future<void>.delayed(Duration.zero);
      expect(disk.stored, [old]); // the save waits for the load
      disk.finishLoad();
      await c.persistNow();
      expect(disk.stored.map((e) => e['exception']['message']), ['from last launch', 'Bad state: right at launch']);
    });

    test('removes events from storage once the server has them', () async {
      final disk = MemoryStorage();
      final c = client(FakeTransport(), storage: disk);
      c.captureException(StateError('x'), null);
      await c.flush();
      await c.persistNow();
      expect(disk.stored, isEmpty);
    });

    test('coalesces a burst of errors into a few writes', () async {
      final disk = MemoryStorage();
      final c = client(FakeTransport([const RetryLater()]), storage: disk);
      for (var i = 0; i < 50; i++) {
        c.captureException(StateError('$i'), null);
      }
      await c.persistNow();
      expect(disk.saves, lessThan(5));
      expect(disk.stored, hasLength(50));
    });
  });
}

class _SlowStorage implements QueueStorage {
  _SlowStorage(this.stored);
  List<JsonMap> stored;
  final _load = Completer<List<JsonMap>>();
  void finishLoad() => _load.complete([for (final e in stored) Map.of(e)]);

  @override
  Future<List<JsonMap>> load() => _load.future;

  @override
  Future<void> save(List<JsonMap> events) async => stored = [for (final e in events) Map.of(e)];
}
