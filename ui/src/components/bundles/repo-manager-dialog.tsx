"use client";

import { useState } from "react";
import { SearchField } from "@/components/core/search-field";
import {
  Dialog,
  DialogContent,
  DialogDescription,
  DialogHeader,
  DialogTitle,
} from "@/components/ui/dialog";
import { Button } from "@/components/ui/button";
import { cn } from "@/lib/utils";

type Filter = "all" | "unused" | "problem";
type Sort = "name" | "used";

/** What the dialog needs to know about a row to count and filter it. */
export type RepoFacts = { repo: string; last_used?: string; status?: string };

// Configure, on a bundle whose every credential is a repository — and Manage repositories, on a
// channel. The five-tab bundle editor is the right shell for a bundle of services: connect a
// credential, list the domains, write the instructions. It is the wrong one for thirty
// repositories, where its Credentials tab offers to connect ClickUp and what you came to do is
// find one repository among the twenty a token opened.
//
// The shell is the same on both pages — the same search, the same three counts, the same Add
// button — and the list inside it is the page's own, over the rows the filter left. A group
// header in that list therefore takes what is showing rather than quietly picking up the rows
// the filter hid.
export function RepoManagerDialog<T>({
  open,
  onOpenChange,
  title,
  description,
  rows,
  facts,
  add,
  empty,
  children,
}: {
  open: boolean;
  onOpenChange: (open: boolean) => void;
  title: string;
  description: string;
  rows: T[];
  facts: (row: T) => RepoFacts;
  /** The Add button and its picker, handed the dialog to portal into: a modal blocks pointer
   * events outside itself, so a popover portalled to the body would open and be unclickable. */
  add?: (container: HTMLElement | null) => React.ReactNode;
  /** Shown in place of the list when there is nothing here at all. */
  empty: React.ReactNode;
  children: (shown: T[]) => React.ReactNode;
}) {
  const [query, setQuery] = useState("");
  const [filter, setFilter] = useState<Filter>("all");
  const [sort, setSort] = useState<Sort>("name");
  const [surface, setSurface] = useState<HTMLElement | null>(null);

  const unused = rows.filter((r) => !facts(r).last_used);
  const problem = rows.filter((r) => (facts(r).status || "active") !== "active");
  const shown = rows
    .filter((r) => {
      const f = facts(r);
      if (filter === "unused" && f.last_used) return false;
      if (filter === "problem" && (f.status || "active") === "active") return false;
      return f.repo.toLowerCase().includes(query.trim().toLowerCase());
    })
    // The list renders what it is given, so the order is decided here. Newest use first puts
    // the repositories that earn their place at the top and the dead weight at the bottom,
    // which is the only way a bundle of thirty stays honest.
    .sort((a, b) =>
      sort === "name"
        ? facts(a).repo.localeCompare(facts(b).repo)
        : (facts(b).last_used ?? "").localeCompare(facts(a).last_used ?? ""),
    );

  const tab = (value: Filter, label: string) => (
    <button
      key={value}
      type="button"
      aria-pressed={filter === value}
      onClick={() => setFilter(value)}
      className={cn(
        "h-7 rounded-md border px-2.5 text-xs whitespace-nowrap",
        filter === value
          ? "border-primary/30 bg-accent font-medium text-accent-foreground"
          : "text-muted-foreground hover:bg-secondary/60",
      )}
    >
      {label}
    </button>
  );

  return (
    <Dialog open={open} onOpenChange={onOpenChange}>
      <DialogContent ref={setSurface} className="flex max-h-[85vh] flex-col gap-0 p-0 sm:max-w-3xl">
        {/* pr-12: the dialog's own ✕ is absolutely positioned at top-4 right-4, and without
            the gutter the Add button slides under it. */}
        <DialogHeader className="border-b p-4 pr-12 pb-3">
          <div className="flex flex-wrap items-start justify-between gap-2">
            <div className="min-w-0">
              <DialogTitle>{title}</DialogTitle>
              <DialogDescription>{description}</DialogDescription>
            </div>
            {add?.(surface)}
          </div>
        </DialogHeader>

        {rows.length > 0 && (
          <div className="flex flex-wrap items-center gap-2 border-b px-4 py-2.5">
            <SearchField
              value={query}
              onChange={setQuery}
              placeholder="Filter repositories…"
              className="min-w-40 flex-1"
            />
            {tab("all", `All ${rows.length}`)}
            {unused.length > 0 && tab("unused", `Never used ${unused.length}`)}
            {problem.length > 0 && tab("problem", `Needs attention ${problem.length}`)}
            <button
              type="button"
              onClick={() => setSort((v) => (v === "name" ? "used" : "name"))}
              className="h-7 rounded-md border px-2.5 text-xs whitespace-nowrap text-muted-foreground hover:bg-secondary/60"
            >
              {sort === "name" ? "Sort: name" : "Sort: last used"}
            </button>
          </div>
        )}

        <div className="min-h-0 flex-1 overflow-y-auto p-0">
          {rows.length === 0 ? (
            empty
          ) : shown.length === 0 ? (
            <p className="px-4 py-10 text-center text-sm text-muted-foreground">
              Nothing matches. Clear the filter to see all {rows.length}.
            </p>
          ) : (
            children(shown)
          )}
        </div>

        <div className="flex items-center justify-end border-t p-3">
          <Button variant="outline" size="sm" onClick={() => onOpenChange(false)}>
            Done
          </Button>
        </div>
      </DialogContent>
    </Dialog>
  );
}
