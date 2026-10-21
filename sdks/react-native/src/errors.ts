import type { Client } from "@juju/core";

/** React Native's global error hook, available as the ErrorUtils global. */
export interface ErrorUtilsLike {
  getGlobalHandler(): GlobalHandler;
  setGlobalHandler(handler: GlobalHandler): void;
}
export type GlobalHandler = (error: unknown, isFatal?: boolean) => void;

/** How long a fatal crash waits for the report to be saved before the app is allowed to die. */
export const FATAL_TIMEOUT_MS = 2_000;

/**
 * Catches every uncaught JavaScript error.
 *
 * React Native has one global handler. In development it shows the red
 * error screen; in a release build it ends the app. We wrap it and always
 * call it afterwards, so the app behaves exactly as before.
 *
 * For a fatal error we first give the report up to 2 seconds to reach
 * storage (and the server, if the network is quick). Calling the previous
 * handler straight away would kill the process mid-write, and the crash, the
 * report we most need, would be the one we lose.
 */
export function installGlobalHandler(
  client: Client,
  errorUtils: ErrorUtilsLike | undefined = (globalThis as { ErrorUtils?: ErrorUtilsLike }).ErrorUtils,
  timeoutMs = FATAL_TIMEOUT_MS,
): () => void {
  if (!errorUtils) return () => {};
  const previous = errorUtils.getGlobalHandler();
  let handlingFatal = false;

  const handler: GlobalHandler = (error, isFatal) => {
    const callPrevious = () => previous?.(error, isFatal);
    try {
      // A fatal error ends the app in a release build: that's a crash, and
      // the session ends as crashed. Non-fatal ones only count as errors.
      client.captureException(error, { tags: { fatal: isFatal ? "true" : "false" }, crashed: Boolean(isFatal) });
    } catch {
      // captureException doesn't throw, but the app's crash behaviour must survive even if it did
    }
    // Non-fatal errors, and a second fatal error while we're already
    // waiting, go straight through.
    if (!isFatal || handlingFatal) return callPrevious();
    handlingFatal = true;

    let done = false;
    const finish = () => {
      if (done) return;
      done = true;
      clearTimeout(timer);
      callPrevious();
    };
    // Never wait forever: a hung disk or network must not hang the app.
    const timer = setTimeout(finish, timeoutMs);
    void Promise.allSettled([client.persistNow(), client.flush()]).then(finish);
  };

  errorUtils.setGlobalHandler(handler);
  return () => {
    // Only undo our own wrap: if something wrapped us since, leave it alone.
    if (errorUtils.getGlobalHandler() === handler) errorUtils.setGlobalHandler(previous);
  };
}

interface HermesLike {
  enablePromiseRejectionTracker?: (options: {
    allRejections: boolean;
    onUnhandled: (id: number, rejection: unknown) => void;
    onHandled: (id: number) => void;
  }) => void;
}

/**
 * Catches promises that fail with no .catch().
 *
 * The global handler above never sees these. Hermes, React Native's default
 * JavaScript engine, reports them through a tracker hook. Installing ours
 * replaces React Native's own tracker, which shows the "Possible unhandled
 * promise rejection" warning in development, so we keep that warning alive.
 */
export function installRejectionTracking(
  client: Client,
  hermes: HermesLike | undefined = (globalThis as { HermesInternal?: HermesLike }).HermesInternal,
  isDev: boolean = Boolean((globalThis as { __DEV__?: boolean }).__DEV__),
  warn: (...args: unknown[]) => void = console.warn.bind(console),
): () => void {
  if (typeof hermes?.enablePromiseRejectionTracker !== "function") return () => {};
  hermes.enablePromiseRejectionTracker({
    allRejections: true,
    onUnhandled: (id, rejection) => {
      client.captureException(rejection ?? "Unhandled promise rejection with no reason", { tags: { unhandled_rejection: "true" } });
      if (isDev) warn(`Possible unhandled promise rejection (id: ${id}):`, rejection);
    },
    onHandled: () => {},
  });
  // Hermes has no way to remove a tracker; close() leaves it installed but
  // the closed client ignores further errors.
  return () => {};
}
