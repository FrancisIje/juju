import 'dart:convert';
import 'dart:io';

import 'package:path_provider/path_provider.dart';

import '../core/client.dart';

/// Keeps the event queue in a JSON file in the app's private support
/// folder, so events survive the app being killed.
class FileQueueStorage implements QueueStorage {
  FileQueueStorage({Future<Directory> Function()? directory}) : _directory = directory ?? getApplicationSupportDirectory;

  final Future<Directory> Function() _directory;
  Future<File>? _file;

  Future<File> get _queueFile => _file ??= _directory().then((dir) async {
        await dir.create(recursive: true);
        return File('${dir.path}/juju_queue.json');
      });

  @override
  Future<List<JsonMap>> load() async {
    final file = await _queueFile;
    if (!await file.exists()) return [];
    try {
      final decoded = jsonDecode(await file.readAsString());
      if (decoded is! List) return [];
      return [
        for (final e in decoded)
          if (e is Map) Map<String, dynamic>.from(e)
      ];
    } on FormatException {
      return []; // unreadable file: start fresh rather than fail
    }
  }

  @override
  Future<void> save(List<JsonMap> events) async {
    final file = await _queueFile;
    if (events.isEmpty) {
      if (await file.exists()) await file.delete();
      return;
    }
    // Write to a temporary file, then rename it over the real one. A rename
    // is atomic: if the app is killed mid-write, the old file is still
    // intact, never half-written.
    final tmp = File('${file.path}.tmp');
    await tmp.writeAsString(jsonEncode(events), flush: true);
    await tmp.rename(file.path);
  }
}
