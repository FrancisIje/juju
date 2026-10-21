import { Breadcrumbs } from "./breadcrumbs";
import { parseDsn, type Dsn } from "./dsn";
import { defaultIsInApp, exceptionFromThrown, type InAppFn } from "./stacktrace";
import { isoTime, monotonicClock, noopSpan, Transaction, type Span, type SpanOptions, type TransactionHost, type TransactionOptions } from "./tracing";
import { byteLength, fetchTransport, type SendResult, type Transport } from "./transport";
import type { AnalyticsEvent, Breadcrumb, Envelope, IdentifyEvent, JujuEvent, OutgoingEvent, SdkInfo, SessionData, SessionEvent, TransactionEvent, User } from "./types";
import { uuid4 } from "./uuid";

export interface ClientOptions {
  dsn: string;
  release?: string;
  environment?: string;
  /** Set by the platform SDK, e.g. "javascript". */
  platform?: string;
  sdk?: SdkInfo;
  transport?: Transport;
  /** Decides which stack frames are the app's own code. */
  isInApp?: InAppFn;
  /** Extra context attached to every event, e.g. browser and page URL. */
  context?: () => Record<string, unknown>;
  /** Change or drop (return null) an event before it is queued. */
  beforeSend?: (event: JujuEvent) => JujuEvent | null;
  /** How long to wait to collect a batch before sending. Default 5000 ms. */
  flushIntervalMs?: number;
  /** Send immediately once this many events are waiting. Default 20. */
  maxBatchSize?: number;
  /** Errors kept while offline. New ones past this are dropped. Default 100. */
  maxQueueSize?: number;
  /**
   * Everything that isn't an error (analytics events, session updates,
   * transactions) kept while offline, counted separately from errors so a
   * burst of it can never push a crash report out. Default 1000.
   */
  maxAnalyticsQueueSize?: number;
  /**
   * Where the anonymous id (and the user id after identify) is kept between
   * launches, so one device stays one person. localStorage on the web,
   * AsyncStorage on React Native. Without it the id lasts for this session.
   */
  identityStorage?: KeyValueStorage;
  maxBreadcrumbs?: number;
  debug?: boolean;
  logger?: Pick<Console, "warn">;
  /** Injectable for tests: returns a number in [0, 1). */
  random?: () => number;
  /**
   * Saves the queue so events survive the app being killed, and reloads
   * them on the next start. Used by React Native, where a crash ends the
   * process before a send can finish.
   */
  persistence?: QueueStorage;
  /** The clock for transactions, in ms since 1970. Defaults to a monotonic clock. */
  now?: () => number;
  /** Share of transactions to send, 0 to 1. Default 1: all of them. */
  tracesSampleRate?: number;
  /**
   * Share of errors to send, 0 to 1. Default 1: all of them. For apps with
   * so many users that every error would be far more than you need (or
   * than your plan's quota). A dropped error still counts in its session,
   * so crash-free rates stay exact. Sessions and analytics events are
   * never sampled: counts built from them would be wrong.
   */
  sampleRate?: number;
}

/** Somewhere to keep queued events between app launches. */
export interface QueueStorage {
  load(): Promise<OutgoingEvent[]>;
  save(events: OutgoingEvent[]): Promise<void>;
}

/** How long an app can sit in the background and still continue the same session. */
export const SESSION_RESUME_MS = 30_000;

interface SessionState extends Omit<SessionData, "started"> {
  startedMs: number;
  pausedAt?: number;
}

/** The shape localStorage and React Native's AsyncStorage both have. */
export interface KeyValueStorage {
  getItem(key: string): string | null | Promise<string | null>;
  setItem(key: string, value: string): void | Promise<void>;
}

const ANON_KEY = "juju.anon_id";
const DISTINCT_KEY = "juju.distinct_id";

const BASE_BACKOFF_MS = 1_000;
const MAX_BACKOFF_MS = 60_000;

export class Client implements TransactionHost {
  readonly dsn: Dsn;
  readonly breadcrumbs: Breadcrumbs;

  private readonly opts: Required<Pick<ClientOptions, "flushIntervalMs" | "maxBatchSize" | "maxQueueSize" | "random">> & ClientOptions;
  private readonly transport: Transport;
  private readonly isInApp: InAppFn;
  private queue: OutgoingEvent[] = [];
  private session: SessionState | undefined;
  private active: Transaction | undefined;
  private readonly clock: () => number;
  // Identity: anonId is this device's random id; distinctId is who events
  // are from (anonId until identify(), then the user id).
  private anonId = "";
  private distinctId = "";
  // capture/identify/reset run in call order on this chain, after the stored
  // ids have loaded: on React Native storage is asynchronous, and events
  // captured in the first moments must use the stored id, not a new one.
  private identityChain: Promise<void>;
  private timer: ReturnType<typeof setTimeout> | undefined;
  private timerDueAt = 0;
  private inFlight: Promise<void> | undefined;
  private failures = 0;
  private backoffUntil = 0;
  private user: User | undefined;
  private tags: Record<string, string> = {};
  private closed = false;
  // The same Error object can reach us twice: the app reports it with
  // captureException and then rethrows it to the global handler. A WeakSet
  // remembers objects without keeping them alive for the garbage collector.
  private seen = new WeakSet<object>();
  // Saves run one after another, in order, so an older snapshot can never
  // overwrite a newer one. saveQueued coalesces bursts into one write.
  private persistChain: Promise<void> = Promise.resolve();
  private saveQueued = false;
  /** Resolves once events saved by the previous launch are back in the queue. */
  readonly ready: Promise<void>;

  constructor(options: ClientOptions) {
    this.dsn = parseDsn(options.dsn); // throws early on a bad DSN: a config mistake should be loud
    this.opts = {
      flushIntervalMs: 5_000,
      maxBatchSize: 20,
      maxQueueSize: 100,
      random: Math.random,
      ...options,
    };
    this.transport = options.transport ?? fetchTransport(this.dsn);
    this.isInApp = options.isInApp ?? defaultIsInApp;
    this.breadcrumbs = new Breadcrumbs(options.maxBreadcrumbs ?? 100);
    this.clock = options.now ?? monotonicClock();

    // Load before any save: an error captured in the first moments after
    // launch would otherwise save a queue without the old events, wiping
    // the crash report we're about to read back.
    this.ready = this.loadSaved();
    this.persistChain = this.ready;
    this.identityChain = this.loadIdentity();
  }

  /**
   * Records something a user did, for product analytics: capture("checkout_started",
   * { plan: "pro" }). Property values are stored as text (numbers and true/false
   * too), which is what breakdowns group by. Never throws.
   */
  capture(name: string, properties?: Record<string, unknown>): void {
    try {
      if (this.closed || typeof name !== "string" || !name.trim()) return;
      // Stamped now, so the time is when it happened even if the id loads later.
      const base = this.analyticsBase();
      const props = properties && typeof properties === "object" ? { ...properties } : undefined;
      this.afterIdentity(() => {
        const event: AnalyticsEvent = { ...base, type: "event", name, distinct_id: this.distinctId };
        if (props && Object.keys(props).length > 0) event.properties = props;
        this.enqueue(event);
      });
    } catch (err) {
      this.log("could not capture event", err);
    }
  }

  /**
   * Says who this device's user is, after they log in. Events from before
   * (sent with the anonymous id) and after count as one person. Also sets
   * the user on error reports, unless setUser already did.
   */
  identify(userId: string): void {
    try {
      if (this.closed || typeof userId !== "string" || !userId.trim()) return;
      const base = this.analyticsBase();
      if (!this.user) this.user = { id: userId };
      this.afterIdentity(() => {
        if (userId === this.distinctId) return; // already identified as this user
        const event: IdentifyEvent = { ...base, type: "identify", distinct_id: userId };
        // Link the anonymous history only while still anonymous. Someone
        // switching accounts on a shared device starts fresh, instead of
        // merging two people.
        if (this.distinctId === this.anonId) event.anon_id = this.anonId;
        this.distinctId = userId;
        void this.saveIdentity();
        this.enqueue(event);
      });
    } catch (err) {
      this.log("could not identify", err);
    }
  }

  /**
   * Forgets the user, after they log out: a new anonymous id, so whoever
   * uses the device next isn't counted as them.
   */
  reset(): void {
    this.user = undefined;
    this.afterIdentity(() => {
      this.anonId = uuid4();
      this.distinctId = this.anonId;
      void this.saveIdentity();
    });
  }

  /** Who events are currently sent as. Resolves once the stored id has loaded. */
  async getDistinctId(): Promise<string> {
    await this.identityChain;
    return this.distinctId;
  }

  /**
   * Reports an error and returns its event_id, or "" if it was dropped.
   * Never throws: a bug in the error reporter must not crash the app.
   */
  captureException(thrown: unknown, extra?: { tags?: Record<string, string>; crashed?: boolean }): string {
    try {
      if (this.closed) return "";
      if (typeof thrown === "object" && thrown !== null) {
        if (this.seen.has(thrown)) {
          // Already reported (say, by the app, before it rethrew). Not a
          // second error, but if it's now crashing the app, the session
          // must still end as crashed.
          if (extra?.crashed) this.sessionError(true, false);
          return "";
        }
        this.seen.add(thrown);
      }
      this.sessionError(extra?.crashed ?? false, true);
      const rate = this.opts.sampleRate ?? 1;
      if (rate < 1 && this.opts.random() >= rate) {
        this.log(`error dropped by sampleRate ${rate}`);
        return "";
      }
      let event: JujuEvent | null = this.buildEvent(thrown, extra?.tags);
      if (this.opts.beforeSend) event = this.opts.beforeSend(event);
      if (!event) return "";
      return this.enqueue(event) ? event.event_id : "";
    } catch (err) {
      this.log("could not capture error", err);
      return "";
    }
  }

  addBreadcrumb(crumb: Omit<Breadcrumb, "timestamp"> & { timestamp?: string }): void {
    try {
      this.breadcrumbs.add({ ...crumb, timestamp: crumb.timestamp ?? new Date().toISOString() });
    } catch {
      // never throw into the app
    }
  }

  // Sessions -----------------------------------------------------------
  //
  // A session is one run of the app in the foreground, or one page load.
  // Each change is sent as an update with a higher seq; the server merges
  // them into one row. Updates wait in the same queue as errors, so they
  // get the same retries, offline saving and batching.

  /** Starts a new session, ending the current one first. */
  startSession(): void {
    if (this.closed) return;
    if (this.session) this.endSession();
    this.session = { sid: uuid4(), seq: 0, status: "ok", errors: 0, startedMs: Date.now() };
    this.sendSession();
  }

  /** Ends the session normally (the page closes, the app quits). */
  endSession(): void {
    const s = this.session;
    if (!s) return;
    this.session = undefined;
    if (s.pausedAt !== undefined) return; // already sent as exited when it was paused
    s.status = "exited";
    s.duration_ms = Math.max(0, Math.round(Date.now() - s.startedMs));
    s.seq++;
    this.sendSession(s);
  }

  /**
   * The app went to the background. The OS may kill it there without
   * warning, so the session is sent as ended now. If the app comes back
   * soon, resumeSession continues it.
   */
  pauseSession(): void {
    const s = this.session;
    if (!s || s.pausedAt !== undefined) return;
    s.pausedAt = Date.now();
    s.status = "exited";
    s.duration_ms = Math.max(0, Math.round(s.pausedAt - s.startedMs));
    s.seq++;
    this.sendSession(s);
  }

  /**
   * The app is back in the foreground. A short trip away (checking a
   * message) continues the same session; after SESSION_RESUME_MS it's a
   * new one. Without this, every app switch would count as a session.
   */
  resumeSession(): void {
    const s = this.session;
    if (!s) return this.startSession();
    if (s.pausedAt === undefined) return;
    if (Date.now() - s.pausedAt > SESSION_RESUME_MS) {
      this.session = undefined;
      return this.startSession();
    }
    s.pausedAt = undefined;
    s.status = "ok";
    delete s.duration_ms;
    s.seq++;
    this.sendSession(s);
  }

  /** The current session's id, if one is running. */
  sessionId(): string | undefined {
    return this.session?.sid;
  }

  private sessionError(crashed: boolean, count: boolean): void {
    const s = this.session;
    if (!s || this.closed) return;
    if (count) s.errors++;
    if (crashed) {
      s.status = "crashed";
      s.duration_ms = Math.max(0, Math.round(Date.now() - s.startedMs));
      this.session = undefined; // a crash ends the session
    }
    s.seq++;
    this.sendSession(s);
  }

  private sendSession(s: SessionState | undefined = this.session): void {
    if (!s) return;
    const session: SessionData = { sid: s.sid, seq: s.seq, status: s.status, started: new Date(s.startedMs).toISOString(), errors: s.errors };
    if (s.duration_ms !== undefined) session.duration_ms = s.duration_ms;
    const event: SessionEvent = { event_id: uuid4(), type: "session", timestamp: new Date().toISOString(), platform: this.opts.platform ?? "javascript", session };
    if (this.opts.release) event.release = this.opts.release;
    if (this.opts.environment) event.environment = this.opts.environment;
    if (this.user) event.user = { ...this.user };
    // Only the latest state of a session matters. If an older update is
    // still waiting (offline), replace it instead of queueing both: an app
    // switched away from 50 times offline sends one update, not 50.
    const waiting = this.queue.findIndex((e) => e.type === "session" && e.session.sid === s.sid);
    if (waiting >= 0) {
      this.queue[waiting] = event;
      void this.persist();
      this.schedule(this.opts.flushIntervalMs);
      return;
    }
    this.enqueue(event);
  }

  // Transactions ---------------------------------------------------------

  now(): number {
    return this.clock();
  }

  /**
   * Starts timing an operation. `active: true` makes it the transaction
   * that automatic spans (HTTP requests) join; starting another active one
   * finishes the previous one first.
   */
  startTransaction(opts: TransactionOptions & { active?: boolean }): Transaction {
    const rate = this.opts.tracesSampleRate ?? 1;
    const tx = new Transaction(this, { sampled: rate >= 1 || this.opts.random() < rate, ...opts });
    if (opts.active) {
      this.active?.finish();
      this.active = tx;
    }
    return tx;
  }

  /** The transaction automatic spans join, if one is running. */
  activeTransaction(): Transaction | undefined {
    return this.active?.finished ? undefined : this.active;
  }

  /** Starts a span in the active transaction, or a no-op span if there is none. */
  startSpan(opts: SpanOptions): Span {
    return this.activeTransaction()?.startChild(opts) ?? noopSpan();
  }

  /** @internal Called by Transaction.finish. */
  finishTransaction(tx: Transaction): void {
    if (this.active === tx) this.active = undefined;
    if (!tx.sampled || this.closed || tx.end === undefined) return;
    try {
      const event: TransactionEvent = {
        event_id: uuid4(),
        type: "transaction",
        timestamp: isoTime(tx.end),
        platform: this.opts.platform ?? "javascript",
        transaction: { name: tx.name, op: tx.op, status: tx.status, start_timestamp: isoTime(tx.start) },
      };
      const spans = tx.spans.filter((sp) => sp.end !== undefined && sp.start <= tx.end!);
      if (spans.length > 0) {
        event.transaction.spans = spans.map((sp) => {
          const out: NonNullable<TransactionEvent["transaction"]["spans"]>[number] = {
            span_id: sp.spanId,
            op: sp.op,
            start_timestamp: isoTime(sp.start),
            timestamp: isoTime(Math.min(sp.end!, tx.end!)),
          };
          if (sp.description) out.description = sp.description;
          if (sp.status) out.status = sp.status;
          if (sp.data) out.data = sp.data;
          return out;
        });
      }
      if (Object.keys(tx.measurements).length > 0) event.transaction.measurements = { ...tx.measurements };
      if (this.opts.release) event.release = this.opts.release;
      if (this.opts.environment) event.environment = this.opts.environment;
      if (this.user) event.user = { ...this.user };
      if (Object.keys(this.tags).length > 0) event.tags = { ...this.tags };
      const context = this.opts.context?.();
      if (context) event.context = context;
      this.enqueue(event);
    } catch (err) {
      this.log("could not record transaction", err);
    }
  }

  setUser(user: User | null): void {
    this.user = user ?? undefined;
  }

  setTag(key: string, value: string): void {
    this.tags[key] = value;
  }

  /** Number of events waiting to be sent. */
  pending(): number {
    return this.queue.length;
  }

  /**
   * Sends everything queued now, batch by batch, and resolves when done or
   * when a send fails (a retry is then scheduled). Only one send runs at a
   * time, so calling flush repeatedly can't create a burst of requests.
   */
  flush(): Promise<void> {
    if (!this.inFlight) {
      this.inFlight = this.sendLoop().finally(() => {
        this.inFlight = undefined;
      });
    }
    return this.inFlight;
  }

  /** Forget the backoff and try now: the platform calls this when the device comes back online. */
  retryNow(): void {
    this.failures = 0;
    this.backoffUntil = 0;
    if (this.queue.length > 0) this.schedule(0);
  }

  /**
   * Hands queued events to `send` in envelopes of at most maxBytes, removing
   * each batch only if `send` returns true. Used for sendBeacon when the page
   * is being hidden, where we can't wait for a response.
   */
  drain(send: (envelope: Envelope, body: string) => boolean, maxBytes: number): void {
    while (this.queue.length > 0) {
      const batch: OutgoingEvent[] = [];
      let body = "";
      for (const e of this.queue) {
        const next = JSON.stringify(this.envelope([...batch, e]));
        const bytes = byteLength(next); // bytes, not string.length: the beacon limit is in bytes
        if (bytes > maxBytes && batch.length > 0) break;
        if (bytes > maxBytes) {
          // A single event too big for a beacon: leave it for a normal send.
          this.log("event too large for sendBeacon, keeping it for the next send");
          return;
        }
        batch.push(e);
        body = next;
      }
      if (!send(this.envelope(batch), body)) return;
      this.remove(batch);
    }
  }

  close(): void {
    this.active?.finish();
    this.closed = true;
    clearTimeout(this.timer);
    this.timer = undefined;
  }

  private buildEvent(thrown: unknown, extraTags?: Record<string, string>): JujuEvent {
    const event: JujuEvent = {
      event_id: uuid4(), // created now, reused by every retry, so the server can dedupe
      type: "error",
      timestamp: new Date().toISOString(),
      platform: this.opts.platform ?? "javascript",
      exception: exceptionFromThrown(thrown, this.isInApp),
    };
    if (this.opts.release) event.release = this.opts.release;
    if (this.opts.environment) event.environment = this.opts.environment;
    const crumbs = this.breadcrumbs.snapshot();
    if (crumbs.length > 0) event.breadcrumbs = crumbs;
    if (this.user) event.user = { ...this.user };
    const tags = { ...this.tags, ...extraTags };
    if (Object.keys(tags).length > 0) event.tags = tags;
    const context = this.opts.context?.();
    if (context) event.context = context;
    return event;
  }

  private analyticsBase() {
    const base: { event_id: string; timestamp: string; platform: string; release?: string; environment?: string; context?: Record<string, unknown> } = {
      event_id: uuid4(),
      timestamp: new Date().toISOString(),
      platform: this.opts.platform ?? "javascript",
    };
    if (this.opts.release) base.release = this.opts.release;
    if (this.opts.environment) base.environment = this.opts.environment;
    // The device context lets breakdowns by OS and browser work.
    const context = this.opts.context?.();
    if (context) base.context = context;
    return base;
  }

  private afterIdentity(fn: () => void): void {
    this.identityChain = this.identityChain.then(fn).catch((err) => this.log("analytics", err));
  }

  private async loadIdentity(): Promise<void> {
    const store = this.opts.identityStorage;
    let anon: string | null = null;
    let distinct: string | null = null;
    if (store) {
      try {
        anon = await store.getItem(ANON_KEY);
        distinct = await store.getItem(DISTINCT_KEY);
      } catch (err) {
        this.log("could not read the stored id", err);
      }
    }
    this.anonId = anon || uuid4();
    this.distinctId = distinct || this.anonId;
    if (!anon) await this.saveIdentity();
  }

  private async saveIdentity(): Promise<void> {
    const store = this.opts.identityStorage;
    if (!store) return;
    try {
      await store.setItem(ANON_KEY, this.anonId);
      await store.setItem(DISTINCT_KEY, this.distinctId);
    } catch (err) {
      this.log("could not store the id", err); // the id lasts for this session, which is still useful
    }
  }

  private enqueue(event: OutgoingEvent): boolean {
    // Errors get their own limit, so a flood of anything else (analytics,
    // session updates, transactions) can never push out a crash report.
    const analytics = event.type !== "error";
    const sameKind = this.queue.reduce((n, e) => n + ((e.type !== "error") === analytics ? 1 : 0), 0);
    const limit = analytics ? (this.opts.maxAnalyticsQueueSize ?? 1000) : this.opts.maxQueueSize;
    if (sameKind >= limit) {
      // Drop the newest, not the oldest: when the queue overflows it's
      // usually a loop repeating the same error, and the first ones are
      // the most likely to show the cause.
      this.log(`queue full (${limit} ${analytics ? "non-error events" : "errors"}), dropping event`);
      return false;
    }
    this.queue.push(event);
    void this.persist();
    this.schedule(this.queue.length >= this.opts.maxBatchSize ? 0 : this.opts.flushIntervalMs);
    return true;
  }

  private async sendLoop(): Promise<void> {
    while (this.queue.length > 0 && !this.closed) {
      const batch = this.queue.slice(0, this.opts.maxBatchSize);
      let result: SendResult;
      try {
        result = await this.transport.send(this.envelope(batch));
      } catch {
        result = { ok: false, retry: true };
      }

      if (result.ok || !result.retry) {
        if (!result.ok) this.log(`server rejected ${batch.length} event(s) with ${result.status}; not retrying`);
        this.remove(batch);
        this.failures = 0;
        this.backoffUntil = 0;
        continue;
      }

      this.failures++;
      const delay = Math.max(result.retryAfterMs ?? 0, this.backoffDelay());
      this.backoffUntil = Date.now() + delay;
      this.log(`send failed, retrying in ${Math.round(delay)} ms`);
      // Replace any timer set earlier (say, the 5 s batch timer): it was
      // scheduled before we knew about the backoff and would fire too soon.
      this.schedule(delay, true);
      return;
    }
  }

  /**
   * Exponential backoff with jitter: 1 s, 2 s, 4 s, ... capped at 60 s, each
   * multiplied by a random factor between 0.5 and 1 so that thousands of
   * apps recovering from the same outage don't all retry at the same second.
   */
  private backoffDelay(): number {
    const exp = Math.min(MAX_BACKOFF_MS, BASE_BACKOFF_MS * 2 ** (this.failures - 1));
    return exp * (0.5 + this.opts.random() / 2);
  }

  /**
   * Runs flush after `ms`, never earlier than the backoff allows. An existing
   * timer that is due sooner is kept, unless `replace` is set.
   */
  private schedule(ms: number, replace = false): void {
    if (this.closed) return;
    const dueAt = Math.max(Date.now() + ms, this.backoffUntil);
    if (!replace && this.timer !== undefined && this.timerDueAt <= dueAt) return;
    clearTimeout(this.timer);
    this.timerDueAt = dueAt;
    this.timer = setTimeout(() => {
      this.timer = undefined;
      void this.flush();
    }, dueAt - Date.now());
  }

  // Remove by identity, not by position: a beacon may have taken events from
  // the front of the queue while this batch was in flight.
  private remove(batch: OutgoingEvent[]): void {
    const sent = new Set(batch);
    this.queue = this.queue.filter((e) => !sent.has(e));
    void this.persist();
  }

  /**
   * Writes the current queue to storage. Returns a promise for that write,
   * so a fatal-crash handler can wait for it before the app dies.
   */
  persistNow(): Promise<void> {
    return this.persist();
  }

  private persist(): Promise<void> {
    const store = this.opts.persistence;
    if (!store) return Promise.resolve();
    if (this.saveQueued) return this.persistChain; // a write that will see this change is already waiting
    this.saveQueued = true;
    this.persistChain = this.persistChain.then(async () => {
      this.saveQueued = false;
      try {
        await store.save(this.queue.slice()); // the queue as it is when the write starts: always the latest
      } catch (err) {
        this.log("could not save queued events", err);
      }
    });
    return this.persistChain;
  }

  private async loadSaved(): Promise<void> {
    const store = this.opts.persistence;
    if (!store) return;
    let saved: unknown;
    try {
      saved = await store.load();
    } catch (err) {
      this.log("could not load saved events", err);
      return;
    }
    if (!Array.isArray(saved)) return;
    // The same event can be both in memory and on disk (it was captured
    // after launch and saved). event_id tells them apart.
    const known = new Set(this.queue.map((e) => e.event_id));
    const restored = saved.filter(
      (e): e is OutgoingEvent => typeof e === "object" && e !== null && typeof (e as OutgoingEvent).event_id === "string" && !known.has((e as OutgoingEvent).event_id),
    );
    if (restored.length === 0) return;
    // Older events first, and never more than the queue allows.
    this.queue = [...restored, ...this.queue].slice(0, this.opts.maxQueueSize);
    this.log(`restored ${restored.length} event(s) saved by a previous launch`);
    this.schedule(0);
  }

  private envelope(events: OutgoingEvent[]): Envelope {
    return { sdk: this.opts.sdk ?? { name: "juju.core", version: VERSION }, events };
  }

  private log(message: string, err?: unknown): void {
    if (this.opts.debug) (this.opts.logger ?? console).warn(`[juju] ${message}`, ...(err === undefined ? [] : [err]));
  }
}

export const VERSION = "0.1.0";
