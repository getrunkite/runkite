import { pollRequest } from "../api/poll-request.ts";

/** A hard read budget also stops a slow final request crossing two minutes. */
export function pollReview<T>(load: (signal: AbortSignal) => Promise<T>, loaded: (value: T) => void, failed: (error: unknown) => void, poll = true): () => void {
  const stopRequests = pollRequest(load, loaded, failed, () => {}, poll ? 8000 : undefined, 120_000);
  const budget = setTimeout(stopRequests, 120_000);
  return () => { clearTimeout(budget); stopRequests(); };
}
