"use client";

import { useState } from "react";
import { Building2, GitBranch, Hash, KeyRound, LogOut, Loader2, Lock, Package, PackageOpen, MessagesSquare, Trash2, Unplug } from "lucide-react";
import { toast } from "sonner";
import { AllowRulesEditor } from "@/components/core/allow-rules-editor";
import { AttachedChip } from "@/components/core/attached-chip";
import { useConfirm } from "@/components/core/confirm-dialog";
import { Disclosure } from "@/components/core/disclosure";
import { ErrorBanner } from "@/components/core/error-banner";
import { ModelCombobox } from "@/components/core/model-combobox";
import { SettingsGroup, SettingsSection } from "@/components/core/settings-section";
import { StatusChip } from "@/components/core/status-chip";
import { AccessSummaryTable } from "@/components/scopes/access-summary-table";
import { AddAccessPopover } from "@/components/scopes/add-access-popover";
import { ConnectRepoPopover, type TokenSource } from "@/components/scopes/connect-repo-popover";
import { RepoManagerDialog } from "@/components/bundles/repo-manager-dialog";
import { useInstallations } from "@/components/scopes/repo-sources";
import { ScopeRepoList, removableRepo } from "@/components/scopes/scope-repo-list";
import { Button } from "@/components/ui/button";
import { Input } from "@/components/ui/input";
import { Label } from "@/components/ui/label";
import {
  Select,
  SelectContent,
  SelectItem,
  SelectTrigger,
  SelectValue,
} from "@/components/ui/select";
import { Skeleton } from "@/components/ui/skeleton";
import { Textarea } from "@/components/ui/textarea";
import {
  api,
  errorMessage,
  useApi,
  type Bundle,
  type Connection,
  type InheritedInstructions,
  type InheritedRules,
  type RepoRow,
  type Scope,
  type ScopeDetail as Detail,
  type Team,
} from "@/lib/api";
import { scopeName } from "@/lib/format";

// Shared by the two three-way switches below: read every message, and member edits.
const INHERIT_OPTIONS: { value: string; label: string }[] = [
  { value: "inherit", label: "Inherit" },
  { value: "on", label: "On" },
  { value: "off", label: "Off" },
];

const MEMBER_EDIT_OPTIONS: { value: string; label: string; hint: string }[] = [
  { value: "inherit", label: "Inherit", hint: "Follow the workspace setting." },
  { value: "allow", label: "Allow", hint: "Channel members may change instructions and memory from Slack." },
  { value: "block", label: "Block", hint: "Only admins change this scope, here." },
];

// Radix Select cannot carry an empty value, so "inherit / none" rides on a sentinel.
const NO_REPO = "__inherit";

// One attached thing: a bundle, or a connection on its own shown as bundle/name.
// What a source contributes, headed by where it came from. An empty box in the
// console used to read as "the bot has no instructions here" when a channel in
// fact carried a page of them from the workspace and its bundles; these two
// readouts sit under the editable field and say what is already in force.
function InheritedCard({
  kind,
  source,
  note,
  children,
}: {
  kind: string;
  source: string;
  note?: string;
  children: React.ReactNode;
}) {
  const label = kind === "bundle" ? "Bundle" : kind === "settings" ? "Settings" : "Workspace";
  return (
    <li className="space-y-1.5 px-3 py-2.5">
      <div className="flex flex-wrap items-center gap-2">
        <StatusChip variant={kind === "bundle" ? "neutral" : "info"}>{label}</StatusChip>
        {/* Settings has no name of its own, so the chip already said it. */}
        {source !== label && <span className="min-w-0 truncate text-xs font-medium">{source}</span>}
        {note && <span className="text-xs text-muted-foreground">{note}</span>}
      </div>
      {children}
    </li>
  );
}

function InheritedInstructionsList({ items }: { items: InheritedInstructions[] }) {
  return (
    <ul className="divide-y overflow-hidden rounded-lg border">
      {items.map((it, i) => (
        <InheritedCard
          key={`${it.kind}-${it.source}-${i}`}
          kind={it.kind}
          source={it.source}
          note={it.kind === "bundle" && it.where === "workspace" ? "attached on the workspace" : undefined}
        >
          <p className="whitespace-pre-wrap break-words text-xs leading-relaxed text-muted-foreground">
            {it.text}
          </p>
        </InheritedCard>
      ))}
    </ul>
  );
}

function InheritedRulesList({ groups }: { groups: InheritedRules[] }) {
  return (
    <ul className="divide-y overflow-hidden rounded-lg border">
      {groups.map((g, i) => (
        <InheritedCard key={`${g.kind}-${g.source}-${i}`} kind={g.kind} source={g.source}>
          <ul className="space-y-1">
            {g.rules.map((rule, j) => (
              <li
                key={`${j}-${rule}`}
                className="whitespace-pre-wrap break-words text-xs leading-relaxed text-muted-foreground"
              >
                {rule}
              </li>
            ))}
          </ul>
        </InheritedCard>
      ))}
    </ul>
  );
}

// What actually makes answers better, grouped by what each line buys. Defaults come first
// because a named default saves the bot a discovery round trip on every question: told which
// list or repository is meant, it stops looking for one.
const INSTRUCTION_EXAMPLES: { group: string; lines: string[] }[] = [
  {
    group: "Defaults, so it stops hunting for ids",
    lines: [
      "Create ClickUp tasks in the backlog list unless someone names another list.",
      "Our repository is owner/name — assume it when a question does not say which.",
      "When someone reports a bug, search for an existing task before creating a new one.",
    ],
  },
  {
    group: "What every answer should carry",
    lines: [
      "Always include a link to anything you create or change, so people can open it.",
      "Quote the document or task you took an answer from; do not answer policy questions from memory.",
    ],
  },
  {
    group: "Limits",
    lines: [
      "Never comment on a pull request or close a task without asking first.",
      "This channel is customer-facing: no internal ticket numbers or names in answers.",
    ],
  },
];

// Examples under the instructions box: clicking one appends it, because a starting line people
// then edit teaches the shape far better than a description of it does.
function InstructionExamples({ onAdd }: { onAdd: (line: string) => void }) {
  return (
    <Disclosure
      className="pt-1"
      label="Examples"
      summary="what to write here"
      contentClassName="space-y-3"
    >
      <p className="text-xs text-muted-foreground">
        Click a line to add it, then edit it to fit. Instructions are plain sentences; the more
        specific, the fewer questions the bot has to ask or look up.
      </p>
      {INSTRUCTION_EXAMPLES.map((section) => (
        <div key={section.group} className="space-y-1.5">
          <p className="text-xs font-medium">{section.group}</p>
          <ul className="space-y-1">
            {section.lines.map((line) => (
              <li key={line}>
                <button
                  type="button"
                  onClick={() => onAdd(line)}
                  className="w-full rounded-md border px-2.5 py-1.5 text-left text-xs leading-relaxed text-muted-foreground transition-colors hover:bg-accent hover:text-accent-foreground"
                >
                  {line}
                </button>
              </li>
            ))}
          </ul>
        </div>
      ))}
    </Disclosure>
  );
}

// Right-hand panel of the Workspaces page for one scope, laid out like the Settings
// page: titled groups, one card each, heading and explanation on the left and
// the control on the right. Instructions come first because they are what
// people come here to change; then what the bot may reach, its repositories,
// how it behaves, and finally the resolved access table.
export function ScopeDetail({
  scope,
  team,
  bundles,
  onChanged,
  onRemoved,
  onRemovedWorkspace,
}: {
  scope: Scope;
  /** The install this scope sits in: status, who connected it, and what it granted. */
  team?: Team;
  bundles: Bundle[];
  onChanged: () => void;
  /** A channel just left the rail, so the page has to select something that is still in it. */
  onRemoved: (teamID: string) => void;
  /** This workspace and its channels have gone from the rail, so nothing here can be selected. */
  onRemovedWorkspace: () => void;
}) {
  const detail = useApi<Detail>(`/api/scopes/${scope.id}`);
  const { confirm, confirmDialog } = useConfirm();
  const [busy, setBusy] = useState(false);
  // The repository manager: the same dialog the Bundles page opens, over this scope's rows.
  const [managingRepos, setManagingRepos] = useState(false);
  const [addRepoOpen, setAddRepoOpen] = useState(false);

  const [instructions, setInstructions] = useState(scope.instructions);
  const [model, setModel] = useState(scope.default_model);
  const [budget, setBudget] = useState(String(scope.monthly_budget_usd ?? 0));
  const [allowRules, setAllowRules] = useState<string[]>(scope.allow_rules ?? []);
  const [savingField, setSavingField] = useState<string | null>(null);
  const budgetStored = String(scope.monthly_budget_usd ?? 0);

  const attachedBundles = scope.bundle_ids ?? [];
  const attachedConnections = scope.connection_ids ?? [];
  // Three levels now: the account, one connected Slack workspace, and a channel inside it.
  // isWide is "everything below inherits what I set here" — which is what most of the copy
  // turns on, and is true of the account and of a Slack workspace alike.
  const isAccount = scope.kind === "workspace";
  const isTeam = scope.kind === "team";
  const isWide = isAccount || isTeam;
  const Icon = isAccount ? Building2 : isTeam ? MessagesSquare : scope.is_private ? Lock : Hash;
  const below = isAccount ? "Every connected workspace" : "Every channel here";

  // A one-off connection is shown as bundle/name, so find it across every bundle.
  const connectionOf = (id: number): { bundle: Bundle; connection: Connection } | null => {
    for (const bundle of bundles) {
      const connection = (bundle.connections ?? []).find((c) => c.id === id);
      if (connection) return { bundle, connection };
    }
    return null;
  };

  const save = async (field: string, body: Record<string, string>, done: string) => {
    setSavingField(field);
    try {
      await api.put(`/api/scopes/${scope.id}`, body);
      toast.success(done);
      onChanged();
      detail.reload();
    } catch (err) {
      toast.error(errorMessage(err));
    } finally {
      setSavingField(null);
    }
  };

  // Attach or detach one thing, then refresh the scope list and the access table.
  const change = async (call: () => Promise<unknown>, done: string) => {
    setBusy(true);
    try {
      await call();
      toast.success(done);
      onChanged();
      detail.reload();
    } catch (err) {
      toast.error(errorMessage(err));
    } finally {
      setBusy(false);
    }
  };

  const attachBundle = (bundleId: number) =>
    change(() => api.post(`/api/scopes/${scope.id}/bundles/${bundleId}`), "Bundle attached");

  const attachConnection = (connectionId: number) =>
    change(() => api.post(`/api/scopes/${scope.id}/connections/${connectionId}`), "Connection attached");

  const detachBundle = async (bundle: Bundle) => {
    const ok = await confirm({
      title: `Detach ${bundle.name} from ${scope.name}?`,
      description: isWide
        ? `${below} loses this bundle's connections and domains.`
        : "The bot stops using this bundle's connections and domains in this channel. The bundle itself is kept.",
      confirmLabel: "Detach",
      destructive: true,
    });
    if (!ok) return;
    await change(() => api.del(`/api/scopes/${scope.id}/bundles/${bundle.id}`), "Bundle detached");
  };

  const detachConnection = async (bundle: Bundle, connection: Connection) => {
    const ok = await confirm({
      title: `Detach ${bundle.name}/${connection.name} from ${scope.name}?`,
      description: isWide
        ? `${below} loses this connection, unless it has it another way. The connection itself is kept.`
        : "The bot stops using this connection in this channel, unless the channel has it another way. The connection itself is kept.",
      confirmLabel: "Detach",
      destructive: true,
    });
    if (!ok) return;
    await change(
      () => api.del(`/api/scopes/${scope.id}/connections/${connection.id}`),
      "Connection detached",
    );
  };

  const removeRepos = async (rows: RepoRow[]) => {
    if (rows.length === 0) return;
    const ok = await confirm({
      title:
        rows.length === 1
          ? `Remove ${rows[0].repo} from ${scope.name}?`
          : `Remove ${rows.length} repositories from ${scope.name}?`,
      description: isWide
        ? `${below} loses them. The connections stay under Access bundles › Repositories.`
        : "The bot stops using them in this channel. The connections stay under Access bundles › Repositories.",
      confirmLabel: "Remove",
      destructive: true,
    });
    if (!ok) return;
    await change(async () => {
      for (const row of rows) {
        await api.del(`/api/scopes/${scope.id}/connections/${row.connection_id}`);
      }
    }, rows.length === 1 ? "Repository removed" : `${rows.length} repositories removed`);
  };

  // Repository connections have their own section, so keep them out of the chips.
  const plainConnections = attachedConnections.filter((id) => !connectionOf(id)?.connection.repo);
  const nothingAttached = attachedBundles.length === 0 && plainConnections.length === 0;

  const repos = detail.data?.repos ?? [];
  const repoInstalls = useInstallations(
    repos.some((r) => (connectionOf(r.connection_id)?.connection.github_installation_id ?? 0) > 0),
  );
  const attachedHere = repos.filter(removableRepo).length;
  // Repositories that arrive through a whole bundle rather than one at a time. They cannot be
  // ticked or removed here — the bundle grants them — and saying so beats a row that silently
  // has no checkbox while the one under it does.
  const viaBundles = [...new Set(repos.filter((r) => r.via === "bundle").map((r) => r.bundle))];
  const removeLabel = `Remove from ${isAccount ? "this account" : isTeam ? "this workspace" : "this channel"}`;
  // Every repository saved in this account, so one saved from the Bundles page can be added here.
  const savedRepos: TokenSource[] = bundles.flatMap((b) =>
    (b.connections ?? [])
      .filter((c) => c.repo)
      .map((c) => ({
        connection_id: c.id,
        repo: c.repo,
        name: c.name,
        installation_id: c.github_installation_id,
        fp: c.secret_fp,
      })),
  );
  const inheritedInstructions = detail.data?.inherited?.instructions ?? [];
  const inheritedRules = detail.data?.inherited?.allow_rules ?? [];
  const inheritedRuleCount = inheritedRules.reduce((n, g) => n + g.rules.length, 0);
  // Resolved down the chain by the API, so a channel reading "Inherit" still says what applies.
  const emailAutoWrites = detail.data?.inherited?.email_auto_writes ?? "off";
  const access = detail.data?.access ?? [];
  const effectiveRepo = detail.data?.default_repo_effective ?? "";
  const ownRepo = scope.default_repo || "";
  const repoOptions = repos.map((r) => r.repo);
  const ownRepoGone = ownRepo !== "" && !repoOptions.includes(ownRepo);
  const repoHint = ownRepoGone
    ? "This repository is no longer connected here. Pick another or clear it."
    : ownRepo
      ? "The bot assumes this repository when a question does not name one."
      : isWide
        ? "No default. The bot asks which repository is meant when several are connected; narrower scopes can set their own."
        : effectiveRepo
          ? `Inheriting ${effectiveRepo}.`
          : "Nothing above sets one either; the bot asks which repository is meant when several are connected.";

  return (
    <div className="min-w-0 space-y-6">
      <div className="flex items-center gap-3">
        <span className="flex size-9 shrink-0 items-center justify-center rounded-md bg-accent text-accent-foreground">
          <Icon className="size-5" />
        </span>
        <div className="min-w-0">
          <h2 className="truncate text-base font-semibold leading-tight text-foreground">
            {scopeName(scope)}
          </h2>
          <p className="font-mono text-xs text-muted-foreground">
            {isAccount ? "Workspace" : isTeam ? "Slack workspace" : scope.is_private ? "Private channel" : "Channel"}
            {scope.slack_id ? ` · ${scope.slack_id}` : " · everything below inherits this"}
          </p>
        </div>
      </div>

      {isTeam && team && (
        <TeamInstallCard
          team={team}
          onChanged={onChanged}
          onRemovedWorkspace={onRemovedWorkspace}
          confirm={confirm}
        />
      )}

      {isAccount && (
        <p className="rounded-lg border bg-muted/40 px-3 py-2 text-xs text-muted-foreground">
          Access bundles, connections, documents and skills belong to this Workspace, so anything
          attached here is reachable from <span className="font-medium">every</span> connected Slack
          workspace. Attach it to one workspace or channel below instead when it should not be.
        </p>
      )}

      <SettingsGroup title="Instructions">
        <SettingsSection
          title="Custom instructions"
          description={
            isWide
              ? `How the bot should behave: tone, what to avoid, who it works for. ${below} inherits this.`
              : "Anything specific to this channel, added on top of what comes down from above."
          }
        >
          <div className="space-y-2">
            <Textarea
              id={`instructions-${scope.id}`}
              aria-label="Custom instructions"
              rows={6}
              className="min-h-32"
              value={instructions}
              onChange={(e) => setInstructions(e.target.value)}
              placeholder={
                isWide
                  ? "e.g. Keep answers short and cite the document you used."
                  : "e.g. This channel is for support escalations; always link the ClickUp task."
              }
            />
            <div className="flex items-center gap-2">
              <Button
                size="sm"
                disabled={instructions === scope.instructions || savingField === "instructions"}
                onClick={() => save("instructions", { instructions }, "Instructions saved")}
              >
                Save instructions
              </Button>
              {instructions !== scope.instructions && (
                <Button size="sm" variant="ghost" onClick={() => setInstructions(scope.instructions)}>
                  Discard
                </Button>
              )}
            </div>
            <InstructionExamples
              onAdd={(line) =>
                setInstructions((cur) => (cur.trim() === "" ? line : cur.replace(/\s*$/, "") + "\n" + line))
              }
            />
            {inheritedInstructions.length > 0 && (
              <Disclosure
                className="pt-1"
                label="Also in force here"
                summary={`${inheritedInstructions.length} inherited ${
                  inheritedInstructions.length === 1 ? "source" : "sources"
                }`}
              >
                <InheritedInstructionsList items={inheritedInstructions} />
              </Disclosure>
            )}
          </div>
        </SettingsSection>
      </SettingsGroup>

      <SettingsGroup
        title="Access"
        accessory={
          <AddAccessPopover
            bundles={bundles}
            attachedBundles={attachedBundles}
            attachedConnections={attachedConnections}
            onAttachBundle={attachBundle}
            onAttachConnection={attachConnection}
            busy={busy}
          />
        }
      >
        <SettingsSection
          title="Bundles and connections"
          description="A bundle brings all of its connections, domains, instructions and skills. A connection added on its own (bundle/name) brings just that connection."
        >
          {nothingAttached ? (
            <p className="text-sm text-muted-foreground">
              {isWide
                ? `Nothing attached. Everything attached here is inherited by ${isAccount ? "every connected workspace" : "every channel in this workspace"}.`
                : "Nothing attached here; this channel only has what it inherits."}
            </p>
          ) : (
            <div className="flex flex-wrap gap-1.5">
              {attachedBundles.map((id) => {
                const bundle = bundles.find((b) => b.id === id);
                if (!bundle) return null;
                return (
                  <AttachedChip
                    key={`bundle-${id}`}
                    icon={Package}
                    label={bundle.name}
                    disabled={busy}
                    onDetach={() => detachBundle(bundle)}
                  />
                );
              })}
              {plainConnections.map((id) => {
                const hit = connectionOf(id);
                if (!hit) return null;
                return (
                  <AttachedChip
                    key={`connection-${id}`}
                    icon={KeyRound}
                    prefix={`${hit.bundle.name}/`}
                    label={hit.connection.name}
                    disabled={busy}
                    onDetach={() => detachConnection(hit.bundle, hit.connection)}
                  />
                );
              })}
            </div>
          )}
        </SettingsSection>

        <div className="px-6 py-4">
          <Disclosure
            label="Access summary"
            summary={
              detail.data
                ? `${access.length} host${access.length === 1 ? "" : "s"} reachable`
                : detail.error
                  ? "could not be loaded"
                  : "loading…"
            }
          >
            {detail.error && !detail.data ? (
              <ErrorBanner message={detail.error} onRetry={detail.reload} />
            ) : detail.loading || !detail.data ? (
              <div className="space-y-2">
                <Skeleton className="h-8 w-full" />
                <Skeleton className="h-8 w-full" />
                <Skeleton className="h-8 w-3/4" />
              </div>
            ) : (
              <AccessSummaryTable rows={access} />
            )}
          </Disclosure>
        </div>
      </SettingsGroup>

      <SettingsGroup
        title="Repositories"
        accessory={
          <div className="flex items-center gap-2">
            {/* Past a handful, the card is a list to scroll rather than a list to work in:
                the manager is the same dialog the Bundles page opens, with the filter and the
                counts, over this scope's own rows. */}
            {repos.length > 3 && (
              <Button variant="outline" size="sm" disabled={busy} onClick={() => setManagingRepos(true)}>
                <PackageOpen className="size-4" />
                Manage repositories
              </Button>
            )}
            <ConnectRepoPopover
              scopeId={scope.id}
              busy={busy}
              savedTokens={savedRepos}
              connectedRepos={repos.map((r) => r.repo)}
              onConnected={() => {
                onChanged();
                detail.reload();
              }}
            />
          </div>
        }
      >
        <SettingsSection
          title="Connected repositories"
          description="GitHub repositories the bot can work with here: search issues and pull requests, list commits, comment, and hand fixes to the worker. Connect one with an access token; it is stored sealed. The test command is what the fix worker runs before and after its change; leave it empty and it detects one."
        >
          {detail.loading && !detail.data ? (
            <div className="space-y-2">
              <Skeleton className="h-8 w-full max-w-sm" />
              <Skeleton className="h-8 w-2/3 max-w-sm" />
            </div>
          ) : repos.length === 0 ? (
            <p className="text-sm text-muted-foreground">
              {isWide
                ? "None yet. Repositories connected here are available everywhere below."
                : "None yet, here or above. Connect one to let the bot answer questions about its issues, pull requests and commits."}
            </p>
          ) : (
            <>
              {viaBundles.length > 0 && (
                <p className="mb-2 text-xs text-muted-foreground">
                  Some of these arrive through the{" "}
                  <span className="font-medium text-foreground">{viaBundles.join(" and ")}</span>{" "}
                  bundle, which also carries every repository added to it later — those rows have
                  no tick and no Remove. Detach the bundle above to choose repositories one at a
                  time instead.
                </p>
              )}
              <ScopeRepoList
                rows={repos}
                installs={repoInstalls}
                connectionOf={connectionOf}
                busy={busy}
                removeLabel={removeLabel}
                onRemove={removeRepos}
              />
            </>
          )}
        </SettingsSection>
        <SettingsSection
          title="Default repository"
          description={
            isWide
              ? "Assumed below when a question does not name a repository. A narrower scope can pick its own."
              : "Assumed in this channel when a question does not name a repository."
          }
        >
          <div className="space-y-1">
            <Select
              value={ownRepo || NO_REPO}
              onValueChange={(v) =>
                save("default_repo", { default_repo: v === NO_REPO ? "" : v }, "Default repository saved")
              }
              disabled={savingField === "default_repo" || (repoOptions.length === 0 && !ownRepo)}
            >
              <SelectTrigger id={`default-repo-${scope.id}`} aria-label="Default repository" className="w-full max-w-sm">
                <SelectValue />
              </SelectTrigger>
              <SelectContent>
                <SelectItem value={NO_REPO}>{isAccount ? "None" : "Inherit"}</SelectItem>
                {repoOptions.map((r) => (
                  <SelectItem key={r} value={r}>
                    {r}
                  </SelectItem>
                ))}
                {ownRepoGone && (
                  <SelectItem value={ownRepo}>{ownRepo} (no longer connected)</SelectItem>
                )}
              </SelectContent>
            </Select>
            <p className="text-xs text-muted-foreground">{repoHint}</p>
          </div>
        </SettingsSection>
      </SettingsGroup>

      <SettingsGroup title="Behaviour">
        <SettingsSection
          title="Default model"
          description={
            <>
              Any model the LLM endpoint serves, or an id typed in. Left empty, this inherits: a
              channel answers on its workspace&apos;s default, a workspace on the organisation&apos;s,
              and the organisation on the model in Settings. Advanced is the configured advanced
              model.
            </>
          }
        >
          <div className="flex gap-2">
            <ModelCombobox
              id={`model-${scope.id}`}
              aria-label="Default model"
              value={model}
              onChange={setModel}
              emptyLabel="Inherit"
              options={[{ value: "heavy", label: "Advanced", hint: "the configured advanced model" }]}
              className="w-full min-w-0 max-w-sm"
            />
            <Button
              size="sm"
              variant="outline"
              className="h-8"
              disabled={model === scope.default_model || savingField === "default_model"}
              onClick={() => save("default_model", { default_model: model }, "Model saved")}
            >
              Save
            </Button>
          </div>
        </SettingsSection>

        <SettingsSection
          title="Read every message"
          description={
            <>
              When off, the bot replies {isWide ? "in the channels below" : "here"} only when
              @-mentioned. When on, it reads every message {isWide ? "in any channel below" : "here"}{" "}
              and decides for itself whether to answer, react with an emoji, or say nothing —
              judged against the instructions set above. Each message costs a small classifier
              call, so this stays off until someone asks for it.
            </>
          }
        >
          <Select
            value={scope.read_all || "inherit"}
            onValueChange={(v) => save("read_all", { read_all: v }, "Read every message updated")}
            disabled={savingField === "read_all"}
          >
            <SelectTrigger id={`read-all-${scope.id}`} aria-label="Read every message" className="w-full max-w-sm">
              <SelectValue />
            </SelectTrigger>
            <SelectContent>
              {INHERIT_OPTIONS.map((o) => (
                <SelectItem key={o.value} value={o.value}>
                  {o.label}
                </SelectItem>
              ))}
            </SelectContent>
          </Select>
        </SettingsSection>

        <SettingsSection
          title="Email intake"
          description={
            <>
              Whether a message forwarded to {isWide ? "a channel's" : "this channel's"} Slack email
              address becomes a turn. Slack posts a forwarded mail as Slackbot with the body
              attached, and with this off nothing happens — nobody @-mentioned anyone, and there is
              no text for the classifier above to judge.
              <br />
              <br />
              When on, the mail is answered as a turn nobody in the workspace started: it runs on
              this {isWide ? "workspace's" : "channel's"} own connections and never on anyone's
              personal account, it cannot create routines or save memories, and every write it
              proposes waits for a named approver rather than a Confirm button in the thread. Set
              up an approval role first, or there will be nobody to ask.
            </>
          }
        >
          <Select
            value={scope.email_intake || "inherit"}
            onValueChange={(v) => save("email_intake", { email_intake: v }, "Email intake updated")}
            disabled={savingField === "email_intake"}
          >
            <SelectTrigger
              id={`email-intake-${scope.id}`}
              aria-label="Email intake"
              className="w-full max-w-sm"
            >
              <SelectValue />
            </SelectTrigger>
            <SelectContent>
              {INHERIT_OPTIONS.map((o) => (
                <SelectItem key={o.value} value={o.value}>
                  {o.label}
                </SelectItem>
              ))}
            </SelectContent>
          </Select>
        </SettingsSection>

        <SettingsSection
          title="Member edits"
          description="Whether channel members may change instructions and memory from Slack, without console access."
        >
          <div className="space-y-1">
            <Select
              value={scope.member_edits || "inherit"}
              onValueChange={(v) => save("member_edits", { member_edits: v }, "Member edits updated")}
              disabled={savingField === "member_edits"}
            >
              <SelectTrigger id={`member-edits-${scope.id}`} aria-label="Member edits" className="w-full max-w-sm">
                <SelectValue />
              </SelectTrigger>
              <SelectContent>
                {MEMBER_EDIT_OPTIONS.map((o) => (
                  <SelectItem key={o.value} value={o.value}>
                    {o.label}
                  </SelectItem>
                ))}
              </SelectContent>
            </Select>
            <p className="text-xs text-muted-foreground">
              {MEMBER_EDIT_OPTIONS.find((o) => o.value === (scope.member_edits || "inherit"))?.hint}
            </p>
          </div>
        </SettingsSection>

        <SettingsSection
          title="Auto mode allow rules"
          description={
            <>
              Writes the bot would otherwise hold until someone replies{" "}
              <span className="font-mono">confirm</span> in the thread run straight away when a rule
              covers them. Plain sentences, one per rule.
              {isWide
                ? " These apply everywhere below, on top of the rules in Settings."
                : " These apply here, on top of the rules inherited from above and from Settings."}
            </>
          }
        >
          <div className="space-y-3">
            <AllowRulesEditor
              id={`allow-rules-${scope.id}`}
              value={allowRules}
              disabled={savingField === "allow_rules"}
              onChange={async (next) => {
                const before = allowRules;
                setAllowRules(next);
                setSavingField("allow_rules");
                try {
                  await api.put(`/api/scopes/${scope.id}`, { allow_rules: JSON.stringify(next) });
                  toast.success("Allow rules saved");
                  onChanged();
                  detail.reload();
                } catch (err) {
                  setAllowRules(before);
                  toast.error(errorMessage(err));
                } finally {
                  setSavingField(null);
                }
              }}
            />
            {inheritedRules.length > 0 && (
              <Disclosure
                label="Also in force here"
                summary={`${inheritedRuleCount} inherited ${
                  inheritedRuleCount === 1 ? "rule" : "rules"
                }`}
              >
                <InheritedRulesList groups={inheritedRules} />
              </Disclosure>
            )}

            {/* Whether the rules above reach the one lane that holds every write by default.
                It lives here rather than beside Email intake because it is a question about
                these rules — taking mail and acting on it unattended are two decisions. */}
            <div className="space-y-2 border-t pt-3">
              <Label htmlFor={`email-auto-writes-${scope.id}`}>On forwarded email</Label>
              <Select
                value={scope.email_auto_writes || "inherit"}
                onValueChange={(v) =>
                  save("email_auto_writes", { email_auto_writes: v }, "Saved")
                }
                disabled={savingField === "email_auto_writes"}
              >
                <SelectTrigger
                  id={`email-auto-writes-${scope.id}`}
                  aria-label="Allow rules on forwarded email"
                  className="w-full max-w-sm"
                >
                  <SelectValue />
                </SelectTrigger>
                <SelectContent>
                  {INHERIT_OPTIONS.map((o) => (
                    <SelectItem key={o.value} value={o.value}>
                      {o.label}
                    </SelectItem>
                  ))}
                </SelectContent>
              </Select>
              <p className="text-xs text-muted-foreground">
                Off by default, and separately from the rest: nobody in the workspace wrote a
                forwarded mail, so a turn one starts holds every write for a named approver.
                {emailAutoWrites === "on" ? (
                  <>
                    {" "}
                    <b>On here.</b> A rule above that covers a write runs it straight away, judged
                    on where the write goes — the service, the credential and the verb — and never
                    on anything the mail itself says. Opening a pull request is never covered: that
                    always waits for an approver.
                  </>
                ) : (
                  " With this on, a rule above can let a write through — judged on where it goes, never on what the mail says."
                )}
              </p>
            </div>
          </div>
        </SettingsSection>

        {!isAccount && (
          <SettingsSection
            title="Monthly budget"
            description={
              isTeam
                ? "Spend cap for this Slack workspace in USD. 0 means no cap of its own; the account budget in Settings still applies."
                : "Spend cap for this channel in USD. 0 means no channel cap; the workspace and account budgets still apply."
            }
          >
            <form
              onSubmit={(e) => {
                e.preventDefault();
                save("monthly_budget_usd", { monthly_budget_usd: budget.trim() }, "Channel budget saved");
              }}
              className="flex gap-2"
            >
              <div className="relative w-full max-w-[12rem]">
                <span className="pointer-events-none absolute left-3 top-1/2 -translate-y-1/2 text-sm text-muted-foreground">
                  $
                </span>
                <Input
                  id={`budget-${scope.id}`}
                  aria-label="Monthly budget in USD"
                  type="number"
                  min={0}
                  step="0.01"
                  value={budget}
                  onChange={(e) => setBudget(e.target.value)}
                  className="pl-7 tabular-nums"
                />
              </div>
              <Button
                type="submit"
                size="sm"
                variant="outline"
                className="h-8"
                disabled={budget === budgetStored || budget.trim() === "" || savingField === "monthly_budget_usd"}
              >
                Save
              </Button>
            </form>
          </SettingsSection>
        )}
      </SettingsGroup>

      <RepoManagerDialog
        open={managingRepos}
        onOpenChange={setManagingRepos}
        title={`Repositories in ${scope.name}`}
        description={
          attachedHere === repos.length
            ? `${repos.length} reachable here, all attached here.`
            : `${repos.length} reachable here — ${attachedHere} attached here, ${repos.length - attachedHere} inherited from above or carried by a bundle.`
        }
        rows={repos}
        facts={(row) => ({
          repo: row.repo,
          last_used: connectionOf(row.connection_id)?.connection.last_used,
          status: connectionOf(row.connection_id)?.connection.status,
        })}
        add={(container) => (
          <ConnectRepoPopover
            scopeId={scope.id}
            busy={busy}
            savedTokens={savedRepos}
            connectedRepos={repos.map((r) => r.repo)}
            open={addRepoOpen}
            onOpenChange={setAddRepoOpen}
            container={container}
            anchor={
              <Button size="sm" onClick={() => setAddRepoOpen(true)}>
                <GitBranch className="size-4" />
                Add repositories
              </Button>
            }
            onConnected={() => {
              onChanged();
              detail.reload();
            }}
          />
        )}
        empty={
          <p className="px-4 py-10 text-center text-sm text-muted-foreground">
            None yet. Add one and the bot can answer questions about its issues, pull requests
            and commits here.
          </p>
        }
      >
        {(shown) => (
          <div className="p-3">
            <ScopeRepoList
              key={shown.length}
              rows={shown}
              installs={repoInstalls}
              connectionOf={connectionOf}
              busy={busy}
              removeLabel={removeLabel}
              onRemove={removeRepos}
            />
          </div>
        )}
      </RepoManagerDialog>
      {confirmDialog}
    </div>
  );
}

// One connected Slack workspace: what the install actually granted, and how to end it. The
// scopes Slack granted are shown because a workspace that granted fewer than the app asks for
// still works, just partly — and "the bot went quiet in one workspace" is otherwise a mystery.
function TeamInstallCard({
  team,
  onChanged,
  onRemovedWorkspace,
  confirm,
}: {
  team: Team;
  onChanged: () => void;
  onRemovedWorkspace: () => void;
  confirm: ReturnType<typeof useConfirm>["confirm"];
}) {
  const [busy, setBusy] = useState(false);
  const [removing, setRemoving] = useState(false);

  const disconnect = async () => {
    const ok = await confirm({
      title: `Disconnect ${team.name}?`,
      description:
        "The bot stops answering in that workspace and its token is revoked with Slack. Its channels, " +
        "instructions and history are kept, so reconnecting picks up where it left off — or, once it is " +
        "disconnected, you can remove it and them for good.",
      confirmLabel: "Disconnect",
      destructive: true,
    });
    if (!ok) return;
    setBusy(true);
    try {
      await api.post(`/api/teams/${team.team_id}/disconnect`, {});
      toast.success(`${team.name} disconnected`);
      onChanged();
    } catch (err) {
      toast.error(errorMessage(err));
    } finally {
      setBusy(false);
    }
  };

  // Only worth saying while the workspace is still meant to be answering; a disconnected one
  // has a simpler explanation for its silence, and two warnings would compete.
  const consequences = [
    !team.email_scope && "nobody there can be checked against the allowed email domains",
    !team.dm_scope && "approval cards cannot be delivered by DM",
  ].filter(Boolean) as string[];
  const missing = [
    !team.email_scope && "users:read.email",
    !team.dm_scope && "im:write",
  ].filter(Boolean) as string[];
  const showMissing = team.status === "active" && missing.length > 0;

  return (
    <div className="space-y-3 rounded-xl border bg-card p-3">
      <div className="flex flex-wrap items-center gap-2">
        <StatusChip variant={team.status === "active" ? "success" : "danger"}>
          {team.status === "active" ? "Connected" : "Disconnected"}
        </StatusChip>
        {team.status === "active" && team.needs_reinstall && (
          <StatusChip variant="warning">Reinstall needed</StatusChip>
        )}
        <span className="text-xs text-muted-foreground">
          {team.installed_by
            ? `Connected by ${team.installed_by_name || team.installed_by}`
            : "Connected"}
          {team.installed_at ? ` · ${team.installed_at}` : ""}
          {team.bot_user_id ? ` · bot ${team.bot_user_name || team.bot_user_id}` : ""}
        </span>
        <span className="flex-1" />
        {team.status === "active" ? (
          <Button size="sm" variant="ghost" disabled={busy} onClick={disconnect}>
            <Unplug className="size-4" />
            Disconnect
          </Button>
        ) : (
          <>
            <Button asChild size="sm" variant="ghost">
              <a href="/slack/install">Reconnect</a>
            </Button>
            {/* Only on a disconnected workspace, and deliberately: ending the install and
                erasing what it did are two steps, so a slip stops at the reversible one. */}
            <Button
              size="sm"
              variant="ghost"
              className="text-destructive hover:bg-destructive/10 hover:text-destructive"
              disabled={removing}
              onClick={() => setRemoving(true)}
            >
              <Trash2 className="size-4" />
              Remove
            </Button>
          </>
        )}
      </div>
      {showMissing && (
        <p className="text-xs text-warning">
          This install is missing {missing.join(" and ")}, so {consequences.join(", and ")}.
          Reconnect the workspace to grant {missing.length > 1 ? "them" : "it"}.
        </p>
      )}
      {team.status !== "active" && team.last_error && (
        <p className="text-xs text-muted-foreground">{team.last_error}</p>
      )}
      {removing && (
        <RemoveWorkspaceForm
          team={team}
          onCancel={() => setRemoving(false)}
          onRemoved={onRemovedWorkspace}
        />
      )}
    </div>
  );
}

// Removing a disconnected workspace for good. The typed name is the confirmation — a dialog on
// top of it would be a second Cancel to click past, not a second thought — and what it buys is
// the pause to read the name and notice it is the wrong workspace. It says plainly what goes and
// what stays, because the difference between "the account's" and "this workspace's" is exactly
// what somebody about to press this is unsure of.
function RemoveWorkspaceForm({
  team,
  onCancel,
  onRemoved,
}: {
  team: Team;
  onCancel: () => void;
  onRemoved: () => void;
}) {
  const [typed, setTyped] = useState("");
  const [busy, setBusy] = useState(false);
  const [error, setError] = useState<string | null>(null);
  // Slack does not always give a workspace a name, and the console shows the id for such a row,
  // so the id is what it asks to be typed. The server decides the same way.
  const named = team.name || team.team_id;
  const ready = typed.trim().toLowerCase() === named.trim().toLowerCase();

  const submit = async (e: React.FormEvent) => {
    e.preventDefault();
    if (!ready || busy) return;
    setBusy(true);
    setError(null);
    try {
      await api.post(`/api/teams/${team.team_id}/delete`, { confirm: typed });
      toast.success(`${named} removed`);
      onRemoved();
    } catch (err) {
      setError(errorMessage(err));
      setBusy(false);
    }
  };

  return (
    <form onSubmit={submit} className="space-y-2 rounded-lg border border-destructive/40 bg-destructive/5 p-3">
      <p className="text-xs text-muted-foreground">
        Everything recorded for this workspace goes with it: its channels and what was set on
        them, every thread the bot answered in, its memories, artifacts and access requests.
        Access bundles, credentials, documents, skills and people belong to the account and stay,
        as does the audit log. It cannot be undone.
      </p>
      {/* The label sits above its field rather than beside it. Inline, the input needs a width
          to stop it collapsing, and the width it needed was w-full — which in a wrapping row
          pushed itself onto its own line and left the label and the two buttons stranded on
          theirs. A stack is what this actually is. */}
      <div className="space-y-1.5">
        <label htmlFor={`remove-${team.team_id}`} className="block text-xs text-muted-foreground">
          Type <span className="font-medium text-foreground">{named}</span> to confirm
        </label>
        <Input
          id={`remove-${team.team_id}`}
          autoFocus
          autoComplete="off"
          value={typed}
          onChange={(e) => setTyped(e.target.value)}
          placeholder={named}
          className="h-8 max-w-64"
        />
      </div>
      {error && <p className="text-xs text-danger">{error}</p>}
      <div className="flex items-center gap-2">
        <Button type="submit" size="sm" variant="destructive" className="h-8" disabled={!ready || busy}>
          {busy && <Loader2 className="size-3.5 animate-spin" />}
          Remove workspace
        </Button>
        <Button type="button" size="sm" variant="ghost" className="h-8" disabled={busy} onClick={onCancel}>
          Cancel
        </Button>
      </div>
    </form>
  );
}
