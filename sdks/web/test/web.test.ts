// @vitest-environment happy-dom
import { Client } from "@juju/core";
import type { Envelope, JujuEvent, OutgoingEvent, SendResult, SessionEvent, TransactionEvent, Transport } from "@juju/core";
import { afterEach, beforeEach, describe, expect, it, vi } from "vitest";
import { describeElement, redact } from "../src/breadcrumbs";
import { addNavigationSpans, routeName, shortUrl } from "../src/performance";
import * as juju from "../src/index";

const DSN = "https://pk_abc@juju.example.com/1";

let sent: Envelope[];
let transport: Transport;

function setup(options: Partial<juju.WebOptions> = {}) {
  juju.init({ dsn: DSN, transport, flushIntervalMs: 10, ...options });
}

/** Every event sent so far, of every type. */
async function allEvents(): Promise<OutgoingEvent[]> {
  await juju.flush();
  return sent.flatMap((env) => env.events);
}

/** The errors sent so far. Sessions and page loads travel in the same envelopes. */
async function sentEvents(): Promise<JujuEvent[]> {
  return (await allEvents()).filter((e): e is JujuEvent => e.type === "error");
}

beforeEach(() => {
  sent = [];
  transport = { send: vi.fn(async (env: Envelope): Promise<SendResult> => (sent.push(structuredClone(env)), { ok: true })) };
});
afterEach(() => juju.close());

describe("catching errors", () => {
  it("captures uncaught errors", async () => {
    setup();
    window.dispatchEvent(new ErrorEvent("error", { error: new TypeError("x is undefined"), message: "x is undefined" }));
    const [e] = await sentEvents();
    expect(e).toMatchObject({ platform: "javascript", exception: { type: "TypeError", message: "x is undefined" } });
  });

  it("captures promise rejections nobody caught", async () => {
    setup();
    const ev = new Event("unhandledrejection") as Event & { reason: unknown };
    ev.reason = new Error("fetch failed");
    window.dispatchEvent(ev);
    const [e] = await sentEvents();
    expect(e?.exception.message).toBe("fetch failed");
  });

  // happy-dom doesn't call window.onerror for dispatched events (real
  // browsers do), so we check what we control: we never replace the app's
  // handler, and other listeners still see the error. The real-browser check
  // is part of the manual verification in Chrome.
  it("leaves the app's own error handlers in place", async () => {
    const appHandler = vi.fn();
    const appListener = vi.fn();
    window.onerror = appHandler;
    window.addEventListener("error", appListener);
    setup();
    expect(window.onerror).toBe(appHandler);
    window.dispatchEvent(new ErrorEvent("error", { error: new Error("boom"), message: "boom" }));
    expect(appListener).toHaveBeenCalled();
    expect(await sentEvents()).toHaveLength(1);
    window.onerror = null;
    window.removeEventListener("error", appListener);
  });

  it("reports an error once when the app captures it and then rethrows", async () => {
    setup();
    const err = new Error("handled, then rethrown");
    juju.captureException(err);
    window.dispatchEvent(new ErrorEvent("error", { error: err, message: err.message }));
    expect(await sentEvents()).toHaveLength(1);
  });

  it("stops listening after close()", async () => {
    setup();
    juju.close();
    window.dispatchEvent(new ErrorEvent("error", { error: new Error("after close"), message: "after close" }));
    expect(sent).toHaveLength(0);
  });

  it("does nothing, and doesn't throw, before init", () => {
    expect(juju.captureException(new Error("early"))).toBe("");
    expect(() => juju.setUser({ id: "u" })).not.toThrow();
  });

  it("throws on an invalid DSN so the mistake is noticed", () => {
    expect(() => juju.init({ dsn: "not-a-dsn" })).toThrow(/not a valid DSN/);
  });
});

describe("breadcrumbs", () => {
  it("records console output and still prints it", async () => {
    const original = vi.fn();
    console.warn = original;
    setup();
    console.warn("coupon response missing", { price: undefined });
    expect(original).toHaveBeenCalledWith("coupon response missing", { price: undefined });
    juju.captureException(new Error("x"));
    const [e] = await sentEvents();
    expect(e?.breadcrumbs).toContainEqual(expect.objectContaining({ type: "console", message: "coupon response missing {}", data: { level: "warn" } }));
  });

  it("restores console after close()", () => {
    const original = console.log;
    setup();
    expect(console.log).not.toBe(original);
    juju.close();
    expect(console.log).toBe(original);
  });

  it("records fetch calls without the query string, and skips juju's own", async () => {
    window.fetch = vi.fn(async () => new Response("{}", { status: 500 })) as typeof fetch;
    setup();
    await fetch("https://shop.example.com/api/coupon?token=secret", { method: "post" });
    await fetch("https://juju.example.com/api/v1/projects/1/events");
    juju.captureException(new Error("x"));
    const crumbs = (await sentEvents())[0]?.breadcrumbs ?? [];
    const fetches = crumbs.filter((c) => c.type === "fetch");
    expect(fetches).toHaveLength(1);
    expect(fetches[0]?.message).toBe("POST https://shop.example.com/api/coupon → 500");
    expect(JSON.stringify(crumbs)).not.toContain("secret");
  });

  it("records failed fetches and rethrows the error", async () => {
    window.fetch = vi.fn(async () => {
      throw new TypeError("Failed to fetch");
    }) as typeof fetch;
    setup();
    await expect(fetch("/api/x")).rejects.toThrow("Failed to fetch");
    juju.captureException(new Error("x"));
    const crumbs = (await sentEvents())[0]?.breadcrumbs ?? [];
    expect(crumbs.find((c) => c.type === "fetch")?.message).toBe("GET /api/x → failed");
  });

  it("records clicks as the element, never its text", async () => {
    setup();
    const button = document.createElement("button");
    button.id = "apply-coupon";
    button.className = "btn primary extra";
    button.textContent = "Apply code SECRET10";
    document.body.append(button);
    button.click();
    juju.captureException(new Error("x"));
    const crumbs = (await sentEvents())[0]?.breadcrumbs ?? [];
    expect(crumbs).toContainEqual(expect.objectContaining({ type: "click", message: "button#apply-coupon.btn.primary" }));
    expect(JSON.stringify(crumbs)).not.toContain("SECRET10");
    button.remove();
  });

  it("records single-page app navigation", async () => {
    history.replaceState(null, "", "/products");
    setup();
    history.pushState(null, "", "/cart");
    juju.captureException(new Error("x"));
    const crumbs = (await sentEvents())[0]?.breadcrumbs ?? [];
    expect(crumbs).toContainEqual(expect.objectContaining({ type: "navigation", message: "/products → /cart" }));
  });

  it("can turn sources off", async () => {
    const original = console.info;
    setup({ breadcrumbs: { console: false } });
    expect(console.info).toBe(original);
  });

  it("describes and redacts as expected", () => {
    expect(redact("/a?b=1#c")).toBe("/a");
    const div = document.createElement("div");
    expect(describeElement(div)).toBe("div");
  });
});

describe("page lifecycle", () => {
  function hide() {
    Object.defineProperty(document, "visibilityState", { value: "hidden", configurable: true });
    document.dispatchEvent(new Event("visibilitychange"));
  }
  afterEach(() => {
    Object.defineProperty(document, "visibilityState", { value: "visible", configurable: true });
  });

  it("sends queued events with sendBeacon when the page is hidden", async () => {
    const beacon = vi.fn((_url: string, _body: Blob) => true);
    navigator.sendBeacon = beacon as typeof navigator.sendBeacon;
    setup({ flushIntervalMs: 60_000 }); // so the normal send doesn't happen first
    juju.captureException(new Error("closing the tab"));
    hide();

    expect(beacon).toHaveBeenCalledTimes(1);
    const [url, blob] = beacon.mock.calls[0]!;
    // No headers on a beacon, so the key travels in the URL...
    expect(url).toBe("https://juju.example.com/api/v1/projects/1/events?juju_key=pk_abc");
    // ...and text/plain avoids the CORS preflight a beacon can't do.
    expect(blob.type).toBe("text/plain");
    const envelope = JSON.parse(await blob.text()) as Envelope;
    const errors = envelope.events.filter((e): e is JujuEvent => e.type === "error");
    expect(errors[0]?.exception.message).toBe("closing the tab");
    expect(await sentEvents()).toHaveLength(0); // nothing left for fetch to resend
  });

  it("keeps events if the browser refuses the beacon", async () => {
    navigator.sendBeacon = vi.fn(() => false) as typeof navigator.sendBeacon;
    setup({ flushIntervalMs: 60_000 });
    juju.captureException(new Error("kept"));
    hide();
    expect(await sentEvents()).toHaveLength(1);
  });

  it("retries straight away when the browser comes back online", async () => {
    let online = false;
    transport = { send: vi.fn(async (env: Envelope): Promise<SendResult> => (online ? (sent.push(env), { ok: true }) : { ok: false, retry: true })) };
    vi.useFakeTimers();
    setup();
    juju.captureException(new Error("thrown offline"));
    await vi.advanceTimersByTimeAsync(10); // first send fails, next retry in about 1 s
    online = true;
    window.dispatchEvent(new Event("online"));
    await vi.advanceTimersByTimeAsync(0);
    expect(sent).toHaveLength(1);
    vi.useRealTimers();
  });
});

describe("sessions", () => {
  const sessions = async () => (await allEvents()).filter((e): e is SessionEvent => e.type === "session");

  it("starts one when the page loads and ends it when the page goes away", async () => {
    const beacon = vi.fn((_url: string, _body: Blob) => true);
    navigator.sendBeacon = beacon as typeof navigator.sendBeacon;
    setup({ flushIntervalMs: 60_000 });
    window.dispatchEvent(new Event("pagehide"));
    // The start was still waiting, so the beacon carries just the end.
    const envelope = JSON.parse(await beacon.mock.calls[0]![1].text()) as Envelope;
    expect(envelope.events).toEqual([expect.objectContaining({ type: "session", session: expect.objectContaining({ seq: 1, status: "exited" }) })]);
  });

  // The page keeps running, but something the person did broke.
  it("an uncaught error crashes the session; a handled one doesn't", async () => {
    setup();
    juju.captureException(new Error("handled"));
    const [first] = await sessions();
    expect(first?.session).toMatchObject({ status: "ok", errors: 1 });
    window.dispatchEvent(new ErrorEvent("error", { error: new Error("uncaught"), message: "uncaught" }));
    const all = await sessions();
    expect(all[all.length - 1]?.session).toMatchObject({ sid: first?.session.sid, status: "crashed", errors: 2 });
  });

  it("can be turned off", async () => {
    setup({ sessions: false });
    juju.captureException(new Error("x"));
    expect((await allEvents()).map((e) => e.type)).toEqual(["error"]);
  });
});

describe("tracing", () => {
  const transactions = async () => (await allEvents()).filter((e): e is TransactionEvent => e.type === "transaction");

  beforeEach(() => vi.useFakeTimers({ toFake: ["setTimeout", "clearTimeout", "Date", "performance", "requestAnimationFrame"] }));
  afterEach(() => vi.useRealTimers());

  it("times the page load, with the requests made while loading", async () => {
    window.fetch = vi.fn(async () => new Response("{}", { status: 200 })) as typeof fetch;
    history.replaceState(null, "", "/orders/8812");
    setup();
    await fetch("/api/orders/8812?token=secret");
    await vi.advanceTimersByTimeAsync(2_000); // load + 1 s with no requests
    const [tx] = await transactions();
    expect(tx?.transaction).toMatchObject({ name: "/orders/:id", op: "pageload", status: "ok" });
    expect(tx?.transaction.spans).toContainEqual(expect.objectContaining({ op: "http.client", description: "GET /api/orders/8812", data: expect.objectContaining({ status_code: 200 }) }));
    expect(JSON.stringify(tx)).not.toContain("secret");
  });

  it("times single-page navigations under the route's name", async () => {
    history.replaceState(null, "", "/");
    setup({ tracing: true });
    await vi.advanceTimersByTimeAsync(2_000); // let the page load finish first
    history.pushState(null, "", "/users/3f2a9c1e-0b7d-4e8a-9c1f-2a3b4c5d6e7f/settings");
    await vi.advanceTimersByTimeAsync(2_000);
    const names = (await transactions()).map((t) => `${t.transaction.op} ${t.transaction.name}`);
    expect(names).toEqual(["pageload /", "navigation /users/:id/settings"]);
  });

  it("sends nothing when turned off", async () => {
    setup({ tracing: false });
    await vi.advanceTimersByTimeAsync(5_000);
    expect(await transactions()).toEqual([]);
  });
});

// Span names show what was requested, not the text in the code: after a
// route change "../api/x" points somewhere else.
it("span URLs are resolved against the page, without the query string", () => {
  history.replaceState(null, "", "/shop/cart/42");
  expect(shortUrl("../api/coupon?token=secret")).toBe("/shop/api/coupon");
  expect(shortUrl("https://cdn.example.com/a.js?v=1")).toBe("https://cdn.example.com/a.js");
});

describe("routeName", () => {
  it.each([
    ["/", "/"],
    ["/cart", "/cart"],
    ["/orders/8812/items", "/orders/:id/items"],
    ["/u/3f2a9c1e-0b7d-4e8a-9c1f-2a3b4c5d6e7f", "/u/:id"],
    ["/blog/how-we-grew-a-garden", "/blog/how-we-grew-a-garden"], // words, not an id
    ["/s/a8f3c9d2e1b04f7a9c3d", "/s/:id"],
  ])("%s → %s", (path, want) => expect(routeName(path)).toBe(want));
});

// Real Navigation Timing from headless Chrome, where the request sat queued
// for 158 ms after the connection was ready. The browser phases must cover
// 0 → load with no unexplained gap.
it("page load phases leave no gap, including time queued in the browser", async () => {
  const sent: Envelope[] = [];
  const c = new Client({ dsn: DSN, transport: { send: async (env) => (sent.push(env), { ok: true }) }, now: () => 1_000_000 });
  const tx = c.startTransaction({ name: "/", op: "pageload", start: 1_000_000 });
  const nav = { fetchStart: 4.4, domainLookupStart: 4.4, domainLookupEnd: 4.4, connectStart: 4.4, connectEnd: 4.4, requestStart: 162.1, responseStart: 165.7, responseEnd: 166.3, domContentLoadedEventEnd: 246.4, loadEventEnd: 247.8 } as PerformanceNavigationTiming;
  addNavigationSpans(tx, nav, (t) => 1_000_000 + t);
  tx.finish(1_000_000 + 247.8);
  await c.flush();
  const spans = (sent[0]!.events[0] as TransactionEvent).transaction.spans!;
  const ms = (iso: string) => Date.parse(iso) - 1_000_000 + Number(iso.slice(23, 26)) / 1000;
  let at = 0;
  for (const s of spans) {
    expect(ms(s.start_timestamp)).toBeCloseTo(at, 3);
    at = ms(s.timestamp);
  }
  expect(at).toBeCloseTo(247.8, 3);
  expect(spans.map((s) => s.description)).toContain("Queued in the browser");
});
