import 'dart:async';
import 'dart:convert';
import 'dart:io';
import 'dart:math';

import 'package:flutter_test/flutter_test.dart';
import 'package:juju_flutter/juju_flutter.dart';

class _Transport implements Transport {
  final sent = <Map<String, dynamic>>[];
  @override
  Future<SendResult> send(Map<String, dynamic> envelope) async {
    sent.add(jsonDecode(jsonEncode(envelope)) as Map<String, dynamic>);
    return const Delivered();
  }

  List<Map<String, dynamic>> get events => [for (final env in sent) ...(env['events'] as List).cast<Map<String, dynamic>>()];
}

class _Ids implements IdentityStorage {
  _Ids([Map<String, String>? initial, this.delay = Duration.zero]) : data = {...?initial};
  final Map<String, String> data;
  final Duration delay;
  @override
  Future<Map<String, String>> load() async {
    await Future<void>.delayed(delay);
    return {...data};
  }

  @override
  Future<void> save(Map<String, String> ids) async => data
    ..clear()
    ..addAll(ids);
}

// A real (seeded) Random: ids must differ from each other here.
JujuClient _client(_Transport t, {IdentityStorage? ids, int maxQueueSize = 100, int maxAnalytics = 1000}) => JujuClient(ClientOptions(
      dsn: 'https://pk_abc@juju.example.com/1',
      transport: t,
      identityStorage: ids,
      maxQueueSize: maxQueueSize,
      maxAnalyticsQueueSize: maxAnalytics,
      context: () => {
        'os': {'name': 'android'}
      },
      random: Random(42),
    ));

Future<List<Map<String, dynamic>>> _sent(JujuClient c, _Transport t) async {
  await c.distinctId; // the identity chain has run
  await c.flush();
  return t.events;
}

void main() {
  test('capture sends the event, who did it, properties and device context', () async {
    final t = _Transport();
    final c = _client(t);
    c.capture('checkout_started', {'plan': 'pro', 'items': 3});
    final e = (await _sent(c, t)).single;
    expect(e['type'], 'event');
    expect(e['name'], 'checkout_started');
    expect(e['properties'], {'plan': 'pro', 'items': 3});
    expect(e['context'], {
      'os': {'name': 'android'}
    });
    expect(e['distinct_id'], matches(RegExp(r'^[0-9a-f-]{36}$')));
  });

  test('one install keeps one anonymous id across launches', () async {
    final ids = _Ids();
    final t1 = _Transport();
    final first = _client(t1, ids: ids)..capture('a');
    final a = (await _sent(first, t1)).single['distinct_id'];
    final t2 = _Transport();
    final second = _client(t2, ids: ids)..capture('b');
    expect((await _sent(second, t2)).single['distinct_id'], a);
  });

  test('events captured before the stored id has loaded use the stored id', () async {
    final t = _Transport();
    final c = _client(t, ids: _Ids({'anon_id': 'anon-stored', 'distinct_id': 'anon-stored'}, const Duration(milliseconds: 20)));
    c.capture('right_at_launch'); // the file is still being read
    expect((await _sent(c, t)).single['distinct_id'], 'anon-stored');
  });

  test('identify links the anonymous history once, then sends as the user', () async {
    final t = _Transport();
    final ids = _Ids();
    final c = _client(t, ids: ids);
    c.capture('viewed_pricing');
    c.identify('user_42');
    c.identify('user_42');
    c.capture('checkout_started');
    final all = await _sent(c, t);
    final anon = all[0]['distinct_id'];
    expect([for (final e in all) e['type']], ['event', 'identify', 'event']);
    expect(all[1], containsPair('anon_id', anon));
    expect(all[1], containsPair('distinct_id', 'user_42'));
    expect(all[2]['distinct_id'], 'user_42');
    expect(ids.data['distinct_id'], 'user_42');
  });

  test('switching accounts without reset does not merge two people', () async {
    final t = _Transport();
    final c = _client(t)
      ..identify('alice')
      ..identify('bob');
    final all = await _sent(c, t);
    expect(all[0].containsKey('anon_id'), isTrue);
    expect(all[1]['distinct_id'], 'bob');
    expect(all[1].containsKey('anon_id'), isFalse);
  });

  test('reset gives the next person a new anonymous id', () async {
    final t = _Transport();
    final c = _client(t)
      ..capture('before')
      ..identify('alice')
      ..reset()
      ..capture('after');
    final all = await _sent(c, t);
    final before = all.firstWhere((e) => e['name'] == 'before')['distinct_id'];
    final after = all.firstWhere((e) => e['name'] == 'after')['distinct_id'];
    expect(after, isNot('alice'));
    expect(after, isNot(before));
  });

  test('a flood of analytics events never pushes out an error', () async {
    final c = JujuClient(ClientOptions(
      dsn: 'https://pk_abc@juju.example.com/1',
      transport: _Never(),
      maxQueueSize: 2,
      maxAnalyticsQueueSize: 3,
      random: Random(1),
    ));
    for (var i = 0; i < 10; i++) {
      c.capture('click');
    }
    await c.distinctId;
    expect(c.captureException(StateError('crash'), StackTrace.current), isNotEmpty);
    expect(c.pending, 4);
  });

  test('FileIdentityStorage survives a relaunch and an unreadable file', () async {
    final dir = await Directory.systemTemp.createTemp('juju-ids');
    addTearDown(() => dir.delete(recursive: true));
    final s = FileIdentityStorage(directory: () async => dir);
    await s.save({'anon_id': 'a1', 'distinct_id': 'u1'});
    expect(await FileIdentityStorage(directory: () async => dir).load(), {'anon_id': 'a1', 'distinct_id': 'u1'});
    File('${dir.path}/juju_identity.json').writeAsStringSync('{not json');
    expect(await s.load(), isEmpty); // start fresh rather than fail
  });
}

class _Never implements Transport {
  @override
  Future<SendResult> send(Map<String, dynamic> envelope) => Completer<SendResult>().future;
}
