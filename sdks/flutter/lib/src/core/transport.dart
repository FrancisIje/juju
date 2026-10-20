import 'dart:async';
import 'dart:convert';
import 'dart:io' show gzip;

import 'package:clock/clock.dart';
import 'package:http/http.dart' as http;

import 'dsn.dart';

// The status code contract with the server (server/internal/ingest):
//
//   2xx                  stored: delete the events
//   400 401 413 415 ...  broken: a retry can never succeed, so delete them
//   429                  busy: keep them, wait at least Retry-After
//   5xx, network error   temporary: keep them, retry with backoff

sealed class SendResult {
  const SendResult();
}

class Delivered extends SendResult {
  const Delivered();
}

class Rejected extends SendResult {
  const Rejected(this.status);
  final int status;
}

class RetryLater extends SendResult {
  const RetryLater({this.status, this.retryAfter});
  final int? status;
  final Duration? retryAfter;
}

abstract class Transport {
  Future<SendResult> send(Map<String, dynamic> envelope);
}

SendResult classify(int status, String? retryAfter) {
  if (status >= 200 && status < 300) return const Delivered();
  if (status == 429 || status >= 500) return RetryLater(status: status, retryAfter: parseRetryAfter(retryAfter));
  return Rejected(status);
}

/// Retry-After is either a number of seconds or an HTTP date.
Duration? parseRetryAfter(String? value) {
  if (value == null || value.trim().isEmpty) return null;
  final seconds = int.tryParse(value.trim());
  if (seconds != null) return Duration(seconds: seconds);
  try {
    final date = HttpDate.parse(value);
    final diff = date.difference(clock.now());
    return diff.isNegative ? Duration.zero : diff;
  } catch (_) {
    return null;
  }
}

/// Bodies smaller than this are sent as-is: compressing them costs more CPU than it saves.
const compressMinBytes = 1024;

class HttpTransport implements Transport {
  HttpTransport(this.dsn, {http.Client? client, this.compress = true, this.timeout = const Duration(seconds: 30)})
      : _client = client ?? http.Client();

  final Dsn dsn;
  final bool compress;
  final Duration timeout;
  final http.Client _client;

  @override
  Future<SendResult> send(Map<String, dynamic> envelope) async {
    List<int> body = utf8.encode(jsonEncode(envelope));
    final headers = {'Content-Type': 'application/json', 'X-Juju-Key': dsn.publicKey};
    // Batches repeat the same frames and breadcrumbs, so they compress to a
    // few percent of their size. The server decompresses, within its 1 MB limit.
    if (compress && body.length >= compressMinBytes) {
      final gz = gzip.encode(body);
      if (gz.length < body.length) {
        body = gz;
        headers['Content-Encoding'] = 'gzip';
      }
    }
    try {
      final res = await _client.post(dsn.endpoint, headers: headers, body: body).timeout(timeout);
      return classify(res.statusCode, res.headers['retry-after']);
    } catch (_) {
      // Offline, DNS failure, connection refused, timeout: all worth retrying.
      return const RetryLater();
    }
  }
}

/// Minimal HTTP-date parsing (RFC 7231 IMF-fixdate), e.g. "Mon, 19 Oct 2026 12:00:10 GMT".
class HttpDate {
  static const _months = {
    'Jan': 1,
    'Feb': 2,
    'Mar': 3,
    'Apr': 4,
    'May': 5,
    'Jun': 6,
    'Jul': 7,
    'Aug': 8,
    'Sep': 9,
    'Oct': 10,
    'Nov': 11,
    'Dec': 12
  };

  static DateTime parse(String value) {
    final m = RegExp(r'^\w{3}, (\d{2}) (\w{3}) (\d{4}) (\d{2}):(\d{2}):(\d{2}) GMT$').firstMatch(value.trim());
    final month = m == null ? null : _months[m.group(2)];
    if (m == null || month == null) throw FormatException('not an HTTP date: $value');
    return DateTime.utc(
        int.parse(m.group(3)!), month, int.parse(m.group(1)!), int.parse(m.group(4)!), int.parse(m.group(5)!), int.parse(m.group(6)!));
  }
}
