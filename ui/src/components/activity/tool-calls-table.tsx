"use client";

import { Fragment, useEffect, useRef, useState } from "react";
import { ChevronRight, Lock, Wrench } from "lucide-react";
import { EmptyState } from "@/components/core/empty-state";
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
import { api, type ToolCallRow } from "@/lib/api";
import { cn } from "@/lib/utils";
import { formatBytes, formatDateTime, formatMillis } from "@/lib/format";

const ARGS_MAX = 60;
const OUTPUT_MAX = 70;

// Rows arrive already narrowed by channel, window and failures, so the count on the tab is the
// number of rows underneath it.
export function ToolCallsTable({
  rows,
  failedOnly,
  focus,
}: {
  rows: ToolCallRow[];
  /** The list is already narrowed to failures, which the empty state should say. */
  failedOnly?: boolean;
  /** Id of the call the reader was linked to: it opens, and the row is scrolled to. */
  focus?: number;
}) {
  // null means untouched, so the linked call is the one showing; 0 means it was closed again.
  const [open, setOpen] = useState<number | null>(null);
  const openKey = open === null ? (focus ?? 0) : open;
  const focused = useRef<HTMLTableRowElement>(null);
  useEffect(() => {
    const row = focused.current;
    if (!row) return;
    // A row that is already on screen stays where it is; scrolling it to the middle would
    // only push the page header out of sight.
    const box = row.getBoundingClientRect();
    if (box.top >= 0 && box.bottom <= window.innerHeight) return;
    row.scrollIntoView({ block: "center" });
  }, [focus]);
  const visible = rows;
  if (visible.length === 0) {
    return failedOnly ? (
      <EmptyState
        icon={Wrench}
        title="No failed tool calls"
        description="Nothing the bot ran came back with an error. Switch the filter back to All to see every call."
      />
    ) : (
      <EmptyState
        icon={Wrench}
        title="No tool calls"
        description="When the bot searches documents, calls a connected API or runs a tool, each call is logged here with its arguments, its output and timing."
      />
    );
  }
  return (
    <div className="overflow-hidden rounded-xl border bg-card">
      <Table>
        <TableHeader>
          <TableRow>
            <TableHead className="w-8" />
            <TableHead>Time</TableHead>
            <TableHead>Channel</TableHead>
            <TableHead>Tool</TableHead>
            <TableHead>Arguments</TableHead>
            <TableHead>Output</TableHead>
            <TableHead>Status</TableHead>
            <TableHead className="text-right">Took</TableHead>
          </TableRow>
        </TableHeader>
        <TableBody>
          {visible.map((t, i) => {
            const key = t.ID || -i - 1;
            const expanded = openKey === key;
            const isFocus = focus === t.ID;
            return (
              <Fragment key={key}>
                <TableRow
                  ref={isFocus ? focused : undefined}
                  onClick={() => setOpen(expanded ? 0 : key)}
                  aria-expanded={expanded}
                  className={cn("cursor-pointer", isFocus && "bg-accent/70")}
                >
                  <TableCell className="pr-0">
                    <ChevronRight
                      className={cn(
                        "size-4 text-muted-foreground transition-transform",
                        expanded && "rotate-90",
                      )}
                    />
                  </TableCell>
                  <TableCell className="whitespace-nowrap text-xs">{formatDateTime(t.At)}</TableCell>
                  <TableCell className="max-w-[12rem]">
                    <span className="block truncate text-xs" title={t.ChannelName}>
                      {t.ChannelName || "—"}
                    </span>
                  </TableCell>
                  <TableCell className="font-mono text-xs">{t.Name}</TableCell>
                  <TableCell className="max-w-[13rem]">
                    <Excerpt
                      text={t.Private ? keptRequest(t) : t.Args}
                      max={ARGS_MAX}
                      locked={t.Private}
                    />
                  </TableCell>
                  <TableCell className="max-w-[15rem]">
                    <Excerpt
                      text={t.Private ? keptOutcome(t.Result) : firstLine(t.Result)}
                      max={OUTPUT_MAX}
                      locked={t.Private}
                    />
                  </TableCell>
                  <TableCell>
                    <StatusChip variant={t.OK ? "success" : "danger"}>{t.OK ? "OK" : "Failed"}</StatusChip>
                  </TableCell>
                  <TableCell className="text-right text-xs tabular-nums">{formatMillis(t.MS)}</TableCell>
                </TableRow>
                {expanded && (
                  <TableRow className="hover:bg-transparent">
                    <TableCell colSpan={8} className="bg-muted/30 p-4">
                      <CallDetail row={t} />
                    </TableCell>
                  </TableRow>
                )}
              </Fragment>
            );
          })}
        </TableBody>
      </Table>
    </div>
  );
}

/** The whole call: arguments in, output back — the same text the model saw. */
function CallDetail({ row }: { row: ToolCallRow }) {
  const [result, setResult] = useState(row.Result);
  const [more, setMore] = useState(row.More);
  const [loading, setLoading] = useState(false);
  const [error, setError] = useState("");

  const loadFull = async () => {
    setLoading(true);
    setError("");
    try {
      const full = await api.get<ToolCallRow>(`/api/tool-calls/${row.ID}`);
      setResult(full.Result);
      setMore(false);
    } catch (e) {
      setError(e instanceof Error ? e.message : String(e));
    } finally {
      setLoading(false);
    }
  };

  return (
    <div className="space-y-3">
      {row.Private && <PrivateNote row={row} />}
      <Block
        title="Arguments"
        body={pretty(row.Args)}
        empty={row.Private ? "Not logged." : "No arguments."}
      />
      <Block
        title="Output"
        body={row.Private ? pretty(result) : result}
        empty={
          row.Private
            ? "Not logged."
            : row.OK
              ? "No output recorded for this call — it ran before the console started keeping outputs."
              : "No output recorded for this call."
        }
        footer={
          <>
            {more && (
              <Button size="sm" variant="outline" onClick={loadFull} disabled={loading}>
                {loading ? "Loading…" : "Show full output"}
              </Button>
            )}
            {error && <span className="text-xs text-destructive">{error}</span>}
          </>
        }
      />
      {row.ThreadTS && (
        <p className="text-xs text-muted-foreground">Thread {row.ThreadTS}</p>
      )}
    </div>
  );
}

function Block({
  title,
  body,
  empty,
  footer,
}: {
  title: string;
  body: string;
  empty: string;
  footer?: React.ReactNode;
}) {
  return (
    <div className="space-y-1.5">
      <p className="text-xs font-semibold text-foreground">{title}</p>
      {body ? (
        <pre className="max-h-80 overflow-auto rounded-lg border bg-background p-3 font-mono text-xs leading-relaxed whitespace-pre-wrap break-words">
          {body}
        </pre>
      ) : (
        <p className="text-xs text-muted-foreground">{empty}</p>
      )}
      {footer && <div className="flex items-center gap-2">{footer}</div>}
    </div>
  );
}

function Excerpt({ text, max, locked }: { text: string; max: number; locked?: boolean }) {
  const shown = text.length > max ? text.slice(0, max) + "…" : text;
  if (!locked) {
    return (
      <span className="block truncate font-mono text-xs text-muted-foreground">{shown || "—"}</span>
    );
  }
  return (
    <span className="flex min-w-0 items-center gap-1 font-mono text-xs text-muted-foreground">
      <Lock className="size-3 shrink-0" aria-label="Private" />
      <span className="truncate">{shown}</span>
    </span>
  );
}

/**
 * What the log keeps of a call on somebody's own account, in place of its arguments and result
 * (privateMark in internal/app/private_calls.go). Nobody reads more than this, an admin included:
 * what they asked and what came back are that person's.
 */
type Kept = {
  connection?: string;
  owner?: string;
  method?: string;
  endpoint?: string;
  status?: number;
  bytes?: number;
  error?: string;
  outcome?: string;
};

const NOTE_TOOLS = new Set(["remember_personal", "recall_personal", "forget_personal"]);

function parseKept(text: string): Kept | null {
  try {
    const v: unknown = JSON.parse(text);
    return v && typeof v === "object" && !Array.isArray(v) ? (v as Kept) : null;
  } catch {
    return null;
  }
}

/** The row's line for what a private call asked: the endpoint it called, or whose it was. */
function keptRequest(row: ToolCallRow): string {
  const k = parseKept(row.Args);
  if (k?.endpoint) return `${k.method ?? ""} ${k.endpoint.replace(/^https?:\/\//, "")}`.trim();
  if (k?.connection) return k.connection;
  if (k?.owner) return NOTE_TOOLS.has(row.Name) ? `${k.owner}'s notes` : `${k.owner}'s account`;
  return "Private";
}

/** The row's line for how it went. A refused repeat keeps its refusal, which is nobody's. */
function keptOutcome(result: string): string {
  const k = parseKept(result);
  if (!k) return firstLine(result) || "Private";
  if (k.status) return k.bytes ? `HTTP ${k.status} · ${formatBytes(k.bytes)}` : `HTTP ${k.status}`;
  if (k.error) return k.error;
  if (k.outcome) return k.outcome;
  if (k.bytes !== undefined) return formatBytes(k.bytes);
  return "Private";
}

/** Says whose the call was and why the expansion shows so little of it. */
function PrivateNote({ row }: { row: ToolCallRow }) {
  const k = parseKept(row.Args);
  const whose = k?.owner ? `${k.owner}'s` : "somebody's";
  const connection = k?.connection ? `${k.connection} connection` : "connection";
  let text: string;
  if (!k) {
    text =
      "This call used somebody's own account or their personal notes, and was logged before the console kept whose it was.";
  } else if (NOTE_TOOLS.has(row.Name)) {
    text = `This call used ${whose} personal notes, which are theirs alone. The log keeps whose they were and the size of what came back.`;
  } else if (k.endpoint) {
    text = `This call used ${whose} own ${connection}, so what it asked for and what came back are theirs. The log keeps whose account it was, the endpoint without its query string, the status and the size.`;
  } else {
    text = `This call used ${whose} own ${connection}, so what it asked for and what came back are theirs. The log keeps whose account it was and the size of what came back.`;
  }
  return (
    // whitespace-normal: a table cell does not wrap, and this sits in one.
    <p className="flex gap-2 text-xs whitespace-normal text-muted-foreground">
      <Lock className="mt-0.5 size-3.5 shrink-0" />
      <span>
        <span className="font-medium text-foreground">Private.</span> {text}
      </span>
    </p>
  );
}

/** The row shows the first line of the output; the expansion has the rest. */
function firstLine(result: string): string {
  return result.split("\n").find((l) => l.trim() !== "")?.trim() ?? "";
}

function pretty(args: string): string {
  try {
    return JSON.stringify(JSON.parse(args), null, 2);
  } catch {
    return args;
  }
}
