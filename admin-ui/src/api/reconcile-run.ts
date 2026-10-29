/** Reconcile terminal SSE against the durable record, which may commit later. */
export function reconcileRun(
  refresh: () => void,
  isTerminal: () => boolean,
): () => void {
  const delays = [250, 500, 1000, 2000, 4000];
  let timer: ReturnType<typeof setTimeout> | undefined;
  let stopped = false;
  let attempt = 0;
  const tick = () => {
    if (stopped) return;
    refresh();
    if (attempt < delays.length) {
      timer = setTimeout(() => {
        if (!stopped && !isTerminal()) tick();
      }, delays[attempt++]);
    }
  };
  tick();
  return () => {
    stopped = true;
    if (timer !== undefined) clearTimeout(timer);
  };
}
