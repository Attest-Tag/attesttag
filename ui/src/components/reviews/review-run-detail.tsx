"use client";

import { useEffect } from "react";
import { toast } from "sonner";
import { ExternalLink, FileText, GitPullRequest, ListChecks, MessageSquare, RotateCw, Shuffle } from "lucide-react";
import { useConfirm } from "@/components/core/confirm-dialog";
import { Disclosure } from "@/components/core/disclosure";
import { ErrorBanner } from "@/components/core/error-banner";
import { Markdown } from "@/components/core/markdown";
import { RelativeTime } from "@/components/core/relative-time";
import { StatusChip } from "@/components/core/status-chip";
import { RepoLink } from "@/components/jobs/repo-link";
import { PausedChip, ResumeButton, pausedWhy, planRefusalLine, usePlanRefusal } from "@/components/reviews/review-access";
import { usd } from "@/components/reviews/review-format";
import {
  FINDING_STATUSES,
  dropReason,
  findingStatus,
  findingWhere,
  formatDuration,
  isRunActive,
  notReviewedReason,
  placementLabel,
  runStatus,
  scoreVariant,
  severityVariant,
  shortSHA,
  triggerLabel,
} from "@/components/reviews/review-run-format";
import { ReviewsTabLink } from "@/components/reviews/reviews-nav";
import type { StartReviewTarget } from "@/components/reviews/start-review-dialog";
import { Button } from "@/components/ui/button";
import { Dialog, DialogContent, DialogDescription, DialogHeader, DialogTitle } from "@/components/ui/dialog";
import { Skeleton } from "@/components/ui/skeleton";
import { useStickyFlags } from "@/hooks/use-sticky";
import {
  api,
  errorMessage,
  useApi,
  type ReviewDrop,
  type ReviewFinding,
  type ReviewRunDetail,
  type ReviewRunSummary,
  type ReviewStartResponse,
  type ReviewTypeSummary,
} from "@/lib/api";
import { formatDateTime, formatNumber, formatUSD } from "@/lib/format";
import { cn } from "@/lib/utils";

// One review in full, as the Jobs page shows one job: a dialog over the list, named in the address
// as ?run=<public id> so a link from Settings, a toast or a colleague lands on it. What it was asked
// for and under which rule and types, what it came to, every finding it raised and where each stands
// now, what it dropped and why, what it did not read, and what it cost — with Run again and Run with
// other types for somebody who may start reviews.

export function ReviewRunDialog({
  id,
  onOpenChange,
  canManage,
  onStarted,
  onRerunWith,
}: {
  id: string | null;
  onOpenChange: (open: boolean) => void;
  canManage: boolean;
  /** A run Run again queued, or the earlier review that answered it. */
  onStarted: (run: ReviewRunSummary) => void;
  /** Run with other types…: the Start review dialog, on this run. */
  onRerunWith: (target: StartReviewTarget) => void;
}) {
  return (
    <Dialog open={id !== null} onOpenChange={onOpenChange}>
      <DialogContent className="max-h-[88vh] overflow-y-auto sm:max-w-3xl">
        {id !== null && (
          <RunBody key={id} id={id} canManage={canManage} onStarted={onStarted} onRerunWith={onRerunWith} />
        )}
      </DialogContent>
    </Dialog>
  );
}

/** A run's detail, polled while it is still in the lane. Shared with Types › Try on a PR. */
export function useReviewRun(id: string | null) {
  const d = useApi<ReviewRunDetail>(id ? `/api/reviews/${id}` : null);
  const active = !!d.data && isRunActive(d.data.run.status);
  const reload = d.reload;
  useEffect(() => {
    if (!active) return;
    const t = setInterval(reload, 4_000);
    return () => clearInterval(t);
  }, [active, reload]);
  return d;
}

/** Review type keys as the console names them: a name where the type is known, else its key. */
export function useTypeNames(): (key: string) => string {
  const types = useApi<{ types: ReviewTypeSummary[] }>("/api/review-types");
  return (key: string) => types.data?.types.find((t) => t.key === key)?.name ?? key;
}

function RunBody({
  id,
  canManage,
  onStarted,
  onRerunWith,
}: {
  id: string;
  canManage: boolean;
  onStarted: (run: ReviewRunSummary) => void;
  onRerunWith: (target: StartReviewTarget) => void;
}) {
  const d = useReviewRun(id);
  const nameOf = useTypeNames();
  const { confirm, confirmDialog } = useConfirm();
  const sections = useStickyFlags("reviews:run-sections");
  // Run again starts a review, which the plan refuses where it has no code review: off, with why.
  const refused = usePlanRefusal();

  if (d.error && !d.data) {
    return (
      <>
        <DialogHeader>
          <DialogTitle>Review</DialogTitle>
        </DialogHeader>
        <ErrorBanner message={d.error} onRetry={d.reload} />
      </>
    );
  }
  if (!d.data) {
    return (
      <>
        <DialogHeader>
          <DialogTitle className="sr-only">Loading the review</DialogTitle>
        </DialogHeader>
        <div className="space-y-3">
          <Skeleton className="h-6 w-2/3" />
          <Skeleton className="h-4 w-1/2" />
          <Skeleton className="h-32 w-full" />
        </div>
      </>
    );
  }

  const detail = d.data;
  const { run, request, pr, links } = detail;
  const status = runStatus(run.status);
  const active = isRunActive(run.status);
  const isTry = run.kind === "try";
  const typeKeys = run.types.map((t) => t.key);

  const runAgain = async (force: boolean) => {
    try {
      const out = await api.post<ReviewStartResponse>(`/api/reviews/${run.id}/rerun`, force ? { force: true } : undefined);
      if (out.answered_from_state && !force) {
        const again = await confirm({
          title: "Already reviewed at this commit",
          description: `${run.repo}#${run.pr} is still at ${shortSHA(out.run.head_sha)}, which was reviewed with these types under these settings. That review answers it at no cost; run it anyway to look again from scratch.`,
          confirmLabel: "Run anyway",
          cancelLabel: "Keep that review",
        });
        if (again) return runAgain(true);
        onStarted(out.run);
        return;
      }
      const left = out.left_out ?? [];
      toast.success(`Review of ${run.repo}#${run.pr} started again`, {
        description:
          left.length > 0
            ? `${left.map(nameOf).join(", ")} ${left.length === 1 ? "is" : "are"} turned off now, so ${left.length === 1 ? "it was" : "they were"} left out.`
            : undefined,
      });
      onStarted(out.run);
    } catch (err) {
      toast.error(errorMessage(err));
    }
  };

  const openDropped = () => {
    sections.set("dropped", true);
    requestAnimationFrame(() => document.getElementById("run-dropped")?.scrollIntoView({ behavior: "smooth", block: "start" }));
  };

  return (
    <div className="space-y-6">
      <DialogHeader className="text-left">
        <DialogTitle className="flex flex-wrap items-center gap-2 pr-6">
          <a href={pr.url} target="_blank" rel="noreferrer" className="font-mono underline-offset-2 hover:underline">
            {run.repo}#{run.pr}
          </a>
          <StatusChip variant={status.variant}>{status.label}</StatusChip>
          {isTry && <StatusChip variant="ai">Try</StatusChip>}
          <PausedChip paused={pr} />
        </DialogTitle>
        <DialogDescription className="text-xs">
          {run.head_sha && (
            <a
              href={`https://github.com/${run.repo}/commit/${run.head_sha}`}
              target="_blank"
              rel="noreferrer"
              className="font-mono underline-offset-2 hover:underline"
            >
              {shortSHA(run.head_sha)}
            </a>
          )}
          {run.head_sha ? " · " : ""}
          {triggerLabel(run.trigger)}
          {run.requested_by ? ` by ${run.requested_by.replace(/^(console|github):/, "")}` : ""} ·{" "}
          <RelativeTime value={run.created_at} />
          {pr.author ? ` · pull request by ${pr.author}` : ""}
          {pr.state && pr.state !== "open" ? ` · ${pr.state}` : ""}
          {pr.is_fork ? " · from a fork" : ""}
        </DialogDescription>
      </DialogHeader>

      <div className="flex flex-wrap items-center gap-2">
        <Button asChild variant="outline" size="sm">
          <a href={links.pull_request} target="_blank" rel="noreferrer">
            <GitPullRequest /> Pull request
          </a>
        </Button>
        {links.review && (
          <Button asChild variant="outline" size="sm">
            <a href={links.review} target="_blank" rel="noreferrer">
              <MessageSquare /> Review on GitHub
            </a>
          </Button>
        )}
        {links.summary && (
          <Button asChild variant="outline" size="sm">
            <a href={links.summary} target="_blank" rel="noreferrer">
              <FileText /> Summary comment
            </a>
          </Button>
        )}
        <span className="flex-1" />
        {canManage && !isTry && (
          <>
            {/* Said, not left to a title: a disabled button takes no hover, and this dialog covers
                the page's plan strip. */}
            {refused && <span className="text-xs text-warning">{planRefusalLine(refused)}</span>}
            <Button
              variant="outline"
              size="sm"
              disabled={active || !!refused}
              onClick={() => void runAgain(false)}
            >
              <RotateCw /> Run again
            </Button>
            <Button
              variant="outline"
              size="sm"
              disabled={active || !!refused}
              onClick={() => onRerunWith({ repo: run.repo, pr: run.pr, rerunOf: run.id, types: typeKeys, post: run.post })}
            >
              <Shuffle /> Run with other types…
            </Button>
          </>
        )}
        {isTry && request.inline?.key && request.inline.key !== "draft" && (
          <ReviewsTabLink
            tab="types"
            params={{ type: request.inline.key }}
            className="inline-flex h-8 items-center gap-1.5 rounded-md border px-3 text-sm font-medium hover:bg-secondary"
          >
            <ListChecks className="size-4" /> Open the type
          </ReviewsTabLink>
        )}
      </div>

      {isTry && (
        <p className="rounded-lg border bg-muted/40 px-3 py-2 text-xs text-muted-foreground">
          A try of {request.inline?.name ? `“${request.inline.name}”` : "a review type"} as it stood in the editor, before it was
          saved. Recorded in shadow: nothing was posted, and the pull request&apos;s own review took no notice of it.
        </p>
      )}

      {/* Where the pull request stands now, not where it stood when this run was made: a pause is the
          pull request's, and Resume is the reason to say it here. */}
      {pr.paused && (
        <div className="flex flex-wrap items-center gap-x-3 gap-y-2 rounded-lg border border-warning/30 bg-warning-soft px-3 py-2">
          <p className="min-w-0 flex-1 text-xs text-foreground">
            {pausedWhy(pr)} {run.repo}#{run.pr} is not reviewed again by itself — on a push, or by the catch-up — until
            they are resumed; a review somebody asks for still runs.
          </p>
          {canManage && <ResumeButton repo={run.repo} pr={run.pr} onResumed={d.reload} />}
        </div>
      )}

      <Outcome detail={detail} />

      <Section title="Review types">
        <ul className="space-y-2">
          {detail.types.map((t) => (
            <li key={t.key} className="text-sm">
              <div className="flex flex-wrap items-center gap-2">
                <span className="font-medium">{nameOf(t.key)}</span>
                <span className="font-mono text-xs text-muted-foreground">
                  {t.key}
                  {t.version !== undefined ? (t.version > 0 ? ` · v${t.version}` : isTry ? " · unsaved" : " · as shipped") : ""}
                </span>
                {t.skipped && <StatusChip variant="warning">Not run: {t.skipped}</StatusChip>}
              </div>
              {t.summary && detail.types.length > 1 && (
                <Disclosure label="Its summary" className="mt-1 pl-1" contentClassName="pt-2">
                  <Markdown text={t.summary} className="text-muted-foreground" />
                </Disclosure>
              )}
            </li>
          ))}
          {detail.types.length === 0 && <li className="text-sm text-muted-foreground">None ran.</li>}
        </ul>
        <p className="mt-2 text-xs text-muted-foreground">
          {run.rule ? (
            <>
              Chosen by the branch rule <span className="font-mono">{run.rule}</span>.
            </>
          ) : request.types.length > 0 ? (
            "Named by whoever asked, in place of the branch rule's."
          ) : isTry ? (
            "The type being tried, and nothing else."
          ) : (
            "No branch rule is recorded for this run."
          )}
        </p>
      </Section>

      <Section title={`Findings (${detail.findings.length})`}>
        <RunFindings findings={detail.findings} nameOf={nameOf} active={active} />
      </Section>

      <section id="run-dropped" className="scroll-mt-4">
        <StickyDisclosure
          flags={sections}
          id="dropped"
          label="Dropped candidates"
          summary={detail.dropped.length ? `${detail.dropped.length} the review did not keep, and why` : "none"}
        >
          <DroppedList drops={detail.dropped} nameOf={nameOf} />
        </StickyDisclosure>
      </section>

      {(detail.not_reviewed.length > 0 || !detail.full_coverage) && !active && (
        <StickyDisclosure
          flags={sections}
          id="not-reviewed"
          label="Not reviewed"
          summary={
            detail.not_reviewed.length
              ? `${detail.not_reviewed.length} file${detail.not_reviewed.length === 1 ? "" : "s"}`
              : "every file was read"
          }
        >
          {detail.not_reviewed.length > 0 ? (
            <ul className="divide-y rounded-lg border text-xs">
              {detail.not_reviewed.map((f) => (
                <li key={f.path} className="flex flex-wrap items-baseline justify-between gap-x-3 gap-y-0.5 px-3 py-1.5">
                  <span className="min-w-0 break-all font-mono">{f.path}</span>
                  <span className="text-muted-foreground">{notReviewedReason(f.reason)}</span>
                </li>
              ))}
            </ul>
          ) : (
            <p className="text-xs text-muted-foreground">No file was left out, though the review did not see every line.</p>
          )}
        </StickyDisclosure>
      )}

      {(detail.context_repos.length > 0 || detail.context_notes.length > 0) && (
        <Section title="Context">
          {detail.context_repos.length > 0 && (
            <p className="text-sm">
              Read beside it:{" "}
              {detail.context_repos.map((r, i) => (
                <span key={r}>
                  {i > 0 && ", "}
                  <RepoLink repo={r} className="font-mono text-primary underline-offset-2 hover:underline" />
                </span>
              ))}
            </p>
          )}
          {detail.context_notes.map((n) => (
            <p key={n} className="mt-1 text-xs text-muted-foreground">
              {n}
            </p>
          ))}
        </Section>
      )}

      {(detail.skills?.length ?? 0) > 0 && (
        <Section title="Skills">
          <ul className="space-y-2">
            {detail.skills!.map((k) => {
              const left = (k.files ?? []).filter((f) => !(k.given ?? []).includes(f));
              return (
                <li key={`${k.repo}|${k.ref ?? ""}|${k.path}`} className="text-sm">
                  <div className="flex flex-wrap items-baseline gap-x-2 gap-y-0.5">
                    <span className="font-medium">{k.name}</span>
                    {k.sha ? (
                      <a
                        href={`https://github.com/${k.repo}/tree/${k.sha}/${k.path}`}
                        target="_blank"
                        rel="noreferrer"
                        className="inline-flex items-center gap-0.5 font-mono text-xs text-primary underline-offset-2 hover:underline"
                      >
                        {k.here ? k.path : `${k.repo}:${k.path}`} @ {k.sha.slice(0, 7)} <ExternalLink className="size-3" />
                      </a>
                    ) : (
                      <span className="font-mono text-xs text-muted-foreground">{k.here ? k.path : `${k.repo}:${k.path}`}</span>
                    )}
                    <span className="text-xs text-muted-foreground">
                      {k.here ? "this repository, at the pull request's base" : k.public ? "public, read without credentials" : "a connected repository"}
                      {" · for "}
                      {k.types.map(nameOf).join(", ")}
                    </span>
                  </div>
                  {k.error ? (
                    <p className="text-xs text-danger">Not read: {k.error}</p>
                  ) : (
                    <p className="text-xs text-muted-foreground">
                      Given to the finder: <span className="font-mono">{(k.given ?? []).join(", ") || "nothing"}</span>
                      {left.length > 0 && (
                        <>
                          {" · left out for length: "}
                          <span className="font-mono">{left.join(", ")}</span>
                        </>
                      )}
                    </p>
                  )}
                  {k.note && <p className="text-xs text-warning">{k.note}</p>}
                  {(k.omitted?.length ?? 0) > 0 && <p className="text-xs text-muted-foreground">Not read: {k.omitted!.join("; ")}</p>}
                </li>
              );
            })}
          </ul>
        </Section>
      )}

      <Section title="Run">
        <dl className="grid gap-x-6 gap-y-1.5 text-sm sm:grid-cols-[9rem_1fr]">
          <dt className="text-muted-foreground">Cost</dt>
          <dd className="tabular-nums">
            {formatUSD(detail.usage.cost_usd)}
            {detail.usage.reserved_usd > 0 && active ? ` · ${usd(detail.usage.reserved_usd)} held while it runs` : ""}
          </dd>
          <dt className="text-muted-foreground">Tokens</dt>
          <dd className="tabular-nums">
            {formatNumber(detail.usage.tokens_in)} in ({formatNumber(detail.usage.tokens_cached)} cached) /{" "}
            {formatNumber(detail.usage.tokens_out)} out
          </dd>
          <dt className="text-muted-foreground">Model</dt>
          <dd className="truncate font-mono text-xs">{detail.usage.model || "—"}</dd>
          <dt className="text-muted-foreground">Read</dt>
          <dd>
            {run.files_reviewed} file{run.files_reviewed === 1 ? "" : "s"} · {run.candidates} candidate
            {run.candidates === 1 ? "" : "s"} → {run.findings} kept,{" "}
            {run.dropped > 0 ? (
              <button type="button" onClick={openDropped} className="text-primary underline-offset-2 hover:underline">
                {run.dropped} dropped
              </button>
            ) : (
              "none dropped"
            )}
            {!detail.full_coverage && !active && run.status !== "skipped" ? " · not every line was read" : ""}
            {detail.injection ? " · text in the diff tried to steer the reviewer" : ""}
          </dd>
          <dt className="text-muted-foreground">Timing</dt>
          <dd>
            queued {formatDateTime(detail.timings.created_at)}
            {detail.timings.started_at ? ` · started ${formatDateTime(detail.timings.started_at)}` : ""}
            {detail.timings.finished_at ? ` · finished ${formatDateTime(detail.timings.finished_at)}` : ""}
            {detail.timings.duration_ms != null ? ` · ${formatDuration(detail.timings.duration_ms)}` : ""}
          </dd>
          <dt className="text-muted-foreground">Asked for</dt>
          <dd>
            {triggerLabel(request.trigger)}
            {request.requested_by ? ` by ${request.requested_by.replace(/^(console|github):/, "")}` : ""}
            {" · "}
            {request.post === "live" ? "post live" : request.post === "shadow" ? "record in shadow" : "where the settings say"}
            {request.scope === "since_last" ? " · changes since the last review" : request.scope === "whole" ? " · the whole pull request" : ""}
            {request.types.length > 0 ? ` · types ${request.types.map(nameOf).join(", ")}` : ""}
            {request.full ? " · from scratch" : ""}
          </dd>
          <dt className="text-muted-foreground">Commits</dt>
          <dd className="font-mono text-xs">
            {shortSHA(run.base_sha) || "?"} → {shortSHA(run.head_sha) || "?"}
            {pr.head_sha && pr.head_sha !== run.head_sha ? (
              <span className="font-sans text-muted-foreground"> · the pull request is at {shortSHA(pr.head_sha)} now</span>
            ) : null}
          </dd>
        </dl>
      </Section>
      {confirmDialog}
    </div>
  );
}

/** What the run came to: its score and summary, or why it ended without one. */
function Outcome({ detail }: { detail: ReviewRunDetail }) {
  const { run, summary, risk } = detail;
  if (isRunActive(run.status)) {
    return (
      <div className="rounded-lg border bg-muted/40 px-3 py-2.5 text-sm text-muted-foreground">
        {run.status === "queued" ? "Waiting in the lane for its turn." : "Reviewing now: this page follows it."}
      </div>
    );
  }
  const failed = run.status === "failed";
  return (
    <div className="space-y-3">
      <div className="flex flex-wrap items-center gap-2">
        {run.score !== null && run.score !== undefined && (
          <StatusChip variant={scoreVariant(run.score)}>Confidence {run.score}/5</StatusChip>
        )}
        {run.post && (
          <StatusChip variant={run.post === "live" ? "success" : "info"}>{run.post === "live" ? "Live" : "Shadow"}</StatusChip>
        )}
        {risk && <span className="text-sm font-medium">{risk}</span>}
      </div>
      {run.error && (
        <div
          className={cn(
            "rounded-lg border px-3 py-2 text-sm",
            failed ? "border-danger/30 bg-danger-soft" : "bg-muted/40 text-muted-foreground",
          )}
        >
          {run.error}
        </div>
      )}
      {summary && <Markdown text={summary} />}
    </div>
  );
}

/**
 * A run's findings, grouped by where each stands now — open first, since that is what still needs
 * somebody — each with its severity, the types that raised it, where it is and where it was said.
 */
export function RunFindings({
  findings,
  nameOf,
  active,
}: {
  findings: ReviewFinding[];
  nameOf: (key: string) => string;
  active?: boolean;
}) {
  if (findings.length === 0) {
    return (
      <p className="text-sm text-muted-foreground">
        {active ? "None yet: findings appear once the review is checked and kept." : "Nothing the review stood behind."}
      </p>
    );
  }
  const known = new Set(FINDING_STATUSES.map((s) => s.value));
  const groups = [
    ...FINDING_STATUSES.map((s) => ({ ...s, items: findings.filter((f) => f.status === s.value) })),
    { ...findingStatus("other"), label: "Other", items: findings.filter((f) => !known.has(f.status)) },
  ].filter((g) => g.items.length > 0);
  return (
    <div className="space-y-4">
      {groups.map((g) => (
        <div key={g.value} className="space-y-2">
          <h4 className="flex items-baseline gap-2 text-xs font-medium tracking-wide text-muted-foreground uppercase">
            {g.label} <span className="tabular-nums">{g.items.length}</span>
            <span className="font-normal tracking-normal normal-case">{g.hint}</span>
          </h4>
          <ul className="space-y-2">
            {g.items.map((f) => (
              <FindingCard key={f.id} f={f} nameOf={nameOf} />
            ))}
          </ul>
        </div>
      ))}
    </div>
  );
}

function FindingCard({ f, nameOf }: { f: ReviewFinding; nameOf: (key: string) => string }) {
  const st = findingStatus(f.status);
  return (
    <li className="space-y-2 rounded-lg border bg-card px-3 py-2.5">
      <div className="flex flex-wrap items-center gap-1.5">
        <StatusChip variant={severityVariant(f.severity)}>{f.severity || "P2"}</StatusChip>
        {f.kind === "note" && <StatusChip variant="neutral">Note</StatusChip>}
        {f.pre_existing && <StatusChip variant="neutral">Pre-existing</StatusChip>}
        {f.types.map((k) => (
          <span key={k} className="rounded-sm bg-secondary px-1.5 py-0.5 text-xs text-secondary-foreground">
            {nameOf(k)}
          </span>
        ))}
        <span className="flex-1" />
        {f.status !== "open" && <StatusChip variant={st.variant}>{st.label}</StatusChip>}
      </div>
      <p className="text-sm font-medium">{f.title}</p>
      <p className="text-xs text-muted-foreground">
        <span className="break-all font-mono">{findingWhere(f)}</span>
        {f.rule_ids.length > 0 ? ` · cites ${f.rule_ids.join(", ")}` : ""} · {placementLabel(f.placement, f.place)}
        {f.possibly_outdated ? " · its file changed before it was posted" : ""}
        {f.comment_url && (
          <>
            {" · "}
            <a href={f.comment_url} target="_blank" rel="noreferrer" className="inline-flex items-center gap-0.5 text-primary underline-offset-2 hover:underline">
              comment <ExternalLink className="size-3" />
            </a>
          </>
        )}
      </p>
      {f.body && <Markdown text={f.body} className="text-muted-foreground" />}
      {f.status !== "open" && (f.status_reason || f.status_by) && (
        <p className="text-xs text-muted-foreground">
          {st.label}
          {f.status_by ? ` by ${f.status_by}` : ""}
          {f.status_reason ? `: ${f.status_reason}` : ""}
        </p>
      )}
      {(f.suggestion?.code || f.evidence.length > 0 || f.history.length > 1) && (
        <div className="flex flex-col gap-1.5">
          {f.suggestion?.code && (
            <Disclosure label="Suggested change" contentClassName="pt-1.5">
              <pre className="max-h-48 overflow-auto rounded-md border bg-background p-2.5 font-mono text-xs leading-relaxed whitespace-pre-wrap break-words">
                {f.suggestion.code}
              </pre>
            </Disclosure>
          )}
          {f.evidence.length > 0 && (
            <Disclosure label="Evidence" summary={String(f.evidence.length)} contentClassName="space-y-1.5 pt-1.5">
              {f.evidence.map((e, i) => (
                <div key={i} className="space-y-1">
                  <p className="font-mono text-xs text-muted-foreground">
                    {e.repo ? `${e.repo} · ` : ""}
                    {e.path}:{e.start_line}
                    {e.end_line ? `–${e.end_line}` : ""}
                  </p>
                  <pre className="max-h-40 overflow-auto rounded-md border bg-background p-2.5 font-mono text-xs leading-relaxed whitespace-pre-wrap break-words">
                    {e.quote}
                  </pre>
                </div>
              ))}
            </Disclosure>
          )}
          {f.history.length > 1 && (
            <Disclosure label="Its thread" summary={`${f.history.length - 1} since it was raised`} contentClassName="pt-1.5">
              <ol className="space-y-1 text-xs">
                {f.history.map((h, i) => (
                  <li key={i} className="flex flex-wrap gap-x-2">
                    <span className="text-muted-foreground tabular-nums">{formatDateTime(h.at)}</span>
                    <span>
                      {h.what === "raised"
                        ? "Raised"
                        : h.what === "reply"
                          ? `Reply from ${h.by.replace(/^github:/, "")}${h.outcome ? `: ${h.outcome}` : ""}${h.verdict ? ` (${h.verdict})` : ""}`
                          : `${findingStatus(h.status ?? "").label}${h.by ? ` by ${h.by}` : ""}${h.reason ? `: ${h.reason}` : ""}`}
                    </span>
                  </li>
                ))}
              </ol>
            </Disclosure>
          )}
        </div>
      )}
    </li>
  );
}

function DroppedList({ drops, nameOf }: { drops: ReviewDrop[]; nameOf: (key: string) => string }) {
  if (drops.length === 0) return <p className="text-xs text-muted-foreground">Every candidate was kept.</p>;
  return (
    <ul className="divide-y rounded-lg border">
      {drops.map((x, i) => (
        <li key={i} className="space-y-0.5 px-3 py-2 text-sm">
          <div className="flex flex-wrap items-center gap-1.5">
            <StatusChip variant="neutral">{dropReason(x.reason)}</StatusChip>
            {x.severity && <StatusChip variant={severityVariant(x.severity)}>{x.severity}</StatusChip>}
            {x.type && <span className="text-xs text-muted-foreground">{nameOf(x.type)}</span>}
            {x.confidence ? <span className="text-xs text-muted-foreground tabular-nums">verifier {x.confidence}%</span> : null}
          </div>
          {x.title && <p className="font-medium">{x.title}</p>}
          {x.path && <p className="break-all font-mono text-xs text-muted-foreground">{findingWhere({ path: x.path, line: x.line ?? 0 })}</p>}
          {x.detail && <p className="text-xs text-muted-foreground">{x.detail}</p>}
        </li>
      ))}
    </ul>
  );
}

/** A Disclosure whose open or closed is remembered in this browser, as every fold in the console is. */
function StickyDisclosure({
  flags,
  id,
  label,
  summary,
  children,
}: {
  flags: ReturnType<typeof useStickyFlags>;
  id: string;
  label: string;
  summary: string;
  children: React.ReactNode;
}) {
  const open = flags.get(id, false);
  return (
    <Disclosure label={label} summary={summary} open={open} onOpenChange={(o) => flags.set(id, o)}>
      {children}
    </Disclosure>
  );
}

function Section({ title, children }: { title: string; children: React.ReactNode }) {
  return (
    <section>
      <h3 className="mb-2 text-sm font-semibold">{title}</h3>
      {children}
    </section>
  );
}
