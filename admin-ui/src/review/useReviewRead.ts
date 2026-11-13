import { useEffect, useState } from "react";
import { pollReview } from "./pollReview";
import { reviewClient, ReviewError } from "./client";
import { useReview } from "./context";

export function useReviewRead<T>(path: string, poll = true) {
  const { revision, identity } = useReview();
  const scope = JSON.stringify([revision, path]);
  const [refresh, setRefresh] = useState(0);
  const [state, setState] = useState<{ scope: string; data: T | null; error: string | null; loading: boolean; nextCursor: string | null }>({ scope, data: null, error: null, loading: true, nextCursor: null });
  useEffect(() => {
    if (!identity) return;
    setState({ scope, data: null, error: null, loading: true, nextCursor: null });
    let stop = () => {};
    stop = pollReview(
      (signal) => reviewClient.getWithMeta<T>(path, signal),
      (result) => setState({ scope, data: result.data, error: null, loading: false, nextCursor: result.nextCursor }),
      (err) => {
        setState({ scope, data: null, error: err instanceof Error ? err.message : "Review could not be refreshed.", loading: false, nextCursor: null });
        if (err instanceof ReviewError && [400, 401, 403, 404, 501].includes(err.status)) stop();
      },
      poll,
    );
    return stop;
  }, [path, scope, identity, poll, refresh]);
  return {
    data: identity && state.scope === scope ? state.data : null,
    error: state.scope === scope ? state.error : null,
    loading: state.scope !== scope || state.loading,
    nextCursor: state.scope === scope ? state.nextCursor : null,
    reload: () => setRefresh((value) => value + 1),
  };
}
