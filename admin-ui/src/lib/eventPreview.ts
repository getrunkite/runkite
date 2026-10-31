const encoder = new TextEncoder();
const decoder = new TextDecoder();

/** Bound the debugging view without modifying the retained API event. */
export function eventPreview(data: string, limit = 16_384): string {
  if (data.length <= limit) return decoder.decode(encoder.encode(data));
  let end = limit;
  const last = data.charCodeAt(end - 1);
  const next = data.charCodeAt(end);
  if (last >= 0xd800 && last <= 0xdbff && next >= 0xdc00 && next <= 0xdfff) end--;
  // Copy the prefix so a sliced string cannot retain the full multi-MiB frame.
  const prefix = decoder.decode(encoder.encode(data.slice(0, end)));
  return `${prefix}\n[Admin preview truncated; use the run stream API for the complete event]`;
}
