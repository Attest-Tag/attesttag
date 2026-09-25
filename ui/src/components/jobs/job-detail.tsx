"use client";

import { useEffect } from "react";
import { Cloud, Download, ExternalLink, GitPullRequest, MessageSquare } from "lucide-react";
import { Disclosure } from "@/components/core/disclosure";
import { ErrorBanner } from "@/components/core/error-banner";
import { StatusChip } from "@/components/core/status-chip";
import {
  JOB_PHASES,
  checkedPackages,
  describeRecipe,
  formatJobCost,
  formatJobDuration,
  jobBranchShape,
  jobConsoleLabel,
  jobStatusVariant,
  phaseGlyph,
  recipeSourceLabel,
} from "@/components/jobs/job-format";
import { BranchLink, RepoLink } from "@/components/jobs/repo-link";
import { Badge } from "@/components/ui/badge";
import { Button } from "@/components/ui/button";
import {
  Dialog,
  DialogContent,
  DialogDescription,
  DialogHeader,
  DialogTitle,
} from "@/components/ui/dialog";
import { Skeleton } from "@/components/ui/skeleton";
import {
  isJobActive,
  useApi,
  type JobCheck,
  type JobDetail,
  type JobEvent,
  type JobPackage,
  type JobResult,
  type JobTestRun,
} from "@/lib/api";
import { formatBytes, formatDateTime, formatNumber } from "@/lib/format";

export function JobDetailDialog({
  id,
  onOpenChange,
}: {
  id: number | null;
  onOpenChange: (open: boolean) => void;
}) {
  return (
    <Dialog open={id !== null} onOpenChange={onOpenChange}>
      <DialogContent className="max-h-[85vh] overflow-y-auto sm:max-w-3xl">
        {id !== null && <JobDetailBody key={id} id={id} />}
      </DialogContent>
    </Dialog>
  );
}

// The header links keep the description's muted colour; the underline on hover is
// enough of a signal at this size, and three coloured links would shout over the title.
const headerLink = "underline-offset-2 hover:underline";

function JobDetailBody({ id }: { id: number }) {
  const d = useApi<JobDetail>(`/api/jobs/${id}`);
  const active = !!d.data && isJobActive(d.data.job.status);
  const reload = d.reload;
  useEffect(() => {
    if (!active) return;
    const t = setInterval(reload, 10_000);
    return () => clearInterval(t);
  }, [active, reload]);

  if (d.error && !d.data) {
    return (
      <>
        <DialogHeader>
          <DialogTitle>Job #{id}</DialogTitle>
        </DialogHeader>
        <ErrorBanner message={d.error} onRetry={d.reload} />
      </>
    );
  }
  if (!d.data) {
    return (
      <>
        <DialogHeader>
          <DialogTitle className="sr-only">Loading job #{id}</DialogTitle>
        </DialogHeader>
        <div className="space-y-3">
          <Skeleton className="h-6 w-2/3" />
          <Skeleton className="h-4 w-1/2" />
          <Skeleton className="h-24 w-full" />
        </div>
      </>
    );
  }

  const { job, spec, result, events, diff_bytes, diff_link } = d.data;
  const phases = new Map<string, string>();
  for (const e of events ?? []) {
    if (e.kind === "phase" && e.phase && e.status) phases.set(e.phase, e.status);
  }
  const approval =
    job.approval === "confirm"
      ? `Confirmed by ${job.approved_by_name || job.approved_by || "someone"}`
      : job.approval.startsWith("rule:")
        ? `Allow rule: ${job.approval.slice(5)}`
        : "—";
  // A job that opened a pull request after the engine hit its turn or spend cap is a success
  // with a caveat. Newer workers send it as result.note; jobs finished before that carry it as
  // an "engine_error … stopped early" error, which is read the same way rather than shown red.
  const stoppedEarly =
    job.status === "succeeded" &&
    /stopped early \((max_rounds|budget)\)/.test(result?.error?.message || job.error || "");
  const note =
    result?.note ||
    (stoppedEarly
      ? "The coding agent reached its turn or spend cap before finishing on its own; what it had changed was committed and pushed, so check the pull request for loose ends"
      : "");

  return (
    <div className="space-y-5">
      <DialogHeader>
        <DialogTitle className="flex flex-wrap items-center gap-2">
          {job.pr_url ? (
            <a
              href={job.pr_url}
              target="_blank"
              rel="noreferrer"
              className="underline-offset-2 hover:underline"
            >
              #{job.id} {job.title || "(untitled)"}
            </a>
          ) : (
            <span>
              #{job.id} {job.title || "(untitled)"}
            </span>
          )}
          <StatusChip variant={jobStatusVariant(job.status)}>{job.status}</StatusChip>
        </DialogTitle>
        <DialogDescription className="font-mono text-xs">
          <RepoLink repo={job.repo} className={headerLink} />
          {" · "}
          {job.branch && (
            <>
              <BranchLink repo={job.repo} branch={job.branch} className={headerLink} /> →{" "}
            </>
          )}
          <BranchLink repo={job.repo} branch={job.base_branch} className={headerLink} />
        </DialogDescription>
      </DialogHeader>

      <div className="flex flex-wrap gap-2">
        {job.thread_link && (
          <Button asChild variant="outline" size="sm">
            <a href={job.thread_link} target="_blank" rel="noreferrer">
              <MessageSquare /> Slack thread
            </a>
          </Button>
        )}
        {job.pr_url && (
          <Button asChild variant="outline" size="sm">
            <a href={job.pr_url} target="_blank" rel="noreferrer">
              <GitPullRequest /> Pull request
              {result?.pr?.draft && <Badge variant="secondary">draft</Badge>}
            </a>
          </Button>
        )}
        {job.console_url && (
          <Button asChild variant="outline" size="sm">
            <a href={job.console_url} target="_blank" rel="noreferrer">
              <Cloud /> {jobConsoleLabel(job.dispatcher)}
            </a>
          </Button>
        )}
        {diff_bytes > 0 && (
          <Button asChild variant="outline" size="sm">
            <a href={`/api/jobs/${job.id}/diff`} download>
              <Download /> Diff ({formatBytes(diff_bytes)})
            </a>
          </Button>
        )}
        {diff_link && (
          <Button asChild variant="outline" size="sm">
            <a href={diff_link} target="_blank" rel="noreferrer">
              <ExternalLink /> Diff in Slack
            </a>
          </Button>
        )}
        {result?.log_tail && (
          <Button asChild variant="outline" size="sm">
            <a href={`/api/jobs/${job.id}/log`} download>
              <Download /> Log
            </a>
          </Button>
        )}
      </div>

      <Section title="Progress">
        <ol className="flex flex-wrap gap-x-4 gap-y-1 text-sm">
          {JOB_PHASES.map(([key, label]) => {
            const g = phaseGlyph(phases.get(key));
            return (
              <li key={key} className="flex items-center gap-1.5" title={`${label}: ${g.label}`}>
                <span className={g.className}>{g.glyph}</span>
                <span>{label}</span>
              </li>
            );
          })}
        </ol>
        {active && (
          <p className="mt-2 text-xs text-muted-foreground">
            {job.phase ? `Current phase: ${job.phase.replace("_", " ")}` : "Waiting for the worker"}
            {job.last_event_at ? ` · last word ${formatDateTime(job.last_event_at)}` : ""}
            {job.cancel_requested ? " · cancel requested" : ""}
          </p>
        )}
      </Section>

      {(job.error || result?.error?.code) && !stoppedEarly && (
        <div className="rounded-lg border border-danger/30 bg-danger-soft p-3 text-sm">
          {result?.error?.code && <span className="mr-2 font-mono text-xs">{result.error.code}</span>}
          {result?.error?.message || job.error}
        </div>
      )}

      {note && (
        <div className="rounded-lg border border-warning/30 bg-warning-soft p-3 text-sm">{note}.</div>
      )}

      {result?.summary && (
        <Section title="Summary">
          <p className="whitespace-pre-wrap text-sm">{result.summary}</p>
        </Section>
      )}

      {spec && (
        <Section title="Brief">
          <p className="whitespace-pre-wrap text-sm">{spec.requirement}</p>
          {spec.acceptance && spec.acceptance.length > 0 && (
            <ul className="mt-2 list-disc space-y-0.5 pl-5 text-sm">
              {spec.acceptance.map((a, i) => (
                <li key={i}>{a}</li>
              ))}
            </ul>
          )}
          {spec.ticket && (
            <p className="mt-2 text-xs text-muted-foreground">
              Ticket:{" "}
              {spec.ticket.startsWith("https://") ? (
                <a href={spec.ticket} target="_blank" rel="noreferrer" className="underline">
                  {spec.ticket}
                </a>
              ) : (
                <span className="font-mono">{spec.ticket}</span>
              )}
            </p>
          )}
          {spec.files_hint && spec.files_hint.length > 0 && (
            <p className="mt-2 font-mono text-xs text-muted-foreground">Files: {spec.files_hint.join(", ")}</p>
          )}
          {spec.evidence && (
            <Disclosure label="Evidence" className="mt-3">
              <pre className="max-h-64 overflow-auto rounded-lg border bg-background p-3 font-mono text-xs leading-relaxed whitespace-pre-wrap break-words">
                {spec.evidence}
              </pre>
            </Disclosure>
          )}
          <p className="mt-3 font-mono text-xs text-muted-foreground">
            {spec.constraints.engine}/{spec.constraints.model} · budget {formatJobCost(spec.constraints.budget_usd)} ·{" "}
            {Math.round(spec.constraints.timeout_s / 60)} min · branch rule {jobBranchShape(spec.constraints.branch_prefix, spec.constraints.branch_suffix, "*")}
            {spec.constraints.test_cmd ? ` · tests: ${spec.constraints.test_cmd}` : ""}
          </p>
        </Section>
      )}

      {result && <Checks result={result} />}

      {result && (result.diff_stat.files > 0 || (result.files_changed?.length ?? 0) > 0) && (
        <Section title="Diff">
          <p className="text-sm">
            {result.diff_stat.files} files, +{result.diff_stat.insertions} −{result.diff_stat.deletions}
          </p>
          {result.files_changed && result.files_changed.length > 0 && (
            <Disclosure label="Files" summary={`${result.files_changed.length}`} className="mt-2">
              <ul className="font-mono text-xs">
                {result.files_changed.map((f) => (
                  <li key={f}>{f}</li>
                ))}
              </ul>
            </Disclosure>
          )}
        </Section>
      )}

      <Section title="Run">
        <dl className="grid gap-x-6 gap-y-1.5 text-sm sm:grid-cols-[9rem_1fr]">
          <dt className="text-muted-foreground">Cost</dt>
          <dd className="tabular-nums">
            {formatJobCost(job.cost_usd)} of {formatJobCost(job.budget_usd)} budget ·{" "}
            {formatNumber(job.tokens_in)} in / {formatNumber(job.tokens_out)} out tokens
          </dd>
          <dt className="text-muted-foreground">Coding agent</dt>
          <dd className="font-mono text-xs">
            {job.engine} · {job.model}
          </dd>
          <dt className="text-muted-foreground">Approval</dt>
          <dd>{approval}</dd>
          <dt className="text-muted-foreground">Dispatcher</dt>
          <dd className="truncate font-mono text-xs">
            {job.dispatcher}
            {job.execution_ref ? ` · ${job.execution_ref}` : ""}
          </dd>
          {job.worker_info && (
            <>
              <dt className="text-muted-foreground">Worker</dt>
              <dd className="truncate font-mono text-xs">
                {job.worker_info} · {job.claim_count} claim{job.claim_count === 1 ? "" : "s"}
              </dd>
            </>
          )}
          <dt className="text-muted-foreground">Timing</dt>
          <dd>
            created {formatDateTime(job.created_at)}
            {job.started_at ? ` · started ${formatDateTime(job.started_at)}` : ""}
            {job.finished_at ? ` · finished ${formatDateTime(job.finished_at)}` : ""} ·{" "}
            {formatJobDuration(job.duration_s)}
          </dd>
          {job.cancel_requested && (
            <>
              <dt className="text-muted-foreground">Cancel</dt>
              <dd>
                {job.cancel_reason || "requested"}
                {job.cancel_by ? ` by ${job.cancel_by}` : ""}
                {job.cancel_requested_at ? ` at ${formatDateTime(job.cancel_requested_at)}` : ""}
              </dd>
            </>
          )}
        </dl>
      </Section>

      <Section title="Events">
        {events && events.length > 0 ? (
          <ol className="max-h-72 divide-y overflow-auto rounded-lg border font-mono text-xs">
            {events.map((e) => (
              <EventRow key={e.id ?? e.seq} e={e} />
            ))}
          </ol>
        ) : (
          <p className="text-xs text-muted-foreground">No events yet.</p>
        )}
      </Section>
    </div>
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

/**
 * What the job checked, package by package: the primary first, then each other package the
 * brief pointed at, then the packages the change touched that nothing checked, so their silence
 * is not read as a pass.
 */
function Checks({ result }: { result: JobResult }) {
  const packages = checkedPackages(result);
  const several = packages.length > 1;
  const unchecked = result.unchecked ?? [];
  return (
    <Section title="Checks">
      <div className="space-y-5">
        {packages.map((p, i) => (
          <PackageChecks key={`${i}:${p.workdir}`} pkg={p} several={several} />
        ))}
        {unchecked.length > 0 && (
          <p className="text-sm">
            Also changed, not checked:{" "}
            {unchecked.map((dir, i) => (
              <span key={`${i}:${dir}`}>
                {i > 0 && ", "}
                <DirName dir={dir} />
              </span>
            ))}
          </p>
        )}
      </div>
    </Section>
  );
}

/** A gate with something to show: a command, or a run. */
const hasCheck = (c: JobCheck) => !!c.command || c.before.ran || c.after.ran;

function PackageChecks({ pkg: p, several }: { pkg: JobPackage; several: boolean }) {
  const setup = p.setup?.ran ? p.setup : null;
  const build = hasCheck(p.build) ? p.build : null;
  const lint = p.lint?.after.ran ? p.lint : null;
  // Rows name their gate once there is more to show than the one suite: an install, a build, a
  // linter, another package. A row written before those were steps of their own has only the
  // suite, and reads the way the Tests section always did.
  const named = several || !!setup || !!build || !!lint;
  // The recipe line already gives the reason nothing could run; it is not said twice.
  const testsSkipped = p.tests.skipped && p.tests.skipped !== p.recipe?.why ? p.tests.skipped : "";
  return (
    <div className="space-y-2">
      {several && (
        <h4 className="text-sm font-medium">
          <DirName dir={p.workdir} />
          {p.recipe?.ecosystem && (
            <span className="font-normal text-muted-foreground"> · {p.recipe.ecosystem}</span>
          )}
        </h4>
      )}
      {p.recipe && (
        <p className="text-xs text-muted-foreground">
          {/* Commands read best in mono; a recipe with none is a sentence saying why. */}
          <span className={p.recipe.build || p.recipe.lint || p.recipe.test ? "font-mono" : undefined}>
            {describeRecipe(p.recipe)}
          </span>{" "}
          — {recipeSourceLabel(p.recipe.source)}
        </p>
      )}
      {p.note && (
        <div className="rounded-lg border border-warning/30 bg-warning-soft p-3 text-sm">{p.note}.</div>
      )}
      {p.skipped && <p className="text-xs text-muted-foreground">Not checked: {p.skipped}.</p>}
      {named ? (
        <>
          {setup && (
            <RunRow label="Install" run={setup} okLabel="ok" wide>
              {setup.command && <span className="font-mono">{setup.command}</span>}
            </RunRow>
          )}
          {!p.skipped && (
            <>
              {build && (
                <>
                  <RunRow label="Build before" run={build.before} wide />
                  <RunRow label="Build after" run={build.after} wide />
                </>
              )}
              {hasCheck(p.tests) ? (
                <>
                  <RunRow label="Tests before" run={p.tests.before} counts wide />
                  <RunRow label="Tests after" run={p.tests.after} counts wide />
                </>
              ) : (
                <RunRow label="Tests" run={p.tests.after} wide>
                  {testsSkipped || "no test suite detected"}
                </RunRow>
              )}
              {lint && <RunRow label="Lint after" run={lint.after} wide />}
            </>
          )}
        </>
      ) : (
        <div>
          {!p.recipe && (
            <p className="font-mono text-xs text-muted-foreground">{p.tests.command || "no test suite detected"}</p>
          )}
          {testsSkipped && <p className="mt-1 text-xs text-muted-foreground first:mt-0">Not run: {testsSkipped}.</p>}
          <div className="mt-2 space-y-2 first:mt-0">
            <RunRow label="Before" run={p.tests.before} counts />
            <RunRow label="After" run={p.tests.after} counts />
          </div>
        </div>
      )}
    </div>
  );
}

/** A package directory as the report names it: the root in words, anything else as its path. */
function DirName({ dir }: { dir: string }) {
  if (!dir || dir === ".") return <>repository root</>;
  return <span className="font-mono">{dir}</span>;
}

function RunRow({
  label,
  run,
  counts = false,
  wide = false,
  okLabel = "passed",
  children,
}: {
  label: string;
  run: JobTestRun;
  /** A suite says how many passed and failed; a build, a linter or an install has no count to give. */
  counts?: boolean;
  /** Room for a label that names the gate as well as when it ran: "Tests before". */
  wide?: boolean;
  /** The chip's word for a run that went well. */
  okLabel?: string;
  /** Quieter detail after the result: the command an install ran, why a suite did not run. */
  children?: React.ReactNode;
}) {
  const chip = !run.ran ? (
    <StatusChip variant="neutral">not run</StatusChip>
  ) : run.ok ? (
    <StatusChip variant="success">{okLabel}</StatusChip>
  ) : (
    <StatusChip variant="danger">failed</StatusChip>
  );
  const facts = run.ran
    ? [counts ? `${run.passed ?? 0} passed, ${run.failed ?? 0} failed` : "", run.seconds ? `${run.seconds.toFixed(1)} s` : ""]
        .filter(Boolean)
        .join(" · ")
    : "";
  return (
    <div>
      <div className="flex flex-wrap items-center gap-2 text-sm">
        <span className={wide ? "w-28 text-muted-foreground" : "w-14 text-muted-foreground"}>{label}</span>
        {chip}
        {(facts || children) && (
          <span className="text-xs text-muted-foreground">
            {facts}
            {facts && children && " · "}
            {children}
          </span>
        )}
      </div>
      {run.output && (
        <Disclosure label="Output" className={wide ? "mt-1 pl-30" : "mt-1 pl-16"}>
          <pre className="max-h-48 overflow-auto rounded-lg border bg-background p-3 font-mono text-xs leading-relaxed whitespace-pre-wrap break-words">
            {run.output}
          </pre>
        </Disclosure>
      )}
    </div>
  );
}

function EventRow({ e }: { e: JobEvent }) {
  const kind = e.kind === "phase" ? `${e.phase ?? ""}:${e.status ?? ""}` : e.kind;
  return (
    <li className="grid grid-cols-[3rem_7rem_7rem_1fr] gap-2 px-3 py-1.5">
      <span className="text-muted-foreground">{e.seq}</span>
      <span className="text-muted-foreground">{formatDateTime(e.at || e.created_at || "")}</span>
      <span>
        <Badge variant="outline" className={e.kind === "warn" ? "border-danger/40 text-danger" : ""}>
          {kind}
        </Badge>
      </span>
      <span className="whitespace-pre-wrap break-words">
        {e.message}
        {e.usage && (
          <span className="text-muted-foreground">
            {e.message ? " · " : ""}
            {formatJobCost(e.usage.cost_usd)} · {formatNumber(e.usage.in)} in / {formatNumber(e.usage.out)} out
          </span>
        )}
      </span>
    </li>
  );
}
