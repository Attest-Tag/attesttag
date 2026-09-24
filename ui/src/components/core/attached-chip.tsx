"use client";

import type { ComponentType } from "react";
import { X } from "lucide-react";

// One thing attached somewhere — a bundle, or a connection as bundle/name — with the button that
// takes it off again. Shared by a channel's Access group and the approval tiers, so an attached
// bundle looks the same wherever it was attached.
export function AttachedChip({
  icon: Icon,
  prefix,
  label,
  disabled,
  onDetach,
}: {
  icon: ComponentType<{ className?: string }>;
  prefix?: string;
  label: string;
  disabled: boolean;
  onDetach: () => void;
}) {
  return (
    <span className="inline-flex h-7 max-w-full items-center gap-1.5 rounded-md border bg-card pl-2 pr-1 text-xs font-medium">
      <Icon className="size-3.5 shrink-0 text-muted-foreground" />
      <span className="truncate">
        {prefix && <span className="font-normal text-muted-foreground">{prefix}</span>}
        {label}
      </span>
      <button
        type="button"
        aria-label={`Detach ${prefix ?? ""}${label}`}
        disabled={disabled}
        onClick={onDetach}
        className="flex size-5 shrink-0 items-center justify-center rounded-sm text-muted-foreground hover:bg-foreground/10 hover:text-foreground"
      >
        <X className="size-3" />
      </button>
    </span>
  );
}
