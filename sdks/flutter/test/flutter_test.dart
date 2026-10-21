import 'dart:async';
import 'dart:convert';
import 'dart:io';
import 'dart:ui' show ErrorCallback, PlatformDispatcher;

import 'package:flutter/foundation.dart';

import 'package:flutter/material.dart';
import 'package:flutter_test/flutter_test.dart';
import 'package:http/http.dart' as http;
import 'package:http/testing.dart';
import 'package:juju_flutter/juju_flutter.dart';
import 'package:juju_flutter/src/flutter/context.dart' show osContext;
import 'package:juju_flutter/src/flutter/hooks.dart';

import 'core_test.dart' show FakeTransport, MemoryStorage;

const dsn = 'https://pk_abc@juju.example.com/1';

JujuClient newClient(FakeTransport t) => JujuClient(ClientOptions(dsn: dsn, transport: t));

void main() {
  TestWidgetsFlutterBinding.ensureInitialized();

  group('FlutterError hook (framework errors)', () {
    late FlutterExceptionHandler? saved;
    setUp(() => saved = FlutterError.onError);
    tearDown(() => FlutterError.onError = saved);

    test('reports the error and still calls the previous handler', () {
      final previous = <FlutterErrorDetails>[];
      FlutterError.onError = previous.add;
      final c = newClient(FakeTransport());
      installFlutterErrorHook(c);

      FlutterError.reportError(
          FlutterErrorDetails(exception: StateError('build failed'), stack: StackTrace.current, library: 'widgets library'));
      expect(c.pending, 1);
      expect(previous, hasLength(1)); // the red/grey error screen and console output still happen
    });

    test('skips errors Flutter marks as silent', () {
      FlutterError.onError = (_) {};
      final c = newClient(FakeTransport());
      installFlutterErrorHook(c);
      FlutterError.reportError(FlutterErrorDetails(exception: StateError('quiet'), silent: true));
      expect(c.pending, 0);
    });

    test('uninstall puts the previous handler back', () {
      void previous(FlutterErrorDetails _) {}
      FlutterError.onError = previous;
      final uninstall = installFlutterErrorHook(newClient(FakeTransport()));
      expect(FlutterError.onError, isNot(previous));
      uninstall();
      expect(FlutterError.onError, previous);
    });
  });

  group('PlatformDispatcher hook (async errors)', () {
    // The real dispatcher: flutter_test's TestPlatformDispatcher silently
    // ignores writes to onError.
    final dispatcher = PlatformDispatcher.instance;
    late ErrorCallback? saved;
    setUp(() => saved = dispatcher.onError);
    tearDown(() => dispatcher.onError = saved);

    test('reports and returns the previous handler\'s answer', () {
      dispatcher.onError = (e, s) => true;
      final c = newClient(FakeTransport());
      installPlatformErrorHook(c, dispatcher);
      expect(dispatcher.onError!(StateError('unawaited future failed'), StackTrace.current), isTrue);
      expect(c.pending, 1);
    });

    test('returns false without a previous handler, so Flutter still prints it', () {
      dispatcher.onError = null;
      final c = newClient(FakeTransport());
      installPlatformErrorHook(c, dispatcher);
      expect(dispatcher.onError!(StateError('x'), StackTrace.current), isFalse);
    });
  });

  group('Juju.init end to end', () {
    late FlutterExceptionHandler? savedFlutter;
    late ErrorCallback? savedDispatcher;
    setUp(() {
      savedFlutter = FlutterError.onError;
      savedDispatcher = PlatformDispatcher.instance.onError;
    });
    tearDown(() {
      Juju.close();
      FlutterError.onError = savedFlutter;
      PlatformDispatcher.instance.onError = savedDispatcher;
    });

    test('framework, async and zone errors each arrive with their mechanism', () async {
      final t = FakeTransport();
      final done = Completer<void>();
      Juju.runZoned(() async {
        await Juju.init(JujuOptions(dsn: dsn, transport: t, storage: MemoryStorage(), release: 'shop@1.0.0+1'));
        FlutterError.reportError(FlutterErrorDetails(exception: StateError('framework')));
        PlatformDispatcher.instance.onError!(StateError('async'), StackTrace.current);
        print('about to fail in the zone'); // ignore: avoid_print
        Timer.run(() => throw StateError('zone'));
        await Future<void>.delayed(const Duration(milliseconds: 10));
        await Juju.flush();
        done.complete();
      });
      await done.future;

      final byMessage = {for (final e in t.errors) e['exception']['message']: e};
      expect(byMessage['Bad state: framework']!['tags']['mechanism'], 'flutter_error');
      expect(byMessage['Bad state: async']!['tags']['mechanism'], 'platform_dispatcher');
      expect(byMessage['Bad state: zone']!['tags']['mechanism'], 'zone');
      final zoneEvent = byMessage['Bad state: zone']!;
      expect(zoneEvent['breadcrumbs'], contains(containsPair('message', 'about to fail in the zone')));
      expect(t.sent.first['sdk']['name'], 'juju.flutter');
      expect(zoneEvent['context']['runtime']['name'], 'flutter');
      expect(zoneEvent['environment'], 'development'); // tests run in debug mode
    });

    test('calling init twice does not double the hooks', () async {
      final t = FakeTransport();
      await Juju.init(JujuOptions(dsn: dsn, transport: t, storage: MemoryStorage()));
      await Juju.init(JujuOptions(dsn: dsn, transport: t, storage: MemoryStorage()));
      FlutterError.reportError(FlutterErrorDetails(exception: StateError('once')));
      await Juju.flush();
      expect(t.errors, hasLength(1));
    });

    test('everything is a safe no-op before init', () {
      Juju.close();
      expect(Juju.captureException(StateError('early')), isEmpty);
      expect(() => Juju.addBreadcrumb('x'), returnsNormally);
    });

    test('a bad DSN fails loudly at init', () {
      expect(() => Juju.init(const JujuOptions(dsn: 'not-a-dsn')), throwsFormatException);
    });
  });

  group('breadcrumbs', () {
    tearDown(Juju.close);

    testWidgets('the navigator observer records named routes', (tester) async {
      final t = FakeTransport();
      await tester.runAsync(() => Juju.init(JujuOptions(dsn: dsn, transport: t, storage: MemoryStorage())));
      final navKey = GlobalKey<NavigatorState>();
      await tester.pumpWidget(MaterialApp(
        navigatorKey: navKey,
        navigatorObservers: [Juju.navigatorObserver],
        routes: {'/': (_) => const Text('home'), '/cart': (_) => const Text('cart')},
      ));
      navKey.currentState!.pushNamed('/cart');
      await tester.pumpAndSettle();
      navKey.currentState!.pop();
      await tester.pumpAndSettle();
      final nav = Juju.client!.breadcrumbs.where((b) => b['type'] == 'navigation').map((b) => b['message']).toList();
      expect(nav, containsAllInOrder(['/ → /cart', '/cart → /']));
      Juju.close();
    });

    test('the HTTP client records requests without query strings', () async {
      await Juju.init(JujuOptions(dsn: dsn, transport: FakeTransport(), storage: MemoryStorage()));
      final client = Juju.httpClient(MockClient((r) async => http.Response('nope', 500)));
      await client.post(Uri.parse('https://shop.example.com/api/coupon?token=secret#x'));
      final crumb = Juju.client!.breadcrumbs.singleWhere((b) => b['type'] == 'http');
      expect(crumb['message'], 'POST https://shop.example.com/api/coupon → 500');
      expect(jsonEncode(crumb), isNot(contains('secret')));
    });
  });

  group('OS context', () {
    test('parses the version on iOS', () {
      expect(osContext('ios', 'Version 26.5 (Build 23F79)'), {'name': 'ios', 'version': '26.5', 'build': 'Version 26.5 (Build 23F79)'});
    });

    test("doesn't pass off Android's build id as a version", () {
      expect(osContext('android', 'CE2A.260420.019 dev-keys'), {'name': 'android', 'build': 'CE2A.260420.019 dev-keys'});
    });
  });

  group('FileQueueStorage', () {
    late Directory dir;
    setUp(() => dir = Directory.systemTemp.createTempSync('juju_test_'));
    tearDown(() => dir.deleteSync(recursive: true));

    test('round-trips the queue and deletes the file when empty', () async {
      final s = FileQueueStorage(directory: () async => dir);
      await s.save([
        {'event_id': 'a'}
      ]);
      expect(await s.load(), [
        {'event_id': 'a'}
      ]);
      expect(File('${dir.path}/juju_queue.json.tmp').existsSync(), isFalse); // renamed into place
      await s.save([]);
      expect(File('${dir.path}/juju_queue.json').existsSync(), isFalse);
    });

    test('treats an unreadable file as empty instead of failing', () async {
      File('${dir.path}/juju_queue.json').writeAsStringSync('[{"event_id": "a", "exc');
      expect(await FileQueueStorage(directory: () async => dir).load(), isEmpty);
    });
  });
}
