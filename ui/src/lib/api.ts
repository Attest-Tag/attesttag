"use client";

import { useCallback, useEffect, useState } from "react";

// ---- wire types (mirror internal/app on the Go side) ----

/** One organisation a person belongs to, and what they may do in it. */
export type OrgMembership = {
  /** The organisation's public id: opaque, 32 hex characters, never a row number. */
  org_id: string;
  org_name: string;
  org_slug: string;
  role: string;
};

export type AdminUser = {
  /** The account's public id — stable across organisations, and opaque. */
  id: string;
  /** The Slack user id when they signed in that way; display only. */
  user_id: string;
  name: string;
  email: string;
  /** The organisation this session is acting in, by its public id. */
  org_id: string;
  org_name: string;
  org_slug: string;
  /** How this session was signed in: "password", "slack", "microsoft", "sso" or "signup". */
  via?: string;
  /** The console role this session holds, resolved per request. */
  role?: string;
  /** What that role may do — the console hides what a viewer cannot use. */
  permissions?: Record<string, boolean> | null;
};

export type Me = {
  signed_in: boolean;
  user: AdminUser | null;
  password_login: boolean;
  slack_login: boolean;
  /** Whether this deployment has a Microsoft Teams app registration, so a tenant can be connected. */
  msteams?: boolean;
  /** Whether it offers Sign in with Microsoft, which needs MSTEAMS_SIGNIN on top of that. */
  microsoft_login?: boolean;
  /** False while registration is closed: the sign-up form says so instead of failing on submit. */
  signup_open: boolean;
  /** Set while this browser has a workspace install parked behind the sign-in it is looking at. */
  install_pending?: boolean;
  /**
   * This deployment's public site — scheme and host only — or "" when it has none. The Get
   * started page reads its walkthrough library from there. The server sends it instead of the
   * console baking it in at build time, so the origin the page asks is always the origin the
   * CSP permits.
   */
  site_url?: string | null;
  /** Every organisation this person belongs to, for the switcher. */
  orgs?: OrgMembership[] | null;
  /** Where this session stands with the organisation's two-factor requirement. */
  two_factor?: { enabled: boolean; required: boolean; owed: boolean } | null;
  /** Which credentials this organisation accepts: "any" | "password" | "slack" | "microsoft" | "sso". */
  auth_policy?: AuthPolicy | null;
  /** The plan, the month's spend against what it allows, and whether an upgrade was asked for. */
  plan?: PlanState | null;
  /**
   * The organisation's own model key, when it has one: whether the bot is answering on it, and
   * if it cannot, why. Absent when the organisation answers on the deployment's key. Where the
   * key points is on GET /api/settings/model-key, for settings.manage only.
   */
  model_key?: { own: boolean; failing: boolean; blocked: boolean; refusal?: string } | null;
};

/** What the shell needs to know about money without asking a second endpoint. */
export type PlanState = {
  /** "free", "pro" or "enterprise". */
  plan: string;
  /** What the account may spend this month; 0 means no cap. */
  budget_usd: number;
  /** What it has spent so far this month. */
  spend_usd: number;
  /** When this organisation last asked support to upgrade it; "" if it has not. */
  requested_at?: string | null;
  support_email?: string | null;
  /** Whether this deployment sells plans at all. False on every self-host. */
  billing_enabled?: boolean;
  /** Whether prepaid credit is what actually limits this account. */
  credit_enabled?: boolean;
  /** The organisation's model calls are on its own key, so none of them draws on credit. */
  own_model_key?: boolean;
  /** What is left of it. Only sent when credit_enabled. */
  credit_balance_usd?: number;
  /**
   * Which limit has stopped the bot: "" while it is running, "credit" when the money has run
   * out, "budget" when the account reached the ceiling it set itself.
   *
   * The server says this rather than the console working it out from two numbers, because the
   * rule belongs to the code that actually refuses the turn. Two copies of it would drift, and
   * the one people would see is the wrong one.
   */
  paused_by?: "" | "credit" | "budget";
  /**
   * Which limit this account is ABOUT to reach, while there is still something to do about it.
   * "" when nothing is close. Decided by the server for the same reason paused_by is.
   *
   * Independent of paused_by: once something has actually stopped, paused_by is the thing to
   * say, and the warning is no longer news.
   */
  warn_by?: "" | "credit" | "users";
  /** People who used the bot in the last 30 days, and what the plan size allows (0 = no limit). */
  users_active?: number;
  users_limit?: number;
  /** What is left of this month's included credit, and what the month started with. */
  credit_allowance_usd?: number;
  credit_month_allowance_usd?: number;
};

// ---- billing ----

/** One plan size a Pro subscription can be bought at. */
export type BillingSize = {
  key: string;
  label: string;
  price_usd: number;
  /** Model credit the size hands over every month. 0 means it includes none. */
  included_usd: number;
  /** False for a size this deployment has no price configured for: quoted, not bought. */
  available: boolean;
  /** Fix jobs a month the size is sold with. 0 means no figure is printed for it. */
  jobs_per_month: number;
};

export type BillingSubscription = {
  status: string;
  size: string;
  size_label: string;
  unit_price_usd: number;
  quantity: number;
  amount_usd: number;
  period_start: string;
  period_end: string;
  cancel_at_period_end: boolean;
  /** Recorded by an operator with nothing being charged for it. */
  comped: boolean;
  /** Credit this size adds to the balance at each renewal. */
  included_usd: number;
  /**
   * A move to a smaller size that Stripe is holding until the period ends.
   *
   * `size` above is still what this account is ON and billed for until `pending_at` — these two
   * must never be swapped, or the screen tells somebody they have already been downgraded weeks
   * before they are.
   *
   * All three are absent unless something is genuinely waiting, and absent again if Stripe could
   * not be reached — so missing means "nothing waiting" and never an error. `pending_size_label`
   * is filled in by the server, including for a price this deployment no longer sells, where it
   * reads "another plan size" and `pending_size` is empty. Render the label, never the key.
   */
  pending_size?: string;
  pending_size_label?: string;
  pending_at?: string;
};

/** Active users in one connected workspace. These do NOT sum to the total: somebody in two
 *  workspaces is one user overall and appears in both rows. */
export type TeamActiveUsers = {
  team_id: string;
  name: string;
  users: number;
};

/** The 30-day active-user count and the size's ceiling. `over` is decided by the server, beside
 *  `paused_by` and for the same reason — a second copy of the rule in the browser would drift. */
export type ActiveUserCount = {
  users: number;
  /** 0 for a size with no ceiling, and for an account with no subscription to be over. */
  limit: number;
  over: boolean;
  window_days: number;
  by_team?: TeamActiveUsers[];
};

/** One person on the drill-down list behind the count. */
export type ActiveUser = {
  identity: string;
  team_id: string;
  team_name: string;
  slack_user: string;
  /** Resolved from Slack when the list is read; falls back to the raw id. */
  name: string;
  first_seen: string;
  last_seen: string;
  days: number;
  turns: number;
};

export type ActiveUsersResponse = {
  users: ActiveUser[];
  count: ActiveUserCount;
};

/** What POST /api/billing/size did. Three outcomes, and the console must read which rather than
 *  assume: an upgrade charges now, a downgrade is only SCHEDULED for the period end, and picking
 *  the size you are already on calls off a scheduled one. */
export type SizeChangeResult = {
  /** Money moved just now. Only ever true for an upgrade. */
  charged?: boolean;
  amount_minor?: number;
  currency?: string;
  /** The hosted invoice, when there is one worth showing. */
  invoice_url?: string;
  /** The move takes effect at the end of the current period, not now. */
  scheduled?: boolean;
  /** What they move TO at that point. `size` on the same response is what they are still ON. */
  pending_size?: string;
  pending_at?: string;
  /** A scheduled change was called off. */
  cancelled?: boolean;
};

/** One movement on the credit statement. Signed: a debit is negative. */
export type CreditEntry = {
  at: string;
  kind: "topup" | "debit" | "refund" | "adjustment" | "grant" | "included";
  amount_usd: number;
  balance_after_usd: number;
  currency: string;
  external_id: string;
  note: string;
  actor: string;
};

/**
 * An enterprise account's deal, as its own Billing screen shows it. The operator writes it; nothing
 * on this screen changes it. The operator's own note about the deal is deliberately not here.
 */
export type EnterpriseDeal = {
  /** People the deal is sold for. 0 = no ceiling. Shown, never enforced. */
  user_limit: number;
  /** Fix jobs a month in the deal. 0 = no figure printed. */
  job_limit: number;
  /** What the deal costs per `interval`. 0 = by agreement, and not printed. */
  fee_usd: number;
  interval: "month" | "year" | string;
  /** Model credit the deal includes each month, spent first and gone at the month's end. */
  included_usd: number;
  /** How it is paid: by subscribing to its own Stripe price here, at a link, or by invoice. */
  paid_by: "subscription" | "link" | "invoice";
  /** The page to pay at, when paid_by is "link". Only sent to members who can manage billing. */
  pay_url?: string;
  /** Already subscribed to the deal's price: the Subscribe button has done its job. */
  subscribed: boolean;
};

/** Everything the Billing tab draws, in one call. */
export type BillingView = {
  enabled: boolean;
  /** Mirrors the billing.manage permission, so the panel never re-derives a permission rule. */
  can_manage: boolean;
  plan: string;
  currency: string;
  subscription: BillingSubscription | null;
  credit: {
    /** Prepaid credit only — money the customer bought. Never expires. */
    balance_usd: number;
    enforced: boolean;
    low_threshold_usd: number;
    /** How far past zero spending already in flight may take it before the bot refuses. */
    overdraft_usd: number;
    lifetime_topup_usd: number;
    lifetime_debit_usd: number;
    /** What is left of this period's included credit. 0 when there is no plan allowance. */
    allowance_usd: number;
    /** What the period started with — the two differ after a mid-period upgrade. */
    allowance_granted_usd: number;
    /** When the allowance lapses. Empty when there is none, or it has already passed. */
    allowance_expires: string;
    /** allowance_usd + balance_usd. The figure the overdraft floor is compared against, added
     *  up by the server so the console cannot get a different answer from the bot. */
    spendable_usd: number;
    /** Whether the credit floor applies at all — bought credit, or a live allowance. */
    metered: boolean;
  };
  budget: {
    monthly_budget_usd: number;
    platform_budget_usd: number;
    effective_budget_usd: number;
    month_spend_usd: number;
  };
  paused_by: "" | "credit" | "budget";
  /** The organisation's model calls are on its own key: the credit shown here is not being spent. */
  own_model_key?: boolean;
  /** People who used the bot in the last 30 days, against the size's ceiling. */
  users: ActiveUserCount;
  /** This calendar month's fix jobs against the plan's figure. limit 0 means no figure was
   *  printed for this plan. Shown, never enforced. */
  jobs: { used: number; limit: number };
  /** The last "Talk to us" request, when there has been one: the size with no price mails
   *  support, and the screen says when it last did. */
  size_request?: { at: string; size: string; size_label: string } | null;
  /** The deal, when the account is on the enterprise plan; absent otherwise. When present the
   *  screen shows it instead of the size picker — there is nothing on the ladder to buy. */
  enterprise?: EnterpriseDeal | null;
  sizes: BillingSize[];
  topup: { min_usd: number; max_usd: number; presets: number[] };
  ledger: CreditEntry[] | null;
  /** A payment has been taken and its webhook has not landed yet. */
  pending_checkout: boolean;
  support_email: string;
  /** Held back from members without billing.manage: these name the account to Stripe. */
  stripe?: { customer_id: string; subscription_id: string };
};

/** Which sign-in methods an organisation allows. Mirrors the Go constants in settings.go. */
export type AuthPolicy = "any" | "password" | "slack" | "microsoft" | "sso";

/** An organisation's registered identity provider. The client secret is write-only: it goes in
 *  at registration, is stored sealed, and no endpoint gives it back. */
export type SSOProvider = {
  provider_id: string;
  kind: "oidc";
  domain: string;
  issuer: string;
  client_id: string;
  /** Until this is true the provider is inert — registered, and nobody can sign in through it. */
  domain_verified: boolean;
  created_at: string;
  verified_at?: string;
};

/** GET /api/settings/sso: the row, plus the two values that have to be pasted elsewhere — one
 *  into the identity provider, one into DNS. Empty object when nothing is registered. */
export type SSOView = {
  provider?: SSOProvider | null;
  /** The redirect URI to register on the IdP application. */
  redirect_uri?: string;
  record_name?: string;
  record_value?: string;
};

/** GET /api/account — you, and the credentials that get you in. */
/** The account itself, as the users table holds it — not the session's AdminUser. */
export type AccountUser = {
  id: number;
  email: string;
  email_verified: boolean;
  name: string;
  status: string;
  created_at: string;
  last_seen: string;
};

/**
 * What proveIdentity will accept from this account, for a screen that is about to need it:
 * "password" when the account has one, "code" when an authenticator is enrolled, "recent" when
 * it has neither and only a session minted minutes ago will do.
 */
export type ProofKind = "password" | "code" | "recent";

export type Account = {
  user: AccountUser;
  role: string;
  org: { id: string; name: string; slug: string };
  has_password: boolean;
  identities: Identity[] | null;
  methods: Record<string, boolean>;
  /** How the session reading this was signed in: "password" | "slack" | "signup". */
  signed_in_with: string;
  two_factor: {
    enabled: boolean;
    /** A secret exists but no code has been typed back: enrolment was abandoned. */
    pending: boolean;
    required: boolean;
    recovery_left: number;
    /** What turning it on will ask for — the same proof taking it off has always needed. */
    proof: ProofKind;
  };
  auth_policy: AuthPolicy;
};

/**
 * The organisation as the console calls it: the account. Everything here is readable by any
 * member — the Users tab already shows them each other — but `can_delete` is the session's own
 * answer, so the danger zone appears for the person who may actually press it and nobody else.
 */
export type OrgAccount = {
  /** The organisation's public id — opaque, not a row number. */
  id: string;
  name: string;
  slug: string;
  created_at: string;
  /** Who signed the account up. Null on an account old enough to predate the field. */
  owner: {
    /** Their account's public id. */
    id: string;
    name: string;
    email: string;
    is_you: boolean;
    /** False once they have left and been removed from Users: the admins inherit the delete. */
    is_member: boolean;
  } | null;
  members: number;
  workspaces: number;
  can_delete: boolean;
  /** Why not, in the words the screen shows. Empty when `can_delete`. */
  why_not: string;
  /** What deleting will ask for. */
  proof: ProofKind;
};

export type UsageRow = {
  TeamID: string;
  Channel: string;
  /** What the console calls it: the channel's name, "DM with …", or one of the console's own
   *  lanes. Only /api/overview sends it; /v1/usage has snake_case rows of its own. */
  ChannelName?: string;
  Turns: number;
  In: number;
  Out: number;
  Cost: number;
};

export type Overview = {
  bot: string;
  team: string;
  month_spend_usd: number;
  /** The enforced budget: the setting under the plan's ceiling. */
  budget_usd: number;
  /** "free", "pro" or "enterprise". */
  plan: "free" | "pro" | "enterprise" | string;
  turns_today: number;
  turns_7d: number;
  docs: number;
  chunks: number;
  bundles: number;
  connections: number;
  /** Channels the bot is in, across every connected workspace. */
  scopes: number;
  /** Connected Slack workspaces. */
  teams: number;
  routines: number;
  memories: number;
  recent_errors: RecentError[] | null;
  top_channels: UsageRow[] | null;
  /** The chart series. Only the console's own endpoint fills this in; /v1/usage never does. */
  charts?: OverviewCharts | null;
};

/** What the overview plots: a day-by-day series, and this month split by model and by tool. */
export type OverviewCharts = {
  /** One entry per day, oldest first, with no gaps — a quiet day arrives as a zero. */
  daily: DayUsage[] | null;
  models: ModelUsage[] | null;
  tools: ToolUsage[] | null;
};

/** One UTC day of turns. `day` is "YYYY-MM-DD" and must be formatted without a zone shift. */
export type DayUsage = {
  day: string;
  turns: number;
  in: number;
  out: number;
  cost: number;
};

/** What one model cost this month. An empty `model` is a turn that never named one. */
export type ModelUsage = {
  model: string;
  turns: number;
  in: number;
  out: number;
  cost: number;
};

/** One tool's month: how often the bot reached for it, and how much of that failed. */
export type ToolUsage = {
  name: string;
  calls: number;
  failed: number;
};

/** One failed tool call, as the overview card shows it. `id` opens the same row on Activity. */
export type RecentError = {
  id: number;
  at: string;
  name: string;
  /** The call's arguments, cut short by the server. */
  args: string;
};

/** One connected Slack workspace. The account can hold several. */
export type Team = {
  team_id: string;
  /** Which chat platform the workspace is on. A Teams tenant's team_id is "msteams:" + its id. */
  platform?: "slack" | "msteams" | string;
  name: string;
  domain: string;
  /** image_132 from team.info; may be empty. */
  icon: string;
  bot_user_id: string;
  /** Name the workspace shows for bot_user_id, resolved per request; may be absent. */
  bot_user_name?: string;
  status: "active" | "revoked" | string;
  installed_by: string;
  /** Name the workspace shows for installed_by, resolved per request; may be absent. */
  installed_by_name?: string;
  installed_at: string;
  revoked_at: string;
  last_error: string;
  email_scope: boolean;
  dm_scope: boolean;
  scopes: string;
  /** Connected, but granted fewer scopes than the app needs: it works only partly. */
  needs_reinstall: boolean;
};

export type TeamsResponse = {
  teams: Team[];
  /** False when SLACK_CLIENT_ID / SLACK_CLIENT_SECRET are unset: nothing can be connected. */
  install_configured: boolean;
  /** Where "Add to Slack" points. A real navigation, not a fetch: it leaves for Slack. */
  install_url: string;
};

/**
 * GET /api/onboarding — where a new organisation stands on the walk from nothing to a bot
 * answering in Slack. Derived server-side from what actually happened, never stored, so there is
 * no "dismissed" flag for the console to get wrong.
 */
export type OnboardingStep = "install" | "channel" | "mention" | "invite" | "done";

/** The workspace behind this person's Slack sign-in, already connected to another organisation. */
export type TakenWorkspace = {
  name: string;
  /** Who connected it, as their own workspace names them; "" when Slack cannot say. */
  installer: string;
  /** The bot can DM them: the workspace granted im:write and we know who they are. */
  can_ask: boolean;
  /** They have already been messaged on this person's behalf today. */
  asked: boolean;
};

export type Onboarding = {
  step: OnboardingStep;
  done: boolean;
  /** Whether this person may connect a workspace; a viewer is told to ask instead. */
  can_install: boolean;
  /** False when SLACK_CLIENT_ID / SLACK_CLIENT_SECRET are unset on the server. */
  install_configured: boolean;
  install_url: string;
  /** Whether a Microsoft Teams organisation can be connected on this deployment. */
  msteams?: boolean;
  /** The platform of the first connected workspace, once there is one. */
  platform?: "slack" | "msteams";
  teams: { team_id: string; name: string }[];
  channels: { team_id: string; slack_id: string; name: string }[];
  /** What the workspace calls the bot, for the /invite and mention lines. */
  bot_handle: string;
  turns: number;
  /** Set when Add to Slack cannot succeed: that workspace belongs to another organisation. */
  taken?: TakenWorkspace | null;
  members: number;
  invited: number;
  /** False for a role that cannot invite; the last step is then not theirs to do. */
  can_invite: boolean;
  /** True while the address behind the account is unconfirmed: steps one and four are refused. */
  email_unverified: boolean;
};

/** GET /api/auth/invite — what the join screen shows before it spends the invitation. */
export type InvitePreview = {
  org: { id: string; name: string };
  role: string;
  email: string;
  /** True when the link names nobody — anyone holding it may join. */
  shared?: boolean;
  /** The organisation this session is in now, and whether the join can offer to leave it. */
  own?: {
    id: string;
    name: string;
    leavable: boolean;
    reason?: string;
  } | null;
};

export type MemberEdits = "inherit" | "allow" | "block";

export type Scope = {
  id: number;
  /** Where this sits in the hierarchy: the account, one connected Slack workspace, or a channel. */
  kind: "workspace" | "team" | "channel";
  /** The connected Slack workspace this belongs to; "" for the account itself. */
  team_id: string;
  team_name: string;
  slack_id: string;
  name: string;
  /** A private channel. Slack stopped encoding this in the id, so it is stored per channel. */
  is_private: boolean;
  instructions: string;
  default_model: string;
  member_edits: string;
  /** Cap in USD; 0 = none. The caps above it still apply. */
  monthly_budget_usd: number;
  /** Read every message and decide for itself whether to answer, react, or stay quiet. Off unless asked for. */
  read_all: "inherit" | "on" | "off" | string;
  /** Answer mail forwarded to this channel's Slack address. Off unless asked for; admins only. */
  email_intake: "inherit" | "on" | "off" | string;
  /** Whether this scope's allow rules are consulted on a turn a forwarded email started. */
  email_auto_writes: "inherit" | "on" | "off" | string;
  /** owner/name assumed for repo questions; "" = inherit (channel) or none (workspace). */
  default_repo: string;
  bundle_ids: number[] | null;
  /** Connections attached on their own, outside a bundle. */
  connection_ids: number[] | null;
  /** Channel-level auto mode allow rules, applied on top of the ones in Settings. */
  allow_rules: string[] | null;
};

export type AccessRow = {
  host: string;
  connection: string;
  bundle: string;
  /** How the grant reached this scope: "bundle", "connection" (one-off) or "domain". */
  via?: string;
  origin: string;
  credential: string;
  writes?: string;
};

/** A repository connection reachable from a scope, for the Repositories section. */
export type RepoRow = {
  connection_id: number;
  repo: string;
  name: string;
  bundle: string;
  via?: string;
  origin: string;
};

/** Instructions this scope carries but cannot edit here: the workspace's, or a bundle's. */
export type InheritedInstructions = {
  source: string;
  kind: "workspace" | "bundle" | string;
  /** Where a bundle is attached: "workspace" or "here". */
  where?: string;
  text: string;
};

/** Allow rules in force here that were written elsewhere: Settings, or the workspace. */
export type InheritedRules = {
  source: string;
  kind: "settings" | "workspace" | string;
  rules: string[];
};

export type ScopeDetail = {
  scope: Scope;
  access: AccessRow[];
  repos: RepoRow[];
  /** The default repo in force here: the scope's own, else the workspace's. */
  default_repo_effective: string;
  /** What comes down from the workspace, Settings and the attached bundles. */
  inherited?: {
    instructions: InheritedInstructions[];
    allow_rules: InheritedRules[];
    /** email_auto_writes resolved down the chain: what actually applies here, never "inherit". */
    email_auto_writes?: "on" | "off";
  };
};

/** One tool call a playground turn made, in the order it ran. */
export type PlaygroundTool = {
  name: string;
  args: string;
  result: string;
  ok: boolean;
  ms: number;
};

/** POST /api/playground — one turn run against a channel's real settings, with no Slack in it. */
export type PlaygroundReply = {
  /** The conversation this turn belongs to; send it back to carry on in the same one. */
  thread: string;
  reply: string;
  model: string;
  rounds: number;
  tools: PlaygroundTool[];
  /** Writes the turn stopped at: what would have gone out behind a Confirm card. */
  held: string[];
  tokens_in: number;
  tokens_out: number;
  cost_usd: number;
  /** A turn that failed rather than a request that was refused; the tools it ran still came back. */
  error: string;
};

/** One earlier turn of an assistant conversation. The browser holds the thread, so this rides
 *  back with every question rather than living on the server. */
export type AssistantMessage = { role: "you" | "assistant"; text: string };

/** One field as the proposal card shows it, before and after. */
export type ProposalChange = { key: string; label: string; from: string; to: string };

/**
 * One call Confirm makes. The server builds these from ids it resolved inside the organisation
 * — the model never writes a method or a path — and each one is an endpoint the console already
 * offers on a page, so pressing Confirm is the person making a request they could have made by
 * hand, under the same permission.
 */
export type ProposalStep = {
  method: string;
  path: string;
  body?: Record<string, unknown>;
  label: string;
};

/**
 * A change the assistant staged and wrote nowhere. `id` rides in each step's body, where the
 * endpoint ignores it for writing and records it — which is what ties the proposal's audit row
 * to the save's.
 */
export type Proposal = {
  id: string;
  kind: "channel" | "approval";
  target: string;
  changes: ProposalChange[];
  steps: ProposalStep[];
};

/** GET /api/assistant/turns — one recorded console question and what came back. */
export type AssistantTurnRow = {
  id: number;
  at: string;
  actor_name: string;
  conversation: string;
  question: string;
  reply: string;
  model: string;
  /** The console page it was asked from, which is what makes "what can this reach" legible later. */
  page: string;
  tokens_in: number;
  tokens_out: number;
  cost_usd: number;
  tool_calls: number;
  proposals: number;
  error: string;
};

/** One file that was attached to a question, as the reply reports it back. */
export type AssistantFile = { name: string; kind: "image" | "text" | "skipped"; note?: string };

/** POST /api/assistant — one console question, answered. */
export type AssistantReply = {
  reply: string;
  model: string;
  rounds: number;
  tools: PlaygroundTool[];
  files: AssistantFile[] | null;
  /** Staged for a Confirm button. Nothing has been written. */
  proposals: Proposal[] | null;
  tokens_in: number;
  tokens_out: number;
  cost_usd: number;
  /** A turn that failed rather than a request that was refused; what it did still came back. */
  error: string;
};

export type Header = { name: string; prefix: string };

export type CredType =
  | "bearer"
  | "basic"
  | "header"
  | "query"
  | "oauth2_cc"
  | "gcp_sa"
  | "aws_sigv4"
  | "oauth_user"
  | "mcp";

/** A GitHub App installation: an account whose admin ticked which repositories we may see. */
export type GithubInstall = {
  installation_id: number;
  account_login: string;
  repo_selection: string;
  status: string;
};

export type GithubInstalls = {
  installations: GithubInstall[];
  install_configured: boolean;
  install_url?: string;
  setup_url?: string;
  /** What is missing when install_configured is false, so the disabled entry can name it. */
  install_missing?: string[];
};

export type Connection = {
  id: number;
  bundle_id: number;
  name: string;
  preset: string;
  cred_type: CredType | string;
  allowed_hosts: string[] | null;
  path_prefixes: string[] | null;
  methods: string[] | null;
  headers: Header[] | null;
  writes: "confirm" | "auto" | "all" | string;
  notes: string;
  /** May an approved access request spend this credential. Off unless an admin turns it on. */
  allow_grants: boolean;
  /** owner/name when the connection is a repository (github preset). */
  repo: string;
  /** Non-zero when the repository is reached through a GitHub App installation rather than a
   * token of its own. It is what groups repositories by the credential behind them. */
  github_installation_id: number;
  /** A short digest of the token a repository was connected with — never the token. Two
   * repositories sharing one token share this, which is what groups them exactly rather than
   * by owner. Empty on app-backed rows and on ones connected before it was recorded. */
  secret_fp: string;
  /** Repositories: the older single-command test override; folded into `recipe`. */
  test_cmd: string;
  /** Repositories: how the fix worker sets up, builds and tests it. Absent = work it out. */
  recipe?: Recipe | null;
  status: string;
  last_used: string;
  created_by: string;
  created_at: string;
  has_secret: boolean;
  /** Scopes this connection is attached to on its own, outside its bundle. */
  scope_ids: number[] | null;
};

/** One command the worker runs inside the repository: a program and its arguments, never a
 * shell line. `argv` is what the API stores; the console edits it as a single line. */
export type RecipeStep = {
  name?: string;
  /** Write-only: one command line, split into `argv` when saved. Never returned. */
  run?: string;
  argv: string[];
  dir?: string;
  timeout_s?: number;
  optional?: boolean;
};

/** How one repository is set up, built and tested. `source` says who decided: the repository's
 * own .attest/recipe.yaml, an admin here, or the worker working it out from the clone. */
export type Recipe = {
  source: "repo_file" | "connection" | "detected" | "none" | string;
  ecosystem?: string;
  workdir?: string;
  tools?: Record<string, string> | null;
  setup?: RecipeStep[] | null;
  build?: RecipeStep | null;
  lint?: RecipeStep | null;
  test?: RecipeStep | null;
  services?: string[] | null;
  why?: string;
};

export const stepLine = (s?: RecipeStep | null): string => (s?.argv ?? []).join(" ");

export type Domain = { id: number; bundle_id: number; host: string; ports: string };

export type Skill = {
  id: number;
  bundle_id: number;
  name: string;
  content: string;
  enabled: boolean;
  updated_at: string;
};

/** GET /api/connections/{id}/oauth/status for an MCP connection. */
export type OAuthStatus = {
  signed_in: boolean;
  /** Unix seconds; 0 = no expiry. */
  expires_at: number;
  has_refresh: boolean;
  client_id: string;
};

export type OAuthStart = { ok: true; url: string } | { ok: false; error: string };

/** One person's sign-in to an oauth_user connection. Never carries a token: the sealed
 * credential stays on the server, and what comes back is the label and the dates. */
export type ConnectionMember = {
  id: number;
  conn_id: number;
  team_id: string;
  slack_user_id: string;
  /** The address they connected as, a label only. */
  account: string;
  status: string;
  created_at: string;
  last_used: string;
};

/** GET /api/connections/{id}/members. `redirect_uri` is what the admin registers with the
 * provider, and it has to match character for character. `options` is what this organisation
 * actually configured — which parts are on and which may write — read back off the connection's
 * own scopes, so the edit form can show that instead of the preset's defaults. */
export type ConnectionMembers = {
  members: ConnectionMember[] | null;
  options: ConnectionOption[] | null;
  redirect_uri: string;
};

export type Bundle = {
  id: number;
  name: string;
  instructions: string;
  tool_packs: string[] | null;
  created_by: string;
  created_at: string;
  connections: Connection[] | null;
  domains: Domain[] | null;
  skills: Skill[] | null;
  scope_ids: number[] | null;
  used_in: number;
};

export type TestCall = { method: string; path: string; body?: string };

export type Preset = {
  id: string;
  name: string;
  category: string;
  cred_type: CredType | "custom";
  hosts: string[] | null;
  header_name?: string;
  header_prefix?: string;
  extra_headers?: Header[];
  static_headers?: Record<string, string>;
  scopes?: string;
  path_prefixes?: string[] | null;
  /** oauth_user: where a person consents, and where the code is spent. */
  auth_url?: string;
  token_url?: string;
  secret_label: string;
  secret_hint: string;
  placeholder: string;
  docs_url: string;
  test: TestCall;
  notes: string;
  has_pack: boolean;
  /** Parts of a multi-service preset an admin may leave out. Absent means the whole service. */
  options?: PresetOption[] | null;
};

/**
 * One part of a service, offered as a checkbox. Ticking it adds its hosts and path prefixes to
 * the connection and its `read_scopes` to what each person is asked to consent to; the write
 * toggle adds `write_scopes` on top. A part with no `write_scopes` is only ever read, so no
 * toggle is drawn for it.
 */
export type PresetOption = {
  id: string;
  label: string;
  hint?: string;
  hosts?: string[] | null;
  path_prefixes?: string[] | null;
  read_scopes?: string;
  write_scopes?: string;
  write_label?: string;
  default?: boolean;
  default_write?: boolean;
};

export type ConnectionOption = { id: string; write: boolean };

export type Secret = {
  token?: string;
  user?: string;
  password?: string;
  header_name?: string;
  client_id?: string;
  client_secret?: string;
  token_url?: string;
  scopes?: string;
  sa_json?: string;
  mcp_url?: string;
  aws_key_id?: string;
  aws_secret?: string;
  aws_region?: string;
  aws_service?: string;
  aws_session_token?: string;
};

export type ConnectionInput = {
  name: string;
  preset: string;
  cred_type: string;
  allowed_hosts: string[];
  path_prefixes: string[];
  methods: string[];
  headers: Header[];
  writes: string;
  notes: string;
  allow_grants?: boolean;
  secret?: Secret;
  header_values?: Record<string, string>;
  /** Which parts of a multi-service preset were ticked. Sent only for a preset that has them. */
  options?: ConnectionOption[];
};

export type TestResult = {
  ok: boolean;
  status?: number;
  body?: string;
  error?: string;
};

export type Document = {
  id: number;
  path: string;
  name: string;
  size: number;
  uploaded_by: string;
  scope: string;
  status: string;
  chunks: number;
  last_error: string;
  updated_at: string;
  /** The Drive sync that owns this document, when one does. Editing it by hand is undone by
   *  the next sync, so the table says so rather than letting somebody find out later. */
  drive_sync_id?: number;
};

/** A Drive folder kept in step with a folder in Documents. */
export type DriveSync = {
  id: number;
  connection_id: number;
  connection_name: string;
  /** The access bundle the credential is filed under. Two bundles can each hold a "Drive". */
  bundle_name: string;
  folder_id: string;
  folder_name: string;
  /** Where in Documents it lands. Empty is the top level. */
  dest: string;
  scope: string;
  recurse: boolean;
  enabled: boolean;
  last_run: string;
  /** "" before the first run, then ok | error | running. */
  last_status: string;
  last_error: string;
  last_added: number;
  last_updated: number;
  last_removed: number;
  /** How many documents this sync currently owns. */
  docs: number;
  created_by: string;
  created_at: string;
};

export type DriveReport = {
  folder_name: string;
  added: number;
  updated: number;
  removed: number;
  unchanged: number;
  /** "name — reason", for files Drive has that the index cannot read. */
  skipped: string[] | null;
  took: string;
};

export type DriveSyncs = { syncs: DriveSync[] | null; every_hours: number };

/** One entry in a Drive preview: a folder with what is under it, or a file with the name it
 *  would have as a document — or the reason it would not become one. */
export type DriveNode = {
  name: string;
  folder?: boolean;
  /** The document's name, once it is one: a native Doc gains the extension its export implies. */
  doc?: string;
  /** Why it would not come in. On a folder, only ever "subfolders are off". */
  skip?: string;
  size?: number;
  children?: DriveNode[];
};

/** What a pass over a folder would do, worked out from Drive's listing without copying anything. */
export type DrivePreview = {
  folder_name: string;
  folder_id: string;
  tree: DriveNode;
  files: number;
  skipped: number;
  folders: number;
  bytes: number;
  /** Entries the tree leaves out; the counts still cover them. */
  more: number;
  /** The walk stopped at the limits a pass has, so even the counts are of the first slice. */
  capped: boolean;
  took: string;
};

/** A connection that can reach Drive, for the picker when a sync is added. */
export type DriveConnection = {
  id: number;
  name: string;
  preset: string;
  status: string;
  bundle_id: number;
  bundle_name: string;
};

export type IngestReport = {
  Docs: number;
  Chunks: number;
  Embedded: number;
  Unchanged: number;
  Deleted: number;
  Errors: string[] | null;
  Took: number;
};

export type Memory = {
  ID: number;
  TeamID: string;
  /** "team:T…" or "channel:T…/C…" — the workspace is part of the key. */
  Scope: string;
  Text: string;
  CreatedBy: string;
  At: string;
};

/**
 * A note belonging to one person. It has no scope, because it has an owner instead: the server
 * keys these by (workspace, Slack user) and every read takes the owner as an argument, so this
 * list is only ever your own. Nobody else in the organisation — admins included — has a route
 * that returns somebody else's.
 */
export type PersonalMemory = {
  id: number;
  team_id: string;
  team_name: string;
  text: string;
  created_at: string;
  updated_at: string;
};

/** One Slack account the signed-in person has proved they control, in a connected workspace. */
export type PersonalStore = {
  team_id: string;
  team_name: string;
  slack_user_id: string;
};

export type PersonalMemories = {
  identities: PersonalStore[] | null;
  memories: PersonalMemory[];
};

export type Artifact = {
  ID: number;
  TeamID: string;
  TeamName: string;
  Title: string;
  /** md | txt | csv | json | yaml | html */
  Kind: string;
  Bytes: number;
  Channel: string;
  ChannelName: string;
  ThreadTS: string;
  CreatedBy: string;
  CreatedByName: string;
  /** Slack permalink to the file in the thread; "" if the lookup failed. */
  Permalink: string;
  At: string;
  /** Only present on GET /api/artifacts/{id}. */
  Content?: string;
};

export type Routine = {
  ID: number;
  TeamID: string;
  Channel: string;
  Cron: string;
  TZ: string;
  Prompt: string;
  CreatedBy: string;
  Enabled: boolean;
  NextRun: string;
  LastRun: string;
  LastError: string;
  /** "always" posts every run; "when_needed" stays out of the channel unless NotifyWhen is met. */
  Notify: string;
  NotifyWhen: string;
  LastStatus: string;
  /**
   * Which model its runs answer on: "" follows the channel, then Settings; "heavy" is the
   * advanced model; otherwise one of the models Settings offers to channels.
   */
  Model: string;
  /** Whether this routine's writes run without a Confirm card. */
  AutoConfirm: boolean;
};

/**
 * One execution of a routine — including the ones that decided to stay quiet, which leave no
 * trace in Slack at all. Output is previewed in the listing (More says it was cut) and comes
 * back whole from /api/routine-runs/{id}.
 */
export type RoutineRun = {
  ID: number;
  RoutineID: number;
  /** posted | quiet | failed | skipped */
  Status: string;
  Reason: string;
  Output: string;
  More: boolean;
  Error: string;
  ThreadTS: string;
  TokensIn: number;
  TokensOut: number;
  CostUSD: number;
  StartedAt: string;
  FinishedAt: string;
  MS: number;
};

// ---- fix jobs (internal/app/jobs_store.go, jobs_proto.go, jobs_api.go) ----

export type JobStatus =
  | "queued"
  | "starting"
  | "running"
  | "stale"
  | "cancelling"
  | "succeeded"
  | "failed"
  | "cancelled"
  | "timeout"
  | string;

/** Statuses before a terminal one (mirrors jobActiveStatuses on the Go side). */
export const JOB_ACTIVE_STATUSES = new Set(["queued", "starting", "running", "stale", "cancelling"]);

export function isJobActive(status: string): boolean {
  return JOB_ACTIVE_STATUSES.has(status);
}

/** One row of GET /api/jobs: the job's columns plus names and links resolved by the server. */
export type Job = {
  id: number;
  status: JobStatus;
  channel: string;
  thread_ts: string;
  requester: string;
  approved_by: string;
  /** "confirm" | "rule:<text>" | "" */
  approval: string;
  connection_id: number;
  repo: string;
  base_branch: string;
  branch: string;
  title: string;
  engine: string;
  model: string;
  budget_usd: number;
  timeout_s: number;
  draft_pr: boolean;
  dispatcher: string;
  execution_ref: string;
  claimed_at: string;
  claim_count: number;
  worker_info: string;
  status_ts: string;
  phase: string;
  last_seq: number;
  last_event_at: string;
  cancel_requested: boolean;
  cancel_by: string;
  cancel_reason: string;
  cancel_requested_at: string;
  pr_url: string;
  error: string;
  cost_usd: number;
  tokens_in: number;
  tokens_out: number;
  created_at: string;
  started_at: string;
  finished_at: string;
  channel_name: string;
  requester_name: string;
  approved_by_name?: string;
  duration_s: number;
  /** The platform console's page for the execution; "" where there is none (local, k8s, docker). */
  console_url: string;
  thread_link: string;
};

export type JobUsage = { in: number; out: number; cost_usd: number };

export type JobEvent = {
  id?: number;
  seq: number;
  at?: string;
  /** phase | log | tests | usage | heartbeat | warn */
  kind: string;
  phase?: string;
  /** started | ok | failed | skipped, on phase events */
  status?: string;
  message?: string;
  data?: unknown;
  usage?: JobUsage;
  created_at?: string;
};

export type JobToolEvidence = { tool: string; args: string; result: string };

export type JobConstraints = {
  engine: string;
  model: string;
  budget_usd: number;
  timeout_s: number;
  draft_pr: boolean;
  test_cmd?: string;
  recipe?: Recipe | null;
  branch_prefix?: string;
  branch_suffix: string;
  max_rounds: number;
};

export type JobSpec = {
  v: number;
  repo: string;
  connection_id: number;
  base_branch: string;
  branch: string;
  title: string;
  requirement: string;
  evidence?: string;
  acceptance: string[] | null;
  ticket?: string;
  files_hint?: string[] | null;
  thread_text?: string;
  thread_link?: string;
  tool_evidence?: JobToolEvidence[] | null;
  requester: string;
  requester_name?: string;
  channel: string;
  thread_ts: string;
  constraints: JobConstraints;
};

export type JobPR = { url: string; number: number; branch: string; base: string; head_sha?: string; draft: boolean };

export type JobTestRun = { ran: boolean; ok: boolean; passed?: number; failed?: number; seconds?: number; output?: string };

export type JobTests = {
  command?: string;
  /** Why nothing ran: no suite found, or its runner is not installed in the worker. */
  skipped?: string;
  before: JobTestRun;
  after: JobTestRun;
};

export type JobDiffStat = { files: number; insertions: number; deletions: number };

export type JobError = { code?: string; message?: string };

export type JobResult = {
  seq: number;
  status: string;
  summary: string;
  /** A caveat on a job that still succeeded: the engine stopped at its turn or spend cap. */
  note?: string;
  pr?: JobPR | null;
  branch?: string;
  head_sha?: string;
  tests: JobTests;
  diff_stat: JobDiffStat;
  files_changed?: string[] | null;
  usage: JobUsage;
  model?: string;
  engine?: string;
  log_tail?: string;
  error: JobError;
  ticket_comment?: string;
  duration_s?: number;
};

/** GET /api/jobs/{id}. */
export type JobDetail = {
  job: Job;
  spec: JobSpec | null;
  result: JobResult | null;
  events: JobEvent[] | null;
  diff_bytes: number;
  /** Slack permalink of the diff file, when it was posted. */
  diff_link: string;
};

export type TurnRow = {
  TeamID: string;
  TeamName: string;
  At: string;
  Channel: string;
  ChannelName: string;
  ThreadTS: string;
  Model: string;
  In: number;
  Out: number;
  Cost: number;
};

export type ToolCallRow = {
  ID: number;
  TeamID: string;
  At: string;
  Channel: string;
  ThreadTS: string;
  Name: string;
  Args: string;
  /** What the tool handed back to the model, cut to a preview when More is set. */
  Result: string;
  /** What the console calls the conversation. Only the Activity list sends it. */
  ChannelName?: string;
  OK: boolean;
  MS: number;
  /** Result was cut; GET /api/tool-calls/{ID} has the whole thing. */
  More: boolean;
  /**
   * The call was on somebody's own account — their personal connection or their notes — so Args
   * and Result are not what it sent and got back but what the log kept of it, as JSON: whose it
   * was, the connection, method and endpoint, the status and the size. Both are empty on a row
   * logged before the log kept anything.
   */
  Private?: boolean;
};

export type ProxyAudit = {
  id: number;
  team_id: string;
  channel: string;
  /** What the console calls the conversation: "#ops", "DM with …", "Group chat". */
  channel_name?: string;
  thread_ts: string;
  requester: string;
  connection_id: number;
  method: string;
  host: string;
  path: string;
  status: number;
  ms: number;
  blocked: string;
  created_at: string;
};

export type Activity = {
  turns: TurnRow[] | null;
  tool_calls: ToolCallRow[] | null;
  proxy: ProxyAudit[] | null;
};

/** One line of the audit log: who did what, from where, and what came of it. */
export type AuditEvent = {
  id: number;
  at: string;
  /** A dotted verb — "auth.sign_in", "connection.deleted" — or "console.request" for a write no handler named. */
  action: string;
  outcome: "ok" | "denied" | "failed" | string;
  via: "console" | "api_key" | "slack" | "operator" | "system" | string;
  /** The actor's public account id; empty for a Slack-side actor, the operator or the system. */
  actor_id: string;
  actor_email: string;
  actor_name: string;
  /** The Slack user id, when the act happened in Slack. */
  actor_slack: string;
  team_id: string;
  target_kind: string;
  target_id: string;
  target_name: string;
  ip: string;
  user_agent: string;
  /** Free-form JSON the event wrote about itself. Never a secret. */
  details: Record<string, unknown> | null;
};

/** GET /api/audit: a page of events, the actions this organisation's log uses, and the cursor for the next page. */
export type AuditPage = {
  events: AuditEvent[] | null;
  actions: string[] | null;
  /** Pass as ?before= for the page after this one; 0 when this page was the last. */
  next_before: number;
};

export type EffectiveSettings = {
  Model: string;
  HeavyModel: string;
  EmbedModel: string;
  MonthlyBudgetUSD: number;
  /** The ceiling the setting cannot lift: the free plan's budget, or the operator's. 0 = none. */
  PlatformBudgetUSD: number;
  /** What is actually enforced: the setting, under the ceiling. */
  EffectiveBudgetUSD: number;
  /** "free", "pro" or "enterprise". Only the operator changes it; a free account asks by email. */
  Plan: "free" | "pro" | "enterprise" | string;
  Timezone: string;
  HistoryLimit: number;
  MaxToolRounds: number;
  RoutineRounds: number;
  RoutineMinutes: number;
  BotName: string;
  /** Turns per user per hour; 0 = off. */
  UserRateLimit: number;
  /** Slack channel id (C0123…) or empty. */
  AlertChannel: string;
  /** Answers longer than this go out as a Markdown attachment; 0 = never. */
  LongAnswerChars: number;
  /** Extra models a channel may pick on its Configure page, beyond Default and Advanced. */
  ChannelModels?: string[] | null;
  /** Show what a turn cost in the reply footer. */
  ShowCost: boolean;
  /** Auto mode allow rules: plain sentences that pre-approve writes. */
  AllowRules: string[] | null;
  /** Testing switch: let a requester approve their own access request. */
  AllowSelfApprove: boolean;
  ConfigVersion: number;
  /** Which credentials open this organisation's console. */
  AuthPolicy: AuthPolicy;
  /** Every password sign-in must carry a second factor. */
  RequireTwoFactor: boolean;
  /** Slack members whose email domain is listed may use the bot; empty means every member. */
  AllowedEmailDomains: string[] | null;
  /** Whether guests and Slack Connect members may use the bot at all. */
  AllowExternalUsers: boolean;
  // How the bot reaches the public web (Settings → Web).
  /** The search engine: "builtin", "tavily", "exa", "brave", "serper", … */
  WebProvider: string;
  /** The page reader, when it differs. Empty = follow the search engine, or built-in if it cannot read pages. */
  WebFetchProvider: string;
  /** Cloudflare account id; unused by the other providers. */
  WebAccountID: string;
  /** Providers a key is stored for. The keys themselves never come back. */
  WebKeyProviders: string[] | null;
  // Fix worker (Settings → Fix worker).
  WorkerEngine: string;
  WorkerModel: string;
  WorkerJobBudgetUSD: number;
  WorkerTimeoutMinutes: number;
  WorkerMaxJobs: number;
  WorkerBranchPrefix: string;
  WorkerBranchSuffix: string;
  WorkerEventRetentionDays: number;
  /** Days of turns, tool calls, proxied requests and artifacts to keep; 0 keeps everything. */
  DataRetentionDays: number;
  /** Days of audit log to keep, as its own policy; 0 keeps everything. */
  AuditRetentionDays: number;
  WorkerAllowRules: boolean;
};

/**
 * One web provider on offer (GET /api/settings → env.web_providers). Search and fetch are
 * separate because they are: Cloudflare renders a page but has no web search, so choosing it
 * leaves searching on the built-in path.
 */
export type WebProviderInfo = {
  id: string;
  name: string;
  search: boolean;
  fetch: boolean;
  needs_account: boolean;
  /** Reads after the provider name: "Firecrawl API key", "Brave Search subscription token". */
  key_label?: string;
  key_hint?: string;
  placeholder?: string;
  docs_url?: string;
  blurb?: string;
};

/** Deploy-time facts about the fix worker (GET /api/settings → env.worker). */
export type WorkerEnv = {
  // What WORKER_MODE says, and what it resolved to. `workers` works the platform out from the
  // environment, so the two differ whenever the operator did not name one.
  mode: "off" | "workers" | "local" | "cloudrun" | "ecs" | "aca" | "k8s" | "docker" | string;
  platform?: "" | "local" | "cloudrun" | "ecs" | "aca" | "k8s" | "docker" | string;
  require_oidc: boolean;
  engines: string[] | null;
  provisioning_key: boolean;
  engine_key: boolean;
};

/** Somebody in this organisation. The id is their account; the role is their membership. */
export type ConsoleUser = {
  /** Their account's public id — what the role and removal routes take. */
  id: string;
  name: string;
  email: string;
  email_verified: boolean;
  role: string;
  created_at: string;
  last_seen: string;
  permissions: string[] | null;
  is_you: boolean;
};

/** A console role: the built-ins are code-defined, customs are rows. */
export type ConsoleRole = {
  key: string;
  label: string;
  permissions: string[] | null;
  builtin: boolean;
};

export type ConsoleRolesResponse = {
  roles: ConsoleRole[] | null;
  all_permissions: string[] | null;
};

/** An invitation that has not been accepted, revoked or expired. */
/**
 * One invitation waiting to be accepted. All three kinds are the same row on the server and
 * differ only in who they name: `email` and `slack` name a person, `link` names nobody and is
 * meant to be passed around.
 */
export type ConsoleInvite = {
  id: number;
  kind: "email" | "slack" | "link";
  /** Who it is for, already resolved: an address, a Slack display name, or the link's label. */
  who: string;
  email: string;
  slack_user_id: string;
  role: string;
  label: string;
  /** A share link narrowed to one email domain, or "" for anyone. */
  domain: string;
  /** How many more people the link may let in: -1 for no limit. */
  max_uses: number;
  uses: number;
  expires_at: string;
  created_at: string;
  invited_by: string;
};

/**
 * What creating an invitation returns. The link comes back whether or not it was delivered,
 * because a failed send must not lose an invitation that is already written — the console
 * offers it to copy instead.
 */
export type InviteResult = {
  ok: boolean;
  link: string;
  /** How the server tried to deliver it: a Slack DM, an email, or not at all for a share link. */
  via: "dm" | "email" | "link";
  delivered: boolean;
  delivery_error?: string;
  /** Who it went to, as the console should name them. */
  to: string;
};

/**
 * A developer API key, as the console lists it. The raw key is not here and never comes back: it
 * exists once, in the response that created it (see ApiKeyCreated).
 */
export type ApiKey = {
  id: number;
  name: string;
  /** The first few characters of the key — enough to recognise, useless to authenticate with. */
  prefix: string;
  /** The email of the account the key acts as. */
  owner: string;
  created_by: string;
  created_at: string;
  /** Empty until the key has been used. */
  last_used_at: string;
  /** Empty means it never expires. */
  expires_at: string;
  /** Non-empty once it has been turned off. */
  revoked_at: string;
};

export type ApiKeysResponse = {
  keys: ApiKey[] | null;
  rate_limit_per_minute: number;
};

/** The one and only response that carries a raw key. */
export type ApiKeyCreated = { key: ApiKey; raw: string };

/** One tool the MCP server offers, and whether this person's role may use it. */
export type McpTool = {
  name: string;
  title: string;
  description: string;
  /** The permission the tool needs beyond a working connection; empty for any member. */
  permission: string;
  read_only: boolean;
  destructive: boolean;
  allowed: boolean;
};

/** An MCP client somebody connected with OAuth. It acts as them, like an API key. */
export type McpGrant = {
  id: number;
  owner: string;
  client_name: string;
  /** The host the app sends people back to — what it can actually be checked by. */
  returns_to: string;
  created_at: string;
  last_used_at: string;
  /** When it lapses if the app stops refreshing it. */
  expires_at: string;
  revoked_at: string;
};

export type McpInfo = {
  url: string;
  tools: McpTool[];
  grants: McpGrant[];
  /** Why this person cannot approve an app with OAuth; empty when they can. */
  refusal: string;
};

/**
 * What the consent screen shows about one authorization request — or, when the request does not
 * check out, the problem, with the address to send the person back to when the app can be
 * trusted to hear about it.
 */
export type OAuthConsent = {
  client_name?: string;
  returns_to?: string;
  redirect_uri?: string;
  org_name?: string;
  email?: string;
  refusal?: string;
  problem?: { error: string; code: string; redirect: string };
};

/** The ways one account can sign in, for the account settings panel. */
export type Identity = { provider: string; subject: string; created_at: string };

export type IdentitiesResponse = {
  identities: Identity[] | null;
  has_password: boolean;
  email_verified: boolean;
  /** False when no RESEND_API_KEY is set: the console offers links to copy instead of promising email. */
  email_configured: boolean;
};

/** One approval tier: what it can grant, and who holds it. */
export type ApprovalRole = {
  id: number;
  name: string;
  /** Higher grants more; a tier covers everything below it. */
  rank: number;
  /** What it grants from: whole bundles, and connections picked on their own (bundle/name). */
  bundle_ids: number[] | null;
  connection_ids: number[] | null;
  members: { ref: string; name: string }[] | null;
  /** How many of those connections are actually enabled for grants, counted once each. */
  grantable: number;
};

/** One access request, from GET /api/access-requests. */
export type AccessRequest = {
  id: number;
  channel: string;
  channel_name: string;
  thread_ts: string;
  requester: string;
  requester_name: string;
  what: string;
  why: string;
  status: string;
  steps: number;
  approvers: string[] | null;
  decided_by: string;
  decided_by_name?: string;
  decided_at: string;
  reason: string;
  created_at: string;
  expires_at: string;
  self_approved?: boolean;
  role_id?: number;
  /** Detail view only. */
  ask?: string;
  plan?: string;
  result?: string;
};

/** One model from the provider's OpenAI-compatible list (GET /api/models). */
export type ModelInfo = {
  id: string;
  /** Display name when the provider has one (OpenRouter); absent when it equals the id. */
  name?: string;
  kind: "chat" | "embedding" | string;
  context_length?: number;
  input_modalities?: string[] | null;
  owned_by?: string;
  /** USD per million tokens, when the provider reports pricing. */
  prompt_per_m?: number;
  completion_per_m?: number;
};

export type ModelsResponse = {
  models: ModelInfo[] | null;
  /** The host the list came from: the deployment's endpoint, or the organisation's own. */
  source: string;
  /** True when that is the organisation's own endpoint (Settings → Models → Your model key). */
  own?: boolean;
  /** The list names every model the endpoint serves, so one missing from it is not served there. */
  complete?: boolean;
};

// ---- the organisation's own model key ----

/** An organisation's own model endpoint as the console is told it. Never the key itself. */
export type ModelKeyRef = {
  /** Which form it was saved from: "openai" | "openrouter" | "compatible". */
  preset: string;
  base_url: string;
  /** "…abcd": enough to tell two keys apart. */
  key_hint: string;
  key_fp: string;
  default_model: string;
  /** "" means document search is off while this key is in use. */
  embed_model: string;
  fix_jobs: boolean;
  updated_by: string;
  created_at: string;
  updated_at: string;
  last_ok_at: string;
  last_error: string;
  last_error_at: string;
};

export type ModelKeyPreset = { id: string; name: string; base_url: string; hint: string };

export type ModelKeyView = {
  /** Whether this organisation may bring a key at all; policy and plan say why not. */
  allowed: boolean;
  policy: "all" | "enterprise" | "off";
  plan: string;
  key: ModelKeyRef | null;
  /** The bot is answering on it now. */
  active: boolean;
  failing: boolean;
  /** Why a stored key is not in use, in the words a turn is refused with. */
  refusal?: string;
  /** What saving a new key or address will ask for. */
  proof: ProofKind;
  /** The organisation's own monthly budget: the one limit left on its own key. 0 = none. */
  monthly_budget_usd: number;
  /** Whether calls on it carry a figure that budget can be measured in. */
  priced: boolean;
  presets: ModelKeyPreset[];
};

export type ModelKeyTest = { ok: boolean; detail: string; models?: ModelInfo[] | null };

export type SettingsResponse = {
  effective: EffectiveSettings;
  stored: Record<string, string> | null;
  env: {
    llm_base_url: string;
    slack_login: { configured: boolean; redirect_url: string };
    worker?: WorkerEnv;
    web_providers?: WebProviderInfo[] | null;
    secrets: Record<string, boolean>;
    /** Where a free account writes to have its budget raised. */
    support_email?: string;
    /** The console draws a banner from this during a cutover freeze. */
    maintenance?: boolean;
    /**
     * Whether this deployment sells plans. Top level rather than inside `secrets`, which the
     * server only fills in for settings.manage holders: the Billing tab is gated on
     * billing.manage, and a flag behind the wrong permission hides the tab from exactly the
     * people allowed to use it.
     */
    billing_enabled?: boolean;
    /**
     * ALLOWED_EMAIL_DOMAINS: the email domains an organisation that keeps no list of its own is
     * held to. Empty where the deployment sets none, and only there does an empty list mean
     * every member.
     */
    default_email_domains?: string[] | null;
  };
};

// ---- transport ----

export class ApiError extends Error {
  status: number;
  constructor(status: number, message: string) {
    super(message);
    this.name = "ApiError";
    this.status = status;
  }
}

/** Fired when any call comes back 401, so the shell can drop to the login card. */
export const UNAUTHORIZED_EVENT = "attest-tag:unauthorized";

/**
 * The CSRF token the server set alongside the session cookie. Double submit: the cookie is
 * readable so we can echo it in a header, which an attacker on another origin cannot do — they
 * can make the browser send the cookie, but not read it.
 */
function csrfToken(): string {
  if (typeof document === "undefined") return "";
  const hit = document.cookie.split("; ").find((c) => c.startsWith("attest_csrf="));
  return hit ? decodeURIComponent(hit.slice("attest_csrf=".length)) : "";
}

const SAFE_METHODS = new Set(["GET", "HEAD", "OPTIONS"]);

async function request<T>(path: string, init: RequestInit = {}, retried = false): Promise<T> {
  const method = (init.method ?? "GET").toUpperCase();
  const headers = new Headers(init.headers);
  const token = SAFE_METHODS.has(method) ? "" : csrfToken();
  if (!SAFE_METHODS.has(method)) headers.set("X-CSRF-Token", token);
  const res = await fetch(path, { credentials: "include", ...init, headers });
  if (res.status === 401 && typeof window !== "undefined") {
    window.dispatchEvent(new Event(UNAUTHORIZED_EVENT));
  }
  const text = await res.text();
  let body: unknown = null;
  if (text) {
    try {
      body = JSON.parse(text);
    } catch {
      body = text;
    }
  }
  if (!res.ok) {
    const message =
      body && typeof body === "object" && "error" in body
        ? String((body as { error: unknown }).error)
        : typeof body === "string" && body
          ? body
          : `${res.status} ${res.statusText}`;
    // A session from before the CSRF cookie existed has none to send. The server mints one on
    // /api/me (and on the refusal itself), so pick it up and retry once instead of asking the
    // person to reload.
    if (res.status === 403 && !retried && !token && !SAFE_METHODS.has(method) && /csrf/i.test(message)) {
      await fetch("/api/me", { credentials: "include", cache: "no-store" }).catch(() => undefined);
      if (csrfToken()) return request<T>(path, init, true);
    }
    throw new ApiError(res.status, message);
  }
  return body as T;
}

function jsonInit(method: string, body?: unknown): RequestInit {
  return {
    method,
    headers: body === undefined ? undefined : { "Content-Type": "application/json" },
    body: body === undefined ? undefined : JSON.stringify(body),
  };
}

export const api = {
  get: <T>(path: string) => request<T>(path),
  post: <T>(path: string, body?: unknown) => request<T>(path, jsonInit("POST", body)),
  put: <T>(path: string, body?: unknown) => request<T>(path, jsonInit("PUT", body)),
  del: <T>(path: string) => request<T>(path, { method: "DELETE" }),
  /**
   * One call by method and path, for a caller that was handed both rather than writing them.
   * The assistant's proposal cards are the only user: a staged change is a list of steps the
   * server built, and replaying them means making whichever request each one names. Everything
   * else should say `api.put(...)` and read as what it does.
   */
  send: <T>(method: string, path: string, body?: unknown) =>
    body === undefined
      ? request<T>(path, { method })
      : request<T>(path, jsonInit(method, body)),
  upload: <T>(path: string, form: FormData) =>
    request<T>(path, { method: "POST", body: form }),
  /** Raw body as text (document contents), with the same 401 and error handling. */
  text: async (path: string): Promise<string> => {
    const res = await fetch(path, { credentials: "include", cache: "no-store" });
    if (res.status === 401 && typeof window !== "undefined") {
      window.dispatchEvent(new Event(UNAUTHORIZED_EVENT));
    }
    const body = await res.text();
    if (!res.ok) {
      let message = `${res.status} ${res.statusText}`;
      try {
        const parsed = JSON.parse(body);
        if (parsed && typeof parsed === "object" && "error" in parsed) message = String(parsed.error);
      } catch {
        if (body) message = body;
      }
      throw new ApiError(res.status, message);
    }
    return body;
  },
};

/** Text document types the console can open in its editor (mirrors editableExts in Go). */
const EDITABLE_EXTS = new Set([".md", ".markdown", ".txt", ".rst", ".html", ".htm", ".csv", ".json"]);

export function isEditableDocument(path: string): boolean {
  const i = path.lastIndexOf(".");
  return i >= 0 && EDITABLE_EXTS.has(path.slice(i).toLowerCase());
}

/** Document paths carry slashes and spaces; encode each segment, keep the slashes. */
export function encodePath(p: string): string {
  return p.split("/").map(encodeURIComponent).join("/");
}

// ---- useApi ----

type ApiState<T> = {
  data?: T;
  error?: string;
  /** Which request the data/error belongs to; a stale reply is dropped. */
  key: string;
};

/**
 * Fetches `path` on mount and whenever it changes. `reload()` refetches while
 * keeping the current data on screen, so a page can mutate then refresh
 * without flashing back to its skeleton. Pass `null` to hold off.
 */
export function useApi<T>(path: string | null) {
  const [version, setVersion] = useState(0);
  const [state, setState] = useState<ApiState<T>>({ key: "" });
  const key = path ? `${path}#${version}` : "";

  useEffect(() => {
    if (!path) return;
    let alive = true;
    request<T>(path).then(
      (data) => {
        if (alive) setState({ key, data });
      },
      (err: unknown) => {
        if (alive) setState((s) => ({ key, data: s.data, error: errorMessage(err) }));
      },
    );
    return () => {
      alive = false;
    };
  }, [path, key]);

  const reload = useCallback(() => setVersion((v) => v + 1), []);
  const mutate = useCallback(
    (update: T | ((current: T | undefined) => T | undefined)) =>
      setState((s) => ({
        ...s,
        data: typeof update === "function" ? (update as (c: T | undefined) => T | undefined)(s.data) : update,
      })),
    [],
  );

  const settled = state.key === key;
  return {
    data: state.data,
    error: settled ? state.error : undefined,
    /** True until the first reply for the current path; reloads don't flip it. */
    loading: !!path && state.data === undefined && !(settled && state.error),
    /** True while a reload is in flight over existing data. */
    refreshing: !!path && !settled && state.data !== undefined,
    reload,
    mutate,
  };
}

export function errorMessage(err: unknown): string {
  if (err instanceof Error) return err.message;
  return String(err);
}
