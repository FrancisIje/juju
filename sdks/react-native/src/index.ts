// @juju/react-native: juju error tracking for React Native apps.
//
//   import AsyncStorage from "@react-native-async-storage/async-storage";
//   import * as juju from "@juju/react-native";
//
//   juju.init({ dsn: "https://pk_...@juju.example.com/1", release: "shop@1.4.0+12", storage: AsyncStorage });
//
// Call init as early as possible (top of index.js), so crashes during
// startup are caught too. Every function except init does nothing before
// init and never throws. init throws on an invalid DSN, so a configuration
// mistake is noticed at once.

import { Client, instrumentConsole, instrumentXHR, VERSION, type ClientOptions, type Transport, type User } from "@juju/core";
import { deviceContext, isInApp } from "./context";
import { installGlobalHandler, installRejectionTracking } from "./errors";
import { installAppState, trackNavigation as track, type NavigationRefLike } from "./lifecycle";
import { asyncStorageQueue, type AsyncStorageLike } from "./storage";

export type { JujuEvent, User, Breadcrumb } from "@juju/core";
export type { AsyncStorageLike, NavigationRefLike };

export interface ReactNativeOptions {
  dsn: string;
  /** e.g. "shop@1.4.0+12": the app version and build number. */
  release?: string;
  /** Defaults to "development" in debug builds and "production" in release builds. */
  environment?: string;
  /**
   * Where to keep unsent events between launches. Pass AsyncStorage from
   * @react-native-async-storage/async-storage. Without it, an error that
   * crashes the app can be lost before it's sent.
   */
  storage?: AsyncStorageLike;
  beforeSend?: ClientOptions["beforeSend"];
  breadcrumbs?: Partial<Record<"console" | "xhr" | "appState", boolean>>;
  debug?: boolean;
  flushIntervalMs?: number;
  /** Replace how events are sent (used by tests). */
  transport?: Transport;
}

let client: Client | undefined;
let uninstallers: Array<() => void> = [];

export function init(options: ReactNativeOptions): void {
  close();
  // Keep the real console.warn before we wrap console, so the SDK's own
  // messages don't become breadcrumbs.
  const warn = console.warn.bind(console);
  const isDev = Boolean((globalThis as { __DEV__?: boolean }).__DEV__);

  const clientOptions: ClientOptions = {
    dsn: options.dsn,
    platform: "javascript",
    sdk: { name: "juju.react-native", version: VERSION },
    environment: options.environment ?? (isDev ? "development" : "production"),
    context: deviceContext,
    isInApp,
    logger: { warn },
  };
  if (options.release) clientOptions.release = options.release;
  if (options.beforeSend) clientOptions.beforeSend = options.beforeSend;
  if (options.debug) clientOptions.debug = options.debug;
  if (options.flushIntervalMs) clientOptions.flushIntervalMs = options.flushIntervalMs;
  if (options.transport) clientOptions.transport = options.transport;
  if (options.storage) {
    clientOptions.persistence = asyncStorageQueue(options.storage);
    clientOptions.identityStorage = options.storage; // the anonymous id, so one install stays one person
  } else {
    warn("[juju] init() was called without storage, so a crash can be lost before it's sent. Pass AsyncStorage: init({ dsn, storage: AsyncStorage })");
  }

  const c = new Client(clientOptions);
  client = c;

  const isJujuUrl = (url: string) => url.startsWith(c.dsn.origin + "/api/");
  const crumbs = { console: true, xhr: true, appState: true, ...options.breadcrumbs };
  uninstallers = [installGlobalHandler(c), installRejectionTracking(c, undefined, isDev, warn)];
  if (crumbs.appState) uninstallers.push(installAppState(c));
  if (crumbs.console) uninstallers.push(instrumentConsole(c));
  // React Native's fetch is built on XMLHttpRequest, so this one wrapper
  // records fetch calls too, each exactly once.
  if (crumbs.xhr) uninstallers.push(instrumentXHR(c, isJujuUrl, typeof XMLHttpRequest === "undefined" ? undefined : XMLHttpRequest));
}

/** Report an error the app caught. Returns the event_id, or "" if not sent. */
export function captureException(error: unknown, extra?: { tags?: Record<string, string> }): string {
  return client?.captureException(error, extra) ?? "";
}

/**
 * Records something a user did, for product analytics:
 * capture("checkout_started", { plan: "pro" }).
 */
export function capture(name: string, properties?: Record<string, unknown>): void {
  client?.capture(name, properties);
}

/** After login: events from this device so far, and from now on, belong to userId. */
export function identify(userId: string): void {
  client?.identify(userId);
}

/** After logout: forget the user, so whoever uses the device next is a new person. */
export function reset(): void {
  client?.reset();
}

export function setUser(user: User | null): void {
  client?.setUser(user);
}

export function setTag(key: string, value: string): void {
  client?.setTag(key, value);
}

export function addBreadcrumb(crumb: { type: string; message?: string; data?: Record<string, unknown> }): void {
  client?.addBreadcrumb(crumb);
}

/**
 * Records screen changes as breadcrumbs. Pass React Navigation's container ref:
 *
 *   const ref = createNavigationContainerRef();
 *   <NavigationContainer ref={ref} onReady={() => juju.trackNavigation(ref)}>
 */
export function trackNavigation(ref: NavigationRefLike): () => void {
  if (!client) return () => {};
  const stop = track(client, ref);
  uninstallers.push(stop);
  return stop;
}

/** Send queued events now. */
export function flush(): Promise<void> {
  return client?.flush() ?? Promise.resolve();
}

/** Resolves once events saved by the previous launch are queued again. */
export function ready(): Promise<void> {
  return client?.ready ?? Promise.resolve();
}

/** Remove every hook and stop sending. */
export function close(): void {
  for (const uninstall of uninstallers.reverse()) {
    try {
      uninstall();
    } catch {
      // keep removing the rest
    }
  }
  uninstallers = [];
  client?.close();
  client = undefined;
}
