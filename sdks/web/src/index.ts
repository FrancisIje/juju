// @juju/web: juju error tracking for the browser.
//
//   import * as juju from "@juju/web";
//   juju.init({ dsn: "https://pk_...@juju.example.com/1", release: "web@2.3.1" });
//
// Every function except init is safe to call before init (it does nothing)
// and never throws, so adding juju can't be what breaks an app. init itself
// throws on an invalid DSN: a configuration mistake should be loud, at
// startup, not a silent absence of error reports.

import { Client, fetchTransport, parseDsn, VERSION, type ClientOptions, type Transaction, type Transport, type User } from "@juju/core";
import { instrumentClicks, instrumentConsole, instrumentFetch, instrumentHistory, instrumentXHR } from "./breadcrumbs";
import { installErrorHandlers } from "./errors";
import { installLifecycle } from "./lifecycle";
import { installNavigationTracing, installPageLoad, shortUrl } from "./performance";

export type { JujuEvent, User, Breadcrumb, Transaction, Span } from "@juju/core";

export interface WebOptions {
  dsn: string;
  release?: string;
  environment?: string;
  /** Change or drop (return null) an event before it is sent. */
  beforeSend?: ClientOptions["beforeSend"];
  /** Which breadcrumb sources to record. All are on by default. */
  breadcrumbs?: Partial<Record<"console" | "fetch" | "xhr" | "clicks" | "navigation", boolean>>;
  /** Count sessions (page loads) for the crash-free rate. Default true. */
  sessions?: boolean;
  /** Time page loads, route changes and their requests. Default true. */
  tracing?: boolean;
  /** Share of page loads and navigations to send, 0 to 1. Default 1. */
  tracesSampleRate?: number;
  /** Log what the SDK is doing to the console. */
  debug?: boolean;
  /** Replace how events are sent (used by tests). */
  transport?: Transport;
  /** Defaults to files served from this page's own origin. */
  isInApp?: (filename: string) => boolean;
  flushIntervalMs?: number;
}

let client: Client | undefined;
// Hands out transactions before init (or after close) that go nowhere, so
// app code timing things never has to check whether juju is set up.
const inert = new Client({ dsn: "https://pk_inert@localhost/0", transport: { send: async () => ({ ok: true }) } });
inert.close();
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
  if (options.tracesSampleRate !== undefined) clientOptions.tracesSampleRate = options.tracesSampleRate;
  const c = new Client({ ...clientOptions, transport: options.transport ?? fetchTransport(parseDsn(options.dsn), nativeFetch) });
  client = c;

  const isJujuUrl = (url: string) => url.startsWith(c.dsn.origin + "/api/");
  const crumbs = { console: true, fetch: true, xhr: true, clicks: true, navigation: true, ...options.breadcrumbs };
  uninstallers = [installErrorHandlers(c), installLifecycle(c, options.sessions ?? true)];
  if (options.tracing ?? true) uninstallers.push(installPageLoad(c), installNavigationTracing(c));
  if (crumbs.console) uninstallers.push(instrumentConsole(c));
  if (crumbs.fetch) uninstallers.push(instrumentFetch(c, isJujuUrl));
  if (crumbs.xhr) uninstallers.push(instrumentXHR(c, isJujuUrl, typeof XMLHttpRequest === "undefined" ? undefined : XMLHttpRequest, shortUrl));
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

/**
 * Times something the app does, as a transaction with spans:
 *
 *   const tx = juju.startTransaction({ name: "checkout", op: "custom" });
 *   const span = tx.startChild({ op: "db", description: "save order" });
 *   ...; span.finish(); tx.finish();
 *
 * Before init it returns a transaction that sends nothing.
 */
export function startTransaction(options: { name: string; op?: string }): Transaction {
  return (client ?? inert).startTransaction({ op: "custom", ...options });
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

