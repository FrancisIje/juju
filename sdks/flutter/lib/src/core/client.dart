import 'dart:async';
import 'dart:math';

import 'package:clock/clock.dart';

import 'dsn.dart';
import 'stacktrace.dart';
import 'transport.dart';

typedef JsonMap = Map<String, dynamic>;

/// Somewhere to keep queued events between app launches.
abstract class QueueStorage {
  Future<List<JsonMap>> load();
  Future<void> save(List<JsonMap> events);
}

/// Where the anonymous id (and the user id after identify) is kept between
/// launches, so one install stays one person.
abstract class IdentityStorage {
  Future<Map<String, String>> load();
  Future<void> save(Map<String, String> ids);
}

class ClientOptions {
  ClientOptions({
    required this.dsn,
    this.release,
    this.environment,
    this.platform = 'dart',
    this.sdkName = 'juju.dart',
    this.sdkVersion = jujuVersion,
    this.transport,
    this.storage,
    this.identityStorage,
    this.isInApp,
    this.context,
    this.beforeSend,
    this.flushInterval = const Duration(seconds: 5),
    this.maxBatchSize = 20,
    this.maxQueueSize = 100,
    this.maxAnalyticsQueueSize = 1000,
    this.maxBreadcrumbs = 100,
    this.debug = false,
    this.log,
    Random? random,
  }) : random = random ?? Random();

  final String dsn;
  final String? release;
  final String? environment;
  final String platform;
  final String sdkName;
  final String sdkVersion;
  final Transport? transport;
  final QueueStorage? storage;
  final IdentityStorage? identityStorage;
  final InAppFn? isInApp;

  /// Extra context attached to every event (device, OS, runtime).
  final JsonMap Function()? context;

  /// Change or drop (return null) an event before it is queued.
  final JsonMap? Function(JsonMap event)? beforeSend;
  final Duration flushInterval;
  final int maxBatchSize;

  /// Errors kept while offline. New ones past this are dropped.
  final int maxQueueSize;

  /// Analytics events kept while offline, counted apart from errors so a
  /// burst of tracking can never push a crash report out.
  final int maxAnalyticsQueueSize;
  final int maxBreadcrumbs;
  final bool debug;
  final void Function(String message)? log;
  final Random random;
}

const jujuVersion = '0.1.0';
const _baseBackoff = Duration(seconds: 1);
// The server's limits (server/internal/event): at most 250 frames, keeping
// the 50 nearest the crash and the 200 nearest the program's entry point.
const _maxFrames = 250;
const _keepTopFrames = 50;
const _keepRootFrames = 200;
const _maxBackoff = Duration(seconds: 60);

/// Builds events, queues them, sends them in batches and retries with
/// backoff. A port of @juju/core's Client, so the JavaScript and Dart SDKs
/// follow the same rules.
class JujuClient {
  JujuClient(this.options)
      : dsn = Dsn.parse(options.dsn), // throws early on a bad DSN
        _isInApp = options.isInApp ?? makeIsInApp() {
    _transport = options.transport ?? HttpTransport(dsn);
    // Load before any save: an error captured in the first moments after
    // launch would otherwise save a queue without the old events, wiping
    // the report we're about to read back.
    ready = _loadSaved();
    _persistChain = ready;
    _identityChain = _loadIdentity();
  }

  final ClientOptions options;
  final Dsn dsn;
  final InAppFn _isInApp;
  late final Transport _transport;

  /// Completes once events saved by the previous launch are queued again.
  late final Future<void> ready;

  final List<JsonMap> _queue = [];
  final List<JsonMap> _breadcrumbs = [];
  Timer? _timer;
  DateTime? _timerDueAt;
  Future<void>? _inFlight;
  int _failures = 0;
  DateTime _backoffUntil = DateTime.fromMillisecondsSinceEpoch(0);
  JsonMap? _user;
  final Map<String, String> _tags = {};
  bool _closed = false;
  late Future<void> _persistChain;

  // Who analytics events are from: the anonymous id of this install until
  // identify(), then the user id. capture/identify/reset run in call order
  // on _identityChain, after the stored ids have loaded, so the first events
  // after launch use the stored id, not a new one.
  String _anonId = '';
  String _distinctId = '';
  late Future<void> _identityChain;
  bool _saveQueued = false;

  // The same error object can reach us twice (reported with
  // captureException, then rethrown to a global hook). An Expando attaches
  // a mark to an object without keeping it alive.
  final _seen = Expando<bool>('juju-seen');

  int get pending => _queue.length;
  List<JsonMap> get breadcrumbs => List.unmodifiable(_breadcrumbs);

  /// Reports an error and returns its event_id, or "" if it was dropped.
  /// Never throws: a bug in the error reporter must not break the app.
  String captureException(Object error, StackTrace? stack, {Map<String, String>? tags, String? mechanism}) {
    try {
      if (_closed) return '';
      if (_markSeen(error)) return '';
      JsonMap? event = _buildEvent(error, stack, tags, mechanism);
      if (options.beforeSend != null) event = options.beforeSend!(event);
      if (event == null) return '';
      return _enqueue(event) ? event['event_id'] as String : '';
    } catch (e) {
      _log('could not capture error: $e');
      return '';
    }
  }

  void addBreadcrumb(String type, {String? message, JsonMap? data}) {
    _breadcrumbs.add({
      'type': type,
      if (message != null) 'message': message.length > 1000 ? message.substring(0, 1000) : message,
      'timestamp': clock.now().toUtc().toIso8601String(),
      if (data != null) 'data': data,
    });
    // A ring buffer: the oldest falls off, so memory stays flat.
    if (_breadcrumbs.length > options.maxBreadcrumbs) _breadcrumbs.removeAt(0);
  }

  void setUser(JsonMap? user) => _user = user;

  /// Records something a user did: capture('checkout_started', {'plan': 'pro'}).
  /// Never throws.
  void capture(String name, [JsonMap? properties]) {
    try {
      if (_closed || name.trim().isEmpty) return;
      final base = _analyticsBase(); // stamped now: the time it happened
      final props = properties == null ? null : JsonMap.of(properties);
      _afterIdentity(() {
        _enqueue({
          ...base,
          'type': 'event',
          'name': name,
          'distinct_id': _distinctId,
          if (props != null && props.isNotEmpty) 'properties': props,
        });
      });
    } catch (e) {
      _log('could not capture event: $e');
    }
  }

  /// After login: events from this install so far and from now on belong to
  /// [userId]. Also sets the user on error reports, unless setUser did.
  void identify(String userId) {
    try {
      if (_closed || userId.trim().isEmpty) return;
      final base = _analyticsBase();
      _user ??= {'id': userId};
      _afterIdentity(() {
        if (userId == _distinctId) return; // already this user
        // Link the anonymous history only while still anonymous: switching
        // accounts on a shared device starts fresh instead of merging people.
        final wasAnonymous = _distinctId == _anonId;
        _enqueue({...base, 'type': 'identify', 'distinct_id': userId, if (wasAnonymous) 'anon_id': _anonId});
        _distinctId = userId;
        _saveIdentity();
      });
    } catch (e) {
      _log('could not identify: $e');
    }
  }

  /// After logout: a new anonymous id, so the next person isn't counted as this one.
  void reset() {
    _user = null;
    _afterIdentity(() {
      _anonId = uuid4(options.random);
      _distinctId = _anonId;
      _saveIdentity();
    });
  }

  /// Who events are sent as, once the stored id has loaded.
  Future<String> get distinctId async {
    await _identityChain;
    return _distinctId;
  }

  JsonMap _analyticsBase() {
    final context = options.context?.call();
    return {
      'event_id': uuid4(options.random),
      'timestamp': clock.now().toUtc().toIso8601String(),
      'platform': options.platform,
      if (options.release != null) 'release': options.release,
      if (options.environment != null) 'environment': options.environment,
      // Device context makes breakdowns by OS work.
      if (context != null && context.isNotEmpty) 'context': context,
    };
  }

  void _afterIdentity(void Function() fn) {
    _identityChain = _identityChain.then((_) => fn()).catchError((Object e) => _log('analytics: $e'));
  }

  Future<void> _loadIdentity() async {
    Map<String, String> ids = const {};
    try {
      ids = await options.identityStorage?.load() ?? const {};
    } catch (e) {
      _log('could not read the stored id: $e');
    }
    _anonId = ids['anon_id'] ?? '';
    if (_anonId.isEmpty) _anonId = uuid4(options.random);
    _distinctId = ids['distinct_id'] ?? '';
    if (_distinctId.isEmpty) _distinctId = _anonId;
    if (ids['anon_id'] == null) await _saveIdentity();
  }

  Future<void> _saveIdentity() async {
    try {
      await options.identityStorage?.save({'anon_id': _anonId, 'distinct_id': _distinctId});
    } catch (e) {
      _log('could not store the id: $e'); // lasts for this run, which still counts correctly
    }
  }

  void setTag(String key, String value) => _tags[key] = value;

  /// Sends everything queued now. Only one send runs at a time.
  Future<void> flush() => _inFlight ??= _sendLoop().whenComplete(() => _inFlight = null);

  /// Forget the backoff and try now (the app came back to the foreground).
  void retryNow() {
    _failures = 0;
    _backoffUntil = DateTime.fromMillisecondsSinceEpoch(0);
    if (_queue.isNotEmpty) _schedule(Duration.zero, replace: true);
  }

  /// Writes the queue to storage now; completes when the write is done.
  Future<void> persistNow() => _persist();

  void close() {
    _closed = true;
    _timer?.cancel();
    _timer = null;
  }

  bool _markSeen(Object error) {
    // Strings, numbers, booleans and null can't carry an Expando: they are
    // values, not objects with identity, so they're never deduplicated.
    if (error is String || error is num || error is bool) return false;
    try {
      if (_seen[error] == true) return true;
      _seen[error] = true;
    } catch (_) {}
    return false;
  }

  JsonMap _buildEvent(Object error, StackTrace? stack, Map<String, String>? extraTags, String? mechanism) {
    final parsed = parseStackTrace(stack, _isInApp);
    // Flutter's widget trees make deep stacks (300+ frames is common). Keep
    // the same frames the server would keep, so phones don't upload the rest.
    final frames = parsed.frames.length > _maxFrames
        ? [...parsed.frames.take(_keepTopFrames), ...parsed.frames.skip(parsed.frames.length - _keepRootFrames)]
        : parsed.frames;
    final tags = {..._tags, ...?extraTags, if (mechanism != null) 'mechanism': mechanism};
    final context = {...?options.context?.call()};
    if (!parsed.symbolic) {
      // Keep what symbolication (Phase 5) will need: the build id and the raw trace.
      context['dart_stack'] = {'build_id': parsed.buildId, 'raw': stack.toString()};
    }
    return {
      'event_id': uuid4(options.random), // created now and reused by every retry, so the server can dedupe
      'type': 'error',
      'timestamp': clock.now().toUtc().toIso8601String(),
      'platform': options.platform,
      if (options.release != null) 'release': options.release,
      if (options.environment != null) 'environment': options.environment,
      'exception': {
        'type': _typeName(error),
        'message': _message(error),
        if (frames.isNotEmpty)
          'stacktrace': {
            'frames': [for (final f in frames) f.toJson()]
          },
      },
      if (_breadcrumbs.isNotEmpty) 'breadcrumbs': List<JsonMap>.of(_breadcrumbs),
      if (_user != null) 'user': {..._user!},
      if (tags.isNotEmpty) 'tags': tags,
      if (context.isNotEmpty) 'context': context,
    };
  }

  bool _enqueue(JsonMap event) {
    final analytics = event['type'] != 'error';
    final sameKind = _queue.where((e) => (e['type'] != 'error') == analytics).length;
    final limit = analytics ? options.maxAnalyticsQueueSize : options.maxQueueSize;
    if (sameKind >= limit) {
      // Drop the newest: an overflowing queue is usually a loop repeating
      // the same thing, and the first ones are the most likely to show the cause.
      _log('queue full ($limit ${analytics ? 'analytics events' : 'errors'}), dropping event');
      return false;
    }
    _queue.add(event);
    _persist();
    _schedule(_queue.length >= options.maxBatchSize ? Duration.zero : options.flushInterval);
    return true;
  }

  Future<void> _sendLoop() async {
    while (_queue.isNotEmpty && !_closed) {
      final batch = _queue.take(options.maxBatchSize).toList();
      SendResult result;
      try {
        result = await _transport.send({
          'sdk': {'name': options.sdkName, 'version': options.sdkVersion},
          'events': batch,
        });
      } catch (_) {
        result = const RetryLater();
      }
      switch (result) {
        case Delivered():
        case Rejected():
          if (result is Rejected) _log('server rejected ${batch.length} event(s) with ${result.status}; not retrying');
          _remove(batch);
          _failures = 0;
          _backoffUntil = DateTime.fromMillisecondsSinceEpoch(0);
        case RetryLater(:final retryAfter):
          _failures++;
          var delay = _backoffDelay();
          if (retryAfter != null && retryAfter > delay) delay = retryAfter;
          _backoffUntil = clock.now().add(delay);
          _log('send failed, retrying in ${delay.inMilliseconds} ms');
          // Replace any earlier timer (like the batch timer): it was set
          // before we knew about the backoff and would fire too soon.
          _schedule(delay, replace: true);
          return;
      }
    }
  }

  /// 1 s, 2 s, 4 s ... capped at 60 s, times a random 0.5–1 so that many
  /// apps recovering from the same outage don't all retry at the same second.
  Duration _backoffDelay() {
    final exp = _baseBackoff * pow(2, _failures - 1).toInt();
    final capped = exp > _maxBackoff ? _maxBackoff : exp;
    return capped * (0.5 + options.random.nextDouble() / 2);
  }

  void _schedule(Duration wait, {bool replace = false}) {
    if (_closed) return;
    var dueAt = clock.now().add(wait);
    if (_backoffUntil.isAfter(dueAt)) dueAt = _backoffUntil;
    if (!replace && _timer != null && !_timerDueAt!.isAfter(dueAt)) return;
    _timer?.cancel();
    _timerDueAt = dueAt;
    _timer = Timer(dueAt.difference(clock.now()), () {
      _timer = null;
      flush();
    });
  }

  // Remove by identity, so events added while a send was in flight stay.
  void _remove(List<JsonMap> batch) {
    _queue.removeWhere((e) => batch.any((b) => identical(b, e)));
    _persist();
  }

  Future<void> _persist() {
    final storage = options.storage;
    if (storage == null) return Future.value();
    if (_saveQueued) return _persistChain; // a write that will see this change is already waiting
    _saveQueued = true;
    // Writes run one after another, so an older snapshot can never land
    // after a newer one; each writes the queue as it is when it starts.
    _persistChain = _persistChain.then((_) async {
      _saveQueued = false;
      try {
        await storage.save(List<JsonMap>.of(_queue));
      } catch (e) {
        _log('could not save queued events: $e');
      }
    });
    return _persistChain;
  }

  Future<void> _loadSaved() async {
    final storage = options.storage;
    if (storage == null) return;
    List<JsonMap> saved;
    try {
      saved = await storage.load();
    } catch (e) {
      _log('could not load saved events: $e');
      return;
    }
    final known = {for (final e in _queue) e['event_id']};
    final restored = saved.where((e) => e['event_id'] is String && !known.contains(e['event_id'])).toList();
    if (restored.isEmpty) return;
    _queue.insertAll(0, restored); // older events first
    if (_queue.length > options.maxQueueSize) _queue.removeRange(options.maxQueueSize, _queue.length);
    _log('restored ${restored.length} event(s) saved by a previous launch');
    _schedule(Duration.zero);
  }

  void _log(String message) {
    if (options.debug) (options.log ?? print)('[juju] $message');
  }
}

String _typeName(Object error) {
  final name = error.runtimeType.toString();
  // Private implementation classes (_TypeError, _CastError) read better without the underscore.
  return name.startsWith('_') ? name.substring(1) : name;
}

String _message(Object error) {
  final text = error.toString();
  // Many Dart errors start their toString() with their own type
  // ("FormatException: Unexpected character", "Exception: x"). The type is
  // reported separately, so drop the repeat.
  for (final prefix in ['${_typeName(error)}: ', '${error.runtimeType}: ', 'Exception: ']) {
    if (text.startsWith(prefix)) return text.substring(prefix.length);
  }
  return text;
}

/// A random version 4 UUID.
String uuid4(Random random) {
  final b = List<int>.generate(16, (_) => random.nextInt(256));
  b[6] = (b[6] & 0x0f) | 0x40; // version 4
  b[8] = (b[8] & 0x3f) | 0x80; // RFC 4122 variant
  final hex = b.map((x) => x.toRadixString(16).padLeft(2, '0')).join();
  return '${hex.substring(0, 8)}-${hex.substring(8, 12)}-${hex.substring(12, 16)}-${hex.substring(16, 20)}-${hex.substring(20)}';
}
