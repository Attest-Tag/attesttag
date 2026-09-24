import { Globe } from "lucide-react";
import { EmptyState } from "@/components/core/empty-state";
import { StatusChip, type StatusChipVariant } from "@/components/core/status-chip";
import {
  Table,
  TableBody,
  TableCell,
  TableHead,
  TableHeader,
  TableRow,
} from "@/components/ui/table";
import type { ProxyAudit } from "@/lib/api";
import { formatDateTime, formatMillis } from "@/lib/format";

function statusTone(status: number): StatusChipVariant {
  if (status === 0) return "neutral";
  if (status < 300) return "success";
  if (status < 400) return "info";
  if (status < 500) return "warning";
  return "danger";
}

// Rows arrive already narrowed by channel, window and failures, so the count on the tab is the
// number of rows underneath it.
export function ProxyTable({
  rows,
  failedOnly,
}: {
  rows: ProxyAudit[];
  /** The list is already narrowed to failures, which the empty state should say. */
  failedOnly?: boolean;
}) {
  const visible = rows;
  if (visible.length === 0) {
    return failedOnly ? (
      <EmptyState
        icon={Globe}
        title="No failed requests"
        description="Nothing was blocked, and every proxied request came back with a good status. Switch the filter back to All to see them all."
      />
    ) : (
      <EmptyState
        icon={Globe}
        title="No proxied requests"
        description="Every HTTP call the bot makes through a connection is recorded here — host, path, status, and why it was blocked when it was."
      />
    );
  }
  return (
    <div className="overflow-hidden rounded-xl border bg-card">
      <Table>
        <TableHeader>
          <TableRow>
            <TableHead>Time</TableHead>
            <TableHead>Channel</TableHead>
            <TableHead>Method</TableHead>
            <TableHead>Host</TableHead>
            <TableHead>Path</TableHead>
            <TableHead>Status</TableHead>
            <TableHead className="text-right">Took</TableHead>
          </TableRow>
        </TableHeader>
        <TableBody>
          {visible.map((r) => (
            <TableRow key={r.id}>
              <TableCell className="whitespace-nowrap text-xs">{formatDateTime(r.created_at)}</TableCell>
              <TableCell className="max-w-[12rem]">
                <span className="block truncate text-xs" title={r.channel_name}>
                  {r.channel_name || "—"}
                </span>
              </TableCell>
              <TableCell className="font-mono text-xs">{r.method}</TableCell>
              <TableCell className="font-mono text-xs">{r.host}</TableCell>
              <TableCell className="max-w-xs">
                <span className="block truncate font-mono text-xs" title={r.path}>
                  {r.path}
                </span>
              </TableCell>
              <TableCell>
                {r.blocked ? (
                  <span className="flex items-center gap-1.5">
                    <StatusChip variant="danger">Blocked</StatusChip>
                    <span className="text-xs text-muted-foreground">{r.blocked}</span>
                  </span>
                ) : (
                  <StatusChip variant={statusTone(r.status)}>{r.status || "—"}</StatusChip>
                )}
              </TableCell>
              <TableCell className="text-right text-xs tabular-nums">{formatMillis(r.ms)}</TableCell>
            </TableRow>
          ))}
        </TableBody>
      </Table>
    </div>
  );
}
