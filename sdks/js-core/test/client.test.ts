import { afterEach, beforeEach, describe, expect, it, vi } from "vitest";
import { Client, type ClientOptions } from "../src/client";
import type { SendResult, Transport } from "../src/transport";
import type { Envelope } from "../src/types";

const DSN = "https://pk_abc@juju.example.com/1";

/** A transport that records every envelope and answers from a script. */
function fakeTransport(...answers: SendResult[]) {
  const sent: Envelope[] = [];
  const transport: Transport = {
    send: vi.fn(async (env: Envelope): Promise<SendResult> => {
      sent.push(structuredClone(env));
      return answers.shift() ?? { ok: true };
    }),
  };
  return { transport, sent };
}

function client(transport: Transport, extra: Partial<ClientOptions> = {}) {
  // random() = 1 removes jitter, so backoff delays are exact in tests.
  return new Client({ dsn: DSN, transport, random: () => 1, ...extra });
}

// Fake timers let a test jump ahead "5 seconds" instantly.
beforeEach(() => vi.useFakeTimers());
afterEach(() => vi.useRealTimers());

describe("batching", () => {
  it("collects events for 5 s and sends them in one request", async () => {
    const { transport, sent } = fakeTransport();
    const c = client(transport);
    c.captureException(new Error("a"));
    c.captureException(new Error("b"));
    c.captureException(new Error("c"));

    await vi.advanceTimersByTimeAsync(4_999);
    expect(sent).toHaveLength(0);
    await vi.advanceTimersByTimeAsync(1);
    expect(sent).toHaveLength(1);
    expect(sent[0]!.events.map((e) => e.exception.message)).toEqual(["a", "b", "c"]);
    expect(c.pending()).toBe(0);
  });

  it("sends at once when a batch is full, in batches of maxBatchSize", async () => {
    const { transport, sent } = fakeTransport();
    const c = client(transport, { maxBatchSize: 2 });
    for (const m of ["a", "b", "c", "d", "e"]) c.captureException(new Error(m));
    await vi.advanceTimersByTimeAsync(0);
    expect(sent.map((env) => env.events.length)).toEqual([2, 2, 1]);
  });

  it("never runs two sends at once", async () => {
    let release!: () => void;
    const transport: Transport = { send: vi.fn(() => new Promise<SendResult>((r) => (release = () => r({ ok: true })))) };
    const c = client(transport);
    c.captureException(new Error("a"));
    const first = c.flush();
    const second = c.flush();
    expect(first).toBe(second);
    release();
    await first;
    expect(transport.send).toHaveBeenCalledTimes(1);
  });
});

describe("retries", () => {
  it("keeps events and backs off 1 s, 2 s, 4 s on temporary failures", async () => {
    const fail: SendResult = { ok: false, retry: true, status: 503 };
    const { transport, sent } = fakeTransport(fail, fail, fail, { ok: true });
    const c = client(transport);
    c.captureException(new Error("a"));
    await c.flush(); // attempt 1 fails at t=0
    expect(sent).toHaveLength(1);

    await vi.advanceTimersByTimeAsync(999);
    expect(sent).toHaveLength(1);
    await vi.advanceTimersByTimeAsync(1); // t=1s: attempt 2
    expect(sent).toHaveLength(2);
    await vi.advanceTimersByTimeAsync(2_000); // t=3s: attempt 3
    expect(sent).toHaveLength(3);
    await vi.advanceTimersByTimeAsync(4_000); // t=7s: attempt 4 succeeds
    expect(sent).toHaveLength(4);
    expect(c.pending()).toBe(0);
  });

  it("resends the same event_id on every attempt, so the server can dedupe", async () => {
    const { transport, sent } = fakeTransport({ ok: false, retry: true });
    const c = client(transport);
    const id = c.captureException(new Error("a"));
    await c.flush();
    await vi.advanceTimersByTimeAsync(1_000);
    expect(sent.map((env) => env.events[0]!.event_id)).toEqual([id, id]);
  });

  it("waits at least Retry-After on 429", async () => {
    const { transport, sent } = fakeTransport({ ok: false, retry: true, status: 429, retryAfterMs: 10_000 });
    const c = client(transport);
    c.captureException(new Error("a"));
    await c.flush();
    await vi.advanceTimersByTimeAsync(9_999);
    expect(sent).toHaveLength(1);
    await vi.advanceTimersByTimeAsync(1);
    expect(sent).toHaveLength(2);
  });

  it("drops events the server rejects as broken, instead of retrying forever", async () => {
    const { transport, sent } = fakeTransport({ ok: false, retry: false, status: 400 });
    const c = client(transport);
    c.captureException(new Error("a"));
    await c.flush();
    await vi.advanceTimersByTimeAsync(120_000);
    expect(sent).toHaveLength(1);
    expect(c.pending()).toBe(0);
  });

  it("doesn't let new events skip the backoff", async () => {
    const { transport, sent } = fakeTransport({ ok: false, retry: true }, { ok: false, retry: true });
    const c = client(transport, { maxBatchSize: 1 }); // full batches would normally send at once
    c.captureException(new Error("a"));
    await vi.advanceTimersByTimeAsync(0); // attempt 1 fails
    c.captureException(new Error("b"));
    await vi.advanceTimersByTimeAsync(500);
    expect(sent).toHaveLength(1);
  });

  it("retryNow skips the remaining backoff (the device came back online)", async () => {
    const { transport, sent } = fakeTransport({ ok: false, retry: true }, { ok: false, retry: true });
    const c = client(transport);
    c.captureException(new Error("a"));
    await c.flush();
    await vi.advanceTimersByTimeAsync(1_000); // second failure, next try in 2 s
    expect(sent).toHaveLength(2);
    c.retryNow();
    await vi.advanceTimersByTimeAsync(0);
    expect(sent).toHaveLength(3);
  });

  it("caps backoff at 60 s and adds jitter", async () => {
    const c = new Client({ dsn: DSN, transport: { send: async () => ({ ok: false, retry: true }) }, random: () => 0 });
    c.captureException(new Error("a"));
    const spy = vi.spyOn(globalThis, "setTimeout");
    const lastDelay = () => spy.mock.calls[spy.mock.calls.length - 1]![1] as number;

    await c.flush(); // failure 1
    const delays = [lastDelay()];
    for (let i = 0; i < 8; i++) {
      await vi.advanceTimersByTimeAsync(lastDelay()); // the retry fires and fails again
      delays.push(lastDelay());
    }
    // random() = 0 halves each delay: 0.5 s, 1 s, 2 s ... capped at 30 s (half of 60 s).
    expect(delays).toEqual([500, 1_000, 2_000, 4_000, 8_000, 16_000, 30_000, 30_000, 30_000]);
  });
});

describe("events", () => {
  it("attach release, environment, user, tags, breadcrumbs and context", async () => {
    const { transport, sent } = fakeTransport();
    const c = client(transport, { release: "web@1.0.0", environment: "staging", context: () => ({ page: { url: "/cart" } }) });
    c.setUser({ id: "u_1" });
    c.setTag("flavor", "prod");
    c.addBreadcrumb({ type: "navigation", message: "/cart" });
    c.captureException(new Error("boom"), { tags: { step: "checkout" } });
    await c.flush();

    const e = sent[0]!.events[0]!;
    expect(e).toMatchObject({
      type: "error",
      platform: "javascript",
      release: "web@1.0.0",
      environment: "staging",
      user: { id: "u_1" },
      tags: { flavor: "prod", step: "checkout" },
      context: { page: { url: "/cart" } },
      exception: { type: "Error", message: "boom" },
    });
    expect(e.breadcrumbs).toHaveLength(1);
    expect(e.event_id).toMatch(/^[0-9a-f]{8}-[0-9a-f]{4}-4[0-9a-f]{3}-[89ab][0-9a-f]{3}-[0-9a-f]{12}$/);
  });

  it("keeps only the last 100 breadcrumbs", () => {
    const c = client(fakeTransport().transport);
    for (let i = 0; i < 150; i++) c.addBreadcrumb({ type: "x", message: String(i) });
    const crumbs = c.breadcrumbs.snapshot();
    expect(crumbs).toHaveLength(100);
    expect(crumbs[0]!.message).toBe("50");
  });

  it("reports the same Error object only once", () => {
    const c = client(fakeTransport().transport);
    const err = new Error("caught, reported, then rethrown");
    expect(c.captureException(err)).not.toBe("");
    expect(c.captureException(err)).toBe("");
    expect(c.pending()).toBe(1);
  });

  it("beforeSend can change or drop events", () => {
    const c = client(fakeTransport().transport, { beforeSend: (e) => (e.exception.message.includes("ignore") ? null : e) });
    expect(c.captureException(new Error("ignore me"))).toBe("");
    expect(c.captureException(new Error("keep me"))).not.toBe("");
    expect(c.pending()).toBe(1);
  });

  it("drops new events once the offline queue is full", () => {
    const c = client(fakeTransport().transport, { maxQueueSize: 3 });
    for (const m of ["first", "2", "3", "4", "5"]) c.captureException(new Error(m));
    expect(c.pending()).toBe(3);
  });

  it("never throws into the app, even if beforeSend does", () => {
    const c = client(fakeTransport().transport, {
      beforeSend: () => {
        throw new Error("bug in user code");
      },
    });
    expect(() => c.captureException(new Error("a"))).not.toThrow();
  });

  it("survives a transport that throws", async () => {
    const c = client({ send: () => Promise.reject(new Error("boom")) });
    c.captureException(new Error("a"));
    await expect(c.flush()).resolves.toBeUndefined();
    expect(c.pending()).toBe(1); // kept for retry
  });
});

describe("drain (sendBeacon)", () => {
  it("removes events only when the send is accepted", () => {
    const c = client(fakeTransport().transport);
    c.captureException(new Error("a"));
    c.drain(() => false, 60_000);
    expect(c.pending()).toBe(1);
    c.drain(() => true, 60_000);
    expect(c.pending()).toBe(0);
  });

  it("splits the queue into envelopes under the size limit", () => {
    const c = client(fakeTransport().transport);
    // Strings, not Errors: an Error carries a long stack trace, and here we
    // want small events of a predictable size.
    for (let i = 0; i < 6; i++) c.captureException("x".repeat(300));
    const sizes: number[] = [];
    c.drain((env, body) => {
      sizes.push(env.events.length);
      expect(body.length).toBeLessThanOrEqual(1_000);
      expect(JSON.parse(body)).toEqual(env);
      return true;
    }, 1_000);
    expect(sizes.reduce((a, b) => a + b)).toBe(6);
    expect(sizes.length).toBeGreaterThan(1);
  });

  it("keeps an event too big for a beacon for the next normal send", () => {
    const c = client(fakeTransport().transport);
    c.captureException("x".repeat(2_000));
    const send = vi.fn(() => true);
    c.drain(send, 1_000);
    expect(send).not.toHaveBeenCalled();
    expect(c.pending()).toBe(1);
  });

  it("doesn't resend events a beacon took while a fetch was in flight", async () => {
    let release!: (r: SendResult) => void;
    const sends: number[] = [];
    const c = client({
      send: (env) => {
        sends.push(env.events.length);
        return new Promise((r) => (release = r));
      },
    });
    c.captureException(new Error("a"));
    const inFlight = c.flush();
    c.captureException(new Error("b"));
    c.drain(() => true, 60_000); // page hidden: beacon takes a and b
    release({ ok: false, retry: true }); // the fetch for "a" fails afterwards
    await inFlight;
    expect(c.pending()).toBe(0); // nothing left to retry
  });
});
