import type { Client } from "@juju/core";
import { AppState, type AppStateStatus } from "react-native";

/**
 * The mobile version of the web SDK's page lifecycle handling.
 *
 * - background: the OS may kill a backgrounded app at any time, without
 *   warning. Save the queue and try to send it now.
 * - active: the user is back, and the network may be too. Retry now
 *   instead of waiting out the backoff.
 *
 * Each change is also a breadcrumb: "the app came back from the background
 * just before the crash" is often the clue.
 */
export function installAppState(client: Client, appState: typeof AppState = AppState): () => void {
  let last: AppStateStatus = appState.currentState;
  const subscription = appState.addEventListener("change", (next) => {
    if (next === last) return;
    client.addBreadcrumb({ type: "app", message: `${last} → ${next}`, data: { from: last, to: next } });
    last = next;
    if (next === "background") {
      void client.persistNow();
      void client.flush();
    } else if (next === "active") {
      client.retryNow();
    }
  });
  return () => subscription.remove();
}

/** The parts of a React Navigation container ref that we use. */
export interface NavigationRefLike {
  addListener(event: "state", callback: () => void): () => void;
  getCurrentRoute(): { name: string } | undefined;
}

/** Records screen changes from React Navigation as breadcrumbs. */
export function trackNavigation(client: Client, ref: NavigationRefLike): () => void {
  let last = ref.getCurrentRoute()?.name;
  return ref.addListener("state", () => {
    const next = ref.getCurrentRoute()?.name;
    if (!next || next === last) return;
    client.addBreadcrumb({ type: "navigation", message: `${last ?? "(start)"} → ${next}`, data: { from: last, to: next } });
    last = next;
  });
}
