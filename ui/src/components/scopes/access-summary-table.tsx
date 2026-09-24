import { StatusChip, type StatusChipVariant } from "@/components/core/status-chip";
import {
  Table,
  TableBody,
  TableCell,
  TableHead,
  TableHeader,
  TableRow,
} from "@/components/ui/table";
import type { AccessRow } from "@/lib/api";

const ORIGIN_TONE: Record<string, StatusChipVariant> = {
  "attached here": "success",
  "inherited from workspace": "info",
  "domain, no credential": "neutral",
};

// Everything the bot can reach from this scope, resolved: each connection as
// bundle/name with its hosts, whether it came through its bundle or on its
// own, and whether the grant is the channel's own or comes down from the
// workspace.
export function AccessSummaryTable({ rows }: { rows: AccessRow[] }) {
  if (rows.length === 0) {
    return (
      <p className="text-sm text-muted-foreground">
        Nothing is reachable from here. Attach a bundle, or a single connection from one.
      </p>
    );
  }
  return (
    <div className="overflow-hidden rounded-lg border">
      <Table>
        <TableHeader>
          <TableRow>
            <TableHead>Host</TableHead>
            <TableHead>Connection</TableHead>
            <TableHead>Via</TableHead>
            <TableHead>Credential</TableHead>
            <TableHead>Writes</TableHead>
            <TableHead>Origin</TableHead>
          </TableRow>
        </TableHeader>
        <TableBody>
          {rows.map((row, i) => (
            <TableRow key={`${row.host}-${row.connection}-${i}`}>
              <TableCell className="font-mono text-xs">{row.host}</TableCell>
              <TableCell>
                {row.connection ? (
                  <>
                    <span className="text-muted-foreground">{row.bundle}/</span>
                    {row.connection}
                  </>
                ) : (
                  <span className="text-muted-foreground">—</span>
                )}
              </TableCell>
              <TableCell>
                {row.via === "connection" ? (
                  <StatusChip variant="neutral">on its own</StatusChip>
                ) : (
                  <span className="text-xs">bundle {row.bundle}</span>
                )}
              </TableCell>
              <TableCell className="font-mono text-xs">{row.credential}</TableCell>
              <TableCell>
                {row.writes ? (
                  <span className="text-xs">{row.writes}</span>
                ) : (
                  <span className="text-muted-foreground">—</span>
                )}
              </TableCell>
              <TableCell>
                <StatusChip variant={ORIGIN_TONE[row.origin] ?? "neutral"}>
                  {row.origin}
                </StatusChip>
              </TableCell>
            </TableRow>
          ))}
        </TableBody>
      </Table>
    </div>
  );
}
