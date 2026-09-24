import { Activity } from "lucide-react";
import { EmptyState } from "@/components/core/empty-state";
import {
  Table,
  TableBody,
  TableCell,
  TableHead,
  TableHeader,
  TableRow,
} from "@/components/ui/table";
import { useTeamNames } from "@/components/core/team-cell";
import type { TurnRow } from "@/lib/api";
import { formatDateTime, formatNumber, formatUSD } from "@/lib/format";

export function TurnsTable({ rows }: { rows: TurnRow[] }) {
  const teams = useTeamNames();
  if (rows.length === 0) {
    return (
      <EmptyState
        icon={Activity}
        title="No turns"
        description="Every reply the bot writes is a turn. Mention it in a channel and the first one appears here."
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
            {teams.several && <TableHead>Workspace</TableHead>}
            <TableHead>Model</TableHead>
            <TableHead className="text-right">In</TableHead>
            <TableHead className="text-right">Out</TableHead>
            <TableHead className="text-right">Cost</TableHead>
          </TableRow>
        </TableHeader>
        <TableBody>
          {rows.map((t, i) => (
            <TableRow key={`${t.At}-${t.ThreadTS}-${i}`}>
              <TableCell className="whitespace-nowrap text-xs">{formatDateTime(t.At)}</TableCell>
              <TableCell className="max-w-[16rem]">
                <span className="block truncate" title={t.ChannelName || t.Channel}>
                  {t.ChannelName || t.Channel || "—"}
                </span>
                {t.ThreadTS && (
                  <span className="block font-mono text-[10px] text-muted-foreground">
                    {t.ThreadTS}
                  </span>
                )}
              </TableCell>
              {teams.several && (
                <TableCell className="text-xs">
                  {t.TeamName || teams.name(t.TeamID) || "—"}
                </TableCell>
              )}
              <TableCell className="font-mono text-xs">{t.Model || "—"}</TableCell>
              <TableCell className="text-right tabular-nums">{formatNumber(t.In)}</TableCell>
              <TableCell className="text-right tabular-nums">{formatNumber(t.Out)}</TableCell>
              <TableCell className="text-right tabular-nums">{formatUSD(t.Cost)}</TableCell>
            </TableRow>
          ))}
        </TableBody>
      </Table>
    </div>
  );
}
