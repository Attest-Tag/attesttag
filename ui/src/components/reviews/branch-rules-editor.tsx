"use client";

import { useState } from "react";
import { ArrowDown, ArrowUp, GitBranch, Plus, Tag, Trash2 } from "lucide-react";
import { ChannelCombobox } from "@/components/core/channel-combobox";
import { useConfirm } from "@/components/core/confirm-dialog";
import { SegmentedControl } from "@/components/core/segmented-control";
import { StatusChip } from "@/components/core/status-chip";
import { Button } from "@/components/ui/button";
import { Input } from "@/components/ui/input";
import { Label } from "@/components/ui/label";
import { Select, SelectContent, SelectItem, SelectTrigger, SelectValue } from "@/components/ui/select";
import type { ReviewBranchRule, ReviewMode, ReviewNotify, ReviewTrigger, ReviewTypeSummary, Scope } from "@/lib/api";
import { useDirtyOnScreen } from "@/lib/assistant-screen";
import { cn } from "@/lib/utils";
import { INHERIT, TypesPicker } from "@/components/reviews/review-fields";
import {
  STRICTNESS,
  TRIGGERS,
  channelLabel,
  choiceLabel,
  isFallback,
  isLabelRule,
  labelsProblem,
  modelLabel,
  parseLabels,
  ruleLabel,
  rulesHeld,
} from "@/components/reviews/review-format";

// Branch rules: an ordered list where the first match wins, ending in the fallback every other
// branch falls to. The nearest level that has a list supplies all of it — two lists cannot be
// merged when order is the meaning — so the choice here is the whole list: inherit it, or set one
// of this level's own. Edited as a draft and saved together, because a half-edited list (a rule
// moved above another but not yet narrowed) is exactly the state a pull request must never meet.
//
// A rule with labels is a label rule (review.LabelTypes): it chooses nothing, and adds its types to
// whatever the branch rule a pull request meets chose, when the pull request carries one of them. It
// sits in the same list, above the fallback, and overrides nothing — the server refuses a label rule
// that sets when, strictness, where it posts, the model or the channel — so its row offers its types
// and no overrides, and reads "label perf → +Performance".

type Override = "trigger" | "strictness" | "post" | "model";

const POSTS = [
  { value: "shadow", label: "Shadow" },
  { value: "live", label: "Live" },
] as const;

/** The list the server keeps: the fallback last, and exactly one. */
function withFallback(rules: ReviewBranchRule[]): ReviewBranchRule[] {
  const rest = rules.filter((r) => !isFallback(r));
  const fallback = rules.find(isFallback) ?? { types: [] };
  return [...rest.map((r) => ({ ...r })), { ...fallback }];
}

/** A level's own list as a draft is compared with it, the way Save sends it: "null" when it inherits. */
function storedList(own: ReviewBranchRule[] | null | undefined): string {
  return JSON.stringify(own?.length ? withFallback(own).map(clean) : null);
}

/** A glob git could hold: no spaces, which a ref never has, so one there is a typo worth saying. */
function globProblem(g: string | undefined): string | null {
  if (!g) return null;
  if (/\s/.test(g)) return "no spaces";
  if (g.length > 200) return "at most 200 characters";
  return null;
}

/** A rule's channel as the server stores it: the pair, or {} for "none" — never a team alone. */
function cleanNotify(n: ReviewNotify): ReviewNotify {
  if (!n.channel) return {};
  return n.team ? { team: n.team, channel: n.channel } : { channel: n.channel };
}

/**
 * Leaves out what is empty, as the server stores it: an empty override inherits. Every field a rule
 * can hold is carried, the channel included — a field dropped here would be a field every save of
 * the list takes away from every rule. A label rule carries no override, which the server would
 * refuse: a rule given labels drops the ones it had, and its row says why.
 */
function clean(r: ReviewBranchRule): ReviewBranchRule {
  const out: ReviewBranchRule = {};
  if (r.base?.trim()) out.base = r.base.trim();
  if (r.head?.trim()) out.head = r.head.trim();
  if (r.labels?.length) out.labels = r.labels;
  if (r.types?.length) out.types = r.types;
  if (isLabelRule(r)) return out;
  if (r.trigger) out.trigger = r.trigger;
  if (r.strictness) out.strictness = r.strictness;
  if (r.post) out.post = r.post;
  if (r.model) out.model = r.model;
  // An empty channel is a value — "keep these quiet" — not an absent one.
  if (r.notify) out.notify = cleanNotify(r.notify);
  return out;
}

export function BranchRulesEditor({
  id,
  own,
  inherited,
  inheritedFrom,
  mode,
  trigger,
  types,
  models,
  heavy,
  channels,
  notify,
  canManage,
  canReach,
  busy,
  onSave,
}: {
  id: string;
  /** This level's own list; absent or empty inherits. */
  own: ReviewBranchRule[] | undefined;
  /** The list it would inherit, and where from. */
  inherited: ReviewBranchRule[];
  inheritedFrom: string;
  /** What the level resolves to, which a rule's own post and trigger override: what decides where each rule goes. */
  mode: ReviewMode;
  trigger: ReviewTrigger;
  types: ReviewTypeSummary[];
  /** The models a rule may name: Advanced and the ones Settings offers to channels. */
  models: string[];
  heavy?: string;
  /** The channels the bot is in, for a rule's own; null while they load. */
  channels: Scope[] | null | undefined;
  /** The channel the level announces in, which a rule naming none uses: "" for none. */
  notify: string;
  canManage: boolean;
  canReach: boolean;
  busy: boolean;
  /** Saves the whole list, or undefined to inherit again. */
  onSave: (rules: ReviewBranchRule[] | undefined) => Promise<boolean>;
}) {
  const isSet = (own?.length ?? 0) > 0;
  const [here, setHere] = useState(isSet);
  const [draft, setDraft] = useState<ReviewBranchRule[]>(withFallback(isSet ? own! : inherited));
  // A save or a reset of this editor's own on its way: the list it brings back is the draft's.
  const [saving, setSaving] = useState(false);
  // The draft was kept over a list somebody else saved since it was made (below).
  const [elsewhere, setElsewhere] = useState(false);
  // A save, a reset, or a change above that alters what is inherited: the list starts again from
  // what is stored now. Followed in place rather than by remounting, which would drop the focus of
  // whoever pressed Save from the keyboard. Not over edits, though: a draft that is neither the list
  // it was made from nor the one stored now is somebody's work, and another person's save landing
  // under it — found when a save of this one is refused for it, and the level read again — is said
  // rather than allowed to wipe it.
  const sig = JSON.stringify([own ?? null, inherited]);
  const [seen, setSeen] = useState(sig);
  if (seen !== sig) {
    const [wasOwn, wasInherited] = JSON.parse(seen) as [ReviewBranchRule[] | null, ReviewBranchRule[]];
    const mine = JSON.stringify(draft.map(clean));
    const madeFrom = JSON.stringify(withFallback(wasOwn?.length ? wasOwn : wasInherited).map(clean));
    const keep = !saving && here && mine !== madeFrom && mine !== storedList(own);
    setSeen(sig);
    setSaving(false);
    // Only a change to the list the draft would replace is news. A level with a list of its own
    // reads nothing from above, so a save up there leaves this draft as true as it was — the
    // server agrees, and keeps the digest a save of it sends — and says nothing.
    const replaces = JSON.stringify(withFallback(isSet ? own! : inherited).map(clean));
    if (!keep) setElsewhere(false);
    else if (replaces !== madeFrom) setElsewhere(true);
    if (!keep) {
      setHere(isSet);
      setDraft(withFallback(isSet ? own! : inherited));
    }
  }
  const stored = storedList(own);
  const dirty = here && JSON.stringify(draft.map(clean)) !== stored;
  // Told to the assistant with the level on screen: a card changing this list would land under
  // these edits, and says so.
  useDirtyOnScreen(dirty);
  const disabled = !canManage || busy;
  // A list that sends some branches live, to every push or to a model of its own, and not all of
  // them — or names a channel for any: any change to which branch meets which rule could start one,
  // so its shape is connections.manage's (reviewTierNeeds). Types and strictness stay editable; the
  // rest reads.
  const held = !canReach && rulesHeld(withFallback(isSet ? own! : inherited), mode, trigger);
  const { confirm, confirmDialog } = useConfirm();

  const problems = draft.map((r, i) => {
    if (i === draft.length - 1) return null;
    if (!r.base?.trim() && !r.head?.trim() && !isLabelRule(r))
      return "Name a branch on at least one side or a label, or delete the rule: one matching every branch belongs last.";
    const p = globProblem(r.base) ?? globProblem(r.head);
    if (p) return `Branch globs take ${p}.`;
    if (isLabelRule(r) && !r.types?.length) return "A label rule adds review types: pick at least one.";
    return labelsProblem(r.labels);
  });
  const invalid = problems.some(Boolean) || draft.length > 20;

  const patch = (i: number, change: Partial<ReviewBranchRule>) =>
    setDraft((d) => d.map((r, j) => (j === i ? { ...r, ...change } : r)));
  const move = (i: number, by: -1 | 1) => {
    setDraft((d) => {
      const next = [...d];
      [next[i], next[i + by]] = [next[i + by], next[i]];
      return next;
    });
    // The rows are the list's positions, so the button pressed stays where it was while its rule
    // moves away. Focus follows the rule instead, onto the same arrow if it can go further, so a
    // keyboard user pressing Move up three times moves one rule three places.
    const to = i + by;
    requestAnimationFrame(() => {
      const same = document.getElementById(`${id}-rule-${to}-${by < 0 ? "up" : "down"}`) as HTMLButtonElement | null;
      const other = document.getElementById(`${id}-rule-${to}-${by < 0 ? "down" : "up"}`) as HTMLButtonElement | null;
      (same && !same.disabled ? same : other)?.focus();
    });
  };
  // The focus goes where the hand goes next: into the new rule's first box, or — after a delete
  // unmounts the button pressed — to the rule that took its place, else Add rule.
  const focusLater = (elementId: string, fallback?: string) =>
    requestAnimationFrame(() => (document.getElementById(elementId) ?? (fallback ? document.getElementById(fallback) : null))?.focus());
  const remove = (i: number) => {
    setDraft((d) => d.filter((_, j) => j !== i));
    focusLater(`${id}-rule-${i}-head`, `${id}-add`);
  };
  const add = () => {
    focusLater(`${id}-rule-${draft.length - 1}-head`);
    setDraft((d) => [...d.slice(0, -1), { base: "", head: "", types: [] }, d[d.length - 1]]);
  };

  // The editor's own save: what comes back is followed, whatever the draft held (see above).
  const saveOwn = async (rules: ReviewBranchRule[] | undefined) => {
    setSaving(true);
    const ok = await onSave(rules);
    if (!ok) setSaving(false);
    return ok;
  };

  // Back to what is stored. After a change elsewhere that is the level as somebody else left it,
  // set here or inherited.
  const discard = () => {
    if (elsewhere) setHere(isSet);
    setElsewhere(false);
    setDraft(withFallback(isSet ? own! : inherited));
  };

  const choose = async (next: "inherit" | "here") => {
    if (next === "here") {
      // Starting from the inherited list: the usual edit is one rule more than what is there.
      if (!isSet) setDraft(withFallback(inherited));
      setHere(true);
      return;
    }
    if (isSet) {
      // One press would otherwise throw away an ordered list of up to twenty rules.
      const n = withFallback(own!).length - 1;
      const ok = await confirm({
        title: "Inherit the branch rules?",
        description: `This level's ${n === 1 ? "rule goes" : `${n} rules go`}, and it takes the list ${inheritedFrom === "built-in default" ? "built in" : inheritedFrom} instead.`,
        confirmLabel: "Inherit",
        destructive: true,
      });
      if (!ok || !(await saveOwn(undefined))) return;
    }
    setHere(false);
  };

  const submit = async (e: React.FormEvent) => {
    e.preventDefault();
    if (disabled || invalid || !dirty) return;
    await saveOwn(draft.map(clean));
  };

  const shown = here ? draft : withFallback(inherited);
  const editing = here && !disabled;
  const shaping = editing && !held;

  return (
    <div className="space-y-3">
      <div className="flex flex-wrap items-center gap-3">
        <SegmentedControl
          value={here ? "here" : "inherit"}
          onValueChange={(v) => void choose(v)}
          options={[
            { value: "inherit", label: `Inherit · ${inheritedFrom}`, disabled: disabled || (held && isSet) },
            { value: "here", label: "Set here", disabled },
          ]}
        />
        <span className="text-xs text-muted-foreground">
          First match wins, top to bottom. A rule with labels adds its types to that match instead.
        </span>
      </div>

      <form onSubmit={submit} className="space-y-2">
        <ol className="divide-y overflow-hidden rounded-lg border">
          {shown.map((rule, i) => {
            const fallback = i === shown.length - 1;
            const rid = `${id}-rule-${i}`;
            return (
              <li key={i} className={cn("space-y-2 px-3 py-2.5", !here && "bg-muted/30")}>
                <div className="flex flex-wrap items-center gap-2">
                  <span className="w-5 shrink-0 text-xs tabular-nums text-muted-foreground">{i + 1}.</span>
                  {fallback ? (
                    <span className="flex min-w-0 flex-1 items-center gap-1.5 text-sm font-medium">
                      <GitBranch className="size-3.5 text-muted-foreground" />
                      Any other branch
                    </span>
                  ) : shaping ? (
                    <div className="grid min-w-0 flex-1 grid-cols-[1fr_auto_1fr] items-center gap-1.5">
                      <Input
                        id={`${rid}-head`}
                        aria-label={`Rule ${i + 1}: from branch (head)`}
                        value={rule.head ?? ""}
                        placeholder="from: any"
                        onChange={(e) => patch(i, { head: e.target.value })}
                        className="h-8 font-mono text-xs"
                      />
                      <span aria-hidden className="text-xs text-muted-foreground">→</span>
                      <Input
                        id={`${rid}-base`}
                        aria-label={`Rule ${i + 1}: into branch (base)`}
                        value={rule.base ?? ""}
                        placeholder="into: any"
                        onChange={(e) => patch(i, { base: e.target.value })}
                        className="h-8 font-mono text-xs"
                      />
                    </div>
                  ) : isLabelRule(rule) ? (
                    <span className="flex min-w-0 flex-1 items-center gap-1.5 truncate font-mono text-xs">
                      <Tag className="size-3.5 shrink-0 text-muted-foreground" />
                      <span className="truncate">{labelRuleLine(rule, types)}</span>
                    </span>
                  ) : (
                    <span className="min-w-0 flex-1 truncate font-mono text-xs">
                      {rule.head || "any"} → {rule.base || "any"}
                    </span>
                  )}
                  {shaping && !fallback && (
                    <div className="flex shrink-0 items-center">
                      <Button id={`${rid}-up`} type="button" variant="ghost" size="icon-sm" aria-label={`Move rule ${i + 1} up`} disabled={i === 0} onClick={() => move(i, -1)}>
                        <ArrowUp />
                      </Button>
                      <Button id={`${rid}-down`} type="button" variant="ghost" size="icon-sm" aria-label={`Move rule ${i + 1} down`} disabled={i >= shown.length - 2} onClick={() => move(i, 1)}>
                        <ArrowDown />
                      </Button>
                      <Button type="button" variant="ghost" size="icon-sm" aria-label={`Delete rule ${i + 1}`} onClick={() => remove(i)}>
                        <Trash2 />
                      </Button>
                    </div>
                  )}
                </div>
                {shaping && !fallback && (
                  <LabelsField id={`${rid}-labels`} n={i + 1} value={rule.labels ?? []} onChange={(labels) => patch(i, { labels })} />
                )}
                <RuleBody
                  id={rid}
                  rule={rule}
                  editing={editing}
                  held={held}
                  types={types}
                  models={models}
                  heavy={heavy}
                  channels={channels}
                  notify={notify}
                  canReach={canReach}
                  onChange={(change) => patch(i, change)}
                />
                {editing && problems[i] && <p className="pl-7 text-xs text-danger">{problems[i]}</p>}
              </li>
            );
          })}
        </ol>
        {shaping && (
          <p className="text-xs text-muted-foreground">
            <span className="font-medium text-foreground">Labels</span> make a rule a label rule: a pull request
            carrying any of them gets the rule&apos;s types added to what its branch rule runs, on any branch or only
            the ones the rule names. Commas between labels; case does not matter.
          </p>
        )}
        {editing && elsewhere && dirty && (
          // Beside the Save it is about: pressed after a refusal, this is where the eye already is.
          <p role="status" className="rounded-lg border border-warning/30 bg-warning-soft px-3 py-2 text-xs text-warning">
            These rules changed elsewhere. Saving replaces them with yours.
          </p>
        )}
        {editing && (
          <div className="flex flex-wrap items-center gap-2">
            {!held && (
              <Button id={`${id}-add`} type="button" size="sm" variant="ghost" onClick={add} disabled={draft.length >= 20}>
                <Plus /> Add rule
              </Button>
            )}
            <span className="flex-1" />
            {dirty && (
              <Button type="button" size="sm" variant="ghost" onClick={discard}>
                Discard
              </Button>
            )}
            <Button type="submit" size="sm" disabled={invalid || !dirty}>
              Save branch rules
            </Button>
          </div>
        )}
        {!canReach && canManage && (
          <p className={cn("text-xs", held ? "text-warning" : "text-muted-foreground")}>
            {held
              ? "Some of these rules post live, review every push, pick a model or name a channel, so adding, deleting or reordering rules, their branches and those four need Manage connections as well. Types and strictness are yours to change."
              : "A rule that posts live, reviews every push, picks a model or names a channel needs Manage connections as well — and so, once there is one, does changing the order or the branches of the list."}
          </p>
        )}
      </form>
      {confirmDialog}
    </div>
  );
}

/** One rule's types and overrides: read as a sentence when inherited, as controls when edited. */
function RuleBody({
  id,
  rule,
  editing,
  held,
  types,
  models,
  heavy,
  channels,
  notify,
  canReach,
  onChange,
}: {
  id: string;
  rule: ReviewBranchRule;
  editing: boolean;
  /** The list's shape is connections.manage's: where the rule posts, when, on what model and to which channel, read only. */
  held: boolean;
  types: ReviewTypeSummary[];
  models: string[];
  heavy?: string;
  channels: Scope[] | null | undefined;
  notify: string;
  canReach: boolean;
  onChange: (change: Partial<ReviewBranchRule>) => void;
}) {
  const named = rule.types ?? [];
  const typeOf = (k: string) => types.find((t) => t.key === k);
  // A rule naming a type that was turned off, or no longer exists, skips it: worth a warning on
  // the rule itself, since this is where somebody would look for why a type never ran.
  const missing = named.filter((k) => !typeOf(k)?.enabled);
  const labelled = isLabelRule(rule);

  if (!editing && labelled) {
    // Its line above says what it adds; here only what it would skip.
    if (missing.length === 0) return null;
    return (
      <div className="flex flex-wrap items-center gap-1.5 pl-7">
        {missing.map((k) => (
          <StatusChip key={k} variant="warning">
            +{typeOf(k)?.name ?? k}
            {typeOf(k) ? " · off" : " · no such type"}
          </StatusChip>
        ))}
      </div>
    );
  }

  if (!editing) {
    const overrides = [
      rule.trigger && choiceLabel(TRIGGERS, rule.trigger).toLowerCase(),
      rule.strictness && `${rule.strictness} strictness`,
      rule.post && (rule.post === "live" ? "posts live" : "records in shadow"),
      rule.model && modelLabel(rule.model, heavy),
      rule.notify &&
        (rule.notify.channel ? `announced in ${channelLabel(rule.notify, channels ?? undefined)}` : "not announced"),
    ].filter(Boolean);
    return (
      <div className="flex flex-wrap items-center gap-1.5 pl-7">
        {(named.length > 0 ? named : ["general"]).map((k) => (
          <StatusChip key={k} variant={missing.includes(k) ? "warning" : "neutral"}>
            {typeOf(k)?.name ?? k}
            {missing.includes(k) ? (typeOf(k) ? " · off" : " · no such type") : ""}
          </StatusChip>
        ))}
        {overrides.length > 0 && <span className="text-xs text-muted-foreground">· {overrides.join(" · ")}</span>}
      </div>
    );
  }

  const pick = (key: Override, extra: { value: string; label: string; reach?: boolean }[]) => (
    <div className="min-w-0 space-y-1">
      <Label htmlFor={`${id}-${key}`} className="text-xs font-normal text-muted-foreground">
        {key === "trigger" ? "When" : key === "post" ? "Post" : key === "model" ? "Model" : "Strictness"}
      </Label>
      <Select
        value={(rule[key] as string | undefined) || INHERIT}
        onValueChange={(v) => onChange({ [key]: v === INHERIT ? undefined : v })}
        disabled={(key === "model" && !canReach && !rule.model) || (held && key !== "strictness")}
      >
        <SelectTrigger id={`${id}-${key}`} size="sm" className="h-8 w-full">
          <SelectValue />
        </SelectTrigger>
        <SelectContent>
          <SelectItem value={INHERIT}>Inherit</SelectItem>
          {extra.map((o) => (
            <SelectItem key={o.value} value={o.value} disabled={o.reach && !canReach}>
              {o.label}
            </SelectItem>
          ))}
        </SelectContent>
      </Select>
    </div>
  );

  return (
    <div className="space-y-2 pl-7">
      <div className="space-y-1">
        <TypesPicker
          id={`${id}-types`}
          value={named}
          types={types}
          adds={labelled}
          onChange={(next) => onChange({ types: next })}
        />
        {missing.length > 0 && (
          <p className="text-xs text-warning">
            {missing.map((k) => typeOf(k)?.name ?? k).join(", ")} {missing.length === 1 ? "is" : "are"} off or missing,
            so this rule skips {missing.length === 1 ? "it" : "them"}.
          </p>
        )}
      </div>
      {labelled ? (
        // The server refuses a label rule that overrides anything: how a review is published, how sure
        // it must be and what it costs are the branch rule's, and a label is anybody's with triage.
        <p className="text-xs text-muted-foreground">
          Adds types only: when, strictness, where it posts, the model and the channel are the branch rule&apos;s.
        </p>
      ) : (
        <div className="grid grid-cols-2 gap-2 sm:grid-cols-4">
          {pick(
            "trigger",
            TRIGGERS.map((t) => ({ value: t.value, label: t.label, reach: t.value === "push" })),
          )}
          {pick("strictness", STRICTNESS.map((s) => ({ value: s.value, label: s.label })))}
          {pick(
            "post",
            POSTS.map((p) => ({ value: p.value, label: p.label, reach: p.value === "live" })),
          )}
          {pick(
            "model",
            [
              { value: "heavy", label: modelLabel("heavy", heavy), reach: true },
              { value: "default", label: "The default model", reach: true },
              ...models.map((m) => ({ value: m, label: m, reach: true })),
              ...(rule.model && rule.model !== "heavy" && rule.model !== "default" && !models.includes(rule.model)
                ? [{ value: rule.model, label: `${rule.model} (no longer offered)`, reach: true }]
                : []),
            ],
          )}
          {/* Two columns wide: a channel's name is longer than any of the four choices above. */}
          <div className="col-span-2 min-w-0 space-y-1">
            <Label htmlFor={`${id}-notify`} className="text-xs font-normal text-muted-foreground">
              Notify
            </Label>
            <ChannelCombobox
              id={`${id}-notify`}
              aria-label="Channel this rule's pull requests are announced in"
              value={rule.notify === undefined ? INHERIT : (rule.notify.channel ?? "")}
              team={rule.notify?.team}
              onChange={(value, team) =>
                onChange({
                  notify: value === INHERIT ? undefined : value === "" ? {} : team ? { team, channel: value } : { channel: value },
                })
              }
              // "—": no channel of the rule's own, so the settings' one is told — named, so the
              // choice reads as what it does.
              options={[{ value: INHERIT, label: `— ${notify ? `the settings' ${notify}` : "the settings' (none)"}` }]}
              emptyLabel="No announcements"
              scopes={channels}
              disabled={!canReach || held}
              className="h-8"
            />
          </div>
        </div>
      )}
    </div>
  );
}

/** A label rule read as one line: "label perf → +Performance", with its branches when it names any. */
function labelRuleLine(r: ReviewBranchRule, types: ReviewTypeSummary[]): string {
  const name = (k: string) => types.find((t) => t.key === k)?.name ?? k;
  const adds = (r.types ?? []).map((k) => `+${name(k)}`).join(" ");
  return `${ruleLabel(r)} → ${adds || "nothing yet"}`;
}

/**
 * A rule's labels, typed on one line with commas between them. The text is the box's own, so typing
 * "perf, " is not cut back to "perf" between keystrokes; it follows the rule when its labels change
 * from outside — Discard, a move, a save.
 */
function LabelsField({
  id,
  n,
  value,
  onChange,
}: {
  id: string;
  /** The rule's place in the list, for its accessible name. */
  n: number;
  value: string[];
  onChange: (labels: string[]) => void;
}) {
  const [text, setText] = useState(value.join(", "));
  const sig = JSON.stringify(value);
  const [seen, setSeen] = useState(sig);
  if (seen !== sig) {
    setSeen(sig);
    if (JSON.stringify(parseLabels(text)) !== sig) setText(value.join(", "));
  }
  return (
    <div className="flex items-center gap-1.5 pl-7">
      <Tag aria-hidden className="size-3.5 shrink-0 text-muted-foreground" />
      <Input
        id={id}
        aria-label={`Rule ${n}: labels it adds its types for`}
        value={text}
        placeholder="labels: none, so a branch rule"
        onChange={(e) => {
          setText(e.target.value);
          onChange(parseLabels(e.target.value));
        }}
        className="h-8 font-mono text-xs"
      />
    </div>
  );
}
