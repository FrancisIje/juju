import type { AnalyticsEvent, Envelope, JujuEvent, SendResult, Transport } from "@juju/core";
import { Client } from "@juju/core";
import { afterEach, beforeEach, describe, expect, it, vi } from "vitest";
import { deviceContext, isInApp } from "../src/context";
import { installGlobalHandler, installRejectionTracking, type ErrorUtilsLike, type GlobalHandler } from "../src/errors";
import * as juju from "../src/index";
import { trackNavigation } from "../src/lifecycle";
import { asyncStorageQueue, STORAGE_KEY, type AsyncStorageLike } from "../src/storage";
import { emitAppState, Platform } from "./react-native-fake";

const DSN = "https://pk_abc@juju.example.com/1";

/** React Native's ErrorUtils, with a "default handler" that records calls the way RN would crash. */
function fakeErrorUtils() {
  const defaultHandler = vi.fn<GlobalHandler>();
  let current: GlobalHandler = defaultHandler;
  const utils: ErrorUtilsLike = {
    getGlobalHandler: () => current,
    setGlobalHandler: (h) => void (current = h),
  };
  return { utils, defaultHandler, crash: (err: unknown, fatal = true) => current(err, fatal) };
}

function fakeAsyncStorage(initial: Record<string, string> = {}) {
  const data = new Map(Object.entries(initial));
  const storage: AsyncStorageLike = {
    getItem: vi.fn(async (k: string) => data.get(k) ?? null),
    setItem: vi.fn(async (k: string, v: string) => void data.set(k, v)),
    removeItem: vi.fn(async (k: string) => void data.delete(k)),
  };
  return { storage, data };
}

let sent: Envelope[];
let transport: Transport;
beforeEach(() => {
  sent = [];
  transport = { send: vi.fn(async (env: Envelope): Promise<SendResult> => (sent.push(structuredClone(env)), { ok: true })) };
});
afterEach(() => {
  juju.close();
  vi.useRealTimers();
});

describe("the global error handler", () => {
  it("reports non-fatal errors and passes them straight on", () => {
    const { utils, defaultHandler, crash } = fakeErrorUtils();
    const c = new Client({ dsn: DSN, transport });
    installGlobalHandler(c, utils);
    const err = new Error("non-fatal");
    crash(err, false);
    expect(c.pending()).toBe(1);
    expect(defaultHandler).toHaveBeenCalledWith(err, false);
  });

  it("on a fatal crash, lets React Native end the app only after the report is saved", async () => {
    vi.useFakeTimers();
    let finishSave!: () => void;
    const persistence = { load: async (): Promise<JujuEvent[]> => [], save: vi.fn((_events: JujuEvent[]) => new Promise<void>((r) => (finishSave = r))) };
    const c = new Client({ dsn: DSN, transport: { send: () => new Promise(() => {}) }, persistence }); // network hangs
    await c.ready;
    const { utils, defaultHandler, crash } = fakeErrorUtils();
    installGlobalHandler(c, utils);

    crash(new TypeError("fatal"));
    await vi.advanceTimersByTimeAsync(100);
    expect(defaultHandler).not.toHaveBeenCalled(); // still saving: the app must stay alive

    finishSave();
    await vi.advanceTimersByTimeAsync(2_000); // the hanging network can't hold it longer than the timeout
    expect(defaultHandler).toHaveBeenCalledTimes(1);
    const saved = persistence.save.mock.calls[0]![0];
    expect(saved[0]).toMatchObject({ exception: { type: "TypeError", message: "fatal" }, tags: { fatal: "true" } });
  });

  it("never holds a crash longer than the timeout, even if storage hangs", async () => {
    vi.useFakeTimers();
    const persistence = { load: async () => [], save: () => new Promise<void>(() => {}) };
    const c = new Client({ dsn: DSN, transport: { send: () => new Promise(() => {}) }, persistence });
    const { utils, defaultHandler, crash } = fakeErrorUtils();
    installGlobalHandler(c, utils, 500);
    crash(new Error("fatal"));
    await vi.advanceTimersByTimeAsync(499);
    expect(defaultHandler).not.toHaveBeenCalled();
    await vi.advanceTimersByTimeAsync(1);
    expect(defaultHandler).toHaveBeenCalledTimes(1);
  });

  it("calls React Native's handler exactly once per crash", async () => {
    const { utils, defaultHandler, crash } = fakeErrorUtils();
    const c = new Client({ dsn: DSN, transport });
    installGlobalHandler(c, utils, 50);
    crash(new Error("fatal"));
    await new Promise((r) => setTimeout(r, 100)); // both the save and the timeout have finished
    expect(defaultHandler).toHaveBeenCalledTimes(1);
  });

  it("puts the previous handler back on close", () => {
    const { utils, defaultHandler } = fakeErrorUtils();
    const uninstall = installGlobalHandler(new Client({ dsn: DSN, transport }), utils);
    expect(utils.getGlobalHandler()).not.toBe(defaultHandler);
    uninstall();
    expect(utils.getGlobalHandler()).toBe(defaultHandler);
  });
});

describe("unhandled promise rejections", () => {
  it("are reported through Hermes's tracker, and the dev warning is kept", () => {
    let options: { onUnhandled: (id: number, r: unknown) => void } | undefined;
    const hermes = { enablePromiseRejectionTracker: vi.fn((o) => void (options = o)) };
    const warn = vi.fn();
    const c = new Client({ dsn: DSN, transport });
    installRejectionTracking(c, hermes, true, warn);

    expect(hermes.enablePromiseRejectionTracker).toHaveBeenCalledWith(expect.objectContaining({ allRejections: true }));
    options!.onUnhandled(7, new Error("Checkout API failed: 503"));
    expect(c.pending()).toBe(1);
    expect(warn).toHaveBeenCalledWith("Possible unhandled promise rejection (id: 7):", expect.any(Error));
  });

  it("does nothing without Hermes", () => {
    expect(() => installRejectionTracking(new Client({ dsn: DSN, transport }), undefined)).not.toThrow();
  });
});

describe("storage", () => {
  it("round-trips the queue through AsyncStorage and clears it when empty", async () => {
    const { storage, data } = fakeAsyncStorage();
    const q = asyncStorageQueue(storage);
    const e = { event_id: "x", exception: { type: "E", message: "m" } } as JujuEvent;
    await q.save([e]);
    expect(await q.load()).toEqual([e]);
    await q.save([]);
    expect(data.has(STORAGE_KEY)).toBe(false);
  });

  it("treats a half-written value as empty instead of failing", async () => {
    const { storage } = fakeAsyncStorage({ [STORAGE_KEY]: '[{"event_id":"x", "exc' });
    expect(await asyncStorageQueue(storage).load()).toEqual([]);
  });
});

describe("init", () => {
  it("sends events a previous launch saved before it crashed", async () => {
    const saved = [{ event_id: "33333333-3333-4333-8333-333333333333", type: "error", timestamp: "", platform: "javascript", exception: { type: "TypeError", message: "crashed last time" } }];
    const { storage } = fakeAsyncStorage({ [STORAGE_KEY]: JSON.stringify(saved) });
    juju.init({ dsn: DSN, transport, storage });
    await juju.ready();
    await juju.flush();
    expect(sent.flatMap((env) => env.events.map((e) => e.event_id))).toEqual(["33333333-3333-4333-8333-333333333333"]);
  });

  it("warns when no storage is passed, because crashes could be lost", () => {
    const warn = vi.spyOn(console, "warn").mockImplementation(() => {});
    juju.init({ dsn: DSN, transport });
    expect(warn).toHaveBeenCalledWith(expect.stringContaining("without storage"));
  });

  it("names the SDK and attaches device context", async () => {
    juju.init({ dsn: DSN, transport, storage: fakeAsyncStorage().storage, release: "shop@1.4.0+12" });
    juju.captureException(new Error("x"));
    await juju.flush();
    expect(sent[0]!.sdk.name).toBe("juju.react-native");
    expect(sent[0]!.events[0]).toMatchObject({ release: "shop@1.4.0+12", environment: "production", context: { os: { name: "ios" } } });
  });

  it("saves and sends when the app goes to the background, and records it", async () => {
    const { storage, data } = fakeAsyncStorage();
    juju.init({ dsn: DSN, transport: { send: () => new Promise(() => {}) }, storage, flushIntervalMs: 60_000 });
    juju.captureException(new Error("before backgrounding"));
    emitAppState("background");
    await new Promise((r) => setTimeout(r, 10));
    expect(JSON.parse(data.get(STORAGE_KEY)!)[0].exception.message).toBe("before backgrounding");
    juju.captureException(new Error("after"));
    await new Promise((r) => setTimeout(r, 10));
    const events = JSON.parse(data.get(STORAGE_KEY)!) as JujuEvent[];
    expect(events[1]!.breadcrumbs).toContainEqual(expect.objectContaining({ type: "app", message: "active → background" }));
    emitAppState("active");
  });

  it("does nothing, and doesn't throw, before init", () => {
    expect(juju.captureException(new Error("early"))).toBe("");
    expect(() => juju.trackNavigation({ addListener: () => () => {}, getCurrentRoute: () => undefined })).not.toThrow();
  });
});

describe("device context", () => {
  it("describes iOS", () => {
    expect(deviceContext()).toEqual({
      os: { name: "ios", version: "18.2" },
      device: { idiom: "phone", is_tablet: false },
      screen: { width: 402, height: 874, scale: 3 },
      runtime: { name: "react-native", version: "0.87.1", engine: "jsc", dev: false },
    });
  });

  it("describes Android without device identifiers", () => {
    const saved = { ...Platform };
    Object.assign(Platform, {
      OS: "android",
      Version: 35,
      constants: { Release: "15", Model: "Pixel 10 Pro", Manufacturer: "Google", Brand: "google", Serial: "unknown", Fingerprint: "google/x/y", reactNativeVersion: { major: 0, minor: 87, patch: 1 } },
    });
    const ctx = deviceContext();
    Object.assign(Platform, saved);
    expect(ctx).toMatchObject({ os: { name: "android", version: "15", api_level: 35 }, device: { model: "Pixel 10 Pro", manufacturer: "Google", brand: "google" } });
    expect(JSON.stringify(ctx)).not.toMatch(/Serial|Fingerprint|google\/x/);
  });
});

describe("navigation breadcrumbs", () => {
  it("records screen changes from a React Navigation ref", () => {
    let route = "Home";
    let onState = () => {};
    const ref = { addListener: (_e: "state", cb: () => void) => ((onState = cb), () => {}), getCurrentRoute: () => ({ name: route }) };
    const c = new Client({ dsn: DSN, transport });
    trackNavigation(c, ref);
    route = "Cart";
    onState();
    onState(); // same screen again: not recorded twice
    expect(c.breadcrumbs.snapshot()).toEqual([expect.objectContaining({ type: "navigation", message: "Home → Cart" })]);
  });
});

describe("isInApp", () => {
  it.each([
    ["main.jsbundle", true],
    ["/var/containers/Bundle/Application/X/App.app/main.jsbundle", true],
    ["index.android.bundle", true],
    ["InternalBytecode.js", false],
    ["native", false],
    ["[native code]", false],
    ["http://localhost:8081/node_modules/react-native/Libraries/x.js", false],
  ])("%s → %s", (file, want) => {
    expect(isInApp(file)).toBe(want);
  });
});

describe("analytics", () => {
  it("captures events with device context, and keeps one id per install in AsyncStorage", async () => {
    const { storage, data } = fakeAsyncStorage();
    juju.init({ dsn: DSN, transport, storage });
    juju.capture("checkout_started", { plan: "pro" });
    await juju.flush();
    await new Promise((r) => setTimeout(r, 0));
    await juju.flush();
    const e = sent.flatMap((env) => env.events).find((x) => x.type === "event") as AnalyticsEvent;
    expect(e).toMatchObject({ name: "checkout_started", properties: { plan: "pro" }, context: { os: { name: Platform.OS } } });
    expect(data.get("juju.anon_id")).toBe(e.distinct_id);

    // A relaunch (new init, same storage) is the same person.
    juju.close();
    sent = [];
    juju.init({ dsn: DSN, transport, storage });
    juju.identify("user_42");
    await new Promise((r) => setTimeout(r, 0));
    await juju.flush();
    expect(sent.flatMap((env) => env.events)).toContainEqual(expect.objectContaining({ type: "identify", distinct_id: "user_42", anon_id: e.distinct_id }));
  });
});
