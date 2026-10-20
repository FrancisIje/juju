import 'package:flutter/widgets.dart';
import 'package:http/http.dart' as http;

import '../core/client.dart';

// Breadcrumbs record what led up to an error. Privacy: we record methods,
// URL paths, status codes and route names. Never request or response
// bodies, or query strings, which often carry tokens.

/// Records screen changes. Add it to your app's navigator:
///
///     MaterialApp(navigatorObservers: [Juju.navigatorObserver], ...)
class JujuNavigatorObserver extends NavigatorObserver {
  JujuNavigatorObserver(this._client);

  final JujuClient? Function() _client;

  @override
  void didPush(Route<dynamic> route, Route<dynamic>? previousRoute) => _record(previousRoute, route, 'push');

  @override
  void didPop(Route<dynamic> route, Route<dynamic>? previousRoute) => _record(route, previousRoute, 'pop');

  @override
  void didReplace({Route<dynamic>? newRoute, Route<dynamic>? oldRoute}) => _record(oldRoute, newRoute, 'replace');

  void _record(Route<dynamic>? from, Route<dynamic>? to, String action) {
    final a = _name(from), b = _name(to);
    _client()?.addBreadcrumb('navigation', message: '$a → $b', data: {'from': a, 'to': b, 'action': action});
  }

  static String _name(Route<dynamic>? route) => route?.settings.name ?? (route == null ? '(start)' : '(unnamed)');
}

/// Wraps an http.Client and records each request as a breadcrumb:
///
///     final client = Juju.httpClient(); // use instead of http.Client()
class JujuHttpClient extends http.BaseClient {
  JujuHttpClient(this._client, [http.Client? inner]) : _inner = inner ?? http.Client();

  final JujuClient? Function() _client;
  final http.Client _inner;

  @override
  Future<http.StreamedResponse> send(http.BaseRequest request) async {
    final url = redact(request.url);
    final started = DateTime.now();
    try {
      final res = await _inner.send(request);
      _client()?.addBreadcrumb('http', message: '${request.method} $url → ${res.statusCode}', data: {
        'method': request.method,
        'url': url,
        'status': res.statusCode,
        'duration_ms': DateTime.now().difference(started).inMilliseconds,
      });
      return res;
    } catch (e) {
      _client()
          ?.addBreadcrumb('http', message: '${request.method} $url → failed', data: {'method': request.method, 'url': url, 'error': '$e'});
      rethrow;
    }
  }

  @override
  void close() => _inner.close();
}

/// Drops the query string and fragment, which often carry tokens.
String redact(Uri url) => url.replace(query: '', fragment: '').toString().replaceAll(RegExp(r'[?#]+$'), '');

/// The mobile version of the web SDK's page lifecycle handling.
///
/// - paused: the OS may kill a backgrounded app at any time without
///   warning. Save the queue and try to send it now.
/// - resumed: the user is back, and the network may be too. Retry now.
class JujuLifecycleObserver with WidgetsBindingObserver {
  JujuLifecycleObserver(this._client);

  final JujuClient _client;
  AppLifecycleState? _last;

  @override
  void didChangeAppLifecycleState(AppLifecycleState state) {
    if (state == _last) return;
    _client.addBreadcrumb('app', message: '${_last?.name ?? 'start'} → ${state.name}');
    _last = state;
    if (state == AppLifecycleState.paused || state == AppLifecycleState.hidden) {
      _client.persistNow();
      _client.flush();
    } else if (state == AppLifecycleState.resumed) {
      _client.retryNow();
    }
  }
}
