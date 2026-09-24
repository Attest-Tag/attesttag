"use client";

import { useEffect, useImperativeHandle, useRef, useState } from "react";
import { X } from "lucide-react";
import { cn } from "@/lib/utils";

/** What a form reaches for when it is about to read the list. See `flush`. */
export type ChipsHandle = {
  /**
   * The list including whatever is still half-typed in the box, committed as it goes. A form
   * reads this rather than the value it was last handed: pressing Enter is how a chip appears,
   * but it is not what somebody means by typing a value and then clicking Save.
   */
  flush: () => string[];
};

// A list of short values edited as chips: type, then Enter, comma or space to
// add; Backspace on an empty field removes the last. Used for hosts, path
// prefixes and methods, where a comma-separated string would hide typos.
export function ChipsInput({
  id,
  value,
  onChange,
  placeholder,
  normalize,
  mono = true,
  disabled,
  className,
  ref,
  "aria-invalid": ariaInvalid,
}: {
  id?: string;
  value: string[];
  onChange: (next: string[]) => void;
  placeholder?: string;
  /** Cleans a typed value before it becomes a chip; return "" to reject it. */
  normalize?: (raw: string) => string;
  mono?: boolean;
  disabled?: boolean;
  className?: string;
  ref?: React.Ref<ChipsHandle>;
  "aria-invalid"?: boolean;
}) {
  const [draft, setDraft] = useState("");
  // Read by things that run outside a render — the deferred blur below, and flush — so they see
  // what is in the box now rather than what it held when they were created.
  const draftRef = useRef("");
  const valueRef = useRef(value);
  valueRef.current = value;

  const setBoth = (next: string) => {
    draftRef.current = next;
    setDraft(next);
  };

  /** The list with everything in `raw` appended, keeping the order and dropping repeats. */
  const merged = (raw: string) => {
    const parts = raw
      .split(/[,\s]+/)
      .map((p) => (normalize ? normalize(p.trim()) : p.trim()))
      .filter(Boolean);
    if (parts.length === 0) return null;
    const next = [...valueRef.current];
    for (const p of parts) if (!next.includes(p)) next.push(p);
    return next;
  };

  const commit = (raw: string) => {
    const next = merged(raw);
    if (next === null) return;
    onChange(next);
    setBoth("");
  };

  // Committing on blur changes the height of the box — a chip long enough to need its own line
  // pushes everything below it down — and a layout that moves between mousedown and mouseup
  // takes the button out from under the pointer, so the click never lands and Save does
  // nothing. The commit therefore waits for the click it would have stolen; anything that reads
  // the list in the meantime takes the half-typed value with it through flush().
  const later = useRef<number | null>(null);
  const cancel = () => {
    if (later.current !== null) window.clearTimeout(later.current);
    later.current = null;
  };
  useEffect(() => cancel, []);

  useImperativeHandle(ref, () => ({
    flush: () => {
      cancel();
      const next = merged(draftRef.current);
      if (next === null) return valueRef.current;
      onChange(next);
      setBoth("");
      return next;
    },
  }));

  return (
    <div
      className={cn(
        "flex min-h-8 flex-wrap items-center gap-1.5 rounded-md border border-input bg-transparent px-2 py-1 text-sm shadow-xs transition-[color,box-shadow] focus-within:border-ring focus-within:ring-[3px] focus-within:ring-ring/50 dark:bg-input/30",
        ariaInvalid && "border-destructive ring-destructive/20",
        disabled && "opacity-50",
        className,
      )}
      onClick={(e) => (e.currentTarget.querySelector("input") as HTMLInputElement | null)?.focus()}
    >
      {value.map((item) => (
        <span
          key={item}
          className={cn(
            "inline-flex h-6 max-w-full items-center gap-1 rounded-sm bg-secondary px-2 text-xs text-secondary-foreground",
            mono && "font-mono",
          )}
          title={item}
        >
          <span className="truncate">{item}</span>
          {!disabled && (
            <button
              type="button"
              aria-label={`Remove ${item}`}
              className="-mr-1 flex size-4 shrink-0 items-center justify-center rounded-sm text-muted-foreground hover:bg-foreground/10 hover:text-foreground"
              onClick={(e) => {
                e.stopPropagation();
                onChange(valueRef.current.filter((v) => v !== item));
              }}
            >
              <X className="size-3" />
            </button>
          )}
        </span>
      ))}
      <input
        id={id}
        value={draft}
        disabled={disabled}
        placeholder={value.length === 0 ? placeholder : undefined}
        className={cn(
          "h-6 min-w-24 flex-1 bg-transparent text-sm outline-none placeholder:text-muted-foreground",
          mono && "font-mono",
        )}
        onChange={(e) => setBoth(e.target.value)}
        onBlur={() => {
          cancel();
          later.current = window.setTimeout(() => {
            later.current = null;
            commit(draftRef.current);
          }, 0);
        }}
        onKeyDown={(e) => {
          if (e.key === "Enter" || e.key === "," || e.key === " ") {
            e.preventDefault();
            commit(draft);
          } else if (e.key === "Backspace" && draft === "" && value.length > 0) {
            onChange(value.slice(0, -1));
          }
        }}
      />
    </div>
  );
}
