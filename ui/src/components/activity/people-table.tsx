"use client";

import { Users } from "lucide-react";
import { EmptyState } from "@/components/core/empty-state";
import {
  Table,
  TableBody,
  TableCell,
  TableHead,
  TableHeader,
  TableRow,
} from "@/components/ui/table";
import type { ActiveUsersResponse } from "@/lib/api";
import { formatDateTime, formatNumber } from "@/lib/format";

/**
 * The rows behind "Users, last 30 days" on the Billing screen.
 *
 * One line per Slack account rather than per person. Where somebody is in two connected
 * workspaces and both installs can read their address, they are one user in the count and two
 * lines here — collapsing them would hide the very thing an admin came to check, which is why
 * the total is smaller than the list. The note under the table says so when it applies.
 */
export function PeopleTable({ data }: { data: ActiveUsersResponse }) {
  const rows = data.users ?? [];
  const count = data.count;

  if (rows.length === 0) {
    return (
      <EmptyState
        icon={Users}
        title="Nobody yet"
        description="Everyone who asked the bot something in the last 30 days appears here. This is the number your plan size is counted against."
      />
    );
  }

  const several = new Set(rows.map((r) => r.team_id)).size > 1;
  const deduped = rows.length > count.users;

  return (
    <div className="space-y-3">
      <div className="overflow-x-auto rounded-xl border">
        <Table>
          <TableHeader>
            <TableRow>
              <TableHead>Person</TableHead>
              {several && <TableHead>Workspace</TableHead>}
              <TableHead>Last active</TableHead>
              <TableHead className="text-right">Days</TableHead>
              <TableHead className="text-right">Turns</TableHead>
            </TableRow>
          </TableHeader>
          <TableBody>
            {rows.map((r) => (
              <TableRow key={r.identity}>
                <TableCell>
                  <span className="block">{r.name || r.slack_user}</span>
                  <span className="block font-mono text-[10px] text-muted-foreground">
                    {r.slack_user}
                  </span>
                </TableCell>
                {several && (
                  <TableCell className="text-xs">{r.team_name || r.team_id || "—"}</TableCell>
                )}
                <TableCell className="whitespace-nowrap text-xs">
                  {formatDateTime(r.last_seen)}
                </TableCell>
                <TableCell className="text-right tabular-nums">{formatNumber(r.days)}</TableCell>
                <TableCell className="text-right tabular-nums">{formatNumber(r.turns)}</TableCell>
              </TableRow>
            ))}
          </TableBody>
        </Table>
      </div>
      <p className="text-xs text-muted-foreground">
        {formatNumber(count.users)} {count.users === 1 ? "person" : "people"} in the last{" "}
        {count.window_days} days
        {count.limit > 0 && ` — ${formatNumber(count.limit)} users allowed on your size`}.
        {deduped &&
          ` ${formatNumber(rows.length)} Slack accounts are listed: somebody in two connected workspaces is counted once.`}
      </p>
    </div>
  );
}
