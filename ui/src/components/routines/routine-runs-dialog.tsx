"use client";

import { useState } from "react";
import { History, Loader2, RefreshCw } from "lucide-react";
import { Disclosure } from "@/components/core/disclosure";
import { EmptyState } from "@/components/core/empty-state";
import { ErrorBanner } from "@/components/core/error-banner";
import { RelativeTime } from "@/components/core/relative-time";
import { StatusChip, type StatusChipVariant } from "@/components/core/status-chip";
import { Button } from "@/components/ui/button";
import {
  Dialog,
  DialogContent,
  DialogDescription,
  DialogHeader,
  DialogTitle,
} from "@/components/ui/dialog";
import { Skeleton } from "@/components/ui/skeleton";
import { api, useApi, type Routine, type RoutineRun } from "@/lib/api";
import { formatMillis, formatNumber, formatUSD } from "@/lib/format";

/** How a run reads in the list. "Quiet" is a success: it ran, looked, and had nothing to say. */
const RUN_STATUS: Record<string, { label: string; variant: StatusChipVariant }> = {
  posted: { label: "Posted", variant: "success" },
  quiet: { label: "Quiet", variant: "neutral" },
  failed: { label: "Failed", variant: "danger" },
  skipped: { label: "Skipped", variant: "warning" },
};

export function runStatusChip(status: string) {
  const s = RUN_STATUS[status];
  if (!s) return null;
  return <StatusChip variant={s.variant}>{s.label}</StatusChip>;
}

/** Every run of one routine, the quiet ones included. Keyed on the routine id by the caller. */
export function RoutineRunsDialog({
  routine,
  onOpenChange,
}: {
  routine: Routine | null;
  onOpenChange: (open: boolean) => void;
}) {
  return (
    <Dialog open={routine !== null} onOpenChange={onOpenChange}>
      <DialogContent className="sm:max-w-2xl">
        {routine && <RunList key={routine.ID} routine={routine} />}
      </DialogContent>
    </Dialog>
  );
}

function RunList({ routine }: { routine: Routine }) {
  const runs = useApi<{ runs: RoutineRun[] }>(`/api/routines/${routine.ID}/runs`);
  const quiet = routine.Notify === "when_needed";

  return (
    <div className="space-y-4">
      <DialogHeader>
        <DialogTitle className="flex items-center gap-2">
          Routine #{routine.ID} runs
          <Button
            variant="ghost"
            size="icon-sm"
            onClick={runs.reload}
            aria-label="Reload runs"
            disabled={runs.refreshing}
          >
            {runs.refreshing ? <Loader2 className="animate-spin" /> : <RefreshCw />}
          </Button>
        </DialogTitle>
        <DialogDescription>
          {quiet
            ? "Every run is recorded here, including the ones that stayed out of the channel."
            : "Every run, with the answer it posted."}
        </DialogDescription>
      </DialogHeader>

      {runs.error && !runs.data && <ErrorBanner message={runs.error} onRetry={runs.reload} />}

      {runs.loading ? (
        <div className="space-y-2">
          {[0, 1, 2].map((i) => (
            <Skeleton key={i} className="h-12 w-full" />
          ))}
        </div>
      ) : runs.data && runs.data.runs.length === 0 ? (
        <EmptyState
          icon={History}
          title="No runs yet"
          description={`Nothing has run on this schedule so far. The next one is due ${
            routine.Enabled && routine.NextRun ? routine.NextRun : "once the routine is enabled"
          }.`}
        />
      ) : runs.data ? (
        <div className="max-h-[26rem] divide-y overflow-y-auto rounded-lg border">
          {runs.data.runs.map((run) => (
            <RunRow key={run.ID} run={run} />
          ))}
        </div>
      ) : null}
    </div>
  );
}

function RunRow({ run }: { run: RoutineRun }) {
  // The listing previews the output; the whole of it is fetched only if someone opens the row.
  const [full, setFull] = useState<string | null>(null);
  const [loading, setLoading] = useState(false);

  const expand = async (open: boolean) => {
    if (!open || full !== null || !run.More || loading) return;
    setLoading(true);
    try {
      const detail = await api.get<RoutineRun>(`/api/routine-runs/${run.ID}`);
      setFull(detail.Output);
    } catch {
      setFull(run.Output); // the preview is still better than nothing
    } finally {
      setLoading(false);
    }
  };

  const body = full ?? run.Output;
  // A failed run's error is the point; a quiet one's note is why it said nothing.
  const subtitle = run.Error || run.Reason;

  return (
    <div className="px-3 py-2.5 text-sm">
      <div className="flex flex-wrap items-center gap-x-3 gap-y-1">
        <span className="min-w-32 text-xs text-muted-foreground">
          {run.StartedAt ? <RelativeTime value={run.StartedAt} /> : "—"}
        </span>
        {runStatusChip(run.Status)}
        <span className="ml-auto flex items-center gap-3 text-xs text-muted-foreground tabular-nums">
          {run.MS > 0 && <span>{formatMillis(run.MS)}</span>}
          {run.CostUSD > 0 && <span>{formatUSD(run.CostUSD)}</span>}
          {run.TokensIn + run.TokensOut > 0 && (
            <span>{formatNumber(run.TokensIn + run.TokensOut)} tok</span>
          )}
        </span>
      </div>
      {subtitle && (
        <p className={`mt-1 text-xs ${run.Error ? "text-danger" : "text-muted-foreground"}`}>
          {subtitle}
        </p>
      )}
      {body && (
        <Disclosure
          className="mt-1.5"
          label={<span className="text-xs">Answer</span>}
          summary={loading ? <Loader2 className="size-3 animate-spin" /> : null}
          onOpenChange={expand}
        >
          <pre className="max-h-72 overflow-auto whitespace-pre-wrap break-words text-xs text-muted-foreground">
            {body}
          </pre>
        </Disclosure>
      )}
    </div>
  );
}
