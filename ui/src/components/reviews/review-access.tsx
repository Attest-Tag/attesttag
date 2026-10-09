"use client";

import { useState } from "react";
import Link from "next/link";
import { toast } from "sonner";
import { Lock, Play } from "lucide-react";
import { StatusChip } from "@/components/core/status-chip";
import { UpgradeButton } from "@/components/core/upgrade-button";
import { useAuth } from "@/components/shell/auth-provider";
import { Button } from "@/components/ui/button";
import { DEFAULT_AUTO_PAUSE_AFTER } from "@/components/reviews/review-format";
import { api, errorMessage, type CodeReviewAccess, type ReviewPaused, type ReviewResumeResponse } from "@/lib/api";

// Two things that stop reviews from starting by themselves, and what a person can do about each.
//
// The plan: where the deployment gates code review on it (CODE_REVIEW=pro or enterprise,
// review_plan.go) an organisation without it keeps what it had — settings, history, the summaries on
// its pull requests — and starts nothing new. The server decides and says why (/api/me's
// code_review), as it does for paused_by on spend; the console never works the rule out from the plan.
//
// The pause: a pull request's automatic reviews stop after five, or when a member says
// `@… pause`, and Resume here does what `@… resume` does there (POST /api/review-pulls/resume).

/** The plan's refusal, when the organisation's plan has no code review here; null when it has. */
export function usePlanRefusal(): CodeReviewAccess | null {
  const { me } = useAuth();
  const a = me?.code_review;
  return a && !a.available && a.reason === "plan" ? a : null;
}

/** "Pro" or "Enterprise": the plan a refusal names, as a sentence says it. */
export function neededPlan(a: CodeReviewAccess): string {
  return a.needs === "enterprise" ? "Enterprise" : "Pro";
}

/**
 * The strip at the top of Reviews where the plan has no code review: which plan has it, why the
 * pages still read, and the way to it this person may take — Choose a plan on Billing where this
 * deployment sells plans, else Upgrade, which asks support; an Enterprise plan is always agreed with
 * support. Somebody who can do neither is told who can, rather than offered a button that refuses.
 */
export function CodeReviewPlanNotice({ access }: { access: CodeReviewAccess }) {
  const { me } = useAuth();
  const plan = me?.plan;
  // /api/me sends only the keys a role holds: absent is no, and no map yet shows the way.
  const perms = me?.user?.permissions;
  const canBill = !perms || perms["billing.manage"] === true;
  const canAsk = !perms || perms["settings.manage"] === true;
  const sells = !!plan?.billing_enabled;
  const support = plan?.support_email ?? "";
  const name = neededPlan(access);

  let way: React.ReactNode = null;
  if (access.needs !== "enterprise" && sells && canBill) {
    way = (
      <Button asChild size="sm">
        <Link href="/settings?tab=billing">Choose a plan</Link>
      </Button>
    );
  } else if (access.needs !== "enterprise" && !sells && support && canAsk) {
    way = <UpgradeButton support={support} askedAt={plan?.requested_at} />;
  } else if (access.needs === "enterprise" && support) {
    way = (
      <Button asChild size="sm" variant="outline">
        <a href={`mailto:${support}`}>Write to {support}</a>
      </Button>
    );
  }

  return (
    <div className="flex flex-wrap items-start gap-x-4 gap-y-2.5 rounded-xl border border-warning/30 bg-warning-soft px-4 py-3 text-sm">
      <Lock className="mt-0.5 size-4 shrink-0 text-warning" />
      <div className="min-w-0 flex-1 space-y-0.5">
        <p className="font-medium text-foreground">Code review needs the {name} plan</p>
        <p className="text-xs leading-relaxed text-muted-foreground">
          {access.message ? `${access.message} ` : ""}
          Its settings and past reviews are here as they were, and nothing new is reviewed — not when a pull
          request opens, not from this page — until the account is on the {name} plan.
          {!way && (sells && access.needs !== "enterprise" ? " Somebody who manages billing can choose it." : " An admin can ask for it.")}
        </p>
      </div>
      {way && <div className="shrink-0">{way}</div>}
    </div>
  );
}

/** Why the starts are off, for a button's title or a dialog's line. */
export function planRefusalLine(a: CodeReviewAccess): string {
  return `Code review needs the ${neededPlan(a)} plan.`;
}

/** "Paused", with who paused it on hover: the ceiling or a member's `@… pause`. */
export function PausedChip({ paused }: { paused: ReviewPaused }) {
  if (!paused.paused) return null;
  return (
    <span title={pausedWhy(paused)}>
      <StatusChip variant="warning">Paused</StatusChip>
    </span>
  );
}

/** Why a pull request's automatic reviews are paused, in a sentence. */
export function pausedWhy(p: ReviewPaused): string {
  return p.paused_by === "auto"
    ? `Automatic reviews paused by themselves after ${p.auto_pause_after ?? DEFAULT_AUTO_PAUSE_AFTER}.`
    : "Automatic reviews paused by a member (@… pause).";
}

/**
 * Resume, for somebody with reviews.manage: the pull request's automatic reviews start again, with the
 * count towards the next pause back at nothing. One resumed meanwhile from GitHub is said so, and the
 * caller reloads either way.
 */
export function ResumeButton({
  repo,
  pr,
  onResumed,
  size = "sm",
}: {
  repo: string;
  pr: number;
  onResumed: () => void;
  size?: "sm" | "xs";
}) {
  const [busy, setBusy] = useState(false);
  const resume = async () => {
    setBusy(true);
    try {
      const out = await api.post<ReviewResumeResponse>("/api/review-pulls/resume", { repo, pr });
      toast.success(
        out.resumed
          ? `Automatic reviews of #${pr} resumed`
          : `Automatic reviews of #${pr} were not paused any more`,
        out.resumed ? { description: "The next push or opening is reviewed by itself again." } : undefined,
      );
      onResumed();
    } catch (err) {
      toast.error(errorMessage(err));
    } finally {
      setBusy(false);
    }
  };
  return (
    <Button type="button" size={size} variant="outline" loading={busy} onClick={() => void resume()}>
      <Play /> Resume
    </Button>
  );
}
