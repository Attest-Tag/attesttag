"use client";

import { useRef, useState } from "react";
import { Plus, X } from "lucide-react";
import { Button } from "@/components/ui/button";
import { Textarea } from "@/components/ui/textarea";
import { cn } from "@/lib/utils";

export const ALLOW_RULES_MAX = 50;
export const ALLOW_RULE_MAX_LEN = 1024;

/** The stored form is a JSON array of strings; anything else reads as no rules. */
export function parseAllowRules(raw: string | null | undefined): string[] {
  if (!raw) return [];
  try {
    const v: unknown = JSON.parse(raw);
    return Array.isArray(v) ? v.filter((x): x is string => typeof x === "string") : [];
  } catch {
    return [];
  }
}

// Plain-sentence rules that pre-approve writes: one row per rule, "Add rule"
// at the bottom and the count against the limits. Rows read as text rather
// than inputs: a rule is written once and read many times.
export function AllowRulesEditor({
  id,
  value,
  onChange,
  disabled,
}: {
  id?: string;
  value: string[];
  onChange: (next: string[]) => void;
  disabled?: boolean;
}) {
  const [adding, setAdding] = useState(false);
  const [draft, setDraft] = useState("");
  const draftRef = useRef<HTMLTextAreaElement>(null);
  const full = value.length >= ALLOW_RULES_MAX;
  const tooLong = draft.length > ALLOW_RULE_MAX_LEN;

  const startAdding = () => {
    setAdding(true);
    setTimeout(() => draftRef.current?.focus(), 0);
  };
  const cancel = () => {
    setDraft("");
    setAdding(false);
  };
  const add = () => {
    const rule = draft.trim();
    if (!rule || tooLong || full) return;
    onChange([...value, rule]);
    cancel();
  };

  return (
    <div className={cn("overflow-hidden rounded-lg border", disabled && "opacity-50")}>
      {value.length === 0 ? (
        <p className="px-4 py-3 text-sm text-muted-foreground">No rules yet.</p>
      ) : (
        <ul className="divide-y">
          {value.map((rule, i) => (
            <li key={`${i}-${rule}`} className="flex items-start gap-3 px-4 py-2.5 text-sm">
              <span className="min-w-0 flex-1 whitespace-pre-wrap break-words">{rule}</span>
              <button
                type="button"
                aria-label={`Remove rule ${i + 1}`}
                disabled={disabled}
                onClick={() => onChange(value.filter((_, j) => j !== i))}
                className="flex size-6 shrink-0 items-center justify-center rounded-sm text-muted-foreground hover:bg-foreground/10 hover:text-foreground"
              >
                <X className="size-3.5" />
              </button>
            </li>
          ))}
        </ul>
      )}
      <div className="border-t bg-muted/30">
        {adding ? (
          <div className="space-y-2 p-3">
            <Textarea
              id={id}
              ref={draftRef}
              rows={2}
              value={draft}
              onChange={(e) => setDraft(e.target.value)}
              placeholder='e.g. "Creating tasks in ClickUp is expected and approved."'
              aria-invalid={tooLong}
              onKeyDown={(e) => {
                if (e.key === "Enter" && !e.shiftKey) {
                  e.preventDefault();
                  add();
                } else if (e.key === "Escape") {
                  cancel();
                }
              }}
            />
            <div className="flex items-center justify-between gap-3">
              <span className={cn("text-xs tabular-nums text-muted-foreground", tooLong && "text-destructive")}>
                {draft.length.toLocaleString()} / {ALLOW_RULE_MAX_LEN.toLocaleString()}
              </span>
              <div className="flex gap-2">
                <Button size="sm" variant="ghost" onClick={cancel}>
                  Cancel
                </Button>
                <Button size="sm" onClick={add} disabled={!draft.trim() || tooLong}>
                  Add rule
                </Button>
              </div>
            </div>
          </div>
        ) : (
          <div className="flex items-center justify-between gap-3 px-2 py-1.5">
            <Button size="sm" variant="ghost" disabled={disabled || full} onClick={startAdding}>
              <Plus className="size-4" />
              Add rule
            </Button>
            <span className="pr-2 text-xs tabular-nums text-muted-foreground">
              {value.length} of {ALLOW_RULES_MAX} rules · up to {ALLOW_RULE_MAX_LEN.toLocaleString()} characters each
            </span>
          </div>
        )}
      </div>
    </div>
  );
}
