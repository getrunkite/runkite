/** Poll after each completed request, so a slow response cannot overwrite a newer one. */
export function pollRequest<T>(
  load: (signal: AbortSignal) => Promise<T>,
  loaded: (value: T) => void,
  failed: (error: unknown) => void,
  settled: () => void,
  pollMs?: number,
): () => void {
  const controller = new AbortController();
  let stopped = false;
  let timer: ReturnType<typeof setTimeout> | undefined;

  async function tick() {
    try {
      const value = await load(controller.signal);
      if (!stopped) loaded(value);
    } catch (error) {
      if (!stopped) failed(error);
    } finally {
      if (!stopped) {
        settled();
        if (pollMs && pollMs > 0) timer = setTimeout(() => void tick(), pollMs);
      }
    }
  }

  void tick();
  return () => {
    stopped = true;
    if (timer !== undefined) clearTimeout(timer);
    controller.abort();
  };
}
