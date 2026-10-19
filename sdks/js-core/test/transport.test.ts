import { describe, expect, it, vi } from "vitest";
import { parseDsn } from "../src/dsn";
import { classify, fetchTransport, parseRetryAfter } from "../src/transport";

describe("classify: the status code contract", () => {
  it.each([
    [202, { ok: true }],
    [400, { ok: false, retry: false, status: 400 }],
    [401, { ok: false, retry: false, status: 401 }],
    [413, { ok: false, retry: false, status: 413 }],
    [500, { ok: false, retry: true, status: 500 }],
    [503, { ok: false, retry: true, status: 503 }],
    [429, { ok: false, retry: true, status: 429 }],
  ])("%i → %o", (status, want) => {
    expect(classify(status, null)).toEqual(want);
  });

  it("reads Retry-After on 429", () => {
    expect(classify(429, "5")).toEqual({ ok: false, retry: true, status: 429, retryAfterMs: 5000 });
  });
});

describe("parseRetryAfter", () => {
  it("accepts seconds and HTTP dates", () => {
    const now = Date.parse("2026-10-19T12:00:00Z");
    expect(parseRetryAfter("30", now)).toBe(30_000);
    expect(parseRetryAfter("Mon, 19 Oct 2026 12:00:10 GMT", now)).toBe(10_000);
    expect(parseRetryAfter("soon", now)).toBeUndefined();
    expect(parseRetryAfter(null, now)).toBeUndefined();
  });
});

describe("fetchTransport", () => {
  const dsn = parseDsn("https://pk_abc@juju.example.com/7");
  const envelope = { sdk: { name: "t", version: "0" }, events: [] };

  it("posts JSON with the key header to the project endpoint", async () => {
    const fetchMock = vi.fn(async () => new Response("{}", { status: 202 }));
    const result = await fetchTransport(dsn, fetchMock).send(envelope);

    expect(result).toEqual({ ok: true });
    const [url, init] = fetchMock.mock.calls[0] as unknown as [string, RequestInit];
    expect(url).toBe("https://juju.example.com/api/v1/projects/7/events");
    expect(init.method).toBe("POST");
    expect(init.headers).toMatchObject({ "Content-Type": "application/json", "X-Juju-Key": "pk_abc" });
    expect(JSON.parse(init.body as string)).toEqual(envelope);
    expect(init.keepalive).toBe(true);
  });

  it("turns off keepalive for bodies browsers would reject", async () => {
    const fetchMock = vi.fn(async () => new Response("{}", { status: 202 }));
    const big = { ...envelope, events: [{ exception: { message: "x".repeat(70_000) } }] } as never;
    await fetchTransport(dsn, fetchMock).send(big);
    expect((fetchMock.mock.calls[0] as unknown as [string, RequestInit])[1].keepalive).toBe(false);
  });

  it("treats a network failure as retryable", async () => {
    const fetchMock = vi.fn(async () => {
      throw new TypeError("Failed to fetch");
    });
    expect(await fetchTransport(dsn, fetchMock).send(envelope)).toEqual({ ok: false, retry: true });
  });
});
