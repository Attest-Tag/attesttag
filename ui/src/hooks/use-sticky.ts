"use client";

import { useEffect, useState } from "react";

// What the console remembers about how a page is arranged — which workspaces are folded, which
// channels are starred, which bundles are open — lives in this browser and nowhere else. None of
// it is worth a column, a request or a moment of somebody else's attention: it is how one person
// left one screen, and the worst that a lost value can do is open a card they would have closed.
//
// Both hooks read after mount rather than during render. The console is a static export, so a
// value read while rendering is the build machine's answer in the served HTML and the reader's
// answer a moment later — which React counts as a hydration mismatch.

function read<T>(key: string): T | null {
  try {
    const raw = window.localStorage.getItem(key);
    return raw ? (JSON.parse(raw) as T) : null;
  } catch {
    // Private mode, storage turned off, or a value someone edited by hand.
    return null;
  }
}

function write(key: string, value: unknown) {
  try {
    window.localStorage.setItem(key, JSON.stringify(value));
  } catch {
    // Losing the arrangement is not worth taking the page down for.
  }
}

/**
 * A set of ids that survives a reload — the things someone has picked out: folded workspaces,
 * starred channels.
 *
 * `dropForNow` takes an id out of the set without writing it down, for a reveal that should not
 * outlive the visit: arriving at a link inside a folded group has to show it, but that is the
 * link's doing, not a decision to unfold, and the next visit should find the fold as it was left.
 */
export function useStickySet<T extends string | number>(key: string) {
  const [value, setValue] = useState<Set<T>>(new Set());
  useEffect(() => {
    const stored = read<T[]>(key);
    // eslint-disable-next-line react-hooks/set-state-in-effect -- one-shot read of stored UI state
    if (stored) setValue(new Set(stored));
  }, [key]);

  const toggle = (id: T) =>
    setValue((prev) => {
      const next = new Set(prev);
      if (!next.delete(id)) next.add(id);
      write(key, [...next]);
      return next;
    });

  const dropForNow = (id: T) =>
    setValue((prev) => {
      if (!prev.has(id)) return prev;
      const next = new Set(prev);
      next.delete(id);
      return next;
    });

  return { value, toggle, dropForNow } as const;
}

/**
 * Open/closed per id, where "never touched" has to stay distinct from "closed" — a card whose
 * default is open, closed by hand, has to come back closed, and a set of open ids cannot say
 * that. `get` takes the default to fall back on for anything nobody has touched.
 */
export function useStickyFlags(key: string) {
  const [flags, setFlags] = useState<Record<string, boolean>>({});
  useEffect(() => {
    const stored = read<Record<string, boolean>>(key);
    // eslint-disable-next-line react-hooks/set-state-in-effect -- one-shot read of stored UI state
    if (stored) setFlags(stored);
  }, [key]);

  const get = (id: string | number, fallback: boolean) => flags[String(id)] ?? fallback;

  const set = (id: string | number, on: boolean) =>
    setFlags((prev) => {
      const next = { ...prev, [String(id)]: on };
      write(key, next);
      return next;
    });

  return { get, set } as const;
}
