// @vitest-environment happy-dom
import type { AnalyticsEvent, Envelope, JujuEvent, SendResult, Transport } from "@juju/core";
import { afterEach, beforeEach, describe, expect, it, vi } from "vitest";
import { describeElement, redact } from "../src/breadcrumbs";
import * as juju from "../src/index";

const DSN = "https://pk_abc@juju.example.com/1";

let sent: Envelope[];
let transport: Transport;

function setup(options: Partial<juju.WebOptions> = {}) {
  juju.init({ dsn: DSN, transport, flushIntervalMs: 10, ...options });
}

async function sentEvents() {
  await juju.flush();
  return sent.flatMap((env) => env.events) as JujuEvent[]; // these tests send only errors
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
    expect((envelope.events[0] as JujuEvent | undefined)?.exception.message).toBe("closing the tab");
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

describe("analytics", () => {
  it("captures events with the browser context, and keeps the anonymous id in localStorage", async () => {
    localStorage.clear();
    setup();
    juju.capture("viewed_pricing", { plan: "pro" });
    await new Promise((r) => setTimeout(r, 0));
    await juju.flush();
    const e = sent.flatMap((env) => env.events).find((x) => x.type === "event") as AnalyticsEvent;
    expect(e).toMatchObject({ name: "viewed_pricing", properties: { plan: "pro" }, context: { browser: { user_agent: navigator.userAgent } } });
    expect(localStorage.getItem("juju.anon_id")).toBe(e.distinct_id);
  });

  it("still works when localStorage throws (private windows, blocked storage)", async () => {
    const get = vi.spyOn(Storage.prototype, "getItem").mockImplementation(() => {
      throw new Error("SecurityError");
    });
    setup();
    juju.capture("viewed_pricing");
    await new Promise((r) => setTimeout(r, 0));
    await juju.flush();
    expect(sent.flatMap((env) => env.events).some((x) => x.type === "event")).toBe(true);
    get.mockRestore();
  });
});
