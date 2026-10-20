import 'dart:io' show Platform;

import 'package:flutter/foundation.dart';

/// Device, OS and runtime details attached to every event, from Dart and
/// Flutter's own APIs. No extra plugin is needed. Device model names would
/// need a native plugin, so they're left out for now.
Map<String, dynamic> deviceContext() {
  final view = PlatformDispatcher.instance.views.firstOrNull;
  final ratio = view?.devicePixelRatio ?? 1;
  return {
    'os': osContext(Platform.operatingSystem, Platform.operatingSystemVersion),
    if (view != null)
      'screen': {
        'width': (view.physicalSize.width / ratio).round(),
        'height': (view.physicalSize.height / ratio).round(),
        'scale': ratio,
      },
    'runtime': {
      'name': 'flutter',
      'dart': Platform.version.split(' ').first,
      'mode': kReleaseMode ? 'release' : (kProfileMode ? 'profile' : 'debug'),
    },
    'locale': Platform.localeName,
  };
}

/// Dart's operatingSystemVersion means different things per platform. On
/// iOS and macOS it's "Version 26.5 (Build 23F79)". On Android it's the
/// system build id ("CE2A.260420.019 dev-keys"), not the version people know;
/// reading that needs a native plugin. So the raw value is kept as "build",
/// and "version" is only set when it's really there.
Map<String, String> osContext(String name, String raw) {
  final version = RegExp(r'Version ([\d.]+)').firstMatch(raw)?.group(1);
  return {'name': name, if (version != null) 'version': version, 'build': raw};
}
