import { useEffect, useState, useSyncExternalStore, type ReactNode } from "react";
import { reviewClient } from "./client";
import { ReviewContext } from "./context";

export function ReviewProvider({ children }: { children: ReactNode }) {
  const snapshot = useSyncExternalStore(reviewClient.subscribe, reviewClient.snapshot);
  const [error, setError] = useState<string | null>(null);
  const [retry, setRetry] = useState(0);
  useEffect(() => {
    const controller = new AbortController();
    setError(null);
    void reviewClient.status(controller.signal).catch((err: unknown) => {
      if (!controller.signal.aborted) {
        setError(err instanceof Error ? err.message : "Review authentication is unavailable.");
        reviewClient.expire();
      }
    });
    return () => controller.abort();
  }, [retry]);
  useEffect(() => {
    if (!snapshot.identity) return;
    const remaining = Math.min(15 * 60_000, Date.parse(snapshot.identity.expires_at) - Date.now());
    const timer = setTimeout(reviewClient.expire, Math.max(0, remaining));
    return () => clearTimeout(timer);
  }, [snapshot.identity]);
  return <ReviewContext.Provider value={{ ...snapshot, error, retry: () => setRetry((value) => value + 1) }}>{children}</ReviewContext.Provider>;
}
