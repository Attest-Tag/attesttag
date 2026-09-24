"use client";

import { Sparkles } from "lucide-react";
import { Disclosure } from "@/components/core/disclosure";
import { EmptyState } from "@/components/core/empty-state";
import { Markdown } from "@/components/core/markdown";
import { RelativeTime } from "@/components/core/relative-time";
import { StatusChip } from "@/components/core/status-chip";
import type { AssistantTurnRow } from "@/lib/api";

// What people asked the console assistant, and what it said back.
//
// A list rather than a table, unlike the other three tabs: the interesting column is prose, and
// prose in a table cell is either clipped to uselessness or makes every row a different height.
// The question is the line you scan; the answer is one click away.

function money(usd: number): string {
  if (!usd) return "$0";
  return usd < 0.01 ? `$${usd.toFixed(4)}` : `$${usd.toFixed(2)}`;
}

export function AssistantTurnsTable({ rows }: { rows: AssistantTurnRow[] }) {
  if (rows.length === 0) {
    return (
      <EmptyState
        icon={Sparkles}
        title="Nothing asked yet"
        description="Questions people put to the assistant in the console appear here, with what it answered and what it proposed."
      />
    );
  }
  return (
    <div className="divide-y rounded-lg border">
      {rows.map((r) => (
        <div key={r.id} className="px-3 py-2.5">
          <Disclosure
            label={
              <span className="flex min-w-0 items-center gap-2">
                <span className="min-w-0 truncate text-sm">{r.question || "(a file, with no question)"}</span>
                {r.error && <StatusChip variant="danger">failed</StatusChip>}
                {r.proposals > 0 && (
                  <StatusChip variant="ai">
                    {r.proposals} proposed
                  </StatusChip>
                )}
              </span>
            }
            summary={
              <span className="flex items-center gap-2 whitespace-nowrap">
                <span>{r.actor_name || "somebody"}</span>
                {r.page && <span>· {r.page}</span>}
                <RelativeTime value={r.at} />
              </span>
            }
          >
            <div className="space-y-2">
              {r.reply ? (
                <Markdown text={r.reply} className="text-sm" />
              ) : (
                <p className="text-sm text-muted-foreground italic">It answered with nothing.</p>
              )}
              {r.error && (
                <p className="rounded-md border border-danger/30 bg-danger-soft px-2 py-1.5 text-xs text-danger">
                  {r.error}
                </p>
              )}
              <p className="flex flex-wrap items-center gap-x-2 gap-y-0.5 text-[11px] text-muted-foreground">
                <span className="font-mono">{r.model || "—"}</span>
                <span>·</span>
                <span>
                  {r.tool_calls} tool {r.tool_calls === 1 ? "call" : "calls"}
                </span>
                <span>·</span>
                <span>
                  {r.tokens_in} in · {r.tokens_out} out
                </span>
                <span>·</span>
                <span>{money(r.cost_usd)}</span>
              </p>
            </div>
          </Disclosure>
        </div>
      ))}
    </div>
  );
}
