import type { Client, Transaction } from "@juju/core";

// App start and screen load timing for React Native.

/** How long a screen must be quiet (no requests) before its load counts as done. */
export const IDLE_TIMEOUT_MS = 1_000;
/** startTime this much before the runtime start counts as a real native start. */
const NATIVE_START_MIN_MS = 1;
/** An app start nobody marked as finished within this is dropped, not sent. */
export const APP_START_DEADLINE_MS = 30_000;

/**
 * React Native 0.76+ reports when the app process started and when the
 * JavaScript runtime and bundle began, relative to performance.timeOrigin.
 * Values can be null: the native side only records what it implements.
 */
interface StartupTiming {
  startTime?: number | null;
  initializeRuntimeStart?: number | null;
  executeJavaScriptBundleEntryPointStart?: number | null;
}

interface PerformanceLike {
  now(): number;
  rnStartupTiming?: StartupTiming;
}

/**
 * Starts the app.start transaction. It begins when the native app started
 * if the platform recorded it (Android), otherwise when the JavaScript
 * runtime started (iOS), otherwise now (juju's init), and ends when the
 * first screen is ready: trackNavigation's onReady, or appStarted().
 */
export function startAppStart(
  client: Client,
  perf: PerformanceLike | undefined = (globalThis as { performance?: PerformanceLike }).performance,
): { tx: Transaction; done: () => void } {
  const initAt = client.now();
  let timing: StartupTiming | undefined;
  try {
    timing = perf?.rnStartupTiming; // a getter that calls into native code; never let it break init
  } catch {
    timing = undefined;
  }
  // performance.now() counts from timeOrigin, like the startup times do.
  const abs = (t: number) => (perf ? initAt - perf.now() + t : initAt);
  const runtimeStart = valid(timing?.initializeRuntimeStart);
  const bundleStart = valid(timing?.executeJavaScriptBundleEntryPointStart);
  // startTime is the native app's start only where the platform records
  // one. Where it doesn't (iOS, as of React Native 0.87), React Native fills
  // it with the runtime's start: the same moment, give or take microseconds.
  // Calling that "native start" would hide the native part of the start.
  let nativeStart = valid(timing?.startTime);
  if (nativeStart !== undefined && runtimeStart !== undefined && runtimeStart - nativeStart < NATIVE_START_MIN_MS) nativeStart = undefined;
  const known = [nativeStart, runtimeStart, bundleStart].filter((t): t is number => t !== undefined);
  const first = known.length ? Math.min(...known) : undefined;

  const tx = client.startTransaction({ name: "app.start", op: "app.start", start: first !== undefined ? abs(first) : initAt, active: true });
  const step = (description: string, from: number | undefined, to: number | undefined) => {
    if (from !== undefined && to !== undefined && to > from) tx.startChild({ op: "app.start.phase", description, start: abs(from) }).finish(abs(to));
  };
  step("Native app start", nativeStart, runtimeStart);
  step("Start the JavaScript runtime", runtimeStart, bundleStart);
  if (first !== undefined) tx.startChild({ op: "app.start.phase", description: "Run the JavaScript bundle until juju.init", start: abs(Math.max(first, bundleStart ?? first)) }).finish(initAt);
  // Where the start time came from, so nobody compares a native-start
  // measurement with a runtime-start one by accident.
  tx.setMeasurement(nativeStart !== undefined ? "from_native_start" : first !== undefined ? "from_runtime_start" : "from_init", 1);

  // If the app never says its first screen is ready, the "start" would run
  // until some unrelated event: better no number than a wrong one.
  const timer = setTimeout(() => tx.discard(), APP_START_DEADLINE_MS);
  const done = () => {
    clearTimeout(timer);
    if (tx.finished) return;
    const end = client.now();
    tx.startChild({ op: "app.start.phase", description: "First screen", start: initAt }).finish(end);
    tx.finish(end);
  };
  return { tx, done };
}

function valid(t: number | null | undefined): number | undefined {
  return typeof t === "number" && Number.isFinite(t) && t >= 0 ? t : undefined;
}

/**
 * A screen.load transaction for a screen that was just navigated to: until
 * it has rendered and the requests it made have finished.
 */
export function startScreenLoad(client: Client, screen: string): Transaction {
  const tx = client.startTransaction({ name: screen, op: "screen.load", idleTimeoutMs: IDLE_TIMEOUT_MS, active: true });
  // The navigation state changes before React renders the new screen. Two
  // animation frames later, it has been drawn.
  const release = tx.hold();
  const raf = typeof requestAnimationFrame === "function" ? requestAnimationFrame : (cb: () => void) => setTimeout(cb, 16);
  raf(() => raf(() => release()));
  return tx;
}
