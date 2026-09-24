"use client";

import * as React from "react";

// Elements that are already consuming the keystroke. The tag test covers real
// text entry; the overlay test covers menus, selects, comboboxes and modals,
// whose items are divs rather than inputs but which own the keyboard while
// they're open — Radix runs its own letter typeahead in there.
// Exported because every global shortcut has to be quiet in the same places, and three
// selectors copied into a second file are three selectors that drift. shouldFocusSearch below
// is only for the unmodified single-key case — a chord shortcut refuses modifiers there and
// needs its own handler over these.
export const TYPING = /^(INPUT|TEXTAREA|SELECT)$/;
export const OVERLAY =
  '[role="menu"],[role="listbox"],[role="dialog"],[role="alertdialog"],[role="combobox"]';
// A modal above us marks the rest of the page aria-hidden/inert. Pulling focus
// down there would fight the dialog's own focus trap.
export const BURIED = '[aria-hidden="true"],[inert]';

/** The bits of a keydown event and an element the decision below actually reads. */
type KeyLike = Pick<KeyboardEvent, "key"> &
  Partial<Pick<KeyboardEvent, "metaKey" | "ctrlKey" | "altKey" | "isComposing">>;
type ElementLike = {
  tagName: string;
  isContentEditable: boolean;
  closest: (selectors: string) => unknown;
};

/**
 * Whether `event` should pull focus into the search field — the whole safety
 * question in one place, so it can be tested without a DOM. Exported for
 * `use-search-shortcut.test.ts`; callers want the hook.
 */
export function shouldFocusSearch(
  key: string,
  event: KeyLike,
  target: ElementLike | null,
  input: ElementLike | null,
): boolean {
  // event.key is the character actually produced, so layouts where "/" needs
  // Shift (AZERTY, QWERTZ) work without a shiftKey check of their own.
  if (event.key !== key) return false;
  if (event.metaKey || event.ctrlKey || event.altKey) return false;
  if (event.isComposing) return false; // the keystroke belongs to the IME
  if (target && (target.isContentEditable || TYPING.test(target.tagName)))
    return false;
  if (target?.closest(OVERLAY)) return false;
  if (!input || input.closest(BURIED)) return false;
  return true;
}

/**
 * Focus the field behind `ref` when `key` is pressed anywhere on the page.
 *
 * The shortcut stays quiet whenever the keystroke belongs to something else, so
 * a screen can carry one without stealing a literal "/" from the assistant
 * composer, a comment box, or an open dropdown.
 */
export function useSearchShortcut(
  key: string | undefined,
  ref: React.RefObject<HTMLInputElement | null>,
) {
  React.useEffect(() => {
    if (!key) return;
    const onKey = (e: KeyboardEvent) => {
      const input = ref.current;
      if (!shouldFocusSearch(key, e, e.target as HTMLElement | null, input))
        return;
      e.preventDefault();
      input?.focus();
      input?.select();
    };
    document.addEventListener("keydown", onKey);
    return () => document.removeEventListener("keydown", onKey);
  }, [key, ref]);
}
