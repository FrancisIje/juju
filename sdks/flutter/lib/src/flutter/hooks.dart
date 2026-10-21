import 'package:flutter/foundation.dart';

import '../core/client.dart';

// Flutter reports errors through two separate global hooks, and each one
// sees a different kind of error. Missing either one misses real crashes.
//
// Both hooks keep whatever handler was there before and call it, so the
// app's own error handling (and Flutter's console output) keeps working.

/// Framework errors: thrown while building, laying out or painting widgets.
/// In debug they show the red error screen; in release, a grey box. The app
/// keeps running either way, so these are easy to miss without reporting.
VoidCallback installFlutterErrorHook(JujuClient client) {
  final previous = FlutterError.onError;
  void handler(FlutterErrorDetails details) {
    // "silent" errors are ones Flutter itself considers unimportant, such as
    // some image loading failures. They're not printed in release either.
    if (!details.silent) {
      client.captureException(
        details.exception,
        details.stack,
        mechanism: 'flutter_error',
        // Nothing caught it: the screen broke (red in debug, grey in
        // release). That counts as a crash of the session.
        crashed: true,
        tags: {if (details.library != null) 'flutter_library': details.library!},
      );
    }
    previous?.call(details);
  }

  FlutterError.onError = handler;
  return () {
    // Only undo our own wrap: if something wrapped us since, leave it alone.
    if (FlutterError.onError == handler) FlutterError.onError = previous;
  };
}

/// Async errors nobody caught: a Future that fails with no catchError or
/// try/await, an exception in a Timer or stream callback.
VoidCallback installPlatformErrorHook(JujuClient client, PlatformDispatcher dispatcher) {
  final previous = dispatcher.onError;
  bool handler(Object error, StackTrace stack) {
    client.captureException(error, stack, mechanism: 'platform_dispatcher', crashed: true);
    // Returning false means "not handled", so Flutter still prints it as
    // usual. If the app had its own handler, its answer wins.
    return previous?.call(error, stack) ?? false;
  }

  dispatcher.onError = handler;
  return () {
    if (identical(dispatcher.onError, handler)) dispatcher.onError = previous;
  };
}
