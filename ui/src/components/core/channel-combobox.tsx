"use client";

import { useId, useState } from "react";
import { defaultFilter, useCommandState } from "cmdk";
import { Check, ChevronsUpDown, Hash, Loader2, Lock, PencilLine } from "lucide-react";
import {
  Command,
  CommandGroup,
  CommandInput,
  CommandItem,
  CommandList,
} from "@/components/ui/command";
import { Popover, PopoverContent, PopoverTrigger } from "@/components/ui/popover";
import { useApi, type Scope } from "@/lib/api";
import { cn } from "@/lib/utils";

const EMPTY = "__empty__";
const CUSTOM = "__custom__";

// Same arrangement as the model and time zone pickers: the "as typed" row scores zero so it
// sorts last among matches, and shows alone when nothing matches.
const filter: typeof defaultFilter = (value, search, keywords) =>
  value === CUSTOM ? 0 : defaultFilter(value, search, keywords);

function WhenNothingMatches({ children }: { children: React.ReactNode }) {
  const show = useCommandState((s) => s.search.trim() !== "" && s.filtered.count === 0);
  return show ? <CommandGroup forceMount>{children}</CommandGroup> : null;
}

function WhenSomethingMatches({ children }: { children: React.ReactNode }) {
  const show = useCommandState((s) => s.filtered.count > 0);
  return show ? <>{children}</> : null;
}

/**
 * A Slack channel field: a searchable dropdown of the channels the bot is actually in, grouped
 * by workspace, with the option to type an id the list doesn't carry. The stored value is still
 * the channel id — that is what Slack takes and what the alert path looks up — so this only
 * changes how one is chosen. Renders like an `Input` so it drops into the same rows.
 */
export function ChannelCombobox({
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
  /** What an empty value means, e.g. "No alerts". Omit when a channel is required. */
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
  const scopes = useApi<Scope[]>("/api/scopes?sync=0");

  const channels = (scopes.data ?? []).filter((s) => s.kind === "channel");
  const chosen = channels.find((c) => c.slack_id === value);
  // One heading per workspace, but only when there is more than one to tell apart.
  const teams = Array.from(new Set(channels.map((c) => c.team_id)));
  const typed = query.trim();
  const exact = typed !== "" && channels.some((c) => c.slack_id === typed);

  const pick = (next: string) => {
    onChange(next);
    setOpen(false);
    setQuery("");
  };

  // An id can still be pasted: a channel the bot was invited to a minute ago is not on the list
  // until the next sync, and refusing it here would be a step back from the plain field.
  const asTyped = typed !== "" && !exact && (
    <CommandItem value={CUSTOM} forceMount onSelect={() => pick(typed)}>
      <PencilLine className="size-4 text-muted-foreground" />
      <span className="min-w-0 flex-1 truncate">
        Use <span className="font-mono">{typed}</span>
      </span>
      <span className="text-xs text-muted-foreground">as typed</span>
    </CommandItem>
  );

  const row = (c: Scope) => (
    <CommandItem
      key={c.id}
      value={c.slack_id}
      keywords={[c.name, c.team_name].filter(Boolean)}
      onSelect={() => pick(c.slack_id)}
    >
      {c.is_private ? (
        <Lock className="size-3.5 text-muted-foreground" />
      ) : (
        <Hash className="size-3.5 text-muted-foreground" />
      )}
      <span className="min-w-0 flex-1 truncate">{c.name}</span>
      <span className="shrink-0 font-mono text-[11px] text-muted-foreground">{c.slack_id}</span>
      {value === c.slack_id && <Check className="size-4" />}
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
            <span className="truncate text-muted-foreground">{emptyLabel ?? "Choose a channel"}</span>
          ) : chosen ? (
            <span className="flex min-w-0 items-baseline gap-2">
              <span className="max-w-full shrink-0 truncate">{chosen.name}</span>
              <span className="hidden min-w-0 truncate font-mono text-xs text-muted-foreground sm:inline">
                {chosen.slack_id}
              </span>
            </span>
          ) : (
            <span className="flex min-w-0 items-baseline gap-2">
              <span className="max-w-full shrink-0 truncate font-mono">{value}</span>
              {/* An id outside every connected workspace is a channel nothing can post to, which
                  is worth saying here rather than only in the server's log. */}
              {!scopes.loading && scopes.data && (
                <span className="text-xs text-danger">not a channel the bot is in</span>
              )}
            </span>
          )}
          <ChevronsUpDown className="size-4 shrink-0 opacity-50" />
        </button>
      </PopoverTrigger>
      <PopoverContent
        className="w-[max(var(--radix-popover-trigger-width),22rem)] p-0"
        align="start"
        container={container}
      >
        <Command filter={filter}>
          <CommandInput
            placeholder="Search channels, or paste an id…"
            value={query}
            onValueChange={setQuery}
          />
          <CommandList id={listId} className="max-h-72">
            {asTyped && channels.length > 0 && <WhenNothingMatches>{asTyped}</WhenNothingMatches>}

            {emptyLabel && (
              <CommandGroup heading="Off">
                <CommandItem value={EMPTY} keywords={[emptyLabel, "none", "off"]} onSelect={() => pick("")}>
                  <span className="min-w-0 flex-1 truncate">{emptyLabel}</span>
                  {value === "" && <Check className="size-4" />}
                </CommandItem>
              </CommandGroup>
            )}

            {channels.length === 0 ? (
              <>
                {asTyped && <CommandGroup forceMount>{asTyped}</CommandGroup>}
                {scopes.loading ? (
                  <div className="flex items-center justify-center gap-2 py-6 text-sm text-muted-foreground">
                    <Loader2 className="size-4 animate-spin" />
                    Loading channels…
                  </div>
                ) : (
                  <p className="px-3 py-4 text-xs text-muted-foreground">
                    The bot is not in any channel yet. Invite it to one in Slack, or paste a channel
                    id above.
                  </p>
                )}
              </>
            ) : teams.length > 1 ? (
              teams.map((team) => {
                const mine = channels.filter((c) => c.team_id === team);
                return (
                  <CommandGroup key={team} heading={mine[0]?.team_name || team}>
                    {mine.map(row)}
                  </CommandGroup>
                );
              })
            ) : (
              <CommandGroup heading="Channels">{channels.map(row)}</CommandGroup>
            )}
            {asTyped && channels.length > 0 && (
              <WhenSomethingMatches>
                <CommandGroup forceMount>{asTyped}</CommandGroup>
              </WhenSomethingMatches>
            )}
          </CommandList>
          <div className="border-t px-3 py-1.5 text-[11px] text-muted-foreground">
            Channels the bot has been invited to. Invite it to one in Slack to see it here.
          </div>
        </Command>
      </PopoverContent>
    </Popover>
  );
}
