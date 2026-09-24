"use client";

import { useMemo, useRef } from "react";
import Link from "next/link";
import { AlertCircle, ArrowRight } from "lucide-react";
import { BarList, ChartLegend, type BarRow } from "@/components/charts/bar-list";
import { DAILY_METRICS, DailyChart, type DailyMetric } from "@/components/charts/daily-chart";
import { ErrorBanner } from "@/components/core/error-banner";
import { PageHeader } from "@/components/core/page-header";
import { SegmentedControl } from "@/components/core/segmented-control";
import { SortHead, sortRows, useTableSort } from "@/components/core/sort-header";
import { StatCard } from "@/components/core/stat-card";
import { useAuth } from "@/components/shell/auth-provider";
import { TableSkeleton } from "@/components/core/table-skeleton";
import { Card, CardContent, CardHeader, CardTitle } from "@/components/ui/card";
import { Skeleton } from "@/components/ui/skeleton";
import {
  Table,
  TableBody,
  TableCell,
  TableHeader,
  TableRow,
} from "@/components/ui/table";
import { useStickyTab } from "@/hooks/use-sticky-tab";
import { useApi, type Overview, type RecentError, type UsageRow } from "@/lib/api";
import { formatDateTime, formatNumber, formatUSD } from "@/lib/format";

// Calls are the bar and failures are a slice of it, so the two colours are doing different jobs:
// iris is identity (this is the tool's month) and red is status (this part of it went wrong).
const CALLS_COLOR = "var(--chart-2)";
const FAILED_COLOR = "var(--danger)";

export function OverviewPage() {
  const { data, loading, error, reload } = useApi<Overview>("/api/overview");
  // The balance rides on the session the shell already fetches, rather than a field added to
  // /api/overview: it is the same number the banner reads, and one source keeps them agreeing.
  const { me } = useAuth();
  const plan = me?.plan;
  const charts = data?.charts;

  const [metric, setMetric] = useStickyTab("overview-metric", "turns", [
    "turns",
    "cost",
    "tokens",
  ]);

  return (
    <div className="space-y-5">
      <PageHeader
        title="Overview"
        description="Spend, usage and what the bot has to work with."
      />

      {error && !data && <ErrorBanner message={error} onRetry={reload} />}

      <div className="grid gap-3 sm:grid-cols-2 lg:grid-cols-4">
        {loading || !data ? (
          Array.from({ length: 8 }).map((_, i) => (
            <div key={i} className="rounded-xl border bg-card p-5 shadow-sm">
              <Skeleton className="h-3.5 w-24" />
              <Skeleton className="mt-3 h-8 w-20" />
              <Skeleton className="mt-2 h-3 w-16" />
            </div>
          ))
        ) : (
          <>
            <StatCard
              label="Spend this month"
              value={formatUSD(data.month_spend_usd)}
              hint={
                data.budget_usd > 0
                  ? `of ${formatUSD(data.budget_usd)} ${data.plan === "free" ? "free-plan " : ""}budget · ${Math.round(
                      (data.month_spend_usd / data.budget_usd) * 100,
                    )}%`
                  : "No monthly budget set"
              }
              hintTone={
                data.budget_usd > 0 && data.month_spend_usd >= data.budget_usd
                  ? "danger"
                  : "muted"
              }
              href="/activity?range=month"
            />
            {/* Credit only where there is any, and then in place of Turns today: the grid is four
                across, so eight tiles are two full rows and a ninth sat alone on a third. Turns
                today is the one that gives way because Turns, last 7 days says the same thing
                over a longer window. Credit is second because money is what people open this
                page to check, and it is the harder of the two limits beside it. */}
            {plan?.credit_enabled && (
              <StatCard
                label="Credit balance"
                value={formatUSD(plan.credit_balance_usd ?? 0)}
                hint={
                  plan.paused_by === "credit"
                    ? "Spent — the bot has stopped"
                    : "Prepaid model spend"
                }
                hintTone={plan.paused_by === "credit" ? "danger" : "muted"}
                href="/settings?tab=billing"
              />
            )}
            {!plan?.credit_enabled && (
              <StatCard
                label="Turns today"
                value={formatNumber(data.turns_today)}
                href="/activity?range=today"
              />
            )}
            <StatCard
              label="Turns, last 7 days"
              value={formatNumber(data.turns_7d)}
              href="/activity?range=7d"
            />
            <StatCard
              label="Documents"
              value={formatNumber(data.docs)}
              hint={`${formatNumber(data.chunks)} chunks indexed`}
              href="/documents"
            />
            <StatCard
              label="Access bundles"
              value={formatNumber(data.bundles)}
              hint={`${formatNumber(data.connections)} connections`}
              href="/bundles"
            />
            <StatCard
              label="Slack scopes"
              value={formatNumber(data.scopes)}
              hint="Workspace and channels"
              href="/workspaces"
            />
            <StatCard
              label="Routines"
              value={formatNumber(data.routines)}
              hint="Enabled and scheduled"
              href="/routines"
            />
            <StatCard
              label="Memories"
              value={formatNumber(data.memories)}
              href="/memory"
            />
          </>
        )}
      </div>

      {/* The tiles say where the account stands; this says how it got there. It is the one chart
          wide enough to carry a month, so it goes above the breakdowns rather than beside them. */}
      <Card>
        <CardHeader className="flex flex-row items-center justify-between gap-3 space-y-0">
          <CardTitle>Last 30 days</CardTitle>
          <SegmentedControl
            value={metric}
            onValueChange={setMetric}
            options={(Object.keys(DAILY_METRICS) as DailyMetric[]).map((key) => ({
              value: key,
              label: DAILY_METRICS[key].label,
            }))}
          />
        </CardHeader>
        <CardContent>
          {loading || !data ? (
            <Skeleton className="h-48 w-full" />
          ) : charts?.daily?.length ? (
            <DailyChart days={charts.daily} metric={metric as DailyMetric} />
          ) : (
            <p className="text-sm text-muted-foreground">No usage recorded yet.</p>
          )}
        </CardContent>
      </Card>

      {/* minmax(0, …) columns, stacked too, so a wide table scrolls inside its card rather than
          widening the column: a bare fr or auto track grows to fit its content, and a long channel
          name pushed the errors off the page. */}
      <div className="grid grid-cols-1 gap-5 lg:grid-cols-[minmax(0,3fr)_minmax(0,2fr)]">
        <Card>
          <CardHeader>
            <CardTitle>Top channels this month</CardTitle>
          </CardHeader>
          {/* A column, so the table can grow into height the errors card gives the row. */}
          <CardContent className="flex flex-1 flex-col">
            {loading || !data ? (
              <TableSkeleton rows={4} columns={5} />
            ) : (
              <ChannelTable rows={data.top_channels} />
            )}
          </CardContent>
        </Card>

        <Card>
          <CardHeader>
            <CardTitle>Recent errors</CardTitle>
          </CardHeader>
          <CardContent>
            {loading || !data ? (
              <div className="space-y-2">
                <Skeleton className="h-3.5 w-full" />
                <Skeleton className="h-3.5 w-5/6" />
                <Skeleton className="h-3.5 w-2/3" />
              </div>
            ) : !data.recent_errors || data.recent_errors.length === 0 ? (
              <p className="text-sm text-muted-foreground">
                Nothing has failed recently.
              </p>
            ) : (
              <>
                <ul className="divide-y">
                  {data.recent_errors.map((err) => (
                    <ErrorRow key={err.id} error={err} />
                  ))}
                </ul>
                <Link
                  href={activityErrorsHref()}
                  className="mt-3 inline-flex items-center gap-1 text-xs text-muted-foreground hover:text-foreground"
                >
                  See every failure on Activity
                  <ArrowRight className="size-3.5" />
                </Link>
              </>
            )}
          </CardContent>
        </Card>
      </div>

      {/* What the month is made of, the two ways the table above cannot say: which model the
          money went to, and which tools the bot actually reached for. */}
      <div className="grid gap-5 lg:grid-cols-2">
        <Card>
          <CardHeader>
            <CardTitle>Spend by model</CardTitle>
          </CardHeader>
          <CardContent>
            {loading || !data ? (
              <Skeleton className="h-24 w-full" />
            ) : (
              <ModelBars rows={charts?.models} total={data.month_spend_usd} />
            )}
          </CardContent>
        </Card>

        <Card>
          <CardHeader>
            <CardTitle>Tools this month</CardTitle>
          </CardHeader>
          <CardContent>
            {loading || !data ? (
              <Skeleton className="h-24 w-full" />
            ) : (
              <ToolBars rows={charts?.tools} />
            )}
          </CardContent>
        </Card>
      </div>
    </div>
  );
}

/** The columns of the channel table, and which way each one opens when it is first clicked. */
const CHANNEL_COLUMNS = {
  channel: "text",
  turns: "number",
  in: "number",
  out: "number",
  cost: "number",
} as const;

type ChannelColumn = keyof typeof CHANNEL_COLUMNS;

/**
 * This month's channels, sortable.
 *
 * The server sends them dearest-first, which is the right default and the wrong only answer: the
 * channel that costs the most is rarely the one that talks the most, and "who is using this" and
 * "what is this costing" are two different questions asked of the same five columns.
 */
function ChannelTable({ rows }: { rows: UsageRow[] | null }) {
  const { sort, toggle } = useTableSort<ChannelColumn>("overview-channels", CHANNEL_COLUMNS, {
    by: "cost",
    dir: "desc",
  });
  // A new order starts at its top: re-sorting halfway down the list would otherwise land the
  // reader in the middle of an order they never saw the start of.
  const scroller = useRef<HTMLDivElement>(null);
  const resort = (by: ChannelColumn) => {
    toggle(by);
    scroller.current?.scrollTo({ top: 0 });
  };
  // The server names each row, because only it can say who a DM is with; the scope list this
  // used to be read from knew only channels, so a DM showed its id. Sorting by name means sorting
  // by the name on screen, not the id underneath it: the reader is looking at "#support", and an
  // order that put it under C0… would look like no order at all.
  const named = useMemo(
    () => (rows ?? []).map((row) => ({ row, name: row.ChannelName || row.Channel })),
    [rows],
  );
  const sorted = useMemo(
    () =>
      sortRows(named, sort, (item, by) =>
        by === "channel"
          ? item.name
          : by === "turns"
            ? item.row.Turns
            : by === "in"
              ? item.row.In
              : by === "out"
                ? item.row.Out
                : item.row.Cost,
      ),
    [named, sort],
  );

  if (sorted.length === 0) {
    return (
      <p className="text-sm text-muted-foreground">
        No usage yet this month. Mention the bot in a channel and its turns will show up here.
      </p>
    );
  }

  // A month can run to dozens of channels, so the table scrolls rather than stretching the row to
  // fit every one. It asks the row for about eight rows of height at most, then grows into
  // whatever height the row has spare (the errors beside it are often taller), but never past its
  // own last row, so a short month is not framed in empty border. This div is the scroller, not
  // the table's own container, so the header sticks to it. The header stays in view because its
  // labels are also the sort controls. It needs an opaque background (the tint over the card,
  // mixed rather than see-through) and draws its rule as a shadow, because a collapsed border
  // stays with the table when the cells above it stick.
  return (
    <div
      ref={scroller}
      className="max-h-fit grow basis-96 overflow-auto rounded-lg border *:data-[slot=table-container]:overflow-visible"
    >
      <Table>
        <TableHeader className="[&_th]:sticky [&_th]:top-0 [&_th]:bg-[color-mix(in_srgb,var(--muted)_50%,var(--card))] [&_th]:shadow-[inset_0_-1px_0_var(--border)] [&_tr]:border-b-0">
          <TableRow className="border-b-0">
            <SortHead by="channel" sort={sort} onSort={resort}>
              Channel
            </SortHead>
            <SortHead by="turns" sort={sort} onSort={resort} align="right">
              Turns
            </SortHead>
            <SortHead by="in" sort={sort} onSort={resort} align="right">
              In
            </SortHead>
            <SortHead by="out" sort={sort} onSort={resort} align="right">
              Out
            </SortHead>
            <SortHead by="cost" sort={sort} onSort={resort} align="right">
              Cost
            </SortHead>
          </TableRow>
        </TableHeader>
        <TableBody>
          {sorted.map(({ row, name }) => (
            <TableRow key={`${row.TeamID}:${row.Channel}`}>
              {/* Takes the width the numbers leave and cuts a long name short. max-w-0 is what
                  lets a table cell shrink below its text; the title carries the whole name. */}
              <TableCell className="w-full max-w-0 font-medium">
                {row.Channel ? (
                  <Link
                    href={`/activity?channel=${encodeURIComponent(row.Channel)}&range=month`}
                    className="block truncate hover:underline"
                    title={name}
                  >
                    {name}
                  </Link>
                ) : (
                  // Filed under no channel, so there is no Activity filter to link to.
                  <span
                    className="block truncate"
                    title="Deciding whether a message needs a reply, where “Read every message” is on"
                  >
                    {name}
                  </span>
                )}
              </TableCell>
              <TableCell className="text-right tabular-nums">
                {formatNumber(row.Turns)}
              </TableCell>
              <TableCell className="text-right tabular-nums">{formatNumber(row.In)}</TableCell>
              <TableCell className="text-right tabular-nums">{formatNumber(row.Out)}</TableCell>
              <TableCell className="text-right tabular-nums">{formatUSD(row.Cost)}</TableCell>
            </TableRow>
          ))}
        </TableBody>
      </Table>
    </div>
  );
}

/** Where the month's money went. One measure, one colour, so no legend. */
function ModelBars({
  rows,
  total,
}: {
  rows: NonNullable<Overview["charts"]>["models"] | undefined;
  total: number;
}) {
  if (!rows || rows.length === 0) {
    return <p className="text-sm text-muted-foreground">Nothing has been spent this month.</p>;
  }
  const bars: BarRow[] = rows.map((m) => ({
    key: m.model || "unnamed",
    // A turn that never named a model still cost money, so it is labelled rather than dropped:
    // the bars have to add up to the tile above them.
    label: m.model || "Not recorded",
    value: m.cost,
    valueLabel: formatUSD(m.cost),
    hint: `${formatNumber(m.turns)} turns · ${total > 0 ? Math.round((m.cost / total) * 100) : 0}% of the month`,
  }));
  return <BarList rows={bars} />;
}

/** What the bot reached for, and how much of it came back an error. */
function ToolBars({ rows }: { rows: NonNullable<Overview["charts"]>["tools"] | undefined }) {
  if (!rows || rows.length === 0) {
    return (
      <p className="text-sm text-muted-foreground">
        The bot has not run a tool this month. Tools are how it searches documents, reads a
        repository and calls a connected API.
      </p>
    );
  }
  const anyFailed = rows.some((t) => t.failed > 0);
  const bars: BarRow[] = rows.map((t) => ({
    key: t.name,
    label: <span className="font-mono">{t.name}</span>,
    value: t.calls,
    valueLabel: formatNumber(t.calls),
    segments: [
      { key: "ok", value: t.calls - t.failed, color: CALLS_COLOR },
      { key: "failed", value: t.failed, color: FAILED_COLOR },
    ],
    hint:
      t.failed > 0 ? (
        <Link href={activityErrorsHref()} className="text-danger hover:underline">
          {formatNumber(t.failed)} failed
        </Link>
      ) : undefined,
  }));
  return (
    <>
      <BarList rows={bars} />
      {anyFailed && (
        <ChartLegend
          className="mt-4"
          items={[
            { label: "Calls", color: CALLS_COLOR },
            { label: "Failed", color: FAILED_COLOR },
          ]}
        />
      )}
    </>
  );
}

/** Where Activity lands when it is asked for failures only, optionally on one call. */
function activityErrorsHref(callID?: number): string {
  const q = new URLSearchParams({ tab: "tools", errors: "1" });
  if (callID) q.set("call", String(callID));
  return `/activity?${q.toString()}`;
}

/** A failed call, linked to its own row on Activity: same time, tool and arguments. */
function ErrorRow({ error }: { error: RecentError }) {
  return (
    <li className="first:pt-0 last:pb-0">
      <Link
        href={activityErrorsHref(error.id)}
        className="-mx-2 flex gap-2 rounded-md px-2 py-2 text-xs transition-colors hover:bg-accent/50"
      >
        <AlertCircle className="mt-0.5 size-3.5 shrink-0 text-danger" />
        <span className="min-w-0 flex-1">
          <span className="flex flex-wrap items-baseline gap-x-2">
            <span className="font-mono font-medium text-foreground">{error.name}</span>
            <span className="text-muted-foreground">{formatDateTime(error.at)}</span>
          </span>
          {error.args && (
            <span className="mt-0.5 block break-words font-mono text-muted-foreground">
              {error.args}
            </span>
          )}
        </span>
      </Link>
    </li>
  );
}
