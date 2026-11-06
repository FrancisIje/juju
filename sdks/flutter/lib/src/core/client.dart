import 'dart:async';
import 'dart:convert';
import 'dart:math';

import 'package:clock/clock.dart';

import 'dsn.dart';
import 'stacktrace.dart';
import 'tracing.dart';
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
    this.tracesSampleRate = 1,
    this.sampleRate = 1,
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

  /// Share of transactions to send, 0 to 1.
  final double tracesSampleRate;

  /// Share of errors to send, 0 to 1. A dropped error still counts in its
  /// session, so crash-free rates stay exact. Sessions and analytics events
  /// are never sampled.
  final double sampleRate;
  final Random random;
}

/// How long an app can sit in the background and still continue the same session.
const sessionResumeAfter = Duration(seconds: 30);

class _Session {
  _Session(this.sid, this.started);
  final String sid;
  final DateTime started;
  int seq = 0;
  String status = 'ok';
  int errors = 0;
  int? durationMs;
  DateTime? pausedAt;
}

const jujuVersion = '0.1.0';
const _baseBackoff = Duration(seconds: 1);
// The server's limits (server/internal/event): at most 250 frames, keeping
// the 50 nearest the crash and the 200 nearest the program's entry point.
const _maxFrames = 250;
const _keepTopFrames = 50;
const _keepRootFrames = 200;
const _maxBackoff = Duration(seconds: 60);

/// The longest a server's Retry-After is obeyed. It's a number from the
/// network: a wrong one must not silence the SDK for good, or overflow a date.
const _maxRetryAfter = Duration(hours: 1);

/// Saves of the queue are at least this far apart. Each one encodes the whole
/// queue on the UI thread, so saving for every event of a burst drops frames.
const _minSaveInterval = Duration(seconds: 2);

/// How long to wait for the stored id before going on without it.
const _storageTimeout = Duration(seconds: 2);

// Memory limits. Counting events isn't enough: an exception whose message
// holds a whole response body can be megabytes, and an app that runs out of
// memory is killed by the OS.
const _maxMessageChars = 8192; // the server's limit (server/internal/event)
const _maxEventBytes = 1 << 20; // the server's limit for a whole request
const _maxQueueBytes = 8 << 20; // for errors, and again for everything else

/// Builds events, queues them, sends them in batches and retries with
/// backoff. A port of @juju/core's Client, so the JavaScript and Dart SDKs
/// follow the same rules.
class JujuClient implements TransactionHost {
  JujuClient(this.options)
      : dsn = Dsn.parse(options.dsn), // throws early on a bad DSN
        _isInApp = options.isInApp ?? makeIsInApp() {
    _transport = options.transport ?? HttpTransport(dsn);
    // Load before any save: an error captured in the first moments after
    // launch would otherwise save a queue without the old events, wiping
    // the report we're about to read back.
    ready = _loadSaved();
    _persistChain = ready;
    _identityChain = _loadIdentity().catchError((Object e) => _log('could not load identity: $e'));
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

  _Session? _session;
  JujuTransaction? _active;

  // Encoded size of each queued event, for the memory limits. An Expando
  // attaches it without changing the event, which is what gets sent.
  final _sizes = Expando<int>('juju-size');
  Timer? _saveTimer;
  DateTime _lastSave = DateTime.fromMillisecondsSinceEpoch(0);

  int get pending => _queue.length;
  List<JsonMap> get breadcrumbs => List.unmodifiable(_breadcrumbs);

  /// Reports an error and returns its event_id, or "" if it was dropped.
  /// Never throws: a bug in the error reporter must not break the app.
  /// [crashed]: the error reached a global hook nothing else handled, and
  /// ends the session as crashed.
  String captureException(Object error, StackTrace? stack, {Map<String, String>? tags, String? mechanism, bool crashed = false}) {
    try {
      if (_closed) return '';
      if (_markSeen(error)) {
        // Already reported (by the app, before rethrowing it). Not a second
        // error, but if it's now unhandled the session still crashed.
        if (crashed) _sessionError(crashed: true, count: false);
        return '';
      }
      _sessionError(crashed: crashed, count: true);
      // Full queue: skip the work of parsing a stack that would be dropped. A
      // widget failing on every frame must not get slower because of us.
      if (_kindIsFull(analytics: false)) {
        _log('queue full (${options.maxQueueSize} errors), dropping event');
        return '';
      }
      if (options.sampleRate < 1 && options.random.nextDouble() >= options.sampleRate) {
        _log('error dropped by sampleRate ${options.sampleRate}');
        return '';
      }
      JsonMap? event = _buildEvent(error, stack, tags, mechanism);
      if (options.beforeSend != null) event = options.beforeSend!(event);
      if (event == null) return '';
      return _enqueue(event) ? event['event_id'] as String : '';
    } catch (e) {
      _log('could not capture error: $e');
      return '';
    }
  }

  void addBreadcrumb(String type, {String? message, JsonMap? data}) => _guard('add breadcrumb', () {
        // Made safe now: a bad value kept here would be in every later error.
        final safeData = data == null ? null : _jsonSafe(data)?.$1;
        _breadcrumbs.add({
          'type': type,
          if (message != null) 'message': message.length > 1000 ? message.substring(0, 1000) : message,
          'timestamp': clock.now().toUtc().toIso8601String(),
          if (safeData != null) 'data': safeData,
        });
        // A ring buffer: the oldest falls off, so memory stays flat.
        if (_breadcrumbs.length > options.maxBreadcrumbs) _breadcrumbs.removeAt(0);
      });

  // Sessions: one run of the app in the foreground. Each change is sent as
  // an update with a higher seq; the server merges them into one row. See
  // @juju/core's client.ts for the reasoning, which is the same here.

  /// Starts a new session, ending the current one first.
  void startSession() => _guard('start session', () {
        if (_closed) return;
        if (_session != null) endSession();
        _session = _Session(uuid4(options.random), clock.now());
        _sendSession(_session!);
      });

  /// Ends the session normally.
  void endSession() => _guard('end session', () {
        final s = _session;
        if (s == null) return;
        _session = null;
        if (s.pausedAt != null) return; // already sent as exited when paused
        s
          ..status = 'exited'
          ..durationMs = clock.now().difference(s.started).inMilliseconds
          ..seq += 1;
        _sendSession(s);
      });

  /// The app went to the background, where the OS may kill it without
  /// warning: send the session as ended now. resumeSession may continue it.
  void pauseSession() => _guard('pause session', () {
        final s = _session;
        if (s == null || s.pausedAt != null) return;
        s
          ..pausedAt = clock.now()
          ..status = 'exited'
          ..durationMs = clock.now().difference(s.started).inMilliseconds
          ..seq += 1;
        _sendSession(s);
      });

  /// Back in the foreground: continue the session after a short time away,
  /// start a new one after [sessionResumeAfter].
  void resumeSession() => _guard('resume session', () {
        final s = _session;
        if (s == null) return startSession();
        final paused = s.pausedAt;
        if (paused == null) return;
        if (clock.now().difference(paused) > sessionResumeAfter) {
          _session = null;
          return startSession();
        }
        s
          ..pausedAt = null
          ..status = 'ok'
          ..durationMs = null
          ..seq += 1;
        _sendSession(s);
      });

  String? get sessionId => _session?.sid;

  void _sessionError({required bool crashed, required bool count}) {
    final s = _session;
    if (s == null || _closed) return;
    if (count) s.errors++;
    if (crashed) {
      s
        ..status = 'crashed'
        ..durationMs = clock.now().difference(s.started).inMilliseconds;
      _session = null; // a crash ends the session
    }
    s.seq++;
    _sendSession(s);
  }

  void _sendSession(_Session s) {
    final event = <String, dynamic>{
      'event_id': uuid4(options.random),
      'type': 'session',
      'timestamp': clock.now().toUtc().toIso8601String(),
      'platform': options.platform,
      if (options.release != null) 'release': options.release,
      if (options.environment != null) 'environment': options.environment,
      if (_user != null) 'user': {..._user!},
      'session': {
        'sid': s.sid,
        'seq': s.seq,
        'status': s.status,
        'started': s.started.toUtc().toIso8601String(),
        'errors': s.errors,
        if (s.durationMs != null) 'duration_ms': s.durationMs,
      },
    };
    // Only the latest state matters: replace an update still waiting
    // (offline) instead of queueing both.
    // The queue may hold anything a restored file had in it, hence the checks.
    final waiting = _queue.indexWhere((e) {
      final session = e['session'];
      return e['type'] == 'session' && session is Map && session['sid'] == s.sid;
    });
    final safe = waiting >= 0 ? _jsonSafe(event) : null;
    if (safe != null) {
      _queue[waiting] = safe.$1;
      _sizes[safe.$1] = safe.$2;
      _persistSoon();
      _schedule(options.flushInterval);
      return;
    }
    _enqueue(event);
  }

  // Transactions.

  /// Starts timing an operation. [active] makes it the transaction that
  /// automatic spans (HTTP requests) join; starting another active one
  /// finishes the previous one first.
  JujuTransaction startTransaction(String name, String op,
      {DateTime? start, Duration? idleTimeout, Duration? maxDuration, bool active = false}) {
    try {
      final rate = options.tracesSampleRate;
      final tx = JujuTransaction(this,
          name: name,
          op: op,
          start: start,
          idleTimeout: idleTimeout,
          maxDuration: maxDuration,
          sampled: rate >= 1 || options.random.nextDouble() < rate,
          random: options.random);
      if (active) {
        _active?.finish();
        _active = tx;
      }
      return tx;
    } catch (e) {
      _log('could not start transaction: $e');
      return inertTransaction(name, op);
    }
  }

  JujuTransaction? get activeTransaction => (_active?.finished ?? true) ? null : _active;

  /// A span in the active transaction, or a no-op span if there is none.
  JujuSpan startSpan(String op, {String? description, JsonMap? data}) {
    try {
      return activeTransaction?.startChild(op, description: description, data: data) ?? noopSpan();
    } catch (e) {
      _log('could not start span: $e');
      return noopSpan();
    }
  }

  @override
  void finishTransaction(JujuTransaction tx) {
    try {
      if (identical(_active, tx)) _active = null;
      final end = tx.end;
      if (!tx.sampled || _closed || end == null) return;
      final spans = tx.spans.where((s) => s.end != null && !s.start.isAfter(end)).toList();
      _enqueue({
        'event_id': uuid4(options.random),
        'type': 'transaction',
        'timestamp': isoTime(end),
        'platform': options.platform,
        if (options.release != null) 'release': options.release,
        if (options.environment != null) 'environment': options.environment,
        if (_user != null) 'user': {..._user!},
        if (_tags.isNotEmpty) 'tags': {..._tags},
        if (options.context != null) 'context': options.context!(),
        'transaction': {
          'name': tx.name,
          'op': tx.op,
          'status': tx.status,
          'start_timestamp': isoTime(tx.start),
          if (tx.measurements.isNotEmpty) 'measurements': {...tx.measurements},
          if (spans.isNotEmpty) 'spans': [for (final s in spans) s.toJson(end)],
        },
      });
    } catch (e) {
      _log('could not record transaction: $e');
    }
  }

  // Made safe now: a bad value kept here would be in every later event.
  void setUser(JsonMap? user) => _guard('set user', () => _user = user == null ? null : _jsonSafe(user)?.$1);

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
  void reset() => _guard('reset', () {
        _user = null;
        _afterIdentity(() {
          _anonId = uuid4(options.random);
          _distinctId = _anonId;
          _saveIdentity();
        });
      });

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
      // A timeout, because capture() waits for this: if storage hangs, events
      // would pile up in memory behind it, with nothing ever sent.
      ids = await options.identityStorage?.load().timeout(_storageTimeout) ?? const {};
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
      await options.identityStorage?.save({'anon_id': _anonId, 'distinct_id': _distinctId}).timeout(_storageTimeout);
    } catch (e) {
      _log('could not store the id: $e'); // lasts for this run, which still counts correctly
    }
  }

  void setTag(String key, String value) => _tags[key] = value;

  /// Sends everything queued now. Only one send runs at a time. Never fails:
  /// timers call this with nobody listening, so a failure here would reach
  /// the app's global error handler as an error of the app's.
  Future<void> flush() => _inFlight ??= _sendSafely().whenComplete(() => _inFlight = null);

  Future<void> _sendSafely() async {
    try {
      await _sendLoop();
    } catch (e) {
      _log('could not send: $e');
    }
  }

  /// Forget the backoff and try now (the app came back to the foreground).
  void retryNow() => _guard('retry', () {
        _failures = 0;
        _backoffUntil = DateTime.fromMillisecondsSinceEpoch(0);
        if (_queue.isNotEmpty) _schedule(Duration.zero, replace: true);
      });

  /// Writes the queue to storage now; completes when the write is done.
  Future<void> persistNow() {
    _saveTimer?.cancel();
    _saveTimer = null;
    return _persist();
  }

  void close() {
    _guard('finish transaction', () => _active?.finish());
    _closed = true;
    _timer?.cancel();
    _timer = null;
    if (_saveTimer != null) persistNow(); // don't lose a save that was waiting
  }

  void _guard(String what, void Function() fn) {
    try {
      fn();
    } catch (e) {
      _log('could not $what: $e');
    }
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

  // Errors get their own limits, so a flood of anything else (analytics,
  // session updates, transactions) can never push out a crash report.
  Iterable<JsonMap> _ofKind({required bool analytics}) => _queue.where((e) => (e['type'] != 'error') == analytics);

  bool _kindIsFull({required bool analytics}) =>
      _ofKind(analytics: analytics).length >= (analytics ? options.maxAnalyticsQueueSize : options.maxQueueSize);

  /// A plain JSON copy of [value], and its encoded size. What can't be encoded
  /// (a DateTime, NaN, an object) becomes text: jsonEncode would otherwise
  /// throw at send time, and that batch could never be sent or get out of the
  /// way of the events behind it. Null if it can't be encoded at all (a cycle).
  (JsonMap, int)? _jsonSafe(JsonMap value) {
    try {
      final text = jsonEncode(value, toEncodable: _encodableText);
      return (jsonDecode(text) as JsonMap, text.length);
    } catch (e) {
      _log('dropping data that cannot be sent: $e');
      return null;
    }
  }

  bool _enqueue(JsonMap raw) {
    final analytics = raw['type'] != 'error';
    if (_kindIsFull(analytics: analytics)) {
      // Drop the newest: an overflowing queue is usually a loop repeating
      // the same thing, and the first ones are the most likely to show the cause.
      _log(
          'queue full (${analytics ? '${options.maxAnalyticsQueueSize} non-error events' : '${options.maxQueueSize} errors'}), dropping event');
      return false;
    }
    final safe = _jsonSafe(raw);
    if (safe == null) return false;
    final (event, size) = safe;
    final queued = _ofKind(analytics: analytics).fold<int>(0, (sum, e) => sum + (_sizes[e] ?? 0));
    if (size > _maxEventBytes || queued + size > _maxQueueBytes) {
      _log('event of $size bytes is over the memory limit, dropping it');
      return false;
    }
    _sizes[event] = size;
    _queue.add(event);
    _persistSoon();
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
          if (retryAfter != null && retryAfter > delay) delay = retryAfter > _maxRetryAfter ? _maxRetryAfter : retryAfter;
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
    // The exponent stops at 6 (64 s, past the cap). Dart ints wrap at 64 bits,
    // so an uncapped one turns into a negative delay, then 0, after about 45
    // failures: half an hour of outage would end in retrying non-stop.
    final exp = _baseBackoff * (1 << min(_failures - 1, 6));
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
    _persistSoon();
  }

  /// Saves now if the last save was a while ago, so a lone error is on disk at
  /// once; otherwise once the interval has passed, so a burst costs one save.
  /// A kill inside that window can lose the newest events, or send delivered
  /// ones again. The server dedupes those by event_id.
  void _persistSoon() {
    if (options.storage == null) return;
    if (_saveTimer != null) return;
    final wait = _lastSave.add(_minSaveInterval).difference(clock.now());
    if (wait <= Duration.zero) {
      _persist();
    } else {
      _saveTimer = Timer(wait, () {
        _saveTimer = null;
        _guard('save', _persist);
      });
    }
  }

  Future<void> _persist() {
    final storage = options.storage;
    if (storage == null) return Future.value();
    _lastSave = clock.now();
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
    try {
      if (options.debug) (options.log ?? print)('[juju] $message');
    } catch (_) {}
  }
}

Object? _encodableText(Object? value) {
  try {
    return value.toString();
  } catch (_) {
    return '<unprintable>';
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
  var message = text;
  for (final prefix in ['${_typeName(error)}: ', '${error.runtimeType}: ', 'Exception: ']) {
    if (text.startsWith(prefix)) {
      message = text.substring(prefix.length);
      break;
    }
  }
  return message.length > _maxMessageChars ? message.substring(0, _maxMessageChars) : message;
}

/// A random version 4 UUID.
String uuid4(Random random) {
  final b = List<int>.generate(16, (_) => random.nextInt(256));
  b[6] = (b[6] & 0x0f) | 0x40; // version 4
  b[8] = (b[8] & 0x3f) | 0x80; // RFC 4122 variant
  final hex = b.map((x) => x.toRadixString(16).padLeft(2, '0')).join();
  return '${hex.substring(0, 8)}-${hex.substring(8, 12)}-${hex.substring(12, 16)}-${hex.substring(16, 20)}-${hex.substring(20)}';
}
