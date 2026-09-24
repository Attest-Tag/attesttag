import type { LucideIcon } from "lucide-react";
import { cn } from "@/lib/utils";

// Design-system empty state: dashed container, squircle icon tile, title,
// short description, one primary action — see DESIGN.md.
export function EmptyState({
  icon: Icon,
  title,
  description,
  action,
  className,
}: {
  icon: LucideIcon;
  title: string;
  description?: string;
  action?: React.ReactNode;
  className?: string;
}) {
  return (
    <div
      className={cn(
        "flex flex-col items-center justify-center rounded-2xl border border-dashed bg-muted/40 px-6 py-20 text-center",
        className,
      )}
    >
      <div className="flex size-14 items-center justify-center rounded-2xl bg-card shadow-sm border">
        <Icon className="size-6 text-muted-foreground" strokeWidth={1.75} />
      </div>
      <h3 className="mt-5 text-lg font-semibold tracking-[-0.01em] text-foreground">
        {title}
      </h3>
      {description && (
        <p className="mt-2 max-w-lg text-sm leading-relaxed text-muted-foreground">
          {description}
        </p>
      )}
      {action && <div className="mt-6">{action}</div>}
    </div>
  );
}
