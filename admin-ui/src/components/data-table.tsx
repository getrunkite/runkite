import {
  flexRender,
  getCoreRowModel,
  getSortedRowModel,
  useReactTable,
  type ColumnDef,
  type SortingState,
} from "@tanstack/react-table";
import { useState } from "react";
import { ArrowDown, ArrowUp, ChevronsUpDown } from "lucide-react";
import { cn } from "../lib/utils";
import { Table, TableBody, TableCell, TableHead, TableHeader, TableRow } from "./ui/table";
import { TableSkeleton } from "./common";

interface DataTableProps<TData> {
  columns: ColumnDef<TData, unknown>[];
  data: TData[];
  /** Row-key extractor -- every list page's data has a natural unique
   * id (run_id, thread_id, agent_id+tenant, etc.), so this is required
   * rather than falling back to array index, which would break
   * TanStack's row identity across sorts/re-fetches. */
  getRowId: (row: TData) => string;
  onRowClick?: (row: TData) => void;
  loading?: boolean;
  /** Default sort, e.g. [{ id: "updated_at", desc: true }] -- most of
   * this app's lists read best most-recent-first out of the box, with
   * every column still independently re-sortable by clicking it. */
  initialSorting?: SortingState;
}

/** A real, sortable table (TanStack Table v8) wired to this app's own
 * Table/TableHead/TableRow/TableCell primitives -- every list page
 * (Runs, Threads, Agents, Registry, Cron, Connectors, Webhooks) uses
 * this instead of a hand-rolled <table>.map(), so "sortable" is an
 * actual feature and not just a claim: click any header to cycle
 * asc -> desc -> unsorted, with a chevron indicating the current state. */
export function DataTable<TData>({
  columns,
  data,
  getRowId,
  onRowClick,
  loading,
  initialSorting,
}: DataTableProps<TData>) {
  const [sorting, setSorting] = useState<SortingState>(initialSorting ?? []);

  const table = useReactTable({
    data,
    columns,
    state: { sorting },
    onSortingChange: setSorting,
    getCoreRowModel: getCoreRowModel(),
    getSortedRowModel: getSortedRowModel(),
    getRowId,
  });

  return (
    <Table>
      <TableHeader>
        {table.getHeaderGroups().map((headerGroup) => (
          <TableRow key={headerGroup.id} className="hover:bg-transparent">
            {headerGroup.headers.map((header) => {
              const sortable = header.column.getCanSort();
              const sortState = header.column.getIsSorted();
              return (
                <TableHead key={header.id}>
                  {header.isPlaceholder ? null : sortable ? (
                    <button
                      type="button"
                      onClick={header.column.getToggleSortingHandler()}
                      className="flex items-center gap-1 select-none hover:text-foreground"
                    >
                      {flexRender(header.column.columnDef.header, header.getContext())}
                      {sortState === "asc" && <ArrowUp className="size-3" />}
                      {sortState === "desc" && <ArrowDown className="size-3" />}
                      {!sortState && <ChevronsUpDown className="size-3 opacity-40" />}
                    </button>
                  ) : (
                    flexRender(header.column.columnDef.header, header.getContext())
                  )}
                </TableHead>
              );
            })}
          </TableRow>
        ))}
      </TableHeader>
      <TableBody>
        {loading && data.length === 0 && <TableSkeleton columns={columns.length} />}
        {table.getRowModel().rows.map((row) => (
          <TableRow
            key={row.id}
            onClick={onRowClick ? () => onRowClick(row.original) : undefined}
            className={cn(onRowClick && "cursor-pointer")}
          >
            {row.getVisibleCells().map((cell) => (
              <TableCell key={cell.id}>{flexRender(cell.column.columnDef.cell, cell.getContext())}</TableCell>
            ))}
          </TableRow>
        ))}
      </TableBody>
    </Table>
  );
}
