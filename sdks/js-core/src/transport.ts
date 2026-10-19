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

export function fetchTransport(dsn: Dsn, fetchImpl: typeof fetch = globalThis.fetch.bind(globalThis)): Transport {
  return {
    async send(envelope) {
      const body = JSON.stringify(envelope);
      try {
        const res = await fetchImpl(dsn.endpoint, {
          method: "POST",
          headers: { "Content-Type": "application/json", "X-Juju-Key": dsn.publicKey },
          body,
          // keepalive lets the request finish even if the page is closing,
          // but browsers reject keepalive bodies over 64 KB outright. Using
          // it on a big batch would fail every time and retry forever.
          keepalive: body.length < KEEPALIVE_LIMIT,
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
