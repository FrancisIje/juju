import { useCallback, useEffect, useRef, useState } from "react";
import { api, ApiError } from "./api";

/**
 * Loads `path` and reloads when it changes. `refreshMs` re-fetches in the
 * background while the tab is visible, so the issue list stays live without
 * hammering the server from forgotten tabs.
 */
export function useApi<T>(path: string | null, refreshMs = 0) {
  const [data, setData] = useState<T | null>(null);
  const [error, setError] = useState<ApiError | null>(null);
  const [loading, setLoading] = useState(path !== null);
  const current = useRef(path);
  current.current = path;

  const load = useCallback(
    async (quiet = false) => {
      if (!path) return;
      if (!quiet) setLoading(true);
      try {
        const d = await api<T>(path);
        if (current.current === path) {
          setData(d);
          setError(null);
        }
      } catch (e) {
        if (current.current === path) setError(e instanceof ApiError ? e : new ApiError(String(e), 0));
      } finally {
        if (current.current === path) setLoading(false);
      }
    },
    [path],
  );

  useEffect(() => {
    void load();
  }, [load]);

  useEffect(() => {
    if (!refreshMs || !path) return;
    const id = setInterval(() => {
      if (document.visibilityState === "visible") void load(true);
    }, refreshMs);
    return () => clearInterval(id);
  }, [load, refreshMs, path]);

  return { data, error, loading, reload: load, setData };
}
