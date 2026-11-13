import { useEffect, useState } from "react";
import { api, ApiError } from "./client";
import { pollRequest } from "./poll-request";
import { evidenceForPath } from "../lib/connectorActions";

interface EvidenceState<T> {
  path: string;
  data: T | null;
  error: string | null;
  loading: boolean;
  nextCursor: string | null;
}

/** Read-only refreshes are serialized, abort on navigation and stop after 2m. */
export function useConnectorEvidence<T>(path: string, active = true) {
  const [state, setState] = useState<EvidenceState<T>>({
    path,
    data: null,
    error: null,
    loading: true,
    nextCursor: null,
  });
  const [refresh, setRefresh] = useState(0);
  useEffect(() => {
    setState((old) =>
      old.path === path
        ? { ...old, loading: true }
        : { path, data: null, error: null, loading: true, nextCursor: null },
    );
    let stop = () => {};
    stop = pollRequest(
      (signal) => api.getWithMeta<T>(path, AbortSignal.any([signal, AbortSignal.timeout(10_000)])),
      (result) => setState({ path, data: result.data, error: null, loading: false, nextCursor: result.nextCursor }),
      (error) => {
        setState((old) => ({
          ...old,
          path,
          loading: false,
          error:
            error instanceof ApiError && error.status === 501
              ? "Connector action history requires a SQL state backend (Postgres, MySQL or SQLite)."
              : error instanceof ApiError
                ? error.message
                : "Could not refresh connector action history.",
        }));
        if (error instanceof ApiError && [400, 401, 403, 404, 501].includes(error.status)) stop();
      },
      () => setState((old) => ({ ...old, loading: false })),
      active ? 8000 : undefined,
      120_000,
    );
    return stop;
  }, [path, active, refresh]);
  return {
    data: evidenceForPath(state, path),
    error: state.path === path ? state.error : null,
    loading: state.path !== path || state.loading,
    nextCursor: state.path === path ? state.nextCursor : null,
    reload: () => setRefresh((value) => value + 1),
  };
}
