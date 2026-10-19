import { Breadcrumbs } from "./breadcrumbs";
import { parseDsn, type Dsn } from "./dsn";
import { defaultIsInApp, exceptionFromThrown, type InAppFn } from "./stacktrace";
import { fetchTransport, type SendResult, type Transport } from "./transport";
import type { Breadcrumb, Envelope, JujuEvent, SdkInfo, User } from "./types";
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
  /** Events kept while offline. New events past this are dropped. Default 100. */
  maxQueueSize?: number;
  maxBreadcrumbs?: number;
  debug?: boolean;
  logger?: Pick<Console, "warn">;
  /** Injectable for tests: returns a number in [0, 1). */
  random?: () => number;
}

const BASE_BACKOFF_MS = 1_000;
const MAX_BACKOFF_MS = 60_000;

export class Client {
  readonly dsn: Dsn;
  readonly breadcrumbs: Breadcrumbs;

  private readonly opts: Required<Pick<ClientOptions, "flushIntervalMs" | "maxBatchSize" | "maxQueueSize" | "random">> & ClientOptions;
  private readonly transport: Transport;
  private readonly isInApp: InAppFn;
  private queue: JujuEvent[] = [];
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
  }

  /**
   * Reports an error and returns its event_id, or "" if it was dropped.
   * Never throws: a bug in the error reporter must not crash the app.
   */
  captureException(thrown: unknown, extra?: { tags?: Record<string, string> }): string {
    try {
      if (this.closed) return "";
      if (typeof thrown === "object" && thrown !== null) {
        if (this.seen.has(thrown)) return "";
        this.seen.add(thrown);
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
      const batch: JujuEvent[] = [];
      let body = "";
      for (const e of this.queue) {
        const next = JSON.stringify(this.envelope([...batch, e]));
        if (next.length > maxBytes && batch.length > 0) break;
        if (next.length > maxBytes) {
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

  private enqueue(event: JujuEvent): boolean {
    if (this.queue.length >= this.opts.maxQueueSize) {
      // Drop the newest, not the oldest: when the queue overflows it's
      // usually a loop repeating the same error, and the first ones are
      // the most likely to show the cause.
      this.log(`queue full (${this.opts.maxQueueSize}), dropping event`);
      return false;
    }
    this.queue.push(event);
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
  private remove(batch: JujuEvent[]): void {
    const sent = new Set(batch);
    this.queue = this.queue.filter((e) => !sent.has(e));
  }

  private envelope(events: JujuEvent[]): Envelope {
    return { sdk: this.opts.sdk ?? { name: "juju.core", version: VERSION }, events };
  }

  private log(message: string, err?: unknown): void {
    if (this.opts.debug) (this.opts.logger ?? console).warn(`[juju] ${message}`, ...(err === undefined ? [] : [err]));
  }
}

export const VERSION = "0.1.0";
