"use client";

import { useState } from "react";
import type { DayUsage } from "@/lib/api";
import { formatCompact, formatDayKey, formatNumber, formatUSD } from "@/lib/format";
import { cn } from "@/lib/utils";

export type DailyMetric = "turns" | "cost" | "tokens";

// One series, one colour: the chart's title says what is plotted, so there is nothing for a
// legend to disambiguate. --chart-2 is the iris the rest of the console spends on primary
// actions, and it has its own step in the dark theme rather than a flipped one.
const MARK = "var(--chart-2)";

type Metric = {
  label: string;
  of: (d: DayUsage) => number;
  /** The exact figure, for the tooltip and the totals. */
  exact: (n: number) => string;
  /** The rounded figure, for an axis tick where the eye only wants the scale. */
  tick: (n: number) => string;
  /** What the figure is, when the sentence needs saying. Money says itself. */
  noun: string;
  /** How the biggest day is described. */
  peak: string;
};

export const DAILY_METRICS: Record<DailyMetric, Metric> = {
  turns: {
    label: "Turns",
    of: (d) => d.turns,
    exact: formatNumber,
    tick: formatCompact,
    noun: "turns",
    peak: "busiest",
  },
  cost: {
    label: "Cost",
    of: (d) => d.cost,
    exact: formatUSD,
    tick: (n) => (n >= 1000 ? `$${formatCompact(n)}` : formatUSD(n)),
    noun: "",
    peak: "dearest",
  },
  tokens: {
    label: "Tokens",
    of: (d) => d.in + d.out,
    exact: formatNumber,
    tick: formatCompact,
    noun: "tokens",
    peak: "busiest",
  },
};

/** "884 turns", "$18.42" — the noun only where the figure does not carry it. */
function say(m: Metric, n: number): string {
  return m.noun ? `${m.exact(n)} ${m.noun}` : m.exact(n);
}

/**
 * Turns per day over the window the server sent, as columns.
 *
 * The metric only repaints the bars: whichever one is showing, the tooltip carries all three, so
 * "why was that Tuesday expensive" is answered by pointing at the Tuesday rather than by
 * switching the chart and finding it again.
 */
export function DailyChart({
  days,
  metric,
  className,
}: {
  days: DayUsage[];
  metric: DailyMetric;
  className?: string;
}) {
  const [hover, setHover] = useState<number | null>(null);
  const m = DAILY_METRICS[metric];

  const values = days.map(m.of);
  const total = values.reduce((a, b) => a + b, 0);

  // Before anything is measured: a summary of nothing has no busiest day to name, and there is
  // no axis worth drawing for a row of zeroes.
  if (total === 0) {
    return (
      <p className={cn("text-sm text-muted-foreground", className)}>
        {days.length > 0 ? `No turns in the last ${days.length} days.` : "No turns yet."}
      </p>
    );
  }

  const top = niceMax(Math.max(...values, 0));
  // Two hairlines and the baseline. More than that is chrome competing with the data, and the
  // tooltip is where an exact figure comes from anyway.
  const ticks = [top, top / 2];

  const peak = values.reduce((best, v, i) => (v > values[best] ? i : best), 0);
  // An average is a rounded figure by nature, and "29.667 a day" reads as a measurement nobody
  // took. Counts round to whole things; money keeps its cents, which are the unit it is in.
  const average = metric === "cost" ? total / days.length : Math.round(total / days.length);
  const summary = `${say(m, total)} over ${days.length} days · ${say(m, average)} a day on average · ${m.peak} ${formatDayKey(days[peak].day)} (${m.exact(values[peak])})`;

  return (
    <div className={className}>
      <p className="mb-3 text-xs text-muted-foreground">{summary}</p>
      {/* The top tick's label is centred on its gridline, so half of it sits above the plot.
          This padding is the room it needs; without it the figure lands on the summary line. */}
      <div className="pt-2">
        {/* role="img" with the summary as its label: a reader who cannot see the bars gets the
            shape of the month in a sentence — the total, the average and the busiest day — which
            is what the chart is for. The rows themselves are on Activity, a link away. */}
        <div
          className="relative h-[160px] pl-10"
          role="img"
          aria-label={`${m.label} per day. ${summary}.`}
        >
          {ticks.map((t) => (
            <div
              key={t}
              className="pointer-events-none absolute inset-x-0 left-10 h-px bg-border"
              style={{ bottom: `${(t / top) * 100}%` }}
            />
          ))}
          {ticks.map((t) => (
            <span
              key={t}
              className="pointer-events-none absolute left-0 w-8 translate-y-1/2 text-right text-[11px] tabular-nums text-muted-foreground"
              style={{ bottom: `${(t / top) * 100}%` }}
            >
              {m.tick(t)}
            </span>
          ))}
          <div className="relative flex h-full items-end gap-[2px] border-b">
            {days.map((d, i) => {
              const v = m.of(d);
              return (
                <div
                  key={d.day}
                  className="group relative flex h-full flex-1 items-end justify-center"
                  onMouseEnter={() => setHover(i)}
                  onMouseLeave={() => setHover((at) => (at === i ? null : at))}
                >
                  {/* The hover target is the whole column, not the bar: a day with no turns has
                      no bar to point at, and "nothing happened" is an answer somebody came for. */}
                  <div className="pointer-events-none absolute inset-0 rounded-[2px] bg-muted/60 opacity-0 group-hover:opacity-100" />
                  <div
                    className="relative w-full max-w-[24px] rounded-t-[4px] transition-opacity group-hover:opacity-80"
                    style={{
                      background: MARK,
                      height: `${(v / top) * 100}%`,
                      // A day that cost a fraction of the busiest one still happened, and two
                      // pixels is the least that reads as a mark. Zero stays zero.
                      minHeight: v > 0 ? 2 : 0,
                    }}
                  />
                </div>
              );
            })}
            {hover !== null && (
              <DayTooltip days={days} at={hover} share={m.of(days[hover]) / top} />
            )}
          </div>
        </div>
        <div className="mt-2 flex justify-between pl-10 text-[11px] text-muted-foreground">
          <span>{formatDayKey(days[0].day)}</span>
          <span>{formatDayKey(days[Math.floor(days.length / 2)].day)}</span>
          <span>{formatDayKey(days[days.length - 1].day)}</span>
        </div>
      </div>
    </div>
  );
}

/**
 * The hovered day, in full. It sits inside the plot area rather than above the card, so it can
 * never push the layout around, and it swaps to the floor when the bar it belongs to is tall
 * enough to be underneath it.
 */
function DayTooltip({ days, at, share }: { days: DayUsage[]; at: number; share: number }) {
  const d = days[at];
  const frac = (at + 0.5) / days.length;
  const rows: [string, string][] = [
    ["Turns", formatNumber(d.turns)],
    ["Cost", formatUSD(d.cost)],
    ["Tokens", `${formatNumber(d.in)} in · ${formatNumber(d.out)} out`],
  ];
  return (
    <div
      className={cn(
        "pointer-events-none absolute z-10 w-max rounded-md border bg-popover px-2.5 py-2 text-xs shadow-md",
        share > 0.55 ? "bottom-1" : "top-0",
      )}
      style={{
        left: `${frac * 100}%`,
        transform: `translateX(${frac < 0.15 ? "0" : frac > 0.85 ? "-100%" : "-50%"})`,
      }}
    >
      <p className="font-medium text-foreground">
        {formatDayKey(d.day, { weekday: "short", month: "short", day: "numeric" })}
      </p>
      <dl className="mt-1 space-y-0.5">
        {rows.map(([label, value]) => (
          <div key={label} className="flex gap-3">
            <dt className="text-muted-foreground">{label}</dt>
            <dd className="ml-auto tabular-nums text-foreground">{value}</dd>
          </div>
        ))}
      </dl>
    </div>
  );
}

/** The next round number at or above v, so the top gridline is a figure and not a maximum. */
function niceMax(v: number): number {
  if (v <= 0) return 1;
  const base = Math.pow(10, Math.floor(Math.log10(v)));
  for (const step of [1, 2, 2.5, 5]) {
    if (v <= step * base) return step * base;
  }
  return 10 * base;
}
