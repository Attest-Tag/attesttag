"use client";

import { useEffect, useId, useState, useSyncExternalStore } from "react";
import { defaultFilter, useCommandState } from "cmdk";
import { Check, ChevronsUpDown, Loader2, PencilLine, RefreshCw, X } from "lucide-react";
import { Checkbox } from "@/components/ui/checkbox";
import {
  Command,
  CommandGroup,
  CommandInput,
  CommandItem,
  CommandList,
} from "@/components/ui/command";
import { Popover, PopoverContent, PopoverTrigger } from "@/components/ui/popover";
import { api, errorMessage, type ModelInfo, type ModelsResponse } from "@/lib/api";
import { cn } from "@/lib/utils";

// ---- shared model list ----
//
// Every picker on a page reads the same list, so it lives in one module-level
// store rather than per component: the settings page has four pickers and
// should fetch once. The backend caches the provider call as well.

type ModelsState = {
  models: ModelInfo[];
  source: string;
  /** Whether a model missing from `models` is one the endpoint does not serve. */
  complete?: boolean;
  loading: boolean;
  error?: string;
};

let state: ModelsState = { models: [], source: "", loading: false };
let loaded = false;
let inflight: Promise<void> | null = null;
const listeners = new Set<() => void>();

function emit(next: ModelsState) {
  state = next;
  listeners.forEach((l) => l());
}

function subscribe(listener: () => void) {
  listeners.add(listener);
  return () => {
    listeners.delete(listener);
  };
}

/** Fetch the provider's models once; `refresh` asks the backend to skip its cache too. */
export function loadModels(refresh = false): Promise<void> {
  if (inflight) return inflight;
  if (loaded && !refresh) return Promise.resolve();
  emit({ ...state, loading: true, error: undefined });
  inflight = api
    .get<ModelsResponse>(refresh ? "/api/models?refresh=1" : "/api/models")
    .then(
      (res) => {
        loaded = true;
        emit({ models: res.models ?? [], source: res.source, complete: res.complete, loading: false });
      },
      (err: unknown) => {
        emit({ ...state, loading: false, error: errorMessage(err) });
      },
    )
    .finally(() => {
      inflight = null;
    });
  return inflight;
}

export function useModels(): ModelsState {
  const snapshot = useSyncExternalStore(subscribe, () => state, () => state);
  useEffect(() => {
    void loadModels();
  }, []);
  return snapshot;
}

// ---- display helpers ----

/** "128k", "1M" — context windows read better rounded than exact. */
function formatContext(n: number): string {
  if (n >= 1_000_000) return `${Math.round(n / 100_000) / 10}M`.replace(".0M", "M");
  if (n >= 1000) return `${Math.round(n / 1024)}k`;
  return String(n);
}

/** "$0.40 / $1.60" per million tokens in and out; "free" when both are zero. */
function formatPrice(m: ModelInfo): string | null {
  if (m.prompt_per_m === undefined) return null;
  const out = m.completion_per_m ?? 0;
  if (m.prompt_per_m === 0 && out === 0) return "free";
  const fmt = (v: number) => (v >= 10 ? `$${v.toFixed(0)}` : v >= 1 ? `$${v.toFixed(1)}` : `$${v.toFixed(2)}`);
  return m.kind === "embedding" || m.completion_per_m === undefined
    ? fmt(m.prompt_per_m)
    : `${fmt(m.prompt_per_m)} / ${fmt(out)}`;
}

function hostOf(url: string): string {
  try {
    return new URL(url).host;
  } catch {
    return url;
  }
}

const EMPTY = "__empty__";
const CUSTOM = "__custom__";

// cmdk orders the items of a group by match score, and the "as typed" row
// would otherwise score highest (it contains the whole query) and steal Enter
// from the best real match. Scoring it zero keeps it mounted (forceMount) but
// last. It has to live inside the models group for that: cmdk only reorders
// groups that hold a matching item, so a zero-score group of its own would
// never move from wherever it was rendered.
const filter: typeof defaultFilter = (value, search, keywords) =>
  value === CUSTOM ? 0 : defaultFilter(value, search, keywords);

// When the query matches nothing, cmdk hides every group, the models group
// and the "as typed" row inside it included. This shows that row on its own
// then, so a completely unknown id can still be entered.
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

export type ModelOption = { value: string; label: string; hint?: string };

/**
 * A model id field backed by the provider's model list: a searchable dropdown
 * of what the endpoint actually serves, with the option to type any id the
 * list doesn't know. Renders like an `Input` so it drops into the same rows.
 */
export function ModelCombobox({
  id,
  value,
  onChange,
  kind = "chat",
  emptyLabel,
  options = [],
  disabled,
  className,
  container,
  "aria-label": ariaLabel,
}: {
  id?: string;
  value: string;
  onChange: (value: string) => void;
  /** Which part of the list to offer; anything can still be typed. */
  kind?: "chat" | "embedding";
  /** What an empty value means, e.g. "Same as model". Omit when a model is required. */
  emptyLabel?: string;
  /** Fixed choices ahead of the provider list, such as "heavy". */
  options?: ModelOption[];
  disabled?: boolean;
  className?: string;
  /** Portal target; pass the dialog's content element when used inside a modal (see PopoverContent). */
  container?: React.ComponentProps<typeof PopoverContent>["container"];
  "aria-label"?: string;
}) {
  const [open, setOpen] = useState(false);
  const [query, setQuery] = useState("");
  const listId = useId();
  const { models, source, complete, loading, error } = useModels();

  const listed = models.filter((m) => m.kind === kind);
  const known = models.find((m) => m.id === value);
  // A pick made for another endpoint — before the organisation brought its own key, or before it
  // removed one — stays saved until somebody changes it, and the bot quietly uses the endpoint's
  // default meanwhile. Marked only where the list is complete: Azure lists base models while its
  // requests name deployments, so there a missing id means nothing.
  const unserved = value !== "" && !known && !!complete && listed.length > 0 && !loading && !options.some((o) => o.value === value);
  const fixed = options.find((o) => o.value === value);
  const typed = query.trim();
  const exact = typed !== "" && (listed.some((m) => m.id === typed) || options.some((o) => o.value === typed));

  const pick = (next: string) => {
    onChange(next);
    setOpen(false);
    setQuery("");
  };

  // Shown while typing, so an id the list doesn't carry (a new release, a
  // private deployment) is one click or an arrow-up-and-Enter away.
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
            <span className="truncate text-muted-foreground">{emptyLabel ?? "Choose a model"}</span>
          ) : fixed ? (
            <span className="truncate">
              {fixed.label}
              {fixed.hint && <span className="text-muted-foreground"> · {fixed.hint}</span>}
            </span>
          ) : (
            <span className="flex min-w-0 items-baseline gap-2">
              <span className="max-w-full shrink-0 truncate font-mono">{value}</span>
              {known?.name && (
                <span className="hidden min-w-0 truncate text-xs text-muted-foreground sm:inline">{known.name}</span>
              )}
              {unserved && (
                <span className="min-w-0 truncate text-xs text-warning" title={`Not served by ${hostOf(source)}: the default model answers instead. Pick another, or clear it.`}>
                  not on {hostOf(source)}
                </span>
              )}
            </span>
          )}
          <ChevronsUpDown className="size-4 shrink-0 opacity-50" />
        </button>
      </PopoverTrigger>
      <PopoverContent className="w-[max(var(--radix-popover-trigger-width),24rem)] p-0" align="start" container={container}>
        <Command filter={filter}>
          <CommandInput
            placeholder="Search models, or type an id…"
            value={query}
            onValueChange={setQuery}
          />
          <CommandList id={listId} className="max-h-72">
            {asTyped && listed.length > 0 && <WhenNothingMatches>{asTyped}</WhenNothingMatches>}
            {(emptyLabel || options.length > 0) && (
              <CommandGroup heading="Defaults">
                {emptyLabel && (
                  <CommandItem value={EMPTY} keywords={[emptyLabel, "default", "none"]} onSelect={() => pick("")}>
                    <span className="min-w-0 flex-1 truncate">{emptyLabel}</span>
                    {value === "" && <Check className="size-4" />}
                  </CommandItem>
                )}
                {options.map((o) => (
                  <CommandItem key={o.value} value={o.value} keywords={[o.label]} onSelect={() => pick(o.value)}>
                    <span className="min-w-0 flex-1 truncate">
                      {o.label}
                      {o.hint && <span className="text-muted-foreground"> · {o.hint}</span>}
                    </span>
                    {value === o.value && <Check className="size-4" />}
                  </CommandItem>
                ))}
              </CommandGroup>
            )}

            {listed.length === 0 ? (
              <>
                {asTyped && <CommandGroup forceMount>{asTyped}</CommandGroup>}
                {loading ? (
                  <div className="flex items-center justify-center gap-2 py-6 text-sm text-muted-foreground">
                    <Loader2 className="size-4 animate-spin" />
                    Loading models…
                  </div>
                ) : error ? (
                  <div className="space-y-1 px-3 py-4 text-xs">
                    <p className="text-danger">Couldn&apos;t load the model list: {error}</p>
                    <p className="text-muted-foreground">Type a model id above to use it anyway.</p>
                  </div>
                ) : (
                  <p className="px-3 py-4 text-xs text-muted-foreground">
                    {hostOf(source)} lists no {kind} models. Type a model id above to use it anyway.
                  </p>
                )}
              </>
            ) : (
              <CommandGroup heading={`${kind === "embedding" ? "Embedding models" : "Models"} on ${hostOf(source)}`}>
                {listed.map((m) => {
                  const price = formatPrice(m);
                  return (
                    <CommandItem
                      key={m.id}
                      value={m.id}
                      keywords={m.name ? [m.name] : undefined}
                      onSelect={() => pick(m.id)}
                    >
                      <span className="min-w-0 flex-1">
                        <span className="block truncate font-mono text-xs">{m.id}</span>
                        {m.name && <span className="block truncate text-xs text-muted-foreground">{m.name}</span>}
                      </span>
                      {(m.context_length || price) && (
                        <span className="shrink-0 text-right text-[11px] tabular-nums text-muted-foreground">
                          {m.context_length ? <span className="block">{formatContext(m.context_length)}</span> : null}
                          {price && <span className="block">{price}</span>}
                        </span>
                      )}
                      {value === m.id && <Check className="size-4" />}
                    </CommandItem>
                  );
                })}
                {asTyped && <WhenSomethingMatches>{asTyped}</WhenSomethingMatches>}
              </CommandGroup>
            )}
          </CommandList>
          <div className="flex items-center justify-between gap-2 border-t px-3 py-1.5 text-[11px] text-muted-foreground">
            <span className="truncate">
              {listed.length > 0
                ? `${listed.length} ${kind === "embedding" ? "embedding " : ""}model${listed.length === 1 ? "" : "s"}${
                    kind === "chat" ? " · prices per 1M tokens in / out" : ""
                  }`
                : source
                  ? hostOf(source)
                  : ""}
            </span>
            <button
              type="button"
              className="flex shrink-0 items-center gap-1 rounded-sm px-1 py-0.5 hover:bg-accent hover:text-accent-foreground disabled:opacity-50"
              disabled={loading}
              onClick={() => void loadModels(true)}
              aria-label="Refresh the model list"
            >
              <RefreshCw className={cn("size-3", loading && "animate-spin")} />
              Refresh
            </button>
          </div>
        </Command>
      </PopoverContent>
    </Popover>
  );
}

/**
 * The same list, picking several. Used for the short menu of models a channel may choose
 * between: the trigger shows what is picked, the list toggles rather than closing, and an id the
 * provider doesn't list can still be typed in.
 *
 * `value` is the stored comma-separated string rather than an array, so it drops into the
 * settings form beside every other field, which is a `Record<SettingKey, string>`.
 */
export function ModelMultiCombobox({
  id,
  value,
  onChange,
  kind = "chat",
  max = 12,
  disabled,
  className,
  container,
  "aria-label": ariaLabel,
}: {
  id?: string;
  value: string;
  onChange: (value: string) => void;
  kind?: "chat" | "embedding";
  max?: number;
  disabled?: boolean;
  className?: string;
  container?: React.ComponentProps<typeof PopoverContent>["container"];
  "aria-label"?: string;
}) {
  const [open, setOpen] = useState(false);
  const [query, setQuery] = useState("");
  const listId = useId();
  const { models, source, loading, error } = useModels();

  const picked = value.split(",").map((m) => m.trim()).filter(Boolean);
  const listed = models.filter((m) => m.kind === kind);
  const typed = query.trim();
  const exact = typed !== "" && listed.some((m) => m.id === typed);
  const full = picked.length >= max;

  const toggle = (m: string) => {
    const next = picked.includes(m) ? picked.filter((p) => p !== m) : full ? picked : [...picked, m];
    onChange(next.join(","));
    setQuery("");
  };

  const asTyped = typed !== "" && !exact && !picked.includes(typed) && (
    <CommandItem value={CUSTOM} forceMount onSelect={() => toggle(typed)}>
      <PencilLine className="size-4 text-muted-foreground" />
      <span className="min-w-0 flex-1 truncate">
        Add <span className="font-mono">{typed}</span>
      </span>
      <span className="text-xs text-muted-foreground">as typed</span>
    </CommandItem>
  );

  return (
    <div className={cn("space-y-2", className)}>
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
            )}
          >
            {picked.length === 0 ? (
              <span className="truncate text-muted-foreground">Default and Advanced only</span>
            ) : (
              <span className="truncate">
                {picked.length} model{picked.length === 1 ? "" : "s"} offered
              </span>
            )}
            <ChevronsUpDown className="size-4 shrink-0 opacity-50" />
          </button>
        </PopoverTrigger>
        <PopoverContent
          className="w-[max(var(--radix-popover-trigger-width),24rem)] p-0"
          align="start"
          container={container}
        >
          <Command filter={filter}>
            <CommandInput
              placeholder="Search models, or type an id…"
              value={query}
              onValueChange={setQuery}
            />
            <CommandList id={listId} className="max-h-72">
              {asTyped && listed.length > 0 && <WhenNothingMatches>{asTyped}</WhenNothingMatches>}
              {listed.length === 0 ? (
                <>
                  {asTyped && <CommandGroup forceMount>{asTyped}</CommandGroup>}
                  {loading ? (
                    <div className="flex items-center justify-center gap-2 py-6 text-sm text-muted-foreground">
                      <Loader2 className="size-4 animate-spin" />
                      Loading models…
                    </div>
                  ) : error ? (
                    <div className="space-y-1 px-3 py-4 text-xs">
                      <p className="text-danger">Couldn&apos;t load the model list: {error}</p>
                      <p className="text-muted-foreground">Type a model id above to add it anyway.</p>
                    </div>
                  ) : (
                    <p className="px-3 py-4 text-xs text-muted-foreground">
                      {hostOf(source)} lists no {kind} models. Type a model id above to add it anyway.
                    </p>
                  )}
                </>
              ) : (
                <CommandGroup heading={`Models on ${hostOf(source)}`}>
                  {listed.map((m) => {
                    const on = picked.includes(m.id);
                    return (
                      <CommandItem
                        key={m.id}
                        value={m.id}
                        keywords={m.name ? [m.name] : undefined}
                        disabled={full && !on}
                        onSelect={() => toggle(m.id)}
                      >
                        {/* A box, like every other list you tick several things in. The single
                            pickers above keep a bare tick: there the tick says which one is in
                            force, and a box would offer a choice the field cannot hold. */}
                        <Checkbox checked={on} className="pointer-events-none" />
                        <span className="min-w-0 flex-1">
                          <span className="block truncate font-mono text-xs">{m.id}</span>
                          {m.name && <span className="block truncate text-xs text-muted-foreground">{m.name}</span>}
                        </span>
                      </CommandItem>
                    );
                  })}
                  {asTyped && <WhenSomethingMatches>{asTyped}</WhenSomethingMatches>}
                </CommandGroup>
              )}
            </CommandList>
            <div className="flex items-center justify-between gap-2 border-t px-3 py-1.5 text-[11px] text-muted-foreground">
              <span className="truncate">
                {picked.length} of {max} chosen{full ? " — the most a channel menu should hold" : ""}
              </span>
              <button
                type="button"
                className="flex shrink-0 items-center gap-1 rounded-sm px-1 py-0.5 hover:bg-accent hover:text-accent-foreground disabled:opacity-50"
                disabled={loading}
                onClick={() => void loadModels(true)}
                aria-label="Refresh the model list"
              >
                <RefreshCw className={cn("size-3", loading && "animate-spin")} />
                Refresh
              </button>
            </div>
          </Command>
        </PopoverContent>
      </Popover>

      {/* The picks, in the order a channel will see them, each removable. Chips rather than a
          summary line because this is a list somebody curates, not a single value they set. */}
      {picked.length > 0 && (
        <ul className="flex flex-wrap gap-1.5">
          {picked.map((m) => (
            <li key={m}>
              <button
                type="button"
                disabled={disabled}
                onClick={() => toggle(m)}
                className="flex items-center gap-1.5 rounded-md border bg-muted/40 py-0.5 pr-1.5 pl-2 font-mono text-xs hover:bg-muted disabled:pointer-events-none disabled:opacity-50"
                aria-label={`Remove ${m}`}
              >
                {m}
                <X className="size-3 opacity-60" />
              </button>
            </li>
          ))}
        </ul>
      )}
    </div>
  );
}
