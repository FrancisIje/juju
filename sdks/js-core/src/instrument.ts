import type { Client } from "./client";
import type { Span } from "./tracing";

// Breadcrumb sources that exist on more than one platform: console is
// everywhere, and XMLHttpRequest exists in browsers and in React Native
// (where fetch itself is built on XHR, so wrapping XHR sees every request).
//
// Core never reaches for globals itself: the platform passes in what to
// wrap. Each installer records a short note, calls the original so the app
// behaves exactly as before, and returns a function that undoes the wrap.
//
// Privacy: we record methods, URL paths and status codes. Never request or
// response bodies, or query strings, which often carry tokens.

export type Uninstall = () => void;

const CONSOLE_LEVELS = ["debug", "info", "log", "warn", "error"] as const;

export function instrumentConsole(client: Client, target: Console = console): Uninstall {
  const originals = new Map<string, (...args: unknown[]) => void>();
  for (const level of CONSOLE_LEVELS) {
    const original = target[level];
    if (typeof original !== "function") continue;
    originals.set(level, original);
    target[level] = function (this: Console, ...args: unknown[]) {
      client.addBreadcrumb({ type: "console", message: args.map(toText).join(" ").slice(0, 1000), data: { level } });
      return original.apply(this, args);
    };
  }
  return () => {
    for (const [level, original] of originals) (target as unknown as Record<string, unknown>)[level] = original;
  };
}

interface XHRLike {
  prototype: {
    open: (...args: never[]) => unknown;
    send: (...args: never[]) => unknown;
  };
}

/**
 * describeUrl names a request's span; the web passes one that resolves
 * relative URLs against the page, so the span shows what was requested.
 */
export function instrumentXHR(client: Client, isJujuUrl: (url: string) => boolean, XHR: XHRLike | undefined, describeUrl: (url: string) => string = redact): Uninstall {
  if (!XHR) return () => {};
  const proto = XHR.prototype as unknown as {
    open: (this: XMLHttpRequest, method: string, url: string | URL, ...rest: unknown[]) => void;
    send: (this: XMLHttpRequest, body?: unknown) => void;
  };
  const originalOpen = proto.open;
  const originalSend = proto.send;
  const info = new WeakMap<object, { method: string; url: string }>();

  proto.open = function (method, url, ...rest) {
    info.set(this, { method: String(method).toUpperCase(), url: String(url) });
    return originalOpen.call(this, method, url, ...rest);
  };

  proto.send = function (body) {
    const req = info.get(this);
    // Skip juju's own requests: recording them would add a breadcrumb for
    // every send, and on failure could feed back into itself.
    if (req && !isJujuUrl(req.url)) {
      const started = Date.now();
      // A span in the transaction that's running (a page or screen load),
      // if any, so slow requests show up as the reason it was slow.
      const span = client.startSpan({ op: "http.client", description: `${req.method} ${describeUrl(req.url)}`, data: { method: req.method, url: describeUrl(req.url) } });
      this.addEventListener("loadend", () => {
        const status = this.status || "failed";
        client.addBreadcrumb({
          type: "xhr",
          message: `${req.method} ${redact(req.url)} → ${status}`,
          data: { method: req.method, url: redact(req.url), status, duration_ms: Date.now() - started },
        });
        finishHttpSpan(span, this.status);
      });
    }
    return originalSend.call(this, body);
  };

  return () => {
    proto.open = originalOpen;
    proto.send = originalSend;
  };
}

/** Ends an HTTP span with its status code; 0 means the request never got an answer. */
export function finishHttpSpan(span: Span, status: number): void {
  if (status) span.setData("status_code", status);
  span.setStatus(status === 0 ? "failed" : status >= 500 ? "error" : "ok").finish();
}

/** Drops the query string and fragment, which often carry tokens. */
export function redact(url: string): string {
  const cut = url.search(/[?#]/);
  return cut >= 0 ? url.slice(0, cut) : url;
}

export function toText(v: unknown): string {
  if (typeof v === "string") return v;
  if (v instanceof Error) return `${v.name}: ${v.message}`;
  try {
    return JSON.stringify(v) ?? String(v);
  } catch {
    return String(v);
  }
}
