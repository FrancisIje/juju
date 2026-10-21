import type { Client, Transaction } from "@juju/core";

// Page load and navigation timing in the browser.
//
// Most of the page load already happened before any script ran, but the
// browser recorded it: Navigation Timing has when DNS, the connection, the
// first byte and the load event happened, Resource Timing has every script
// and stylesheet. We turn those into spans once the page has loaded, and
// requests the page makes while loading join as http.client spans.

type Uninstall = () => void;

/** How long a page or route must be quiet (no requests) before its load counts as done. */
export const IDLE_TIMEOUT_MS = 1_000;

/**
 * Turns a URL path into a transaction name, replacing ids with ":id":
 * /orders/8812/items → /orders/:id/items. Without this every order page
 * would be its own transaction, each seen once, and percentiles over one
 * sample mean nothing.
 */
export function routeName(path: string): string {
  return (
    path
      .split("/")
      .map((seg) =>
        /^\d+$/.test(seg) || // 8812
        /^[0-9a-f]{8}-[0-9a-f]{4}-[0-9a-f]{4}-[0-9a-f]{4}-[0-9a-f]{12}$/i.test(seg) || // a uuid
        (/^[0-9a-z_-]{16,}$/i.test(seg) && /\d/.test(seg)) // long tokens with digits: hashes, ids
          ? ":id"
          : seg,
      )
      .join("/") || "/"
  );
}

interface Timing {
  perf: Performance;
  /** performance.now()-style time → the client's clock. */
  abs: (t: number) => number;
}

function timing(client: Client): Timing | undefined {
  const perf = typeof performance !== "undefined" ? performance : undefined;
  if (!perf || typeof perf.now !== "function") return undefined;
  // Both clocks are read now, so the conversion lines up with the client's
  // own clock (used by every other span) even if the system clock moved.
  const offset = client.now() - perf.now();
  return { perf, abs: (t) => offset + t };
}

export function installPageLoad(client: Client): Uninstall {
  const t = timing(client);
  if (!t) return () => {};
  const { perf, abs } = t;
  // Starts at the navigation itself (time 0), not when juju loaded.
  const tx = client.startTransaction({ name: routeName(location.pathname), op: "pageload", start: abs(0), idleTimeoutMs: IDLE_TIMEOUT_MS, active: true });
  const release = tx.hold();
  const stopObservers = observePaint(tx);

  const onLoad = () => {
    // loadEventEnd is only filled in after the load handlers return.
    setTimeout(() => {
      const nav = perf.getEntriesByType?.("navigation")[0] as PerformanceNavigationTiming | undefined;
      if (nav) addNavigationSpans(tx, nav, abs);
      addResourceSpans(tx, perf, abs, nav?.loadEventEnd);
      release(nav?.loadEventEnd ? abs(nav.loadEventEnd) : undefined);
    }, 0);
  };
  if (document.readyState === "complete") onLoad();
  else window.addEventListener("load", onLoad, { once: true });

  return () => {
    window.removeEventListener("load", onLoad);
    stopObservers();
  };
}

function span(tx: Transaction, op: string, description: string, start: number, end: number, abs: (t: number) => number) {
  if (!(end > start) || start < 0) return; // a phase that didn't happen (cached DNS, reused connection)
  tx.startChild({ op, description, start: abs(start) }).finish(abs(end));
}

export function addNavigationSpans(tx: Transaction, nav: PerformanceNavigationTiming, abs: (t: number) => number) {
  // Everything before the browser starts fetching: unloading the previous
  // page, redirects, a new tab's process starting. Without this span the
  // waterfall would start with an unexplained gap.
  span(tx, "browser", "Start the navigation", 0, nav.fetchStart, abs);
  span(tx, "browser", "DNS lookup", nav.domainLookupStart, nav.domainLookupEnd, abs);
  span(tx, "browser", "Connect", nav.connectStart, nav.connectEnd, abs);
  // The browser had a connection but hadn't sent the request yet (DevTools
  // calls this "Stalled"): a busy connection pool, the network process
  // still starting. Seen at 150-600 ms in a cold headless Chrome.
  span(tx, "browser", "Queued in the browser", Math.max(nav.connectEnd, nav.fetchStart), nav.requestStart, abs);
  span(tx, "browser", "Waiting for the server", nav.requestStart, nav.responseStart, abs);
  span(tx, "browser", "Download page", nav.responseStart, nav.responseEnd, abs);
  span(tx, "browser", "Build page (DOMContentLoaded)", nav.responseEnd, nav.domContentLoadedEventEnd, abs);
  span(tx, "browser", "Load images and the rest", nav.domContentLoadedEventEnd, nav.loadEventEnd, abs);
  tx.setMeasurement("ttfb", nav.responseStart);
  tx.setMeasurement("dom_content_loaded", nav.domContentLoadedEventEnd);
  tx.setMeasurement("load", nav.loadEventEnd);
}

/** Scripts and stylesheets block the first paint, so each one gets a span. */
function addResourceSpans(tx: Transaction, perf: Performance, abs: (t: number) => number, before = Infinity) {
  for (const e of (perf.getEntriesByType?.("resource") ?? []) as PerformanceResourceTiming[]) {
    const kind = e.initiatorType === "script" ? "resource.script" : e.initiatorType === "link" || e.initiatorType === "css" ? "resource.css" : undefined;
    if (!kind || e.startTime > before) continue;
    span(tx, kind, shortUrl(e.name), e.startTime, e.responseEnd, abs);
  }
}

/**
 * The URL as requested, without the query string: the same-origin path, or
 * origin + path for other sites. "../api/cart" in the code becomes
 * "/shop/api/cart", which is what the server saw.
 */
export function shortUrl(url: string): string {
  try {
    const u = new URL(url, location.href);
    return u.origin === location.origin ? u.pathname : u.origin + u.pathname;
  } catch {
    return url;
  }
}

/**
 * First and largest contentful paint: when something, and then the main
 * thing, appeared. These are what a person perceives as "loaded", so they
 * go on the page load as measurements.
 */
function observePaint(tx: Transaction): Uninstall {
  if (typeof PerformanceObserver === "undefined") return () => {};
  const observers: PerformanceObserver[] = [];
  const watch = (type: string, onEntry: (e: PerformanceEntry) => void) => {
    try {
      const o = new PerformanceObserver((list) => list.getEntries().forEach(onEntry));
      o.observe({ type, buffered: true }); // buffered: entries from before juju loaded too
      observers.push(o);
    } catch {
      // this browser doesn't support that entry type
    }
  };
  watch("paint", (e) => e.name === "first-contentful-paint" && tx.setMeasurement("fcp", e.startTime));
  // LCP is reported again each time a larger element paints; the last one wins.
  watch("largest-contentful-paint", (e) => tx.setMeasurement("lcp", e.startTime));
  return () => observers.forEach((o) => o.disconnect());
}

/**
 * Single-page apps change pages without a page load, through the History
 * API. Each change is a "navigation" transaction: from the URL changing
 * until the new view has rendered and its requests have finished.
 */
export function installNavigationTracing(client: Client): Uninstall {
  let lastPath = location.pathname;
  const onChange = () => {
    const to = location.pathname;
    if (to === lastPath) return;
    lastPath = to;
    const tx = client.startTransaction({ name: routeName(to), op: "navigation", idleTimeoutMs: IDLE_TIMEOUT_MS, active: true });
    // Two animation frames: the first runs before the browser paints the
    // new view, the second after it.
    const release = tx.hold();
    const raf = typeof requestAnimationFrame === "function" ? requestAnimationFrame : (cb: () => void) => setTimeout(cb, 16);
    raf(() => raf(() => release()));
  };
  const originalPush = history.pushState;
  const originalReplace = history.replaceState;
  history.pushState = function (this: History, ...args: Parameters<History["pushState"]>) {
    const result = originalPush.apply(this, args);
    onChange();
    return result;
  };
  history.replaceState = function (this: History, ...args: Parameters<History["replaceState"]>) {
    const result = originalReplace.apply(this, args);
    onChange();
    return result;
  };
  window.addEventListener("popstate", onChange);
  return () => {
    history.pushState = originalPush;
    history.replaceState = originalReplace;
    window.removeEventListener("popstate", onChange);
  };
}
