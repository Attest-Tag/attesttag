import Link from "next/link";
import { ArrowRight } from "lucide-react";
import { cn } from "@/lib/utils";

// Single big-number metric tile, used on the dashboard. Pass `href` to make the
// whole tile a link to the drill-down list for that metric.
export function StatCard({
  label,
  value,
  hint,
  hintTone = "muted",
  href,
  className,
}: {
  label: string;
  value: string | number;
  hint?: string;
  /** `danger` tints the hint for attention-worthy counts (e.g. overdue). */
  hintTone?: "muted" | "danger";
  href?: string;
  className?: string;
}) {
  const body = (
    <>
      <p className="flex items-center gap-1.5 text-sm text-muted-foreground">
        {label}
        {href && (
          <ArrowRight className="size-3.5 shrink-0 opacity-0 transition-opacity group-hover:opacity-100" />
        )}
      </p>
      <p className="font-display tabular-nums mt-1.5 text-[34px] font-medium leading-none tracking-tight text-foreground">
        {value}
      </p>
      {hint && (
        <p
          className={cn(
            "mt-1 text-xs",
            hintTone === "danger" ? "text-danger" : "text-muted-foreground",
          )}
        >
          {hint}
        </p>
      )}
    </>
  );

  const base = "rounded-xl bg-card p-5 shadow-sm border";

  if (!href) return <div className={cn(base, className)}>{body}</div>;

  return (
    <Link
      href={href}
      className={cn(
        base,
        "group block transition-colors hover:bg-accent/40 hover:border-foreground/20 focus-visible:ring-[3px] focus-visible:ring-ring/50 focus-visible:outline-none",
        className,
      )}
    >
      {body}
    </Link>
  );
}
