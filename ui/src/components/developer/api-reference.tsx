"use client";

import { useMemo, useState, useSyncExternalStore } from "react";
import Link from "next/link";
import { ChevronDown } from "lucide-react";
import { CopyButton } from "@/components/core/copy-button";
import { PageHeader } from "@/components/core/page-header";
import { SearchField } from "@/components/core/search-field";
import { Card } from "@/components/ui/card";
import { cn } from "@/lib/utils";

type Method = "GET" | "POST" | "PUT" | "DELETE";

type Endpoint = {
  group: string;
  method: Method;
  path: string;
  summary: string;
  /** What it does, what it is for, and anything surprising about it. */
  description: string;
  /** The permission the caller's role must hold, if any beyond a working key. */
  permission?: string;
  request: string;
  response: string;
};

// Every endpoint the API serves. Documentation, not a generated spec: it is written by hand
// and has to be kept in step with internal/app/api_v1.go by whoever changes that file.
const ENDPOINTS: Endpoint[] = [
  {
    group: "Your key",
    method: "GET",
    path: "/v1/whoami",
    summary: "What this key is",
    description:
      "The organisation the key acts in, the account it acts as, and the permissions that account's role holds right now. The first call worth making, and the one that explains a 403 without a support ticket.",
    request: `curl $BASE/v1/whoami \\
  -H "Authorization: Bearer $ATTESTTAG_KEY"`,
    response: `{
  "organisation": { "id": "9f2c41ab7e0d4c58b1e6a03d5c7f8b21", "name": "Acme Ltd", "slug": "acme-ltd" },
  "account": { "id": "3d81b6f0a94e47c2ae5f0b7c2d13e9aa", "email": "dana@acme.example", "name": "Dana" },
  "role": "editor",
  "permissions": ["scopes.manage", "bundles.manage", "documents.manage"],
  "rate_limit": { "requests_per_minute": 240 }
}`,
  },
  {
    group: "Workspaces and channels",
    method: "GET",
    path: "/v1/workspaces",
    summary: "Connected Slack workspaces",
    description:
      "Every workspace this organisation has installed the bot into, revoked ones included, so a workspace that stopped answering is visible rather than simply absent.",
    request: `curl $BASE/v1/workspaces \\
  -H "Authorization: Bearer $ATTESTTAG_KEY"`,
    response: `{
  "workspaces": [
    {
      "team_id": "T0123456",
      "name": "Acme",
      "domain": "acme",
      "status": "active",
      "installed_at": "2026-08-14 09:12:03"
    }
  ]
}`,
  },
  {
    group: "Workspaces and channels",
    method: "GET",
    path: "/v1/scopes",
    summary: "Channel and workspace settings",
    description:
      "What the bot has been told about each channel: its instructions, its model, its budget, and the bundles and connections attached to it. Read-only — changing what a channel may reach is a decision with a person behind it, and the console is where the consequences are shown.",
    request: `curl $BASE/v1/scopes \\
  -H "Authorization: Bearer $ATTESTTAG_KEY"`,
    response: `{
  "scopes": [
    {
      "id": 7,
      "team_id": "T0123456",
      "kind": "channel",
      "slack_id": "C0456789",
      "name": "#platform",
      "instructions": "Answer with runbook links where there is one.",
      "default_model": "",
      "monthly_budget_usd": 25,
      "bundle_ids": [2],
      "connection_ids": []
    }
  ]
}`,
  },
  {
    group: "Documents",
    method: "GET",
    path: "/v1/documents",
    summary: "List documents",
    description:
      "Everything the bot can search when it answers, with the scope each document is limited to and how many chunks it indexed into. A document with status \"error\" carries the reason in last_error.",
    request: `curl $BASE/v1/documents \\
  -H "Authorization: Bearer $ATTESTTAG_KEY"`,
    response: `{
  "documents": [
    {
      "path": "runbooks/deploys.md",
      "name": "deploys.md",
      "size": 8412,
      "scope": "",
      "status": "indexed",
      "chunks": 9,
      "updated_at": "2026-09-01 11:04:22"
    }
  ]
}`,
  },
  {
    group: "Documents",
    method: "POST",
    path: "/v1/documents",
    summary: "Upload files",
    description:
      "Uploads one file or many, a PDF as readily as text, and re-indexes them in the background. It is the multipart form the console's Upload button sends: one files part per file, folder for where the batch lands, paths (one per file, in order) when a file should keep a path of its own, and scope for the channel it is limited to. The bot reads .csv, .htm, .html, .json, .markdown, .md, .pdf, .rst and .txt; one file of another type refuses the whole batch before anything is stored. A file already at a path is replaced, and a request may be 32 MB (413 past that).",
    permission: "documents.manage",
    request: `curl $BASE/v1/documents \\
  -H "Authorization: Bearer $ATTESTTAG_KEY" \\
  -F folder=policies \\
  -F files=@handbook.pdf \\
  -F files=@leave.md`,
    response: `{
  "ok": true,
  "saved": ["policies/handbook.pdf", "policies/leave.md"],
  "indexing": true
}`,
  },
  {
    group: "Documents",
    method: "GET",
    path: "/v1/documents/{path}",
    summary: "Read one document",
    description:
      "The document's contents as text/plain. Text types only — a PDF or a spreadsheet answers 415, because there is nothing useful to hand back as text.",
    request: `curl $BASE/v1/documents/runbooks/deploys.md \\
  -H "Authorization: Bearer $ATTESTTAG_KEY"`,
    response: `# Deploys

Run make deploy from main. …`,
  },
  {
    group: "Documents",
    method: "PUT",
    path: "/v1/documents/{path}",
    summary: "Write a document",
    description:
      "Creates or replaces a text document and re-indexes it in the background, so the call returns as soon as the file is stored. This is the endpoint that earns the API: a repository hook or a nightly export can keep what the bot knows in step with a source of truth somewhere else. Send scope on its own to move an existing document between channels without touching its contents. A PDF goes through POST /v1/documents instead.",
    permission: "documents.manage",
    request: `curl $BASE/v1/documents/runbooks/deploys.md \\
  -X PUT \\
  -H "Authorization: Bearer $ATTESTTAG_KEY" \\
  -H "Content-Type: application/json" \\
  -d '{"content": "# Deploys\\n\\nRun make deploy from main.\\n"}'`,
    response: `{ "ok": true, "path": "runbooks/deploys.md", "indexing": true }`,
  },
  {
    group: "Documents",
    method: "DELETE",
    path: "/v1/documents/{path}",
    summary: "Remove a document",
    description: "Deletes the file and drops what it contributed to the index.",
    permission: "documents.manage",
    request: `curl $BASE/v1/documents/runbooks/old.md \\
  -X DELETE \\
  -H "Authorization: Bearer $ATTESTTAG_KEY"`,
    response: `{ "ok": true }`,
  },
  {
    group: "Memory",
    method: "GET",
    path: "/v1/memories",
    summary: "What the bot remembers",
    description:
      "Facts kept per workspace or per channel and folded into every reply there. Scopes read \"team:T0123456\" for a whole workspace and \"channel:T0123456/C0456789\" for one channel. Private notes kept for one person are deliberately not here and have no /v1 route at all: a key is a script rather than a person, and a leaked one must not be able to read anybody's own notes.",
    request: `curl $BASE/v1/memories \\
  -H "Authorization: Bearer $ATTESTTAG_KEY"`,
    response: `{
  "memories": [
    {
      "id": 12,
      "team_id": "T0123456",
      "scope": "channel:T0123456/C0456789",
      "text": "Staging deploys need the release manager's sign-off.",
      "created_by": "dana@acme.example",
      "created_at": "2026-08-28 16:41:10"
    }
  ]
}`,
  },
  {
    group: "Memory",
    method: "POST",
    path: "/v1/memories",
    summary: "Remember something",
    description:
      "Adds a fact to a workspace or a channel. The workspace named in the scope has to be one of yours: a key cannot write into somebody else's workspace by naming it, and a scope that is not yours answers 404 rather than saying so.",
    permission: "memory.manage",
    request: `curl $BASE/v1/memories \\
  -X POST \\
  -H "Authorization: Bearer $ATTESTTAG_KEY" \\
  -H "Content-Type: application/json" \\
  -d '{
    "scope": "channel:T0123456/C0456789",
    "text": "Staging deploys need the release manager'"'"'s sign-off."
  }'`,
    response: `{ "ok": true }`,
  },
  {
    group: "Memory",
    method: "PUT",
    path: "/v1/memories/{id}",
    summary: "Correct something",
    description:
      "Rewrites one memory's text in place. The memory keeps its id, scope and author, and the bot reads the corrected fact on its next turn. A memory that is not yours answers 404.",
    permission: "memory.manage",
    request: `curl $BASE/v1/memories/12 \\
  -X PUT \\
  -H "Authorization: Bearer $ATTESTTAG_KEY" \\
  -H "Content-Type: application/json" \\
  -d '{ "text": "Staging deploys need the release manager'"'"'s sign-off, or Dana'"'"'s." }'`,
    response: `{ "ok": true }`,
  },
  {
    group: "Memory",
    method: "DELETE",
    path: "/v1/memories/{id}",
    summary: "Forget something",
    description: "Removes one memory. The bot stops using it on the next turn.",
    permission: "memory.manage",
    request: `curl $BASE/v1/memories/12 \\
  -X DELETE \\
  -H "Authorization: Bearer $ATTESTTAG_KEY"`,
    response: `{ "ok": true }`,
  },
  {
    group: "Artifacts",
    method: "GET",
    path: "/v1/artifacts",
    summary: "Files the bot made",
    description:
      "What the bot produced and posted in Slack, newest first. Contents are left out here so the list stays small — ask for one artifact to get its body. Takes ?limit (50 by default, 200 at most).",
    permission: "artifacts.view",
    request: `curl "$BASE/v1/artifacts?limit=20" \\
  -H "Authorization: Bearer $ATTESTTAG_KEY"`,
    response: `{
  "artifacts": [
    {
      "id": 88,
      "title": "Q3 incident summary",
      "kind": "md",
      "bytes": 4210,
      "team_id": "T0123456",
      "team_name": "Acme",
      "channel": "C0456789",
      "channel_name": "#platform",
      "created_by": "U0999",
      "created_by_name": "Dana",
      "permalink": "https://acme.slack.com/archives/C0456789/p1788320047708289",
      "created_at": "2026-09-01 10:02:55"
    }
  ]
}`,
  },
  {
    group: "Artifacts",
    method: "GET",
    path: "/v1/artifacts/{id}",
    summary: "One artifact, with its contents",
    description:
      "The same row as the list, plus content. An id from another organisation answers 404, not 403.",
    permission: "artifacts.view",
    request: `curl $BASE/v1/artifacts/88 \\
  -H "Authorization: Bearer $ATTESTTAG_KEY"`,
    response: `{
  "id": 88,
  "title": "Q3 incident summary",
  "kind": "md",
  "content": "# Q3 incidents\\n\\n…"
}`,
  },
  {
    group: "Routines",
    method: "GET",
    path: "/v1/routines",
    summary: "Scheduled prompts",
    description:
      "Prompts that run on a schedule and post into a channel, with when each one next runs and how the last run went. Steps are the calls a routine makes before the model is asked anything, and finish is what it does with them: \"answer\" (one model call, no tools), \"agent\" (it carries on with its tools), or \"raw\" (no model call at all — the output is the post).",
    request: `curl $BASE/v1/routines \\
  -H "Authorization: Bearer $ATTESTTAG_KEY"`,
    response: `{
  "routines": [
    {
      "id": 3,
      "team_id": "T0123456",
      "channel": "C0456789",
      "cron": "0 9 * * 1",
      "timezone": "Europe/London",
      "prompt": "Summarise last week's incidents.",
      "enabled": true,
      "next_run": "2026-09-07 09:00:00",
      "last_run": "2026-08-31 09:00:02",
      "last_error": "",
      "notify": "when_needed",
      "notify_when": "any VM is over 80% CPU",
      "last_status": "quiet",
      "steps": [{ "tool": "http_request", "args": { "method": "GET", "url": "https://api.example.com/incidents?since={{date-7}}" } }],
      "finish": "answer",
      "channel_name": "#ops",
      "auto_confirm": false
    }
  ]
}`,
  },
  {
    group: "Routines",
    method: "GET",
    path: "/v1/routines/{id}/runs",
    summary: "Run history",
    description:
      "Every run of one routine, newest first, with what it cost and the whole answer. A routine set to notify \"when_needed\" posts nothing to Slack on a run that does not clear its bar, so for anything built on this API these rows are the only record that the run happened. Status is posted, quiet, failed or skipped; reason is the model's own note on a quiet run. Page with ?limit (default and maximum 200); the last 100 runs per routine are kept.",
    request: `curl $BASE/v1/routines/3/runs \\
  -H "Authorization: Bearer $ATTESTTAG_KEY"`,
    response: `{
  "runs": [
    {
      "id": 812,
      "routine_id": 3,
      "status": "quiet",
      "reason": "all 6 VMs under 80% CPU",
      "output": "NOTHING\\nall 6 VMs under 80% CPU",
      "error": "",
      "thread_ts": "routine:3:1788905043172713000",
      "tokens_in": 3120,
      "tokens_out": 48,
      "cost_usd": 0.0031,
      "started_at": "2026-09-08 15:15:00",
      "finished_at": "2026-09-08 15:15:04",
      "ms": 4210
    }
  ]
}`,
  },
  {
    group: "Routines",
    method: "POST",
    path: "/v1/routines/{id}/run",
    summary: "Run one now",
    description:
      "Starts a routine out of band. Answers 202 as soon as it is accepted — the turn happens in Slack, in its own time, and its result lands in the channel the routine posts to, not in this response. The run is recorded either way and the schedule is left alone: asking for a run now does not move the next one.",
    permission: "routines.manage",
    request: `curl $BASE/v1/routines/3/run \\
  -X POST \\
  -H "Authorization: Bearer $ATTESTTAG_KEY"`,
    response: `{ "ok": true, "routine_id": 3 }`,
  },
  {
    group: "Routines",
    method: "POST",
    path: "/v1/routines",
    summary: "Create a routine",
    description:
      "A prompt the bot runs on a schedule, posting in a channel. channel is the one you name — its id, or its name as people type it (\"#ops\") — and it must be a channel the bot is in; GET /v1/scopes lists them, and team_id settles a name that is in more than one workspace. cron is five fields, no more often than every 15 minutes; timezone defaults to the organisation's; notify is always or when_needed, with notify_when saying what is worth posting; model is empty for the default. Two things differ from the console: it runs as the bot, never as the person calling, so it reaches the channel's connections and nobody's own; and its writes ask first (auto_confirm false). Letting them run without asking is switched on in the console, and sending auto_confirm: true here is refused.",
    permission: "routines.manage",
    request: `curl $BASE/v1/routines \\
  -H "Authorization: Bearer $ATTESTTAG_KEY" \\
  -H "Content-Type: application/json" \\
  -d '{"channel": "#ops", "cron": "0 9 * * 1", "prompt": "Summarise the past week of incidents.", "notify": "when_needed", "notify_when": "any incident is still open"}'`,
    response: `{
  "id": 7,
  "team_id": "T0123456",
  "channel": "C0456789",
  "channel_name": "#ops",
  "cron": "0 9 * * 1",
  "timezone": "Europe/London",
  "prompt": "Summarise the past week of incidents.",
  "enabled": true,
  "created_by": "",
  "next_run": "2026-09-28 08:00:00",
  "notify": "when_needed",
  "notify_when": "any incident is still open",
  "auto_confirm": false
}`,
  },
  {
    group: "Routines",
    method: "PUT",
    path: "/v1/routines/{id}",
    summary: "Change a routine",
    description:
      "Only the fields you send change: enabled (false pauses it, true resumes it), cron, timezone, prompt, channel (and team_id), notify, notify_when, model, steps, finish. The console's rule decides whose it is afterwards: pausing it or widening its schedule leaves it running as whoever wrote it, but rewriting what it does takes it off them — it runs as the bot from then on, they are told in Slack, and the answer carries was_running_as. auto_confirm may be turned off here, never on.",
    permission: "routines.manage",
    request: `curl $BASE/v1/routines/7 \\
  -X PUT \\
  -H "Authorization: Bearer $ATTESTTAG_KEY" \\
  -H "Content-Type: application/json" \\
  -d '{"enabled": false}'`,
    response: `{ "id": 7, "enabled": false, "channel_name": "#ops", "auto_confirm": false, … }`,
  },
  {
    group: "Routines",
    method: "DELETE",
    path: "/v1/routines/{id}",
    summary: "Delete a routine",
    description: "Deletes the routine and its run history.",
    permission: "routines.manage",
    request: `curl $BASE/v1/routines/7 \\
  -X DELETE \\
  -H "Authorization: Bearer $ATTESTTAG_KEY"`,
    response: `{ "ok": true }`,
  },
  {
    group: "Jobs",
    method: "GET",
    path: "/v1/jobs",
    summary: "Fix jobs",
    description:
      "The fixes the bot handed to a worker: what it is doing, where the pull request went, and what it cost. Filter with ?status (active, done, failed…) and page with ?limit.",
    permission: "jobs.view",
    request: `curl "$BASE/v1/jobs?status=active" \\
  -H "Authorization: Bearer $ATTESTTAG_KEY"`,
    response: `{
  "jobs": [
    {
      "id": 41,
      "status": "running",
      "phase": "editing",
      "repo": "acme/platform",
      "branch": "attesttag/fix-login-timeout",
      "title": "Fix the login timeout",
      "requester": "U0999",
      "engine": "qwen_code",
      "budget_usd": 2,
      "pr_url": "",
      "cost_usd": 0.41,
      "created_at": "2026-09-02 14:22:19"
    }
  ]
}`,
  },
  {
    group: "Jobs",
    method: "GET",
    path: "/v1/jobs/{id}",
    summary: "One job",
    description: "The same shape as the list, for a single job — what to poll while one runs.",
    permission: "jobs.view",
    request: `curl $BASE/v1/jobs/41 \\
  -H "Authorization: Bearer $ATTESTTAG_KEY"`,
    response: `{ "id": 41, "status": "done", "pr_url": "https://github.com/acme/platform/pull/812" }`,
  },
  {
    group: "Access requests",
    method: "GET",
    path: "/v1/access-requests",
    summary: "Who asked for what",
    description:
      "The access requests raised from Slack, with who asked, what they wanted and how it was closed. Filter with ?status.",
    permission: "access_requests.view",
    request: `curl "$BASE/v1/access-requests?status=open" \\
  -H "Authorization: Bearer $ATTESTTAG_KEY"`,
    response: `{
  "access_requests": [
    {
      "id": 9,
      "team_id": "T0123456",
      "channel_name": "#platform",
      "requester_name": "Dana",
      "what": "Read the billing repo",
      "why": "Tracing a failed invoice job",
      "status": "open",
      "steps": 2
    }
  ]
}`,
  },
  {
    group: "Activity and spend",
    method: "GET",
    path: "/v1/activity",
    summary: "Recent turns",
    description:
      "Every turn the bot took, with the model it used, the tokens it spent and what that cost. Narrow it with ?channel, ?limit (100 by default, 500 at most) and ?range=today|7d|month \u2014 the same windows /v1/usage counts in, so turns_today and ?range=today are the same set of turns. It pages the way /v1/audit does: each turn has an id, ?before=<id> carries on back through history, ?after=<id> walks forward from the last turn you saw, oldest first, and last_id is the mark for the next ?after (on an empty page, the mark you sent).",
    permission: "activity.view",
    request: `curl "$BASE/v1/activity?after=90411&limit=500" \\
  -H "Authorization: Bearer $ATTESTTAG_KEY"`,
    response: `{
  "turns": [
    {
      "id": 90412,
      "at": "2026-09-02 15:41:09",
      "team_name": "Acme",
      "channel_name": "#platform",
      "thread_ts": "1788320047.708289",
      "model": "z-ai/glm-5.3-flash",
      "tokens_in": 8120,
      "tokens_out": 402,
      "cost_usd": 0.0031
    }
  ],
  "last_id": 90412
}`,
  },
  {
    group: "Activity and spend",
    method: "GET",
    path: "/v1/audit",
    summary: "The audit log",
    description:
      "Who signed in and from where, what they changed, what they approved in Slack, what they exported — every write through the console or this API, named where the handler could name it and as a plain console.request otherwise. Narrow it with ?action (one action, or a family with a trailing dot: auth.), ?actor (an account id, email or Slack id), ?outcome=denied, ?q (a word in the actor, target, details or address), ?range=today|7d|month or ?since=…, and ?limit (100 by default, 500 at most). For a collector: ?after=<id> walks forward from the last row you saw, oldest first, and last_id is the mark to keep for the next call (on an empty page, the mark you sent); ?before=<id> pages back through history instead.",
    permission: "audit.view",
    request: `curl "$BASE/v1/audit?after=4180&limit=500" \\
  -H "Authorization: Bearer $ATTESTTAG_KEY"`,
    response: `{
  "events": [
    {
      "id": 4181,
      "at": "2026-09-15 09:12:44",
      "action": "connection.updated",
      "outcome": "ok",
      "via": "console",
      "actor_id": "3d81b6f0a94e47c2ae5f0b7c2d13e9aa",
      "actor_email": "dana@acme.example",
      "actor_name": "Dana",
      "actor_slack": "",
      "team_id": "",
      "target_kind": "connection",
      "target_id": "12",
      "target_name": "GitHub",
      "ip": "203.0.113.9",
      "user_agent": "Mozilla/5.0 …",
      "details": { "secret_rotated": true, "preset": "github", "allowed_hosts": ["api.github.com"] }
    }
  ],
  "last_id": 4181
}`,
  },
  {
    group: "Activity and spend",
    method: "GET",
    path: "/v1/usage",
    summary: "Spend and totals",
    description:
      "This month's spend against the budget actually enforced (a free account's is the free plan's, whatever Settings says), the plan, and what the bot has to work with: documents, chunks, workspaces, channels, routines and memories. Where prepaid credit is in use it also carries the balance and paused_by, which is empty until one of the two limits binds and then names which — the field to poll if you want to know before the bot goes quiet rather than after. top_channels is the month split by conversation, dearest first, in the fields /v1/activity uses for a turn; channel_name is what the console calls it: the channel, “DM with …”, or “Message screening” for the checks Read every message runs, whose channel is empty. active_users counts the people behind the turns over the last window_days.",
    request: `curl $BASE/v1/usage \\
  -H "Authorization: Bearer $ATTESTTAG_KEY"`,
    response: `{
  "month_spend_usd": 18.42,
  "monthly_budget_usd": 100,
  "plan": "pro",
  "credit_enabled": true,
  "credit_balance_usd": 412.55,
  "paused_by": "",
  "turns_today": 37,
  "turns_7d": 214,
  "documents": 62,
  "chunks": 918,
  "workspaces": 2,
  "channels": 11,
  "routines": 3,
  "memories": 24,
  "top_channels": [
    {
      "team_id": "T0123ABCD",
      "channel": "C0456789",
      "channel_name": "#platform",
      "turns": 88,
      "tokens_in": 412803,
      "tokens_out": 20511,
      "cost_usd": 6.12
    }
  ],
  "active_users": { "users": 14, "limit": 25, "over": false, "window_days": 30 }
}`,
  },
  {
    group: "Activity and spend",
    method: "GET",
    path: "/v1/billing",
    summary: "Plan, fee and credit",
    description:
      "The commercial picture rather than the operational one: what the plan costs, when it renews, and how much prepaid credit is left. Present only on a deployment that sells plans; elsewhere it answers 404 along with the rest of billing. It carries no Stripe identifiers at any permission — those name the account to Stripe's own support, and a key has no business holding them. There is deliberately no checkout or portal here either: both mint URLs bound to a browser session.",
    request: `curl $BASE/v1/billing \\
  -H "Authorization: Bearer $ATTESTTAG_KEY"`,
    response: `{
  "plan": "pro",
  "currency": "usd",
  "month_spend_usd": 231.08,
  "monthly_budget_usd": 0,
  "paused_by": "",
  "credit": { "balance_usd": 412.55, "enabled": true, "overdraft_usd": 50 },
  "subscription": {
    "status": "active",
    "size": "25_100",
    "size_label": "25–100 employees",
    "amount_usd": 499,
    "period_end": "2026-10-17 00:00:00",
    "cancel_at_period_end": false
  }
}`,
  },
];

// The origin never changes while the page is mounted, so the subscription is a no-op: the
// snapshot is read once at hydration and again only if React re-subscribes.
function subscribeToNothing() {
  return () => {};
}

function readOrigin() {
  return window.location.origin;
}

/** The two lines every example expects, ready to paste. */
function shellSetup(origin: string): string {
  return `export BASE=${origin || "https://your-console"}\nexport ATTESTTAG_KEY=atk1.…`;
}

const METHOD_STYLES: Record<Method, string> = {
  GET: "bg-info-soft text-info",
  POST: "bg-success-soft text-success-text",
  PUT: "bg-warning-soft text-warning",
  DELETE: "bg-danger-soft text-danger",
};

function MethodChip({ method }: { method: Method }) {
  return (
    <span
      className={cn(
        "inline-flex h-[22px] w-14 shrink-0 items-center justify-center rounded-sm text-[11px] font-semibold tracking-wide",
        METHOD_STYLES[method],
      )}
    >
      {method}
    </span>
  );
}

function Snippet({ label, code }: { label: string; code: string }) {
  return (
    <div className="space-y-1.5">
      <div className="flex items-center justify-between">
        <span className="text-xs font-medium text-muted-foreground">{label}</span>
        <CopyButton text={code} label={`Copy ${label.toLowerCase()}`} />
      </div>
      <pre className="overflow-x-auto rounded-lg border bg-muted/50 p-3 font-mono text-xs leading-relaxed">
        {code}
      </pre>
    </div>
  );
}

function EndpointRow({ endpoint }: { endpoint: Endpoint }) {
  const [open, setOpen] = useState(false);
  return (
    <div className="border-b last:border-b-0">
      <button
        type="button"
        onClick={() => setOpen((v) => !v)}
        aria-expanded={open}
        className="flex w-full items-center gap-3 px-4 py-3 text-left hover:bg-muted/40"
      >
        <MethodChip method={endpoint.method} />
        <code className="font-mono text-sm">{endpoint.path}</code>
        <span className="hidden min-w-0 flex-1 truncate text-sm text-muted-foreground sm:block">
          {endpoint.summary}
        </span>
        <ChevronDown
          className={cn(
            "ml-auto size-4 shrink-0 text-muted-foreground transition-transform sm:ml-0",
            open && "rotate-180",
          )}
        />
      </button>
      {open && (
        <div className="space-y-4 border-t bg-muted/20 px-4 py-4">
          <p className="text-sm leading-relaxed text-muted-foreground">{endpoint.description}</p>
          <p className="text-xs text-muted-foreground">
            {endpoint.permission ? (
              <>
                Needs the{" "}
                <code className="rounded bg-secondary px-1">{endpoint.permission}</code>{" "}
                permission — a key holds whatever its owner&apos;s role holds.
              </>
            ) : (
              <>Any working key may call this.</>
            )}
          </p>
          <Snippet label="Request" code={endpoint.request} />
          <Snippet label="Response" code={endpoint.response} />
        </div>
      )}
    </div>
  );
}

/**
 * The endpoint reference.
 *
 * One page rather than a sidebar of pages: there are eighteen endpoints, and a reader looking
 * for one of them is better served by a single searchable list than by navigation. Each row
 * opens in place, so comparing two of them does not mean losing the first.
 */
export function ApiReference() {
  const [q, setQ] = useState("");
  // The origin is a browser value, not React state: the console is a static export, so anything
  // read while rendering would be baked into the served HTML as whatever the build machine
  // thought the origin was. Subscribed the same way as useIsMobile, with "" as the pre-hydration
  // snapshot — it never changes without a navigation, so there is nothing to listen to.
  const origin = useSyncExternalStore(subscribeToNothing, readOrigin, () => "");

  const groups = useMemo(() => {
    const needle = q.trim().toLowerCase();
    const shown = needle
      ? ENDPOINTS.filter((e) =>
          [e.path, e.summary, e.description, e.group].some((f) =>
            f.toLowerCase().includes(needle),
          ),
        )
      : ENDPOINTS;
    const map = new Map<string, Endpoint[]>();
    for (const e of shown) map.set(e.group, [...(map.get(e.group) ?? []), e]);
    return [...map.entries()];
  }, [q]);

  return (
    <div className="space-y-5">
      <PageHeader
        title="API reference"
        description={
          <>
            Read and drive attest_tag from code. Every request carries{" "}
            <code className="rounded bg-secondary px-1">Authorization: Bearer &lt;key&gt;</code> —
            make one on the{" "}
            <Link href="/developer/api-keys" className="text-primary hover:underline">
              API keys
            </Link>{" "}
            page.
          </>
        }
      />

      <Card className="gap-3 p-4">
        <div className="space-y-1.5">
          <p className="text-sm font-medium">Set this up first</p>
          <p className="text-sm text-muted-foreground">
            Every example below reads these two, so paste them into your shell and the requests
            run as they are written.
          </p>
          <div className="flex items-start gap-1">
            <pre className="min-w-0 flex-1 overflow-x-auto rounded bg-muted/60 px-2 py-1.5 font-mono text-xs leading-relaxed">
              {shellSetup(origin)}
            </pre>
            <CopyButton text={shellSetup(origin)} label="Copy shell setup" />
          </div>
        </div>
        <p className="text-sm text-muted-foreground">
          A key acts as the person who made it, with their permissions, in their organisation
          only. Anything outside it answers 404 rather than 403 — a key should not be able to
          tell a record it may not have from one that does not exist. Connection credentials are
          not reachable here at any permission: they leave through the proxy and nowhere else.
        </p>
        <div className="space-y-1.5">
          <p className="text-sm font-medium">Errors</p>
          <p className="text-sm text-muted-foreground">
            Every failure is{" "}
            <code className="rounded bg-secondary px-1">{`{"error": "…"}`}</code> with a status:
            401 for a key that is missing, unknown, revoked, expired or whose owner has left; 403
            when their role does not hold the permission; 404 for anything outside the
            organisation; 429 when the key passes 240 requests a minute.
          </p>
        </div>
      </Card>

      <SearchField value={q} onChange={setQ} placeholder="Search endpoints" />

      {groups.map(([group, endpoints]) => (
        <section key={group} className="space-y-2">
          <h2 className="text-sm font-semibold text-muted-foreground">{group}</h2>
          <Card className="overflow-hidden p-0">
            {endpoints.map((e) => (
              <EndpointRow key={e.method + e.path} endpoint={e} />
            ))}
          </Card>
        </section>
      ))}

      {groups.length === 0 && (
        <p className="py-10 text-center text-sm text-muted-foreground">
          Nothing matches “{q}”.
        </p>
      )}
    </div>
  );
}
