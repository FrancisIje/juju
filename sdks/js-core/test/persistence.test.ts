import { afterEach, beforeEach, describe, expect, it, vi } from "vitest";
import { Client, type QueueStorage } from "../src/client";
import type { SendResult, Transport } from "../src/transport";
import type { Envelope, JujuEvent, OutgoingEvent } from "../src/types";

const DSN = "https://pk_abc@juju.example.com/1";

/** In-memory stand-in for AsyncStorage, recording every save. */
function memoryStorage(initial: JujuEvent[] = []) {
  let stored = structuredClone(initial);
  const saves: string[][] = [];
  const storage: QueueStorage = {
    load: vi.fn(async () => structuredClone(stored)),
    save: vi.fn(async (events: JujuEvent[]) => {
      stored = structuredClone(events);
      saves.push(events.map((e) => e.exception.message));
    }),
  };
  return { storage, saves, stored: () => stored };
}

function failingTransport(): Transport {
  return { send: async (): Promise<SendResult> => ({ ok: false, retry: true }) };
}

function okTransport(sent: Envelope[]): Transport {
  return { send: async (env): Promise<SendResult> => (sent.push(structuredClone(env)), { ok: true }) };
}

beforeEach(() => vi.useFakeTimers());
afterEach(() => vi.useRealTimers());

describe("saving the queue", () => {
  it("saves each captured event, so a crash can't lose it", async () => {
    const { storage, stored } = memoryStorage();
    const c = new Client({ dsn: DSN, transport: failingTransport(), persistence: storage });
    c.captureException(new Error("before the crash"));
    await c.persistNow();
    expect(stored().map((e) => e.exception.message)).toEqual(["before the crash"]);
  });

  it("removes events from storage once the server has them", async () => {
    const sent: Envelope[] = [];
    const { storage, stored } = memoryStorage();
    const c = new Client({ dsn: DSN, transport: okTransport(sent), persistence: storage });
    c.captureException(new Error("a"));
    await c.flush();
    await c.persistNow();
    expect(sent).toHaveLength(1);
    expect(stored()).toEqual([]);
  });

  it("coalesces a burst of errors into few writes, always ending with the latest queue", async () => {
    const { storage, saves } = memoryStorage();
    const c = new Client({ dsn: DSN, transport: failingTransport(), persistence: storage });
    for (let i = 0; i < 50; i++) c.captureException(new Error(String(i)));
    await c.persistNow();
    expect(saves.length).toBeLessThan(5);
    expect(saves[saves.length - 1]).toHaveLength(50);
  });

  it("keeps working if storage fails", async () => {
    const storage: QueueStorage = { load: async () => [], save: async () => Promise.reject(new Error("disk full")) };
    const sent: Envelope[] = [];
    const c = new Client({ dsn: DSN, transport: okTransport(sent), persistence: storage });
    expect(c.captureException(new Error("a"))).not.toBe("");
    await c.flush();
    expect(sent).toHaveLength(1);
  });
});

describe("the next launch", () => {
  it("sends events saved by the previous launch", async () => {
    // Launch 1: capture, the network is down, the app crashes.
    const disk = memoryStorage();
    const first = new Client({ dsn: DSN, transport: failingTransport(), persistence: disk.storage });
    const id = first.captureException(new Error("crashed offline"));
    await first.persistNow();

    // Launch 2: a fresh client reads the same storage.
    const sent: Envelope[] = [];
    const second = new Client({ dsn: DSN, transport: okTransport(sent), persistence: memoryStorage(disk.stored()).storage });
    await second.ready;
    await vi.advanceTimersByTimeAsync(0);
    expect(sent.flatMap((env) => env.events.map((e) => e.event_id))).toEqual([id]); // same id, so the server dedupes any copy
  });

  it("doesn't wipe saved events when an error happens before loading finishes", async () => {
    let finishLoad!: (events: JujuEvent[]) => void;
    const old = { event_id: "11111111-1111-4111-8111-111111111111", type: "error", timestamp: "", platform: "javascript", exception: { type: "Error", message: "from last launch" } } as JujuEvent;
    let stored: OutgoingEvent[] = [old];
    const storage: QueueStorage = {
      load: () => new Promise((r) => (finishLoad = r)), // slow disk
      save: async (events) => void (stored = events),
    };
    const c = new Client({ dsn: DSN, transport: failingTransport(), persistence: storage });
    c.captureException(new Error("right at launch")); // before load() returns
    await Promise.resolve();
    expect(stored).toEqual([old]); // not overwritten yet: the save waits for the load
    finishLoad([old]);
    await c.persistNow();
    expect(stored.map((e) => (e as JujuEvent).exception.message)).toEqual(["from last launch", "right at launch"]);
  });

  it("ignores saved events already in the queue, and garbage in storage", async () => {
    const e = { event_id: "22222222-2222-4222-8222-222222222222", type: "error", timestamp: "", platform: "javascript", exception: { type: "Error", message: "x" } } as JujuEvent;
    const storage: QueueStorage = { load: async () => [e, e, null, "junk", { no: "id" }] as never, save: async () => {} };
    const c = new Client({ dsn: DSN, transport: failingTransport(), persistence: storage });
    await c.ready;
    expect(c.pending()).toBe(2); // the duplicate copy came from storage itself; both are the same id and the server dedupes
  });

  it("keeps working if loading fails", async () => {
    const storage: QueueStorage = { load: async () => Promise.reject(new Error("corrupt")), save: async () => {} };
    const c = new Client({ dsn: DSN, transport: failingTransport(), persistence: storage });
    await c.ready;
    expect(c.captureException(new Error("a"))).not.toBe("");
  });
});
