import type { Client } from "@juju/core";

// Breadcrumbs record what led up to an error. Each source below wraps a
// browser API, records a short note, then calls the original so the app
// behaves exactly as before. Every installer returns an "uninstall" function
// that puts the original back.
//
// Privacy: we record methods, URL paths, status codes and element names.
// Never request or response bodies, query strings, or what the user typed:
// those can contain passwords, tokens and personal data.

type Uninstall = () => void;

const CONSOLE_LEVELS = ["debug", "info", "log", "warn", "error"] as const;

export function instrumentConsole(client: Client): Uninstall {
  const originals = new Map<string, (...args: unknown[]) => void>();
  for (const level of CONSOLE_LEVELS) {
    const original = console[level];
    if (typeof original !== "function") continue;
    originals.set(level, original);
    console[level] = function (this: Console, ...args: unknown[]) {
      client.addBreadcrumb({ type: "console", message: args.map(toText).join(" ").slice(0, 1000), data: { level } });
      return original.apply(this, args);
    };
  }
  return () => {
    for (const [level, original] of originals) (console as unknown as Record<string, unknown>)[level] = original;
  };
}

export function instrumentFetch(client: Client, isJujuUrl: (url: string) => boolean): Uninstall {
  const original = window.fetch;
  if (typeof original !== "function") return () => {};
  window.fetch = function (this: unknown, input: RequestInfo | URL, init?: RequestInit) {
    const url = requestUrl(input);
    // Skip juju's own requests: recording them would add a breadcrumb for
    // every send, and on failure could feed back into itself.
    if (isJujuUrl(url)) return original.call(this, input, init);
    const method = (init?.method ?? (input instanceof Request ? input.method : "GET")).toUpperCase();
    const started = Date.now();
    return original.call(this, input, init).then(
      (res) => {
        client.addBreadcrumb({ type: "fetch", message: `${method} ${redact(url)} → ${res.status}`, data: { method, url: redact(url), status: res.status, duration_ms: Date.now() - started } });
        return res;
      },
      (err: unknown) => {
        client.addBreadcrumb({ type: "fetch", message: `${method} ${redact(url)} → failed`, data: { method, url: redact(url), error: toText(err) } });
        throw err;
      },
    );
  } as typeof fetch;
  return () => {
    window.fetch = original;
  };
}

export function instrumentXHR(client: Client, isJujuUrl: (url: string) => boolean): Uninstall {
  if (typeof XMLHttpRequest === "undefined") return () => {};
  const proto = XMLHttpRequest.prototype;
  const originalOpen = proto.open;
  const originalSend = proto.send;
  const info = new WeakMap<XMLHttpRequest, { method: string; url: string }>();

  proto.open = function (this: XMLHttpRequest, method: string, url: string | URL, ...rest: unknown[]) {
    info.set(this, { method: method.toUpperCase(), url: String(url) });
    return (originalOpen as (...a: unknown[]) => void).call(this, method, url, ...rest);
  } as typeof proto.open;

  proto.send = function (this: XMLHttpRequest, body?: Document | XMLHttpRequestBodyInit | null) {
    const req = info.get(this);
    if (req && !isJujuUrl(req.url)) {
      this.addEventListener("loadend", () => {
        const status = this.status || "failed";
        client.addBreadcrumb({ type: "xhr", message: `${req.method} ${redact(req.url)} → ${status}`, data: { method: req.method, url: redact(req.url), status } });
      });
    }
    return originalSend.call(this, body);
  };

  return () => {
    proto.open = originalOpen;
    proto.send = originalSend;
  };
}

export function instrumentClicks(client: Client): Uninstall {
  // One listener on the document in the capture phase sees every click,
  // even ones the app stops from bubbling.
  const onClick = (ev: Event) => {
    if (ev.target instanceof Element) client.addBreadcrumb({ type: "click", message: describeElement(ev.target) });
  };
  document.addEventListener("click", onClick, true);
  return () => document.removeEventListener("click", onClick, true);
}

export function instrumentHistory(client: Client): Uninstall {
  // Single-page apps change pages without reloading, through the History
  // API, so page views only show up if we wrap pushState and replaceState.
  let lastPath = location.pathname;
  const record = () => {
    const to = location.pathname;
    if (to === lastPath) return;
    client.addBreadcrumb({ type: "navigation", message: `${lastPath} → ${to}`, data: { from: lastPath, to } });
    lastPath = to;
  };
  const originalPush = history.pushState;
  const originalReplace = history.replaceState;
  history.pushState = function (this: History, ...args: Parameters<History["pushState"]>) {
    const result = originalPush.apply(this, args);
    record();
    return result;
  };
  history.replaceState = function (this: History, ...args: Parameters<History["replaceState"]>) {
    const result = originalReplace.apply(this, args);
    record();
    return result;
  };
  window.addEventListener("popstate", record); // back and forward buttons
  return () => {
    history.pushState = originalPush;
    history.replaceState = originalReplace;
    window.removeEventListener("popstate", record);
  };
}

/** e.g. button#apply-coupon.btn.primary — the element, never its text. */
export function describeElement(el: Element): string {
  let s = el.tagName.toLowerCase();
  if (el.id) s += `#${el.id}`;
  const classes = Array.from(el.classList).slice(0, 2);
  if (classes.length) s += `.${classes.join(".")}`;
  return s;
}

/** Drops the query string and fragment, which often carry tokens. */
export function redact(url: string): string {
  const cut = url.search(/[?#]/);
  return cut >= 0 ? url.slice(0, cut) : url;
}

function requestUrl(input: RequestInfo | URL): string {
  if (typeof input === "string") return input;
  if (input instanceof URL) return input.href;
  return input.url;
}

function toText(v: unknown): string {
  if (typeof v === "string") return v;
  if (v instanceof Error) return `${v.name}: ${v.message}`;
  try {
    return JSON.stringify(v) ?? String(v);
  } catch {
    return String(v);
  }
}
