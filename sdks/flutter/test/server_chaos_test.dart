// Real sockets, not fakes: a misbehaving juju server, in each way a real one
// can misbehave. A fake http client can only fail the ways we imagined; dart:io
// is what actually parses a garbage response or a reset connection.
//
// This file doesn't start a Flutter test binding, because the binding replaces
// HttpClient with one that refuses every request.

import 'dart:async';
import 'dart:convert';
import 'dart:io';

import 'package:flutter_test/flutter_test.dart';
import 'package:juju_flutter/juju_flutter.dart';

const _crlf = '\r\n';

/// A server that answers each connection however [onConnect] says.
Future<ServerSocket> _serve(void Function(Socket socket) onConnect) async {
  final server = await ServerSocket.bind(InternetAddress.loopbackIPv4, 0);
  server.listen((s) {
    s.handleError((_) {});
    onConnect(s);
  });
  addTearDown(server.close);
  return server;
}

/// Answers once the request has arrived, so a reply can't race the upload.
void Function(Socket) _reply(FutureOr<void> Function(Socket) answer) => (s) {
      var answered = false;
      s.listen((_) async {
        if (answered) return;
        answered = true;
        try {
          await answer(s);
        } catch (_) {}
      }, onError: (_) {}, cancelOnError: true);
    };

void Function(Socket) _status(String status, {String headers = ''}) => _reply((s) async {
      s.write('HTTP/1.1 $status${_crlf}Content-Length: 0${_crlf}Connection: close$_crlf$headers$_crlf');
      await s.flush();
      await s.close();
    });

Map<String, dynamic> _envelope() => {
      'sdk': {'name': 'juju.test', 'version': '0'},
      'events': [
        {'event_id': 'a', 'type': 'event', 'name': 'x', 'padding': 'y' * 3000}
      ],
    };

void main() {
  HttpTransport transport(int port, {Duration timeout = const Duration(seconds: 5)}) =>
      HttpTransport(Dsn.parse('http://pk_x@127.0.0.1:$port/1'), timeout: timeout);

  // Every case must come back as a value the retry logic understands, and fast
  // enough to be bounded by the timeout. Nothing may escape as an exception.
  final cases = <String, ({void Function(Socket) server, bool Function(SendResult) expected})>{
    '500': (server: _status('500 Internal Server Error'), expected: (r) => r is RetryLater && r.status == 500),
    '502 from a proxy with juju down': (server: _status('502 Bad Gateway'), expected: (r) => r is RetryLater && r.status == 502),
    '503': (server: _status('503 Service Unavailable'), expected: (r) => r is RetryLater),
    '429 with Retry-After': (
      server: _status('429 Too Many Requests', headers: 'Retry-After: 7$_crlf'),
      expected: (r) => r is RetryLater && r.retryAfter == const Duration(seconds: 7)
    ),
    '429 with a garbage Retry-After': (
      server: _status('429 Too Many Requests', headers: 'Retry-After: soon, maybe$_crlf'),
      expected: (r) => r is RetryLater && r.retryAfter == null
    ),
    '401, a revoked key': (server: _status('401 Unauthorized'), expected: (r) => r is Rejected),
    '413, batch too large': (server: _status('413 Content Too Large'), expected: (r) => r is Rejected),
    'not HTTP at all': (
      server: _reply((s) async {
        s.add(utf8.encode('\x00\x01\x02 this is not http\n\n'));
        await s.flush();
        await s.close();
      }),
      expected: (r) => r is RetryLater
    ),
    'connection closed without an answer': (server: _reply((s) => s.destroy()), expected: (r) => r is RetryLater),
    'connection closed as soon as it opens': (server: (s) => s.destroy(), expected: (r) => r is RetryLater),
    'answer cut off halfway': (
      server: _reply((s) async {
        s.write('HTTP/1.1 202 Accepted${_crlf}Content-Length: 100000$_crlf${_crlf}partial');
        await s.flush();
        s.destroy();
      }),
      expected: (r) => r is RetryLater
    ),
    'a redirect loop': (
      server: _status('302 Found', headers: 'Location: /api/v1/projects/1/events$_crlf'),
      expected: (r) => r is Rejected || r is RetryLater
    ),
    'a nonsense status code': (
      server: _reply((s) async {
        s.write('HTTP/1.1 999 What$_crlf$_crlf');
        await s.flush();
        await s.close();
      }),
      expected: (r) => r is Rejected || r is RetryLater
    ),
  };

  cases.forEach((name, c) {
    test('server: $name', () async {
      final server = await _serve(c.server);
      final result = await transport(server.port).send(_envelope());
      expect(c.expected(result), isTrue, reason: 'got ${result.runtimeType}');
    });
  });

  test('server: nothing listening (the server is off)', () async {
    final s = await ServerSocket.bind(InternetAddress.loopbackIPv4, 0);
    final port = s.port;
    await s.close();
    expect(await transport(port).send(_envelope()), isA<RetryLater>());
  });

  test('server: accepts the connection and never answers', () async {
    final server = await _serve((s) => s.listen((_) {}, onError: (_) {}));
    final watch = Stopwatch()..start();
    final result = await transport(server.port, timeout: const Duration(milliseconds: 300)).send(_envelope());
    expect(result, isA<RetryLater>());
    expect(watch.elapsedMilliseconds, lessThan(2000));
  });

  test('server: answers forever with an endless body, bounded by the timeout', () async {
    final server = await _serve(_reply((s) async {
      s.write('HTTP/1.1 200 OK${_crlf}Content-Type: text/plain$_crlf$_crlf');
      for (var i = 0; i < 2000; i++) {
        s.add(List.filled(1024, 65));
        await s.flush();
        await Future<void>.delayed(const Duration(milliseconds: 5));
      }
    }));
    final watch = Stopwatch()..start();
    final result = await transport(server.port, timeout: const Duration(milliseconds: 300)).send(_envelope());
    expect(result, isA<RetryLater>());
    expect(watch.elapsedMilliseconds, lessThan(3000));
  });

  test('a host that does not resolve', () async {
    final t = HttpTransport(Dsn.parse('https://pk_x@juju.invalid/1'), timeout: const Duration(seconds: 5));
    expect(await t.send(_envelope()), isA<RetryLater>());
  });

  test('TLS to a server that does not speak it', () async {
    final server = await _serve(_status('200 OK'));
    final t = HttpTransport(Dsn.parse('https://pk_x@127.0.0.1:${server.port}/1'), timeout: const Duration(seconds: 5));
    expect(await t.send(_envelope()), isA<RetryLater>());
  });
}
