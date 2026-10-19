import { KEEPALIVE_LIMIT, type Client } from "@juju/core";

/**
 * Makes sure queued events survive the page going away, and get sent as
 * soon as the network comes back.
 */
export function installLifecycle(client: Client): () => void {
  // visibilitychange to "hidden" is the last event browsers reliably fire
  // before a tab is closed, backgrounded or discarded. The older "unload"
  // event often never fires on mobile (for example when switching apps).
  const onVisibility = () => {
    if (document.visibilityState === "hidden") sendWithBeacon(client);
  };

  // The browser tells us when the network is back. Don't wait out the
  // backoff timer: send now.
  const onOnline = () => client.retryNow();

  document.addEventListener("visibilitychange", onVisibility);
  window.addEventListener("pagehide", onVisibility); // older Safari fires this instead
  window.addEventListener("online", onOnline);
  return () => {
    document.removeEventListener("visibilitychange", onVisibility);
    window.removeEventListener("pagehide", onVisibility);
    window.removeEventListener("online", onOnline);
  };
}

/**
 * sendBeacon hands the request to the browser, which delivers it even after
 * the page is gone. Its limits shape this code:
 *
 * - it can't set headers, so the key goes in the URL (?juju_key=). That's
 *   fine for the public key, which every app already exposes.
 * - a JSON content type would need a CORS preflight, which beacons can't do,
 *   so the body is sent as text/plain. The server still parses it as JSON.
 * - bodies are capped at 64 KB, so large queues go in several beacons.
 */
export function sendWithBeacon(client: Client): void {
  if (typeof navigator === "undefined" || typeof navigator.sendBeacon !== "function") return;
  const url = `${client.dsn.endpoint}?juju_key=${encodeURIComponent(client.dsn.publicKey)}`;
  client.drain((_envelope, body) => {
    try {
      return navigator.sendBeacon(url, new Blob([body], { type: "text/plain" }));
    } catch {
      return false;
    }
  }, KEEPALIVE_LIMIT);
}
