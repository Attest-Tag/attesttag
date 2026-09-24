"use client";

import { useEffect, useState } from "react";
import { Ban, ExternalLink, Eye, Hammer, MoreHorizontal } from "lucide-react";
import { toast } from "sonner";
import { useConfirm } from "@/components/core/confirm-dialog";
import { EmptyState } from "@/components/core/empty-state";
import { ErrorBanner } from "@/components/core/error-banner";
import { PageHeader } from "@/components/core/page-header";
import { RelativeTime } from "@/components/core/relative-time";
import { StatusChip } from "@/components/core/status-chip";
import { TableSkeleton } from "@/components/core/table-skeleton";
import { JobDetailDialog } from "@/components/jobs/job-detail";
import { formatJobCost, formatJobDuration, jobStatusVariant } from "@/components/jobs/job-format";
import { BranchLink, RepoLink } from "@/components/jobs/repo-link";
import { Button } from "@/components/ui/button";
import {
  DropdownMenu,
  DropdownMenuContent,
  DropdownMenuItem,
  DropdownMenuSeparator,
  DropdownMenuTrigger,
} from "@/components/ui/dropdown-menu";
import {
  Table,
  TableBody,
  TableCell,
  TableHead,
  TableHeader,
  TableRow,
} from "@/components/ui/table";
import { api, errorMessage, isJobActive, useApi, type Job } from "@/lib/api";
import { parseTime } from "@/lib/format";

export function JobsPage() {
  const jobs = useApi<Job[]>("/api/jobs");
  const { confirm, confirmDialog } = useConfirm();
  const [open, setOpen] = useState<number | null>(null);

  // Poll while anything is in flight; the worker reports every minute at most.
  const anyActive = (jobs.data ?? []).some((j) => isJobActive(j.status));
  const reload = jobs.reload;
  useEffect(() => {
    if (!anyActive) return;
    const t = setInterval(reload, 10_000);
    return () => clearInterval(t);
  }, [anyActive, reload]);

  const cancel = async (j: Job) => {
    const ok = await confirm({
      title: `Cancel job #${j.id}?`,
      description:
        "The worker is told to stop at its next step. Nothing more is pushed, and the thread is told the job was cancelled.",
      confirmLabel: "Cancel job",
      destructive: true,
    });
    if (!ok) return;
    try {
      await api.post(`/api/jobs/${j.id}/cancel`);
      toast.success("Cancel requested");
    } catch (err) {
      toast.error(errorMessage(err));
    } finally {
      jobs.reload();
    }
  };

  return (
    <div className="space-y-5">
      <PageHeader
        title="Jobs"
        description="Fix jobs the bot handed to a worker: it clones the repository, makes the change, runs the tests and opens a draft pull request. Nothing merges on its own."
      />

      {jobs.error && !jobs.data && <ErrorBanner message={jobs.error} onRetry={jobs.reload} />}

      {jobs.loading ? (
        <TableSkeleton rows={4} columns={8} />
      ) : jobs.data && jobs.data.length === 0 ? (
        <EmptyState
          icon={Hammer}
          title="No fix jobs yet"
          description="A job starts when someone asks the bot to fix something and raise a PR in a channel with a connected repository, and a person presses Confirm. It appears here the moment it is dispatched."
        />
      ) : jobs.data ? (
        <div className="overflow-hidden rounded-xl border bg-card">
          <Table>
            <TableHeader>
              <TableRow>
                <TableHead>Started</TableHead>
                <TableHead>Status</TableHead>
                <TableHead>Job</TableHead>
                <TableHead>Channel</TableHead>
                <TableHead>Requester</TableHead>
                <TableHead>PR</TableHead>
                <TableHead className="text-right">Cost</TableHead>
                <TableHead>Duration</TableHead>
                <TableHead className="w-12"></TableHead>
              </TableRow>
            </TableHeader>
            <TableBody>
              {jobs.data.map((j) => {
                const started = parseTime(j.created_at);
                return (
                  <TableRow key={j.id} className="cursor-pointer" onClick={() => setOpen(j.id)}>
                    <TableCell className="whitespace-nowrap text-xs">
                      {started ? <RelativeTime value={started} /> : "—"}
                    </TableCell>
                    <TableCell>
                      <StatusChip variant={jobStatusVariant(j.status)}>{j.status}</StatusChip>
                    </TableCell>
                    <TableCell>
                      <span className="block max-w-xs truncate text-sm font-medium">
                        #{j.id} {j.title || "(untitled)"}
                      </span>
                      <span className="block max-w-xs truncate font-mono text-xs text-muted-foreground">
                        <RepoLink repo={j.repo} className="underline-offset-2 hover:underline" />
                      </span>
                    </TableCell>
                    <TableCell className="text-xs">{j.channel_name || j.channel || "—"}</TableCell>
                    <TableCell className="text-xs">{j.requester_name || j.requester || "—"}</TableCell>
                    <TableCell className="text-xs" onClick={(e) => e.stopPropagation()}>
                      {j.pr_url ? (
                        <a
                          href={j.pr_url}
                          target="_blank"
                          rel="noreferrer"
                          className="inline-flex items-center gap-1 text-primary underline-offset-2 hover:underline"
                        >
                          PR <ExternalLink className="size-3" />
                        </a>
                      ) : j.branch ? (
                        <span className="font-mono text-muted-foreground">
                          <BranchLink
                            repo={j.repo}
                            branch={j.branch}
                            className="underline-offset-2 hover:underline"
                          />
                        </span>
                      ) : (
                        "—"
                      )}
                    </TableCell>
                    <TableCell className="text-right text-xs tabular-nums">{formatJobCost(j.cost_usd)}</TableCell>
                    <TableCell className="text-xs tabular-nums">{formatJobDuration(j.duration_s)}</TableCell>
                    <TableCell onClick={(e) => e.stopPropagation()}>
                      <DropdownMenu>
                        <DropdownMenuTrigger asChild>
                          <Button variant="ghost" size="icon-sm" aria-label="Job actions">
                            <MoreHorizontal />
                          </Button>
                        </DropdownMenuTrigger>
                        <DropdownMenuContent align="end">
                          <DropdownMenuItem onClick={() => setOpen(j.id)}>
                            <Eye className="size-4" /> View
                          </DropdownMenuItem>
                          {isJobActive(j.status) && (
                            <>
                              <DropdownMenuSeparator />
                              <DropdownMenuItem variant="destructive" onClick={() => cancel(j)}>
                                <Ban className="size-4" /> Cancel
                              </DropdownMenuItem>
                            </>
                          )}
                        </DropdownMenuContent>
                      </DropdownMenu>
                    </TableCell>
                  </TableRow>
                );
              })}
            </TableBody>
          </Table>
        </div>
      ) : null}

      <JobDetailDialog id={open} onOpenChange={(o) => !o && setOpen(null)} />
      {confirmDialog}
    </div>
  );
}
