"use client";

import { useEffect, useState } from "react";
import { ChevronLeft, ChevronRight, GitPullRequest, History } from "lucide-react";
import { EmptyState } from "@/components/core/empty-state";
import { ErrorBanner } from "@/components/core/error-banner";
import { RelativeTime } from "@/components/core/relative-time";
import { StatusChip } from "@/components/core/status-chip";
import { TableSkeleton } from "@/components/core/table-skeleton";
import { reposOf } from "@/components/reviews/review-format";
import { ReviewRunDialog, useTypeNames } from "@/components/reviews/review-run-detail";
import {
  RUN_STATUSES,
  isRunActive,
  runCost,
  runStatus,
  scoreVariant,
  shortSHA,
  triggerLabel,
} from "@/components/reviews/review-run-format";
import { StartReviewDialog, type StartReviewTarget } from "@/components/reviews/start-review-dialog";
import { useAuth } from "@/components/shell/auth-provider";
import { Button } from "@/components/ui/button";
import { Select, SelectContent, SelectItem, SelectTrigger, SelectValue } from "@/components/ui/select";
import { Table, TableBody, TableCell, TableHead, TableHeader, TableRow } from "@/components/ui/table";
import { useApi, type ReviewRunSummary, type ReviewRunsPage, type ReviewSettingsTree } from "@/lib/api";
import { cn } from "@/lib/utils";

// Reviews › History: every review and try, newest first, from GET /api/reviews — a page at a time by
// its cursor, narrowed by repository and status — and one run's detail over it (review-run-detail),
// with Start review for somebody who may start one. The filters and the open run ride on the
// address (?repo=, ?status=, ?run=), which is how Settings links a repository's reviews and one run
// here, and how a reload or a shared link comes back to the same rows.

const ALL = "__all";
const PAGE = 25;

export function ReviewHistoryTab() {
  const { me } = useAuth();
  const perms = me?.user?.permissions;
  const canManage = !perms || perms["reviews.manage"] === true;

  const [repo, setRepo] = useState("");
  const [status, setStatus] = useState("");
  // The cursors of the pages before this one, newest first: Older pushes, Newer pops.
  const [cursors, setCursors] = useState<string[]>([""]);
  const [open, setOpen] = useState<string | null>(null);
  const [start, setStart] = useState<StartReviewTarget | null>(null);
  const [ready, setReady] = useState(false);

  // The address names the filters and the run when a link brought somebody here.
  useEffect(() => {
    const p = new URLSearchParams(window.location.search);
    /* eslint-disable react-hooks/set-state-in-effect -- one-shot URL read on mount */
    setRepo(p.get("repo") ?? "");
    setStatus(p.get("status") ?? "");
    setOpen(p.get("run"));
    setReady(true);
    /* eslint-enable react-hooks/set-state-in-effect */
  }, []);

  // …and follows them, so a reload comes back to the same rows and the same run.
  useEffect(() => {
    if (!ready) return;
    const url = new URL(window.location.href);
    const put = (k: string, v: string | null) => (v ? url.searchParams.set(k, v) : url.searchParams.delete(k));
    put("repo", repo);
    put("status", status);
    put("run", open);
    window.history.replaceState(null, "", url);
  }, [ready, repo, status, open]);

  const cursor = cursors[cursors.length - 1];
  const qs = new URLSearchParams({ limit: String(PAGE) });
  if (repo) qs.set("repo", repo);
  if (status) qs.set("status", status);
  if (cursor) qs.set("cursor", cursor);
  const runs = useApi<ReviewRunsPage>(ready ? `/api/reviews?${qs}` : null);
  const tree = useApi<ReviewSettingsTree>("/api/review-settings");
  const nameOf = useTypeNames();

  // Poll while anything on the page is still in the lane: a review takes a minute or three.
  const anyActive = (runs.data?.runs ?? []).some((r) => isRunActive(r.status));
  const reload = runs.reload;
  useEffect(() => {
    if (!anyActive) return;
    const t = setInterval(reload, 8_000);
    return () => clearInterval(t);
  }, [anyActive, reload]);

  // A filter starts the list again from its newest row.
  const filter = (set: (v: string) => void) => (v: string) => {
    set(v === ALL ? "" : v);
    setCursors([""]);
  };

  // What the repository filter offers: every repository the settings tree knows, and whichever one
  // a link named that it does not — a repository whose connection was stopped still has history.
  const repoOptions = [
    ...new Set([
      ...(tree.data?.connections.flatMap((c) => reposOf(c).map((r) => r.repo ?? "")) ?? []),
      ...(repo ? [repo.toLowerCase()] : []),
    ]),
  ]
    .filter(Boolean)
    .sort();

  const started = (run: ReviewRunSummary) => {
    setStart(null);
    // The new run heads the unfiltered list, which is where it is shown from.
    if ((repo && run.repo !== repo.toLowerCase()) || status) {
      setRepo("");
      setStatus("");
    }
    setCursors([""]);
    runs.reload();
    setOpen(run.id);
  };

  const list = runs.data?.runs ?? [];
  const filtered = !!repo || !!status;
  const firstPage = cursors.length === 1;
  const hasOlder = !!runs.data?.next_cursor;

  const startButton = canManage && (
    <Button size="sm" onClick={() => setStart(repo ? { repo } : {})}>
      <GitPullRequest /> Start review
    </Button>
  );

  return (
    <div className="space-y-4">
      <div className="flex flex-wrap items-center gap-2">
        <Select value={repo || ALL} onValueChange={filter(setRepo)}>
          <SelectTrigger size="sm" className={cn("w-full sm:w-56", repo && "font-mono text-xs")} aria-label="Repository">
            <SelectValue />
          </SelectTrigger>
          <SelectContent>
            <SelectItem value={ALL} className="font-sans">
              Every repository
            </SelectItem>
            {repoOptions.map((r) => (
              <SelectItem key={r} value={r} className="font-mono text-xs">
                {r}
              </SelectItem>
            ))}
          </SelectContent>
        </Select>
        <Select value={status || ALL} onValueChange={filter(setStatus)}>
          <SelectTrigger size="sm" className="w-[calc(50%-0.25rem)] sm:w-44" aria-label="Status">
            <SelectValue />
          </SelectTrigger>
          <SelectContent>
            <SelectItem value={ALL}>Every status</SelectItem>
            {RUN_STATUSES.map((s) => (
              <SelectItem key={s.value} value={s.value}>
                {s.label}
              </SelectItem>
            ))}
          </SelectContent>
        </Select>
        {filtered && (
          <Button
            variant="ghost"
            size="sm"
            onClick={() => {
              setRepo("");
              setStatus("");
              setCursors([""]);
            }}
          >
            Clear
          </Button>
        )}
        <span className="flex-1" />
        {startButton}
      </div>

      {runs.error && !runs.data && <ErrorBanner message={runs.error} onRetry={runs.reload} />}

      {runs.loading || !ready ? (
        <TableSkeleton rows={5} columns={7} />
      ) : runs.data && list.length === 0 && firstPage ? (
        filtered ? (
          <EmptyState
            icon={History}
            title="No review matches"
            description={`Nothing ${status ? `${runStatus(status).label.toLowerCase()} ` : ""}${repo ? `on ${repo} ` : ""}yet. Clear the filters to see every review.`}
          />
        ) : (
          <EmptyState
            icon={History}
            title="No reviews yet"
            description="A review appears here when a pull request on a reviewed repository opens, when somebody asks for one with an @-command on GitHub, or when it is started from here — in Shadow, recorded on this page with nothing posted, or Live on the pull request."
            action={startButton || undefined}
          />
        )
      ) : runs.data ? (
        <>
          <div className={cn("overflow-hidden rounded-xl border bg-card transition-opacity", runs.refreshing && "opacity-70")}>
            <Table>
              <TableHeader>
                <TableRow>
                  {/* On a phone the row is two cells — the pull request and how it went — with the
                      time, score and findings folded under them rather than scrolled off sideways. */}
                  <TableHead className="hidden sm:table-cell">When</TableHead>
                  <TableHead>Pull request</TableHead>
                  <TableHead className="hidden md:table-cell">Types</TableHead>
                  <TableHead className="hidden lg:table-cell">Trigger</TableHead>
                  <TableHead>Status</TableHead>
                  <TableHead className="hidden text-right sm:table-cell">Score</TableHead>
                  <TableHead className="hidden text-right sm:table-cell">Findings</TableHead>
                  <TableHead className="hidden text-right sm:table-cell">Cost</TableHead>
                </TableRow>
              </TableHeader>
              <TableBody>
                {list.map((r) => (
                  <RunRow key={r.id} run={r} nameOf={nameOf} onOpen={() => setOpen(r.id)} />
                ))}
              </TableBody>
            </Table>
          </div>
          {(!firstPage || hasOlder) && (
            <div className="flex items-center justify-end gap-2">
              <Button variant="outline" size="sm" disabled={firstPage} onClick={() => setCursors((c) => c.slice(0, -1))}>
                <ChevronLeft /> Newer
              </Button>
              <Button
                variant="outline"
                size="sm"
                disabled={!hasOlder}
                onClick={() => runs.data?.next_cursor && setCursors((c) => [...c, runs.data!.next_cursor])}
              >
                Older <ChevronRight />
              </Button>
            </div>
          )}
        </>
      ) : null}

      <ReviewRunDialog
        id={open}
        canManage={canManage}
        onOpenChange={(o) => !o && setOpen(null)}
        onStarted={started}
        onRerunWith={(target) => {
          setOpen(null);
          setStart(target);
        }}
      />
      <StartReviewDialog target={start} onOpenChange={(o) => !o && setStart(null)} onStarted={started} />
    </div>
  );
}

function RunRow({ run: r, nameOf, onOpen }: { run: ReviewRunSummary; nameOf: (key: string) => string; onOpen: () => void }) {
  const st = runStatus(r.status);
  const done = !isRunActive(r.status);
  const score =
    r.score !== null && done ? (
      <StatusChip variant={scoreVariant(r.score)} className="tabular-nums">
        {r.score}/5
      </StatusChip>
    ) : null;
  const findings =
    r.findings > 0 ? (
      // The count opens the run, whose Findings are these rows.
      <button
        type="button"
        onClick={(e) => {
          e.stopPropagation();
          onOpen();
        }}
        className="underline-offset-2 hover:underline"
      >
        {r.open !== undefined && r.open !== r.findings ? (
          <>
            <span className={cn(r.open > 0 && "font-medium text-foreground")}>{r.open} open</span>
            <span className="text-muted-foreground"> of {r.findings}</span>
          </>
        ) : (
          <span className={cn(r.open ? "font-medium" : "text-muted-foreground")}>
            {r.findings}
            {r.open ? " open" : ""}
          </span>
        )}
      </button>
    ) : null;
  return (
    // The row is the way into a run, so it takes focus and Enter like a button: a run with no
    // findings has no other control in it that opens the detail. Only the row's own keys — Enter on
    // the pull request link inside it is that link's.
    <TableRow
      className="cursor-pointer outline-none focus-visible:bg-accent/60 focus-visible:ring-[3px] focus-visible:ring-ring/50"
      onClick={onOpen}
      tabIndex={0}
      onKeyDown={(e) => {
        if (e.target !== e.currentTarget || (e.key !== "Enter" && e.key !== " ")) return;
        e.preventDefault();
        onOpen();
      }}
    >
      <TableCell className="hidden text-xs whitespace-nowrap sm:table-cell">
        <RelativeTime value={r.created_at} />
      </TableCell>
      <TableCell>
        <span className="block max-w-[11rem] truncate font-mono text-xs text-muted-foreground sm:max-w-xs">{r.repo}</span>
        <span className="flex flex-wrap items-center gap-x-1.5 text-sm">
          <a
            href={`https://github.com/${r.repo}/pull/${r.pr}`}
            target="_blank"
            rel="noreferrer"
            onClick={(e) => e.stopPropagation()}
            className="font-medium tabular-nums text-primary underline-offset-2 hover:underline"
          >
            #{r.pr}
          </a>
          {r.head_sha && <span className="font-mono text-xs text-muted-foreground">{shortSHA(r.head_sha)}</span>}
          {r.kind === "try" && <StatusChip variant="ai">Try</StatusChip>}
        </span>
        <span className="block text-xs text-muted-foreground sm:hidden">
          <RelativeTime value={r.created_at} />
          {r.cost_usd > 0 ? ` · ${runCost(r.cost_usd)}` : ""}
        </span>
      </TableCell>
      <TableCell className="hidden md:table-cell">
        <span className="flex max-w-[16rem] flex-wrap gap-1">
          {r.types.map((t) => (
            <span
              key={t.key}
              title={t.version ? `${t.key} v${t.version}` : r.kind === "try" ? `${t.key}, unsaved` : `${t.key}, as shipped`}
              className="rounded-sm bg-secondary px-1.5 py-0.5 text-xs text-secondary-foreground"
            >
              {nameOf(t.key)}
            </span>
          ))}
          {r.types.length === 0 && <span className="text-xs text-muted-foreground">—</span>}
        </span>
      </TableCell>
      <TableCell className="hidden text-xs lg:table-cell">
        {triggerLabel(r.trigger)}
        {r.rule && <span className="block font-mono text-muted-foreground">{r.rule}</span>}
      </TableCell>
      <TableCell>
        <StatusChip variant={st.variant}>{st.label}</StatusChip>
        {(score || findings) && (
          <span className="mt-1 flex items-center gap-2 text-xs sm:hidden">
            {score}
            {findings}
          </span>
        )}
      </TableCell>
      <TableCell className="hidden text-right sm:table-cell">{score ?? <span className="text-xs text-muted-foreground">—</span>}</TableCell>
      <TableCell className="hidden text-right text-xs whitespace-nowrap tabular-nums sm:table-cell">
        {findings ?? <span className="text-muted-foreground">{done ? "0" : "—"}</span>}
      </TableCell>
      <TableCell className="hidden text-right text-xs tabular-nums sm:table-cell">{runCost(r.cost_usd)}</TableCell>
    </TableRow>
  );
}
