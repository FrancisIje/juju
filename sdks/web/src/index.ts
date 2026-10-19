// @juju/web: juju error tracking for the browser.
//
//   import * as juju from "@juju/web";
//   juju.init({ dsn: "https://pk_...@juju.example.com/1", release: "web@2.3.1" });
//
// Every function except init is safe to call before init (it does nothing)
// and never throws, so adding juju can't be what breaks an app. init itself
// throws on an invalid DSN: a configuration mistake should be loud, at
// startup, not a silent absence of error reports.

import { Client, fetchTransport, parseDsn, VERSION, type ClientOptions, type Transport, type User } from "@juju/core";
import { instrumentClicks, instrumentConsole, instrumentFetch, instrumentHistory, instrumentXHR } from "./breadcrumbs";
import { installErrorHandlers } from "./errors";
import { installLifecycle } from "./lifecycle";

export type { JujuEvent, User, Breadcrumb } from "@juju/core";

export interface WebOptions {
  dsn: string;
  release?: string;
  environment?: string;
  /** Change or drop (return null) an event before it is sent. */
  beforeSend?: ClientOptions["beforeSend"];
  /** Which breadcrumb sources to record. All are on by default. */
  breadcrumbs?: Partial<Record<"console" | "fetch" | "xhr" | "clicks" | "navigation", boolean>>;
  /** Log what the SDK is doing to the console. */
  debug?: boolean;
  /** Replace how events are sent (used by tests). */
  transport?: Transport;
  /** Defaults to files served from this page's own origin. */
  isInApp?: (filename: string) => boolean;
  flushIntervalMs?: number;
}

let client: Client | undefined;
let uninstallers: Array<() => void> = [];

export function init(options: WebOptions): void {
  close(); // calling init twice replaces the old setup instead of doubling every hook

  // Keep the real fetch and console before we wrap them, so juju's own sends
  // and debug logs don't go through our breadcrumb wrappers.
  const nativeFetch = window.fetch.bind(window);
  const nativeConsole = { warn: console.warn.bind(console) };

  const clientOptions: ClientOptions = {
    dsn: options.dsn,
    platform: "javascript",
    sdk: { name: "juju.web", version: VERSION },
    isInApp: options.isInApp ?? ((file) => file.startsWith(location.origin) && !file.includes("/node_modules/")),
    context: () => ({
      browser: { user_agent: navigator.userAgent, language: navigator.language },
      page: { url: location.origin + location.pathname },
    }),
    logger: nativeConsole,
  };
  if (options.release) clientOptions.release = options.release;
  if (options.environment) clientOptions.environment = options.environment;
  if (options.beforeSend) clientOptions.beforeSend = options.beforeSend;
  if (options.debug) clientOptions.debug = options.debug;
  if (options.flushIntervalMs) clientOptions.flushIntervalMs = options.flushIntervalMs;
  const c = new Client({ ...clientOptions, transport: options.transport ?? fetchTransport(parseDsn(options.dsn), nativeFetch) });
  client = c;

  const isJujuUrl = (url: string) => url.startsWith(c.dsn.origin + "/api/");
  const crumbs = { console: true, fetch: true, xhr: true, clicks: true, navigation: true, ...options.breadcrumbs };
  uninstallers = [installErrorHandlers(c), installLifecycle(c)];
  if (crumbs.console) uninstallers.push(instrumentConsole(c));
  if (crumbs.fetch) uninstallers.push(instrumentFetch(c, isJujuUrl));
  if (crumbs.xhr) uninstallers.push(instrumentXHR(c, isJujuUrl, typeof XMLHttpRequest === "undefined" ? undefined : XMLHttpRequest));
  if (crumbs.clicks) uninstallers.push(instrumentClicks(c));
  if (crumbs.navigation) uninstallers.push(instrumentHistory(c));
}

/** Report an error the app caught. Returns the event_id, or "" if not sent. */
export function captureException(error: unknown, extra?: { tags?: Record<string, string> }): string {
  return client?.captureException(error, extra) ?? "";
}

/** Attach a user to future events (call with null on logout). */
export function setUser(user: User | null): void {
  client?.setUser(user);
}

export function setTag(key: string, value: string): void {
  client?.setTag(key, value);
}

export function addBreadcrumb(crumb: { type: string; message?: string; data?: Record<string, unknown> }): void {
  client?.addBreadcrumb(crumb);
}

/** Send queued events now. Resolves when the send finishes or fails. */
export function flush(): Promise<void> {
  return client?.flush() ?? Promise.resolve();
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

