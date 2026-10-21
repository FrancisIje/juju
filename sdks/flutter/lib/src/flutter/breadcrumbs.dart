import 'package:flutter/widgets.dart';
import 'package:http/http.dart' as http;

import '../core/client.dart';
import 'performance.dart';

// Breadcrumbs record what led up to an error. Privacy: we record methods,
// URL paths, status codes and route names. Never request or response
// bodies, or query strings, which often carry tokens.

/// Records screen changes. Add it to your app's navigator:
///
///     MaterialApp(navigatorObservers: [Juju.navigatorObserver], ...)
class JujuNavigatorObserver extends NavigatorObserver {
  JujuNavigatorObserver(this._client, {bool Function()? tracing}) : _tracing = tracing ?? (() => false);

  final JujuClient? Function() _client;
  final bool Function() _tracing;

  @override
  void didPush(Route<dynamic> route, Route<dynamic>? previousRoute) {
    _record(previousRoute, route, 'push');
    _timeLoad(route, previousRoute);
  }

  @override
  void didPop(Route<dynamic> route, Route<dynamic>? previousRoute) => _record(route, previousRoute, 'pop');

  @override
  void didReplace({Route<dynamic>? newRoute, Route<dynamic>? oldRoute}) {
    _record(oldRoute, newRoute, 'replace');
    if (newRoute != null) _timeLoad(newRoute, oldRoute);
  }

  /// A newly pushed screen gets a screen.load transaction. Popping back
  /// shows a screen that's already built, so there's nothing to time; and
  /// the very first route is part of the app start.
  void _timeLoad(Route<dynamic> route, Route<dynamic>? previous) {
    final client = _client();
    if (client == null || previous == null || !_tracing() || route is PopupRoute) return;
    startScreenLoad(client, _name(route));
  }

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
    // Joins the app start or screen load that's running, if any.
    final span = _client()?.startSpan('http.client', description: '${request.method} $url', data: {'method': request.method, 'url': url});
    try {
      final res = await _inner.send(request);
      span
        ?..setData('status_code', res.statusCode)
        ..status = res.statusCode >= 500 ? 'error' : 'ok'
        ..finish();
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
      span
        ?..status = 'failed'
        ..finish();
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
  JujuLifecycleObserver(this._client, {this.sessions = false});

  final JujuClient _client;

  /// The session pauses (sent as ended) in the background and resumes, or
  /// starts anew, when the app is back.
  final bool sessions;
  AppLifecycleState? _last;

  @override
  void didChangeAppLifecycleState(AppLifecycleState state) {
    if (state == _last) return;
    _client.addBreadcrumb('app', message: '${_last?.name ?? 'start'} → ${state.name}');
    _last = state;
    if (state == AppLifecycleState.paused || state == AppLifecycleState.hidden) {
      if (sessions) _client.pauseSession(); // before saving, so the saved queue has it
      _client.persistNow();
      _client.flush();
    } else if (state == AppLifecycleState.resumed) {
      if (sessions) _client.resumeSession();
      _client.retryNow();
    }
  }
}
