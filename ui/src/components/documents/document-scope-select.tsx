"use client";

import {
  Select,
  SelectContent,
  SelectItem,
  SelectTrigger,
  SelectValue,
} from "@/components/ui/select";
import type { Scope } from "@/lib/api";
import { cn } from "@/lib/utils";

const WORKSPACE = "__workspace__";

// Where a document applies: everywhere (empty scope) or one channel.
// Radix Select can't carry an empty-string item, hence the sentinel.
//
// A null value is the folder case: the documents underneath do not agree on one scope, so
// the trigger stands empty and shows the placeholder until somebody picks. Radix reads a
// value of "" as no value at all, which is exactly the state wanted there.
export function DocumentScopeSelect({
  value,
  onChange,
  scopes,
  compact,
  placeholder,
  "aria-label": ariaLabel,
}: {
  value: string | null;
  onChange: (scope: string) => void;
  scopes: Scope[] | undefined;
  compact?: boolean;
  placeholder?: string;
  "aria-label"?: string;
}) {
  const channels = (scopes ?? []).filter((s) => s.kind === "channel");
  const known = value === null || value === "" || channels.some((c) => c.slack_id === value);
  return (
    <Select
      value={value === null ? "" : value === "" ? WORKSPACE : value}
      onValueChange={(v) => onChange(v === WORKSPACE ? "" : v)}
    >
      <SelectTrigger
        aria-label={ariaLabel}
        size={compact ? "sm" : "default"}
        className={cn(compact ? "h-7 min-w-36 border-transparent bg-transparent text-xs shadow-none hover:bg-secondary/60" : "w-56")}
      >
        <SelectValue placeholder={placeholder ?? "Workspace-wide"} />
      </SelectTrigger>
      <SelectContent>
        <SelectItem value={WORKSPACE}>Workspace-wide</SelectItem>
        {channels.map((c) => (
          <SelectItem key={c.id} value={c.slack_id}>
            {c.name}
          </SelectItem>
        ))}
        {!known && value !== null && <SelectItem value={value}>{value}</SelectItem>}
      </SelectContent>
    </Select>
  );
}
