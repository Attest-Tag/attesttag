"use client";

import { Fragment, useState } from "react";
import { ChevronRight, Download, ExternalLink, FileText, Trash2 } from "lucide-react";
import { toast } from "sonner";
import { useTeamNames } from "@/components/core/team-cell";
import { useConfirm } from "@/components/core/confirm-dialog";
import { EmptyState } from "@/components/core/empty-state";
import { ErrorBanner } from "@/components/core/error-banner";
import { PageHeader } from "@/components/core/page-header";
import { StatusChip } from "@/components/core/status-chip";
import { Button } from "@/components/ui/button";
import {
  Table,
  TableBody,
  TableCell,
  TableHead,
  TableHeader,
  TableRow,
} from "@/components/ui/table";
import { TableSkeleton } from "@/components/core/table-skeleton";
import { api, errorMessage, useApi, type Artifact } from "@/lib/api";
import { cn } from "@/lib/utils";
import { formatBytes, formatDateTime } from "@/lib/format";

export function ArtifactsPage() {
  const artifacts = useApi<Artifact[]>("/api/artifacts");
  const teams = useTeamNames();
  const { confirm, confirmDialog } = useConfirm();
  const [open, setOpen] = useState<number | null>(null);

  const remove = async (a: Artifact) => {
    const ok = await confirm({
      title: "Delete this artifact?",
      description: `"${a.Title}" is removed from the console record. The file already posted in Slack stays where it is.`,
      confirmLabel: "Delete",
      destructive: true,
    });
    if (!ok) return;
    try {
      await api.del(`/api/artifacts/${a.ID}`);
      toast.success("Artifact deleted");
      artifacts.reload();
    } catch (err) {
      toast.error(errorMessage(err));
    }
  };

  return (
    <div className="space-y-5">
      <PageHeader
        title="Artifacts"
        description="Files the bot wrote and posted in a thread, kept here so there is one record of everything it has produced."
      />

      {artifacts.error && !artifacts.data && (
        <ErrorBanner message={artifacts.error} onRetry={artifacts.reload} />
      )}

      {artifacts.loading ? (
        <TableSkeleton rows={5} />
      ) : (artifacts.data ?? []).length === 0 ? (
        <EmptyState
          icon={FileText}
          title="Nothing made yet"
          description="Ask the bot for something as a file in Slack — 'give me this as a file', 'send that as CSV' — and it writes one, posts it in the thread and records it here."
        />
      ) : (
        <div className="overflow-hidden rounded-xl border bg-card">
          <Table>
            <TableHeader>
              <TableRow>
                <TableHead className="w-8" />
                <TableHead>Made</TableHead>
                <TableHead>Title</TableHead>
                <TableHead>Format</TableHead>
                <TableHead>Channel</TableHead>
                {teams.several && <TableHead>Workspace</TableHead>}
                <TableHead>Asked by</TableHead>
                <TableHead className="text-right">Size</TableHead>
                <TableHead className="w-24" />
              </TableRow>
            </TableHeader>
            <TableBody>
              {(artifacts.data ?? []).map((a) => {
                const expanded = open === a.ID;
                return (
                  <Fragment key={a.ID}>
                    <TableRow
                      onClick={() => setOpen(expanded ? null : a.ID)}
                      aria-expanded={expanded}
                      className="cursor-pointer"
                    >
                      <TableCell className="pr-0">
                        <ChevronRight
                          className={cn(
                            "size-4 text-muted-foreground transition-transform",
                            expanded && "rotate-90",
                          )}
                        />
                      </TableCell>
                      <TableCell className="whitespace-nowrap text-xs">
                        {formatDateTime(a.At)}
                      </TableCell>
                      <TableCell className="max-w-[18rem] truncate text-sm font-medium">
                        {a.Title}
                      </TableCell>
                      <TableCell>
                        <StatusChip variant="neutral">{a.Kind}</StatusChip>
                      </TableCell>
                      <TableCell className="text-xs">{a.ChannelName || a.Channel || "—"}</TableCell>
                      {teams.several && (
                        <TableCell className="text-xs">
                          {a.TeamName || teams.name(a.TeamID) || "—"}
                        </TableCell>
                      )}
                      <TableCell className="text-xs">{a.CreatedByName || a.CreatedBy || "—"}</TableCell>
                      <TableCell className="text-right text-xs tabular-nums">
                        {formatBytes(a.Bytes)}
                      </TableCell>
                      <TableCell onClick={(e) => e.stopPropagation()}>
                        <div className="flex items-center justify-end gap-1">
                          {a.Permalink && (
                            <Button
                              asChild
                              variant="ghost"
                              size="icon-sm"
                              aria-label="Open in Slack"
                              title="Open in Slack"
                            >
                              <a href={a.Permalink} target="_blank" rel="noreferrer">
                                <ExternalLink />
                              </a>
                            </Button>
                          )}
                          <Button
                            asChild
                            variant="ghost"
                            size="icon-sm"
                            aria-label="Download"
                            title="Download"
                          >
                            <a href={`/api/artifacts/${a.ID}/raw`} download>
                              <Download />
                            </a>
                          </Button>
                          <Button
                            variant="ghost"
                            size="icon-sm"
                            aria-label="Delete artifact"
                            className="text-muted-foreground hover:text-destructive"
                            onClick={() => remove(a)}
                          >
                            <Trash2 />
                          </Button>
                        </div>
                      </TableCell>
                    </TableRow>
                    {expanded && (
                      <TableRow className="hover:bg-transparent">
                        <TableCell colSpan={8} className="bg-muted/30 p-4">
                          <ArtifactBody id={a.ID} />
                        </TableCell>
                      </TableRow>
                    )}
                  </Fragment>
                );
              })}
            </TableBody>
          </Table>
        </div>
      )}

      {confirmDialog}
    </div>
  );
}

// The body is fetched on expand — the index deliberately ships without it. Always shown as
// text, never rendered: an html artifact is model-written markup and this origin holds the
// admin session.
function ArtifactBody({ id }: { id: number }) {
  const full = useApi<Artifact>(`/api/artifacts/${id}`);
  if (full.loading) return <p className="text-xs text-muted-foreground">Loading…</p>;
  if (full.error) return <ErrorBanner message={full.error} onRetry={full.reload} />;
  const body = full.data?.Content ?? "";
  return (
    <div className="space-y-1.5">
      <p className="text-xs font-semibold text-foreground">Contents</p>
      {body ? (
        <pre className="max-h-96 overflow-auto rounded-lg border bg-background p-3 font-mono text-xs leading-relaxed whitespace-pre-wrap break-words">
          {body}
        </pre>
      ) : (
        <p className="text-xs text-muted-foreground">This artifact is empty.</p>
      )}
      {full.data?.ThreadTS && (
        <p className="text-xs text-muted-foreground">Thread {full.data.ThreadTS}</p>
      )}
    </div>
  );
}
