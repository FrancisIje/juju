import type { JujuEvent, QueueStorage } from "@juju/core";

/** The parts of @react-native-async-storage/async-storage we use. */
export interface AsyncStorageLike {
  getItem(key: string): Promise<string | null>;
  setItem(key: string, value: string): Promise<void>;
  removeItem(key: string): Promise<void>;
}

export const STORAGE_KEY = "@juju/queue";

/** Keeps the event queue in AsyncStorage, so it survives the app being killed. */
export function asyncStorageQueue(storage: AsyncStorageLike, key = STORAGE_KEY): QueueStorage {
  return {
    async load(): Promise<JujuEvent[]> {
      const raw = await storage.getItem(key);
      if (!raw) return [];
      try {
        const parsed: unknown = JSON.parse(raw);
        return Array.isArray(parsed) ? (parsed as JujuEvent[]) : [];
      } catch {
        return []; // a half-written value from a crash mid-write: start fresh rather than fail
      }
    },
    async save(events: JujuEvent[]): Promise<void> {
      if (events.length === 0) await storage.removeItem(key);
      else await storage.setItem(key, JSON.stringify(events));
    },
  };
}
