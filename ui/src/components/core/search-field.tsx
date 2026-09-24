"use client";

import type * as React from "react";
import { useCallback, useRef } from "react";
import { Search, X } from "lucide-react";
import { Input } from "@/components/ui/input";
import { useSearchShortcut } from "@/hooks/use-search-shortcut";
import { cn } from "@/lib/utils";

// The compact search input that sits above a scrolling list — the attach
// dialog's file/template/case rails, the report page's case picker, and every
// list screen's toolbar. One implementation so "search this list" looks and
// behaves the same everywhere.
export function SearchField({
  value,
  onChange,
  placeholder,
  className,
  inputClassName,
  shortcut,
  clearable,
  disabled,
  onKeyDown,
  ref,
}: {
  value: string;
  onChange: (value: string) => void;
  placeholder: string;
  /** Classes for the wrapper — sizing lives here (the input is `w-full`). */
  className?: string;
  inputClassName?: string;
  /**
   * Key that focuses this field from anywhere on the page, shown as a chip
   * inside it while it's empty — the same affordance as the topbar's ⌘K. The
   * field owns the handler; see `useSearchShortcut` for when it declines to
   * fire. Only one field per screen should claim a given key.
   */
  shortcut?: string;
  /**
   * Show an ✕ inside the field once something is typed. It sits where the
   * shortcut chip sits when empty, so the two never fight for the corner.
   */
  clearable?: boolean;
  disabled?: boolean;
  /** Keys the list below wants for itself — Enter to take the first hit, Escape to clear. */
  onKeyDown?: React.KeyboardEventHandler<HTMLInputElement>;
  ref?: React.Ref<HTMLInputElement>;
}) {
  const innerRef = useRef<HTMLInputElement>(null);
  useSearchShortcut(shortcut, innerRef);

  // The shortcut needs a ref of its own, so fan the node out to the caller's.
  const setRef = useCallback(
    (node: HTMLInputElement | null) => {
      innerRef.current = node;
      if (typeof ref === "function") ref(node);
      else if (ref) (ref as React.RefObject<HTMLInputElement | null>).current = node;
    },
    [ref],
  );

  return (
    // `flex`, not a plain block: an <input> is inline-block on a baseline, so a
    // block wrapper adds the line box's descender gap under it and the field
    // measures 25px around a 24px control. That phantom pixel is invisible on
    // its own and moves the page where a 24px row swaps in — a list's toolbar
    // and its selection bar share one min-h-8 slot, so ticking a checkbox
    // nudged the table by 1px. Flex has no line box, so the wrapper is the
    // control's own height. The decorations are absolute and unaffected.
    <div className={cn("relative flex", className)}>
      <Search
        strokeWidth={1.75}
        className="pointer-events-none absolute top-1/2 left-3 size-4 -translate-y-1/2 text-muted-foreground"
      />
      <Input
        ref={setRef}
        value={value}
        onChange={(e) => onChange(e.target.value)}
        onKeyDown={onKeyDown}
        disabled={disabled}
        placeholder={placeholder}
        aria-label={placeholder}
        // Same recipe as the topbar's ⌘K trigger: a filled field rather than an
        // outlined one, with the border kept transparent so it can appear on
        // hover/focus without shifting anything. Filling instead of outlining is
        // what lets the key chip below stay boxed — it's the only box in the
        // composition, so it reads as a key rather than as more chrome.
        className={cn(
          "h-8 border-transparent bg-secondary/70 pl-9 text-sm transition-colors hover:bg-secondary dark:bg-secondary/70",
          (shortcut || clearable) && "pr-8",
          inputClassName,
        )}
      />
      {clearable && value !== "" && (
        <button
          type="button"
          disabled={disabled}
          aria-label="Clear search"
          className="absolute top-1/2 right-2 -translate-y-1/2 rounded-sm p-0.5 text-muted-foreground hover:bg-foreground/5 hover:text-foreground"
          onClick={() => {
            onChange("");
            innerRef.current?.focus();
          }}
        >
          <X className="size-3.5" />
        </button>
      )}
      {shortcut && value === "" && (
        // Filled a touch darker than the field rather than card-white: on a
        // filled input a white chip is the brightest thing in the control and
        // pulls the eye off the placeholder. foreground/5 tracks the theme, so
        // it lightens against a dark field instead of needing a second colour.
        <kbd className="pointer-events-none absolute top-1/2 right-1.5 -translate-y-1/2 rounded border border-transparent bg-foreground/5 px-1.5 py-0.5 text-[10px] font-medium text-muted-foreground">
          {shortcut}
        </kbd>
      )}
    </div>
  );
}
