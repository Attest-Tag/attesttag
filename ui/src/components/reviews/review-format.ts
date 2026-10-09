// Words and small helpers shared by Reviews › Settings. Free of React, like jobs/job-format.ts, so
// both the tree and the panels read one vocabulary: a mode called "Shadow" on the rail and
// "Recorded here" in the panel would be two settings to anybody reading them.

import type {
  ReviewBranchRule,
  ReviewConnectionNode,
  ReviewEffective,
  ReviewForks,
  ReviewGroupNode,
  ReviewLevel,
  ReviewMode,
  ReviewNode,
  ReviewNodeDetail,
  ReviewNotify,
  ReviewNotifyEvent,
  ReviewSettingsTree,
  ReviewSettingsValues,
  ReviewStrictness,
  ReviewTrigger,
  Scope,
} from "@/lib/api";
import type { StatusChipVariant } from "@/components/core/status-chip";

export type Choice<T extends string> = { value: T; label: string; hint: string };

export const MODES: Choice<ReviewMode>[] = [
  { value: "off", label: "Off", hint: "Not reviewed at all, not even when somebody asks." },
  { value: "shadow", label: "Shadow", hint: "Reviewed and recorded here. Nothing is posted on GitHub." },
  { value: "live", label: "Live", hint: "Reviewed and posted on the pull request." },
];

export const TRIGGERS: Choice<ReviewTrigger>[] = [
  { value: "command", label: "Only when asked", hint: "An @-command on the pull request, or Start review here." },
  { value: "open", label: "When a pull request opens", hint: "Once per pull request; a command reviews it again." },
  { value: "push", label: "On every push", hint: "Every new head is reviewed again, and paid for again." },
];

export const STRICTNESS: Choice<ReviewStrictness>[] = [
  { value: "low", label: "Low", hint: "More comments, some of them less sure." },
  { value: "medium", label: "Medium", hint: "The default balance." },
  { value: "high", label: "High", hint: "Fewer, surer comments." },
];

export const FORKS: Choice<ReviewForks>[] = [
  { value: "command", label: "When a member asks", hint: "Never automatically: a member's command starts one." },
  { value: "off", label: "Never", hint: "Pull requests from forks are not reviewed." },
];

export const DRAFTS: Choice<"yes" | "no">[] = [
  { value: "no", label: "Skip drafts", hint: "A draft is reviewed once it is marked ready, or when asked." },
  { value: "yes", label: "Review drafts", hint: "Drafts are reviewed like any other pull request." },
];

export const AUTO_TYPES: Choice<"yes" | "no">[] = [
  {
    value: "yes",
    label: "Add them",
    hint: "A type whose pattern the diff matches — Concurrency and state on async code — runs beside the rule's, on the parts it matches.",
  },
  { value: "no", label: "Only the rule's types", hint: "A review runs the types its branch rule and labels chose, and nothing else." },
];

export const CONTEXT_AUTO: Choice<"yes" | "no">[] = [
  {
    value: "yes",
    label: "The connection's other repositories",
    hint: "With none named, up to five of this connection's other repositories in code review, the most recently reviewed first.",
  },
  { value: "no", label: "None", hint: "Only the repositories named here are read." },
];

export const DEFAULT_AUTO_PAUSE_AFTER = 10;

export const FIXES: Choice<"yes" | "no">[] = [
  {
    value: "yes",
    label: "Fix when asked",
    hint: "Somebody who can push ticks a finding's fix box, or comments @… fix, and a commit is pushed to the pull request.",
  },
  { value: "no", label: "Never", hint: "Findings carry no fix box, and a request to fix one is refused." },
];

export const NOTIFY_EVENTS: Choice<ReviewNotifyEvent>[] = [
  { value: "started", label: "Started", hint: "The message says a review is under way. No reply." },
  { value: "finished", label: "Finished", hint: "The message shows the result, and a reply says what changed." },
  { value: "failed", label: "Failed or not run", hint: "A failure, or no review for the budget, a pause or the plan." },
  { value: "merged", label: "Merged", hint: "The message says it was merged, and a reply says by whom." },
];

const NOTIFY_WORDS: Record<ReviewNotifyEvent, string> = {
  started: "starts",
  finished: "results",
  failed: "failures",
  merged: "merges",
};

/** "starts, results and merges" — the events a channel hears of, in the console's order; absent is every one. */
export function notifyEventsLabel(events: ReviewNotifyEvent[] | undefined): string {
  const words = NOTIFY_EVENTS.filter((c) => !events || events.includes(c.value)).map((c) => NOTIFY_WORDS[c.value]);
  if (words.length === 0) return "nothing";
  return words.length === 1 ? words[0] : `${words.slice(0, -1).join(", ")} and ${words[words.length - 1]}`;
}

export function choiceLabel<T extends string>(choices: Choice<T>[], value: T | undefined | null): string {
  return choices.find((c) => c.value === value)?.label ?? String(value ?? "");
}

export function modeVariant(mode: ReviewMode | undefined): StatusChipVariant {
  return mode === "live" ? "success" : mode === "shadow" ? "info" : "neutral";
}

/** "Advanced" for the alias every level starts on; a model id as itself. */
export function modelLabel(model: string, heavy?: string): string {
  // "default" names the deployment's everyday model outright (review_engine.go's reviewDefaultModel).
  if (!model || model === "default") return "the default model";
  if (model === "heavy") return heavy ? `Advanced (${heavy})` : "Advanced";
  return model;
}

export function usd(n: number): string {
  return `$${n.toFixed(2)}`;
}

/** How a connection is named everywhere: the GitHub account it is installed on. */
export function connectionName(c: { account_login?: string; installation_id?: number }): string {
  return c.account_login || `Installation ${c.installation_id ?? "?"}`;
}

/**
 * The installation's own page at GitHub, where its owner accepts new permissions: an organisation's
 * installations and a user's live under two different settings pages.
 */
export function installationURL(c: { account_type?: string; account_login?: string; installation_id: number }): string {
  return c.account_type === "Organization" && c.account_login
    ? `https://github.com/organizations/${c.account_login}/settings/installations/${c.installation_id}`
    : `https://github.com/settings/installations/${c.installation_id}`;
}

/**
 * Why a connection reviews nothing whatever mode its settings hold, as the lane gates it; "" when
 * it reviews. `short` is the rail's and a repository row's few words, `why` the sentence a panel says.
 */
export function stoppedBecause(c: ReviewConnectionNode): { short: string; why: string } | null {
  if (c.removed_at) return { short: "connection stopped", why: `reviews are stopped on ${connectionName(c)}` };
  if (c.status === "uninstalled") return { short: "App uninstalled", why: `the GitHub App was uninstalled from ${connectionName(c)}` };
  if (c.status === "suspended") return { short: "App suspended", why: `the GitHub App is suspended on ${connectionName(c)}` };
  return null;
}

/**
 * A channel reviews are announced in, as people know it — "#eng-reviews" — from the console's list
 * of channels the bot is in; its id when the list does not have it, and "" for none.
 */
export function channelLabel(n: ReviewNotify | null | undefined, channels: Scope[] | undefined): string {
  if (!n?.channel) return "";
  const c =
    channels?.find((s) => s.kind === "channel" && s.slack_id === n.channel && (!n.team || s.team_id === n.team)) ??
    channels?.find((s) => s.kind === "channel" && s.slack_id === n.channel);
  if (!c) return n.channel;
  return c.name.startsWith("#") ? c.name : `#${c.name}`;
}

/** "acme/web" → "web": the rail already says which account a repository sits under. */
export function repoShort(repo: string): string {
  const i = repo.indexOf("/");
  return i >= 0 ? repo.slice(i + 1) : repo;
}

// ---- the tree ----

/** Every repository of a connection, grouped or not. */
export function reposOf(c: ReviewConnectionNode): ReviewNode[] {
  return [...c.groups.flatMap((g) => g.repos), ...c.repos];
}

/**
 * What is selected, as the URL carries it: a node's public id, or a repository by name — a
 * repository nothing was set on has no id, and a link from elsewhere (Access bundles' row menu)
 * only knows the name. `conn` disambiguates a repository two installations both reach.
 */
export type Selection = { node: string; conn?: string };

export type Resolved =
  | { kind: "connection"; conn: ReviewConnectionNode }
  | { kind: "group"; conn: ReviewConnectionNode; group: ReviewConnectionNode["groups"][number] }
  | { kind: "repo"; conn: ReviewConnectionNode; repo: ReviewNode; group?: ReviewConnectionNode["groups"][number] };

export function resolveSelection(tree: ReviewSettingsTree, sel: Selection | null): Resolved | null {
  if (!sel) return null;
  const isRepo = sel.node.includes("/");
  for (const conn of tree.connections) {
    if (sel.conn && conn.id !== sel.conn) continue;
    if (!isRepo && conn.id === sel.node) return { kind: "connection", conn };
    for (const group of conn.groups) {
      if (!isRepo && group.id === sel.node) return { kind: "group", conn, group };
      const repo = group.repos.find((r) => (isRepo ? r.repo === sel.node.toLowerCase() : r.id === sel.node));
      if (repo) return { kind: "repo", conn, repo, group };
    }
    const repo = conn.repos.find((r) => (isRepo ? r.repo === sel.node.toLowerCase() : r.id !== "" && r.id === sel.node));
    if (repo) return { kind: "repo", conn, repo };
  }
  return null;
}

/** The selection a resolved node is written back to the URL as. */
export function selectionOf(r: Resolved, tree: ReviewSettingsTree): Selection {
  if (r.kind !== "repo") return { node: r.kind === "group" ? r.group.id : r.conn.id };
  const repo = r.repo.repo ?? "";
  // Name it by connection only when the name alone would land on another one first.
  const first = resolveSelection(tree, { node: repo });
  return first && first.conn.id !== r.conn.id ? { node: repo, conn: r.conn.id } : { node: repo };
}

/** The settings route for a node: a repository through its connection, which works with or without a row. */
export function settingsPath(r: Resolved): string {
  if (r.kind === "repo") {
    return `/api/review-settings/${r.conn.id}?repo=${encodeURIComponent(r.repo.repo ?? "")}`;
  }
  return `/api/review-settings/${r.kind === "group" ? r.group.id : r.conn.id}`;
}

// ---- where a value comes from ----

/** One level above a node, with what it sets itself: the rows an inherited list entry is credited to. */
export type Ancestor = { level: ReviewLevel; name: string; settings: ReviewSettingsValues };

/**
 * The levels above the node, broadest first, each with its own settings from the tree. The tree
 * carries every node's own settings, which is what lets a list say which level each inherited
 * entry came from — the detail call's `source` only names the nearest one that added any.
 */
export function ancestorsOf(detail: ReviewNodeDetail, tree: ReviewSettingsTree): Ancestor[] {
  const out: Ancestor[] = [];
  for (const link of detail.chain.slice(0, -1)) {
    if (link.kind === "connection") {
      const c = tree.connections.find((x) => x.id === link.id);
      if (c) out.push({ level: "connection", name: connectionName(c), settings: c.settings ?? {} });
    } else if (link.kind === "group") {
      const g = tree.connections.flatMap((c) => c.groups).find((x) => x.id === link.id);
      if (g) out.push({ level: "group", name: g.name, settings: g.settings ?? {} });
    }
  }
  return out;
}

/** "from Frontend", "from octo-org", "built-in default": what the Inherit line credits a value to. */
export function sourceLabel(level: ReviewLevel | undefined, ancestors: Ancestor[]): string {
  if (!level || level === "default") return "built-in default";
  const a = [...ancestors].reverse().find((x) => x.level === level);
  return a ? `from ${a.name}` : `from the ${level}`;
}

export type ListField = "instructions" | "exclude_authors" | "review_bots" | "ignore_paths" | "context_repos";

/** Whether a list compares entries without case, as the server's addUp does: logins and repository names. */
export const FOLD_CASE: Record<ListField, boolean> = {
  instructions: false,
  exclude_authors: true,
  review_bots: true,
  ignore_paths: false,
  context_repos: true,
};

/** The inherited entries of a list, each with the level that added it, in the order they resolve. */
export function inheritedEntries(field: ListField, ancestors: Ancestor[]): { value: string; from: Ancestor }[] {
  const key = (s: string) => (FOLD_CASE[field] ? s.toLowerCase() : s);
  const out: { value: string; from: Ancestor }[] = [];
  for (const a of ancestors) {
    for (const raw of a.settings[field] ?? []) {
      const v = raw.trim();
      if (v !== "" && !out.some((e) => key(e.value) === key(v))) out.push({ value: v, from: a });
    }
  }
  return out;
}

// ---- branch rules ----

/**
 * "hotfix/* → main", "any → testing": a rule named head first, as a pull request reads. A label rule
 * is named by its labels, and its branches when it has any: "label perf", "label perf on any → main".
 */
export function ruleLabel(r: ReviewBranchRule): string {
  const branches = `${r.head || "any"} → ${r.base || "any"}`;
  if (!isLabelRule(r)) return branches;
  return `label ${r.labels!.join(", ")}${r.base || r.head ? ` on ${branches}` : ""}`;
}

/** The fallback: a branch rule naming no branch, which matches every pull request and comes last. */
export function isFallback(r: ReviewBranchRule): boolean {
  return !r.base && !r.head && !isLabelRule(r);
}

/** A label rule adds its types for a label on the pull request, and chooses nothing by itself. */
export function isLabelRule(r: ReviewBranchRule): boolean {
  return (r.labels?.length ?? 0) > 0;
}

/** review.maxRuleLabels and maxLabelLen. */
export const MAX_RULE_LABELS = 10;
export const MAX_LABEL_LEN = 50;

/**
 * A Labels box's text as the labels it names: split on commas, trimmed, the empty ones dropped, and
 * one of each — GitHub compares label names without regard to case, and so does the server, which
 * refuses one listed twice.
 */
export function parseLabels(text: string): string[] {
  const out: string[] = [];
  for (const raw of text.split(",")) {
    const l = raw.trim();
    if (l && !out.some((x) => x.toLowerCase() === l.toLowerCase())) out.push(l);
  }
  return out;
}

/** What is wrong with a rule's labels, in a sentence; null when nothing is. */
export function labelsProblem(labels: string[] | undefined): string | null {
  if (!labels?.length) return null;
  if (labels.length > MAX_RULE_LABELS) return `At most ${MAX_RULE_LABELS} labels.`;
  // Characters, as GitHub and the server count them: .length counts UTF-16 units, two for an emoji.
  if (labels.some((l) => [...l].length > MAX_LABEL_LEN)) return `A label is at most ${MAX_LABEL_LEN} characters.`;
  return null;
}

// ---- the Effective line ----

/**
 * What will actually run, in one line: the panel's last word on the settings above it. `channel` is
 * the channel it is announced in, as channelLabel names it.
 */
export function effectiveSummary(e: ReviewEffective, heavy?: string, stopped?: string, channel?: string): string {
  // A connection that reviews nothing whatever its settings say: the line says what will run, and
  // what the settings would run there is not it.
  if (stopped) return `Off: ${stopped}, so nothing is reviewed whatever the settings say.`;
  if (e.mode === "off") return "Off: not reviewed at all.";
  const rules = e.branch_rules?.length ?? 0;
  return [
    choiceLabel(MODES, e.mode),
    choiceLabel(TRIGGERS, e.trigger).toLowerCase(),
    e.drafts ? "drafts reviewed" : "drafts skipped",
    e.forks === "off" ? "forks never" : "forks when a member asks",
    e.fixes ? "fixes when asked" : "no fixes",
    `${e.strictness} strictness`,
    `up to ${e.max_comments} comment${e.max_comments === 1 ? "" : "s"}`,
    modelLabel(e.model, heavy),
    `up to ${usd(e.max_usd)} a review`,
    `${rules} branch rule${rules === 1 ? "" : "s"}`,
    channel
      ? `${notifyEventsLabel(e.notify_on)} announced in ${channel}`
      : rulesAnnounce(e)
        ? `${notifyEventsLabel(e.notify_on)} announced only where a branch rule names a channel`
        : "not announced",
  ].join(" · ");
}

const LEVEL_RANK: Record<ReviewLevel, number> = { default: 0, connection: 1, group: 2, repo: 3, rule: 4 };

/**
 * Whether a level that names no channel still announces some of its pull requests: a branch rule
 * naming a channel takes those it matches there, where the rule list is at least as near as the
 * level's empty channel — Effective.WithRule's nearness, read from the server's sources — and the
 * level's notify_on is what that channel hears of them.
 */
export function rulesAnnounce(e: ReviewEffective): boolean {
  if (e.notify?.channel) return false;
  const near = LEVEL_RANK[e.source?.branch_rules ?? "default"] >= LEVEL_RANK[e.source?.notify ?? "default"];
  return near && (e.branch_rules ?? []).some((r) => !!r.notify?.channel);
}

// ---- what needs connections.manage ----
//
// The server judges every change by what it makes effective under the level changed
// (reviewTierNeeds in review_api.go) and refuses what needs connections.manage. These are the
// cases cheap to see from the tree, so a control that would only earn a 403 is shown disabled with
// the reason beside it instead. They look at modes and own settings only; the server stays the
// judge of the rest, a push trigger inherited from above included.

/** Whether a level's own settings, copied onto a new connection, set something only connections.manage may. */
export function settingsNeedReach(s: ReviewSettingsValues | null | undefined): boolean {
  if (!s) return false;
  return (
    !!s.model ||
    s.max_usd !== undefined ||
    s.forks !== undefined ||
    // A channel, or none over one set above: either is where a private repository's findings go.
    s.notify !== undefined ||
    (s.context_repos?.length ?? 0) > 0 ||
    s.trigger === "push" ||
    (s.branch_rules ?? []).some((r) => r.trigger === "push" || !!r.model || r.notify !== undefined)
  );
}

// The moves, deletes and restores below are judged the server's way, from the tree: each level a
// change reaches is resolved through its chain either side of it (Resolve), and compared as
// reviewTierNeeds compares — the fields that need the permission whatever they resolve to, then mode,
// trigger, the channel, and branch by branch where each rule sends a pull request. The built-in values
// are review.Defaults()'s; a level nothing sets falls back to them on both sides alike.

const DEFAULTS = { trigger: "open", forks: "command", model: "heavy", max_usd: 1 } as const;

/** One level of a chain, broadest first, with its nearness: 1 a connection, 2 a group, 3 a repository. */
type ChainLevel = { rank: number; settings?: ReviewSettingsValues | null };

/** A chain resolved, cut down to what the connections permission is about. */
type Reach = {
  mode: ReviewMode;
  trigger: ReviewTrigger;
  forks: string;
  model: string;
  maxUSD: number;
  context: string[];
  /** The channel as "team|channel", "" for none. */
  notify: string;
  rules: ReviewBranchRule[];
  /** Where each value came from, by nearness; absent is the built-in value. */
  src: Partial<Record<keyof ReviewSettingsValues, number>>;
};

const notifyKey = (n: ReviewNotify | undefined) => (n?.channel ? `${n.team ?? ""}|${n.channel}` : "");

function resolveReach(levels: ChainLevel[], fallbackMode: ReviewMode): Reach {
  const src: Reach["src"] = {};
  const nearest = <K extends keyof ReviewSettingsValues>(
    k: K,
    set: (v: ReviewSettingsValues[K]) => boolean = (v) => v !== undefined,
  ) => {
    for (let i = levels.length - 1; i >= 0; i--) {
      const v = levels[i].settings?.[k];
      if (v !== undefined && set(v)) {
        src[k] = levels[i].rank;
        return v;
      }
    }
    return undefined;
  };
  // Lists add up, broadest first, an entry already there in another case not added twice.
  const context: string[] = [];
  for (const l of levels) {
    for (const raw of l.settings?.context_repos ?? []) {
      const c = raw.trim();
      if (c && !context.some((x) => x.toLowerCase() === c.toLowerCase())) context.push(c);
    }
  }
  return {
    mode: nearest("mode") ?? fallbackMode,
    trigger: nearest("trigger") ?? DEFAULTS.trigger,
    forks: nearest("forks") ?? DEFAULTS.forks,
    model: nearest("model") ?? DEFAULTS.model,
    maxUSD: nearest("max_usd") ?? DEFAULTS.max_usd,
    context,
    notify: notifyKey(nearest("notify")),
    // A level's list is taken whole when it has any rule; the built-in list is the fallback alone.
    rules: nearest("branch_rules", (v) => (v?.length ?? 0) > 0) ?? [{}],
    src,
  };
}

/** The chain of a connection, one of its groups and a repository in it, any of the last two left out. */
function reachAt(conn: ReviewConnectionNode, group?: { settings?: ReviewSettingsValues | null }, repo?: ReviewNode): Reach {
  const levels: ChainLevel[] = [{ rank: 1, settings: conn.settings }];
  if (group) levels.push({ rank: 2, settings: group.settings });
  if (repo) levels.push({ rank: 3, settings: repo.settings });
  // conn.mode is what the connection resolves to, so where nothing in the chain sets a mode it is the
  // built-in one.
  return resolveReach(levels, conn.mode ?? "shadow");
}

/** A level that reviews nothing: off, asked for by nobody, telling no channel, sending nothing anywhere. */
function stoppedReach(e: Reach): Reach {
  return { ...e, mode: "off", trigger: "command", notify: "", rules: [] };
}

/** Each rule as Effective.WithRule applies it: where a pull request it matches goes. */
function ruleReach(e: Reach) {
  if (e.mode === "off") return [];
  const ruleAt = e.src.branch_rules ?? 0;
  const overrides = (f: keyof ReviewSettingsValues) => ruleAt >= (e.src[f] ?? 0);
  return e.rules.map((r) => ({
    base: r.base ?? "",
    head: r.head ?? "",
    live: (r.post && overrides("mode") ? r.post : e.mode) === "live",
    push: (r.trigger && overrides("trigger") ? r.trigger : e.trigger) === "push",
    model: r.model && overrides("model") ? r.model : "",
    notify: r.notify !== undefined && overrides("notify") ? notifyKey(r.notify) : e.notify,
  }));
}

/** Whether going from `before` to `after` at one level needs connections.manage: reviewTierNeeds, as a yes or no. */
function needsReach(before: Reach, after: Reach): boolean {
  if (before.forks !== after.forks || before.model !== after.model || before.maxUSD !== after.maxUSD) return true;
  if (
    before.context.length !== after.context.length ||
    before.context.some((c, i) => c.toLowerCase() !== after.context[i].toLowerCase())
  ) {
    return true;
  }
  if (before.mode === "off") before = stoppedReach(before);
  const off = after.mode === "off";
  if (!off && after.mode === "live" && before.mode !== "live") return true;
  if (!off && after.notify !== before.notify) return true;
  if (!off && after.trigger === "push" && before.trigger !== "push") return true;
  const was = ruleReach(before);
  const now = ruleReach(after);
  type R = (typeof now)[number];
  const same = (eq: (a: R, b: R) => boolean) =>
    was.length === now.length && was.every((a, i) => a.base === now[i].base && a.head === now[i].head && eq(a, now[i]));
  // Live and every push: not where every branch already did.
  if (now.some((x) => x.live) && !(was.length > 0 && was.every((x) => x.live)) && !same((a, b) => a.live === b.live)) return true;
  if (now.some((x) => x.push) && !(was.length > 0 && was.every((x) => x.push)) && !same((a, b) => a.push === b.push)) return true;
  if (now.some((x) => x.model) && !same((a, b) => a.model === b.model)) return true;
  // The channel, where a list names one: any branch announced elsewhere, or — from nothing — anywhere.
  if (!off && [...before.rules, ...after.rules].some((r) => r.notify !== undefined)) {
    if (was.length === 0 ? now.some((x) => x.notify) : !same((a, b) => a.notify === b.notify)) return true;
  }
  return false;
}

/**
 * Whether moving a repository from `from` (its group, or none) to `to` (a group, or none: the
 * connection) needs connections.manage: it inherits differently there, and goes live, onto another
 * model or budget, or is announced elsewhere (handleReviewSettingMove).
 */
export function moveNeedsReach(
  conn: ReviewConnectionNode,
  repo: ReviewNode,
  from: ReviewGroupNode | undefined,
  to: ReviewGroupNode | undefined,
): boolean {
  return needsReach(reachAt(conn, from, repo), reachAt(conn, to, repo));
}

/**
 * Whether deleting a group needs connections.manage: it takes away a model, budget, fork policy,
 * context repositories or channel of its own, or its repositories, moving up to the connection, go
 * live or are announced elsewhere there.
 */
export function deleteNeedsReach(conn: ReviewConnectionNode, group: ReviewGroupNode): boolean {
  const own = group.settings ?? {};
  if (
    own.forks !== undefined ||
    own.model !== undefined ||
    own.max_usd !== undefined ||
    own.notify !== undefined ||
    (own.context_repos?.length ?? 0) > 0
  ) {
    return true;
  }
  return group.repos.some((r) => needsReach(reachAt(conn, group, r), reachAt(conn, undefined, r)));
}

/**
 * Whether restoring a stopped connection needs connections.manage: somewhere under it, a level that
 * reviews posts live, reviews every push, announces in a channel or runs a branch rule's own model —
 * all of which the restore switches back on (reviewRestoreNeeds). A level that is off switches on
 * nothing, and a rule that only silences announces nowhere.
 */
export function restoreNeedsReach(conn: ReviewConnectionNode): boolean {
  const levels = [
    reachAt(conn),
    ...conn.groups.map((g) => reachAt(conn, g)),
    ...conn.repos.map((r) => reachAt(conn, undefined, r)),
    ...conn.groups.flatMap((g) => g.repos.map((r) => reachAt(conn, g, r))),
  ];
  return levels.some((e) => needsReach(stoppedReach(e), e));
}

/**
 * Whether restoring a repository removed from code review needs connections.manage: while it was
 * out nothing on it was reviewed, so what its settings resolve to — over the group it goes back into
 * — is switched on by the restore (restoreReviewRepo), judged as restoring a connection is.
 */
export function repoRestoreNeedsReach(conn: ReviewConnectionNode, repo: ReviewNode): boolean {
  const e = reachAt(
    conn,
    conn.groups.find((g) => g.id === repo.parent_id),
    repo,
  );
  return needsReach(stoppedReach(e), e);
}

/**
 * Whether a branch rule list's shape — which branches meet which rule, and its post, trigger, model
 * and channel — is held for connections.manage, under a level that resolves to mode and trigger:
 * some rule posts live, reviews every push, picks a model or names a channel (an empty one too), and
 * not every one does, so moving a branch from one rule to another could start it. Its types and
 * strictness stay anybody's with reviews.manage.
 */
export function rulesHeld(rules: ReviewBranchRule[], mode: ReviewMode, trigger: ReviewTrigger): boolean {
  // Where a pull request is announced is held whatever this level's own mode: a repository under it
  // that is on meets the list too, and the server judges the list where it does.
  if (rules.some((r) => r.notify !== undefined)) return true;
  if (mode === "off" || rules.length === 0) return false;
  const live = rules.map((r) => (r.post ?? mode) === "live");
  const push = rules.map((r) => (r.trigger ?? trigger) === "push");
  const some = (xs: boolean[]) => xs.some(Boolean) && !xs.every(Boolean);
  return some(live) || some(push) || rules.some((r) => !!r.model);
}

/**
 * Brings a panel into view after a pick in the list beside it — on a narrow screen, where the list
 * and the panel stack and the panel is otherwise below the fold, so a tap seemed to do nothing. On
 * a wide one they sit side by side and nothing moves. lg is where the two tabs' grids go to columns.
 */
export function revealPanel(el: HTMLElement | null) {
  if (!el || typeof window === "undefined" || !window.matchMedia("(max-width: 1023px)").matches) return;
  requestAnimationFrame(() => el.scrollIntoView({ block: "start", behavior: "smooth" }));
}
