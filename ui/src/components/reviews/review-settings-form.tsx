"use client";

import { useState } from "react";
import { toast } from "sonner";
import { SettingsGroup, SettingsSection } from "@/components/core/settings-section";
import { BranchRulesEditor } from "@/components/reviews/branch-rules-editor";
import {
  ChipsListField,
  ChoiceField,
  ContextReposField,
  InstructionsField,
  NotifyField,
  NotifyOnField,
  TextValueField,
  type FieldEnv,
} from "@/components/reviews/review-fields";
import {
  DRAFTS,
  FORKS,
  MODES,
  STRICTNESS,
  TRIGGERS,
  ancestorsOf,
  channelLabel,
  effectiveSummary,
  modelLabel,
  sourceLabel,
  usd,
  type Choice,
} from "@/components/reviews/review-format";
import {
  ApiError,
  api,
  errorMessage,
  useApi,
  type ReviewNodeDetail,
  type ReviewSettingsTree,
  type ReviewSettingsValues,
  type ReviewTypeSummary,
  type Scope,
} from "@/lib/api";

// Every setting of one level of the tree, in the order a team decides them: whether and when it is
// reviewed, who hears about it, which reviews run on which branches, how much it says, and — last,
// and only for whoever holds connections.manage — what it may read and spend. Each field saves on
// its own, as the Workspaces page does: a select the moment it changes, a box or a list with its own
// Save, so Enter in it saves it. A save names the one field it changes (`fields`) and sends only
// that, and the server keeps every other one as stored: a page open since somebody else saved —
// their branch rules, say — puts back nothing it showed, and the admin-only values it never touched
// are never the reason an editor is refused. The branch rules are the one field saved whole by people
// who each change a rule of it, so their save also says which list it replaces (`expect`): a list
// somebody else saved since is refused, not overwritten, and the editor keeps its draft over the
// list read again.

export function ReviewSettingsForm({
  path,
  level,
  detail,
  tree,
  types,
  orgRepos,
  offeredModels,
  heavy,
  canManage,
  canReach,
  stopped,
  onSaved,
  onStale,
}: {
  /** The level's settings route, which PUT replaces. */
  path: string;
  level: "connection" | "group" | "repo";
  detail: ReviewNodeDetail;
  tree: ReviewSettingsTree;
  types: ReviewTypeSummary[];
  /** The organisation's repositories connected through the App: what context may name. */
  orgRepos: string[];
  /** The models Settings offers to channels, which code review may also run on. */
  offeredModels: string[];
  heavy: string;
  canManage: boolean;
  canReach: boolean;
  /** Why the level's connection reviews nothing whatever these say — stopped, uninstalled, suspended. */
  stopped?: string;
  onSaved: (detail: ReviewNodeDetail) => void;
  /** A save was refused because what it replaces changed since it was read: read the level again. */
  onStale: () => void;
}) {
  const [busy, setBusy] = useState<string | null>(null);
  const own: ReviewSettingsValues = detail.own ?? {};
  const ancestors = ancestorsOf(detail, tree);
  const inherited = detail.inherited;
  const idPrefix = `review-${detail.node.id || detail.node.repo || "node"}`;
  // The channels the bot is in, read once for the channel field and every branch rule's: null
  // while asked, so the pickers wait for this answer rather than each asking again. Only those the
  // server takes are offered — a workspace disconnected since keeps its channels in the list.
  const scopes = useApi<Scope[]>("/api/scopes?sync=0");
  const allowed = tree.channels;
  const channels = scopes.data
    ? scopes.data.filter(
        (s) => s.kind !== "channel" || !allowed || allowed.some((a) => a.team === s.team_id && a.channel === s.slack_id),
      )
    : scopes.error
      ? undefined
      : null;

  const save: FieldEnv["save"] = async (field, value) => {
    // The field alone: left out of settings while named in fields, it is reset to inheriting.
    const settings: ReviewSettingsValues = value === undefined ? {} : { [field]: value };
    // The branch rules are saved whole, so their save says which list it replaces. Somebody else's
    // saved since — here, or above where this level inherits — is refused rather than dropped.
    const digest = field === "branch_rules" ? detail.digests?.branch_rules : undefined;
    setBusy(field);
    try {
      const out = await api.put<ReviewNodeDetail>(path, {
        settings,
        fields: [field],
        ...(digest ? { expect: { branch_rules: digest } } : {}),
      });
      onSaved(out);
      toast.success(value === undefined ? "Reset to inherited" : "Saved");
      return true;
    } catch (err) {
      // Told by the status, not the words: the server's sentence for a lost race may change. The
      // level is read again, and the rules editor keeps whatever was being edited over it. Any other
      // field meets one only when somebody saved this level while the save was being checked, which
      // it would otherwise have put back.
      if (err instanceof ApiError && err.status === 409) {
        toast.error(
          digest
            ? "Branch rules changed since you opened them"
            : "Somebody saved this level while yours was being checked; nothing was saved",
        );
        onStale();
      } else {
        toast.error(errorMessage(err));
      }
      return false;
    } finally {
      setBusy(null);
    }
  };

  const env: FieldEnv = { idPrefix, own, inherited, ancestors, canManage, canReach, busy, save };
  // A level that is off, turned on: whatever its settings would then do is switched on by the turn,
  // so where that is reviewing every push or a rule posting live, Shadow needs connections.manage as
  // much as Live does (reviewTierNeeds judges an off level as reviewing nothing).
  const eff = detail.effective;
  // Telling a channel, or a rule that picks a model, is switched on by the turn the same way.
  const waking =
    !canReach &&
    eff.mode === "off" &&
    (eff.trigger === "push" ||
      !!eff.notify?.channel ||
      (eff.branch_rules ?? []).some((r) => r.post === "live" || r.trigger === "push" || !!r.model || !!r.notify?.channel));
  const below = level === "repo" ? "" : " Everything below inherits it unless it sets its own.";
  const modelChoices: Choice<string>[] = [
    { value: "heavy", label: modelLabel("heavy", heavy), hint: "The advanced model under Settings › Models." },
    // As a review type offers it: the everyday model by name, which the API takes as "default".
    { value: "default", label: "The default model", hint: "The everyday model under Settings › Models." },
    ...offeredModels.map((m) => ({ value: m, label: m, hint: "Offered to channels under Settings › Models." })),
    ...(own.model && own.model !== "heavy" && own.model !== "default" && !offeredModels.includes(own.model)
      ? [{ value: own.model, label: `${own.model} (no longer offered)`, hint: "Withdrawn under Settings since it was set." }]
      : []),
  ];

  return (
    <div className="space-y-6">
      {!canManage && (
        <p className="rounded-lg border bg-muted/40 px-3 py-2 text-xs text-muted-foreground">
          You can read these settings. Changing them needs Manage reviews; posting live, the channel, models and
          money need Manage connections as well.
        </p>
      )}

      <SettingsGroup title="Reviewing">
        <SettingsSection
          title="Review"
          description={`Whether pull requests are reviewed, and whether the result is posted or kept here.${below}`}
        >
          <ChoiceField
            env={env}
            field="mode"
            label="Review"
            choices={MODES}
            own={own.mode}
            inherited={inherited.mode}
            reach={waking ? ["shadow", "live"] : ["live"]}
            onPick={(v) => void save("mode", v)}
          />
          {waking && canManage && (
            <p className="mt-1.5 text-xs text-warning">
              Turned on, it would review every push, post live, announce in a channel or run a branch rule&apos;s own
              model, so turning it on needs Manage connections as well.
            </p>
          )}
        </SettingsSection>
        <SettingsSection
          title="When"
          description="What starts a review without anybody asking. A command or Start review works whatever this says."
        >
          <ChoiceField
            env={env}
            field="trigger"
            label="When to review"
            choices={TRIGGERS}
            own={own.trigger}
            inherited={inherited.trigger}
            reach={["push"]}
            onPick={(v) => void save("trigger", v)}
          />
        </SettingsSection>
        <SettingsSection title="Drafts" description="Whether a draft pull request is reviewed automatically.">
          <ChoiceField
            env={env}
            field="drafts"
            label="Drafts"
            choices={DRAFTS}
            own={own.drafts === undefined ? undefined : own.drafts ? "yes" : "no"}
            inherited={inherited.drafts ? "yes" : "no"}
            onPick={(v) => void save("drafts", v === undefined ? undefined : v === "yes")}
          />
        </SettingsSection>
        <SettingsSection
          title="Forks"
          description="A pull request from a fork is a stranger's code on the organisation's money, so it is never reviewed automatically."
        >
          <ChoiceField
            env={env}
            field="forks"
            label="Forks"
            choices={FORKS}
            own={own.forks}
            inherited={inherited.forks}
            reachAll
            onPick={(v) => void save("forks", v)}
          />
        </SettingsSection>
      </SettingsGroup>

      <SettingsGroup title="Notifications">
        <SettingsSection
          title="Channel"
          description="One message per pull request, edited as a review starts, finishes or fails and when it is merged; each of those but a start is also a reply in its thread."
        >
          <NotifyField env={env} channels={channels} />
        </SettingsSection>
        <SettingsSection
          title="Notify on"
          description="Which of those the channel hears of. The set is inherited whole: set here, it replaces the one above rather than adding to it."
        >
          <NotifyOnField env={env} effective={eff} />
        </SettingsSection>
      </SettingsGroup>

      <SettingsGroup title="Branch rules">
        <SettingsSection
          title="Which reviews run"
          description="Which review types run for a pull request, by the branch it comes from and the one it goes into, with anything those pull requests should do differently. The whole list is inherited or set here; lists are never merged."
        >
          <BranchRulesEditor
            id={`${idPrefix}-rules`}
            own={own.branch_rules}
            inherited={inherited.branch_rules ?? []}
            mode={eff.mode}
            trigger={eff.trigger}
            inheritedFrom={sourceLabel(inherited.source?.branch_rules, ancestors)}
            types={types}
            models={offeredModels}
            heavy={heavy}
            channels={channels}
            notify={channelLabel(eff.notify, channels ?? undefined)}
            canManage={canManage}
            canReach={canReach}
            busy={busy === "branch_rules"}
            onSave={(rules) => save("branch_rules", rules)}
          />
        </SettingsSection>
      </SettingsGroup>

      <SettingsGroup title="What it says">
        <SettingsSection title="Strictness" description="How sure a finding must be before it is posted.">
          <ChoiceField
            env={env}
            field="strictness"
            label="Strictness"
            choices={STRICTNESS}
            own={own.strictness}
            inherited={inherited.strictness}
            onPick={(v) => void save("strictness", v)}
          />
        </SettingsSection>
        <SettingsSection title="Comments per review" description="The most inline comments one review posts, from 1 to 20.">
          <TextValueField
            env={env}
            field="max_comments"
            label="Comments per review"
            type="number"
            min={1}
            max={20}
            step="1"
            stored={own.max_comments !== undefined ? String(own.max_comments) : ""}
            shown={String(inherited.max_comments)}
            placeholder={String(inherited.max_comments)}
            parse={(raw) => {
              const n = Number(raw);
              return Number.isInteger(n) && n >= 1 && n <= 20 ? n : null;
            }}
          />
        </SettingsSection>
        <SettingsSection
          title="Comment header"
          description="A line put at the top of every comment. Saving it empty turns off a header set above."
        >
          <TextValueField
            env={env}
            field="comment_header"
            label="Comment header"
            stored={own.comment_header ?? ""}
            shown={inherited.comment_header ? `“${inherited.comment_header}”` : "no header"}
            // Set here to nothing reads as that, not as the header it turned off above.
            placeholder={own.comment_header === "" ? "No header (turned off here)" : inherited.comment_header || "No header"}
            parse={(raw) => (raw.length <= 400 ? raw : null)}
            emptyIsValue
            className="max-w-sm"
          />
        </SettingsSection>
        <SettingsSection
          title="Instructions"
          description="What the team wants checked, one sentence each. They add up: a repository gets its connection's and its group's, then its own."
        >
          <InstructionsField env={env} />
        </SettingsSection>
        <SettingsSection
          title="Authors to skip"
          description="GitHub logins, or globs of them, never reviewed automatically — bots, mostly. A command still reviews them."
        >
          <ChipsListField
            env={env}
            field="exclude_authors"
            label="Authors"
            placeholder="renovate[bot], *-bot"
          />
        </SettingsSection>
        <SettingsSection title="Paths to ignore" description="Path globs left out of every review: generated code, lockfiles, vendored packages.">
          <ChipsListField
            env={env}
            field="ignore_paths"
            label="Paths"
            placeholder="**/*.lock, vendor/**"
          />
        </SettingsSection>
      </SettingsGroup>

      <SettingsGroup title="Reach and money">
        <SettingsSection
          title="Context repositories"
          description="Other repositories of the organisation a review may read — and so quote — when it checks a change against the code it calls."
        >
          <ContextReposField env={env} repos={orgRepos} />
        </SettingsSection>
        <SettingsSection
          title="Model"
          description="What the review runs on. A branch rule or a review type may pick another for its own pull requests."
        >
          <ChoiceField
            env={env}
            field="model"
            label="Model"
            choices={modelChoices}
            own={own.model}
            inherited={inherited.model}
            reachAll
            onPick={(v) => void save("model", v)}
          />
        </SettingsSection>
        <SettingsSection
          title="Most per review"
          description="What one review may spend, verification included. A review that reaches it stops and lists what it did not read."
        >
          <TextValueField
            env={env}
            field="max_usd"
            label="Most a review may spend, in USD"
            type="number"
            min={0.1}
            max={5}
            step="0.05"
            prefix="$"
            reachAll
            stored={own.max_usd !== undefined ? String(own.max_usd) : ""}
            shown={usd(inherited.max_usd)}
            placeholder={inherited.max_usd.toFixed(2)}
            parse={(raw) => {
              const n = Number(raw);
              return Number.isFinite(n) && n >= 0.1 && n <= 5 ? Math.round(n * 100) / 100 : null;
            }}
          />
        </SettingsSection>
      </SettingsGroup>

      <div className="rounded-xl border bg-card px-4 py-3">
        <p className="text-xs font-medium uppercase tracking-wide text-muted-foreground">
          {level === "repo" ? "Effective" : "Effective for a repository that sets nothing"}
        </p>
        <p className="mt-1 text-sm">
          {effectiveSummary(detail.effective, heavy, stopped, channelLabel(detail.effective.notify, channels ?? undefined))}
        </p>
      </div>
    </div>
  );
}
