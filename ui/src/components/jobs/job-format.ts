// Display helpers for fix jobs, shared by the list page and the detail dialog.
// Kept free of React so either can import it without a cycle.

import type { StatusChipVariant } from "@/components/core/status-chip";

export function jobStatusVariant(status: string): StatusChipVariant {
  switch (status) {
    case "queued":
    case "starting":
      return "info";
    case "running":
      return "ai";
    case "stale":
    case "cancelling":
      return "warning";
    case "succeeded":
      return "success";
    case "failed":
    case "timeout":
      return "danger";
    default:
      return "neutral";
  }
}

/** "$0.0123" under a dollar, "$1.23" from there on. */
export function formatJobCost(usd: number): string {
  if (!usd) return "$0.00";
  return usd < 1 ? `$${usd.toFixed(4)}` : `$${usd.toFixed(2)}`;
}

/** "45s", "3m 20s", "1h 04m". */
export function formatJobDuration(seconds: number): string {
  if (!seconds || seconds < 0) return "—";
  if (seconds < 60) return `${Math.round(seconds)}s`;
  const m = Math.floor(seconds / 60);
  if (m < 60) return `${m}m ${Math.round(seconds % 60)}s`;
  return `${Math.floor(m / 60)}h ${String(m % 60).padStart(2, "0")}m`;
}

/** The execution link's label: the console of whichever platform the job ran on. */
export function jobConsoleLabel(dispatcher: string): string {
  switch (dispatcher) {
    case "cloudrun":
      return "Cloud Run";
    case "ecs":
      return "AWS console";
    case "aca":
      return "Azure portal";
    default:
      return "Execution";
  }
}

/** The checklist rows, in order; several worker phases fold into one label on the Slack card. */
export const JOB_PHASES: Array<[string, string]> = [
  ["clone", "Clone"],
  ["test_before", "Tests before"],
  ["engine", "Change"],
  ["test_after", "Tests after"],
  ["commit", "Commit"],
  ["push", "Push"],
  ["pr", "Pull request"],
];

export function phaseGlyph(status: string | undefined): { glyph: string; className: string; label: string } {
  switch (status) {
    case "started":
      return { glyph: "◐", className: "text-ai", label: "in progress" };
    case "ok":
      return { glyph: "●", className: "text-success-text", label: "done" };
    case "failed":
      return { glyph: "✕", className: "text-danger", label: "failed" };
    case "skipped":
      return { glyph: "–", className: "text-muted-foreground", label: "skipped" };
    default:
      return { glyph: "○", className: "text-muted-foreground/50", label: "pending" };
  }
}

/** GitHub page of an "owner/name" repository; "" for anything else. */
export function repoURL(repo: string): string {
  return /^[\w.-]+\/[\w.-]+$/.test(repo) ? `https://github.com/${repo}` : "";
}

/** GitHub page of one branch; "" when the repository or the branch is missing. */
export function branchURL(repo: string, branch: string): string {
  const base = repoURL(repo);
  if (!base || !branch) return "";
  return `${base}/tree/${branch.split("/").map(encodeURIComponent).join("/")}`;
}

/**
 * The shape of a branch the worker may push: "feature/fix-<n>-<change>-attest_tag" in Settings,
 * where there is no job yet, and "bugfix/*-attest_tag" on a job, where it reads as the rule the
 * job was dispatched under. It mirrors jobBranchID in internal/app/jobs.go — the prefix is the
 * organisation's own convention and may be empty, the suffix marks the branch as the bot's, and
 * neither separator is assumed.
 */
export function jobBranchShape(
  prefix: string | undefined,
  suffix: string | undefined,
  body = "fix-<n>-<change>",
): string {
  let p = prefix ?? "";
  if (p && !/[/\-_]$/.test(p)) p += "/";
  let s = suffix || "attest_tag";
  if (!/^[-_.]/.test(s)) s = `-${s}`;
  return `${p}${body}${s}`;
}

/**
 * The prefixes a worker_branch_prefix setting offers — a list like "feature/, bugfix/, hotfix/".
 * "none" is how an organisation says it keeps no convention; it mirrors branchPrefixes in
 * internal/app/jobs.go.
 */
export function branchPrefixList(setting: string | undefined): string[] {
  const t = (setting ?? "").trim();
  if (!t || t.toLowerCase() === "none") return [];
  return t.split(/[,;\s]+/).filter(Boolean);
}

/**
 * One shape per convention on offer, so Settings shows what a branch will actually be called.
 * With no prefix set there is still one shape: the branch starts at fix-.
 */
export function jobBranchShapes(prefix: string | undefined, suffix: string | undefined): string[] {
  const list = branchPrefixList(prefix);
  if (list.length === 0) return [jobBranchShape("", suffix)];
  return list.map((p) => jobBranchShape(p, suffix));
}
