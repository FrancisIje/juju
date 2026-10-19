import type { Client } from "@juju/core";

/**
 * Catches errors nobody else caught.
 *
 * We use addEventListener instead of assigning window.onerror: assigning
 * would replace a handler the app already set, while listeners stack. And we
 * never call preventDefault, so the error still shows in the console. An
 * error tool must not change how the app behaves.
 */
export function installErrorHandlers(client: Client): () => void {
  const onError = (ev: Event) => {
    const e = ev as ErrorEvent;
    // Errors from cross-origin scripts arrive as "Script error." with no
    // Error object: the browser hides details from other origins. Adding
    // crossorigin="anonymous" to the <script> tag (and CORS on the CDN)
    // reveals them.
    client.captureException(e.error ?? e.message ?? "Unknown error");
  };

  // Promises that fail with no .catch(). window.onerror never sees these,
  // and they're often the most common errors in an async web app.
  const onRejection = (ev: Event) => {
    const reason = (ev as PromiseRejectionEvent).reason;
    client.captureException(reason ?? "Unhandled promise rejection with no reason");
  };

  window.addEventListener("error", onError);
  window.addEventListener("unhandledrejection", onRejection);
  return () => {
    window.removeEventListener("error", onError);
    window.removeEventListener("unhandledrejection", onRejection);
  };
}
