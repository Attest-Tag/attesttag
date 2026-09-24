"use client";

import { useId, useState } from "react";
import { ChevronRight } from "lucide-react";
import { cn } from "@/lib/utils";

/**
 * A summary line that opens to reveal its detail.
 *
 * The line carries its own count, because "how much is in here?" is the
 * question that makes people open one — a bare "Show more" forces a click just
 * to learn there was nothing worth the click.
 */
export function Disclosure({
  label,
  summary,
  defaultOpen = false,
  children,
  className,
  contentClassName,
  onOpenChange,
}: {
  label: React.ReactNode;
  /** Sits after the label, quieter: a count, a source list. */
  summary?: React.ReactNode;
  defaultOpen?: boolean;
  /** Told when it opens or closes, for detail that is only worth fetching once someone looks. */
  onOpenChange?: (open: boolean) => void;
  children: React.ReactNode;
  className?: string;
  contentClassName?: string;
}) {
  const [open, setOpen] = useState(defaultOpen);
  const id = useId();
  return (
    <div className={cn("min-w-0", className)}>
      <button
        type="button"
        aria-expanded={open}
        aria-controls={id}
        onClick={() => {
          const next = !open;
          setOpen(next);
          onOpenChange?.(next);
        }}
        className="flex w-full items-center gap-2 text-left text-sm font-semibold text-foreground"
      >
        <ChevronRight
          className={cn(
            "size-4 shrink-0 text-muted-foreground transition-transform",
            open && "rotate-90",
          )}
        />
        <span className="truncate">{label}</span>
        {summary && <span className="min-w-0 truncate text-xs font-normal text-muted-foreground">{summary}</span>}
      </button>
      {open && (
        <div id={id} className={cn("pt-3", contentClassName)}>
          {children}
        </div>
      )}
    </div>
  );
}
