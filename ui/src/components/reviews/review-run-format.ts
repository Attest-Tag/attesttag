// Words for Reviews › History and a run's detail: what a run's status, trigger and score are called,
// and what became of each finding. Free of React, beside review-format.ts, which says the same for
// the settings — a review recorded in shadow is "Shadow" in both, never "Recorded" in one.

import type { StatusChipVariant } from "@/components/core/status-chip";
import { formatUSD } from "@/lib/format";

export const RUN_STATUSES: { value: string; label: string; variant: StatusChipVariant }[] = [
  { value: "queued", label: "Queued", variant: "neutral" },
  { value: "running", label: "Reviewing", variant: "ai" },
  { value: "posted", label: "Posted", variant: "success" },
  { value: "shadow", label: "Shadow", variant: "info" },
  { value: "noop", label: "Already reviewed", variant: "neutral" },
  { value: "skipped", label: "Skipped", variant: "warning" },
  { value: "superseded", label: "Superseded", variant: "neutral" },
  { value: "failed", label: "Failed", variant: "danger" },
  { value: "cancelled", label: "Cancelled", variant: "neutral" },
];

export function runStatus(status: string): { label: string; variant: StatusChipVariant } {
  return RUN_STATUSES.find((s) => s.value === status) ?? { label: status || "unknown", variant: "neutral" };
}

/** Why a pull request was set aside rather than reviewed, in the words a chip can carry: the lane's reviewSkip reasons. */
const SKIPS: Record<string, string> = {
  off: "review is off",
  not_reviewed: "account not added",
  removed: "removed from reviews",
  installation_mismatch: "another installation",
  closed: "closed",
  trigger: "not asked for",
  draft: "draft",
  bot: "opened by a bot",
  excluded_author: "author skipped",
  fork: "from a fork",
  no_rule: "no branch rule",
  types_off: "its types are off",
  throttle: "daily limit",
  budget: "out of budget",
  own_key_blocked: "own model key refused",
  own_key_off: "own model key not for reviews",
  nothing_to_review: "nothing to review",
  paused: "automatic reviews paused",
  plan: "not on this plan",
};

export function skipLabel(reason: string): string {
  return SKIPS[reason] ?? reason.replace(/_/g, " ");
}

/** Still in the lane: worth polling, and not worth running again yet. */
export function isRunActive(status: string): boolean {
  return status === "queued" || status === "running";
}

const TRIGGERS: Record<string, string> = {
  open: "Pull request opened",
  push: "Push",
  command: "@-command",
  catchup: "Catch-up",
  console: "Console",
  api: "API",
  reply: "Reply",
  label: "Label added",
  chat: "Slack or Teams",
};

export function triggerLabel(trigger: string): string {
  return TRIGGERS[trigger] ?? trigger;
}

/** The score's colour: 5 and 4 are safe to merge, 3 and 2 have a P1 open, 1 and 0 a P0. */
export function scoreVariant(score: number): StatusChipVariant {
  return score >= 4 ? "success" : score >= 2 ? "warning" : "danger";
}

export function severityVariant(sev: string): StatusChipVariant {
  return sev === "P0" ? "danger" : sev === "P1" ? "warning" : "info";
}

/** "a1b2c3d" — the seven characters GitHub shows a commit by. */
export function shortSHA(sha: string): string {
  return (sha ?? "").slice(0, 7);
}

/** A run's cost: "—" for one that spent nothing, so a skip does not read as a cheap review. */
export function runCost(usd: number): string {
  return usd > 0 ? formatUSD(usd) : "—";
}

export function formatDuration(ms: number | null | undefined): string {
  if (ms == null || ms < 0) return "—";
  const s = Math.round(ms / 1000);
  if (s < 60) return `${s} s`;
  const m = Math.floor(s / 60);
  return m < 60 ? `${m} min ${s % 60} s` : `${Math.floor(m / 60)} h ${m % 60} min`;
}

// ---- findings ----

/** The order the detail lists findings in: what still needs somebody first. */
export const FINDING_STATUSES: { value: string; label: string; hint: string; variant: StatusChipVariant }[] = [
  { value: "open", label: "Open", hint: "Still standing on the pull request.", variant: "warning" },
  { value: "disputed", label: "Disputed", hint: "Argued in its thread, and the reviewer kept it.", variant: "danger" },
  { value: "fixed", label: "Fixed", hint: "A later push changed the lines and the problem went with them.", variant: "success" },
  { value: "resolved_by_human", label: "Resolved", hint: "Somebody resolved its thread on GitHub.", variant: "success" },
  { value: "withdrawn", label: "Withdrawn", hint: "Taken back after a reply showed it was wrong.", variant: "neutral" },
  { value: "outdated", label: "Outdated", hint: "Its code moved on before anybody settled it.", variant: "neutral" },
];

export function findingStatus(status: string) {
  return FINDING_STATUSES.find((s) => s.value === status) ?? { value: status, label: status, hint: "", variant: "neutral" as const };
}

/** "src/api/users.go:41–58". */
export function findingWhere(f: { path: string; start_line?: number; line: number }): string {
  if (!f.line) return f.path;
  return f.start_line && f.start_line !== f.line ? `${f.path}:${f.start_line}–${f.line}` : `${f.path}:${f.line}`;
}

const PLACES: Record<string, string> = {
  inline: "Inline on the diff",
  outside_diff: "In the summary: outside the changed lines",
  masked: "In the summary: a credential was masked there",
  unchanged_file: "In the summary: a file this pull request does not change",
  pre_existing: "In the summary: not introduced here",
  more_notes: "In the summary: over the comment cap",
  below_inline_severity: "In the summary: its type comments inline only on worse",
};

/** Where a finding was said: on the diff, or which part of the summary and why. */
export function placementLabel(placement: string, place: string): string {
  if (placement === "inline") return PLACES.inline;
  return PLACES[place] ?? "In the summary";
}

/** What a candidate the review did not keep was dropped for, in a few words; its detail says more. */
const DROPS: Record<string, string> = {
  invalid: "Malformed",
  ignored: "In an ignored path",
  ungrounded: "Not in the code",
  unchanged_file: "File not changed here",
  duplicate: "Duplicate",
  withdrawn: "Withdrawn before",
  rereview: "Re-review limit",
  cap: "Over the cap",
  budget: "Out of money",
  unverified: "Not verified",
  refuted: "Refuted",
  uncertain: "Verifier unsure",
  low_confidence: "Low confidence",
};

export function dropReason(reason: string): string {
  return DROPS[reason] ?? reason.replace(/_/g, " ");
}

const NOT_REVIEWED: Record<string, string> = {
  ignored: "ignored by the settings",
  binary: "binary",
  generated: "generated",
  budget: "the money ran out first",
  timeout: "the time ran out first",
};

export function notReviewedReason(reason: string): string {
  return NOT_REVIEWED[reason] ?? reason.replace(/_/g, " ");
}
