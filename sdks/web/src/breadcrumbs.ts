import { finishHttpSpan, redact, toText, type Client } from "@juju/core";
import { shortUrl } from "./performance";

// Console and XHR wrappers are shared with React Native, so they live in
// @juju/core (instrument.ts). This file has the browser-only sources.
export { instrumentConsole, instrumentXHR, redact } from "@juju/core";

// Breadcrumbs record what led up to an error. Each source below wraps a
// browser API, records a short note, then calls the original so the app
// behaves exactly as before. Every installer returns an "uninstall" function
// that puts the original back.
//
// Privacy: we record methods, URL paths, status codes and element names.
// Never request or response bodies, query strings, or what the user typed:
// those can contain passwords, tokens and personal data.

type Uninstall = () => void;

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
    // Joins the page load or navigation that's running, if any.
    const span = client.startSpan({ op: "http.client", description: `${method} ${shortUrl(url)}`, data: { method, url: shortUrl(url) } });
    return original.call(this, input, init).then(
      (res) => {
        client.addBreadcrumb({ type: "fetch", message: `${method} ${redact(url)} → ${res.status}`, data: { method, url: redact(url), status: res.status, duration_ms: Date.now() - started } });
        finishHttpSpan(span, res.status);
        return res;
      },
      (err: unknown) => {
        client.addBreadcrumb({ type: "fetch", message: `${method} ${redact(url)} → failed`, data: { method, url: redact(url), error: toText(err) } });
        finishHttpSpan(span, 0);
        throw err;
      },
    );
  } as typeof fetch;
  return () => {
    window.fetch = original;
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

function requestUrl(input: RequestInfo | URL): string {
  if (typeof input === "string") return input;
  if (input instanceof URL) return input.href;
  return input.url;
}
