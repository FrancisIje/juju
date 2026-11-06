import 'dart:async';
import 'dart:io';

import 'package:clock/clock.dart';
import 'package:flutter/scheduler.dart';
import 'package:flutter/widgets.dart';

import '../core/client.dart';
import '../core/tracing.dart';

/// How long a screen must be quiet (no requests) before its load counts as done.
const idleTimeout = Duration(seconds: 1);

/// An app start that hasn't drawn its first frame by then is dropped, not sent.
const appStartDeadline = Duration(seconds: 30);

/// A process older than this when juju starts isn't a cold start (Android
/// can keep a process alive and start Flutter in it later), so the start is
/// measured from init instead.
const _maxColdStartAge = Duration(seconds: 60);

/// When this process started, on Android and Linux, read from /proc without
/// a native plugin.
///
/// /proc/self/stat, field 22, is the process's start in clock ticks since
/// boot. To turn that into a time we need "now" on the same clock, and
/// Android doesn't let apps read /proc/uptime (SELinux denies it). So we
/// start a tiny child process, `cat /proc/self/stat`: its own start time is
/// "now", in the same ticks. The difference is the process's age. Android's
/// clock tick is 100 Hz, so this is precise to 10 ms.
///
/// Returns null where there's no /proc (iOS, without spawning anything) or
/// the numbers don't make sense.
Future<DateTime?> processStartTime({String stat = '/proc/self/stat', Future<String?> Function()? statOfNewProcess}) async {
  try {
    final self = _startTicks(File(stat).readAsStringSync());
    final now = clock.now();
    final probe = await (statOfNewProcess ?? _statOfNewProcess)();
    if (probe == null) return null;
    final age = Duration(milliseconds: (_startTicks(probe) - self) * 10);
    if (age.isNegative || age > _maxColdStartAge) return null;
    return now.subtract(age);
  } catch (_) {
    return null;
  }
}

/// The start time field of a `/proc/<pid>/stat` line. The second field is the
/// program name in parentheses, and may itself contain spaces or
/// parentheses, so fields are counted from the last ')'.
int _startTicks(String statLine) => int.parse(statLine.substring(statLine.lastIndexOf(')') + 2).split(' ')[22 - 3]);

Future<String?> _statOfNewProcess() async {
  final r = await Process.run('cat', ['/proc/self/stat']).timeout(const Duration(seconds: 1));
  return r.exitCode == 0 ? r.stdout as String : null;
}

/// Starts the app.start transaction: from the process start (Android) or
/// juju's init (iOS), until the first frame is on screen. Returns a
/// function that cancels it (used by Juju.close).
VoidCallback startAppStart(JujuClient client, {DateTime? processStart, Future<void>? firstFrame}) {
  final initAt = clock.now();
  final tx = client.startTransaction('app.start', 'app.start', start: processStart ?? initAt, active: true);
  if (processStart != null) {
    tx.startChild('app.start.phase', description: 'Process start until juju.init', start: processStart).finish(initAt);
  }
  // Which start the number is from, so nobody compares the two by accident.
  tx.setMeasurement(processStart != null ? 'from_process_start' : 'from_init', 1);

  // If the first frame never comes, better no number than a wrong one.
  final timer = Timer(appStartDeadline, tx.discard);
  (firstFrame ?? WidgetsBinding.instance.waitUntilFirstFrameRasterized).then((_) {
    timer.cancel();
    if (tx.finished) return;
    try {
      final end = clock.now();
      tx.startChild('app.start.phase', description: 'First frame', start: initAt).finish(end);
      tx.finish(end);
    } catch (_) {
      tx.discard(); // nobody is listening to this future: an error here would reach the app
    }
  });
  return () {
    timer.cancel();
    tx.discard();
  };
}

/// A screen.load transaction for a route that was just pushed: until its
/// first frame is drawn and the requests it made have finished.
JujuTransaction startScreenLoad(JujuClient client, String screen) {
  final tx = client.startTransaction(screen, 'screen.load', idleTimeout: idleTimeout, active: true);
  final release = tx.hold();
  // The route is pushed before it's built; it's built and drawn in the
  // next frame, and a post-frame callback runs right after that frame.
  SchedulerBinding.instance.addPostFrameCallback((_) => release());
  SchedulerBinding.instance.scheduleFrame();
  return tx;
}
