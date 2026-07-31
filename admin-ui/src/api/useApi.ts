import { useEffect, useState } from "react";
import { api, ApiError } from "./client";

interface ApiState<T> {
  data: T | null;
  error: string | null;
  loading: boolean;
  reload: () => void;
}

/** Fetches path whenever it (or any of deps) changes, exposing the same
 * loading/error/data shape every page needs -- avoids repeating the same
 * three useState calls and try/catch on every page.
 *
 * pollMs (optional): when set, re-fetches on that interval AFTER the
 * first successful load, without resetting `loading`/clearing `data`
 * on each tick -- a background refresh should update numbers in place,
 * not flash the whole page back to a loading state every few seconds.
 * A poll's own error is surfaced the same way a normal fetch's would be,
 * but doesn't clear previously-good `data` -- a transient blip
 * shouldn't blank out a dashboard that was just showing real numbers. */
export function useApi<T>(path: string, deps: unknown[] = [], pollMs?: number): ApiState<T> {
  const [data, setData] = useState<T | null>(null);
  const [error, setError] = useState<string | null>(null);
  const [loading, setLoading] = useState(true);
  const [reloadKey, setReloadKey] = useState(0);

  useEffect(() => {
    let cancelled = false;
    let isFirstLoad = true;

    function fetchOnce() {
      if (isFirstLoad) {
        setLoading(true);
        setError(null);
      }
      api
        .get<T>(path)
        .then((result) => {
          if (!cancelled) {
            setData(result);
            setError(null);
          }
        })
        .catch((err) => {
          if (!cancelled) setError(err instanceof ApiError ? err.message : "Request failed.");
        })
        .finally(() => {
          if (!cancelled) setLoading(false);
          isFirstLoad = false;
        });
    }

    fetchOnce();
    const interval = pollMs ? setInterval(fetchOnce, pollMs) : undefined;
    return () => {
      cancelled = true;
      if (interval) clearInterval(interval);
    };
    // eslint-disable-next-line react-hooks/exhaustive-deps
  }, [path, reloadKey, pollMs, ...deps]);

  return { data, error, loading, reload: () => setReloadKey((k) => k + 1) };
}
