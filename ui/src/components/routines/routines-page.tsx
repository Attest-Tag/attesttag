"use client";

import { useState } from "react";
import { CalendarClock, History, MoreHorizontal, Pencil, Play, Plus, Trash2 } from "lucide-react";
import { toast } from "sonner";
import { useConfirm } from "@/components/core/confirm-dialog";
import { EmptyState } from "@/components/core/empty-state";
import { ErrorBanner } from "@/components/core/error-banner";
import { PageHeader } from "@/components/core/page-header";
import { RelativeTime } from "@/components/core/relative-time";
import { scopeNameFor } from "@/components/core/scope-label";
import { StatusChip } from "@/components/core/status-chip";
import { TableSkeleton } from "@/components/core/table-skeleton";
import { RoutineEditDialog, routineModelLabel } from "@/components/routines/routine-edit-dialog";
import { RoutineRunsDialog, runStatusChip } from "@/components/routines/routine-runs-dialog";
import { Button } from "@/components/ui/button";
import {
  DropdownMenu,
  DropdownMenuContent,
  DropdownMenuItem,
  DropdownMenuSeparator,
  DropdownMenuTrigger,
} from "@/components/ui/dropdown-menu";
import { Switch } from "@/components/ui/switch";
import {
  Table,
  TableBody,
  TableCell,
  TableHead,
  TableHeader,
  TableRow,
} from "@/components/ui/table";
import { Tooltip, TooltipContent, TooltipTrigger } from "@/components/ui/tooltip";
import { api, errorMessage, useApi, type Routine, type Scope } from "@/lib/api";
import { formatDateTime } from "@/lib/format";

export function RoutinesPage() {
  const routines = useApi<Routine[]>("/api/routines");
  const scopes = useApi<Scope[]>("/api/scopes?sync=0");
  const { confirm, confirmDialog } = useConfirm();
  // The routine the editor is open on: an existing one, or "new" for one being set up here.
  const [editing, setEditing] = useState<Routine | "new" | null>(null);
  const [viewing, setViewing] = useState<Routine | null>(null);

  const toggle = async (r: Routine, enabled: boolean) => {
    routines.mutate((list) => list?.map((x) => (x.ID === r.ID ? { ...x, Enabled: enabled } : x)));
    try {
      await api.put(`/api/routines/${r.ID}`, { enabled });
      toast.success(enabled ? "Routine enabled" : "Routine paused");
    } catch (err) {
      toast.error(errorMessage(err));
    } finally {
      routines.reload();
    }
  };

  const runNow = async (r: Routine) => {
    try {
      await api.post(`/api/routines/${r.ID}/run`);
      toast.success(
        r.Notify === "when_needed"
          ? "Running now — it posts only if it has something to report; either way the run appears here"
          : "Running now — the result lands in the channel",
      );
      routines.reload();
    } catch (err) {
      toast.error(errorMessage(err));
    }
  };

  const remove = async (r: Routine) => {
    const ok = await confirm({
      title: `Delete routine #${r.ID}?`,
      description: "It stops running and its schedule is removed. Past runs in the channel stay.",
      confirmLabel: "Delete",
      destructive: true,
    });
    if (!ok) return;
    try {
      await api.del(`/api/routines/${r.ID}`);
      toast.success("Routine deleted");
      routines.reload();
    } catch (err) {
      toast.error(errorMessage(err));
    }
  };

  const createButton = (
    <Button onClick={() => setEditing("new")}>
      <Plus className="size-4" /> New routine
    </Button>
  );

  return (
    <div className="space-y-5">
      <PageHeader
        title="Routines"
        description="Prompts that run on a cron schedule. They post their answer to a channel — always, or only when there is something worth saying. Each one answers on the default model or one an admin has offered. Select a routine to see its runs."
        actions={createButton}
      />

      {routines.error && !routines.data && (
        <ErrorBanner message={routines.error} onRetry={routines.reload} />
      )}

      {routines.loading ? (
        <TableSkeleton rows={4} columns={8} />
      ) : routines.data && routines.data.length === 0 ? (
        <EmptyState
          icon={CalendarClock}
          title="No routines"
          description="A routine is a prompt the bot runs on a schedule — a Monday standup digest, a nightly error summary. Write one here, or tell the bot in Slack 'every weekday at 9am post …' and it appears in this list."
          action={createButton}
        />
      ) : routines.data ? (
        <div className="overflow-hidden rounded-xl border bg-card">
          <Table>
            <TableHeader>
              <TableRow>
                <TableHead className="w-14">On</TableHead>
                <TableHead>Id</TableHead>
                <TableHead>Channel</TableHead>
                <TableHead>Schedule</TableHead>
                <TableHead>Reply</TableHead>
                <TableHead>Next run</TableHead>
                <TableHead>Last run</TableHead>
                <TableHead className="w-12"></TableHead>
              </TableRow>
            </TableHeader>
            <TableBody>
              {routines.data.map((r) => (
                <TableRow
                  key={r.ID}
                  onClick={() => setViewing(r)}
                  className="cursor-pointer"
                  title="Show runs"
                >
                  <TableCell onClick={(e) => e.stopPropagation()}>
                    <Switch
                      checked={r.Enabled}
                      onCheckedChange={(v) => toggle(r, v)}
                      aria-label={`Routine ${r.ID} enabled`}
                    />
                  </TableCell>
                  <TableCell className="font-mono text-xs">#{r.ID}</TableCell>
                  <TableCell>
                    <span className="block">{scopeNameFor(scopes.data, r.Channel, r.TeamID)}</span>
                    <span className="block max-w-xs truncate text-xs text-muted-foreground">
                      {r.Prompt}
                    </span>
                    {r.Model && (
                      <span className="block text-xs text-muted-foreground">
                        {routineModelLabel(r.Model)}
                      </span>
                    )}
                    {r.AutoConfirm && (
                      <Tooltip>
                        <TooltipTrigger asChild>
                          <span className="block text-xs text-amber-600 underline decoration-dotted underline-offset-4 dark:text-amber-500">
                            Writes run without asking
                          </span>
                        </TooltipTrigger>
                        <TooltipContent className="max-w-sm break-words">
                          This routine changes things without a Confirm card, because nobody is
                          there to press one when it runs. Every write is still recorded, and each
                          run says how many went through.
                        </TooltipContent>
                      </Tooltip>
                    )}
                  </TableCell>
                  <TableCell>
                    <span className="block font-mono text-xs">{r.Cron}</span>
                    <span className="block text-xs text-muted-foreground">{r.TZ}</span>
                  </TableCell>
                  <TableCell className="text-xs">
                    {r.Notify === "when_needed" ? (
                      <Tooltip>
                        <TooltipTrigger asChild>
                          <span className="underline decoration-dotted underline-offset-4">
                            When it matters
                          </span>
                        </TooltipTrigger>
                        <TooltipContent className="max-w-sm break-words">
                          {r.NotifyWhen
                            ? `Posts only when ${r.NotifyWhen}. Every run is recorded here either way.`
                            : "Posts only when it has something worth reporting. Every run is recorded here either way."}
                        </TooltipContent>
                      </Tooltip>
                    ) : (
                      <span className="text-muted-foreground">Always</span>
                    )}
                  </TableCell>
                  <TableCell className="text-xs">
                    {r.Enabled && r.NextRun ? (
                      formatDateTime(r.NextRun)
                    ) : (
                      <span className="text-muted-foreground">Paused</span>
                    )}
                  </TableCell>
                  <TableCell className="text-xs">
                    {r.LastRun ? (
                      <div className="flex items-center gap-2">
                        <RelativeTime value={r.LastRun} />
                        {r.LastError ? (
                          <Tooltip>
                            <TooltipTrigger asChild>
                              <span>{runStatusChip(r.LastStatus) ?? <StatusChip variant="danger">Failed</StatusChip>}</span>
                            </TooltipTrigger>
                            <TooltipContent className="max-w-sm break-words">
                              {r.LastError}
                            </TooltipContent>
                          </Tooltip>
                        ) : (
                          (runStatusChip(r.LastStatus) ?? <StatusChip variant="success">OK</StatusChip>)
                        )}
                      </div>
                    ) : (
                      <span className="text-muted-foreground">Never</span>
                    )}
                  </TableCell>
                  <TableCell onClick={(e) => e.stopPropagation()}>
                    <DropdownMenu>
                      <DropdownMenuTrigger asChild>
                        <Button variant="ghost" size="icon-sm" aria-label="Routine actions">
                          <MoreHorizontal />
                        </Button>
                      </DropdownMenuTrigger>
                      <DropdownMenuContent align="end">
                        <DropdownMenuItem onClick={() => setViewing(r)}>
                          <History className="size-4" /> Runs
                        </DropdownMenuItem>
                        <DropdownMenuItem onClick={() => setEditing(r)}>
                          <Pencil className="size-4" /> Edit
                        </DropdownMenuItem>
                        <DropdownMenuItem onClick={() => runNow(r)}>
                          <Play className="size-4" /> Run now
                        </DropdownMenuItem>
                        <DropdownMenuSeparator />
                        <DropdownMenuItem variant="destructive" onClick={() => remove(r)}>
                          <Trash2 className="size-4" /> Delete
                        </DropdownMenuItem>
                      </DropdownMenuContent>
                    </DropdownMenu>
                  </TableCell>
                </TableRow>
              ))}
            </TableBody>
          </Table>
        </div>
      ) : null}

      <RoutineEditDialog
        routine={editing}
        onOpenChange={(open) => !open && setEditing(null)}
        onSaved={routines.reload}
      />
      <RoutineRunsDialog
        routine={viewing}
        onOpenChange={(open) => !open && setViewing(null)}
      />
      {confirmDialog}
    </div>
  );
}
