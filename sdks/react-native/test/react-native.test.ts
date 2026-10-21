import type { AnalyticsEvent, Envelope, JujuEvent, OutgoingEvent, SendResult, SessionEvent, TransactionEvent, Transport } from "@juju/core";
import { Client } from "@juju/core";
import { afterEach, beforeEach, describe, expect, it, vi } from "vitest";
import { deviceContext, isInApp } from "../src/context";
import { installGlobalHandler, installRejectionTracking, type ErrorUtilsLike, type GlobalHandler } from "../src/errors";
import * as juju from "../src/index";
import { trackNavigation } from "../src/lifecycle";
import { APP_START_DEADLINE_MS, startAppStart } from "../src/performance";
import { asyncStorageQueue, STORAGE_KEY, type AsyncStorageLike } from "../src/storage";
import { emitAppState, Platform } from "./react-native-fake";

const DSN = "https://pk_abc@juju.example.com/1";

/** The error events among events of every type (sessions share the queue). */
const errors = (events: OutgoingEvent[]): JujuEvent[] => events.filter((e): e is JujuEvent => e.type === "error");

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
    expect(errors(sent.flatMap((env) => env.events)).map((e) => e.event_id)).toEqual(["33333333-3333-4333-8333-333333333333"]);
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
    expect(errors(sent[0]!.events)[0]).toMatchObject({ release: "shop@1.4.0+12", environment: "production", context: { os: { name: "ios" } } });
  });

  it("saves and sends when the app goes to the background, and records it", async () => {
    const { storage, data } = fakeAsyncStorage();
    juju.init({ dsn: DSN, transport: { send: () => new Promise(() => {}) }, storage, flushIntervalMs: 60_000 });
    juju.captureException(new Error("before backgrounding"));
    emitAppState("background");
    await new Promise((r) => setTimeout(r, 10));
    expect(errors(JSON.parse(data.get(STORAGE_KEY)!))[0]!.exception.message).toBe("before backgrounding");
    juju.captureException(new Error("after"));
    await new Promise((r) => setTimeout(r, 10));
    const events = errors(JSON.parse(data.get(STORAGE_KEY)!));
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

describe("sessions", () => {
  const sessionsIn = (events: OutgoingEvent[]) => events.filter((e): e is SessionEvent => e.type === "session");

  // The roadmap's "done when": a forced crash must lower the crash-free
  // rate. On a real phone the app dies right after, so the crashed update
  // has to be on disk before React Native's handler runs.
  it("a fatal crash saves the session as crashed before the app dies", async () => {
    vi.useFakeTimers();
    const { storage, data } = fakeAsyncStorage();
    const { utils, defaultHandler, crash } = fakeErrorUtils();
    (globalThis as { ErrorUtils?: ErrorUtilsLike }).ErrorUtils = utils;
    juju.init({ dsn: DSN, transport: { send: () => new Promise(() => {}) }, storage }); // offline
    await juju.ready();
    crash(new TypeError("fatal"));
    await vi.advanceTimersByTimeAsync(2_000);
    expect(defaultHandler).toHaveBeenCalledTimes(1);
    const saved = JSON.parse(data.get(STORAGE_KEY)!) as OutgoingEvent[];
    expect(sessionsIn(saved)).toEqual([expect.objectContaining({ session: expect.objectContaining({ status: "crashed", errors: 1 }) })]);
    expect(errors(saved)).toHaveLength(1);
    delete (globalThis as { ErrorUtils?: ErrorUtilsLike }).ErrorUtils;
  });

  it("a non-fatal error is counted but doesn't crash the session", async () => {
    const { utils, crash } = fakeErrorUtils();
    (globalThis as { ErrorUtils?: ErrorUtilsLike }).ErrorUtils = utils;
    juju.init({ dsn: DSN, transport, storage: fakeAsyncStorage().storage });
    crash(new Error("non-fatal"), false);
    await juju.flush();
    expect(sessionsIn(sent.flatMap((e) => e.events))[0]?.session).toMatchObject({ status: "ok", errors: 1 });
    delete (globalThis as { ErrorUtils?: ErrorUtilsLike }).ErrorUtils;
  });

  it("follows the app into the background and back", async () => {
    juju.init({ dsn: DSN, transport, storage: fakeAsyncStorage().storage });
    await juju.flush();
    emitAppState("background");
    await juju.flush();
    emitAppState("active");
    await juju.flush();
    const s = sessionsIn(sent.flatMap((e) => e.events)).map((e) => e.session);
    expect(s.map((x) => `${x.seq}:${x.status}`)).toEqual(["0:ok", "1:exited", "2:ok"]);
    expect(new Set(s.map((x) => x.sid)).size).toBe(1); // back within 30 s: the same session
  });

  it("can be turned off", async () => {
    juju.init({ dsn: DSN, transport, storage: fakeAsyncStorage().storage, sessions: false, tracing: false });
    emitAppState("background");
    emitAppState("active");
    await juju.flush();
    expect(sent).toEqual([]);
  });
});

describe("app start and screen loads", () => {
  const transactionsIn = (events: OutgoingEvent[]) => events.filter((e): e is TransactionEvent => e.type === "transaction");

  it("measures from the native start that React Native reports", async () => {
    vi.useFakeTimers();
    const c = new Client({ dsn: DSN, transport, now: () => Date.now() });
    // The native app started at 0, the JS runtime at 120 ms, the bundle at
    // 400 ms, and juju.init runs at 900 ms (all from timeOrigin).
    const perf = { now: () => 900, rnStartupTiming: { startTime: 0, initializeRuntimeStart: 120, executeJavaScriptBundleEntryPointStart: 400 } };
    const initAt = Date.now();
    const { done } = startAppStart(c, perf);
    vi.advanceTimersByTime(300); // the first screen takes 300 ms more
    done();
    await c.flush();
    const tx = transactionsIn(sent.flatMap((e) => e.events))[0]!;
    expect(tx.transaction).toMatchObject({ name: "app.start", op: "app.start", measurements: { from_native_start: 1 } });
    expect(Date.parse(tx.timestamp) - Date.parse(tx.transaction.start_timestamp)).toBe(1200);
    expect(Date.parse(tx.transaction.start_timestamp)).toBe(initAt - 900);
    expect(tx.transaction.spans?.map((s) => s.description)).toEqual([
      "Native app start",
      "Start the JavaScript runtime",
      "Run the JavaScript bundle until juju.init",
      "First screen",
    ]);
  });

  // The values an iOS simulator reported (React Native 0.87, release build):
  // startTime is the runtime's start, not the native app's, because iOS
  // doesn't record one. It must not be passed off as a native start.
  it("doesn't call the runtime start a native start (iOS)", async () => {
    vi.useFakeTimers();
    const c = new Client({ dsn: DSN, transport, now: () => Date.now() });
    const perf = {
      now: () => 1209925842,
      rnStartupTiming: { startTime: 1209925496.3882918, initializeRuntimeStart: 1209925496.388167, executeJavaScriptBundleEntryPointStart: 1209925496.3858752 },
    };
    const initAt = Date.now();
    const { done } = startAppStart(c, perf);
    done();
    await c.flush();
    const tx = transactionsIn(sent.flatMap((e) => e.events))[0]!;
    expect(tx.transaction.measurements).toEqual({ from_runtime_start: 1 });
    expect(tx.transaction.spans?.map((s) => s.description)).not.toContain("Native app start");
    // From the earliest value React Native gave, about 346 ms before init.
    expect(initAt - Date.parse(tx.transaction.start_timestamp)).toBe(346);
  });

  it("falls back to juju.init when React Native has no startup timing", async () => {
    vi.useFakeTimers();
    const c = new Client({ dsn: DSN, transport, now: () => Date.now() });
    const { done } = startAppStart(c, { now: () => 900 });
    vi.advanceTimersByTime(300);
    done();
    await c.flush();
    const tx = transactionsIn(sent.flatMap((e) => e.events))[0]!;
    expect(tx.transaction.measurements).toEqual({ from_init: 1 });
    expect(Date.parse(tx.timestamp) - Date.parse(tx.transaction.start_timestamp)).toBe(300);
  });

  // An app start nobody ended would otherwise run until some unrelated
  // moment and pollute the numbers.
  it("drops an app start nobody marked as done", async () => {
    vi.useFakeTimers();
    const c = new Client({ dsn: DSN, transport, now: () => Date.now() });
    startAppStart(c, { now: () => 0 });
    await vi.advanceTimersByTimeAsync(APP_START_DEADLINE_MS + 1);
    await c.flush();
    expect(transactionsIn(sent.flatMap((e) => e.events))).toEqual([]);
  });

  it("trackNavigation ends the app start and times each new screen", async () => {
    vi.useFakeTimers();
    let route = "Home";
    let onState = () => {};
    const ref = { addListener: (_: "state", cb: () => void) => ((onState = cb), () => {}), getCurrentRoute: () => ({ name: route }) };
    juju.init({ dsn: DSN, transport, storage: fakeAsyncStorage().storage, sessions: false });
    juju.trackNavigation(ref);
    route = "Cart";
    onState();
    await vi.advanceTimersByTimeAsync(2_000);
    await juju.flush();
    const names = transactionsIn(sent.flatMap((e) => e.events)).map((t) => `${t.transaction.op} ${t.transaction.name}`);
    expect(names).toEqual(["app.start app.start", "screen.load Cart"]);
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
