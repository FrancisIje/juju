import { afterEach, beforeEach, describe, expect, it, vi } from "vitest";
import { Client, SESSION_RESUME_MS, type ClientOptions } from "../src/client";
import { isoTime } from "../src/tracing";
import type { SendResult, Transport } from "../src/transport";
import type { Envelope, OutgoingEvent, SessionEvent, TransactionEvent } from "../src/types";

const DSN = "https://pk_abc@juju.example.com/1";

function setup(extra: Partial<ClientOptions> = {}, answer: SendResult = { ok: true }) {
  const sent: Envelope[] = [];
  const transport: Transport = { send: vi.fn(async (env: Envelope) => (sent.push(structuredClone(env)), answer)) };
  // Date.now is faked below, so this clock moves only when the test says so.
  const c = new Client({ dsn: DSN, transport, random: () => 1, release: "shop@1.0", now: () => Date.now(), ...extra });
  const events = () => sent.flatMap((e) => e.events);
  const sessions = () => events().filter((e): e is SessionEvent => e.type === "session");
  const transactions = () => events().filter((e): e is TransactionEvent => e.type === "transaction");
  return { c, sent, events, sessions, transactions };
}

beforeEach(() => {
  vi.useFakeTimers();
  vi.setSystemTime(new Date("2026-10-20T10:00:00Z"));
});
afterEach(() => vi.useRealTimers());

describe("sessions", () => {
  it("sends a start and an end with the same sid and a higher seq", async () => {
    const { c, sessions } = setup();
    c.startSession();
    await c.flush();
    await vi.advanceTimersByTimeAsync(90_000);
    c.endSession();
    await c.flush();
    const [start, end] = sessions();
    expect(start?.session).toMatchObject({ seq: 0, status: "ok", errors: 0, started: "2026-10-20T10:00:00.000Z" });
    expect(start?.release).toBe("shop@1.0");
    expect(end?.session).toMatchObject({ sid: start?.session.sid, seq: 1, status: "exited", duration_ms: 90_000 });
  });

  it("counts errors, and a crash ends the session as crashed", async () => {
    const { c, sessions } = setup();
    c.startSession();
    c.captureException(new Error("handled"));
    c.captureException(new Error("fatal"), { crashed: true });
    await c.flush();
    // The updates were still waiting, so only the latest went out.
    expect(sessions()).toHaveLength(1);
    expect(sessions()[0]?.session).toMatchObject({ status: "crashed", errors: 2, seq: 2 });
    expect(c.sessionId()).toBeUndefined();
  });

  // The app reports an error, then rethrows it, and it crashes the app.
  // Dedupe stops a second error event, but the crash must still count.
  it("marks the crash even when the error was already reported", async () => {
    const { c, sessions } = setup();
    c.startSession();
    const err = new Error("rethrown");
    c.captureException(err);
    expect(c.captureException(err, { crashed: true })).toBe("");
    await c.flush();
    expect(sessions()[sessions().length - 1]?.session).toMatchObject({ status: "crashed", errors: 1 });
  });

  it("continues the session after a short trip to the background", async () => {
    const { c, sessions } = setup();
    c.startSession();
    await c.flush();
    c.pauseSession();
    await c.flush();
    await vi.advanceTimersByTimeAsync(SESSION_RESUME_MS - 1);
    c.resumeSession();
    await c.flush();
    const [start, paused, resumed] = sessions();
    expect(paused?.session).toMatchObject({ sid: start?.session.sid, status: "exited", seq: 1 });
    // Same session, back to running: the server keeps the highest seq.
    expect(resumed?.session).toMatchObject({ sid: start?.session.sid, status: "ok", seq: 2 });
    expect(resumed?.session.duration_ms).toBeUndefined();
  });

  it("starts a new session after a long time in the background", async () => {
    const { c, sessions } = setup();
    c.startSession();
    c.pauseSession();
    vi.advanceTimersByTime(SESSION_RESUME_MS + 1);
    c.resumeSession();
    await c.flush();
    const sids = new Set(sessions().map((s) => s.session.sid));
    expect(sids.size).toBe(2);
    expect(sessions()[sessions().length - 1]?.session).toMatchObject({ seq: 0, status: "ok" });
  });

  it("keeps one update per session while offline", async () => {
    const { c } = setup({}, { ok: false, retry: true });
    c.startSession();
    for (let i = 0; i < 50; i++) {
      c.pauseSession();
      c.resumeSession();
    }
    expect(c.pending()).toBe(1);
  });

  it("does nothing without a session", async () => {
    const { c, events } = setup();
    c.captureException(new Error("x"));
    c.pauseSession();
    c.endSession();
    await c.flush();
    expect(events().map((e) => e.type)).toEqual(["error"]);
  });
});

describe("transactions", () => {
  it("sends a transaction with its spans and microsecond times", async () => {
    const { c, transactions } = setup();
    const tx = c.startTransaction({ name: "/checkout", op: "pageload" });
    vi.advanceTimersByTime(250);
    const span = tx.startChild({ op: "http.client", description: "GET /api/cart" });
    vi.advanceTimersByTime(500);
    span.setData("status_code", 200).finish();
    vi.advanceTimersByTime(450);
    tx.finish();
    await c.flush();
    const [t] = transactions();
    expect(t).toMatchObject({
      release: "shop@1.0",
      timestamp: "2026-10-20T10:00:01.200000Z",
      transaction: { name: "/checkout", op: "pageload", status: "ok", start_timestamp: "2026-10-20T10:00:00.000000Z" },
    });
    expect(t?.transaction.spans).toEqual([
      expect.objectContaining({ op: "http.client", start_timestamp: "2026-10-20T10:00:00.250000Z", timestamp: "2026-10-20T10:00:00.750000Z", data: { status_code: 200 } }),
    ]);
  });

  // An idle transaction ends when the last thing in it ended, not when its
  // timer noticed: the timer's wait isn't time the user spent waiting.
  it("an idle transaction ends at its last activity", async () => {
    const { c, transactions } = setup();
    const tx = c.startTransaction({ name: "Cart", op: "screen.load", idleTimeoutMs: 1000, active: true });
    const release = tx.hold(); // e.g. until the first frame
    vi.advanceTimersByTime(50);
    const req = c.startSpan({ op: "http.client" });
    vi.advanceTimersByTime(50);
    release();
    vi.advanceTimersByTime(200);
    req.finish(); // at 300 ms
    vi.advanceTimersByTime(999);
    expect(tx.finished).toBe(false);
    vi.advanceTimersByTime(1);
    expect(tx.finished).toBe(true);
    await c.flush();
    expect(transactions()[0]?.timestamp).toBe("2026-10-20T10:00:00.300000Z");
    expect(c.activeTransaction()).toBeUndefined();
  });

  it("a new span restarts the idle wait", () => {
    const { c } = setup();
    const tx = c.startTransaction({ name: "x", op: "navigation", idleTimeoutMs: 1000, active: true });
    vi.advanceTimersByTime(900);
    c.startSpan({ op: "http.client" }).finish();
    vi.advanceTimersByTime(900);
    expect(tx.finished).toBe(false);
  });

  it("gives up after the deadline and marks what was still open", async () => {
    const { c, transactions } = setup();
    const tx = c.startTransaction({ name: "Feed", op: "screen.load", idleTimeoutMs: 1000, active: true });
    tx.hold(); // never released
    c.startSpan({ op: "http.client", description: "GET /hang" }); // never answers
    vi.advanceTimersByTime(30_000);
    await c.flush();
    const t = transactions()[0]!;
    expect(t.transaction.status).toBe("deadline_exceeded");
    expect(t.timestamp).toBe("2026-10-20T10:00:30.000000Z");
    expect(t.transaction.spans?.[0]).toMatchObject({ status: "cancelled", timestamp: t.timestamp });
  });

  it("spans with no active transaction record nothing", async () => {
    const { c, events } = setup();
    c.startSpan({ op: "http.client" }).finish();
    await c.flush();
    expect(events()).toEqual([]);
  });

  it("starting an active transaction finishes the previous one", async () => {
    const { c, transactions } = setup();
    const first = c.startTransaction({ name: "/a", op: "navigation", idleTimeoutMs: 1000, active: true });
    first.hold();
    vi.advanceTimersByTime(100);
    c.startTransaction({ name: "/b", op: "navigation", idleTimeoutMs: 1000, active: true });
    expect(first.finished).toBe(true);
    await c.flush();
    expect(transactions().map((t) => t.transaction.name)).toEqual(["/a"]);
  });

  it("samples: a rate of 0.25 sends about a quarter", async () => {
    let r = 0;
    const rolls = [0.1, 0.3, 0.6, 0.9]; // only 0.1 is under 0.25
    const { c, transactions } = setup({ tracesSampleRate: 0.25, random: () => rolls[r++ % 4]! });
    for (let i = 0; i < 4; i++) c.startTransaction({ name: `t${i}`, op: "custom" }).finish();
    await c.flush();
    expect(transactions().map((t) => t.transaction.name)).toEqual(["t0"]);
  });

  it("formats sub-millisecond times", () => {
    expect(isoTime(Date.UTC(2026, 9, 20) + 0.5)).toBe("2026-10-20T00:00:00.000500Z");
    expect(isoTime(Date.UTC(2026, 9, 20) + 1.9996)).toBe("2026-10-20T00:00:00.001999Z"); // never rounds up to 1000 µs
  });
});

// Every type travels the same queue, so the offline guarantees apply.
it("sessions and transactions are kept and retried like errors", async () => {
  const { c, sent } = setup({}, { ok: false, retry: true });
  c.startSession();
  c.startTransaction({ name: "t", op: "custom" }).finish();
  await c.flush();
  expect(c.pending()).toBe(2);
  expect(sent[0]?.events.map((e: OutgoingEvent) => e.type)).toEqual(["session", "transaction"]);
});
