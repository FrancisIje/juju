import { describe, expect, it, vi } from "vitest";
import { parseDsn } from "../src/dsn";
import { byteLength, classify, COMPRESS_MIN_BYTES, fetchTransport, gzip, parseRetryAfter } from "../src/transport";

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
    await fetchTransport(dsn, fetchMock, { compress: false }).send(big);
    expect((fetchMock.mock.calls[0] as unknown as [string, RequestInit])[1].keepalive).toBe(false);
  });

  it("measures the keepalive limit in bytes, not characters", async () => {
    const fetchMock = vi.fn(async () => new Response("{}", { status: 202 }));
    // 20,000 emoji: 40,000 string units but 80,000 bytes, over the 64 KB cap.
    const emoji = { ...envelope, events: [{ exception: { message: "😀".repeat(20_000) } }] } as never;
    await fetchTransport(dsn, fetchMock, { compress: false }).send(emoji);
    expect((fetchMock.mock.calls[0] as unknown as [string, RequestInit])[1].keepalive).toBe(false);
  });

  it("treats a network failure as retryable", async () => {
    const fetchMock = vi.fn(async () => {
      throw new TypeError("Failed to fetch");
    });
    expect(await fetchTransport(dsn, fetchMock).send(envelope)).toEqual({ ok: false, retry: true });
  });
});

describe("gzip compression", () => {
  const dsn = parseDsn("https://pk_abc@juju.example.com/7");
  const base = {
    sdk: { name: "t", version: "0" },
    events: Array.from({ length: 10 }, (_, i) => ({ event_id: String(i), exception: { type: "TypeError", message: "Cannot read properties of undefined (reading 'price')" } })),
  };
  const bigEnvelope = base as never;

  function lastInit(fetchMock: ReturnType<typeof vi.fn>) {
    return (fetchMock.mock.calls[fetchMock.mock.calls.length - 1] as unknown as [string, RequestInit])[1];
  }

  async function gunzip(bytes: Uint8Array): Promise<string> {
    const stream = new Blob([bytes]).stream().pipeThrough(new DecompressionStream("gzip"));
    return new Response(stream).text();
  }

  it("gzips bodies over 1 KB, and they decompress to the same JSON", async () => {
    const fetchMock = vi.fn(async () => new Response("{}", { status: 202 }));
    await fetchTransport(dsn, fetchMock).send(bigEnvelope);
    const init = lastInit(fetchMock);
    expect(init.headers).toMatchObject({ "Content-Encoding": "gzip", "Content-Type": "application/json" });
    const body = init.body as Uint8Array;
    expect(body.length).toBeLessThan(byteLength(JSON.stringify(bigEnvelope)));
    expect(JSON.parse(await gunzip(body))).toEqual(bigEnvelope);
  });

  it("leaves small bodies alone", async () => {
    const fetchMock = vi.fn(async () => new Response("{}", { status: 202 }));
    const small = { sdk: { name: "t", version: "0" }, events: [] };
    expect(byteLength(JSON.stringify(small))).toBeLessThan(COMPRESS_MIN_BYTES);
    await fetchTransport(dsn, fetchMock).send(small);
    expect(lastInit(fetchMock).headers).not.toHaveProperty("Content-Encoding");
    expect(typeof lastInit(fetchMock).body).toBe("string");
  });

  it("sends uncompressed in browsers without CompressionStream", async () => {
    vi.stubGlobal("CompressionStream", undefined);
    const fetchMock = vi.fn(async () => new Response("{}", { status: 202 }));
    await fetchTransport(dsn, fetchMock).send(bigEnvelope);
    expect(lastInit(fetchMock).headers).not.toHaveProperty("Content-Encoding");
    expect(JSON.parse(lastInit(fetchMock).body as string)).toEqual(bigEnvelope);
    vi.unstubAllGlobals();
  });

  it("sends uncompressed if compression throws, so no event is lost", async () => {
    vi.stubGlobal(
      "CompressionStream",
      class {
        constructor() {
          throw new Error("compression unavailable");
        }
      },
    );
    expect(await gzip("hello")).toBeNull();
    const fetchMock = vi.fn(async () => new Response("{}", { status: 202 }));
    expect(await fetchTransport(dsn, fetchMock).send(bigEnvelope)).toEqual({ ok: true });
    expect(lastInit(fetchMock).headers).not.toHaveProperty("Content-Encoding");
    vi.unstubAllGlobals();
  });

  it("can be turned off", async () => {
    const fetchMock = vi.fn(async () => new Response("{}", { status: 202 }));
    await fetchTransport(dsn, fetchMock, { compress: false }).send(bigEnvelope);
    expect(lastInit(fetchMock).headers).not.toHaveProperty("Content-Encoding");
  });

  it("lets big batches keep keepalive, because the compressed size is what counts", async () => {
    const fetchMock = vi.fn(async () => new Response("{}", { status: 202 }));
    // 100 KB of JSON that compresses to a few KB.
    const big = { ...base, events: [{ exception: { message: "x".repeat(100_000) } }] } as never;
    await fetchTransport(dsn, fetchMock).send(big);
    expect(lastInit(fetchMock).keepalive).toBe(true);
  });
});

describe("byteLength", () => {
  it("counts UTF-8 bytes the same way with or without TextEncoder", () => {
    const samples = ["", "hello", "é", "Ọ̀rọ̀ Yorùbá", "中文", "😀👍🏽", "mixed é 中 😀 text"];
    const withEncoder = samples.map(byteLength);
    vi.stubGlobal("TextEncoder", undefined); // older React Native
    try {
      expect(samples.map(byteLength)).toEqual(withEncoder);
    } finally {
      vi.unstubAllGlobals();
    }
    expect(byteLength("😀")).toBe(4);
  });
});
