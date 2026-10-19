import type { Dsn } from "./dsn";
import type { Envelope } from "./types";

// The status code contract with the server (see server/internal/ingest):
//
//   2xx                  stored: delete the events
//   400 401 413 415 ...  broken: a retry can never succeed, so delete them
//   429                  busy: keep them, wait at least Retry-After
//   5xx, network error   temporary: keep them, retry with backoff

export type SendResult =
  | { ok: true }
  | { ok: false; retry: false; status: number }
  | { ok: false; retry: true; status?: number; retryAfterMs?: number };

export interface Transport {
  send(envelope: Envelope): Promise<SendResult>;
}

export function classify(status: number, retryAfter: string | null, now = Date.now()): SendResult {
  if (status >= 200 && status < 300) return { ok: true };
  if (status === 429 || status >= 500) {
    const result: SendResult = { ok: false, retry: true, status };
    const ms = parseRetryAfter(retryAfter, now);
    if (ms !== undefined) result.retryAfterMs = ms;
    return result;
  }
  return { ok: false, retry: false, status };
}

/** Retry-After is either a number of seconds or an HTTP date. */
export function parseRetryAfter(value: string | null, now = Date.now()): number | undefined {
  if (!value) return undefined;
  if (/^\d+$/.test(value.trim())) return Number(value) * 1000;
  const date = Date.parse(value);
  return Number.isNaN(date) ? undefined : Math.max(0, date - now);
}

/** Bytes. Kept under the browsers' 64 KB keepalive/sendBeacon cap with some room to spare. */
export const KEEPALIVE_LIMIT = 60_000;

/** Bodies smaller than this are sent as-is: compressing them costs more CPU than it saves. */
export const COMPRESS_MIN_BYTES = 1024;

/**
 * Size in bytes as sent over the network (UTF-8). This is not the same as
 * string.length, which counts UTF-16 code units: "é" is 1 unit but 2 bytes,
 * and an emoji is 2 units but 4 bytes. Browser limits are in bytes.
 */
export function byteLength(s: string): number {
  return new TextEncoder().encode(s).length;
}

/**
 * Gzips text with the browser's built-in CompressionStream. Returns null when
 * the API is missing (older browsers) or anything goes wrong, so the caller
 * can send uncompressed: a failed optimization must never cost an event.
 */
export async function gzip(text: string): Promise<Uint8Array | null> {
  if (typeof CompressionStream === "undefined") return null;
  try {
    const stream = new Blob([text]).stream().pipeThrough(new CompressionStream("gzip"));
    return new Uint8Array(await new Response(stream).arrayBuffer());
  } catch {
    return null;
  }
}

export interface FetchTransportOptions {
  /** Gzip bodies over COMPRESS_MIN_BYTES when the browser supports it. Default true. */
  compress?: boolean;
}

export function fetchTransport(
  dsn: Dsn,
  fetchImpl: typeof fetch = globalThis.fetch.bind(globalThis),
  options: FetchTransportOptions = {},
): Transport {
  const compress = options.compress ?? true;
  return {
    async send(envelope) {
      const json = JSON.stringify(envelope);
      const headers: Record<string, string> = { "Content-Type": "application/json", "X-Juju-Key": dsn.publicKey };
      let body: string | Uint8Array = json;
      let size = byteLength(json);

      // Only this path compresses. The sendBeacon path can't: compression is
      // asynchronous, and a beacon must be handed over in the same moment
      // the page is closing, before it's gone.
      if (compress && size >= COMPRESS_MIN_BYTES) {
        const gz = await gzip(json);
        if (gz && gz.length < size) {
          body = gz;
          size = gz.length;
          headers["Content-Encoding"] = "gzip"; // the server decompresses, within its 1 MB limit
        }
      }

      try {
        const res = await fetchImpl(dsn.endpoint, {
          method: "POST",
          headers,
          body,
          // keepalive lets the request finish even if the page is closing,
          // but browsers reject keepalive bodies over 64 KB outright. Using
          // it on a big batch would fail every time and retry forever.
          // Compression helps here: more batches fit under the limit.
          keepalive: size < KEEPALIVE_LIMIT,
        });
        return classify(res.status, res.headers.get("Retry-After"));
      } catch {
        // Offline, DNS failure, connection refused, CORS rejection: fetch
        // rejects with no status. All of these are worth retrying.
        return { ok: false, retry: true };
      }
    },
  };
}
