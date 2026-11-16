import { createElement } from "react";
import type { FinalAnswerPreview } from "../lib/finalAnswerPreview.ts";

/** Model text is always a React text child, never HTML or Markdown. */
export function FinalAnswerText({ preview }: { preview: FinalAnswerPreview }) {
  return createElement("div", { className: "space-y-3" },
    createElement("p", { className: "max-h-96 overflow-auto whitespace-pre-wrap break-words text-sm" }, preview.text),
    preview.truncated
      ? createElement("p", { className: "text-xs text-muted-foreground" }, "Preview truncated. The complete stored output is available in the raw JSON below.")
      : null,
  );
}
