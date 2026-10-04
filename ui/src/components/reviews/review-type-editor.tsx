"use client";

import { useRef, useState } from "react";
import { toast } from "sonner";
import {
  ArrowDown,
  ArrowUp,
  Check,
  ChevronRight,
  ExternalLink,
  FlaskConical,
  History,
  ListChecks,
  Plus,
  RotateCcw,
  Sparkles,
  Trash2,
  X,
} from "lucide-react";
import { ChipsInput, type ChipsHandle } from "@/components/core/chips-input";
import { useConfirm } from "@/components/core/confirm-dialog";
import { ErrorBanner } from "@/components/core/error-banner";
import { RelativeTime } from "@/components/core/relative-time";
import { SettingsGroup, SettingsSection } from "@/components/core/settings-section";
import { StatusChip, type StatusChipVariant } from "@/components/core/status-chip";
import { REACH_REASON } from "@/components/reviews/review-fields";
import { STRICTNESS, connectionName, modelLabel, ruleLabel } from "@/components/reviews/review-format";
import { ReviewsTabLink } from "@/components/reviews/reviews-nav";
import { ReviewTypeHistoryDialog } from "@/components/reviews/review-type-history";
import { TryTypeDialog } from "@/components/reviews/review-try-dialog";
import { SkillsList, skillDraft, skillLink, skillProblems, type SkillDraft } from "@/components/reviews/review-type-skills";
import { Button } from "@/components/ui/button";
import { Input } from "@/components/ui/input";
import { Label } from "@/components/ui/label";
import { Popover, PopoverContent, PopoverTrigger } from "@/components/ui/popover";
import { Select, SelectContent, SelectItem, SelectTrigger, SelectValue } from "@/components/ui/select";
import { Skeleton } from "@/components/ui/skeleton";
import { Switch } from "@/components/ui/switch";
import { Textarea } from "@/components/ui/textarea";
import { useStickySet } from "@/hooks/use-sticky";
import {
  ApiError,
  api,
  errorMessage,
  useApi,
  type ReviewNode,
  type ReviewSettingsTree,
  type ReviewType,
  type ReviewTypeInput,
  type ReviewTypeRule,
} from "@/lib/api";
import { useDirtyOnScreen } from "@/lib/assistant-screen";
import { cn } from "@/lib/utils";

// One review type in Reviews › Types: what it is for, which files it reads, how strict it is, what
// it may spend, and its rules — edited as one draft and saved together as the type's next version,
// because a run records the {key, version} it ran with and a half-saved rubric would be a version
// nobody meant. Its switch, Reset to built-in and Revert are actions of their own, outside the draft:
// each saves at once, as the server makes them. Try on a PR runs the draft, unsaved.
//
// The bounds are review.ValidateType's (internal/review/types.go), checked here only so the form can
// say what is wrong before the server does: 40 rules of 400 characters, a name of 2 to 40.

const MAX_RULES = 40;
const MAX_RULE = 400;
const MAX_PURPOSE = 2000;
const MAX_EXAMPLE = 2000;
const INHERIT = "__inherit";
const NO_CAP = "__none";

type RuleDraft = {
  /** React's key: a rule not saved yet has no id. */
  uid: string;
  id?: string;
  text: string;
  severity_cap: string;
  path_globs: string[];
  example_bad: string;
  example_good: string;
  enabled: boolean;
  source: string;
  status: string;
  from_comment_url?: string;
};

type TypeDraft = {
  name: string;
  purpose: string;
  path_globs: string[];
  strictness: string;
  inline_min_severity: string;
  model: string;
  max_usd: string;
  rules: RuleDraft[];
  skills: SkillDraft[];
};

let uids = 0;
const uid = () => `r${++uids}`;

function ruleDraft(r: ReviewTypeRule): RuleDraft {
  return {
    uid: uid(),
    id: r.id,
    text: r.text,
    severity_cap: r.severity_cap ?? "",
    path_globs: r.path_globs ?? [],
    example_bad: r.example_bad ?? "",
    example_good: r.example_good ?? "",
    enabled: r.enabled,
    source: r.source,
    status: r.status ?? "",
    from_comment_url: r.from_comment_url,
  };
}

function draftOf(t: ReviewType): TypeDraft {
  return {
    name: t.name,
    purpose: t.purpose,
    path_globs: t.path_globs ?? [],
    strictness: t.strictness ?? "",
    inline_min_severity: t.inline_min_severity ?? "",
    model: t.model ?? "",
    max_usd: t.max_usd ? String(t.max_usd) : "",
    rules: (t.rules ?? []).map(ruleDraft),
    skills: (t.skills ?? []).map(skillDraft),
  };
}

/** What a draft says, without the keys React needs: two drafts that say the same thing compare equal. */
function said(d: TypeDraft): string {
  return JSON.stringify({ ...d, rules: d.rules.map((r) => ({ ...r, uid: "" })), skills: d.skills.map(skillLink) });
}

function parseUSD(raw: string): number | null {
  const t = raw.trim();
  if (t === "" || t === "0") return 0;
  const n = Number(t);
  return Number.isFinite(n) && n >= 0.1 && n <= 5 ? n : null;
}

/** The type as the API takes it: everything, the whole rule list in order. Money only from somebody who may set it. */
export function typeInput(d: TypeDraft, base: ReviewType, canReach: boolean): ReviewTypeInput {
  const out: ReviewTypeInput = {
    version: base.version,
    name: d.name.trim(),
    purpose: d.purpose,
    path_globs: d.path_globs,
    strictness: d.strictness,
    inline_min_severity: d.inline_min_severity,
    rules: d.rules.map((r) => ({
      id: r.id,
      text: r.text.trim(),
      severity_cap: r.severity_cap,
      path_globs: r.path_globs,
      example_bad: r.example_bad,
      example_good: r.example_good,
      enabled: r.enabled,
      status: r.status || undefined,
    })),
    skills: d.skills.map(skillLink),
  };
  if (canReach) {
    out.model = d.model;
    out.max_usd = parseUSD(d.max_usd) ?? 0;
  }
  return out;
}

/** What the server would refuse, said before it is asked: one line per problem. */
function problems(d: TypeDraft): string[] {
  const out: string[] = [];
  const name = d.name.trim().length;
  if (name < 2 || name > 40) out.push("The name is 2 to 40 characters.");
  if (d.purpose.length > MAX_PURPOSE) out.push(`What it is for is at most ${MAX_PURPOSE.toLocaleString("en-US")} characters.`);
  if (d.rules.length > MAX_RULES) out.push(`At most ${MAX_RULES} rules: split the type in two.`);
  d.rules.forEach((r, i) => {
    const n = r.text.trim().length;
    if (n === 0) out.push(`R${i + 1} is empty.`);
    else if (n > MAX_RULE) out.push(`R${i + 1} is over ${MAX_RULE} characters.`);
    if (r.example_bad.length > MAX_EXAMPLE || r.example_good.length > MAX_EXAMPLE) out.push(`R${i + 1}'s examples are at most ${MAX_EXAMPLE} characters each.`);
  });
  if (parseUSD(d.max_usd) === null) out.push("Max $ per review is between $0.10 and $5.00, or empty for the settings' own.");
  out.push(...skillProblems(d.skills));
  return out;
}

/** Which of the three a type is; whether it is switched on is its own chip. */
export function typeKind(t: { edited: boolean; custom: boolean }): { label: string; variant: StatusChipVariant } {
  if (t.custom) return { label: "Custom", variant: "ai" };
  if (t.edited) return { label: "Edited", variant: "info" };
  return { label: "Built-in", variant: "neutral" };
}

const SOURCE: Record<string, string> = { builtin: "built-in", team: "team", learned: "learned" };

export function ReviewTypeEditor({
  typeKey,
  canManage,
  canReach,
  offeredModels,
  heavy,
  onChanged,
}: {
  typeKey: string;
  canManage: boolean;
  canReach: boolean;
  offeredModels: string[];
  heavy: string;
  /** A save, a switch, a reset or a revert: the list beside it says so too. */
  onChanged: () => void;
}) {
  const loaded = useApi<{ type: ReviewType }>(`/api/review-types/${encodeURIComponent(typeKey)}`);
  if (loaded.error && !loaded.data) return <ErrorBanner message={loaded.error} onRetry={loaded.reload} />;
  if (!loaded.data) {
    return (
      <div className="space-y-3">
        <Skeleton className="h-14 w-full rounded-xl" />
        <Skeleton className="h-72 w-full rounded-xl" />
      </div>
    );
  }
  return (
    <Editor
      // One editor per type, not per version: a version saved somewhere else — a card confirmed in
      // the assistant, which reloads this — is followed in place, so edits in progress survive it.
      key={loaded.data.type.key}
      initial={loaded.data.type}
      canManage={canManage}
      canReach={canReach}
      offeredModels={offeredModels}
      heavy={heavy}
      onReload={loaded.reload}
      onChanged={onChanged}
    />
  );
}

function Editor({
  initial,
  canManage,
  canReach,
  offeredModels,
  heavy,
  onReload,
  onChanged,
}: {
  initial: ReviewType;
  canManage: boolean;
  canReach: boolean;
  offeredModels: string[];
  heavy: string;
  onReload: () => void;
  onChanged: () => void;
}) {
  const [base, setBase] = useState(initial);
  const [draft, setDraft] = useState(() => draftOf(initial));
  const [busy, setBusy] = useState<string | null>(null);
  // stale: the save lost a race with somebody else's (409), and the way on is the newest version.
  const [saveError, setSaveError] = useState<{ message: string; stale: boolean } | null>(null);
  const [historyOpen, setHistoryOpen] = useState(false);
  const [tryOpen, setTryOpen] = useState(false);
  const { confirm, confirmDialog } = useConfirm();
  const globsRef = useRef<ChipsHandle>(null);

  const dirty = said(draft) !== said(draftOf(base));
  // Told to the assistant with the type on screen: a card changing it would land under these edits.
  useDirtyOnScreen(dirty);
  const issues = problems(draft);
  const kind = typeKind(base);
  const next = base.version === 0 ? 2 : base.version + 1;
  const ro = !canManage;

  const set = <K extends keyof TypeDraft>(k: K, v: TypeDraft[K]) => setDraft((d) => ({ ...d, [k]: v }));
  const setRule = (uidOf: string, patch: Partial<RuleDraft>) =>
    setDraft((d) => ({ ...d, rules: d.rules.map((r) => (r.uid === uidOf ? { ...r, ...patch } : r)) }));
  const moveRule = (i: number, by: -1 | 1) =>
    setDraft((d) => {
      const rules = [...d.rules];
      const j = i + by;
      if (j < 0 || j >= rules.length) return d;
      [rules[i], rules[j]] = [rules[j], rules[i]];
      return { ...d, rules };
    });
  const dropRule = (uidOf: string) => setDraft((d) => ({ ...d, rules: d.rules.filter((r) => r.uid !== uidOf) }));
  // The rule just added, whose box takes the focus as it mounts: Add rule is followed by typing it.
  const added = useRef<string | null>(null);
  const addRule = () => {
    const fresh = uid();
    added.current = fresh;
    setDraft((d) => ({
      ...d,
      rules: [
        ...d.rules,
        { uid: fresh, text: "", severity_cap: "", path_globs: [], example_bad: "", example_good: "", enabled: true, source: "team", status: "" },
      ],
    }));
  };

  /** The type as somebody else saved it, over whatever this editor held: nothing here is kept. */
  const takeNewest = (t: ReviewType) => {
    setBase(t);
    setDraft(draftOf(t));
    setSaveError(null);
  };

  // A newer version read back — saved elsewhere, and reloaded because the assistant's Confirm said
  // so — is followed when there is nothing here to lose. Over edits it is said instead, with the
  // way on that a refused save offers, since saving them would be refused for the same reason: the
  // draft is against the version before. One this editor saved itself is already its base, and one
  // older than that is a reply overtaken by its own save.
  const [seen, setSeen] = useState(initial.version);
  const [taking, setTaking] = useState(false);
  if (initial.version !== seen) {
    setSeen(initial.version);
    if (initial.version > base.version) {
      if (!dirty || taking) {
        setTaking(false);
        takeNewest(initial);
      } else {
        setSaveError({ message: `${initial.name} was changed elsewhere (v${initial.version}).`, stale: true });
      }
    }
  }
  // "Load the newest version": the one in hand when it is newer, else read again and take that.
  const loadNewest = () => {
    if (initial.version > base.version) takeNewest(initial);
    else {
      setTaking(true);
      onReload();
    }
  };

  /**
   * A save, switch, reset or revert came back: the type as it now stands, and the draft follows
   * unless told to keep it — and kept only when it holds edits. An untouched draft follows too: the
   * switch on a built-in nobody edited makes the copy, whose rules now have ids, and a draft left
   * without them would read as changed and offer a Save that writes the built-in again. A draft with
   * edits takes the ids its rules were given, matched by their text, for the same reason.
   */
  const took = (t: ReviewType, keepDraft = false) => {
    setBase(t);
    if (!keepDraft || !dirty) setDraft(draftOf(t));
    else
      setDraft((d) => ({
        ...d,
        rules: d.rules.map((r) => (r.id ? r : { ...r, id: t.rules.find((b) => b.text === r.text)?.id })),
      }));
    setSaveError(null);
    onChanged();
  };

  const save = async () => {
    const globs = globsRef.current?.flush() ?? draft.path_globs;
    const d = { ...draft, path_globs: globs };
    if (problems(d).length > 0 || busy) return;
    setBusy("save");
    setSaveError(null);
    try {
      const out = await api.put<{ type: ReviewType }>(`/api/review-types/${encodeURIComponent(base.key)}`, typeInput(d, base, canReach));
      took(out.type);
      toast.success(`${out.type.name} saved as v${out.type.version}`);
    } catch (err) {
      // Told by the status, not the words: the server's sentence for a lost race may change.
      setSaveError({ message: errorMessage(err), stale: err instanceof ApiError && err.status === 409 });
    } finally {
      setBusy(null);
    }
  };

  const toggleEnabled = async (on: boolean) => {
    setBusy("switch");
    try {
      const out = await api.post<{ type: ReviewType }>(`/api/review-types/${encodeURIComponent(base.key)}/${on ? "enable" : "disable"}`);
      // Its own switch, not an edit: what is being edited stays as it is, against the new version.
      took(out.type, true);
      toast.success(on ? `${base.name} is on` : `${base.name} is off`);
    } catch (err) {
      toast.error(errorMessage(err));
    } finally {
      setBusy(null);
    }
  };

  const reset = async () => {
    const ok = await confirm({
      title: `Reset ${base.name} to the built-in?`,
      description:
        "Its purpose, files, strictness, model and every rule go back to what attest_tag ships, team and learned rules included. It is saved as the next version, so Revert can bring this one back." +
        (dirty ? " Unsaved edits here are discarded." : ""),
      confirmLabel: "Reset to built-in",
      destructive: true,
    });
    if (!ok) return;
    setBusy("reset");
    try {
      const out = await api.post<{ type: ReviewType }>(`/api/review-types/${encodeURIComponent(base.key)}/reset`);
      took(out.type);
      toast.success(`${out.type.name} is the built-in again, as v${out.type.version}`);
    } catch (err) {
      toast.error(errorMessage(err));
    } finally {
      setBusy(null);
    }
  };

  // New built-in rules the copy predates run as shipped; switching one off puts it in the list, off.
  const offNew = (text: string) => draft.rules.some((r) => r.text === text && !base.rules.some((b) => b.text === text));
  const toggleNew = (rule: ReviewTypeRule, on: boolean) =>
    setDraft((d) =>
      on
        ? { ...d, rules: d.rules.filter((r) => !(r.text === rule.text && !base.rules.some((b) => b.text === rule.text))) }
        : { ...d, rules: [...d.rules, { ...ruleDraft(rule), enabled: false }] },
    );

  const modelChoices = [
    { value: INHERIT, label: "The settings' model" },
    // "default" names the deployment's default model outright, where empty takes whatever the
    // settings run (review_engine.go's reviewDefaultModel).
    { value: "default", label: "The default model" },
    { value: "heavy", label: modelLabel("heavy", heavy) },
    ...offeredModels.map((m) => ({ value: m, label: m })),
    ...(draft.model && draft.model !== "heavy" && draft.model !== "default" && !offeredModels.includes(draft.model)
      ? [{ value: draft.model, label: `${draft.model} (no longer offered)` }]
      : []),
  ];

  return (
    <div className="min-w-0 space-y-6">
      {/* Who it is and how it stands; the actions that save at once sit here, apart from the draft. */}
      <div className="space-y-3">
        <div className="flex flex-wrap items-start gap-3">
          <span className="flex size-9 shrink-0 items-center justify-center rounded-md bg-accent text-accent-foreground">
            <ListChecks className="size-5" />
          </span>
          <div className="min-w-0 flex-1">
            <h2 className="flex flex-wrap items-center gap-2 text-base leading-tight font-semibold">
              <span className="truncate">{base.name}</span>
              <StatusChip variant={kind.variant}>{kind.label}</StatusChip>
              {!base.enabled && <StatusChip variant="warning">Off</StatusChip>}
            </h2>
            <p className="text-xs text-muted-foreground">
              <span className="font-mono">{base.key}</span>
              {base.version > 0 ? (
                <>
                  {" · "}v{base.version}
                  {base.updated_by ? ` saved by ${base.updated_by}` : ""}
                  {base.updated_at ? (
                    <>
                      {" "}
                      <RelativeTime value={base.updated_at} />
                    </>
                  ) : null}
                </>
              ) : (
                " · as it ships"
              )}
              {" · "}
              <UsedBy typeKey={base.key} count={base.used_by} />
            </p>
          </div>
          <label className="flex items-center gap-2 text-sm">
            <Switch
              checked={base.enabled}
              disabled={ro || busy === "switch"}
              onCheckedChange={(on) => void toggleEnabled(on)}
              aria-label={base.enabled ? `Turn ${base.name} off` : `Turn ${base.name} on`}
            />
            {base.enabled ? "On" : "Off"}
          </label>
        </div>
        {!base.enabled && (
          <p className="rounded-lg border border-warning/30 bg-warning-soft px-3 py-2 text-xs text-foreground">
            Turned off: it is not offered in Start review, and{" "}
            {base.used_by > 0
              ? `the ${base.used_by === 1 ? "branch rule that names it skips" : `${base.used_by} branch rules that name it skip`} it, listing it under Not reviewed.`
              : "no branch rule names it."}{" "}
            Its rules and history are kept.
          </p>
        )}
        <div className="flex flex-wrap gap-2">
          {canManage && (
            <Button variant="outline" size="sm" onClick={() => setTryOpen(true)} disabled={issues.length > 0}>
              <FlaskConical /> Try on a PR{dirty ? " (unsaved)" : ""}
            </Button>
          )}
          <Button variant="outline" size="sm" onClick={() => setHistoryOpen(true)}>
            <History /> History
          </Button>
          {canManage && base.builtin && base.version > 0 && (
            <Button variant="outline" size="sm" onClick={() => void reset()} disabled={!base.edited || busy === "reset"}>
              <RotateCcw /> Reset to built-in
            </Button>
          )}
          {/* Said beside it, not in a title: a disabled button shows none, and a phone never hovers. */}
          {canManage && base.builtin && base.version > 0 && !base.edited && (
            <span className="self-center text-xs text-muted-foreground">It says what the built-in says already.</span>
          )}
        </div>
      </div>

      {ro && (
        <p className="rounded-lg border bg-muted/40 px-3 py-2 text-xs text-muted-foreground">
          You can read this type. Changing it needs Manage reviews; its model and money need Manage connections as well.
        </p>
      )}

      <form
        className="space-y-6"
        onSubmit={(e) => {
          e.preventDefault();
          void save();
        }}
      >
        <SettingsGroup title="What it is">
          <SettingsSection title="Name" description="How the console, the summary and every inline comment name it.">
            <Input
              id="type-name"
              aria-label="Name"
              value={draft.name}
              disabled={ro}
              maxLength={60}
              onChange={(e) => set("name", e.target.value)}
              className="h-8 max-w-sm"
            />
            <p className="mt-1.5 text-xs text-muted-foreground">
              Key <span className="font-mono text-foreground">{base.key}</span>, fixed: branch rules, Start review and{" "}
              <span className="font-mono">@… review {base.key}</span> name it by this.
            </p>
          </SettingsSection>
          <SettingsSection
            title="What this review is for"
            description="One paragraph the reviewer reads before anything else. Written by your team and trusted as such: it can add no tool and change nothing about posting."
          >
            <Textarea
              id="type-purpose"
              aria-label="What this review is for"
              value={draft.purpose}
              disabled={ro}
              onChange={(e) => set("purpose", e.target.value)}
              className="min-h-24"
            />
            <p className={cn("mt-1 text-right text-xs tabular-nums text-muted-foreground", draft.purpose.length > MAX_PURPOSE && "text-danger")}>
              {draft.purpose.length.toLocaleString("en-US")} / {MAX_PURPOSE.toLocaleString("en-US")}
            </p>
          </SettingsSection>
          <SettingsSection title="Only look at files" description="Path globs such as src/** or **/*.sql. Empty reads every changed file.">
            <ChipsInput
              ref={globsRef}
              id="type-globs"
              value={draft.path_globs}
              disabled={ro}
              onChange={(v) => set("path_globs", v)}
              placeholder="Every file"
            />
          </SettingsSection>
        </SettingsGroup>

        <SettingsGroup title="How it judges">
          <SettingsSection title="Strictness" description="How sure a finding must be to be kept. Empty takes the settings' strictness.">
            <Select value={draft.strictness || INHERIT} disabled={ro} onValueChange={(v) => set("strictness", v === INHERIT ? "" : v)}>
              <SelectTrigger id="type-strictness" aria-label="Strictness" size="sm" className="w-full max-w-sm">
                <SelectValue />
              </SelectTrigger>
              <SelectContent>
                <SelectItem value={INHERIT}>The settings&apos; strictness</SelectItem>
                {STRICTNESS.map((s) => (
                  <SelectItem key={s.value} value={s.value}>
                    {s.label} — {s.hint}
                  </SelectItem>
                ))}
              </SelectContent>
            </Select>
          </SettingsSection>
          <SettingsSection
            title="Inline comments"
            description="The least severe finding commented on the diff. The rest are listed in the summary under More notes."
          >
            <Select value={draft.inline_min_severity || INHERIT} disabled={ro} onValueChange={(v) => set("inline_min_severity", v === INHERIT ? "" : v)}>
              <SelectTrigger id="type-inline" aria-label="Inline comments" size="sm" className="w-full max-w-sm">
                <SelectValue />
              </SelectTrigger>
              <SelectContent>
                <SelectItem value={INHERIT}>Every severity</SelectItem>
                <SelectItem value="P1">P1 and P0 only</SelectItem>
                <SelectItem value="P0">P0 only</SelectItem>
              </SelectContent>
            </Select>
          </SettingsSection>
          <SettingsSection
            title="Model and money"
            description="A model of its own for this type's pass, and the most one review may spend on it. Empty takes the settings'."
          >
            <div className="grid max-w-xl gap-3 sm:grid-cols-[1fr_9rem]">
              <div className="space-y-1">
                <Label htmlFor="type-model" className="text-xs text-muted-foreground">
                  Model
                </Label>
                <Select value={draft.model || INHERIT} disabled={ro || !canReach} onValueChange={(v) => set("model", v === INHERIT ? "" : v)}>
                  <SelectTrigger id="type-model" size="sm" className="w-full">
                    <SelectValue />
                  </SelectTrigger>
                  <SelectContent>
                    {modelChoices.map((m) => (
                      <SelectItem key={m.value} value={m.value}>
                        {m.label}
                      </SelectItem>
                    ))}
                  </SelectContent>
                </Select>
              </div>
              <div className="space-y-1">
                <Label htmlFor="type-max" className="text-xs text-muted-foreground">
                  Max $ per review
                </Label>
                <div className="relative">
                  <span className="pointer-events-none absolute top-1/2 left-3 -translate-y-1/2 text-sm text-muted-foreground">$</span>
                  <Input
                    id="type-max"
                    inputMode="decimal"
                    value={draft.max_usd}
                    placeholder="settings'"
                    disabled={ro || !canReach}
                    aria-invalid={parseUSD(draft.max_usd) === null}
                    onChange={(e) => set("max_usd", e.target.value)}
                    className="h-8 pl-7 tabular-nums"
                  />
                </div>
              </div>
            </div>
            {!canReach && canManage && <p className="mt-1.5 text-xs text-warning">{REACH_REASON} to change these.</p>}
          </SettingsSection>
        </SettingsGroup>

        <SkillsList skills={draft.skills} saved={base.skills ?? []} ro={ro} canCheck={canManage} onChange={(v) => set("skills", v)} />

        <RulesList
          draft={draft}
          base={base}
          ro={ro}
          setRule={setRule}
          moveRule={moveRule}
          dropRule={dropRule}
          addRule={addRule}
          added={added}
          onSubmit={() => void save()}
        />

        {(base.new_builtin_rules?.length ?? 0) > 0 && (
          <section className="space-y-2 rounded-xl border border-info/30 bg-info-soft/50 p-4">
            <h3 className="flex items-center gap-2 text-sm font-semibold">
              <Sparkles className="size-4 text-info" /> New built-in rules
            </h3>
            <p className="text-xs text-muted-foreground">
              Rules a later release of the built-in added, which your copy predates. They run as shipped; switch one off to keep
              it off in your copy.
            </p>
            <ul className="space-y-2">
              {base.new_builtin_rules!.map((r) => {
                const on = !offNew(r.text);
                return (
                  <li key={r.text} className="flex items-start gap-3 text-sm">
                    <Switch
                      checked={on}
                      disabled={ro}
                      onCheckedChange={(v) => toggleNew(r, v)}
                      aria-label={on ? "Switch this new rule off" : "Switch this new rule on"}
                      className="mt-1"
                    />
                    <span className={cn("min-w-0 flex-1", !on && "text-muted-foreground line-through")}>{r.text}</span>
                    {r.severity_cap && <StatusChip variant="neutral">≤ {r.severity_cap}</StatusChip>}
                  </li>
                );
              })}
            </ul>
          </section>
        )}

        {/* The save bar follows the form down a long rule list, so Save is never a scroll away. */}
        {canManage && (
          <div className="sticky bottom-0 z-10 -mx-1 flex flex-wrap items-center gap-2 border-t bg-background/95 px-1 py-3 backdrop-blur">
            <div className="min-w-0 flex-1 text-xs">
              {saveError ? (
                <span className="text-danger">
                  {saveError.message}{" "}
                  {saveError.stale && (
                    <button type="button" onClick={loadNewest} className="font-medium underline underline-offset-2">
                      Load the newest version
                    </button>
                  )}
                </span>
              ) : issues.length > 0 ? (
                <span className="text-danger">{issues[0]}{issues.length > 1 ? ` (and ${issues.length - 1} more)` : ""}</span>
              ) : dirty ? (
                <span className="text-muted-foreground">Unsaved changes. Every save is a version; runs keep naming the one they used.</span>
              ) : (
                <span className="text-muted-foreground">No changes.</span>
              )}
            </div>
            {dirty && (
              <Button
                variant="ghost"
                size="sm"
                // Discarding over a version saved elsewhere lands on that one, not on the version the
                // edits were made against, which is no longer the type.
                onClick={() => (initial.version > base.version ? takeNewest(initial) : setDraft(draftOf(base)))}
              >
                Discard
              </Button>
            )}
            <Button type="submit" size="sm" disabled={!dirty || issues.length > 0} loading={busy === "save"}>
              Save as v{next}
            </Button>
          </div>
        )}
      </form>

      <ReviewTypeHistoryDialog
        open={historyOpen}
        onOpenChange={setHistoryOpen}
        type={base}
        canManage={canManage}
        dirty={dirty}
        onReverted={(t) => took(t)}
      />
      {tryOpen && (
        <TryTypeDialog
          open={tryOpen}
          onOpenChange={setTryOpen}
          typeName={draft.name.trim() || base.name}
          versionNote={dirty ? `v${base.version || 0} with your unsaved changes` : base.version > 0 ? `v${base.version}` : "as it ships"}
          input={() => ({ key: base.key, ...typeInput(draft, base, canReach) })}
        />
      )}
      {confirmDialog}
    </div>
  );
}

function RulesList({
  draft,
  base,
  ro,
  setRule,
  moveRule,
  dropRule,
  addRule,
  added,
  onSubmit,
}: {
  draft: TypeDraft;
  base: ReviewType;
  ro: boolean;
  setRule: (uid: string, patch: Partial<RuleDraft>) => void;
  moveRule: (i: number, by: -1 | 1) => void;
  dropRule: (uid: string) => void;
  addRule: () => void;
  /** The uid of a rule just added, to be focused once, as it mounts. */
  added: React.RefObject<string | null>;
  onSubmit: () => void;
}) {
  // Which rules have their paths and examples open: a saved rule's remembered in this browser by
  // its id, an unsaved one's only while it is on screen — its uid is a counter that starts again on
  // every load, and stored it would open some other rule next time.
  const { value: open, toggle } = useStickySet<string>("reviews:rule-details");
  const [openUnsaved, setOpenUnsaved] = useState<Set<string>>(new Set());
  const isOpen = (r: RuleDraft) => (r.id ? open.has(r.id) : openUnsaved.has(r.uid));
  const flip = (r: RuleDraft) => {
    if (r.id) toggle(r.id);
    else
      setOpenUnsaved((prev) => {
        const next = new Set(prev);
        if (!next.delete(r.uid)) next.add(r.uid);
        return next;
      });
  };
  const proposed = draft.rules.filter((r) => r.status === "proposed").length;
  const full = draft.rules.length >= MAX_RULES;
  return (
    <section className="space-y-2">
      <div className="flex flex-wrap items-baseline justify-between gap-2 px-1">
        <h2 className="text-sm font-semibold">
          Rules{" "}
          <span className={cn("font-normal tabular-nums text-muted-foreground", draft.rules.length > MAX_RULES && "text-danger")}>
            {draft.rules.length} of {MAX_RULES}
          </span>
        </h2>
        <span className="text-xs text-muted-foreground">
          {proposed > 0 ? `${proposed} proposed from replies on GitHub, waiting for Approve · ` : ""}Findings cite them as R1, R2… by
          place.
        </span>
      </div>
      <ol className="divide-y overflow-hidden rounded-xl border bg-card">
        {draft.rules.map((r, i) => {
          const key = r.id ?? r.uid;
          const expanded = isOpen(r);
          const saved = base.rules.find((b) => (r.id ? b.id === r.id : b.text === r.text));
          const wasProposed = saved?.status === "proposed";
          const off = !r.enabled || r.status === "rejected";
          const extras = r.path_globs.length + (r.example_bad ? 1 : 0) + (r.example_good ? 1 : 0);
          return (
            <li key={r.uid} className={cn("space-y-2 px-3 py-3", r.status === "proposed" && "bg-warning-soft/40")}>
              <div className="flex items-start gap-2.5">
                <span className="mt-2 w-7 shrink-0 text-xs font-medium tabular-nums text-muted-foreground">R{i + 1}</span>
                <Switch
                  checked={r.enabled && r.status !== "rejected"}
                  disabled={ro || r.status === "proposed" || r.status === "rejected"}
                  onCheckedChange={(on) => setRule(r.uid, { enabled: on })}
                  aria-label={`R${i + 1} ${r.enabled ? "on" : "off"}`}
                  className="mt-2.5"
                />
                <div className="min-w-0 flex-1 space-y-1.5">
                  <Textarea
                    ref={(el) => {
                      if (el && added.current === r.uid) {
                        added.current = null;
                        el.focus();
                      }
                    }}
                    aria-label={`Rule R${i + 1}`}
                    value={r.text}
                    disabled={ro}
                    rows={1}
                    placeholder="One thing the reviewer must check, in a sentence."
                    onChange={(e) => setRule(r.uid, { text: e.target.value.replace(/[\r\n]+/g, " ") })}
                    onKeyDown={(e) => {
                      // A rule is one line, so Enter is the form's: it saves, as every console form does.
                      if (e.key === "Enter" && !e.shiftKey) {
                        e.preventDefault();
                        onSubmit();
                      }
                    }}
                    className={cn("min-h-9 py-1.5", off && "text-muted-foreground")}
                    aria-invalid={r.text.trim().length === 0 || r.text.trim().length > MAX_RULE}
                  />
                  <div className="flex flex-wrap items-center gap-1.5">
                    <Select value={r.severity_cap || NO_CAP} disabled={ro} onValueChange={(v) => setRule(r.uid, { severity_cap: v === NO_CAP ? "" : v })}>
                      <SelectTrigger size="sm" className="h-7 w-auto gap-1 px-2 text-xs" aria-label={`R${i + 1}'s severity cap`}>
                        <SelectValue />
                      </SelectTrigger>
                      <SelectContent>
                        <SelectItem value={NO_CAP}>No cap</SelectItem>
                        <SelectItem value="P0">At most P0</SelectItem>
                        <SelectItem value="P1">At most P1</SelectItem>
                        <SelectItem value="P2">At most P2</SelectItem>
                      </SelectContent>
                    </Select>
                    <span className="rounded-sm bg-secondary px-1.5 py-0.5 text-xs text-secondary-foreground">{SOURCE[r.source] ?? r.source}</span>
                    {r.status === "proposed" && <StatusChip variant="warning">Proposed</StatusChip>}
                    {r.status === "rejected" && <StatusChip variant="neutral">Rejected</StatusChip>}
                    {wasProposed && r.status === "active" && <StatusChip variant="success">Approved, unsaved</StatusChip>}
                    {r.text.trim().length > MAX_RULE - 50 && (
                      <span className={cn("text-xs tabular-nums", r.text.trim().length > MAX_RULE ? "text-danger" : "text-muted-foreground")}>
                        {r.text.trim().length}/{MAX_RULE}
                      </span>
                    )}
                    <button
                      type="button"
                      onClick={() => flip(r)}
                      aria-expanded={expanded}
                      className="inline-flex items-center gap-0.5 text-xs text-muted-foreground hover:text-foreground"
                    >
                      <ChevronRight className={cn("size-3.5 transition-transform", expanded && "rotate-90")} />
                      Paths and examples{extras > 0 ? ` (${extras})` : ""}
                    </button>
                    {r.from_comment_url && (
                      <a href={r.from_comment_url} target="_blank" rel="noreferrer" className="inline-flex items-center gap-0.5 text-xs text-primary underline-offset-2 hover:underline">
                        from a reply <ExternalLink className="size-3" />
                      </a>
                    )}
                    <span className="flex-1" />
                    {!ro && (
                      <span className="flex items-center">
                        <Button variant="ghost" size="icon-xs" disabled={i === 0} onClick={() => moveRule(i, -1)} aria-label={`Move R${i + 1} up`}>
                          <ArrowUp />
                        </Button>
                        <Button
                          variant="ghost"
                          size="icon-xs"
                          disabled={i === draft.rules.length - 1}
                          onClick={() => moveRule(i, 1)}
                          aria-label={`Move R${i + 1} down`}
                        >
                          <ArrowDown />
                        </Button>
                        {/* A built-in rule is switched off rather than deleted: the copy keeps it, off. */}
                        {r.source !== "builtin" && (
                          <Button variant="ghost" size="icon-xs" onClick={() => dropRule(r.uid)} aria-label={`Delete R${i + 1}`}>
                            <Trash2 />
                          </Button>
                        )}
                      </span>
                    )}
                  </div>
                  {r.status === "proposed" && !ro && (
                    <div className="flex flex-wrap items-center gap-2">
                      <Button size="xs" onClick={() => setRule(r.uid, { status: "active", enabled: true })}>
                        <Check /> Approve
                      </Button>
                      <Button size="xs" variant="outline" onClick={() => setRule(r.uid, { status: "rejected", enabled: false })}>
                        <X /> Reject
                      </Button>
                      <span className="text-xs text-muted-foreground">Does nothing until it is approved and saved.</span>
                    </div>
                  )}
                  {wasProposed && r.status !== "proposed" && !ro && (
                    <button type="button" onClick={() => setRule(r.uid, { status: "proposed", enabled: saved?.enabled ?? true })} className="text-xs text-muted-foreground underline underline-offset-2 hover:text-foreground">
                      Undo
                    </button>
                  )}
                  {expanded && (
                    <div className="space-y-2 rounded-lg border bg-muted/30 p-2.5">
                      <div className="space-y-1">
                        <Label htmlFor={`rule-${key}-globs`} className="text-xs text-muted-foreground">
                          Only for files
                        </Label>
                        <ChipsInput
                          id={`rule-${key}-globs`}
                          value={r.path_globs}
                          disabled={ro}
                          onChange={(v) => setRule(r.uid, { path_globs: v })}
                          placeholder="Every file the type reads"
                        />
                      </div>
                      <div className="grid gap-2 md:grid-cols-2">
                        <div className="space-y-1">
                          <Label htmlFor={`rule-${key}-bad`} className="text-xs text-muted-foreground">
                            Code it should flag
                          </Label>
                          <Textarea
                            id={`rule-${key}-bad`}
                            value={r.example_bad}
                            disabled={ro}
                            onChange={(e) => setRule(r.uid, { example_bad: e.target.value })}
                            className="min-h-16 font-mono text-xs md:text-xs"
                          />
                        </div>
                        <div className="space-y-1">
                          <Label htmlFor={`rule-${key}-good`} className="text-xs text-muted-foreground">
                            Code it should accept
                          </Label>
                          <Textarea
                            id={`rule-${key}-good`}
                            value={r.example_good}
                            disabled={ro}
                            onChange={(e) => setRule(r.uid, { example_good: e.target.value })}
                            className="min-h-16 font-mono text-xs md:text-xs"
                          />
                        </div>
                      </div>
                    </div>
                  )}
                </div>
              </div>
            </li>
          );
        })}
        {draft.rules.length === 0 && (
          <li className="px-4 py-6 text-center text-sm text-muted-foreground">
            No rules yet: the reviewer goes by what the type is for alone.
          </li>
        )}
      </ol>
      {!ro && (
        <div className="flex items-center gap-3 px-1">
          <Button variant="outline" size="sm" onClick={addRule} disabled={full}>
            <Plus /> Add rule
          </Button>
          {full && <span className="text-xs text-muted-foreground">Forty is the most one type holds: split it in two.</span>}
        </div>
      )}
    </section>
  );
}

/**
 * "named by 3 branch rules", opening onto those rules: which levels of Reviews › Settings name the
 * type, each a link to its level. Read from the settings tree when opened, counted as the server
 * counts used_by — every level's own rules, a rule naming no type counting as General.
 */
function UsedBy({ typeKey, count }: { typeKey: string; count: number }) {
  const [open, setOpen] = useState(false);
  const words = count === 0 ? "named by no branch rule" : count === 1 ? "named by 1 branch rule" : `named by ${count} branch rules`;
  if (count === 0) return <>{words}</>;
  return (
    <Popover open={open} onOpenChange={setOpen}>
      <PopoverTrigger asChild>
        <button type="button" className="underline underline-offset-2 hover:text-foreground">
          {words}
        </button>
      </PopoverTrigger>
      <PopoverContent align="start" className="w-80 p-0">
        {open && <UsedByList typeKey={typeKey} />}
      </PopoverContent>
    </Popover>
  );
}

function UsedByList({ typeKey }: { typeKey: string }) {
  const tree = useApi<ReviewSettingsTree>("/api/review-settings");
  if (tree.error && !tree.data) return <p className="px-3 py-2 text-xs text-danger">{tree.error}</p>;
  if (!tree.data) return <Skeleton className="m-3 h-10" />;
  const rows: { where: string; rule: string; params: Record<string, string> }[] = [];
  const visit = (n: ReviewNode, where: string, params: Record<string, string>) =>
    (n.settings?.branch_rules ?? []).forEach((r, i) => {
      const keys = r.types?.length ? r.types : ["general"];
      if (keys.some((k) => k.toLowerCase() === typeKey)) rows.push({ where, rule: `${i + 1}. ${ruleLabel(r)}`, params });
    });
  for (const c of tree.data.connections) {
    visit(c, connectionName(c), { node: c.id });
    for (const g of c.groups) {
      visit(g, `${connectionName(c)} › ${g.name}`, { node: g.id });
      for (const r of g.repos) visit(r, r.repo ?? "", { node: r.repo ?? "", conn: c.id });
    }
    for (const r of c.repos) visit(r, r.repo ?? "", { node: r.repo ?? "", conn: c.id });
  }
  return (
    <ul className="max-h-72 divide-y overflow-y-auto">
      {rows.map((row, i) => (
        <li key={i}>
          <ReviewsTabLink tab="settings" params={row.params} className="block px-3 py-2 hover:bg-secondary/60">
            <span className="block truncate text-sm font-medium">{row.where}</span>
            <span className="block truncate font-mono text-xs text-muted-foreground">{row.rule}</span>
          </ReviewsTabLink>
        </li>
      ))}
      {rows.length === 0 && <li className="px-3 py-2 text-xs text-muted-foreground">No level names it any more.</li>}
    </ul>
  );
}
