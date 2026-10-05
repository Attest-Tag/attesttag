"use client";

import { useRef, useState } from "react";
import { Check, ChevronsUpDown, Plus } from "lucide-react";
import { AllowRulesEditor } from "@/components/core/allow-rules-editor";
import { ChannelCombobox } from "@/components/core/channel-combobox";
import { ChipsInput, type ChipsHandle } from "@/components/core/chips-input";
import { StatusChip } from "@/components/core/status-chip";
import { Button } from "@/components/ui/button";
import { Checkbox } from "@/components/ui/checkbox";
import {
  Command,
  CommandEmpty,
  CommandGroup,
  CommandInput,
  CommandItem,
  CommandList,
} from "@/components/ui/command";
import { Input } from "@/components/ui/input";
import { Popover, PopoverContent, PopoverTrigger } from "@/components/ui/popover";
import { Select, SelectContent, SelectItem, SelectTrigger, SelectValue } from "@/components/ui/select";
import type { ReviewEffective, ReviewNotifyEvent, ReviewSettingsValues, Scope } from "@/lib/api";
import { cn } from "@/lib/utils";
import {
  NOTIFY_EVENTS,
  channelLabel,
  inheritedEntries,
  notifyEventsLabel,
  rulesAnnounce,
  sourceLabel,
  type Ancestor,
  type Choice,
  type ListField,
} from "@/components/reviews/review-format";

// The pieces every field of Reviews › Settings is built from. Each one says two things beside its
// control: what the level would have with nothing set here and where that comes from ("Inherit:
// High · from Frontend"), or that the value is set here, with the Reset that hands it back. Lists
// add up down the tree instead, so they show what arrives from above, credited level by level,
// above the entries this level adds.

/** Radix Select cannot carry an empty value, so "inherit" rides on a sentinel, as on Workspaces. */
export const INHERIT = "__inherit";

/** What a field needs to read and save its level's own value. */
export type FieldEnv = {
  idPrefix: string;
  own: ReviewSettingsValues;
  /** What the level would resolve to with nothing set on it. */
  inherited: ReviewEffective;
  ancestors: Ancestor[];
  /** reviews.manage: every field. Without it the panel reads, disabled. */
  canManage: boolean;
  /** connections.manage on top: posting live, every push, forks, context, the model, money and the channel. */
  canReach: boolean;
  /** The field being saved, so its control holds still until the answer lands. */
  busy: string | null;
  /** Sets one field of the level (undefined resets it) and saves that field alone, the rest kept as stored. */
  save: <K extends keyof ReviewSettingsValues>(field: K, value: ReviewSettingsValues[K] | undefined) => Promise<boolean>;
};

export const REACH_REASON = "Needs Manage connections as well";

/**
 * A field's draft, which starts from what the level stores and follows it when a save or a reset
 * changes it. Followed here rather than by keying the field on the stored value: a remount after
 * Enter replaced the input under the keyboard user's focus, which then fell to the page and the
 * next Tab started from the top. Adjusted during the render that sees the change, as React advises
 * for state derived from props, so the old draft never paints.
 */
export function useStoredDraft<T>(stored: T): [T, (next: T) => void] {
  const [draft, setDraft] = useState(stored);
  const [seen, setSeen] = useState(stored);
  if (JSON.stringify(seen) !== JSON.stringify(stored)) {
    setSeen(stored);
    setDraft(stored);
  }
  return [draft, setDraft];
}

/**
 * The line under a control: "Inherit: High · from Frontend", or "Set here · Reset to High · from
 * Frontend". Reset comes after the control in the DOM, so tabbing out of the field lands on it
 * rather than skipping past it to the next field.
 */
export function InheritLine({
  env,
  field,
  shown,
  blocked,
}: {
  env: FieldEnv;
  field: keyof ReviewSettingsValues;
  /** The inherited value as the reader should see it. */
  shown: string;
  /** Going back to the inherited value needs more than this person holds; the field says why. */
  blocked?: boolean;
}) {
  const from = sourceLabel(env.inherited.source?.[field], env.ancestors);
  if (env.own[field] === undefined) {
    return (
      <p className="text-xs text-muted-foreground">
        Inherit: <span className="font-medium text-foreground">{shown}</span> · {from}
      </p>
    );
  }
  return (
    <p className="flex flex-wrap items-center gap-x-1.5 text-xs text-muted-foreground">
      <span className="font-medium text-foreground">Set here</span>
      {env.canManage && !blocked ? (
        <>
          <span aria-hidden>·</span>
          <button
            type="button"
            disabled={env.busy !== null}
            onClick={() => void env.save(field, undefined)}
            className="font-medium text-primary underline-offset-2 hover:underline disabled:opacity-50"
          >
            Reset
          </button>
          <span>
            to {shown} · {from}
          </span>
        </>
      ) : (
        <span>
          · inherits {shown} {from === "built-in default" ? "by default" : from}
        </span>
      )}
    </p>
  );
}

/**
 * A single-valued field as a Select whose first entry is Inherit. `reach` lists the values only
 * connections.manage may choose — Live, every push — which stay in the list, disabled, so the
 * reader can see they exist and why they cannot pick them.
 */
export function ChoiceField<T extends string>({
  env,
  field,
  label,
  choices,
  own,
  inherited,
  onPick,
  reach = [],
  reachAll = false,
}: {
  env: FieldEnv;
  field: keyof ReviewSettingsValues;
  label: string;
  choices: Choice<T>[];
  own: T | undefined;
  inherited: T;
  onPick: (value: T | undefined) => void;
  /** Values that need connections.manage. */
  reach?: T[];
  /** The whole field needs connections.manage. */
  reachAll?: boolean;
}) {
  const id = `${env.idPrefix}-${field}`;
  const blockedAll = reachAll && !env.canReach;
  // Going back to an inherited Live is turning Live on here, which the server judges the same
  // way, so Inherit is held back exactly when choosing Live would be.
  const inheritBlocked = !env.canReach && reach.includes(inherited) && own !== inherited;
  const effective = own ?? inherited;
  const hint = choices.find((c) => c.value === effective)?.hint;
  const inheritedLabel = choices.find((c) => c.value === inherited)?.label ?? inherited;
  return (
    <div className="space-y-1.5">
      <Select
        value={own ?? INHERIT}
        // A pick while the last one saves is ignored rather than the control disabled: Radix hands
        // the focus back to the trigger as the list closes, and a disabled trigger drops it.
        onValueChange={(v) => env.busy !== field && onPick(v === INHERIT ? undefined : (v as T))}
        disabled={!env.canManage || blockedAll}
      >
        <SelectTrigger id={id} aria-label={label} className="w-full max-w-sm">
          <SelectValue />
        </SelectTrigger>
        <SelectContent>
          <SelectItem value={INHERIT} disabled={inheritBlocked}>
            Inherit ({inheritedLabel})
          </SelectItem>
          {choices.map((c) => (
            <SelectItem key={c.value} value={c.value} disabled={!env.canReach && reach.includes(c.value)}>
              {c.label}
              {/* Said in the list, not on the closed field: a value already set here reads as itself. */}
              {!env.canReach && reach.includes(c.value) && own !== c.value ? " — needs Manage connections" : ""}
            </SelectItem>
          ))}
        </SelectContent>
      </Select>
      {hint && <p className="text-xs text-muted-foreground">{hint}</p>}
      <InheritLine env={env} field={field} shown={inheritedLabel} blocked={inheritBlocked || blockedAll} />
      {env.canManage && blockedAll ? (
        <p className="text-xs text-warning">{REACH_REASON} to change this.</p>
      ) : env.canManage && inheritBlocked && own !== undefined ? (
        <p className="text-xs text-warning">Going back to the inherited {inheritedLabel} needs Manage connections as well.</p>
      ) : null}
    </div>
  );
}

/**
 * A number or a line of text, set with its own Save so Enter saves it. Empty and saved is Reset:
 * the box with nothing in it already reads as "inherit", from its placeholder.
 */
export function TextValueField({
  env,
  field,
  label,
  stored,
  shown,
  placeholder,
  type = "text",
  min,
  max,
  step,
  prefix,
  reachAll = false,
  parse,
  emptyIsValue = false,
  className,
}: {
  env: FieldEnv;
  field: keyof ReviewSettingsValues;
  label: string;
  /** The level's own value as text; "" when it inherits. */
  stored: string;
  /** The inherited value as the reader should see it. */
  shown: string;
  placeholder: string;
  type?: "text" | "number";
  min?: number;
  max?: number;
  step?: string;
  prefix?: string;
  reachAll?: boolean;
  /** Text to the value saved; null refuses it. */
  parse: (raw: string) => string | number | null;
  /**
   * An empty box saves an empty value rather than resetting: the comment header, where "" turns
   * off a header set further up.
   */
  emptyIsValue?: boolean;
  className?: string;
}) {
  const id = `${env.idPrefix}-${field}`;
  const isSet = env.own[field] !== undefined;
  const [draft, setDraft] = useStoredDraft(stored);
  const blocked = reachAll && !env.canReach;
  // Held still while it saves by being read-only, not disabled: a disabled input drops the focus
  // Enter left in it.
  const saving = env.busy === field;
  const disabled = !env.canManage || blocked || saving;
  const trimmed = draft.trim();
  const parsed = trimmed === "" ? null : parse(trimmed);
  const invalid = trimmed !== "" && parsed === null;
  // An empty box on a level that sets nothing is no change — except where empty is a value of its
  // own and the level inherits something else: saving it then turns the header set above off.
  const inherited = env.inherited[field as keyof ReviewEffective];
  const emptyTurnsOff = emptyIsValue && typeof inherited === "string" && inherited !== "";
  const unchanged = isSet ? draft === stored : trimmed === "" && !emptyTurnsOff;

  // One field of several shapes, so the save is called through its untyped side: what parse
  // returns is already the shape this field stores.
  const put = env.save as (f: keyof ReviewSettingsValues, v: unknown) => Promise<boolean>;
  const submit = async (e: React.FormEvent) => {
    e.preventDefault();
    if (disabled || invalid || unchanged) return;
    await put(field, trimmed === "" ? (emptyIsValue ? "" : undefined) : parsed);
  };

  return (
    <div className="space-y-1.5">
      <form onSubmit={submit} className="flex gap-2">
        <div className={cn("relative w-full", className ?? "max-w-[12rem]")}>
          {prefix && (
            <span className="pointer-events-none absolute left-3 top-1/2 -translate-y-1/2 text-sm text-muted-foreground">
              {prefix}
            </span>
          )}
          <Input
            id={id}
            aria-label={label}
            aria-invalid={invalid}
            type={type}
            min={min}
            max={max}
            step={step}
            inputMode={type === "number" ? "decimal" : undefined}
            value={draft}
            disabled={!env.canManage || blocked}
            readOnly={saving}
            placeholder={placeholder}
            onChange={(e) => setDraft(e.target.value)}
            className={cn("h-8", prefix && "pl-7", type === "number" && "tabular-nums")}
          />
        </div>
        <Button type="submit" size="sm" variant="outline" className="h-8" disabled={disabled || invalid || unchanged}>
          Save
        </Button>
      </form>
      {invalid && (
        <p className="text-xs text-danger">
          {type === "number" ? `Between ${prefix ?? ""}${min} and ${prefix ?? ""}${max}.` : "Not a value this takes."}
        </p>
      )}
      <InheritLine env={env} field={field} shown={shown} blocked={blocked} />
      {blocked && env.canManage && <p className="text-xs text-warning">{REACH_REASON} to change this.</p>}
    </div>
  );
}

// ---- the channel ----

const SOURCE_CHIP: Record<string, string> = { connection: "Connection", group: "Group", default: "Built-in" };

/**
 * The channel a level's pull requests are announced in — one the bot is in, in any of the
 * organisation's connected workspaces — or none, over one set further up. It inherits like any
 * single value; the chip beside it says where the one in force is set. The whole field is
 * connections.manage's, Inherit and Reset included: every way of changing it changes who reads a
 * private repository's findings, so without that permission it reads, disabled, with why.
 */
export function NotifyField({ env, channels }: { env: FieldEnv; channels: Scope[] | null | undefined }) {
  const own = env.own.notify;
  const blocked = !env.canReach;
  const list = channels ?? undefined;
  const inheritedLabel = channelLabel(env.inherited.notify, list) || "none";
  const source = own !== undefined ? "Set here" : (SOURCE_CHIP[env.inherited.source?.notify ?? "default"] ?? "Inherited");
  const pick = (value: string, team?: string) => {
    // As ChoiceField: a pick while the last one saves is ignored, so the trigger keeps its focus.
    if (env.busy === "notify") return;
    if (value === INHERIT) void env.save("notify", undefined);
    else if (value === "") void env.save("notify", {});
    else void env.save("notify", team ? { team, channel: value } : { channel: value });
  };
  return (
    <div className="space-y-1.5">
      <div className="flex flex-wrap items-center gap-2">
        <ChannelCombobox
          id={`${env.idPrefix}-notify`}
          aria-label="Channel reviews are announced in"
          value={own === undefined ? INHERIT : (own.channel ?? "")}
          team={own?.team}
          onChange={pick}
          options={[{ value: INHERIT, label: `Inherit (${inheritedLabel})` }]}
          emptyLabel="No announcements"
          scopes={channels}
          disabled={!env.canManage || blocked}
          className="max-w-sm flex-1"
        />
        <StatusChip variant={own !== undefined ? "info" : "neutral"}>{source}</StatusChip>
      </div>
      <InheritLine env={env} field="notify" shown={inheritedLabel} blocked={blocked} />
      {blocked && env.canManage && (
        <p className="text-xs text-warning">
          {REACH_REASON} to change this: the message carries private repositories&apos; findings to everyone in the
          channel.
        </p>
      )}
    </div>
  );
}

/**
 * Which events the channel hears of, as one box each. The set is inherited whole, like a single
 * value: the boxes show the set in force, ticking one sets the level's own from it, and the chip and
 * the line under them say where the set comes from, with the Reset that hands it back. It is
 * review's noise, not its reach — it sends nothing anywhere the channel was not already being sent
 * it — so Manage reviews changes it. Each tick saves at once, as a select does; one while the last
 * saves is ignored rather than the box disabled, which would drop the keyboard's focus. `effective`
 * is what the level resolves to, which says whether anything here is announced at all: a level with
 * no channel of its own may still have branch rules that name one, and these boxes govern those.
 */
export function NotifyOnField({ env, effective }: { env: FieldEnv; effective: ReviewEffective }) {
  const own = env.own.notify_on;
  const inherited = env.inherited.notify_on;
  const shown = own ?? inherited ?? NOTIFY_EVENTS.map((c) => c.value);
  const source = own !== undefined ? "Set here" : (SOURCE_CHIP[env.inherited.source?.notify_on ?? "default"] ?? "Inherited");
  const channel = effective.notify?.channel;
  const toggle = (event: ReviewNotifyEvent, on: boolean) => {
    if (env.busy === "notify_on") return;
    void env.save(
      "notify_on",
      NOTIFY_EVENTS.map((c) => c.value).filter((v) => (v === event ? on : shown.includes(v))),
    );
  };
  return (
    <div className="space-y-1.5">
      <div className="flex flex-wrap items-start gap-2">
        <div role="group" aria-label="Events the channel is told about" className="max-w-sm flex-1 space-y-2">
          {NOTIFY_EVENTS.map((c) => (
            <label
              key={c.value}
              className={cn("flex items-start gap-2.5 text-sm", env.canManage ? "cursor-pointer" : "cursor-not-allowed")}
            >
              <Checkbox
                id={`${env.idPrefix}-notify_on-${c.value}`}
                checked={shown.includes(c.value)}
                disabled={!env.canManage}
                onCheckedChange={(v) => toggle(c.value, v === true)}
                className="mt-px"
              />
              <span className="min-w-0">
                <span className="block font-medium">{c.label}</span>
                <span className="block text-xs text-muted-foreground">{c.hint}</span>
              </span>
            </label>
          ))}
        </div>
        <StatusChip variant={own !== undefined ? "info" : "neutral"}>{source}</StatusChip>
      </div>
      <InheritLine env={env} field="notify_on" shown={notifyEventsLabel(inherited)} />
      {!channel && (
        <p className="text-xs text-muted-foreground">
          {rulesAnnounce(effective)
            ? "No channel is set here, so only the pull requests a branch rule sends to a channel of its own are announced, under these events."
            : "No channel is in force here, so nothing is announced; a level below that names one inherits these."}
        </p>
      )}
    </div>
  );
}

// ---- lists ----

const LEVEL_CHIP: Record<string, string> = { connection: "Connection", group: "Group" };

/**
 * What a list inherits, grouped under the level that added each entry — the same readout the
 * Workspaces page puts under a channel's instructions, for the same reason: an empty box here
 * would otherwise read as "nothing applies" while a page of entries arrives from above.
 */
export function InheritedList({
  field,
  ancestors,
  mono = false,
  chips = false,
}: {
  field: ListField;
  ancestors: Ancestor[];
  mono?: boolean;
  /** Short values read better side by side than one per line. */
  chips?: boolean;
}) {
  const entries = inheritedEntries(field, ancestors);
  if (entries.length === 0) return null;
  const bySource = ancestors
    .map((a) => ({ a, values: entries.filter((e) => e.from === a).map((e) => e.value) }))
    .filter((g) => g.values.length > 0);
  return (
    <div className="space-y-1">
      <p className="text-xs text-muted-foreground">
        Inherited · {entries.length} {entries.length === 1 ? "entry" : "entries"}, added up from above
      </p>
      <ul className="divide-y overflow-hidden rounded-lg border bg-muted/30">
        {bySource.map(({ a, values }) => (
          <li key={`${a.level}-${a.name}`} className="space-y-1.5 px-3 py-2">
            <div className="flex flex-wrap items-center gap-2">
              <StatusChip variant="info">{LEVEL_CHIP[a.level] ?? a.level}</StatusChip>
              <span className="min-w-0 truncate text-xs font-medium">{a.name}</span>
            </div>
            {chips ? (
              <div className="flex flex-wrap gap-1.5">
                {values.map((v) => (
                  <span
                    key={v}
                    className={cn(
                      "inline-flex h-6 max-w-full items-center rounded-sm bg-secondary px-2 text-xs text-secondary-foreground",
                      mono && "font-mono",
                    )}
                    title={v}
                  >
                    <span className="truncate">{v}</span>
                  </span>
                ))}
              </div>
            ) : (
              <ul className="space-y-1">
                {values.map((v) => (
                  <li key={v} className="whitespace-pre-wrap break-words text-xs leading-relaxed text-muted-foreground">
                    {v}
                  </li>
                ))}
              </ul>
            )}
          </li>
        ))}
      </ul>
    </div>
  );
}

/** Instructions: sentences, saved as each one is added or removed, like a channel's allow rules. */
export function InstructionsField({ env }: { env: FieldEnv }) {
  const own = env.own.instructions ?? [];
  return (
    <div className="space-y-3">
      <InheritedList field="instructions" ancestors={env.ancestors} />
      <div className="space-y-1">
        {env.ancestors.length > 0 && <p className="text-xs text-muted-foreground">Added here</p>}
        <AllowRulesEditor
          id={`${env.idPrefix}-instructions`}
          value={own}
          noun="instruction"
          max={50}
          maxLen={400}
          placeholder='e.g. "Every query on a tenant table filters by org_id."'
          disabled={!env.canManage || env.busy === "instructions"}
          onChange={(next) => void env.save("instructions", next.length > 0 ? next : undefined)}
        />
      </div>
    </div>
  );
}

/** Login or path globs, as chips, saved together with the field's own Save. */
export function ChipsListField({
  env,
  field,
  label,
  placeholder,
  reachAll = false,
}: {
  env: FieldEnv;
  field: "exclude_authors" | "review_bots" | "ignore_paths";
  label: string;
  placeholder: string;
  reachAll?: boolean;
}) {
  const stored = env.own[field] ?? [];
  const [draft, setDraft] = useStoredDraft<string[]>(stored);
  const chips = useRef<ChipsHandle>(null);
  const blocked = reachAll && !env.canReach;
  const disabled = !env.canManage || blocked || env.busy === field;
  const same = (a: string[], b: string[]) => a.length === b.length && a.every((v, i) => v === b[i]);

  const submit = async (e: React.FormEvent) => {
    e.preventDefault();
    const next = chips.current?.flush() ?? draft;
    if (disabled || same(next, stored)) return;
    await env.save(field, next.length > 0 ? next : undefined);
  };

  return (
    <div className="space-y-3">
      <InheritedList field={field} ancestors={env.ancestors} mono chips />
      <form onSubmit={submit} className="space-y-2">
        {env.ancestors.length > 0 && <p className="text-xs text-muted-foreground">Added here</p>}
        <ChipsInput
          ref={chips}
          id={`${env.idPrefix}-${field}`}
          value={draft}
          onChange={setDraft}
          placeholder={placeholder}
          disabled={disabled}
          aria-invalid={false}
        />
        <div className="flex items-center gap-2">
          <Button type="submit" size="sm" variant="outline" className="h-8" disabled={disabled || same(draft, stored)}>
            Save {label.toLowerCase()}
          </Button>
          {!same(draft, stored) && (
            <Button type="button" size="sm" variant="ghost" className="h-8" onClick={() => setDraft(stored)}>
              Discard
            </Button>
          )}
        </div>
      </form>
    </div>
  );
}

/**
 * Context repositories: a pick from the organisation's own repositories connected through the App
 * — the only ones the server takes, so a free-text field would only let somebody type a refusal.
 */
export function ContextReposField({ env, repos }: { env: FieldEnv; repos: string[] }) {
  const stored = env.own.context_repos ?? [];
  const [draft, setDraft] = useStoredDraft<string[]>(stored);
  const [open, setOpen] = useState(false);
  const blocked = !env.canReach;
  const disabled = !env.canManage || blocked || env.busy === "context_repos";
  const inherited = inheritedEntries("context_repos", env.ancestors).map((e) => e.value.toLowerCase());
  const offered = repos.filter((r) => !draft.includes(r) && !inherited.includes(r));
  const same = draft.length === stored.length && draft.every((v, i) => v === stored[i]);

  const submit = async (e: React.FormEvent) => {
    e.preventDefault();
    if (disabled || same) return;
    await env.save("context_repos", draft.length > 0 ? draft : undefined);
  };

  return (
    <div className="space-y-3">
      <InheritedList field="context_repos" ancestors={env.ancestors} mono chips />
      <form onSubmit={submit} className="space-y-2">
        {env.ancestors.length > 0 && <p className="text-xs text-muted-foreground">Added here</p>}
        <div className="flex flex-wrap items-center gap-1.5">
          {draft.length === 0 && <span className="text-sm text-muted-foreground">None added here.</span>}
          {draft.map((r) => (
            <span
              key={r}
              className="inline-flex h-6 max-w-full items-center gap-1 rounded-sm bg-secondary px-2 font-mono text-xs text-secondary-foreground"
            >
              <span className="truncate">{r}</span>
              {!disabled && (
                <button
                  type="button"
                  aria-label={`Remove ${r}`}
                  onClick={() => setDraft(draft.filter((x) => x !== r))}
                  className="-mr-1 flex size-4 shrink-0 items-center justify-center rounded-sm text-muted-foreground hover:bg-foreground/10 hover:text-foreground"
                >
                  ×
                </button>
              )}
            </span>
          ))}
          <Popover open={open} onOpenChange={setOpen}>
            <PopoverTrigger asChild>
              <Button type="button" size="xs" variant="ghost" disabled={disabled}>
                <Plus /> Add repository
              </Button>
            </PopoverTrigger>
            <PopoverContent align="start" className="w-72 p-0">
              <Command>
                <CommandInput placeholder="Search repositories" />
                <CommandList>
                  <CommandEmpty>No other repository is connected through the App.</CommandEmpty>
                  <CommandGroup>
                    {offered.map((r) => (
                      <CommandItem
                        key={r}
                        value={r}
                        onSelect={() => {
                          setDraft([...draft, r]);
                          setOpen(false);
                        }}
                      >
                        <span className="truncate font-mono text-xs">{r}</span>
                      </CommandItem>
                    ))}
                  </CommandGroup>
                </CommandList>
              </Command>
            </PopoverContent>
          </Popover>
        </div>
        <div className="flex items-center gap-2">
          <Button type="submit" size="sm" variant="outline" className="h-8" disabled={disabled || same}>
            Save context repositories
          </Button>
          {!same && (
            <Button type="button" size="sm" variant="ghost" className="h-8" onClick={() => setDraft(stored)}>
              Discard
            </Button>
          )}
        </div>
      </form>
      {blocked && env.canManage && <p className="text-xs text-warning">{REACH_REASON} to change this.</p>}
    </div>
  );
}

/** A small multi-pick of review types, in the order they were ticked: the order they run in. */
export function TypesPicker({
  id,
  value,
  types,
  disabled,
  adds = false,
  onChange,
}: {
  id: string;
  value: string[];
  types: { key: string; name: string; enabled: boolean }[];
  disabled?: boolean;
  /** A label rule's: the types it adds to a branch rule's, of which there must be one — none is not General. */
  adds?: boolean;
  onChange: (next: string[]) => void;
}) {
  const [open, setOpen] = useState(false);
  const nameOf = (k: string) => types.find((t) => t.key === k)?.name ?? k;
  const toggle = (k: string) => onChange(value.includes(k) ? value.filter((x) => x !== k) : [...value, k]);
  const label =
    value.length === 0 ? (adds ? "Pick the types it adds" : "General (default)") : value.map((k) => (adds ? "+" : "") + nameOf(k)).join(", ");
  return (
    <Popover open={open} onOpenChange={setOpen}>
      <PopoverTrigger asChild>
        <Button
          id={id}
          type="button"
          variant="outline"
          size="sm"
          disabled={disabled}
          className="h-8 w-full min-w-0 justify-between font-normal"
          aria-label="Review types"
        >
          <span className="truncate">{label}</span>
          <ChevronsUpDown className="size-3.5 opacity-50" />
        </Button>
      </PopoverTrigger>
      <PopoverContent align="start" className="w-64 p-1">
        <ul role="listbox" aria-multiselectable className="max-h-64 overflow-y-auto">
          {types
            .filter((t) => t.enabled || value.includes(t.key))
            .map((t) => {
              const on = value.includes(t.key);
              return (
                <li key={t.key}>
                  <button
                    type="button"
                    role="option"
                    aria-selected={on}
                    onClick={() => toggle(t.key)}
                    className="flex w-full items-center gap-2 rounded-sm px-2 py-1.5 text-left text-sm hover:bg-accent"
                  >
                    <span className={cn("flex size-4 items-center justify-center rounded-sm border", on && "border-primary bg-primary text-primary-foreground")}>
                      {on && <Check className="size-3" />}
                    </span>
                    <span className="min-w-0 flex-1 truncate">{t.name}</span>
                    <span className="font-mono text-xs text-muted-foreground">{t.key}</span>
                    {!t.enabled && <StatusChip variant="warning">off</StatusChip>}
                  </button>
                </li>
              );
            })}
        </ul>
        <p className="border-t px-2 py-1.5 text-xs text-muted-foreground">
          {adds
            ? "Ticked in order: they run in that order, after the branch rule's."
            : "Ticked in order: that is the order they run in. None ticked runs General."}
        </p>
      </PopoverContent>
    </Popover>
  );
}
