"use client";

import { useEffect, useMemo, useState } from "react";
import { Loader2, PencilLine } from "lucide-react";
import { toast } from "sonner";
import { jobBranchShapes } from "@/components/jobs/job-format";
import { AllowRulesEditor, parseAllowRules } from "@/components/core/allow-rules-editor";
import { ChannelCombobox } from "@/components/core/channel-combobox";
import { ErrorBanner } from "@/components/core/error-banner";
import { ModelCombobox, ModelMultiCombobox } from "@/components/core/model-combobox";
import { TimezoneCombobox } from "@/components/core/timezone-combobox";
import { PageHeader } from "@/components/core/page-header";
import {
  SettingsActions,
  SettingsEditRow,
  SettingsGroup,
  SettingsRow,
  SettingsRows,
  SettingsSection,
} from "@/components/core/settings-section";
import { StatusChip } from "@/components/core/status-chip";
import { UpgradeButton } from "@/components/core/upgrade-button";
import { Button } from "@/components/ui/button";
import { Card } from "@/components/ui/card";
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
import { Switch } from "@/components/ui/switch";
import { Tabs, TabsContent, TabsList, TabsTrigger } from "@/components/ui/tabs";
import { AccountPanel } from "@/components/settings/account-panel";
import { BillingPanel } from "@/components/settings/billing-panel";
import { DeleteAccountPanel } from "@/components/settings/delete-account-panel";
import { ModelKeyPanel } from "@/components/settings/model-key-panel";
import { WebKeyPanel } from "@/components/settings/web-key-panel";
import { SecurityPanel } from "@/components/settings/security-panel";
import { ConsoleRolesPanel } from "@/components/settings/console-roles-panel";
import { ConsoleUsersPanel } from "@/components/settings/console-users-panel";
import { useAuth } from "@/components/shell/auth-provider";
import {
  api,
  errorMessage,
  useApi,
  type EffectiveSettings,
  type SettingsResponse,
  type WebProviderInfo,
  type WorkerEnv,
} from "@/lib/api";

// Settings, in the order somebody looks for things: yourself first, then how you get in, then
// the bot, then the models it runs on, then the worker, then the people.
//
// The split is deliberate. General and Security are about the account making the request and
// save as you go; Workspace, Models and Workers are the organisation's configuration and save
// as a batch, because those are read together and changed together. Users and Roles are their
// own panels, with their own permissions behind them.

// Every editable key as the string the API stores. Booleans are "1"/"0".
type Form = Record<SettingKey, string>;
type SettingKey =
  | "model"
  | "heavy_model"
  | "embed_model"
  | "monthly_budget_usd"
  | "timezone"
  | "history_limit"
  | "max_tool_rounds"
  | "routine_rounds"
  | "routine_minutes"
  | "bot_name"
  | "user_rate_limit"
  | "alert_channel"
  | "long_answer_chars"
  | "channel_models"
  | "show_cost"
  | "allow_rules"
  | "access_allow_self_approve"
  | "web_provider"
  | "web_fetch_provider"
  | "web_account_id"
  | "worker_engine"
  | "worker_model"
  | "worker_job_budget_usd"
  | "worker_timeout_minutes"
  | "worker_max_jobs"
  | "worker_branch_prefix"
  | "worker_branch_suffix"
  | "worker_event_retention_days"
  | "worker_allow_rules"
  | "data_retention_days"
  | "audit_retention_days";

const ENGINE_LABELS: Record<string, string> = {
  qwen_code: "Qwen Code",
  fake: "Fake — one placeholder edit, no model (for testing)",
};

const WORKER_PLATFORMS: Record<string, string> = {
  cloudrun: "Google Cloud Run",
  ecs: "AWS Fargate",
  aca: "Azure Container Apps",
  k8s: "Kubernetes",
  docker: "Docker on this host",
};

// Workers run wherever the deployment does, so the mode is `workers` on every platform and the
// platform is only where it landed. WORKER_MODE=cloudrun and friends pin what `workers` would
// otherwise detect; they are not modes of their own.
function workerModeLabel(w: WorkerEnv | undefined): string {
  const mode = w?.mode ?? "off";
  if (mode === "off") return "off";
  if (mode === "local") return "local (a subprocess of the bot, for development)";
  const where = WORKER_PLATFORMS[w?.platform ?? ""] ?? w?.platform;
  if (!where) return "workers";
  return mode === "workers" ? `workers on ${where} (detected)` : `workers on ${where}`;
}

// Billing sits after the deployment's own settings and before the people: it is what the
// organisation is buying, not who is in it. It is only offered where there is something to buy —
// see `tabs` in SettingsForm, which is also what stops ?tab=billing selecting a tab that has no
// trigger and no content on a self-host.
const TABS = ["general", "security", "workspace", "models", "web", "workers", "billing", "users", "roles"] as const;
type Tab = (typeof TABS)[number];

const TAB_LABELS: Record<Tab, string> = {
  general: "General",
  security: "Security",
  workspace: "Workspace",
  models: "Models",
  web: "Web",
  workers: "Workers",
  billing: "Billing",
  users: "Users",
  roles: "Roles",
};

function fromEffective(s: EffectiveSettings): Form {
  return {
    worker_engine: s.WorkerEngine || "qwen_code",
    worker_model: s.WorkerModel ?? "",
    worker_job_budget_usd: String(s.WorkerJobBudgetUSD ?? 3),
    worker_timeout_minutes: String(s.WorkerTimeoutMinutes ?? 45),
    worker_max_jobs: String(s.WorkerMaxJobs ?? 2),
    worker_branch_prefix: s.WorkerBranchPrefix || "feature/, bugfix/, hotfix/",
    worker_branch_suffix: s.WorkerBranchSuffix || "attest_tag",
    worker_event_retention_days: String(s.WorkerEventRetentionDays ?? 30),
    worker_allow_rules: s.WorkerAllowRules ? "1" : "0",
    data_retention_days: String(s.DataRetentionDays ?? 0),
    audit_retention_days: String(s.AuditRetentionDays ?? 0),
    model: s.Model,
    heavy_model: s.HeavyModel,
    embed_model: s.EmbedModel,
    monthly_budget_usd: String(s.MonthlyBudgetUSD),
    timezone: s.Timezone,
    history_limit: String(s.HistoryLimit),
    max_tool_rounds: String(s.MaxToolRounds),
    routine_rounds: String(s.RoutineRounds ?? 200),
    routine_minutes: String(s.RoutineMinutes ?? 30),
    bot_name: s.BotName,
    user_rate_limit: String(s.UserRateLimit ?? 0),
    alert_channel: s.AlertChannel ?? "",
    long_answer_chars: String(s.LongAnswerChars ?? 0),
    channel_models: (s.ChannelModels ?? []).join(","),
    show_cost: s.ShowCost ? "1" : "0",
    allow_rules: JSON.stringify(s.AllowRules ?? []),
    access_allow_self_approve: s.AllowSelfApprove ? "1" : "0",
    web_provider: s.WebProvider || "builtin",
    web_fetch_provider: s.WebFetchProvider ?? "",
    web_account_id: s.WebAccountID ?? "",
  };
}

export function SettingsPage() {
  const settings = useApi<SettingsResponse>("/api/settings");

  return (
    <div className="space-y-5">
      <PageHeader
        title="Settings"
        description="Your account, who may sign in, and how the bot is configured. Values saved here override the .env defaults without a restart."
      />
      {settings.error && !settings.data ? (
        <ErrorBanner message={settings.error} onRetry={settings.reload} />
      ) : settings.loading || !settings.data ? (
        <Card className="divide-y py-0">
          {Array.from({ length: 5 }).map((_, i) => (
            <div key={i} className="grid gap-3 px-6 py-5 md:grid-cols-[13rem_1fr] md:gap-8">
              <div className="space-y-2">
                <Skeleton className="h-3.5 w-24" />
                <Skeleton className="h-3 w-40" />
              </div>
              <Skeleton className="h-8 w-full max-w-sm" />
            </div>
          ))}
        </Card>
      ) : (
        /* No key on the config version. Every panel with its own endpoint calls onSaved when
           it stores something, and every save bumps that version, so keying on it meant a web
           key saved on one tab threw away a model name half-typed on another. The form merges
           the new server values itself instead. */
        <SettingsForm data={settings.data} onSaved={settings.reload} />
      )}
    </div>
  );
}

/**
 * Which tab is open, kept in the address bar.
 *
 * Read after mount rather than during render: the console is a static export, so the HTML is
 * built with no location to read. Writing it back with replaceState means a reload, a bookmark
 * and the link the Slack callback sends people to all land on the same tab, without adding a
 * history entry for every tab someone clicks through.
 */
function useTabParam(available: readonly Tab[]): [Tab, (next: Tab) => void] {
  const [tab, setTab] = useState<Tab>("general");

  // Validated against the tabs this deployment actually has, not against every tab that exists.
  // Billing is hidden where there is nothing to buy, and ?tab=billing there would otherwise
  // select a tab with no trigger and no content — a blank page with the rest of Settings gone.
  useEffect(() => {
    const asked = new URLSearchParams(window.location.search).get("tab");
    if (asked && (available as readonly string[]).includes(asked)) {
      // eslint-disable-next-line react-hooks/set-state-in-effect -- one-shot URL read
      setTab(asked as Tab);
    }
  }, [available]);

  const change = (next: Tab) => {
    setTab(next);
    const url = new URL(window.location.href);
    url.searchParams.set("tab", next);
    window.history.replaceState(null, "", url);
  };
  return [tab, change];
}

const keysOf = (f: Form) => Object.keys(f) as SettingKey[];

function SettingsForm({ data, onSaved }: { data: SettingsResponse; onSaved: () => void }) {
  const server = fromEffective(data.effective);
  const [form, setForm] = useState<Form>(server);
  // What the server last said, to tell a field somebody edited from one they left alone.
  const [seen, setSeen] = useState<Form>(server);
  const [busy, setBusy] = useState(false);
  const billingEnabled = !!data.env.billing_enabled;
  const tabs = useMemo(
    () => TABS.filter((t) => t !== "billing" || billingEnabled),
    [billingEnabled],
  );
  const [tab, setTab] = useTabParam(tabs);
  const { me } = useAuth();
  // /api/me sends only the keys the role holds, so a permission somebody lacks is absent
  // rather than false. Until it has answered the form stays editable — the server is the one
  // enforcing this, and the fields would otherwise grey out for a frame on every load.
  const perms = me?.user?.permissions;
  const canManageOrg = !perms || perms["settings.manage"] === true;
  // On its own model key, an unset model is the key's default rather than the deployment's, and
  // documents are embedded with the model chosen on the key.
  const ownKey = me?.model_key?.own === true;

  /**
   * The settings came back — this form's own save, or a panel that saves through its own
   * endpoint and then asks for a reload. Take the new value for every field still sitting on
   * the last thing the server said, and keep the edit everywhere else. A save settles its own
   * fields on what was stored; a web key saved on one tab no longer throws away a model name
   * half-typed on another.
   *
   * Adjusting state during the render that noticed the change is React's own answer to state
   * derived from props: the re-render happens before anything paints, where an effect would
   * show the stale form for a frame — and the lint rules here forbid that effect anyway.
   */
  if (keysOf(server).some((k) => server[k] !== seen[k])) {
    const merged = { ...form };
    for (const k of keysOf(server)) if (form[k] === seen[k]) merged[k] = server[k];
    setSeen(server);
    setForm(merged);
  }

  // Against the server, not against whatever it said when this page loaded: after a save the
  // bar has to go quiet, and after somebody else's save it has to count what is still unsent.
  const changed = keysOf(form).filter((k) => form[k] !== server[k]);
  const set = (key: SettingKey, value: string) => setForm((f) => ({ ...f, [key]: value }));

  const save = async () => {
    const body: Record<string, string> = {};
    for (const k of changed) body[k] = form[k];
    setBusy(true);
    try {
      await api.put("/api/settings", body);
      toast.success("Settings saved");
      onSaved();
    } catch (err) {
      toast.error(errorMessage(err));
    } finally {
      setBusy(false);
    }
  };

  /**
   * The web key saved itself. Choosing a provider and pasting its key is one act, but it is two
   * saves — the key has its own endpoint, because a key that came back to a browser would be a
   * key in a log — so the choice goes with the key. Without this, picking Tavily and saving its
   * key left the key stored against a provider nothing had chosen: the dropdown still said
   * Tavily, unsaved, while the bot went on searching through the built-in path.
   *
   * Only the web fields: a save on this tab must not quietly write a half-typed model name
   * somebody left on another. If the choice cannot be stored the reload is skipped, so the page
   * is not told the provider is settled when it is not.
   */
  const webKeyChanged = async () => {
    const pending = changed.filter((k) => k.startsWith("web_"));
    if (pending.length > 0) {
      const body: Record<string, string> = {};
      for (const k of pending) body[k] = form[k];
      try {
        await api.put("/api/settings", body);
      } catch (err) {
        toast.error(`The key was saved, but the provider was not: ${errorMessage(err)}`);
        return;
      }
    }
    onSaved();
  };

  const field = "w-full max-w-sm";

  // Each tab of settings is its own form, so Enter in any field saves the tab rather than
  // doing nothing. Nothing is nested: the tabs that hold panels with forms of their own
  // (General, Security, Users, Roles) are left alone.
  const onSubmit = (e: React.FormEvent) => {
    e.preventDefault();
    if (changed.length > 0 && !busy) save();
  };

  const saveBar = (
    <div className="flex items-center justify-end gap-2">
      {changed.length > 0 && (
        <Button variant="ghost" onClick={() => setForm(server)} disabled={busy}>
          Discard
        </Button>
      )}
      <Button type="submit" disabled={busy || changed.length === 0}>
        {busy && <Loader2 className="animate-spin" />}
        Save{changed.length > 0 ? ` ${changed.length} change${changed.length === 1 ? "" : "s"}` : ""}
      </Button>
    </div>
  );

  return (
    <Tabs value={tab} onValueChange={(v) => setTab(v as Tab)} className="space-y-5">
      {/* Underlined tabs rather than a pill group: nine of them read as a row of headings
          instead of a block of buttons, and the rule under the row carries on past the last
          one, which is what says the panel below belongs to whichever is marked.

          Eight tabs do not fit one line on a phone. Wrapping keeps every one of them reachable
          — a scroller would hide Users and Roles behind a swipe nobody knows to make — and the
          height has to come off with the same variant selector that set it, or the row draws
          around the first line only.

          The 5px of padding under the list is load-bearing: the marker is drawn 5px below its
          trigger (after:bottom-[-5px], set by the shared component), so that much padding is
          exactly what lands it on the rule rather than floating above or below it. */}
      <TabsList
        variant="line"
        className="h-auto w-full flex-wrap justify-start gap-x-6 gap-y-1 rounded-none border-b border-border p-0 pb-[5px] group-data-[orientation=horizontal]/tabs:h-auto [&>button]:after:bg-primary"
      >
        {tabs.map((t) => (
          <TabsTrigger
            key={t}
            value={t}
            className="flex-none px-0.5 pt-1 pb-1 data-[state=active]:text-foreground"
          >
            {TAB_LABELS[t]}
          </TabsTrigger>
        ))}
      </TabsList>

      <TabsContent value="general" className="space-y-5">
        <AccountPanel />
      </TabsContent>

      {billingEnabled && (
        <TabsContent value="billing" className="space-y-5">
          <BillingPanel onSaved={onSaved} />
        </TabsContent>
      )}

      <TabsContent value="security" className="space-y-5">
        <SecurityPanel
          settings={data.effective}
          defaultEmailDomains={data.env.default_email_domains ?? []}
          canManageOrg={canManageOrg}
          onSaved={onSaved}
        />
      </TabsContent>

      <TabsContent value="workspace" className="space-y-5">
        <OrganisationGroup name={me?.user?.org_name ?? ""} onSaved={onSaved} />
        <form onSubmit={onSubmit} className="space-y-5">
          <SettingsGroup title="Behaviour">
            <SettingsSection
              title="Timezone"
              description="The bot's sense of 'today' and the default zone for new routines. It starts as the zone whoever created this organisation signed up from."
            >
              <TimezoneCombobox
                id="timezone"
                aria-label="Timezone"
                className={field}
                value={form.timezone}
                onChange={(v) => set("timezone", v)}
                emptyLabel="Use the .env default (TZ_NAME)"
              />
            </SettingsSection>
            <SettingsSection
              title="History limit"
              description="How many earlier messages of a thread the bot reads before answering."
            >
              <Input
                id="history_limit"
                aria-label="History limit"
                type="number"
                min={0}
                className="w-full max-w-40 tabular-nums"
                value={form.history_limit}
                onChange={(e) => set("history_limit", e.target.value)}
              />
            </SettingsSection>
            <SettingsSection
              title="Max tool rounds"
              description="How many times a single turn may call tools before it has to answer."
            >
              <Input
                id="max_tool_rounds"
                aria-label="Max tool rounds"
                type="number"
                min={0}
                className="w-full max-w-40 tabular-nums"
                value={form.max_tool_rounds}
                onChange={(e) => set("max_tool_rounds", e.target.value)}
              />
            </SettingsSection>
            <SettingsSection
              title="Routine tool rounds"
              description="How many times a scheduled run may call tools before it has to write up what it has. A routine is not a reply — nobody is waiting on it, and the work it was written for is several times a conversation's — so it has its own budget rather than the one above."
            >
              <Input
                id="routine_rounds"
                aria-label="Routine tool rounds"
                type="number"
                min={1}
                max={200}
                className="w-full max-w-40 tabular-nums"
                value={form.routine_rounds}
                onChange={(e) => set("routine_rounds", e.target.value)}
              />
            </SettingsSection>
            <SettingsSection
              title="Routine minutes"
              description="The clock one scheduled run gets to spend those rounds in, including the write-up. Between 2 and 60."
            >
              <Input
                id="routine_minutes"
                aria-label="Routine minutes"
                type="number"
                min={2}
                max={60}
                className="w-full max-w-40 tabular-nums"
                value={form.routine_minutes}
                onChange={(e) => set("routine_minutes", e.target.value)}
              />
            </SettingsSection>
            <SettingsSection title="Bot name" description="How the bot refers to itself in replies.">
              <Input
                id="bot_name"
                aria-label="Bot name"
                className={field}
                value={form.bot_name}
                onChange={(e) => set("bot_name", e.target.value)}
              />
            </SettingsSection>
          </SettingsGroup>

          <SettingsGroup title="Permissions">
            <SettingsSection
              title="Auto mode allow rules"
              description={
                <>
                  Pre-approve writes the bot would otherwise hold until someone presses Confirm in
                  the thread. Reads never wait. Each rule is a plain sentence describing what&apos;s
                  sanctioned everywhere, for example: &quot;Creating tasks in ClickUp is expected and
                  approved.&quot; A channel&apos;s own rules, set under Workspaces, apply on top of these.
                </>
              }
            >
              <AllowRulesEditor
                id="allow_rules"
                value={parseAllowRules(form.allow_rules)}
                onChange={(next) => set("allow_rules", JSON.stringify(next))}
              />
            </SettingsSection>
          </SettingsGroup>

          <SettingsGroup title="Access requests">
            <SettingsSection
              title="Allow self-approval"
              description="Let the person who asked press Approve on their own request. For testing the flow with one person — with it on, an access request is not an approval, so leave it off once two people can answer. Every self-approval is marked as one on the record and said out loud in the thread."
            >
              <label className="flex items-center gap-2.5">
                <Switch
                  id="access_allow_self_approve"
                  checked={form.access_allow_self_approve === "1"}
                  onCheckedChange={(v) => set("access_allow_self_approve", v ? "1" : "0")}
                />
                <span className="text-sm">
                  {form.access_allow_self_approve === "1" ? "On — testing only" : "Off"}
                </span>
              </label>
            </SettingsSection>
          </SettingsGroup>

          <SettingsGroup title="Operations">
            <SettingsSection
              title="Alert channel"
              description="Proxy blocks, budget exhaustion and failing routines are posted here, at most once an hour per cause. Pick one of the channels the bot is in; No alerts turns them off."
            >
              <ChannelCombobox
                id="alert_channel"
                aria-label="Alert channel"
                className={field}
                value={form.alert_channel}
                onChange={(v) => set("alert_channel", v)}
                emptyLabel="No alerts"
              />
            </SettingsSection>
          </SettingsGroup>

          {/* Two numbers rather than one, on purpose: what the bot did and what the people did
              are kept for different reasons and asked about by different people, and one
              field would let a shorter history for turns quietly shorten the audit log. */}
          <SettingsGroup title="Retention">
            <SettingsSection
              title="Activity"
              description="How many days of turns, tool calls, proxied requests and artifacts to keep. 0 keeps everything, which is the default; the shortest policy is 7 days."
            >
              <Input
                id="data_retention_days"
                aria-label="Activity retention in days"
                type="number"
                min={0}
                max={3650}
                className="w-full max-w-40 tabular-nums"
                value={form.data_retention_days}
                onChange={(e) => set("data_retention_days", e.target.value)}
              />
            </SettingsSection>
            <SettingsSection
              title="Audit log"
              description="How many days of the audit log to keep — sign-ins, changes, approvals. Its own policy, so shortening the activity history never shortens this. 0 keeps everything; the shortest policy is 30 days."
            >
              <Input
                id="audit_retention_days"
                aria-label="Audit log retention in days"
                type="number"
                min={0}
                max={3650}
                className="w-full max-w-40 tabular-nums"
                value={form.audit_retention_days}
                onChange={(e) => set("audit_retention_days", e.target.value)}
              />
            </SettingsSection>
          </SettingsGroup>

          <div className="px-1">{saveBar}</div>
        </form>
        {/* Last on the tab, and outside the settings form: this is the one control here that
            does not save a value. */}
        <DeleteAccountPanel />
      </TabsContent>

      <TabsContent value="models" className="space-y-5">
        {/* Its own endpoint and its own form, outside the settings one: the key is saved, tested
            and removed on routes that never hand it back. */}
        {canManageOrg && <ModelKeyPanel billingEnabled={billingEnabled} onChanged={onSaved} />}
        <form onSubmit={onSubmit} className="space-y-5">
          <SettingsGroup title="Models">
            <SettingsSection
              title="Model"
              description={
                ownKey
                  ? "The model for everyday replies, from what your model key's endpoint serves. A channel can override it under Workspaces."
                  : "The model for everyday replies, from whatever the LLM base URL serves. A channel can override it under Workspaces."
              }
            >
              <ModelCombobox
                id="model"
                aria-label="Model"
                className={field}
                value={form.model}
                onChange={(v) => set("model", v)}
                emptyLabel={ownKey ? "Your model key's default model" : "Use the .env default (LLM_MODEL)"}
              />
            </SettingsSection>
            <SettingsSection
              title="Embedding model"
              description="Embeds documents for search. Changing it means re-indexing every document."
            >
              {ownKey ? (
                <p className="text-sm text-muted-foreground">
                  Set on your model key above: documents are embedded on your own endpoint.
                </p>
              ) : (
                <ModelCombobox
                  id="embed_model"
                  aria-label="Embedding model"
                  kind="embedding"
                  className={field}
                  value={form.embed_model}
                  onChange={(v) => set("embed_model", v)}
                  emptyLabel="Use the .env default (EMBED_MODEL)"
                />
              )}
            </SettingsSection>
          </SettingsGroup>

          <SettingsGroup title="Answers">
            <SettingsSection
              title="Advanced model"
              description={
                <>
                  What fix jobs run on. Chat turns reach it only where it is asked for: a
                  channel&rsquo;s default model, or{" "}
                  <span className="font-mono">!model advanced</span> in a thread. Leave empty to
                  use the .env default (HEAVY_MODEL).
                </>
              }
            >
              <ModelCombobox
                id="heavy_model"
                aria-label="Advanced model"
                className={field}
                value={form.heavy_model}
                onChange={(v) => set("heavy_model", v)}
                emptyLabel={ownKey ? "Your model key's default model" : "Use the .env default (HEAVY_MODEL)"}
              />
            </SettingsSection>
            <SettingsSection
              title="Models channels may choose"
              description={
                <>
                  Extra models offered on each channel&apos;s Configure page, alongside Default and
                  Advanced. A channel page is open to everyone who can see the channel, so a model
                  is only offered if you put it here. Leave empty for Default and Advanced only.
                </>
              }
            >
              <ModelMultiCombobox
                id="channel_models"
                aria-label="Models channels may choose"
                className={field}
                value={form.channel_models}
                onChange={(v) => set("channel_models", v)}
              />
            </SettingsSection>
            <SettingsSection
              title="Show cost in replies"
              description="Include what a turn cost in the small footer under each reply, next to the model and the token counts. Off keeps the footer but drops the dollars; spend is still recorded and still shown in the console."
            >
              <label className="flex items-center gap-2.5">
                <Switch
                  id="show_cost"
                  checked={form.show_cost === "1"}
                  onCheckedChange={(v) => set("show_cost", v ? "1" : "0")}
                />
                <span className="text-sm">{form.show_cost === "1" ? "On" : "Off"}</span>
              </label>
            </SettingsSection>
            <SettingsSection
              title="Long answer threshold"
              description="Answers longer than this many characters are attached as a Markdown file instead of posted inline. 0 means never."
            >
              <Input
                id="long_answer_chars"
                aria-label="Long answer threshold in characters"
                type="number"
                min={0}
                className="w-full max-w-40 tabular-nums"
                value={form.long_answer_chars}
                onChange={(e) => set("long_answer_chars", e.target.value)}
              />
            </SettingsSection>
          </SettingsGroup>

          <SettingsGroup title="Limits">
            <SettingsSection
              title="Per-user rate limit"
              description="Turns one person may take per hour across every channel. 0 turns the limit off."
            >
              <Input
                id="user_rate_limit"
                aria-label="Turns per user per hour"
                type="number"
                min={0}
                className="w-full max-w-40 tabular-nums"
                value={form.user_rate_limit}
                onChange={(e) => set("user_rate_limit", e.target.value)}
              />
            </SettingsSection>
            {/* The budget field lives here only where there is no Billing tab to put it on.
                On a deployment that sells plans it moves there, beside the credit balance it
                sits under — one place raises a budget, not two, and the number only makes
                sense next to the harder limit underneath it. */}
            {!billingEnabled && (
              <SettingsSection
                title="Monthly budget"
                description={
                  data.effective.Plan === "free" ? (
                    <>
                      Free accounts have a ${data.effective.PlatformBudgetUSD.toFixed(2)} monthly
                      budget. Once spend reaches it the bot stops replying until the month rolls
                      over. Upgrade sends support a request from the address you sign in with. The
                      reply moves the account to Pro, and this field opens up.
                    </>
                  ) : data.effective.PlatformBudgetUSD > 0 ? (
                    `In US dollars, up to the $${data.effective.PlatformBudgetUSD.toFixed(2)} your plan allows. Once spend reaches it the bot stops replying until the month rolls over. 0 means the most allowed.`
                  ) : (
                    "In US dollars. Once spend reaches it the bot stops replying until the month rolls over. 0 means no cap."
                  )
                }
              >
                <div className="space-y-3">
                  <div className="relative w-full max-w-40">
                    <span className="pointer-events-none absolute left-3 top-1/2 -translate-y-1/2 text-sm text-muted-foreground">
                      $
                    </span>
                    <Input
                      id="monthly_budget_usd"
                      aria-label="Monthly budget in USD"
                      type="number"
                      min={0}
                      step="0.01"
                      className="pl-7 tabular-nums"
                      // A free account sees the number the bot stops at, not the setting it
                      // cannot change; the form value is left alone so nothing is sent for it.
                      value={
                        data.effective.Plan === "free"
                          ? String(data.effective.EffectiveBudgetUSD)
                          : form.monthly_budget_usd
                      }
                      onChange={(e) => set("monthly_budget_usd", e.target.value)}
                      disabled={data.effective.Plan === "free"}
                    />
                  </div>
                  {/* No published support address, no button: the request it sends has nowhere
                      to go, and the server refuses it for the same reason. */}
                  {data.effective.Plan === "free" && data.env.support_email && (
                    <UpgradeButton
                      support={data.env.support_email}
                      askedAt={data.stored?.plan_request_at}
                    />
                  )}
                </div>
              </SettingsSection>
            )}
          </SettingsGroup>

          <div className="px-1">{saveBar}</div>
        </form>
      </TabsContent>

      <TabsContent value="web" className="space-y-5">
        <form onSubmit={onSubmit} className="space-y-5">
          <WebTab
            form={form}
            set={set}
            providers={data.env.web_providers ?? undefined}
            keyProviders={data.effective.WebKeyProviders ?? []}
            onKeyChanged={webKeyChanged}
            field={field}
          />
          <div className="px-1">{saveBar}</div>
        </form>
      </TabsContent>

      <TabsContent value="workers" className="space-y-5">
        <form onSubmit={onSubmit} className="space-y-5">
          <SettingsGroup title="Fix worker">
            <SettingsSection
              title="Worker"
              description="Where fix jobs run. Set at deploy time with WORKER_MODE; everything else in this group is live."
            >
              <dl className="space-y-2 text-sm">
                <EnvRow label="Mode" value={workerModeLabel(data.env.worker)} />
                <EnvRow
                  label="Per-job keys"
                  value={
                    data.env.worker?.provisioning_key
                      ? "on (capped OpenRouter key per job)"
                      : "off (the shared key, uncapped)"
                  }
                />
              </dl>
              {(data.env.worker?.mode ?? "off") !== "off" && !data.env.worker?.provisioning_key && (
                <p className="mt-3 text-xs text-muted-foreground">
                  Every fix job runs on the shared OPENROUTER_API_KEY with no per-job spend cap,
                  inside a container that runs the repository&apos;s own code. Create a provisioning
                  key at openrouter.ai/settings/provisioning-keys and set
                  OPENROUTER_PROVISIONING_KEY at deploy time to give each job its own capped key.
                </p>
              )}
              {(data.env.worker?.mode ?? "off") === "off" && (
                <p className="mt-3 text-xs text-muted-foreground">
                  The fix tool is not offered in Slack until WORKER_MODE is set at deploy time.
                  <code className="mx-1">workers</code> runs each job in a container of its own
                  and works out the platform from the environment; naming one &mdash; cloudrun,
                  ecs, aca, k8s, docker, or local for development &mdash; pins it instead. The
                  settings below still save and apply once it is.
                </p>
              )}
            </SettingsSection>
            <SettingsSection
              title="Coding agent"
              description="The agent the worker runs inside its container to make the change."
            >
              <Select value={form.worker_engine} onValueChange={(v) => set("worker_engine", v)}>
                <SelectTrigger id="worker_engine" aria-label="Coding agent" className={field}>
                  <SelectValue />
                </SelectTrigger>
                <SelectContent>
                  {(data.env.worker?.engines ?? ["qwen_code", "fake"]).map((e) => (
                    <SelectItem key={e} value={e}>
                      {ENGINE_LABELS[e] ?? e}
                    </SelectItem>
                  ))}
                </SelectContent>
              </Select>
            </SettingsSection>
            <SettingsSection
              title="Model"
              description="The model the coding agent calls. Empty uses the advanced model."
            >
              <ModelCombobox
                id="worker_model"
                aria-label="Worker model"
                className={field}
                value={form.worker_model}
                onChange={(v) => set("worker_model", v)}
                emptyLabel="Same as the advanced model"
              />
            </SettingsSection>
            <SettingsSection
              title="Per-job budget"
              description="Model spend one job may reach, in US dollars. The worker stops at this figure, and the bot cancels a job that runs 25% past it."
            >
              <div className="relative w-full max-w-40">
                <span className="pointer-events-none absolute left-3 top-1/2 -translate-y-1/2 text-sm text-muted-foreground">
                  $
                </span>
                <Input
                  id="worker_job_budget_usd"
                  aria-label="Per-job budget in USD"
                  type="number"
                  min={0.1}
                  max={100}
                  step="0.1"
                  className="pl-7 tabular-nums"
                  value={form.worker_job_budget_usd}
                  onChange={(e) => set("worker_job_budget_usd", e.target.value)}
                />
              </div>
            </SettingsSection>
            <SettingsSection
              title="Timeout"
              description="Wall-clock limit per job in minutes, from dispatch to result (5 to 60)."
            >
              <Input
                id="worker_timeout_minutes"
                aria-label="Timeout in minutes"
                type="number"
                min={5}
                max={60}
                className="w-full max-w-40 tabular-nums"
                value={form.worker_timeout_minutes}
                onChange={(e) => set("worker_timeout_minutes", e.target.value)}
              />
            </SettingsSection>
            <SettingsSection
              title="Max concurrent jobs"
              description="Jobs that may run at once across the workspace; one per thread regardless."
            >
              <Input
                id="worker_max_jobs"
                aria-label="Max concurrent jobs"
                type="number"
                min={1}
                max={10}
                className="w-full max-w-40 tabular-nums"
                value={form.worker_max_jobs}
                onChange={(e) => set("worker_max_jobs", e.target.value)}
              />
            </SettingsSection>
            <SettingsSection
              title="Branch prefix"
              description="Your branch convention. A comma-separated list names a prefix per kind of change — the job picks the one that fits, and a job that does not say is a bug fix. Write none for no convention, and the branch starts at fix-."
            >
              <Input
                id="worker_branch_prefix"
                aria-label="Branch prefix"
                className="w-full max-w-60 font-mono"
                placeholder="feature/, bugfix/, hotfix/"
                value={form.worker_branch_prefix}
                onChange={(e) => set("worker_branch_prefix", e.target.value)}
              />
            </SettingsSection>
            <SettingsSection
              title="Branch suffix"
              description="Every branch the worker pushes ends with this, and it never pushes anywhere else. Letters, digits, . _ and -, no slash."
            >
              <Input
                id="worker_branch_suffix"
                aria-label="Branch suffix"
                className="w-full max-w-60 font-mono"
                placeholder="attest_tag"
                value={form.worker_branch_suffix}
                onChange={(e) => set("worker_branch_suffix", e.target.value)}
              />
              <div className="mt-2 space-y-0.5 font-mono text-xs text-muted-foreground">
                {jobBranchShapes(form.worker_branch_prefix, form.worker_branch_suffix).map((shape) => (
                  <p key={shape}>{shape}</p>
                ))}
              </div>
            </SettingsSection>
            <SettingsSection
              title="Event retention"
              description="How many days a finished job keeps its event log and diff. The job row itself stays."
            >
              <Input
                id="worker_event_retention_days"
                aria-label="Event retention in days"
                type="number"
                min={1}
                max={365}
                className="w-full max-w-40 tabular-nums"
                value={form.worker_event_retention_days}
                onChange={(e) => set("worker_event_retention_days", e.target.value)}
              />
            </SettingsSection>
            <SettingsSection
              title="Allow rules may start jobs"
              description="When on, an allow rule that covers a fix request dispatches the job straight away. Otherwise every job waits for a person to press Confirm in the thread, whatever the rules say."
            >
              <label className="flex items-center gap-2.5">
                <Switch
                  id="worker_allow_rules"
                  checked={form.worker_allow_rules === "1"}
                  onCheckedChange={(v) => set("worker_allow_rules", v ? "1" : "0")}
                />
                <span className="text-sm">{form.worker_allow_rules === "1" ? "On" : "Off"}</span>
              </label>
            </SettingsSection>
          </SettingsGroup>

          <div className="px-1">{saveBar}</div>
        </form>
      </TabsContent>

      <TabsContent value="users" className="space-y-5">
        <ConsoleUsersPanel />
      </TabsContent>

      <TabsContent value="roles" className="space-y-5">
        <ConsoleRolesPanel />
      </TabsContent>
    </Tabs>
  );
}

// The organisation's own name, which is not a setting: it lives on the org row and has its own
// endpoint, so it saves on its own rather than joining the batch below it.
function OrganisationGroup({ name, onSaved }: { name: string; onSaved: () => void }) {
  const [open, setOpen] = useState(false);
  const [value, setValue] = useState(name);
  const [busy, setBusy] = useState(false);

  const submit = async (e: React.FormEvent) => {
    e.preventDefault();
    setBusy(true);
    try {
      await api.put("/api/org", { name: value.trim() });
      toast.success("Organisation renamed");
      setOpen(false);
      onSaved();
      // The name is on the session payload the shell renders, so it has to be re-read for the
      // rail and the switcher to agree with this field.
      window.location.reload();
    } catch (err) {
      toast.error(errorMessage(err));
    } finally {
      setBusy(false);
    }
  };

  return (
    <SettingsGroup title="Organisation">
      <SettingsSection
        title="Name"
        description="What this account is called, in the console and in the bot's replies. Members see it in the switcher."
      >
        {open ? (
          <form onSubmit={submit} className="space-y-2">
            <SettingsEditRow label="Name" htmlFor="org-name">
              <Input
                id="org-name"
                autoFocus
                value={value}
                onChange={(e) => setValue(e.target.value)}
                className="max-w-sm"
              />
            </SettingsEditRow>
            <SettingsActions>
              <Button type="submit" size="sm" disabled={busy || value.trim() === ""}>
                {busy && <Loader2 className="animate-spin" />}
                Save
              </Button>
              <Button
                type="button"
                variant="ghost"
                size="sm"
                disabled={busy}
                onClick={() => {
                  setValue(name);
                  setOpen(false);
                }}
              >
                Cancel
              </Button>
            </SettingsActions>
          </form>
        ) : (
          <SettingsRows
            action={
              <Button variant="outline" size="sm" onClick={() => setOpen(true)}>
                <PencilLine className="size-4" />
                Rename
              </Button>
            }
          >
            <SettingsRow label="Name" empty="Unnamed">
              {name}
            </SettingsRow>
          </SettingsRows>
        )}
      </SettingsSection>
    </SettingsGroup>
  );
}

/**
 * Settings → Web: which service the bot's web_search and fetch_url go through.
 *
 * The built-in path scrapes DuckDuckGo and does a plain GET — the curl answer, free and the
 * first thing a site refuses. Naming a provider here puts both tools behind a real one. The
 * two halves are shown separately because providers differ on them: Cloudflare renders pages
 * but has no web search, so picking it leaves searching where it was, and saying so on the
 * page is cheaper than finding out in a channel.
 */
const FOLLOW_SEARCH = "__follow_search";

function WebTab({
  form,
  set,
  providers,
  keyProviders,
  onKeyChanged,
  field,
}: {
  form: Form;
  set: (key: SettingKey, value: string) => void;
  providers?: WebProviderInfo[];
  keyProviders: string[];
  onKeyChanged: () => void;
  field: string;
}) {
  const list = providers ?? [
    { id: "builtin", name: "Built-in (no key)", search: true, fetch: true, needs_account: false },
  ];
  const byId = (id: string) => list.find((p) => p.id === id);
  const builtin = byId("builtin") ?? list[0];

  const search = byId(form.web_provider) ?? builtin;
  // An empty reader follows the search engine where it can read, and otherwise is the built-in
  // path — the same rule the server applies, so the page never claims a route the bot won't take.
  const follower = search.fetch ? search : builtin;
  const reader = form.web_fetch_provider ? (byId(form.web_fetch_provider) ?? builtin) : follower;

  return (
    <SettingsGroup title="Web access">
      <WebHalf
        title="Search"
        description="What web_search asks. The built-in path scrapes DuckDuckGo's HTML page, which is free and the first thing to be rate-limited."
        tool="web_search"
        options={list.filter((p) => p.search)}
        chosen={search}
        value={form.web_provider}
        onChange={(v) => set("web_provider", v)}
        selectId="web_provider"
        keyProviders={keyProviders}
        accountID={form.web_account_id}
        onKeyChanged={onKeyChanged}
        field={field}
      />

      <WebHalf
        title="Page reader"
        description="What fetch_url reads a page with. The built-in path is a plain GET with the tags stripped — no JavaScript, and nothing for a site that wants a browser."
        tool="fetch_url"
        options={list.filter((p) => p.fetch)}
        chosen={reader}
        value={form.web_fetch_provider}
        onChange={(v) => set("web_fetch_provider", v)}
        selectId="web_fetch_provider"
        followLabel={`Follow search — ${follower.name}`}
        /* One key, one box. When both halves land on the same provider — which is what
           Follow search usually means — the key belongs to the half above. */
        showKey={reader.id !== search.id}
        keyProviders={keyProviders}
        accountID={form.web_account_id}
        onKeyChanged={onKeyChanged}
        field={field}
      />

      {(search.needs_account || reader.needs_account) && (
        <SettingsSection
          title="Cloudflare account id"
          description="The account the token belongs to. It goes in the request path, so a token without it cannot be used."
        >
          <Input
            id="web_account_id"
            aria-label="Cloudflare account id"
            className={field}
            autoComplete="off"
            spellCheck={false}
            value={form.web_account_id}
            onChange={(e) => set("web_account_id", e.target.value)}
          />
          {form.web_account_id.trim() === "" && (
            <p className="mt-2 max-w-prose text-xs text-warning">
              Empty, so the Cloudflare token cannot be used and that half stays on the built-in
              path.
            </p>
          )}
        </SettingsSection>
      )}
    </SettingsGroup>
  );
}

/**
 * One half of web access: a provider, what the tool will actually do, and that provider's key.
 *
 * Both halves are the same three questions, which is why they are one component — and they are
 * two sections rather than one because the services genuinely split that way. Brave and Serper
 * search and cannot read a page; Cloudflare reads a page and cannot search. Each dropdown lists
 * only what can do that job, so an impossible pairing is never on offer in the first place.
 */
function WebHalf({
  title,
  description,
  tool,
  options,
  chosen,
  value,
  onChange,
  selectId,
  followLabel,
  showKey = true,
  keyProviders,
  accountID,
  onKeyChanged,
  field,
}: {
  title: string;
  description: string;
  tool: string;
  options: WebProviderInfo[];
  chosen: WebProviderInfo;
  value: string;
  onChange: (v: string) => void;
  selectId: string;
  /** The reader's first option: follow whatever is searching. Absent on the search half. */
  followLabel?: string;
  /** False when the other half already shows this provider's key box. */
  showKey?: boolean;
  keyProviders: string[];
  accountID: string;
  onKeyChanged: () => void;
  field: string;
}) {
  const isBuiltin = chosen.id === "builtin";
  const keySet = keyProviders.includes(chosen.id);
  const needsKey = !isBuiltin && !keySet;
  const needsAccount = chosen.needs_account && accountID.trim() === "";
  const routed = isBuiltin
    ? "Built-in"
    : needsKey
      ? "Built-in — no key stored yet"
      : needsAccount
        ? "Built-in — no account id yet"
        : chosen.name;

  return (
    <>
      <SettingsSection title={title} description={description}>
        {/* Radix refuses an empty item value, so "follow the search engine" travels as a
            sentinel and is turned back into the empty string the setting actually stores. */}
        <Select
          value={value || FOLLOW_SEARCH}
          onValueChange={(v) => onChange(v === FOLLOW_SEARCH ? "" : v)}
        >
          <SelectTrigger id={selectId} aria-label={`${title} provider`} className={field}>
            <SelectValue />
          </SelectTrigger>
          <SelectContent>
            {followLabel && <SelectItem value={FOLLOW_SEARCH}>{followLabel}</SelectItem>}
            {options.map((p) => (
              <SelectItem key={p.id} value={p.id}>
                {p.name}
              </SelectItem>
            ))}
          </SelectContent>
        </Select>
        {chosen.blurb && (
          <p className="mt-2 max-w-prose text-xs text-muted-foreground">{chosen.blurb}</p>
        )}
        <dl className="mt-3">
          <ToolRow label={tool} value={routed} />
        </dl>
        {needsKey && (
          <p className="mt-3 max-w-prose text-xs text-warning">
            No key is stored for {chosen.name}, so {tool} still uses the built-in path.
          </p>
        )}
      </SettingsSection>

      {!isBuiltin && showKey && (
        <SettingsSection
          title={`${chosen.name} ${chosen.key_label ?? "API key"}`}
          description="Stored sealed and never shown again. It saves on its own — the button below, not the one at the bottom of the page — and each provider keeps its own."
        >
          <WebKeyPanel
            key={chosen.id}
            provider={chosen}
            accountID={accountID}
            keySet={keySet}
            onChanged={onKeyChanged}
          />
          {chosen.docs_url && (
            <p className="mt-2 text-xs">
              <a
                className="text-muted-foreground underline underline-offset-2 hover:text-foreground"
                href={chosen.docs_url}
                target="_blank"
                rel="noreferrer"
              >
                {chosen.name} API docs
              </a>
            </p>
          )}
        </SettingsSection>
      )}
    </>
  );
}

// Which path one tool takes. Same shape as EnvRow, without the ".env" chip: this is a live
// setting, and saying it came from a file would be a lie.
function ToolRow({ label, value }: { label: string; value: string }) {
  return (
    <div className="grid gap-1 sm:grid-cols-[8rem_1fr] sm:items-center sm:gap-3">
      <Label className="font-mono text-xs text-muted-foreground">{label}</Label>
      <span className="min-w-0 truncate text-sm">{value}</span>
    </div>
  );
}

function EnvRow({ label, value }: { label: string; value: string }) {
  return (
    <div className="grid gap-1 sm:grid-cols-[8rem_1fr] sm:items-center sm:gap-3">
      <Label className="text-muted-foreground">{label}</Label>
      <div className="flex min-w-0 items-center gap-2">
        <span className="truncate font-mono text-xs">
          {value || <span className="text-muted-foreground">Not set</span>}
        </span>
        <StatusChip variant="neutral">set in .env</StatusChip>
      </div>
    </div>
  );
}
