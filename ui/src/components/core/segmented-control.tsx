"use client";

import Link from "next/link";
import { cn } from "@/lib/utils";

export type SegmentedControlOption<T extends string> = {
  value: T;
  label: React.ReactNode;
  /** Optional leading icon — pass sized (size-3.5). */
  icon?: React.ReactNode;
  disabled?: boolean;
  /** Link mode: renders <Link> instead of a button; value marks active. */
  href?: string;
};

// Compact view toggle (2-5 options) — the only sanctioned segmented control.
// For route-level nav use TabNav; for stateful panels use ui/tabs.
// See DESIGN.md.
export function SegmentedControl<T extends string>({
  value,
  options,
  onValueChange,
  className,
}: {
  value: T;
  options: SegmentedControlOption<T>[];
  onValueChange?: (value: T) => void;
  className?: string;
}) {
  const itemClass = (active: boolean) =>
    cn(
      "flex items-center gap-1.5 rounded-md px-2.5 py-1 text-xs font-medium transition-colors disabled:opacity-40",
      active
        ? "bg-card text-foreground border"
        : "text-muted-foreground hover:text-foreground",
    );

  return (
    <div
      className={cn(
        "flex items-center gap-0.5 rounded-lg border bg-muted/50 p-0.5",
        className,
      )}
    >
      {options.map((option) =>
        option.href ? (
          <Link
            key={option.value}
            href={option.href}
            className={itemClass(option.value === value)}
          >
            {option.icon}
            {option.label}
          </Link>
        ) : (
          <button
            key={option.value}
            type="button"
            disabled={option.disabled}
            onClick={() => onValueChange?.(option.value)}
            className={itemClass(option.value === value)}
          >
            {option.icon}
            {option.label}
          </button>
        ),
      )}
    </div>
  );
}
