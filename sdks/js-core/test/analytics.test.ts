import { afterEach, beforeEach, describe, expect, it, vi } from "vitest";
import { Client, type KeyValueStorage } from "../src/client";
import type { SendResult, Transport } from "../src/transport";
import type { AnalyticsEvent, Envelope, IdentifyEvent, OutgoingEvent } from "../src/types";

const DSN = "https://pk_test@juju.example.com/1";

function setup(storage?: KeyValueStorage, extra: Record<string, unknown> = {}) {
  const sent: Envelope[] = [];
  const transport: Transport = { send: vi.fn(async (env: Envelope): Promise<SendResult> => (sent.push(structuredClone(env)), { ok: true })) };
  const c = new Client({ dsn: DSN, transport, identityStorage: storage, flushIntervalMs: 10, context: () => ({ os: { name: "Android" } }), ...extra });
  const events = async () => {
    await vi.runAllTimersAsync();
    return sent.flatMap((e) => e.events) as OutgoingEvent[];
  };
  return { c, events };
}

function memoryStorage(initial: Record<string, string> = {}, delayMs = 0): KeyValueStorage & { data: Record<string, string> } {
  const data = { ...initial };
  const wait = () => new Promise<void>((r) => setTimeout(r, delayMs));
  return {
    data,
    getItem: async (k) => (await wait(), data[k] ?? null),
    setItem: async (k, v) => void (await wait(), (data[k] = v)),
  };
}

beforeEach(() => vi.useFakeTimers());
afterEach(() => vi.useRealTimers());

describe("capture", () => {
  it("sends the event with who did it, its properties and the device context", async () => {
    const { c, events } = setup();
    c.capture("checkout_started", { plan: "pro", items: 3 });
    const [e] = (await events()) as AnalyticsEvent[];
    expect(e).toMatchObject({ type: "event", name: "checkout_started", properties: { plan: "pro", items: 3 }, context: { os: { name: "Android" } } });
    expect(e!.distinct_id).toMatch(/^[0-9a-f-]{36}$/); // an anonymous id
    expect(e!.event_id).toMatch(/^[0-9a-f-]{36}$/);
  });

  it("ignores a missing name and never throws", async () => {
    const { c, events } = setup();
    // @ts-expect-error: wrong on purpose
    c.capture(undefined);
    c.capture("  ");
    // A property that can't be copied must not break the app.
    expect(() => c.capture("ok", { self: null })).not.toThrow();
    expect((await events()).map((e) => (e as AnalyticsEvent).name)).toEqual(["ok"]);
  });

  it("keeps the same anonymous id across launches", async () => {
    const storage = memoryStorage();
    const first = setup(storage);
    first.c.capture("a");
    const [a] = (await first.events()) as AnalyticsEvent[];
    const second = setup(storage);
    second.c.capture("b");
    const [b] = (await second.events()) as AnalyticsEvent[];
    expect(b!.distinct_id).toBe(a!.distinct_id);
  });

  it("uses the stored id for events captured before storage has loaded (React Native)", async () => {
    const storage = memoryStorage({ "juju.anon_id": "anon-stored", "juju.distinct_id": "anon-stored" }, 50);
    const { c, events } = setup(storage);
    c.capture("right_at_launch"); // storage is still loading
    const [e] = (await events()) as AnalyticsEvent[];
    expect(e!.distinct_id).toBe("anon-stored");
  });
});

describe("identify and reset", () => {
  it("links the anonymous history to the user once, then sends as the user", async () => {
    const storage = memoryStorage();
    const { c, events } = setup(storage);
    c.capture("viewed_pricing");
    c.identify("user_42");
    c.identify("user_42"); // logging in twice links once
    c.capture("checkout_started");
    const all = await events();
    const anon = (all[0] as AnalyticsEvent).distinct_id;
    expect(all.map((e) => e.type)).toEqual(["event", "identify", "event"]);
    expect(all[1]).toMatchObject({ type: "identify", distinct_id: "user_42", anon_id: anon });
    expect((all[2] as AnalyticsEvent).distinct_id).toBe("user_42");
    expect(storage.data["juju.distinct_id"]).toBe("user_42"); // still the user after a relaunch
  });

  it("switching accounts doesn't merge two people", async () => {
    const { c, events } = setup();
    c.identify("alice");
    c.identify("bob"); // same device, another account, without reset()
    const ids = (await events()) as IdentifyEvent[];
    expect(ids[0]!.anon_id).toBeTruthy();
    expect(ids[1]).toMatchObject({ distinct_id: "bob" });
    expect(ids[1]!.anon_id).toBeUndefined(); // alice's anonymous history isn't handed to bob
  });

  it("reset gives the next person a new anonymous id", async () => {
    const { c, events } = setup();
    c.capture("before");
    c.identify("alice");
    c.reset();
    c.capture("after");
    const all = (await events()) as AnalyticsEvent[];
    const before = all.find((e) => e.name === "before")!;
    const after = all.find((e) => e.name === "after")!;
    expect(after.distinct_id).not.toBe("alice");
    expect(after.distinct_id).not.toBe(before.distinct_id);
  });

  it("identify also sets the user on error reports", async () => {
    const { c, events } = setup();
    c.identify("user_42");
    c.captureException(new Error("boom"));
    const err = (await events()).find((e) => e.type === "error")!;
    expect(err).toMatchObject({ user: { id: "user_42" } });
  });
});

describe("queue limits", () => {
  it("a flood of analytics events never pushes out an error", async () => {
    const { c } = setup(undefined, { maxQueueSize: 2, maxAnalyticsQueueSize: 3, transport: { send: () => new Promise(() => {}) } });
    for (let i = 0; i < 10; i++) c.capture("click");
    await vi.advanceTimersByTimeAsync(0);
    expect(c.captureException(new Error("crash"))).not.toBe("");
    expect(c.pending()).toBe(4); // 3 analytics + the error
  });
});
