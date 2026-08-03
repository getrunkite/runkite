import { ChevronLeft, ChevronRight } from "lucide-react";
import { Button } from "./ui/button";

/** Prev/Next for Admin list endpoints that page via opaque ?cursor=
 * tokens (X-Next-Cursor). Callers keep a stack of cursors used to reach
 * the current page (empty = first page). */
export function ListPager({
  pageIndex,
  pageCount,
  hasNext,
  onPrev,
  onNext,
}: {
  pageIndex: number;
  pageCount: number;
  hasNext: boolean;
  onPrev: () => void;
  onNext: () => void;
}) {
  const hasPrev = pageIndex > 0;
  if (!hasPrev && !hasNext) return null;

  return (
    <div className="mt-4 flex items-center justify-between gap-3">
      <p className="text-sm text-muted-foreground">
        Page {pageIndex + 1}
        {pageCount > 0 ? ` · ${pageCount} on this page` : ""}
      </p>
      <div className="flex items-center gap-2">
        <Button type="button" variant="outline" size="sm" disabled={!hasPrev} onClick={onPrev}>
          <ChevronLeft />
          Prev
        </Button>
        <Button type="button" variant="outline" size="sm" disabled={!hasNext} onClick={onNext}>
          Next
          <ChevronRight />
        </Button>
      </div>
    </div>
  );
}

export const ADMIN_PAGE_SIZE = 50;

/** Build an Admin list path with limit + optional cursor (no offset). */
export function adminListPath(base: string, cursor: string | undefined, extra?: Record<string, string>): string {
  const params = new URLSearchParams({ limit: String(ADMIN_PAGE_SIZE), ...extra });
  if (cursor) params.set("cursor", cursor);
  const q = params.toString();
  return q ? `${base}?${q}` : base;
}
