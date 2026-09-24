"use client";

import { useId, useMemo, useState } from "react";
import { defaultFilter, useCommandState } from "cmdk";
import { Check, ChevronsUpDown, Globe, PencilLine } from "lucide-react";
import {
  Command,
  CommandGroup,
  CommandInput,
  CommandItem,
  CommandList,
} from "@/components/ui/command";
import { Popover, PopoverContent, PopoverTrigger } from "@/components/ui/popover";
import { browserZone } from "@/lib/format";
import { cn } from "@/lib/utils";

// ---- zone list ----
//
// The browser knows every IANA zone, so nothing is fetched. Offsets are
// computed for "now", which is what someone choosing a zone wants to see
// (daylight time included), and cached for the page's lifetime.

type Zone = { id: string; offset: string; keywords: string[] };

/** "UTC+5:45", "UTC-8", "UTC"; null when the browser rejects the id. */
export function offsetOf(id: string): string | null {
  try {
    const part = new Intl.DateTimeFormat("en-US", { timeZone: id, timeZoneName: "shortOffset" })
      .formatToParts(new Date())
      .find((p) => p.type === "timeZoneName")?.value;
    return part ? part.replace(/^GMT/, "UTC") : null;
  } catch {
    return null;
  }
}

let cached: Zone[] | null = null;

function allZones(): Zone[] {
  if (cached) return cached;
  let ids: string[] = [];
  try {
    ids = Intl.supportedValuesOf("timeZone");
  } catch {
    ids = [];
  }
  if (!ids.includes("UTC")) ids.push("UTC");
  cached = ids
    .map((id) => {
      const [region, ...rest] = id.split("/");
      const city = rest.join("/").replace(/_/g, " ");
      return {
        id,
        offset: offsetOf(id) ?? "",
        // "New York" and "Asia" are how people search; the offset lets "+5:45" work.
        keywords: [city, region, offsetOf(id) ?? ""].filter(Boolean),
      };
    })
    .sort((a, b) => a.id.localeCompare(b.id));
  return cached;
}

const EMPTY = "__empty__";
const CUSTOM = "__custom__";

// Same arrangement as the model picker: the "as typed" row scores zero so it
// sorts last among matches, and shows alone when nothing matches.
const filter: typeof defaultFilter = (value, search, keywords) =>
  value === CUSTOM ? 0 : defaultFilter(value, search, keywords);

function WhenNothingMatches({ children }: { children: React.ReactNode }) {
  const show = useCommandState((s) => s.search.trim() !== "" && s.filtered.count === 0);
  return show ? <CommandGroup forceMount>{children}</CommandGroup> : null;
}

// The in-group copy of that row: hidden with its group when nothing matches,
// so the row above is the only one on screen then.
function WhenSomethingMatches({ children }: { children: React.ReactNode }) {
  const show = useCommandState((s) => s.filtered.count > 0);
  return show ? <>{children}</> : null;
}

/**
 * An IANA time zone field: a searchable dropdown of every zone the browser
 * knows, with its current UTC offset, plus the option to type a name the list
 * doesn't carry. Renders like an `Input` so it drops into the same rows.
 */
export function TimezoneCombobox({
  id,
  value,
  onChange,
  emptyLabel,
  disabled,
  className,
  container,
  "aria-label": ariaLabel,
}: {
  id?: string;
  value: string;
  onChange: (value: string) => void;
  /** What an empty value means, e.g. "Workspace default". Omit when a zone is required. */
  emptyLabel?: string;
  disabled?: boolean;
  className?: string;
  /** Portal target; pass the dialog's content element when used inside a modal (see PopoverContent). */
  container?: React.ComponentProps<typeof PopoverContent>["container"];
  "aria-label"?: string;
}) {
  const [open, setOpen] = useState(false);
  const [query, setQuery] = useState("");
  const listId = useId();

  // Built on first open, not on mount: the settings page shouldn't pay for
  // four hundred offset computations before anyone clicks.
  const zones = useMemo(() => (open ? allZones() : []), [open]);
  const mine = useMemo(() => (open ? browserZone() : ""), [open]);
  const currentOffset = value ? offsetOf(value) : null;
  const typed = query.trim();
  const exact = typed !== "" && zones.some((z) => z.id === typed);

  const pick = (next: string) => {
    onChange(next);
    setOpen(false);
    setQuery("");
  };

  const asTyped = typed !== "" && !exact && (
    <CommandItem value={CUSTOM} forceMount onSelect={() => pick(typed)}>
      <PencilLine className="size-4 text-muted-foreground" />
      <span className="min-w-0 flex-1 truncate">
        Use <span className="font-mono">{typed}</span>
      </span>
      <span className="text-xs text-muted-foreground">as typed</span>
    </CommandItem>
  );

  return (
    <Popover
      open={open}
      onOpenChange={(next) => {
        setOpen(next);
        if (!next) setQuery("");
      }}
    >
      <PopoverTrigger asChild>
        <button
          type="button"
          id={id}
          role="combobox"
          aria-expanded={open}
          aria-controls={listId}
          aria-label={ariaLabel}
          disabled={disabled}
          className={cn(
            "flex h-9 w-full min-w-0 items-center justify-between gap-2 rounded-md border border-input bg-transparent px-3 py-1 text-left text-sm shadow-xs transition-[color,box-shadow] outline-none",
            "focus-visible:border-ring focus-visible:ring-[3px] focus-visible:ring-ring/50",
            "disabled:pointer-events-none disabled:cursor-not-allowed disabled:opacity-50 dark:bg-input/30 dark:hover:bg-input/50",
            className,
          )}
        >
          {value === "" ? (
            <span className="truncate text-muted-foreground">{emptyLabel ?? "Choose a time zone"}</span>
          ) : (
            <span className="flex min-w-0 items-baseline gap-2">
              <span className="max-w-full shrink-0 truncate">{value}</span>
              {currentOffset ? (
                <span className="text-xs tabular-nums text-muted-foreground">{currentOffset}</span>
              ) : (
                <span className="text-xs text-danger">not a known zone</span>
              )}
            </span>
          )}
          <ChevronsUpDown className="size-4 shrink-0 opacity-50" />
        </button>
      </PopoverTrigger>
      <PopoverContent className="w-[max(var(--radix-popover-trigger-width),20rem)] p-0" align="start" container={container}>
        <Command filter={filter}>
          <CommandInput placeholder="Search by city, region or offset…" value={query} onValueChange={setQuery} />
          <CommandList id={listId} className="max-h-72">
            {asTyped && <WhenNothingMatches>{asTyped}</WhenNothingMatches>}

            {(emptyLabel || mine) && (
              <CommandGroup heading="Defaults">
                {emptyLabel && (
                  <CommandItem value={EMPTY} keywords={[emptyLabel, "default"]} onSelect={() => pick("")}>
                    <span className="min-w-0 flex-1 truncate">{emptyLabel}</span>
                    {value === "" && <Check className="size-4" />}
                  </CommandItem>
                )}
                {mine && (
                  <CommandItem
                    value={`browser:${mine}`}
                    keywords={["browser", mine.replace(/_/g, " ")]}
                    onSelect={() => pick(mine)}
                  >
                    <Globe className="size-4 text-muted-foreground" />
                    <span className="min-w-0 flex-1 truncate">
                      Your browser&apos;s zone <span className="text-muted-foreground">· {mine}</span>
                    </span>
                    <span className="text-xs tabular-nums text-muted-foreground">{offsetOf(mine)}</span>
                  </CommandItem>
                )}
              </CommandGroup>
            )}

            <CommandGroup heading="Time zones">
              {zones.map((z) => (
                <CommandItem key={z.id} value={z.id} keywords={z.keywords} onSelect={() => pick(z.id)}>
                  <span className="min-w-0 flex-1 truncate">{z.id}</span>
                  <span className="shrink-0 text-xs tabular-nums text-muted-foreground">{z.offset}</span>
                  {value === z.id && <Check className="size-4" />}
                </CommandItem>
              ))}
              {asTyped && <WhenSomethingMatches>{asTyped}</WhenSomethingMatches>}
            </CommandGroup>
          </CommandList>
          <div className="border-t px-3 py-1.5 text-[11px] text-muted-foreground">
            IANA names. Offsets are for today, daylight time included.
          </div>
        </Command>
      </PopoverContent>
    </Popover>
  );
}
