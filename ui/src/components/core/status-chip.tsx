import { cn } from "@/lib/utils";

export type StatusChipVariant =
  | "success" // green — Active, Approved, Accepted
  | "info" // blue — In review, Filed, Onboarding
  | "warning" // amber — Pending, RFE, Suggested
  | "danger" // red — Overdue, Denied, Gap
  | "ai" // purple — AI draft
  | "neutral"; // gray

const styles: Record<StatusChipVariant, string> = {
  success: "bg-success-soft text-success-text",
  info: "bg-info-soft text-info",
  warning: "bg-warning-soft text-warning",
  danger: "bg-danger-soft text-danger",
  ai: "bg-ai-soft text-ai",
  neutral: "bg-secondary text-muted-foreground",
};

const dots: Record<StatusChipVariant, string> = {
  success: "bg-success",
  info: "bg-info",
  warning: "bg-warning",
  danger: "bg-danger",
  ai: "bg-ai",
  neutral: "bg-muted-foreground",
};

export function StatusChip({
  variant = "neutral",
  children,
  className,
}: {
  variant?: StatusChipVariant;
  children: React.ReactNode;
  className?: string;
}) {
  return (
    <span
      className={cn(
        // Never wraps: the labels are two words at most, and a chip given a
        // narrow slot has to overflow it rather than grow into a second line
        // that breaks the row height it sits in.
        "inline-flex h-[22px] items-center gap-1.5 rounded-sm px-2.5 text-xs font-medium whitespace-nowrap",
        styles[variant],
        className,
      )}
    >
      <span className={cn("size-1.5 rounded-full", dots[variant])} />
      {children}
    </span>
  );
}
