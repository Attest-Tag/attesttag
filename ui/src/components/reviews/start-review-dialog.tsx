"use client";

import { useEffect, useRef, useState } from "react";
import { toast } from "sonner";
import { AlertTriangle, GitPullRequest, Info, Loader2 } from "lucide-react";
import { SegmentedControl } from "@/components/core/segmented-control";
import { resolveSelection, settingsPath, usd } from "@/components/reviews/review-format";
import { planRefusalLine, usePlanRefusal } from "@/components/reviews/review-access";
import { PullField, RepoField, reviewableRepos } from "@/components/reviews/review-pull-picker";
import { shortSHA } from "@/components/reviews/review-run-format";
import { ReviewsTabLink, useReviewsNav } from "@/components/reviews/reviews-nav";
import { useAuth } from "@/components/shell/auth-provider";
import { Button } from "@/components/ui/button";
import { Checkbox } from "@/components/ui/checkbox";
import {
  Dialog,
  DialogContent,
  DialogDescription,
  DialogFooter,
  DialogHeader,
  DialogTitle,
} from "@/components/ui/dialog";
import { Skeleton } from "@/components/ui/skeleton";
import {
  api,
  errorMessage,
  useApi,
  type ReviewEstimate,
  type ReviewNodeDetail,
  type ReviewPull,
  type ReviewPullsResponse,
  type ReviewRunSummary,
  type ReviewSettingsTree,
  type ReviewStartResponse,
  type ReviewTypeSummary,
} from "@/lib/api";
import { formatNumber } from "@/lib/format";
import { cn } from "@/lib/utils";

// Start review: repository, then one of its open pull requests, then the types the matching branch
// rule ticks, Live or Shadow, the whole pull request or only what changed since the last review,
// the estimate, and Start (POST /api/reviews). Every way in passes what it already knows — a
// repository's panel its repository, a pull request's Review button the number too, a past
// review's "Run with other types…" the whole run — so the dialog opens on the first step still open.
//
// The types are ticked by asking the settings which branch rule the pull request's branches fall
// under (GET /api/review-settings/{id}?base=&head=&labels=), the same review.MatchRule and
// review.LabelTypes the lane runs — with what the pull request's labels add — rather than by a second
// glob matcher here that could drift from it. A pull request typed by its number,
// past what the list reads, has no branches here, so the estimate asked with no types answers in
// its place: it carries the rule's label and types. Left as the rule ticked them, the request names
// no types, so the run is credited to the rule as an automatic one would be; changed, it names them,
// and the summary says a person chose them. Where the result goes works the same way: untouched, the
// request names none and the server sends it where the rule does.
//
// Every answer is used only once it is the answer for what is picked now. useApi keeps the last
// path's data while the next loads, and a list of the last repository's pull requests, or the last
// pull request's rule, would otherwise be shown — and started — under the new one.

export type StartReviewTarget = {
  repo?: string;
  pr?: number;
  title?: string;
  /** "Run with other types…": the run to run again, whose repository and pull request are fixed. */
  rerunOf?: string;
  /** What to tick instead of the branch rule's: the past run's types. */
  types?: string[];
  /** Where the past run went. */
  post?: string;
};

export function StartReviewDialog({
  target,
  onOpenChange,
  onStarted,
  onQueued,
}: {
  /** What to start on; null keeps the dialog closed. */
  target: StartReviewTarget | null;
  onOpenChange: (open: boolean) => void;
  /** Told the run that was queued, or the earlier review that answers it, in place of opening History. */
  onStarted?: (run: ReviewRunSummary) => void;
  /** Told when a run was queued, beside whatever happens next: a list on screen that should say so. */
  onQueued?: (run: ReviewRunSummary) => void;
}) {
  const startRef = useRef<HTMLButtonElement>(null);
  const contentRef = useRef<HTMLDivElement>(null);
  return (
    <Dialog open={target !== null} onOpenChange={onOpenChange}>
      <DialogContent
        ref={contentRef}
        className="max-h-[90vh] overflow-y-auto sm:max-w-lg"
        // Opened on one pull request — its Review button, Run with other types… — every choice is
        // made already, so Enter should start it. Start is disabled until the types have loaded,
        // and a disabled button takes no focus, so the dialog holds it until then (StartReviewBody
        // hands it on). Otherwise it starts on the repository, the first thing still to pick.
        onOpenAutoFocus={(e) => {
          if (target?.repo && target.pr) {
            e.preventDefault();
            contentRef.current?.focus();
          }
        }}
      >
        {target && (
          <StartReviewBody
            key={JSON.stringify(target)}
            target={target}
            startRef={startRef}
            onClose={() => onOpenChange(false)}
            onStarted={onStarted}
            onQueued={onQueued}
          />
        )}
      </DialogContent>
    </Dialog>
  );
}

/** "?repo=&pr=&types=": the estimate route; no types asks it which the branch rule picks. */
function estimatePath(repo: string, pr: number, types: string[] | null): string {
  const q = new URLSearchParams({ repo, pr: String(pr) });
  if (types) q.set("types", types.join(","));
  return `/api/reviews/estimate?${q.toString()}`;
}

type Post = "live" | "shadow";
type Scope = "whole" | "since_last";

function StartReviewBody({
  target,
  startRef,
  onClose,
  onStarted,
  onQueued,
}: {
  target: StartReviewTarget;
  startRef: React.RefObject<HTMLButtonElement | null>;
  onClose: () => void;
  onStarted?: (run: ReviewRunSummary) => void;
  onQueued?: (run: ReviewRunSummary) => void;
}) {
  const { me } = useAuth();
  const perms = me?.user?.permissions;
  const canReach = !perms || perms["connections.manage"] === true;
  const rerun = !!target.rerunOf;
  // The organisation's plan has no code review here: the server would answer 402, so Start is off and
  // the dialog says why before anybody presses it.
  const refused = usePlanRefusal();

  const [repo, setRepo] = useState(target.repo ?? "");
  const [pr, setPr] = useState<number | null>(target.pr ?? null);
  // null follows the rule (or the past run) until somebody changes it.
  const [picked, setPicked] = useState<string[] | null>(null);
  const [post, setPost] = useState<Post | null>(null);
  const [scope, setScope] = useState<Scope>("whole");
  const [busy, setBusy] = useState(false);
  const [error, setError] = useState("");
  const [same, setSame] = useState<ReviewRunSummary | null>(null);

  const tree = useApi<ReviewSettingsTree>("/api/review-settings");
  const typeList = useApi<{ types: ReviewTypeSummary[] }>("/api/review-types");
  const pulls = useApi<ReviewPullsResponse>(repo ? `/api/review-pulls?repo=${encodeURIComponent(repo)}` : null);

  const go = useReviewsNav();
  const repos = reviewableRepos(tree.data);
  const resolved = tree.data && repo ? resolveSelection(tree.data, { node: repo }) : null;
  // The pull requests of the repository picked now — not the last one's, still held while these load.
  const pullList = pulls.refreshing ? undefined : pulls.data;
  const pull: ReviewPull | undefined = pullList?.pulls.find((p) => p.number === pr);
  // Typed by number, past what the list reads: no branches here to match a rule with.
  const byNumber = !!pr && !!pullList && !pull;

  // Which branch rule this pull request falls under, and what it makes of the settings.
  const matchPath =
    resolved && pull
      ? `${settingsPath(resolved)}&base=${encodeURIComponent(pull.base)}&head=${encodeURIComponent(pull.head)}` +
        (pull.labels?.length ? `&labels=${encodeURIComponent(pull.labels.join(","))}` : "")
      : null;
  const match = useApi<ReviewNodeDetail>(matchPath);
  const matched = matchPath && !match.refreshing ? match.data : undefined;
  const rule = matched?.rule;
  // The estimate with no types, for a pull request typed by number: the rule's label and types.
  const probePath = byNumber && !target.types?.length && repo && pr ? estimatePath(repo, pr, null) : null;
  const probe = useApi<ReviewEstimate>(probePath);
  const probed = probePath && !probe.refreshing ? probe.data : undefined;
  const ruleLabel = rule?.label ?? probed?.rule ?? "";
  const ruleTypes = rule?.types ?? (probed?.rule ? probed.types : undefined);
  const ruleMode = rule?.effective.mode ?? matched?.effective.mode;
  // The lane refuses a repository that is off before any rule applies (review_lane.go), a start
  // from here included: said now, with the way to the setting, rather than after Start.
  const repoMode = matched?.effective.mode ?? (resolved?.kind === "repo" ? resolved.repo.mode : undefined);
  const off = repoMode === "off";

  const enabled = (typeList.data?.types ?? []).filter((t) => t.enabled);
  const known = (k: string) => enabled.some((t) => t.key === k);
  // What is ticked before anybody touches it: the past run's types, else the rule's, else General.
  const suggested = target.types?.length ? target.types : (ruleTypes ?? ["general"]);
  const offTypes = suggested.filter((k) => typeList.data && !known(k));
  const baseline = suggested.filter(known);
  const types = picked ?? baseline;
  const changed = picked !== null && !sameList(picked, baseline);
  // Left as the rule ticked them, the request names no types and the rule is credited with choosing
  // them — only when the rule is known: a General ticked for want of one is named. A run again always
  // names them: the server would otherwise take every type the past run had, one turned off since
  // included, and refuse it.
  const credit = !rerun && !changed && !!ruleTypes;

  const liveAllowed = canReach || ruleMode === "live";
  const defaultPost: Post = target.post === "live" || target.post === "shadow" ? target.post : ruleMode === "live" ? "live" : "shadow";
  const chosenPost: Post = (post ?? defaultPost) === "live" && liveAllowed ? "live" : "shadow";
  const reviewedBefore = !!pull?.review?.last_reviewed_sha;

  // Asked once the rule has answered, so the first figure is for the types that will be ticked.
  const settled =
    (!matchPath || !!matched || !!match.error) && (!probePath || !!probed || !!probe.error);
  const estimate = useApi<ReviewEstimate>(repo && pr && types.length > 0 && settled ? estimatePath(repo, pr, types) : null);
  const estimated = estimate.refreshing ? undefined : estimate.data;

  const ready = !!repo && !!pr && types.length > 0 && !off && !refused;

  // The first moment Start can be pressed, on a dialog opened with its pull request chosen: the
  // focus moves to it, unless the person has already put it somewhere else.
  const handedOn = useRef(false);
  useEffect(() => {
    if (!ready || handedOn.current || !(target.repo && target.pr)) return;
    handedOn.current = true;
    const at = document.activeElement;
    if (!at || at === document.body || at.getAttribute("role") === "dialog") startRef.current?.focus();
  }, [ready, target.repo, target.pr, startRef]);

  const pickPull = (n: number) => {
    setPr(n);
    setPicked(null);
    setPost(null);
    setScope("whole");
    setSame(null);
    setError("");
  };

  // Where a run is shown: the caller's own History, or — from Settings, which stays on screen after
  // a start — History itself once somebody asks for it.
  const show = (run: ReviewRunSummary, open: boolean) => {
    if (onStarted) onStarted(run);
    else if (open) go("history", { run: run.id });
    onClose();
  };

  const toggle = (k: string) => {
    const cur = picked ?? baseline;
    setPicked(cur.includes(k) ? cur.filter((x) => x !== k) : [...cur, k]);
  };

  const start = async (force: boolean) => {
    if (!ready || busy) return;
    setBusy(true);
    setError("");
    // Untouched, a start names no destination and goes where the rule sends it — the rule the
    // server matches, which is right even when the one shown here was not known yet. A run again
    // names the one it shows, since the server would otherwise take the past run's.
    const body = { types: credit ? [] : types, post: post !== null || rerun ? chosenPost : "", scope, force };
    try {
      const out = rerun
        ? await api.post<ReviewStartResponse>(`/api/reviews/${target.rerunOf}/rerun`, body)
        : await api.post<ReviewStartResponse>("/api/reviews", { repo, pr, ...body });
      if (out.answered_from_state) {
        setSame(out.run);
        return;
      }
      onQueued?.(out.run);
      const went = out.run.post || chosenPost;
      toast.success(`Review of ${repo}#${pr} started`, {
        description: went === "live" ? "It posts on the pull request when it is done." : "Shadow: it is recorded here and nothing is posted.",
        // Started from somewhere History is not on screen: one press away from following it.
        action: onStarted ? undefined : { label: "Open", onClick: () => go("history", { run: out.run.id }) },
      });
      show(out.run, false);
    } catch (err) {
      setError(errorMessage(err));
    } finally {
      setBusy(false);
    }
  };


  return (
    <form
      className="space-y-5"
      onSubmit={(e) => {
        e.preventDefault();
        void start(false);
      }}
    >
      <DialogHeader>
        <DialogTitle>{rerun ? "Run with other types" : "Start a review"}</DialogTitle>
        <DialogDescription>
          {rerun ? (
            <>
              <span className="font-mono">{repo}</span> · #{pr}, on the commit it is at now.
            </>
          ) : (
            "Reviews one pull request now, whatever the settings say about when. The connection, fork policy, the budgets and the throttles still apply."
          )}
        </DialogDescription>
      </DialogHeader>

      {!rerun && (
        <RepoField
          id="start-repo"
          repos={repos}
          error={tree.error}
          value={repo}
          onChange={(v) => {
            setRepo(v);
            setPr(null);
            setPicked(null);
            setSame(null);
          }}
        />
      )}

      {repo && !rerun && (
        // Keyed on the repository, so a search or a number typed for the last one starts empty.
        <PullField key={repo} id="start-pr" repo={repo} pulls={pullList} error={pulls.error} pr={pr} onPick={pickPull} />
      )}

      {off && (
        <p className="flex gap-2 rounded-lg border border-warning/30 bg-warning-soft px-3 py-2 text-sm">
          <AlertTriangle className="mt-0.5 size-4 shrink-0 text-warning" />
          <span>
            Review is off for <span className="font-mono">{repo}</span>, and a start from here is refused like any
            other. Turn it on under{" "}
            <ReviewsTabLink
              tab="settings"
              params={{ node: repo, ...(resolved ? { conn: resolved.conn.id } : {}) }}
              className="font-medium text-primary underline-offset-2 hover:underline"
            >
              its settings
            </ReviewsTabLink>{" "}
            first.
          </span>
        </p>
      )}

      {repo && pr && (
        <>
          <fieldset className="min-w-0 space-y-2">
            <legend className="mb-1.5 text-sm font-medium">Review types</legend>
            {!typeList.data ? (
              <Skeleton className="h-16 w-full" />
            ) : (
              <div className="grid gap-1.5 sm:grid-cols-2">
                {enabled.map((t) => {
                  const on = types.includes(t.key);
                  const order = on && types.length > 1 ? types.indexOf(t.key) + 1 : 0;
                  return (
                    <label
                      key={t.key}
                      className={cn(
                        "flex cursor-pointer items-center gap-2.5 rounded-lg border px-3 py-2 text-sm transition-colors hover:bg-secondary/60",
                        on && "border-primary/40 bg-accent/40",
                      )}
                    >
                      <Checkbox checked={on} onCheckedChange={() => toggle(t.key)} />
                      <span className="min-w-0 flex-1 truncate">{t.name}</span>
                      {order > 0 && (
                        <span className="text-xs tabular-nums text-muted-foreground" title="The order they run in">
                          {order}
                        </span>
                      )}
                    </label>
                  );
                })}
              </div>
            )}
            <p className="text-xs text-muted-foreground">
              {target.types?.length
                ? "Ticked as the review you are running again had them."
                : rule
                  ? `Ticked by the branch rule ${rule.label}${rule.index >= 0 ? ` (rule ${rule.index + 1})` : ""}${
                      rule.added_types?.length
                        ? `, with ${rule.added_types.length === 1 ? "one type" : `${rule.added_types.length} types`} ${(rule.labels?.length ?? 0) === 1 ? "its label adds" : "its labels add"}`
                        : ""
                    }.`
                  : ruleTypes
                    ? `Ticked by the branch rule ${ruleLabel}.`
                    : !settled
                      ? "Finding the branch rule…"
                      : "No branch rule decides this one, so General is ticked."}
              {offTypes.length > 0 && ` ${offTypes.join(", ")} ${offTypes.length === 1 ? "is" : "are"} turned off, so left out.`}
              {types.length === 0 && " Tick at least one."}
            </p>
            {changed && (
              <button
                type="button"
                onClick={() => setPicked(null)}
                className="text-xs font-medium text-primary underline-offset-2 hover:underline"
              >
                Tick them as {target.types?.length ? "before" : "the rule does"}
              </button>
            )}
          </fieldset>

          <div className="grid gap-4 sm:grid-cols-2">
            <div className="space-y-1.5">
              <p className="text-sm font-medium">Post</p>
              <SegmentedControl<Post>
                value={chosenPost}
                onValueChange={setPost}
                options={[
                  { value: "shadow", label: "Shadow" },
                  { value: "live", label: "Live", disabled: !liveAllowed },
                ]}
                className="w-fit"
              />
              <p className="text-xs text-muted-foreground">
                {!liveAllowed
                  ? "Live needs Manage connections here: this pull request's reviews are recorded in shadow."
                  : chosenPost === "live"
                    ? "Posted on the pull request when it is done."
                    : "Recorded here; nothing is posted on GitHub."}
              </p>
            </div>
            <div className="space-y-1.5">
              <p className="text-sm font-medium">Read</p>
              <SegmentedControl<Scope>
                value={scope}
                onValueChange={setScope}
                options={[
                  { value: "whole", label: "Whole PR" },
                  { value: "since_last", label: "Since last review", disabled: !reviewedBefore && !!pull },
                ]}
                className="w-fit"
              />
              <p className="text-xs text-muted-foreground">
                {scope === "since_last"
                  ? `New minor findings only in files changed since ${shortSHA(pull?.review?.last_reviewed_sha ?? "")}.`
                  : pull && !reviewedBefore
                    ? "Never reviewed, so the whole pull request is read."
                    : "Every changed file is read and judged afresh."}
              </p>
            </div>
          </div>

          <EstimateBox estimate={estimated} error={estimate.error} idle={types.length === 0} />
        </>
      )}

      {same && (
        <div className="space-y-2 rounded-lg border border-info/30 bg-info-soft px-3 py-2.5 text-sm">
          <p className="flex gap-2">
            <Info className="mt-0.5 size-4 shrink-0 text-info" />
            <span>
              Commit <span className="font-mono">{shortSHA(same.head_sha)}</span> was already reviewed with these types under these
              settings, so that review is the answer, at no cost.
            </span>
          </p>
          <div className="flex flex-wrap gap-2 pl-6">
            <Button
              type="button"
              size="sm"
              variant="outline"
              onClick={() => show(same, true)}
            >
              Open that review
            </Button>
            <Button type="button" size="sm" variant="outline" disabled={busy} onClick={() => void start(true)}>
              Run anyway
            </Button>
          </div>
        </div>
      )}

      {refused && (
        <p className="flex gap-2 rounded-lg border border-warning/30 bg-warning-soft px-3 py-2 text-sm">
          <AlertTriangle className="mt-0.5 size-4 shrink-0 text-warning" />
          <span>
            {planRefusalLine(refused)} {refused.message}
          </span>
        </p>
      )}

      {error && (
        <p className="flex gap-2 rounded-lg border border-danger/30 bg-danger-soft px-3 py-2 text-sm">
          <AlertTriangle className="mt-0.5 size-4 shrink-0 text-danger" />
          <span>{error}</span>
        </p>
      )}

      <DialogFooter>
        <Button type="button" variant="outline" onClick={onClose}>
          Cancel
        </Button>
        <Button ref={startRef} type="submit" disabled={!ready} loading={busy}>
          <GitPullRequest />
          {rerun ? "Run again" : "Start review"}
        </Button>
      </DialogFooter>
    </form>
  );

}

/** "About $0.12–$0.48": a range from the pull request's size, the types and the list prices. */
function EstimateBox({ estimate: e, error, idle }: { estimate?: ReviewEstimate; error?: string; idle: boolean }) {
  return (
    <div className="rounded-lg border bg-muted/40 px-3 py-2.5 text-sm">
      {idle ? (
        // Nothing is asked with no type ticked, so nothing is on its way either.
        <p className="text-muted-foreground">Tick a type to see the estimate.</p>
      ) : error && !e ? (
        <p className="text-muted-foreground">No estimate: {error}</p>
      ) : !e ? (
        <p className="flex items-center gap-2 text-muted-foreground">
          <Loader2 className="size-3.5 animate-spin" /> Estimating…
        </p>
      ) : (
        <div className="space-y-1">
          <p>
            <span className="text-muted-foreground">Estimated cost </span>
            <span className="font-medium tabular-nums">
              {e.usd ? (e.usd.low === e.usd.high ? usd(e.usd.high) : `${usd(e.usd.low)}–${usd(e.usd.high)}`) : "unknown"}
            </span>
            {e.capped && <span className="text-muted-foreground"> · held to {usd(e.max_usd)}, where a review stops</span>}
          </p>
          <p className="text-xs text-muted-foreground">
            {formatNumber(e.files)} file{e.files === 1 ? "" : "s"}, +{formatNumber(e.additions)} −{formatNumber(e.deletions)} ·{" "}
            <span className="font-mono">{e.model}</span>
            {!e.priced && " has no list price here, so the range is unknown"} · a range, not a quote
          </p>
        </div>
      )}
    </div>
  );
}

function sameList(a: string[], b: string[]): boolean {
  return a.length === b.length && a.every((x, i) => x === b[i]);
}
