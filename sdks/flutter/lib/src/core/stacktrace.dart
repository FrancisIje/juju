// Dart prints stack traces in two forms.
//
// Symbolic, the default (debug, profile and normal release builds):
//
//     #0      CartPage.build (package:shop/cart_page.dart:88:12)
//     #1      StatelessElement.build (package:flutter/src/widgets/framework.dart:5689:49)
//     <asynchronous suspension>
//
// Non-symbolic, when the app is built with --split-debug-info (often with
// --obfuscate). Names and lines are gone; only addresses remain, to be
// turned back into code with the saved debug info (symbolication, Phase 5):
//
//     build_id: '4b1c...'
//         #00 abs 000000763f6b5a2f virt 00000000002a6a2f _kDartIsolateSnapshotInstructions+0x1a2b3c

/// One stack frame, in the server's v1 shape. Frames are ordered most
/// recent call first: frames[0] is where the error was thrown.
class JujuFrame {
  const JujuFrame({this.function, this.filename, this.lineno, this.colno, required this.inApp});

  final String? function;
  final String? filename;
  final int? lineno;
  final int? colno;
  final bool inApp;

  Map<String, dynamic> toJson() => {
        if (function != null) 'function': function,
        if (filename != null) 'filename': filename,
        if (lineno != null) 'lineno': lineno,
        if (colno != null) 'colno': colno,
        'in_app': inApp,
      };
}

class ParsedStack {
  const ParsedStack(this.frames, {this.buildId, this.symbolic = true});

  final List<JujuFrame> frames;

  /// Set for non-symbolic traces: identifies the build whose debug info
  /// can turn the addresses back into code.
  final String? buildId;
  final bool symbolic;
}

typedef InAppFn = bool Function(String filename);

/// Packages that are never the app's own code.
const _frameworkPackages = {'flutter', 'flutter_test', 'juju_flutter', 'http', 'path_provider', 'clock'};

/// Default rule: `dart:` and framework packages are not app code; any other
/// `package:` is. Pass `appPackages` to [makeIsInApp] to be exact.
InAppFn makeIsInApp([Set<String> appPackages = const {}]) {
  return (filename) {
    if (filename.startsWith('dart:')) return false;
    if (filename.startsWith('package:')) {
      final slash = filename.indexOf('/');
      final pkg = filename.substring('package:'.length, slash < 0 ? null : slash);
      if (appPackages.isNotEmpty) return appPackages.contains(pkg);
      return !_frameworkPackages.contains(pkg);
    }
    return true; // file:// paths in debug, addresses in non-symbolic traces
  };
}

final _symbolic = RegExp(r'^#\d+\s+(.+?) \((.+?)(?::(\d+))?(?::(\d+))?\)$');
final _nonSymbolic = RegExp(r'^#\d+\s+abs\s+[0-9a-f]+(?:\s+virt\s+([0-9a-f]+))?\s*(\S*)');
final _buildId = RegExp(r"^build_id: '([0-9a-f]+)'");

ParsedStack parseStackTrace(StackTrace? stack, InAppFn isInApp) {
  if (stack == null) return const ParsedStack([]);
  final frames = <JujuFrame>[];
  String? buildId;
  var symbolic = true;
  for (final raw in stack.toString().split('\n')) {
    final line = raw.trim();
    if (line.isEmpty || line == '<asynchronous suspension>') continue;

    final id = _buildId.firstMatch(line);
    if (id != null) {
      buildId = id.group(1);
      symbolic = false;
      continue;
    }
    final ns = _nonSymbolic.firstMatch(line);
    if (ns != null) {
      symbolic = false;
      // The virtual address is stable for a given build; the symbol plus
      // offset is kept as the "function" so frames stay distinguishable.
      final symbol = ns.group(2);
      frames.add(
          JujuFrame(function: (symbol == null || symbol.isEmpty) ? '0x${ns.group(1)}' : symbol, filename: '<non-symbolic>', inApp: true));
      continue;
    }
    final m = _symbolic.firstMatch(line);
    if (m != null) {
      final file = m.group(2)!;
      frames.add(JujuFrame(
        function: m.group(1),
        filename: file,
        lineno: m.group(3) == null ? null : int.parse(m.group(3)!),
        colno: m.group(4) == null ? null : int.parse(m.group(4)!),
        inApp: isInApp(file),
      ));
    }
  }
  return ParsedStack(frames, buildId: buildId, symbolic: symbolic);
}
