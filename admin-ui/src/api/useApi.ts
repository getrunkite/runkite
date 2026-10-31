import { useEffect, useState } from "react";
import { api, ApiError } from "./client";
import { pollRequest } from "./poll-request";

interface ApiState<T> {
  data: T | null;
  error: string | null;
  loading: boolean;
  /** Opaque resume token from the last list response (X-Next-Cursor). */
  nextCursor: string | null;
  reload: () => void;
}

/** Fetches path whenever it (or any of deps) changes, exposing the same
 * loading/error/data shape every page needs -- avoids repeating the same
 * three useState calls and try/catch on every page.
 *
 * pollMs (optional): wait this interval after each completed request
 * before refreshing, without resetting `loading`/clearing `data`
 * on each tick -- a background refresh should update numbers in place,
 * not flash the whole page back to a loading state every few seconds.
 * A poll's own error is surfaced the same way a normal fetch's would be,
 * but doesn't clear previously-good `data` -- a transient blip
 * shouldn't blank out a dashboard that was just showing real numbers.
 * Navigation aborts the request and ignores any late result. */
export function useApi<T>(path: string, deps: unknown[] = [], pollMs?: number): ApiState<T> {
  const [data, setData] = useState<T | null>(null);
  const [error, setError] = useState<string | null>(null);
  const [loading, setLoading] = useState(true);
  const [nextCursor, setNextCursor] = useState<string | null>(null);
  const [reloadKey, setReloadKey] = useState(0);

  useEffect(() => {
    if (!path) {
      setLoading(false);
      setError(null);
      setData(null);
      setNextCursor(null);
      return;
    }
    setLoading(true);
    setError(null);
    return pollRequest(
      (signal) => api.getWithMeta<T>(path, signal),
      (result) => {
        setData(result.data);
        setNextCursor(result.nextCursor);
        setError(null);
      },
      (err) => setError(err instanceof ApiError ? err.message : "Request failed."),
      () => setLoading(false),
      pollMs,
    );
    // eslint-disable-next-line react-hooks/exhaustive-deps
  }, [path, reloadKey, pollMs, ...deps]);

  return {
    data,
    error,
    loading,
    nextCursor,
    reload: () => setReloadKey((k) => k + 1),
  };
}
