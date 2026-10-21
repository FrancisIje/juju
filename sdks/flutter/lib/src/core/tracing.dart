import 'dart:async';
import 'dart:math';

import 'package:clock/clock.dart';

import 'client.dart' show JsonMap;

// Transactions and spans: how long things take, and where the time goes.
// A port of @juju/core's tracing.ts, so every SDK times things the same way.
//
// A transaction is one operation a person waits for (an app start, a screen
// opening). Spans are the steps inside it (an HTTP request). Times come
// from clock.now(), which has microsecond precision on Dart and can be
// faked in tests.

/// What a transaction needs from the client that created it.
abstract interface class TransactionHost {
  void finishTransaction(JujuTransaction tx);
}

class JujuSpan {
  JujuSpan._(this._tx, this.op, this.description, this.start, this.data, this.spanId);

  final JujuTransaction? _tx;
  final String spanId;
  final String op;
  final String? description;
  final DateTime start;
  DateTime? end;
  String? status;
  JsonMap? data;

  void setData(String key, Object? value) => (data ??= {})[key] = value;

  /// Ends the span. Calling it again does nothing.
  void finish([DateTime? at]) {
    if (end != null) return;
    end = at ?? clock.now();
    _tx?._activity(end!);
  }

  JsonMap toJson(DateTime txEnd) => {
        'span_id': spanId,
        'op': op,
        if (description != null) 'description': description,
        if (status != null) 'status': status,
        'start_timestamp': _iso(start),
        'timestamp': _iso(end!.isAfter(txEnd) ? txEnd : end!),
        if (data != null) 'data': data,
      };
}

/// A span that records nothing: returned when there's no transaction to join.
JujuSpan noopSpan() => JujuSpan._(null, 'noop', null, DateTime.fromMillisecondsSinceEpoch(0), null, '');

/// Spans beyond this are ignored: a polling loop mustn't grow a transaction forever.
const maxSpans = 500;
const _defaultIdleDeadline = Duration(seconds: 30);

class JujuTransaction {
  /// [idleTimeout] makes this an idle transaction: it finishes by itself
  /// once nothing has happened in it (no open spans, no holds) for that
  /// long, and its end is the moment the last activity ended.
  JujuTransaction(
    this._host, {
    required this.name,
    required this.op,
    DateTime? start,
    Duration? idleTimeout,
    Duration? maxDuration,
    this.sampled = true,
    Random? random,
  })  : start = start ?? clock.now(),
        _idleTimeout = idleTimeout,
        _random = random ?? Random() {
    _lastActivity = this.start;
    final deadline = maxDuration ?? (idleTimeout != null ? _defaultIdleDeadline : null);
    if (deadline != null) {
      final wait = this.start.add(deadline).difference(clock.now());
      _deadlineTimer = Timer(wait.isNegative ? Duration.zero : wait, () {
        status = 'deadline_exceeded';
        finish(this.start.add(deadline));
      });
    }
    _maybeIdle();
  }

  final TransactionHost _host;
  final String name;
  final String op;
  final DateTime start;
  bool sampled;
  final Random _random;
  final Duration? _idleTimeout;
  final spans = <JujuSpan>[];
  final measurements = <String, double>{};
  String status = 'ok';
  DateTime? end;

  int _open = 0;
  late DateTime _lastActivity;
  Timer? _idleTimer;
  Timer? _deadlineTimer;

  bool get finished => end != null;

  JujuSpan startChild(String op, {String? description, JsonMap? data, DateTime? start}) {
    if (finished || spans.length >= maxSpans) return noopSpan();
    final span = JujuSpan._(this, op, description, start ?? clock.now(), data, _spanId());
    spans.add(span);
    _open++;
    _cancelIdle();
    return span;
  }

  /// Keeps an idle transaction open until the returned function is called,
  /// e.g. until the screen's first frame. Its end counts as activity.
  void Function([DateTime? at]) hold() {
    if (finished) return ([_]) {};
    _open++;
    _cancelIdle();
    var released = false;
    return ([at]) {
      if (released) return;
      released = true;
      _activity(at ?? clock.now());
    };
  }

  void setMeasurement(String name, double value) {
    if (value.isFinite) measurements[name] = value;
  }

  /// Ends the transaction and hands it to the client to send.
  void finish([DateTime? at]) {
    if (finished) return;
    end = at ?? clock.now();
    _cancelIdle();
    _deadlineTimer?.cancel();
    // Spans still open at the end (a request that never answered) end with
    // the transaction, marked so nobody mistakes them for fast requests.
    for (final s in spans) {
      if (s.end == null) {
        s.end = end;
        s.status ??= 'cancelled';
      }
    }
    _host.finishTransaction(this);
  }

  /// Stops without sending: what it measured turned out to be meaningless.
  void discard() {
    if (finished) return;
    sampled = false;
    finish();
  }

  void _activity(DateTime at) {
    if (_open > 0) _open--;
    if (at.isAfter(_lastActivity)) _lastActivity = at;
    _maybeIdle();
  }

  void _maybeIdle() {
    final idle = _idleTimeout;
    if (idle == null || _open > 0 || finished) return;
    _cancelIdle();
    _idleTimer = Timer(idle, () => finish(_lastActivity));
  }

  void _cancelIdle() {
    _idleTimer?.cancel();
    _idleTimer = null;
  }

  String _spanId() => List.generate(16, (_) => _random.nextInt(16).toRadixString(16)).join();
}

/// RFC 3339 in UTC with microseconds, which Dart keeps and the server reads.
String _iso(DateTime t) => t.toUtc().toIso8601String();
String isoTime(DateTime t) => _iso(t);
