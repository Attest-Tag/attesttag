"use client";

import { useCallback, useEffect, useId, useRef, useState } from "react";
import { Card } from "@/components/ui/card";
import { Label } from "@/components/ui/label";
import { cn } from "@/lib/utils";

/**
 * A titled card of related settings rows. The title sits above the card so the
 * row headings inside keep their own weight; `accessory` renders beside it
 * (a count chip, a small action).
 */
export function SettingsGroup({
  title,
  accessory,
  children,
  className,
}: {
  title: string;
  accessory?: React.ReactNode;
  children: React.ReactNode;
  className?: string;
}) {
  return (
    <section className={cn("space-y-2", className)}>
      <div className="flex items-center justify-between gap-3 px-1">
        <h2 className="text-sm font-semibold text-foreground">{title}</h2>
        {accessory}
      </div>
      <Card className="divide-y py-0">{children}</Card>
    </section>
  );
}

/**
 * One labelled row of a settings panel: the heading and its explanation on the
 * left, the controls on the right, stacking on narrow screens.
 *
 * Settings screens used to be a stack of one-field cards, which gave every
 * field a full-width input and a heading of equal weight to every other — a
 * form dump with no shape. Putting the label column beside the controls caps
 * the field width at something readable and lets the eye skim headings down
 * the left edge. Group these inside a single `Card` with `divide-y` rather
 * than giving each its own card.
 */
export function SettingsSection({
  title,
  description,
  children,
  className,
}: {
  title: string;
  description?: React.ReactNode;
  children: React.ReactNode;
  className?: string;
}) {
  return (
    <section
      className={cn(
        "grid gap-3 px-6 py-5 md:grid-cols-[13rem_1fr] md:gap-8",
        className,
      )}
    >
      <div className="space-y-1">
        <h3 className="text-sm font-semibold text-foreground">{title}</h3>
        {description && <HelpText>{description}</HelpText>}
      </div>
      <div className="min-w-0">{children}</div>
    </section>
  );
}

// Tailwind only ships the clamp utilities it can see written out, so the
// supported depths live here rather than being built from `lines`.
const CLAMP: Record<number, string> = {
  2: "line-clamp-2",
  3: "line-clamp-3",
  4: "line-clamp-4",
};

/**
 * An explanation shown a few lines deep, with the rest one click away.
 *
 * These rows earn their prose — the long ones say what a setting costs, what
 * it quietly turns off, or what has to exist before it means anything — but a
 * paragraph in a 13rem column runs ten lines or more, so a page of them reads
 * as a wall of text with the controls buried in it and the next setting
 * pushed off the screen. Clamping to the opening lines keeps the column
 * skimmable without deleting a sentence anyone needed.
 *
 * The rest is behind a button, not a hover: help you cannot reach on a phone,
 * from the keyboard or with a screen reader is help that is not there. The
 * button only appears when there is genuinely more to see, so the short
 * descriptions stay plain text.
 */
export function HelpText({
  children,
  lines = 3,
  className,
}: {
  children: React.ReactNode;
  /** How much shows before the toggle: 2, 3 or 4 lines. */
  lines?: 2 | 3 | 4;
  className?: string;
}) {
  const [open, setOpen] = useState(false);
  const [long, setLong] = useState(false);
  const ref = useRef<HTMLParagraphElement>(null);
  const id = useId();

  // Whether clamping this paragraph is worth doing depends on the width it
  // ends up at, so it is measured rather than guessed from the text length:
  // the same sentence runs six lines in the label column and two on a wide
  // screen. A toggle that buys back a single line is worse than the line, so
  // the clamp only comes on once there is more than one line to hide.
  const measure = useCallback(() => {
    const el = ref.current;
    if (!el) return;
    // scrollHeight is the whole paragraph whether or not the clamp is on, so
    // this reads the same open or closed.
    const height = parseFloat(getComputedStyle(el).lineHeight) || 16;
    setLong(Math.round(el.scrollHeight / height) > lines + 1);
  }, [lines]);

  useEffect(() => {
    const el = ref.current;
    if (!el) return;
    measure();
    const observer = new ResizeObserver(measure);
    observer.observe(el);
    // A late-loading font reflows the text without changing the clamped
    // height, which no resize reports.
    document.fonts?.ready.then(measure).catch(() => {});
    return () => observer.disconnect();
  }, [measure]);

  // Rows whose wording changes with what is stored — a plan, a domain, a
  // channel's inherited value — keep the same element, so the copy changing
  // is its own reason to measure again.
  useEffect(measure, [measure, children]);

  const clamped = long && !open;
  return (
    <div className={cn("space-y-1", className)}>
      <p
        ref={ref}
        id={id}
        className={cn(
          "text-xs leading-relaxed text-muted-foreground",
          clamped && CLAMP[lines],
        )}
      >
        {children}
      </p>
      {long && (
        <button
          type="button"
          aria-expanded={open}
          aria-controls={id}
          onClick={() => setOpen(!open)}
          className="text-xs font-medium text-muted-foreground underline-offset-2 hover:text-foreground hover:underline"
        >
          {open ? "Show less" : "Show more"}
        </button>
      )}
    </div>
  );
}

// Both modes share this label column, so switching between them moves the
// values and nothing else — no reflow, no hunting for the field you were
// reading a moment ago.
const ROW = "grid gap-1 sm:grid-cols-[10rem_1fr] sm:items-center sm:gap-3";

/**
 * The resting state of an editable section: the stored values as text, with
 * the control that opens the editor beside them.
 *
 * Sections default to this rather than to live inputs. Most visits to a
 * settings screen are to read something, and a column of empty boxes makes
 * "not filled in" and "editable" look identical — you can't tell what's
 * actually set without clicking into each one.
 */
export function SettingsRows({
  children,
  action,
}: {
  children: React.ReactNode;
  action?: React.ReactNode;
}) {
  return (
    <div className="flex items-start justify-between gap-4">
      <dl className="min-w-0 flex-1 space-y-2 text-sm">{children}</dl>
      {action && <div className="shrink-0">{action}</div>}
    </div>
  );
}

/** One label → value line of a `SettingsRows` readout. */
export function SettingsRow({
  label,
  children,
  empty,
}: {
  label: string;
  /** Rendered when set; falsy falls back to `empty`. */
  children?: React.ReactNode;
  /** What an unset value reads as. Say what's missing, not "—". */
  empty?: string;
}) {
  const filled = children !== null && children !== undefined && children !== "";
  return (
    <div className={ROW}>
      <dt className="text-muted-foreground">{label}</dt>
      <dd className={cn("min-w-0", !filled && "text-muted-foreground/70")}>
        {filled ? children : (empty ?? "Not set")}
      </dd>
    </div>
  );
}

/** One label → control line of an editor, aligned with `SettingsRow`. */
export function SettingsEditRow({
  label,
  htmlFor,
  hint,
  children,
}: {
  label: string;
  htmlFor: string;
  hint?: React.ReactNode;
  children: React.ReactNode;
}) {
  return (
    <div className={ROW}>
      <Label htmlFor={htmlFor} className="text-muted-foreground">
        {label}
      </Label>
      <div className="min-w-0 space-y-1">
        {children}
        {hint && <p className="text-xs text-muted-foreground">{hint}</p>}
      </div>
    </div>
  );
}

/** Buttons closing out an editor, indented to line up with its controls. */
export function SettingsActions({ children }: { children: React.ReactNode }) {
  return (
    <div className={ROW}>
      <span aria-hidden className="hidden sm:block" />
      <div className="flex flex-wrap gap-2">{children}</div>
    </div>
  );
}
