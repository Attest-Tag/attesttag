"use client";

import { useEffect, useState } from "react";
import { Download, Loader2, ShieldAlert } from "lucide-react";
import { AuditTable, actionLabel } from "@/components/audit/audit-table";
import { ErrorBanner } from "@/components/core/error-banner";
import { PageHeader } from "@/components/core/page-header";
import { SearchField } from "@/components/core/search-field";
import { SegmentedControl } from "@/components/core/segmented-control";
import { TableSkeleton } from "@/components/core/table-skeleton";
import { Button } from "@/components/ui/button";
import {
  Select,
  SelectContent,
  SelectItem,
  SelectTrigger,
  SelectValue,
} from "@/components/ui/select";
import { api, errorMessage, useApi, type AuditEvent, type AuditPage as Page } from "@/lib/api";

const ALL = "__all__";
const PAGE = 100;

/** The same windows the Activity page offers, so a reader moving between the two keeps their footing. */
type Range = "all" | "today" | "7d" | "month";

const RANGES: { value: Range; label: string }[] = [
  { value: "all", label: "All time" },
  { value: "today", label: "Today" },
  { value: "7d", label: "Last 7 days" },
  { value: "month", label: "This month" },
];

function isRange(value: string | null): value is Range {
  return RANGES.some((r) => r.value === value);
}

type Outcome = "all" | "denied";

/** The filter as the API takes it: one place, because the page, the export and the next page all send it. */
function paramsFor(f: {
  range: Range;
  action: string;
  outcome: Outcome;
  actor: string;
  q: string;
}): URLSearchParams {
  const p = new URLSearchParams();
  if (f.range !== "all") p.set("range", f.range);
  if (f.action !== ALL) p.set("action", f.action);
  if (f.outcome === "denied") p.set("outcome", "denied");
  if (f.actor) p.set("actor", f.actor);
  if (f.q) p.set("q", f.q);
  return p;
}

/** What the header promises, which is the window and the narrowing the reader arrived with. */
function describe(range: Range, outcome: Outcome): string {
  const what =
    outcome === "denied"
      ? "refused sign-ins and refused requests"
      : "sign-ins, changes, approvals and exports";
  switch (range) {
    case "today":
      return `Today's ${what}, newest first.`;
    case "7d":
      return `The last 7 days of ${what}, newest first.`;
    case "month":
      return `This month's ${what}, newest first.`;
    default:
      return `Every recorded ${what.replace("refused sign-ins", "refused sign-in")}, newest first.`;
  }
}

export function AuditPage() {
  const [range, setRange] = useState<Range>("all");
  const [action, setAction] = useState<string>(ALL);
  const [outcome, setOutcome] = useState<Outcome>("all");
  // actor is set from the address bar only — a users-list link lands here narrowed to one
  // person — and cleared from the chip it renders as.
  const [actor, setActor] = useState("");
  const [typed, setTyped] = useState("");
  const [q, setQ] = useState("");
  // Pages the reader asked for beyond the first, in the order they were loaded.
  const [older, setOlder] = useState<AuditEvent[]>([]);
  const [olderCursor, setOlderCursor] = useState(0);
  const [loadingOlder, setLoadingOlder] = useState(false);
  const [olderError, setOlderError] = useState("");

  // How links land here: ?actor= from a member's row, ?action= from a settings panel that
  // wants to show its own history, ?outcome=denied from an alert.
  useEffect(() => {
    const params = new URLSearchParams(window.location.search);
    /* eslint-disable react-hooks/set-state-in-effect -- one-shot URL read on mount */
    if (isRange(params.get("range"))) setRange(params.get("range") as Range);
    if (params.get("action")) setAction(params.get("action") as string);
    if (params.get("outcome") === "denied") setOutcome("denied");
    if (params.get("actor")) setActor(params.get("actor") as string);
    if (params.get("q")) {
      setTyped(params.get("q") as string);
      setQ(params.get("q") as string);
    }
    /* eslint-enable react-hooks/set-state-in-effect */
  }, []);

  // The search field is applied a beat after the last keystroke rather than on each one: a
  // request per character is a request per character.
  useEffect(() => {
    const handle = window.setTimeout(() => setQ(typed.trim()), 300);
    return () => window.clearTimeout(handle);
  }, [typed]);

  const filter = { range, action, outcome, actor, q };
  const query = paramsFor(filter);
  const listQuery = new URLSearchParams(query);
  listQuery.set("limit", String(PAGE));
  const page = useApi<Page>(`/api/audit?${listQuery.toString()}`);

  // A new filter starts the list over: pages loaded under the old one do not belong under it.
  const key = query.toString();
  useEffect(() => {
    /* eslint-disable react-hooks/set-state-in-effect -- reset derived paging when the filter changes */
    setOlder([]);
    setOlderCursor(0);
    setOlderError("");
    /* eslint-enable react-hooks/set-state-in-effect */
  }, [key]);

  // The address bar keeps the view, so a reload or a shared link comes back to it.
  useEffect(() => {
    const url = new URL(window.location.href);
    for (const k of ["range", "action", "outcome", "actor", "q"]) url.searchParams.delete(k);
    query.forEach((v, k) => url.searchParams.set(k, v));
    window.history.replaceState(null, "", url);
    // eslint-disable-next-line react-hooks/exhaustive-deps -- `key` is the serialised filter
  }, [key]);

  const first = page.data?.events ?? [];
  const rows = [...first, ...older];
  const cursor = olderCursor || page.data?.next_before || 0;
  const filtered = query.toString() !== "";
  const csvHref = key ? `/api/audit.csv?${key}` : "/api/audit.csv";

  const loadOlder = async () => {
    if (!cursor) return;
    setLoadingOlder(true);
    setOlderError("");
    try {
      const next = new URLSearchParams(listQuery);
      next.set("before", String(cursor));
      const more = await api.get<Page>(`/api/audit?${next.toString()}`);
      setOlder((cur) => [...cur, ...(more.events ?? [])]);
      // 0 from the server means that was the last page; -1 keeps the button away without
      // falling back to the first page's cursor.
      setOlderCursor(more.next_before || -1);
    } catch (e) {
      setOlderError(errorMessage(e));
    } finally {
      setLoadingOlder(false);
    }
  };

  // The menu offers what this organisation's log actually contains, plus whatever the address
  // bar asked for, so a link to an action with no rows yet still reads as a filter.
  const actions = [...(page.data?.actions ?? [])];
  if (action !== ALL && !actions.includes(action)) actions.push(action);

  return (
    <div className="space-y-5">
      <PageHeader
        title="Audit log"
        description={describe(range, outcome)}
        actions={
          <Button variant="outline" asChild>
            <a href={csvHref}>
              <Download className="size-4" />
              Export CSV
            </a>
          </Button>
        }
      />

      {page.error && !page.data && <ErrorBanner message={page.error} onRetry={page.reload} />}

      <div className="flex flex-wrap items-center justify-between gap-3">
        <div className="flex flex-wrap items-center gap-2">
          <SearchField
            value={typed}
            onChange={setTyped}
            placeholder="Search people, targets, addresses…"
            className="w-72"
            clearable
          />
          {actor && (
            <Button variant="secondary" size="sm" onClick={() => setActor("")} title="Showing one person. Click to show everyone.">
              Actor: {actor}
              <span aria-hidden className="ml-1 text-muted-foreground">×</span>
            </Button>
          )}
        </div>
        <div className="flex flex-wrap items-center gap-2">
          <SegmentedControl
            value={outcome}
            onValueChange={(v) => setOutcome(v === "denied" ? "denied" : "all")}
            options={[
              { value: "all", label: "All" },
              { value: "denied", label: "Denied", icon: <ShieldAlert className="size-3.5" /> },
            ]}
          />
          <Select value={range} onValueChange={(v) => isRange(v) && setRange(v)}>
            <SelectTrigger className="w-40" aria-label="Filter by time">
              <SelectValue />
            </SelectTrigger>
            <SelectContent>
              {RANGES.map((r) => (
                <SelectItem key={r.value} value={r.value}>
                  {r.label}
                </SelectItem>
              ))}
            </SelectContent>
          </Select>
          <Select value={action} onValueChange={setAction}>
            <SelectTrigger className="w-64" aria-label="Filter by action">
              <SelectValue />
            </SelectTrigger>
            <SelectContent>
              <SelectItem value={ALL}>All actions</SelectItem>
              {actions.map((a) => (
                <SelectItem key={a} value={a}>
                  {actionLabel(a)}
                  <span className="ml-1.5 font-mono text-[10px] text-muted-foreground">{a}</span>
                </SelectItem>
              ))}
            </SelectContent>
          </Select>
        </div>
      </div>

      {page.loading || !page.data ? (
        <TableSkeleton rows={8} columns={7} />
      ) : (
        <>
          <AuditTable rows={rows} filtered={filtered} />
          {cursor > 0 && (
            <div className="flex items-center gap-3">
              <Button variant="outline" onClick={loadOlder} disabled={loadingOlder}>
                {loadingOlder && <Loader2 className="animate-spin" />}
                Load older
              </Button>
              {olderError && <span className="text-xs text-destructive">{olderError}</span>}
            </div>
          )}
        </>
      )}
    </div>
  );
}
