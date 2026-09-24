"use client";

import type { ReactNode } from "react";
import { cn } from "@/lib/utils";

export type BarSegment = {
  key: string;
  value: number;
  /** A CSS colour, normally one of the theme's chart or status tokens. */
  color: string;
};

export type BarRow = {
  key: string;
  label: ReactNode;
  /** What the bar's length is proportional to, against the longest row. */
  value: number;
  /** The figure printed at the row's right — the exact one, never a rounded axis label. */
  valueLabel: string;
  /** A second line under the bar: a share, a count, a link to the rows behind it. */
  hint?: ReactNode;
  /** Splits `value` left to right. One segment is the default and needs no legend. */
  segments?: BarSegment[];
};

/**
 * A ranked list of bars sharing one baseline — the form for "which of these is biggest", where
 * the category names have to be readable and there are fewer than a dozen of them.
 *
 * The bars carry the colour and the text does not: a label in a chart hue is unreadable at 13px
 * against the card, and it would say nothing the bar beside it has not already said.
 */
export function BarList({ rows, className }: { rows: BarRow[]; className?: string }) {
  const max = Math.max(...rows.map((r) => r.value), 0);
  return (
    <ul className={cn("space-y-3", className)}>
      {rows.map((row) => {
        const segments = row.segments?.length
          ? row.segments
          : [{ key: row.key, value: row.value, color: "var(--chart-2)" }];
        // The rounded end belongs to whichever segment actually ends the bar, so a row with no
        // failures does not get a clipped corner where the empty segment would have been.
        const last = segments.reduce((at, s, i) => (s.value > 0 ? i : at), 0);
        return (
          <li key={row.key}>
            <div className="flex items-baseline justify-between gap-3">
              <span className="min-w-0 truncate text-sm font-medium text-foreground">
                {row.label}
              </span>
              <span className="shrink-0 text-sm tabular-nums text-muted-foreground">
                {row.valueLabel}
              </span>
            </div>
            <div className="relative mt-1.5 h-[6px] w-full rounded-[2px] bg-muted">
              <div
                className="absolute inset-y-0 left-0 flex gap-[2px]"
                style={{ width: `${max > 0 ? (row.value / max) * 100 : 0}%` }}
              >
                {segments.map((s, i) => (
                  <div
                    key={s.key}
                    className={cn("h-full", i === last && "rounded-r-[4px]")}
                    style={{
                      background: s.color,
                      width: `${row.value > 0 ? (s.value / row.value) * 100 : 0}%`,
                    }}
                  />
                ))}
              </div>
            </div>
            {row.hint && <p className="mt-1 text-xs text-muted-foreground">{row.hint}</p>}
          </li>
        );
      })}
    </ul>
  );
}

/** The identity channel that is not colour. Present whenever a bar carries more than one thing. */
export function ChartLegend({
  items,
  className,
}: {
  items: { label: string; color: string }[];
  className?: string;
}) {
  return (
    <ul className={cn("flex flex-wrap items-center gap-3", className)}>
      {items.map((item) => (
        <li key={item.label} className="flex items-center gap-1.5 text-xs text-muted-foreground">
          <span
            aria-hidden
            className="size-2 shrink-0 rounded-[1px]"
            style={{ background: item.color }}
          />
          {item.label}
        </li>
      ))}
    </ul>
  );
}
