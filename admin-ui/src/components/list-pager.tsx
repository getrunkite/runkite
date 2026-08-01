import { ChevronLeft, ChevronRight } from "lucide-react";
import { Button } from "./ui/button";

/** Prev/Next for Admin list endpoints that return a bare array.
 * has-more when the current page filled `limit` (Agent Protocol convention). */
export function ListPager({
  offset,
  limit,
  pageCount,
  onChange,
}: {
  offset: number;
  limit: number;
  pageCount: number;
  onChange: (offset: number) => void;
}) {
  const hasPrev = offset > 0;
  const hasNext = pageCount >= limit;
  if (!hasPrev && !hasNext) return null;

  const page = Math.floor(offset / limit) + 1;
  return (
    <div className="mt-4 flex items-center justify-between gap-3">
      <p className="text-sm text-muted-foreground">
        Page {page}
        {pageCount > 0 ? ` · ${pageCount} on this page` : ""}
      </p>
      <div className="flex items-center gap-2">
        <Button type="button" variant="outline" size="sm" disabled={!hasPrev} onClick={() => onChange(Math.max(0, offset - limit))}>
          <ChevronLeft />
          Prev
        </Button>
        <Button type="button" variant="outline" size="sm" disabled={!hasNext} onClick={() => onChange(offset + limit)}>
          Next
          <ChevronRight />
        </Button>
      </div>
    </div>
  );
}

export const ADMIN_PAGE_SIZE = 50;
