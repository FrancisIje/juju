// Live updates: the server sends "refresh" over server-sent events when a
// project's issues change (a new event arrives, a teammate resolves
// something), and the page reloads its data through the normal API.
import { useEffect, useRef } from "react";

const FALLBACK_POLL_MS = 30_000;

/**
 * Calls onRefresh whenever the project changes. The stream is open only
 * while the tab is visible: a browser allows just 6 connections per site
 * over HTTP/1.1, so background tabs each holding one would starve the rest.
 * Coming back to the tab reconnects and refreshes once, to catch up.
 * If the stream can't be opened at all, it falls back to polling.
 */
export function useLive(projectId: number | null, onRefresh: () => void) {
  const cb = useRef(onRefresh);
  cb.current = onRefresh;

  useEffect(() => {
    if (!projectId) return;
    let es: EventSource | null = null;
    let poll: ReturnType<typeof setInterval> | undefined;

    const startPolling = () => {
      poll ??= setInterval(() => cb.current(), FALLBACK_POLL_MS);
    };
    const connect = () => {
      if (typeof EventSource === "undefined") return startPolling();
      let first = true;
      const source = new EventSource(`/api/ui/projects/${projectId}/live`);
      es = source;
      source.onmessage = () => cb.current();
      // EventSource reconnects by itself after a drop; changes made while it
      // was down were missed, so refresh on every reconnect.
      source.onopen = () => {
        if (!first) cb.current();
        first = false;
      };
      source.onerror = () => {
        // CLOSED means it gave up (the server answered 404 or 401), not a drop.
        if (source.readyState === EventSource.CLOSED) {
          es = null;
          startPolling();
        }
      };
    };
    const disconnect = () => {
      es?.close();
      es = null;
      clearInterval(poll);
      poll = undefined;
    };
    const onVisibility = () => {
      if (document.visibilityState === "hidden") return disconnect();
      if (es || poll) return;
      cb.current();
      connect();
    };

    if (document.visibilityState !== "hidden") connect();
    document.addEventListener("visibilitychange", onVisibility);
    return () => {
      document.removeEventListener("visibilitychange", onVisibility);
      disconnect();
    };
  }, [projectId]);
}
