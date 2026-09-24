"use client";

import { useState } from "react";
import {
  FolderSync,
  Loader2,
  MoreHorizontal,
  Pause,
  Pencil,
  Play,
  Plus,
  RefreshCw,
  Trash2,
} from "lucide-react";
import { toast } from "sonner";
import { useConfirm } from "@/components/core/confirm-dialog";
import { RelativeTime } from "@/components/core/relative-time";
import { StatusChip } from "@/components/core/status-chip";
import { DriveSyncDialog } from "@/components/documents/drive-sync-dialog";
import { Button } from "@/components/ui/button";
import {
  DropdownMenu,
  DropdownMenuContent,
  DropdownMenuItem,
  DropdownMenuSeparator,
  DropdownMenuTrigger,
} from "@/components/ui/dropdown-menu";
import {
  api,
  errorMessage,
  useApi,
  type DriveReport,
  type DriveSync,
  type DriveSyncs,
  type Scope,
} from "@/lib/api";

type RunResult = { ok: boolean; error?: string; report?: DriveReport };
type RunAllResult = { ok: boolean; ran: number; report: DriveReport; failed: string[] | null };

/** What one pass did, as a sentence. Zeroes are dropped: "nothing changed" is the useful line
 *  when nothing changed, not "0 added, 0 updated, 0 removed". */
function describe(r: DriveReport | undefined): string {
  if (!r) return "";
  const parts: string[] = [];
  if (r.added) parts.push(`${r.added} added`);
  if (r.updated) parts.push(`${r.updated} updated`);
  if (r.removed) parts.push(`${r.removed} removed`);
  if (parts.length === 0) parts.push("nothing changed");
  if (r.unchanged) parts.push(`${r.unchanged} already current`);
  const skipped = r.skipped?.length ?? 0;
  if (skipped) parts.push(`${skipped} skipped`);
  return parts.join(", ") + (r.took ? ` in ${r.took}` : "");
}

/** The last run, as the row's status chip. */
function statusOf(s: DriveSync): { label: string; tone: "success" | "warning" | "danger" | "neutral" } {
  if (!s.enabled) return { label: "Paused", tone: "neutral" };
  switch (s.last_status) {
    case "running":
      return { label: "Syncing", tone: "warning" };
    case "error":
      return { label: "Failed", tone: "danger" };
    case "ok":
      return { label: "Synced", tone: "success" };
    default:
      return { label: "Never run", tone: "neutral" };
  }
}

// Google Drive folders this organisation follows. The scheduled pass runs on its own; this is
// where somebody adds a folder, sees what the last pass did, and asks for one now.
export function DriveSyncPanel({
  folders,
  scopes,
  onChanged,
}: {
  folders: string[] | undefined;
  scopes: Scope[] | undefined;
  /** Documents moved, so the table above should look again. */
  onChanged: () => void;
}) {
  const syncs = useApi<DriveSyncs>("/api/drive/syncs");
  const { confirm, confirmDialog } = useConfirm();
  const [dialog, setDialog] = useState<{ open: boolean; editing: DriveSync | null }>({
    open: false,
    editing: null,
  });
  // Which rows are mid-run. A set rather than a boolean, so one folder syncing does not put a
  // spinner on every other row.
  const [running, setRunning] = useState<number[]>([]);
  const [runningAll, setRunningAll] = useState(false);

  const list = syncs.data?.syncs ?? [];
  const everyHours = syncs.data?.every_hours ?? 6;
  const enabled = list.filter((s) => s.enabled);

  const after = () => {
    syncs.reload();
    onChanged();
  };

  const runOne = async (s: DriveSync) => {
    setRunning((r) => [...r, s.id]);
    try {
      const res = await api.post<RunResult>(`/api/drive/syncs/${s.id}/run`);
      const line = describe(res.report);
      if (!res.ok) toast.error(`${s.folder_name || "Sync"} failed`, { description: res.error });
      else toast.success(`Synced ${res.report?.folder_name || s.folder_name}`, { description: line });
      if ((res.report?.skipped?.length ?? 0) > 0) {
        toast.warning("Some files were skipped", {
          description: res.report!.skipped!.slice(0, 4).join("; "),
        });
      }
    } catch (err) {
      toast.error(errorMessage(err));
    } finally {
      setRunning((r) => r.filter((id) => id !== s.id));
      after();
    }
  };

  const runAll = async () => {
    setRunningAll(true);
    try {
      const res = await api.post<RunAllResult>("/api/drive/syncs/run");
      const line = describe(res.report);
      if (res.ran === 0) toast.info("Nothing to sync — every folder is paused");
      else if (res.ok)
        toast.success(`Synced ${res.ran} folder${res.ran === 1 ? "" : "s"}`, { description: line });
      else
        toast.warning(`${res.failed?.length ?? 0} of ${res.ran} failed`, {
          description: res.failed?.slice(0, 3).join("; "),
        });
    } catch (err) {
      toast.error(errorMessage(err));
    } finally {
      setRunningAll(false);
      after();
    }
  };

  const toggle = async (s: DriveSync) => {
    try {
      await api.put(`/api/drive/syncs/${s.id}`, {
        dest: s.dest,
        scope: s.scope,
        recurse: s.recurse,
        enabled: !s.enabled,
      });
      toast.success(s.enabled ? `Paused ${s.folder_name}` : `Resumed ${s.folder_name}`);
      syncs.reload();
    } catch (err) {
      toast.error(errorMessage(err));
    }
  };

  const remove = async (s: DriveSync) => {
    const ok = await confirm({
      title: `Stop following ${s.folder_name || s.folder_id}?`,
      description:
        s.docs > 0
          ? `The ${s.docs} document${s.docs === 1 ? "" : "s"} it brought in stay where they are and the bot goes on searching them — they just stop being updated. Delete them in the table if you don't want them.`
          : "Nothing has been brought in yet, so nothing else goes with it.",
      confirmLabel: "Stop following",
      destructive: true,
    });
    if (!ok) return;
    try {
      await api.del(`/api/drive/syncs/${s.id}`);
      toast.success("Stopped following");
      after();
    } catch (err) {
      toast.error(errorMessage(err));
    }
  };

  return (
    <section className="rounded-xl border">
      <header className="flex flex-wrap items-center justify-between gap-3 border-b px-4 py-3">
        <div className="flex items-center gap-3">
          <span className="flex size-8 items-center justify-center rounded-md border bg-card">
            <FolderSync className="size-4 text-muted-foreground" />
          </span>
          <div>
            <h2 className="text-sm font-medium">Google Drive</h2>
            <p className="text-xs text-muted-foreground">
              {list.length === 0
                ? "Follow a Drive folder and its files are kept here for the bot to search."
                : `${list.length} folder${list.length === 1 ? "" : "s"} followed · checked every ${everyHours} hours`}
            </p>
          </div>
        </div>
        <div className="flex items-center gap-2">
          {enabled.length > 0 && (
            <Button variant="outline" size="sm" onClick={runAll} disabled={runningAll}>
              <RefreshCw className={runningAll ? "size-4 animate-spin" : "size-4"} />
              Sync now
            </Button>
          )}
          <Button size="sm" onClick={() => setDialog({ open: true, editing: null })}>
            <Plus className="size-4" />
            Add folder
          </Button>
        </div>
      </header>

      {syncs.loading ? (
        <p className="px-4 py-6 text-sm text-muted-foreground">Loading…</p>
      ) : list.length === 0 ? (
        <p className="px-4 py-6 text-sm text-muted-foreground">
          Nothing followed yet. Adding a folder copies what is in it into Documents and checks it
          again every {everyHours} hours — Docs as Markdown, Sheets as CSV, Slides as PDF.
        </p>
      ) : (
        <ul className="divide-y">
          {list.map((s) => {
            const busy = running.includes(s.id) || s.last_status === "running";
            const status = statusOf(s);
            return (
              <li key={s.id} className="flex flex-wrap items-center gap-3 px-4 py-3">
                <div className="min-w-0 flex-1">
                  <div className="flex flex-wrap items-center gap-2">
                    <a
                      className="truncate text-sm font-medium underline-offset-2 hover:underline"
                      href={`https://drive.google.com/drive/folders/${s.folder_id}`}
                      target="_blank"
                      rel="noreferrer"
                    >
                      {s.folder_name || s.folder_id}
                    </a>
                    <StatusChip variant={status.tone}>{status.label}</StatusChip>
                  </div>
                  <p className="mt-0.5 truncate text-xs text-muted-foreground">
                    → {s.dest || "top level"} · {s.docs} document{s.docs === 1 ? "" : "s"} ·{" "}
                    {/* Bundle first, then the credential: two bundles can each hold a "Drive",
                        and which bundle it is decides which channels can reach it. */}
                    {s.connection_name
                      ? s.bundle_name
                        ? `${s.bundle_name} / ${s.connection_name}`
                        : s.connection_name
                      : "credential deleted"}
                    {s.last_run && (
                      <>
                        {" · last run "}
                        <RelativeTime value={s.last_run} />
                      </>
                    )}
                  </p>
                  {s.last_status === "error" && s.last_error && (
                    <p className="mt-1 text-xs text-destructive">{s.last_error}</p>
                  )}
                  {s.last_status === "ok" && (
                    <p className="mt-1 text-xs text-muted-foreground">
                      {describe({
                        folder_name: s.folder_name,
                        added: s.last_added,
                        updated: s.last_updated,
                        removed: s.last_removed,
                        unchanged: 0,
                        skipped: null,
                        took: "",
                      })}
                    </p>
                  )}
                </div>
                <Button variant="outline" size="sm" onClick={() => runOne(s)} disabled={busy}>
                  {busy ? (
                    <Loader2 className="size-4 animate-spin" />
                  ) : (
                    <RefreshCw className="size-4" />
                  )}
                  Sync now
                </Button>
                <DropdownMenu>
                  <DropdownMenuTrigger asChild>
                    <Button variant="ghost" size="icon" aria-label={`More for ${s.folder_name}`}>
                      <MoreHorizontal className="size-4" />
                    </Button>
                  </DropdownMenuTrigger>
                  <DropdownMenuContent align="end">
                    <DropdownMenuItem onClick={() => setDialog({ open: true, editing: s })}>
                      <Pencil className="size-4" /> Edit
                    </DropdownMenuItem>
                    <DropdownMenuItem onClick={() => toggle(s)}>
                      {s.enabled ? (
                        <>
                          <Pause className="size-4" /> Pause
                        </>
                      ) : (
                        <>
                          <Play className="size-4" /> Resume
                        </>
                      )}
                    </DropdownMenuItem>
                    <DropdownMenuSeparator />
                    <DropdownMenuItem variant="destructive" onClick={() => remove(s)}>
                      <Trash2 className="size-4" /> Stop following
                    </DropdownMenuItem>
                  </DropdownMenuContent>
                </DropdownMenu>
              </li>
            );
          })}
        </ul>
      )}

      <DriveSyncDialog
        open={dialog.open}
        editing={dialog.editing}
        folders={folders}
        scopes={scopes}
        onOpenChange={(open) => setDialog((d) => ({ ...d, open }))}
        onSaved={after}
      />
      {confirmDialog}
    </section>
  );
}
