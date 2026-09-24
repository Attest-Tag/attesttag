"use client";

import { useMemo } from "react";
import { ChevronDown, ChevronUp, ChevronsUpDown } from "lucide-react";
import { TableHead } from "@/components/ui/table";
import { useStickyTab } from "@/hooks/use-sticky-tab";
import { cn } from "@/lib/utils";

export type SortDir = "asc" | "desc";
export type Sort<K extends string> = { by: K; dir: SortDir };

/** What a column holds, which is what "sort it" means and which way it should start. */
export type ColumnKinds<K extends string> = Record<K, "text" | "number">;

/**
 * A table's sort order, remembered per browser.
 *
 * It is stored rather than held in state for the reason the folds and the starred channels are:
 * somebody who sorts a table by what they care about means it, and having to say so again on
 * every visit is the console forgetting something it was told. Nothing about it is worth a
 * column on the server — it is how one person reads one screen.
 *
 * A new column starts in the direction that answers the question it was clicked for: a count or
 * an amount opens biggest-first, a name opens A to Z. Clicking the column that is already
 * sorting flips it.
 */
export function useTableSort<K extends string>(
  storageKey: string,
  columns: ColumnKinds<K>,
  fallback: Sort<K>,
) {
  const keys = useMemo(() => Object.keys(columns) as K[], [columns]);
  const allowed = useMemo(
    () => keys.flatMap((k) => [`${k}:asc`, `${k}:desc`]),
    [keys],
  );
  const [stored, setStored] = useStickyTab(storageKey, `${fallback.by}:${fallback.dir}`, allowed);

  const [by, dir] = stored.split(":") as [K, SortDir];
  const sort: Sort<K> = keys.includes(by) ? { by, dir } : fallback;

  const toggle = (next: K) => {
    if (next === sort.by) {
      setStored(`${next}:${sort.dir === "asc" ? "desc" : "asc"}`);
      return;
    }
    setStored(`${next}:${columns[next] === "number" ? "desc" : "asc"}`);
  };

  return { sort, toggle } as const;
}

/**
 * Rows in the order `sort` asks for, as a new array — the caller's is left alone, because it is
 * usually the response object a re-render would otherwise re-sort in place.
 *
 * `read` gives the value for a column. Numbers compare as numbers and everything else compares
 * as text in the reader's locale, so "#alpha" and "#Beta" land where a reader expects rather
 * than where their code points fall.
 */
export function sortRows<T, K extends string>(
  rows: readonly T[],
  sort: Sort<K>,
  read: (row: T, by: K) => number | string,
): T[] {
  const sign = sort.dir === "asc" ? 1 : -1;
  return [...rows].sort((a, b) => {
    const left = read(a, sort.by);
    const right = read(b, sort.by);
    if (typeof left === "number" && typeof right === "number") return (left - right) * sign;
    return String(left).localeCompare(String(right), undefined, { sensitivity: "base" }) * sign;
  });
}

/**
 * A column header that sorts. The label is a button inside the `th`, which is what lets it be
 * reached by keyboard and announced as a control; `aria-sort` on the `th` is what tells a screen
 * reader that the table is ordered and by which column.
 */
export function SortHead<K extends string>({
  by,
  sort,
  onSort,
  align = "left",
  className,
  children,
}: {
  by: K;
  sort: Sort<K>;
  onSort: (by: K) => void;
  align?: "left" | "right";
  className?: string;
  children: React.ReactNode;
}) {
  const active = sort.by === by;
  const Icon = !active ? ChevronsUpDown : sort.dir === "asc" ? ChevronUp : ChevronDown;
  return (
    <TableHead
      aria-sort={active ? (sort.dir === "asc" ? "ascending" : "descending") : "none"}
      className={cn(align === "right" && "text-right", className)}
    >
      <button
        type="button"
        onClick={() => onSort(by)}
        className={cn(
          // A button opts out of the inherited text-transform the header sets, so the micro-caps
          // are repeated here rather than left to TableHead.
          "group flex w-full items-center gap-1 rounded-sm py-0.5 text-[11px] font-medium tracking-[0.04em] uppercase transition-colors hover:text-foreground focus-visible:ring-[2px] focus-visible:ring-ring/50 focus-visible:outline-none",
          active ? "text-foreground" : "text-muted-foreground",
          align === "right" && "justify-end",
        )}
      >
        {children}
        <Icon
          className={cn(
            "size-3 shrink-0 transition-opacity",
            active ? "opacity-100" : "opacity-0 group-hover:opacity-60 group-focus-visible:opacity-60",
          )}
        />
      </button>
    </TableHead>
  );
}
