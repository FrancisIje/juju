// Transactions and spans: how long things take, and where the time goes.
//
// A transaction is one operation a person waits for (a page load, an app
// start, a screen opening). Spans are the steps inside it (an HTTP
// request). Times are milliseconds since 1970 as floating point numbers, so
// sub-millisecond steps keep their precision until they're sent.

import { uuid4 } from "./uuid";

/** What a transaction needs from the client that created it. */
export interface TransactionHost {
  now(): number;
  finishTransaction(tx: Transaction): void;
}

export interface SpanOptions {
  op: string;
  description?: string;
  data?: Record<string, unknown>;
  /** Defaults to now. */
  start?: number;
}

export interface TransactionOptions {
  name: string;
  op: string;
  /** Defaults to now. */
  start?: number;
  /**
   * Makes this an idle transaction: it finishes by itself once nothing has
   * been happening in it (no open spans, no holds) for this long. Its end
   * is the moment the last activity ended, not when the timer fired.
   */
  idleTimeoutMs?: number;
  /** Finish with status "deadline_exceeded" after this long. Idle transactions default to 30 s. */
  maxDurationMs?: number;
  /** false: measured but never sent (sampling). */
  sampled?: boolean;
}

export class Span {
  readonly spanId = uuid4().replace(/-/g, "").slice(0, 16);
  readonly op: string;
  readonly description: string | undefined;
  readonly start: number;
  end: number | undefined;
  status: string | undefined;
  data: Record<string, unknown> | undefined;

  constructor(
    private readonly tx: Transaction | null,
    opts: SpanOptions,
    start: number,
  ) {
    this.op = opts.op;
    this.description = opts.description;
    this.data = opts.data;
    this.start = start;
  }

  setStatus(status: string): this {
    this.status = status;
    return this;
  }

  setData(key: string, value: unknown): this {
    (this.data ??= {})[key] = value;
    return this;
  }

  /** Ends the span. Calling it again does nothing. */
  finish(end?: number): void {
    if (this.end !== undefined) return;
    if (!this.tx) {
      this.end = end ?? 0;
      return;
    }
    this.end = end ?? this.tx.now();
    this.tx.childFinished(this);
  }
}

/** A span that records nothing: returned when there's no transaction to join. */
export const noopSpan = (): Span => new Span(null, { op: "noop" }, 0);

/** Spans beyond this are ignored: a polling loop mustn't grow a transaction forever. */
export const MAX_SPANS = 500;
const DEFAULT_IDLE_DEADLINE_MS = 30_000;

export class Transaction {
  readonly name: string;
  readonly op: string;
  readonly start: number;
  sampled: boolean;
  readonly spans: Span[] = [];
  readonly measurements: Record<string, number> = {};
  status = "ok";
  end: number | undefined;

  private open = 0;
  private lastActivity: number;
  private idleTimer: ReturnType<typeof setTimeout> | undefined;
  private deadlineTimer: ReturnType<typeof setTimeout> | undefined;
  private readonly idleTimeoutMs: number | undefined;

  constructor(
    private readonly host: TransactionHost,
    opts: TransactionOptions,
  ) {
    this.name = opts.name;
    this.op = opts.op;
    this.start = opts.start ?? host.now();
    this.sampled = opts.sampled ?? true;
    this.lastActivity = this.start;
    this.idleTimeoutMs = opts.idleTimeoutMs;
    const deadline = opts.maxDurationMs ?? (opts.idleTimeoutMs !== undefined ? DEFAULT_IDLE_DEADLINE_MS : undefined);
    if (deadline !== undefined) {
      this.deadlineTimer = setTimeout(() => {
        this.status = "deadline_exceeded";
        this.finish(this.start + deadline);
      }, Math.max(0, this.start + deadline - host.now()));
    }
    this.maybeIdle();
  }

  now(): number {
    return this.host.now();
  }

  get finished(): boolean {
    return this.end !== undefined;
  }

  startChild(opts: SpanOptions): Span {
    if (this.finished || this.spans.length >= MAX_SPANS) return noopSpan();
    const span = new Span(this, opts, opts.start ?? this.host.now());
    this.spans.push(span);
    this.open++;
    this.cancelIdle();
    return span;
  }

  /**
   * Keeps an idle transaction open until the returned function is called,
   * e.g. until the page's load event or the screen's first frame. Its end
   * then counts as activity.
   */
  hold(): (at?: number) => void {
    if (this.finished) return () => {};
    this.open++;
    this.cancelIdle();
    let released = false;
    return (at?: number) => {
      if (released) return;
      released = true;
      this.activity(at ?? this.host.now());
    };
  }

  setMeasurement(name: string, valueMs: number): void {
    if (Number.isFinite(valueMs)) this.measurements[name] = valueMs;
  }

  /** @internal Called by Span.finish. */
  childFinished(span: Span): void {
    this.activity(span.end ?? this.host.now());
  }

  /** Ends the transaction and hands it to the client to send. Calling it again does nothing. */
  finish(end?: number): void {
    if (this.finished) return;
    this.end = end ?? this.host.now();
    this.cancelIdle();
    clearTimeout(this.deadlineTimer);
    // Spans still open at the end (a request that never answered) end with
    // the transaction, marked so nobody mistakes them for fast requests.
    for (const s of this.spans) {
      if (s.end === undefined) {
        s.end = this.end;
        s.status ??= "cancelled";
      }
    }
    this.host.finishTransaction(this);
  }

  /** Stops the transaction without sending it: what it measured turned out to be meaningless. */
  discard(): void {
    if (this.finished) return;
    this.sampled = false;
    this.finish();
  }

  private activity(at: number): void {
    this.open = Math.max(0, this.open - 1);
    this.lastActivity = Math.max(this.lastActivity, at);
    this.maybeIdle();
  }

  private maybeIdle(): void {
    if (this.idleTimeoutMs === undefined || this.open > 0 || this.finished) return;
    this.cancelIdle();
    this.idleTimer = setTimeout(() => this.finish(this.lastActivity), this.idleTimeoutMs);
  }

  private cancelIdle(): void {
    clearTimeout(this.idleTimer);
    this.idleTimer = undefined;
  }
}

/**
 * Milliseconds since 1970 → RFC 3339 with microseconds. Date's own
 * toISOString stops at milliseconds, and a fast HTTP request on a local
 * network can take less than one.
 */
export function isoTime(ms: number): string {
  const whole = Math.floor(ms);
  const micros = Math.min(999, Math.round((ms - whole) * 1000));
  return new Date(whole).toISOString().slice(0, -1) + String(micros).padStart(3, "0") + "Z";
}

/**
 * A clock that never goes backwards. Date.now() jumps when the device
 * syncs its time, which would make durations negative; performance.now()
 * only moves forward but counts from page or app start. Adding the two at
 * startup gives a monotonic clock in milliseconds since 1970.
 */
export function monotonicClock(): () => number {
  const perf = (globalThis as { performance?: { now?: () => number } }).performance;
  if (typeof perf?.now !== "function") return () => Date.now();
  const offset = Date.now() - perf.now();
  return () => offset + perf.now!();
}
